import SwiftUI
import F33D3RKit

/// Music: a player and a store over the works this deployment's Music surface
/// selects.
///
/// The surface is a server-side definition, not a client filter. `feed_surfaces`
/// holds a tag set and a content type for `music`, and the same row backs the
/// home lane, the web's discovery sheet and this screen — so the surface cannot
/// mean one thing here and another there, and the app never assembles its own
/// idea of what counts as music.
///
/// What the app does with the page is arrange it the way a music app does
/// rather than the way a feed does. A track is a work with sound attached, so
/// the page is read into three sections, each drawn from the same rows:
///
/// - **New** — the latest tracks as artwork tiles, scrolling sideways.
/// - **Store** — the tracks with a price, with the price or "Owned" beside
///   each, and a buy control that sends `work_purchase` and waits for the
///   server's row.
/// - **Tracks** — every track, in the order the server sent them, with the
///   one loaded marked.
///
/// A work on the surface with no sound — a post tagged music — is still on the
/// surface, and is drawn below as the card it is, so the page shows what the
/// server sent and hides none of it.
///
/// Tapping a track plays it through `MusicPlayer`, with the section's tracks
/// as the queue. Nothing here is a filter: the sections are the same rows
/// sorted into what a listener wants to do with them.
///
/// Drawn as the Music lane inside Home, so it is a list without chrome of its
/// own — the lane strip above it has already said where the reader is.
struct MusicLaneView: View {
    /// How far the lane has scrolled, for the chrome above it. Nil on the Music
    /// tab, which is this same view rooted in its own screen with a navigation
    /// bar of its own and no floating chrome to get out of the way.
    var onScroll: ((CGFloat) -> Void)?

    @Environment(AppModel.self) private var model

    /// Home's chrome floats over this lane; the content starts below it.
    @Environment(\.laneTopInset) private var laneTopInset

    var body: some View {
        let feed = model.musicFeed()
        let player = model.musicPlayer()
        ScrollView {
            LazyVStack(alignment: .leading, spacing: 0) {
                MusicSurfaceNote()

                switch feed.phase {
                case .idle, .loadingFirstPage:
                    MusicSkeleton()
                case .empty:
                    Self.emptyState
                case .failed(let error):
                    ErrorStateView(error: error) {
                        Task { await feed.reload() }
                    }
                case .loaded:
                    sections(works: feed.works.map(model.current), player: player)
                    footer(feed)
                }
            }
            .modifier(LegacyScrollOffset(onScroll: onScroll))
        }
        .coordinateSpace(name: ScrollOffsetSpace.lane)
        // The same offsets a `WorkList` lane reports, through the same path, so
        // Home's chrome behaves identically whichever lane the pager is on.
        .modifier(ScrollOffsetReporter(onScroll: onScroll))
        .contentMargins(.top, laneTopInset, for: .scrollContent)
        .contentMargins(.top, laneTopInset, for: .scrollIndicators)
        .background(F33Color.bg)
        .refreshable { await feed.reload() }
        .task {
            await feed.loadFirstPageIfNeeded()
            #if DEBUG
            if MusicDebug.autoPlay, player.nowPlaying == nil {
                let tracks = feed.works.filter(\.isPlayable)
                if let first = tracks.first { player.play(first, queue: tracks) }
            }
            #endif
        }
    }

    // MARK: Sections

    @ViewBuilder
    private func sections(works: [Work], player: MusicPlayer) -> some View {
        let tracks = works.filter { $0.isPlayable && !model.isDeleted($0.id) }
        let posts = works.filter { !$0.isPlayable && !model.isDeleted($0.id) }
        let store = tracks.filter(\.isForSale)

        if !tracks.isEmpty {
            MusicSectionHeader(title: "New", subtitle: "The latest tracks on F33D3R.")
            ScrollView(.horizontal, showsIndicators: false) {
                LazyHStack(alignment: .top, spacing: F33Spacing.md) {
                    ForEach(tracks.prefix(12)) { track in
                        NewTrackTile(
                            work: track,
                            isCurrent: player.isCurrent(track),
                            isPlaying: player.isPlaying
                        ) {
                            player.play(track, queue: tracks)
                        }
                    }
                }
                .padding(.horizontal, F33Card.paddingHorizontal)
            }
            // Margins are inherited by nested scroll views; this row has none.
            // It clips at its own bounds — the default, and it has to stay the
            // default: tiles drawn outside a horizontal run land on whatever
            // the lane floats above it, which is how the Sports scoreboard
            // ended up smeared sideways under the strip.
            .contentMargins(.top, 0, for: .scrollContent)
        }

        if !store.isEmpty {
            MusicSectionHeader(title: "Store", subtitle: "Buy a track outright. Paid straight to the artist.")
            VStack(spacing: 0) {
                ForEach(store) { track in
                    StoreRow(
                        work: track,
                        isCurrent: player.isCurrent(track),
                        isPlaying: player.isPlaying
                    ) {
                        player.play(track, queue: tracks)
                    }
                    if track.id != store.last?.id {
                        CardDivider().padding(.leading, F33Card.paddingHorizontal + 56 + F33Spacing.md)
                    }
                }
            }
        }

        if !tracks.isEmpty {
            MusicSectionHeader(title: "Tracks", subtitle: tracks.count == 1 ? "1 track" : "\(tracks.count) tracks")
            VStack(spacing: 0) {
                ForEach(tracks) { track in
                    TrackRow(
                        work: track,
                        isCurrent: player.isCurrent(track),
                        isPlaying: player.isPlaying
                    ) {
                        player.play(track, queue: tracks)
                    }
                    if track.id != tracks.last?.id {
                        CardDivider().padding(.leading, F33Card.paddingHorizontal + 48 + F33Spacing.md)
                    }
                }
            }
        }

        if !posts.isEmpty {
            MusicSectionHeader(title: "From the scene", subtitle: "Works tagged music without a track attached.")
            ForEach(posts) { work in
                WorkCard(work: work, currentUser: model.state.user)
                CardDivider()
            }
        }
    }

    /// The next page, asked for when the reader reaches the bottom, and the
    /// retry when asking failed. Same contract as `WorkList`'s footer.
    @ViewBuilder
    private func footer(_ feed: WorkFeed) -> some View {
        if let error = feed.loadMoreError {
            Button {
                Task { await feed.loadMore() }
            } label: {
                Label(error.userMessage, systemImage: "arrow.clockwise")
                    .font(.footnote)
                    .foregroundStyle(F33Color.accent)
                    .frame(maxWidth: .infinity, minHeight: F33Layout.minTouchTarget)
            }
            .buttonStyle(.plain)
        } else if feed.hasMore {
            ProgressView()
                .frame(maxWidth: .infinity, minHeight: F33Layout.minTouchTarget)
                .padding(.vertical, F33Spacing.lg)
                .task { await feed.loadMore() }
        } else {
            Color.clear.frame(height: F33Spacing.xxl)
        }
    }

    /// An empty Music lane says what the surface selects.
    ///
    /// Static because the home pager's lane table needs the same value for the
    /// case it can no longer reach, and two copies of one surface's explanation
    /// is two copies that drift.
    static let emptyState = EmptyStateView(
        icon: "waveform",
        title: "No music yet",
        message: """
            Works tagged music — a track, a voice note, a video of a set — land \
            here newest first. Tracks uploaded to the studio aren't part of this feed yet.
            """
    )
}

/// What the Music surface is, in one line.
///
/// The same posture as the note on Explore, for the same reason: this is a
/// surface a definition chose, and a reader is owed the definition. The
/// difference is that Music's is a stated rule rather than a ranking, so it can
/// be said plainly.
struct MusicSurfaceNote: View {
    var body: some View {
        HStack(alignment: .top, spacing: F33Spacing.sm) {
            Image(systemName: "waveform")
                .font(.system(size: 11, weight: .semibold))
                .foregroundStyle(F33Color.accent)
                .padding(.top, 1)

            Text("Works tagged music, newest first. Studio tracks aren't in this feed yet.")
                .font(.system(size: 12))
                .foregroundStyle(F33Color.ink4)
                .fixedSize(horizontal: false, vertical: true)

            Spacer(minLength: 0)
        }
        .padding(.horizontal, F33Card.paddingHorizontal)
        .padding(.top, F33Spacing.sm)
        .padding(.bottom, F33Spacing.sm)
        .background(F33Color.bg)
        .overlay(alignment: .bottom) { CardDivider() }
        .accessibilityElement(children: .combine)
    }
}

/// The surface's shape while the first page loads: a row of tiles and a few
/// track rows, so content replaces placeholders of the same size rather than
/// arriving into a blank.
private struct MusicSkeleton: View {
    @State private var isPulsing = false

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            bar(width: 80, height: 22).padding(.horizontal, F33Card.paddingHorizontal).padding(.top, F33Spacing.xl).padding(.bottom, F33Spacing.md)
            HStack(spacing: F33Spacing.md) {
                ForEach(0..<3, id: \.self) { _ in
                    VStack(alignment: .leading, spacing: F33Spacing.sm) {
                        RoundedRectangle(cornerRadius: F33Radius.md)
                            .fill(F33Color.ink5.opacity(0.22))
                            .frame(width: NewTrackTile.size, height: NewTrackTile.size)
                        bar(width: 110, height: 12)
                        bar(width: 70, height: 10)
                    }
                }
            }
            .padding(.horizontal, F33Card.paddingHorizontal)
            bar(width: 100, height: 22).padding(.horizontal, F33Card.paddingHorizontal).padding(.top, F33Spacing.xl).padding(.bottom, F33Spacing.md)
            ForEach(0..<4, id: \.self) { _ in
                HStack(spacing: F33Spacing.md) {
                    RoundedRectangle(cornerRadius: F33Radius.xs)
                        .fill(F33Color.ink5.opacity(0.22))
                        .frame(width: 48, height: 48)
                    VStack(alignment: .leading, spacing: 6) {
                        bar(width: 160, height: 12)
                        bar(width: 90, height: 10)
                    }
                    Spacer()
                    bar(width: 32, height: 10)
                }
                .padding(.horizontal, F33Card.paddingHorizontal)
                .padding(.vertical, F33Spacing.sm)
            }
        }
        .opacity(isPulsing ? 0.55 : 1)
        .animation(.easeInOut(duration: 0.9).repeatForever(autoreverses: true), value: isPulsing)
        .onAppear { isPulsing = true }
        .accessibilityHidden(true)
    }

    private func bar(width: CGFloat, height: CGFloat) -> some View {
        RoundedRectangle(cornerRadius: 3)
            .fill(F33Color.ink5.opacity(0.3))
            .frame(width: width, height: height)
    }
}

#Preview("Music") {
    NavigationStack {
        MusicLaneView()
    }
    .environment(AppModel())
}
