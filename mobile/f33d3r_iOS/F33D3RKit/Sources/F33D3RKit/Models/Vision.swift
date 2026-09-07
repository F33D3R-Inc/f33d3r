import Foundation

/// How a text vision is drawn. Preset KEYS resolved by the server, never
/// styles chosen by a client — a vision looks the same to every viewer.
public struct VisionArtboard: Codable, Hashable, Sendable {
    /// void | ember | tide | bloom | pulp | signal
    public let background: String
    /// grotesk | serif | mono | display
    public let typeface: String
    /// s | m | l | xl — already resolved from `auto` by the server.
    public let typeScale: String
    /// left | center | right
    public let align: String

    enum CodingKeys: String, CodingKey {
        case background, typeface, align
        case typeScale = "type_scale"
    }

    public init(background: String = "void", typeface: String = "grotesk", typeScale: String = "l", align: String = "center") {
        self.background = background
        self.typeface = typeface
        self.typeScale = typeScale
        self.align = align
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        background = try c.decodeIfPresent(String.self, forKey: .background) ?? "void"
        typeface = try c.decodeIfPresent(String.self, forKey: .typeface) ?? "grotesk"
        typeScale = try c.decodeIfPresent(String.self, forKey: .typeScale) ?? "l"
        align = try c.decodeIfPresent(String.self, forKey: .align) ?? "center"
    }

    public static let backgrounds = ["void", "ember", "tide", "bloom", "pulp", "signal"]
    public static let typefaces = ["grotesk", "serif", "mono", "display"]
}

/// One 24-hour post.
public struct Vision: Codable, Hashable, Sendable, Identifiable {
    public let id: String
    public let author: WorkAuthor
    /// text | image
    public let contentType: String
    public let body: String
    public let artboard: VisionArtboard
    public let mediaURLs: [String]
    public let poll: Poll?
    public let isNSFW: Bool
    /// everyone | subscribers
    public let audience: String
    public let allowReplies: Bool
    public let createdAt: Date
    public let expiresAt: Date
    public let seen: Bool
    /// Only the author receives this.
    public let viewCount: Int?

    enum CodingKeys: String, CodingKey {
        case id, author, body, artboard, poll, audience, seen
        case contentType = "content_type"
        case mediaURLs = "media_urls"
        case isNSFW = "is_nsfw"
        case allowReplies = "allow_replies"
        case createdAt = "created_at"
        case expiresAt = "expires_at"
        case viewCount = "view_count"
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        author = try c.decode(WorkAuthor.self, forKey: .author)
        contentType = try c.decodeIfPresent(String.self, forKey: .contentType) ?? "text"
        body = try c.decodeIfPresent(String.self, forKey: .body) ?? ""
        artboard = try c.decodeIfPresent(VisionArtboard.self, forKey: .artboard) ?? VisionArtboard()
        mediaURLs = try c.decodeIfPresent([String].self, forKey: .mediaURLs) ?? []
        poll = try c.decodeIfPresent(Poll.self, forKey: .poll)
        isNSFW = try c.decodeIfPresent(Bool.self, forKey: .isNSFW) ?? false
        audience = try c.decodeIfPresent(String.self, forKey: .audience) ?? "everyone"
        allowReplies = try c.decodeIfPresent(Bool.self, forKey: .allowReplies) ?? true
        createdAt = try c.decode(Date.self, forKey: .createdAt)
        expiresAt = try c.decode(Date.self, forKey: .expiresAt)
        seen = try c.decodeIfPresent(Bool.self, forKey: .seen) ?? false
        viewCount = try c.decodeIfPresent(Int.self, forKey: .viewCount)
    }

    public var isImage: Bool { contentType == "image" && !mediaURLs.isEmpty }

    /// The same vision marked seen, for mirroring a confirmed view into the
    /// tray without refetching.
    public func markedSeen() -> Vision {
        Vision(copying: self, seen: true)
    }

    private init(copying f: Vision, seen: Bool) {
        id = f.id; author = f.author; contentType = f.contentType; body = f.body; artboard = f.artboard
        mediaURLs = f.mediaURLs; poll = f.poll; isNSFW = f.isNSFW; audience = f.audience
        allowReplies = f.allowReplies; createdAt = f.createdAt; expiresAt = f.expiresAt
        self.seen = seen; viewCount = f.viewCount
    }
}

/// One author's live visions as the viewer sees them: the circle in the tray.
public struct VisionRing: Codable, Hashable, Sendable, Identifiable {
    public var id: String { author.handle }

    public let author: WorkAuthor
    /// unseen | seen
    public let state: String
    public let count: Int
    public let unseenCount: Int
    public let latestAt: Date
    public let isLive: Bool
    public let provenance: WorkProvenance?
    public let visions: [Vision]

    enum CodingKeys: String, CodingKey {
        case author, state, count, provenance, visions
        case unseenCount = "unseen_count"
        case latestAt = "latest_at"
        case isLive = "is_live"
    }

    public init(author: WorkAuthor, state: String, count: Int, unseenCount: Int, latestAt: Date, isLive: Bool, provenance: WorkProvenance?, visions: [Vision]) {
        self.author = author
        self.state = state
        self.count = count
        self.unseenCount = unseenCount
        self.latestAt = latestAt
        self.isLive = isLive
        self.provenance = provenance
        self.visions = visions
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        author = try c.decode(WorkAuthor.self, forKey: .author)
        state = try c.decodeIfPresent(String.self, forKey: .state) ?? "unseen"
        visions = try c.decodeIfPresent([Vision].self, forKey: .visions) ?? []
        count = try c.decodeIfPresent(Int.self, forKey: .count) ?? visions.count
        unseenCount = try c.decodeIfPresent(Int.self, forKey: .unseenCount) ?? visions.filter { !$0.seen }.count
        latestAt = try c.decodeIfPresent(Date.self, forKey: .latestAt) ?? visions.last?.createdAt ?? .distantPast
        isLive = try c.decodeIfPresent(Bool.self, forKey: .isLive) ?? false
        provenance = try c.decodeIfPresent(WorkProvenance.self, forKey: .provenance)
    }

    public var hasUnseen: Bool { unseenCount > 0 }

    /// Where the viewer opens: the first vision not yet seen, else the first.
    public var startIndex: Int {
        visions.firstIndex { !$0.seen } ?? 0
    }

    /// The same ring with one vision marked seen and the counts recomputed.
    public func marking(seen visionID: String) -> VisionRing {
        let updated = visions.map { $0.id == visionID ? $0.markedSeen() : $0 }
        let unseen = updated.filter { !$0.seen }.count
        return VisionRing(
            author: author, state: unseen > 0 ? "unseen" : "seen", count: updated.count,
            unseenCount: unseen, latestAt: latestAt, isLive: isLive, provenance: provenance, visions: updated
        )
    }
}

/// `GET /api/v1/visions`.
public struct VisionTray: Codable, Hashable, Sendable {
    /// The reader's own ring; nil when they have no live vision.
    public let own: VisionRing?
    public let rings: [VisionRing]

    public init(own: VisionRing? = nil, rings: [VisionRing] = []) {
        self.own = own
        self.rings = rings
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        own = try c.decodeIfPresent(VisionRing.self, forKey: .own)
        rings = try c.decodeIfPresent([VisionRing].self, forKey: .rings) ?? []
    }

    public var isEmpty: Bool { own == nil && rings.isEmpty }
}
