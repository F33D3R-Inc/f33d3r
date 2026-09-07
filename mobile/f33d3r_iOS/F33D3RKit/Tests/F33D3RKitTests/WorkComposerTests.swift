import Foundation
import Testing
@testable import F33D3RKit

/// What the composer puts on the wire for the things the web composer can do
/// and this one could not: a scheduled work, a thread, a draft kept for later,
/// and the three attachments that are not pictures — a video that has to be
/// transcoded before it can be posted, a voice note whose length only the
/// recorder knows, and a GIF from a provider this server does not host.
///
/// The requests are read back as the server would read them — the envelope
/// bytes parsed, the signed payload inspected field by field — because the
/// point of each feature is a specific shape on the wire, and a test of the
/// composer's own properties would prove only that it agrees with itself.
///
/// Serialized because the tests script one request interceptor between them,
/// and two running at once would answer each other's requests.
@MainActor
@Suite("Work composer", .serialized)
struct WorkComposerTests {

    static let pial = MalkuthCanonicalTests.pial

    // MARK: Harness

    static func makeComposer(mode: WorkComposer.Mode = .post) throws -> (WorkComposer, APIClient) {
        ComposerMockURLProtocol.reset()
        let client = APIClient(
            baseURL: URL(string: "https://f33d3r.com")!,
            tokens: StubTokens(token: "tok"),
            session: ComposerMockURLProtocol.makeSession()
        )
        let identity = AuthenticatedIdentity(user: try MalkuthClientTests.sampleUser(), pialID: pial)
        let composer = WorkComposer(
            mode: mode, client: client, signer: MalkuthSigner(store: InMemoryMalkuthKeyStore()), identity: identity
        )
        return (composer, client)
    }

    /// Answers the key registration with 200 and every work with 201 echoing
    /// the CID it was sent, the way `workEvent` does.
    static func acceptEverything() {
        ComposerMockURLProtocol.handler = { request in
            guard let body = request.bodyData, let envelope = Self.object(body), let cid = envelope["cid"] as? String else {
                return (200, Data("{}".utf8))
            }
            return (201, Data(#"{"work_id":"w-\#(cid.suffix(6))","cid":"\#(cid)"}"#.utf8))
        }
    }

    /// Called from inside `URLProtocol` callbacks, which are not on the main
    /// actor.
    nonisolated static func object(_ data: Data) -> [String: Any]? {
        (try? JSONSerialization.jsonObject(with: data)) as? [String: Any]
    }

    /// The signed payloads of the works posted so far, in order.
    static func postedPayloads() -> [[String: Any]] {
        ComposerMockURLProtocol.recorded.compactMap { request in
            guard let body = request.bodyData, let envelope = object(body), envelope["cid"] != nil else { return nil }
            return envelope["payload"] as? [String: Any]
        }
    }

    // MARK: Scheduling

    @Test("A scheduled work carries scheduled_at inside the signed payload, to the minute, in UTC")
    func scheduledAtIsSigned() async throws {
        let (composer, _) = try Self.makeComposer()
        Self.acceptEverything()
        composer.body = "later"

        // 14:30:41 local is stored as 14:30:00 — the picker shows minutes.
        var parts = DateComponents()
        parts.year = 2030; parts.month = 6; parts.day = 1; parts.hour = 14; parts.minute = 30; parts.second = 41
        parts.timeZone = TimeZone(identifier: "UTC")
        let picked = Calendar(identifier: .gregorian).date(from: parts)!
        #expect(composer.schedule(for: picked, now: picked.addingTimeInterval(-3600)))
        #expect(composer.scheduledAt == picked.addingTimeInterval(-41))

        let outcome = await composer.submit()
        guard case .accepted = outcome else {
            Issue.record("expected acceptance, got \(String(describing: outcome)); failure: \(composer.failure ?? "none")")
            return
        }
        let payload = try #require(Self.postedPayloads().last)
        #expect(payload["scheduled_at"] as? String == "2030-06-01T14:30:00Z")
        #expect(payload["kind"] as? String == "post")
    }

    @Test("A time under the floor is refused, not rounded up")
    func scheduleFloor() throws {
        let (composer, _) = try Self.makeComposer()
        let now = Date(timeIntervalSince1970: 1_900_000_000)
        #expect(!composer.schedule(for: now.addingTimeInterval(10 * 60), now: now))
        #expect(composer.scheduledAt == nil)
        #expect(composer.schedule(for: WorkComposer.earliestSchedule(from: now), now: now))
        #expect(composer.scheduledAt != nil)
    }

    @Test("A schedule that has slipped into the past blocks the post and says why")
    func staleScheduleBlocks() throws {
        let (composer, _) = try Self.makeComposer()
        composer.body = "stale"
        let past = Date().addingTimeInterval(-60)
        #expect(composer.schedule(for: past, now: past.addingTimeInterval(-3600)))
        #expect(composer.scheduleProblem != nil)
        #expect(!composer.canSubmit)
        composer.clearSchedule()
        #expect(composer.canSubmit)
    }

    @Test("Only a new work can be scheduled or threaded")
    func repliesCannotBeScheduledOrThreaded() throws {
        let parent = SampleData.works[0]
        let (reply, _) = try Self.makeComposer(mode: .reply(to: parent))
        #expect(!reply.canSchedule)
        #expect(!reply.canAddSegment)
        #expect(!reply.canSaveDraft)
        #expect(reply.addSegment() == nil)
    }

    @Test("A scheduled poll closes `duration` after it publishes, not after it was written")
    func scheduledPollEndsAfterPublish() async throws {
        let (composer, _) = try Self.makeComposer()
        Self.acceptEverything()
        composer.body = "which?"
        composer.addPoll()
        composer.poll?.options = ["a", "b"]
        let opens = Date().addingTimeInterval(3 * 86_400)
        #expect(composer.schedule(for: opens))

        _ = await composer.submit()
        let payload = try #require(Self.postedPayloads().last)
        let ends = try #require(payload["poll_ends_at"] as? String)
        let formatter = ISO8601DateFormatter()
        let endsAt = try #require(formatter.date(from: ends))
        let scheduledAt = try #require(composer.scheduledAt)
        #expect(abs(endsAt.timeIntervalSince(scheduledAt) - 86_400) < 1)
    }

    // MARK: Threads

    @Test("Every part is thread_post, chained by parent_cid, with media on the head only")
    func threadChain() async throws {
        let (composer, _) = try Self.makeComposer()
        Self.acceptEverything()
        composer.body = "one"
        composer.addSegment()
        composer.addSegment()
        composer.setSegmentBody(composer.segments[0].id, "two")
        composer.setSegmentBody(composer.segments[1].id, "  three  ")
        #expect(composer.isThread)
        #expect(composer.kind == .threadPost)
        #expect(!composer.canAddPoll)

        let outcome = await composer.submit()
        guard case .accepted(let head) = outcome else {
            Issue.record("expected acceptance, got \(String(describing: outcome)); failure: \(composer.failure ?? "none")")
            return
        }

        let payloads = Self.postedPayloads()
        #expect(payloads.count == 3)
        #expect(payloads.map { $0["kind"] as? String } == ["thread_post", "thread_post", "thread_post"])
        #expect(payloads.map { $0["body"] as? String } == ["one", "two", "three"])
        #expect(payloads[0]["parent_cid"] is NSNull)

        // Each part names the one before it by the CID the server answered.
        let cids = ComposerMockURLProtocol.recorded.compactMap { request -> String? in
            request.bodyData.flatMap(Self.object).flatMap { $0["cid"] as? String }
        }
        #expect(payloads[1]["parent_cid"] as? String == cids[0])
        #expect(payloads[2]["parent_cid"] as? String == cids[1])
        #expect(head.cid == cids[0])

        // Timestamps step so the parts read in order.
        let stamps = payloads.map { ($0["timestamp_ms"] as? NSNumber)?.int64Value ?? 0 }
        #expect(stamps[1] == stamps[0] + 1 && stamps[2] == stamps[0] + 2)

        // Finished: nothing left on the screen, and the next work is a new one.
        #expect(composer.publishedParts.count == 3)
        #expect(!composer.isThread)
        #expect(!composer.hasContent)
    }

    @Test("An empty part blocks the thread rather than being dropped")
    func emptyPartBlocks() throws {
        let (composer, _) = try Self.makeComposer()
        composer.body = "head"
        composer.addSegment()
        #expect(!composer.canSubmit)
        composer.setSegmentBody(composer.segments[0].id, "tail")
        #expect(composer.canSubmit)
        composer.removeSegment(composer.segments[0].id)
        #expect(!composer.isThread)
        #expect(composer.kind == .post)
    }

    @Test("A chain that stops keeps the rest, addressed to the last part that landed")
    func threadStopsHonestly() async throws {
        let (composer, _) = try Self.makeComposer()
        composer.body = "one"
        composer.addSegment()
        composer.addSegment()
        composer.setSegmentBody(composer.segments[0].id, "two")
        composer.setSegmentBody(composer.segments[1].id, "three")

        // The second part is refused for good.
        let works = Mutex<Int>(0)
        ComposerMockURLProtocol.handler = { request in
            guard let body = request.bodyData, let envelope = Self.object(body), let cid = envelope["cid"] as? String else {
                return (200, Data("{}".utf8))
            }
            let n = works.withLock { $0 += 1; return $0 }
            if n == 2 { return (400, Data("body too long\n".utf8)) }
            return (201, Data(#"{"work_id":"w-\#(n)","cid":"\#(cid)"}"#.utf8))
        }

        let outcome = await composer.submit()
        guard case .rejected = outcome else {
            Issue.record("expected refusal, got \(String(describing: outcome))")
            return
        }

        // Exactly one part is live, and the composer says so.
        #expect(composer.publishedParts.count == 1)
        let failure = try #require(composer.failure)
        #expect(failure.contains("1 of 3 parts"))
        #expect(failure.contains("body too long"))

        // The rest is still here, now headed by what was part two, and it
        // continues from part one.
        #expect(composer.body == "two")
        #expect(composer.segments.map(\.body) == ["three"])
        #expect(composer.continuesThreadFrom == composer.publishedParts[0].cid)
        #expect(composer.isThread)
        #expect(composer.kind == .threadPost)

        // Posting again sends two more parts, the first of them a child of the
        // part that landed — never the head again.
        works.withLock { $0 = 0 }
        Self.acceptEverything()
        let again = await composer.submit()
        guard case .accepted(let head) = again else {
            Issue.record("expected acceptance, got \(String(describing: again))")
            return
        }
        let payloads = Self.postedPayloads()
        #expect(payloads.count == 4)
        #expect(payloads[2]["body"] as? String == "two")
        #expect(payloads[2]["parent_cid"] as? String == composer.publishedParts[0].cid)
        #expect(payloads[3]["body"] as? String == "three")
        // The row handed back is the head from the first attempt.
        #expect(head == composer.publishedParts[0])
        #expect(composer.publishedParts.count == 3)
    }

    @Test("An uncertain part is retried with the same bytes, from that part")
    func threadRetriesSameBytes() async throws {
        let (composer, _) = try Self.makeComposer()
        composer.body = "one"
        composer.addSegment()
        composer.setSegmentBody(composer.segments[0].id, "two")

        let works = Mutex<[Data]>([])
        ComposerMockURLProtocol.handler = { request in
            guard let body = request.bodyData, let envelope = Self.object(body), let cid = envelope["cid"] as? String else {
                return (200, Data("{}".utf8))
            }
            let count = works.withLock { $0.append(body); return $0.count }
            // Part one lands; part two answers 500 on every attempt.
            if count == 1 { return (201, Data(#"{"work_id":"w-1","cid":"\#(cid)"}"#.utf8)) }
            return (500, Data("server error\n".utf8))
        }

        let outcome = await composer.submit()
        guard case .uncertain = outcome else {
            Issue.record("expected uncertainty, got \(String(describing: outcome))")
            return
        }
        #expect(composer.uncertain != nil)
        #expect(composer.publishedParts.count == 1)
        #expect(composer.body == "two")

        // Now it lands. The bytes sent are the ones signed the first time.
        let sent = works.withLock { $0 }
        let secondPart = sent[1]
        works.withLock { $0 = [] }
        ComposerMockURLProtocol.handler = { request in
            guard let body = request.bodyData, let envelope = Self.object(body), let cid = envelope["cid"] as? String else {
                return (200, Data("{}".utf8))
            }
            works.withLock { $0.append(body) }
            return (201, Data(#"{"work_id":"w-2","cid":"\#(cid)"}"#.utf8))
        }
        let again = await composer.submit()
        guard case .accepted = again else {
            Issue.record("expected acceptance, got \(String(describing: again))")
            return
        }
        let resent = works.withLock { $0 }
        #expect(resent.count == 1)
        #expect(resent[0] == secondPart)
        #expect(composer.uncertain == nil)
        #expect(composer.publishedParts.count == 2)
    }

    @Test("Make it a thread cuts at blank lines, one part per paragraph")
    func makeThreadFromParagraphs() throws {
        let (composer, _) = try Self.makeComposer()
        composer.body = "first\n\nsecond\n\n\nthird"
        #expect(composer.makeThread(from: ComposeAnalysis.paragraphs(of: composer.body)))
        #expect(composer.body == "first")
        #expect(composer.segments.map(\.body) == ["second", "third"])
    }

    // MARK: Drafts

    @Test("A draft round-trips everything but the image bytes")
    func draftRoundTrip() throws {
        let (composer, _) = try Self.makeComposer()
        composer.body = "kept"
        composer.addSegment()
        composer.setSegmentBody(composer.segments[0].id, "for later")
        composer.subscriberOnly = true
        composer.isNSFW = true
        composer.commentGating = .circle
        // On a minute boundary, because scheduling floors to one.
        let when = Date(timeIntervalSince1970: 1_899_999_960)
        #expect(composer.schedule(for: when, now: when.addingTimeInterval(-86_400)))

        let draft = composer.draft(savedAt: when)
        #expect(composer.draftID == draft.id)
        #expect(draft.body == "kept")
        #expect(draft.segments == ["for later"])
        #expect(draft.scheduledAt == when)
        #expect(draft.commentGating == .circle)

        let (restored, _) = try Self.makeComposer()
        restored.restore(draft)
        #expect(restored.draftID == draft.id)
        #expect(restored.body == "kept")
        #expect(restored.segments.map(\.body) == ["for later"])
        #expect(restored.subscriberOnly && restored.isNSFW)
        #expect(restored.commentGating == .circle)
        #expect(restored.scheduledAt == when)
        // Saving again names the same file.
        #expect(restored.draft().id == draft.id)
    }

    @Test("A draft with a poll restores the poll, options and length")
    func draftPoll() throws {
        let (composer, _) = try Self.makeComposer()
        composer.body = "which"
        composer.addPoll()
        composer.poll?.options = ["a", "b", "c"]
        composer.poll?.duration = .seconds(3 * 86_400)
        let draft = composer.draft()
        #expect(draft.poll == ComposeDraft.Poll(options: ["a", "b", "c"], durationSeconds: 3 * 86_400))

        let (restored, _) = try Self.makeComposer()
        restored.restore(draft)
        #expect(restored.poll?.options == ["a", "b", "c"])
        #expect(restored.poll?.duration == .seconds(3 * 86_400))
        #expect(restored.kind == .poll)
    }

    @Test("The store writes one file per draft and reads them back newest first")
    func storeRoundTrip() throws {
        let directory = FileManager.default.temporaryDirectory
            .appendingPathComponent("f33d3r-drafts-\(UUID().uuidString)", isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }

        let store = ComposeDraftStore(directory: directory)
        try store.reload()
        #expect(store.drafts.isEmpty)

        let older = ComposeDraft(savedAt: Date(timeIntervalSince1970: 1_000), body: "older\nsecond line")
        let newer = ComposeDraft(
            savedAt: Date(timeIntervalSince1970: 2_000), body: "",
            attachments: [.init(remotePath: "/media/a.jpg", filename: "a.jpg", mimeType: "image/jpeg")]
        )
        try store.save(older)
        try store.save(newer)
        #expect(store.drafts.map(\.id) == [newer.id, older.id])
        #expect(older.title == "older")
        #expect(newer.title == "Photo")

        // A fresh store over the same directory sees the same drafts.
        let reopened = ComposeDraftStore(directory: directory)
        try reopened.reload()
        #expect(reopened.drafts == [newer, older])

        try reopened.delete(id: newer.id)
        #expect(reopened.drafts == [older])
        #expect(!FileManager.default.fileExists(atPath: directory.appendingPathComponent("\(newer.id.uuidString).json").path))
    }

    // MARK: Video, voice, GIF

    /// Answers the upload lane the way api_v1_media.go does — `scripted` is
    /// consulted by path for the media routes; works are accepted as usual.
    static func acceptMedia(_ scripted: @escaping @Sendable (URLRequest) -> (Int, Data)?) {
        ComposerMockURLProtocol.handler = { request in
            if let answer = scripted(request) { return answer }
            guard let body = request.bodyData, let envelope = Self.object(body), let cid = envelope["cid"] as? String else {
                return (200, Data("{}".utf8))
            }
            return (201, Data(#"{"work_id":"w-\#(cid.suffix(6))","cid":"\#(cid)"}"#.utf8))
        }
    }

    /// A small file standing in for a clip, so the upload has bytes to stream.
    static func temporaryFile(_ contents: String, extension ext: String) throws -> URL {
        let url = FileManager.default.temporaryDirectory
            .appendingPathComponent("f33d3r-test-\(UUID().uuidString).\(ext)", isDirectory: false)
        try Data(contents.utf8).write(to: url)
        return url
    }

    nonisolated static let readyJob = #"{"upload_id":"upl-1","status":"ready","master_url":"/static/media/posts/a1/master.m3u8","poster_url":"/static/media/posts/a1/poster.jpg","watermarked_url":"/static/media/posts/a1/watermarked.mp4","duration_secs":42.25,"width":1280,"height":720}"#

    @Test("A video is uploaded as a streamed multipart file, polled to ready, and lands in the signed and unsigned halves")
    func videoFlow() async throws {
        let (composer, _) = try Self.makeComposer()
        composer.videoPollInterval = .milliseconds(5)
        let polls = Mutex<Int>(0)
        Self.acceptMedia { request in
            switch request.url?.path {
            case "/api/v1/media/video":
                return (202, Data(#"{"upload_id":"upl-1","status":"queued"}"#.utf8))
            case "/api/v1/media/video/upl-1":
                let n = polls.withLock { $0 += 1; return $0 }
                return n < 3
                    ? (200, Data(#"{"upload_id":"upl-1","status":"processing"}"#.utf8))
                    : (200, Data(Self.readyJob.utf8))
            default:
                return nil
            }
        }

        let clip = try Self.temporaryFile("not really mp4 bytes", extension: "mp4")
        defer { try? FileManager.default.removeItem(at: clip) }
        composer.body = "watch this"
        composer.attachVideo(fileURL: clip, filename: "clip.mp4", mimeType: "video/mp4")
        #expect(composer.video?.isInFlight == true)
        #expect(!composer.canSubmit)
        #expect(!composer.canAttach && !composer.canAddPoll && !composer.canAttachGIF)
        #expect(composer.blockingActivity?.hasPrefix("Uploading") == true)
        await composer.settleVideo()

        // The upload went from a file, not a Data in memory: the request that
        // reached the transport carries no body of its own — Foundation streams
        // the file itself, which is also why an interceptor cannot see it, and
        // why the framing is checked on the spooled file below.
        let upload = try #require(ComposerMockURLProtocol.recorded.first { $0.url?.path == "/api/v1/media/video" })
        #expect(upload.httpBody == nil)
        #expect(upload.httpMethod == "POST")
        #expect(upload.value(forHTTPHeaderField: "Content-Type")?.hasPrefix("multipart/form-data; boundary=") == true)
        #expect(upload.value(forHTTPHeaderField: "Authorization") == "Bearer tok")

        // Polled until ready, then the tile settled.
        #expect(polls.withLock { $0 } == 3)
        let renditions = try #require(composer.video?.renditions)
        #expect(renditions.masterURL == "/static/media/posts/a1/master.m3u8")
        #expect(renditions.durationSecs == 42)
        #expect(renditions.uploadID == "upl-1")
        #expect(composer.kind == .video)
        #expect(composer.blockingActivity == nil)
        #expect(composer.canSubmit)

        let outcome = await composer.submit()
        guard case .accepted = outcome else {
            Issue.record("expected acceptance, got \(String(describing: outcome)); failure: \(composer.failure ?? "none")")
            return
        }
        let request = try #require(ComposerMockURLProtocol.recorded.last { $0.url?.path == "/events" })
        let envelope = try #require(request.bodyData.flatMap(Self.object))
        let payload = try #require(envelope["payload"] as? [String: Any])
        // Signed: master, poster, whole seconds, the kind.
        #expect(payload["kind"] as? String == "video")
        #expect(payload["video_master_url"] as? String == "/static/media/posts/a1/master.m3u8")
        #expect(payload["video_poster_url"] as? String == "/static/media/posts/a1/poster.jpg")
        #expect((payload["video_duration_secs"] as? NSNumber)?.intValue == 42)
        #expect(payload["media_urls"] as? [String] == [])
        // Unsigned, beside the payload: the watermarked file and the frame size.
        #expect(envelope["video_watermarked_url"] as? String == "/static/media/posts/a1/watermarked.mp4")
        #expect((envelope["video_width"] as? NSNumber)?.intValue == 1280)
        #expect((envelope["video_height"] as? NSNumber)?.intValue == 720)
        // Inside the payload object but outside the signature: the upload id.
        #expect(payload["tus_upload_id"] as? String == "upl-1")
        #expect(!(payload.keys.contains("video_watermarked_url")))
    }

    @Test("A spooled multipart body is the prefix, the file's bytes and the suffix, and nothing else")
    func multipartSpool() throws {
        let clip = try Self.temporaryFile("not really mp4 bytes", extension: "mp4")
        defer { try? FileManager.default.removeItem(at: clip) }
        let part = MultipartPart(field: "file", filename: "clip.mp4", mimeType: "video/mp4")
        let spooled = try part.spool(clip)
        defer { try? FileManager.default.removeItem(at: spooled) }

        let bytes = try Data(contentsOf: spooled)
        // Byte for byte what the in-memory framing produces, so the two paths
        // put the same thing on the wire.
        #expect(bytes == part.body(wrapping: Data("not really mp4 bytes".utf8)))
        let text = String(decoding: bytes, as: UTF8.self)
        #expect(text.hasPrefix("--\(part.boundary)\r\nContent-Disposition: form-data; name=\"file\"; filename=\"clip.mp4\"\r\nContent-Type: video/mp4\r\n\r\n"))
        #expect(text.hasSuffix("not really mp4 bytes\r\n--\(part.boundary)--\r\n"))
        #expect(part.contentType == "multipart/form-data; boundary=\(part.boundary)")
    }

    @Test("A duplicate is not attached until the author says so, and then carries the original's renditions and credit")
    func videoDuplicate() async throws {
        let (composer, _) = try Self.makeComposer()
        Self.acceptMedia { request in
            guard request.url?.path == "/api/v1/media/video" else { return nil }
            return (200, Data(#"{"status":"duplicate","duplicate_of_work_id":"w-1","duplicate_of_handle":"miiyazuko","master_url":"/static/media/posts/a1/master.m3u8","poster_url":"/static/media/posts/a1/poster.jpg","duration_secs":12.5,"width":1920,"height":1080}"#.utf8))
        }
        let clip = try Self.temporaryFile("dup", extension: "mov")
        defer { try? FileManager.default.removeItem(at: clip) }
        composer.body = "again"
        composer.attachVideo(fileURL: clip, filename: "clip.mov", mimeType: "video/quicktime")
        await composer.settleVideo()

        guard case .duplicate(let handle, let workID, let renditions) = composer.video?.state else {
            Issue.record("expected a duplicate, got \(String(describing: composer.video?.state))")
            return
        }
        #expect(handle == "miiyazuko" && workID == "w-1")
        #expect(renditions?.masterURL == "/static/media/posts/a1/master.m3u8")
        // Waiting on the author: nothing to post yet, and nothing blocking either.
        #expect(!composer.canSubmit)
        #expect(composer.video?.isInFlight == false)
        #expect(composer.blockingActivity == nil)

        composer.acceptDuplicateVideo()
        let accepted = try #require(composer.video?.renditions)
        #expect(accepted.creditedTo == "miiyazuko")
        #expect(accepted.uploadID == nil)
        #expect(accepted.durationSecs == 13)
        #expect(composer.canSubmit)

        _ = await composer.submit()
        let request = try #require(ComposerMockURLProtocol.recorded.last { $0.url?.path == "/events" })
        let envelope = try #require(request.bodyData.flatMap(Self.object))
        let payload = try #require(envelope["payload"] as? [String: Any])
        #expect(payload["video_master_url"] as? String == "/static/media/posts/a1/master.m3u8")
        // No upload of ours to keep from the sweep.
        #expect(payload["tus_upload_id"] == nil)
    }

    @Test("A failed transcode shows the server's reason; a refused upload shows the server's code")
    func videoFailures() async throws {
        let (composer, _) = try Self.makeComposer()
        composer.videoPollInterval = .milliseconds(5)
        Self.acceptMedia { request in
            switch request.url?.path {
            case "/api/v1/media/video":
                return (202, Data(#"{"upload_id":"upl-2","status":"queued"}"#.utf8))
            case "/api/v1/media/video/upl-2":
                return (200, Data(#"{"upload_id":"upl-2","status":"failed","error":"ffmpeg exited with 1"}"#.utf8))
            default:
                return nil
            }
        }
        let clip = try Self.temporaryFile("bad", extension: "mp4")
        defer { try? FileManager.default.removeItem(at: clip) }
        composer.attachVideo(fileURL: clip, filename: "clip.mp4", mimeType: "video/mp4")
        await composer.settleVideo()
        #expect(composer.video?.state == .failed("ffmpeg exited with 1"))
        #expect(!composer.canSubmit)

        // Taking it off the draft removes the picker's copy of the file: the
        // composer owns what it was handed, and nothing references it now.
        composer.removeVideo()
        #expect(composer.video == nil)
        #expect(!FileManager.default.fileExists(atPath: clip.path))

        // Refused at the door: the 415's message reaches the tile, and a retry
        // sends the file again. A new pick is a new copy, as from the picker.
        let second = try Self.temporaryFile("bad", extension: "mp4")
        defer { try? FileManager.default.removeItem(at: second) }
        Self.acceptMedia { request in
            guard request.url?.path == "/api/v1/media/video" else { return nil }
            return (415, Data(#"{"error":{"code":"unsupported_media","message":"Only MP4, MOV, M4V, WebM and MKV video is accepted."}}"#.utf8))
        }
        composer.attachVideo(fileURL: second, filename: "clip.mp4", mimeType: "video/mp4")
        await composer.settleVideo()
        #expect(composer.video?.state == .failed("Only MP4, MOV, M4V, WebM and MKV video is accepted."))

        Self.acceptMedia { request in
            guard request.url?.path == "/api/v1/media/video" else { return nil }
            return (200, Data(Self.readyJob.utf8))
        }
        composer.retryVideo()
        await composer.settleVideo()
        #expect(composer.video?.renditions != nil)
    }

    @Test("A voice note is uploaded under `audio`, its length is the recorder's, and it makes the work a voice work")
    func voiceFlow() async throws {
        let (composer, _) = try Self.makeComposer()
        Self.acceptMedia { request in
            guard request.url?.path == "/api/v1/media/voice" else { return nil }
            return (201, Data(#"{"url":"/static/media/voice/v1.m4a"}"#.utf8))
        }
        let recording = try Self.temporaryFile("aac bytes", extension: "m4a")
        defer { try? FileManager.default.removeItem(at: recording) }
        composer.body = "listen"
        await composer.attachVoice(fileURL: recording, durationSecs: 7, filename: "voice.m4a", mimeType: "audio/mp4")

        let upload = try #require(ComposerMockURLProtocol.recorded.first { $0.url?.path == "/api/v1/media/voice" })
        let body = try #require(upload.bodyData.map { String(decoding: $0, as: UTF8.self) })
        #expect(body.contains("name=\"audio\"; filename=\"voice.m4a\""))
        #expect(composer.voice?.remotePath == "/static/media/voice/v1.m4a")
        #expect(composer.kind == .voice)
        #expect(!composer.canAddPoll)
        #expect(composer.canSubmit)

        _ = await composer.submit()
        let payload = try #require(Self.postedPayloads().last)
        #expect(payload["kind"] as? String == "voice")
        #expect(payload["voice_url"] as? String == "/static/media/voice/v1.m4a")
        #expect((payload["voice_duration_secs"] as? NSNumber)?.intValue == 7)
    }

    @Test("Voice beats video beats a reply, a quote or a poll; a thread beats them all")
    func kindPrecedence() throws {
        let ready = WorkComposer.VideoRenditions(masterURL: "/static/media/posts/a1/master.m3u8", durationSecs: 3)
        let staged = WorkComposer.VideoAttachment(fileURL: nil, filename: "clip.mp4", mimeType: "video/mp4", state: .ready(ready))

        let (post, _) = try Self.makeComposer()
        post.body = "which"
        post.addPoll()
        #expect(post.kind == .poll)
        // A poll on the draft keeps a video off it; the other way round too.
        #expect(!post.canAttachVideo)
        post.removePoll()
        post.debugStageVideo(staged)
        #expect(post.kind == .video)
        #expect(!post.canAddPoll)

        let (reply, _) = try Self.makeComposer(mode: .reply(to: SampleData.works[0]))
        #expect(reply.kind == .reply)
        reply.debugStageVideo(staged)
        #expect(reply.kind == .video)

        let (quote, _) = try Self.makeComposer(mode: .quote(SampleData.works[0]))
        quote.debugStageVideo(staged)
        #expect(quote.kind == .video)

        // A voice note over a video: voice.
        let (both, _) = try Self.makeComposer()
        both.debugStageVideo(staged)
        both.restore(ComposeDraft(
            body: "spoken",
            video: .init(renditions: ready, filename: "clip.mp4"),
            voice: .init(remotePath: "/static/media/voice/v1.m4a", durationSecs: 4)
        ))
        #expect(both.video != nil && both.voice != nil)
        #expect(both.kind == .voice)

        // A thread is a thread whatever rides on its head.
        both.addSegment()
        #expect(both.kind == .threadPost)
    }

    @Test("A GIF joins media_urls as the provider's URL, takes a media slot, and there is only one")
    func gifJoinsMedia() async throws {
        let (composer, _) = try Self.makeComposer()
        Self.acceptEverything()
        let cat = GifResult(id: "1", url: "https://cdn.klipy.test/md.gif", previewURL: "https://cdn.klipy.test/sm.gif", width: 480, height: 270, title: "cat typing")
        let dog = GifResult(id: "2", url: "https://cdn.klipy.test/dog.gif", previewURL: "https://cdn.klipy.test/dog-sm.gif", width: 480, height: 270, title: "dog")
        composer.attachGIF(cat)
        #expect(composer.gif == cat)
        #expect(!composer.canAttachGIF)
        composer.attachGIF(dog)
        #expect(composer.gif == cat)
        // A GIF is a picture as far as the rest of the draft is concerned.
        #expect(!composer.canAttachVideo)
        #expect(!composer.canAddPoll)
        #expect(composer.canAttach)
        #expect(composer.hasContent)
        #expect(composer.kind == .post)

        _ = await composer.submit()
        let payload = try #require(Self.postedPayloads().last)
        #expect(payload["media_urls"] as? [String] == ["https://cdn.klipy.test/md.gif"])

        composer.removeGIF()
        #expect(composer.gif == nil && composer.canAttachGIF && composer.canAttachVideo)
    }

    @Test("A draft round-trips a ready video, an uploaded voice note and a GIF, and drops an upload still in flight")
    func draftMedia() throws {
        let (composer, _) = try Self.makeComposer()
        let renditions = WorkComposer.VideoRenditions(
            masterURL: "/static/media/posts/a1/master.m3u8", posterURL: "/static/media/posts/a1/poster.jpg",
            watermarkedURL: "/static/media/posts/a1/watermarked.mp4", durationSecs: 42, width: 1280, height: 720,
            uploadID: "upl-1", creditedTo: "miiyazuko"
        )
        composer.body = "kept media"
        composer.debugStageVideo(.init(fileURL: nil, filename: "clip.mp4", mimeType: "video/mp4", state: .ready(renditions)))
        let gif = GifResult(id: "1", url: "https://cdn.klipy.test/md.gif", previewURL: "https://cdn.klipy.test/sm.gif", width: 480, height: 270, title: "cat")
        // A GIF beside a video is refused; the draft carries what the composer let on.
        composer.attachGIF(gif)
        #expect(composer.gif == nil)

        var draft = composer.draft()
        #expect(draft.video == ComposeDraft.Video(renditions: renditions, filename: "clip.mp4"))
        #expect(draft.gif == nil)
        #expect(draft.title == "kept media")
        draft.body = ""
        #expect(draft.title == "Video")

        // Add the voice and the GIF at the draft level, as an author who
        // removed the video and attached them would have.
        draft.video = nil
        draft.voice = ComposeDraft.Voice(remotePath: "/static/media/voice/v1.m4a", durationSecs: 9)
        draft.gif = gif
        #expect(draft.title == "Voice note")
        let encoded = try JSONEncoder().encode(draft)
        let decoded = try JSONDecoder().decode(ComposeDraft.self, from: encoded)
        #expect(decoded == draft)

        let (restored, _) = try Self.makeComposer()
        restored.restore(decoded)
        #expect(restored.video == nil)
        #expect(restored.voice?.remotePath == "/static/media/voice/v1.m4a")
        #expect(restored.voice?.durationSecs == 9)
        #expect(restored.voice?.fileURL == nil)
        #expect(restored.gif == gif)
        #expect(restored.kind == .voice)
        #expect(restored.canSubmit)

        // Round trip of the video itself, through a second composer.
        let (again, _) = try Self.makeComposer()
        again.restore(ComposeDraft(body: "clip", video: .init(renditions: renditions, filename: "clip.mp4")))
        #expect(again.video?.renditions == renditions)
        #expect(again.video?.fileURL == nil)
        #expect(again.draft().video?.renditions == renditions)

        // A video still in flight is not a draft's to keep.
        let (inFlight, _) = try Self.makeComposer()
        inFlight.debugStageVideo(.init(fileURL: nil, filename: "clip.mp4", mimeType: "video/mp4", state: .processing, uploadID: "upl-9"))
        #expect(inFlight.draft().video == nil)

        // A draft written before these fields existed still opens.
        let old = Data(#"{"id":"6F9619FF-8B86-D011-B42D-00C04FC964FF","savedAtMS":1000,"body":"old","segments":[],"subscriberOnly":false,"isNSFW":false,"commentGating":"everyone","attachments":[]}"#.utf8)
        let legacy = try JSONDecoder().decode(ComposeDraft.self, from: old)
        #expect(legacy.video == nil && legacy.voice == nil && legacy.gif == nil)
    }

    @Test("VideoJob decodes every state the server documents")
    func videoJobStates() throws {
        let decoder = APIClient.makeDecoder()
        let queued = try decoder.decode(VideoJob.self, from: Data(#"{"upload_id":"upl-1","status":"queued"}"#.utf8))
        #expect(queued.status == .queued && queued.uploadID == "upl-1" && !queued.isSettled)
        #expect(queued.masterURL == nil && queued.width == nil && queued.durationSecs == nil)

        let processing = try decoder.decode(VideoJob.self, from: Data(#"{"upload_id":"upl-1","status":"processing"}"#.utf8))
        #expect(processing.status == .processing && !processing.isSettled)

        let ready = try decoder.decode(VideoJob.self, from: Data(Self.readyJob.utf8))
        #expect(ready.status == .ready && ready.isSettled)
        #expect(ready.masterURL == "/static/media/posts/a1/master.m3u8")
        #expect(ready.watermarkedURL == "/static/media/posts/a1/watermarked.mp4")
        #expect(ready.durationSecs == 42.25 && ready.width == 1280 && ready.height == 720)

        let failed = try decoder.decode(VideoJob.self, from: Data(#"{"upload_id":"upl-1","status":"failed","error":"ffmpeg exited with 1"}"#.utf8))
        #expect(failed.status == .failed && failed.error == "ffmpeg exited with 1" && failed.isSettled)

        let duplicate = try decoder.decode(VideoJob.self, from: Data(#"{"status":"duplicate","duplicate_of_work_id":"w-1","duplicate_of_handle":"miiyazuko","master_url":"/static/media/posts/a1/master.m3u8","poster_url":"/static/media/posts/a1/poster.jpg","duration_secs":12.5,"width":1920,"height":1080}"#.utf8))
        #expect(duplicate.status == .duplicate && duplicate.isSettled)
        #expect(duplicate.uploadID == nil)
        #expect(duplicate.duplicateOfHandle == "miiyazuko" && duplicate.duplicateOfWorkID == "w-1")
        #expect(WorkComposer.VideoRenditions(job: duplicate, uploadID: nil)?.durationSecs == 13)

        // A word outside the contract is a decoding error, not a guess.
        #expect(throws: (any Error).self) {
            try decoder.decode(VideoJob.self, from: Data(#"{"status":"transcoding"}"#.utf8))
        }

        let voice = try decoder.decode(VoiceUpload.self, from: Data(#"{"url":"/static/media/voice/v1.m4a"}"#.utf8))
        #expect(voice.url == "/static/media/voice/v1.m4a")
        let gifs = try decoder.decode(GifSearchPage.self, from: Data(#"{"results":[{"id":"12345","url":"https://cdn.klipy.test/md.gif","preview_url":"https://cdn.klipy.test/sm.gif","width":480,"height":270,"title":"cat typing"}]}"#.utf8))
        #expect(gifs.results == [GifResult(id: "12345", url: "https://cdn.klipy.test/md.gif", previewURL: "https://cdn.klipy.test/sm.gif", width: 480, height: 270, title: "cat typing")])
    }

    @Test("GIF search surfaces the server's code, and an empty query asks for trending")
    func gifSearchErrors() async throws {
        let (_, client) = try Self.makeComposer()
        ComposerMockURLProtocol.handler = { request in
            guard request.url?.path == "/api/v1/gif/search" else { return (500, Data()) }
            return (503, Data(#"{"error":{"code":"gif_search_unavailable","message":"GIF search is not available on this server."}}"#.utf8))
        }
        await #expect(throws: APIError.api(status: 503, body: APIErrorBody(code: "gif_search_unavailable", message: "GIF search is not available on this server."))) {
            try await client.gifSearch("cats")
        }
        let asked = try #require(ComposerMockURLProtocol.recorded.last)
        #expect(asked.url?.query() == "q=cats")

        ComposerMockURLProtocol.handler = { _ in (200, Data(#"{"results":[]}"#.utf8)) }
        let trending = try await client.gifSearch("")
        #expect(trending.isEmpty)
        #expect(ComposerMockURLProtocol.recorded.last?.url?.query() == "q=")
    }

    @Test("A file that will not decode is reported and does not hide the others")
    func storeReportsUnreadableFiles() throws {
        let directory = FileManager.default.temporaryDirectory
            .appendingPathComponent("f33d3r-drafts-\(UUID().uuidString)", isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }

        let store = ComposeDraftStore(directory: directory)
        let good = ComposeDraft(body: "fine")
        try store.save(good)
        try Data("not json".utf8).write(to: directory.appendingPathComponent("broken.json"))

        let reopened = ComposeDraftStore(directory: directory)
        #expect(throws: ComposeDraftStore.UnreadableDrafts.self) { try reopened.reload() }
        #expect(reopened.drafts == [good])
    }
}

/// The web's `ComposeAER`, restated.
@Suite("Compose analysis")
struct ComposeAnalysisTests {

    @Test("Five hundred characters is a long post, read at two hundred words a minute")
    func readingTime() {
        #expect(ComposeAnalysis.readingMinutes(of: String(repeating: "a", count: 499)) == nil)
        // 100 words of five letters and a space: 600 characters, half a minute, rounds to one.
        let short = Array(repeating: "words", count: 100).joined(separator: " ")
        #expect(ComposeAnalysis.readingMinutes(of: short) == 1)
        // 700 words is three and a half minutes; JavaScript's Math.round goes up.
        let long = Array(repeating: "words", count: 700).joined(separator: " ")
        #expect(ComposeAnalysis.readingMinutes(of: long) == 4)
    }

    @Test("Two real paragraphs and three hundred characters suggest a thread; a long post does not")
    func threadSuggestion() {
        let paragraph = String(repeating: "x", count: 160)
        #expect(ComposeAnalysis.suggestsThread(paragraph + "\n\n" + paragraph))
        // One paragraph, however long, is not a thread.
        #expect(!ComposeAnalysis.suggestsThread(paragraph + paragraph))
        // A second paragraph of twenty characters or fewer does not count.
        #expect(!ComposeAnalysis.suggestsThread(paragraph + paragraph + "\n\n" + "short line"))
        // Under three hundred characters nothing is suggested.
        #expect(!ComposeAnalysis.suggestsThread("some words here\n\nsome more words here"))
        // Long wins.
        let long = String(repeating: "y", count: 300)
        #expect(!ComposeAnalysis.suggestsThread(long + "\n\n" + long))
        #expect(ComposeAnalysis.readingMinutes(of: long + "\n\n" + long) != nil)
    }

    @Test("Paragraphs split on two or more newlines and drop the blanks")
    func paragraphs() {
        #expect(ComposeAnalysis.paragraphs(of: "a\n\nb\n\n\n\nc\n\n") == ["a", "b", "c"])
        #expect(ComposeAnalysis.paragraphs(of: "one line\nstill one") == ["one line\nstill one"])
    }
}

/// This suite's own interceptor. The statics on a `URLProtocol` subclass are
/// shared by every test that uses the class, and suites run in parallel, so a
/// suite that borrowed `MalkuthMockURLProtocol` would answer that suite's
/// requests and vice versa.
final class ComposerMockURLProtocol: URLProtocol, @unchecked Sendable {
    nonisolated(unsafe) static var handler: (@Sendable (URLRequest) -> (Int, Data))?
    nonisolated(unsafe) static var recorded: [URLRequest] = []

    static func reset() {
        handler = nil
        recorded = []
    }

    static func makeSession() -> URLSession {
        let config = URLSessionConfiguration.ephemeral
        config.protocolClasses = [ComposerMockURLProtocol.self]
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
