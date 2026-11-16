import Foundation

/// The signed half of a Work.
///
/// Mirrors `workCanonicalPayload` in
/// `feed-engine/internal/handler/work_event.go` field for field. These nineteen
/// values, and nothing else, are what the author's signature covers: everything
/// the compose screen sends outside this struct — `is_nsfw`,
/// `video_width`/`video_height`, `quoted_work_id`, `react_layout`,
/// `downloads_allowed` — rides unsigned on the envelope. See ``WorkEnvelope``.
///
/// `kind` lives here and only here. The envelope's `event_type` is an unsigned
/// outer field; the server reads the kind from the payload precisely so that the
/// content type is inside what the author actually signed.
///
/// `is_repost` and `repost_source_id` are two of the nineteen hashed keys, but
/// they are not fields of this struct: a repost is a reaction, never a signed
/// work, and the server refuses any envelope that sets them. They are hashed as
/// the constants `false` and `null` (``canonicalFields()``), exactly as
/// `malkuth.js` sends them, so nothing on this client can set them.
public struct WorkPayload: Hashable, Sendable {

    /// The account (persona) the work is published as — the `account_id` that
    /// `GET /api/v1/me` answers to its owner. Signed; the server refuses a
    /// payload whose `author_account` is not the session's account.
    public var authorAccount: String
    /// The normalised body (``WorkText/normalize(_:)``). The server refuses a
    /// body that is not already normalised, because it can never rewrite what
    /// was signed.
    public var body: String
    public var commentGating: CommentGating
    public var kind: WorkKind
    /// Attachment URLs. **Order here is not the hashed order** — see
    /// ``canonicalFields()``.
    public var mediaURLs: [String]
    /// CID of the work being replied to. Required when `kind == .reply`.
    public var parentCID: String?
    /// RFC 3339. Required on a poll: 5 minutes to 7 days after the work opens.
    public var pollEndsAt: String?
    /// `nil` and `[]` are different on the wire: `null` versus `[]`. A work
    /// that is not a poll is `nil`. Each option is normalised text.
    public var pollOptions: [String]?
    /// RFC 3339. When set and in the future the work is stored unpublished until
    /// the scheduler picks it up.
    public var scheduledAt: String?
    public var subscriberOnly: Bool
    /// Client-supplied tags. The server takes the union of these and any
    /// `#hashtags` it finds in the body, so omitting them loses nothing —
    /// but whatever is sent is signed. Unlike `mediaURLs`, these are **not**
    /// sorted.
    public var tags: [String]
    /// Milliseconds since the epoch, stamped when the work is signed. The
    /// server accepts a work signed within the last hour and no more than five
    /// minutes ahead, so it is stamped at send time — see ``WorkSubmission``.
    public var timestampMS: Int64
    public var videoDurationSecs: Int?
    public var videoMasterURL: String?
    public var videoPosterURL: String?
    public var voiceDurationSecs: Int?
    public var voiceURL: String?

    public init(
        authorAccount: String,
        body: String = "",
        commentGating: CommentGating = .default,
        kind: WorkKind = .post,
        mediaURLs: [String] = [],
        parentCID: String? = nil,
        pollEndsAt: String? = nil,
        pollOptions: [String]? = nil,
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
        self.authorAccount = authorAccount
        self.body = body
        self.commentGating = commentGating
        self.kind = kind
        self.mediaURLs = mediaURLs
        self.parentCID = parentCID
        self.pollEndsAt = pollEndsAt
        self.pollOptions = pollOptions
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

    /// This payload with its text normalised and the signing time stamped now:
    /// what is actually signed. Normalising is idempotent, so a payload that
    /// was already normalised is unchanged apart from the stamp.
    public func preparedForSigning(at timestampMS: Int64 = WorkPayload.nowMS()) -> WorkPayload {
        var p = self
        p.body = WorkText.normalize(body)
        p.pollOptions = pollOptions?.map(WorkText.normalize)
        p.timestampMS = timestampMS
        return p
    }

    // MARK: - Canonical form

    /// The nineteen fields in the exact order `workCanonicalPayload` declares
    /// them, with the two normalisations `verifyCID` applies before hashing.
    /// `is_repost` and `repost_source_id` are hashed as `false` and `null`.
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
            ("author_account", .string(authorAccount)),
            ("body", .string(body)),
            ("comment_gating", .string(commentGating.rawValue)),
            ("is_repost", .bool(false)),
            ("kind", .string(kind.rawValue)),
            ("media_urls", .array(Self.sortedByUTF8(mediaURLs).map { .string($0) })),
            ("parent_cid", .string(orNull: parentCID)),
            ("poll_ends_at", .string(orNull: pollEndsAt)),
            ("poll_options", pollOptions.map { .array($0.map { .string($0) }) } ?? .null),
            ("repost_source_id", .null),
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
    ///
    /// Freshness is not judged here: the stamp is this device's clock at
    /// signing, so only the server's clock can say it is outside the window
    /// (``WorkSubmission`` handles that answer). A poll's run is measured from
    /// the signing stamp, which is when this device says the work opens.
    public func validate() throws(MalkuthError) {
        guard !authorAccount.trimmingCharacters(in: .whitespaces).isEmpty else {
            throw .accountUnknown
        }
        try Self.requireUUID(authorAccount, field: "author_account")

        if kind == .reply, (parentCID?.isEmpty ?? true) {
            throw .replyMissingParentCID
        }

        if let parentCID { try Self.requireContentID(parentCID, field: "parent_cid") }
        if let pollEndsAt { try Self.requireRFC3339(pollEndsAt, field: "poll_ends_at") }
        if let scheduledAt { try Self.requireRFC3339(scheduledAt, field: "scheduled_at") }

        if let videoDurationSecs {
            try Self.requireDuration(videoDurationSecs, field: "video_duration_secs")
        }
        if let voiceDurationSecs {
            try Self.requireDuration(voiceDurationSecs, field: "voice_duration_secs")
        }

        guard WorkText.isNormalized(body) else { throw .textNotNormalized(field: "body") }
        let over = WorkText.overLimit(body)
        if over > 0 { throw .textTooLong(over: over) }
        try validatePoll()
        for tag in tags {
            guard !tag.isEmpty, WorkText.isNormalized(tag),
                  WorkText.scalarCount(tag) <= WorkPayload.tagMaxScalars,
                  !tag.unicodeScalars.contains(where: { WorkText.isSpace($0.value) })
            else { throw .invalidTag(tag) }
        }
        if tags.count > WorkPayload.maxClientTags { throw .tooManyTags }

        for url in mediaURLs {
            if url.isEmpty { throw .emptyMediaURL }
            try Self.requireNoNUL(url, field: "media_urls")
        }
        if let voiceURL { try Self.requireNoNUL(voiceURL, field: "voice_url") }
        if let videoMasterURL { try Self.requireNoNUL(videoMasterURL, field: "video_master_url") }
        if let videoPosterURL { try Self.requireNoNUL(videoPosterURL, field: "video_poster_url") }
    }

    /// The server's poll rule (`validatePoll`): options and the poll kind come
    /// together, 2–4 distinct one-line options of at most 80 code points, and an
    /// end 5 minutes to 7 days after the work opens.
    private func validatePoll() throws(MalkuthError) {
        guard let pollOptions else {
            if kind == .poll { throw .pollInvalid("A poll needs options.") }
            if pollEndsAt != nil { throw .pollInvalid("Only a poll has an end time.") }
            return
        }
        guard kind == .poll else { throw .pollInvalid("Only a poll has options.") }
        guard (WorkPayload.pollMinOptions...WorkPayload.pollMaxOptions).contains(pollOptions.count) else {
            throw .pollInvalid("A poll has 2 to 4 options.")
        }
        var seen = Set<String>()
        for option in pollOptions {
            guard WorkText.isNormalized(option), WorkText.isSingleLine(option), !WorkText.isBlank(option) else {
                throw .pollInvalid("Every option needs text, on one line.")
            }
            guard WorkText.scalarCount(option) <= WorkText.pollOptionMaxScalars else {
                throw .pollInvalid("Each option is at most \(WorkText.pollOptionMaxScalars) characters.")
            }
            guard seen.insert(option.lowercased()).inserted else {
                throw .pollInvalid("Options must differ.")
            }
        }
        guard let pollEndsAt, let ends = Self.parseRFC3339(pollEndsAt) else {
            throw .pollInvalid("A poll needs an end time.")
        }
        let opens = scheduledAt.flatMap(Self.parseRFC3339)
            ?? Date(timeIntervalSince1970: Double(timestampMS) / 1000)
        let runs = ends.timeIntervalSince(opens)
        guard runs >= WorkPayload.pollMinDuration, runs <= WorkPayload.pollMaxDuration else {
            throw .pollInvalid("A poll runs from 5 minutes to 7 days.")
        }
    }

    public static let pollMinOptions = 2
    public static let pollMaxOptions = 4
    public static let pollMinDuration: TimeInterval = 5 * 60
    public static let pollMaxDuration: TimeInterval = 7 * 24 * 60 * 60
    public static let maxClientTags = 10
    public static let tagMaxScalars = 64

    static func parseRFC3339(_ value: String) -> Date? {
        let withFraction = ISO8601DateFormatter()
        withFraction.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        let plain = ISO8601DateFormatter()
        plain.formatOptions = [.withInternetDateTime]
        return plain.date(from: value) ?? withFraction.date(from: value)
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
