import SwiftUI
import F33D3RKit

/// The scrolling list of works, and every state it can be in.
///
/// The home feed, a profile tab and a reply list are all this view over a
/// different `WorkFeed`. Pagination, pull-to-refresh, the skeleton, the empty
/// state and the retry footer are written once here rather than three times.
struct WorkList<Header: View>: View {
    let feed: WorkFeed
    let currentUser: CurrentUser?
    let emptyState: EmptyStateView
    /// Set on ranked surfaces, where the reader is entitled to push back on a
    /// choice the server made for them. Nil everywhere else: Following and a
    /// profile are lists the reader assembled themselves, and "don't recommend
    /// this" is a meaningless thing to say about a person you chose to follow.
    ///
    /// A closure rather than a view, so the list stays a list — the surface that
    /// owns the ranking is the surface that decides what the gesture does and
    /// what it says while there is no write path for it.
    var notInterested: ((Work) -> Void)?
    /// Chrome pinned above the list, with the works passing behind it.
    ///
    /// It goes on the scroll view itself rather than on whatever contains the
    /// list, because that is the only placement that both insets the content and
    /// keeps the bar above it — an inset applied further out is measured against
    /// the `GeometryReader` this view is rooted in, and the scroll view then
    /// draws straight over the bar as soon as anything is scrolled. It is also
    /// where `refreshable` looks for its offset, so the spinner lands under the
    /// bar rather than behind it.
    ///
    /// It pins *below* whatever a surface above already floats over this list
    /// (`laneTopInset`), never under it. That also makes it the wrong home for
    /// anything on a surface whose own chrome hides: this bar holds a fixed
    /// place, so it would sit over bare page the moment the chrome above it
    /// left. A bar that should travel with the works belongs in `header`.
    ///
    /// Type-erased to keep this one generic parameter instead of two: every call
    /// site that has no chrome would otherwise have to name a second phantom
    /// type, and this is one view built once per list rather than per row.
    var topChrome: AnyView?
    /// Called with how far the list has scrolled past its top, in points.
    /// Chrome above the list uses it to collapse.
    var onScroll: ((CGFloat) -> Void)?
    @ViewBuilder var header: () -> Header

    /// Bumped by the shell when this list's tab is tapped again, or by the
    /// lane's "new works" pill.
    @Environment(\.scrollToTopTick) private var scrollToTopTick
    /// Chrome a surface above this list floats over it (Home's header).
    @Environment(\.laneTopInset) private var laneTopInset
    /// The height of this list's own `topChrome`, measured.
    @State private var topChromeHeight: CGFloat = 0

    var body: some View {
        // The column is pinned to the width it is given, rather than to the
        // width of its widest row. One row that will not shrink — a long chip
        // at an accessibility text size, a tag list, a count that refuses to
        // wrap — would otherwise widen the whole stack, and the result is not
        // one wide row: it is every card in the feed sliding sideways off both
        // edges of the screen. A reader who has turned text size up is exactly
        // the reader who can least afford that.
        GeometryReader { proxy in
            list(width: proxy.size.width)
        }
    }

    private func list(width: CGFloat) -> some View {
        ScrollViewReader { proxy in
            scrollView(width: width, proxy: proxy)
                .onChange(of: scrollToTopTick) { _, _ in
                    withAnimation(F33Motion.easeOut) {
                        proxy.scrollTo(ScrollTopAnchor.top, anchor: .top)
                    }
                }
        }
    }

    private func scrollView(width: CGFloat, proxy: ScrollViewProxy) -> some View {
        ScrollView {
            LazyVStack(spacing: 0) {
                Color.clear
                    .frame(height: 0)
                    .id(ScrollTopAnchor.top)

                header()

                switch feed.phase {
                case .idle, .loadingFirstPage:
                    skeletons
                case .empty:
                    emptyState
                case .failed(let error):
                    ErrorStateView(error: error) {
                        Task { await feed.reload() }
                    }
                case .loaded:
                    works
                }
            }
            .frame(width: width)
            .clipped()
            .modifier(LegacyScrollOffset(onScroll: onScroll))
        }
        .coordinateSpace(name: ScrollOffsetSpace.lane)
        .modifier(ScrollOffsetReporter(onScroll: onScroll))
        // The chrome floats; the content keeps a margin its height. See
        // `laneTopInset` for why this is a margin and not a safe-area inset.
        .contentMargins(.top, laneTopInset + topChromeHeight, for: .scrollContent)
        .contentMargins(.top, laneTopInset + topChromeHeight, for: .scrollIndicators)
        .overlay(alignment: .top) {
            if let topChrome {
                topChrome
                    .onGeometryChange(for: CGFloat.self) { $0.size.height } action: { height in
                        if height > 0, height != topChromeHeight { topChromeHeight = height }
                    }
                    // Below the surface's own chrome, not under it. The margin
                    // above keeps room for both, so a bar drawn flush to the top
                    // hides behind the other one and leaves a band of empty page
                    // exactly its own height. Measured first, then offset, or
                    // the inset would be counted into the margin twice.
                    .padding(.top, laneTopInset)
            }
        }
        .background(F33Color.bg)
        .scrollDismissesKeyboard(.immediately)
        .refreshable { await feed.reload() }
        .modifier(DebugScrollAnchor())
        .modifier(DebugScrollDrive(feed: feed, proxy: proxy))
        // Keyed on the store's identity, not the view's: switching feed surface
        // swaps the store while the view stays put, and a plain `.task` would
        // never fire for the new one.
        .task(id: ObjectIdentifier(feed)) { await feed.loadFirstPageIfNeeded() }
    }

    private var skeletons: some View {
        // Three is enough to fill a phone screen; more would just animate
        // offscreen.
        ForEach(0..<3, id: \.self) { _ in
            WorkCardSkeleton()
            CardDivider()
        }
    }

    @ViewBuilder
    private var works: some View {
        ForEach(feed.works) { work in
            NavigationLink(value: Route.work(id: work.id)) {
                WorkCard(work: work, currentUser: currentUser)
            }
            .buttonStyle(.plain)
            // Long press rather than a control on the card: the card already
            // carries five actions and an overflow, and a sixth affordance drawn
            // on every row to serve a gesture used on one row in fifty is a worse
            // trade than the discoverability it buys. It is additive — the row
            // still opens on tap, and VoiceOver reaches it as a custom action.
            .modifier(NotInterestedGesture(work: work, action: notInterested))
            .onAppear {
                guard feed.shouldLoadMore(after: work) else { return }
                Task { await feed.loadMore() }
            }

            CardDivider()
        }

        footer
    }

    @ViewBuilder
    private var footer: some View {
        if let error = feed.loadMoreError {
            // The list above is still good, so this is a footer rather than a
            // replacement for the screen.
            VStack(spacing: F33Spacing.sm) {
                Text(error.userMessage)
                    .font(.footnote)
                    .foregroundStyle(F33Color.ink4)
                Button("Try again") {
                    Task { await feed.loadMore() }
                }
                .font(.subheadline.weight(.semibold))
                .foregroundStyle(F33Color.accent)
                .frame(minHeight: F33Layout.minTouchTarget)
            }
            .frame(maxWidth: .infinity)
            .padding(F33Spacing.lg)
        } else if feed.isLoadingMore {
            ProgressView()
                .frame(maxWidth: .infinity)
                .padding(F33Spacing.lg)
        } else if !feed.hasMore, !feed.works.isEmpty {
            Text("You're all caught up.")
                .font(.footnote)
                .foregroundStyle(F33Color.ink5)
                .frame(maxWidth: .infinity)
                .padding(.vertical, F33Spacing.xl)
        }
    }
}

extension WorkList where Header == EmptyView {
    init(
        feed: WorkFeed,
        currentUser: CurrentUser?,
        emptyState: EmptyStateView,
        notInterested: ((Work) -> Void)? = nil,
        topChrome: AnyView? = nil,
        onScroll: ((CGFloat) -> Void)? = nil
    ) {
        self.init(
            feed: feed,
            currentUser: currentUser,
            emptyState: emptyState,
            notInterested: notInterested,
            topChrome: topChrome,
            onScroll: onScroll
        ) { EmptyView() }
    }
}

/// Attaches "Not interested" to a row, and attaches nothing at all when the
/// surface did not ask for it.
///
/// A modifier rather than an `if` around the row, because branching on the
/// closure inside the `ForEach` would give the two branches different view
/// identities — and a list whose rows change identity is a list that loses its
/// scroll position the moment anything above it reloads.
private struct NotInterestedGesture: ViewModifier {
    let work: Work
    let action: ((Work) -> Void)?

    func body(content: Content) -> some View {
        if let action {
            content
                .contextMenu {
                    Button {
                        action(work)
                    } label: {
                        Label("Not interested", systemImage: "hand.thumbsdown")
                    }
                }
                // The context menu is a long press, which VoiceOver does not
                // offer and Switch Control cannot make. The same action as a
                // rotor entry is how it reaches everyone else.
                .accessibilityAction(named: Text("Not interested")) { action(work) }
        } else {
            content
        }
    }
}

/// Opens a list already scrolled, for one run.
///
/// The glass chrome only shows what it is for once something is passing behind
/// it, and a Simulator driven through `simctl` cannot scroll — there is no
/// gesture to send. Without this, "the feed runs under the bar" is a claim
/// nobody can check against a screenshot, which is how the last two layout bugs
/// shipped.
///
/// DEBUG only, opt-in per run, and it changes nothing else: `F33D3R_SCROLLED=1`.
private struct DebugScrollAnchor: ViewModifier {
    func body(content: Content) -> some View {
        #if DEBUG
        if ProcessInfo.processInfo.environment["F33D3R_SCROLLED"] == "1" {
            content.defaultScrollAnchor(.bottom)
        } else {
            content
        }
        #else
        content
        #endif
    }
}

/// Scrolls a list the way a finger would, for one run.
///
/// `F33D3R_SCROLL_TO_ROW=3` animates to the fourth work two seconds after the
/// first page lands; `F33D3R_SCROLL_BACK_ROW=0` animates back to that row
/// three seconds later. An animated scroll reports the same rising and
/// falling offsets a drag does, so chrome that hides on the way down and
/// returns on the way up can be watched doing both on a Simulator nobody can
/// touch.
///
/// DEBUG only, opt-in per run, writes nothing.
private struct DebugScrollDrive: ViewModifier {
    let feed: WorkFeed
    let proxy: ScrollViewProxy

    func body(content: Content) -> some View {
        #if DEBUG
        content.task(id: feed.phase) {
            guard feed.phase == .loaded,
                  let raw = ProcessInfo.processInfo.environment["F33D3R_SCROLL_TO_ROW"],
                  let row = Int(raw), feed.works.indices.contains(row) else { return }
            try? await Task.sleep(for: .seconds(2))
            withAnimation(.easeInOut(duration: 1.2)) {
                proxy.scrollTo(feed.works[row].id, anchor: .top)
            }
            guard let backRaw = ProcessInfo.processInfo.environment["F33D3R_SCROLL_BACK_ROW"],
                  let back = Int(backRaw), feed.works.indices.contains(back) else { return }
            try? await Task.sleep(for: .seconds(3))
            withAnimation(.easeInOut(duration: 1.2)) {
                proxy.scrollTo(feed.works[back].id, anchor: .top)
            }
        }
        #else
        content
        #endif
    }
}
