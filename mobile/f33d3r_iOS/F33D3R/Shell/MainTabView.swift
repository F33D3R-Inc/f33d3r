import SwiftUI
import F33D3RKit

/// The tabs of the signed-in shell.
enum AppTab: Hashable {
    case home, explore, messages, notifications, wallet
}

/// The signed-in shell: Home, Explore, Messages, Notifications, Wallet.
///
/// Five, because a sixth folds into "More" and a tab behind "More" is a tab
/// nobody visits. Visions is a lane on Home, one swipe from Following, and is
/// not repeated here.
///
/// Labelled tabs. Icons always carry their word — an icon-only bar makes every
/// reader guess at a glyph they see fifty times a day, and the guess is wrong
/// often enough to be a real cost. The bar is the system's, because a system
/// tab bar already gets the safe area, the 44pt targets, the selected state and
/// VoiceOver right, and a hand-built one would be re-earning all four.
///
/// On iOS 26 the bar is Liquid Glass and shrinks out of the way as the feed
/// scrolls down. Older systems get the same five tabs with a material behind
/// them. Search is on neither: it is a field in Explore's own chrome, which is
/// one tap from anywhere and needs no sixth tab.
///
/// One navigation stack per tab, each with the same route table, so a handle
/// tapped inside a work in Home pushes a profile onto Home rather than throwing
/// the reader into a different tab and losing where they were. Tapping the
/// selected tab again pops its stack, and if it is already at its root,
/// scrolls the list back to the top — the gesture every feed app has taught.
struct MainTabView: View {
    @Environment(AppModel.self) private var model

    @State private var selection: AppTab = .home
    /// One counter per tab. A re-tap bumps it; the tab's stack and lists watch.
    @State private var reselects: [AppTab: Int] = [:]

    var body: some View {
        Group {
            if #available(iOS 26.0, *) {
                modernTabs
            } else {
                legacyTabs
            }
        }
        .tint(F33Color.accent)
        // The bar floats over the feed, so it has to be glass or it is a white
        // brick sitting on a photograph. iOS 26 draws that itself; this is what
        // puts a material behind it everywhere else.
        .f33GlassTabBar()
        // Density is set once for the whole shell so a card looks the same in a
        // profile or a thread as it does in the feed the reader set it from.
        .feedDensity(model.preferences.density)
    }

    /// Selecting the tab already selected is a request, not a change.
    private var selectionBinding: Binding<AppTab> {
        Binding(
            get: { selection },
            set: { tab in
                if tab == selection {
                    reselects[tab, default: 0] += 1
                } else {
                    selection = tab
                }
            }
        )
    }

    @available(iOS 26.0, *)
    private var modernTabs: some View {
        TabView(selection: selectionBinding) {
            Tab("Home", systemImage: "house", value: AppTab.home) {
                stack(.home) { FeedView() }
            }
            Tab("Explore", systemImage: "safari", value: AppTab.explore) {
                stack(.explore) { ExploreView() }
            }
            Tab("Messages", systemImage: "bubble.left.and.bubble.right", value: AppTab.messages) {
                stack(.messages) { MessagesView() }
            }
            .badge(model.unreadMessageCount)
            Tab("Notifications", systemImage: "bell", value: AppTab.notifications) {
                stack(.notifications) { NotificationsView() }
            }
            .badge(model.unreadCount)
            Tab("Wallet", systemImage: "wallet.pass", value: AppTab.wallet) {
                stack(.wallet) { WalletView() }
            }
        }
        // The bar gets out of the way of what the reader is reading and comes
        // back on the first scroll up.
        .tabBarMinimizeBehavior(.onScrollDown)
        // The now-playing bar, in the tab bar's accessory slot while a track
        // is loaded. See `NowPlayingShell`.
        .nowPlayingShell()
    }

    private var legacyTabs: some View {
        TabView(selection: selectionBinding) {
            stack(.home) { FeedView() }
                .tabItem { Label("Home", systemImage: "house") }
                .tag(AppTab.home)

            stack(.explore) { ExploreView() }
                .tabItem { Label("Explore", systemImage: "safari") }
                .tag(AppTab.explore)

            stack(.messages) { MessagesView() }
                .tabItem { Label("Messages", systemImage: "bubble.left.and.bubble.right") }
                .badge(model.unreadMessageCount)
                .tag(AppTab.messages)

            stack(.notifications) { NotificationsView() }
                .tabItem { Label("Notifications", systemImage: "bell") }
                .badge(model.unreadCount)
                .tag(AppTab.notifications)

            stack(.wallet) { WalletView() }
                .tabItem { Label("Wallet", systemImage: "wallet.pass") }
                .tag(AppTab.wallet)

            // No Search tab here. Six labelled tabs fold the sixth into
            // "More", and a tab behind "More" is a tab nobody visits. Search
            // is a field in Explore's chrome instead — one tap to the tab, and
            // the field is already there to type into.
        }
    }

    private func stack<Content: View>(_ tab: AppTab, @ViewBuilder content: @escaping () -> Content) -> some View {
        RoutedStack(reselectTick: reselects[tab] ?? 0, content: content)
            // Before iOS 26 there is no accessory slot; the now-playing bar is
            // inset above the tab bar inside each tab instead. No-op on 26.
            .legacyNowPlayingInset()
    }
}

struct RoutedStack<Content: View>: View {
    /// Bumped when this tab is tapped while already selected.
    var reselectTick = 0
    @ViewBuilder let content: () -> Content

    @State private var path = NavigationPath()
    /// Forwarded to the lists only when the stack is already at its root; a
    /// re-tap with screens pushed pops them instead.
    @State private var scrollTick = 0
    @State private var composeHidden = false
    @State private var isComposing = false

    var body: some View {
        NavigationStack(path: $path) {
            content()
                .background(F33Color.bg)
                .navigationDestination(for: Route.self) { route in
                    destination(route)
                        .background(F33Color.bg)
                }
        }
        // Posting lives in the shell so it is in the same place on every
        // screen. Screens that should not have it say so themselves — see
        // `hidesComposeFAB`.
        .overlay(alignment: .bottomTrailing) {
            if !composeHidden {
                ComposeFAB { isComposing = true }
                    .transition(.scale.combined(with: .opacity))
            }
        }
        .animation(F33Motion.easeOut, value: composeHidden)
        // The listening bar, above the tab bar and outside the stack, so it
        // survives every push the reader makes while a frequency is on.
        .safeAreaInset(edge: .bottom, spacing: 0) {
            FrequencyMiniBar { id in path.append(Route.frequency(id: id)) }
        }
        .onComposeFABVisibility { composeHidden = $0 }
        .sheet(isPresented: $isComposing) {
            ComposeSheet(mode: .post)
        }
        .environment(\.scrollToTopTick, scrollTick)
        .environment(\.openURL, OpenURLAction { url in
            guard let route = WorkBodyText.route(from: url) else { return .systemAction }
            path.append(route)
            return .handled
        })
        .onChange(of: reselectTick) { _, _ in
            if path.isEmpty {
                scrollTick += 1
            } else {
                path = NavigationPath()
            }
        }
    }

    @ViewBuilder
    private func destination(_ route: Route) -> some View {
        switch route {
        case .work(let id):
            WorkDetailView(workID: id)
        case .workQuotes(let id):
            WorkQuotesView(workID: id)
        case .profile(let handle):
            ProfileView(handle: handle)
        case .live(let id):
            LiveViewerView(streamID: id)
        case .frequency(let id):
            FrequencyRoomView(frequencyID: id)
        case .frequencies:
            FrequencyListView()
        case .conversation(let id):
            ConversationView(conversationID: id)
        case .tag(let tag):
            TagView(tag: tag)
        case .stocks(let ticker):
            StocksView(ticker: ticker)
        case .search:
            SearchView()
        case .followers(let handle):
            FollowListView(handle: handle, kind: .followers)
        case .following(let handle):
            FollowListView(handle: handle, kind: .following)
        case .settings:
            SettingsView()
        case .numbers:
            NumbersView()
        }
    }
}
