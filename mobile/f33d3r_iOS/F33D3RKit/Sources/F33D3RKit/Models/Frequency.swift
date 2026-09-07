import Foundation

// Frequencies — F33D3R's live audio rooms.
//
// The wire shapes here are feed-engine's flattening of the Auralis brain's
// double-nested reply. What the brain calls a `*_pial_id` never reaches this
// file: feed-engine resolves every identity to a handle and a ``WorkAuthor``
// before it answers, and a client that held a PIAL would be a leak. Targets
// are addressed by **handle** on the way back out, for the same reason.
//
// Fields, exactly as `GET /api/v1/frequencies/{id}` sends them:
//
//   FrequencySummaryDTO → Frequency
//     id, host, title, description?, state, visibility, language, is_nsfw,
//     scheduled_at?, started_at?, ended_at?, end_reason?, recording_enabled,
//     replay_status, max_speakers, max_listeners, requests_open, locked,
//     listener_count, speaker_count, participant_count, audio_unavailable?
//   FrequencyParticipantDTO → FrequencyParticipant
//     author, role, muted, present, joined_at
//   FrequencyRequestDTO → FrequencyRequest
//     id, author, reason, upvotes, created_at
//   FrequencyViewerDTO → FrequencyViewer
//     role?, muted, present, blocked, request?, can_request_mic, can_moderate,
//     can_end, can_speak, can_listen
//   FrequencyRoomDTO → FrequencyRoom
//     frequency, speakers, cohosts, requests, viewer?
//   FrequencyListDTO → FrequencyList
//     lane, frequencies, count, own?
//
// Everything except `id` decodes through `decodeIfPresent … ?? default`, so a
// server that has not yet grown a field answers a room the client can still
// draw. Dates use the client's RFC 3339 strategy (`APIClient.makeDecoder`).

/// Where a frequency is in its life. The brain's own vocabulary.
public enum FrequencyState: String, Codable, Hashable, Sendable, CaseIterable {
    case draft
    case scheduled
    case starting
    case live
    case ending
    case ended
    case processingReplay = "processing_replay"
    case archived
    case cancelled
    case failed
    case moderationTerminated = "moderation_terminated"

    /// True once the room can no longer be listened to. `ending` is not over —
    /// the host has asked, the brain has not finished.
    public var isOver: Bool {
        switch self {
        case .ended, .cancelled, .failed, .moderationTerminated, .archived, .processingReplay:
            return true
        case .draft, .scheduled, .starting, .live, .ending:
            return false
        }
    }
}

/// What someone is in a room. Ordered as the room draws them.
public enum FrequencyRole: String, Codable, Hashable, Sendable, CaseIterable {
    case host
    case cohost
    case speaker
    case listener

    /// "Host", "Co-host", "Speaker", "Listener".
    public var label: String {
        switch self {
        case .host: return "Host"
        case .cohost: return "Co-host"
        case .speaker: return "Speaker"
        case .listener: return "Listener"
        }
    }
}

/// One audio room as the server knows it.
///
/// There is no audio transport in v1 — `audioUnavailable` is the server's own
/// sentence for why there is no sound, the same way ``LiveStream/videoUnavailable``
/// is. It is a string to be shown, not a flag to be interpreted.
public struct Frequency: Codable, Hashable, Sendable, Identifiable {
    public let id: String
    public let host: WorkAuthor
    public let title: String
    public let description: String?
    /// draft | scheduled | starting | live | ending | ended | processing_replay
    /// | archived | cancelled | failed | moderation_terminated
    public internal(set) var state: String
    /// public | followers | subscribers | private
    public let visibility: String
    public let language: String
    public let isNSFW: Bool
    public let scheduledAt: Date?
    public let startedAt: Date?
    public let endedAt: Date?
    public internal(set) var endReason: String?
    public let recordingEnabled: Bool
    /// none | processing | ready | failed
    public let replayStatus: String
    public let maxSpeakers: Int
    public let maxListeners: Int
    public internal(set) var requestsOpen: Bool
    public internal(set) var locked: Bool
    public internal(set) var listenerCount: Int
    public internal(set) var speakerCount: Int
    public internal(set) var participantCount: Int
    public let audioUnavailable: String?

    enum CodingKeys: String, CodingKey {
        case id, host, title, description, state, visibility, language, locked
        case isNSFW = "is_nsfw"
        case scheduledAt = "scheduled_at"
        case startedAt = "started_at"
        case endedAt = "ended_at"
        case endReason = "end_reason"
        case recordingEnabled = "recording_enabled"
        case replayStatus = "replay_status"
        case maxSpeakers = "max_speakers"
        case maxListeners = "max_listeners"
        case requestsOpen = "requests_open"
        case listenerCount = "listener_count"
        case speakerCount = "speaker_count"
        case participantCount = "participant_count"
        case audioUnavailable = "audio_unavailable"
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        host = try c.decode(WorkAuthor.self, forKey: .host)
        title = try c.decodeIfPresent(String.self, forKey: .title) ?? ""
        description = try c.decodeIfPresent(String.self, forKey: .description)
        state = try c.decodeIfPresent(String.self, forKey: .state) ?? "live"
        visibility = try c.decodeIfPresent(String.self, forKey: .visibility) ?? "public"
        language = try c.decodeIfPresent(String.self, forKey: .language) ?? "en"
        isNSFW = try c.decodeIfPresent(Bool.self, forKey: .isNSFW) ?? false
        scheduledAt = try c.decodeIfPresent(Date.self, forKey: .scheduledAt)
        startedAt = try c.decodeIfPresent(Date.self, forKey: .startedAt)
        endedAt = try c.decodeIfPresent(Date.self, forKey: .endedAt)
        endReason = try c.decodeIfPresent(String.self, forKey: .endReason)
        recordingEnabled = try c.decodeIfPresent(Bool.self, forKey: .recordingEnabled) ?? false
        replayStatus = try c.decodeIfPresent(String.self, forKey: .replayStatus) ?? "none"
        maxSpeakers = try c.decodeIfPresent(Int.self, forKey: .maxSpeakers) ?? 10
        maxListeners = try c.decodeIfPresent(Int.self, forKey: .maxListeners) ?? 1000
        requestsOpen = try c.decodeIfPresent(Bool.self, forKey: .requestsOpen) ?? true
        locked = try c.decodeIfPresent(Bool.self, forKey: .locked) ?? false
        listenerCount = try c.decodeIfPresent(Int.self, forKey: .listenerCount) ?? 0
        speakerCount = try c.decodeIfPresent(Int.self, forKey: .speakerCount) ?? 0
        participantCount = try c.decodeIfPresent(Int.self, forKey: .participantCount) ?? 0
        audioUnavailable = try c.decodeIfPresent(String.self, forKey: .audioUnavailable)
    }

    public init(
        id: String,
        host: WorkAuthor,
        title: String,
        description: String? = nil,
        state: String = "live",
        visibility: String = "public",
        language: String = "en",
        isNSFW: Bool = false,
        scheduledAt: Date? = nil,
        startedAt: Date? = nil,
        endedAt: Date? = nil,
        endReason: String? = nil,
        recordingEnabled: Bool = false,
        replayStatus: String = "none",
        maxSpeakers: Int = 10,
        maxListeners: Int = 1000,
        requestsOpen: Bool = true,
        locked: Bool = false,
        listenerCount: Int = 0,
        speakerCount: Int = 0,
        participantCount: Int = 0,
        audioUnavailable: String? = nil
    ) {
        self.id = id
        self.host = host
        self.title = title
        self.description = description
        self.state = state
        self.visibility = visibility
        self.language = language
        self.isNSFW = isNSFW
        self.scheduledAt = scheduledAt
        self.startedAt = startedAt
        self.endedAt = endedAt
        self.endReason = endReason
        self.recordingEnabled = recordingEnabled
        self.replayStatus = replayStatus
        self.maxSpeakers = maxSpeakers
        self.maxListeners = maxListeners
        self.requestsOpen = requestsOpen
        self.locked = locked
        self.listenerCount = listenerCount
        self.speakerCount = speakerCount
        self.participantCount = participantCount
        self.audioUnavailable = audioUnavailable
    }

    /// The state as a value, or nil for a word this client does not know —
    /// which is a server ahead of the app, not an error.
    public var stateValue: FrequencyState? { FrequencyState(rawValue: state) }

    public var isLive: Bool { stateValue == .live }

    public var isScheduled: Bool { stateValue == .scheduled }

    /// True once the room can no longer be listened to. A state this build
    /// does not know is not assumed to be over.
    public var isOver: Bool { stateValue?.isOver ?? false }

    /// "312 listening" — the line under the title.
    public var listenerLabel: String {
        switch listenerCount {
        case 0: return "No one listening yet"
        case 1: return "1 listening"
        default: return "\(Counts.exact(listenerCount)) listening"
        }
    }
}

/// Someone on the stage, or in the room.
public struct FrequencyParticipant: Codable, Hashable, Sendable, Identifiable {
    public var id: String { author.handle }

    public let author: WorkAuthor
    /// host | cohost | speaker | listener
    public internal(set) var role: String
    public internal(set) var muted: Bool
    /// False when their heartbeat has lapsed — drawn dimmed, not removed.
    public internal(set) var present: Bool
    public let joinedAt: Date

    enum CodingKeys: String, CodingKey {
        case author, role, muted, present
        case joinedAt = "joined_at"
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        author = try c.decode(WorkAuthor.self, forKey: .author)
        role = try c.decodeIfPresent(String.self, forKey: .role) ?? "listener"
        muted = try c.decodeIfPresent(Bool.self, forKey: .muted) ?? false
        present = try c.decodeIfPresent(Bool.self, forKey: .present) ?? true
        joinedAt = try c.decodeIfPresent(Date.self, forKey: .joinedAt) ?? Date()
    }

    public init(
        author: WorkAuthor,
        role: String = "listener",
        muted: Bool = false,
        present: Bool = true,
        joinedAt: Date = Date()
    ) {
        self.author = author
        self.role = role
        self.muted = muted
        self.present = present
        self.joinedAt = joinedAt
    }

    public var roleValue: FrequencyRole? { FrequencyRole(rawValue: role) }

    /// "Host" / "Co-host" / "Speaker" / "Listener" — the badge under the face.
    public var roleLabel: String { roleValue?.label ?? role.capitalized }

    public var isHost: Bool { roleValue == .host }
    public var isCohost: Bool { roleValue == .cohost }
    /// Host and cohosts speak too; this is "may be heard", not "is on stage".
    public var isSpeaking: Bool {
        switch roleValue {
        case .host, .cohost, .speaker: return true
        default: return false
        }
    }
}

/// A raised hand, waiting on a moderator.
public struct FrequencyRequest: Codable, Hashable, Sendable, Identifiable {
    public let id: String
    public let author: WorkAuthor
    public let reason: String
    public internal(set) var upvotes: Int
    public let createdAt: Date

    enum CodingKeys: String, CodingKey {
        case id, author, reason, upvotes
        case createdAt = "created_at"
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        author = try c.decode(WorkAuthor.self, forKey: .author)
        reason = try c.decodeIfPresent(String.self, forKey: .reason) ?? ""
        upvotes = try c.decodeIfPresent(Int.self, forKey: .upvotes) ?? 0
        createdAt = try c.decodeIfPresent(Date.self, forKey: .createdAt) ?? Date()
    }

    public init(
        id: String,
        author: WorkAuthor,
        reason: String = "",
        upvotes: Int = 0,
        createdAt: Date = Date()
    ) {
        self.id = id
        self.author = author
        self.reason = reason
        self.upvotes = upvotes
        self.createdAt = createdAt
    }
}

/// What *this* reader may do in this room.
///
/// Every control is gated on one of these and never on a role the client
/// worked out for itself: the server knows about blocks, verity tiers, a full
/// stage and a locked room, and the client knows none of it.
public struct FrequencyViewer: Codable, Hashable, Sendable {
    /// host | cohost | speaker | listener, or nil when they have not tuned in.
    public internal(set) var role: String?
    public internal(set) var muted: Bool
    public internal(set) var present: Bool
    public let blocked: Bool
    /// Their own pending request, if they have one raised.
    public internal(set) var request: FrequencyRequest?
    public let canRequestMic: Bool
    public let canModerate: Bool
    public let canEnd: Bool
    public let canSpeak: Bool
    public let canListen: Bool

    enum CodingKeys: String, CodingKey {
        case role, muted, present, blocked, request
        case canRequestMic = "can_request_mic"
        case canModerate = "can_moderate"
        case canEnd = "can_end"
        case canSpeak = "can_speak"
        case canListen = "can_listen"
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        role = try c.decodeIfPresent(String.self, forKey: .role)
        muted = try c.decodeIfPresent(Bool.self, forKey: .muted) ?? false
        present = try c.decodeIfPresent(Bool.self, forKey: .present) ?? false
        blocked = try c.decodeIfPresent(Bool.self, forKey: .blocked) ?? false
        request = try c.decodeIfPresent(FrequencyRequest.self, forKey: .request)
        canRequestMic = try c.decodeIfPresent(Bool.self, forKey: .canRequestMic) ?? false
        canModerate = try c.decodeIfPresent(Bool.self, forKey: .canModerate) ?? false
        canEnd = try c.decodeIfPresent(Bool.self, forKey: .canEnd) ?? false
        canSpeak = try c.decodeIfPresent(Bool.self, forKey: .canSpeak) ?? false
        canListen = try c.decodeIfPresent(Bool.self, forKey: .canListen) ?? false
    }

    public init(
        role: String? = nil,
        muted: Bool = false,
        present: Bool = false,
        blocked: Bool = false,
        request: FrequencyRequest? = nil,
        canRequestMic: Bool = false,
        canModerate: Bool = false,
        canEnd: Bool = false,
        canSpeak: Bool = false,
        canListen: Bool = true
    ) {
        self.role = role
        self.muted = muted
        self.present = present
        self.blocked = blocked
        self.request = request
        self.canRequestMic = canRequestMic
        self.canModerate = canModerate
        self.canEnd = canEnd
        self.canSpeak = canSpeak
        self.canListen = canListen
    }

    public var roleValue: FrequencyRole? { role.flatMap(FrequencyRole.init(rawValue:)) }

    /// True once they have tuned in and the server counts them present.
    public var isJoined: Bool { role != nil && present }

    /// True while their hand is up.
    public var hasPendingRequest: Bool { request != nil }
}

/// `GET /api/v1/frequencies/{id}`.
///
/// `speakers` arrives host first, then cohosts, then speakers — the brain's
/// order, kept as sent. `requests` is pending-only and is empty unless
/// `viewer.canModerate`.
public struct FrequencyRoom: Codable, Hashable, Sendable {
    public internal(set) var frequency: Frequency
    public internal(set) var speakers: [FrequencyParticipant]
    public internal(set) var cohosts: [WorkAuthor]
    public internal(set) var requests: [FrequencyRequest]
    public internal(set) var viewer: FrequencyViewer?

    enum CodingKeys: String, CodingKey {
        case frequency, speakers, cohosts, requests, viewer
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        frequency = try c.decode(Frequency.self, forKey: .frequency)
        speakers = try c.decodeIfPresent([FrequencyParticipant].self, forKey: .speakers) ?? []
        cohosts = try c.decodeIfPresent([WorkAuthor].self, forKey: .cohosts) ?? []
        requests = try c.decodeIfPresent([FrequencyRequest].self, forKey: .requests) ?? []
        viewer = try c.decodeIfPresent(FrequencyViewer.self, forKey: .viewer)
    }

    public init(
        frequency: Frequency,
        speakers: [FrequencyParticipant] = [],
        cohosts: [WorkAuthor] = [],
        requests: [FrequencyRequest] = [],
        viewer: FrequencyViewer? = nil
    ) {
        self.frequency = frequency
        self.speakers = speakers
        self.cohosts = cohosts
        self.requests = requests
        self.viewer = viewer
    }

    public var id: String { frequency.id }
}

/// `GET /api/v1/frequencies?lane=…`.
public struct FrequencyList: Codable, Hashable, Sendable {
    /// live | scheduled | ended | mine
    public let lane: String
    public let frequencies: [Frequency]
    public let count: Int
    /// The caller's own open room, so the UI can offer to return to it.
    public let own: Frequency?

    enum CodingKeys: String, CodingKey {
        case lane, frequencies, count, own
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        lane = try c.decodeIfPresent(String.self, forKey: .lane) ?? "live"
        frequencies = try c.decodeIfPresent([Frequency].self, forKey: .frequencies) ?? []
        count = try c.decodeIfPresent(Int.self, forKey: .count) ?? frequencies.count
        own = try c.decodeIfPresent(Frequency.self, forKey: .own)
    }

    public init(lane: String, frequencies: [Frequency], count: Int? = nil, own: Frequency? = nil) {
        self.lane = lane
        self.frequencies = frequencies
        self.count = count ?? frequencies.count
        self.own = own
    }
}

/// What the start sheet collects, as `frequency_create` and `frequency_update`
/// take it.
///
/// `scheduledAt` nil means "now" on create and "leave it alone" on update; the
/// schedule is cleared with ``APIClient/frequencySchedule(id:at:)`` instead,
/// which is the event that owns that transition.
public struct FrequencyDraft: Sendable, Equatable {
    public var title = ""
    public var description = ""
    /// public | followers | subscribers | private
    public var visibility = "public"
    public var language = "en"
    public var isNSFW = false
    public var recordingEnabled = false
    public var scheduledAt: Date?
    public var maxSpeakers = 10
    public var maxListeners = 1000

    public init() {}

    public var isValid: Bool {
        let trimmed = title.trimmingCharacters(in: .whitespacesAndNewlines)
        return !trimmed.isEmpty && trimmed.count <= 120
    }
}
