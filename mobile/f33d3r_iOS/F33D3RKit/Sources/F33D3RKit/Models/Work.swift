import Foundation

/// A work — F33D3R's unit of content. Posts, replies, quotes, videos, voice
/// notes and polls are all works distinguished by `kind`.
///
/// Mirrors `WorkDTO` in `feed-engine/internal/handler/apiv1_dto.go`, which is a
/// deliberate allow-list over `model.Work`. Several fields on the model are
/// withheld and must stay withheld: `AuthorPIAL` and `LineagePIAL` are the
/// identity spine and must never reach a client, and the ranking internals
/// behind `scoreBand` never leave the engine. If the app needs a value that is
/// missing here, the fix is to add it to the Go DTO on purpose.
public struct Work: Codable, Hashable, Sendable, Identifiable {
    /// The work's UUID. Public — it appears in `/work/{id}` URLs on the web.
    public let id: String
    /// Content-addressed identity, `sha256:{hex}`. Needed to reply to or quote
    /// this work, since citations are by CID rather than by row id.
    public let cid: String
    /// post | reply | quote | poll | video | voice | thread_post | react_video | vision
    public let kind: String

    public let author: WorkAuthor
    public let body: String
    public let createdAt: Date
    public let isEdited: Bool
    public let editedAt: Date?
    /// Set only on visions, which the server expires 24h after posting. The
    /// countdown is drawn from this; the client cannot set it.
    public let expiresAt: Date?

    // MARK: Attachments

    /// Image URLs, already resolved to `/media/...` paths by the server.
    public let mediaURLs: [String]
    /// Present on video works. Carries the clean master stream — the
    /// watermarked variant is what escapes the platform, not what the in-app
    /// player is given.
    public let video: WorkVideo?
    public let voice: WorkVoice?
    public let poll: Poll?
    public let linkPreview: LinkPreview?
    public let youTubeID: String?
    public let youTubePlaylistID: String?

    /// The embedded quote. Non-recursive by design: a quote cites exactly one
    /// CID, and the server does not expand a quote inside a quote.
    public let quoted: QuotedWork?
    /// CID this work replies to. Empty when it is not a reply.
    ///
    /// Populated only on a thread's focal work and its ancestors — never on feed
    /// rows and never on replies — so nothing that has to work in a list may be
    /// built on it.
    public let parentCID: String?
    /// CID this work quotes. Same restriction as `parentCID`.
    public let quotedCID: String?

    // MARK: Classification

    public let tags: [String]
    /// Declared as text | image | video | voice | poll | article, and in
    /// practice always "text": the column defaults to it and the insert path
    /// never writes it. Nothing may be displayed or decided from this — see
    /// `kindLabel`, which reads the attachments instead.
    public let contentType: String
    public let isNSFW: Bool
    public let isGore: Bool
    /// Nothing writes this column today, so it is `false` on every row. The two
    /// flags above are the ones moderation actually sets; this is carried
    /// because the DTO has it, and nothing is gated on it alone.
    public let isSensitive: Bool
    public let subscriberOnly: Bool
    public let isPinned: Bool
    /// open | followers | verified | none — who may reply. When the viewer may
    /// not, the reply control is disabled, never hidden.
    public let commentGating: String
    /// pending | clean | age_gated | flagged | human_review | blocked
    public let scanState: String
    /// trending | rising | steady | fading. The band is all a client ever sees;
    /// the underlying score is never exposed.
    public let scoreBand: String?

    // MARK: Counts

    /// The seven counts are `var` for one reason: ``applying(_:)`` rewrites
    /// them from a `work_engagement` frame. Nothing else mutates a `Work` — it
    /// is the server's row, and every other field stays `let` so it cannot be.
    public var likeCount: Int
    public var dislikeCount: Int
    public var repostCount: Int
    public var quoteCount: Int
    public var replyCount: Int
    public var bookmarkCount: Int
    public var viewCount: Int

    // MARK: Viewer state

    public let likedByViewer: Bool
    public let dislikedByViewer: Bool
    public let repostedByViewer: Bool
    public let bookmarkedByViewer: Bool
    public let viewerFollowsAuthor: Bool
    /// The viewer has bought this priced work outright. The server's row
    /// carries it on every read, so a track the reader owns is drawn as owned
    /// wherever it appears — the store, the player, a profile — from one field.
    public let purchasedByViewer: Bool

    // MARK: Surfacing

    /// Set when this work is in the feed because someone reposted it. The card
    /// header shows the *poster* — the reposter — with the original author
    /// below, never the other way round.
    public let repostedBy: WorkAuthor?
    public let repostedAt: Date?

    /// The original uploader, when content-scan matched this media to an earlier
    /// upload. Shown as a small credit badge, never as the author.
    public let lineageHandle: String?
    public let lineageDisplayName: String?

    /// Up to three recent repliers, for the avatar stack under the body.
    public let latestReplierHandles: [String]
    public let latestReplierAvatars: [String]

    /// Why this work is in front of this viewer — what the `why:` chip says and
    /// what its sheet explains.
    ///
    /// Optional because no handler serves it yet: the ranking engine knows the
    /// reason, but the DTO has nowhere to put it. Absent draws no chip, which is
    /// the honest state — a card that claims a reason it was not given would be
    /// worse than one that admits it does not know.
    public let provenance: WorkProvenance?

    /// Everything tipped to this work so far, in micro-AET. Optional for the
    /// same reason as `provenance`; absent draws the tip control with no amount.
    /// `var` with the counts above, and for the same reason.
    public var tipTotalUAET: Int64?

    /// What it costs to unlock this work, in micro-AET, when it is sold outright
    /// rather than bundled into a subscription. Optional and, when present,
    /// always alongside a locked body.
    public let priceUAET: Int64?

    enum CodingKeys: String, CodingKey {
        case id, cid, kind, author, body, video, voice, poll, quoted, tags
        case createdAt = "created_at"
        case isEdited = "is_edited"
        case editedAt = "edited_at"
        case expiresAt = "expires_at"
        case mediaURLs = "media_urls"
        case linkPreview = "link_preview"
        case youTubeID = "youtube_id"
        case youTubePlaylistID = "youtube_playlist_id"
        case parentCID = "parent_cid"
        case quotedCID = "quoted_cid"
        case contentType = "content_type"
        case isNSFW = "is_nsfw"
        case isGore = "is_gore"
        case isSensitive = "is_sensitive"
        case subscriberOnly = "subscriber_only"
        case isPinned = "is_pinned"
        case commentGating = "comment_gating"
        case scanState = "scan_state"
        case scoreBand = "score_band"
        case likeCount = "like_count"
        case dislikeCount = "dislike_count"
        case repostCount = "repost_count"
        case quoteCount = "quote_count"
        case replyCount = "reply_count"
        case bookmarkCount = "bookmark_count"
        case viewCount = "view_count"
        case likedByViewer = "liked_by_viewer"
        case dislikedByViewer = "disliked_by_viewer"
        case repostedByViewer = "reposted_by_viewer"
        case bookmarkedByViewer = "bookmarked_by_viewer"
        case viewerFollowsAuthor = "viewer_follows_author"
        case purchasedByViewer = "work_purchased_by_viewer"
        case repostedBy = "reposted_by"
        case repostedAt = "reposted_at"
        case lineageHandle = "lineage_handle"
        case lineageDisplayName = "lineage_display_name"
        case latestReplierHandles = "latest_replier_handles"
        case latestReplierAvatars = "latest_replier_avatars"
        case provenance
        case tipTotalUAET = "tip_total_uaet"
        case priceUAET = "price_uaet"
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        cid = try c.decode(String.self, forKey: .cid)
        kind = try c.decode(String.self, forKey: .kind)
        author = try c.decode(WorkAuthor.self, forKey: .author)
        body = try c.decodeIfPresent(String.self, forKey: .body) ?? ""
        createdAt = try c.decode(Date.self, forKey: .createdAt)
        isEdited = try c.decodeIfPresent(Bool.self, forKey: .isEdited) ?? false
        editedAt = try c.decodeIfPresent(Date.self, forKey: .editedAt)
        expiresAt = try c.decodeIfPresent(Date.self, forKey: .expiresAt)

        // Go omits empty slices with `omitempty`, so every collection decodes
        // through a default rather than requiring the key. A missing array and
        // an empty array mean the same thing here.
        mediaURLs = try c.decodeIfPresent([String].self, forKey: .mediaURLs) ?? []
        video = try c.decodeIfPresent(WorkVideo.self, forKey: .video)
        voice = try c.decodeIfPresent(WorkVoice.self, forKey: .voice)
        poll = try c.decodeIfPresent(Poll.self, forKey: .poll)
        linkPreview = try c.decodeIfPresent(LinkPreview.self, forKey: .linkPreview)
        youTubeID = try c.decodeIfPresent(String.self, forKey: .youTubeID)
        youTubePlaylistID = try c.decodeIfPresent(String.self, forKey: .youTubePlaylistID)
        quoted = try c.decodeIfPresent(QuotedWork.self, forKey: .quoted)
        parentCID = try c.decodeIfPresent(String.self, forKey: .parentCID)
        quotedCID = try c.decodeIfPresent(String.self, forKey: .quotedCID)

        tags = try c.decodeIfPresent([String].self, forKey: .tags) ?? []
        contentType = try c.decodeIfPresent(String.self, forKey: .contentType) ?? "text"
        isNSFW = try c.decodeIfPresent(Bool.self, forKey: .isNSFW) ?? false
        isGore = try c.decodeIfPresent(Bool.self, forKey: .isGore) ?? false
        isSensitive = try c.decodeIfPresent(Bool.self, forKey: .isSensitive) ?? false
        subscriberOnly = try c.decodeIfPresent(Bool.self, forKey: .subscriberOnly) ?? false
        isPinned = try c.decodeIfPresent(Bool.self, forKey: .isPinned) ?? false
        commentGating = try c.decodeIfPresent(String.self, forKey: .commentGating) ?? "open"
        scanState = try c.decodeIfPresent(String.self, forKey: .scanState) ?? "clean"
        scoreBand = try c.decodeIfPresent(String.self, forKey: .scoreBand)

        likeCount = try c.decodeIfPresent(Int.self, forKey: .likeCount) ?? 0
        dislikeCount = try c.decodeIfPresent(Int.self, forKey: .dislikeCount) ?? 0
        repostCount = try c.decodeIfPresent(Int.self, forKey: .repostCount) ?? 0
        quoteCount = try c.decodeIfPresent(Int.self, forKey: .quoteCount) ?? 0
        replyCount = try c.decodeIfPresent(Int.self, forKey: .replyCount) ?? 0
        bookmarkCount = try c.decodeIfPresent(Int.self, forKey: .bookmarkCount) ?? 0
        viewCount = try c.decodeIfPresent(Int.self, forKey: .viewCount) ?? 0

        likedByViewer = try c.decodeIfPresent(Bool.self, forKey: .likedByViewer) ?? false
        dislikedByViewer = try c.decodeIfPresent(Bool.self, forKey: .dislikedByViewer) ?? false
        repostedByViewer = try c.decodeIfPresent(Bool.self, forKey: .repostedByViewer) ?? false
        bookmarkedByViewer = try c.decodeIfPresent(Bool.self, forKey: .bookmarkedByViewer) ?? false
        viewerFollowsAuthor = try c.decodeIfPresent(Bool.self, forKey: .viewerFollowsAuthor) ?? false
        purchasedByViewer = try c.decodeIfPresent(Bool.self, forKey: .purchasedByViewer) ?? false

        repostedBy = try c.decodeIfPresent(WorkAuthor.self, forKey: .repostedBy)
        repostedAt = try c.decodeIfPresent(Date.self, forKey: .repostedAt)
        lineageHandle = try c.decodeIfPresent(String.self, forKey: .lineageHandle)
        lineageDisplayName = try c.decodeIfPresent(String.self, forKey: .lineageDisplayName)
        latestReplierHandles = try c.decodeIfPresent([String].self, forKey: .latestReplierHandles) ?? []
        latestReplierAvatars = try c.decodeIfPresent([String].self, forKey: .latestReplierAvatars) ?? []

        provenance = try c.decodeIfPresent(WorkProvenance.self, forKey: .provenance)
        tipTotalUAET = try c.decodeIfPresent(Int64.self, forKey: .tipTotalUAET)
        priceUAET = try c.decodeIfPresent(Int64.self, forKey: .priceUAET)
    }
}

// MARK: - Derived state

public extension Work {
    /// The server refuses to render a blocked work and the app must not either;
    /// a tombstone goes in its place.
    var isBlocked: Bool { scanState == "blocked" }

    /// True when the viewer is not entitled to the content and the card should
    /// show the paywall rather than the body.
    func isLockedForViewer(currentHandle: String?) -> Bool {
        subscriberOnly && author.handle != currentHandle
    }

    /// Whether anything is attached that needs a media block drawn.
    var hasMedia: Bool { video != nil || !mediaURLs.isEmpty }

    /// Whether the media must sit behind a gate before it is shown. In practice
    /// this is the NSFW and gore flags: `isSensitive` is never set server-side,
    /// and is kept in the test only so the gate still appears if that changes.
    var needsContentGate: Bool { isNSFW || isGore || isSensitive }

    /// Whether the viewer may reply. `commentGating` is enforced server-side —
    /// this only decides whether the control is drawn disabled.
    func viewerMayReply(currentUser: CurrentUser?) -> Bool {
        switch commentGating {
        case "none": return false
        case "verified": return currentUser?.user.isVerified ?? false
        case "followers": return viewerFollowsAuthor || author.handle == currentUser?.user.handle
        default: return true
        }
    }
}

// MARK: - Engagement

/// The counts on one work, as the server pushes them on the user stream.
///
/// `work_engagement` is published after every reaction, reply and quote on a
/// work the reader is watching, so a card on screen shows the same numbers as
/// the server without anyone asking for them again. It carries counts only:
/// nothing viewer-relative, because the frame goes to every watcher at once and
/// "did *you* like this" is not the same answer for two of them.
///
/// Every count is optional and applied only when present. The server sends the
/// keys `WorkDTO` has and omits the ones it does not, and a key that did not
/// arrive means "unchanged", never zero.
public struct WorkEngagement: Codable, Hashable, Sendable {
    public let workID: String
    public let likeCount: Int?
    public let dislikeCount: Int?
    public let replyCount: Int?
    public let repostCount: Int?
    public let quoteCount: Int?
    public let bookmarkCount: Int?
    public let viewCount: Int?
    public let tipTotalUAET: Int64?

    enum CodingKeys: String, CodingKey {
        case workID = "work_id"
        case likeCount = "like_count"
        case dislikeCount = "dislike_count"
        case replyCount = "reply_count"
        case repostCount = "repost_count"
        case quoteCount = "quote_count"
        case bookmarkCount = "bookmark_count"
        case viewCount = "view_count"
        case tipTotalUAET = "tip_total_uaet"
    }

    public init(
        workID: String,
        likeCount: Int? = nil,
        dislikeCount: Int? = nil,
        replyCount: Int? = nil,
        repostCount: Int? = nil,
        quoteCount: Int? = nil,
        bookmarkCount: Int? = nil,
        viewCount: Int? = nil,
        tipTotalUAET: Int64? = nil
    ) {
        self.workID = workID
        self.likeCount = likeCount
        self.dislikeCount = dislikeCount
        self.replyCount = replyCount
        self.repostCount = repostCount
        self.quoteCount = quoteCount
        self.bookmarkCount = bookmarkCount
        self.viewCount = viewCount
        self.tipTotalUAET = tipTotalUAET
    }
}

public extension Work {
    /// This work with the pushed counts written over its own.
    ///
    /// Pure, and deliberately narrow: it touches the seven counts and the tip
    /// total and nothing else. A frame naming a different work is not applied —
    /// the caller matches by id, and this refuses to be the place that gets it
    /// wrong. Viewer state (`likedByViewer` and its siblings) is untouched
    /// because the frame does not carry it: the reader's own answer changes
    /// only through their own action, which comes back as a whole row.
    func applying(_ engagement: WorkEngagement) -> Work {
        guard engagement.workID == id else { return self }
        var copy = self
        if let v = engagement.likeCount { copy.likeCount = v }
        if let v = engagement.dislikeCount { copy.dislikeCount = v }
        if let v = engagement.replyCount { copy.replyCount = v }
        if let v = engagement.repostCount { copy.repostCount = v }
        if let v = engagement.quoteCount { copy.quoteCount = v }
        if let v = engagement.bookmarkCount { copy.bookmarkCount = v }
        if let v = engagement.viewCount { copy.viewCount = v }
        if let v = engagement.tipTotalUAET { copy.tipTotalUAET = v }
        return copy
    }
}

// MARK: - Nested types

/// The display identity attached to a work.
///
/// Deliberately smaller than `User`: a feed card needs a name, an avatar and the
/// badges, and asking the server for a full profile projection per row would
/// cost a join the feed query does not otherwise need.
public struct WorkAuthor: Codable, Hashable, Sendable, Identifiable {
    public var id: String { handle }

    public let handle: String
    public let displayName: String
    public let avatarURL: String?
    public let isVerified: Bool
    public let isCreator: Bool
    /// "" | government | business
    public let officialType: String?
    /// user | creator | admin | founder
    public let role: String
    public let realm: Int

    enum CodingKeys: String, CodingKey {
        case handle, role, realm
        case displayName = "display_name"
        case avatarURL = "avatar_url"
        case isVerified = "is_verified"
        case isCreator = "is_creator"
        case officialType = "official_type"
    }

    public init(
        handle: String,
        displayName: String,
        avatarURL: String? = nil,
        isVerified: Bool = false,
        isCreator: Bool = false,
        officialType: String? = nil,
        role: String = "user",
        realm: Int = 1
    ) {
        self.handle = handle
        self.displayName = displayName
        self.avatarURL = avatarURL
        self.isVerified = isVerified
        self.isCreator = isCreator
        self.officialType = officialType
        self.role = role
        self.realm = realm
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        handle = try c.decode(String.self, forKey: .handle)
        displayName = try c.decodeIfPresent(String.self, forKey: .displayName) ?? ""
        avatarURL = try c.decodeIfPresent(String.self, forKey: .avatarURL)
        isVerified = try c.decodeIfPresent(Bool.self, forKey: .isVerified) ?? false
        isCreator = try c.decodeIfPresent(Bool.self, forKey: .isCreator) ?? false
        officialType = try c.decodeIfPresent(String.self, forKey: .officialType)
        role = try c.decodeIfPresent(String.self, forKey: .role) ?? "user"
        realm = try c.decodeIfPresent(Int.self, forKey: .realm) ?? 1
    }
}

/// The HLS stream for a video work.
public struct WorkVideo: Codable, Hashable, Sendable {
    /// The clean master playlist. This is what the in-app player is given; the
    /// moving-watermark rendition exists for files that leave the platform.
    public let masterURL: String
    public let posterURL: String?
    public let durationSecs: Double
    public let width: Int
    public let height: Int

    enum CodingKeys: String, CodingKey {
        case masterURL = "master_url"
        case posterURL = "poster_url"
        case durationSecs = "duration_secs"
        case width, height
    }

    public init(masterURL: String, posterURL: String? = nil, durationSecs: Double = 0, width: Int = 0, height: Int = 0) {
        self.masterURL = masterURL
        self.posterURL = posterURL
        self.durationSecs = durationSecs
        self.width = width
        self.height = height
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        masterURL = try c.decode(String.self, forKey: .masterURL)
        posterURL = try c.decodeIfPresent(String.self, forKey: .posterURL)
        durationSecs = try c.decodeIfPresent(Double.self, forKey: .durationSecs) ?? 0
        width = try c.decodeIfPresent(Int.self, forKey: .width) ?? 0
        height = try c.decodeIfPresent(Int.self, forKey: .height) ?? 0
    }

    /// Falls back to 16:9 when the server has no dimensions yet — a transcode
    /// still in flight should not collapse the player to zero height.
    public var aspectRatio: Double {
        guard width > 0, height > 0 else { return 16.0 / 9.0 }
        return Double(width) / Double(height)
    }

    /// Portrait video is shown in a narrow centred column rather than
    /// full-bleed, matching `.pcd-media-portrait` on the web.
    public var isPortrait: Bool { aspectRatio < 1 }
}

/// A voice work's audio.
public struct WorkVoice: Codable, Hashable, Sendable {
    public let url: String
    public let durationSecs: Double

    enum CodingKeys: String, CodingKey {
        case url
        case durationSecs = "duration_secs"
    }

    public init(url: String, durationSecs: Double) {
        self.url = url
        self.durationSecs = durationSecs
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        url = try c.decode(String.self, forKey: .url)
        durationSecs = try c.decodeIfPresent(Double.self, forKey: .durationSecs) ?? 0
    }
}

/// The work a quote cites.
///
/// A separate type rather than a recursive `Work` because the server bounds the
/// chain: the loader attaches exactly two levels of quotes, so a quote carries
/// at most the work it cites and, under that, the work *that* one cites —
/// `nested`, which the web draws as a bare rail inside the quote card. Nothing
/// deeper is ever loaded, so nothing deeper is ever on the wire, and a third
/// level is not something this type can express.
public struct QuotedWork: Codable, Hashable, Sendable, Identifiable {
    public let id: String
    public let cid: String
    public let author: WorkAuthor
    public let body: String
    public let createdAt: Date
    public let mediaURLs: [String]
    public let video: WorkVideo?
    /// Quoted media stays behind a chip when it is flagged — the embed has no
    /// viewer context to run a full gate against.
    public let isNSFW: Bool
    public let isGore: Bool
    /// The second and last level of the chain, when the quoted work is itself a
    /// quote. Boxed: see `QuotedWorkRef`.
    public let nested: QuotedWorkRef?

    enum CodingKeys: String, CodingKey {
        case id, cid, author, body, video, nested
        case createdAt = "created_at"
        case mediaURLs = "media_urls"
        case isNSFW = "is_nsfw"
        case isGore = "is_gore"
    }

    public init(
        id: String,
        cid: String,
        author: WorkAuthor,
        body: String,
        createdAt: Date,
        mediaURLs: [String] = [],
        video: WorkVideo? = nil,
        isNSFW: Bool = false,
        isGore: Bool = false,
        nested: QuotedWorkRef? = nil
    ) {
        self.id = id
        self.cid = cid
        self.author = author
        self.body = body
        self.createdAt = createdAt
        self.mediaURLs = mediaURLs
        self.video = video
        self.isNSFW = isNSFW
        self.isGore = isGore
        self.nested = nested
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        cid = try c.decode(String.self, forKey: .cid)
        author = try c.decode(WorkAuthor.self, forKey: .author)
        body = try c.decodeIfPresent(String.self, forKey: .body) ?? ""
        createdAt = try c.decode(Date.self, forKey: .createdAt)
        mediaURLs = try c.decodeIfPresent([String].self, forKey: .mediaURLs) ?? []
        video = try c.decodeIfPresent(WorkVideo.self, forKey: .video)
        isNSFW = try c.decodeIfPresent(Bool.self, forKey: .isNSFW) ?? false
        isGore = try c.decodeIfPresent(Bool.self, forKey: .isGore) ?? false
        nested = try c.decodeIfPresent(QuotedWorkRef.self, forKey: .nested)
    }

    public var hasMedia: Bool { video != nil || !mediaURLs.isEmpty }
    public var isFlagged: Bool { isNSFW || isGore }
    /// The quoted work's own quote, unboxed.
    public var nestedWork: QuotedWork? { nested?.work }
}

/// One more level of a quote chain: the work the quoted work itself quotes.
///
/// A class rather than a struct only because a struct cannot hold a value of
/// its own type — the reference is the indirection the recursion needs, and
/// that is the whole reason this type exists. On the wire it is transparent:
/// `nested` is a plain `QuotedWork` object, and this decodes and encodes it as
/// one, so the JSON has no extra layer for a box that is Swift's concern alone.
public final class QuotedWorkRef: Codable, Hashable, Sendable {
    public let work: QuotedWork

    public init(_ work: QuotedWork) { self.work = work }

    public init(from decoder: any Decoder) throws {
        work = try QuotedWork(from: decoder)
    }

    public func encode(to encoder: any Encoder) throws {
        try work.encode(to: encoder)
    }

    public static func == (lhs: QuotedWorkRef, rhs: QuotedWorkRef) -> Bool { lhs.work == rhs.work }
    public func hash(into hasher: inout Hasher) { hasher.combine(work) }
}

/// Open Graph metadata for the first external link in a body.
public struct LinkPreview: Codable, Hashable, Sendable {
    public let url: String
    public let title: String
    public let description: String?
    public let imageURL: String?
    public let siteName: String?

    enum CodingKeys: String, CodingKey {
        case url, title, description
        case imageURL = "image_url"
        case siteName = "site_name"
    }

    public init(url: String, title: String, description: String? = nil, imageURL: String? = nil, siteName: String? = nil) {
        self.url = url
        self.title = title
        self.description = description
        self.imageURL = imageURL
        self.siteName = siteName
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        url = try c.decode(String.self, forKey: .url)
        title = try c.decodeIfPresent(String.self, forKey: .title) ?? ""
        description = try c.decodeIfPresent(String.self, forKey: .description)
        imageURL = try c.decodeIfPresent(String.self, forKey: .imageURL)
        siteName = try c.decodeIfPresent(String.self, forKey: .siteName)
    }

    /// The host, for the small caption line under the card.
    public var host: String {
        URL(string: url)?.host?.replacingOccurrences(of: "www.", with: "") ?? url
    }
}
