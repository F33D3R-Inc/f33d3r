import Foundation
import Testing
@testable import F33D3RKit

/// The settings surface's half of the wire contract.
///
/// The Go side pins these same key names in its own contract test by reading
/// the Swift models as text, so a rename on either side fails on both. What
/// these tests add is the behaviour around the keys: what a missing one means,
/// and what exactly leaves the device when one switch is flipped.
struct SettingsModelsTests {

    private static func decoder() -> JSONDecoder { ContractTests.makeDecoder() }

    // MARK: Blocks

    @Test("BlockList decodes both lists")
    func decodesBlockList() throws {
        let list = try Self.decoder().decode(BlockList.self, from: try ContractTests.fixture("block_list"))

        #expect(list.blocked.count == 1)
        #expect(list.blocked.first?.handle == "spammer")
        #expect(list.muted.count == 1)
        #expect(list.muted.first?.handle == "loudneighbour")
        #expect(!list.isEmpty)
    }

    /// A server that has blocked nobody may send `{}` rather than two empty
    /// arrays. An empty screen and a decode failure look nothing alike to the
    /// reader, so the model must not confuse them.
    @Test("BlockList treats absent lists as empty")
    func decodesEmptyBlockList() throws {
        let list = try Self.decoder().decode(BlockList.self, from: Data("{}".utf8))

        #expect(list.blocked.isEmpty)
        #expect(list.muted.isEmpty)
        #expect(list.isEmpty)
    }

    // MARK: Notification preferences

    @Test("NotificationPrefs decodes every switch and the quiet window")
    func decodesNotificationPrefs() throws {
        let prefs = try Self.decoder().decode(
            NotificationPrefs.self, from: try ContractTests.fixture("notification_prefs")
        )

        #expect(prefs.pushEnabled)
        #expect(prefs.messagesEnabled)
        #expect(!prefs.likesEnabled)
        #expect(prefs.repostsEnabled)
        #expect(prefs.repliesEnabled)
        #expect(!prefs.followsEnabled)
        #expect(prefs.achievementsEnabled)
        #expect(prefs.mentionsEnabled)
        #expect(!prefs.frequenciesEnabled)
        #expect(prefs.quietHoursEnabled)
        #expect(prefs.quietHoursStart == "23:00")
        #expect(prefs.quietHoursEnd == "07:30")
    }

    @Test("A toggle reads through the same key it would post")
    func togglesReadThroughSubscript() throws {
        let prefs = try Self.decoder().decode(
            NotificationPrefs.self, from: try ContractTests.fixture("notification_prefs")
        )

        #expect(prefs[.push])
        #expect(!prefs[.likes])
        #expect(!prefs[.follows])
        #expect(!prefs[.frequencies])
        // Every switch the screen draws has a wire key, and no two share one.
        #expect(Set(NotificationPrefs.Toggle.allCases.map(\.rawValue)).count
            == NotificationPrefs.Toggle.allCases.count)
    }

    /// A deployment that has not shipped `frequencies_enabled` yet must not
    /// take the whole screen down with it.
    @Test("NotificationPrefs defaults every absent key")
    func decodesSparseNotificationPrefs() throws {
        let prefs = try Self.decoder().decode(
            NotificationPrefs.self, from: Data(#"{"likes_enabled":false}"#.utf8)
        )

        #expect(!prefs.likesEnabled)
        #expect(prefs.pushEnabled)
        #expect(prefs.frequenciesEnabled)
        #expect(!prefs.quietHoursEnabled)
        #expect(prefs.quietHoursStart == nil)
    }

    /// The whole point of the change object: one flip sends one key. Posting
    /// the whole set back would let a stale read undo a change made elsewhere
    /// between the read and the write.
    @Test("One flipped switch encodes as exactly one key")
    func encodesSingleNotificationKey() throws {
        let json = try JSONSerialization.jsonObject(
            with: JSONEncoder().encode(NotificationPrefsChange.toggle(.likes, false))
        ) as? [String: Any]

        #expect(json?.count == 1)
        #expect(json?["likes_enabled"] as? Bool == false)
    }

    @Test("The quiet-hours window encodes as its two times")
    func encodesQuietHoursWindow() throws {
        let json = try JSONSerialization.jsonObject(
            with: JSONEncoder().encode(NotificationPrefsChange.quietHours(start: "22:00", end: "06:00"))
        ) as? [String: Any]

        #expect(json?.count == 2)
        #expect(json?["quiet_hours_start"] as? String == "22:00")
        #expect(json?["quiet_hours_end"] as? String == "06:00")
        #expect(json?["quiet_hours_enabled"] == nil)
    }

    // MARK: Two-factor

    @Test("TwoFactorSetup decodes the uri and the typed secret")
    func decodesTwoFactorSetup() throws {
        let setup = try Self.decoder().decode(
            TwoFactorSetup.self, from: try ContractTests.fixture("two_factor_setup")
        )

        #expect(setup.uri.hasPrefix("otpauth://totp/"))
        #expect(setup.secret == "JBSWY3DPEHPK3PXP")
        // Grouped for someone typing it into an authenticator by hand.
        #expect(setup.groupedSecret == "JBSW Y3DP EHPK 3PXP")
    }

    // MARK: Me

    @Test("Me decodes the settings fields the newer server sends")
    func decodesMeWithSettingsFields() throws {
        // Whatever the Go golden writer last emitted must decode as it stands.
        _ = try Self.decoder().decode(CurrentUser.self, from: try ContractTests.fixture("me"))

        var object = try JSONSerialization.jsonObject(with: try ContractTests.fixture("me")) as! [String: Any]
        object["has_password"] = false
        object["birthday_md_visibility"] = "followers"
        object["birthday_year_visibility"] = "only_me"
        object["kyc_status"] = "pending"
        object["kyc_submitted_at"] = "2026-09-01T10:00:00Z"
        object["payout_enabled"] = true
        object["social_links"] = ["youtube": "https://youtube.com/@dev"]
        object["external_tip_links"] = ["bitcoin": "bc1qexample"]
        object["country_code"] = "US"

        let me = try Self.decoder().decode(
            CurrentUser.self, from: try JSONSerialization.data(withJSONObject: object)
        )

        #expect(!me.hasPassword)
        #expect(me.birthdayMdVisibility == "followers")
        #expect(me.birthdayYearVisibility == "only_me")
        #expect(me.kycStatus == "pending")
        #expect(me.kycSubmittedAt != nil)
        #expect(me.payoutEnabled)
        #expect(me.socialLinks["youtube"] == "https://youtube.com/@dev")
        #expect(me.externalTipLinks["bitcoin"] == "bc1qexample")
        #expect(me.countryCode == "US")
    }

    /// An older server sends none of these keys. Every one must fall back to
    /// what the column defaults to on the far side, or the settings screen
    /// refuses to open against a deployment that has not been rebuilt yet.
    @Test("Me decodes without any of the settings fields")
    func decodesMeWithoutSettingsFields() throws {
        var object = try JSONSerialization.jsonObject(with: try ContractTests.fixture("me")) as! [String: Any]
        for key in [
            "has_password", "birthday_md_visibility", "birthday_year_visibility",
            "kyc_status", "kyc_submitted_at", "payout_enabled",
            "social_links", "external_tip_links", "country_code",
        ] {
            object.removeValue(forKey: key)
        }

        let me = try Self.decoder().decode(
            CurrentUser.self, from: try JSONSerialization.data(withJSONObject: object)
        )

        #expect(me.hasPassword)
        #expect(me.birthdayMdVisibility == "everyone")
        #expect(me.birthdayYearVisibility == "everyone")
        #expect(me.kycStatus == nil)
        #expect(me.kycSubmittedAt == nil)
        #expect(!me.payoutEnabled)
        #expect(me.socialLinks.isEmpty)
        #expect(me.externalTipLinks.isEmpty)
        #expect(me.countryCode == nil)
    }

    /// The NSFW option's rule is the server's, and it is not `is_adult`: a
    /// minor never qualifies, and above that it takes a verified age or a
    /// verified account. The fixture is an adult, age-verified and verified.
    @Test("The adult-content rule is age or verification, never minority")
    func adultContentRule() throws {
        let me = try Self.decoder().decode(CurrentUser.self, from: try ContractTests.fixture("me"))
        #expect(me.canEnableAdultContent)

        var object = try JSONSerialization.jsonObject(with: try ContractTests.fixture("me")) as! [String: Any]
        object["is_minor"] = true
        let minor = try Self.decoder().decode(
            CurrentUser.self, from: try JSONSerialization.data(withJSONObject: object)
        )
        #expect(!minor.canEnableAdultContent)

        object["is_minor"] = false
        object["is_age_verified"] = false
        object["is_verified"] = false
        let unverified = try Self.decoder().decode(
            CurrentUser.self, from: try JSONSerialization.data(withJSONObject: object)
        )
        #expect(!unverified.canEnableAdultContent)
    }

    // MARK: profile_update

    @Test("An untouched ProfileUpdate sends only its event type")
    func encodesEmptyProfileUpdate() throws {
        let json = try JSONSerialization.jsonObject(
            with: JSONEncoder().encode(ProfileUpdate().event())
        ) as? [String: Any]

        #expect(json?.count == 1)
        #expect(json?["event_type"] as? String == "profile_update")
        #expect(ProfileUpdate().isEmpty)
    }

    @Test("ProfileUpdate encodes every field under the server's own key")
    func encodesProfileUpdateKeys() throws {
        var update = ProfileUpdate()
        update.displayName = "Dev"
        update.bio = "hello"
        update.pronouns = "they/them"
        update.location = "Portland, OR"
        update.website = "example.com"
        update.accentHex = "#7c5cff"
        update.themeID = "void"
        update.avatarURL = "/media/a.webp"
        update.headerURL = "/media/h.webp"
        update.isAdultCreator = true
        update.birthdayMdVisibility = BirthdayVisibility.mutualFollowers.rawValue
        update.birthdayYearVisibility = BirthdayVisibility.onlyMe.rawValue
        update.countryCode = "US"
        update.socialLinks = ["youtube": "https://youtube.com/@dev", "other": ""]
        update.externalTipLinks = ["cashapp": "dev", "xrp": "rXXXX"]

        #expect(!update.isEmpty)

        let json = try JSONSerialization.jsonObject(
            with: JSONEncoder().encode(update.event())
        ) as? [String: Any]

        #expect(json?["event_type"] as? String == "profile_update")
        #expect(json?["display_name"] as? String == "Dev")
        #expect(json?["accent_hex"] as? String == "#7c5cff")
        #expect(json?["theme_id"] as? String == "void")
        #expect(json?["avatar_url"] as? String == "/media/a.webp")
        #expect(json?["header_url"] as? String == "/media/h.webp")
        #expect(json?["is_adult_creator"] as? Bool == true)
        #expect(json?["birthday_md_visibility"] as? String == "mutual_followers")
        #expect(json?["birthday_year_visibility"] as? String == "only_me")
        #expect(json?["country_code"] as? String == "US")
        #expect((json?["social_links"] as? [String: String])?["youtube"] == "https://youtube.com/@dev")
        #expect((json?["external_tip_links"] as? [String: String])?["xrp"] == "rXXXX")
    }

    /// The four ids are the web's select options, verbatim. Renaming one here
    /// would store a visibility the web form cannot show.
    @Test("Birthday visibility keeps the web's option ids")
    func birthdayVisibilityIDs() {
        #expect(BirthdayVisibility.allCases.map(\.rawValue)
            == ["everyone", "followers", "mutual_followers", "only_me"])
        #expect(BirthdayVisibility.named("mutual_followers") == .mutualFollowers)
        // An id this build has never heard of falls back rather than throwing.
        #expect(BirthdayVisibility.named("nobody_at_all") == .everyone)
        #expect(BirthdayVisibility.named(nil) == .everyone)
    }

    /// The editor draws these rows and the server normalises what goes in them.
    @Test("The link rows are the web's, in the web's order")
    func linkRows() {
        #expect(ProfileLinks.social.map(\.key)
            == ["youtube", "twitch", "spotify", "soundcloud", "kick", "other"])
        #expect(ProfileLinks.payment.map(\.key)
            == ["cashapp", "venmo", "paypal", "kofi", "buymeacoffee", "bitcoin", "xrp"])
    }

    // MARK: Errors

    @Test("A two-factor refusal arrives as a typed case")
    func twoFactorErrorCodes() {
        let required = APIError.api(
            status: 401, body: APIErrorBody(code: "two_fa_required", message: "Enter your code.")
        )
        let invalid = APIError.api(
            status: 401, body: APIErrorBody(code: "two_fa_invalid", message: "That code didn't work.")
        )
        let other = APIError.api(
            status: 401, body: APIErrorBody(code: "invalid_credentials", message: "No.")
        )

        #expect(required.twoFactor == .required)
        #expect(invalid.twoFactor == .invalid)
        #expect(other.twoFactor == nil)
        #expect(APIError.unexpectedStatus(401).twoFactor == nil)
    }

    /// The content setting's field is `setting`; every other switch's is
    /// `value`. Getting that wrong is a picker that springs back with no error.
    @Test("The settings switches keep the server's event names")
    func settingEventNames() {
        #expect(AccountSetting.showSensitive.rawValue == "settings.privacy.show_sensitive")
        #expect(AccountSetting.celebrations.rawValue == "settings.privacy.celebrations")
        #expect(AccountSetting.accountPrivate.rawValue == "settings.privacy.account_private")
        #expect(AccountSetting.contentSetting.rawValue == "settings.privacy.content_setting")
    }
}
