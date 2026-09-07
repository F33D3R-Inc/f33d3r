import Foundation
import Testing
@testable import F33D3RKit

/// The wire format of `POST /events` for a signed Work, checked against what
/// `workEvent` actually does with it.
///
/// The canonical-bytes tests prove the hashed half and the signature tests prove
/// the crypto. This is the join: an envelope whose payload bytes were altered
/// between hashing and sending would pass both of those and fail every post.
///
/// `Fixtures/malkuth_envelopes.json` records the verdicts a Go program returned
/// when handed bodies this code built. That program runs `workEvent`'s admission
/// sequence verbatim — the `rawBody` parse loop, the `sha256:` prefix check,
/// `verifyCID`, `verifyMalkuthSig` steps 2-6, the `workKinds` lookup, and the
/// reply and repost rules — stopping just before `InsertWork`, which is the last
/// point that does not need a database. The only substitution is the signing
/// key, which comes from the vector instead of from Elohim Veni.
///
/// So what is proven here is: **every check on the way in accepts these bytes,
/// and the ones that should refuse do refuse.** What is not proven is anything
/// past `InsertWork` — see the report.
struct MalkuthEnvelopeTests {

    struct VerdictFile: Decodable {
        let goVersion: String
        let results: [Verdict]
        enum CodingKeys: String, CodingKey {
            case goVersion = "go_version"
            case results
        }
    }

    struct Verdict: Decodable {
        let name: String
        let accepted: Bool
        let status: Int
        let reason: String
        let kind: String
        let parentCID: String
        let tusUploadID: String
        let isNSFW: Bool
        let quotedWorkID: String
        let expectAccepted: Bool

        enum CodingKeys: String, CodingKey {
            case name, accepted, status, reason, kind
            case parentCID = "parent_cid"
            case tusUploadID = "tus_upload_id"
            case isNSFW = "is_nsfw"
            case quotedWorkID = "quoted_work_id"
            case expectAccepted = "expect_accepted"
        }
    }

    /// The cases, defined once and used by both the export and the assertions,
    /// so what Go was asked about is exactly what is described here.
    struct EnvelopeCase {
        let name: String
        let expectAccepted: Bool
        let expectReason: String
        let build: (MalkuthKey) throws -> Data
    }

    static let pial = MalkuthCanonicalTests.pial
    static let parentCID = "sha256:" + String(repeating: "9a", count: 32)
    static let sourceUUID = "7c9e6679-7425-40de-944b-e07fc1f90ae7"

    static func cases() -> [EnvelopeCase] {
        func payload(
            _ mutate: (inout WorkPayload) -> Void = { _ in }
        ) -> WorkPayload {
            var p = WorkPayload(authorPIAL: pial, body: "hello", timestampMS: 1_767_225_600_000)
            mutate(&p)
            return p
        }

        return [
            .init(name: "plain_post", expectAccepted: true, expectReason: "would insert") { key in
                try WorkEnvelope(payload: payload(), key: key).httpBody
            },
            .init(name: "html_and_emoji_body", expectAccepted: true, expectReason: "would insert") { key in
                try WorkEnvelope(payload: payload { $0.body = "a & b < c > d \u{1F703} caf\u{E9}" }, key: key).httpBody
            },
            .init(name: "media_sorted_on_the_way_out", expectAccepted: true, expectReason: "would insert") { key in
                try WorkEnvelope(
                    payload: payload { $0.mediaURLs = ["/media/z.webp", "/media/A.webp", "/media/a.webp"] },
                    key: key
                ).httpBody
            },
            .init(name: "reply", expectAccepted: true, expectReason: "would insert") { key in
                try WorkEnvelope(
                    payload: payload { $0.kind = .reply; $0.parentCID = parentCID },
                    key: key
                ).httpBody
            },
            .init(name: "repost", expectAccepted: true, expectReason: "would insert") { key in
                try WorkEnvelope(
                    payload: payload { $0.isRepost = true; $0.repostSourceID = sourceUUID },
                    key: key
                ).httpBody
            },
            .init(name: "poll", expectAccepted: true, expectReason: "would insert") { key in
                try WorkEnvelope(
                    payload: payload {
                        $0.kind = .poll
                        $0.pollOptions = ["a", "b"]
                        $0.pollEndsAt = "2027-01-01T00:00:00Z"
                    },
                    key: key
                ).httpBody
            },
            .init(name: "video_with_unsigned_extras", expectAccepted: true, expectReason: "would insert") { key in
                try WorkEnvelope(
                    payload: payload {
                        $0.kind = .video
                        $0.videoMasterURL = "/media/video/master.m3u8"
                        $0.videoDurationSecs = 184
                    },
                    key: key,
                    isNSFW: true,
                    videoWatermarkedURL: "/media/video/wm.mp4",
                    videoWidth: 1920,
                    videoHeight: 1080,
                    tusUploadID: "tus-abc-123"
                ).httpBody
            },
            .init(name: "event_type_disagrees_with_kind", expectAccepted: true, expectReason: "would insert") { key in
                // The outer event_type is unsigned and says "post"; the signed
                // payload says "voice". The server must store the payload's.
                try WorkEnvelope(
                    payload: payload {
                        $0.kind = .voice
                        $0.voiceURL = "/media/voice/a.m4a"
                        $0.voiceDurationSecs = 12
                    },
                    key: key,
                    eventType: "post"
                ).httpBody
            },
            .init(name: "quote_with_quoted_work_id", expectAccepted: true, expectReason: "would insert") { key in
                try WorkEnvelope(
                    payload: payload { $0.kind = .quote },
                    key: key,
                    quotedWorkID: sourceUUID
                ).httpBody
            },

            // Negatives. Each one is a specific way the wire format can go
            // wrong, and each must be refused for the reason named.
            .init(name: "tampered_body_after_signing", expectAccepted: false, expectReason: "cid mismatch") { key in
                let envelope = try WorkEnvelope(payload: payload(), key: key)
                var text = String(decoding: envelope.httpBody, as: UTF8.self)
                text = text.replacingOccurrences(of: #""body":"hello""#, with: #""body":"HELLO""#)
                return Data(text.utf8)
            },
            .init(name: "media_sent_unsorted", expectAccepted: false, expectReason: "cid mismatch") { key in
                // What a client that skipped the sort would send: the CID is
                // over the unsorted order, so the server's re-sort disagrees.
                var p = payload()
                p.mediaURLs = ["/media/z.webp", "/media/a.webp"]
                let unsorted = CanonicalJSON.object(p.canonicalFields().map { field in
                    field.0 == "media_urls"
                        ? ("media_urls", .array(p.mediaURLs.map { .string($0) }))
                        : field
                })
                let cid = ContentID.of(unsorted)
                var body = CanonicalJSON.object([
                    ("event_type", .string("post")),
                    ("cid", .string(cid)),
                    ("signature", .string(try key.signature(forContentID: cid))),
                ])
                body.removeLast()
                body.append(contentsOf: Array(",\"payload\":".utf8))
                body.append(unsorted)
                body.append(UInt8(ascii: "}"))
                return body
            },
            .init(name: "signature_over_the_payload", expectAccepted: false, expectReason: "signature invalid") { key in
                let envelope = try WorkEnvelope(payload: payload(), key: key)
                // The classic mistake: sign the payload rather than the CID.
                let wrong = try key.signature(
                    forContentID: String(decoding: envelope.canonicalPayload, as: UTF8.self)
                )
                var text = String(decoding: envelope.httpBody, as: UTF8.self)
                text = text.replacingOccurrences(
                    of: "\"signature\":\"\(envelope.signature)\"",
                    with: "\"signature\":\"\(wrong)\""
                )
                return Data(text.utf8)
            },
            .init(name: "signed_by_another_key", expectAccepted: false, expectReason: "signature invalid") { key in
                let other = MalkuthKey.generate()
                let envelope = try WorkEnvelope(payload: payload(), key: other)
                _ = key
                return envelope.httpBody
            },
        ]
    }

    // MARK: The recorded verdicts

    static func verdicts() throws -> VerdictFile {
        try JSONDecoder().decode(
            VerdictFile.self, from: try ContractTests.fixture("malkuth_envelopes")
        )
    }

    @Test("Go admitted every envelope this client builds, and refused the ones it should")
    func goAdmittedTheseEnvelopes() throws {
        let file = try Self.verdicts()
        let byName = Dictionary(uniqueKeysWithValues: file.results.map { ($0.name, $0) })

        for testCase in Self.cases() {
            let verdict = try #require(byName[testCase.name], "no recorded verdict for \(testCase.name)")
            #expect(verdict.accepted == testCase.expectAccepted, "\(testCase.name): \(verdict.reason)")
            #expect(verdict.reason == testCase.expectReason, "\(testCase.name)")
            #expect(verdict.expectAccepted == testCase.expectAccepted,
                    "\(testCase.name): the recorded run asked a different question than this test does")
        }

        // A run where nothing was refused would be a run that proved nothing.
        #expect(file.results.contains { !$0.accepted })
    }

    @Test("The unsigned outer fields reach the server and stay outside the signature")
    func unsignedFieldsSurvive() throws {
        let verdict = try #require(
            try Self.verdicts().results.first { $0.name == "video_with_unsigned_extras" }
        )
        // Read back out of the payload object, where it rides without breaking
        // the CID — which is the whole trick.
        #expect(verdict.tusUploadID == "tus-abc-123")
        #expect(verdict.isNSFW)
        #expect(verdict.kind == "video")

        let quote = try #require(
            try Self.verdicts().results.first { $0.name == "quote_with_quoted_work_id" }
        )
        #expect(quote.quotedWorkID == Self.sourceUUID)
        #expect(quote.kind == "quote")
    }

    @Test("The kind Go stores comes from the signed payload, not from event_type")
    func kindComesFromThePayload() throws {
        let key = MalkuthKey.generate()
        var payload = WorkPayload(authorPIAL: Self.pial, body: "x", timestampMS: 1)
        payload.kind = .voice
        payload.voiceURL = "/media/voice/a.m4a"

        // event_type says one thing, the payload says another.
        let envelope = try WorkEnvelope(payload: payload, key: key, eventType: "post")
        let text = String(decoding: envelope.httpBody, as: UTF8.self)
        #expect(text.contains(#""event_type":"post""#))
        #expect(text.contains(#""kind":"voice""#))

        // The recorded Go run confirms which one wins: this case was sent with
        // event_type "post" and a payload kind of "voice".
        let verdict = try #require(
            try Self.verdicts().results.first { $0.name == "event_type_disagrees_with_kind" }
        )
        #expect(verdict.accepted)
        #expect(verdict.kind == "voice")
    }

    // MARK: Local structure

    @Test("The payload in the body is byte-identical to the payload that was hashed")
    func payloadIsSplicedNotReencoded() throws {
        let key = MalkuthKey.generate()
        var payload = WorkPayload(
            authorPIAL: Self.pial,
            body: "a & b < c > \"quoted\" \u{1F703}",
            timestampMS: 1_767_225_600_000
        )
        payload.mediaURLs = ["/z", "/a"]
        payload.tags = ["zeta", "alpha"]
        let envelope = try WorkEnvelope(payload: payload, key: key)

        let body = envelope.httpBody
        let marker = Array(#""payload":"#.utf8)
        let start = try #require(Self.range(of: marker, in: body)?.upperBound)
        let spliced = body[start..<(body.count - 1)]

        #expect(Data(spliced) == envelope.canonicalPayload)
        #expect(ContentID.of(Data(spliced)) == envelope.contentID)
    }

    @Test("tus_upload_id rides inside the payload without changing the CID")
    func tusUploadIDDoesNotChangeTheCID() throws {
        let key = MalkuthKey.generate()
        let payload = WorkPayload(authorPIAL: Self.pial, body: "v", timestampMS: 2)

        let without = try WorkEnvelope(payload: payload, key: key)
        let with = try WorkEnvelope(payload: payload, key: key, tusUploadID: "tus-1")

        #expect(without.contentID == with.contentID)
        #expect(String(decoding: with.httpBody, as: UTF8.self).contains(#""tus_upload_id":"tus-1""#))
        #expect(!String(decoding: without.httpBody, as: UTF8.self).contains("tus_upload_id"))

        // And the payload object is still valid JSON with the extra key.
        let parsed = try #require(
            try JSONSerialization.jsonObject(with: with.httpBody) as? [String: Any]
        )
        let payloadObject = try #require(parsed["payload"] as? [String: Any])
        #expect(payloadObject["tus_upload_id"] as? String == "tus-1")
        #expect(payloadObject.count == 20)  // the nineteen signed fields plus one
    }

    @Test("The body is valid JSON with every field the server reads")
    func bodyIsWellFormed() throws {
        let key = MalkuthKey.generate()
        let envelope = try WorkEnvelope(
            payload: WorkPayload(authorPIAL: Self.pial, body: "b", timestampMS: 3),
            key: key,
            isNSFW: true,
            quotedWorkID: Self.sourceUUID
        )
        let parsed = try #require(
            try JSONSerialization.jsonObject(with: envelope.httpBody) as? [String: Any]
        )
        #expect(parsed["event_type"] as? String == "post")
        #expect(parsed["cid"] as? String == envelope.contentID)
        #expect(parsed["signature"] as? String == envelope.signature)
        #expect(parsed["is_nsfw"] as? Bool == true)
        #expect(parsed["quoted_work_id"] as? String == Self.sourceUUID)
        #expect(parsed["payload"] is [String: Any])
        // Optional outer fields that were not set are absent rather than null,
        // which is what `json.Unmarshal` into the local variables expects.
        #expect(parsed["react_layout"] == nil)
        #expect(parsed["video_width"] == nil)
    }

    @Test("Identical payloads produce identical bytes, so a retry is the same request")
    func envelopeBytesAreStableAcrossBuilds() throws {
        let key = MalkuthKey.generate()
        let payload = WorkPayload(authorPIAL: Self.pial, body: "same", timestampMS: 42)
        let first = try WorkEnvelope(payload: payload, key: key)
        let second = try WorkEnvelope(payload: payload, key: key)

        // The CID is a hash of content, so it is identical...
        #expect(first.contentID == second.contentID)
        #expect(first.canonicalPayload == second.canonicalPayload)
        // ...and ECDSA is randomised, so the signatures are not. Which is
        // exactly why a retry must resend the *stored* envelope rather than
        // rebuild one: the CID would match either way, but rebuilding invites
        // re-stamping the timestamp, and that is what makes a second post.
        #expect(first.signature != second.signature)
    }

    // MARK: Export for the Go verifier

    @Test(
        "Export envelope bodies for the Go admission checker",
        .enabled(if: ProcessInfo.processInfo.environment["MALKUTH_EXPORT_DIR"] != nil)
    )
    func exportEnvelopesForGo() throws {
        let directory = try #require(ProcessInfo.processInfo.environment["MALKUTH_EXPORT_DIR"])
        let key = MalkuthKey.generate()

        var cases: [[String: Any]] = []
        for testCase in Self.cases() {
            cases.append([
                "name": testCase.name,
                "public_key_spki_b64": key.publicKeySPKIBase64,
                "body_b64": try testCase.build(key).base64EncodedString(),
                "expect_accepted": testCase.expectAccepted,
                "expect_reason": testCase.expectReason,
            ])
        }

        let url = URL(fileURLWithPath: directory).appendingPathComponent("envelopes_from_swift.json")
        try JSONSerialization
            .data(withJSONObject: ["cases": cases], options: [.prettyPrinted, .sortedKeys])
            .write(to: url)
        print("[malkuth] wrote \(cases.count) envelopes to \(url.path)")
    }

    // MARK: Helpers

    static func range(of needle: [UInt8], in haystack: Data) -> Range<Int>? {
        let bytes = Array(haystack)
        guard needle.count <= bytes.count else { return nil }
        for start in 0...(bytes.count - needle.count) where Array(bytes[start..<(start + needle.count)]) == needle {
            return start..<(start + needle.count)
        }
        return nil
    }
}
