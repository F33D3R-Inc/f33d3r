import Foundation

/// One api/v1 request, described independently of how it is sent.
///
/// Keeping this a value type means a call site names *what* it wants and the
/// client decides *how* — auth headers, decoding, error mapping — in one place.
public struct Endpoint: Hashable, Sendable {
    public enum Method: String, Sendable {
        case get = "GET"
        case post = "POST"
        case delete = "DELETE"
    }

    /// Which of Nantar's two surfaces the path is resolved under.
    ///
    /// There are two because the server has two. `api/v1` is the JSON surface
    /// added for native clients and it is read-only: `registerAPIV1` wires
    /// login, logout, me, and five GETs, and nothing else. Every **write** —
    /// `POST /events`, `POST /api/pial/signing-key/register` — is on the
    /// original D-070 mutation lane the web app uses, at the origin root.
    ///
    /// Naming that split rather than papering over it matters: a path resolved
    /// under the wrong root is a 404 from a mux that has no idea what was meant.
    public enum Root: Hashable, Sendable {
        /// Resolved beneath `/api/v1/`.
        case apiV1
        /// Resolved at the origin. `path` is the full path without its leading
        /// slash: `"events"`, `"api/pial/signing-key/register"`.
        case origin
    }

    public var method: Method
    /// Path relative to `root`, without a leading slash: "auth/login".
    public var path: String
    public var query: [URLQueryItem]
    /// Whether this call requires a session token. Login is the only endpoint
    /// that does not.
    public var requiresAuth: Bool
    public var root: Root

    public init(
        method: Method = .get,
        path: String,
        query: [URLQueryItem] = [],
        requiresAuth: Bool = true,
        root: Root = .apiV1
    ) {
        self.method = method
        self.path = path
        self.query = query
        self.requiresAuth = requiresAuth
        self.root = root
    }
}

public extension Endpoint {
    static let login = Endpoint(method: .post, path: "auth/login", requiresAuth: false)
    static let logout = Endpoint(method: .post, path: "auth/logout")
    static let me = Endpoint(path: "me")

    /// A page of the ranked feed.
    ///
    /// `cursor` is opaque and comes from the previous page; passing nil asks for
    /// the first page. The server caps `limit`, so a client asking for more than
    /// it allows gets the cap rather than an error.
    static func feed(surface: FeedSurface, cursor: String? = nil, limit: Int = 20) -> Endpoint {
        var query = [
            URLQueryItem(name: "surface", value: surface.rawValue),
            URLQueryItem(name: "limit", value: String(limit)),
        ]
        if let cursor { query.append(URLQueryItem(name: "cursor", value: cursor)) }
        return Endpoint(path: "feed", query: query)
    }

    /// One work with its ancestors and first page of replies.
    static func work(id: String) -> Endpoint {
        Endpoint(path: "works/\(id)")
    }

    /// A later page of one work's replies.
    static func replies(workID: String, cursor: String? = nil, limit: Int = 20) -> Endpoint {
        var query = [URLQueryItem(name: "limit", value: String(limit))]
        if let cursor { query.append(URLQueryItem(name: "cursor", value: cursor)) }
        return Endpoint(path: "works/\(workID)/replies", query: query)
    }

    /// A user's profile. Handles, not UUIDs — the API exposes no account ids.
    static func profile(handle: String) -> Endpoint {
        Endpoint(path: "users/\(handle)")
    }

    /// Creates an account. Handle and password only — F33D3R takes no email
    /// or phone. Answers a `Session` like login does.
    static let signup = Endpoint(method: .post, path: "auth/signup", requiresAuth: false)

    /// A page of the reader's notifications, grouped server-side.
    static func notifications(cursor: String? = nil, limit: Int = 20) -> Endpoint {
        var query = [URLQueryItem(name: "limit", value: String(limit))]
        if let cursor { query.append(URLQueryItem(name: "cursor", value: cursor)) }
        return Endpoint(path: "notifications", query: query)
    }

    /// The reader's wallet: settled and pending balances and the recent ledger.
    static let wallet = Endpoint(path: "wallet")

    /// Works and people matching a query.
    static func search(_ query: String) -> Endpoint {
        Endpoint(path: "search", query: [URLQueryItem(name: "q", value: query)])
    }

    /// Uploads one image. Multipart, field `file`; answers `{"url": "/media/…"}`.
    static let uploadMedia = Endpoint(method: .post, path: "media")

    /// Hands a video to the transcoder. Multipart, field `file`; answers
    /// `202` with a ``VideoJob`` to poll, or `200` with a `duplicate` job when
    /// the same bytes are already on F33D3R and nothing was queued.
    /// `415 unsupported_media` when the bytes are not a video container,
    /// `400 content_refused` and `503 scan_unavailable` from the safety gate.
    static let uploadVideo = Endpoint(method: .post, path: "media/video")

    /// One video upload's state, for polling until it is ``VideoJob/isSettled``.
    /// `404 video_job_not_found` for an id this account did not upload.
    static func videoJob(id: String) -> Endpoint {
        Endpoint(path: "media/video/\(id)")
    }

    /// Uploads one voice recording. Multipart, field `audio`; answers `201`
    /// with a ``VoiceUpload``. The duration is not in the answer — the
    /// recorder measures it and sends it with the work.
    static let uploadVoice = Endpoint(method: .post, path: "media/voice")

    /// GIFs matching `query`; trending when it is empty. `503
    /// gif_search_unavailable` on a server with no provider, `502
    /// gif_search_failed` when the provider did not answer.
    static func gifSearch(_ query: String) -> Endpoint {
        Endpoint(path: "gif/search", query: [URLQueryItem(name: "q", value: query)])
    }

    /// A tag's timeline: works carrying `tag`, newest first.
    static func tagFeed(_ tag: String, cursor: String? = nil, limit: Int = 20) -> Endpoint {
        var query = [
            URLQueryItem(name: "surface", value: "tag"),
            URLQueryItem(name: "tag", value: tag),
            URLQueryItem(name: "limit", value: String(limit)),
        ]
        if let cursor { query.append(URLQueryItem(name: "cursor", value: cursor)) }
        return Endpoint(path: "feed", query: query)
    }

    /// Who follows a user, and who they follow.
    static func followers(handle: String, cursor: String? = nil) -> Endpoint {
        Endpoint(path: "users/\(handle)/followers", query: cursor.map { [URLQueryItem(name: "cursor", value: $0)] } ?? [])
    }

    static func following(handle: String, cursor: String? = nil) -> Endpoint {
        Endpoint(path: "users/\(handle)/following", query: cursor.map { [URLQueryItem(name: "cursor", value: $0)] } ?? [])
    }

    /// The tags most used in the last two days.
    static let trendingTags = Endpoint(path: "tags/trending")

    /// The account's signed-in devices.
    static let sessions = Endpoint(path: "sessions")

    /// The signed-in user's own event stream (SSE): unread count, new works
    /// from followed accounts, deletions, balance.
    static let userEvents = Endpoint(path: "events")

    // MARK: Visions

    /// The vision tray: the reader's own ring and the accounts they follow.
    static let visions = Endpoint(path: "visions")

    /// Who has seen one of the reader's own visions.
    static func visionViewers(id: String) -> Endpoint {
        Endpoint(path: "visions/\(id)/viewers")
    }

    /// Rooms live now.
    static let live = Endpoint(path: "live")

    /// The scoreboard: every league this deployment follows and its games.
    /// `503 sports_unavailable` where no provider is configured.
    static func sports(league: String? = nil) -> Endpoint {
        Endpoint(path: "sports", query: league.map { [URLQueryItem(name: "league", value: $0)] } ?? [])
    }

    /// One stock quote, for the card under a work carrying `$TICKER`.
    /// `503 cashtag_unavailable` where no quotes provider is configured, and
    /// `404 cashtag_not_found` for a symbol nobody lists — which are different
    /// answers on purpose: the first means this deployment has no quotes at
    /// all, the second means that particular word was never a company.
    static func cashtag(ticker: String) -> Endpoint {
        Endpoint(path: "cashtag/\(ticker)")
    }

    /// The composer's `$` dropdown. A query under two characters answers an
    /// empty list rather than an error, so a call per keystroke is safe.
    static func cashtagSearch(_ query: String) -> Endpoint {
        Endpoint(path: "cashtag/search", query: [URLQueryItem(name: "q", value: query)])
    }

    /// One room with its chat backlog.
    static func liveRoom(id: String) -> Endpoint {
        Endpoint(path: "live/\(id)")
    }

    /// The room's event stream (SSE). Opened with `APIClient.liveEvents`.
    static func liveEvents(id: String) -> Endpoint {
        Endpoint(path: "live/\(id)/events")
    }

    // MARK: Frequencies

    /// One lane of the audio rooms: `live`, `scheduled`, `ended` or `mine`.
    /// The lane is the server's word, echoed back in the answer, so a client
    /// can tell which list it is holding without remembering what it asked.
    static func frequencies(lane: String, limit: Int = 24) -> Endpoint {
        Endpoint(
            path: "frequencies",
            query: [
                URLQueryItem(name: "lane", value: lane),
                URLQueryItem(name: "limit", value: String(limit)),
            ]
        )
    }

    /// One room: the frequency, its stage, its pending hands and what this
    /// reader may do in it.
    static func frequency(id: String) -> Endpoint {
        Endpoint(path: "frequencies/\(id)")
    }

    /// The room's event stream (SSE). Opened with ``FrequencyEventStream``.
    /// Holding it open is also what keeps the reader present: feed-engine
    /// heartbeats the brain for as long as this connection lives.
    static func frequencyEvents(id: String) -> Endpoint {
        Endpoint(path: "frequencies/\(id)/events")
    }

    // MARK: Writes
    //
    // These are not under api/v1 because api/v1 has no write surface. They are
    // the same D-070 lane the web app posts to, which is also why their answers
    // are shaped for HTMX — 204 with no body, or a line of plain text on error
    // — rather than for a JSON client. `APIClient.postEvent` handles that.

    /// The single mutation lane. Every state change carries an `event_type`;
    /// a body that also carries a `cid` is routed to the signed-work handler.
    static let events = Endpoint(method: .post, path: "events", root: .origin)

    // MARK: Messages
    //
    // Reads are under api/v1 like every other read. The sealed-mode key
    // routes are not: they are the same `/api/gnosis/*` endpoints the web
    // client calls, and they are already JSON, so the app uses them as they
    // are rather than growing a second copy of a surface that works.

    /// The reader's conversations, newest activity first.
    static let conversations = Endpoint(path: "messages")

    /// One conversation with a page of its messages, oldest first.
    ///
    /// `before` is the opaque cursor from a previous page and asks for what
    /// came before it, which is the direction a thread is read backwards in.
    static func messageThread(id: String, before: String? = nil, limit: Int = 50) -> Endpoint {
        var query = [URLQueryItem(name: "limit", value: String(limit))]
        if let before { query.append(URLQueryItem(name: "before", value: before)) }
        return Endpoint(path: "messages/\(id)", query: query)
    }

    // MARK: Numbers
    //
    // Two reads, both the owner's own. There is no route that resolves a
    // Number to a person and there must never be one: a Number is spent
    // through `POST /events`, where the answer is the same three states
    // whatever the reason, after a fixed delay. A lookup endpoint would be an
    // enumeration oracle with a nicer name.

    /// The account's own Numbers, its contact policy, and the policies this
    /// server offers.
    static let contactNumbers = Endpoint(path: "numbers")

    /// The contact requests waiting on the reader.
    static let contactRequests = Endpoint(path: "contact/requests")

    /// This account's own wrapped private key, for unwrapping at sign-in.
    static let gnosisBootstrap = Endpoint(path: "api/gnosis/bootstrap", root: .origin)

    /// Stores, or rotates, this account's messaging identity.
    static let gnosisProvision = Endpoint(
        method: .post, path: "api/gnosis/provision", root: .origin
    )

    /// The public keys of one conversation's members.
    ///
    /// Scoped to a conversation on purpose: an open directory is a harvesting
    /// surface, so the server only answers for rooms the caller is already in.
    static func gnosisDirectory(conversation: String) -> Endpoint {
        Endpoint(
            path: "api/gnosis/directory",
            query: [URLQueryItem(name: "c", value: conversation)],
            root: .origin
        )
    }

    /// Stores one sealed envelope and fans it out.
    static let gnosisSendSealed = Endpoint(
        method: .post, path: "api/gnosis/send-sealed", root: .origin
    )

    /// Hands this device's public signing key to the key authority.
    ///
    /// Nantar keeps no copy — it forwards to Elohim Veni and answers
    /// `502 key_authority_unavailable` if that refuses or is unreachable, which
    /// in a local deployment without Elohim Veni running is every time.
    static let registerSigningKey = Endpoint(
        method: .post, path: "api/pial/signing-key/register", root: .origin
    )

    // MARK: - Settings
    //
    // The account's own reads. Every one of them is about the reader and
    // nobody else, which is why none of them takes an argument: the server
    // knows who is asking from the bearer token, and a settings route that
    // accepted a handle would be a settings route somebody could point at
    // somebody else.

    /// Everyone this account has blocked, and everyone it has muted.
    static let blocks = Endpoint(path: "blocks")

    /// What Herald may send, and the quiet-hours window.
    static let notificationPreferences = Endpoint(path: "notifications/preferences")

    /// A fresh TOTP secret to enrol an authenticator with. Reading it does not
    /// turn two-factor on — the `two_fa_enable` event with a working code does,
    /// which is what proves the secret arrived intact.
    static let twoFactorSetup = Endpoint(path: "2fa/setup")

    /// Everything F33D3R holds about this account, as one JSON document. Read
    /// as bytes and handed to a share sheet rather than decoded: it is the
    /// reader's copy of their own data, not a model this app has a use for.
    static let accountExport = Endpoint(path: "account/export")

    /// One tab of a profile's works.
    static func profileWorks(
        handle: String,
        tab: Profile.Tab = .works,
        cursor: String? = nil,
        limit: Int = 20
    ) -> Endpoint {
        var query = [
            URLQueryItem(name: "tab", value: tab.rawValue),
            URLQueryItem(name: "limit", value: String(limit)),
        ]
        if let cursor { query.append(URLQueryItem(name: "cursor", value: cursor)) }
        return Endpoint(path: "users/\(handle)/works", query: query)
    }

    // MARK: - Quotes

    /// The works that quote one work.
    ///
    /// A page of ordinary works, the same shape `replies` answers with, so the
    /// screen behind "View quotes" is the same list every other list is.
    static func quotes(workID: String, cursor: String? = nil, limit: Int = 20) -> Endpoint {
        var query = [URLQueryItem(name: "limit", value: String(limit))]
        if let cursor { query.append(URLQueryItem(name: "cursor", value: cursor)) }
        return Endpoint(path: "works/\(workID)/quotes", query: query)
    }

}
