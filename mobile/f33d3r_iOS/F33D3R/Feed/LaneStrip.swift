import SwiftUI
import F33D3RKit

/// The lane selector: the reader's own row of feeds.
///
/// What is in it is the reader's, not ours. Following and For you are fixed at
/// the front; everything after them — Trending, Music, Visions, Live, and any
/// topic they added, like #sports — can be taken out, and the trailing button
/// is where they are chosen.
///
/// Horizontally scrollable because a row the reader can add to has no fixed
/// width, and because six lanes already do not fit 402pt at a legible size.
///
/// The strip follows the pager: swiping the feed moves the underline, and the
/// active lane is scrolled back into view so the reader is never looking at a
/// strip whose selection is off screen.
struct LaneStrip: View {
    @Binding var lane: FeedLane
    /// The strip, in the reader's order.
    let lanes: [FeedLane]
    /// Opens the sheet that manages which feeds are up.
    var onManage: () -> Void = {}
    /// Absent when the server has not said. A badge reading zero would claim
    /// nobody is live, which is a different statement from not knowing.
    let liveCount: Int?
    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    /// The strip scrolls, so a larger label costs nothing but a shorter run of
    /// visible lanes — which is the right trade for someone who needs it.
    @ScaledMetric(relativeTo: .subheadline) private var scale: CGFloat = 1

    /// How many lanes are laid out plainly before the strip starts scrolling.
    ///
    /// Three, because three of the longest labels this app ships — Following,
    /// For you, Trending — plus the add button come to roughly 300pt at the
    /// largest accessibility size, and the narrowest supported phone is 320.
    static let lanesThatAlwaysFit = 3

    var body: some View {
        // Two layouts, chosen by counting rather than by measuring.
        //
        // A short strip lays out plainly, with the button immediately after the
        // last lane. A full one scrolls, with the button pinned to the trailing
        // edge where it stays reachable.
        //
        // This used to ask `ViewThatFits` which layout fitted, and that was the
        // bug the strip has been wearing: a lane label can be squeezed below its
        // ideal width, so the plain row reported that it fitted at any size, and
        // seven lanes were crushed into the space of two — five of them
        // flattened to their padding, and the add button stranded in the middle
        // of what looked like an empty bar. Three lanes at the largest
        // accessibility size fit the narrowest phone this app runs on, so the
        // count is a fact rather than a measurement, and it cannot lie.
        Group {
            if lanes.count <= Self.lanesThatAlwaysFit {
                HStack(spacing: 0) {
                    ForEach(lanes) { option in
                        laneButton(option)
                    }
                    manageButton
                    Spacer(minLength: 0)
                }
                .padding(.leading, F33Spacing.sm)
            } else {
                // The button is stacked over the row, not placed beside it. As
                // a sibling it starved the scroll view of width; over it, the
                // scroll view is proposed the whole bar and the button still
                // stays where a thumb can reach it.
                ZStack(alignment: .trailing) {
                    scrollingLanes
                    manageButton
                }
            }
        }
        .f33GlassBar(bottomHairline: true)
    }

    private var scrollingLanes: some View {
        // The scroll view owns the whole row and the add button sits on top of
        // it, rather than the two sharing a row as siblings.
        //
        // That is the fix for the bug this strip wore: as siblings, the scroll
        // view was handed a width of about two lanes and squeezed the other five
        // into slivers a few points wide — seven feeds that read as two and a
        // smudge, in a strip with nothing to scroll. Overlaid, the scroll view
        // is proposed the full bar, lays its lanes out at the widths their
        // labels ask for, and scrolls the rest.
        //
        // The lanes keep a lane's worth of trailing padding so the last one can
        // be scrolled out from under the button, and fade as they pass beneath
        // it so the row reads as continuing rather than being cut off.
        ScrollViewReader { proxy in
            ScrollView(.horizontal, showsIndicators: false) {
                HStack(spacing: 0) {
                    ForEach(lanes) { option in
                        laneButton(option)
                    }
                }
                .padding(.leading, F33Spacing.sm)
                .padding(.trailing, Self.manageButtonWidth + Self.fadeWidth)
            }
            .scrollBounceBehavior(.basedOnSize, axes: .horizontal)
            .onChange(of: lane) { _, new in
                withAnimation(reduceMotion ? nil : F33Motion.easeOut) {
                    proxy.scrollTo(new, anchor: .center)
                }
            }
            // A fixed width, not a fraction of the strip: a proportional fade is
            // a couple of points on a pad and most of a lane label on a phone.
            .mask(
                HStack(spacing: 0) {
                    Rectangle()
                    LinearGradient(colors: [.black, .clear], startPoint: .leading, endPoint: .trailing)
                        .frame(width: Self.fadeWidth)
                    // Fully clear under the button, so a label never shares its
                    // space with the plus.
                    Color.clear.frame(width: Self.manageButtonWidth)
                }
            )
        }
    }

    /// How much of the trailing edge the add button occupies, and therefore how
    /// much room the lanes keep to scroll clear of it.
    private static let manageButtonWidth: CGFloat = 46

    /// How far the lanes fade before they reach the button.
    private static let fadeWidth: CGFloat = 28

    /// The way to a feed that is not up yet. It sits at the end of the strip
    /// rather than behind a settings screen, because the moment a reader wants
    /// another feed is the moment they are looking at this row.
    private var manageButton: some View {
        Button(action: onManage) {
            VStack(spacing: 6) {
                Image(systemName: "plus")
                    .font(.system(size: 14 * scale, weight: .semibold))
                    .foregroundStyle(F33Color.ink3)
                Capsule().fill(.clear).frame(height: 3)
            }
            // A capsule given only a height takes every point it is offered.
            // Left unfixed it made this button as wide as the whole bar with a
            // plus floating in the middle of it, and made each lane button ask
            // for unbounded width — which is what crushed the lanes into
            // slivers. The row is sized by its label, and the capsule follows.
            .fixedSize(horizontal: true, vertical: false)
            .padding(.horizontal, 14)
            .frame(minHeight: F33Layout.minTouchTarget)
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .accessibilityLabel("Add a feed")
    }

    private func laneButton(_ option: FeedLane) -> some View {
        let isActive = option == lane

        return Button {
            withAnimation(reduceMotion ? nil : F33Motion.easeOut) { lane = option }
        } label: {
            VStack(spacing: 6) {
                HStack(spacing: 5) {
                    Text(option.title)
                        .font(.system(size: 15 * scale, weight: isActive ? .bold : .regular))
                        .foregroundStyle(isActive ? F33Color.ink : F33Color.ink3)
                        .lineLimit(1)
                        .fixedSize()

                    if option.showsLiveCount, let liveCount, liveCount > 0 {
                        LiveBadge(count: liveCount)
                    }
                }

                // Drawn on every lane and made transparent when inactive, so
                // selecting one does not change the strip's height.
                Capsule()
                    .fill(isActive ? F33Color.ink : .clear)
                    .frame(height: 3)
            }
            // Sized by the label, not by the underline — see `manageButton`.
            .fixedSize(horizontal: true, vertical: false)
            .padding(.horizontal, 14)
            .frame(minHeight: F33Layout.minTouchTarget)
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .accessibilityLabel(accessibilityLabel(option))
        .accessibilityAddTraits(isActive ? [.isSelected, .isButton] : .isButton)
    }

    private func accessibilityLabel(_ option: FeedLane) -> String {
        guard option.showsLiveCount, let liveCount, liveCount > 0 else { return option.title }
        return "\(option.title), \(liveCount) live now"
    }
}

/// The count beside Live. Danger red because it is the one lane whose contents
/// stop existing if you do not go now.
struct LiveBadge: View {
    let count: Int

    var body: some View {
        Text(Counts.label(count) ?? "")
            .font(.system(size: 10, weight: .bold).monospacedDigit())
            .foregroundStyle(.white)
            .lineLimit(1)
            .fixedSize()
            .padding(.horizontal, 5)
            .padding(.vertical, 2)
            .background(F33Color.danger, in: Capsule())
            .accessibilityHidden(true)
    }
}
