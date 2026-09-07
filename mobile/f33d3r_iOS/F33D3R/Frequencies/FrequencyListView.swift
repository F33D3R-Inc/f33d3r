import SwiftUI
import F33D3RKit

/// Every frequency: what is on air, what is coming, and the reader's own.
///
/// Three lanes because the server keeps three. The client does not filter one
/// list into three — "live" is the brain's judgement about presence and
/// heartbeats, not a predicate over a state string — so the segmented control
/// picks which of the store's three arrays is drawn and nothing else.
struct FrequencyListView: View {
    enum Lane: String, CaseIterable, Hashable {
        case live, scheduled, mine

        var label: String {
            switch self {
            case .live: return "Live"
            case .scheduled: return "Scheduled"
            case .mine: return "Mine"
            }
        }
    }

    @Environment(FrequencySession.self) private var session
    @Environment(AppModel.self) private var model

    @State private var lane: Lane = .live
    @State private var isStarting = false
    @State private var openedID: String?

    var body: some View {
        Group {
            if let lanes = session.lanes {
                list(lanes)
            } else {
                ProgressView()
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
            }
        }
        .background(F33Color.bg)
        .navigationTitle("Frequencies")
        .navigationBarTitleDisplayMode(.inline)
        .navigationDestination(item: $openedID) { id in
            FrequencyRoomView(frequencyID: id)
        }
        .sheet(isPresented: $isStarting) {
            StartFrequencySheet(
                onStarted: { id in openedID = id },
                onScheduled: { lane = .scheduled }
            )
        }
        .toolbar {
            ToolbarItem(placement: .primaryAction) {
                Button {
                    isStarting = true
                } label: {
                    Image(systemName: "plus")
                        .font(.system(size: 15, weight: .semibold))
                }
                .tint(F33Color.accent)
                .accessibilityLabel("Start a Frequency")
            }
        }
        .task {
            guard let lanes = session.lanes, !lanes.hasLoaded else { return }
            await lanes.refresh()
        }
        .onAppear {
            #if DEBUG
            if FrequencyDebug.opensStartSheet { isStarting = true }
            #endif
        }
    }

    private func list(_ lanes: FrequencyListStore) -> some View {
        ScrollView {
            LazyVStack(spacing: 0) {
                Picker("Lane", selection: $lane) {
                    ForEach(Lane.allCases, id: \.self) { lane in
                        Text(lane.label).tag(lane)
                    }
                }
                .pickerStyle(.segmented)
                .padding(.horizontal, F33Card.paddingHorizontal)
                .padding(.vertical, F33Spacing.sm)

                startRow(lanes)
                CardDivider()

                let rows = frequencies(lanes)

                if lanes.isLoading, rows.isEmpty {
                    ForEach(0..<2, id: \.self) { _ in
                        WorkCardSkeleton()
                        CardDivider()
                    }
                } else if rows.isEmpty {
                    empty(lanes)
                } else {
                    ForEach(rows) { frequency in
                        NavigationLink(value: Route.frequency(id: frequency.id)) {
                            FrequencyRow(frequency: frequency)
                        }
                        .buttonStyle(.plain)
                        CardDivider()
                    }
                }
            }
        }
        .refreshable { await lanes.refresh() }
    }

    private func frequencies(_ lanes: FrequencyListStore) -> [Frequency] {
        switch lane {
        case .live: return lanes.live
        case .scheduled: return lanes.scheduled
        case .mine: return lanes.mine
        }
    }

    /// "Start a Frequency", or the way back into the one already open — the
    /// same shape the Live lane's go-live row has, so the two read as siblings.
    private func startRow(_ lanes: FrequencyListStore) -> some View {
        Button {
            if let own = lanes.own {
                openedID = own.id
            } else {
                isStarting = true
            }
        } label: {
            HStack(spacing: F33Spacing.md) {
                if let user = model.state.user?.user {
                    F33Avatar(user: user, size: 40)
                }
                VStack(alignment: .leading, spacing: 2) {
                    Text(lanes.own == nil ? "Start a Frequency" : "You're on air")
                        .font(.system(size: 15, weight: .semibold))
                        .foregroundStyle(F33Color.ink)
                    Text(lanes.own == nil
                         ? "A title, who's allowed in, and a stage. No camera needed."
                         : "Back to your frequency")
                        .font(.system(size: 12))
                        .foregroundStyle(F33Color.ink4)
                        .lineLimit(1)
                }
                Spacer(minLength: F33Spacing.sm)
                Image(systemName: lanes.own == nil ? "waveform" : "chevron.right")
                    .font(.system(size: 15, weight: .semibold))
                    .foregroundStyle(lanes.own == nil ? F33Color.accentInk : F33Color.ink3)
                    .frame(width: 36, height: 36)
                    .background(lanes.own == nil ? F33Color.accent : F33Color.bgSunken, in: Circle())
            }
            .padding(.horizontal, F33Card.paddingHorizontal)
            .padding(.vertical, F33Spacing.md)
            .frame(minHeight: F33Layout.minTouchTarget)
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
    }

    @ViewBuilder
    private func empty(_ lanes: FrequencyListStore) -> some View {
        if let message = lanes.lastError, !lanes.hasLoaded {
            EmptyStateView(
                icon: "wifi.slash",
                title: "Frequencies aren't answering",
                message: FrequencyErrorCopy.sentence(message) ?? message,
                actionTitle: "Try again",
                action: { Task { await lanes.refresh() } }
            )
        } else {
            switch lane {
            case .live:
                EmptyStateView(
                    icon: "waveform",
                    title: "Nobody is on air",
                    message: "Frequencies show up here the moment somebody starts one. You could be the somebody.",
                    actionTitle: "Start a Frequency",
                    action: { isStarting = true }
                )
            case .scheduled:
                EmptyStateView(
                    icon: "calendar",
                    title: "Nothing scheduled",
                    message: "Rooms with a start time on them appear here until they go on air."
                )
            case .mine:
                EmptyStateView(
                    icon: "mic",
                    title: "You haven't opened one",
                    message: "Every frequency you host — on air, scheduled or finished — lives here.",
                    actionTitle: "Start a Frequency",
                    action: { isStarting = true }
                )
            }
        }
    }
}

/// One frequency as a row: the faces, what it's called, who's hosting, and how
/// many are in.
struct FrequencyRow: View {
    let frequency: Frequency

    var body: some View {
        HStack(alignment: .top, spacing: F33Card.columnGap) {
            F33Avatar(author: frequency.host, size: F33Card.avatarColumnWidth)
                .overlay(
                    Circle().strokeBorder(
                        frequency.isLive ? F33Color.accent : Color.clear,
                        lineWidth: 2.5
                    )
                )

            VStack(alignment: .leading, spacing: 4) {
                HStack(spacing: F33Spacing.xs) {
                    FrequencyStateChip(frequency: frequency)
                    if frequency.isNSFW { ContentLabel(kind: .nsfw) }
                    Spacer(minLength: 0)
                    if frequency.isLive {
                        FrequencyAudioGlyph(tint: F33Color.accent, size: 14)
                    }
                }

                Text(frequency.title.isEmpty ? "Untitled frequency" : frequency.title)
                    .font(.system(size: 15, weight: .semibold))
                    .foregroundStyle(F33Color.ink)
                    .lineLimit(2)
                    .fixedSize(horizontal: false, vertical: true)

                HStack(spacing: 5) {
                    Text("@\(frequency.host.handle)")
                        .font(.system(size: 12, weight: .medium))
                        .foregroundStyle(F33Color.ink3)
                    if let badge = frequency.host.badge { BadgePill(badge: badge) }
                }

                HStack(spacing: F33Spacing.sm) {
                    if frequency.participantCount > 1 {
                        FrequencyAvatarStack(
                            authors: [frequency.host],
                            overflow: frequency.participantCount - 1,
                            size: 20
                        )
                    }
                    Text(detail)
                        .font(.system(size: 11).monospacedDigit())
                        .foregroundStyle(F33Color.ink4)
                    Spacer(minLength: 0)
                }
            }
        }
        .padding(.top, F33Card.paddingTop)
        .padding(.horizontal, F33Card.paddingHorizontal)
        .padding(.bottom, F33Card.paddingBottom)
        .frame(maxWidth: .infinity, alignment: .leading)
        .contentShape(Rectangle())
        .accessibilityElement(children: .combine)
        .accessibilityLabel("\(frequency.title), hosted by @\(frequency.host.handle). \(detail)")
    }

    private var detail: String {
        if frequency.isLive { return frequency.listenerLabel }
        if let at = frequency.scheduledAt, frequency.isScheduled {
            return "Starts \(RelativeTime.fullLabel(for: at))"
        }
        if let at = frequency.endedAt { return "Ended \(RelativeTime.compactLabel(for: at))" }
        return frequency.state.replacingOccurrences(of: "_", with: " ").capitalized
    }
}
