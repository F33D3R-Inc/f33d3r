import Foundation

/// A signed Work, ready to be the body of `POST /events`.
///
/// Mirrors `workEnvelope` plus the unsigned outer fields `workEvent` reads
/// alongside it. The split between the two is the point of the whole design:
///
/// - **Signed** (inside `payload`, covered by `cid`): everything in
///   ``WorkPayload``. Content and the author's intent about it.
/// - **Unsigned** (beside `payload`): `is_nsfw`, `video_watermarked_url`,
///   `video_width`, `video_height`, `quoted_work_id`, `react_layout`, and
///   `tus_upload_id` — which sits *inside* the payload object but outside
///   `workCanonicalPayload`, so `verifyCID` never sees it. These are derived by
///   the transcoder or resolved by the server, and putting them under the
///   signature would mean the author signing values they never chose.
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
    public var videoWatermarkedURL: String?
    public var videoWidth: Int?
    public var videoHeight: Int?
    /// The UUID of a quoted work. The server resolves it to a CID for
    /// `work_citations`; it is not in the signature because the client is
    /// naming a row, and the row's CID is the server's to look up.
    public var quotedWorkID: String?
    public var reactLayout: String?
    /// Ties a resumable upload to this work so the orphan sweep does not delete
    /// the video. Carried inside the `payload` object, outside the signature.
    public var tusUploadID: String?

    /// Validates, canonicalises, hashes and signs — in that order, because each
    /// step's input is the previous step's output and skipping the first is how
    /// an invalid work acquires a valid signature.
    public init(
        payload: WorkPayload,
        key: MalkuthKey,
        eventType: String? = nil,
        isNSFW: Bool = false,
        videoWatermarkedURL: String? = nil,
        videoWidth: Int? = nil,
        videoHeight: Int? = nil,
        quotedWorkID: String? = nil,
        reactLayout: String? = nil,
        tusUploadID: String? = nil
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
        self.videoWatermarkedURL = videoWatermarkedURL
        self.videoWidth = videoWidth
        self.videoHeight = videoHeight
        self.quotedWorkID = quotedWorkID
        self.reactLayout = reactLayout
        self.tusUploadID = tusUploadID
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
        if let videoWatermarkedURL { fields.append(("video_watermarked_url", .string(videoWatermarkedURL))) }
        if let videoWidth { fields.append(("video_width", .int(Int64(videoWidth)))) }
        if let videoHeight { fields.append(("video_height", .int(Int64(videoHeight)))) }
        if let quotedWorkID { fields.append(("quoted_work_id", .string(quotedWorkID))) }
        if let reactLayout { fields.append(("react_layout", .string(reactLayout))) }

        var out = CanonicalJSON.object(fields)
        // Drop the closing brace, append `,"payload":<bytes>}`.
        out.removeLast()
        out.append(contentsOf: Array(",\"payload\":".utf8))
        out.append(payloadObject)
        out.append(UInt8(ascii: "}"))
        return out
    }

    /// The payload object as sent: the canonical bytes, with `tus_upload_id`
    /// appended when there is one.
    ///
    /// Appending a key the canonical struct does not declare is safe by
    /// construction — `verifyCID` unmarshals into `workCanonicalPayload`, which
    /// ignores unknown keys, and re-marshals only its own nineteen fields. The
    /// upload id is then read out by a second, separate unmarshal on the server.
    /// This is the mechanism the Go comment describes; it is used here rather
    /// than worked around.
    private var payloadObject: Data {
        guard let tusUploadID, !tusUploadID.isEmpty else { return canonicalPayload }
        var out = canonicalPayload
        out.removeLast()
        out.append(UInt8(ascii: ","))
        // Splice the single field out of its own object braces so it is escaped
        // by the same writer as everything else.
        let field = CanonicalJSON.object([("tus_upload_id", .string(tusUploadID))])
        out.append(contentsOf: field.dropFirst().dropLast())
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
