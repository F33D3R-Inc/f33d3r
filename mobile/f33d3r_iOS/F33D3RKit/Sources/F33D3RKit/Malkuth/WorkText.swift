import Foundation

/// What a work's text is allowed to be: the client half of
/// `feed-engine/internal/worktext` (normalizer version 1), mirrored line for
/// line from `F33WorkText` in `malkuth.js`, the reference client.
///
/// The body and every poll option are inside the signed content address, so
/// the server never rewrites them: it refuses any text that is not already
/// what ``normalize(_:)`` makes of it. The composer therefore signs
/// `normalize(body)`, and the counter measures ``weightedLength(_:)`` of that
/// same normalised text, because that is the length the server limits.
///
/// Everything here works on Unicode scalars (code points), as Go's `range` over
/// a string and JavaScript's `for…of` do, never on Swift `Character`s
/// (grapheme clusters), which would weigh an emoji sequence as one.
public enum WorkText {

    /// The rule set implemented here. A change to the rules is a new version on
    /// the server first.
    public static let normalizerVersion = 1

    /// The one length limit of a body, in weighted units.
    public static let maxWeightedLength = 2000

    /// The stored-size bound of a body, in code points, independent of weight.
    public static let maxScalars = 10_000

    /// What one link weighs, whatever its length.
    public static let urlWeight = 23

    /// The longest a poll option may be, in code points.
    public static let pollOptionMaxScalars = 80

    // MARK: - Normalisation

    /// Normalizer v1, preceded by NFC as `malkuth.js` applies it:
    ///
    /// 1. NFC.
    /// 2. CRLF and lone CR become LF.
    /// 3. Control characters other than LF and TAB, bidi controls and invisible
    ///    characters are removed. ZWJ and ZWNJ are kept.
    /// 4. A line holding only whitespace becomes empty, and runs of empty lines
    ///    collapse to one.
    /// 5. Leading and trailing whitespace is trimmed.
    public static func normalize(_ text: String) -> String {
        let nfc = text.precomposedStringWithCanonicalMapping
        var kept = String.UnicodeScalarView()
        var scalars = Array(nfc.unicodeScalars)[...]
        while let s = scalars.popFirst() {
            if s.value == 0x0D {
                // CRLF and a lone CR are both one LF.
                if scalars.first?.value == 0x0A { scalars.removeFirst() }
                kept.append("\n")
                continue
            }
            if !isStripped(s.value) { kept.append(s) }
        }

        var lines: [String] = []
        var blank = false
        for line in String(kept).split(separator: "\n", omittingEmptySubsequences: false) {
            if line.unicodeScalars.allSatisfy({ isSpace($0.value) }) {
                if !blank { lines.append("") }
                blank = true
                continue
            }
            blank = false
            lines.append(String(line))
        }
        return trimSpace(lines.joined(separator: "\n"))
    }

    /// Whether `text` is already what ``normalize(_:)`` makes of it — the
    /// condition the server puts on every signed body and poll option.
    public static func isNormalized(_ text: String) -> Bool {
        normalize(text) == text
    }

    /// Whether `text` has nothing a reader can see: no letter, number,
    /// punctuation or symbol.
    public static func isBlank(_ text: String) -> Bool {
        !text.unicodeScalars.contains { isVisible($0) }
    }

    /// Whether a normalised value fits a one-line field (a poll option, a tag).
    public static func isSingleLine(_ text: String) -> Bool {
        !text.unicodeScalars.contains { $0.value == 0x0A }
    }

    /// The number of code points, the unit the server's rune bounds use.
    public static func scalarCount(_ text: String) -> Int {
        text.unicodeScalars.count
    }

    // MARK: - Weighted length

    /// The length the body limit is measured in. Each link weighs
    /// ``urlWeight``; everything else weighs what its code points weigh.
    ///
    /// A link is what Go's `https?://[^\s"'<>\x00-\x1f]+` matches, where `\s`
    /// is RE2's ASCII class — the server is the verifier, so its pattern is the
    /// one implemented. Sentence punctuation at a link's end is not the link,
    /// and is weighed as ordinary text.
    public static func weightedLength(_ text: String) -> Int {
        let s = Array(text.unicodeScalars)
        var n = 0
        var last = 0
        var i = 0
        while i < s.count {
            guard let schemeEnd = schemeEnd(s, at: i) else { i += 1; continue }
            var end = schemeEnd
            while end < s.count, isURLScalar(s[end].value) { end += 1 }
            guard end > schemeEnd else { i += 1; continue }
            var linkEnd = end
            while linkEnd > i, isURLTrailing(s[linkEnd - 1].value) { linkEnd -= 1 }
            n += weight(s[last..<i]) + urlWeight
            last = linkEnd
            i = end
        }
        return n + weight(s[last...])
    }

    /// How far over the limit `text` is, or 0. Measures both of the server's
    /// bounds and reports the one it would refuse on first.
    public static func overLimit(_ text: String) -> Int {
        let scalars = scalarCount(text)
        if scalars > maxScalars { return scalars - maxScalars }
        return max(0, weightedLength(text) - maxWeightedLength)
    }

    // MARK: - Rules, by code point

    static func isStripped(_ v: UInt32) -> Bool {
        switch v {
        case 0x0A, 0x09: return false
        case 0x00..<0x20, 0x7F, 0x80...0x9F: return true                 // C0, DEL, C1
        case 0x061C, 0x200E, 0x200F, 0x202A...0x202E, 0x2066...0x2069:   // bidi
            return true
        case 0x00AD, 0x180E, 0x200B, 0x2060...0x2064, 0x206A...0x206F,
             0x3164, 0xFFA0, 0xFEFF, 0xFFF9...0xFFFB:                     // invisible
            return true
        default: return false
        }
    }

    /// Go's `unicode.IsSpace`.
    static func isSpace(_ v: UInt32) -> Bool {
        switch v {
        case 0x09...0x0D, 0x20, 0x85, 0xA0, 0x1680, 0x2000...0x200A,
             0x2028, 0x2029, 0x202F, 0x205F, 0x3000:
            return true
        default: return false
        }
    }

    static func trimSpace(_ text: String) -> String {
        var scalars = Array(text.unicodeScalars)[...]
        while let f = scalars.first, isSpace(f.value) { scalars.removeFirst() }
        while let l = scalars.last, isSpace(l.value) { scalars.removeLast() }
        var view = String.UnicodeScalarView()
        view.append(contentsOf: scalars)
        return String(view)
    }

    static func isVisible(_ s: Unicode.Scalar) -> Bool {
        switch s.properties.generalCategory {
        case .uppercaseLetter, .lowercaseLetter, .titlecaseLetter, .modifierLetter, .otherLetter,
             .decimalNumber, .letterNumber, .otherNumber,
             .connectorPunctuation, .dashPunctuation, .openPunctuation, .closePunctuation,
             .initialPunctuation, .finalPunctuation, .otherPunctuation,
             .mathSymbol, .currencySymbol, .modifierSymbol, .otherSymbol:
            return true
        default:
            return false
        }
    }

    static func scalarWeight(_ s: Unicode.Scalar) -> Int {
        let v = s.value
        switch s.properties.generalCategory {
        case .nonspacingMark, .enclosingMark: return 0
        default: break
        }
        switch v {
        case 0x200C, 0x200D, 0xFE00...0xFE0F, 0xE0100...0xE01EF,
             0x1F3FB...0x1F3FF, 0xE0020...0xE007F:
            return 0
        case 0...0x10FF, 0x2000...0x200D, 0x2010...0x201F, 0x2032...0x2037:
            return 1
        default:
            return 2
        }
    }

    private static func weight(_ scalars: ArraySlice<Unicode.Scalar>) -> Int {
        scalars.reduce(0) { $0 + scalarWeight($1) }
    }

    /// The index just past `http://` or `https://` starting at `i`, or nil.
    private static func schemeEnd(_ s: [Unicode.Scalar], at i: Int) -> Int? {
        func matches(_ literal: String, _ at: Int) -> Bool {
            let lit = Array(literal.unicodeScalars)
            guard at + lit.count <= s.count else { return false }
            for k in 0..<lit.count where s[at + k] != lit[k] { return false }
            return true
        }
        if matches("https://", i) { return i + 8 }
        if matches("http://", i) { return i + 7 }
        return nil
    }

    /// `[^\s"'<>\x00-\x1f]` with RE2's ASCII `\s` (tab, LF, FF, CR, space).
    private static func isURLScalar(_ v: UInt32) -> Bool {
        switch v {
        case 0x00...0x1F, 0x20, 0x22, 0x27, 0x3C, 0x3E: return false
        default: return true
        }
    }

    /// `.,;:!?)'"`
    private static func isURLTrailing(_ v: UInt32) -> Bool {
        switch v {
        case 0x2E, 0x2C, 0x3B, 0x3A, 0x21, 0x3F, 0x29, 0x27, 0x22: return true
        default: return false
        }
    }
}
