import Foundation

/// A tag and how many live works carry it.
public struct TagCount: Codable, Hashable, Sendable, Identifiable {
    public var id: String { tag }
    public let tag: String
    public let count: Int

    public init(tag: String, count: Int) {
        self.tag = tag
        self.count = count
    }
}

/// A page of people: a follower or following list.
public struct UserPage: Codable, Hashable, Sendable {
    public let users: [User]
    public let nextCursor: String?
    /// Handles among `users` the viewer follows.
    public let viewerFollows: Set<String>

    enum CodingKeys: String, CodingKey {
        case users
        case nextCursor = "next_cursor"
        case viewerFollows = "viewer_follows"
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        users = try c.decodeIfPresent([User].self, forKey: .users) ?? []
        let cursor = try c.decodeIfPresent(String.self, forKey: .nextCursor)
        nextCursor = (cursor?.isEmpty ?? true) ? nil : cursor
        viewerFollows = Set(try c.decodeIfPresent([String].self, forKey: .viewerFollows) ?? [])
    }

    public func encode(to encoder: any Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(users, forKey: .users)
        try c.encodeIfPresent(nextCursor, forKey: .nextCursor)
        try c.encode(Array(viewerFollows), forKey: .viewerFollows)
    }
}

/// One signed-in device.
public struct SessionInfo: Codable, Hashable, Sendable, Identifiable {
    public let id: String
    public let deviceName: String
    public let ipAddress: String?
    public let createdAt: Date
    public let lastSeenAt: Date
    public let isCurrent: Bool

    enum CodingKeys: String, CodingKey {
        case id
        case deviceName = "device_name"
        case ipAddress = "ip_address"
        case createdAt = "created_at"
        case lastSeenAt = "last_seen_at"
        case isCurrent = "is_current"
    }
}

/// Who may see a birthday. The four the web's select offers, by the same ids.
public enum BirthdayVisibility: String, CaseIterable, Sendable, Identifiable {
    case everyone
    case followers
    case mutualFollowers = "mutual_followers"
    case onlyMe = "only_me"

    public var id: String { rawValue }

    public var title: String {
        switch self {
        case .everyone: return "Everyone"
        case .followers: return "Your followers"
        case .mutualFollowers: return "People you follow"
        case .onlyMe: return "Only you"
        }
    }

    /// The server's value, or `everyone` for a string this build does not know.
    public static func named(_ raw: String?) -> BirthdayVisibility {
        raw.flatMap(BirthdayVisibility.init(rawValue:)) ?? .everyone
    }
}

/// One place a creator can be found, and one place they can be paid.
///
/// Both lists are the server's, in the server's order, so the editor draws the
/// same rows the web's form does and posts the same keys. Normalisation — a
/// bare username into a URL, an address checked for shape — happens on the far
/// side; a regex copied onto the device is a second definition of the rule that
/// will drift from the first.
public enum ProfileLinks {
    public static let social: [(key: String, label: String, placeholder: String)] = [
        ("youtube", "YouTube", "YouTube URL or username"),
        ("twitch", "Twitch", "Twitch URL or username"),
        ("spotify", "Spotify", "Spotify URL or username"),
        ("soundcloud", "SoundCloud", "SoundCloud URL or username"),
        ("kick", "Kick", "Kick URL or username"),
        ("other", "Other link", "any URL"),
    ]

    public static let payment: [(key: String, label: String, prefix: String, placeholder: String)] = [
        ("cashapp", "Cash App", "$", "username"),
        ("venmo", "Venmo", "@", "username"),
        ("paypal", "PayPal", "paypal.me/", "username"),
        ("kofi", "Ko-fi", "ko-fi.com/", "username"),
        ("buymeacoffee", "Buy Me a Coffee", "buymeacoffee.com/", "username"),
        ("bitcoin", "Bitcoin (BTC)", "", "1A1zP1… or bc1q…"),
        ("xrp", "XRP", "", "rXXXX…"),
    ]
}

/// The fields the edit-profile screen can change. Nil leaves a field alone.
public struct ProfileUpdate: Sendable, Equatable {
    public var displayName: String?
    public var bio: String?
    public var pronouns: String?
    public var location: String?
    public var website: String?
    public var accentHex: String?
    public var themeID: String?
    public var avatarURL: String?
    public var headerURL: String?

    /// Marks the whole profile adult: every work behind age verification.
    public var isAdultCreator: Bool?
    public var birthdayMdVisibility: String?
    public var birthdayYearVisibility: String?
    /// ISO-3166 alpha-2, alongside the location.
    public var countryCode: String?
    /// Sent whole when any one of them changed: the server replaces the map,
    /// so a partial send would clear the links the editor did not touch.
    public var socialLinks: [String: String]?
    public var externalTipLinks: [String: String]?

    public init() {}

    /// Only the fields that differ from `user`, so a save changes what the
    /// reader touched and nothing else.
    public static func diff(from user: User, displayName: String, bio: String, pronouns: String, location: String, website: String, accentHex: String, avatarURL: String?, headerURL: String?) -> ProfileUpdate {
        var u = ProfileUpdate()
        if displayName != user.displayName { u.displayName = displayName }
        if bio != (user.bio ?? "") { u.bio = bio }
        if pronouns != (user.pronouns ?? "") { u.pronouns = pronouns }
        if location != (user.location ?? "") { u.location = location }
        if website != (user.website ?? "") { u.website = website }
        if accentHex != (user.accentHex ?? "") { u.accentHex = accentHex }
        if let avatarURL, avatarURL != (user.avatarURL ?? "") { u.avatarURL = avatarURL }
        if let headerURL, headerURL != (user.headerURL ?? "") { u.headerURL = headerURL }
        return u
    }

    public var isEmpty: Bool {
        [displayName, bio, pronouns, location, website, accentHex, themeID, avatarURL, headerURL,
         birthdayMdVisibility, birthdayYearVisibility, countryCode].allSatisfy { $0 == nil }
            && isAdultCreator == nil
            && socialLinks == nil
            && externalTipLinks == nil
    }

    /// The `profile_update` event exactly as it goes on the wire.
    ///
    /// A struct rather than a hand-built dictionary so the coding keys are
    /// declared once and a test can read them: the server matches on these
    /// strings, and a typo in one of them is a field that silently never
    /// arrives. Optionals are omitted by the synthesised encoder, which is the
    /// "nil leaves it alone" rule holding at the encoding layer rather than at
    /// every call site.
    public struct Event: Encodable, Sendable, Equatable {
        public let eventType = "profile_update"
        public var displayName: String?
        public var bio: String?
        public var pronouns: String?
        public var location: String?
        public var website: String?
        public var accentHex: String?
        public var themeID: String?
        public var avatarURL: String?
        public var headerURL: String?
        public var isAdultCreator: Bool?
        public var birthdayMdVisibility: String?
        public var birthdayYearVisibility: String?
        public var countryCode: String?
        public var socialLinks: [String: String]?
        public var externalTipLinks: [String: String]?

        enum CodingKeys: String, CodingKey {
            case eventType = "event_type"
            case displayName = "display_name"
            case bio, pronouns, location, website
            case accentHex = "accent_hex"
            case themeID = "theme_id"
            case avatarURL = "avatar_url"
            case headerURL = "header_url"
            case isAdultCreator = "is_adult_creator"
            case birthdayMdVisibility = "birthday_md_visibility"
            case birthdayYearVisibility = "birthday_year_visibility"
            case countryCode = "country_code"
            case socialLinks = "social_links"
            case externalTipLinks = "external_tip_links"
        }
    }

    public func event() -> Event {
        var e = Event()
        e.displayName = displayName
        e.bio = bio
        e.pronouns = pronouns
        e.location = location
        e.website = website
        e.accentHex = accentHex
        e.themeID = themeID
        e.avatarURL = avatarURL
        e.headerURL = headerURL
        e.isAdultCreator = isAdultCreator
        e.birthdayMdVisibility = birthdayMdVisibility
        e.birthdayYearVisibility = birthdayYearVisibility
        e.countryCode = countryCode
        e.socialLinks = socialLinks
        e.externalTipLinks = externalTipLinks
        return e
    }
}

/// The web's `settings.privacy.*` switches.
public enum AccountSetting: String, Sendable {
    case showSensitive = "settings.privacy.show_sensitive"
    case celebrations = "settings.privacy.celebrations"
    case accountPrivate = "settings.privacy.account_private"
    case contentSetting = "settings.privacy.content_setting"
}
