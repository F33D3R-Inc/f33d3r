import Foundation
import Testing
@testable import F33D3RKit

/// What counts as a cashtag, and what the server says one is worth.
///
/// The pattern here is not this app's to choose: the web linkifies `$TICKER`,
/// the composer underlines it and the server stores the first one against the
/// work, all through `\$([A-Z]{1,5})\b`. Matching anything else means the app
/// draws a card the server has no quote for, or leaves a live cashtag as plain
/// text on this platform alone. The Go side reads the literal in
/// CashtagQuote.swift and fails if these two ever drift.
struct CashtagTests {

    @Test("A ticker is one to five capitals after a dollar sign")
    func matchesTheServersRule() {
        #expect(Cashtag.tickers(in: "$AAPL") == ["AAPL"])
        #expect(Cashtag.tickers(in: "$A") == ["A"])
        #expect(Cashtag.tickers(in: "$GOOGL to the moon") == ["GOOGL"])
        #expect(Cashtag.tickers(in: "bought $TSLA, sold $NVDA.") == ["TSLA", "NVDA"])
        #expect(Cashtag.tickers(in: "line one\n$MSFT") == ["MSFT"])
    }

    @Test("What is not a cashtag stays plain text")
    func rejectsWhatTheServerRejects() {
        // Lower case is not a ticker — "$5 for coffee" and "$myfund" must not
        // put a stock card under somebody's work.
        #expect(Cashtag.tickers(in: "$aapl").isEmpty)
        #expect(Cashtag.tickers(in: "it cost $5").isEmpty)
        #expect(Cashtag.tickers(in: "$").isEmpty)
        // Six letters is past the server's cap, and there is no boundary after
        // five, so nothing in it matches — not even a prefix.
        #expect(Cashtag.tickers(in: "$TOOLONG").isEmpty)
        // A digit is a word character, so no boundary follows the letters.
        #expect(Cashtag.tickers(in: "$AAPL2").isEmpty)
    }

    @Test("A boundary ends a ticker, punctuation and all")
    func stopsAtWordBoundaries() {
        #expect(Cashtag.tickers(in: "$AAPL.") == ["AAPL"])
        #expect(Cashtag.tickers(in: "($AAPL)") == ["AAPL"])
        #expect(Cashtag.tickers(in: "$AAPL/$MSFT") == ["AAPL", "MSFT"])
        // The web matches this too: the dollar is what starts a cashtag, not
        // the start of a word.
        #expect(Cashtag.tickers(in: "US$AAPL") == ["AAPL"])
    }

    @Test("One card per work, and it is the first ticker — the one the server stored")
    func firstTickerWins() {
        #expect(Cashtag.first(in: "$TSLA vs $NVDA vs $AMD") == "TSLA")
        #expect(Cashtag.first(in: "no tickers here") == nil)
        // The same ticker twice is one card, not two.
        #expect(Cashtag.tickers(in: "$AAPL and $AAPL again") == ["AAPL"])
    }

    @Test("A ticker still being typed is not a ticker yet, but the alphabet is the same")
    func prefixRuleForTheComposer() {
        // The moment after "$": a dropdown may open on nothing.
        #expect(Cashtag.typedTicker("") == "")
        #expect(Cashtag.typedTicker("AAP") == "AAP")
        // Offered on lower case, inserted upper — the server linkifies
        // capitals only, so anything else lands in a body as plain text.
        #expect(Cashtag.typedTicker("appl") == "APPL")
        #expect(Cashtag.typedTicker("TOOLONG") == nil)
        #expect(Cashtag.typedTicker("AA1") == nil)
        #expect(Cashtag.typedTicker("AA PL") == nil)
    }

    @Test("A finished ticker is what the quote route accepts")
    func finishedTickers() {
        #expect(Cashtag.isTicker("AAPL"))
        #expect(Cashtag.isTicker("A"))
        #expect(!Cashtag.isTicker(""))
        #expect(!Cashtag.isTicker("aapl"))
        #expect(!Cashtag.isTicker("TOOLONG"))
        #expect(!Cashtag.isTicker("BRK-B"))
    }

    // MARK: The quote

    @Test("A quote decodes with every label the server wrote")
    func decodesQuote() throws {
        let json = Data("""
        {"ticker":"AAPL","display":"$AAPL","company_name":"Apple Inc.","exchange":"NASDAQ",
         "price":227.52,"price_label":"227.52","change_pct":-0.84,"change_label":"-0.84%",
         "is_positive":false,"sparkline":[226.1,227.9,228.4,227.5],"as_of":"2026-09-06T12:00:00Z"}
        """.utf8)

        let quote = try ContractTests.makeDecoder().decode(CashtagQuote.self, from: json)

        #expect(quote.ticker == "AAPL")
        #expect(quote.display == "$AAPL")
        #expect(quote.companyName == "Apple Inc.")
        #expect(quote.exchange == "NASDAQ")
        #expect(quote.price == 227.52)
        // Printed as the server wrote it. The app never formats a price.
        #expect(quote.priceLabel == "227.52")
        #expect(quote.changeLabel == "-0.84%")
        #expect(quote.isPositive == false)
        #expect(quote.sparkline == [226.1, 227.9, 228.4, 227.5])
        #expect(quote.hasSparkline)
        #expect(quote.asOf == Date(timeIntervalSince1970: 1_788_696_000))
    }

    @Test("A quote with nothing to draw a line from says so")
    func decodesQuoteWithoutASeries() throws {
        let json = Data("""
        {"ticker":"F","display":"$F","company_name":"Ford Motor Company","exchange":"NYSE",
         "price":11.2,"price_label":"11.20","change_pct":0,"change_label":"+0.00%",
         "is_positive":true,"sparkline":[],"as_of":"2026-09-06T12:00:00Z"}
        """.utf8)

        let quote = try ContractTests.makeDecoder().decode(CashtagQuote.self, from: json)
        #expect(quote.sparkline.isEmpty)
        #expect(!quote.hasSparkline)
        #expect(quote.isPositive)
    }

    @Test("A dropdown row decodes")
    func decodesSuggestion() throws {
        let json = Data("""
        {"results":[{"ticker":"AAPL","display":"$AAPL","company_name":"Apple Inc.","exchange":"NASDAQ"}]}
        """.utf8)
        struct Page: Decodable { let results: [CashtagSuggestion] }

        let page = try ContractTests.makeDecoder().decode(Page.self, from: json)
        #expect(page.results.count == 1)
        #expect(page.results[0].display == "$AAPL")
        #expect(page.results[0].companyName == "Apple Inc.")
        #expect(page.results[0].id == "AAPL")
    }
}
