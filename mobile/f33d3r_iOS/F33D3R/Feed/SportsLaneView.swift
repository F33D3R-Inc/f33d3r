import SwiftUI
import F33D3RKit

/// The Sports lane: the scoreboard, then the works.
///
/// The same two halves the website's lane has. The scoreboard is the games this
/// deployment follows, from the server's own cache; the works below are the
/// `sports` surface — every work tagged sports, nfl, nba, soccer, f1 and the
/// two dozen others that row lists — which is why this is a lane and not a
/// topic somebody typed.
struct SportsLaneView: View {
    /// How far the lane has scrolled, for the chrome above it. Nil when this
    /// lane is drawn somewhere with no chrome to move.
    var onScroll: ((CGFloat) -> Void)?

    @Environment(AppModel.self) private var model
    @State private var scores: SportsBoardStore?

    var body: some View {
        WorkList(
            feed: model.homeFeed(surface: .sports),
            currentUser: model.state.user,
            emptyState: EmptyStateView(
                icon: "sportscourt",
                title: "No sports works yet",
                message: "Works tagged sports, and every league tag with it, land here."
            ),
            onScroll: onScroll
        ) {
            scoreboard
        }
        .task {
            let store = scores ?? SportsBoardStore(client: model.client)
            scores = store
            await store.load()
            store.startRefreshing()
        }
        .onDisappear { scores?.stopRefreshing() }
    }

    /// Above the works, and only when there is something to show. A lane that
    /// draws an empty scoreboard on a day with no games is spending the top of
    /// the screen saying nothing.
    ///
    /// It rides in the list rather than pinned over it. Home's header, its
    /// visions row and the lane strip already float over this lane, and a second
    /// pinned bar under three leagues of cards would take a third of the screen
    /// for as long as the reader reads — and would hold that place over bare
    /// page the moment the chrome above it hid. Scrolling away is also what
    /// scores are: something checked at the top of the lane, not a bar the feed
    /// is read through. A flick back to the top brings them straight back.
    @ViewBuilder
    private var scoreboard: some View {
        if let scores, let board = scores.board, !board.isEmpty {
            SportsRail(board: board)
        }
    }
}
