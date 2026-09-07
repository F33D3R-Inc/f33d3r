import Foundation
import Observation

/// How much room a work card is given.
///
/// A reading preference, not a state the server owns: two people looking at the
/// same feed on the same account are entitled to different amounts of it on
/// screen.
/// How much room a work card is given.
///
/// One value, and that is the point rather than an oversight. There were two —
/// the full card and a tighter one — and the tighter one is gone because the
/// card *is* the work: the author, the badge, the media at the size it was
/// made, the row of things you can do about it. A denser feed shows more works
/// by showing less of each, and what it takes away is the part that made a work
/// worth stopping on.
///
/// The type survives with a single case instead of being deleted, because the
/// card's measurements are still asked for by name all over the drawing code
/// and a bare set of constants would lose the thread of why they belong
/// together.
public enum FeedDensity: String, CaseIterable, Sendable, Hashable, Identifiable {
    /// Full card — the geometry ported from `.post-card`.
    case card

    public static let `default`: FeedDensity = .card

    public var id: String { rawValue }

    public var title: String { "card" }
}

/// The two per-device reading preferences: which lane the feed opens on, and how
/// dense the cards are.
///
/// These are stored on the device rather than on the account, and deliberately
/// so. Nothing here is content, nothing here is identity, and nothing here is
/// worth a round trip — a lane chosen on a phone should not follow the reader to
/// a borrowed desktop.
///
/// Remembering the lane is the point of the whole strip. A feed that reopens on
/// the algorithmic lane after the reader deliberately left it is the specific
/// complaint this layout was drawn to answer, so the default is Following and
/// the last choice survives a relaunch.
///
/// Backed by `UserDefaults` rather than `@AppStorage` because a property wrapper
/// only exists inside a `View`, and this has to be readable — and testable —
/// from outside one.
@MainActor
@Observable
public final class FeedPreferences {

    public var lane: FeedLane {
        didSet {
            guard lane != oldValue else { return }
            defaults.set(lane.storageKey, forKey: Key.lane)
        }
    }

    /// The strip itself: which feeds are up, in the reader's order.
    ///
    /// A row of feeds is a reading preference like the other two here. It also
    /// decides what the reader can reach at all, so the two lanes that cannot
    /// be removed are enforced on the way in rather than trusted to the caller.
    public var lanes: [FeedLane] {
        didSet {
            guard lanes != oldValue else { return }
            defaults.set(lanes.map(\.storageKey), forKey: Key.lanes)
            // A selected lane that has just been removed would leave the pager
            // on a column that is not in the strip.
            if !lanes.contains(lane), let first = lanes.first { lane = first }
        }
    }

    /// The lanes this reader has taken out on purpose.
    ///
    /// Kept because "not in the strip" and "not wanted" are different, and only
    /// the second should survive a new lane shipping. Without it, a reader who
    /// tidied their row once would never see Sports, or anything added after
    /// it, and would have no way of knowing it existed.
    private(set) public var removed: Set<String> {
        didSet {
            guard removed != oldValue else { return }
            defaults.set(Array(removed), forKey: Key.removed)
        }
    }

    public var density: FeedDensity {
        didSet {
            guard density != oldValue else { return }
            defaults.set(density.rawValue, forKey: Key.density)
        }
    }

    /// Light, dark, or the device's own. Kept here with the lane and the
    /// density because it is the same kind of thing: how this reader likes to
    /// read on this device, and nothing the server needs to know.
    public var appearance: Appearance {
        didSet {
            guard appearance != oldValue else { return }
            defaults.set(appearance.rawValue, forKey: Key.appearance)
        }
    }

    @ObservationIgnored private let defaults: UserDefaults

    public init(defaults: UserDefaults = .standard) {
        self.defaults = defaults
        // A stored value from a build that had a lane this one does not — or a
        // first launch with nothing stored at all — falls back to the default
        // rather than trapping. Reading is where an unknown value has to be
        // survived; writing can only ever produce a known one.
        // A lane stored by a build that wrote a bare surface name still opens:
        // FeedLane reads an unprefixed value as a surface.
        var lane = FeedLane(storageKey: defaults.string(forKey: Key.lane) ?? "") ?? .surface(.default)
        var lanes = (defaults.array(forKey: Key.lanes) as? [String])?
            .compactMap(FeedLane.init(storageKey:)) ?? FeedLane.default
        if lanes.isEmpty { lanes = FeedLane.default }
        // The two that cannot leave are put back at the front if a stored strip
        // is missing them, whatever wrote it.
        for fixed in [FeedLane.surface(.forYou), .surface(.following)] where !lanes.contains(fixed) {
            lanes.insert(fixed, at: 0)
        }

        // Lanes this build ships that the reader has never turned down are
        // added at the end. This is what lets a new lane reach somebody who
        // edited their strip a month ago.
        let removed = Set((defaults.array(forKey: Key.removed) as? [String]) ?? [])
        for shipped in FeedLane.default where !lanes.contains(shipped) && !removed.contains(shipped.storageKey) {
            lanes.append(shipped)
        }
        if !lanes.contains(lane) { lane = lanes[0] }
        var density = FeedDensity(rawValue: defaults.string(forKey: Key.density) ?? "") ?? .default
        var appearance = Appearance(rawValue: defaults.string(forKey: Key.appearance) ?? "") ?? .system

        #if DEBUG
        // Opening straight onto a given lane or density, for looking at a screen
        // on a Simulator that has no way to tap one. Same shape as
        // `F33D3R_SAMPLE_FROM`, and the same rule: opt-in per run, never a
        // fallback. Resolved before the properties are set so the override is
        // not written back — a run started this way leaves whatever the reader
        // last chose exactly where it was.
        let environment = ProcessInfo.processInfo.environment
        // Takes a surface name or a topic: `F33D3R_LANE=music`, `=t:sports`.
        if let raw = environment["F33D3R_LANE"], let forced = FeedLane(storageKey: raw) {
            if !lanes.contains(forced) { lanes.append(forced) }
            lane = forced
        }
        if let raw = environment["F33D3R_DENSITY"], let forced = FeedDensity(rawValue: raw) {
            density = forced
        }
        if let raw = environment["F33D3R_APPEARANCE"], let forced = Appearance(rawValue: raw) {
            appearance = forced
        }
        #endif

        self.lane = lane
        self.lanes = lanes
        self.removed = removed
        self.density = density
        self.appearance = appearance
    }


    /// Puts a feed in the strip, at the end, and opens it. Adding one already
    /// there just goes to it.
    public func add(_ newLane: FeedLane) {
        removed.remove(newLane.storageKey)
        if !lanes.contains(newLane) { lanes.append(newLane) }
        lane = newLane
    }

    /// Takes a feed out. Following and For you refuse to go.
    public func remove(_ old: FeedLane) {
        guard old.isRemovable, let index = lanes.firstIndex(of: old) else { return }
        lanes.remove(at: index)
        removed.insert(old.storageKey)
    }

    /// Puts the strip back to what this build ships, and forgets every removal,
    /// so a reader who has pruned it down to nothing has one way back that does
    /// not involve remembering what used to be there.
    public func resetLanes() {
        removed = []
        lanes = FeedLane.default
        lane = lanes[0]
    }

    public func move(fromOffsets source: IndexSet, toOffset destination: Int) {
        lanes.move(fromOffsets: source, toOffset: destination)
    }

    private enum Key {
        static let lane = "f33d3r.feed.lane"
        static let lanes = "f33d3r.feed.lanes"
        static let removed = "f33d3r.feed.lanes.removed"
        static let density = "f33d3r.feed.density"
        static let appearance = "f33d3r.appearance"
    }
}
