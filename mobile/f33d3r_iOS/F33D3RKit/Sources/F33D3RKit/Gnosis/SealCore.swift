import Foundation
import CryptoKit

/// The Swift twin of `sealcore`, the Rust crate the browser runs as WebAssembly.
///
/// Gnosis sealed mode is end-to-end encrypted, so the server holds nothing it
/// can read and nothing it can fix. Whatever this file does differently from
/// `sealcore/src/lib.rs` shows up as a message that one client can read and the
/// other cannot, with no error anywhere that says which byte disagreed. So the
/// scheme is restated here in full, and the tests pin it against vectors
/// produced outside Swift.
///
/// ## The scheme
///
/// Per message: one random 32-byte AES-256-GCM **content key** encrypts the UTF-8
/// body exactly once, under a random 12-byte nonce.
///
/// Per recipient: a fresh ephemeral X25519 secret does ECDH against that
/// recipient's public key; HKDF-SHA256 with **no salt** and info
/// `sealcore-v1-x25519-wrap` expands the shared secret into a 32-byte wrap key;
/// AES-256-GCM seals the content key to it under its own nonce. A ten-person
/// conversation is one encrypted body and ten wrapped copies of one key.
///
/// ## Four places this is easy to get wrong
///
/// 1. **The GCM tag.** The Rust `aes-gcm` crate returns ciphertext with the
///    16-byte tag appended and the nonce separate. CryptoKit keeps all three
///    apart, and its `.combined` is `nonce || ciphertext || tag` — a *different*
///    layout that would decrypt to garbage on the other side. So the wire form
///    is built by hand as `ciphertext || tag`, and consumed by splitting the
///    last 16 bytes back off. See ``seal(_:usingContentKey:)`` and ``open(_:)``.
/// 2. **The HKDF salt.** `Hkdf::<Sha256>::new(None, ikm)` is HKDF with an
///    *absent* salt, which the RFC defines as a zero-filled block of the hash
///    length. That is passed explicitly here rather than relying on CryptoKit's
///    no-salt overload doing the same thing; `SealCoreHKDFTests` proves the two
///    agree.
/// 3. **What `wrap_with_key` encrypts.** It base64-*decodes* its argument first,
///    so the vault holds the 32 raw private-key bytes — not the 44 characters of
///    base64 that spell them. Wrapping the text would round-trip perfectly here
///    and produce a vault the browser cannot open.
/// 4. **The public key on the wire.** Raw 32 bytes, standard base64 with
///    padding. Not SPKI, not JWK, not hex — which is the opposite of
///    ``MalkuthKey``, where the same base64 carries an SPKI DER blob. The two
///    live in the same app and mean different things.
///
/// ## What this scheme does not have
///
/// No forward secrecy, no post-compromise security, no ratchet, no sequence
/// numbers, no sender identity inside the envelope (the sender is a server-side
/// column). A recipient's long-term key opens every message ever sent to them.
/// The one defence against a swapped key is trust-on-first-use pinning, which is
/// ``SealPinStore``, and a client that seals without consulting it has quietly
/// dropped the only protection there is.
public enum SealCore {

    /// `sealcore-v1-x25519-wrap`, from `HKDF_INFO` in lib.rs. Changing it is a
    /// protocol version bump, not a rename.
    static let hkdfInfo = Data("sealcore-v1-x25519-wrap".utf8)

    /// HKDF-Extract with an absent salt, which RFC 5869 defines as HashLen zero
    /// bytes. Written out rather than assumed.
    static let hkdfAbsentSalt = Data(repeating: 0, count: 32)

    static let contentKeyByteCount = 32
    static let nonceByteCount = 12
    static let tagByteCount = 16

    // MARK: - Wire types

    /// `{priv_b64, pub_b64}` — what `generate_keypair` returns.
    ///
    /// `privateKeyBase64` is the raw X25519 scalar. It is the account's whole
    /// identity: it goes to the server only ever wrapped, and never leaves this
    /// device unwrapped.
    public struct Keypair: Codable, Sendable, Equatable {
        public let privateKeyBase64: String
        public let publicKeyBase64: String

        public init(privateKeyBase64: String, publicKeyBase64: String) {
            self.privateKeyBase64 = privateKeyBase64
            self.publicKeyBase64 = publicKeyBase64
        }

        enum CodingKeys: String, CodingKey {
            case privateKeyBase64 = "priv_b64"
            case publicKeyBase64 = "pub_b64"
        }
    }

    /// One entry of the directory `GET /api/gnosis/directory` returns, and one
    /// element of the `recipients_json` `seal_message` takes.
    public struct Recipient: Codable, Sendable, Equatable {
        /// The per-account messaging identity — not the human's PIAL, and never
        /// rendered.
        public let account: String
        public let publicKeyBase64: String

        public init(account: String, publicKeyBase64: String) {
            self.account = account
            self.publicKeyBase64 = publicKeyBase64
        }

        enum CodingKeys: String, CodingKey {
            case account
            case publicKeyBase64 = "pub_b64"
        }
    }

    /// The content key wrapped to one recipient.
    public struct SealedKey: Codable, Sendable, Equatable {
        public let recipientAccount: String
        /// The public half of the ephemeral key used for this recipient only.
        public let ephemeralPublicKeyBase64: String
        /// The wrapped content key: AES-GCM ciphertext with its tag appended.
        public let sealedBase64: String
        public let sealedNonceBase64: String

        public init(
            recipientAccount: String,
            ephemeralPublicKeyBase64: String,
            sealedBase64: String,
            sealedNonceBase64: String
        ) {
            self.recipientAccount = recipientAccount
            self.ephemeralPublicKeyBase64 = ephemeralPublicKeyBase64
            self.sealedBase64 = sealedBase64
            self.sealedNonceBase64 = sealedNonceBase64
        }

        enum CodingKeys: String, CodingKey {
            case recipientAccount = "recipient_account"
            case ephemeralPublicKeyBase64 = "eph_pub_b64"
            case sealedBase64 = "sealed_b64"
            case sealedNonceBase64 = "sealed_nonce_b64"
        }
    }

    /// A whole sealed message: the body once, and the key once per recipient.
    public struct Envelope: Codable, Sendable, Equatable {
        public let bodyCiphertextBase64: String
        public let bodyNonceBase64: String
        public let sealed: [SealedKey]

        public init(bodyCiphertextBase64: String, bodyNonceBase64: String, sealed: [SealedKey]) {
            self.bodyCiphertextBase64 = bodyCiphertextBase64
            self.bodyNonceBase64 = bodyNonceBase64
            self.sealed = sealed
        }

        enum CodingKeys: String, CodingKey {
            case bodyCiphertextBase64 = "body_ct_b64"
            case bodyNonceBase64 = "body_nonce_b64"
            case sealed
        }

        /// The entry addressed to `account`, if this envelope has one. Matching
        /// on the account rather than trying every entry matters: a wrong entry
        /// fails as a decryption error, which is indistinguishable from a real
        /// tamper.
        public func sealedKey(for account: String) -> SealedKey? {
            sealed.first { $0.recipientAccount == account }
        }
    }

    /// `{ct_b64, nonce_b64}` — what `wrap_with_key` returns. The vault blob the
    /// server stores as `wrapped_priv` / `wrap_nonce`.
    public struct Wrapped: Codable, Sendable, Equatable {
        public let ciphertextBase64: String
        public let nonceBase64: String

        public init(ciphertextBase64: String, nonceBase64: String) {
            self.ciphertextBase64 = ciphertextBase64
            self.nonceBase64 = nonceBase64
        }

        enum CodingKeys: String, CodingKey {
            case ciphertextBase64 = "ct_b64"
            case nonceBase64 = "nonce_b64"
        }
    }

    // MARK: - Identity

    /// A fresh X25519 identity keypair, both halves as standard padded base64 of
    /// the raw 32 bytes.
    public static func generateKeypair() -> Keypair {
        let privateKey = Curve25519.KeyAgreement.PrivateKey()
        return Keypair(
            privateKeyBase64: privateKey.rawRepresentation.base64EncodedString(),
            publicKeyBase64: privateKey.publicKey.rawRepresentation.base64EncodedString()
        )
    }

    /// 16 random bytes, base64 — `generate_salt`.
    public static func generateSalt() -> String {
        Data(randomBytes(16)).base64EncodedString()
    }

    /// The salt the browser derives the vault key with: base64 of SHA-256 of the
    /// lower-cased handle.
    ///
    /// It is deterministic and public on purpose — there is nowhere to store a
    /// random salt that a user signing in on a new device could reach before
    /// they have their key. What it buys is that two accounts with the same
    /// password do not share a vault key. `saltFromHandle` in `gnosis-seal.js`
    /// produces this string, `derive_backup_key` base64-decodes it, and Argon2
    /// therefore sees the 32 raw digest bytes — not the 44 characters.
    public static func saltFromHandle(_ handle: String) -> String {
        Data(SHA256.hash(data: Data(handle.lowercased().utf8))).base64EncodedString()
    }

    // MARK: - Sealing

    /// Seals `plaintext` to every recipient.
    ///
    /// One content key for the message, one ephemeral keypair per recipient.
    /// Both are fresh every call: there is no session and nothing is cached, so
    /// sealing the same text twice produces two entirely different envelopes.
    public static func seal(
        _ plaintext: String, to recipients: [Recipient]
    ) throws(SealCoreError) -> Envelope {
        guard !recipients.isEmpty else { throw .noRecipients }

        let contentKey = SymmetricKey(size: .bits256)
        let body = try aesSeal(key: contentKey, plaintext: Data(plaintext.utf8))

        var sealed: [SealedKey] = []
        sealed.reserveCapacity(recipients.count)
        for recipient in recipients {
            let theirPublicKey = try publicKey(
                fromBase64: recipient.publicKeyBase64, field: "recipient pub"
            )
            let ephemeral = Curve25519.KeyAgreement.PrivateKey()
            let wrapKey = try wrapKey(privateKey: ephemeral, peer: theirPublicKey)
            let wrapped = try aesSeal(
                key: wrapKey,
                plaintext: contentKey.withUnsafeBytes { Data($0) }
            )
            sealed.append(
                SealedKey(
                    recipientAccount: recipient.account,
                    ephemeralPublicKeyBase64: ephemeral.publicKey.rawRepresentation
                        .base64EncodedString(),
                    sealedBase64: wrapped.ciphertext.base64EncodedString(),
                    sealedNonceBase64: wrapped.nonce.base64EncodedString()
                )
            )
        }

        return Envelope(
            bodyCiphertextBase64: body.ciphertext.base64EncodedString(),
            bodyNonceBase64: body.nonce.base64EncodedString(),
            sealed: sealed
        )
    }

    /// Opens the entry in `envelope` addressed to `account`.
    ///
    /// Throws ``SealCoreError/notAddressedToAccount(_:)`` when there is no such
    /// entry, which is a different situation from a decryption failure and must
    /// not be shown to the user as "cannot decrypt": it means the sender did not
    /// have this account's key when they sealed, and the fix is a re-send, not a
    /// re-login.
    public static func open(
        _ envelope: Envelope, for account: String, privateKeyBase64: String
    ) throws(SealCoreError) -> String {
        guard let entry = envelope.sealedKey(for: account) else {
            throw .notAddressedToAccount(account)
        }
        return try open(envelope, sealedKey: entry, privateKeyBase64: privateKeyBase64)
    }

    /// Opens a specific entry — the shape the DOM path uses, where the bubble
    /// already carries the one entry meant for this reader.
    public static func open(
        _ envelope: Envelope, sealedKey entry: SealedKey, privateKeyBase64: String
    ) throws(SealCoreError) -> String {
        try open(
            privateKeyBase64: privateKeyBase64,
            ephemeralPublicKeyBase64: entry.ephemeralPublicKeyBase64,
            sealedKeyBase64: entry.sealedBase64,
            sealedNonceBase64: entry.sealedNonceBase64,
            bodyCiphertextBase64: envelope.bodyCiphertextBase64,
            bodyNonceBase64: envelope.bodyNonceBase64
        )
    }

    /// `open_message`, argument for argument, for callers holding loose fields
    /// from the server rather than a decoded envelope.
    public static func open(
        privateKeyBase64: String,
        ephemeralPublicKeyBase64: String,
        sealedKeyBase64: String,
        sealedNonceBase64: String,
        bodyCiphertextBase64: String,
        bodyNonceBase64: String
    ) throws(SealCoreError) -> String {
        let myKey = try privateKey(fromBase64: privateKeyBase64, field: "my priv")
        let ephemeralPublicKey = try publicKey(
            fromBase64: ephemeralPublicKeyBase64, field: "eph pub"
        )
        let unwrapKey = try wrapKey(privateKey: myKey, peer: ephemeralPublicKey)

        let contentKeyBytes = try aesOpen(
            key: unwrapKey,
            nonce: try decode(sealedNonceBase64, field: "sealed nonce"),
            ciphertext: try decode(sealedKeyBase64, field: "sealed")
        )
        guard contentKeyBytes.count == contentKeyByteCount else {
            throw .badKeyLength(field: "content key", count: contentKeyBytes.count)
        }

        let body = try aesOpen(
            key: SymmetricKey(data: contentKeyBytes),
            nonce: try decode(bodyNonceBase64, field: "body nonce"),
            ciphertext: try decode(bodyCiphertextBase64, field: "body ct")
        )
        guard let text = String(data: body, encoding: .utf8) else { throw .plaintextNotUTF8 }
        return text
    }

    // MARK: - Account key custody

    /// Argon2id over the backup code (in practice, the login password) and the
    /// handle-derived salt, base64 of the 32-byte tag.
    ///
    /// This is the slow one: ``Argon2/Parameters/sealcore`` touches 19 MiB. Call
    /// it off the main actor.
    public static func deriveBackupKey(
        code: String, saltBase64: String
    ) throws(SealCoreError) -> String {
        let salt = try decode(saltBase64, field: "salt")
        do {
            let key = try Argon2.hash(
                password: Data(code.utf8), salt: salt, parameters: .sealcore
            )
            return key.base64EncodedString()
        } catch {
            throw .argon2(error)
        }
    }

    /// Wraps a secret under a 32-byte key.
    ///
    /// `plaintextBase64` is **decoded first**, exactly as `wrap_with_key_impl`
    /// does, so what is sealed is the bytes it spells. Passing the private key's
    /// base64 string here wraps 32 bytes, not 44 characters.
    public static func wrap(
        keyBase64: String, plaintextBase64: String
    ) throws(SealCoreError) -> Wrapped {
        let key = try symmetricKey(fromBase64: keyBase64)
        let plaintext = try decode(plaintextBase64, field: "plaintext")
        let sealed = try aesSeal(key: key, plaintext: plaintext)
        return Wrapped(
            ciphertextBase64: sealed.ciphertext.base64EncodedString(),
            nonceBase64: sealed.nonce.base64EncodedString()
        )
    }

    /// Unwraps what ``wrap(keyBase64:plaintextBase64:)`` produced, returning the
    /// plaintext re-encoded as base64 — which is how the private key gets back
    /// to the form the rest of this API takes.
    public static func unwrap(
        keyBase64: String, ciphertextBase64: String, nonceBase64: String
    ) throws(SealCoreError) -> String {
        let key = try symmetricKey(fromBase64: keyBase64)
        let plaintext = try aesOpen(
            key: key,
            nonce: try decode(nonceBase64, field: "nonce"),
            ciphertext: try decode(ciphertextBase64, field: "ct")
        )
        return plaintext.base64EncodedString()
    }

    // MARK: - Primitives

    /// ECDH then HKDF, the only way a wrap key is ever made.
    ///
    /// CryptoKit refuses a key agreement whose result is the all-zero point,
    /// where `x25519-dalek` would hand back the zeros; that is a low-order
    /// public key, which only an attacker sends, so failing is the better
    /// answer even though it is a place the two implementations differ.
    static func wrapKey(
        privateKey: Curve25519.KeyAgreement.PrivateKey,
        peer: Curve25519.KeyAgreement.PublicKey
    ) throws(SealCoreError) -> SymmetricKey {
        let shared: SharedSecret
        do {
            shared = try privateKey.sharedSecretFromKeyAgreement(with: peer)
        } catch {
            throw .keyAgreementFailed
        }
        return shared.withUnsafeBytes { bytes in
            HKDF<SHA256>.deriveKey(
                inputKeyMaterial: SymmetricKey(data: Data(bytes)),
                salt: hkdfAbsentSalt,
                info: hkdfInfo,
                outputByteCount: 32
            )
        }
    }

    /// AES-256-GCM with a random 12-byte nonce, returning the Rust crate's
    /// layout: **ciphertext with the tag appended**, nonce separate.
    static func aesSeal(
        key: SymmetricKey, plaintext: Data
    ) throws(SealCoreError) -> (ciphertext: Data, nonce: Data) {
        let nonceBytes = Data(randomBytes(nonceByteCount))
        do {
            let nonce = try AES.GCM.Nonce(data: nonceBytes)
            let box = try AES.GCM.seal(plaintext, using: key, nonce: nonce)
            // Never `box.combined`: that prefixes the nonce, and the Rust side
            // would treat the first 12 bytes as ciphertext.
            return (box.ciphertext + box.tag, nonceBytes)
        } catch {
            throw .encryptionFailed
        }
    }

    /// The inverse: split the trailing 16-byte tag back off, then open.
    ///
    /// Every failure past this point — wrong key, wrong recipient's entry,
    /// flipped bit, truncated blob — arrives as the same
    /// ``SealCoreError/decryptionFailed``, because that is all GCM will tell
    /// anyone. It is an ordinary error and never a crash.
    static func aesOpen(
        key: SymmetricKey, nonce: Data, ciphertext: Data
    ) throws(SealCoreError) -> Data {
        guard nonce.count == nonceByteCount else {
            throw .badNonceLength(nonce.count)
        }
        guard ciphertext.count >= tagByteCount else {
            throw .ciphertextTooShort(ciphertext.count)
        }
        do {
            let box = try AES.GCM.SealedBox(
                nonce: try AES.GCM.Nonce(data: nonce),
                ciphertext: ciphertext.dropLast(tagByteCount),
                tag: ciphertext.suffix(tagByteCount)
            )
            return try AES.GCM.open(box, using: key)
        } catch {
            throw .decryptionFailed
        }
    }

    // MARK: - Decoding

    static func decode(_ base64: String, field: String) throws(SealCoreError) -> Data {
        // `b64d` in Rust trims the string first; a value that came through JSON
        // or an HTML attribute can carry whitespace, and the browser accepts it.
        let trimmed = base64.trimmingCharacters(in: .whitespacesAndNewlines)
        guard let data = Data(base64Encoded: trimmed) else { throw .malformedBase64(field: field) }
        return data
    }

    static func privateKey(
        fromBase64 base64: String, field: String
    ) throws(SealCoreError) -> Curve25519.KeyAgreement.PrivateKey {
        let raw = try decode(base64, field: field)
        guard raw.count == 32 else { throw .badKeyLength(field: field, count: raw.count) }
        do {
            return try Curve25519.KeyAgreement.PrivateKey(rawRepresentation: raw)
        } catch {
            throw .badKeyLength(field: field, count: raw.count)
        }
    }

    static func publicKey(
        fromBase64 base64: String, field: String
    ) throws(SealCoreError) -> Curve25519.KeyAgreement.PublicKey {
        let raw = try decode(base64, field: field)
        guard raw.count == 32 else { throw .badKeyLength(field: field, count: raw.count) }
        do {
            return try Curve25519.KeyAgreement.PublicKey(rawRepresentation: raw)
        } catch {
            throw .badKeyLength(field: field, count: raw.count)
        }
    }

    static func symmetricKey(fromBase64 base64: String) throws(SealCoreError) -> SymmetricKey {
        let raw = try decode(base64, field: "wrap key")
        guard raw.count == 32 else { throw .badKeyLength(field: "wrap key", count: raw.count) }
        return SymmetricKey(data: raw)
    }

    static func randomBytes(_ count: Int) -> [UInt8] {
        var bytes = [UInt8](repeating: 0, count: count)
        // CryptoKit's own RNG, which is the system CSPRNG; `SystemRandomNumberGenerator`
        // is the same source and this spelling says so at the call site.
        var generator = SystemRandomNumberGenerator()
        for i in 0..<count { bytes[i] = UInt8.random(in: 0...255, using: &generator) }
        return bytes
    }
}

// MARK: - Errors

/// Why a seal or an open did not happen.
///
/// Every one of these is an ordinary error. A message that will not decrypt is a
/// normal event in a system where keys rotate and devices come and go — the web
/// client renders it as a padlock and moves on — so nothing here traps, and
/// nothing here is a precondition.
public enum SealCoreError: Error, Equatable, Sendable {
    /// A field that should have been base64 was not. `field` matches the Rust
    /// error text so the two implementations' logs line up.
    case malformedBase64(field: String)
    /// A key or a content key that decoded to something other than 32 bytes.
    case badKeyLength(field: String, count: Int)
    /// AES-GCM nonces are 12 bytes; `aes_open` refuses anything else before it
    /// tries to decrypt, and so does this.
    case badNonceLength(Int)
    /// Shorter than the tag it is supposed to end with.
    case ciphertextTooShort(Int)
    /// Wrong key, wrong recipient's entry, or tampered bytes. GCM does not say
    /// which, and neither does this.
    case decryptionFailed
    case encryptionFailed
    /// The envelope carries no entry for this account: the sender sealed without
    /// this account's public key.
    case notAddressedToAccount(String)
    /// X25519 refused the peer key — a low-order point, so a hostile one.
    case keyAgreementFailed
    /// Decrypted fine and was not text.
    case plaintextNotUTF8
    case noRecipients
    case argon2(Argon2Error)
}

extension SealCoreError: CustomStringConvertible {
    public var description: String {
        switch self {
        case .malformedBase64(let field): return "\(field): not base64"
        case .badKeyLength(let field, let count): return "\(field): expected 32 bytes, got \(count)"
        case .badNonceLength(let count): return "nonce: expected 12 bytes, got \(count)"
        case .ciphertextTooShort(let count):
            return "ciphertext: shorter than a GCM tag (\(count) bytes)"
        case .decryptionFailed: return "cannot decrypt (wrong key or tampered)"
        case .encryptionFailed: return "encryption failed"
        case .notAddressedToAccount(let account):
            return "this message was not sealed to \(account)"
        case .keyAgreementFailed: return "the recipient's public key was rejected"
        case .plaintextNotUTF8: return "the decrypted message was not text"
        case .noRecipients: return "no recipients"
        case .argon2(let error): return "argon2: \(error)"
        }
    }
}
