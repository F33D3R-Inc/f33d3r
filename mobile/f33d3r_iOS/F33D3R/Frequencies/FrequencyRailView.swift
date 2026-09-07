import SwiftUI
import F33D3RKit

/// The Frequencies row on Home: what is on air, right now, as a line of pills.
///
/// It sits directly under the lane strip, in the same slot family as the
/// visions tray, and it follows the same rule that row does — it is a bar of
/// capsules the reader scans by colour, not a stack of cards. A pill carries
/// the faces in the room, how many more there are, the title, and a moving
/// three-bar mark that means somebody is talking.
///
/// It is **hidden entirely when nothing is on air**. An empty rail on Home is a
/// permanent strip of grey saying "no", and the whole point of the row is that
/// its presence is the news. The reader's own scheduled room is the one thing
/// that keeps it open with nothing live: it shows dimmed, with a clock, because
/// forgetting you scheduled a room is the failure this costs nothing to prevent.
struct FrequencyRailView: View {
    @Environment(FrequencySession.self) private var session

    /// The row's own height, matching the visions tray's, so the two stacked
    /// read as one piece of chrome.
    static let rowHeight: CGFloat = F33Layout.minTouchTarget + 4

    var body: some View {
        Group {
            if let lanes = session.lanes, !items(lanes).isEmpty {
                row(items(lanes))
            }
        }
        .task {
            guard let lanes = session.lanes, !lanes.hasLoaded else { return }
            await lanes.refresh()
        }
    }

    /// On air first, then the reader's own scheduled rooms. Nobody else's
    /// schedule: a row of things that are not happening is not news.
    private func items(_ lanes: FrequencyListStore) -> [Frequency] {
        let live = lanes.live
        let liveIDs = Set(live.map(\.id))
        let mineScheduled = lanes.mine.filter { $0.isScheduled && !liveIDs.contains($0.id) }
        return live + mineScheduled
    }

    private func row(_ frequencies: [Frequency]) -> some View {
        ScrollView(.horizontal, showsIndicators: false) {
            HStack(spacing: F33Spacing.sm) {
                ForEach(frequencies) { frequency in
                    NavigationLink(value: Route.frequency(id: frequency.id)) {
                        FrequencyPill(frequency: frequency)
                    }
                    .buttonStyle(.plain)
                    .accessibilityLabel(accessibilityLabel(frequency))
                }
                seeAll
            }
            .padding(.horizontal, F33Spacing.lg)
            .padding(.vertical, 2)
        }
        .scrollBounceBehavior(.basedOnSize, axes: .horizontal)
        .frame(height: Self.rowHeight)
        .frame(maxWidth: .infinity)
        .f33GlassBar()
        .transition(.opacity)
    }

    /// The way out of the row and into the whole list. A chevron rather than a
    /// pill, so it is plainly not one of the rooms.
    private var seeAll: some View {
        NavigationLink(value: Route.frequencies) {
            HStack(spacing: 3) {
                Text("All")
                    .font(.system(size: 13, weight: .semibold))
                    .foregroundStyle(F33Color.ink3)
                Image(systemName: "chevron.right")
                    .font(.system(size: 11, weight: .bold))
                    .foregroundStyle(F33Color.ink4)
            }
            .padding(.horizontal, 12)
            .frame(height: 36)
            .overlay(Capsule().strokeBorder(F33Color.hairlineStrong, lineWidth: 1))
            .frame(height: F33Layout.minTouchTarget)
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .accessibilityLabel("All frequencies")
    }

    private func accessibilityLabel(_ frequency: Frequency) -> String {
        var parts = [frequency.title.isEmpty ? "Untitled frequency" : frequency.title]
        parts.append("hosted by @\(frequency.host.handle)")
        if frequency.isLive {
            parts.append(frequency.listenerLabel)
        } else if let at = frequency.scheduledAt {
            parts.append("scheduled for \(RelativeTime.fullLabel(for: at))")
        }
        return parts.joined(separator: ", ")
    }
}

/// One room as a capsule: who is in it, what it is called, and that it is on.
struct FrequencyPill: View {
    let frequency: Frequency

    /// How many faces the pill has room for before it starts counting instead.
    private static let faces = 3

    var body: some View {
        HStack(spacing: 7) {
            if frequency.isLive {
                FrequencyAvatarStack(
                    authors: authors,
                    overflow: overflow,
                    size: 22,
                    maxFaces: Self.faces
                )
            } else {
                Image(systemName: "clock")
                    .font(.system(size: 13, weight: .semibold))
                    .foregroundStyle(F33Color.ink3)
                    .frame(width: 22, height: 22)
            }

            Text(frequency.title.isEmpty ? "Untitled" : frequency.title)
                .font(.system(size: 13, weight: .semibold))
                .foregroundStyle(F33Color.ink)
                .lineLimit(1)
                .truncationMode(.tail)
                .frame(maxWidth: 132, alignment: .leading)

            if frequency.isLive {
                FrequencyAudioGlyph(tint: F33Color.accent, size: 13)
            } else if let at = frequency.scheduledAt {
                Text(RelativeTime.compactLabel(for: at))
                    .font(.system(size: 11, weight: .medium).monospacedDigit())
                    .foregroundStyle(F33Color.ink3)
                    .fixedSize()
            }
        }
        .padding(.leading, 4)
        .padding(.trailing, 10)
        .frame(height: 36)
        .background(F33Color.accent.opacity(frequency.isLive ? 0.16 : 0.06), in: Capsule())
        .overlay(Capsule().strokeBorder(F33Color.accent.opacity(frequency.isLive ? 0.40 : 0.16), lineWidth: 1))
        .frame(height: F33Layout.minTouchTarget)
        .contentShape(Rectangle())
        .opacity(frequency.isLive ? 1 : 0.72)
    }

    /// Host first. The lane read carries only the host — the stage arrives with
    /// the room — so the count carries everyone the pill cannot picture.
    private var authors: [WorkAuthor] { [frequency.host] }

    private var overflow: Int {
        max(0, frequency.participantCount - authors.count)
    }
}
