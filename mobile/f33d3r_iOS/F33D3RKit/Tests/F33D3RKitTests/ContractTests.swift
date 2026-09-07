import Foundation
import Testing
@testable import F33D3RKit

/// Decodes the JSON the Go server actually emits.
///
/// The fixtures in Fixtures/ are written by TestWriteAPIV1GoldenFixtures in
/// feed-engine, from the real DTO projections. If someone renames a field in
/// apiv1_dto.go and regenerates, these tests fail — which is the point. Without
/// them a rename is only discovered as a decode failure on a device.
struct ContractTests {

    static func fixture(_ name: String) throws -> Data {
        guard let url = Bundle.module.url(forResource: "Fixtures/\(name)", withExtension: "json")
            ?? Bundle.module.url(forResource: name, withExtension: "json", subdirectory: "Fixtures")
        else {
            Issue.record("Missing fixture \(name).json — regenerate with F33D3R_GOLDEN_DIR")
            throw APIError.decoding("missing fixture")
        }
        return try Data(contentsOf: url)
    }

    /// Mirrors the decoder configured in APIClient. Kept in sync deliberately:
    /// if this needs changing, APIClient does too.
    static func makeDecoder() -> JSONDecoder {
        let d = JSONDecoder()
        d.dateDecodingStrategy = .custom { decoder in
            let raw = try decoder.singleValueContainer().decode(String.self)
            let fractional = ISO8601DateFormatter()
            fractional.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
            if let date = fractional.date(from: raw) { return date }
            let plain = ISO8601DateFormatter()
            plain.formatOptions = [.withInternetDateTime]
            if let date = plain.date(from: raw) { return date }
            throw DecodingError.dataCorruptedError(
                in: try decoder.singleValueContainer(),
                debugDescription: "Not an RFC 3339 timestamp: \(raw)"
            )
        }
        return d
    }

    @Test("UserDTO decodes with every public field intact")
    func decodesUser() throws {
        let user = try Self.makeDecoder().decode(User.self, from: Self.fixture("user"))

        #expect(user.handle == "dev")
        #expect(user.displayName == "Dev Account")
        #expect(user.bio == "Engineer at F33D3R. Building the thing.")
        #expect(user.pronouns == "they/them")
        #expect(user.isVerified)
        #expect(user.isCreator)
        #expect(user.role == "user")
        #expect(user.followerCount == 1240)
        #expect(user.followingCount == 310)
        #expect(user.postCount == 874)
        #expect(user.realm == 3)
        // Resolved server-side; the client must never derive the ladder itself.
        #expect(user.realmName == "Seeker")
        #expect(user.xp == 2500)
        #expect(user.themeID == "void")
        #expect(user.accentHex == "#7c5cff")
        #expect(!user.isPrivate)
        #expect(user.id == user.handle)
    }

    /// MeDTO embeds UserDTO in Go, so the keys arrive in one flat object rather
    /// than under a "user" key. CurrentUser's custom decoder exists for exactly
    /// this; if the server ever nests it instead, this test catches it.
    @Test("MeDTO decodes flattened, carrying both user and settings fields")
    func decodesCurrentUser() throws {
        let me = try Self.makeDecoder().decode(CurrentUser.self, from: Self.fixture("me"))

        #expect(me.user.handle == "dev")
        #expect(me.user.realmName == "Seeker")

        #expect(me.contentSetting == "default")
        #expect(me.showSensitive == false)
        #expect(me.tier == "creator")
        #expect(me.unreadCount == 7)
        #expect(me.isAdult)
        #expect(!me.isMinor)
        #expect(me.isAgeVerified)
        #expect(!me.isAdultCreator)
        #expect(me.twoFAEnabled)
        #expect(me.celebrationsEnabled)
    }

    /// The server emits RFC 3339 with fractional seconds, which plain .iso8601
    /// rejects. That would fail every response carrying a timestamp, so it is
    /// worth pinning explicitly.
    @Test("SessionDTO decodes, including a fractional-second timestamp")
    func decodesSession() throws {
        let session = try Self.makeDecoder().decode(Session.self, from: Self.fixture("session"))

        #expect(session.token.count == 64)
        #expect(!session.needsBackupCodes)
        #expect(session.user.user.handle == "dev")

        var components = DateComponents()
        components.year = 2026; components.month = 10; components.day = 4
        components.hour = 12; components.minute = 30; components.second = 45
        components.timeZone = TimeZone(identifier: "UTC")
        let expected = Calendar(identifier: .gregorian).date(from: components)!
        #expect(abs(session.expiresAt.timeIntervalSince(expected) - 0.123) < 0.01)
    }

    @Test("The error envelope decodes into a branchable code and a showable message")
    func decodesError() throws {
        struct Wrapper: Decodable { let error: APIErrorBody }
        let wrapper = try Self.makeDecoder().decode(Wrapper.self, from: Self.fixture("error"))

        #expect(wrapper.error.code == "invalid_credentials")
        #expect(wrapper.error.message == "Invalid handle or password.")
    }

    /// The server's allow-list is tested on the Go side; this asserts the same
    /// invariant from the client's vantage point, over the bytes that actually
    /// crossed the wire.
    @Test("No fixture carries identity or ranking internals", arguments: ["user", "me", "session"])
    func fixturesCarryNoSecrets(_ name: String) throws {
        let raw = String(decoding: try Self.fixture(name), as: UTF8.self).lowercased()
        for forbidden in ["pial", "jung", "archetype", "aesq", "shadow",
                          "interest_vector", "creator_affinities", "safety_epsilon"] {
            #expect(!raw.contains(forbidden), "\(name).json contains \(forbidden)")
        }
    }
}
