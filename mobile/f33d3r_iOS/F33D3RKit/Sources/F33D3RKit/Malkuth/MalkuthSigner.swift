import Foundation

/// Holds this device's signing keys, one per signed-in account, and turns a
/// ``WorkPayload`` into a signed envelope.
///
/// An actor because keys are loaded lazily from the Keychain and two compose
/// screens signing at once must not each generate a key and each register it —
/// the authority holds one key per identity, so a race there ends with works
/// signed by a key that is no longer registered.
///
/// The key a payload is signed with is the key of its `author_account`, so a
/// device holding two accounts never signs one account's work with the
/// other's key.
public actor MalkuthSigner {

    private let storeFor: @Sendable (String) -> any MalkuthKeyStore
    private var cached: [String: MalkuthKey] = [:]
    /// Accounts whose key registration has succeeded this launch, so a compose
    /// after sign-in does not re-register on every post.
    private var registered: Set<String> = []

    /// `storeFor` names where an account's key lives; the default is the
    /// Keychain item named by the account id.
    public init(storeFor: @escaping @Sendable (String) -> any MalkuthKeyStore = {
        KeychainMalkuthKeyStore(accountID: $0)
    }) {
        self.storeFor = storeFor
    }

    /// One store for every account: for tests.
    public init(store: any MalkuthKeyStore) {
        self.storeFor = { _ in store }
    }

    /// The account's key on this device, created on first use.
    public func key(for accountID: String) throws -> MalkuthKey {
        if let key = cached[accountID] { return key }
        let key = try storeFor(accountID).loadOrCreate()
        cached[accountID] = key
        return key
    }

    /// The SPKI base64 to register for the account.
    public func publicKeySPKIBase64(for accountID: String) throws -> String {
        try key(for: accountID).publicKeySPKIBase64
    }

    /// Registers the account's public key with the authority unless it already
    /// succeeded this launch.
    ///
    /// `force` re-registers regardless, which is what every sign-in does: the
    /// most likely reason a signature stops verifying is that another device
    /// registered over this one's key, and re-registering is the fix.
    ///
    /// Throws ``MalkuthError/keyAuthorityUnavailable`` when the key authority is
    /// not reachable. It must never be swallowed, because a client that quietly
    /// proceeds produces works that are all refused.
    @discardableResult
    public func registerIfNeeded(with client: APIClient, accountID: String, force: Bool = false) async throws -> Bool {
        if registered.contains(accountID) && !force { return false }
        try await client.registerSigningKey(publicKeySPKIBase64: try key(for: accountID).publicKeySPKIBase64)
        registered.insert(accountID)
        return true
    }

    /// Signs `payload` with its author account's key.
    ///
    /// Validation happens inside ``WorkEnvelope/init(payload:key:eventType:isNSFW:videoWidth:videoHeight:quotedWorkID:reactLayout:downloadsAllowed:)``
    /// before anything is hashed, so a rejected payload costs no request.
    public func sign(
        _ payload: WorkPayload,
        eventType: String? = nil,
        isNSFW: Bool = false,
        videoWidth: Int? = nil,
        videoHeight: Int? = nil,
        quotedWorkID: String? = nil,
        reactLayout: String? = nil,
        downloadsAllowed: Bool? = nil
    ) throws -> WorkEnvelope {
        guard !payload.authorAccount.isEmpty else { throw MalkuthError.accountUnknown }
        return try WorkEnvelope(
            payload: payload,
            key: try key(for: payload.authorAccount),
            eventType: eventType,
            isNSFW: isNSFW,
            videoWidth: videoWidth,
            videoHeight: videoHeight,
            quotedWorkID: quotedWorkID,
            reactLayout: reactLayout,
            downloadsAllowed: downloadsAllowed
        )
    }

    /// Forgets the account's key on this device.
    ///
    /// Not called on sign-out: the key belongs to the device, and a user signing
    /// back in should keep signing with the key the authority already holds.
    /// This is for "reset this device's identity" and for tests.
    public func destroyKey(for accountID: String) throws {
        try storeFor(accountID).delete()
        cached[accountID] = nil
        registered.remove(accountID)
    }
}
