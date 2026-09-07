import SwiftUI
import F33D3RKit

/// The works that quote one work.
///
/// `WorkList` over `GET /api/v1/works/{id}/quotes`, like every other list in
/// the app, because a quote is an ordinary work — it draws with the same card
/// it draws with in a feed, bordered quote embed and all. The screen exists
/// only to give "View quotes" on a work's detail somewhere to go.
///
/// The feed is held in `@State` rather than taken from the model's cache: this
/// list is scoped to one work and read once on the way past, so caching it
/// would keep a page of works alive for every quoted work the reader ever
/// opened.
struct WorkQuotesView: View {
    let workID: String

    @Environment(AppModel.self) private var model

    @State private var feed: WorkFeed?

    var body: some View {
        // A real view, not an `EmptyView`-resolving `Group`: a placeholder that
        // resolves to nothing gets no `task`, and the feed would never be made.
        Group {
            if let feed {
                WorkList(
                    feed: feed,
                    currentUser: model.state.user,
                    emptyState: EmptyStateView(
                        icon: "quote.bubble",
                        title: "No quotes yet",
                        message: "When someone quotes this work, it shows up here."
                    )
                ) {
                    EmptyView()
                }
            } else {
                Color.clear
            }
        }
        .navigationTitle("Quotes")
        .navigationBarTitleDisplayMode(.inline)
        .task {
            guard feed == nil else { return }
            let client = model.client
            let id = workID
            feed = WorkFeed { cursor in
                try await client.quotes(workID: id, cursor: cursor)
            }
        }
    }
}
