import Foundation
import Testing
@testable import F33D3RKit

/// A quote chain as the wire carries it: the quoted work, and under it the work
/// that one quotes. The server loads exactly two levels, so `nested` is the
/// last thing the model can hold — a third level is not a case, it is absent.
struct QuotedWorkTests {

    private static let author = """
    {"handle":"mia","display_name":"mia","is_verified":true,"is_creator":true,"role":"creator","realm":2}
    """

    private static func quotedJSON(nested: String?) -> Data {
        let tail = nested.map { ",\"nested\":\($0)" } ?? ""
        return Data("""
        {"id":"w2","cid":"sha256:2","author":\(author),"body":"level two",
         "created_at":"2026-09-01T12:00:00Z","media_urls":["/media/a.webp","/media/b.webp","/media/c.webp"],
         "is_nsfw":false,"is_gore":false\(tail)}
        """.utf8)
    }

    private static let level3 = """
    {"id":"w3","cid":"sha256:3","author":\(author),"body":"level three",
     "created_at":"2026-08-28T22:35:00Z","media_urls":["/media/x.webp"],"is_nsfw":true,"is_gore":false}
    """

    @Test("The second level decodes as a plain QuotedWork under `nested`")
    func decodesTheSecondLevel() throws {
        let quoted = try APIClient.makeDecoder().decode(QuotedWork.self, from: Self.quotedJSON(nested: Self.level3))
        let nested = try #require(quoted.nestedWork)
        #expect(nested.id == "w3")
        #expect(nested.body == "level three")
        #expect(nested.isFlagged)
        #expect(nested.mediaURLs == ["/media/x.webp"])
        // The wire never carries a third level, and if it did the model would
        // not know what to do with it — it is simply not there.
        #expect(nested.nestedWork == nil)
    }

    @Test("No second level is nil, not an error")
    func noSecondLevelIsNil() throws {
        let quoted = try APIClient.makeDecoder().decode(QuotedWork.self, from: Self.quotedJSON(nested: nil))
        #expect(quoted.nested == nil)
        #expect(quoted.mediaURLs.count == 3)
    }

    @Test("The box is invisible on the wire: `nested` encodes as the work itself")
    func boxEncodesTransparently() throws {
        let quoted = try APIClient.makeDecoder().decode(QuotedWork.self, from: Self.quotedJSON(nested: Self.level3))
        let raw = try JSONEncoder().encode(quoted)
        let object = try #require(try JSONSerialization.jsonObject(with: raw) as? [String: Any])
        let nested = try #require(object["nested"] as? [String: Any])
        #expect(nested["id"] as? String == "w3")
        #expect(nested["work"] == nil)
    }

    @Test("Equal chains are equal, and the box does not break Hashable")
    func boxedEquality() throws {
        let a = try APIClient.makeDecoder().decode(QuotedWork.self, from: Self.quotedJSON(nested: Self.level3))
        let b = try APIClient.makeDecoder().decode(QuotedWork.self, from: Self.quotedJSON(nested: Self.level3))
        let c = try APIClient.makeDecoder().decode(QuotedWork.self, from: Self.quotedJSON(nested: nil))
        #expect(a == b)
        #expect(a.hashValue == b.hashValue)
        #expect(a != c)
    }

    /// The chain as a card actually receives it: the outer work, the work it
    /// quotes, and the work *that* one quotes. Three levels drawn — the card,
    /// the bordered embed, the compact rail — which is the whole depth the
    /// server loads and the whole depth the model can hold.
    private static let threeDeepWork = Data("""
    {"id":"w1","cid":"sha256:1","kind":"quote","author":\(author),"body":"Like that",
     "created_at":"2026-09-01T13:00:00Z","is_edited":false,"media_urls":[],"tags":[],
     "content_type":"text","is_nsfw":false,"is_gore":false,"is_sensitive":false,
     "subscriber_only":false,"is_pinned":false,"comment_gating":"open","scan_state":"clean",
     "like_count":2,"dislike_count":0,"repost_count":0,"quote_count":0,"reply_count":0,
     "bookmark_count":1,"view_count":91,"liked_by_viewer":false,"disliked_by_viewer":false,
     "reposted_by_viewer":false,"bookmarked_by_viewer":false,"viewer_follows_author":true,
     "quoted":{"id":"w2","cid":"sha256:2","author":\(author),"body":"best thing on here all week",
       "created_at":"2026-09-01T12:00:00Z","is_nsfw":false,"is_gore":false,
       "nested":{"id":"w3","cid":"sha256:3","author":\(author),"body":"four frames from the roll",
         "created_at":"2026-08-31T22:35:00Z","is_nsfw":false,"is_gore":false,
         "media_urls":["/media/a.webp","/media/b.webp","/media/c.webp","/media/d.webp"]}}}
    """.utf8)

    @Test("A work carries its whole chain: card, quoted card, nested rail")
    func decodesThreeDeepFromAWork() throws {
        let work = try APIClient.makeDecoder().decode(Work.self, from: Self.threeDeepWork)
        #expect(work.body == "Like that")

        let quoted = try #require(work.quoted)
        #expect(quoted.id == "w2")
        // Level one has no media of its own — the embed is a body and a head.
        #expect(quoted.hasMedia == false)

        let nested = try #require(quoted.nestedWork)
        #expect(nested.id == "w3")
        // Four frames on the deepest level: the rail draws one thumbnail and a
        // "+3" badge over it, which is the only place that count comes from.
        #expect(nested.mediaURLs.count == 4)
        #expect(nested.hasMedia)
        #expect(nested.isFlagged == false)
        // And the chain stops. Nothing draws a fourth level, so nothing carries
        // one.
        #expect(nested.nestedWork == nil)
    }

    @Test("An embed says how old, not when")
    func compactAge() {
        let now = Date(timeIntervalSince1970: 1_788_000_000) // 2026-08-29
        #expect(RelativeTime.compactLabel(for: now.addingTimeInterval(-30), now: now) == "now")
        #expect(RelativeTime.compactLabel(for: now.addingTimeInterval(-42 * 60), now: now) == "42m")
        #expect(RelativeTime.compactLabel(for: now.addingTimeInterval(-5 * 3600), now: now) == "5h")
        #expect(RelativeTime.compactLabel(for: now.addingTimeInterval(-3 * 86_400), now: now) == "3d")
        // A week and older names the day; another year names the year too.
        let older = RelativeTime.compactLabel(for: now.addingTimeInterval(-40 * 86_400), now: now)
        #expect(older.contains("Jul"))
        #expect(!older.contains("2026"))
        let lastYear = RelativeTime.compactLabel(for: now.addingTimeInterval(-400 * 86_400), now: now)
        #expect(lastYear.contains("2025"))
    }
}
