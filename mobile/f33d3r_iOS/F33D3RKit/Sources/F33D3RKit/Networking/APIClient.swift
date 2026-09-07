import Foundation

/// Supplies the bearer token for authenticated requests.
///
/// A protocol rather than a stored string because the token can change
/// underneath an in-flight request — login, logout, expiry — and because tests
/// need to drive it without a Keychain.
public protocol TokenProviding: Sendable {
    func currentToken() async -> String?
    /// Called when the server rejects the token, so the owner can clear it and
    /// send the user back to sign-in.
    func tokenRejected() async
}

/// The HTTP client for Nantar's api/v1 surface.
///
/// An actor because it is shared across every screen and holds the base URL and
/// token provider; serialising access keeps configuration changes from racing
/// in-flight requests.
public actor APIClient {
    /// Origin of the F33D3R instance, e.g. https://f33d3r.com. All api/v1 paths
    /// are resolved beneath `/api/v1/`.
    ///
    /// `nonisolated` because it is an immutable `URL` and the UI reads it
    /// synchronously — the media environment and the sign-in screen's help link
    /// both need it while building a view, and awaiting the actor for a
    /// constant would mean either an async view body or a copy of the value
    /// kept somewhere it could drift.
    public nonisolated let baseURL: URL
    private let session: URLSession
    private let tokens: any TokenProviding

    private let decoder = APIClient.makeDecoder()

    /// The decoder every api/v1 payload is read through.
    ///
    /// Exposed because tests and preview fixtures must decode with exactly the
    /// same rules the app uses — a fixture that only parses under a laxer
    /// decoder proves nothing about the contract.
    public static func makeDecoder() -> JSONDecoder {
        let d = JSONDecoder()
        // The Go side emits RFC 3339 with fractional seconds; `.iso8601` alone
        // rejects those, which would fail every response carrying a timestamp.
        d.dateDecodingStrategy = .custom { decoder in
            let raw = try decoder.singleValueContainer().decode(String.self)
            if let date = APIClient.rfc3339Fractional.date(from: raw) { return date }
            if let date = APIClient.rfc3339.date(from: raw) { return date }
            throw DecodingError.dataCorruptedError(
                in: try decoder.singleValueContainer(),
                debugDescription: "Not an RFC 3339 timestamp: \(raw)"
            )
        }
        return d
    }

    private let encoder: JSONEncoder = {
        let e = JSONEncoder()
        e.dateEncodingStrategy = .iso8601
        return e
    }()

    nonisolated(unsafe) private static let rfc3339Fractional: ISO8601DateFormatter = {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return f
    }()

    nonisolated(unsafe) private static let rfc3339: ISO8601DateFormatter = {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime]
        return f
    }()

    public init(
        baseURL: URL,
        tokens: any TokenProviding,
        session: URLSession = .shared
    ) {
        self.baseURL = baseURL
        self.tokens = tokens
        self.session = session
    }

    // MARK: - Requests

    /// Sends `endpoint` and decodes the response as `T`.
    public func send<T: Decodable>(
        _ endpoint: Endpoint,
        body: (some Encodable)? = Optional<Never>.none,
        as type: T.Type = T.self
    ) async throws -> T {
        let data = try await sendReturningData(endpoint, body: body)
        if data.isEmpty {
            throw APIError.decoding("Expected \(T.self) but the response body was empty.")
        }
        do {
            return try decoder.decode(T.self, from: data)
        } catch {
            throw APIError.decoding(String(describing: error))
        }
    }

    /// Sends `endpoint` and ignores the response body. For endpoints that answer
    /// 204, like logout.
    public func send(
        _ endpoint: Endpoint,
        body: (some Encodable)? = Optional<Never>.none
    ) async throws {
        _ = try await sendReturningData(endpoint, body: body)
    }

    private func sendReturningData(
        _ endpoint: Endpoint,
        body: (some Encodable)?
    ) async throws -> Data {
        var encoded: Data?
        if let body {
            do {
                encoded = try encoder.encode(body)
            } catch {
                throw APIError.decoding("Could not encode request body: \(error)")
            }
        }
        let (data, http) = try await perform(
            endpoint, body: encoded, contentType: encoded == nil ? nil : "application/json"
        )
        if (200..<300).contains(http.statusCode) { return data }
        throw mapFailure(status: http.statusCode, data: data)
    }

    /// Sends `endpoint` with a body this client did not encode, and returns the
    /// response whatever its status.
    ///
    /// Both halves matter for the write lane. The body must pass through
    /// untouched because a signed work's payload bytes are the bytes that were
    /// hashed — re-encoding them is the one thing that cannot happen. And the
    /// status must reach the caller because `/events` answers with plain text
    /// (`cid mismatch`, `signature invalid`) that the generic error mapping,
    /// which is tuned for the JSON surface, correctly refuses to invent a code
    /// for.
    func sendRaw(
        _ endpoint: Endpoint,
        body: Data?,
        contentType: String?
    ) async throws -> (data: Data, status: Int) {
        let (data, http) = try await perform(endpoint, body: body, contentType: contentType)
        return (data, http.statusCode)
    }

    /// Sends `endpoint` with a body read from a file, and returns the response
    /// whatever its status.
    ///
    /// The file-backed twin of ``sendRaw(_:body:contentType:)``, for a body
    /// that must not be held in memory: a video from the camera roll can be
    /// several gigabytes, and `Data(contentsOf:)` on one is a jetsam. Foundation
    /// streams an upload task's file from disk in pieces, so the whole request
    /// costs a buffer, not the file. `progress` is told the fraction sent as the
    /// bytes go out — the one number the uploader can show while it waits.
    func sendRawFile(
        _ endpoint: Endpoint,
        fileURL: URL,
        contentType: String,
        progress: (@Sendable (Double) -> Void)? = nil
    ) async throws -> (data: Data, status: Int) {
        // Foundation sizes the body from the file itself, so the server sees a
        // Content-Length and not a chunked stream, and the task reports its
        // own byte counts to the delegate as they go.
        let request = try await makeRequest(endpoint, contentType: contentType)

        let data: Data
        let response: URLResponse
        do {
            (data, response) = try await session.upload(
                for: request, fromFile: fileURL, delegate: progress.map(UploadProgressRelay.init)
            )
        } catch {
            throw APIError.unreachable(origin: baseURL.absoluteString, reason: error.localizedDescription)
        }
        guard let http = response as? HTTPURLResponse else {
            throw APIError.transport("Response was not HTTP.")
        }
        if http.statusCode == 401 {
            await tokens.tokenRejected()
        }
        return (data, http.statusCode)
    }

    /// An authenticated request for a long-lived stream, for callers that
    /// read the body incrementally rather than through `send`. The bearer
    /// token is attached the same way `perform` attaches it.
    ///
    /// `lastEventID` is the id of the last frame this client saw on a previous
    /// connection. The server keeps a short ring buffer per account and replays
    /// everything after that id before going live again, so a reconnect after a
    /// tunnel or a backgrounded app does not lose the frames in between.
    func streamRequest(_ endpoint: Endpoint, lastEventID: String? = nil) async throws -> URLRequest {
        var request = URLRequest(url: try url(for: endpoint))
        request.httpMethod = endpoint.method.rawValue
        request.setValue("text/event-stream", forHTTPHeaderField: "Accept")
        request.timeoutInterval = 60 * 60
        if let lastEventID, !lastEventID.isEmpty {
            request.setValue(lastEventID, forHTTPHeaderField: "Last-Event-ID")
        }
        if endpoint.requiresAuth {
            guard let token = await tokens.currentToken() else {
                throw APIError.api(status: 401, body: APIErrorBody(code: "unauthenticated", message: "Sign in to continue."))
            }
            request.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        }
        return request
    }

    /// The session streams are read through. Exposed for the SSE reader.
    var streamSession: URLSession { session }

    /// Clears the token the way a 401 on a normal request does.
    ///
    /// ``perform(_:body:contentType:)`` does this itself, but a stream never
    /// goes through it: it opens its own connection and reads bytes. Without
    /// this an expired session leaves the app reconnecting forever instead of
    /// returning to sign-in.
    func noteTokenRejected() async {
        await tokens.tokenRejected()
    }

    /// The credential for a media URL on this instance, or nil when the URL
    /// belongs to somebody else.
    ///
    /// Media paths arrive relative and are resolved against `baseURL`, but the
    /// DTO allows an absolute URL because media moves to its own hostname
    /// eventually. The origin is therefore checked rather than assumed: a
    /// bearer token is the account for thirty days, and the one thing that must
    /// never happen is sending it to a host the server merely named.
    public func mediaCredential(for url: URL) async -> MediaCredential? {
        guard isSameOrigin(url) else { return nil }
        guard let token = await tokens.currentToken() else { return nil }
        return MediaCredential(token: token, origin: baseURL)
    }

    /// Whether `url` is served by this instance: scheme, host and port all
    /// equal. Port is compared through `effectivePort` so that
    /// `https://host/x` and `https://host:443/x` are the same origin.
    nonisolated func isSameOrigin(_ url: URL) -> Bool {
        guard let lhs = url.host?.lowercased(), let rhs = baseURL.host?.lowercased(), lhs == rhs,
              url.scheme?.lowercased() == baseURL.scheme?.lowercased()
        else { return false }
        return effectivePort(of: url) == effectivePort(of: baseURL)
    }

    private nonisolated func effectivePort(of url: URL) -> Int? {
        if let port = url.port { return port }
        switch url.scheme?.lowercased() {
        case "https": return 443
        case "http": return 80
        default: return nil
        }
    }

    private func perform(
        _ endpoint: Endpoint,
        body: Data?,
        contentType: String?
    ) async throws -> (Data, HTTPURLResponse) {
        var request = try await makeRequest(endpoint, contentType: body == nil ? nil : contentType)
        request.httpBody = body

        let data: Data
        let response: URLResponse
        do {
            (data, response) = try await session.data(for: request)
        } catch {
            throw APIError.unreachable(origin: baseURL.absoluteString, reason: error.localizedDescription)
        }

        guard let http = response as? HTTPURLResponse else {
            throw APIError.transport("Response was not HTTP.")
        }

        // Let the token's owner clear it before the error propagates, so the app
        // never sits in a loop retrying with a token the server has revoked.
        if http.statusCode == 401 {
            await tokens.tokenRejected()
        }

        return (data, http)
    }

    /// The request every call is built from — URL, method, `Accept`, the
    /// `Origin` the CSRF middleware wants, and the bearer — with the body left
    /// to the caller, because a body in memory and a body on disk are attached
    /// differently and everything else about the request is the same.
    private func makeRequest(_ endpoint: Endpoint, contentType: String?) async throws -> URLRequest {
        var request = URLRequest(url: try url(for: endpoint))
        request.httpMethod = endpoint.method.rawValue
        request.setValue("application/json", forHTTPHeaderField: "Accept")
        if let contentType {
            request.setValue(contentType, forHTTPHeaderField: "Content-Type")
        }

        // Nantar applies `middleware.CSRF` to every mutating request on every
        // path, api/v1 included: with neither Origin nor Referer it allows the
        // request only when the Host is localhost, and answers 403 otherwise.
        // That rule is written for browsers, where Origin is stamped by the
        // agent and cannot be forged cross-site. A native client has no ambient
        // credentials to be confused into spending — it presents a bearer token
        // it was given — so it is not the threat the middleware defends against;
        // it just has to state the origin it is actually talking to, which is
        // this base URL and could not be anything else. Without this every write
        // from the app against a real deployment is a 403 whose body says only
        // "403 Forbidden".
        if endpoint.method != .get, let origin = originHeaderValue {
            request.setValue(origin, forHTTPHeaderField: "Origin")
        }

        if endpoint.requiresAuth {
            guard let token = await tokens.currentToken() else {
                throw APIError.api(
                    status: 401,
                    body: APIErrorBody(code: "unauthenticated", message: "Sign in to continue.")
                )
            }
            request.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        }
        return request
    }

    private func mapFailure(status: Int, data: Data) -> APIError {
        if let body = try? decoder.decode(APIErrorBody.self, from: data) {
            return .api(status: status, body: body)
        }
        if let wrapped = try? decoder.decode(WrappedError.self, from: data) {
            return .api(status: status, body: wrapped.error)
        }
        return .unexpectedStatus(status)
    }

    /// The `Origin` a browser on this instance would send. `nil` when the base
    /// URL has no scheme and host to build one from, in which case the header is
    /// omitted rather than guessed.
    private nonisolated var originHeaderValue: String? {
        guard let scheme = baseURL.scheme, let host = baseURL.host() else { return nil }
        var origin = "\(scheme)://\(host)"
        if let port = baseURL.port {
            let isDefault = (scheme == "http" && port == 80) || (scheme == "https" && port == 443)
            if !isDefault { origin += ":\(port)" }
        }
        return origin
    }

    private func url(for endpoint: Endpoint) throws -> URL {
        let trimmed = endpoint.path.trimmingCharacters(in: CharacterSet(charactersIn: "/"))
        let path = switch endpoint.root {
        case .apiV1: "api/v1/" + trimmed
        case .origin: trimmed
        }
        guard var components = URLComponents(
            url: baseURL.appendingPathComponent(path),
            resolvingAgainstBaseURL: false
        ) else {
            throw APIError.transport("Could not build a URL for \(endpoint.path).")
        }
        if !endpoint.query.isEmpty {
            components.queryItems = endpoint.query
        }
        guard let url = components.url else {
            throw APIError.transport("Could not build a URL for \(endpoint.path).")
        }
        return url
    }

    private struct WrappedError: Decodable {
        let error: APIErrorBody
    }
}

// MARK: - Typed calls

public extension APIClient {
    struct LoginRequest: Encodable, Sendable {
        public let handle: String
        public let password: String
        public let deviceName: String
        /// The six digits from the authenticator, when the account has one.
        ///
        /// Omitted rather than sent empty on the first attempt: the app does
        /// not know whether an account has two-factor on until the server says
        /// so, and asking for a code before it is needed makes every sign-in
        /// look like it has one. The refusal that comes back carries
        /// ``APIError/TwoFactorFailure/required``, and the same call is made
        /// again with the code filled in.
        public let code: String?

        enum CodingKeys: String, CodingKey {
            case handle, password, code
            case deviceName = "device_name"
        }

        public init(handle: String, password: String, deviceName: String, code: String? = nil) {
            self.handle = handle
            self.password = password
            self.deviceName = deviceName
            self.code = code
        }
    }

    func login(_ request: LoginRequest) async throws -> Session {
        try await send(.login, body: request, as: Session.self)
    }

    func logout() async throws {
        try await send(.logout, body: Optional<Never>.none)
    }

    func me() async throws -> CurrentUser {
        try await send(.me, body: Optional<Never>.none, as: CurrentUser.self)
    }

    // MARK: Reads

    func feed(surface: FeedSurface, cursor: String? = nil, limit: Int = 20) async throws -> WorkPage {
        try await send(
            .feed(surface: surface, cursor: cursor, limit: limit),
            body: Optional<Never>.none,
            as: WorkPage.self
        )
    }

    func work(id: String) async throws -> WorkThread {
        try await send(.work(id: id), body: Optional<Never>.none, as: WorkThread.self)
    }

    func replies(workID: String, cursor: String? = nil, limit: Int = 20) async throws -> WorkPage {
        try await send(
            .replies(workID: workID, cursor: cursor, limit: limit),
            body: Optional<Never>.none,
            as: WorkPage.self
        )
    }

    func profile(handle: String) async throws -> Profile {
        try await send(.profile(handle: handle), body: Optional<Never>.none, as: Profile.self)
    }

    func profileWorks(
        handle: String,
        tab: Profile.Tab = .works,
        cursor: String? = nil,
        limit: Int = 20
    ) async throws -> WorkPage {
        try await send(
            .profileWorks(handle: handle, tab: tab, cursor: cursor, limit: limit),
            body: Optional<Never>.none,
            as: WorkPage.self
        )
    }

    // MARK: Media

    /// Resolves a server-supplied media path against this instance's origin.
    ///
    /// Media URLs arrive relative (`/media/...`) because the web app is served
    /// from the same origin as Caeor's Caddy route. A native client has no
    /// document base to resolve against, so it happens here — once, next to the
    /// base URL rather than at every image call site.
    nonisolated func mediaURL(_ path: String?) -> URL? {
        guard let path, !path.isEmpty else { return nil }
        if let absolute = URL(string: path), absolute.scheme != nil { return absolute }
        return URL(string: path, relativeTo: baseURL)?.absoluteURL
    }
}

/// Forwards an upload task's byte counts as a fraction.
///
/// `URLSession.upload(for:fromFile:delegate:)` reports progress only through
/// a task delegate, and a delegate is a class. This is the smallest one that
/// can be: it holds the callback and does the division.
private final class UploadProgressRelay: NSObject, URLSessionTaskDelegate, Sendable {
    private let report: @Sendable (Double) -> Void

    init(report: @escaping @Sendable (Double) -> Void) {
        self.report = report
    }

    func urlSession(
        _ session: URLSession, task: URLSessionTask,
        didSendBodyData bytesSent: Int64, totalBytesSent: Int64, totalBytesExpectedToSend: Int64
    ) {
        guard totalBytesExpectedToSend > 0 else { return }
        report(min(1, Double(totalBytesSent) / Double(totalBytesExpectedToSend)))
    }
}
