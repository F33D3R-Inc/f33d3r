import Foundation

/// Everything that can stop a Work being signed or accepted.
///
/// These are typed and specific because the server's answers are not: a payload
/// the server dislikes comes back as `400` with a bare line of text
/// (`cid mismatch`, `unknown work kind`, `reply requires parent_cid`) and a
/// signature it cannot check comes back as `403 signature invalid` whether the
/// key is wrong, missing, or the key authority is down. Anything this client can
/// determine locally is determined locally, so the round trip is spent only on
/// things only the server knows.
public enum MalkuthError: Error, Equatable, Sendable {

    // MARK: Identity

    /// The signed payload carries `author_pial` and this device does not know
    /// its own PIAL yet.
    ///
    /// Signing with an empty string would produce a valid signature over a
    /// payload naming nobody: `verifyCID` would pass, `verifyMalkuthSig` would
    /// fetch the key for the *session's* PIAL and pass too, and the Work would
    /// be stored with an author field that disagrees with its own signature.
    /// Refusing is the only correct answer.
    case pialUnknown

    // MARK: Payload

    /// `kind == .reply` with no `parent_cid`. Server: `400 reply requires parent_cid`.
    case replyMissingParentCID
    /// `is_repost` with no `repost_source_id`. Server: `400 repost requires repost_source_id`.
    case repostMissingSourceID
    /// A CID that is not `sha256:` followed by 64 lower-case hex digits. The
    /// server checks this prefix on the envelope's own CID and nothing else, so
    /// a malformed `parent_cid` becomes a reply whose parent lookup silently
    /// finds nothing and which is stored as a top-level Work.
    case malformedCID(field: String, value: String)
    /// A `repost_source_id` that is not a UUID. The insert casts it with
    /// `$15::uuid`, so a non-UUID is a Postgres error surfaced as `500`.
    case malformedUUID(field: String, value: String)
    /// A timestamp that `time.Parse(time.RFC3339, …)` would reject.
    ///
    /// This one is worth the strictness: the server ignores the parse error and
    /// leaves the field NULL, so a `scheduled_at` the client got wrong does not
    /// fail — it publishes the Work immediately instead of on the date the
    /// author chose.
    case malformedTimestamp(field: String, value: String)
    /// A duration outside `0 ..< 2^53`. Go carries these through `interface{}`,
    /// so they are re-marshalled from `float64`: past 2^53 the value the server
    /// hashes is not the value the client hashed.
    case durationOutOfRange(field: String, value: Int)
    /// A NUL byte in text bound for a Postgres `TEXT` column. Postgres rejects
    /// these outright, and the insert failure surfaces as an opaque `500`.
    case nulByteInText(field: String)
    /// An empty string in `media_urls`, which would be stored as an empty array
    /// element and render as a broken attachment.
    case emptyMediaURL

    // MARK: Keys

    /// No signing key exists on this device and one could not be created.
    case keyGenerationFailed(String)
    /// The Keychain refused to store or return the private key.
    case keychain(OSStatus)
    /// Stored key material that is not a valid P-256 private key — a truncated
    /// Keychain item, or a value written by something else under the same
    /// account.
    case keyMaterialCorrupt

    // MARK: Server

    /// `POST /events` or the key registration answered non-2xx. `reason` is the
    /// server's own line of text, kept verbatim because those strings are the
    /// only diagnostic the write path emits.
    case rejected(status: Int, reason: String)

    /// The key authority (Elohim Veni) could not be reached, so the public key
    /// was not registered and no signature this device makes can be verified
    /// yet. Distinguished from every other failure because it is the one that is
    /// expected in a local deployment and is not the client's fault.
    case keyAuthorityUnavailable

    public var isKeyAuthorityUnavailable: Bool {
        switch self {
        case .keyAuthorityUnavailable: return true
        case .rejected(let status, let reason):
            return status == 502 && reason.contains("key_authority_unavailable")
        default: return false
        }
    }
}

extension MalkuthError: CustomStringConvertible {
    public var description: String {
        switch self {
        case .pialUnknown:
            return "This device does not know its own PIAL yet, and author_pial is a signed field."
        case .replyMissingParentCID:
            return "A reply must carry the CID of the work it replies to."
        case .repostMissingSourceID:
            return "A repost must carry the id of the work it reposts."
        case .malformedCID(let field, let value):
            return "\(field) is not a content id: \(value)"
        case .malformedUUID(let field, let value):
            return "\(field) is not a UUID: \(value)"
        case .malformedTimestamp(let field, let value):
            return "\(field) is not an RFC 3339 timestamp: \(value)"
        case .durationOutOfRange(let field, let value):
            return "\(field) is out of range: \(value)"
        case .nulByteInText(let field):
            return "\(field) contains a NUL byte, which cannot be stored."
        case .emptyMediaURL:
            return "A media attachment has an empty URL."
        case .keyGenerationFailed(let detail):
            return "Could not create a signing key: \(detail)"
        case .keychain(let status):
            // -34018 is errSecMissingEntitlement, and on this app it has one
            // cause: a build with no `application-identifier`, which is what
            // `CODE_SIGNING_ALLOWED=NO` produces. Every Keychain call then
            // fails, so the session token silently does not persist and the
            // signing key cannot be stored — and the only symptom anyone sees
            // is this error, on the first attempt to post. Naming it here is
            // the difference between a five-minute fix and an afternoon.
            if status == -34018 {
                return """
                    This build cannot use the Keychain, so it has no signing key. \
                    It was built without code signing; run it from Xcode, or build \
                    without CODE_SIGNING_ALLOWED=NO.
                    """
            }
            return "Keychain error \(status)."
        case .keyMaterialCorrupt:
            return "The stored signing key is not usable."
        case .rejected(let status, let reason):
            return "Server rejected the work (\(status)): \(reason)"
        case .keyAuthorityUnavailable:
            return "The key authority is unreachable, so this device's signing key is not registered."
        }
    }
}
