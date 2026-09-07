import Foundation

/// Decides whether Home's chrome is out of the way, from the scroll offsets
/// the list reports.
///
/// A value type, and here rather than in the view, because this is the whole
/// behaviour and it is worth being able to run a scroll through it. The bug it
/// was extracted to fix could not be seen by reading the view: the rule was
/// written against the step between two reports, and `onScrollGeometryChange`
/// fires every frame, so a report moves a handful of points and almost never
/// more. It was waiting for a jump that scrolling does not make, and the chrome
/// sat there through an entire flick.
///
/// What it measures instead is travel since the reader last turned around. A
/// hundred small steps one way add up and trip it; jitter around one spot never
/// does.
public struct ChromeScrollTracker: Equatable, Sendable {
    /// How far down before the chrome leaves. Long enough that a thumb resting
    /// on a moving list does not flicker it away.
    public static let travelToHide: CGFloat = 44

    /// How far up before it comes back. Deliberately shorter: going away should
    /// take a real scroll, coming back should not. Someone scrolling up is
    /// usually reaching for something that just left, and making them earn it
    /// twice reads as the screen fighting them.
    public static let travelToShow: CGFloat = 16

    /// Within this of the top, the chrome is always whole, whatever the reader
    /// did to get there.
    public static let atTop: CGFloat = 8

    /// The chrome never goes away before the content has moved at least this
    /// far, so the room the list keeps for it is never left bare.
    public static let minimumOffsetToHide: CGFloat = 160

    /// Whether the feed currently has the screen to itself.
    public private(set) var isHidden = false

    /// The chrome's full height, which is also the room the list keeps for it.
    /// Set by the view as it measures; the chrome is never hidden before the
    /// content has cleared it.
    public var chromeHeight: CGFloat = 0

    private var lastOffset: CGFloat = 0
    /// Where the current direction of travel began.
    private var travelOrigin: CGFloat = 0

    public init() {}

    /// Feeds one reported offset in. Returns true when the answer changed, so a
    /// caller can animate only on the transition.
    @discardableResult
    public mutating func offsetChanged(to offset: CGFloat) -> Bool {
        let previous = lastOffset
        lastOffset = offset

        guard offset > Self.atTop else {
            travelOrigin = offset
            return set(false)
        }
        guard offset != previous else { return false }

        // Turning around restarts the measurement from where the turn happened.
        let movingDown = offset > previous
        if movingDown != (offset > travelOrigin) { travelOrigin = previous }

        let travelled = offset - travelOrigin
        if travelled > Self.travelToHide, offset > max(Self.minimumOffsetToHide, chromeHeight) {
            return set(true)
        }
        if travelled < -Self.travelToShow {
            return set(false)
        }
        return false
    }

    private mutating func set(_ hidden: Bool) -> Bool {
        guard hidden != isHidden else { return false }
        isHidden = hidden
        // The next toggle measures from here, so turning around right after one
        // needs its own full travel rather than inheriting this one's.
        travelOrigin = lastOffset
        return true
    }
}
