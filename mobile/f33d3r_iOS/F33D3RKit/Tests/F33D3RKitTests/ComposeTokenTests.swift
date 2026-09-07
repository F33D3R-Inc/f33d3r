import Foundation
import Testing
@testable import F33D3RKit

/// What the composer's dropdown may offer, and what it may not.
///
/// Every case here is the server's answer restated: the three regexes in
/// `feed-engine/internal/handler/handlers.go` decide what a posted body is
/// linkified as, and a dropdown that completed something outside them would
/// hand the reader a token that renders as plain text.
@Suite("Compose token detection")
struct ComposeTokenDetectionTests {

    @Test("A mention being typed is the token under the caret")
    func mention() throws {
        let token = try #require(ComposeToken.detect(in: "hello @ali", caret: 10))

        #expect(token.trigger == .mention)
        #expect(token.query == "ali")
        #expect(token.range == NSRange(location: 6, length: 4))
    }

    @Test("A hashtag and a ticker are their own triggers")
    func otherTriggers() throws {
        let tag = try #require(ComposeToken.detect(in: "on #swift", caret: 9))
        #expect(tag.trigger == .hashtag)
        #expect(tag.query == "swift")

        let ticker = try #require(ComposeToken.detect(in: "buy $AAP", caret: 8))
        #expect(ticker.trigger == .cashtag)
        #expect(ticker.query == "AAP")
    }

    @Test("A ticker typed in lower case still offers a completion")
    func lowerCaseTicker() throws {
        // The server matches only [A-Z], but nobody reaches for shift before
        // they reach for a ticker. What gets inserted is upper case, which is
        // what makes the finished body match.
        let token = try #require(ComposeToken.detect(in: "$appl", caret: 5))

        #expect(token.trigger == .cashtag)
        #expect(token.query == "appl")
    }

    @Test("The caret inside a token takes the whole token, not the half before it")
    func caretInsideToken() throws {
        let token = try #require(ComposeToken.detect(in: "hi @alice there", caret: 6))

        #expect(token.query == "alice")
        #expect(token.range == NSRange(location: 3, length: 6))
    }

    @Test("A trigger with nothing after it is a token with an empty query")
    func bareTrigger() throws {
        let token = try #require(ComposeToken.detect(in: "hey @", caret: 5))

        #expect(token.trigger == .mention)
        #expect(token.query == "")
    }

    @Test("A mid-word trigger is a token, because the server linkifies one")
    func triggerWithoutLeadingBoundary() throws {
        // `mentionRe` has no leading boundary: posting "mail me at a@bob" puts
        // a link on @bob. Offering the completion is offering what will happen.
        let token = try #require(ComposeToken.detect(in: "a@bob", caret: 5))

        #expect(token.trigger == .mention)
        #expect(token.query == "bob")
    }

    @Test("Plain text, a caret before the trigger, and an empty body are not tokens")
    func noToken() {
        #expect(ComposeToken.detect(in: "just words", caret: 10) == nil)
        #expect(ComposeToken.detect(in: "foo@bar", caret: 3) == nil)
        #expect(ComposeToken.detect(in: "", caret: 0) == nil)
        #expect(ComposeToken.detect(in: "@ali", caret: 0) == nil)
    }

    @Test("A caret outside the body is not a token")
    func caretOutOfRange() {
        #expect(ComposeToken.detect(in: "@ali", caret: 99) == nil)
        #expect(ComposeToken.detect(in: "@ali", caret: -1) == nil)
    }

    @Test("A ticker holding a digit or an underscore is not a ticker")
    func tickerCharacterSet() {
        // Go finds no match at all in "$AA_1": the \b after the letters cannot
        // be satisfied while a word character follows.
        #expect(ComposeToken.detect(in: "$AA_1", caret: 5) == nil)
        #expect(ComposeToken.detect(in: "$AA1", caret: 4) == nil)
        // A mention takes both.
        #expect(ComposeToken.detect(in: "@a_1", caret: 4)?.query == "a_1")
    }

    @Test("Past the server's length cap there is nothing left to complete")
    func lengthCaps() {
        let longHandle = String(repeating: "a", count: 51)
        #expect(ComposeToken.detect(in: "@" + longHandle, caret: 52) == nil)
        #expect(ComposeToken.detect(in: "@" + String(repeating: "a", count: 50), caret: 51) != nil)

        #expect(ComposeToken.detect(in: "$ABCDEF", caret: 7) == nil)
        #expect(ComposeToken.detect(in: "$ABCDE", caret: 6) != nil)

        let longTag = String(repeating: "b", count: 101)
        #expect(ComposeToken.detect(in: "#" + longTag, caret: 102) == nil)
    }

    @Test("Offsets are counted the way the text view counts them")
    func astralCharactersDoNotShiftTheRange() throws {
        // "🎉" is two UTF-16 units, so the token starts at 3 and not at 2.
        let token = try #require(ComposeToken.detect(in: "🎉 @ali", caret: 7))

        #expect(token.query == "ali")
        #expect(token.range == NSRange(location: 3, length: 4))
    }
}

@Suite("Compose token completion")
struct ComposeTokenCompletionTests {

    @Test("Choosing a name replaces the token and nothing else")
    func replacesOnlyTheToken() throws {
        let body = "morning @ali, how are you"
        let token = try #require(ComposeToken.detect(in: body, caret: 12))

        let (completed, caret) = token.completing(with: "alice", in: body)

        #expect(completed == "morning @alice, how are you")
        #expect(caret == 14)
    }

    @Test("A completion at the end leaves the caret after a space, ready for the next word")
    func trailingSpace() throws {
        let body = "hello @ali"
        let token = try #require(ComposeToken.detect(in: body, caret: 10))

        let (completed, caret) = token.completing(with: "alice", in: body)

        #expect(completed == "hello @alice ")
        #expect(caret == 13)
    }

    @Test("A completion with text after it takes no space of its own")
    func noSpaceMidBody() throws {
        let body = "hello @ali world"
        let token = try #require(ComposeToken.detect(in: body, caret: 10))

        let (completed, caret) = token.completing(with: "alice", in: body)

        #expect(completed == "hello @alice world")
        #expect(caret == 12)
    }

    @Test("A ticker is inserted upper case, which is what the server linkifies")
    func tickerInsertedUpperCase() throws {
        let body = "buying $appl"
        let token = try #require(ComposeToken.detect(in: body, caret: 12))

        let (completed, _) = token.completing(with: "AAPL", in: body)

        #expect(completed == "buying $AAPL ")
        #expect(ComposeToken.highlights(in: completed).map(\.trigger) == [.cashtag])
    }

    @Test("A completion after an astral character lands where it was aimed")
    func completionAfterEmoji() throws {
        let body = "🎉 @ali"
        let token = try #require(ComposeToken.detect(in: body, caret: 7))

        let (completed, caret) = token.completing(with: "alice", in: body)

        #expect(completed == "🎉 @alice ")
        #expect(caret == 10)
    }
}

@Suite("Compose highlighting")
struct ComposeHighlightTests {

    @Test("All three triggers are coloured, in the order they were written")
    func allThree() {
        let found = ComposeToken.highlights(in: "hey @ali about #swift and $AAPL")

        #expect(found.map(\.trigger) == [.mention, .hashtag, .cashtag])
        #expect(found[0].range == NSRange(location: 4, length: 4))
        #expect(found[2].range == NSRange(location: 26, length: 5))
    }

    @Test("A ticker is not a ticker until it is upper case and finished")
    func tickerRules() {
        // `cashtagRe` is `\$([A-Z]{1,5})\b` — this is the whole of it.
        #expect(ComposeToken.highlights(in: "$appl").isEmpty)
        #expect(ComposeToken.highlights(in: "$AA_1").isEmpty)
        #expect(ComposeToken.highlights(in: "$AAPL.").map(\.trigger) == [.cashtag])
        #expect(ComposeToken.highlights(in: "$AAPL").map(\.trigger) == [.cashtag])
    }

    @Test("A bare trigger colours nothing")
    func bareTriggers() {
        #expect(ComposeToken.highlights(in: "@ # $").isEmpty)
        #expect(ComposeToken.highlights(in: "cost $5").isEmpty)
    }

    @Test("A token stops where the server stops reading it")
    func stopsAtTheCap() throws {
        let long = "@" + String(repeating: "a", count: 60)
        let found = try #require(ComposeToken.highlights(in: long).first)

        #expect(found.range == NSRange(location: 0, length: 51))
    }

    @Test("One token ending does not swallow the next")
    func adjacentTokens() {
        let found = ComposeToken.highlights(in: "#tag@name")

        #expect(found.map(\.trigger) == [.hashtag, .mention])
        #expect(found[1].range == NSRange(location: 4, length: 5))
    }
}
