import Foundation
import Testing
@testable import F33D3RKit

/// The user stream, line by line.
///
/// Every frame the app reacts to arrives through these two steps — bytes to
/// ``SSEFrames/Frame``, frame to ``UserEvent`` — and neither can be exercised
/// against a real server in a test. So they are driven directly: a list of
/// lines in, the frames and events they must produce out. A kind the server
/// whitelists but this parser drops is a feature that silently does nothing,
/// which is the failure this suite exists to catch.
@Suite("SSE framing")
struct SSEFramingTests {

    /// Feeds every line through a fresh parser and collects what came out,
    /// including whatever the flush at end-of-stream produces.
    private func frames(_ lines: [String]) -> [SSEFrames.Frame] {
        var parser = SSEFrames.Parser()
        var out: [SSEFrames.Frame] = []
        for line in lines {
            if let frame = parser.consume(line) { out.append(frame) }
        }
        if let frame = parser.flush() { out.append(frame) }
        return out
    }

    @Test("A frame ends on the blank line the server sends")
    func blankLineEndsFrame() {
        let out = frames([
            "event: notify",
            "data: {\"unread\":3}",
            "",
            "event: balance",
            "data: {\"balance_uaet\":1,\"pending_uaet\":0}",
            "",
        ])
        #expect(out.count == 2)
        #expect(out[0].event == "notify")
        #expect(out[0].data == "{\"unread\":3}")
        #expect(out[1].event == "balance")
    }

    @Test("Several data lines in one frame are joined with a newline")
    func multiLineData() {
        // The first frame proves the reader delivers blank lines; only after
        // that does the parser buffer, which is exactly the fallback the
        // framing is built around.
        let out = frames([
            ": ping",
            "",
            "event: notify",
            "data: {",
            "data:   \"unread\": 3",
            "data: }",
            "",
        ])
        #expect(out.count == 1)
        #expect(out[0].data == "{\n  \"unread\": 3\n}")
        #expect(out[0].event == "notify")
    }

    @Test("A frame's id is carried on the frame and remembered by the parser")
    func idCapture() {
        var parser = SSEFrames.Parser()
        var out: [SSEFrames.Frame] = []
        for line in ["id: 41", "event: notify", "data: {}", "", "id: 42", "event: notify", "data: {}", ""] {
            if let frame = parser.consume(line) { out.append(frame) }
        }
        #expect(out.count == 2)
        #expect(out[0].id == "41")
        #expect(out[1].id == "42")
        #expect(parser.lastEventID == "42")
    }

    @Test("A parser that has seen no ids has no last id to send back")
    func noIDs() {
        var parser = SSEFrames.Parser()
        _ = parser.consume("event: notify")
        _ = parser.consume("data: {}")
        #expect(parser.lastEventID == nil)
    }

    @Test("Comment lines keep the connection warm and produce nothing")
    func commentLines() {
        let out = frames([": ping", ":", ": keep-alive", ""])
        #expect(out.isEmpty)
    }

    @Test("A reader that never delivers a blank line still delivers every frame")
    func noBlankLinesEverDelivered() {
        // This is the behaviour the first version of the parser had, and the
        // reason it dispatched on the data line. It has to keep working: the
        // frames are the whole point, and a frame held back waiting for a
        // terminator that never comes is a badge that never updates.
        let out = frames([
            "event: notify",
            "data: {\"unread\":1}",
            "event: notify",
            "data: {\"unread\":2}",
            "event: post_deleted",
            "data: {\"work_id\":\"w1\"}",
        ])
        #expect(out.count == 3)
        #expect(out[0].data == "{\"unread\":1}")
        #expect(out[1].data == "{\"unread\":2}")
        #expect(out[2].event == "post_deleted")
    }

    @Test("A frame left buffered when the stream ends is still delivered")
    func flushAtEnd() {
        let out = frames([
            "event: notify",
            "data: {\"unread\":1}",
            "",
            "event: notify",
            "data: {\"unread\":2}",
        ])
        #expect(out.count == 2)
        #expect(out[1].data == "{\"unread\":2}")
    }

    @Test("A trailing carriage return is not part of the value")
    func stripsCarriageReturn() {
        let out = frames(["event: notify\r", "data: {\"unread\":7}\r", "\r"])
        #expect(out.count == 1)
        #expect(out[0].event == "notify")
        #expect(out[0].data == "{\"unread\":7}")
    }
}

@Suite("User stream events")
struct UserEventParsingTests {

    private let decoder = APIClient.makeDecoder()

    private func parse(_ event: String, _ data: String) -> UserEvent? {
        UserEventStream.parse(SSEFrames.Frame(event: event, data: data, id: nil), decoder: decoder)
    }

    @Test("Every kind the server whitelists parses to a case")
    func everyWhitelistedKind() {
        // `apiUserStreamKinds` on the server. A kind added there and not here
        // arrives and is thrown away.
        #expect(parse("notify", "{\"unread\":4}") == .notify(unread: 4))
        #expect(parse("new_post", "{\"work_id\":\"w1\",\"author_handle\":\"dev\"}")
                == .newPost(workID: "w1", authorHandle: "dev"))
        #expect(parse("post_deleted", "{\"work_id\":\"w1\"}") == .postDeleted(workID: "w1"))
        #expect(parse("balance", "{\"balance_uaet\":250,\"pending_uaet\":10}")
                == .balance(settledUAET: 250, pendingUAET: 10))
        #expect(parse("live_start", "{\"stream_id\":\"s1\"}") == .liveStart(streamID: "s1"))
        #expect(parse("frequency_start", "{\"frequency_id\":\"f1\"}") == .frequencyStart(id: "f1"))

        if case .message(let id, let message)? = parse("gnosis_message", "{\"conversation_id\":\"c1\"}") {
            #expect(id == "c1")
            #expect(message == nil)
        } else {
            Issue.record("gnosis_message did not parse")
        }

        if case .workEngagement(let engagement)? = parse(
            "work_engagement",
            "{\"work_id\":\"w1\",\"like_count\":9,\"reply_count\":2}"
        ) {
            #expect(engagement.workID == "w1")
            #expect(engagement.likeCount == 9)
            #expect(engagement.replyCount == 2)
        } else {
            Issue.record("work_engagement did not parse")
        }
    }

    @Test("An unknown kind is ignored rather than guessed at")
    func unknownKind() {
        #expect(parse("something_new", "{\"work_id\":\"w1\"}") == nil)
        #expect(parse("", "{}") == nil)
    }

    @Test("A frame of the right kind with the wrong body is dropped, not crashed on")
    func malformedPayload() {
        #expect(parse("notify", "not json") == nil)
        #expect(parse("work_engagement", "{\"like_count\":3}") == nil)
    }

    @Test("An engagement frame carries counts and nothing viewer-relative")
    func engagementDecoding() throws {
        let json = """
        {"work_id":"w1","like_count":5,"dislike_count":1,"reply_count":2,"repost_count":3,
         "quote_count":4,"bookmark_count":6,"view_count":700,"tip_total_uaet":1000000}
        """
        let engagement = try decoder.decode(WorkEngagement.self, from: Data(json.utf8))
        #expect(engagement.workID == "w1")
        #expect(engagement.likeCount == 5)
        #expect(engagement.dislikeCount == 1)
        #expect(engagement.replyCount == 2)
        #expect(engagement.repostCount == 3)
        #expect(engagement.quoteCount == 4)
        #expect(engagement.bookmarkCount == 6)
        #expect(engagement.viewCount == 700)
        #expect(engagement.tipTotalUAET == 1_000_000)
    }
}

@Suite("Applying pushed counts")
struct WorkEngagementApplyTests {

    private var work: Work { SampleData.first }

    @Test("The counts named in the frame are the ones that change")
    func countsAreWritten() {
        let updated = work.applying(
            WorkEngagement(
                workID: work.id,
                likeCount: 101,
                dislikeCount: 2,
                replyCount: 3,
                repostCount: 4,
                quoteCount: 5,
                bookmarkCount: 6,
                viewCount: 7,
                tipTotalUAET: 8
            )
        )
        #expect(updated.likeCount == 101)
        #expect(updated.dislikeCount == 2)
        #expect(updated.replyCount == 3)
        #expect(updated.repostCount == 4)
        #expect(updated.quoteCount == 5)
        #expect(updated.bookmarkCount == 6)
        #expect(updated.viewCount == 7)
        #expect(updated.tipTotalUAET == 8)
    }

    @Test("A count the server did not send is left alone, not zeroed")
    func absentCountsAreUntouched() {
        let updated = work.applying(WorkEngagement(workID: work.id, likeCount: 42))
        #expect(updated.likeCount == 42)
        #expect(updated.replyCount == work.replyCount)
        #expect(updated.repostCount == work.repostCount)
        #expect(updated.viewCount == work.viewCount)
        #expect(updated.tipTotalUAET == work.tipTotalUAET)
    }

    @Test("Nothing but the counts moves")
    func everythingElseIsUntouched() {
        let updated = work.applying(WorkEngagement(workID: work.id, likeCount: work.likeCount + 1))
        #expect(updated.id == work.id)
        #expect(updated.cid == work.cid)
        #expect(updated.body == work.body)
        #expect(updated.author == work.author)
        #expect(updated.kind == work.kind)
        #expect(updated.createdAt == work.createdAt)
        #expect(updated.mediaURLs == work.mediaURLs)
        // Viewer state is not on the frame: it is the reader's own answer, and
        // one frame goes to every watcher at once.
        #expect(updated.likedByViewer == work.likedByViewer)
        #expect(updated.bookmarkedByViewer == work.bookmarkedByViewer)
        #expect(updated.repostedByViewer == work.repostedByViewer)
    }

    @Test("A frame about another work is not applied to this one")
    func wrongWorkIsRefused() {
        let updated = work.applying(WorkEngagement(workID: "somebody-else", likeCount: 9999))
        #expect(updated == work)
    }
}
