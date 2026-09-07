import Foundation

/// Custody of this account's messaging key.
///
/// The private key is generated on a device and the server only ever holds it
/// wrapped, under a key derived from the password at sign-in. So the one moment
/// the key can be unwrapped is the moment somebody signs in, and everything
/// here follows from that:
///
/// - ``unlock(handle:password:)`` is called from the sign-in path and nowhere
///   else. It derives the wrapping key, fetches the wrapped blob, and either
///   opens it or — on a first run, or after a password reset made the old
///   wrapping key wrong — makes a fresh identity and files it.
/// - The unwrapped key is then kept in the Keychain, so relaunching the app does
///   not require signing in again. It is per-account, because each persona is
///   its own sealed island and they deliberately do not share message keys.
/// - ``lock()`` on sign-out removes it.
///
/// A device that has never unlocked can still read plain conversations and send
/// in them. It simply cannot open sealed ones, and says so rather than showing
/// an empty thread.
public actor GnosisVault: SealedMessageOpener {

    /// Why an unlock did not happen. Reported so a sign-in can carry on: not
    /// being able to read sealed messages is a reduced app, not a failed login.
    public enum Outcome: Sendable, Equatable {
        /// The key is on this device and sealed messages will open.
        case unlocked
        /// A new identity was made and filed. Messages sealed to the previous
        /// key, if there was one, cannot be opened by anybody any more.
        case provisioned
        /// The key store could not be reached. Nothing was changed.
        case unavailable(String)
    }

    private let client: APIClient
    private let keychain: KeychainStore
    private let pins: any SealPinStore

    /// The unwrapped private key, base64. Read from the Keychain on first use.
    private var privateKey: String?
    private var didLoadFromKeychain = false

    public init(
        client: APIClient,
        account: String,
        keychain: KeychainStore? = nil,
        pins: (any SealPinStore)? = nil
    ) {
        self.client = client
        // Pins belong to the signed-in account for the same reason the key
        // does: what one persona has verified says nothing about another.
        self.pins = pins ?? DefaultsSealPinStore(account: account)
        // One Keychain entry per account, under the app's existing service, so
        // two personas signed in on one phone never share a message key.
        self.keychain = keychain ?? KeychainStore(account: "gnosis-key-" + account)
    }

    // MARK: Locking and unlocking

    public var isUnlocked: Bool {
        get async { loadedPrivateKey() != nil }
    }

    /// Opens this account's messaging identity, or makes one.
    ///
    /// Safe to call on every sign-in. When the key is already on the device this
    /// does nothing but confirm it.
    @discardableResult
    public func unlock(handle: String, password: String) async -> Outcome {
        if loadedPrivateKey() != nil { return .unlocked }

        let wrappingKey: String
        do {
            wrappingKey = try SealCore.deriveBackupKey(
                code: password, saltBase64: SealCore.saltFromHandle(handle)
            )
        } catch {
            return .unavailable("The encryption key could not be derived.")
        }

        let blob: GnosisIdentityBlob
        do {
            blob = try await client.gnosisIdentity()
        } catch let error as MessagingError {
            return .unavailable(error.userMessage)
        } catch {
            return .unavailable("The key store could not be reached.")
        }

        if blob.isUnwrappable,
           let opened = try? SealCore.unwrap(
               keyBase64: wrappingKey,
               ciphertextBase64: blob.wrappedPriv,
               nonceBase64: blob.wrapNonce
           ) {
            store(opened)
            return .unlocked
        }

        // Either there was no identity, or the wrapping key no longer opens the
        // one on file — which is what a password changed through account
        // recovery looks like. Both are answered the same way, by filing a new
        // identity. The server records the replacement as a security event,
        // because a key that changes quietly is what an interception looks like.
        let keypair = SealCore.generateKeypair()
        let wrapped: SealCore.Wrapped
        do {
            wrapped = try SealCore.wrap(
                keyBase64: wrappingKey, plaintextBase64: keypair.privateKeyBase64
            )
        } catch {
            return .unavailable("A new encryption key could not be made.")
        }
        do {
            try await client.provisionGnosisIdentity(
                pubB64: keypair.publicKeyBase64,
                wrappedPriv: wrapped.ciphertextBase64,
                wrapNonce: wrapped.nonceBase64
            )
        } catch let error as MessagingError {
            return .unavailable(error.userMessage)
        } catch {
            return .unavailable("The new key could not be filed.")
        }
        store(keypair.privateKeyBase64)
        return .provisioned
    }

    /// Forgets the key. Called on sign-out.
    public func lock() {
        privateKey = nil
        didLoadFromKeychain = true
        try? keychain.delete()
    }

    // MARK: Reading and writing sealed messages

    public func open(_ message: ChatMessage) async -> String? {
        guard message.isSealed, message.isOpenable, let key = loadedPrivateKey() else { return nil }
        return try? SealCore.open(
            privateKeyBase64: key,
            ephemeralPublicKeyBase64: message.ephPubB64,
            sealedKeyBase64: message.sealedB64,
            sealedNonceBase64: message.sealedNonceB64,
            bodyCiphertextBase64: message.bodyCtB64,
            bodyNonceBase64: message.bodyNonceB64
        )
    }

    public func seal(_ text: String, conversation: String) async throws -> SealedEnvelope {
        guard loadedPrivateKey() != nil else { throw MessagingError.sealedConversation }

        let directory = try await client.gnosisDirectory(conversation: conversation)
        let recipients = directory
            .filter { !$0.pubB64.isEmpty }
            .map { SealCore.Recipient(account: $0.account, publicKeyBase64: $0.pubB64) }
        guard !recipients.isEmpty else { throw MessagingError.noRecipients }

        // Trust on first use. This scheme has no forward secrecy, so a key that
        // changes under us is the one thing worth stopping for: it is a new
        // device, or it is somebody standing in the middle. The change is
        // reported rather than swallowed, and the new key is not trusted until
        // it is answered.
        let changes = pins.check(recipients)
        guard changes.isEmpty else { throw MessagingError.keyChanged(changes.map(\.account)) }

        let envelope = try SealCore.seal(text, to: recipients)
        return SealedEnvelope(
            bodyCtB64: envelope.bodyCiphertextBase64,
            bodyNonceB64: envelope.bodyNonceBase64,
            sealed: envelope.sealed.map {
                SealedEnvelope.SealedKey(
                    recipientAccount: $0.recipientAccount,
                    ephPubB64: $0.ephemeralPublicKeyBase64,
                    sealedB64: $0.sealedBase64,
                    sealedNonceB64: $0.sealedNonceBase64
                )
            }
        )
    }

    /// Trusts an account's new key, after the reader has said to.
    public func acceptKeyChange(for accounts: [String], in conversation: String) async {
        guard let directory = try? await client.gnosisDirectory(conversation: conversation) else { return }
        for entry in directory where accounts.contains(entry.account) && !entry.pubB64.isEmpty {
            pins.pin(entry.pubB64, for: entry.account)
        }
    }

    // MARK: Keychain

    private func loadedPrivateKey() -> String? {
        if let privateKey { return privateKey }
        guard !didLoadFromKeychain else { return nil }
        didLoadFromKeychain = true
        privateKey = try? keychain.get()
        return privateKey
    }

    private func store(_ key: String) {
        privateKey = key
        didLoadFromKeychain = true
        // A Keychain that refuses is not a reason to fail the unlock: the key
        // is good for this run, and the next sign-in will derive it again.
        try? keychain.set(key)
    }
}
