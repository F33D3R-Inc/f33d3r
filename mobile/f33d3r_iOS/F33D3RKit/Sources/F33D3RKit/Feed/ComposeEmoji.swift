import Foundation
import Observation

/// The composer's emoji.
///
/// The phone has a whole emoji keyboard, but no app can switch to it on the
/// reader's behalf, and a button that says "tap the globe key" is a button
/// that does nothing. So the composer offers the same sixty the web's picker
/// offers — `_gnEmojiPicker` in `f33d3r.js`, the list shared by the work and
/// message composers there — in the same order, so a reader who knows where
/// the flame is on the site finds it in the same place here.
public enum ComposeEmoji {

    /// The web's list, verbatim.
    public static let palette: [String] = [
        "😀", "😂", "🥹", "😍", "🥰", "😎", "🤔", "😤", "😭", "😈",
        "❤️", "🧡", "💛", "💚", "💙", "💜", "🖤", "🤍", "💔", "💯",
        "🔥", "✨", "⚡", "💡", "🌙", "☀️", "🌈", "🌊", "🦋", "🌸",
        "👀", "💪", "🙏", "👏", "🎉", "🎊", "🎵", "🎶", "🔮", "🚀",
        "🍕", "☕", "🥂", "🎯", "🏆", "🌟", "💫", "👾", "🤖", "💀",
        "😏", "🫶", "🤝", "✌️", "🫠", "🥲", "😬", "🤯", "🥳", "💃",
    ]
}

/// The emoji this device has used lately, most recent first.
///
/// A convenience of the same kind as the web's `localStorage`: it says nothing
/// about the account and follows nobody anywhere, so it lives in defaults on
/// this device and is not the server's to know. One row's worth, because the
/// point is reaching for the one used a minute ago, not a second palette.
@MainActor
@Observable
public final class RecentEmoji {

    /// One row of the picker's grid.
    public static let capacity = 8

    public private(set) var items: [String]

    @ObservationIgnored private let defaults: UserDefaults

    public init(defaults: UserDefaults = .standard) {
        self.defaults = defaults
        let stored = defaults.array(forKey: Key.recent) as? [String] ?? []
        // Only what the palette still offers. A stored value from a build with
        // a different list would otherwise sit in the row forever.
        items = Array(stored.filter { ComposeEmoji.palette.contains($0) }.prefix(Self.capacity))
    }

    /// Records a use. The emoji moves to the front; the oldest falls off.
    public func note(_ emoji: String) {
        var next = items.filter { $0 != emoji }
        next.insert(emoji, at: 0)
        items = Array(next.prefix(Self.capacity))
        defaults.set(items, forKey: Key.recent)
    }

    private enum Key {
        static let recent = "f33d3r.compose.recentEmoji"
    }
}
