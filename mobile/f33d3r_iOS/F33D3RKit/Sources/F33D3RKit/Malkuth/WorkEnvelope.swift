import Foundation

/// A signed Work, ready to be the body of `POST /events`.
///
/// Mirrors `workEnvelope` plus the unsigned outer fields `workEvent` reads
/// alongside it. The split between the two is the point of the whole design:
///
/// - **Signed** (inside `payload`, covered by `cid`): everything in
///   ``WorkPayload``. Content and the author's intent about it.
/// - **Unsigned** (beside `payload`): `is_nsfw`, `video_width`,
///   `video_height`, `quoted_work_id`, `react_layout` and `downloads_allowed`
///   — the outer fields `parseWorkEnvelope` reads. They are derived by the
///   transcoder, resolved by the server, or a switch about the work rather
///   than its content.
///
/// Nothing else is sent. The downloadable file and the upload binding are the
/// media registry's, never the client's, so there is no watermarked URL and no
/// upload id here.
///
/// ## The bytes are built once
///
/// ``httpBody`` splices ``canonicalPayload`` into the envelope **verbatim**
/// rather than re-encoding the payload. That is deliberate and it removes a
/// whole class of bug: the bytes that were hashed are literally the bytes that
/// are sent, so there is no second encoder whose escaping or ordering could
/// drift from the first. `JSONEncoder` never touches the signed half.
public struct WorkEnvelope: Sendable, Equatable {

    /// The unsigned outer `event_type`. Required non-empty by `eventsPost`, and
    /// then ignored for routing — the request is routed to `workEvent` by the
    /// presence of a `cid`, and the kind is read from the signed payload. It
    /// defaults to the kind's name so the two agree, but the server does not
    /// care and must not be relied on to.
    public let eventType: String
    public let payload: WorkPayload
    /// The exact bytes hashed to produce ``contentID``.
    public let canonicalPayload: Data
    public let contentID: String
    /// base64url, unpadded, over the UTF-8 bytes of ``contentID``.
    public let signature: String

    // Unsigned outer fields.
    public var isNSFW: Bool
    public var videoWidth: Int?
    public var videoHeight: Int?
    /// The UUID of a quoted work. The server resolves it to a CID for
    /// `work_citations`; it is not in the signature because the client is
    /// naming a row, and the row's CID is the server's to look up.
    public var quotedWorkID: String?
    public var reactLayout: String?
    /// The author's download switch for the work; nil sends nothing, which the
    /// server reads as its default, allowed.
    public var downloadsAllowed: Bool?

    /// Validates, canonicalises, hashes and signs — in that order, because each
    /// step's input is the previous step's output and skipping the first is how
    /// an invalid work acquires a valid signature.
    public init(
        payload: WorkPayload,
        key: MalkuthKey,
        eventType: String? = nil,
        isNSFW: Bool = false,
        videoWidth: Int? = nil,
        videoHeight: Int? = nil,
        quotedWorkID: String? = nil,
        reactLayout: String? = nil,
        downloadsAllowed: Bool? = nil
    ) throws {
        try payload.validate()

        let canonical = payload.canonicalJSON()
        let cid = ContentID.of(canonical)

        self.payload = payload
        self.canonicalPayload = canonical
        self.contentID = cid
        self.signature = try key.signature(forContentID: cid)
        self.eventType = eventType ?? payload.kind.rawValue
        self.isNSFW = isNSFW
        self.videoWidth = videoWidth
        self.videoHeight = videoHeight
        self.quotedWorkID = quotedWorkID
        self.reactLayout = reactLayout
        self.downloadsAllowed = downloadsAllowed
    }

    /// The request body for `POST /events`.
    ///
    /// Built with ``CanonicalJSON`` for the same reason the payload is: the
    /// signed bytes must appear unaltered, and the only way to guarantee that is
    /// to place them rather than re-encode them. The outer object's own key
    /// order is irrelevant to the server — it reads the body into a
    /// `map[string]json.RawMessage` — but it is fixed here anyway so the bytes
    /// of a retry are identical to the bytes of the original attempt.
    public var httpBody: Data {
        var fields: [(String, CanonicalJSON.Value)] = [
            ("event_type", .string(eventType)),
            ("cid", .string(contentID)),
            ("signature", .string(signature)),
            ("is_nsfw", .bool(isNSFW)),
        ]
        if let videoWidth { fields.append(("video_width", .int(Int64(videoWidth)))) }
        if let videoHeight { fields.append(("video_height", .int(Int64(videoHeight)))) }
        if let quotedWorkID { fields.append(("quoted_work_id", .string(quotedWorkID))) }
        if let reactLayout { fields.append(("react_layout", .string(reactLayout))) }
        if let downloadsAllowed { fields.append(("downloads_allowed", .bool(downloadsAllowed))) }

        var out = CanonicalJSON.object(fields)
        // Drop the closing brace, append `,"payload":<bytes>}`.
        out.removeLast()
        out.append(contentsOf: Array(",\"payload\":".utf8))
        out.append(canonicalPayload)
        out.append(UInt8(ascii: "}"))
        return out
    }
}

/// What `workEvent` answers with on success: `201` and the work's row id
/// alongside the CID it was stored under.
public struct WorkAccepted: Decodable, Hashable, Sendable {
    public let workID: String
    public let cid: String

    enum CodingKeys: String, CodingKey {
        case workID = "work_id"
        case cid
    }
}
