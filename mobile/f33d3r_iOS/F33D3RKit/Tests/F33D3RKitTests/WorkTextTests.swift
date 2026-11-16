import Foundation
import Testing
@testable import F33D3RKit

/// ``WorkText`` against `Fixtures/worktext_golden.json`, which is written by
/// `Tools/malkuth-golden` from a verbatim copy of
/// `feed-engine/internal/worktext/worktext.go` (normalizer v1). Every input there
/// is already NFC, so the client's NFC step changes none of them and the Go
/// answers are the client's answers.
struct WorkTextTests {

    struct File: Decodable {
        let normalizerVersion: Int
        let maxWeightedLength: Int
        let cases: [Case]
        enum CodingKeys: String, CodingKey {
            case normalizerVersion = "normalizer_version"
            case maxWeightedLength = "max_weighted_length"
            case cases
        }
    }

    struct Case: Decodable {
        let name: String
        let input: String
        let normalized: String
        let isNormalized: Bool
        let isBlank: Bool
        let weightedLength: Int
        let runes: Int
        enum CodingKeys: String, CodingKey {
            case name, input, normalized, runes
            case isNormalized = "is_normalized"
            case isBlank = "is_blank"
            case weightedLength = "weighted_length"
        }
    }

    static func golden() throws -> File {
        try JSONDecoder().decode(File.self, from: try ContractTests.fixture("worktext_golden"))
    }

    @Test("The rule set is the server's")
    func versionAndLimit() throws {
        let file = try Self.golden()
        #expect(file.normalizerVersion == WorkText.normalizerVersion)
        #expect(file.maxWeightedLength == WorkText.maxWeightedLength)
    }

    @Test("normalize, isNormalized, isBlank and weightedLength agree with Go on every case")
    func agreesWithGo() throws {
        for c in try Self.golden().cases {
            let normalized = WorkText.normalize(c.input)
            #expect(normalized == c.normalized, "\(c.name): normalize")
            #expect(WorkText.isNormalized(c.input) == c.isNormalized, "\(c.name): isNormalized")
            #expect(WorkText.isBlank(normalized) == c.isBlank, "\(c.name): isBlank")
            #expect(WorkText.weightedLength(normalized) == c.weightedLength, "\(c.name): weightedLength")
            #expect(WorkText.scalarCount(normalized) == c.runes, "\(c.name): runes")
            // Normalising is idempotent: what is signed is already normalised.
            #expect(WorkText.isNormalized(normalized), "\(c.name): idempotent")
        }
    }

    @Test("A body is signed normalised, so a payload built from raw text validates once prepared")
    func preparedPayloadValidates() throws {
        let raw = WorkPayload(
            authorAccount: MalkuthCanonicalTests.account,
            body: "  hello\r\n\r\n\r\nworld  ",
            timestampMS: 1
        )
        #expect(throws: MalkuthError.textNotNormalized(field: "body")) { try raw.validate() }
        let prepared = raw.preparedForSigning(at: 1_767_225_600_000)
        #expect(prepared.body == "hello\n\nworld")
        #expect(prepared.timestampMS == 1_767_225_600_000)
        try prepared.validate()
    }
}
