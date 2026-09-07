import Foundation
import Testing
@testable import F33D3RKit

/// Remembering the lane is the behaviour the whole strip exists for, so it is
/// pinned here rather than left to a screenshot. Each test gets its own defaults
/// suite: a shared one would let the order tests run in decide what they see.
@Suite("Feed preferences")
@MainActor
struct FeedPreferencesTests {

    private func defaults(_ name: String = UUID().uuidString) -> UserDefaults {
        UserDefaults(suiteName: name)!
    }

    @Test("A first launch opens on Following, not on the algorithmic lane")
    func defaultsToFollowing() {
        let preferences = FeedPreferences(defaults: defaults())

        #expect(preferences.lane == .surface(.following))
        #expect(FeedSurface.default == .following)
        #expect(preferences.density == .card)
        #expect(preferences.lanes == FeedLane.default)
    }

    @Test("The chosen lane survives a relaunch")
    func lanePersists() {
        let store = defaults()

        let first = FeedPreferences(defaults: store)
        first.lane = .surface(.music)

        // A second instance is what the next launch builds.
        let second = FeedPreferences(defaults: store)
        #expect(second.lane == .surface(.music))
    }

    @Test("A lane stored by a build that had one this build does not falls back")
    func unknownStoredLaneFallsBack() {
        let store = defaults()
        store.set("aquarium", forKey: "f33d3r.feed.lane")
        store.set("enormous", forKey: "f33d3r.feed.density")

        let preferences = FeedPreferences(defaults: store)
        #expect(preferences.lane == .surface(.following))
        #expect(preferences.density == .card)
    }

    @Test("Setting the same lane again does not churn the store")
    func idempotentWrite() {
        let store = defaults()
        let preferences = FeedPreferences(defaults: store)

        preferences.lane = .surface(.visions)
        preferences.lane = .surface(.visions)

        #expect(store.string(forKey: "f33d3r.feed.lane") == "s:visions")
    }

    @Test("Following leads the strip and every lane has a label")
    func laneOrderAndTitles() {
        #expect(FeedSurface.allCases.first == .following)
        #expect(FeedSurface.allCases.map(\.title) == ["Following", "For you", "Trending", "Sports", "18+", "Music", "Visions", "Live"])
        // The wire values are what `?surface=` carries; a rename here is a
        // protocol change, not a copy change. Every one of these is a surface
        // feed-engine's apiV1Feed switch actually answers.
        #expect(FeedSurface.allCases.map(\.rawValue) == ["following", "foryou", "trending", "sports", "nsfw", "music", "visions", "live"])
    }

    // MARK: The reader's own strip

    @Test("A topic is a tag read as a feed, and survives a relaunch")
    func topicLanePersists() {
        let store = defaults()
        let first = FeedPreferences(defaults: store)
        first.add(.topic("sports"))

        #expect(first.lane == .topic("sports"))
        #expect(first.lanes.contains(.topic("sports")))

        let second = FeedPreferences(defaults: store)
        #expect(second.lanes.contains(.topic("sports")))
        #expect(second.lane == .topic("sports"))
    }

    @Test("A tag is normalised to what the server matches on")
    func topicsAreNormalised() {
        #expect(FeedLane.normalise("#Sports") == "sports")
        #expect(FeedLane.normalise("  NEWS  ") == "news")
        // Nothing that is not a tag can become a lane.
        #expect(FeedLane.normalise("") == nil)
        #expect(FeedLane.normalise("#") == nil)
        #expect(FeedLane.normalise("two words") == nil)
    }

    /// Removing the lane the reader is on would leave the pager showing a
    /// column that is not in the strip.
    @Test("Removing the open lane moves off it")
    func removingTheOpenLaneMovesOff() {
        let preferences = FeedPreferences(defaults: defaults())
        preferences.add(.topic("sports"))
        #expect(preferences.lane == .topic("sports"))

        preferences.remove(.topic("sports"))
        #expect(!preferences.lanes.contains(.topic("sports")))
        #expect(preferences.lane != .topic("sports"))
        #expect(preferences.lanes.contains(preferences.lane))
    }

    /// A strip written by a build that had no strip at all, or one edited down
    /// to nothing, still opens on something.
    @Test("A stored strip missing the fixed lanes gets them back")
    func fixedLanesAreRestored() {
        let store = defaults()
        store.set(["s:music"], forKey: "f33d3r.feed.lanes")

        let preferences = FeedPreferences(defaults: store)
        #expect(preferences.lanes.prefix(2) == [.surface(.following), .surface(.forYou)])
        #expect(preferences.lanes.contains(.surface(.music)))
    }

    /// The old key held a bare surface name. A reader who updates should not
    /// find themselves back on Following.
    @Test("A lane stored by an older build still opens")
    func olderStoredLaneStillOpens() {
        let store = defaults()
        store.set("visions", forKey: "f33d3r.feed.lane")

        #expect(FeedPreferences(defaults: store).lane == .surface(.visions))
    }

    /// The 18+ lane is the only one an account has to be cleared for, and it
    /// is never in the strip a reader starts with: an adult feed is opted into,
    /// never arrived at.
    @Test("18+ is gated and not in the default strip")
    func adultLaneIsOptIn() {
        #expect(FeedSurface.nsfw.requiresAdultContent)
        #expect(FeedSurface.allCases.filter(\.requiresAdultContent) == [.nsfw])
        #expect(!FeedLane.default.contains(.surface(.nsfw)))
    }

    @Test("Only Live carries a count")
    func onlyLiveIsBadged() {
        #expect(FeedSurface.live.showsLiveCount)
        #expect(FeedSurface.allCases.filter(\.showsLiveCount) == [.live])
    }

    // MARK: A new lane reaching a reader who has already edited their strip

    @Test("A lane this build ships is added to a strip stored before it existed")
    func shippedLaneJoinsAnOldStrip() {
        let store = defaults()
        // What a build without Sports wrote.
        store.set(["s:following", "s:foryou", "s:trending"], forKey: "f33d3r.feed.lanes")

        let preferences = FeedPreferences(defaults: store)

        #expect(preferences.lanes.contains(.surface(.sports)))
        // At the end, so a strip the reader ordered stays in their order.
        #expect(Array(preferences.lanes.prefix(3)) == [FeedLane.surface(.following), .surface(.forYou), .surface(.trending)])
    }

    @Test("A lane the reader removed stays removed across a relaunch")
    func removalIsRemembered() {
        let store = defaults()

        let first = FeedPreferences(defaults: store)
        first.remove(.surface(.music))
        #expect(!first.lanes.contains(.surface(.music)))

        let second = FeedPreferences(defaults: store)
        #expect(!second.lanes.contains(.surface(.music)))
    }

    @Test("Adding a lane back forgets that it was ever removed")
    func addingClearsTheRemoval() {
        let store = defaults()

        let first = FeedPreferences(defaults: store)
        first.remove(.surface(.music))
        first.add(.surface(.music))

        let second = FeedPreferences(defaults: store)
        #expect(second.lanes.contains(.surface(.music)))
    }

    @Test("Following and For you cannot be removed")
    func fixedLanesStay() {
        let preferences = FeedPreferences(defaults: defaults())

        preferences.remove(.surface(.following))
        preferences.remove(.surface(.forYou))

        #expect(preferences.lanes.contains(.surface(.following)))
        #expect(preferences.lanes.contains(.surface(.forYou)))
    }

    @Test("Reset puts back every shipped lane, including ones taken out")
    func resetRestoresTheStrip() {
        let store = defaults()

        let preferences = FeedPreferences(defaults: store)
        preferences.remove(.surface(.music))
        preferences.remove(.surface(.live))
        preferences.add(.topic("nba"))

        preferences.resetLanes()

        #expect(preferences.lanes == FeedLane.default)
        #expect(preferences.lane == .surface(.following))
        // And the removals are forgotten, so nothing comes back missing.
        #expect(FeedPreferences(defaults: store).lanes == FeedLane.default)
    }

}

/// Likes are private on this platform. The tab list is where that rule is
/// enforced in the UI, so it is asserted rather than assumed.
@Suite("Profile tabs")
struct ProfileTabTests {

    @Test("Another person's profile has no Likes tab")
    func likesHiddenForVisitors() {
        #expect(!Profile.Tab.visible(isOwner: false).contains(.likes))
        #expect(Profile.Tab.visible(isOwner: false) == [.works, .replies, .media])
    }

    @Test("Your own profile keeps all four")
    func likesVisibleToOwner() {
        #expect(Profile.Tab.visible(isOwner: true) == Profile.Tab.allCases)
    }
}
