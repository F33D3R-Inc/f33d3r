import Foundation

/// What authorises a request for this instance's media.
///
/// Media on F33D3R is walled. Anything under `/posts/` is checked against the
/// work that owns it — deleted, expired, 18+, subscriber-only, or by a private
/// author — and the answer depends on who is asking. A request that names
/// nobody is refused with `403`, which is what the app's images and video were
/// getting: `AsyncImage` and `AVPlayer` both use their own networking and send
/// no credential, so every post image and every HLS playlist came back
/// forbidden while avatars, which are not owned by a work, loaded fine.
///
/// The server takes the session two ways, and both are used here because the
/// two clients differ. `URLSession` can set a header, so images carry
/// `Authorization: Bearer`. `AVPlayer` cannot be given headers through public
/// API, but it can be given cookies, and the server reads the same session from
/// `f33d3r_session` — that is how the web's `<video>` authenticates.
///
/// The cookie is built here and handed to one asset. It is never put in
/// `HTTPCookieStorage`, which persists to a plist inside the app container: the
/// token is a 30-day bearer credential and belongs in the Keychain and nowhere
/// else, exactly as ``KeychainStore`` says.
public struct MediaCredential: Sendable {
    /// The session token, as `SessionStore` holds it.
    public let token: String
    /// The origin it is good for. A credential is never sent anywhere else.
    public let origin: URL

    /// The name the server reads the session cookie under.
    /// `handler.SessionCookieName` in feed-engine.
    public static let cookieName = "f33d3r_session"

    public init(token: String, origin: URL) {
        self.token = token
        self.origin = origin
    }

    public var authorizationHeader: String { "Bearer \(token)" }

    /// The session as a cookie, for the players that cannot take a header.
    ///
    /// Scoped to the origin's host and to `/`, so it reaches `/static/media`
    /// and the HLS segments the playlist points at. `secure` follows the
    /// origin's scheme rather than being asserted: a development instance is
    /// reached over plain HTTP by IP, and a cookie marked secure would simply
    /// not be sent, which reads as "still forbidden" rather than as a mistake.
    public func sessionCookie() -> HTTPCookie? {
        guard let host = origin.host else { return nil }
        var properties: [HTTPCookiePropertyKey: Any] = [
            .name: Self.cookieName,
            .value: token,
            .domain: host,
            .path: "/",
        ]
        if origin.scheme == "https" { properties[.secure] = "TRUE" }
        return HTTPCookie(properties: properties)
    }
}

/// Loads walled media with the session attached, and remembers what it has
/// already fetched.
///
/// An actor because a feed asks for the same avatar from a dozen rows at once:
/// the in-flight table collapses those into one request, and the cache keeps a
/// scroll back up from re-fetching what is already on screen.
///
/// Nothing is written to disk. The wall can close between two requests — a work
/// is deleted, a subscription lapses — so a copy left in a disk cache would
/// outlive the reader's right to see it. Memory only, and it dies with the
/// process.
public actor MediaLoader {
    /// How much decoded media to keep. Roughly a few screens of a feed.
    private static let memoryLimit = 64 * 1024 * 1024

    /// A loader with no session, for previews and for public media. It fetches
    /// exactly what an anonymous reader may see, which is what a preview should
    /// be showing anyway.
    public static let unauthenticated = MediaLoader(client: nil)

    private let client: APIClient?
    private let session: URLSession
    private let cache = NSCache<NSURL, NSData>()
    private var inFlight: [URL: Task<Data, Error>] = [:]

    public init(client: APIClient?) {
        self.client = client
        let configuration = URLSessionConfiguration.default
        // The wall again: nothing persisted, and no cookie jar of our own.
        configuration.urlCache = URLCache(memoryCapacity: Self.memoryLimit, diskCapacity: 0)
        configuration.requestCachePolicy = .useProtocolCachePolicy
        configuration.httpCookieStorage = nil
        configuration.httpShouldSetCookies = false
        self.session = URLSession(configuration: configuration)
        cache.totalCostLimit = Self.memoryLimit
    }

    /// The bytes at `url`, from cache when they are already held.
    ///
    /// Throws ``MediaLoadError`` so a caller can tell "you may not see this"
    /// from "that did not load", which are different things to put on screen.
    public func data(for url: URL) async throws -> Data {
        if let cached = cache.object(forKey: url as NSURL) { return cached as Data }
        if let existing = inFlight[url] { return try await existing.value }

        let task = Task<Data, Error> { [client, session] in
            var request = URLRequest(url: url)
            // The credential goes only to this instance. A media URL can be
            // absolute and point anywhere, and a bearer token handed to a
            // stranger's host is the token gone.
            if let credential = await client?.mediaCredential(for: url) {
                request.setValue(credential.authorizationHeader, forHTTPHeaderField: "Authorization")
            }
            let (data, response) = try await session.data(for: request)
            guard let http = response as? HTTPURLResponse else { return data }
            switch http.statusCode {
            case 200..<300:
                return data
            case 401, 403:
                throw MediaLoadError.forbidden
            case 404:
                throw MediaLoadError.missing
            default:
                throw MediaLoadError.status(http.statusCode)
            }
        }
        inFlight[url] = task
        defer { inFlight[url] = nil }

        let data = try await task.value
        cache.setObject(data as NSData, forKey: url as NSURL, cost: data.count)
        return data
    }

    /// Drops everything held. Called at sign-out: the next reader of this
    /// device is not the last one.
    public func clear() {
        cache.removeAllObjects()
        for (_, task) in inFlight { task.cancel() }
        inFlight.removeAll()
    }
}

/// Why a media object did not arrive.
public enum MediaLoadError: Error, Equatable, Sendable {
    /// The wall refused it for this viewer.
    case forbidden
    /// No such object.
    case missing
    case status(Int)
}
