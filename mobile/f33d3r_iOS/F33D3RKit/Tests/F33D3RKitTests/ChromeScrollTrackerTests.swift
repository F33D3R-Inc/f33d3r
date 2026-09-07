import Foundation
import Testing
@testable import F33D3RKit

/// Home's chrome gets out of the way while the reader is reading. The rule it
/// runs on used to be "did this one report move more than ten points", and
/// `onScrollGeometryChange` fires every frame, so a report moves two or three.
/// It was waiting for a jump that scrolling does not make. These run scrolls
/// through it the way a list reports them: many small steps.
@Suite("Chrome follows the reading")
struct ChromeScrollTrackerTests {
    /// One flick, reported the way the system reports it.
    static func scroll(_ tracker: inout ChromeScrollTracker, from: CGFloat, to: CGFloat, step: CGFloat = 3) {
        var offset = from
        let down = to > from
        while down ? offset < to : offset > to {
            offset += down ? step : -step
            tracker.offsetChanged(to: down ? min(offset, to) : max(offset, to))
        }
    }

    static func tracker(chromeHeight: CGFloat = 200) -> ChromeScrollTracker {
        var t = ChromeScrollTracker()
        t.chromeHeight = chromeHeight
        return t
    }

    @Test("A scroll down in ordinary small steps hides the chrome")
    func hidesOnTheWayDown() {
        var tracker = Self.tracker()
        #expect(!tracker.isHidden)

        Self.scroll(&tracker, from: 0, to: 400)
        #expect(tracker.isHidden, "a 400 point scroll reported three points at a time left the chrome up")
    }

    @Test("Scrolling back up brings it straight back")
    func showsOnTheWayUp() {
        var tracker = Self.tracker()
        Self.scroll(&tracker, from: 0, to: 400)
        #expect(tracker.isHidden)

        Self.scroll(&tracker, from: 400, to: 300)
        #expect(!tracker.isHidden, "the first real scroll up has to bring it back")
    }

    /// The room the list keeps for the chrome must never be left bare, so it
    /// stays until the content has cleared it.
    @Test("It stays while the content is still behind it")
    func staysUntilTheContentClears() {
        var tracker = Self.tracker(chromeHeight: 200)
        Self.scroll(&tracker, from: 0, to: 150)
        #expect(!tracker.isHidden)

        Self.scroll(&tracker, from: 150, to: 260)
        #expect(tracker.isHidden)
    }

    @Test("A thumb resting on a moving list does not flicker it")
    func jitterDoesNothing() {
        var tracker = Self.tracker()
        Self.scroll(&tracker, from: 0, to: 400)
        #expect(tracker.isHidden)

        // Two points either way, over and over.
        for _ in 0..<40 {
            tracker.offsetChanged(to: 402)
            tracker.offsetChanged(to: 400)
        }
        #expect(tracker.isHidden, "small movement around one spot changed the answer")
    }

    @Test("Reaching the top always brings it back")
    func topAlwaysShows() {
        var tracker = Self.tracker()
        Self.scroll(&tracker, from: 0, to: 400)
        #expect(tracker.isHidden)

        tracker.offsetChanged(to: 0)
        #expect(!tracker.isHidden)
    }

    /// Down, up, down again — each leg is measured from where the reader turned
    /// around, not from wherever they started.
    @Test("Turning around restarts the measurement")
    func turningAroundRestarts() {
        var tracker = Self.tracker()
        Self.scroll(&tracker, from: 0, to: 400)
        #expect(tracker.isHidden)

        Self.scroll(&tracker, from: 400, to: 375)   // a short scroll up is enough
        #expect(!tracker.isHidden)

        Self.scroll(&tracker, from: 375, to: 440)   // down again
        #expect(tracker.isHidden)
    }

    @Test("It reports only the changes, so a view animates once")
    func reportsTransitionsOnly() {
        var tracker = Self.tracker()
        var changes = 0
        for offset in stride(from: CGFloat(0), through: 400, by: 3) {
            if tracker.offsetChanged(to: offset) { changes += 1 }
        }
        #expect(changes == 1)
    }
}

/// Going away takes a real scroll; coming back does not. The two thresholds are
/// deliberately different and the difference is the feel of the screen.
@Suite("Leaving is reluctant, returning is eager")
struct ChromeThresholdTests {
    @Test("A short scroll up returns it, the same distance down does not hide it")
    func returningIsCheaperThanLeaving() {
        #expect(ChromeScrollTracker.travelToShow < ChromeScrollTracker.travelToHide)

        var tracker = ChromeScrollTracker()
        tracker.chromeHeight = 200
        ChromeScrollTrackerTests.scroll(&tracker, from: 0, to: 400)
        #expect(tracker.isHidden)

        // Twenty points up is enough to bring it back.
        ChromeScrollTrackerTests.scroll(&tracker, from: 400, to: 380)
        #expect(!tracker.isHidden)

        // Twenty points down is not enough to take it away again.
        ChromeScrollTrackerTests.scroll(&tracker, from: 380, to: 400)
        #expect(!tracker.isHidden)
    }
}
