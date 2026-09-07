import Foundation

/// One frame from a room's event stream, already decoded.
public enum LiveEvent: Sendable, Equatable {
    case chat(LiveChatMessage)
    case tip(LiveChatMessage)
    case viewers(Int)
    case hearts(Int)
    case pinned(String)
    case tips(totalUAET: Int64, goalUAET: Int64)
    case ended
}

/// Server-Sent Events, read by hand.
///
/// `URLSession.bytes` hands the body over a line at a time and this assembles
/// those lines into frames the way the SSE grammar says to: `event:`, `id:` and
/// one or more `data:` lines, terminated by a blank line, with `:` comment
/// lines (`: ping`) dropped.
///
/// Two departures from a textbook parser, both because the reader underneath
/// cannot be trusted to deliver the blank line that ends a frame — the reason
/// the first version of this dispatched on the `data:` line instead:
///
/// - Until a blank line has actually been seen on this connection, the first
///   `data:` line of a frame dispatches it immediately. That is exactly the
///   old behaviour, so a reader that swallows empty lines still delivers every
///   frame the server sent. The first blank line proves the reader passes them
///   on, and from then on frames are assembled properly and multi-line `data:`
///   is joined with a newline.
/// - An `event:` or `id:` line arriving while data is already buffered ends the
///   previous frame, since neither can appear twice in one frame.
///
/// The stream ends when the server closes it or the task is cancelled;
/// reconnecting is the caller's decision.
public enum SSEFrames {
    public struct Frame: Sendable, Equatable {
        public let event: String
        public let data: String
        /// The `id:` the server stamped on this frame, when it stamped one.
        /// Handed back on reconnect as `Last-Event-ID` so the server can replay
        /// what was missed.
        public let id: String?

        public init(event: String, data: String, id: String? = nil) {
            self.event = event
            self.data = data
            self.id = id
        }
    }

    /// The framing rules, as a value, so they can be driven line by line in a
    /// test without a socket.
    struct Parser {
        /// The last `id:` this connection has seen, whether or not its frame
        /// has been dispatched yet.
        private(set) var lastEventID: String?

        private var event = ""
        private var data: [String] = []
        private var id: String?
        private var sawBlankLine = false
        /// This frame was already handed over on its first `data:` line.
        private var dispatched = false

        init() {}

        /// Feeds one line in and returns the frame it completed, if any.
        mutating func consume(_ rawLine: String) -> Frame? {
            var line = rawLine
            if line.hasSuffix("\r") { line.removeLast() }

            if line.isEmpty {
                sawBlankLine = true
                return endFrame()
            }
            // A comment. `: ping` keeps the connection warm and says nothing.
            if line.hasPrefix(":") { return nil }

            let field: String
            let value: String
            if let colon = line.firstIndex(of: ":") {
                field = String(line[line.startIndex..<colon])
                var rest = line[line.index(after: colon)...]
                if rest.first == " " { rest = rest.dropFirst() }
                value = String(rest)
            } else {
                field = line
                value = ""
            }

            switch field {
            case "event", "id":
                // Neither can occur twice in one frame, so if anything is
                // buffered the previous frame ended without its blank line.
                let finished = (!data.isEmpty || dispatched) ? endFrame() : nil
                if field == "event" {
                    event = value
                } else {
                    id = value
                    lastEventID = value
                }
                return finished
            case "data":
                data.append(value)
                if !sawBlankLine && !dispatched {
                    dispatched = true
                    return Frame(event: event, data: value, id: id)
                }
                return nil
            default:
                // `retry:` and anything else the server adds later.
                return nil
            }
        }

        /// The frame left buffered when the stream ended.
        mutating func flush() -> Frame? { endFrame() }

        private mutating func endFrame() -> Frame? {
            defer {
                event = ""
                data = []
                id = nil
                dispatched = false
            }
            guard !dispatched, !data.isEmpty else { return nil }
            return Frame(event: event, data: data.joined(separator: "\n"), id: id)
        }
    }

    public static func open(
        _ endpoint: Endpoint,
        client: APIClient,
        lastEventID: String? = nil
    ) async throws -> AsyncThrowingStream<Frame, Error> {
        let request = try await client.streamRequest(endpoint, lastEventID: lastEventID)
        let session = await client.streamSession
        let (bytes, response) = try await session.bytes(for: request)
        guard let http = response as? HTTPURLResponse else {
            throw APIError.transport("Response was not HTTP.")
        }
        guard (200..<300).contains(http.statusCode) else {
            // The stream's own 401 has to clear the token the same way a
            // request's does, or the app sits reconnecting against a session
            // the server has already revoked.
            if http.statusCode == 401 {
                await client.noteTokenRejected()
                throw APIError.api(
                    status: 401,
                    body: APIErrorBody(code: "unauthenticated", message: "Sign in to continue.")
                )
            }
            throw APIError.unexpectedStatus(http.statusCode)
        }
        return AsyncThrowingStream { continuation in
            let task = Task {
                var parser = Parser()
                do {
                    for try await line in bytes.lines {
                        if Task.isCancelled { break }
                        if let frame = parser.consume(line) { continuation.yield(frame) }
                    }
                    if let frame = parser.flush() { continuation.yield(frame) }
                    continuation.finish()
                } catch {
                    continuation.finish(throwing: error)
                }
            }
            continuation.onTermination = { _ in task.cancel() }
        }
    }
}

/// Reads `GET /api/v1/live/{id}/events` as a sequence of ``LiveEvent``s.
public enum LiveEventStream {

    public static func open(id: String, client: APIClient) async throws -> AsyncThrowingStream<LiveEvent, Error> {
        let frames = try await SSEFrames.open(.liveEvents(id: id), client: client)
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

    static func parse(event: String, data: String, decoder: JSONDecoder) -> LiveEvent? {
        let payload = Data(data.utf8)
        switch event {
        case "chat":
            return (try? decoder.decode(LiveChatMessage.self, from: payload)).map { .chat($0) }
        case "tip":
            return (try? decoder.decode(LiveChatMessage.self, from: payload)).map { .tip($0) }
        case "viewers":
            struct Count: Decodable { let count: Int }
            return (try? decoder.decode(Count.self, from: payload)).map { .viewers($0.count) }
        case "hearts":
            struct Count: Decodable { let count: Int }
            return (try? decoder.decode(Count.self, from: payload)).map { .hearts($0.count) }
        case "pinned":
            struct Pinned: Decodable { let body: String }
            return (try? decoder.decode(Pinned.self, from: payload)).map { .pinned($0.body) }
        case "tips":
            struct Totals: Decodable {
                let totalUAET: Int64
                let goalUAET: Int64
                enum CodingKeys: String, CodingKey { case totalUAET = "total_uaet"; case goalUAET = "goal_uaet" }
            }
            return (try? decoder.decode(Totals.self, from: payload)).map { .tips(totalUAET: $0.totalUAET, goalUAET: $0.goalUAET) }
        case "status":
            struct Status: Decodable { let status: String }
            if let s = try? decoder.decode(Status.self, from: payload), s.status == "ended" { return .ended }
            return nil
        default:
            return nil
        }
    }
}
