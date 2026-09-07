import Foundation
import Observation

/// One room, kept current by its event stream.
///
/// Both the viewer and the broadcaster read through this. Every value is the
/// server's: the chat is what the stream delivered, the viewer count is what
/// the server counted, a tip appears when the ledger has moved. Sending
/// something does not add it locally — the server echoes it on the stream to
/// everyone, the sender included, and that echo is the one copy drawn.
@MainActor
@Observable
public final class LiveRoomStore {

    public enum Phase: Equatable, Sendable {
        case idle
        case loading
        case live
        case ended
        case failed(APIError)
    }

    public let streamID: String
    public private(set) var stream: LiveStream?
    public private(set) var chat: [LiveChatMessage] = []
    public private(set) var viewerCount = 0
    public private(set) var heartCount = 0
    public private(set) var pinned: String?
    public private(set) var tipTotalUAET: Int64 = 0
    public private(set) var tipGoalUAET: Int64 = 0
    public private(set) var phase: Phase = .idle
    /// True while the event stream is open. False briefly during a reconnect.
    public private(set) var isConnected = false
    /// Set when a send fails, for the composer to show.
    public private(set) var lastError: String?
    /// The summary the broadcaster received on ending.
    public private(set) var summary: LiveSummary?

    private let client: APIClient
    private var streamTask: Task<Void, Never>?

    /// Chat is kept to a window; a long broadcast should not grow memory
    /// without bound, and nobody scrolls back a thousand lines in a live chat.
    private let chatWindow = 200

    public init(streamID: String, client: APIClient) {
        self.streamID = streamID
        self.client = client
    }

    /// Loads the room and opens the stream. Safe to call again.
    public func connect() async {
        if streamTask != nil { return }
        phase = .loading
        do {
            let room = try await client.liveRoom(id: streamID)
            apply(room)
        } catch let error as APIError {
            phase = .failed(error)
            return
        } catch {
            phase = .failed(.transport(error.localizedDescription))
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

    private func apply(_ room: LiveRoom) {
        stream = room.stream
        chat = room.chat
        viewerCount = room.stream.viewerCount
        heartCount = room.stream.heartCount
        pinned = room.stream.pinnedBody.flatMap { $0.isEmpty ? nil : $0 }
        tipTotalUAET = room.stream.tipTotalUAET
        tipGoalUAET = room.stream.tipGoalUAET
        phase = room.stream.isLive ? .live : .ended
    }

    /// Reads the stream; reconnects after a drop while the room is live.
    private func consume() async {
        var backoff: Duration = .seconds(1)
        while !Task.isCancelled, phase == .live {
            do {
                let events = try await LiveEventStream.open(id: streamID, client: client)
                isConnected = true
                backoff = .seconds(1)
                for try await event in events {
                    handle(event)
                    if phase == .ended { break }
                }
                isConnected = false
                if phase == .ended { return }
            } catch {
                isConnected = false
                if Task.isCancelled { return }
            }
            // Re-read the room before reconnecting: it may have ended while
            // the connection was down.
            if let room = try? await client.liveRoom(id: streamID) {
                apply(room)
                if phase != .live { return }
            }
            try? await Task.sleep(for: backoff)
            backoff = min(backoff * 2, .seconds(30))
        }
    }

    private func handle(_ event: LiveEvent) {
        switch event {
        case .chat(let message), .tip(let message):
            if !chat.contains(where: { $0.id == message.id }) {
                chat.append(message)
                if chat.count > chatWindow { chat.removeFirst(chat.count - chatWindow) }
            }
        case .viewers(let n):
            viewerCount = n
        case .hearts(let n):
            heartCount = n
        case .pinned(let body):
            pinned = body.isEmpty ? nil : body
        case .tips(let total, let goal):
            tipTotalUAET = total
            tipGoalUAET = goal
        case .ended:
            phase = .ended
            isConnected = false
        }
    }

    // MARK: - Actions

    /// Says something. The line appears when the stream echoes it.
    public func send(_ text: String) async {
        let trimmed = text.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty else { return }
        lastError = nil
        do {
            let message = try await client.sendLiveChat(id: streamID, body: trimmed)
            // The sender's own line comes back on the HTTP response as well as
            // the stream; whichever lands first is drawn, the other is dropped.
            if !chat.contains(where: { $0.id == message.id }) { chat.append(message) }
        } catch {
            lastError = Self.message(error)
        }
    }

    public func heart() async {
        do { try await client.heartLive(id: streamID) } catch { lastError = Self.message(error) }
    }

    /// Tips in hundredths of an AET.
    public func tip(amountHundredths: Int) async throws {
        lastError = nil
        do {
            try await client.tipLive(id: streamID, amountAETHundredths: amountHundredths)
        } catch {
            lastError = Self.message(error)
            throw error
        }
    }

    /// Broadcaster only.
    public func pin(_ text: String) async {
        do { try await client.pinLive(id: streamID, body: text) } catch { lastError = Self.message(error) }
    }

    /// Broadcaster only. Ends the room and keeps the summary.
    public func end() async throws -> LiveSummary {
        let result = try await client.endLive(id: streamID)
        summary = result
        phase = .ended
        disconnect()
        return result
    }

    /// The tip goal's progress, 0…1, or nil when there is no goal.
    public var tipProgress: Double? {
        guard tipGoalUAET > 0 else { return nil }
        return min(1, Double(tipTotalUAET) / Double(tipGoalUAET))
    }

    private static func message(_ error: Error) -> String {
        switch error {
        case let e as MalkuthError:
            if case .rejected(let status, let reason) = e, status == 402 { return "Not enough in your wallet. \(reason)" }
            return e.description
        case let e as APIError: return e.userMessage
        default: return error.localizedDescription
        }
    }
}
