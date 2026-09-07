import Foundation
import Observation

/// One frame from the signed-in user's stream.
public enum UserEvent: Sendable, Equatable {
    case notify(unread: Int)
    case newPost(workID: String, authorHandle: String)
    case postDeleted(workID: String)
    case balance(settledUAET: Int64, pendingUAET: Int64)
    case liveStart(streamID: String)
    /// A message landed. Carries the message itself when the server sent one,
    /// so a thread already on screen can show it without asking again.
    ///
    /// The plain text of a plain message is deliberately not on this stream —
    /// see the note in ``UserEventStream/parse(_:decoder:)``.
    case message(conversationID: String, message: ChatMessage?)
    /// The counts on one work changed, for a work this client asked to watch.
    case workEngagement(WorkEngagement)
    /// A frequency room went live.
    case frequencyStart(id: String)
}

/// One decoded frame with the id the server stamped on it.
///
/// The id is carried alongside rather than inside ``UserEvent`` because it is
/// about the *stream*, not about what happened: it is what goes back as
/// `Last-Event-ID` on the next connection so the server can replay the gap.
public struct UserEventFrame: Sendable, Equatable {
    public let event: UserEvent
    public let id: String?

    public init(event: UserEvent, id: String?) {
        self.event = event
        self.id = id
    }
}

/// Reads `GET /api/v1/events`.
public enum UserEventStream {
    public static func open(
        client: APIClient,
        lastEventID: String? = nil
    ) async throws -> AsyncThrowingStream<UserEventFrame, Error> {
        let frames = try await SSEFrames.open(.userEvents, client: client, lastEventID: lastEventID)
        // The client's decoder, not a bare one: a message frame carries a
        // timestamp, and the two RFC 3339 shapes the server sends are only
        // understood by the strategy that read every other date in this app.
        let decoder = APIClient.makeDecoder()
        return AsyncThrowingStream { continuation in
            let task = Task {
                do {
                    for try await frame in frames {
                        if let parsed = parse(frame, decoder: decoder) {
                            continuation.yield(UserEventFrame(event: parsed, id: frame.id))
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

    static func parse(_ frame: SSEFrames.Frame, decoder: JSONDecoder) -> UserEvent? {
        let payload = Data(frame.data.utf8)
        switch frame.event {
        case "notify":
            struct P: Decodable { let unread: Int }
            return (try? decoder.decode(P.self, from: payload)).map { .notify(unread: $0.unread) }
        case "new_post":
            struct P: Decodable { let work_id: String; let author_handle: String }
            return (try? decoder.decode(P.self, from: payload)).map { .newPost(workID: $0.work_id, authorHandle: $0.author_handle) }
        case "post_deleted":
            struct P: Decodable { let work_id: String }
            return (try? decoder.decode(P.self, from: payload)).map { .postDeleted(workID: $0.work_id) }
        case "balance":
            struct P: Decodable { let balance_uaet: Int64; let pending_uaet: Int64 }
            return (try? decoder.decode(P.self, from: payload)).map { .balance(settledUAET: $0.balance_uaet, pendingUAET: $0.pending_uaet) }
        case "live_start":
            struct P: Decodable { let stream_id: String }
            return (try? decoder.decode(P.self, from: payload)).map { .liveStart(streamID: $0.stream_id) }
        case "work_engagement":
            return (try? decoder.decode(WorkEngagement.self, from: payload)).map { .workEngagement($0) }
        case "frequency_start":
            struct P: Decodable { let frequency_id: String }
            return (try? decoder.decode(P.self, from: payload)).map { .frequencyStart(id: $0.frequency_id) }
        case "gnosis_message":
            // Read twice, leniently, because this frame is the one the server
            // may sensibly send in more than one shape. A full message is used
            // as it stands; anything that names only the conversation still
            // tells the list something arrived, and the thread fetches the rest
            // itself. Losing the notification because a field was missing would
            // be worse than one extra request.
            if let full = try? decoder.decode(ChatMessage.self, from: payload), !full.conversationID.isEmpty {
                return .message(conversationID: full.conversationID, message: full)
            }
            struct P: Decodable { let conversation_id: String }
            return (try? decoder.decode(P.self, from: payload))
                .map { .message(conversationID: $0.conversation_id, message: nil) }
        default:
            return nil
        }
    }
}

/// What the user's stream has said so far: the badge, the works that landed
/// while the reader was elsewhere, the balance. Connected once per sign-in
/// and reconnected with backoff; every value is the server's.
@MainActor
@Observable
public final class UserSignals {
    /// The unread count, once the stream has answered. Nil before.
    public private(set) var unread: Int?
    /// Works from followed accounts posted since the Following lane was last
    /// refreshed, newest first. The lane shows a "N new" pill and clears
    /// this when the reader pulls them in.
    public private(set) var pendingNewWorkIDs: [String] = []
    public private(set) var deletedWorkIDs: [String] = []
    public private(set) var balanceUAET: Int64?
    public private(set) var pendingUAET: Int64?
    public private(set) var isConnected = false

    /// Told about every frame after this store has applied it, so the app can
    /// react — drop a deleted work from its lists, refetch a wallet.
    public var onEvent: (@MainActor (UserEvent) -> Void)?

    /// Told whenever the connection comes up or goes down.
    ///
    /// Watch registrations live on the server for the length of a connection,
    /// so the app has to re-register what is on screen when a new one opens and
    /// let go of what it registered when one closes. Nothing else about the
    /// connection is anybody's business.
    public var onConnectionChange: (@MainActor (Bool) -> Void)?

    private let client: APIClient
    private var task: Task<Void, Never>?
    /// The id of the last frame this client saw, across connections. Sent as
    /// `Last-Event-ID` so the server replays what was missed in the gap.
    private var lastEventID: String?

    public init(client: APIClient) {
        self.client = client
    }

    public func connect() {
        guard task == nil else { return }
        task = Task { [weak self] in await self?.run() }
    }

    /// Closes the stream. The last seen id is kept, so a later ``connect()``
    /// — coming back from the background, say — resumes from it rather than
    /// from nothing.
    public func disconnect() {
        task?.cancel()
        task = nil
        setConnected(false)
    }

    /// The Following lane has caught up.
    public func clearPendingWorks() {
        pendingNewWorkIDs.removeAll()
    }

    public func consumeDeleted() -> [String] {
        defer { deletedWorkIDs.removeAll() }
        return deletedWorkIDs
    }

    private func run() async {
        var backoff: Duration = .seconds(1)
        while !Task.isCancelled {
            do {
                let frames = try await UserEventStream.open(client: client, lastEventID: lastEventID)
                setConnected(true)
                backoff = .seconds(1)
                for try await frame in frames {
                    if let id = frame.id { lastEventID = id }
                    apply(frame.event)
                }
                setConnected(false)
            } catch {
                setConnected(false)
                // A rejected token is the end of this stream and of the
                // session: ``SSEFrames`` has already told the token's owner,
                // which puts the app back on sign-in. Reconnecting would only
                // be a loop against a session the server has revoked.
                if let api = error as? APIError, api.isUnauthenticated {
                    lastEventID = nil
                    return
                }
            }
            if Task.isCancelled { return }
            try? await Task.sleep(for: backoff)
            backoff = min(backoff * 2, .seconds(30))
        }
    }

    private func setConnected(_ connected: Bool) {
        guard connected != isConnected else { return }
        isConnected = connected
        onConnectionChange?(connected)
    }

    private func apply(_ event: UserEvent) {
        switch event {
        case .notify(let n):
            unread = n
        case .newPost(let id, _):
            if !pendingNewWorkIDs.contains(id) { pendingNewWorkIDs.insert(id, at: 0) }
        case .postDeleted(let id):
            deletedWorkIDs.append(id)
            pendingNewWorkIDs.removeAll { $0 == id }
        case .balance(let settled, let pending):
            balanceUAET = settled
            pendingUAET = pending
        case .liveStart, .frequencyStart:
            // Nothing to hold. Both are "go and look": the Live lane and the
            // frequency list are the two things that know what is on them, and
            // they are told through `onEvent`.
            break
        case .workEngagement:
            // Counts belong to a work, and the works are held by the feeds.
            // Passed straight through to `onEvent`, which hands them to the
            // lists that have that row.
            break
        case .message:
            // Nothing to hold here. A message belongs to a conversation, and
            // the conversation list and the open thread are the two things that
            // know what to do with it, so it is handed straight to them.
            break
        }
        onEvent?(event)
    }
}
