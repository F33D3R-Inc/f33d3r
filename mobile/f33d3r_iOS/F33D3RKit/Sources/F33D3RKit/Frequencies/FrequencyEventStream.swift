import Foundation

/// One frame from a frequency's event stream, already decoded.
///
/// feed-engine holds the SSE connection, heartbeats the caller's presence to
/// the brain, re-reads the room, and emits the *narrowest* frame that says what
/// changed — falling back to a whole `room` whenever the change is not one of
/// the small ones. So `room` is always sufficient and the partial frames are
/// only an optimisation; a client that understood nothing but `room` would
/// still be correct, and a client that invents state from a partial frame would
/// not be.
public enum FrequencyEvent: Sendable, Equatable {
    /// The whole room. First frame, and on any change not covered below.
    case room(FrequencyRoom)
    case counts(listeners: Int, speakers: Int, participants: Int)
    case speakerJoined(FrequencyParticipant)
    case speakerLeft(FrequencyParticipant)
    case muted(handle: String, muted: Bool)
    /// A raised hand. Moderators only — the server does not send it to anyone
    /// whose `viewer.canModerate` is false.
    case request(FrequencyRequest)
    /// approved | declined | withdrawn
    case requestResolved(requestID: String, outcome: String)
    case cohost(handle: String, isCohost: Bool)
    case flags(locked: Bool, requestsOpen: Bool)
    case state(FrequencyState, endReason: String?)
}

/// Reads `GET /api/v1/frequencies/{id}/events` as a sequence of
/// ``FrequencyEvent``s.
///
/// Built on ``SSEFrames``, which is the one place in the app that parses the
/// wire format. An `event:` name this build has never heard of is dropped, not
/// thrown: a server that grows a frame must not break a client that shipped
/// before it.
public enum FrequencyEventStream {

    public static func open(id: String, client: APIClient) async throws -> AsyncThrowingStream<FrequencyEvent, Error> {
        let frames = try await SSEFrames.open(.frequencyEvents(id: id), client: client)
        let decoder = APIClient.makeDecoder()
        return AsyncThrowingStream { continuation in
            let task = Task {
                do {
                    for try await frame in frames {
                        if let parsed = parse(event: frame.event, data: frame.data, decoder: decoder) {
                            continuation.yield(parsed)
                        }
                    }
                    continuation.finish()
                } catch {
                    continuation.finish(throwing: error)
                }
            }
            continuation.onTermination = { _ in task.cancel() }
        }
    }

    static func parse(event: String, data: String, decoder: JSONDecoder) -> FrequencyEvent? {
        let payload = Data(data.utf8)
        switch event {
        case "room":
            return (try? decoder.decode(FrequencyRoom.self, from: payload)).map { .room($0) }
        case "counts":
            struct Counts: Decodable {
                let listeners: Int
                let speakers: Int
                let participants: Int
            }
            return (try? decoder.decode(Counts.self, from: payload)).map {
                .counts(listeners: $0.listeners, speakers: $0.speakers, participants: $0.participants)
            }
        case "speaker_joined":
            return (try? decoder.decode(FrequencyParticipant.self, from: payload)).map { .speakerJoined($0) }
        case "speaker_left":
            return (try? decoder.decode(FrequencyParticipant.self, from: payload)).map { .speakerLeft($0) }
        case "muted":
            struct Muted: Decodable {
                let handle: String
                let muted: Bool
            }
            return (try? decoder.decode(Muted.self, from: payload)).map { .muted(handle: $0.handle, muted: $0.muted) }
        case "request":
            return (try? decoder.decode(FrequencyRequest.self, from: payload)).map { .request($0) }
        case "request_resolved":
            struct Resolved: Decodable {
                let requestID: String
                let outcome: String
                enum CodingKeys: String, CodingKey {
                    case requestID = "request_id"
                    case outcome
                }
            }
            return (try? decoder.decode(Resolved.self, from: payload)).map {
                .requestResolved(requestID: $0.requestID, outcome: $0.outcome)
            }
        case "cohost":
            struct Cohost: Decodable {
                let handle: String
                let isCohost: Bool
                enum CodingKeys: String, CodingKey {
                    case handle
                    case isCohost = "is_cohost"
                }
            }
            return (try? decoder.decode(Cohost.self, from: payload)).map { .cohost(handle: $0.handle, isCohost: $0.isCohost) }
        case "flags":
            struct Flags: Decodable {
                let locked: Bool
                let requestsOpen: Bool
                enum CodingKeys: String, CodingKey {
                    case locked
                    case requestsOpen = "requests_open"
                }
            }
            return (try? decoder.decode(Flags.self, from: payload)).map { .flags(locked: $0.locked, requestsOpen: $0.requestsOpen) }
        case "state":
            struct State: Decodable {
                let state: String
                let endReason: String?
                enum CodingKeys: String, CodingKey {
                    case state
                    case endReason = "end_reason"
                }
            }
            guard let decoded = try? decoder.decode(State.self, from: payload),
                  let value = FrequencyState(rawValue: decoded.state)
            else { return nil }
            return .state(value, endReason: decoded.endReason)
        default:
            return nil
        }
    }
}

public extension FrequencyRoom {

    /// The room after one frame, as a pure function of `(room, event)`.
    ///
    /// Every partial frame lands here and nowhere else, so what a `cohost` or a
    /// `request_resolved` does to a room is one testable expression rather than
    /// something a store does on the way past. Nothing here invents a value the
    /// server did not send: a frame carries the new truth, this applies it.
    ///
    /// The viewer's own `role`, `muted` and capability flags are **not** derived
    /// from partial frames — a `muted` frame carries a handle and this type does
    /// not know which handle is the reader's. Those come from a `room` frame or
    /// from the room a mutation answered with, both of which the server built
    /// for this reader specifically.
    func applying(_ event: FrequencyEvent) -> FrequencyRoom {
        var room = self
        switch event {
        case .room(let fresh):
            return fresh

        case .counts(let listeners, let speakers, let participants):
            room.frequency.listenerCount = listeners
            room.frequency.speakerCount = speakers
            room.frequency.participantCount = participants

        case .speakerJoined(let participant):
            if let index = room.speakers.firstIndex(where: { $0.author.handle == participant.author.handle }) {
                room.speakers[index] = participant
            } else {
                room.speakers.append(participant)
            }

        case .speakerLeft(let participant):
            room.speakers.removeAll { $0.author.handle == participant.author.handle }

        case .muted(let handle, let muted):
            if let index = room.speakers.firstIndex(where: { $0.author.handle == handle }) {
                room.speakers[index].muted = muted
            }

        case .request(let request):
            if let index = room.requests.firstIndex(where: { $0.id == request.id }) {
                room.requests[index] = request
            } else {
                room.requests.append(request)
            }

        case .requestResolved(let requestID, _):
            room.requests.removeAll { $0.id == requestID }
            if room.viewer?.request?.id == requestID {
                room.viewer?.request = nil
            }

        case .cohost(let handle, let isCohost):
            if let index = room.speakers.firstIndex(where: { $0.author.handle == handle }) {
                room.speakers[index].role = isCohost ? FrequencyRole.cohost.rawValue : FrequencyRole.speaker.rawValue
                if isCohost {
                    if !room.cohosts.contains(where: { $0.handle == handle }) {
                        room.cohosts.append(room.speakers[index].author)
                    }
                } else {
                    room.cohosts.removeAll { $0.handle == handle }
                }
            } else if !isCohost {
                room.cohosts.removeAll { $0.handle == handle }
            }

        case .flags(let locked, let requestsOpen):
            room.frequency.locked = locked
            room.frequency.requestsOpen = requestsOpen

        case .state(let state, let endReason):
            room.frequency.state = state.rawValue
            room.frequency.endReason = endReason
        }
        return room
    }
}
