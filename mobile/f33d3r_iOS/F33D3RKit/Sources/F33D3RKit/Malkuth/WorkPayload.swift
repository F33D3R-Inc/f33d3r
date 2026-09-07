import Foundation

/// The signed half of a Work.
///
/// Mirrors `workCanonicalPayload` in
/// `feed-engine/internal/handler/work_event.go` field for field. These nineteen
/// values, and nothing else, are what the author's signature covers: everything
/// the compose screen sends outside this struct — `is_nsfw`,
/// `video_watermarked_url`, `video_width`/`video_height`, `quoted_work_id`,
/// `react_layout`, `tus_upload_id` — is server- or transcoder-derived and rides
/// unsigned on the envelope. See ``WorkEnvelope``.
///
/// `kind` lives here and only here. The envelope's `event_type` is an unsigned
/// outer field; the server reads the kind from the payload precisely so that the
/// content type is inside what the author actually signed.
public struct WorkPayload: Hashable, Sendable {

    /// The author's PIAL. Signed, so it cannot be filled in by the server, and
    /// optional on `CurrentUser` because the API did not always carry it —
    /// signing a payload whose author is `""` is refused with
    /// ``MalkuthError/pialUnknown`` rather than attempted.
    public var authorPIAL: String
    public var body: String
    public var commentGating: CommentGating
    public var isRepost: Bool
    public var kind: WorkKind
    /// Attachment URLs. **Order here is not the hashed order** — see
    /// ``canonicalFields()``.
    public var mediaURLs: [String]
    /// CID of the work being replied to. Required when `kind == .reply`.
    public var parentCID: String?
    /// RFC 3339. `nil` and `""` are different things on the wire; this is `nil`
    /// or a validated string, never empty.
    public var pollEndsAt: String?
    /// `nil` and `[]` are different on the wire: `null` versus `[]`. A poll
    /// whose options are still being written is `[]`; a work that is not a poll
    /// is `nil`.
    public var pollOptions: [String]?
    public var repostSourceID: String?
    /// RFC 3339. When set and in the future the work is stored unpublished until
    /// the scheduler picks it up.
    public var scheduledAt: String?
    public var subscriberOnly: Bool
    /// Client-supplied tags. The server takes the union of these and any
    /// `#hashtags` it finds in the body, so omitting them loses nothing —
    /// but whatever is sent is signed. Unlike `mediaURLs`, these are **not**
    /// sorted.
    public var tags: [String]
    /// Milliseconds since the epoch. This is the only thing that distinguishes
    /// two identical posts, which makes it the thing that decides whether a
    /// retry is the same post or a second one — see ``WorkSubmission``.
    public var timestampMS: Int64
    public var videoDurationSecs: Int?
    public var videoMasterURL: String?
    public var videoPosterURL: String?
    public var voiceDurationSecs: Int?
    public var voiceURL: String?

    public init(
        authorPIAL: String,
        body: String = "",
        commentGating: CommentGating = .default,
        isRepost: Bool = false,
        kind: WorkKind = .post,
        mediaURLs: [String] = [],
        parentCID: String? = nil,
        pollEndsAt: String? = nil,
        pollOptions: [String]? = nil,
        repostSourceID: String? = nil,
        scheduledAt: String? = nil,
        subscriberOnly: Bool = false,
        tags: [String] = [],
        timestampMS: Int64,
        videoDurationSecs: Int? = nil,
        videoMasterURL: String? = nil,
        videoPosterURL: String? = nil,
        voiceDurationSecs: Int? = nil,
        voiceURL: String? = nil
    ) {
        self.authorPIAL = authorPIAL
        self.body = body
        self.commentGating = commentGating
        self.isRepost = isRepost
        self.kind = kind
        self.mediaURLs = mediaURLs
        self.parentCID = parentCID
        self.pollEndsAt = pollEndsAt
        self.pollOptions = pollOptions
        self.repostSourceID = repostSourceID
        self.scheduledAt = scheduledAt
        self.subscriberOnly = subscriberOnly
        self.tags = tags
        self.timestampMS = timestampMS
        self.videoDurationSecs = videoDurationSecs
        self.videoMasterURL = videoMasterURL
        self.videoPosterURL = videoPosterURL
        self.voiceDurationSecs = voiceDurationSecs
        self.voiceURL = voiceURL
    }

    // MARK: - Canonical form

    /// The nineteen fields in the exact order `workCanonicalPayload` declares
    /// them, with the two normalisations `verifyCID` applies before hashing.
    ///
    /// Both normalisations are the server's, reproduced here because the server
    /// applies them to what it receives and then compares against a CID the
    /// client already computed. A client that skips them computes a CID of
    /// different bytes than the ones the server hashes, and every post is
    /// rejected:
    ///
    /// - **`media_urls` is sorted.** `verifyCID` calls `sort.Strings`, which
    ///   orders by UTF-8 byte value. Swift's `<` on `String` is Unicode
    ///   canonical ordering, which is *not* the same relation, so the sort is
    ///   done on the UTF-8 bytes explicitly. (`malkuth.js` uses JavaScript's
    ///   default sort, which orders by UTF-16 code unit — a third relation that
    ///   agrees with Go on ASCII and can disagree above U+FFFF. Go is the
    ///   verifier, so Go's relation is the one implemented.)
    /// - **nil slices become `[]`.** `media_urls` and `tags` are never `null`.
    ///   `poll_options` is `interface{}` and *is* `null` when absent, which is
    ///   why it is modelled as `[String]?` and these two are not.
    public func canonicalFields() -> [(String, CanonicalJSON.Value)] {
        [
            ("author_pial", .string(authorPIAL)),
            ("body", .string(body)),
            ("comment_gating", .string(commentGating.rawValue)),
            ("is_repost", .bool(isRepost)),
            ("kind", .string(kind.rawValue)),
            ("media_urls", .array(Self.sortedByUTF8(mediaURLs).map { .string($0) })),
            ("parent_cid", .string(orNull: parentCID)),
            ("poll_ends_at", .string(orNull: pollEndsAt)),
            ("poll_options", pollOptions.map { .array($0.map { .string($0) }) } ?? .null),
            ("repost_source_id", .string(orNull: repostSourceID)),
            ("scheduled_at", .string(orNull: scheduledAt)),
            ("subscriber_only", .bool(subscriberOnly)),
            ("tags", .array(tags.map { .string($0) })),
            ("timestamp_ms", .int(timestampMS)),
            ("video_duration_secs", .int(orNull: videoDurationSecs)),
            ("video_master_url", .string(orNull: videoMasterURL)),
            ("video_poster_url", .string(orNull: videoPosterURL)),
            ("voice_duration_secs", .int(orNull: voiceDurationSecs)),
            ("voice_url", .string(orNull: voiceURL)),
        ]
    }

    /// The exact bytes the server will hash. Not valid until ``validate()``
    /// has passed — ``WorkEnvelope`` calls it first.
    public func canonicalJSON() -> Data {
        CanonicalJSON.object(canonicalFields())
    }

    /// `sha256:` + lower-case hex of the SHA-256 of ``canonicalJSON()``.
    public func contentID() -> String {
        ContentID.of(canonicalJSON())
    }

    /// Go's `sort.Strings`: lexicographic by UTF-8 byte, which for `String` in
    /// Swift means comparing `utf8` and not the strings themselves.
    static func sortedByUTF8(_ values: [String]) -> [String] {
        values.sorted { lhs, rhs in
            var l = lhs.utf8.makeIterator()
            var r = rhs.utf8.makeIterator()
            while true {
                switch (l.next(), r.next()) {
                case (nil, nil): return false        // equal
                case (nil, _): return true           // lhs is a prefix
                case (_, nil): return false
                case (let a?, let b?):
                    if a != b { return a < b }
                }
            }
        }
    }

    // MARK: - Validation

    /// Rejects everything this client can know is wrong before a request is
    /// spent finding out.
    ///
    /// Each check corresponds to a specific server behaviour, named in
    /// ``MalkuthError``. Two of them are not "the server would reject this" but
    /// the worse case — "the server would *accept* this and do something other
    /// than what the author meant": a `scheduled_at` Go cannot parse becomes an
    /// immediate publish, and a malformed `parent_cid` becomes a top-level work
    /// instead of a reply.
    public func validate() throws(MalkuthError) {
        guard !authorPIAL.trimmingCharacters(in: .whitespaces).isEmpty else {
            throw .pialUnknown
        }
        try Self.requireUUID(authorPIAL, field: "author_pial")

        if kind == .reply, (parentCID?.isEmpty ?? true) {
            throw .replyMissingParentCID
        }
        if isRepost, (repostSourceID?.trimmingCharacters(in: .whitespaces).isEmpty ?? true) {
            throw .repostMissingSourceID
        }

        if let parentCID { try Self.requireContentID(parentCID, field: "parent_cid") }
        if let repostSourceID { try Self.requireUUID(repostSourceID, field: "repost_source_id") }
        if let pollEndsAt { try Self.requireRFC3339(pollEndsAt, field: "poll_ends_at") }
        if let scheduledAt { try Self.requireRFC3339(scheduledAt, field: "scheduled_at") }

        if let videoDurationSecs {
            try Self.requireDuration(videoDurationSecs, field: "video_duration_secs")
        }
        if let voiceDurationSecs {
            try Self.requireDuration(voiceDurationSecs, field: "voice_duration_secs")
        }

        try Self.requireNoNUL(body, field: "body")
        for tag in tags { try Self.requireNoNUL(tag, field: "tags") }
        for option in pollOptions ?? [] { try Self.requireNoNUL(option, field: "poll_options") }
        for url in mediaURLs {
            if url.isEmpty { throw .emptyMediaURL }
            try Self.requireNoNUL(url, field: "media_urls")
        }
        if let voiceURL { try Self.requireNoNUL(voiceURL, field: "voice_url") }
        if let videoMasterURL { try Self.requireNoNUL(videoMasterURL, field: "video_master_url") }
        if let videoPosterURL { try Self.requireNoNUL(videoPosterURL, field: "video_poster_url") }
    }

    private static func requireContentID(_ value: String, field: String) throws(MalkuthError) {
        guard ContentID.isWellFormed(value) else { throw .malformedCID(field: field, value: value) }
    }

    private static func requireUUID(_ value: String, field: String) throws(MalkuthError) {
        guard UUID(uuidString: value) != nil else { throw .malformedUUID(field: field, value: value) }
    }

    private static func requireDuration(_ value: Int, field: String) throws(MalkuthError) {
        guard value >= 0, value < (1 << 53) else {
            throw .durationOutOfRange(field: field, value: value)
        }
    }

    private static func requireNoNUL(_ value: String, field: String) throws(MalkuthError) {
        if value.utf8.contains(0) { throw .nulByteInText(field: field) }
    }

    /// Exactly what Go's `time.RFC3339` layout accepts, and nothing else.
    ///
    /// `ISO8601DateFormatter` is close but not the same relation — it is happy
    /// with forms Go's layout rejects — so the shape is checked directly. Go's
    /// layout is `2006-01-02T15:04:05Z07:00`: a date, `T`, a time, optional
    /// fractional seconds, then either `Z` or `±hh:mm`. Lower-case `t`/`z` are
    /// accepted by Go's parser as well.
    private static func requireRFC3339(_ value: String, field: String) throws(MalkuthError) {
        guard Self.rfc3339.firstMatch(
            in: value, range: NSRange(value.startIndex..., in: value)
        ) != nil else {
            throw .malformedTimestamp(field: field, value: value)
        }
    }

    private static let rfc3339 = try! NSRegularExpression(
        pattern: #"^\d{4}-\d{2}-\d{2}[Tt]\d{2}:\d{2}:\d{2}(\.\d+)?([Zz]|[+-]\d{2}:\d{2})$"#
    )

    /// Formats `date` the way Go's `time.RFC3339` reads it, for callers holding
    /// a `Date` rather than a string.
    ///
    /// Second precision, UTC. Fractional seconds parse fine but are noise in a
    /// field that is hashed: two clients formatting the same instant must
    /// produce the same string or they produce different works.
    public static func rfc3339String(_ date: Date) -> String {
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime]
        formatter.timeZone = TimeZone(identifier: "UTC")
        return formatter.string(from: date)
    }
}

private extension CanonicalJSON.Value {
    static func string(orNull value: String?) -> CanonicalJSON.Value {
        value.map { .string($0) } ?? .null
    }
    static func int(orNull value: Int?) -> CanonicalJSON.Value {
        value.map { .int(Int64($0)) } ?? .null
    }
}
