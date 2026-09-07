import SwiftUI
import F33D3RKit

/// Chooses the app's top-level surface from the session state.
///
/// Every transition here is driven by `SessionStore`, so a session that expires
/// or is revoked from another device lands the user back on sign-in wherever
/// they happen to be.
struct RootView: View {
    @Environment(AppModel.self) private var model

    /// Frequencies' own projection of the shell: the three lanes, and the room
    /// the reader is tuned into. It lives here rather than on ``AppModel``
    /// because a listener who leaves the room screen is still listening, and
    /// the store has to outlive the view for the mini bar to have anything to
    /// draw.
    @State private var frequencies = FrequencySession()

    /// The profile's accent theme. Read here, at the root, so every token
    /// below repaints when `/me` says the theme changed.
    private var theme: F33Theme {
        F33Theme.named(model.state.user?.user.themeID)
    }

    var body: some View {
        // The tokens read `F33Theme.current`; it is set before the tree below
        // is built so the first frame is already the right colour.
        let _ = { F33Theme.current = theme }()

        Group {
            switch model.state {
            case .unknown:
                LaunchView()
            case .signedOut:
                LoginView()
            case .needsBackupCodes(let user):
                BackupCodesRequiredView(user: user)
            case .signedIn:
                #if DEBUG
                if let page = FrequencyDebug.page {
                    FrequencyDebugHost(page: page)
                } else if let page = model.debugPage {
                    DebugPageHost(page: page)
                } else {
                    MainTabView()
                }
                #else
                MainTabView()
                #endif
            }
        }
        // A new theme rebuilds the shell: colour tokens are read when a view
        // renders, and a theme is chosen once in a long while — the moment
        // after "Save" is when the reader expects everything to change.
        .id(theme.id)
        .environment(frequencies)
        .task { frequencies.attach(client: model.client) }
        .tint(F33Color.accent)
        .preferredColorScheme(model.preferences.appearance.colorScheme)
        .animation(F33Motion.easeOut, value: model.state)
    }
}

#if DEBUG
/// Roots the app at one named surface for a single run.
///
/// The Simulator this app is developed against has no Simulator.app, so there is
/// no way to tap a tab: every run is `simctl launch` followed by a screenshot.
/// Without a way in, a screen that is not the first tab is a screen that ships
/// having been compiled and never looked at — which is exactly how the two
/// layout bugs before this one got through.
///
/// It stands in for `MainTabView` rather than living inside it, so nothing about
/// the real shell changes and no tab bar has to be driven. It reproduces the two
/// things the shell puts around a surface — a routed navigation stack and the
/// reader's density — because a screen looked at without them is not the screen
/// that ships.
///
/// DEBUG only, opt-in per run, writes nothing: `F33D3R_PAGE=music`.
private struct DebugPageHost: View {
    let page: AppModel.DebugPage

    @Environment(AppModel.self) private var model

    var body: some View {
        RoutedStack {
            surface
        }
        .feedDensity(model.preferences.density)
    }

    @ViewBuilder
    private var surface: some View {
        switch page {
        case .home:
            FeedView()
        case .explore:
            ExploreView()
        case .music:
            MusicLaneView()
                .navigationTitle("Music")
                .navigationBarTitleDisplayMode(.inline)
        case .visions:
            VisionsView()
        case .notifications:
            NotificationsView()
        case .messages:
            MessagesView()
        case .conversation:
            // `F33D3R_PAGE=conversation F33D3R_CONVERSATION=<id>` roots at one
            // thread, which is otherwise only reachable by tapping a row.
            if let id = ProcessInfo.processInfo.environment["F33D3R_CONVERSATION"] {
                ConversationView(conversationID: id)
            } else {
                MessagesView()
            }
        case .wallet:
            WalletView()
        case .profile:
            ProfileView(handle: model.debugProfile ?? "dev")
        case .work:
            if let id = model.debugWork {
                WorkDetailView(workID: id)
            } else {
                FeedView()
            }
        case .workquotes:
            // `F33D3R_PAGE=workquotes F33D3R_WORK=<id>` roots at the quotes
            // list, which is otherwise only reachable by tapping "View quotes"
            // under a work.
            if let id = model.debugWork {
                WorkQuotesView(workID: id)
            } else {
                FeedView()
            }
        case .live:
            if let id = ProcessInfo.processInfo.environment["F33D3R_LIVE"] {
                LiveViewerView(streamID: id)
            } else {
                FeedView()
            }
        case .search:
            SearchView()
                .onAppear {
                    if let query = model.debugQuery { model.searchStore().query = query }
                }
        case .settings:
            SettingsView()
        case .followers:
            FollowListView(handle: model.debugProfile ?? "dev", kind: .followers)
        case .following:
            FollowListView(handle: model.debugProfile ?? "dev", kind: .following)
        case .tag:
            TagView(tag: model.debugTag ?? "f33d3r")
        case .editprofile:
            ProfileView(handle: model.state.user?.user.handle ?? "dev", debugOpensEditor: true)
        case .stocks:
            StocksView(ticker: model.debugTicker ?? "AAPL")
        }
    }
}
#endif

/// Shown while the stored token is checked against the server. Deliberately bare
/// — it is on screen for a few hundred milliseconds and a spinner that flashes
/// is worse than one that doesn't.
struct LaunchView: View {
    var body: some View {
        ZStack {
            F33Color.bg.ignoresSafeArea()
            ProgressView()
        }
    }
}

/// The server requires backup codes before it will let this account do anything,
/// so the app must not drop the user into a feed it will bounce them out of.
///
/// The codes are generated here, in the app. It used to open
/// `/backup-codes/setup` in Safari, which is a cookie-authenticated web page
/// reached from a browser holding no session — a link to a sign-in screen
/// dressed as a setup flow. The event lane already has the generator; a
/// deployment whose lane does not, says so in the sheet.
struct BackupCodesRequiredView: View {
    let user: CurrentUser
    @Environment(AppModel.self) private var model

    @State private var isGenerating = false

    var body: some View {
        ZStack {
            F33Color.bg.ignoresSafeArea()

            VStack(spacing: F33Spacing.lg) {
                Image(systemName: "key.horizontal.fill")
                    .font(.system(size: 44))
                    .foregroundStyle(F33Color.accent)

                Text("One more step")
                    .font(.title2.weight(.semibold))
                    .foregroundStyle(F33Color.ink)

                Text("""
                    \(user.user.displayName), your account needs backup codes before you \
                    can continue. They're the only way back in if you lose your password.
                    """)
                    .font(.callout)
                    .foregroundStyle(F33Color.ink3)
                    .multilineTextAlignment(.center)

                Button("Set up backup codes") {
                    isGenerating = true
                }
                .buttonStyle(F33PrimaryButtonStyle())

                Button("Sign out") {
                    Task { await model.signOut() }
                }
                .font(.callout)
                .foregroundStyle(F33Color.ink3)
            }
            .frame(maxWidth: F33Layout.feedMaxWidth)
            .padding(F33Spacing.xl)
        }
        .sheet(isPresented: $isGenerating) {
            BackupCodesSheet {
                // The gate was waiting on exactly this. Re-reading `/me` is
                // what lets the reader through, and it happens as soon as the
                // codes exist rather than when the sheet is dismissed — so
                // the screen behind has already changed by the time they look.
                Task { await model.refreshMe() }
            }
        }
    }
}
