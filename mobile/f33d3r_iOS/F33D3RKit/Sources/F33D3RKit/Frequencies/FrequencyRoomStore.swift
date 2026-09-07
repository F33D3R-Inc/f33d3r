import Foundation
import Observation

/// One audio room, kept current by its event stream.
///
/// The listener, the speaker and the host all read through this, and it holds
/// exactly one piece of application state: the ``FrequencyRoom`` the server
/// last sent. Nothing is inferred. Raising a hand does not add a hand locally,
/// muting does not grey a face locally, ending does not close the room
/// locally — each of those calls the server, and the room that comes back (or
/// the frame that follows it) is the one drawn. That is why every control in
/// the UI is gated on ``FrequencyRoom/viewer``'s capability flags rather than
/// on a role the client worked out: the server knows about blocks, verity
/// tiers, a full stage and a locked room, and this does not.
///
/// The stream is the runtime. A drop is reconnected with a backoff capped at
/// thirty seconds, and the room is re-read before every reconnect, because it
/// may have ended while the connection was down.
@MainActor
@Observable
public final class FrequencyRoomStore {

    public enum Phase: Equatable, Sendable {
        case idle
        case loading
        /// Open and current. A scheduled room that has not started is here too:
        /// the stream is what tells the client it began.
        case live
        case ended(reason: String?)
        case failed(String)
    }

    public let frequencyID: String
    public private(set) var room: FrequencyRoom?
    public private(set) var phase: Phase = .idle
    /// True while the event stream is open. False briefly during a reconnect.
    public private(set) var isConnected = false
    /// Set when a call fails, for the room to show. Cleared by the next call.
    public private(set) var lastError: String?

    private let client: APIClient
    private var streamTask: Task<Void, Never>?

    public init(id: String, client: APIClient) {
        self.frequencyID = id
        self.client = client
    }

    /// The room's own convenience reads, so a screen does not reach through
    /// three optionals to ask a yes/no question.
    public var frequency: Frequency? { room?.frequency }
    public var viewer: FrequencyViewer? { room?.viewer }
    public var speakers: [FrequencyParticipant] { room?.speakers ?? [] }
    public var requests: [FrequencyRequest] { room?.requests ?? [] }
    public var isEnded: Bool { if case .ended = phase { return true } else { return false } }

    // MARK: - Connection

    /// Reads the room and opens its stream. Safe to call again.
    public func connect() async {
        if streamTask != nil { return }
        phase = .loading
        do {
            apply(try await client.frequency(id: frequencyID))
        } catch {
            phase = .failed(Self.message(error))
            return
        }
        guard phase == .live else { return }
        streamTask = Task { [weak self] in await self?.consume() }
    }

    public func disconnect() {
        streamTask?.cancel()
        streamTask = nil
        isConnected = false
    }

    /// Reads the room again from the server, without touching the stream.
    public func reload() async {
        do {
            apply(try await client.frequency(id: frequencyID))
        } catch {
            lastError = Self.message(error)
        }
    }

    private func apply(_ room: FrequencyRoom) {
        self.room = room
        phase = room.frequency.isOver ? .ended(reason: room.frequency.endReason) : .live
    }

    private func consume() async {
        var backoff: Duration = .seconds(1)
        while !Task.isCancelled, phase == .live {
            do {
                let events = try await FrequencyEventStream.open(id: frequencyID, client: client)
                isConnected = true
                backoff = .seconds(1)
                for try await event in events {
                    handle(event)
                    if phase != .live { break }
                }
                isConnected = false
                if phase != .live { return }
            } catch {
                isConnected = false
                if Task.isCancelled { return }
            }
            // Re-read before reconnecting: the room may have ended while the
            // connection was down, and a stream reopened on a dead room is a
            // reconnect loop nobody sees.
            if let fresh = try? await client.frequency(id: frequencyID) {
                apply(fresh)
                if phase != .live { return }
            }
            try? await Task.sleep(for: backoff)
            backoff = min(backoff * 2, .seconds(30))
        }
    }

    /// One frame. The room it produces is ``FrequencyRoom/applying(_:)``'s
    /// answer and nothing else; the only thing decided here is whether the
    /// room is still worth listening to.
    private func handle(_ event: FrequencyEvent) {
        guard let current = room else { return }
        room = current.applying(event)
        if case .state(let state, let reason) = event, state.isOver {
            phase = .ended(reason: reason)
            disconnect()
        }
    }

    // MARK: - Actions
    //
    // Each one posts the event and replaces the room with what the server
    // answered. None of them edits the room first.

    public func join() async {
        await run { try await self.client.frequencyJoin(id: self.frequencyID) }
    }

    /// Leaves the room and stops listening. The server answers `204`, so the
    /// state afterwards is a fresh read rather than a guess.
    public func leave() async {
        lastError = nil
        do {
            try await client.frequencyLeave(id: frequencyID)
        } catch {
            lastError = Self.message(error)
            return
        }
        disconnect()
        if let fresh = try? await client.frequency(id: frequencyID) { room = fresh }
        if phase == .live { phase = .idle }
    }

    public func start() async {
        await run { try await self.client.frequencyStart(id: self.frequencyID) }
    }

    public func end() async {
        await run { try await self.client.frequencyEnd(id: self.frequencyID) }
        if room?.frequency.isOver == true { disconnect() }
    }

    public func cancel() async {
        await run { try await self.client.frequencyCancel(id: self.frequencyID) }
        if room?.frequency.isOver == true { disconnect() }
    }

    public func update(_ draft: FrequencyDraft) async {
        await run { try await self.client.frequencyUpdate(id: self.frequencyID, draft: draft) }
    }

    public func schedule(at date: Date?) async {
        await run { try await self.client.frequencySchedule(id: self.frequencyID, at: date) }
    }

    /// Raises this reader's hand. The request answers alone, so the room —
    /// which carries `viewer.request` — is read again rather than patched.
    public func requestMic(reason: String = "") async {
        lastError = nil
        do {
            _ = try await client.frequencyRequestMic(id: frequencyID, reason: reason)
        } catch {
            lastError = Self.message(error)
            return
        }
        await reload()
    }

    public func withdrawRequest(_ requestID: String) async {
        lastError = nil
        do {
            try await client.frequencyRequestWithdraw(id: frequencyID, requestID: requestID)
        } catch {
            lastError = Self.message(error)
            return
        }
        await reload()
    }

    /// Backs someone's hand. The server answers the count it now holds, and
    /// that count — not a local increment — is what the queue shows.
    public func upvoteRequest(_ requestID: String) async {
        lastError = nil
        do {
            let upvotes = try await client.frequencyRequestUpvote(id: frequencyID, requestID: requestID)
            if let index = room?.requests.firstIndex(where: { $0.id == requestID }) {
                room?.requests[index].upvotes = upvotes
            }
        } catch {
            lastError = Self.message(error)
        }
    }

    public func approveRequest(_ requestID: String) async {
        await run { try await self.client.frequencyRequestApprove(id: self.frequencyID, requestID: requestID) }
    }

    public func declineRequest(_ requestID: String) async {
        await run { try await self.client.frequencyRequestDecline(id: self.frequencyID, requestID: requestID) }
    }

    public func setMuted(_ muted: Bool, handle: String) async {
        await run { try await self.client.frequencyMute(id: self.frequencyID, handle: handle, muted: muted) }
    }

    public func demote(handle: String) async {
        await run { try await self.client.frequencyDemote(id: self.frequencyID, handle: handle) }
    }

    public func remove(handle: String, reason: String = "") async {
        await run { try await self.client.frequencyRemove(id: self.frequencyID, handle: handle, reason: reason) }
    }

    public func block(handle: String, reason: String = "") async {
        await run { try await self.client.frequencyBlock(id: self.frequencyID, handle: handle, reason: reason) }
    }

    public func unblock(handle: String) async {
        await run { try await self.client.frequencyUnblock(id: self.frequencyID, handle: handle) }
    }

    public func setCohost(_ cohost: Bool, handle: String) async {
        await run { try await self.client.frequencyCohost(id: self.frequencyID, handle: handle, cohost: cohost) }
    }

    public func setLocked(_ locked: Bool) async {
        await run { try await self.client.frequencyLock(id: self.frequencyID, locked: locked) }
    }

    public func setRequestsOpen(_ open: Bool) async {
        await run { try await self.client.frequencyRequestsOpen(id: self.frequencyID, open: open) }
    }

    /// Runs a room-answering call and installs its answer whole.
    private func run(_ call: @escaping () async throws -> FrequencyRoom) async {
        lastError = nil
        do {
            apply(try await call())
        } catch {
            lastError = Self.message(error)
        }
    }

    /// The server's own words. The brain's error codes (`locked`,
    /// `speakers_full`, `verity_tier_too_low`) survive into this string on
    /// purpose — they are the difference between "you cannot speak" and
    /// "the stage is full", and a screen may branch on them.
    private static func message(_ error: Error) -> String {
        switch error {
        case let e as MalkuthError: return e.description
        case let e as APIError: return e.userMessage
        default: return error.localizedDescription
        }
    }
}
