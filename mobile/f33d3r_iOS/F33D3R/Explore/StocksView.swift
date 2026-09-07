import SwiftUI
import F33D3RKit

/// One ticker: the quote, then what people are saying about it.
///
/// The web's /stocks/{ticker}, which is where a `$CASHTAG` in a body goes. The
/// card at the top is the same one the feed draws under a work — one store, so
/// arriving here does not re-ask for a price the feed already read.
struct StocksView: View {
    let ticker: String

    @Environment(AppModel.self) private var model

    var body: some View {
        WorkList(
            feed: model.stocksFeed(ticker: ticker),
            currentUser: model.state.user,
            emptyState: EmptyStateView(
                icon: "chart.xyaxis.line",
                title: "Nothing about $\(ticker) yet",
                message: "Works mentioning this ticker collect here as they are posted."
            )
        ) {
            CashtagCard(ticker: ticker)
                .padding(.horizontal, F33Card.paddingHorizontal)
                .padding(.bottom, F33Spacing.sm)
        }
        .navigationTitle("$\(ticker)")
        .navigationBarTitleDisplayMode(.inline)
    }
}
