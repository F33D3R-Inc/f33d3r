import Foundation

/// Holds this device's signing key and turns a ``WorkPayload`` into a signed
/// envelope.
///
/// An actor because the key is loaded lazily from the Keychain and two compose
/// screens signing at once must not each generate a key and each register it —
/// the authority holds one key per PIAL, so a race there ends with works signed
/// by a key that is no longer registered.
public actor MalkuthSigner {

    private let store: any MalkuthKeyStore
    private var cached: MalkuthKey?
    /// Set once registration has succeeded this launch, so a compose that
    /// happens after sign-in does not re-register on every post.
    private var registered = false

    public init(store: any MalkuthKeyStore = KeychainMalkuthKeyStore()) {
        self.store = store
    }

    /// This device's key, created on first use.
    public func key() throws -> MalkuthKey {
        if let cached { return cached }
        let key = try store.loadOrCreate()
        cached = key
        return key
    }

    /// The SPKI base64 to register.
    public func publicKeySPKIBase64() throws -> String {
        try key().publicKeySPKIBase64
    }

    /// Registers the public key with the authority unless it already succeeded
    /// this launch.
    ///
    /// `force` re-registers regardless, which is what a `403 signature invalid`
    /// warrants: the most likely cause is that another device registered over
    /// this one's key, and re-registering is the fix.
    ///
    /// Throws ``MalkuthError/keyAuthorityUnavailable`` when Elohim Veni is not
    /// reachable. That is expected in a local deployment and callers should
    /// treat it as "signing will not verify yet", not as a bug — but it must
    /// never be swallowed, because a client that quietly proceeds produces
    /// works that are all rejected with the same opaque 403.
    @discardableResult
    public func registerIfNeeded(with client: APIClient, force: Bool = false) async throws -> Bool {
        if registered && !force { return false }
        try await client.registerSigningKey(publicKeySPKIBase64: try key().publicKeySPKIBase64)
        registered = true
        return true
    }

    /// Signs `payload`.
    ///
    /// Validation happens inside ``WorkEnvelope/init(payload:key:eventType:isNSFW:videoWatermarkedURL:videoWidth:videoHeight:quotedWorkID:reactLayout:tusUploadID:)``
    /// before anything is hashed, so a rejected payload costs no key access and
    /// no request.
    public func sign(
        _ payload: WorkPayload,
        eventType: String? = nil,
        isNSFW: Bool = false,
        videoWatermarkedURL: String? = nil,
        videoWidth: Int? = nil,
        videoHeight: Int? = nil,
        quotedWorkID: String? = nil,
        reactLayout: String? = nil,
        tusUploadID: String? = nil
    ) throws -> WorkEnvelope {
        try WorkEnvelope(
            payload: payload,
            key: try key(),
            eventType: eventType,
            isNSFW: isNSFW,
            videoWatermarkedURL: videoWatermarkedURL,
            videoWidth: videoWidth,
            videoHeight: videoHeight,
            quotedWorkID: quotedWorkID,
            reactLayout: reactLayout,
            tusUploadID: tusUploadID
        )
    }

    /// Forgets the key on this device.
    ///
    /// Not called on sign-out: the key belongs to the device, and a user signing
    /// back in should keep signing with the key the authority already holds.
    /// This is for "reset this device's identity" and for tests.
    public func destroyKey() throws {
        try store.delete()
        cached = nil
        registered = false
    }
}
