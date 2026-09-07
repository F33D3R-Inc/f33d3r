import Foundation

/// One page of works.
///
/// Pagination is cursor-based rather than offset-based because the feed is
/// ranked and live: an offset would skip or repeat rows every time something new
/// landed above the window. `nextCursor` is opaque — the server decides what it
/// encodes, and the client only ever hands it back.
public struct WorkPage: Codable, Hashable, Sendable {
    public let works: [Work]
    public let nextCursor: String?

    /// How many rooms are live right now, for the badge on the Live lane.
    ///
    /// Page metadata rather than a call of its own: the number is cheap for the
    /// server to attach to a feed response it is already building, and asking
    /// for it separately would mean a second request per lane switch.
    ///
    /// Optional because no handler serves it yet. Absent means "unknown", which
    /// draws no badge — a badge reading zero would claim nobody is live.
    public let liveCount: Int?

    enum CodingKeys: String, CodingKey {
        case works
        case nextCursor = "next_cursor"
        case liveCount = "live_count"
    }

    public init(works: [Work], nextCursor: String? = nil, liveCount: Int? = nil) {
        self.works = works
        self.nextCursor = nextCursor
        self.liveCount = liveCount
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        works = try c.decodeIfPresent([Work].self, forKey: .works) ?? []
        // An absent cursor and an empty one both mean "no more pages"; collapse
        // them here so callers only have to check for nil.
        let cursor = try c.decodeIfPresent(String.self, forKey: .nextCursor)
        nextCursor = (cursor?.isEmpty ?? true) ? nil : cursor
        liveCount = try c.decodeIfPresent(Int.self, forKey: .liveCount)
    }

    public var hasMore: Bool { nextCursor != nil }
}

/// One lane of the feed — a ranking surface the reader can move between.
///
/// The server owns what each one means and receives the raw value as `surface`.
/// `foryou` is AethyrRank's ordering; `following` is chronological among
/// accounts the viewer follows; the rest are filtered surfaces.
///
/// `following` is first in the strip and is `default` because a feed that
/// reopens on the algorithmic lane is a feed the reader did not choose — the
/// one complaint this layout exists to fix.
public enum FeedSurface: String, CaseIterable, Sendable, Hashable, Identifiable {
    case following
    case forYou = "foryou"
    /// What the platform is reading right now. It was only ever reachable
    /// inside Explore, which is a strange place for a feed people ask for by
    /// name.
    case trending
    /// The scoreboard lane. `sports` is a row in feed_surfaces carrying two
    /// dozen league tags, so this is a feed the platform defines rather than a
    /// tag somebody typed — which is exactly why it is a case here.
    case sports
    /// The 18+ feed. Hard-gated by the server, which answers
    /// `403 adult_content_disabled` to an account that has not turned adult
    /// content on, so the lane is only offered where it can be opened.
    case nsfw
    case music
    case visions
    case live

    public static let `default`: FeedSurface = .following

    public var id: String { rawValue }

    /// The lane label. Matches the web's feed switcher wording where the two
    /// surfaces overlap.
    public var title: String {
        switch self {
        case .following: return "Following"
        case .forYou: return "For you"
        case .trending: return "Trending"
        case .sports: return "Sports"
        case .nsfw: return "18+"
        case .music: return "Music"
        case .visions: return "Visions"
        case .live: return "Live"
        }
    }

    /// Only Live carries a count beside its label; the others have no number
    /// that changes often enough to be worth a badge.
    public var showsLiveCount: Bool { self == .live }

    /// Whether an account has to be cleared for adult content before this lane
    /// is worth offering. Only the reader's own settings decide; the server
    /// refuses it regardless, and this is what keeps a lane the reader cannot
    /// open out of the strip in the first place.
    public var requiresAdultContent: Bool { self == .nsfw }
}

/// A work with its conversation, as the detail screen needs it.
public struct WorkThread: Codable, Hashable, Sendable {
    /// The work the screen is about.
    public let work: Work
    /// Ancestors from the root down to the direct parent, so the detail screen
    /// can show what is being replied to without walking `parent_cid` itself.
    public let ancestors: [Work]
    public let replies: [Work]
    public let repliesCursor: String?

    enum CodingKeys: String, CodingKey {
        case work, ancestors, replies
        case repliesCursor = "replies_cursor"
    }

    public init(work: Work, ancestors: [Work] = [], replies: [Work] = [], repliesCursor: String? = nil) {
        self.work = work
        self.ancestors = ancestors
        self.replies = replies
        self.repliesCursor = repliesCursor
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        work = try c.decode(Work.self, forKey: .work)
        ancestors = try c.decodeIfPresent([Work].self, forKey: .ancestors) ?? []
        replies = try c.decodeIfPresent([Work].self, forKey: .replies) ?? []
        let cursor = try c.decodeIfPresent(String.self, forKey: .repliesCursor)
        repliesCursor = (cursor?.isEmpty ?? true) ? nil : cursor
    }
}

/// A profile: the user plus the viewer's relationship to them.
///
/// The relationship flags live here rather than on `User` because they are only
/// meaningful in the context of a viewer, and `User` is also used to describe
/// people in lists where no such context exists.
public struct Profile: Codable, Hashable, Sendable {
    public let user: User
    public let viewerFollows: Bool
    public let followsViewer: Bool
    public let viewerIsBlocked: Bool
    /// Set when the author has pinned a work to the top of their profile.
    public let pinnedWork: Work?

    enum CodingKeys: String, CodingKey {
        case user
        case viewerFollows = "viewer_follows"
        case followsViewer = "follows_viewer"
        case viewerIsBlocked = "viewer_is_blocked"
        case pinnedWork = "pinned_work"
    }

    public init(
        user: User,
        viewerFollows: Bool = false,
        followsViewer: Bool = false,
        viewerIsBlocked: Bool = false,
        pinnedWork: Work? = nil
    ) {
        self.user = user
        self.viewerFollows = viewerFollows
        self.followsViewer = followsViewer
        self.viewerIsBlocked = viewerIsBlocked
        self.pinnedWork = pinnedWork
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        user = try c.decode(User.self, forKey: .user)
        viewerFollows = try c.decodeIfPresent(Bool.self, forKey: .viewerFollows) ?? false
        followsViewer = try c.decodeIfPresent(Bool.self, forKey: .followsViewer) ?? false
        viewerIsBlocked = try c.decodeIfPresent(Bool.self, forKey: .viewerIsBlocked) ?? false
        pinnedWork = try c.decodeIfPresent(Work.self, forKey: .pinnedWork)
    }

    /// Which tab of a profile's works to load. The server filters; the client
    /// only names the slice it wants.
    public enum Tab: String, CaseIterable, Sendable {
        case works
        case replies
        case media
        case likes
        case saves

        public var title: String {
            switch self {
            case .works: return "Works"
            case .replies: return "Replies"
            case .media: return "Media"
            case .likes: return "Likes"
            case .saves: return "Saves"
            }
        }

        /// Likes and saves are the owner's alone.
        public var isPrivate: Bool { self == .likes || self == .saves }

        /// The tabs to draw on a profile.
        ///
        /// Likes are private on this platform — the server answers 403
        /// `likes_private` to anyone but the owner — so the tab is not shown on
        /// someone else's profile at all. A tab that exists only to say "you
        /// may not look at this" advertises a surface nobody can reach and
        /// invites the tap that proves it.
        public static func visible(isOwner: Bool) -> [Tab] {
            isOwner ? allCases : allCases.filter { !$0.isPrivate }
        }
    }
}
