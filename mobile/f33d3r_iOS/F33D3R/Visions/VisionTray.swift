import SwiftUI
import F33D3RKit

/// The visions row under the Home header.
///
/// Only people the reader follows, nobody suggested. One pill per author rather
/// than one pill for the lane: a vision belongs to somebody, and whose it is is
/// the only thing the reader is deciding on here. Each pill carries the face,
/// the name, and how much is waiting. Violet is a vision, red is a creator who
/// is live.
///
/// Pills rather than the column of large circles a stories tray usually gets.
/// The tray used to stand 88pt tall between the wordmark and the first work, and
/// that was the one complaint this design exists to answer; the row is 48 now,
/// and collapses further to a 24pt strip of faces once the reader is moving.
struct VisionTrayView: View {
    let store: VisionTrayStore
    let currentUser: User?
    let isCollapsed: Bool
    let onOpen: (VisionRing) -> Void
    let onCompose: () -> Void
    let onMute: (VisionRing) -> Void

    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    /// The whole row: a 44pt target with a point of air either side of it. The
    /// pill inside is shorter than its button — see ``VisionPill`` — so a thumb
    /// still gets 44 without the bar carrying a 44pt slab of colour.
    static let rowHeight: CGFloat = F33Layout.minTouchTarget + 4

    var body: some View {
        Group {
            if isCollapsed {
                collapsed
            } else {
                expanded
            }
        }
        .frame(maxWidth: .infinity)
        .f33GlassBar()
        .animation(reduceMotion ? nil : F33Motion.easeOut, value: isCollapsed)
        .task { await store.loadIfNeeded() }
    }

    /// A carousel, deliberately overflowing: the next pill sitting half in view
    /// at the right edge is the only thing telling the reader there is more of
    /// the row to come.
    private var expanded: some View {
        ScrollView(.horizontal, showsIndicators: false) {
            HStack(spacing: F33Spacing.sm) {
                ownEntry
                ForEach(store.rings) { ring in
                    Button { onOpen(ring) } label: {
                        VisionPill(ring: ring)
                    }
                    .buttonStyle(.plain)
                    .contextMenu {
                        Button(role: .destructive) { onMute(ring) } label: {
                            Label("Mute visions from @\(ring.author.handle)", systemImage: "eye.slash")
                        }
                    }
                    .accessibilityLabel(accessibilityLabel(ring))
                }
            }
            .padding(.horizontal, F33Spacing.lg)
            // Padded rather than left to centre itself: a horizontal scroll
            // view hangs its content from the top edge, and 44pt of pills in a
            // 48pt bar would sit 4pt high in it.
            .padding(.vertical, 2)
        }
        .scrollBounceBehavior(.basedOnSize, axes: .horizontal)
        .frame(height: Self.rowHeight)
        .transition(.opacity)
    }

    /// The 24pt strip: the unseen faces stacked, and a count.
    private var collapsed: some View {
        Button {
            if let first = store.rings.first { onOpen(first) } else { onCompose() }
        } label: {
            HStack(spacing: F33Spacing.sm) {
                HStack(spacing: -7) {
                    ForEach(store.rings.filter(\.hasUnseen).prefix(4)) { ring in
                        F33Avatar(author: ring.author, size: 20)
                            .overlay(Circle().strokeBorder(ring.isLive ? F33Color.danger : F33Color.vision, lineWidth: 1.5))
                    }
                }
                Text(collapsedLabel)
                    .font(.system(size: 12, weight: .medium))
                    .foregroundStyle(F33Color.ink3)
                    .lineLimit(1)
                Spacer(minLength: 0)
            }
            .padding(.horizontal, F33Spacing.lg)
            .frame(height: 24)
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .transition(.opacity)
        .accessibilityLabel(collapsedLabel)
    }

    private var collapsedLabel: String {
        let unseen = store.rings.filter(\.hasUnseen).count
        let live = store.rings.filter(\.isLive).count
        switch (unseen, live) {
        case (0, 0): return store.rings.isEmpty ? "Post a vision" : "Visions · all seen"
        case (_, 0): return "\(unseen) new vision\(unseen == 1 ? "" : "s")"
        default: return "\(unseen) new · \(live) live"
        }
    }

    /// "Your vision": the reader's own pill when they have one, else the pill
    /// that opens the composer. Either way it leads the row, because the reader
    /// posting is the one thing here that is not about somebody else.
    @ViewBuilder
    private var ownEntry: some View {
        if let own = store.own {
            Button { onOpen(own) } label: {
                VisionPillShell(tint: F33Color.accent, isProminent: true) {
                    ZStack(alignment: .bottomTrailing) {
                        VisionRingCell.ring(for: own, size: VisionPill.ringSize) {
                            if let user = currentUser {
                                F33Avatar(user: user, size: VisionPill.avatarSize)
                            }
                        }
                        addBadge
                    }
                    VisionPillLabel(text: "Your vision")
                }
            }
            .buttonStyle(.plain)
            .accessibilityLabel("Your vision, \(own.count) live")
        } else {
            Button(action: onCompose) {
                VisionPillShell(tint: F33Color.accent, isProminent: true) {
                    Circle()
                        .strokeBorder(style: StrokeStyle(lineWidth: 1.5, dash: [3, 2.5]))
                        .foregroundStyle(F33Color.ink4)
                        .frame(width: VisionPill.ringSize, height: VisionPill.ringSize)
                        .overlay {
                            Image(systemName: "plus")
                                .font(.system(size: 12, weight: .semibold))
                                .foregroundStyle(F33Color.ink2)
                        }
                    VisionPillLabel(text: "Add vision")
                }
            }
            .buttonStyle(.plain)
            .accessibilityLabel("Post a vision")
        }
    }

    /// The plus on the reader's own face. Small on purpose — the pill around it
    /// is the target and this is the second action on it — with a hit area
    /// padded past the glyph but kept to the corner of the face, so tapping the
    /// name still opens the visions rather than the composer.
    private var addBadge: some View {
        Button(action: onCompose) {
            Image(systemName: "plus")
                .font(.system(size: 8, weight: .bold))
                .foregroundStyle(F33Color.accentInk)
                .frame(width: 14, height: 14)
                .background(F33Color.accent, in: Circle())
                .overlay(Circle().strokeBorder(F33Color.bg, lineWidth: 1.5))
                .frame(width: 22, height: 22, alignment: .bottomTrailing)
                .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .accessibilityLabel("Add to your vision")
    }

    private func accessibilityLabel(_ ring: VisionRing) -> String {
        var parts = ["@\(ring.author.handle)"]
        parts.append(ring.hasUnseen ? "\(ring.unseenCount) new" : "seen")
        if ring.isLive { parts.append("live now") }
        return parts.joined(separator: ", ")
    }
}

/// One author's visions as a pill: their ringed face, their name, and a word on
/// what is waiting behind it.
struct VisionPill: View {
    let ring: VisionRing

    /// The ringed circle, and the face inside it. The gap between the two is
    /// what makes the ring read as a ring rather than as a border on the
    /// photograph, and it is also the room a realm ring needs.
    static let ringSize: CGFloat = 28
    static let avatarSize: CGFloat = 22

    var body: some View {
        VisionPillShell(tint: tint, isProminent: ring.hasUnseen) {
            VisionRingCell(ring: ring)
            VisionPillLabel(text: ring.author.displayName.isEmpty ? "@\(ring.author.handle)" : ring.author.displayName)
            hint
        }
    }

    /// Red for a creator who is live, violet for everyone else. The colour is
    /// the whole point of the row: it is read before either the face or the
    /// name is.
    private var tint: Color {
        ring.isLive ? F33Color.danger : F33Color.vision
    }

    /// What is behind the pill, in as few characters as will carry it. A ring
    /// the reader has already been through says so with its dashed grey circle
    /// and needs no words for it.
    @ViewBuilder
    private var hint: some View {
        if ring.isLive {
            HStack(spacing: 4) {
                Circle()
                    .fill(F33Color.danger)
                    .frame(width: 5, height: 5)
                Text("LIVE")
                    .font(.system(size: 10, weight: .bold))
                    .foregroundStyle(F33Color.danger)
                    .fixedSize()
            }
        } else if ring.hasUnseen {
            Text("\(ring.unseenCount) new")
                .font(.system(size: 11, weight: .medium).monospacedDigit())
                .foregroundStyle(F33Color.vision)
                .fixedSize()
        }
    }
}

/// The capsule every item in the row is drawn in.
///
/// One shell, so the reader's own pill and an author's cannot drift apart. The
/// visible capsule is 36pt and the button around it 44: the target is Apple's
/// minimum, but a bar of 44pt capsules is a bar of stripes, and the row is
/// meant to sit quietly under the wordmark.
struct VisionPillShell<Content: View>: View {
    let tint: Color
    /// Whether there is something new behind the pill. A pill that has been
    /// read keeps its colour but loses most of its weight.
    let isProminent: Bool
    @ViewBuilder let content: Content

    private static var height: CGFloat { 36 }

    var body: some View {
        HStack(spacing: 7) {
            content
        }
        .padding(.leading, 4)
        .padding(.trailing, 10)
        .frame(height: Self.height)
        .background(tint.opacity(isProminent ? 0.16 : 0.08), in: Capsule())
        .overlay(Capsule().strokeBorder(tint.opacity(isProminent ? 0.40 : 0.18), lineWidth: 1))
        .frame(height: F33Layout.minTouchTarget)
        .contentShape(Rectangle())
    }
}

/// The name on a pill.
///
/// Capped rather than left to its ideal width: one person with a long display
/// name would otherwise take the whole row and push everybody else off the edge
/// of the screen, where the reader has no reason to think they are.
struct VisionPillLabel: View {
    let text: String

    var body: some View {
        Text(text)
            .font(.system(size: 13, weight: .semibold))
            .foregroundStyle(F33Color.ink)
            .lineLimit(1)
            .truncationMode(.tail)
            .frame(maxWidth: 108, alignment: .leading)
    }
}

/// The face in the tray: the avatar inside the ring that says seen, unseen or
/// live.
struct VisionRingCell: View {
    let ring: VisionRing
    var size: CGFloat = VisionPill.ringSize

    var body: some View {
        Self.ring(for: ring, size: size) {
            F33Avatar(author: ring.author, size: size - (VisionPill.ringSize - VisionPill.avatarSize))
        }
    }

    /// The ring stroke around any content. Live wins, then unseen, then seen.
    @ViewBuilder
    static func ring<Content: View>(
        for ring: VisionRing,
        size: CGFloat,
        lineWidth: CGFloat = 2.5,
        @ViewBuilder content: () -> Content
    ) -> some View {
        content()
            .frame(width: size, height: size)
            .overlay {
                if ring.isLive {
                    Circle().strokeBorder(F33Color.danger, lineWidth: lineWidth)
                } else if ring.hasUnseen {
                    Circle().strokeBorder(F33Color.vision, lineWidth: lineWidth)
                } else {
                    Circle()
                        .strokeBorder(style: StrokeStyle(lineWidth: lineWidth - 1, dash: [3, 2.5]))
                        .foregroundStyle(F33Color.ink5)
                }
            }
    }
}
