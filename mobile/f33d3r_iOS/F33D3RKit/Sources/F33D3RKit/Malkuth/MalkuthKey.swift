import Foundation
import CryptoKit
import Security

/// This device's Work-signing key: ECDSA P-256, private half in the Keychain,
/// public half exported as SPKI DER for registration with the key authority.
///
/// ## What it signs, and what it does not
///
/// It signs the **UTF-8 bytes of the CID string** — `sha256:9f2c…` — and not the
/// payload and not the raw digest. `verifyMalkuthSig` does
/// `sha256.Sum256([]byte(cid))` and verifies that as the pre-hashed message,
/// which is what WebCrypto's `ECDSA + SHA-256` over the same bytes produces and
/// what CryptoKit's `signature(for:)` over `Data` produces. Signing the digest
/// bytes instead would hash them a second time and verify against nothing.
///
/// ## Why not the Secure Enclave
///
/// The Enclave would fit on paper: `SecureEnclave.P256.Signing.PrivateKey` gives
/// a non-exportable key whose `publicKey.derRepresentation` is the SPKI this
/// registration needs, and whose signature `rawRepresentation` is the same
/// P1363 `r||s`. It is not used, for three reasons in ascending order of weight:
///
/// 1. It does not exist everywhere the client runs. `SecureEnclave.isAvailable`
///    is false on much of the Simulator estate, so an Enclave key means a second
///    code path — and a second code path in the one component whose failure mode
///    is "every post is silently rejected" is worse than the thing it protects
///    against.
/// 2. It cannot be exercised by `swift test`. The golden vectors and the Go
///    cross-verification both need a key a headless test can create and sign
///    with; an Enclave key needs an entitlement and a real device. A key that
///    cannot be tested here is a key whose signatures are proven nowhere.
/// 3. The thing it defends against is already lost. An attacker who can read
///    this app's Keychain also holds the session bearer token in the same
///    Keychain, and can therefore post as the user through `/events` without
///    ever touching the signing key. Non-exportability would buy
///    non-repudiation of *past* posts, which is real but small, at the cost of
///    (1) and (2).
///
/// The decision is isolated behind ``MalkuthKeyStore`` so an Enclave-backed
/// store can be added later without the canonical encoder or the envelope
/// knowing. What must not change with it is the wire format, which is why that
/// lives in ``WorkEnvelope`` and not here.
///
/// ## One key per PIAL, not per device
///
/// `fetchSigningPubkey` returns a single `public_key_b64` for a PIAL, so the key
/// authority stores one signing key per identity. A second device registering
/// its own key replaces the first, and works signed by the first stop verifying.
/// This is a property of the server's key model, not of this client; the client
/// mirrors `malkuth.js` and re-registers on every launch so that the key in the
/// authority is the key of the device actually posting.
public struct MalkuthKey: Sendable {

    /// The private key. Never leaves this struct; there is no accessor.
    private let privateKey: P256.Signing.PrivateKey

    public init(privateKey: P256.Signing.PrivateKey) {
        self.privateKey = privateKey
    }

    /// Creates a fresh key. Nothing about it is derived from the user, the
    /// device, or the PIAL — it is random, and its binding to an identity is the
    /// registration, not the key material.
    public static func generate() -> MalkuthKey {
        MalkuthKey(privateKey: P256.Signing.PrivateKey())
    }

    /// The public key as X.509 SubjectPublicKeyInfo DER, base64 with padding.
    ///
    /// This is the `public_key_b64` the registration sends, and what
    /// `x509.ParsePKIXPublicKey` on the far side expects. CryptoKit's
    /// `derRepresentation` for a `P256.Signing.PublicKey` is SPKI;
    /// `x963Representation` is the raw uncompressed point and would fail to
    /// parse. Standard base64 (not URL-safe): the Go side tries `StdEncoding`
    /// and falls back to `RawStdEncoding`, so padded standard base64 is the form
    /// both accept.
    public var publicKeySPKIBase64: String {
        privateKey.publicKey.derRepresentation.base64EncodedString()
    }

    /// Signs the UTF-8 bytes of `contentID`, returning unpadded base64url.
    ///
    /// Two details the server will not forgive:
    ///
    /// - **`rawRepresentation`, never `derRepresentation`.** The verifier checks
    ///   `len(sigBytes) != 64` and then splits it in half into `r` and `s`. That
    ///   is IEEE P1363, which is what `rawRepresentation` is. A DER signature is
    ///   70–72 bytes and fails the length check outright — which at least fails
    ///   loudly, unlike most of the traps here.
    /// - **base64url without padding.** The verifier decodes with
    ///   `base64.RawURLEncoding`, which is strict: `+` and `/` are not accepted,
    ///   and a trailing `=` is a decode error, not slack to be trimmed.
    public func signature(forContentID contentID: String) throws -> String {
        let signature = try privateKey.signature(for: Data(contentID.utf8))
        let raw = signature.rawRepresentation
        precondition(
            raw.count == 64,
            "P-256 P1363 signature must be r||s = 64 bytes, got \(raw.count)"
        )
        return Base64URL.encode(raw)
    }

    /// Verifies a signature this device made. Useful for a self-check on
    /// startup; it proves the key round-trips, and nothing about whether the
    /// server agrees — only the Go verifier can prove that, which is what
    /// `MalkuthCrossVerificationTests` exports vectors for.
    public func verify(_ signatureBase64URL: String, forContentID contentID: String) -> Bool {
        guard let raw = Base64URL.decode(signatureBase64URL), raw.count == 64,
              let signature = try? P256.Signing.ECDSASignature(rawRepresentation: raw)
        else { return false }
        return privateKey.publicKey.isValidSignature(signature, for: Data(contentID.utf8))
    }

    /// The private key as raw scalar bytes, for a store to persist. Internal:
    /// only ``MalkuthKeyStore`` implementations have a reason to see it.
    var rawPrivateKey: Data { privateKey.rawRepresentation }

    init(rawPrivateKey: Data) throws(MalkuthError) {
        guard let key = try? P256.Signing.PrivateKey(rawRepresentation: rawPrivateKey) else {
            throw .keyMaterialCorrupt
        }
        self.privateKey = key
    }
}

// MARK: - Storage

/// Where a device's signing key lives between launches.
///
/// A protocol so the Keychain is not compiled into the signing path: tests use
/// ``InMemoryMalkuthKeyStore``, and a Secure Enclave implementation could be
/// added without the rest of Malkuth changing.
public protocol MalkuthKeyStore: Sendable {
    /// Returns the stored key, or nil when this device has never made one.
    func load() throws -> MalkuthKey?
    func save(_ key: MalkuthKey) throws
    func delete() throws
}

public extension MalkuthKeyStore {
    /// The key for this device, creating and storing one on first use.
    ///
    /// Idempotent: two callers racing produce one key, because the second finds
    /// what the first stored. Not synchronised beyond that — callers hold this
    /// behind ``MalkuthSigner``, which is an actor.
    func loadOrCreate() throws -> MalkuthKey {
        if let existing = try load() { return existing }
        let key = MalkuthKey.generate()
        try save(key)
        return key
    }
}

/// Keychain-backed key storage, following `KeychainStore`'s attributes exactly.
///
/// `kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly` for the same two reasons
/// the session token uses it: the app may need to act before the user has
/// unlocked the device this boot, and `ThisDeviceOnly` keeps the key out of
/// iCloud Keychain — a signing key restored onto a second device would be a
/// second device able to sign as this identity, which is exactly what the
/// identity spine exists to prevent.
///
/// The item is stored under its own account name, so it is independent of the
/// session token: signing out clears the token and leaves the key, which is
/// correct — the key is the device's, not the session's.
public struct KeychainMalkuthKeyStore: MalkuthKeyStore {
    private let service: String
    private let account: String

    public init(
        service: String = "com.f33d3r.ios",
        account: String = "malkuth-signing-key"
    ) {
        self.service = service
        self.account = account
    }

    private var baseQuery: [String: Any] {
        [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: account,
        ]
    }

    public func load() throws -> MalkuthKey? {
        var query = baseQuery
        query[kSecReturnData as String] = true
        query[kSecMatchLimit as String] = kSecMatchLimitOne

        var item: CFTypeRef?
        let status = SecItemCopyMatching(query as CFDictionary, &item)
        if status == errSecItemNotFound { return nil }
        guard status == errSecSuccess else { throw MalkuthError.keychain(status) }
        guard let data = item as? Data else { throw MalkuthError.keyMaterialCorrupt }
        return try MalkuthKey(rawPrivateKey: data)
    }

    public func save(_ key: MalkuthKey) throws {
        // Delete-then-add, as KeychainStore does: one path for "first key" and
        // "key replaced", and the accessibility attribute is reapplied rather
        // than inherited from whatever wrote the previous item.
        SecItemDelete(baseQuery as CFDictionary)

        var query = baseQuery
        query[kSecValueData as String] = key.rawPrivateKey
        query[kSecAttrAccessible as String] = kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly

        let status = SecItemAdd(query as CFDictionary, nil)
        guard status == errSecSuccess else { throw MalkuthError.keychain(status) }
    }

    public func delete() throws {
        let status = SecItemDelete(baseQuery as CFDictionary)
        guard status == errSecSuccess || status == errSecItemNotFound else {
            throw MalkuthError.keychain(status)
        }
    }
}

/// A key store with no Keychain, for tests and for `swift test` on a machine
/// with no signing entitlement.
public final class InMemoryMalkuthKeyStore: MalkuthKeyStore, @unchecked Sendable {
    private let lock = NSLock()
    private var key: MalkuthKey?

    public init(key: MalkuthKey? = nil) { self.key = key }

    public func load() throws -> MalkuthKey? {
        lock.lock(); defer { lock.unlock() }
        return key
    }

    public func save(_ key: MalkuthKey) throws {
        lock.lock(); defer { lock.unlock() }
        self.key = key
    }

    public func delete() throws {
        lock.lock(); defer { lock.unlock() }
        key = nil
    }
}

// MARK: - base64url

/// base64url as `base64.RawURLEncoding` defines it: `-` and `_` for the last two
/// characters, and **no padding**.
///
/// Written out rather than reached for because the Go decoder is `Raw`: it
/// treats a trailing `=` as corrupt input, so `base64EncodedString()` on its own
/// produces a signature that fails to decode before it ever fails to verify.
public enum Base64URL {
    public static func encode(_ data: Data) -> String {
        data.base64EncodedString()
            .replacingOccurrences(of: "+", with: "-")
            .replacingOccurrences(of: "/", with: "_")
            .replacingOccurrences(of: "=", with: "")
    }

    public static func decode(_ string: String) -> Data? {
        var padded = string
            .replacingOccurrences(of: "-", with: "+")
            .replacingOccurrences(of: "_", with: "/")
        let remainder = padded.count % 4
        if remainder > 0 { padded += String(repeating: "=", count: 4 - remainder) }
        return Data(base64Encoded: padded)
    }
}
