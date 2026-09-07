import Foundation
import Testing
@testable import F33D3RKit

/// The things that stop a bad post before a request is spent, and the things
/// that stop a good post becoming two.
@Suite(.serialized)
struct MalkuthClientTests {

    static let pial = MalkuthCanonicalTests.pial
    static let uuid = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
    static let cid = "sha256:" + String(repeating: "ab", count: 32)

    static func payload(_ mutate: (inout WorkPayload) -> Void = { _ in }) -> WorkPayload {
        var p = WorkPayload(authorPIAL: pial, body: "hello", timestampMS: 1_767_225_600_000)
        mutate(&p)
        return p
    }

    static func makeClient(token: String? = "tok", baseURL: String = "https://f33d3r.com")
        -> (APIClient, StubTokens)
    {
        MalkuthMockURLProtocol.reset()
        let tokens = StubTokens(token: token)
        return (
            APIClient(
                baseURL: URL(string: baseURL)!,
                tokens: tokens,
                session: MalkuthMockURLProtocol.makeSession()
            ),
            tokens
        )
    }

    // MARK: - Validation

    /// Signing with an empty `author_pial` is the failure that would be silent:
    /// `verifyCID` recomputes the hash of whatever was sent and passes, and
    /// `verifyMalkuthSig` looks up the key by the *session's* PIAL and passes
    /// too. The work is stored, permanently, with an author field naming nobody.
    @Test("Signing without a PIAL is refused rather than attempted")
    func refusesToSignWithoutPIAL() throws {
        let key = MalkuthKey.generate()
        for absent in ["", "   "] {
            #expect(throws: MalkuthError.pialUnknown) {
                try WorkEnvelope(payload: Self.payload { $0.authorPIAL = absent }, key: key)
            }
        }

        let identity = AuthenticatedIdentity(user: try Self.sampleUser(), pialID: nil)
        #expect(!identity.canSign)
        #expect(throws: MalkuthError.pialUnknown) { try identity.requirePIAL() }
    }

    @Test("A reply with no parent is refused here, not by a round trip")
    func refusesReplyWithoutParent() throws {
        let key = MalkuthKey.generate()
        #expect(throws: MalkuthError.replyMissingParentCID) {
            try WorkEnvelope(payload: Self.payload { $0.kind = .reply }, key: key)
        }
        // With a parent it is fine.
        #expect(throws: Never.self) {
            try WorkEnvelope(
                payload: Self.payload { $0.kind = .reply; $0.parentCID = Self.cid }, key: key
            )
        }
    }

    @Test("A repost with no source is refused")
    func refusesRepostWithoutSource() throws {
        let key = MalkuthKey.generate()
        #expect(throws: MalkuthError.repostMissingSourceID) {
            try WorkEnvelope(payload: Self.payload { $0.isRepost = true }, key: key)
        }
    }

    /// The server never checks `parent_cid`'s shape. A malformed one matches no
    /// row, so the reply is stored as a top-level work — accepted, wrong, and
    /// silent. This is the only place that can catch it.
    @Test("A malformed parent CID is caught, because the server would accept it")
    func rejectsMalformedParentCID() throws {
        let key = MalkuthKey.generate()
        for bad in [
            "not-a-cid",
            "sha256:short",
            "sha256:" + String(repeating: "AB", count: 32),   // upper case: %x never emits it
            String(repeating: "a", count: 64),                 // no prefix
        ] {
            #expect(throws: MalkuthError.malformedCID(field: "parent_cid", value: bad)) {
                try WorkEnvelope(
                    payload: Self.payload { $0.kind = .reply; $0.parentCID = bad }, key: key
                )
            }
        }
    }

    @Test("A repost source that is not a UUID is caught, because ::uuid would 500")
    func rejectsMalformedRepostSource() throws {
        let key = MalkuthKey.generate()
        #expect(throws: MalkuthError.malformedUUID(field: "repost_source_id", value: "nope")) {
            try WorkEnvelope(
                payload: Self.payload { $0.isRepost = true; $0.repostSourceID = "nope" }, key: key
            )
        }
    }

    /// Go parses these with `if err == nil`. An unparseable `scheduled_at` is
    /// therefore not an error — it is a NULL, and a work the author scheduled
    /// for next week publishes immediately.
    @Test("A timestamp Go cannot parse is refused, because Go would silently drop it")
    func rejectsMalformedTimestamps() throws {
        let key = MalkuthKey.generate()
        let bad = [
            "2026-12-25",                    // date only
            "2026-12-25 09:00:00Z",          // space instead of T
            "2026-12-25T09:00:00",           // no zone
            "Fri, 25 Dec 2026 09:00:00 GMT", // RFC 1123
            "",
        ]
        for value in bad {
            #expect(throws: MalkuthError.malformedTimestamp(field: "scheduled_at", value: value)) {
                try WorkEnvelope(payload: Self.payload { $0.scheduledAt = value }, key: key)
            }
        }

        let good = [
            "2026-12-25T09:00:00Z",
            "2026-12-25T09:00:00+02:00",
            "2026-12-25T09:00:00-05:00",
            "2026-12-25T09:00:00.123Z",
        ]
        for value in good {
            #expect(throws: Never.self) {
                try WorkEnvelope(payload: Self.payload { $0.scheduledAt = value }, key: key)
            }
        }
    }

    @Test("rfc3339String formats what the validator and Go both accept")
    func formatsTimestampsGoCanRead() throws {
        let formatted = WorkPayload.rfc3339String(Date(timeIntervalSince1970: 1_767_225_600))
        #expect(formatted == "2026-01-01T00:00:00Z")
        #expect(throws: Never.self) {
            try WorkEnvelope(payload: Self.payload { $0.pollEndsAt = formatted },
                             key: MalkuthKey.generate())
        }
    }

    /// Postgres `TEXT` rejects NUL outright, so this reaches the author as
    /// `500 server error` with nothing to act on.
    @Test("A NUL byte is caught here rather than as a 500 from Postgres")
    func rejectsNULBytes() throws {
        let key = MalkuthKey.generate()
        #expect(throws: MalkuthError.nulByteInText(field: "body")) {
            try WorkEnvelope(payload: Self.payload { $0.body = "a\u{0}b" }, key: key)
        }
        #expect(throws: MalkuthError.nulByteInText(field: "tags")) {
            try WorkEnvelope(payload: Self.payload { $0.tags = ["ok", "b\u{0}d"] }, key: key)
        }
    }

    @Test("A duration past float64's exact range is refused")
    func rejectsUnrepresentableDurations() throws {
        let key = MalkuthKey.generate()
        // Go carries these through interface{}, so they round-trip via float64.
        #expect(throws: MalkuthError.durationOutOfRange(field: "voice_duration_secs", value: 1 << 53)) {
            try WorkEnvelope(
                payload: Self.payload { $0.kind = .voice; $0.voiceDurationSecs = 1 << 53 }, key: key
            )
        }
        #expect(throws: MalkuthError.durationOutOfRange(field: "video_duration_secs", value: -1)) {
            try WorkEnvelope(payload: Self.payload { $0.videoDurationSecs = -1 }, key: key)
        }
        // Zero is a real value, not an absence — the JS `|| null` coercion loses
        // this and the Swift client keeps it.
        #expect(throws: Never.self) {
            try WorkEnvelope(
                payload: Self.payload { $0.kind = .voice; $0.voiceDurationSecs = 0 }, key: key
            )
        }
    }

    @Test("An empty media URL is refused")
    func rejectsEmptyMediaURL() throws {
        #expect(throws: MalkuthError.emptyMediaURL) {
            try WorkEnvelope(
                payload: Self.payload { $0.mediaURLs = ["/media/a.webp", ""] },
                key: MalkuthKey.generate()
            )
        }
    }

    /// The kind vocabulary is closed by the type system, so the round trip that
    /// ends in `400 unknown work kind` cannot happen.
    @Test("The kind vocabulary matches the server's, exactly")
    func mirrorsTheServerKindVocabulary() {
        #expect(Set(WorkKind.allCases.map(\.rawValue)) == [
            "post", "reply", "quote", "poll", "video", "voice", "thread_post", "react_video",
        ])
        // `vision` moved to the visions table and is not a Work.
        #expect(WorkKind(rawValue: "vision") == nil)
    }

    // MARK: - Idempotency

    /// The whole of idempotency here: the CID is a content hash and `works.cid`
    /// is UNIQUE, so re-sending identical bytes cannot make a second row. What
    /// makes a second row is re-*signing*, because that re-stamps
    /// `timestamp_ms`.
    @Test("Re-sending the same submission cannot post twice; re-signing would")
    func cidIsTheIdempotencyKey() async throws {
        let signer = MalkuthSigner(store: InMemoryMalkuthKeyStore())
        let payload = Self.payload()

        let first = try await WorkSubmission.prepare(payload, signer: signer)
        let second = try await WorkSubmission.prepare(payload, signer: signer)
        // Same payload, same clock reading, therefore same CID: the second send
        // is refused by the unique index, not accepted as a new work.
        #expect(first.contentID == second.contentID)

        // Re-stamping the clock is what makes a different work. This is why
        // WorkSubmission holds the envelope instead of the payload.
        var later = payload
        later.timestampMS += 1
        let third = try await WorkSubmission.prepare(later, signer: signer)
        #expect(third.contentID != first.contentID)
    }

    @Test("A timed-out post is retried with the identical bytes, not rebuilt")
    func retriesTheSameBytes() async throws {
        let (client, _) = Self.makeClient()
        let signer = MalkuthSigner(store: InMemoryMalkuthKeyStore())
        let submission = try await WorkSubmission.prepare(Self.payload(), signer: signer)

        let bodies = Mutex<[Data]>([])
        MalkuthMockURLProtocol.handler = { request in
            bodies.withLock { $0.append(request.bodyData ?? Data()) }
            return bodies.withLock { $0.count } < 3
                ? (500, Data("server error\n".utf8))
                : (201, Data(#"{"work_id":"w-1","cid":"x"}"#.utf8))
        }

        let outcome = await submission.sendWithRetries(
            using: client, attempts: 3, backoff: { _ in .zero }
        )
        guard case .accepted(let accepted) = outcome else {
            Issue.record("expected acceptance, got \(outcome)")
            return
        }
        #expect(accepted.workID == "w-1")

        let sent = bodies.withLock { $0 }
        #expect(sent.count == 3)
        // Every attempt is byte-identical, so at most one of them can have
        // created a row.
        #expect(Set(sent).count == 1)
        #expect(sent[0] == submission.envelope.httpBody)
    }

    /// `500` is what a *duplicate insert* answers as well as what a genuine
    /// fault answers — `InsertWork` returns the unique violation and `workEvent`
    /// turns it into `server error`. Reporting that as a refusal would be a lie
    /// in one direction and reporting it as success a lie in the other.
    @Test("An exhausted retry is uncertain, never a claim either way")
    func reportsUncertaintyHonestly() async throws {
        let (client, _) = Self.makeClient()
        let signer = MalkuthSigner(store: InMemoryMalkuthKeyStore())
        let submission = try await WorkSubmission.prepare(Self.payload(), signer: signer)

        MalkuthMockURLProtocol.handler = { _ in (500, Data("server error\n".utf8)) }
        let outcome = await submission.sendWithRetries(
            using: client, attempts: 2, backoff: { _ in .zero }
        )
        guard case .uncertain(let cid, _) = outcome else {
            Issue.record("expected uncertainty, got \(outcome)")
            return
        }
        // The CID travels with the uncertainty, because it is the question a
        // read would need to answer: "is there a work with this content id?"
        #expect(cid == submission.contentID)
    }

    @Test("A deterministic refusal is not retried")
    func doesNotRetryDeterministicRefusals() async throws {
        let (client, _) = Self.makeClient()
        let signer = MalkuthSigner(store: InMemoryMalkuthKeyStore())
        let submission = try await WorkSubmission.prepare(Self.payload(), signer: signer)

        let attempts = Mutex<Int>(0)
        MalkuthMockURLProtocol.handler = { _ in
            attempts.withLock { $0 += 1 }
            return (400, Data("cid mismatch\n".utf8))
        }

        let outcome = await submission.sendWithRetries(
            using: client, attempts: 5, backoff: { _ in .zero }
        )
        guard case .rejected(let error) = outcome else {
            Issue.record("expected refusal, got \(outcome)")
            return
        }
        // The server's own words survive: `cid mismatch` is the entire
        // diagnostic the write path emits for a canonical-encoding bug.
        #expect(error == .rejected(status: 400, reason: "cid mismatch"))
        #expect(attempts.withLock { $0 } == 1)
    }

    // MARK: - Routing and headers

    @Test("Writes go to the origin, not under /api/v1 which has no write surface")
    func writesResolveAtTheOrigin() async throws {
        let (client, _) = Self.makeClient()
        MalkuthMockURLProtocol.handler = { _ in (204, Data()) }

        try await client.react(.like, workID: Self.uuid)
        #expect(MalkuthMockURLProtocol.recorded.first?.url?.path == "/events")

        MalkuthMockURLProtocol.reset()
        MalkuthMockURLProtocol.handler = { _ in (200, Data(#"{"ok":true}"#.utf8)) }
        try? await client.registerSigningKey(publicKeySPKIBase64: "AAA")
        #expect(MalkuthMockURLProtocol.recorded.first?.url?.path == "/api/pial/signing-key/register")
    }

    @Test("Reads still resolve under /api/v1")
    func readsStillResolveUnderAPIV1() async throws {
        let (client, _) = Self.makeClient()
        MalkuthMockURLProtocol.handler = { _ in (200, Data("{}".utf8)) }
        _ = try? await client.send(.me, body: Optional<Never>.none, as: [String: String].self)
        #expect(MalkuthMockURLProtocol.recorded.first?.url?.path == "/api/v1/me")
    }

    /// Without this every write against a real deployment is `403 Forbidden`
    /// with no explanation: `middleware.CSRF` allows an Origin-less mutating
    /// request only when the Host is localhost.
    @Test("Mutating requests state their origin so the CSRF middleware admits them")
    func sendsOriginOnWrites() async throws {
        let (client, _) = Self.makeClient()
        MalkuthMockURLProtocol.handler = { _ in (204, Data()) }

        try await client.react(.like, workID: Self.uuid)
        let request = try #require(MalkuthMockURLProtocol.recorded.first)
        #expect(request.value(forHTTPHeaderField: "Origin") == "https://f33d3r.com")

        // A non-default port is part of the origin: `sameOriginHost` compares
        // against `r.Host`, which carries the port.
        MalkuthMockURLProtocol.reset()
        let (local, _) = Self.makeClient(baseURL: "http://127.0.0.1:8081")
        MalkuthMockURLProtocol.handler = { _ in (204, Data()) }
        try await local.react(.like, workID: Self.uuid)
        #expect(MalkuthMockURLProtocol.recorded.first?.value(forHTTPHeaderField: "Origin")
                == "http://127.0.0.1:8081")
    }

    @Test("Reads do not send an Origin — the middleware does not ask for one")
    func doesNotSendOriginOnReads() async throws {
        let (client, _) = Self.makeClient()
        MalkuthMockURLProtocol.handler = { _ in (200, Data("{}".utf8)) }
        _ = try? await client.send(.me, body: Optional<Never>.none, as: [String: String].self)
        #expect(MalkuthMockURLProtocol.recorded.first?.value(forHTTPHeaderField: "Origin") == nil)
    }

    // MARK: - Event bodies

    @Test("Reactions use the work_* vocabulary the server actually has")
    func usesTheWorkReactionVocabulary() async throws {
        // `like` is not an event_type this server knows: eventsPost has no such
        // case and answers 404 `unknown event_type: like`.
        #expect(APIClient.WorkReaction.like.rawValue == "work_like")
        #expect(APIClient.WorkReaction.bookmark.rawValue == "work_bookmark")
        for reaction in APIClient.WorkReaction.allCases {
            #expect(reaction.rawValue.hasPrefix("work_"))
            #expect(reaction.inverse.inverse == reaction)
        }

        let (client, _) = Self.makeClient()
        MalkuthMockURLProtocol.handler = { _ in (204, Data()) }
        try await client.react(.bookmark, workID: Self.uuid)

        let body = try #require(MalkuthMockURLProtocol.recorded.first?.bodyData)
        #expect(String(decoding: body, as: UTF8.self)
                == #"{"event_type":"work_bookmark","work_id":"\#(Self.uuid)"}"#)
    }

    /// The lane is Visions on both sides of the wire: the app's word, the
    /// server's routes, its event types and its JSON keys. "Fleet" was Twitter's
    /// word for it and appears nowhere. This test is the whole vocabulary, so a
    /// half-finished rename on either side fails here rather than at runtime.
    @Test("The Visions lane says visions on the wire, never fleet")
    func visionsSpeakTheServersVocabulary() async throws {
        #expect(Endpoint.visions.path == "visions")
        #expect(Endpoint.visionViewers(id: Self.uuid).path == "visions/\(Self.uuid)/viewers")

        let (client, _) = Self.makeClient()
        MalkuthMockURLProtocol.handler = { _ in (204, Data()) }
        try await client.markVisionSeen(id: Self.uuid)
        let body = String(decoding: try #require(MalkuthMockURLProtocol.recorded.first?.bodyData), as: UTF8.self)
        #expect(body == #"{"event_type":"vision_seen","vision_id":"\#(Self.uuid)"}"#)

        // Every event this lane sends, so none is missed by a later rename.
        for (send, expected) in [
            ("vision_delete", { try await client.deleteVision(id: Self.uuid) }),
            ("vision_poll_vote", { try await client.voteVisionPoll(id: Self.uuid, optionIndex: 0) }),
        ] as [(String, () async throws -> Void)] {
            MalkuthMockURLProtocol.reset()
            MalkuthMockURLProtocol.handler = { _ in (204, Data()) }
            try await expected()
            let sent = String(decoding: try #require(MalkuthMockURLProtocol.recorded.first?.bodyData), as: UTF8.self)
            #expect(sent.contains(#""event_type":"\#(send)""#))
            #expect(sent.contains(#""vision_id""#))
        }

        // A ring carries its own posts under the key the lane is named for.
        let ring = try APIClient.makeDecoder().decode(VisionRing.self, from: Data(#"""
        {"author":{"handle":"dev","display_name":"Dev","role":"user","realm":1,
                   "is_verified":false,"is_creator":false},
         "state":"unseen","count":1,"unseen_count":1,"is_live":false,
         "latest_at":"2026-09-05T12:00:00Z","visions":[]}
        """#.utf8))
        #expect(ring.visions.isEmpty)
        #expect(ring.unseenCount == 1)
    }

    /// `followEvent` reads `target_handle` through `r.FormValue` and never looks
    /// at a JSON body, so JSON here is answered `400 target_pial or
    /// target_handle required` — a failure that looks like a missing field
    /// rather than a wrong encoding.
    @Test("Form-only handlers are sent form bodies")
    func usesFormEncodingWhereTheHandlerDemandsIt() async throws {
        let (client, _) = Self.makeClient()
        MalkuthMockURLProtocol.handler = { _ in (200, Data("<button>Following</button>".utf8)) }

        try await client.setFollowing(true, handle: "@miiyazuko")
        let request = try #require(MalkuthMockURLProtocol.recorded.first)
        #expect(request.value(forHTTPHeaderField: "Content-Type")
                == "application/x-www-form-urlencoded")
        let body = String(decoding: try #require(request.bodyData), as: UTF8.self)
        #expect(body.contains("event_type=follow"))
        #expect(body.contains("target_handle=miiyazuko"))  // the @ is stripped
        // A PIAL never travels as an address from this client.
        #expect(!body.contains("target_pial"))
    }

    @Test("A tip states its amount in the hundredths the server parses")
    func sendsTipAsForm() async throws {
        let (client, _) = Self.makeClient()
        MalkuthMockURLProtocol.handler = { _ in (200, Data()) }
        try await client.tip(handle: "dev", amountAETHundredths: 250)
        let body = String(decoding: try #require(MalkuthMockURLProtocol.recorded.first?.bodyData), as: UTF8.self)
        #expect(body.contains("amount_aet=250"))
        #expect(body.contains("event_type=tip"))
    }

    @Test("The key authority being down is its own error, not a generic failure")
    func namesTheKeyAuthorityOutage() async throws {
        let (client, _) = Self.makeClient()
        MalkuthMockURLProtocol.handler = { _ in (502, Data(#"{"error":"key_authority_unavailable"}"#.utf8)) }

        do {
            try await client.registerSigningKey(publicKeySPKIBase64: "AAA")
            Issue.record("expected a throw")
        } catch let error as MalkuthError {
            #expect(error == .keyAuthorityUnavailable)
            #expect(error.isKeyAuthorityUnavailable)
        }
    }

    @Test("A signing key is generated once and reused")
    func reusesTheStoredKey() async throws {
        let store = InMemoryMalkuthKeyStore()
        let first = try await MalkuthSigner(store: store).key()
        let second = try await MalkuthSigner(store: store).key()
        #expect(first.publicKeySPKIBase64 == second.publicKeySPKIBase64)

        try await MalkuthSigner(store: store).destroyKey()
        let third = try await MalkuthSigner(store: store).key()
        #expect(third.publicKeySPKIBase64 != first.publicKeySPKIBase64)
    }

    @Test("Registration happens once a launch unless forced")
    func registersOncePerLaunch() async throws {
        let (client, _) = Self.makeClient()
        let calls = Mutex<Int>(0)
        MalkuthMockURLProtocol.handler = { _ in
            calls.withLock { $0 += 1 }
            return (200, Data(#"{"ok":true}"#.utf8))
        }

        let signer = MalkuthSigner(store: InMemoryMalkuthKeyStore())
        #expect(try await signer.registerIfNeeded(with: client))
        #expect(!(try await signer.registerIfNeeded(with: client)))
        #expect(calls.withLock { $0 } == 1)

        // A 403 from a post means another device may have registered over this
        // key; forcing is the fix.
        #expect(try await signer.registerIfNeeded(with: client, force: true))
        #expect(calls.withLock { $0 } == 2)
    }

    @Test("The registration body is what pial_keys.go decodes")
    func sendsTheRegistrationShape() async throws {
        let (client, _) = Self.makeClient()
        MalkuthMockURLProtocol.handler = { _ in (200, Data(#"{"ok":true}"#.utf8)) }
        let key = MalkuthKey.generate()
        try await client.registerSigningKey(publicKeySPKIBase64: key.publicKeySPKIBase64)

        let body = String(decoding: try #require(MalkuthMockURLProtocol.recorded.first?.bodyData), as: UTF8.self)
        #expect(body == #"{"public_key_b64":"\#(key.publicKeySPKIBase64)","algorithm":"ECDSA-P256"}"#)
        // Padded standard base64, which is what `base64.StdEncoding` reads.
        #expect(!key.publicKeySPKIBase64.contains("-"))
        #expect(!key.publicKeySPKIBase64.contains("_"))
    }

    // MARK: - Identity

    @Test("meIdentity reads the PIAL when the server sends one, and nil when it does not")
    func readsPIALFromMe() async throws {
        let (client, _) = Self.makeClient()
        let base = try ContractTests.fixture("me")

        // Today's MeDTO, with no pial_id.
        MalkuthMockURLProtocol.handler = { _ in (200, base) }
        let without = try await client.meIdentity()
        #expect(without.pialID == nil)
        #expect(!without.canSign)

        // With pial_id, as the DTO is about to carry it.
        var object = try #require(try JSONSerialization.jsonObject(with: base) as? [String: Any])
        object["pial_id"] = Self.pial
        let withPIAL = try JSONSerialization.data(withJSONObject: object)
        MalkuthMockURLProtocol.reset()
        MalkuthMockURLProtocol.handler = { _ in (200, withPIAL) }
        let identity = try await client.meIdentity()
        #expect(identity.pialID == Self.pial)
        #expect(identity.canSign)
        #expect(try identity.requirePIAL() == Self.pial)

        // One request per call, not two.
        #expect(MalkuthMockURLProtocol.recorded.count == 1)
    }

    @Test("An empty pial_id is treated as absent, not as a PIAL")
    func treatsEmptyPIALAsAbsent() async throws {
        let (client, _) = Self.makeClient()
        var object = try #require(
            try JSONSerialization.jsonObject(with: try ContractTests.fixture("me")) as? [String: Any]
        )
        object["pial_id"] = ""
        let data = try JSONSerialization.data(withJSONObject: object)
        MalkuthMockURLProtocol.handler = { _ in (200, data) }
        #expect(try await client.meIdentity().pialID == nil)
    }

    // MARK: Helpers

    static func sampleUser() throws -> CurrentUser {
        try ContractTests.makeDecoder().decode(CurrentUser.self, from: try ContractTests.fixture("me"))
    }
}

/// `URLProtocol` moves a body to `httpBodyStream` when the request is built from
/// `httpBody`, so a test that reads `httpBody` back sees nil. This reads
/// whichever one is there.
extension URLRequest {
    var bodyData: Data? {
        if let httpBody { return httpBody }
        guard let stream = httpBodyStream else { return nil }
        stream.open()
        defer { stream.close() }
        var data = Data()
        let size = 4096
        let buffer = UnsafeMutablePointer<UInt8>.allocate(capacity: size)
        defer { buffer.deallocate() }
        while stream.hasBytesAvailable {
            let read = stream.read(buffer, maxLength: size)
            if read <= 0 { break }
            data.append(buffer, count: read)
        }
        return data
    }
}

/// A request interceptor private to this suite.
///
/// `MockURLProtocol` in `APIClientTests` holds its scripted handler and its
/// record of requests in statics, and Swift Testing runs suites in parallel:
/// two suites sharing those statics answer each other's requests. `.serialized`
/// does not help, because it orders tests *within* a suite. So this suite gets
/// its own class and its own statics, and the two cannot see each other.
final class MalkuthMockURLProtocol: URLProtocol, @unchecked Sendable {
    nonisolated(unsafe) static var handler: (@Sendable (URLRequest) -> (Int, Data))?
    nonisolated(unsafe) static var recorded: [URLRequest] = []

    static func reset() {
        handler = nil
        recorded = []
    }

    static func makeSession() -> URLSession {
        let config = URLSessionConfiguration.ephemeral
        config.protocolClasses = [MalkuthMockURLProtocol.self]
        return URLSession(configuration: config)
    }

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }

    override func startLoading() {
        Self.recorded.append(request)
        let (status, body) = Self.handler?(request) ?? (500, Data())
        let response = HTTPURLResponse(
            url: request.url!, statusCode: status,
            httpVersion: "HTTP/1.1", headerFields: ["Content-Type": "application/json"]
        )!
        client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: body)
        client?.urlProtocolDidFinishLoading(self)
    }

    override func stopLoading() {}
}

/// A tiny mutex so the scripted handlers can count calls without an actor hop
/// in the middle of a `URLProtocol` callback.
final class Mutex<Value>: @unchecked Sendable {
    private let lock = NSLock()
    private var value: Value
    init(_ value: Value) { self.value = value }
    func withLock<T>(_ body: (inout Value) -> T) -> T {
        lock.lock(); defer { lock.unlock() }
        return body(&value)
    }
}

/// The Keychain refusing this app has exactly one cause in practice, and the
/// message has to say it: an unsigned build carries no `application-identifier`,
/// so every Keychain call fails, the session token never persists, and the first
/// visible symptom is a failed post. A bare OSStatus sends people looking in the
/// wrong place.
@Suite("Keychain failures explain themselves")
struct KeychainMessageTests {
    @Test("A missing entitlement names the build, not the number")
    func missingEntitlementIsLegible() {
        let message = MalkuthError.keychain(-34018).description
        #expect(message.contains("built without code signing"))
        #expect(!message.contains("-34018"))
    }

    @Test("Any other status still reports itself exactly")
    func otherStatusesKeepTheirCode() {
        #expect(MalkuthError.keychain(-25300).description == "Keychain error -25300.")
    }
}
