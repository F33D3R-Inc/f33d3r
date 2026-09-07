import Foundation

/// What Herald is allowed to send this account, and when it must stay quiet.
///
/// Mirrors `GET /api/v1/notifications/preferences` and the answer to the
/// `notification_prefs` event. Every field is optional on the wire and defaults
/// to the permissive value, so a deployment that has not shipped a preference
/// yet reads as "on" rather than as a decode failure — the one thing a settings
/// screen must never do is invent a switch position the server did not send.
public struct NotificationPrefs: Codable, Hashable, Sendable {
    public var pushEnabled: Bool
    public var messagesEnabled: Bool
    public var likesEnabled: Bool
    public var repostsEnabled: Bool
    public var repliesEnabled: Bool
    public var followsEnabled: Bool
    public var achievementsEnabled: Bool
    public var mentionsEnabled: Bool
    public var frequenciesEnabled: Bool

    public var quietHoursEnabled: Bool
    /// "HH:MM", the account's own clock. Nil when the server holds no window.
    public var quietHoursStart: String?
    public var quietHoursEnd: String?

    enum CodingKeys: String, CodingKey {
        case pushEnabled = "push_enabled"
        case messagesEnabled = "messages_enabled"
        case likesEnabled = "likes_enabled"
        case repostsEnabled = "reposts_enabled"
        case repliesEnabled = "replies_enabled"
        case followsEnabled = "follows_enabled"
        case achievementsEnabled = "achievements_enabled"
        case mentionsEnabled = "mentions_enabled"
        case frequenciesEnabled = "frequencies_enabled"
        case quietHoursEnabled = "quiet_hours_enabled"
        case quietHoursStart = "quiet_hours_start"
        case quietHoursEnd = "quiet_hours_end"
    }

    public init(
        pushEnabled: Bool = true,
        messagesEnabled: Bool = true,
        likesEnabled: Bool = true,
        repostsEnabled: Bool = true,
        repliesEnabled: Bool = true,
        followsEnabled: Bool = true,
        achievementsEnabled: Bool = true,
        mentionsEnabled: Bool = true,
        frequenciesEnabled: Bool = true,
        quietHoursEnabled: Bool = false,
        quietHoursStart: String? = nil,
        quietHoursEnd: String? = nil
    ) {
        self.pushEnabled = pushEnabled
        self.messagesEnabled = messagesEnabled
        self.likesEnabled = likesEnabled
        self.repostsEnabled = repostsEnabled
        self.repliesEnabled = repliesEnabled
        self.followsEnabled = followsEnabled
        self.achievementsEnabled = achievementsEnabled
        self.mentionsEnabled = mentionsEnabled
        self.frequenciesEnabled = frequenciesEnabled
        self.quietHoursEnabled = quietHoursEnabled
        self.quietHoursStart = quietHoursStart
        self.quietHoursEnd = quietHoursEnd
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        func flag(_ key: CodingKeys, or fallback: Bool) throws -> Bool {
            try c.decodeIfPresent(Bool.self, forKey: key) ?? fallback
        }
        pushEnabled = try flag(.pushEnabled, or: true)
        messagesEnabled = try flag(.messagesEnabled, or: true)
        likesEnabled = try flag(.likesEnabled, or: true)
        repostsEnabled = try flag(.repostsEnabled, or: true)
        repliesEnabled = try flag(.repliesEnabled, or: true)
        followsEnabled = try flag(.followsEnabled, or: true)
        achievementsEnabled = try flag(.achievementsEnabled, or: true)
        mentionsEnabled = try flag(.mentionsEnabled, or: true)
        frequenciesEnabled = try flag(.frequenciesEnabled, or: true)
        quietHoursEnabled = try flag(.quietHoursEnabled, or: false)
        quietHoursStart = try c.decodeIfPresent(String.self, forKey: .quietHoursStart)
        quietHoursEnd = try c.decodeIfPresent(String.self, forKey: .quietHoursEnd)
    }
}

public extension NotificationPrefs {

    /// One switch on the notifications screen, named by the wire key it writes.
    ///
    /// The raw value is the server's field name, so a row cannot post one key
    /// while reading another — the two are the same string.
    enum Toggle: String, CaseIterable, Sendable, Identifiable {
        case push = "push_enabled"
        case messages = "messages_enabled"
        case likes = "likes_enabled"
        case reposts = "reposts_enabled"
        case replies = "replies_enabled"
        case follows = "follows_enabled"
        case achievements = "achievements_enabled"
        case mentions = "mentions_enabled"
        case frequencies = "frequencies_enabled"

        public var id: String { rawValue }

        public var title: String {
            switch self {
            case .push: return "Push notifications"
            case .messages: return "Messages"
            case .likes: return "Likes"
            case .reposts: return "Reposts"
            case .replies: return "Replies"
            case .follows: return "New followers"
            case .achievements: return "Realm milestones"
            case .mentions: return "Mentions"
            case .frequencies: return "Frequencies"
            }
        }
    }

    /// The wire key for quiet hours, so the sheet and the event agree.
    static let quietHoursKey = "quiet_hours_enabled"
    static let quietHoursStartKey = "quiet_hours_start"
    static let quietHoursEndKey = "quiet_hours_end"

    subscript(_ toggle: Toggle) -> Bool {
        switch toggle {
        case .push: return pushEnabled
        case .messages: return messagesEnabled
        case .likes: return likesEnabled
        case .reposts: return repostsEnabled
        case .replies: return repliesEnabled
        case .follows: return followsEnabled
        case .achievements: return achievementsEnabled
        case .mentions: return mentionsEnabled
        case .frequencies: return frequenciesEnabled
        }
    }
}

/// The subset of preferences one flip changes.
///
/// A settings screen sends what the finger touched and nothing else: the
/// server treats an absent key as "leave it alone", so posting the whole object
/// back would let a stale read undo a change made on another device between the
/// two round trips.
public struct NotificationPrefsChange: Encodable, Hashable, Sendable {
    public enum Value: Hashable, Sendable {
        case flag(Bool)
        case text(String)
    }

    public private(set) var changed: [String: Value]

    public init(_ changed: [String: Value] = [:]) {
        self.changed = changed
    }

    /// One switch.
    public static func toggle(_ toggle: NotificationPrefs.Toggle, _ on: Bool) -> Self {
        Self([toggle.rawValue: .flag(on)])
    }

    /// Quiet hours on or off, without touching the window.
    public static func quietHours(enabled: Bool) -> Self {
        Self([NotificationPrefs.quietHoursKey: .flag(enabled)])
    }

    /// The window itself. Both ends are "HH:MM" — a start alone is not a window.
    public static func quietHours(start: String, end: String) -> Self {
        Self([
            NotificationPrefs.quietHoursStartKey: .text(start),
            NotificationPrefs.quietHoursEndKey: .text(end),
        ])
    }

    public var isEmpty: Bool { changed.isEmpty }

    private struct Key: CodingKey {
        let stringValue: String
        var intValue: Int? { nil }
        init(_ stringValue: String) { self.stringValue = stringValue }
        init?(stringValue: String) { self.stringValue = stringValue }
        init?(intValue: Int) { nil }
    }

    public func encode(to encoder: any Encoder) throws {
        var c = encoder.container(keyedBy: Key.self)
        // Sorted so the bytes are the same for the same change — a request
        // that differs only in key order is not a different request, and a
        // test that has to allow for both is a test of nothing.
        for (key, value) in changed.sorted(by: { $0.key < $1.key }) {
            switch value {
            case .flag(let on): try c.encode(on, forKey: Key(key))
            case .text(let text): try c.encode(text, forKey: Key(key))
            }
        }
    }
}
