import Foundation
import CryptoKit
import Testing
@testable import F33D3RKit

/// The fixture `Tools/sealcore-golden` writes, decoded with the field names the
/// wire uses.
struct SealCoreGoldenFixture: Decodable {
    struct Argon2Vector: Decodable {
        let name: String
        /// `go`, `rfc9106` or `argon2-0.5.3-kat` — which implementation or
        /// document is being trusted for this row.
        let source: String
        let variant: String
        let version: Int
        let timeCost: Int
        let memoryKiB: Int
        let lanes: Int
        let passwordB64: String
        let saltB64: String
        let secretB64: String
        let adB64: String
        let tagHex: String

        enum CodingKeys: String, CodingKey {
            case name, source, variant, version
            case timeCost = "t"
            case memoryKiB = "m_kib"
            case lanes = "p"
            case passwordB64 = "password_b64"
            case saltB64 = "salt_b64"
            case secretB64 = "secret_b64"
            case adB64 = "ad_b64"
            case tagHex = "tag_hex"
        }
    }

    struct HKDFVector: Decodable {
        let name: String
        let ikmB64: String
        let saltB64: String
        let info: String
        let okmB64: String

        enum CodingKeys: String, CodingKey {
            case name, info
            case ikmB64 = "ikm_b64"
            case saltB64 = "salt_b64"
            case okmB64 = "okm_b64"
        }
    }

    struct AgreementVector: Decodable {
        let name: String
        let privB64: String
        let pubB64: String
        let peerPubB64: String
        let sharedB64: String
        let wrapKeyB64: String

        enum CodingKeys: String, CodingKey {
            case name
            case privB64 = "priv_b64"
            case pubB64 = "pub_b64"
            case peerPubB64 = "peer_pub_b64"
            case sharedB64 = "shared_b64"
            case wrapKeyB64 = "wrap_key_b64"
        }
    }

    struct Recipient: Decodable {
        let account: String
        let privB64: String
        let pubB64: String

        enum CodingKeys: String, CodingKey {
            case account
            case privB64 = "priv_b64"
            case pubB64 = "pub_b64"
        }
    }

    struct SealedCase: Decodable {
        let name: String
        let plaintext: String
        let recipients: [Recipient]
        let envelope: SealCore.Envelope
    }

    struct Vault: Decodable {
        let handle: String
        let password: String
        let saltB64: String
        let derivedKeyB64: String
        let identityPrivB64: String
        let identityPubB64: String
        let wrappedCtB64: String
        let wrappedNonceB64: String

        enum CodingKeys: String, CodingKey {
            case handle, password
            case saltB64 = "salt_b64"
            case derivedKeyB64 = "derived_key_b64"
            case identityPrivB64 = "identity_priv_b64"
            case identityPubB64 = "identity_pub_b64"
            case wrappedCtB64 = "wrapped_ct_b64"
            case wrappedNonceB64 = "wrapped_nonce_b64"
        }
    }

    /// What Go answered when it was handed envelopes Swift had sealed. Recorded
    /// rather than re-run: `swift test` has no Go.
    struct FromSwift: Decodable {
        struct Verdict: Decodable {
            let name: String
            let shouldOpen: Bool
            let openedByGo: Bool

            enum CodingKeys: String, CodingKey {
                case name
                case shouldOpen = "should_open"
                case openedByGo = "opened_by_go"
            }
        }
        let goVersion: String
        let verdicts: [Verdict]
        let vaultVerified: Bool
        let vaultNote: String

        enum CodingKeys: String, CodingKey {
            case goVersion = "go_version"
            case verdicts
            case vaultVerified = "vault_verified"
            case vaultNote = "vault_note"
        }
    }

    let note: String
    let generatedBy: String
    let argon2: [Argon2Vector]
    let hkdf: [HKDFVector]
    let agreement: [AgreementVector]
    let sealed: [SealedCase]
    let vault: Vault
    let fromSwift: FromSwift?

    enum CodingKeys: String, CodingKey {
        case note, argon2, hkdf, agreement, sealed, vault
        case generatedBy = "generated_by"
        case fromSwift = "from_swift"
    }

    static func load() throws -> SealCoreGoldenFixture {
        try JSONDecoder().decode(
            SealCoreGoldenFixture.self, from: try ContractTests.fixture("sealcore_golden")
        )
    }
}

/// Proves the Swift port of `sealcore` produces and consumes exactly what the
/// browser does.
///
/// A round trip inside one implementation is worth nothing here. Put the GCM tag
/// in the wrong place, salt the HKDF wrong, or wrap the base64 text of the
/// private key instead of its bytes, and every seal-then-open test in this file
/// still passes — while not one message written by the web client can be read.
/// So the load-bearing cases are the ones driven by
/// `Fixtures/sealcore_golden.json`, sealed by a Go implementation of the same
/// scheme in `Tools/sealcore-golden`, which had never seen this Swift.
struct SealCoreTests {

    // MARK: HKDF — the "no salt" question

    @Test("Hkdf::new(None, ikm) means a HashLen block of zeros, and CryptoKit agrees")
    func hkdfAbsentSaltIsZeroBytes() throws {
        let ikm = SymmetricKey(data: Data("shared secret bytes, exactly 32.".utf8))

        // What `SealCore` does: the RFC's definition, written out.
        let explicit = HKDF<SHA256>.deriveKey(
            inputKeyMaterial: ikm,
            salt: Data(repeating: 0, count: 32),
            info: SealCore.hkdfInfo,
            outputByteCount: 32
        )
        // CryptoKit's no-salt overload, which is what a port would reach for
        // first. HMAC zero-pads any key shorter than its 64-byte block, so an
        // empty salt and 32 zero bytes are the same HMAC key — but that is an
        // argument, and this is a check.
        let noSaltOverload = HKDF<SHA256>.deriveKey(
            inputKeyMaterial: ikm, info: SealCore.hkdfInfo, outputByteCount: 32
        )
        #expect(explicit == noSaltOverload)

        // And a salt that is genuinely different must not collide, or the
        // assertion above would be vacuous.
        let different = HKDF<SHA256>.deriveKey(
            inputKeyMaterial: ikm,
            salt: Data(repeating: 1, count: 32),
            info: SealCore.hkdfInfo,
            outputByteCount: 32
        )
        #expect(explicit != different)
    }

    @Test("Go's HKDF over the same input produces the same wrap key")
    func hkdfMatchesGo() throws {
        let fixture = try SealCoreGoldenFixture.load()
        #expect(fixture.hkdf.count == 2)

        for vector in fixture.hkdf {
            let ikm = try #require(Data(base64Encoded: vector.ikmB64))
            let salt = Data(base64Encoded: vector.saltB64) ?? Data()
            let expected = try #require(Data(base64Encoded: vector.okmB64))
            let derived = HKDF<SHA256>.deriveKey(
                inputKeyMaterial: SymmetricKey(data: ikm),
                salt: salt,
                info: Data(vector.info.utf8),
                outputByteCount: 32
            )
            #expect(derived.withUnsafeBytes { Data($0) } == expected, "\(vector.name)")
        }

        // The info string is part of the contract, not a comment.
        #expect(fixture.hkdf.allSatisfy { $0.info == "sealcore-v1-x25519-wrap" })
        #expect(String(decoding: SealCore.hkdfInfo, as: UTF8.self) == "sealcore-v1-x25519-wrap")
    }

    @Test("X25519 agreement and the wrap key derived from it match Go byte for byte")
    func agreementMatchesGo() throws {
        let fixture = try SealCoreGoldenFixture.load()
        #expect(!fixture.agreement.isEmpty)

        for vector in fixture.agreement {
            let privateKey = try SealCore.privateKey(fromBase64: vector.privB64, field: "priv")
            // The public half Go recorded must be the one this key derives, or
            // the two are clamping the scalar differently.
            #expect(privateKey.publicKey.rawRepresentation.base64EncodedString() == vector.pubB64)

            let peer = try SealCore.publicKey(fromBase64: vector.peerPubB64, field: "peer")
            let shared = try privateKey.sharedSecretFromKeyAgreement(with: peer)
            #expect(
                shared.withUnsafeBytes { Data($0) }
                    == (try #require(Data(base64Encoded: vector.sharedB64))),
                "\(vector.name): shared secret"
            )

            let wrapKey = try SealCore.wrapKey(privateKey: privateKey, peer: peer)
            #expect(
                wrapKey.withUnsafeBytes { Data($0) }
                    == (try #require(Data(base64Encoded: vector.wrapKeyB64))),
                "\(vector.name): wrap key"
            )
        }
    }

    // MARK: Go sealed it, Swift opens it

    @Test("Envelopes sealed by another implementation open here, for every recipient")
    func opensGoSealedEnvelopes() throws {
        let fixture = try SealCoreGoldenFixture.load()
        #expect(fixture.sealed.count >= 3)

        for testCase in fixture.sealed {
            for (index, recipient) in testCase.recipients.enumerated() {
                // By account, which is the path the app takes.
                let byAccount = try SealCore.open(
                    testCase.envelope,
                    for: recipient.account,
                    privateKeyBase64: recipient.privB64
                )
                #expect(byAccount == testCase.plaintext, "\(testCase.name)[\(index)]")

                // And by loose fields, which is the path a DOM-shaped payload
                // takes. Both must agree.
                let entry = testCase.envelope.sealed[index]
                let byFields = try SealCore.open(
                    privateKeyBase64: recipient.privB64,
                    ephemeralPublicKeyBase64: entry.ephemeralPublicKeyBase64,
                    sealedKeyBase64: entry.sealedBase64,
                    sealedNonceBase64: entry.sealedNonceBase64,
                    bodyCiphertextBase64: testCase.envelope.bodyCiphertextBase64,
                    bodyNonceBase64: testCase.envelope.bodyNonceBase64
                )
                #expect(byFields == testCase.plaintext, "\(testCase.name)[\(index)] by fields")
            }
        }
    }

    @Test("A recipient cannot open another recipient's entry in the same envelope")
    func cannotOpenSomeoneElsesEntry() throws {
        let fixture = try SealCoreGoldenFixture.load()
        let group = try #require(fixture.sealed.first { $0.recipients.count > 1 })

        // Recipient 0's key against recipient 1's sealed content key. The body
        // is identical for both, so this isolates the wrapping.
        #expect(throws: SealCoreError.decryptionFailed) {
            try SealCore.open(
                group.envelope,
                sealedKey: group.envelope.sealed[1],
                privateKeyBase64: group.recipients[0].privB64
            )
        }
    }

    @Test("The vault Go wrapped opens with a key derived here from the password alone")
    func opensGoVault() throws {
        let fixture = try SealCoreGoldenFixture.load()
        let vault = fixture.vault

        // The salt has to be derived, not read from the fixture: `saltFromHandle`
        // is client code on both sides, and if Swift lower-cased differently or
        // hashed the wrong bytes, the key would differ and nothing else would say so.
        let salt = SealCore.saltFromHandle(vault.handle)
        #expect(salt == vault.saltB64)

        let key = try SealCore.deriveBackupKey(code: vault.password, saltBase64: salt)
        #expect(key == vault.derivedKeyB64)

        let unwrapped = try SealCore.unwrap(
            keyBase64: key,
            ciphertextBase64: vault.wrappedCtB64,
            nonceBase64: vault.wrappedNonceB64
        )
        #expect(unwrapped == vault.identityPrivB64)

        // And the unwrapped scalar is the identity whose public half the
        // directory publishes — the thing the whole vault exists to protect.
        let identity = try SealCore.privateKey(fromBase64: unwrapped, field: "identity")
        #expect(identity.publicKey.rawRepresentation.base64EncodedString() == vault.identityPubB64)
    }

    @Test("Go read the envelopes Swift sealed")
    func goOpenedSwiftEnvelopes() throws {
        let fixture = try SealCoreGoldenFixture.load()
        // Recorded, because `swift test` cannot run Go. Regenerate with the
        // steps in Tools/sealcore-golden/README.md when the seal path changes.
        let fromSwift = try #require(
            fixture.fromSwift,
            "the fixture carries no Go verdicts for Swift-sealed envelopes"
        )
        #expect(!fromSwift.verdicts.isEmpty)
        for verdict in fromSwift.verdicts {
            #expect(verdict.openedByGo == verdict.shouldOpen, "\(verdict.name)")
        }
        // A cross-check where nothing can fail is not a cross-check.
        #expect(fromSwift.verdicts.contains { !$0.shouldOpen })
        #expect(fromSwift.vaultVerified, "\(fromSwift.vaultNote)")
    }

    // MARK: Round trips

    @Test("Seal then open, one recipient")
    func roundTripsSingleRecipient() throws {
        let recipient = SealCore.generateKeypair()
        let envelope = try SealCore.seal(
            "hello sealed",
            to: [.init(account: "B", publicKeyBase64: recipient.publicKeyBase64)]
        )
        let opened = try SealCore.open(
            envelope, for: "B", privateKeyBase64: recipient.privateKeyBase64
        )
        #expect(opened == "hello sealed")
    }

    @Test("Seal to several recipients; each opens with their own key and no one else's")
    func roundTripsGroup() throws {
        let keys = (0..<4).map { _ in SealCore.generateKeypair() }
        let accounts = ["alice", "bob", "carol", "dave"]
        let recipients = zip(accounts, keys).map {
            SealCore.Recipient(account: $0, publicKeyBase64: $1.publicKeyBase64)
        }

        let envelope = try SealCore.seal("group msg", to: recipients)
        #expect(envelope.sealed.count == 4)

        for (account, key) in zip(accounts, keys) {
            #expect(
                try SealCore.open(envelope, for: account, privateKeyBase64: key.privateKeyBase64)
                    == "group msg"
            )
        }

        // Every pairing that is not the right one must fail, and fail the same
        // way: an envelope that opened for the wrong reader would be silent.
        for (index, key) in keys.enumerated() {
            for (otherIndex, entry) in envelope.sealed.enumerated() where otherIndex != index {
                #expect(throws: SealCoreError.decryptionFailed) {
                    try SealCore.open(
                        envelope, sealedKey: entry, privateKeyBase64: key.privateKeyBase64
                    )
                }
            }
        }

        // One body, encrypted once, no matter how many recipients.
        #expect(Set(envelope.sealed.map(\.ephemeralPublicKeyBase64)).count == 4)
    }

    @Test("Nothing is reused between two seals of the same text")
    func reusesNothing() throws {
        let recipient = SealCore.generateKeypair()
        let to = [SealCore.Recipient(account: "B", publicKeyBase64: recipient.publicKeyBase64)]
        let first = try SealCore.seal("same words", to: to)
        let second = try SealCore.seal("same words", to: to)

        #expect(first.bodyCiphertextBase64 != second.bodyCiphertextBase64)
        #expect(first.bodyNonceBase64 != second.bodyNonceBase64)
        #expect(first.sealed[0].ephemeralPublicKeyBase64 != second.sealed[0].ephemeralPublicKeyBase64)
        #expect(first.sealed[0].sealedNonceBase64 != second.sealed[0].sealedNonceBase64)
    }

    @Test("A body of any shape survives: empty, emoji, a newline, something long")
    func roundTripsAwkwardBodies() throws {
        let recipient = SealCore.generateKeypair()
        let to = [SealCore.Recipient(account: "B", publicKeyBase64: recipient.publicKeyBase64)]
        let bodies = [
            "",
            "🔐 sealed — 三人",
            "line one\nline two\r\n\ttabbed",
            String(repeating: "long ", count: 4000),
        ]
        for body in bodies {
            let envelope = try SealCore.seal(body, to: to)
            #expect(
                try SealCore.open(envelope, for: "B", privateKeyBase64: recipient.privateKeyBase64)
                    == body
            )
        }
    }

    // MARK: Failing cleanly

    @Test("The wrong private key is an error, not a crash and not garbage")
    func wrongKeyFailsCleanly() throws {
        let recipient = SealCore.generateKeypair()
        let stranger = SealCore.generateKeypair()
        let envelope = try SealCore.seal(
            "secret",
            to: [.init(account: "B", publicKeyBase64: recipient.publicKeyBase64)]
        )

        #expect(throws: SealCoreError.decryptionFailed) {
            try SealCore.open(
                envelope, sealedKey: envelope.sealed[0],
                privateKeyBase64: stranger.privateKeyBase64
            )
        }
    }

    @Test("A tampered byte anywhere in the envelope is caught by the tag")
    func tamperingFails() throws {
        let recipient = SealCore.generateKeypair()
        let envelope = try SealCore.seal(
            "authenticated",
            to: [.init(account: "B", publicKeyBase64: recipient.publicKeyBase64)]
        )

        func flippingLastByte(of base64: String) throws -> String {
            var bytes = [UInt8](try #require(Data(base64Encoded: base64)))
            bytes[bytes.count - 1] ^= 0x01
            return Data(bytes).base64EncodedString()
        }

        // The body ciphertext, whose tag is at the end...
        let tamperedBody = SealCore.Envelope(
            bodyCiphertextBase64: try flippingLastByte(of: envelope.bodyCiphertextBase64),
            bodyNonceBase64: envelope.bodyNonceBase64,
            sealed: envelope.sealed
        )
        #expect(throws: SealCoreError.decryptionFailed) {
            try SealCore.open(
                tamperedBody, for: "B", privateKeyBase64: recipient.privateKeyBase64
            )
        }

        // ...the wrapped content key...
        let entry = envelope.sealed[0]
        let tamperedKey = SealCore.Envelope(
            bodyCiphertextBase64: envelope.bodyCiphertextBase64,
            bodyNonceBase64: envelope.bodyNonceBase64,
            sealed: [
                .init(
                    recipientAccount: entry.recipientAccount,
                    ephemeralPublicKeyBase64: entry.ephemeralPublicKeyBase64,
                    sealedBase64: try flippingLastByte(of: entry.sealedBase64),
                    sealedNonceBase64: entry.sealedNonceBase64
                )
            ]
        )
        #expect(throws: SealCoreError.decryptionFailed) {
            try SealCore.open(tamperedKey, for: "B", privateKeyBase64: recipient.privateKeyBase64)
        }

        // ...and the ephemeral public key, which changes the derived wrap key.
        let tamperedEphemeral = SealCore.Envelope(
            bodyCiphertextBase64: envelope.bodyCiphertextBase64,
            bodyNonceBase64: envelope.bodyNonceBase64,
            sealed: [
                .init(
                    recipientAccount: entry.recipientAccount,
                    ephemeralPublicKeyBase64: try flippingLastByte(of: entry.ephemeralPublicKeyBase64),
                    sealedBase64: entry.sealedBase64,
                    sealedNonceBase64: entry.sealedNonceBase64
                )
            ]
        )
        #expect(throws: SealCoreError.decryptionFailed) {
            try SealCore.open(tamperedEphemeral, for: "B", privateKeyBase64: recipient.privateKeyBase64)
        }
    }

    @Test("Malformed input is named, not swallowed")
    func malformedInputIsTyped() throws {
        let recipient = SealCore.generateKeypair()
        let envelope = try SealCore.seal(
            "x", to: [.init(account: "B", publicKeyBase64: recipient.publicKeyBase64)]
        )

        #expect(throws: SealCoreError.malformedBase64(field: "my priv")) {
            try SealCore.open(envelope, for: "B", privateKeyBase64: "not base64 at all!!")
        }
        #expect(throws: SealCoreError.badKeyLength(field: "my priv", count: 16)) {
            try SealCore.open(
                envelope, for: "B",
                privateKeyBase64: Data(repeating: 0, count: 16).base64EncodedString()
            )
        }
        #expect(throws: SealCoreError.notAddressedToAccount("someone-else")) {
            try SealCore.open(
                envelope, for: "someone-else", privateKeyBase64: recipient.privateKeyBase64
            )
        }
        #expect(throws: SealCoreError.noRecipients) {
            try SealCore.seal("x", to: [])
        }
        #expect(throws: SealCoreError.badKeyLength(field: "recipient pub", count: 31)) {
            try SealCore.seal(
                "x",
                to: [.init(
                    account: "B",
                    publicKeyBase64: Data(repeating: 7, count: 31).base64EncodedString()
                )]
            )
        }

        // A ciphertext shorter than the tag it is meant to end with: the wrong
        // answer here is an index crash while splitting.
        #expect(throws: SealCoreError.ciphertextTooShort(4)) {
            try SealCore.open(
                privateKeyBase64: recipient.privateKeyBase64,
                ephemeralPublicKeyBase64: envelope.sealed[0].ephemeralPublicKeyBase64,
                sealedKeyBase64: Data([1, 2, 3, 4]).base64EncodedString(),
                sealedNonceBase64: envelope.sealed[0].sealedNonceBase64,
                bodyCiphertextBase64: envelope.bodyCiphertextBase64,
                bodyNonceBase64: envelope.bodyNonceBase64
            )
        }
        // A nonce of the wrong length, which `aes_open` refuses before it
        // decrypts.
        #expect(throws: SealCoreError.badNonceLength(8)) {
            try SealCore.open(
                privateKeyBase64: recipient.privateKeyBase64,
                ephemeralPublicKeyBase64: envelope.sealed[0].ephemeralPublicKeyBase64,
                sealedKeyBase64: envelope.sealed[0].sealedBase64,
                sealedNonceBase64: Data(repeating: 0, count: 8).base64EncodedString(),
                bodyCiphertextBase64: envelope.bodyCiphertextBase64,
                bodyNonceBase64: envelope.bodyNonceBase64
            )
        }
    }

    // MARK: The vault

    @Test("Wrap then unwrap under a password-derived key")
    func vaultRoundTrips() throws {
        let identity = SealCore.generateKeypair()
        let salt = SealCore.saltFromHandle("MiiYazuko")
        let key = try SealCore.deriveBackupKey(code: "f33d3rdev", saltBase64: salt)

        let wrapped = try SealCore.wrap(keyBase64: key, plaintextBase64: identity.privateKeyBase64)
        // What the server stores is a blob and a nonce, nothing else.
        #expect(Data(base64Encoded: wrapped.ciphertextBase64)?.count == 32 + 16)
        #expect(Data(base64Encoded: wrapped.nonceBase64)?.count == 12)

        // Deriving again from the same password must produce the same key —
        // there is no stored salt to lose.
        let again = try SealCore.deriveBackupKey(code: "f33d3rdev", saltBase64: salt)
        #expect(again == key)

        let unwrapped = try SealCore.unwrap(
            keyBase64: again,
            ciphertextBase64: wrapped.ciphertextBase64,
            nonceBase64: wrapped.nonceBase64
        )
        #expect(unwrapped == identity.privateKeyBase64)
    }

    @Test("wrap_with_key seals the key's 32 bytes, never the 44 characters of its base64")
    func vaultWrapsBytesNotText() throws {
        let identity = SealCore.generateKeypair()
        let key = Data(repeating: 9, count: 32).base64EncodedString()
        let wrapped = try SealCore.wrap(keyBase64: key, plaintextBase64: identity.privateKeyBase64)

        // 32 bytes plus a 16-byte tag. Wrapping the text would give 44 + 16,
        // and would still round-trip perfectly through this same code.
        let ciphertext = try #require(Data(base64Encoded: wrapped.ciphertextBase64))
        #expect(ciphertext.count == 48)
        #expect(identity.privateKeyBase64.count == 44)
    }

    @Test("A vault key derived from the wrong password fails to unwrap, cleanly")
    func vaultRejectsWrongPassword() throws {
        let identity = SealCore.generateKeypair()
        let salt = SealCore.saltFromHandle("dev")
        let key = try SealCore.deriveBackupKey(code: "right", saltBase64: salt)
        let wrapped = try SealCore.wrap(keyBase64: key, plaintextBase64: identity.privateKeyBase64)

        let wrongKey = try SealCore.deriveBackupKey(code: "wrong", saltBase64: salt)
        #expect(throws: SealCoreError.decryptionFailed) {
            try SealCore.unwrap(
                keyBase64: wrongKey,
                ciphertextBase64: wrapped.ciphertextBase64,
                nonceBase64: wrapped.nonceBase64
            )
        }
    }

    @Test("The handle salt is the lower-cased handle's digest, and nothing else")
    func saltFromHandleMatchesTheBrowser() throws {
        // `saltFromHandle` in gnosis-seal.js: SHA-256 of the lower-cased handle,
        // base64. Case-folding is the part that matters — the same person
        // signing in as @MiiYazuko and @miiyazuko must reach the same vault.
        #expect(SealCore.saltFromHandle("MiiYazuko") == SealCore.saltFromHandle("miiyazuko"))

        let expected = Data(SHA256.hash(data: Data("miiyazuko".utf8))).base64EncodedString()
        #expect(SealCore.saltFromHandle("MiiYazuko") == expected)

        // And it is 32 bytes once decoded, which is what Argon2 sees.
        #expect(Data(base64Encoded: SealCore.saltFromHandle("dev"))?.count == 32)
    }

    // MARK: The wire

    @Test("An envelope decodes from the exact field names the server sends")
    func decodesServerJSON() throws {
        // Written out by hand rather than round-tripped through the encoder: a
        // key that is misspelled in both directions round-trips fine.
        let json = """
        {
          "body_ct_b64": "3q2+7w==",
          "body_nonce_b64": "AAAAAAAAAAAAAAAA",
          "sealed": [
            {
              "recipient_account": "acct-1",
              "eph_pub_b64": "ZXBo",
              "sealed_b64": "c2VhbGVk",
              "sealed_nonce_b64": "bm9uY2U="
            }
          ]
        }
        """
        let envelope = try JSONDecoder().decode(SealCore.Envelope.self, from: Data(json.utf8))
        #expect(envelope.bodyCiphertextBase64 == "3q2+7w==")
        #expect(envelope.bodyNonceBase64 == "AAAAAAAAAAAAAAAA")
        #expect(envelope.sealed.count == 1)
        #expect(envelope.sealed[0].recipientAccount == "acct-1")
        #expect(envelope.sealed[0].ephemeralPublicKeyBase64 == "ZXBo")
        #expect(envelope.sealed[0].sealedBase64 == "c2VhbGVk")
        #expect(envelope.sealed[0].sealedNonceBase64 == "bm9uY2U=")
        #expect(envelope.sealedKey(for: "acct-1") != nil)
        #expect(envelope.sealedKey(for: "nobody") == nil)

        // And re-encodes to those same names, because this is what
        // POST /api/gnosis/send-sealed reads.
        let encoded = try JSONSerialization.jsonObject(
            with: try JSONEncoder().encode(envelope)
        ) as? [String: Any]
        #expect(encoded?["body_ct_b64"] as? String == "3q2+7w==")
        #expect(encoded?["body_nonce_b64"] as? String == "AAAAAAAAAAAAAAAA")
    }

    @Test("A keypair, a recipient and a wrapped blob use the server's names too")
    func decodesTheOtherWireTypes() throws {
        let keypair = try JSONDecoder().decode(
            SealCore.Keypair.self,
            from: Data(#"{"priv_b64":"cHJpdg==","pub_b64":"cHVi"}"#.utf8)
        )
        #expect(keypair.privateKeyBase64 == "cHJpdg==")
        #expect(keypair.publicKeyBase64 == "cHVi")

        let recipient = try JSONDecoder().decode(
            SealCore.Recipient.self,
            from: Data(#"{"account":"acct-9","pub_b64":"cHVi"}"#.utf8)
        )
        #expect(recipient.account == "acct-9")
        #expect(recipient.publicKeyBase64 == "cHVi")

        let wrapped = try JSONDecoder().decode(
            SealCore.Wrapped.self,
            from: Data(#"{"ct_b64":"Y3Q=","nonce_b64":"bm9uY2U="}"#.utf8)
        )
        #expect(wrapped.ciphertextBase64 == "Y3Q=")
        #expect(wrapped.nonceBase64 == "bm9uY2U=")
    }

    @Test("Keys go on the wire as raw 32 bytes in padded standard base64, not SPKI")
    func keysAreRawBytes() throws {
        let keypair = SealCore.generateKeypair()
        // 32 bytes is 44 base64 characters with one '=' of padding. An SPKI DER
        // X25519 public key would be 44 *bytes* and encode to 60 characters —
        // which is what ``MalkuthKey`` sends, for a different key, on a
        // different endpoint.
        #expect(keypair.publicKeyBase64.count == 44)
        #expect(keypair.publicKeyBase64.hasSuffix("="))
        #expect(Data(base64Encoded: keypair.publicKeyBase64)?.count == 32)
        #expect(Data(base64Encoded: keypair.privateKeyBase64)?.count == 32)
        // Standard alphabet, not URL-safe: '-' and '_' never appear.
        #expect(!keypair.publicKeyBase64.contains("-"))
        #expect(!keypair.publicKeyBase64.contains("_"))
    }

    @Test("Base64 with surrounding whitespace is accepted, as the Rust b64d trims")
    func toleratesWhitespaceInBase64() throws {
        let recipient = SealCore.generateKeypair()
        let envelope = try SealCore.seal(
            "trimmed",
            to: [.init(account: "B", publicKeyBase64: "  " + recipient.publicKeyBase64 + "\n")]
        )
        #expect(
            try SealCore.open(
                envelope, for: "B", privateKeyBase64: "\n" + recipient.privateKeyBase64 + " "
            ) == "trimmed"
        )
    }

    @Test("Nonces are 12 bytes and ciphertext carries its tag appended, as aes-gcm returns it")
    func wireLayoutMatchesTheRustCrate() throws {
        let recipient = SealCore.generateKeypair()
        let body = "twelve chars"
        let envelope = try SealCore.seal(
            body, to: [.init(account: "B", publicKeyBase64: recipient.publicKeyBase64)]
        )

        #expect(Data(base64Encoded: envelope.bodyNonceBase64)?.count == 12)
        #expect(Data(base64Encoded: envelope.sealed[0].sealedNonceBase64)?.count == 12)

        // Body ciphertext is exactly the plaintext length plus a 16-byte tag —
        // no nonce prefix. `AES.GCM.SealedBox.combined` would be 12 longer, and
        // would decrypt to nothing on the far side.
        let ciphertext = try #require(Data(base64Encoded: envelope.bodyCiphertextBase64))
        #expect(ciphertext.count == body.utf8.count + 16)
        // The wrapped content key: 32 bytes plus the tag.
        #expect(Data(base64Encoded: envelope.sealed[0].sealedBase64)?.count == 48)
    }

    // MARK: Export, for the Go half of the cross-check

    /// Seals with Swift and writes the result for `sealcore-golden verify`.
    ///
    /// Not a test — the second half of it runs in Go. Enabled by
    /// `SEALCORE_EXPORT_DIR`; the recorded verdicts live in the fixture's
    /// `from_swift`.
    @Test(
        "Export Swift-sealed envelopes for the Go opener",
        .enabled(if: ProcessInfo.processInfo.environment["SEALCORE_EXPORT_DIR"] != nil)
    )
    func exportEnvelopesForGo() throws {
        let directory = try #require(ProcessInfo.processInfo.environment["SEALCORE_EXPORT_DIR"])

        var cases: [[String: Any]] = []

        func record(
            _ name: String, plaintext: String, recipients: [SealCore.Keypair],
            accounts: [String], opener: Int, shouldOpen: Bool, entry: Int? = nil
        ) throws {
            let envelope = try SealCore.seal(
                plaintext,
                to: zip(accounts, recipients).map {
                    .init(account: $0, publicKeyBase64: $1.publicKeyBase64)
                }
            )
            let index = entry ?? opener
            cases.append([
                "name": name,
                "plaintext": plaintext,
                "should_open": shouldOpen,
                "recipient_priv_b64": recipients[opener].privateKeyBase64,
                "sealed": [
                    "recipient_account": envelope.sealed[index].recipientAccount,
                    "eph_pub_b64": envelope.sealed[index].ephemeralPublicKeyBase64,
                    "sealed_b64": envelope.sealed[index].sealedBase64,
                    "sealed_nonce_b64": envelope.sealed[index].sealedNonceBase64,
                ],
                "envelope": [
                    "body_ct_b64": envelope.bodyCiphertextBase64,
                    "body_nonce_b64": envelope.bodyNonceBase64,
                    "sealed": [],
                ],
            ])
        }

        let one = SealCore.generateKeypair()
        try record("swift_single", plaintext: "hello from swift", recipients: [one],
                   accounts: ["B"], opener: 0, shouldOpen: true)

        let group = (0..<3).map { _ in SealCore.generateKeypair() }
        let accounts = ["alice", "bob", "carol"]
        for index in 0..<3 {
            try record("swift_group_\(accounts[index])", plaintext: "群 — sealed 🔐",
                       recipients: group, accounts: accounts, opener: index, shouldOpen: true)
        }
        try record("swift_empty_body", plaintext: "", recipients: [one], accounts: ["B"],
                   opener: 0, shouldOpen: true)
        // Negative: hand Go recipient 0's key with recipient 1's entry. A Go
        // that opens this is a Go that is not checking the tag.
        try record("swift_wrong_recipient_entry", plaintext: "not for you",
                   recipients: group, accounts: accounts, opener: 0, shouldOpen: false, entry: 1)

        // The vault, in the direction Go can check: Swift derives and wraps.
        let identity = SealCore.generateKeypair()
        let handle = "MiiYazuko"
        let password = "f33d3rdev"
        let key = try SealCore.deriveBackupKey(
            code: password, saltBase64: SealCore.saltFromHandle(handle)
        )
        let wrapped = try SealCore.wrap(keyBase64: key, plaintextBase64: identity.privateKeyBase64)

        let payload: [String: Any] = [
            "cases": cases,
            "vault": [
                "handle": handle,
                "password": password,
                "derived_key_b64": key,
                "identity_priv_b64": identity.privateKeyBase64,
                "wrapped_ct_b64": wrapped.ciphertextBase64,
                "wrapped_nonce_b64": wrapped.nonceBase64,
            ],
        ]
        let url = URL(fileURLWithPath: directory)
            .appendingPathComponent("envelopes_from_swift.json")
        try JSONSerialization.data(withJSONObject: payload, options: [.prettyPrinted, .sortedKeys])
            .write(to: url)
        print("[sealcore] wrote \(cases.count) envelopes to \(url.path)")
    }
}
