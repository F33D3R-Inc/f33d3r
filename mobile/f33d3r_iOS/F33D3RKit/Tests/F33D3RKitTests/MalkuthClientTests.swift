import Foundation
import Testing
@testable import F33D3RKit

/// The things that stop a bad post before a request is spent, and the things
/// that stop a good post becoming two.
@Suite(.serialized)
struct MalkuthClientTests {

    static let account = MalkuthCanonicalTests.account
    static let uuid = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
    static let cid = "sha256:" + String(repeating: "ab", count: 32)

    static func payload(_ mutate: (inout WorkPayload) -> Void = { _ in }) -> WorkPayload {
        var p = WorkPayload(authorAccount: account, body: "hello", timestampMS: 1_767_225_600_000)
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

    /// The server refuses a work whose `author_account` is not the session's
    /// account, so signing without one is refused here rather than attempted.
    @Test("Signing without an account is refused rather than attempted")
    func refusesToSignWithoutAccount() throws {
        let key = MalkuthKey.generate()
        for absent in ["", "   "] {
            #expect(throws: MalkuthError.accountUnknown) {
                try WorkEnvelope(payload: Self.payload { $0.authorAccount = absent }, key: key)
            }
        }

        let identity = AuthenticatedIdentity(user: try Self.sampleUser(), accountID: nil)
        #expect(!identity.canSign)
        #expect(throws: MalkuthError.accountUnknown) { try identity.requireAccount() }
    }

    @Test("An author_account that is not an account id is refused")
    func refusesMalformedAccount() throws {
        #expect(throws: MalkuthError.malformedUUID(field: "author_account", value: "nope")) {
            try WorkEnvelope(payload: Self.payload { $0.authorAccount = "nope" }, key: MalkuthKey.generate())
        }
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

    /// A repost is a reaction, never a signed work: the payload hashes
    /// `is_repost` and `repost_source_id` as constants, and nothing can set them.
    @Test("is_repost and repost_source_id are hashed as false and null")
    func repostFieldsAreConstants() {
        let text = String(decoding: Self.payload().canonicalJSON(), as: UTF8.self)
        #expect(text.contains(#""is_repost":false"#))
        #expect(text.contains(#""repost_source_id":null"#))
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
        let formatted = WorkPayload.rfc3339String(Date(timeIntervalSince1970: 1_767_225_600 + 86_400))
        #expect(formatted == "2026-01-02T00:00:00Z")
        #expect(throws: Never.self) {
            try WorkEnvelope(
                payload: Self.payload {
                    $0.kind = .poll
                    $0.pollOptions = ["yes", "no"]
                    $0.pollEndsAt = formatted
                },
                key: MalkuthKey.generate()
            )
        }
    }

    /// The server cannot rewrite signed text, so it refuses any that is not
    /// already normalised — a NUL or a bidi override included.
    @Test("Text that is not normalised is refused before signing")
    func rejectsUnnormalisedText() throws {
        let key = MalkuthKey.generate()
        #expect(throws: MalkuthError.textNotNormalized(field: "body")) {
            try WorkEnvelope(payload: Self.payload { $0.body = "a" + String(UnicodeScalar(0)) + "b" }, key: key)
        }
        #expect(throws: MalkuthError.textNotNormalized(field: "body")) {
            try WorkEnvelope(payload: Self.payload { $0.body = " padded " }, key: key)
        }
        #expect(throws: MalkuthError.invalidTag("b d")) {
            try WorkEnvelope(payload: Self.payload { $0.tags = ["ok", "b d"] }, key: key)
        }
    }

    @Test("A body over the weighted limit is refused with how far over it is")
    func rejectsTooLongText() throws {
        // CJK weighs two: 1001 of them is 2002 units.
        #expect(throws: MalkuthError.textTooLong(over: 2)) {
            try WorkEnvelope(
                payload: Self.payload { $0.body = String(repeating: "字", count: 1001) },
                key: MalkuthKey.generate()
            )
        }
    }

    @Test("A poll is two to four distinct options and an end five minutes to seven days out")
    func validatesPolls() throws {
        let key = MalkuthKey.generate()
        let day = WorkPayload.rfc3339String(Date(timeIntervalSince1970: 1_767_225_600 + 86_400))
        func poll(_ options: [String], ends: String?) -> WorkPayload {
            Self.payload { $0.kind = .poll; $0.pollOptions = options; $0.pollEndsAt = ends }
        }
        #expect(throws: Never.self) { try WorkEnvelope(payload: poll(["a", "b"], ends: day), key: key) }
        #expect(throws: MalkuthError.pollInvalid("A poll has 2 to 4 options.")) {
            try WorkEnvelope(payload: poll(["a"], ends: day), key: key)
        }
        #expect(throws: MalkuthError.pollInvalid("Options must differ.")) {
            try WorkEnvelope(payload: poll(["Yes", "yes"], ends: day), key: key)
        }
        #expect(throws: MalkuthError.pollInvalid("A poll needs an end time.")) {
            try WorkEnvelope(payload: poll(["a", "b"], ends: nil), key: key)
        }
        let tooLong = WorkPayload.rfc3339String(Date(timeIntervalSince1970: 1_767_225_600 + 8 * 86_400))
        #expect(throws: MalkuthError.pollInvalid("A poll runs from 5 minutes to 7 days.")) {
            try WorkEnvelope(payload: poll(["a", "b"], ends: tooLong), key: key)
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

    /// The CID is a content hash and the server answers a re-delivered CID with
    /// the work it already created, so re-sending identical bytes cannot make a
    /// second work. What makes a different work is signing again, because that
    /// re-stamps `timestamp_ms` — which a submission does only on the server's
    /// own word that the CID never landed.
    @Test("Signing the same draft at the same moment is the same work; later is a different one")
    func cidIsTheIdempotencyKey() async throws {
        let signer = MalkuthSigner(store: InMemoryMalkuthKeyStore())
        let moment = Date(timeIntervalSince1970: 1_767_225_600)

        var first = WorkSubmission(draft: Self.payload(), signer: signer)
        var second = WorkSubmission(draft: Self.payload(), signer: signer)
        try await first.sign(now: moment)
        try await second.sign(now: moment)
        #expect(first.contentID == second.contentID)

        var third = WorkSubmission(draft: Self.payload(), signer: signer)
        try await third.sign(now: moment.addingTimeInterval(0.001))
        #expect(third.contentID != first.contentID)
    }

    @Test("A submission is signed when it is sent, not when it is made")
    func signsAtSendTime() async throws {
        let (client, _) = Self.makeClient()
        let signer = MalkuthSigner(store: InMemoryMalkuthKeyStore())
        var submission = WorkSubmission(draft: Self.payload(), signer: signer)
        #expect(submission.envelope == nil)

        MalkuthMockURLProtocol.headers = ["X-Work-Create-State": "ok"]
        MalkuthMockURLProtocol.handler = { _ in (201, Data(#"{"work_id":"w-1","cid":"x"}"#.utf8)) }
        let before = WorkPayload.nowMS()
        _ = await submission.send(using: client)
        let stamped = try #require(submission.envelope?.payload.timestampMS)
        #expect(stamped >= before)
    }

    @Test("A timed-out post is retried with the identical bytes, not rebuilt")
    func retriesTheSameBytes() async throws {
        let (client, _) = Self.makeClient()
        let signer = MalkuthSigner(store: InMemoryMalkuthKeyStore())
        var submission = WorkSubmission(draft: Self.payload(), signer: signer)

        let bodies = Mutex<[Data]>([])
        MalkuthMockURLProtocol.handler = { request in
            bodies.withLock { $0.append(request.bodyData ?? Data()) }
            return bodies.withLock { $0.count } < 3
                ? (502, Data("bad gateway\n".utf8))
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
        // created a work.
        #expect(Set(sent).count == 1)
        #expect(sent[0] == submission.envelope?.httpBody)
    }

    @Test("An exhausted retry is uncertain, never a claim either way")
    func reportsUncertaintyHonestly() async throws {
        let (client, _) = Self.makeClient()
        let signer = MalkuthSigner(store: InMemoryMalkuthKeyStore())
        var submission = WorkSubmission(draft: Self.payload(), signer: signer)

        MalkuthMockURLProtocol.headers = ["X-Work-Create-State": "server_error"]
        MalkuthMockURLProtocol.handler = { _ in (500, Data("Something went wrong. Your post was not sent.".utf8)) }
        let outcome = await submission.sendWithRetries(
            using: client, attempts: 2, backoff: { _ in .zero }
        )
        guard case .uncertain(let cid, _) = outcome else {
            Issue.record("expected uncertainty, got \(outcome)")
            return
        }
        #expect(cid == submission.contentID)
    }

    @Test("A Duplicate is final and never retried")
    func duplicateIsFinal() async throws {
        let (client, _) = Self.makeClient()
        let signer = MalkuthSigner(store: InMemoryMalkuthKeyStore())
        var submission = WorkSubmission(draft: Self.payload(), signer: signer)

        let attempts = Mutex<Int>(0)
        MalkuthMockURLProtocol.headers = ["X-Work-Create-State": "duplicate"]
        MalkuthMockURLProtocol.handler = { _ in
            attempts.withLock { $0 += 1 }
            return (409, Data("You already posted this.".utf8))
        }
        let outcome = await submission.sendWithRetries(using: client, attempts: 5, backoff: { _ in .zero })
        #expect(outcome == .rejected(.workRefused(state: .duplicate, message: "You already posted this.")))
        #expect(attempts.withLock { $0 } == 1)
    }

    @Test("A rate limit backs off and retries the same bytes")
    func rateLimitBacksOff() async throws {
        let (client, _) = Self.makeClient()
        let signer = MalkuthSigner(store: InMemoryMalkuthKeyStore())
        var submission = WorkSubmission(draft: Self.payload(), signer: signer)

        let attempts = Mutex<Int>(0)
        let waits = Mutex<[Int]>([])
        MalkuthMockURLProtocol.headers = ["X-Work-Create-State": "rate_limited"]
        MalkuthMockURLProtocol.handler = { _ in
            attempts.withLock { $0 += 1 }
            return attempts.withLock { $0 } < 2
                ? (429, Data("You have reached the posting limit. Try again later.".utf8))
                : (201, Data(#"{"work_id":"w-1","cid":"x"}"#.utf8))
        }
        let outcome = await submission.sendWithRetries(
            using: client, attempts: 3, backoff: { n in waits.withLock { $0.append(n) }; return .zero }
        )
        guard case .accepted = outcome else {
            Issue.record("expected acceptance after backing off, got \(outcome)")
            return
        }
        // The wait after a 429 is longer than the wait after a transport blip.
        #expect(waits.withLock { $0 } == [3])
    }

    @Test("A signature the server calls too old is signed again once, and only then")
    func resignsOnlyOnTheServersWord() async throws {
        let (client, _) = Self.makeClient()
        let signer = MalkuthSigner(store: InMemoryMalkuthKeyStore())
        var submission = WorkSubmission(draft: Self.payload(), signer: signer)

        let cids = Mutex<[String]>([])
        MalkuthMockURLProtocol.handler = { request in
            let body = try? JSONSerialization.jsonObject(with: request.bodyData ?? Data()) as? [String: Any]
            cids.withLock { $0.append(body?["cid"] as? String ?? "") }
            if cids.withLock({ $0.count }) == 1 {
                MalkuthMockURLProtocol.headers = ["X-Work-Create-State": "invalid_field"]
                return (400, Data("This post could not be sent: timestamp_ms — the work must be signed within the last hour.".utf8))
            }
            MalkuthMockURLProtocol.headers = ["X-Work-Create-State": "ok"]
            return (201, Data(#"{"work_id":"w-1","cid":"x"}"#.utf8))
        }
        let outcome = await submission.sendWithRetries(using: client, attempts: 3, backoff: { _ in .zero })
        guard case .accepted = outcome else {
            Issue.record("expected acceptance after re-signing, got \(outcome)")
            return
        }
        let sent = cids.withLock { $0 }
        #expect(sent.count == 2)
        #expect(sent[0] != sent[1])
    }

    @Test("A deterministic refusal is not retried, and keeps the server's words")
    func doesNotRetryDeterministicRefusals() async throws {
        let (client, _) = Self.makeClient()
        let signer = MalkuthSigner(store: InMemoryMalkuthKeyStore())
        var submission = WorkSubmission(draft: Self.payload(), signer: signer)

        let attempts = Mutex<Int>(0)
        let words = "This post could not be sent: cid — does not match the payload."
        MalkuthMockURLProtocol.headers = ["X-Work-Create-State": "invalid_field"]
        MalkuthMockURLProtocol.handler = { _ in
            attempts.withLock { $0 += 1 }
            return (400, Data(words.utf8))
        }

        let outcome = await submission.sendWithRetries(
            using: client, attempts: 5, backoff: { _ in .zero }
        )
        #expect(outcome == .rejected(.workRefused(state: .invalidField, message: words)))
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
        let first = try await MalkuthSigner(store: store).key(for: Self.account)
        let second = try await MalkuthSigner(store: store).key(for: Self.account)
        #expect(first.publicKeySPKIBase64 == second.publicKeySPKIBase64)

        try await MalkuthSigner(store: store).destroyKey(for: Self.account)
        let third = try await MalkuthSigner(store: store).key(for: Self.account)
        #expect(third.publicKeySPKIBase64 != first.publicKeySPKIBase64)
    }

    @Test("Each account on a device has its own key")
    func keysAreKeptPerAccount() async throws {
        let stores = Mutex<[String: InMemoryMalkuthKeyStore]>([:])
        let signer = MalkuthSigner(storeFor: { account in
            stores.withLock { all in
                if let store = all[account] { return store }
                let store = InMemoryMalkuthKeyStore()
                all[account] = store
                return store
            }
        })
        let other = "11111111-2222-3333-4444-555555555555"
        let mine = try await signer.key(for: Self.account)
        let theirs = try await signer.key(for: other)
        #expect(mine.publicKeySPKIBase64 != theirs.publicKeySPKIBase64)
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
        #expect(try await signer.registerIfNeeded(with: client, accountID: Self.account))
        #expect(!(try await signer.registerIfNeeded(with: client, accountID: Self.account)))
        #expect(calls.withLock { $0 } == 1)

        // Every sign-in forces it: another device may have registered over
        // this key.
        #expect(try await signer.registerIfNeeded(with: client, accountID: Self.account, force: true))
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

    @Test("meIdentity reads account_id, and refuses to sign without it")
    func readsAccountFromMe() async throws {
        let (client, _) = Self.makeClient()
        let base = try ContractTests.fixture("me")

        MalkuthMockURLProtocol.handler = { _ in (200, base) }
        let identity = try await client.meIdentity()
        #expect(identity.accountID == Self.account)
        #expect(identity.canSign)
        #expect(try identity.requireAccount() == Self.account)
        // One request per call, not two.
        #expect(MalkuthMockURLProtocol.recorded.count == 1)

        var object = try #require(try JSONSerialization.jsonObject(with: base) as? [String: Any])
        object.removeValue(forKey: "account_id")
        let without = try JSONSerialization.data(withJSONObject: object)
        MalkuthMockURLProtocol.reset()
        MalkuthMockURLProtocol.handler = { _ in (200, without) }
        let absent = try await client.meIdentity()
        #expect(absent.accountID == nil)
        #expect(!absent.canSign)
    }

    @Test("An empty account_id is treated as absent")
    func treatsEmptyAccountAsAbsent() async throws {
        let (client, _) = Self.makeClient()
        var object = try #require(
            try JSONSerialization.jsonObject(with: try ContractTests.fixture("me")) as? [String: Any]
        )
        object["account_id"] = ""
        let data = try JSONSerialization.data(withJSONObject: object)
        MalkuthMockURLProtocol.handler = { _ in (200, data) }
        #expect(try await client.meIdentity().accountID == nil)
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
    /// Extra response headers, such as `X-Work-Create-State`.
    nonisolated(unsafe) static var headers: [String: String] = [:]

    static func reset() {
        handler = nil
        recorded = []
        headers = [:]
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
            httpVersion: "HTTP/1.1",
            headerFields: ["Content-Type": "application/json"].merging(Self.headers) { _, new in new }
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
