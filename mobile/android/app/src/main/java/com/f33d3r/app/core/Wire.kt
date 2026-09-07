package com.f33d3r.app.core

/**
 * A Wire is one destination inside the Shell: the frame stays, the content plane
 * changes. Each Wire names the server endpoint that renders its content facets, and
 * nothing else — the client never decides what a wire contains, only where to ask.
 *
 * The set is the website's set. Every page the web shell's navigation reaches is a
 * wire here, under the same name, so a person who knows one client knows the other.
 */
enum class Wire(val route: String, val title: String) {
    // ── The bottom rail ───────────────────────────────────────────────────────
    PLAYGROUND("playground", "Home"),
    EXPLORE("explore", "Explore"),
    VISIONS("visions", "Visions"),
    NOTIFICATIONS("notifications", "Notifications"),
    CHAT("messages", "Messages"),

    // ── The persona drawer, in the web navigation's order ─────────────────────
    PROFILE("profile", "Profile"),
    BOOKMARKS("bookmarks", "Bookmarks"),
    MUSIC("music", "Music"),
    GOLIVE("golive", "Go Live"),
    CREATOR("create", "Create"),
    ARTICLES("articles", "Articles"),
    LEADERBOARD("achievements", "Leaderboard"),
    WALLET("wallet", "Wallet"),
    MARKETPLACE("marketplace", "Market"),
    ANALYTICS("analytics", "Analytics"),
    LISTS("lists", "Lists"),
    SETTINGS("settings", "Settings"),
    ORG_PANEL("org", "Org Panel"),
    ADMIN("admin", "Admin"),

    // ── Reached from inside a facet: a link, a card, a row ────────────────────
    FOLLOWERS("followers", "Followers"),
    FOLLOWING("following", "Following"),
    SHOP("shop", "Shop"),
    LIBRARY("buyer-library", "Library"),
    LIVE("live", "Live"),
    LIST("list", "List"),
    ARTICLE("article", "Article"),
    SPORTS("sports", "Sports"),
    STOCKS("stocks", "Stocks"),
    TAG("tag", "Tag"),
    WORK("work", "Work"),
    THREAD("thread", "Thread"),
    SEARCH("search", "Search"),
}

/**
 * A content-plane request: the read-lane path plus the query the server needs to
 * render it. Kept as data so a Wire's content can be re-requested — on pull to
 * refresh, on tab change, on paging — without the surface knowing how it was built.
 */
data class ContentSource(
    val path: String,
    val query: Map<String, String> = emptyMap(),
) {
    /** The path and query as the server expects to receive them. */
    fun url(origin: String): String {
        val base = origin.trimEnd('/') + path
        if (query.isEmpty()) return base
        val encoded = query.entries
            .filter { it.value.isNotEmpty() }
            .joinToString("&") { (k, v) -> "$k=" + java.net.URLEncoder.encode(v, "UTF-8") }
        return if (encoded.isEmpty()) base else "$base?$encoded"
    }

    fun with(vararg pairs: Pair<String, String>) = copy(query = query + pairs.toMap())
}

/**
 * The read-lane endpoints this client projects.
 *
 * Every entry is a route the server already serves as a shell-less facet render. The
 * client adds no rendering endpoint of its own and asks for no page shell: the Shell
 * is native, so a full page would arrive carrying a second one.
 *
 * Two kinds of route are here. Facet endpoints (`/facets/...`, `/partials/...`) render
 * one facet by design. Page routes (`/lists`, `/search`, `/analytics`, ...) render the
 * website's page; asked for on the read lane they answer with the page's body as a
 * single facet, because the server knows a facet caller from a page load.
 */
object Endpoints {

    // ── Playground ────────────────────────────────────────────────────────────
    /** The timeline. `surface` selects which feed the server ranks and renders. */
    fun works(surface: String, sessionId: String) =
        ContentSource("/facets/works/feed", mapOf("surface" to surface, "session_id" to sessionId))

    /** One more page of [surface], continuing after the last work the surface holds. */
    fun worksAfter(surface: String, sessionId: String, after: String) =
        works(surface, sessionId).with("after" to after)

    /** The account's pinned interest surfaces, as the tab row the server decided on. */
    fun feedSurfaces(sessionId: String) =
        ContentSource("/feed/surfaces", mapOf("session_id" to sessionId))

    // ── Explore ───────────────────────────────────────────────────────────────
    val articles = ContentSource("/feed", mapOf("surface" to "articles"))
    fun sportsStrip(league: String) = ContentSource("/facets/sports/$league/strip")
    fun hashtagSearch(q: String) = ContentSource("/facets/hashtag/search", mapOf("q" to q))
    val marketplaceFeed = ContentSource("/facets/marketplace/feed")

    /** The search page: people, works, or both, for a query. */
    fun search(q: String, type: String) =
        ContentSource("/search", mapOf("q" to q, "type" to type))

    fun tag(name: String) = ContentSource("/tag/$name")
    fun stocks(ticker: String) = ContentSource("/stocks/$ticker")

    /** The scoreboard: every league, or one. */
    fun sports(league: String) =
        ContentSource(if (league.isEmpty()) "/sports" else "/sports/$league")

    fun sportsGame(league: String, game: String) = ContentSource("/sports/$league/$game")

    // ── Attention ─────────────────────────────────────────────────────────────
    val notifications = ContentSource("/facets/notifications")
    val conversations = ContentSource("/facets/messages_convos")

    /** The conversation list narrowed by the server to [q]; the server holds no filter for it. */
    fun conversations(q: String) =
        if (q.isBlank()) conversations else ContentSource("/facets/messages_convos", mapOf("q" to q.trim()))
    const val MESSAGES_NEW_NUMBER = "/messages/new-number"
    val messageRequests = ContentSource("/facets/messages_requests")
    fun thread(convoId: String) = ContentSource("/facets/messages_thread", mapOf("c" to convoId))

    // ── Works ─────────────────────────────────────────────────────────────────
    fun conversation(workId: String) = ContentSource("/facets/works/conversation/$workId")
    fun replies(workId: String) = ContentSource("/facets/works/replies/$workId")
    fun engagement(workId: String) = ContentSource("/facets/post/$workId/engagement")

    /** The author's own articles, as the dashboard the website draws. */
    val articlesDashboard = ContentSource("/articles")
    fun article(slug: String) = ContentSource("/article/$slug")

    // ── Identity and persona ──────────────────────────────────────────────────
    /**
     * The profile page: header, live stage, walls. The works below it are [profileWorks].
     * The clean URL is the server's canonical one; `/u/{handle}` only redirects to it.
     */
    fun profile(handle: String) = ContentSource("/$handle")

    fun profileWorks(handle: String, tab: String, sessionId: String) =
        ContentSource(
            "/facets/works/feed",
            mapOf("surface" to tab, "handle" to handle, "session_id" to sessionId),
        )

    fun followers(handle: String) = ContentSource("/$handle/followers")
    fun following(handle: String) = ContentSource("/$handle/following")
    fun shop(handle: String) = ContentSource("/shop/$handle")

    /** The ring rail above the timeline: one ring per author with a live Vision, each keyed by that author's PIAL. */
    val visionRings = ContentSource("/facets/vision/rings")

    /**
     * One frame of the sequential Vision viewer, resolved and sequenced by the server.
     *
     * [vision] is the Vision's address, `<author_pial>.<seq>`, exactly as the server
     * wrote it into a ring or into the frame's own next and previous controls. It is
     * handed back as written: nothing here reads a PIAL or a sequence out of it.
     */
    fun visionViewer(vision: String, scope: String) =
        ContentSource("/facets/vision/viewer", mapOf("vision" to vision, "scope" to scope))

    /** The Vision composer the server renders; its markup states what this account may post. */
    fun visionComposer(mode: String) = ContentSource("/facets/vision/composer", mapOf("mode" to mode))

    /** The Visions page whole: the ring rail and the reels under it. */
    val visions = ContentSource("/visions")

    // ── Live ──────────────────────────────────────────────────────────────────
    /** The broadcaster's own surface for a stream they hold. */
    fun liveBroadcast(id: String, facing: String, source: String) =
        ContentSource("/live/$id/broadcast", mapOf("facing" to facing, "source" to source))

    /** The HLS ladder the media server writes for a running broadcast. */
    fun liveMaster(id: String) = "/live/$id/master.m3u8"
    fun liveWhip(id: String) = "/live/$id/whip"
    fun liveEnd(id: String) = "/live/$id/end"
    fun liveHeartbeat(id: String) = "/live/$id/heartbeat"
    fun liveChat(id: String) = "/live/$id/chat"

    /** Where a resumable video upload's transcode stands. */
    fun tusStatus(id: String) = "/upload/tus/status/$id"

    // ── Panels the drawer opens ───────────────────────────────────────────────
    fun settings(section: String) = ContentSource("/partials/settings/$section")

    /**
     * The website's profile editor, which is also where it keeps the accent theme
     * and the other display choices. Shown as the Display section of settings.
     */
    val editProfile = ContentSource("/facets/edit_profile_modal")
    fun wallet(section: String) = ContentSource("/wallet/partials/$section")
    val bookmarks = ContentSource("/bookmarks/items")
    val lists = ContentSource("/lists")
    fun list(id: String) = ContentSource("/lists/$id")
    val liveCards = ContentSource("/facets/live/cards")
    fun liveWatch(id: String) = ContentSource("/live/$id")
    val goLive = ContentSource("/golive")
    val referralPanel = ContentSource("/facets/referral_panel")
    val leaderboard = ContentSource("/achievements")
    fun creatorPanel(section: String) = ContentSource("/create/partials/$section")
    fun analytics(period: String) = ContentSource("/analytics", mapOf("period" to period))
    val orgPanel = ContentSource("/org/panel")
    fun adminPanel(section: String) = ContentSource("/admin/partials/$section")
    val library = ContentSource("/buyer-library")

    /** The track list, for one genre or for all of them. */
    fun musicTracks(genre: String) = ContentSource("/music/tracks", mapOf("genre" to genre))

    /**
     * The server's render of a work being quoted, for the composer's preview. The
     * composer never assembles the quoted work from strings it was handed: the server
     * recalls the whole work by id and renders it.
     */
    fun quotedWork(workId: String) = ContentSource("/facets/quoted_post", mapOf("post_id" to workId))

    // ── Write lane ────────────────────────────────────────────────────────────
    const val EVENTS = "/events"
    /** Where a photo attached to a work is stored before the work cites it. */
    const val POST_MEDIA = "/upload/post-media"
    const val VISIONS = "/visions"
    const val VISION_ARTBOARD = "/facets/vision/artboard"
    const val LIVE_START = "/live/start"
    const val LOGIN = "/login"
    const val LOGOUT = "/logout"
    const val LIVE_STREAM = "/api/events"
    const val WATCH = "/api/events/watch"
    const val FOLLOW = "/api/follow"
    const val NOTIFICATIONS_READ_ALL = "/api/notifications/read-all"

    /** Where the profile editor's form posts; the accent theme is saved through it. */
    const val PROFILE_SAVE = "/profile/save"
    const val GNOSIS_BOOTSTRAP = "/api/gnosis/bootstrap"
    const val GNOSIS_PROVISION = "/api/gnosis/provision"
    const val GNOSIS_DIRECTORY = "/api/gnosis/directory"
    const val GNOSIS_SEND_SEALED = "/api/gnosis/send-sealed"
    const val MESSAGES_SEND = "/messages/send"
    const val MESSAGES_NEW = "/messages/new"
    const val PIAL_SIGNING_KEY = "/api/pial/signing-key/register"
    const val USERS_SEARCH = "/api/users/search"
    const val PROFILES_MINI = "/api/profiles/mini"
    const val ROOT = "/"
}
