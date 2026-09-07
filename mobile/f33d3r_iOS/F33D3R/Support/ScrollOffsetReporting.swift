import SwiftUI

/// How far a lane has scrolled past its top, reported to whatever floats over
/// it.
///
/// This is the one path Home's chrome hears about scrolling on. Every lane in
/// the pager uses it — the plain `WorkList` ones and the three that draw
/// themselves, Sports, Music and Live — because a header that gets out of the
/// way on For you and sits there on Live is read as the feature being broken
/// again, and it was, twice.
///
/// It is deliberately two halves. iOS 18 has the system's own answer,
/// `onScrollGeometryChange`, which fires for the first layout as well as every
/// scroll and knows about the margin the chrome adds; on 17 the offset has to
/// be read off the content's own frame from inside the scroll view, which is
/// somewhere a modifier on the scroll view cannot reach. So the reporter goes
/// on the scroll view, ``LegacyScrollOffset`` goes on its content, and on 18
/// the second one does nothing.
struct ScrollOffsetReporter: ViewModifier {
    let onScroll: ((CGFloat) -> Void)?

    func body(content: Content) -> some View {
        if #available(iOS 18.0, *) {
            content.onScrollGeometryChange(for: CGFloat.self) { geometry in
                geometry.contentOffset.y + geometry.contentInsets.top
            } action: { _, offset in
                onScroll?(offset)
            }
        } else {
            content.onPreferenceChange(ScrollOffsetKey.self) { offset in
                onScroll?(offset)
            }
        }
    }
}

/// The iOS 17 half of ``ScrollOffsetReporter``: publishes the content's top
/// edge as a preference. Does nothing on 18, where the geometry API reports.
///
/// Goes on the scroll view's content, and the scroll view it is measured
/// against is named ``ScrollOffsetSpace/lane``.
struct LegacyScrollOffset: ViewModifier {
    let onScroll: ((CGFloat) -> Void)?

    func body(content: Content) -> some View {
        if #available(iOS 18.0, *) {
            content
        } else {
            content.background {
                GeometryReader { inner in
                    Color.clear.preference(
                        key: ScrollOffsetKey.self,
                        value: -inner.frame(in: .named(ScrollOffsetSpace.lane)).minY
                    )
                }
            }
        }
    }
}

/// The coordinate space the iOS 17 path measures in. A constant rather than a
/// string at both ends, because the two ends are now in different files and a
/// typo between them is a lane whose chrome silently stops moving.
enum ScrollOffsetSpace {
    static let lane = "laneScroll"
}

private struct ScrollOffsetKey: PreferenceKey {
    static let defaultValue: CGFloat = 0
    static func reduce(value: inout CGFloat, nextValue: () -> CGFloat) { value = nextValue() }
}
