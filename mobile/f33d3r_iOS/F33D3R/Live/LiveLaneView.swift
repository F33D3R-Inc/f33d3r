import SwiftUI
import F33D3RKit

/// The Live lane: rooms live now, and the way to open one of your own.
///
/// Rooms are not works, so this is not a `WorkList`. It reads
/// `/api/v1/live`, draws one card per room, and puts "Go live" at the top for
/// the signed-in reader — the lane tag chosen on the go-live sheet decides
/// where a room shows, and this is where a Music-lane room shows too.
struct LiveLaneView: View {
    /// How far the lane has scrolled, for the chrome above it. Nil when this
    /// lane is drawn somewhere with no chrome to move.
    var onScroll: ((CGFloat) -> Void)?

    @Environment(AppModel.self) private var model

    /// The list lives on the model, not in this view.
    ///
    /// `live_start` arrives on the user stream, which nothing in a view body
    /// can hear, and the lane strip's LIVE badge is drawn somewhere else again.
    /// One store both can reach is what lets the server's "somebody went on
    /// air" reach the screen without the lane being open to receive it.
    private var store: LiveLaneStore { model.liveLane() }

    private var rooms: [LiveStream] { store.rooms }
    private var own: LiveStream? { store.own }
    private var error: APIError? { store.error }
    private var isLoading: Bool { store.isLoading }

    @State private var isGoingLive = false
    @State private var broadcasting: LiveStream?
    /// Home's chrome floats over this lane; the content starts below it.
    @Environment(\.laneTopInset) private var laneTopInset

    var body: some View {
        ScrollView {
            LazyVStack(spacing: 0) {
                goLiveRow

                if isLoading, rooms.isEmpty {
                    ForEach(0..<2, id: \.self) { _ in
                        WorkCardSkeleton()
                        CardDivider()
                    }
                } else if let error, rooms.isEmpty {
                    ErrorStateView(error: error) { Task { await load() } }
                } else if rooms.isEmpty {
                    EmptyStateView(
                        icon: "dot.radiowaves.left.and.right",
                        title: "Nobody is live",
                        message: "Live rooms show up here the moment they start."
                    )
                } else {
                    ForEach(rooms) { room in
                        NavigationLink(value: Route.live(id: room.id)) {
                            LiveRoomCard(room: room, isOwn: room.author.handle == model.state.user?.user.handle)
                        }
                        .buttonStyle(.plain)
                        CardDivider()
                    }
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
        .refreshable { await load() }
        .task {
            await load()
            #if DEBUG
            if model.debugGoLive { isGoingLive = true }
            if model.debugBroadcast, let own { broadcasting = own }
            #endif
        }
        .fullScreenCover(isPresented: $isGoingLive) {
            GoLiveSheet { stream in
                broadcasting = stream
            }
        }
        .fullScreenCover(item: $broadcasting, onDismiss: { Task { await load() } }) { stream in
            BroadcasterView(stream: stream)
        }
        .onChange(of: broadcasting) { _, stream in
            if stream != nil { isGoingLive = false }
        }
    }

    /// "Go live" when the reader has no room; "Back to your room" when they do.
    private var goLiveRow: some View {
        Button {
            if let own { broadcasting = own } else { isGoingLive = true }
        } label: {
            HStack(spacing: F33Spacing.md) {
                if let user = model.state.user?.user {
                    F33Avatar(user: user, size: 40)
                }
                VStack(alignment: .leading, spacing: 2) {
                    Text(own == nil ? "Go live" : "You're live")
                        .font(.system(size: 15, weight: .semibold))
                        .foregroundStyle(F33Color.ink)
                    Text(own == nil ? "Camera, a title, and who gets in. Tips go straight to you." : "Back to your room")
                        .font(.system(size: 12))
                        .foregroundStyle(F33Color.ink4)
                        .lineLimit(1)
                }
                Spacer(minLength: F33Spacing.sm)
                Image(systemName: own == nil ? "dot.radiowaves.left.and.right" : "chevron.right")
                    .font(.system(size: 15, weight: .semibold))
                    .foregroundStyle(own == nil ? F33Color.accentInk : F33Color.ink3)
                    .frame(width: 36, height: 36)
                    .background(own == nil ? F33Color.danger : F33Color.bgSunken, in: Circle())
            }
            .padding(.horizontal, F33Card.paddingHorizontal)
            .padding(.vertical, F33Spacing.md)
            .frame(minHeight: F33Layout.minTouchTarget)
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .background(F33Color.bg)
        .overlay(alignment: .bottom) { CardDivider() }
    }

    private func load() async {
        await store.load()
    }
}

/// The Live lane's rooms, as the server last listed them.
///
/// Held by ``AppModel`` so that three callers share one list: the lane that
/// draws it, the strip that badges it, and the user stream's `live_start`
/// frame, which is the whole reason this is not `@State` on a view. Nothing
/// here decides anything — it asks `/api/v1/live` and keeps the answer.
@MainActor
@Observable
final class LiveLaneStore {
    private(set) var rooms: [LiveStream] = []
    /// The reader's own room, when they have one on air.
    private(set) var own: LiveStream?
    private(set) var error: APIError?
    private(set) var isLoading = true
    /// Whether the list has ever come back. A badge drawn from `rooms.count`
    /// before the first answer would say "nobody is live" about a server it has
    /// not asked yet.
    private(set) var hasLoaded = false

    private let client: APIClient

    init(client: APIClient) {
        self.client = client
    }

    func load() async {
        error = nil
        do {
            let list = try await client.liveRooms()
            rooms = list.streams
            own = list.own
            hasLoaded = true
        } catch let apiError as APIError {
            if rooms.isEmpty { error = apiError }
        } catch {
            if rooms.isEmpty { self.error = .transport(error.localizedDescription) }
        }
        isLoading = false
    }
}

/// One room in the lane: who, what, how many watching, and the LIVE mark.
struct LiveRoomCard: View {
    let room: LiveStream
    let isOwn: Bool

    var body: some View {
        VStack(alignment: .leading, spacing: F33Spacing.sm) {
            HStack(alignment: .top, spacing: F33Card.columnGap) {
                F33Avatar(author: room.author, size: F33Card.avatarColumnWidth)
                    .overlay(Circle().strokeBorder(F33Color.danger, lineWidth: 2.5))

                VStack(alignment: .leading, spacing: 2) {
                    HStack(spacing: F33Card.authorRowGap) {
                        Text("@\(room.author.handle)")
                            .font(.system(size: F33Card.authorNameSize, weight: .semibold))
                            .foregroundStyle(F33Color.ink)
                        if let badge = room.author.badge { BadgePill(badge: badge) }
                        Spacer(minLength: F33Spacing.xs)
                        LiveBadgeMark()
                    }
                    Text(subtitle)
                        .font(.system(size: 11))
                        .foregroundStyle(F33Color.ink4)
                }
            }

            // The room's stage. No video flows in this build, and the card
            // says so instead of painting a poster that pretends otherwise.
            ZStack {
                LinearGradient(colors: [Color(hex: 0x1A1A1A), Color(hex: 0x3A1F7A).opacity(0.7)], startPoint: .topLeading, endPoint: .bottomTrailing)
                VStack(spacing: 6) {
                    Text(room.title)
                        .font(.system(size: 17, weight: .semibold))
                        .foregroundStyle(.white)
                        .multilineTextAlignment(.center)
                        .padding(.horizontal, F33Spacing.lg)
                    if let pinned = room.pinnedBody, !pinned.isEmpty {
                        Label(pinned, systemImage: "pin.fill")
                            .font(.system(size: 12))
                            .foregroundStyle(.white.opacity(0.8))
                            .lineLimit(1)
                    }
                    if !room.hasVideo {
                        Text("Tap to join · chat and tips are live")
                            .font(.system(size: 11))
                            .foregroundStyle(.white.opacity(0.6))
                    }
                }
            }
            .frame(height: 180)
            .clipShape(RoundedRectangle(cornerRadius: F33Card.mediaCornerRadius))
            .padding(.leading, F33Card.avatarColumnWidth + F33Card.columnGap)

            HStack(spacing: 18) {
                Label("chat", systemImage: "bubble.left")
                Label(room.tipGoalUAET > 0 ? "\(AET.amount(uAET: room.tipTotalUAET)) / \(AET.label(uAET: room.tipGoalUAET))" : "tip", systemImage: "bolt.fill")
                    .foregroundStyle(F33Action.tip)
                Spacer(minLength: 0)
            }
            .font(.system(size: 12, weight: .medium))
            .foregroundStyle(F33Color.ink3)
            .padding(.leading, F33Card.avatarColumnWidth + F33Card.columnGap)
        }
        .padding(.top, F33Card.paddingTop)
        .padding(.horizontal, F33Card.paddingHorizontal)
        .padding(.bottom, F33Card.paddingBottom)
        .frame(maxWidth: .infinity, alignment: .leading)
        .contentShape(Rectangle())
        .accessibilityElement(children: .combine)
        .accessibilityLabel("\(room.author.handle) is live: \(room.title). \(subtitle)")
    }

    private var subtitle: String {
        var parts: [String] = []
        if let lane = room.laneLabel { parts.append(lane) }
        parts.append(room.viewerCount == 1 ? "1 watching" : "\(Counts.exact(room.viewerCount)) watching")
        if isOwn { parts.append("your room") }
        return parts.joined(separator: " · ")
    }
}

/// The red LIVE mark.
struct LiveBadgeMark: View {
    var text = "LIVE"

    var body: some View {
        Text(text)
            .font(.system(size: 10, weight: .bold))
            .tracking(0.5)
            .foregroundStyle(.white)
            .padding(.horizontal, 7)
            .padding(.vertical, 3)
            .background(F33Color.danger, in: Capsule())
            .fixedSize()
    }
}
