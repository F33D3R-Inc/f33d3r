import Foundation

/// A user as any client may see them.
///
/// Mirrors `UserDTO` in `feed-engine/internal/handler/apiv1_dto.go`. Fields the
/// server deliberately withholds — the PIAL identity spine, ranking internals —
/// are absent here too, and should stay absent: if a value is needed on device,
/// the fix is a considered addition to the server DTO, not a decode of something
/// that was never meant to leave the backend.
public struct User: Codable, Hashable, Sendable, Identifiable {
    /// Handles are unique and stable enough to identify a user in a list. The
    /// server does not expose account UUIDs, so this is the identity clients get.
    public var id: String { handle }

    public let handle: String
    public let displayName: String
    public let bio: String?
    public let pronouns: String?
    public let location: String?
    public let website: String?
    public let avatarURL: String?
    public let headerURL: String?

    public let isVerified: Bool
    public let isCreator: Bool
    /// "" | "government" | "business"
    public let officialType: String?
    /// "user" | "admin" | "founder"
    public let role: String

    public let followerCount: Int
    public let followingCount: Int
    public let postCount: Int

    /// Public progression level, 1–5.
    public let realm: Int
    /// Server-resolved label for `realm`. Never derive this on device — the
    /// ladder is the server's to name.
    public let realmName: String
    public let xp: Int64

    public let themeID: String?
    public let accentHex: String?

    public let isPrivate: Bool

    enum CodingKeys: String, CodingKey {
        case handle, bio, pronouns, location, website, role, realm, xp
        case displayName = "display_name"
        case avatarURL = "avatar_url"
        case headerURL = "header_url"
        case isVerified = "is_verified"
        case isCreator = "is_creator"
        case officialType = "official_type"
        case followerCount = "follower_count"
        case followingCount = "following_count"
        case postCount = "post_count"
        case realmName = "realm_name"
        case themeID = "theme_id"
        case accentHex = "accent_hex"
        case isPrivate = "is_private"
    }
}

/// The authenticated user's own profile and settings.
///
/// Mirrors `MeDTO`. The server flattens `UserDTO` into this object, so the
/// decoder reads both from the same container rather than a nested key.
public struct CurrentUser: Codable, Hashable, Sendable {
    public let user: User

    /// "safe_mode" | "default" | "adult_enabled"
    public let contentSetting: String
    public let showSensitive: Bool
    /// "free" | "subscriber" | "creator"
    public let tier: String
    public let unreadCount: Int

    public let isAdult: Bool
    public let isMinor: Bool
    public let isAgeVerified: Bool
    public let isAdultCreator: Bool
    public let twoFAEnabled: Bool

    public let celebrationsEnabled: Bool

    /// False for an account created through a channel that set no password —
    /// the change-password form then asks for the new one only, because there
    /// is no current one to prove.
    public let hasPassword: Bool

    /// Who may see the birth date, in two halves: the month and day, and the
    /// year. `everyone | followers | mutual_followers | only_me`; the date
    /// itself is owned by PIAL and cannot be changed here.
    public let birthdayMdVisibility: String
    public let birthdayYearVisibility: String

    /// Where identity verification stands, in the server's own words. Nil on a
    /// deployment that does not run it. Read-only on device: the documents go
    /// to the web flow, and the app reports the state rather than inventing a
    /// second way to submit them.
    public let kycStatus: String?
    public let kycSubmittedAt: Date?
    /// Whether this account can be paid out to.
    public let payoutEnabled: Bool

    /// `youtube | twitch | spotify | soundcloud | kick | other` → URL.
    public let socialLinks: [String: String]
    /// `cashapp | venmo | paypal | kofi | buymeacoffee | bitcoin | xrp` →
    /// handle or address. Shown in the tip dialog, never on a public profile.
    public let externalTipLinks: [String: String]

    /// ISO-3166 alpha-2, set alongside the location.
    public let countryCode: String?

    /// Whether the account may choose "Adult enabled" content.
    ///
    /// The rule is the server's and it is not `is_adult`: a minor never
    /// qualifies, and above that it takes either a verified age or a verified
    /// account. Kept here rather than in a view so both the settings screen
    /// and anything else that asks get the same answer.
    public var canEnableAdultContent: Bool {
        !isMinor && (isAgeVerified || user.isVerified)
    }

    enum CodingKeys: String, CodingKey {
        case tier
        case contentSetting = "content_setting"
        case showSensitive = "show_sensitive"
        case unreadCount = "unread_count"
        case isAdult = "is_adult"
        case isMinor = "is_minor"
        case isAgeVerified = "is_age_verified"
        case isAdultCreator = "is_adult_creator"
        case twoFAEnabled = "two_fa_enabled"
        case celebrationsEnabled = "celebrations_enabled"
        case hasPassword = "has_password"
        case birthdayMdVisibility = "birthday_md_visibility"
        case birthdayYearVisibility = "birthday_year_visibility"
        case kycStatus = "kyc_status"
        case kycSubmittedAt = "kyc_submitted_at"
        case payoutEnabled = "payout_enabled"
        case socialLinks = "social_links"
        case externalTipLinks = "external_tip_links"
        case countryCode = "country_code"
    }

    public init(from decoder: any Decoder) throws {
        // MeDTO embeds UserDTO, so both sets of keys arrive in one flat object.
        user = try User(from: decoder)

        let c = try decoder.container(keyedBy: CodingKeys.self)
        contentSetting = try c.decode(String.self, forKey: .contentSetting)
        showSensitive = try c.decode(Bool.self, forKey: .showSensitive)
        tier = try c.decode(String.self, forKey: .tier)
        unreadCount = try c.decode(Int.self, forKey: .unreadCount)
        isAdult = try c.decode(Bool.self, forKey: .isAdult)
        isMinor = try c.decode(Bool.self, forKey: .isMinor)
        isAgeVerified = try c.decode(Bool.self, forKey: .isAgeVerified)
        isAdultCreator = try c.decode(Bool.self, forKey: .isAdultCreator)
        twoFAEnabled = try c.decode(Bool.self, forKey: .twoFAEnabled)
        celebrationsEnabled = try c.decode(Bool.self, forKey: .celebrationsEnabled)

        // Everything below arrived after the first native build shipped. A
        // server that predates it sends none of these keys, and a settings
        // screen that refused to decode would be a screen nobody could open
        // rather than a screen missing two rows — so each falls back to what
        // the column defaults to on the far side.
        hasPassword = try c.decodeIfPresent(Bool.self, forKey: .hasPassword) ?? true
        birthdayMdVisibility = try c.decodeIfPresent(String.self, forKey: .birthdayMdVisibility) ?? "everyone"
        birthdayYearVisibility = try c.decodeIfPresent(String.self, forKey: .birthdayYearVisibility) ?? "everyone"
        kycStatus = try c.decodeIfPresent(String.self, forKey: .kycStatus)
        kycSubmittedAt = try c.decodeIfPresent(Date.self, forKey: .kycSubmittedAt)
        payoutEnabled = try c.decodeIfPresent(Bool.self, forKey: .payoutEnabled) ?? false
        socialLinks = try c.decodeIfPresent([String: String].self, forKey: .socialLinks) ?? [:]
        externalTipLinks = try c.decodeIfPresent([String: String].self, forKey: .externalTipLinks) ?? [:]
        countryCode = try c.decodeIfPresent(String.self, forKey: .countryCode)
    }

    public func encode(to encoder: any Encoder) throws {
        try user.encode(to: encoder)

        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(contentSetting, forKey: .contentSetting)
        try c.encode(showSensitive, forKey: .showSensitive)
        try c.encode(tier, forKey: .tier)
        try c.encode(unreadCount, forKey: .unreadCount)
        try c.encode(isAdult, forKey: .isAdult)
        try c.encode(isMinor, forKey: .isMinor)
        try c.encode(isAgeVerified, forKey: .isAgeVerified)
        try c.encode(isAdultCreator, forKey: .isAdultCreator)
        try c.encode(twoFAEnabled, forKey: .twoFAEnabled)
        try c.encode(celebrationsEnabled, forKey: .celebrationsEnabled)
        try c.encode(hasPassword, forKey: .hasPassword)
        try c.encode(birthdayMdVisibility, forKey: .birthdayMdVisibility)
        try c.encode(birthdayYearVisibility, forKey: .birthdayYearVisibility)
        try c.encodeIfPresent(kycStatus, forKey: .kycStatus)
        try c.encodeIfPresent(kycSubmittedAt, forKey: .kycSubmittedAt)
        try c.encode(payoutEnabled, forKey: .payoutEnabled)
        try c.encode(socialLinks, forKey: .socialLinks)
        try c.encode(externalTipLinks, forKey: .externalTipLinks)
        try c.encodeIfPresent(countryCode, forKey: .countryCode)
    }
}

/// A successful login. Mirrors `SessionDTO`.
public struct Session: Codable, Hashable, Sendable {
    /// The opaque session token. Belongs in the Keychain, never in
    /// UserDefaults, and travels as `Authorization: Bearer <token>`.
    public let token: String
    public let expiresAt: Date
    public let user: CurrentUser
    /// Backup codes are a mandatory server-side gate. When true the app must
    /// route to the backup-code flow; a feed shown here will bounce.
    public let needsBackupCodes: Bool

    enum CodingKeys: String, CodingKey {
        case token, user
        case expiresAt = "expires_at"
        case needsBackupCodes = "needs_backup_codes"
    }
}
