import Foundation

/// `GET /api/v1/cashtag/{ticker}` — one stock quote.
///
/// Every label it prints was written by the server: the price, the percentage
/// and their signs. That is the same rule the scoreboard follows and it is
/// there for the same reason — how a price reads is the platform's statement,
/// and a phone rounding a number its own way would be a second opinion about
/// what a company is worth.
///
/// The one thing that is *not* the web's is the sparkline. The Facet carries an
/// SVG polyline normalised into a viewBox the server chose; this carries the
/// closes themselves, so the card can draw the line at whatever size it has.
public struct CashtagQuote: Codable, Hashable, Sendable, Identifiable {
    public var id: String { ticker }

    /// Bare: "AAPL".
    public let ticker: String
    /// As it is written in a body and on the card: "$AAPL".
    public let display: String
    /// Falls back to the ticker when the provider will not name the company,
    /// so there is always something under the symbol.
    public let companyName: String
    public let exchange: String

    public let price: Double
    public let priceLabel: String
    /// Against the previous close. Zero rather than -100 when the market is
    /// shut and there is no live price to compare against.
    public let changePct: Double
    public let changeLabel: String
    public let isPositive: Bool

    /// Closes over the last five days, oldest first, in the currency of the
    /// price. Fewer than two points is not a line and the card draws none.
    public let sparkline: [Double]
    /// When the provider priced this.
    public let asOf: Date

    enum CodingKeys: String, CodingKey {
        case ticker, display, exchange, price, sparkline
        case companyName = "company_name"
        case priceLabel = "price_label"
        case changePct = "change_pct"
        case changeLabel = "change_label"
        case isPositive = "is_positive"
        case asOf = "as_of"
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        ticker = try c.decode(String.self, forKey: .ticker)
        display = try c.decodeIfPresent(String.self, forKey: .display) ?? "$\(ticker)"
        companyName = try c.decodeIfPresent(String.self, forKey: .companyName) ?? ticker
        exchange = try c.decodeIfPresent(String.self, forKey: .exchange) ?? ""
        price = try c.decodeIfPresent(Double.self, forKey: .price) ?? 0
        priceLabel = try c.decodeIfPresent(String.self, forKey: .priceLabel) ?? ""
        changePct = try c.decodeIfPresent(Double.self, forKey: .changePct) ?? 0
        changeLabel = try c.decodeIfPresent(String.self, forKey: .changeLabel) ?? ""
        isPositive = try c.decodeIfPresent(Bool.self, forKey: .isPositive) ?? true
        sparkline = try c.decodeIfPresent([Double].self, forKey: .sparkline) ?? []
        asOf = try c.decodeIfPresent(Date.self, forKey: .asOf) ?? Date()
    }

    /// Whether there is a line to draw. Two points make one; one does not.
    public var hasSparkline: Bool { sparkline.count > 1 }
}

/// One row of the composer's `$` dropdown.
/// `GET /api/v1/cashtag/search?q=appl`.
public struct CashtagSuggestion: Codable, Hashable, Sendable, Identifiable {
    public var id: String { ticker }
    public let ticker: String
    public let display: String
    public let companyName: String
    public let exchange: String

    enum CodingKeys: String, CodingKey {
        case ticker, display, exchange
        case companyName = "company_name"
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        ticker = try c.decode(String.self, forKey: .ticker)
        display = try c.decodeIfPresent(String.self, forKey: .display) ?? "$\(ticker)"
        companyName = try c.decodeIfPresent(String.self, forKey: .companyName) ?? ticker
        exchange = try c.decodeIfPresent(String.self, forKey: .exchange) ?? ""
    }
}

/// Finding `$TICKER` in a body.
///
/// The pattern is the server's, character for character. `cashtagRe` in
/// handlers.go is what the web linkifies, `composeHighlightRe` is what the
/// composer underlines and `cashtagBodyRe` in works.go is what decides which
/// ticker a work's card is for — all three are `\$([A-Z]{1,5})\b`. A looser
/// rule here would light up words the server does not consider tickers and
/// then ask for a quote nobody can give; a stricter one would leave a live
/// cashtag as plain text on this platform alone.
///
/// `TestCashtagPatternMatchesTheApp` in feed-engine reads the literal below and
/// fails if the two ever drift.
public enum Cashtag {
    public static let pattern = #"\$([A-Z]{1,5})\b"#

    static let regex = try! NSRegularExpression(pattern: pattern)

    /// Every ticker in `body`, in the order they appear, each once.
    public static func tickers(in body: String) -> [String] {
        var seen: Set<String> = []
        var out: [String] = []
        for match in regex.matches(in: body, range: NSRange(body.startIndex..., in: body)) {
            guard match.numberOfRanges > 1,
                  let range = Range(match.range(at: 1), in: body) else { continue }
            let ticker = String(body[range])
            if seen.insert(ticker).inserted { out.append(ticker) }
        }
        return out
    }

    /// The ticker `typed` would become, or nil when it could never be one.
    ///
    /// This is the rule a *composer* needs, and it is deliberately not
    /// `pattern`. With the caret inside `$AAP` there is no word boundary yet,
    /// so the expression that decides what a finished body renders will
    /// correctly refuse it — a trigger built on `pattern` would never fire
    /// until the reader typed a space, which is one keystroke after the
    /// dropdown was any use.
    ///
    /// It accepts either case and answers in upper, because the server
    /// linkifies capitals only: a completion offered on `$appl` has to insert
    /// `$AAPL`, or the body it lands in renders as plain text. An empty string
    /// is a ticker in progress — the moment after `$` is typed — and answers
    /// `""`, so a composer can open its dropdown on the trigger itself.
    ///
    /// Trigger detection and body rendering cannot share one expression, but
    /// they can share one alphabet, and this is it.
    public static func typedTicker(_ typed: String) -> String? {
        guard typed.count <= 5 else { return nil }
        guard typed.allSatisfy({ $0.isASCII && $0.isLetter }) else { return nil }
        return typed.uppercased()
    }

    /// Whether `text` is a finished ticker — the shape `/api/v1/cashtag/
    /// {ticker}` accepts and `safeTickerRe` enforces on the way in.
    public static func isTicker(_ text: String) -> Bool {
        guard (1...5).contains(text.count) else { return false }
        return text.allSatisfy { $0.isASCII && $0.isUppercase && $0.isLetter }
    }

    /// The ticker a work's card is for.
    ///
    /// The first one, because that is the one the server picks:
    /// `extractCashtagTicker` takes the first match and stores it as the work's
    /// single cashtag embed, so a work mentioning three companies shows the
    /// same one card on both surfaces rather than a stack of them.
    public static func first(in body: String) -> String? {
        tickers(in: body).first
    }
}
