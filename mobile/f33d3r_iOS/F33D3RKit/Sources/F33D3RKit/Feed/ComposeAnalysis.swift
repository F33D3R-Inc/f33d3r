import Foundation

/// What the shape of a draft suggests about it.
///
/// A port of `ComposeAER` in `f33d3r.js`: past five hundred characters the web
/// composer calls the draft a long post and shows how long it takes to read;
/// short of that, a draft that already has two paragraphs and three hundred
/// characters is offered as a thread, one paragraph per part. The thresholds
/// and the reading speed are the web's so the two composers say the same thing
/// about the same text.
///
/// Neither observation changes what is sent. The web's long mode once set a
/// `long_post` kind, which the server does not have and refuses with
/// `400 unknown work kind`; here a long post is a post, and the only effect of
/// the reading time is that the author is told it. Making a thread is the
/// author's choice and produces `thread_post` parts like any other thread.
public enum ComposeAnalysis {

    /// Characters at which a draft is called long.
    public static let longThreshold = 500
    /// Characters at which a multi-paragraph draft is offered as a thread.
    public static let threadThreshold = 300
    /// A paragraph shorter than this does not count toward the two the
    /// suggestion needs. `_analyze` uses `p.trim().length > 20`.
    public static let paragraphMinimum = 20
    /// The web's `WORDS_PER_MIN`.
    public static let wordsPerMinute = 200

    /// Whether the draft is long, by the web's rule.
    public static func isLong(_ text: String) -> Bool {
        // JavaScript's `length` counts UTF-16 units, and this is the same count
        // the composer's limit is checked against.
        text.utf16.count >= longThreshold
    }

    /// Minutes to read a long draft at two hundred words a minute, never less
    /// than one. `nil` for a draft that is not long — the label is part of the
    /// long mode and is not shown otherwise.
    public static func readingMinutes(of text: String) -> Int? {
        guard isLong(text) else { return nil }
        let words = text.trimmingCharacters(in: .whitespacesAndNewlines)
            .split(whereSeparator: \.isWhitespace)
            .count
        return max(1, Int((Double(words) / Double(wordsPerMinute)).rounded()))
    }

    /// The draft cut at blank lines, trimmed, with the empty pieces dropped.
    /// This is what "Make it a thread" turns into parts — `convertToThread`
    /// splits on `\n{2,}` and keeps every non-blank piece.
    public static func paragraphs(of text: String) -> [String] {
        text.components(separatedBy: blankLine)
            .map { $0.trimmingCharacters(in: .whitespacesAndNewlines) }
            .filter { !$0.isEmpty }
    }

    /// Whether to offer the draft as a thread: at least two paragraphs of more
    /// than twenty characters, three hundred characters in all, and not already
    /// long — the web's modes are exclusive and long wins.
    public static func suggestsThread(_ text: String) -> Bool {
        guard !isLong(text), text.utf16.count >= threadThreshold else { return false }
        let substantial = text.components(separatedBy: blankLine)
            .filter { $0.trimmingCharacters(in: .whitespacesAndNewlines).utf16.count > paragraphMinimum }
        return substantial.count >= 2
    }

    /// `\n{2,}` — two or more newlines, however many.
    private static let blankLine = try! NSRegularExpression(pattern: #"\n{2,}"#)
}

private extension String {
    func components(separatedBy pattern: NSRegularExpression) -> [String] {
        let whole = NSRange(startIndex..., in: self)
        var pieces: [String] = []
        var cursor = 0
        for match in pattern.matches(in: self, range: whole) {
            let piece = NSRange(location: cursor, length: match.range.location - cursor)
            pieces.append((self as NSString).substring(with: piece))
            cursor = match.range.location + match.range.length
        }
        pieces.append((self as NSString).substring(from: cursor))
        return pieces
    }
}
