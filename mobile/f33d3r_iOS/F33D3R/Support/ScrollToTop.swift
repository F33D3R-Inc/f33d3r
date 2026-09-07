import SwiftUI

/// A counter that lists watch. Each increment is one request to scroll to the
/// top: a tab re-tapped while already selected, or the "new works" pill on a
/// lane. Delivered through the environment so a list four levels down a tab
/// does not need a binding threaded to it.
private struct ScrollToTopTickKey: EnvironmentKey {
    static let defaultValue = 0
}

extension EnvironmentValues {
    var scrollToTopTick: Int {
        get { self[ScrollToTopTickKey.self] }
        set { self[ScrollToTopTickKey.self] = newValue }
    }
}

/// The height of chrome floating over the top of a lane, which the lane's
/// scroll view keeps as a content margin so its first row starts below it.
///
/// A margin rather than a safe-area inset, deliberately: SwiftUI clips a
/// scroll view's content at a safe-area inset, so nothing would ever pass
/// behind the glass and hiding the chrome would uncover bare page. A content
/// margin leaves the scroll view full height — the feed runs under the chrome,
/// the material has something to refract, and when the chrome hides the rows
/// that were behind it are simply there.
private struct LaneTopInsetKey: EnvironmentKey {
    static let defaultValue: CGFloat = 0
}

extension EnvironmentValues {
    var laneTopInset: CGFloat {
        get { self[LaneTopInsetKey.self] }
        set { self[LaneTopInsetKey.self] = newValue }
    }
}

/// The anchor a list scrolls back to.
enum ScrollTopAnchor: Hashable {
    case top
}
