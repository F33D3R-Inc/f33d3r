import Foundation

/// One broadcast as the server knows it. Carries no video URL until the
/// media path exists; `hasVideo` says whether one is flowing, and
/// `videoUnavailable` is the server's own sentence for why not.
public struct LiveStream: Codable, Hashable, Sendable, Identifiable {
    public let id: String
    public let author: WorkAuthor
    public let title: String
    public let description: String?
    /// idle | live | ended
    public let status: String
    public let startedAt: Date?
    public let endedAt: Date?
    public let viewerCount: Int
    public let isNSFW: Bool
    /// everyone | subscribers
    public let audience: String
    public let lane: String?
    public let tipGoalUAET: Int64
    public let tipTotalUAET: Int64
    public let pinnedBody: String?
    public let heartCount: Int
    public let hasVideo: Bool
    public let videoUnavailable: String?
    /// Owner-only.
    public let topTippers: [Tipper]

    public struct Tipper: Codable, Hashable, Sendable, Identifiable {
        public var id: String { author.handle }
        public let author: WorkAuthor
        public let amountUAET: Int64

        enum CodingKeys: String, CodingKey {
            case author
            case amountUAET = "amount_uaet"
        }
    }

    enum CodingKeys: String, CodingKey {
        case id, author, title, description, status, audience, lane
        case startedAt = "started_at"
        case endedAt = "ended_at"
        case viewerCount = "viewer_count"
        case isNSFW = "is_nsfw"
        case tipGoalUAET = "tip_goal_uaet"
        case tipTotalUAET = "tip_total_uaet"
        case pinnedBody = "pinned_body"
        case heartCount = "heart_count"
        case hasVideo = "has_video"
        case videoUnavailable = "video_unavailable"
        case topTippers = "top_tippers"
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        author = try c.decode(WorkAuthor.self, forKey: .author)
        title = try c.decodeIfPresent(String.self, forKey: .title) ?? ""
        description = try c.decodeIfPresent(String.self, forKey: .description)
        status = try c.decodeIfPresent(String.self, forKey: .status) ?? "live"
        startedAt = try c.decodeIfPresent(Date.self, forKey: .startedAt)
        endedAt = try c.decodeIfPresent(Date.self, forKey: .endedAt)
        viewerCount = try c.decodeIfPresent(Int.self, forKey: .viewerCount) ?? 0
        isNSFW = try c.decodeIfPresent(Bool.self, forKey: .isNSFW) ?? false
        audience = try c.decodeIfPresent(String.self, forKey: .audience) ?? "everyone"
        lane = try c.decodeIfPresent(String.self, forKey: .lane)
        tipGoalUAET = try c.decodeIfPresent(Int64.self, forKey: .tipGoalUAET) ?? 0
        tipTotalUAET = try c.decodeIfPresent(Int64.self, forKey: .tipTotalUAET) ?? 0
        pinnedBody = try c.decodeIfPresent(String.self, forKey: .pinnedBody)
        heartCount = try c.decodeIfPresent(Int.self, forKey: .heartCount) ?? 0
        hasVideo = try c.decodeIfPresent(Bool.self, forKey: .hasVideo) ?? false
        videoUnavailable = try c.decodeIfPresent(String.self, forKey: .videoUnavailable)
        topTippers = try c.decodeIfPresent([Tipper].self, forKey: .topTippers) ?? []
    }

    public var isLive: Bool { status == "live" }

    /// "Music lane" / "Music lane · 312 watching" — the label under the host.
    public var laneLabel: String? {
        guard let lane, !lane.isEmpty else { return nil }
        return lane.prefix(1).uppercased() + lane.dropFirst() + " lane"
    }
}

/// One line in a room: someone said something, or tipped.
public struct LiveChatMessage: Codable, Hashable, Sendable, Identifiable {
    public let id: String
    /// chat | tip | system
    public let kind: String
    public let author: WorkAuthor?
    public let body: String
    public let amountUAET: Int64?
    public let createdAt: Date

    enum CodingKeys: String, CodingKey {
        case id, kind, author, body
        case amountUAET = "amount_uaet"
        case createdAt = "created_at"
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        kind = try c.decodeIfPresent(String.self, forKey: .kind) ?? "chat"
        author = try c.decodeIfPresent(WorkAuthor.self, forKey: .author)
        body = try c.decodeIfPresent(String.self, forKey: .body) ?? ""
        amountUAET = try c.decodeIfPresent(Int64.self, forKey: .amountUAET)
        createdAt = try c.decodeIfPresent(Date.self, forKey: .createdAt) ?? Date()
    }

    public var isTip: Bool { kind == "tip" }
}

/// `GET /api/v1/live/{id}`.
public struct LiveRoom: Codable, Hashable, Sendable {
    public let stream: LiveStream
    public let chat: [LiveChatMessage]

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        stream = try c.decode(LiveStream.self, forKey: .stream)
        chat = try c.decodeIfPresent([LiveChatMessage].self, forKey: .chat) ?? []
    }
}

/// `GET /api/v1/live`.
public struct LiveList: Codable, Hashable, Sendable {
    public let streams: [LiveStream]
    public let count: Int
    /// The reader's own open room, if they are broadcasting.
    public let own: LiveStream?

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        streams = try c.decodeIfPresent([LiveStream].self, forKey: .streams) ?? []
        count = try c.decodeIfPresent(Int.self, forKey: .count) ?? streams.count
        own = try c.decodeIfPresent(LiveStream.self, forKey: .own)
    }
}

/// What the broadcaster sees when they end.
public struct LiveSummary: Codable, Hashable, Sendable {
    public let durationSecs: Int64
    public let peakViewers: Int
    public let tipsUAET: Int64
    public let newFollowers: Int
    public let chatLines: Int
    public let hearts: Int
    public let replaySaved: Bool

    enum CodingKeys: String, CodingKey {
        case hearts
        case durationSecs = "duration_secs"
        case peakViewers = "peak_viewers"
        case tipsUAET = "tips_uaet"
        case newFollowers = "new_followers"
        case chatLines = "chat_lines"
        case replaySaved = "replay_saved"
    }
}

/// The go-live sheet's choices, as `live_start` takes them.
public struct LiveStartRequest: Sendable, Equatable {
    public var title = ""
    public var description = ""
    /// everyone | subscribers
    public var audience = "everyone"
    /// "" or a lane id such as `music`.
    public var lane = ""
    /// Whole AET; 0 means no goal.
    public var tipGoalAET = 200
    public var tipsEnabled = true
    public var notifyFollowers = true
    public var saveReplay = true
    public var isNSFW = false

    public init() {}

    public var isValid: Bool {
        !title.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty && title.count <= 120
    }
}
