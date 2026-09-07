import Foundation

/// One column of the lane strip.
///
/// The strip is the reader's own row of feeds, so it is not a fixed list. Two
/// kinds of thing can sit in it and the difference is only where the works come
/// from: a surface the server defines — Following, For you, Trending, Sports,
/// Music, Visions, Live — or a topic, which is any tag at all read as a feed.
///
/// The difference matters. Sports is a surface, not a topic: it is a row in
/// feed_surfaces carrying two dozen league tags and it comes with a scoreboard,
/// so it is a lane the platform ships. A topic is one tag the reader named, and
/// it is how anything the platform has not shipped a lane for still becomes a
/// feed.
///
/// Which lanes are up, and in what order, is a per-device reading preference and
/// lives with the others in ``FeedPreferences``. It is not content and not
/// identity, and a row of lanes chosen on a phone should not follow the reader
/// to a borrowed desktop.
public enum FeedLane: Hashable, Sendable, Identifiable {
    /// A feed the server defines.
    case surface(FeedSurface)
    /// A tag read as a feed. Stored without the `#`, lower-cased, because that
    /// is what the server matches on.
    case topic(String)

    public var id: String { storageKey }

    /// What the strip prints. A topic wears its hash, so a lane the reader
    /// added is visibly a tag rather than a surface somebody shipped.
    public var title: String {
        switch self {
        case .surface(let surface): return surface.title
        case .topic(let tag): return "#" + tag
        }
    }

    /// Only Live carries a count beside its label.
    public var showsLiveCount: Bool {
        if case .surface(let surface) = self { return surface.showsLiveCount }
        return false
    }

    /// Whether the reader may take it out of the strip.
    ///
    /// Following and For you stay. One is the feed of people they chose and the
    /// other is the feed of everyone else, and a strip with neither is a reader
    /// who has removed their way back to the platform.
    public var isRemovable: Bool {
        switch self {
        case .surface(.following), .surface(.forYou): return false
        default: return true
        }
    }

    // MARK: Storage
    //
    // A single string, because this goes in UserDefaults as a list and a
    // prefix is enough to tell the two kinds apart. `s:` for a surface, `t:`
    // for a topic; a bare word is read as a surface so that a lane stored by an
    // older build still opens.

    public var storageKey: String {
        switch self {
        case .surface(let surface): return "s:" + surface.rawValue
        case .topic(let tag): return "t:" + tag
        }
    }

    public init?(storageKey raw: String) {
        if let tag = raw.stripping(prefix: "t:") {
            guard let normalised = FeedLane.normalise(tag) else { return nil }
            self = .topic(normalised)
        } else if let surface = FeedSurface(rawValue: raw.stripping(prefix: "s:") ?? raw) {
            self = .surface(surface)
        } else {
            return nil
        }
    }

    /// A tag as the server matches it: no `#`, no spaces, lower-cased. Nil when
    /// what is left is not a tag at all, so an empty chip can never be added.
    public static func normalise(_ raw: String) -> String? {
        let trimmed = raw.trimmingCharacters(in: .whitespacesAndNewlines)
            .trimmingCharacters(in: CharacterSet(charactersIn: "#"))
            .lowercased()
        guard !trimmed.isEmpty, trimmed.rangeOfCharacter(from: .whitespacesAndNewlines) == nil else {
            return nil
        }
        return trimmed
    }

    /// The strip a reader starts with: the two that cannot leave, and every
    /// surface this platform ships.
    public static let `default`: [FeedLane] = [
        .surface(.following),
        .surface(.forYou),
        .surface(.trending),
        .surface(.sports),
        .surface(.music),
        .surface(.visions),
        .surface(.live),
    ]
}

private extension String {
    func stripping(prefix: String) -> String? {
        hasPrefix(prefix) ? String(dropFirst(prefix.count)) : nil
    }
}
