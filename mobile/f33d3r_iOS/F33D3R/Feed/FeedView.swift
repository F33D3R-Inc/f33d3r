import SwiftUI
import F33D3RKit

/// Home: the header, the lane strip, and one feed per lane.
///
/// The lanes are a pager rather than a switch, so moving between them is a
/// horizontal swipe and each one keeps its own scroll position. `WorkFeed`
/// instances live on the model, so a lane the reader left is still loaded — and
/// still where they left it — when they come back.
///
/// The lane is remembered across launches. That is the single most important
/// behaviour on this screen: a feed that reopens on the algorithmic lane after
/// the reader deliberately left it is the complaint this layout exists to
/// answer.
struct FeedView: View {
    @Environment(AppModel.self) private var model
    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    @State private var isComposing = false
    @State private var isManagingLanes = false
    @State private var isComposingVision = false
    @State private var openRing: VisionRing?
    /// Collapses the visions row once the feed has scrolled past its top.
    @State private var isTrayCollapsed = false
    /// Decides whether the header, the visions tray, the lane strip, the
    /// compose button and the tab bar are out of the way. The rule lives in
    /// ``ChromeScrollTracker`` so a scroll can be run through it in a test;
    /// reading it off a view is how it sat broken through an entire flick.
    @State private var tracker = ChromeScrollTracker()
    /// The chrome's full height, which is the list's constant top inset.
    @State private var chromeHeight: CGFloat = 0
    /// Bumped when the "new works" pill is tapped, on top of the shell's own.
    @State private var localScrollTick = 0
    @Environment(\.scrollToTopTick) private var shellScrollTick

    var body: some View {
        @Bindable var preferences = model.preferences

        lanes(selection: $preferences.lane)
            .background(F33Color.bg)
            .environment(\.scrollToTopTick, shellScrollTick + localScrollTick)
            // New works from followed accounts, announced by the server's
            // stream, wait behind a pill until the reader asks for them. The
            // list never moves under a thumb; the pill is the only way in.
            .overlay(alignment: .top) {
                if preferences.lane == .surface(.following), let pending = model.signals?.pendingNewWorkIDs.count, pending > 0 {
                    NewWorksPill(count: pending) {
                        Task {
                            await model.homeFeed(surface: .following).reload()
                            model.signals?.clearPendingWorks()
                            localScrollTick += 1
                        }
                    }
                    .padding(.top, chromeHeight + F33Spacing.md)
                    .transition(.move(edge: .top).combined(with: .opacity))
                }
            }
            .animation(reduceMotion ? nil : F33Motion.spring, value: model.signals?.pendingNewWorkIDs.count ?? 0)
            // The lanes keep a constant top content margin — the chrome's full
            // height — and the chrome itself is an overlay. The two are
            // deliberately separate: a scroll view whose inset changes while a
            // finger is on it has its geometry pulled out from under the
            // gesture, and that is what "scrolling stopped working" looks like.
            // With the margin fixed, the header can collapse, hide and return
            // freely while the feed under it never moves except by the reader's
            // hand. The margin is measured from the chrome once, fully shown,
            // so it fits any text size. It is a content margin and not a
            // safe-area inset because SwiftUI clips scroll content at an inset:
            // the feed has to run under the glass for the material to have
            // anything to refract, and for hiding the chrome to reveal rows
            // rather than bare page (`laneTopInset`).
            .environment(\.laneTopInset, chromeHeight)
            .overlay(alignment: .top) {
                chrome(preferences: preferences)
                    .onGeometryChange(for: CGFloat.self) { $0.size.height } action: { height in
                        // Only the full chrome sets the inset. Collapsed or hidden
                        // states are shorter than the space the list keeps, and
                        // they only ever occur once the reader has scrolled at
                        // least that far, so no gap opens between strip and feed.
                        if !isChromeHidden, !isTrayCollapsed, height > 0, height != chromeHeight {
                            chromeHeight = height
                        }
                    }
            }
            // The header does the navigation bar's job better than the bar would:
            // 44pt spent saying "Home" to someone who just opened the app is 44pt
            // of feed.
            .toolbar(.hidden, for: .navigationBar)

            // Reading takes the whole screen. The tab bar is hidden from here
            // rather than from the shell because a tab's own content is what
            // decides whether the bar belongs over it, and Home is the only
            // screen that makes this claim.
            .tabBarHidden(isChromeHidden)
            .animation(reduceMotion ? nil : F33Motion.easeOut, value: isChromeHidden)
            .sheet(isPresented: $isComposing) {
                ComposeSheet(mode: .post)
            }
            .sheet(isPresented: $isManagingLanes) {
                ManageLanesSheet()
            }
            .fullScreenCover(item: $openRing) { ring in
                VisionViewerView(store: model.visionTray(), initial: ring)
            }
            .fullScreenCover(isPresented: $isComposingVision) {
                VisionComposeView {
                    Task { await model.visionTray().reload() }
                }
            }
            .onAppear {
                #if DEBUG
                // `F33D3R_CHROME_HIDDEN=1` opens with the chrome already away,
                // for looking at the reading state on a Simulator that cannot
                // be scrolled by hand.
                // `F33D3R_MANAGE_LANES=1` opens the feed picker, which is
                // otherwise behind a tap the Simulator cannot make.
                if ProcessInfo.processInfo.environment["F33D3R_MANAGE_LANES"] == "1" {
                    isManagingLanes = true
                }
                if model.debugCompose { isComposing = true }
                if model.debugVisionCompose { isComposingVision = true }
                if model.debugVision {
                    Task {
                        let tray = model.visionTray()
                        await tray.loadIfNeeded()
                        openRing = tray.rings.first ?? tray.own
                    }
                }
                #endif
            }
    }

    /// The header, the visions row and the lane strip, as one glass slab. The
    /// header extends its fill up through the status bar and the strip carries
    /// the hairline that closes the pair; when the first two are hidden the
    /// strip paints the status bar itself.
    private func chrome(preferences: FeedPreferences) -> some View {
        @Bindable var preferences = preferences
        return VStack(spacing: 0) {
            if !isChromeHidden {
                FeedHeaderRow(account: model.state.user?.user)

                // The visions row sits between the wordmark and the lanes, and
                // shrinks to a strip on scroll so it never stands between the
                // reader and the first work.
                VisionTrayView(
                    store: model.visionTray(),
                    currentUser: model.state.user?.user,
                    isCollapsed: isTrayCollapsed,
                    onOpen: { openRing = $0 },
                    onCompose: { isComposingVision = true },
                    onMute: { ring in Task { try? await model.visionTray().muteVisions(from: ring.author.handle) } }
                )

                // The lane strip goes with them. Reading takes the whole
                // screen, and the first scroll up brings the three back
                // together rather than in pieces.
                LaneStrip(
                    lane: $preferences.lane,
                    lanes: preferences.lanes,
                    onManage: { isManagingLanes = true },
                    liveCount: liveCount
                )

                // Frequencies on air, under the lanes. It draws nothing at all
                // when nobody is on air, so Home is unchanged on a quiet day.
                FrequencyRailView()
            }
        }
        .animation(reduceMotion ? nil : F33Motion.easeOut, value: isChromeHidden)
    }

    /// One offset, reported by whichever lane the reader is on: it collapses
    /// the visions row and decides whether the chrome has left.
    private func laneScrolled(_ offset: CGFloat, on lane: FeedLane) {
        // Only the lane on screen decides; a lane the pager keeps warm
        // off-screen reports offsets too.
        guard lane == model.preferences.lane else { return }
        // Once the content has moved at least as far as the row shrinks, so
        // the strip never has a gap of page under it.
        let collapsed = offset > 96
        if collapsed != isTrayCollapsed { isTrayCollapsed = collapsed }
        trackDirection(offset)
    }

    /// Scrolling down past the first screen hides the chrome; any scroll up,
    /// or a return to the top, shows it. The thresholds keep a finger resting
    /// on the list from flickering it.
    /// Hands one reported offset to the tracker, with the chrome's measured
    /// height so it never leaves while content is still behind it.
    private func trackDirection(_ offset: CGFloat) {
        tracker.chromeHeight = chromeHeight
        tracker.offsetChanged(to: offset)
    }

    /// Whether the feed has the screen to itself.
    private var isChromeHidden: Bool {
        #if DEBUG
        // Pinned for the run, so a Simulator that cannot be scrolled by hand
        // can be photographed in the reading state.
        if ProcessInfo.processInfo.environment["F33D3R_CHROME_HIDDEN"] == "1" { return true }
        #endif
        return tracker.isHidden
    }

    private func lanes(selection: Binding<FeedLane>) -> some View {
        TabView(selection: selection) {
            ForEach(model.preferences.lanes) { lane in
                laneContent(lane)
                    .tag(lane)
            }
        }
        .tabViewStyle(.page(indexDisplayMode: .never))
        // The strip above is the index; a second row of dots over the feed would
        // say the same thing again, in the way of the content.
        .animation(reduceMotion ? nil : F33Motion.easeOut, value: selection.wrappedValue)
    }

    /// The Live badge travels as page metadata on whichever lane is loaded, so
    /// it is read from the lane the reader is on rather than by loading the Live
    /// lane behind their back to count it.
    ///
    /// Once the Live lane itself has looked, its list is the fresher answer:
    /// the stream's `live_start` frame refreshes it the moment somebody goes on
    /// air, while page metadata is only as new as the last feed request.
    private var liveCount: Int? {
        model.liveRoomCount ?? feed(for: model.preferences.lane).liveCount
    }

    /// The lane's own view.
    ///
    /// Music, Live and Sports are the lanes that are a surface rather than a
    /// list: a player, a room list and a scoreboard, each owing the reader
    /// something a feed of works does not carry. The rest are the same
    /// `WorkList` over a different store, which is the whole point of the pager.
    ///
    /// All four report their scrolling the same way. A lane that draws itself is
    /// still a lane, and the chrome getting out of the way on For you while it
    /// stayed put on Live was the same bug arriving by a different door.
    @ViewBuilder
    private func laneContent(_ lane: FeedLane) -> some View {
        switch lane {
        case .surface(.music):
            MusicLaneView(onScroll: { laneScrolled($0, on: lane) })
        case .surface(.live):
            LiveLaneView(onScroll: { laneScrolled($0, on: lane) })
        case .surface(.sports):
            SportsLaneView(onScroll: { laneScrolled($0, on: lane) })
        default:
            WorkList(
                feed: model.homeFeed(lane: lane),
                currentUser: model.state.user,
                emptyState: emptyState(for: lane),
                onScroll: { laneScrolled($0, on: lane) }
            )
        }
    }

    /// The store behind a lane. Music's is shared with the Music surface, so the
    /// reader does not find two scroll positions for one feed.
    private func feed(for lane: FeedLane) -> WorkFeed {
        lane == .surface(.music) ? model.musicFeed() : model.homeFeed(lane: lane)
    }

    /// A lane with nothing in it says what that lane is for.
    ///
    /// This is also what a lane the server cannot serve yet degrades to: a feed
    /// request for a surface with no rows behind it comes back empty, and an
    /// empty lane explaining itself is a better answer than an error explaining
    /// our deployment schedule.
    private func emptyState(for lane: FeedLane) -> EmptyStateView {
        guard case .surface(let surface) = lane else {
            // A topic the reader added that nobody has posted to. It is not a
            // fault and not an outage, so it says what the lane is and leaves
            // it there.
            return EmptyStateView(
                icon: "number",
                title: "Nothing tagged \(lane.title) yet",
                message: "Works carrying this tag land here as they are posted."
            )
        }
        switch surface {
        case .following:
            return EmptyStateView(
                icon: "person.2",
                title: "No works from people you follow",
                message: "Follow a few accounts and their works land here, newest first."
            )
        case .forYou:
            return EmptyStateView(
                icon: "sparkles",
                title: "Nothing here yet",
                message: "As you follow people and interact, this fills up."
            )
        case .music:
            // Unreachable — the Music lane draws `MusicLaneView`, which carries
            // its own. Deferred to the same value rather than restated, so the
            // two cannot drift into saying different things about one surface.
            return MusicLaneView.emptyState
        case .visions:
            // What the server's `visions` surface actually selects: media-rich
            // works from the viewer and the accounts they follow. The 24-hour
            // life belonged to the vision `kind` that migration 0010 removed,
            // and promising it here would be describing a product we no longer
            // ship.
            return EmptyStateView(
                icon: "circle.dashed",
                title: "Nothing to see yet",
                message: "Works with a photo or a video, from the accounts you follow, collect here."
            )
        case .nsfw:
            return EmptyStateView(
                icon: "eye.slash",
                title: "Nothing here yet",
                message: "Works marked 18+ collect here."
            )
        case .sports:
            // Unreachable — the Sports lane draws SportsLaneView, which carries
            // its own. Deferred rather than restated so the two cannot drift.
            return EmptyStateView(
                icon: "sportscourt",
                title: "No sports works yet",
                message: "Works tagged sports, and every league tag with it, land here."
            )
        case .trending:
            return EmptyStateView(
                icon: "chart.line.uptrend.xyaxis",
                title: "Nothing is moving yet",
                message: "Works the platform is reacting to most collect here."
            )
        case .live:
            return EmptyStateView(
                icon: "dot.radiowaves.left.and.right",
                title: "Nobody is live",
                message: "Live rooms show up here the moment they start."
            )
        }
    }
}


/// "3 new works" over the Following lane. Glass, because it floats over the
/// feed and has to stay legible on whatever is scrolling under it.
struct NewWorksPill: View {
    let count: Int
    let action: () -> Void

    var body: some View {
        Button(action: action) {
            HStack(spacing: 6) {
                Image(systemName: "arrow.up")
                    .font(.system(size: 12, weight: .bold))
                Text(count == 1 ? "1 new work" : "\(count) new works")
                    .font(.system(size: 14, weight: .semibold))
                    .monospacedDigit()
            }
            .foregroundStyle(F33Color.accentInk)
            .shadow(color: .black.opacity(0.2), radius: 2, y: 1)
            .padding(.horizontal, F33Spacing.lg)
            .frame(height: 38)
            .f33Glass(in: Capsule(), tint: F33Color.accent, elevation: .floating, interactive: true)
            .frame(minHeight: F33Layout.minTouchTarget)
            .contentShape(Capsule())
        }
        .buttonStyle(.plain)
        .accessibilityLabel(count == 1 ? "Show 1 new work" : "Show \(count) new works")
    }
}
