import Foundation

/// Every destination the app can push.
///
/// One enum rather than scattered `NavigationLink(destination:)` bodies, so a
/// deep link, a tap on a handle inside body text and a tap on a card all arrive
/// at the same screen by the same path.
enum Route: Hashable {
    case work(id: String)
    /// The works that quote one work, from "View quotes" under its detail.
    case workQuotes(id: String)
    case profile(handle: String)
    /// A hashtag, reached from a tag pill under a work or a search result.
    case tag(String)
    /// A live room.
    case live(id: String)
    /// One audio room — a Frequency.
    case frequency(id: String)
    /// The Frequencies list: Live, Scheduled, Mine.
    case frequencies
    /// One ticker, from a $CASHTAG in a body or the quote card under a work.
    /// The web's /stocks/{ticker}.
    case stocks(ticker: String)
    /// One conversation, from the Messages list or a profile.
    case conversation(id: String)
    /// The search screen. Everyday searching happens in the field pinned in
    /// Explore's chrome, so nothing in the app pushes this any more; it stays
    /// as the destination a deep link or a debug run rooted at search needs to
    /// land on, and as the one screen that opens on recents and trending tags.
    case search
    /// Who follows someone, and who they follow. From the counts on a profile.
    case followers(handle: String)
    case following(handle: String)
    /// The signed-in account's settings. From the gear on the reader's own
    /// profile.
    case settings
    /// The account's own F33D3R Numbers, from Settings.
    case numbers
}
