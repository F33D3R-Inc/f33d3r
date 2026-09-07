import Foundation
import Testing
@testable import F33D3RKit

/// Frequencies: the wire shapes, the frame parser, and the one function that
/// turns a frame into a room.
///
/// The fixtures are the contract made checkable. `frequency_room.json` is a
/// live room hosted by @miiyazuko with a cohost, a muted speaker and a hand up;
/// `frequency_list.json` is the discover payload. A rename on the Go side
/// regenerates them and fails here, which is the only way a client finds out
/// about a rename before a device does.
///
/// One deliberate liberty in the room fixture: it carries a populated
/// `requests` queue while `viewer.can_moderate` is false. The server withholds
/// the queue from non-moderators — the fixture keeps it so the row's decode is
/// covered by the same file that covers everything else.
@Suite("Frequencies")
struct FrequencyTests {

    private static func decoder() -> JSONDecoder { ContractTests.makeDecoder() }

    // MARK: - Fixtures

    @Test("The room fixture decodes with every field intact")
    func decodesRoom() throws {
        let room = try Self.decoder().decode(FrequencyRoom.self, from: ContractTests.fixture("frequency_room"))
        let f = room.frequency

        #expect(f.id == "b3f5c1e0-7a41-4a1e-9f0d-2c8e6d5b4a10")
        #expect(f.host.handle == "miiyazuko")
        #expect(f.host.isVerified)
        #expect(f.title == "Late night in the Deep Space lane")
        #expect(f.description?.hasPrefix("Three hours") == true)
        #expect(f.stateValue == .live)
        #expect(f.isLive)
        #expect(!f.isScheduled)
        #expect(!f.isOver)
        #expect(f.visibility == "public")
        #expect(f.language == "en")
        #expect(!f.isNSFW)
        #expect(f.scheduledAt == nil)
        #expect(f.startedAt != nil)
        #expect(f.endedAt == nil)
        #expect(f.endReason == nil)
        #expect(f.recordingEnabled)
        #expect(f.replayStatus == "none")
        #expect(f.maxSpeakers == 10)
        #expect(f.maxListeners == 1000)
        #expect(f.requestsOpen)
        #expect(!f.locked)
        #expect(f.listenerCount == 312)
        #expect(f.speakerCount == 3)
        #expect(f.participantCount == 315)
        #expect(f.audioUnavailable?.hasPrefix("Audio is not flowing yet") == true)
        #expect(f.listenerLabel == "312 listening")

        // Host first, then cohosts, then speakers — the brain's order, kept.
        #expect(room.speakers.map(\.author.handle) == ["miiyazuko", "tehanibentley", "dev"])
        #expect(room.speakers.map(\.roleLabel) == ["Host", "Co-host", "Speaker"])
        #expect(room.speakers[0].isHost)
        #expect(room.speakers[1].isCohost)
        #expect(room.speakers.allSatisfy { $0.isSpeaking })
        #expect(room.speakers[2].muted)
        #expect(room.speakers.allSatisfy { $0.present })
        #expect(room.speakers[0].id == "miiyazuko")

        #expect(room.cohosts.map(\.handle) == ["tehanibentley"])

        #expect(room.requests.count == 1)
        let request = try #require(room.requests.first)
        #expect(request.id == "1d9c4b77-2f30-4a8c-b6de-9f0a1c2b3d4e")
        #expect(request.author.handle == "guest")
        #expect(request.upvotes == 4)
        #expect(request.reason.hasPrefix("I ran the numbers"))

        let viewer = try #require(room.viewer)
        #expect(viewer.roleValue == .listener)
        #expect(viewer.isJoined)
        #expect(!viewer.blocked)
        #expect(!viewer.hasPendingRequest)
        #expect(viewer.canRequestMic)
        #expect(!viewer.canModerate)
        #expect(!viewer.canEnd)
        #expect(!viewer.canSpeak)
        #expect(viewer.canListen)
    }

    @Test("The list fixture decodes, and the lanes read as sent")
    func decodesList() throws {
        let list = try Self.decoder().decode(FrequencyList.self, from: ContractTests.fixture("frequency_list"))

        #expect(list.lane == "live")
        #expect(list.count == 3)
        #expect(list.frequencies.count == 3)
        #expect(list.own == nil)
        #expect(list.frequencies.filter(\.isLive).count == 2)
        #expect(list.frequencies.filter(\.isScheduled).count == 1)

        let scheduled = try #require(list.frequencies.first { $0.isScheduled })
        #expect(scheduled.host.handle == "tehanibentley")
        #expect(scheduled.scheduledAt != nil)
        #expect(scheduled.startedAt == nil)
        #expect(scheduled.visibility == "followers")
        #expect(scheduled.listenerLabel == "No one listening yet")

        let locked = try #require(list.frequencies.first { $0.locked })
        #expect(locked.host.handle == "admin")
        #expect(!locked.requestsOpen)
        #expect(locked.audioUnavailable == nil)
        #expect(locked.listenerLabel == "1 listening")
    }

    // MARK: - Absent keys

    @Test("A summary carrying nothing but an id and a host still decodes")
    func decodesSparseSummary() throws {
        let json = Data(#"{"id":"x","host":{"handle":"dev"}}"#.utf8)
        let f = try Self.decoder().decode(Frequency.self, from: json)

        #expect(f.id == "x")
        #expect(f.host.handle == "dev")
        #expect(f.title.isEmpty)
        #expect(f.description == nil)
        #expect(f.state == "live")
        #expect(f.visibility == "public")
        #expect(f.language == "en")
        #expect(!f.isNSFW)
        #expect(f.replayStatus == "none")
        #expect(f.maxSpeakers == 10)
        #expect(f.maxListeners == 1000)
        #expect(f.requestsOpen)
        #expect(!f.locked)
        #expect(f.listenerCount == 0)
        #expect(f.audioUnavailable == nil)
    }

    @Test("A room with no stage, no queue and no viewer decodes")
    func decodesSparseRoom() throws {
        let json = Data(#"{"frequency":{"id":"x","host":{"handle":"dev"},"state":"scheduled"}}"#.utf8)
        let room = try Self.decoder().decode(FrequencyRoom.self, from: json)

        #expect(room.id == "x")
        #expect(room.speakers.isEmpty)
        #expect(room.cohosts.isEmpty)
        #expect(room.requests.isEmpty)
        #expect(room.viewer == nil)
        #expect(room.frequency.isScheduled)
    }

    @Test("A viewer who has not tuned in has no role and no powers")
    func decodesSparseViewer() throws {
        let json = Data(#"{"role":null}"#.utf8)
        let viewer = try Self.decoder().decode(FrequencyViewer.self, from: json)

        #expect(viewer.roleValue == nil)
        #expect(!viewer.isJoined)
        #expect(!viewer.canRequestMic)
        #expect(!viewer.canListen)
    }

    @Test("A state this build has never heard of is not treated as over")
    func unknownState() throws {
        let json = Data(#"{"id":"x","host":{"handle":"dev"},"state":"reverberating"}"#.utf8)
        let f = try Self.decoder().decode(Frequency.self, from: json)

        #expect(f.stateValue == nil)
        #expect(!f.isOver)
        #expect(!f.isLive)
    }

    @Test("Every state knows whether the room is over")
    func stateIsOver() {
        #expect(FrequencyState.allCases.filter(\.isOver).map(\.rawValue).sorted()
            == ["archived", "cancelled", "ended", "failed", "moderation_terminated", "processing_replay"])
        #expect(!FrequencyState.ending.isOver)
        #expect(!FrequencyState.starting.isOver)
    }

    // MARK: - applying

    private func loadedRoom() throws -> FrequencyRoom {
        try Self.decoder().decode(FrequencyRoom.self, from: ContractTests.fixture("frequency_room"))
    }

    @Test("A room frame replaces everything")
    func applyRoom() throws {
        let room = try loadedRoom()
        var replacement = room
        replacement.frequency.listenerCount = 9
        replacement.speakers = []
        #expect(room.applying(.room(replacement)) == replacement)
    }

    @Test("A counts frame moves the three counts and nothing else")
    func applyCounts() throws {
        let room = try loadedRoom()
        let next = room.applying(.counts(listeners: 400, speakers: 4, participants: 404))

        #expect(next.frequency.listenerCount == 400)
        #expect(next.frequency.speakerCount == 4)
        #expect(next.frequency.participantCount == 404)
        #expect(next.frequency.listenerLabel == "400 listening")
        #expect(next.speakers == room.speakers)
        #expect(next.requests == room.requests)
    }

    @Test("A speaker joining is appended, and joining twice replaces the row")
    func applySpeakerJoined() throws {
        let room = try loadedRoom()
        let arriving = FrequencyParticipant(
            author: WorkAuthor(handle: "guest", displayName: "Guest"),
            role: FrequencyRole.speaker.rawValue
        )
        let next = room.applying(.speakerJoined(arriving))
        #expect(next.speakers.map(\.author.handle) == ["miiyazuko", "tehanibentley", "dev", "guest"])

        // The same handle again is an update in place, not a duplicate face.
        let muted = FrequencyParticipant(
            author: WorkAuthor(handle: "guest", displayName: "Guest"),
            role: FrequencyRole.speaker.rawValue,
            muted: true
        )
        let again = next.applying(.speakerJoined(muted))
        #expect(again.speakers.count == 4)
        #expect(again.speakers.last?.muted == true)
    }

    @Test("A speaker leaving is removed by handle")
    func applySpeakerLeft() throws {
        let room = try loadedRoom()
        let leaving = try #require(room.speakers.first { $0.author.handle == "dev" })
        let next = room.applying(.speakerLeft(leaving))

        #expect(next.speakers.map(\.author.handle) == ["miiyazuko", "tehanibentley"])
        // Someone who was never on stage leaving changes nothing.
        #expect(next.applying(.speakerLeft(leaving)) == next)
    }

    @Test("A muted frame flips one speaker's glyph")
    func applyMuted() throws {
        let room = try loadedRoom()
        let unmuted = room.applying(.muted(handle: "dev", muted: false))
        #expect(unmuted.speakers[2].muted == false)
        #expect(unmuted.speakers[0].muted == false)

        let hostMuted = unmuted.applying(.muted(handle: "miiyazuko", muted: true))
        #expect(hostMuted.speakers[0].muted)

        // A handle nobody on stage answers to is not an error.
        #expect(hostMuted.applying(.muted(handle: "nobody", muted: true)) == hostMuted)
    }

    @Test("A request frame appends, and the same id lands once")
    func applyRequest() throws {
        let room = try loadedRoom()
        let raised = FrequencyRequest(
            id: "req-2",
            author: WorkAuthor(handle: "admin", displayName: "Admin"),
            reason: "One correction.",
            upvotes: 0
        )
        let next = room.applying(.request(raised))
        #expect(next.requests.map(\.id) == ["1d9c4b77-2f30-4a8c-b6de-9f0a1c2b3d4e", "req-2"])

        let upvoted = FrequencyRequest(
            id: "req-2",
            author: WorkAuthor(handle: "admin", displayName: "Admin"),
            reason: "One correction.",
            upvotes: 7
        )
        let again = next.applying(.request(upvoted))
        #expect(again.requests.count == 2)
        #expect(again.requests.last?.upvotes == 7)
    }

    @Test("A resolved request leaves the queue, and the viewer's own hand comes down")
    func applyRequestResolved() throws {
        var room = try loadedRoom()
        let pending = try #require(room.requests.first)
        room.viewer?.request = pending

        for outcome in ["approved", "declined", "withdrawn"] {
            let next = room.applying(.requestResolved(requestID: pending.id, outcome: outcome))
            #expect(next.requests.isEmpty)
            #expect(next.viewer?.request == nil)
        }

        // Someone else's request resolving does not lower this reader's hand.
        let other = room.applying(.requestResolved(requestID: "req-other", outcome: "declined"))
        #expect(other.requests.count == 1)
        #expect(other.viewer?.request != nil)
    }

    @Test("A cohost frame moves the role and the cohost list together")
    func applyCohost() throws {
        let room = try loadedRoom()

        let promoted = room.applying(.cohost(handle: "dev", isCohost: true))
        #expect(promoted.speakers[2].roleValue == .cohost)
        #expect(promoted.speakers[2].roleLabel == "Co-host")
        #expect(promoted.cohosts.map(\.handle) == ["tehanibentley", "dev"])
        // Promoting twice does not list them twice.
        #expect(promoted.applying(.cohost(handle: "dev", isCohost: true)).cohosts.count == 2)

        let demoted = promoted.applying(.cohost(handle: "tehanibentley", isCohost: false))
        #expect(demoted.speakers[1].roleValue == .speaker)
        #expect(demoted.cohosts.map(\.handle) == ["dev"])
    }

    @Test("A flags frame sets the lock and the queue")
    func applyFlags() throws {
        let room = try loadedRoom()
        let next = room.applying(.flags(locked: true, requestsOpen: false))

        #expect(next.frequency.locked)
        #expect(!next.frequency.requestsOpen)
        #expect(next.speakers == room.speakers)
    }

    @Test("A state frame carries the room's ending, reason and all")
    func applyState() throws {
        let room = try loadedRoom()

        let ended = room.applying(.state(.ended, endReason: "host ended"))
        #expect(ended.frequency.stateValue == .ended)
        #expect(ended.frequency.endReason == "host ended")
        #expect(ended.frequency.isOver)
        #expect(!ended.frequency.isLive)

        let terminated = room.applying(.state(.moderationTerminated, endReason: nil))
        #expect(terminated.frequency.isOver)
        #expect(terminated.frequency.endReason == nil)

        let starting = room.applying(.state(.starting, endReason: nil))
        #expect(!starting.frequency.isOver)
    }

    // MARK: - Draft encoding

    @Test("A draft encodes to the field names the create and update events read")
    func draftFields() throws {
        var draft = FrequencyDraft()
        draft.title = "  Late night  "
        draft.description = "Nothing urgent."
        draft.visibility = "followers"
        draft.language = "ja"
        draft.isNSFW = true
        draft.recordingEnabled = true
        draft.maxSpeakers = 6
        draft.maxListeners = 250

        let fields = draft.eventFields()
        #expect(fields.map(\.0) == [
            "title", "description", "visibility", "language",
            "is_nsfw", "recording_enabled", "max_speakers", "max_listeners",
        ])

        let json = String(decoding: CanonicalJSON.object(fields), as: UTF8.self)
        #expect(json == #"{"title":"Late night","description":"Nothing urgent.","visibility":"followers","language":"ja","is_nsfw":true,"recording_enabled":true,"max_speakers":6,"max_listeners":250}"#)
    }

    @Test("A scheduled draft carries scheduled_at as RFC 3339; an unscheduled one omits it")
    func draftScheduledAt() throws {
        var draft = FrequencyDraft()
        draft.title = "What we shipped"
        #expect(!draft.eventFields().contains { $0.0 == "scheduled_at" })

        draft.scheduledAt = Date(timeIntervalSince1970: 1_788_000_000)
        let fields = draft.eventFields()
        let scheduled = try #require(fields.first { $0.0 == "scheduled_at" })
        #expect(scheduled.1 == .string(APIClient.rfc3339String(draft.scheduledAt!)))
        guard case .string(let text) = scheduled.1 else {
            Issue.record("scheduled_at is not a string")
            return
        }
        #expect(text.hasSuffix("Z"))
        // And it round-trips through the decoder the client reads answers with.
        let round = try ContractTests.makeDecoder().decode(Date.self, from: Data("\"\(text)\"".utf8))
        #expect(Int(round.timeIntervalSince1970) == 1_788_000_000)
    }

    @Test("An empty title is not a room")
    func draftValidity() {
        var draft = FrequencyDraft()
        #expect(!draft.isValid)
        draft.title = "   "
        #expect(!draft.isValid)
        draft.title = "Late night"
        #expect(draft.isValid)
        draft.title = String(repeating: "a", count: 121)
        #expect(!draft.isValid)
    }

    // MARK: - Frame parsing

    private func parse(_ event: String, _ data: String) -> FrequencyEvent? {
        FrequencyEventStream.parse(event: event, data: data, decoder: Self.decoder())
    }

    @Test("The parser reads a whole room frame")
    func parseRoom() throws {
        let json = String(decoding: try ContractTests.fixture("frequency_room"), as: UTF8.self)
        guard case .room(let room)? = parse("room", json) else {
            Issue.record("room frame did not parse")
            return
        }
        #expect(room.frequency.host.handle == "miiyazuko")
        #expect(room.speakers.count == 3)
    }

    @Test("The parser reads every partial frame")
    func parsePartials() {
        #expect(parse("counts", #"{"listeners":312,"speakers":3,"participants":315}"#)
            == .counts(listeners: 312, speakers: 3, participants: 315))

        guard case .speakerJoined(let joined)? = parse(
            "speaker_joined",
            #"{"author":{"handle":"guest","display_name":"Guest"},"role":"speaker","muted":false,"present":true,"joined_at":"2026-09-06T21:44:00Z"}"#
        ) else {
            Issue.record("speaker_joined did not parse")
            return
        }
        #expect(joined.author.handle == "guest")
        #expect(joined.roleValue == .speaker)

        guard case .speakerLeft(let left)? = parse(
            "speaker_left",
            #"{"author":{"handle":"dev","display_name":"Dev"},"role":"speaker","muted":true,"present":false,"joined_at":"2026-09-06T21:31:47Z"}"#
        ) else {
            Issue.record("speaker_left did not parse")
            return
        }
        #expect(left.author.handle == "dev")
        #expect(!left.present)

        #expect(parse("muted", #"{"handle":"dev","muted":true}"#) == .muted(handle: "dev", muted: true))

        guard case .request(let request)? = parse(
            "request",
            #"{"id":"req-9","author":{"handle":"guest","display_name":"Guest"},"reason":"one thing","upvotes":2,"created_at":"2026-09-06T21:40:09Z"}"#
        ) else {
            Issue.record("request did not parse")
            return
        }
        #expect(request.id == "req-9")
        #expect(request.upvotes == 2)

        #expect(parse("request_resolved", #"{"request_id":"req-9","outcome":"approved"}"#)
            == .requestResolved(requestID: "req-9", outcome: "approved"))

        #expect(parse("cohost", #"{"handle":"dev","is_cohost":true}"#) == .cohost(handle: "dev", isCohost: true))

        #expect(parse("flags", #"{"locked":true,"requests_open":false}"#) == .flags(locked: true, requestsOpen: false))

        #expect(parse("state", #"{"state":"ended","end_reason":"host ended"}"#)
            == .state(.ended, endReason: "host ended"))
        #expect(parse("state", #"{"state":"live","end_reason":null}"#) == .state(.live, endReason: nil))
    }

    @Test("A frame this build does not know is dropped, never fatal")
    func parseUnknown() {
        #expect(parse("reverb", #"{"anything":1}"#) == nil)
        #expect(parse("", "") == nil)
        // A known name with a body that is not its shape is also dropped: the
        // next `room` frame is the recovery, not a crash.
        #expect(parse("counts", #"{"listeners":"lots"}"#) == nil)
        #expect(parse("state", #"{"state":"reverberating"}"#) == nil)
        #expect(parse("room", "not json at all") == nil)
    }
}
