import Foundation
import Testing
@testable import F33D3RKit

/// The shape of a F33D3R Number, held to the definition rather than to care.
///
/// There are now three implementations of one format: `manhattan/src/number.rs`,
/// which is authoritative, the Go mirror in `feed-engine`, and this one. Two
/// would eventually disagree; three certainly would. So the first test below is
/// not a property test at all — it is the same drift guard the Go suite uses,
/// run against a copy of the corpus the Rust definition generates from its own
/// behaviour.
///
/// `Fixtures/number.conformance` is that copy. Refresh it after any change to
/// the format:
///
///     UPDATE_NUMBER_CONFORMANCE=1 cargo test -p manhattan conformance
///     cp manhattan/number.conformance \
///        mobile/f33d3r_iOS/F33D3RKit/Tests/F33D3RKitTests/Fixtures/
///
/// The tests after it mirror the properties `zz_messages_number_test.go` pins,
/// because those are the ones a person actually feels: a typo caught before it
/// costs a contact attempt, a telephone number named as a mistake rather than
/// reported as a rejection.
@Suite("F33D3R Number")
struct F33NumberTests {

    /// Real Numbers, minted by Manhattan on a running stack and pinned by hand
    /// in the Go suite. The corpus proves the implementations agree; these
    /// prove they agree about what a real allocation looks like, which a
    /// generator on its own cannot.
    static let liveMinted = [
        "0098-7754-8744",
        "9470-7363-5478",
        "6871-6349-1446",
        "4603-4757-1321",
        "4812-1630-4537",
    ]

    // MARK: The drift guard

    @Test("Every case the Rust definition generated gets the same answer here")
    func agreesWithTheDefinitionCaseForCase() throws {
        var accepted = 0
        var refused = 0

        for line in try Self.corpus() {
            let parts = line.components(separatedBy: "\t")
            switch parts.first {
            case "OK":
                accepted += 1
                let number = F33Number(parts[1])
                #expect(number != nil, "the definition accepts \(parts[1]); this mirror refused it")
                #expect(number?.canonical == parts[2])
            case "NO":
                refused += 1
                #expect(
                    F33Number(parts[1]) == nil,
                    "the definition refuses \(parts[1]); this mirror accepted it"
                )
            default:
                Issue.record("unknown verdict in the corpus: \(line)")
            }
        }

        // A corpus that shrank to nothing would pass every assertion above
        // while proving nothing at all.
        #expect(accepted >= 400)
        #expect(refused >= 200)
    }

    // MARK: The properties a person feels

    @Test("Every form a person can type after being told a Number out loud")
    func acceptsEveryFormAPersonCanType() {
        for want in Self.liveMinted {
            let bare = want.replacingOccurrences(of: "-", with: "")
            let groups = (bare.prefix(4), bare.dropFirst(4).prefix(4), bare.suffix(4))
            for input in [
                want,
                bare,
                "\(groups.0) \(groups.1) \(groups.2)",
                "\(groups.0).\(groups.1).\(groups.2)",
                "  \(want)  ",
                "number:" + want,
                "\(bare.prefix(6))_\(bare.suffix(6))",
            ] {
                let number = F33Number(String(input))
                #expect(number?.canonical == want, "\(input) was not read as \(want)")
            }
        }
    }

    @Test("Confusable letters fold, so a Number written down by hand still resolves")
    func foldsConfusableSymbols() {
        // 0098-7754-8744 with its zeros written as the letter O, as somebody
        // copying twelve digits off a badge in a hurry writes them.
        for input in ["OO98-7754-8744", "oo98 7754 8744", "OO98775 48744"] {
            #expect(F33Number(input)?.canonical == "0098-7754-8744")
        }
        // A legacy Number carries both foldings at once.
        for input in ["O1AB-CDEF-GHJKG", "0IAB-CDEF-GHJKG", "OLAB CDEF GHJKG", "olabcdefghjkg"] {
            #expect(F33Number(input)?.canonical == "01AB-CDEF-GHJKG")
        }
    }

    @Test("What is not a Number at all is refused", arguments: [
        "",
        "366158592 80",
        "36615859280",
        "3661585928099",
        "@someone",
        "3661-5859-2808",
        "3661-5859-280X",
        "3661-5859-280é",
        String(repeating: "1", count: 200),
        "number:",
        "96M1-XPA3-345TT",
        "96M1-XPA3-345TU",
    ])
    func refusesWhatIsNotANumber(_ input: String) {
        #expect(F33Number(input) == nil)
    }

    /// A pasted telephone number must be named as a mistake, not refused as if
    /// the person it belongs to had turned the caller away. Keeping the `+`
    /// means an E.164 number can never be mistaken for a F33D3R Number — which
    /// it would be one time in ten if the `+` were stripped as punctuation.
    @Test("A pasted telephone number is refused", arguments: [
        "+441234567890", "+1 415 555 0132", "(415) 555-0132", "+12025550143",
    ])
    func refusesAPastedTelephoneNumber(_ input: String) {
        #expect(F33Number(input) == nil)
    }

    /// The check digit is what makes a transcription error cost arithmetic
    /// instead of one of the caller's few contact attempts. Every single-digit
    /// corruption, in every position, is caught on the device.
    @Test("Every single-digit typo is caught before anything is sent")
    func catchesEverySingleDigitTypo() {
        var checked = 0
        for valid in Self.liveMinted {
            var bare = Array(valid.replacingOccurrences(of: "-", with: ""))
            for position in bare.indices {
                let original = bare[position]
                for digit in "0123456789" where digit != original {
                    bare[position] = digit
                    #expect(F33Number(String(bare)) == nil, "a typo was accepted: \(String(bare))")
                    checked += 1
                }
                bare[position] = original
            }
        }
        #expect(checked == Self.liveMinted.count * F33Number.digitCount * 9)
    }

    /// Luhn's one boundary, measured as the Rust definition measures it: every
    /// adjacent transposition is caught except the pair 09/90, which Luhn is
    /// blind to.
    @Test("Every adjacent transposition outside Luhn's blind spot is caught")
    func catchesEveryAdjacentTransposition() {
        var checked = 0
        for valid in Self.liveMinted {
            var bare = Array(valid.replacingOccurrences(of: "-", with: ""))
            for i in 0..<(bare.count - 1) where bare[i] != bare[i + 1] {
                if Self.isZeroNinePair(bare[i], bare[i + 1]) { continue }
                bare.swapAt(i, i + 1)
                #expect(F33Number(String(bare)) == nil, "a transposition was accepted: \(String(bare))")
                bare.swapAt(i, i + 1)
                checked += 1
            }
        }
        #expect(checked >= 20)
    }

    /// The blind spot itself, asserted rather than left to luck. If it is ever
    /// *caught*, this has stopped being Luhn; if a second pair joins it, the
    /// property has widened silently.
    @Test("Luhn's blind spot is exactly the 0/9 pair and nothing else")
    func theBlindSpotIsExactlyTheZeroNinePair() {
        // Constructed to carry an adjacent 0 and 9, because a minted Number may
        // not. Doubling 0 and doubling 9 differ by exactly nine, which Luhn's
        // casting-out step erases.
        var bare = Array("409128837294")
        #expect(F33Number(String(bare)) != nil, "the constructed Number must itself be valid")

        var found = 0
        for i in 0..<(bare.count - 1) where Self.isZeroNinePair(bare[i], bare[i + 1]) {
            bare.swapAt(i, i + 1)
            #expect(F33Number(String(bare)) != nil, "09/90 is the blind spot; \(String(bare)) was caught")
            bare.swapAt(i, i + 1)
            found += 1
        }
        #expect(found > 0, "the constructed Number no longer carries an adjacent 0 and 9")
    }

    /// A Number somebody has already shared is a promise. The base32 Numbers
    /// minted before the digit format keep resolving, and their owner can be
    /// shown that theirs is an older one.
    @Test("Numbers minted before the digit format still resolve, and know they are older")
    func legacyNumbersStillResolve() {
        for legacy in ["96M1-XPA3-345TS", "EGCC-BPEY-K9KBP", "01AB-CDEF-GHJKG"] {
            let number = F33Number(legacy)
            #expect(number?.canonical == legacy)
            #expect(number?.isLegacy == true)
        }
        for current in Self.liveMinted {
            #expect(F33Number(current)?.isLegacy == false)
        }
    }

    // MARK: Reading one out

    @Test("The spoken form is the groups with spaces, which is how one is read aloud")
    func spokenFormIsGrouped() {
        #expect(F33Number("0098-7754-8744")?.spoken == "0098 7754 8744")
        #expect(F33Number("96M1-XPA3-345TS")?.spoken == "96M1 XPA3 345TS")
    }

    @Test("A partly-typed Number groups as it fills, so it can be checked against a screen")
    func groupsWhileTyping() {
        #expect(F33Number.grouping("") == "")
        #expect(F33Number.grouping("00") == "00")
        #expect(F33Number.grouping("0098") == "0098")
        #expect(F33Number.grouping("00987") == "0098 7")
        #expect(F33Number.grouping("009877548744") == "0098 7754 8744")
    }

    // MARK: Helpers

    private static func isZeroNinePair(_ a: Character, _ b: Character) -> Bool {
        (a == "0" && b == "9") || (a == "9" && b == "0")
    }

    private static func corpus() throws -> [String] {
        guard let url = Bundle.module.url(forResource: "Fixtures/number", withExtension: "conformance")
            ?? Bundle.module.url(forResource: "number", withExtension: "conformance", subdirectory: "Fixtures")
        else {
            Issue.record("Missing Fixtures/number.conformance — see the note at the top of this file")
            throw APIError.decoding("missing corpus")
        }
        return try String(contentsOf: url, encoding: .utf8)
            .components(separatedBy: "\n")
            .filter { !$0.isEmpty && !$0.hasPrefix("#") }
    }
}
