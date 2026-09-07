import Foundation
import Testing
@testable import F33D3RKit

/// Money on a card. Every number here is an integer count of micro-AET on the
/// way in, because a balance rounded through a `Double` is a balance that
/// eventually disagrees with the ledger.
@Suite("AET formatting")
struct AETTests {

    @Test("A whole number of AET prints without decimals")
    func wholeAmounts() {
        #expect(AET.label(uAET: 1_000_000) == "1 AET")
        #expect(AET.label(uAET: 3_000_000) == "3 AET")
        #expect(AET.label(uAET: 1_250_000_000) == "1250 AET")
    }

    @Test("A fraction keeps up to two places and no trailing zeros")
    func fractionalAmounts() {
        #expect(AET.label(uAET: 12_400_000) == "12.4 AET")
        #expect(AET.label(uAET: 1_250_000) == "1.25 AET")
        #expect(AET.label(uAET: 300_000) == "0.3 AET")
        #expect(AET.label(uAET: 890_000) == "0.89 AET")
    }

    @Test("A tip too small to print still reads as more than nothing")
    func dustAmounts() {
        // A tip that happened must not render as one that did not.
        #expect(AET.label(uAET: 500) == "<0.01 AET")
        #expect(AET.label(uAET: 1) == "<0.01 AET")
    }

    @Test("Large amounts abbreviate so the control still fits the row")
    func largeAmounts() {
        #expect(AET.label(uAET: 10_000 * AET.microsPerAET) == "10K AET")
        #expect(AET.label(uAET: 12_400 * AET.microsPerAET) == "12.4K AET")
        #expect(AET.label(uAET: 2_000_000 * AET.microsPerAET) == "2M AET")
    }

    @Test("Nothing tipped is zero, not a blank")
    func zero() {
        #expect(AET.amount(uAET: 0) == "0")
        #expect(AET.amount(uAET: -5) == "0")
    }
}

/// The gate chip and the kind label are the two strings the card derives rather
/// than receives, so both are pinned.
@Suite("Work display")
struct WorkDisplayTests {

    @Test("A subscriber-only work reads as gated on a subscription")
    func subscriberGate() throws {
        let work = try decode(#"{"subscriber_only": true}"#)

        #expect(work.gate == .subscriber)
        #expect(work.gate?.label == "gated · sub")
    }

    @Test("A priced work names its price")
    func pricedGate() throws {
        let work = try decode(#"{"price_uaet": 3000000}"#)

        #expect(work.gate == .priced(uAET: 3_000_000))
        #expect(work.gate?.label == "gated · 3 AET")
    }

    @Test("A price wins over a subscription — it is the more specific offer")
    func priceBeatsSubscription() throws {
        let work = try decode(#"{"subscriber_only": true, "price_uaet": 500000}"#)

        #expect(work.gate == .priced(uAET: 500_000))
        #expect(work.gate?.label == "gated · 0.5 AET")
    }

    @Test("A priced work the viewer owns has no chip — the server says it is theirs")
    func ownedGate() throws {
        let work = try decode(#"{"price_uaet": 2000000, "work_purchased_by_viewer": true}"#)

        #expect(work.gate == nil)
        #expect(work.isForSale)
        #expect(work.purchasedByViewer)
    }

    @Test("A track's title is the first line of its body, without hashtags")
    func trackTitle() throws {
        let track = try decode(#"{"body": "Rooftop, take two #music\nThe clean take.", "voice": {"url": "/media/x.m4a", "duration_secs": 214}}"#)
        #expect(track.trackTitle == "Rooftop, take two")
        #expect(track.isPlayable)
        #expect(track.playbackPath == "/media/x.m4a")
        #expect(track.playbackDurationSecs == 214)
        #expect(track.artworkPath == nil)

        let untitled = try decode(#"{"body": "", "voice": {"url": "/media/y.m4a"}}"#)
        #expect(untitled.trackTitle == "Track")

        let post = try decode(#"{"body": "just words", "media_urls": ["/media/a.png"]}"#)
        #expect(!post.isPlayable)
        #expect(post.artworkPath == "/media/a.png")
    }

    @Test("An ungated work has no chip")
    func noGate() throws {
        #expect(try decode("{}").gate == nil)
        // A price of zero is not a price.
        #expect(try decode(#"{"price_uaet": 0}"#).gate == nil)
    }

    @Test("The tip control shows an amount only once something has been tipped")
    func tipLabel() throws {
        #expect(try decode(#"{"tip_total_uaet": 12400000}"#).tipLabel == "12.4 AET")
        #expect(try decode(#"{"tip_total_uaet": 0}"#).tipLabel == nil)
        #expect(try decode("{}").tipLabel == nil)
    }

    @Test("The kind label comes from the attachments, never from content_type")
    func kindLabelReadsAttachments() throws {
        // Every row in the database says "text" — the column defaults to it and
        // nothing writes it — so a label driven by it would be one word for the
        // whole platform.
        let video = try decode(#"{"content_type": "text", "video": {"master_url": "/media/a.m3u8"}}"#)
        #expect(video.kindLabel == "video")

        let voice = try decode(#"{"content_type": "text", "voice": {"url": "/media/a.m4a", "duration_secs": 12}}"#)
        #expect(voice.kindLabel == "track")

        let photo = try decode(#"{"content_type": "text", "media_urls": ["/media/a.webp"]}"#)
        #expect(photo.kindLabel == "photo")

        let poll = try decode(#"{"content_type": "text", "poll": {"results": [], "total_votes": 0}}"#)
        #expect(poll.kindLabel == "poll")

        #expect(try decode(#"{"content_type": "image"}"#).kindLabel == "post")
    }

    @Test("Relational kinds keep their own word")
    func kindLabelKeepsRelations() throws {
        #expect(try decode(#"{"kind": "reply", "media_urls": ["/media/a.webp"]}"#).kindLabel == "reply")
        #expect(try decode(#"{"kind": "quote"}"#).kindLabel == "quote")
        #expect(try decode(#"{"kind": "thread_post"}"#).kindLabel == "thread")
        #expect(try decode(#"{"kind": "react_video"}"#).kindLabel == "react")
    }

    @Test("Provenance is drawn only when the server sent one")
    func provenanceGate() throws {
        #expect(try decode("{}").visibleProvenance == nil)
        // An empty sentence is nothing to say, whatever the envelope claims.
        #expect(try decode(#"{"provenance": {"kind": "you_follow", "text": ""}}"#).visibleProvenance == nil)

        let work = try decode(#"{"provenance": {"kind": "you_follow", "text": "You follow @dev", "handle": "dev"}}"#)
        #expect(work.visibleProvenance?.text == "You follow @dev")
        #expect(work.visibleProvenance?.kind == "you_follow")
        #expect(work.visibleProvenance?.handle == "dev")
    }

    /// Builds a work from the fields under test, through the production decoder,
    /// so a fixture that does not match the model fails here rather than on a
    /// device.
    private func decode(_ fields: String) throws -> Work {
        let base = """
        {"id":"1","cid":"sha256:aa","kind":"post",
         "author":{"handle":"dev","display_name":"Engineering"},
         "created_at":"2026-01-01T00:00:00Z"}
        """
        var merged = try JSONSerialization.jsonObject(with: Data(base.utf8)) as! [String: Any]
        for (key, value) in try JSONSerialization.jsonObject(with: Data(fields.utf8)) as! [String: Any] {
            merged[key] = value
        }
        let data = try JSONSerialization.data(withJSONObject: merged)
        return try APIClient.makeDecoder().decode(Work.self, from: data)
    }
}

/// A lane the deployment does not serve yet has to look like an empty lane, and
/// a real fault still has to look like a fault.
@Suite("Unserved surfaces")
struct UnservedSurfaceTests {

    @Test("The 400 Live answers today is an unserved lane, not a failure")
    func unknownSurface() {
        let error = APIError.api(
            status: 400,
            body: APIErrorBody(code: "unknown_surface", message: "No such surface.")
        )
        #expect(error.isUnservedSurface)
        #expect(error.code == "unknown_surface")
    }

    @Test("404 and 501 mean the same thing without a code")
    func missingSurface() {
        #expect(APIError.unexpectedStatus(404).isUnservedSurface)
        #expect(APIError.unexpectedStatus(501).isUnservedSurface)
    }

    @Test("A surface that exists and broke is still an error")
    func realFailuresSurvive() {
        #expect(!APIError.unexpectedStatus(500).isUnservedSurface)
        #expect(!APIError.transport("offline").isUnservedSurface)
        // A plain 400 with a different code is a bad request, not a missing lane.
        let badRequest = APIError.api(status: 400, body: APIErrorBody(code: "bad_cursor", message: "…"))
        #expect(!badRequest.isUnservedSurface)
    }

    @Test("Someone else's likes are a rule, recognisable on its own")
    func privateLikes() {
        let error = APIError.api(
            status: 403,
            body: APIErrorBody(code: "likes_private", message: "Likes are private.")
        )
        #expect(error.isPrivateLikes)
        #expect(!error.isUnservedSurface)
        #expect(!APIError.unexpectedStatus(403).isPrivateLikes)
    }
}
