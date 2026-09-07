import Foundation

/// A F33D3R Number: a contact address somebody can be reached at without
/// knowing their handle.
///
/// Twelve decimal digits, the twelfth a Luhn check digit over the other eleven,
/// written `0412-8837-2919` and read aloud "0412 8837 2919".
///
/// This is a mirror. The format is defined in `manhattan/src/number.rs` and
/// mirrored again in Go in `feed-engine/internal/handler/messages_number.go`,
/// and this type matches the Go character for character — the same separators,
/// the same folded letters, the same namespace prefix, the same refusal of a
/// leading `+`. ``F33NumberTests`` pins the properties the Go suite pins, so a
/// mirror that drifts by one digit or one rule fails here rather than at the
/// moment somebody's real Number stops resolving.
///
/// Checking the shape on the device is worth doing for one reason and not for
/// another. It is worth doing because a typo caught here costs arithmetic,
/// while a typo sent costs one of the caller's contact attempts, and those are
/// deliberately few. It is **not** a way to explain a refusal: once the server
/// has answered about a well-formed Number, the app knows nothing about why,
/// and saying otherwise would rebuild the enumeration oracle the server spends
/// a fixed delay refusing to be.
public struct F33Number: Hashable, Sendable, CustomStringConvertible {

    /// The canonical form: `0412-8837-2919`. What goes on the wire.
    public let canonical: String

    /// How many digits a Number is made of, which is also how many the keypad
    /// takes before it will let anyone press Enter.
    public static let digitCount = 12

    /// Builds a Number from anything a person might reasonably type after being
    /// told one out loud, and answers nil for anything that is not a Number at
    /// all.
    ///
    /// Accepted: spaces, hyphens, underscores and full stops between groups;
    /// any case; the letters Crockford folds (I and L for 1, O for 0); and the
    /// fully namespaced `number:0412-8837-2919`.
    public init?(_ input: String) {
        guard let canonical = Self.canonical(input) else { return nil }
        self.canonical = canonical
    }

    /// The form to say out loud, and the form the keypad shows: groups
    /// separated by spaces rather than hyphens.
    public var spoken: String {
        canonical.replacingOccurrences(of: "-", with: " ")
    }

    public var description: String { canonical }

    /// Whether this is one of the base32 Numbers minted before the digit
    /// format.
    ///
    /// Owner-facing only, exactly as in the Go mirror: it exists so somebody
    /// holding an older Number can be told it is older and offered a new one.
    /// Nothing that resolves a Number may ask this, and nothing here does.
    public var isLegacy: Bool {
        canonical.utf8.count(where: { !Self.isSeparator($0) }) == Self.legacyTotalLength
    }

    /// Whether `input` is a Number, without building one.
    public static func isWellFormed(_ input: String) -> Bool {
        canonical(input) != nil
    }

    /// A partly-typed Number, grouped for reading: `0412 88`.
    ///
    /// Grouped as it fills rather than at the end, because twelve unbroken
    /// digits cannot be checked against a Number somebody is reading off a
    /// screen, which is what the person at the keypad is doing.
    public static func grouping(_ digits: String) -> String {
        var out = ""
        for (index, digit) in digits.enumerated() {
            if index == 4 || index == 8 { out.append(" ") }
            out.append(digit)
        }
        return out
    }

    // MARK: The format

    // Crockford base32, for Numbers minted before the digit format. The five
    // check-only symbols are never valid inside a presented Number: a legacy
    // Number containing U is malformed, not unknown.
    private static let legacyAlphabet = Array("0123456789ABCDEFGHJKMNPQRSTVWXYZ".utf8)
    private static let legacyCheckAlphabet = Array("0123456789ABCDEFGHJKMNPQRSTVWXYZ*~$=U".utf8)
    private static let legacyPayloadLength = 12
    private static let legacyTotalLength = 13
    private static let namespace = "number"
    // Twelve digits, and separators and a prefix cannot plausibly quintuple
    // that. Anything longer is refused without being scanned.
    private static let inputMaxBytes = 64

    /// Folds, strips and checks caller input, answering the canonical form.
    ///
    /// Works over UTF-8 bytes rather than characters so that a multi-byte
    /// character can never land mid-symbol — its bytes simply fail the alphabet
    /// check — which is what the Go mirror does and why the two agree about
    /// `3661-5859-280é`.
    private static func canonical(_ input: String) -> String? {
        let body = input.trimmingCharacters(in: .whitespacesAndNewlines)
        var bytes = Array(body.utf8)
        guard bytes.count <= inputMaxBytes else { return nil }

        if bytes.count > namespace.utf8.count,
           bytes[namespace.utf8.count] == UInt8(ascii: ":"),
           zip(bytes, namespace.utf8).allSatisfy({ uppercased($0) == uppercased($1) }) {
            bytes.removeFirst(namespace.utf8.count + 1)
        }

        var symbols: [UInt8] = []
        symbols.reserveCapacity(legacyTotalLength)
        for byte in bytes where !isSeparator(byte) {
            symbols.append(fold(byte))
        }

        switch symbols.count {
        case digitCount:
            guard let sum = luhnSum(symbols), sum % 10 == 0 else { return nil }
            return group(symbols)
        case legacyTotalLength:
            guard symbols.allSatisfy({ legacyAlphabet.contains($0) }),
                  let expected = legacyCheckValue(symbols.prefix(legacyPayloadLength)),
                  legacyCheckAlphabet[expected] == symbols[legacyPayloadLength]
            else { return nil }
            return group(symbols)
        default:
            return nil
        }
    }

    /// Crockford's confusable folding: I and L read as 1, O reads as 0.
    ///
    /// A Number heard aloud and written down by hand has to resolve, and a
    /// twelve-digit Number containing an O is a 0 somebody drew badly — not a
    /// different Number and not an attack. The folded result is still digits.
    private static func fold(_ byte: UInt8) -> UInt8 {
        switch uppercased(byte) {
        case UInt8(ascii: "I"), UInt8(ascii: "L"): return UInt8(ascii: "1")
        case UInt8(ascii: "O"): return UInt8(ascii: "0")
        case let upper: return upper
        }
    }

    private static func uppercased(_ byte: UInt8) -> UInt8 {
        (byte >= UInt8(ascii: "a") && byte <= UInt8(ascii: "z"))
            ? byte - (UInt8(ascii: "a") - UInt8(ascii: "A"))
            : byte
    }

    /// Whether a byte is punctuation between groups.
    ///
    /// Spaces and hyphens are what a Number is printed with; underscores and
    /// full stops are what people actually type. None of them carry meaning.
    ///
    /// A leading `+` is deliberately not one. It is what a pasted telephone
    /// number begins with, and stripping it would turn one into a well-formed
    /// F33D3R Number about one time in ten — so the person would be told their
    /// contact was unreachable instead of that they pasted the wrong thing.
    private static func isSeparator(_ byte: UInt8) -> Bool {
        byte == UInt8(ascii: "-") || byte == UInt8(ascii: " ")
            || byte == UInt8(ascii: "_") || byte == UInt8(ascii: ".")
    }

    /// Folds a run of ASCII digits right to left, doubling every second one and
    /// casting out nines from the doubled value. Nil if any byte is not a digit.
    ///
    /// The doubling position is derived from the distance to the right-hand end
    /// rather than from the length, which is the classic way a hand-written
    /// Luhn drifts when a format's length changes.
    private static func luhnSum(_ digits: [UInt8]) -> Int? {
        var sum = 0
        for (index, byte) in digits.enumerated() {
            guard byte >= UInt8(ascii: "0"), byte <= UInt8(ascii: "9") else { return nil }
            var value = Int(byte - UInt8(ascii: "0"))
            if (digits.count - 1 - index) % 2 == 1 {
                value *= 2
                if value > 9 { value -= 9 }
            }
            sum += value
        }
        return sum
    }

    /// The mod-37 check value of a legacy payload. Iterative, so no wide
    /// integer arithmetic is needed.
    private static func legacyCheckValue(_ payload: ArraySlice<UInt8>) -> Int? {
        var accumulator = 0
        for byte in payload {
            guard let value = legacyAlphabet.firstIndex(of: byte) else { return nil }
            accumulator = (accumulator * 32 + value) % 37
        }
        return accumulator
    }

    /// `XXXX-XXXX-…`: 4-4-4 for a Number, 4-4-5 for a legacy one.
    private static func group(_ symbols: [UInt8]) -> String {
        let text = String(decoding: symbols, as: UTF8.self)
        let first = text.index(text.startIndex, offsetBy: 4)
        let second = text.index(text.startIndex, offsetBy: 8)
        return text[..<first] + "-" + text[first..<second] + "-" + text[second...]
    }
}
