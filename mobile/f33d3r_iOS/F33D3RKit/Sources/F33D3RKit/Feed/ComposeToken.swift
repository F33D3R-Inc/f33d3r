import Foundation

/// The `@`, `#` or `$` token under the caret while a work is being written.
///
/// The three triggers are the server's and so are their shapes: `mentionRe`,
/// `hashtagRe` and `cashtagRe` in `feed-engine/internal/handler/handlers.go`
/// decide what a posted body is actually linkified as. A composer that offered
/// to complete something the server will not linkify would be teaching the
/// wrong thing, so the character sets and the length caps below are those three
/// regexes rather than a looser guess at them.
///
/// Two deliberate differences, both about a token that is still being written:
///
/// - **No boundary is required before the trigger.** The server requires none
///   either — `foo@bar` linkifies `@bar` — so offering a completion there is
///   offering exactly what will happen.
/// - **A cashtag matches either case while it is being typed**, though the
///   server matches only `[A-Z]`. Nobody reaches for shift before they reach
///   for a ticker. What gets *inserted* is always upper case, which is what
///   makes the finished body match `cashtagRe`. That half-typed rule is
///   ``Cashtag/typedTicker(_:)`` and the finished one is ``Cashtag/pattern``;
///   both live there so the composer and the renderer share one alphabet.
///
/// Offsets are UTF-16, because the thing holding the caret is a `UITextView`
/// and UTF-16 is the only unit it counts in. Every character a token may
/// contain is ASCII, so a scan over code units cannot land inside a character:
/// anything above ASCII simply ends the run, which is the right answer anyway.
public struct ComposeToken: Hashable, Sendable {

    /// What starts a token.
    public enum Trigger: Character, Sendable, CaseIterable {
        case mention = "@"
        case hashtag = "#"
        case cashtag = "$"

        /// The cap in the server's regex. Past it the server stops matching and
        /// the tail is plain text, so past it there is nothing to complete.
        public var maxLength: Int {
            switch self {
            case .mention: return 50
            case .hashtag: return 100
            case .cashtag: return 5
            }
        }

        fileprivate var codeUnit: UInt16 {
            switch self {
            case .mention: return 0x40   // @
            case .hashtag: return 0x23   // #
            case .cashtag: return 0x24   // $
            }
        }

        fileprivate init?(codeUnit: UInt16) {
            switch codeUnit {
            case 0x40: self = .mention
            case 0x23: self = .hashtag
            case 0x24: self = .cashtag
            default: return nil
            }
        }

    }

    public let trigger: Trigger
    /// What has been typed after the trigger, as typed.
    public let query: String
    /// The whole token — trigger included — as a UTF-16 range in the body.
    public let range: NSRange

    public init(trigger: Trigger, query: String, range: NSRange) {
        self.trigger = trigger
        self.query = query
        self.range = range
    }

    // MARK: - Finding one

    /// The token the caret is in, if it is in one.
    ///
    /// The run is taken in both directions from the caret, so putting the caret
    /// back into a mention already written offers to complete that mention
    /// rather than half of it.
    ///
    /// - Parameter caret: a UTF-16 offset into `body`.
    public static func detect(in body: String, caret: Int) -> ComposeToken? {
        let units = Array(body.utf16)
        guard caret > 0, caret <= units.count else { return nil }

        // Back to the trigger over anything any trigger could hold, so the
        // trigger is found first and gets to judge its own run.
        var start = caret
        while start > 0, isWordUnit(units[start - 1]) { start -= 1 }
        guard start > 0, let trigger = Trigger(codeUnit: units[start - 1]) else { return nil }

        // Forward over the rest of the word the caret is sitting inside.
        var end = caret
        while end < units.count, isWordUnit(units[end]) { end += 1 }

        let run = units[start..<end]
        let typed = String(decoding: run, as: UTF16.self)

        switch trigger {
        case .mention, .hashtag:
            guard run.count <= trigger.maxLength else { return nil }
        case .cashtag:
            // The half-typed shape of a ticker is `Cashtag/typedTicker(_:)` and
            // not something restated here: it is the same alphabet the finished
            // pattern uses, and one of the two would eventually drift.
            guard Cashtag.typedTicker(typed) != nil else { return nil }
        }

        return ComposeToken(
            trigger: trigger,
            query: typed,
            range: NSRange(location: start - 1, length: run.count + 1)
        )
    }

    // MARK: - Completing one

    /// `body` with this token replaced by `value`, and where the caret goes.
    ///
    /// Only the token's own range is touched — the rest of the body is the
    /// bytes it already was.
    ///
    /// A space is added after the completion when the token ends the body, so
    /// the next word does not run into it. Anywhere else the body already says
    /// what comes next, and adding one would write `@alice ,` or double a space
    /// that is already there.
    public func completing(with value: String, in body: String) -> (body: String, caret: Int) {
        let units = Array(body.utf16)
        let start = min(max(0, range.location), units.count)
        let end = min(start + range.length, units.count)

        let inserted = Array((String(trigger.rawValue) + value).utf16)
        let spacer: [UInt16] = end >= units.count ? [0x20] : []

        let head = String(decoding: units[..<start], as: UTF16.self)
        let tail = String(decoding: units[end...], as: UTF16.self)
        let middle = String(decoding: inserted + spacer, as: UTF16.self)

        return (head + middle + tail, start + inserted.count + spacer.count)
    }

    // MARK: - Highlighting

    /// One stretch of body text the server will linkify.
    public struct Highlight: Hashable, Sendable {
        public let trigger: Trigger
        public let range: NSRange
    }

    /// `composeHighlightRe`, which is what the web composer paints with.
    ///
    /// The cashtag half is `Cashtag/pattern` concatenated rather than copied,
    /// so this matcher and the one that decides which ticker a work's card is
    /// for cannot drift apart — a Go test reads that literal and fails if they
    /// do. The other two alternatives are `mentionRe` and `hashtagRe` written
    /// out, cap included.
    private static let highlightRegex = try! NSRegularExpression(
        pattern: #"@[A-Za-z0-9_]{1,50}|#[A-Za-z0-9_]{1,100}|"# + Cashtag.pattern
    )

    /// Every mention, hashtag and cashtag in `body`, as the server will find
    /// them — the composer's own highlighter, run over what is being typed.
    ///
    /// This is the *finished* shape and not the shape being typed: a ticker is
    /// upper case only and must end on a word boundary, so `$appl` colours
    /// nothing until it is `$AAPL`. The colour arriving is the reader being
    /// told the token is real.
    public static func highlights(in body: String) -> [Highlight] {
        let whole = NSRange(body.startIndex..., in: body)
        let units = Array(body.utf16)
        return highlightRegex.matches(in: body, range: whole).compactMap { match in
            let range = match.range
            guard range.location >= 0, range.location < units.count,
                  let trigger = Trigger(codeUnit: units[range.location])
            else { return nil }
            return Highlight(trigger: trigger, range: range)
        }
    }
}

// MARK: - ASCII

/// `[A-Za-z0-9_]` — the character class shared by all three server regexes,
/// used to find where a word ends before any trigger judges it.
private func isWordUnit(_ unit: UInt16) -> Bool {
    (unit >= 0x41 && unit <= 0x5A) || (unit >= 0x61 && unit <= 0x7A)
        || (unit >= 0x30 && unit <= 0x39) || unit == 0x5F
}
