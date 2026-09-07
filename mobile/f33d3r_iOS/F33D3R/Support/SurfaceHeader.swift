import SwiftUI
import F33D3RKit

/// The title bar on a surface that has a feed under it.
///
/// The same construction as Home's header row — a glass bar whose fill runs out
/// under the status bar, with the works passing behind it — rather than the
/// system navigation bar. Two reasons, in order:
///
/// 1. It is the arrangement Home already has, and a reader moving between tabs
///    should not find the top of the screen behaving differently in each one.
/// 2. A navigation bar decides for itself whether to draw a background, from its
///    own idea of whether the scroll view is at its top edge. Pinning our own
///    chrome to the scroll view means the bar and the content agree about where
///    the top is, and the material is doing its job at every scroll position
///    rather than at the ones the bar happens to notice.
///
/// It is for tab roots only. A pushed screen keeps the system bar, because the
/// back button, the swipe-back gesture and the title transition are worth more
/// there than a matching slab.
struct SurfaceHeaderRow<Trailing: View>: View {
    let title: String
    /// Whether this bar closes the stack with a hairline. False when something
    /// else — a lane strip — sits directly beneath it and carries it instead, so
    /// the two read as one surface rather than two.
    var closesBar = true
    @ViewBuilder var trailing: () -> Trailing

    var body: some View {
        HStack(spacing: F33Spacing.sm) {
            Text(title)
                .font(.system(size: 22, weight: .bold))
                .foregroundStyle(F33Color.ink)
                .lineLimit(1)
                // The title shrinks before it truncates: a surface whose name is
                // cut in half at an accessibility size is a surface nobody can
                // name.
                .minimumScaleFactor(0.7)

            Spacer(minLength: F33Spacing.sm)

            trailing()
        }
        .padding(.horizontal, F33Spacing.lg)
        .padding(.vertical, F33Spacing.sm)
        .frame(maxWidth: .infinity, minHeight: F33Layout.minTouchTarget, alignment: .leading)
        .f33GlassBar(bottomHairline: closesBar, extending: .top)
        .accessibilityElement(children: .contain)
        .accessibilityAddTraits(.isHeader)
    }
}

extension SurfaceHeaderRow where Trailing == EmptyView {
    init(title: String, closesBar: Bool = true) {
        self.init(title: title, closesBar: closesBar) { EmptyView() }
    }
}
