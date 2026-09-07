import SwiftUI
import F33D3RKit
import Observation
import OSLog

/// Every quote the app has read, one per ticker.
///
/// A feed can carry the same ticker in three works and a scroll can bring a
/// card back a dozen times; without this each of those is a request for a
/// number the app already has. The server caches quotes for fifteen minutes,
/// so the answer would be identical every time anyway.
///
/// Nothing here is invented: the store holds what the server sent and hands it
/// back. A ticker it could not read has no entry, and a card with no entry
/// draws nothing at all — a price is not something to guess at, and an error
/// box in the middle of somebody's feed is worse than silence.
@MainActor
@Observable
final class CashtagQuotes {
    static let shared = CashtagQuotes()

    /// Why a ticker has no card. A price the server could not read is not a
    /// thing to pass over in silence — the reason is written down here so a
    /// missing card can be explained instead of guessed at.
    private static let log = Logger(subsystem: "com.f33d3r.ios", category: "cashtag")

    /// How long a quote is worth keeping before asking again. Shorter than the
    /// server's own fifteen minutes, so a card that has been off screen for a
    /// while refreshes on its way back without the app deciding when a price
    /// has gone stale.
    private static let freshFor: TimeInterval = 60

    private var quotes: [String: CashtagQuote] = [:]
    private var readAt: [String: Date] = [:]
    private var inFlight: Set<String> = []
    /// Tickers a read has already been attempted for. Without this a symbol
    /// whose read failed on the network would be indistinguishable from one
    /// nobody has asked about yet, and its card would wait for an answer that
    /// had already come back.
    private var attempted: Set<String> = []
    /// Symbols this deployment's provider does not list. Asked once: a word
    /// that was never a company will not become one while somebody scrolls.
    private var unlisted: Set<String> = []
    /// True once the server has said it has no quotes provider. Nothing asks
    /// again after that, and every card stays empty — the feature is simply
    /// not part of this deployment.
    private var unavailable = false

    private init() {}

    func quote(for ticker: String) -> CashtagQuote? { quotes[ticker] }

    /// Whether this ticker is still worth waiting for: no price yet, nothing
    /// has ruled it out, and the answer is either on its way or not yet asked
    /// for. What a card shows a placeholder for.
    func isPending(_ ticker: String) -> Bool {
        guard quotes[ticker] == nil, !unavailable, !unlisted.contains(ticker) else { return false }
        return inFlight.contains(ticker) || !attempted.contains(ticker)
    }

    /// Reads one ticker, unless the app already has it, is already reading it,
    /// or has been told there is nothing to read.
    func load(_ ticker: String, using client: APIClient) async {
        guard !unavailable, !unlisted.contains(ticker), !inFlight.contains(ticker) else { return }
        if let read = readAt[ticker], Date().timeIntervalSince(read) < Self.freshFor { return }

        inFlight.insert(ticker)
        attempted.insert(ticker)
        defer { inFlight.remove(ticker) }

        do {
            quotes[ticker] = try await client.cashtag(ticker: ticker)
            readAt[ticker] = Date()
        } catch let error as APIError where error.code == "cashtag_unavailable" {
            unavailable = true
            Self.log.error("no quotes provider on this deployment: \(error.localizedDescription, privacy: .public)")
        } catch let error as APIError where error.code == "cashtag_not_found" {
            unlisted.insert(ticker)
            Self.log.info("\(ticker, privacy: .public) is not a listed symbol")
        } catch {
            // A failed read keeps whatever is already on screen and leaves the
            // rest blank. It is retried the next time the card appears.
            Self.log.error("could not read \(ticker, privacy: .public): \(error.localizedDescription, privacy: .public)")
        }
    }
}

/// The quote under a work that mentions a ticker.
///
/// The web draws this as a Facet the browser swaps in; this asks for the same
/// snapshot as data and draws it. Every word on it — the price, the percentage
/// and its sign — was written by the server, for the same reason the
/// scoreboard's are.
struct CashtagCard: View {
    let ticker: String

    @Environment(AppModel.self) private var model

    var body: some View {
        // `content` and not an inline `Group { if let … }`. A conditional that
        // resolves to nothing is an `EmptyView`, an `EmptyView` is never placed
        // in the view hierarchy, and a `.task` attached to one never runs — so
        // the card asked for its price exactly never, drew nothing for want of
        // an answer, and stayed empty for the life of the app. There has to be
        // something on screen for the request to happen inside, which is what
        // the placeholder below is for.
        content
            .task(id: ticker) {
                await CashtagQuotes.shared.load(ticker, using: model.client)
            }
    }

    @ViewBuilder
    private var content: some View {
        if let quote = CashtagQuotes.shared.quote(for: ticker) {
            NavigationLink(value: Route.stocks(ticker: quote.ticker)) {
                row(quote)
            }
            .buttonStyle(.plain)
        } else if CashtagQuotes.shared.isPending(ticker) {
            placeholder
        } else {
            // A symbol the provider does not list, or a deployment with no
            // quotes at all. Nothing to say, and no space held for it.
            Color.clear.frame(height: 0)
        }
    }

    /// The card's shape while the price is on its way. The same height as the
    /// real row, so the body above it does not jump when the answer lands.
    private var placeholder: some View {
        HStack(spacing: F33Spacing.sm) {
            VStack(alignment: .leading, spacing: 4) {
                RoundedRectangle(cornerRadius: 3, style: .continuous)
                    .fill(F33Color.bgSunken)
                    .frame(width: 54, height: 11)
                RoundedRectangle(cornerRadius: 3, style: .continuous)
                    .fill(F33Color.bgSunken)
                    .frame(width: 88, height: 9)
            }
            Spacer(minLength: F33Spacing.sm)
            RoundedRectangle(cornerRadius: 3, style: .continuous)
                .fill(F33Color.bgSunken)
                .frame(width: 64, height: 11)
        }
        .padding(10)
        .background(F33Color.bgElevated, in: RoundedRectangle(cornerRadius: F33Radius.md, style: .continuous))
        .overlay(
            RoundedRectangle(cornerRadius: F33Radius.md, style: .continuous)
                .strokeBorder(F33Color.hairline, lineWidth: 1)
        )
        .padding(.top, F33Card.mediaTopInset)
        .accessibilityHidden(true)
    }

    private func row(_ quote: CashtagQuote) -> some View {
        HStack(spacing: F33Spacing.sm) {
            VStack(alignment: .leading, spacing: 1) {
                Text(quote.display)
                    .font(.system(size: 14, weight: .semibold))
                    .foregroundStyle(F33Color.ink)
                Text(quote.companyName)
                    .font(.system(size: 11))
                    .foregroundStyle(F33Color.ink4)
                    .lineLimit(1)
            }

            Spacer(minLength: F33Spacing.sm)

            if quote.hasSparkline {
                // An explicit frame, both dimensions. A Shape given one is
                // greedy in the other and would push this row as wide as the
                // card will go.
                Sparkline(values: quote.sparkline)
                    .stroke(direction(quote), style: StrokeStyle(lineWidth: 1.5, lineCap: .round, lineJoin: .round))
                    .frame(width: 72, height: 26)
                    .accessibilityHidden(true)
            }

            VStack(alignment: .trailing, spacing: 1) {
                Text(quote.priceLabel)
                    .font(.system(size: 14, weight: .semibold).monospacedDigit())
                    .foregroundStyle(F33Color.ink)
                Text(quote.changeLabel)
                    .font(.system(size: 11, weight: .medium).monospacedDigit())
                    .foregroundStyle(direction(quote))
            }
        }
        .padding(10)
        .background(F33Color.bgElevated, in: RoundedRectangle(cornerRadius: F33Radius.md, style: .continuous))
        .overlay(
            RoundedRectangle(cornerRadius: F33Radius.md, style: .continuous)
                .strokeBorder(F33Color.hairline, lineWidth: 1)
        )
        // A view is hit-tested where it draws, and a background is not drawing
        // by that rule: without this only the two labels would be tappable.
        .contentShape(Rectangle())
        .padding(.top, F33Card.mediaTopInset)
        .accessibilityElement(children: .combine)
        .accessibilityLabel("\(quote.display), \(quote.companyName), \(quote.priceLabel), \(quote.changeLabel)")
    }

    private func direction(_ quote: CashtagQuote) -> Color {
        quote.isPositive ? F33Color.ok : F33Color.danger
    }
}

/// A price series as a line.
///
/// A `Shape` rather than a drawn view, so it takes exactly the frame it is
/// given and the caller decides how much room a sparkline is worth.
///
/// The line is normalised over its own range rather than from zero: a share
/// that moved a dollar on two hundred should show that dollar, which is the
/// whole point of the shape. That is also why it is never labelled — it says
/// which way, not how much, and the percentage beside it says how much.
struct Sparkline: Shape {
    let values: [Double]

    func path(in frame: CGRect) -> Path {
        var path = Path()
        guard values.count > 1 else { return path }

        // Half the stroke of the widest line this is drawn with, so the top and
        // bottom of the run stay inside the frame instead of being clipped by
        // whatever the shape is placed in.
        let rect = frame.insetBy(dx: 1, dy: 1)

        let low = values.min() ?? 0
        let high = values.max() ?? 0
        // A flat series has no range to normalise over; it draws down the
        // middle rather than dividing by nothing.
        let span = high - low
        let step = rect.width / CGFloat(values.count - 1)

        for (index, value) in values.enumerated() {
            let ratio = span > 0 ? (value - low) / span : 0.5
            let point = CGPoint(
                x: rect.minX + CGFloat(index) * step,
                y: rect.maxY - CGFloat(ratio) * rect.height
            )
            index == 0 ? path.move(to: point) : path.addLine(to: point)
        }
        return path
    }
}
