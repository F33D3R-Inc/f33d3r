package com.f33d3r.app.ui

import androidx.compose.runtime.Immutable
import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import com.f33d3r.app.Runtime
import com.f33d3r.app.compose.Mention
import com.f33d3r.app.compose.PickedFile
import com.f33d3r.app.compose.ReplyTarget
import com.f33d3r.app.compose.WorkDraft
import com.f33d3r.app.pial.Publication
import com.f33d3r.app.pial.WorkAttachments
import com.f33d3r.app.pial.WorkPayload
import com.f33d3r.app.net.JsonAnswer
import com.f33d3r.app.core.ContentSource
import com.f33d3r.app.core.Endpoints
import com.f33d3r.app.core.Fragment
import com.f33d3r.app.core.Wire
import com.f33d3r.app.net.Identity
import com.f33d3r.app.net.LaneResult
import com.f33d3r.app.net.Session
import com.f33d3r.app.net.WhoAmI
import com.f33d3r.app.ui.theme.Ink
import com.f33d3r.app.net.live.FaLive
import com.f33d3r.app.net.live.LiveState
import com.f33d3r.app.net.live.Mutation
import com.f33d3r.app.net.live.SignalKind
import com.f33d3r.app.seal.SealState
import com.f33d3r.app.ui.surface.FacetIntent
import com.f33d3r.app.ui.surface.Swipe
import com.f33d3r.app.upload.UploadState
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharedFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asSharedFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import java.io.File
import java.util.UUID

/** One tab in a wire's tab row, as the server named it. */
@Immutable
data class SurfaceTab(val id: String, val label: String)

/**
 * A surface the Shell lifts above the timeline: a Vision frame, a composer, a
 * broadcast, a stream being watched. Each holds only what the native frame around it
 * needs — an address and a name or two, read off the server's own render — never
 * what the surface shows.
 */
sealed interface Overlay {
    data object None : Overlay

    /** The sequential Vision viewer, on one author's run. */
    data class Vision(val authorHandle: String, val scope: String) : Overlay

    /** The camera-first Vision composer. [canNsfw] is what the server's composer offered. */
    data class VisionCompose(val canNsfw: Boolean) : Overlay

    /**
     * The work composer, lifted over the frame on [draft]. [posting] is true while
     * the server is being asked; [refusal] is the server's own word when it said no.
     */
    data class Compose(
        val draft: WorkDraft,
        val posting: Boolean = false,
        val refusal: String = "",
    ) : Overlay

    /** The go-live sheet. [error] is the server's own rejection of the last attempt. */
    data class GoLive(val canNsfw: Boolean, val error: String = "") : Overlay

    /** The broadcaster's surface for a stream this account holds. */
    data class Broadcast(
        val streamId: String,
        val frontCamera: Boolean,
        val source: String,
        val ended: Boolean,
    ) : Overlay

    /** A stream being watched; [minimized] keeps the sound and shows a bar instead. */
    data class Watch(
        val streamId: String,
        val authorHandle: String,
        val title: String,
        val minimized: Boolean = false,
    ) : Overlay
}

/** What the Shell is currently showing, and what its chrome must display. */
@Immutable
data class ShellState(
    val identity: Identity = Identity.NONE,
    val wire: Wire = Wire.PLAYGROUND,
    val tabs: List<SurfaceTab> = emptyList(),
    val selectedTab: String = "",
    val notificationBadge: String = "",
    val messageBadge: Int = 0,
    val balance: String = "",
    val connection: LiveState = LiveState.DROPPED,
    val pendingWorks: Int = 0,
    val sealState: SealState = SealState.Locked,
    val loading: Boolean = false,
    val refusal: String = "",
    /** Set when a wire is scoped to something: a handle, a conversation, a work. */
    val subject: String = "",
    /** On the thread wire: who the conversation is with, as the server's head names them. */
    val threadTitle: String = "",
    /** On the thread wire: the handle and the seal the server's head states. */
    val threadSubtitle: String = "",
    /** "card" or "compact": how tightly the timeline is set. */
    val density: String = Session.DENSITY_CARD,
    /** "dark" or "light": which of the stylesheet's palettes the frame is drawn in. */
    val mode: String = Session.MODE_DARK,
    /** The surface lifted above the timeline, if any. */
    val overlay: Overlay = Overlay.None,
    /** The handle a tip sheet is open for; empty when none is. */
    val tipHandle: String = "",
    /** A short word from the Shell or from a facet's toast; clears itself. */
    val notice: String = "",
    /** What the Shell says when a lane's server answer was empty. */
    val emptyLane: String = "",
)

/** Something the Shell must do to its surface that state alone cannot express. */
sealed interface SurfaceCommand {
    /** Replace the content plane with this render. */
    data class Mount(val fragment: Fragment) : SurfaceCommand

    /** Add the next page to the end of the content plane. */
    data class Extend(val fragment: Fragment) : SurfaceCommand

    /** Replace one facet in place. */
    data class Apply(val fragment: Fragment) : SurfaceCommand

    /** Append into a named container, when that container is on screen. */
    data class Append(val container: String, val fragment: Fragment) : SurfaceCommand

    /** Put a render inside the slot a control named, replacing what the slot held. */
    data class Place(val container: String, val fragment: Fragment) : SurfaceCommand

    /** Insert facets the reader has now asked to see. */
    data class Prepend(val fragments: List<Fragment>) : SurfaceCommand

    /** Take a facet off the surface. */
    data class Remove(val address: String) : SurfaceCommand

    /** A sealed bubble's plaintext, for the bubble at this address. */
    data class Reveal(val address: String, val plaintext: String) : SurfaceCommand

    /** A sealed bubble this identity cannot open. */
    data class SealedFailed(val address: String, val message: String) : SurfaceCommand

    /** The composer at this address has been accepted and should be emptied. */
    data class ClearComposer(val address: String) : SurfaceCommand

    /** Read the sealed bubbles on screen and open the ones this device can. */
    data object OpenSealed : SurfaceCommand

    /** Start the Vision frame's clip, the way the web viewer does on arrival. */
    data object PlayVision : SurfaceCommand

    /** Press the server-rendered control [selector] names — a native swipe taking a tap's path. */
    data class Step(val selector: String) : SurfaceCommand

    /** Set an attribute on the host document; the stylesheet reads it (density, chat shown or not). */
    data class Attr(val name: String, val value: String) : SurfaceCommand
}

/**
 * The Shell's own state, and the only place a wire decides what to ask the server for.
 *
 * What it holds is presentation and address: which wire is open, which tab is
 * selected, what the badges say. What it does not hold is the application — no works,
 * no messages, no counts derived from anything, no rendered markup. Every one of
 * those lives on the server and arrives already rendered, so the honest test of this
 * class is that nothing in it could disagree with the server about the application's
 * state, because it holds none of it.
 */
class ShellModel(private val runtime: Runtime) : ViewModel() {

    private val _state = MutableStateFlow(
        ShellState(
            identity = runtime.session.identity.value,
            density = runtime.session.density,
            mode = runtime.session.mode,
        )
    )

    /**
     * The home lanes: the core surfaces, then the interest surfaces the server has
     * pinned for this account. Kept apart from the open wire's tab row so that
     * returning to the timeline restores the timeline's tabs and not whichever
     * strip the last wire drew.
     */
    private var homeTabs: List<SurfaceTab> = CORE_SURFACES
    val state: StateFlow<ShellState> = _state.asStateFlow()

    private val _commands = MutableSharedFlow<SurfaceCommand>(extraBufferCapacity = 64)
    val commands: SharedFlow<SurfaceCommand> = _commands.asSharedFlow()

    /** Commands for the lifted surface. Same vocabulary, second content plane. */
    private val _overlayCommands = MutableSharedFlow<SurfaceCommand>(extraBufferCapacity = 64)
    val overlayCommands: SharedFlow<SurfaceCommand> = _overlayCommands.asSharedFlow()

    /**
     * Identifies this reading session to the ranker.
     *
     * The server correlates a surface's requests to rank what comes next, so paging
     * must carry the same session as the first page or the second page is ranked as
     * though nobody had read the first.
     */
    private val sessionId = UUID.randomUUID().toString()

    /** The server's renders of works published since this surface was drawn. */
    private val heldWorks = mutableListOf<Fragment>()

    /** The last render each wire received, replayed on return while a fresh one loads. */
    private val lastRender = mutableMapOf<Wire, Fragment>()

    /** The render the lifted surface shows; mounted when its document says it is ready. */
    private var overlayRender: Fragment? = null

    private var pagingCursor = ""
    private var pagingInFlight = false
    private var noticeJob: Job? = null
    private var heartbeatJob: Job? = null

    /** The presence token the watch surface beats with, read off the server's render. */
    private var viewerToken = ""

    init {
        Ink.apply(runtime.session.identity.value.theme, runtime.session.mode)
        observeLive()
        viewModelScope.launch { bootstrap() }
    }

    // ── Session ───────────────────────────────────────────────────────────────

    private suspend fun bootstrap() {
        if (runtime.session.token == null) return
        val identity = when (val answer = runtime.client.whoAmI()) {
            is WhoAmI.Known -> answer.identity

            WhoAmI.Nobody -> {
                // The token no longer names anyone. Say so rather than showing a shell
                // that looks signed in and answers 401 to everything inside it.
                runtime.session.forget()
                _state.update { it.copy(identity = Identity.NONE) }
                return
            }

            // Nothing was learned. The persona this device last knew stays on the
            // frame, the stream keeps trying, and every surface says plainly that
            // it is not live — a server that is down has not ended a session, and
            // must not cost this device its sealed keys.
            WhoAmI.Unreachable -> runtime.session.identity.value.takeIf { it.isSignedIn } ?: return
        }
        adoptIdentity(identity)
        _state.update { it.copy(sealState = runtime.gnosis.state(identity.account)) }
        runtime.live.connect()
        viewModelScope.launch { runtime.malkuth.registerDevice() }
        loadTabs()
        open(Wire.PLAYGROUND)
    }

    /**
     * Signs in, then establishes the sealed-messaging identity in the same step.
     *
     * The password is the only key that unwraps the account's messaging private key,
     * and it exists in this process for exactly this call. Deferring the unwrap to
     * later would mean either storing the password or asking for it twice.
     */
    fun signIn(handle: String, password: String) = viewModelScope.launch {
        _state.update { it.copy(loading = true, refusal = "") }
        when (val result = runtime.client.signIn(handle, password)) {
            is LaneResult.Accepted -> {
                val identity = (runtime.client.whoAmI() as? WhoAmI.Known)?.identity
                if (identity == null) {
                    _state.update { it.copy(loading = false, refusal = "Signed in, but the server did not say who as.") }
                    return@launch
                }
                adoptIdentity(identity)
                val sealState = runtime.gnosis.establish(identity.account, identity.handle, password)
                _state.update { it.copy(sealState = sealState, loading = false) }
                runtime.live.connect()
                runtime.malkuth.registerDevice()
                loadTabs()
                open(Wire.PLAYGROUND)
            }

            is LaneResult.Refused ->
                _state.update { it.copy(loading = false, refusal = result.message) }

            is LaneResult.Unreachable ->
                _state.update { it.copy(loading = false, refusal = "Could not reach the server.") }

            else -> _state.update { it.copy(loading = false, refusal = "Sign-in was refused.") }
        }
    }

    fun signOut() = viewModelScope.launch {
        heartbeatJob?.cancel()
        runtime.playback.stop()
        runtime.publisher.stop()
        runtime.live.disconnect()
        runtime.deviceKey.forget()
        runtime.client.signOut()
        runtime.drafts.clear()
        lastRender.clear()
        heldWorks.clear()
        overlayRender = null
        _state.value = ShellState()
    }

    /** Points this install at a different nantar instance. */
    fun useOrigin(origin: String) {
        runtime.session.origin = origin
    }

    /**
     * Takes the identity the server reported: remembered for the next launch, shown
     * in the frame, and its accent theme resolved into the palette the chrome draws
     * with — the theme is the persona's, so it arrives with the persona.
     */
    private fun adoptIdentity(identity: Identity) {
        runtime.session.adopt(identity)
        Ink.apply(identity.theme, runtime.session.mode)
        _state.update { it.copy(identity = identity) }
    }

    /**
     * Re-reads who is signed in, after the server has been told something about the
     * persona — a profile save, a theme change — so the frame shows what the server
     * now says rather than what it said at sign-in.
     */
    fun refreshIdentity() = viewModelScope.launch {
        (runtime.client.whoAmI() as? WhoAmI.Known)?.let { adoptIdentity(it.identity) }
    }

    /** Dark or light. A choice about the frame, kept with the frame's other choices. */
    fun toggleMode() {
        val next = if (_state.value.mode == Session.MODE_LIGHT) Session.MODE_DARK else Session.MODE_LIGHT
        runtime.session.mode = next
        Ink.apply(_state.value.identity.theme, next)
        _state.update { it.copy(mode = next) }
    }

    // ── Navigation ────────────────────────────────────────────────────────────

    /**
     * Opens a wire, optionally scoped to a subject — a handle, a work, a thread, a
     * query — and optionally on a named tab. On a sectioned wire (settings, wallet,
     * create, admin) the subject is the section, because that is what a link to
     * `/settings/security` means.
     *
     * Two wires are not pages here. Going live and watching a stream are lifted
     * surfaces over whatever is open, so a link to either lifts one rather than
     * replacing the timeline underneath it.
     */
    fun open(wire: Wire, subject: String = "", tab: String = "") {
        if (wire == Wire.GOLIVE) {
            goLive()
            return
        }
        if (wire == Wire.LIVE && subject.isNotEmpty()) {
            watchLive(subject)
            return
        }
        val tabs = tabsFor(wire)
        val selected = when {
            tab.isNotEmpty() -> tab
            // The timeline opens on the lane the reader left, across launches.
            wire == Wire.PLAYGROUND -> _state.value.selectedTab
                .ifEmpty { runtime.session.homeLane }
                .takeIf { lane -> tabs.any { it.id == lane } }
                ?: CORE_SURFACES.first().id
            wire in SECTIONED && subject.isNotEmpty() -> subject
            // A menu wire opens on its menu — a vertical list of sections — not on
            // whichever section happens to be first.
            wire in MENU_WIRES -> ""
            else -> tabs.firstOrNull()?.id.orEmpty()
        }
        _state.update {
            it.copy(
                wire = wire,
                subject = subject,
                selectedTab = selected,
                tabs = tabs,
                pendingWorks = if (wire == Wire.PLAYGROUND) it.pendingWorks else 0,
                emptyLane = "",
                threadTitle = "",
                threadSubtitle = "",
            )
        }
        lastRender[wire]?.let { render -> emit(SurfaceCommand.Mount(render)) }
        loadContent()
    }

    /** Searches, keeping the search tab the reader is on. */
    fun search(query: String) {
        val current = _state.value
        val tab = if (current.wire == Wire.SEARCH) current.selectedTab else ""
        open(Wire.SEARCH, query.trim(), tab)
    }

    /** Selects a tab within the open wire. */
    fun selectTab(id: String) {
        if (_state.value.selectedTab == id) return
        if (_state.value.wire == Wire.PLAYGROUND) runtime.session.homeLane = id
        _state.update { it.copy(selectedTab = id, emptyLane = "") }
        loadContent()
    }

    /** Back from a section of a menu wire: the menu again. */
    fun backToMenu() {
        if (_state.value.wire !in MENU_WIRES) return
        _state.update { it.copy(selectedTab = "") }
        loadContent()
    }

    /** A sideways swipe on the timeline moves one lane; the tab row follows. */
    fun swipeLane(swipe: Swipe) {
        val current = _state.value
        if (current.wire != Wire.PLAYGROUND || current.tabs.isEmpty()) return
        val index = current.tabs.indexOfFirst { it.id == current.selectedTab }
        val next = when (swipe) {
            Swipe.LEFT -> index + 1
            Swipe.RIGHT -> index - 1
            Swipe.DOWN -> return
        }
        current.tabs.getOrNull(next)?.let { selectTab(it.id) }
    }

    /** Card or compact. A choice about the frame, kept with the frame's other choices. */
    fun toggleDensity() {
        val next = if (_state.value.density == Session.DENSITY_COMPACT) Session.DENSITY_CARD else Session.DENSITY_COMPACT
        runtime.session.density = next
        _state.update { it.copy(density = next) }
    }

    // ── Content ───────────────────────────────────────────────────────────────

    /**
     * Asks the server to render the open wire, and mounts what comes back.
     *
     * Nothing about the result is predicted here. If the server renders an empty
     * state, the empty state is what the reader sees, because the server decided the
     * surface is empty — the client has no separate opinion to fall back on.
     */
    fun loadContent() = viewModelScope.launch {
        val state = _state.value
        if (state.wire in MENU_WIRES && state.selectedTab.isEmpty()) {
            // The menu is native chrome; the content plane holds nothing until a
            // section is chosen.
            pagingCursor = ""
            emit(SurfaceCommand.Mount(Fragment(state.wire.route, null, "")))
            _state.update { it.copy(loading = false, refusal = "") }
            return@launch
        }
        val source = sourceFor(state) ?: return@launch

        pagingCursor = ""
        _state.update { it.copy(loading = true, refusal = "") }

        when (val result = runtime.client.read(source)) {
            is LaneResult.Rendered -> {
                lastRender[state.wire] = result.fragment
                // The rings rail is its own facet above the timeline. It is mounted first
                // and the timeline extends it, so both arrive in one surface rather than
                // two — a second surface would mean a second WebView holding a second
                // copy of the stylesheet for one strip of avatars.
                val prelude = preludeFor(state)
                if (prelude != null) {
                    when (val rail = runtime.client.read(prelude)) {
                        is LaneResult.Rendered -> {
                            emit(SurfaceCommand.Mount(rail.fragment))
                            emit(SurfaceCommand.Extend(result.fragment))
                        }
                        else -> emit(SurfaceCommand.Mount(result.fragment))
                    }
                } else {
                    emit(SurfaceCommand.Mount(result.fragment))
                }
                if (state.wire == Wire.PLAYGROUND) {
                    heldWorks.clear()
                    _state.update { it.copy(pendingWorks = 0, emptyLane = "") }
                }
                if (state.wire == Wire.THREAD) {
                    val head = threadHeadOf(result.fragment.html)
                    _state.update { it.copy(threadTitle = head.first, threadSubtitle = head.second) }
                }
                if (state.wire == Wire.CHAT || state.wire == Wire.THREAD) {
                    emit(SurfaceCommand.OpenSealed)
                }
                _state.update { it.copy(loading = false) }
            }

            is LaneResult.Unauthorized -> {
                _state.update { it.copy(loading = false) }
                signOut()
            }

            is LaneResult.Refused ->
                _state.update { it.copy(loading = false, refusal = result.message.take(240)) }

            is LaneResult.Unreachable ->
                _state.update { it.copy(loading = false, refusal = "Could not reach the server.") }

            LaneResult.Accepted -> {
                // The server answered with nothing to draw. The live strip is written
                // that way when nobody is live, so the plane is cleared and the Shell
                // says why in its own words rather than leaving the last lane on screen.
                lastRender.remove(state.wire)
                emit(SurfaceCommand.Mount(Fragment(source.path, null, "")))
                _state.update {
                    it.copy(
                        loading = false,
                        emptyLane = if (state.wire == Wire.PLAYGROUND && state.selectedTab == LANE_LIVE) {
                            "Nobody is live right now."
                        } else {
                            ""
                        },
                    )
                }
            }
        }
    }

    /** Asks for the next page of the open surface, when the server left a cursor. */
    fun pageForward(cursor: String) {
        if (pagingInFlight || cursor.isEmpty() || cursor == pagingCursor) return
        val state = _state.value
        if (!pagesForward(state)) return

        pagingCursor = cursor
        pagingInFlight = true
        viewModelScope.launch {
            val source = sourceFor(state)?.with("after" to cursor)
            if (source != null) {
                (runtime.client.read(source) as? LaneResult.Rendered)?.let {
                    emit(SurfaceCommand.Extend(it.fragment))
                }
            }
            pagingInFlight = false
        }
    }

    /** Puts the works the reader has been holding onto the top of the timeline. */
    fun showHeldWorks() {
        if (heldWorks.isEmpty()) return
        emit(SurfaceCommand.Prepend(heldWorks.toList()))
        heldWorks.clear()
        _state.update { it.copy(pendingWorks = 0) }
    }

    /** Loads the tab row the server keeps for this account. */
    private fun loadTabs() = viewModelScope.launch {
        val result = runtime.client.read(Endpoints.feedSurfaces(sessionId))
        val pinned = (result as? LaneResult.Rendered)
            ?.let { parseSurfaceTabs(it.fragment.html) }
            .orEmpty()
        homeTabs = CORE_SURFACES + pinned
        // The row on screen follows only if the timeline is what is on screen.
        _state.update { if (it.wire == Wire.PLAYGROUND) it.copy(tabs = homeTabs) else it }
    }

    private fun sourceFor(state: ShellState): ContentSource? = when (state.wire) {
        Wire.PLAYGROUND -> when (state.selectedTab) {
            LANE_MUSIC -> Endpoints.musicTracks("")
            LANE_VISIONS -> Endpoints.visions
            LANE_LIVE -> Endpoints.liveCards
            else -> Endpoints.works(state.selectedTab.ifEmpty { "following" }, sessionId)
        }

        Wire.EXPLORE -> when (state.selectedTab) {
            "news" -> Endpoints.articles
            "sports" -> Endpoints.sports("nfl")
            "market" -> Endpoints.marketplaceFeed
            else -> Endpoints.works("trending", sessionId)
        }

        Wire.VISIONS -> Endpoints.visions
        Wire.NOTIFICATIONS -> Endpoints.notifications
        Wire.CHAT -> Endpoints.conversations(state.subject)
        Wire.THREAD -> Endpoints.thread(state.subject)
        Wire.WORK -> Endpoints.conversation(state.subject)

        Wire.PROFILE -> Endpoints.profileWorks(
            handle = state.subject.ifEmpty { state.identity.handle },
            tab = state.selectedTab.ifEmpty { "profile_posts" },
            sessionId = sessionId,
        )
        Wire.FOLLOWERS -> Endpoints.followers(state.subject.ifEmpty { state.identity.handle })
        Wire.FOLLOWING -> Endpoints.following(state.subject.ifEmpty { state.identity.handle })
        Wire.SHOP -> Endpoints.shop(state.subject.ifEmpty { state.identity.handle })

        Wire.SEARCH -> Endpoints.search(state.subject, state.selectedTab.ifEmpty { "top" })
        Wire.TAG -> Endpoints.tag(state.subject)
        Wire.STOCKS -> Endpoints.stocks(state.subject)
        Wire.SPORTS -> sportsSource(state)
        Wire.LISTS -> Endpoints.lists
        Wire.LIST -> Endpoints.list(state.subject)
        Wire.ARTICLES -> Endpoints.articlesDashboard
        Wire.ARTICLE -> Endpoints.article(state.subject)

        Wire.SETTINGS -> when (state.selectedTab) {
            // Display is the website's profile editor: the accent theme lives there.
            SETTINGS_DISPLAY -> Endpoints.editProfile
            else -> Endpoints.settings(state.selectedTab.ifEmpty { "account" })
        }
        Wire.WALLET -> Endpoints.wallet(state.selectedTab.ifEmpty { "overview" })
        Wire.BOOKMARKS -> Endpoints.bookmarks
        Wire.MUSIC -> Endpoints.musicTracks(if (state.selectedTab == "all") "" else state.selectedTab)
        Wire.GOLIVE -> Endpoints.goLive
        Wire.LIVE ->
            if (state.subject.isEmpty()) Endpoints.liveCards else Endpoints.liveWatch(state.subject)
        Wire.MARKETPLACE -> Endpoints.marketplaceFeed
        Wire.LIBRARY -> Endpoints.library
        Wire.CREATOR -> Endpoints.creatorPanel(state.selectedTab.ifEmpty { "overview" })
        Wire.LEADERBOARD -> Endpoints.leaderboard
        Wire.ANALYTICS -> Endpoints.analytics(state.selectedTab.ifEmpty { "30" })
        Wire.ORG_PANEL -> Endpoints.orgPanel
        Wire.ADMIN -> Endpoints.adminPanel(state.selectedTab.ifEmpty { "overview" })
    }

    /** The board for the selected league, or one game when the subject names it. */
    private fun sportsSource(state: ShellState): ContentSource {
        val parts = state.subject.split('/').filter { it.isNotEmpty() }
        return when {
            parts.size >= 2 -> Endpoints.sportsGame(parts[0], parts[1])
            state.selectedTab.isEmpty() || state.selectedTab == "all" -> Endpoints.sports("")
            else -> Endpoints.sports(state.selectedTab)
        }
    }

    /** Whether the open surface is one the server pages with a cursor. */
    private fun pagesForward(state: ShellState): Boolean = when (state.wire) {
        Wire.PLAYGROUND -> state.selectedTab !in PAGE_LANES
        Wire.PROFILE -> true
        Wire.EXPLORE -> state.selectedTab == EXPLORE_TABS.first().id
        else -> false
    }

    /**
     * The facet that is drawn above a wire's own content, when it has one.
     *
     * The timeline has the rings rail; a profile has its header — avatar, bio, counts,
     * the live stage, and whatever wall the server has put in front of the works. Each
     * is a separate read because it is a separate facet with its own lifetime: the
     * rail mutates when someone posts a Vision, the header when someone follows, and
     * neither has anything to do with the works below it.
     */
    private fun preludeFor(state: ShellState): ContentSource? = when {
        state.wire == Wire.PLAYGROUND && state.selectedTab == "following" -> Endpoints.visionRings
        // Who is asking to reach this person sits above their conversations, as on the web.
        state.wire == Wire.CHAT && state.subject.isEmpty() -> Endpoints.messageRequests
        state.wire == Wire.PROFILE -> Endpoints.profile(state.subject.ifEmpty { state.identity.handle })
        else -> null
    }

    /**
     * The tab row a wire draws in the Shell's chrome. Each mirrors the tab strip the
     * website draws on the same page, so the two clients section a page identically.
     */
    private fun tabsFor(wire: Wire): List<SurfaceTab> = when (wire) {
        Wire.PLAYGROUND -> homeTabs
        Wire.EXPLORE -> EXPLORE_TABS
        Wire.PROFILE -> PROFILE_TABS
        Wire.SEARCH -> SEARCH_TABS
        Wire.ANALYTICS -> ANALYTICS_TABS
        Wire.WALLET -> WALLET_TABS
        Wire.CREATOR -> CREATOR_TABS
        Wire.SETTINGS -> SETTINGS_TABS
        Wire.ADMIN -> ADMIN_TABS
        Wire.MUSIC -> MUSIC_TABS
        Wire.SPORTS -> SPORTS_TABS
        else -> emptyList()
    }

    // ── FA Live ───────────────────────────────────────────────────────────────

    /**
     * Applies every mutation the server publishes.
     *
     * The rule the whole client turns on: a mutation carries the server's render, and
     * this is where it is placed. Not merged, not recomputed from, not used to
     * invalidate a cache and trigger a refetch — placed. A re-render is offered to
     * both content planes; the one that holds the address takes it.
     */
    private fun observeLive() = viewModelScope.launch {
        runtime.live.mutations.collect { mutation ->
            when (mutation) {
                is Mutation.Signal -> applySignal(mutation)

                is Mutation.Replace -> emitBoth(SurfaceCommand.Apply(mutation.fragment))

                is Mutation.Append -> {
                    emitBoth(SurfaceCommand.Append(mutation.container, mutation.fragment))
                    if (mutation.container == FaLive.GNOSIS_MESSAGES) {
                        emit(SurfaceCommand.OpenSealed)
                    }
                }

                is Mutation.Remove -> emitBoth(SurfaceCommand.Remove(mutation.address))

                is Mutation.Pending -> {
                    // Held rather than inserted: the reader is looking at the timeline,
                    // and moving it under them is not an improvement.
                    heldWorks += mutation.fragment
                    _state.update { it.copy(pendingWorks = heldWorks.size) }
                }
            }
        }
    }

    private fun applySignal(signal: Mutation.Signal) {
        when (signal.kind) {
            SignalKind.NOTIFY -> _state.update { it.copy(notificationBadge = signal.value) }

            SignalKind.NOTIFY_ARRIVED ->
                // The count grew. The list the server would render now is a different
                // list, so ask for it rather than deciding what changed.
                if (_state.value.wire == Wire.NOTIFICATIONS) loadContent()

            SignalKind.BALANCE -> _state.update { it.copy(balance = signal.value) }

            SignalKind.MESSAGE_ARRIVED ->
                _state.update { it.copy(messageBadge = it.messageBadge + 1) }

            SignalKind.CONNECTION ->
                _state.update {
                    it.copy(connection = runCatching { LiveState.valueOf(signal.value) }.getOrDefault(LiveState.DROPPED))
                }
        }
    }

    // ── Sealed messaging ──────────────────────────────────────────────────────

    /**
     * Opens the sealed bubbles a surface reported.
     *
     * The private key stays on this side of the bridge: ciphertext comes out of the
     * surface, plaintext goes back in, and the key is never in a document that renders
     * anyone else's markup.
     */
    fun openSealed(bubbles: List<com.f33d3r.app.ui.surface.PendingBubble>) {
        val account = _state.value.identity.account
        if (account.isEmpty()) return
        viewModelScope.launch {
            bubbles.forEach { bubble ->
                val plaintext = runtime.gnosis.open(
                    account = account,
                    ephPubB64 = bubble.ephPubB64,
                    sealedKeyB64 = bubble.sealedKeyB64,
                    sealedNonceB64 = bubble.sealedNonceB64,
                    bodyCtB64 = bubble.bodyCtB64,
                    bodyNonceB64 = bubble.bodyNonceB64,
                )
                if (plaintext != null) {
                    emit(SurfaceCommand.Reveal(bubble.address, plaintext))
                } else {
                    emit(
                        SurfaceCommand.SealedFailed(
                            bubble.address,
                            if (runtime.gnosis.state(account) == SealState.Unlocked) {
                                "🔒 (cannot decrypt)"
                            } else {
                                "🔒 Sign in again to read"
                            },
                        )
                    )
                }
            }
        }
    }

    /** Seals a composed message to the conversation and hands it to the relay. */
    fun sealAndSend(convoId: String, body: String, composerAddress: String) =
        viewModelScope.launch {
            val recipients = runtime.gnosis.directory(convoId)
            if (recipients.isEmpty()) {
                _state.update { it.copy(refusal = "No encryption keys for the recipients yet.") }
                return@launch
            }
            val changed = runtime.gnosis.keyChanges(recipients)
            if (changed.isNotEmpty()) {
                // A pinned key changed. This client will not decide whether that is a new
                // device or an interception, so it stops and says so.
                _state.update {
                    it.copy(
                        refusal = changed.joinToString(", ") { change -> change.account } +
                            " changed encryption key. Review before sending.",
                    )
                }
                return@launch
            }
            when (val result = runtime.gnosis.send(convoId, recipients, body)) {
                is LaneResult.Rendered -> {
                    runtime.gnosis.pin(recipients)
                    emit(SurfaceCommand.Append(FaLive.GNOSIS_MESSAGES, result.fragment))
                    emit(SurfaceCommand.ClearComposer(composerAddress))
                    emit(SurfaceCommand.OpenSealed)
                }

                is LaneResult.Accepted -> {
                    runtime.gnosis.pin(recipients)
                    emit(SurfaceCommand.ClearComposer(composerAddress))
                }

                is LaneResult.Refused ->
                    _state.update { it.copy(refusal = result.message.take(240)) }

                else -> _state.update { it.copy(refusal = "The message was not sent.") }
            }
        }

    /** Accepts a changed key and sends anyway, on the person's explicit word. */
    fun trustAndSend(convoId: String, body: String, composerAddress: String) =
        viewModelScope.launch {
            val recipients = runtime.gnosis.directory(convoId)
            runtime.gnosis.pin(recipients)
            _state.update { it.copy(refusal = "") }
            sealAndSend(convoId, body, composerAddress)
        }

    // ── Publishing ────────────────────────────────────────────────────────────

    // ── Composing ─────────────────────────────────────────────────────────────

    /** The drafts this device holds, newest first. */
    val drafts: StateFlow<List<WorkDraft>> get() = runtime.drafts.drafts

    /** The pinned interest lanes a work can be tagged into; the format lanes are not tags. */
    val lanes: List<SurfaceTab>
        get() = homeTabs.filterNot { tab -> CORE_SURFACES.any { it.id == tab.id } }

    /** Lifts the composer on [draft], or on a blank one. */
    fun openComposer(draft: WorkDraft = WorkDraft.blank()) {
        overlayRender = null
        _state.update { it.copy(overlay = Overlay.Compose(draft), refusal = "") }
    }

    /** A card's Quote control: the composer opens on a work the server will render as the embed. */
    fun composeQuote(workId: String) = openComposer(WorkDraft(quoteWorkId = workId))

    /**
     * A card's Reply control. A reply is signed against the parent's content id, so
     * a card that did not state one cannot be replied to from here.
     */
    fun composeReply(workId: String, cid: String, handle: String) {
        if (cid.isEmpty()) {
            notice("This work cannot be replied to from here.")
            return
        }
        openComposer(WorkDraft(replyTo = ReplyTarget(workId, cid, handle)))
    }

    /** Keeps [draft] on this device; the composer calls it as the words change. */
    fun saveDraft(draft: WorkDraft) = runtime.drafts.save(draft)

    fun discardDraft(id: String) = runtime.drafts.remove(id)

    /**
     * Puts the composer down and opens [wire]: the track and article formats have
     * their own server-rendered pages, and the composer's draft is already kept.
     */
    fun leaveComposerFor(wire: Wire) {
        _state.update { it.copy(overlay = Overlay.None) }
        open(wire)
    }

    /** The server's render of the work [workId] as a quoted embed. */
    suspend fun quotePreview(workId: String): Fragment? =
        (runtime.client.read(Endpoints.quotedWork(workId)) as? LaneResult.Rendered)?.fragment

    /** The handles the server offers for the @-token [q]. */
    suspend fun mentionSuggestions(q: String): List<Mention> {
        if (q.isEmpty()) return emptyList()
        val array = runtime.client.readJson(Endpoints.USERS_SEARCH, mapOf("q" to q)) as? JsonArray
            ?: return emptyList()
        return array.mapNotNull { element ->
            val obj = element as? JsonObject ?: return@mapNotNull null
            val handle = obj["handle"]?.jsonPrimitive?.content.orEmpty()
            if (handle.isEmpty()) null else Mention(
                handle = handle,
                displayName = obj["display_name"]?.jsonPrimitive?.content.orEmpty(),
                avatarUrl = obj["avatar_url"]?.jsonPrimitive?.content.orEmpty(),
            )
        }
    }

    /**
     * Stores a photo on the media lane and returns where the server put it, or the
     * server's refusal. The file is the picker's copy and is gone once sent.
     */
    suspend fun uploadImage(picked: PickedFile): UploadState {
        val answer = runtime.client.uploadJson(
            Endpoints.POST_MEDIA, emptyMap(), picked.file, "media", picked.name, picked.mimeType.ifEmpty { "image/jpeg" },
        )
        picked.file.delete()
        return when (answer) {
            is JsonAnswer.Ok -> {
                val url = (answer.body as? JsonObject)?.get("url")?.jsonPrimitive?.content.orEmpty()
                if (url.isEmpty()) UploadState.Refused("The server stored nothing for that photo.") else UploadState.Stored(url)
            }
            JsonAnswer.Unauthorized -> UploadState.Refused("Sign in again to attach a photo.")
            is JsonAnswer.Refused -> UploadState.Refused(answer.message)
            is JsonAnswer.Unreachable -> UploadState.Refused("The photo could not be uploaded.")
        }
    }

    /** A clip goes up resumably and through the transcoder; [onState] reports where that stands. */
    suspend fun uploadVideo(picked: PickedFile, onState: (UploadState) -> Unit): UploadState {
        val state = runtime.uploader.upload(picked.file, picked.name, onState)
        picked.file.delete()
        return state
    }

    private fun composing(transform: (Overlay.Compose) -> Overlay.Compose) =
        _state.update { s -> (s.overlay as? Overlay.Compose)?.let { s.copy(overlay = transform(it)) } ?: s }

    /**
     * Signs and publishes what the composer holds: one work, or a thread of them.
     *
     * Each body is hashed into a content identifier and signed by the key held in
     * this device's secure hardware, so the server can verify the author independently
     * of the session. The server answers with a receipt, not a render; the timeline
     * shows the work when it is next read. A thread is a chain of receipts, each post
     * citing the content id of the one before it.
     */
    fun publish(draft: WorkDraft) = viewModelScope.launch {
        val identity = _state.value.identity
        if (!identity.isSignedIn) return@launch
        val bodies = draft.bodies
        if (bodies.isEmpty() && draft.images.isEmpty() && draft.video == null) return@launch

        composing { it.copy(posting = true, refusal = "") }

        val segments = if (bodies.isEmpty()) listOf("") else bodies
        var parentCid: String? = draft.replyTo?.cid ?: draft.continuesCid.ifEmpty { null }
        var posted = 0
        var failure: Publication? = null

        for ((index, body) in segments.withIndex()) {
            val first = index == 0 && draft.continuesCid.isEmpty()
            val kind = when {
                first && draft.replyTo != null -> "reply"
                first && draft.quoteWorkId.isNotEmpty() -> "quote"
                first && draft.video != null -> "video"
                else -> "post"
            }
            val gated = draft.subscriberOnly && !(draft.previewFirst && index == 0 && draft.isThread)
            val payload = WorkPayload(
                authorPial = identity.pial,
                body = body,
                commentGating = draft.replyRule,
                kind = kind,
                mediaUrls = if (first) draft.images else emptyList(),
                parentCid = parentCid,
                scheduledAt = draft.scheduledAt.ifEmpty { null },
                subscriberOnly = gated,
                tags = listOfNotNull(draft.lane.takeIf { it.isNotEmpty() }),
                videoDurationSecs = if (first) draft.video?.durationSecs?.takeIf { it > 0 } else null,
                videoMasterUrl = if (first) draft.video?.masterUrl else null,
                videoPosterUrl = if (first) draft.video?.posterUrl?.takeIf { it.isNotEmpty() } else null,
            )
            val attachments = WorkAttachments(
                isNsfw = draft.nsfw,
                videoWatermarkedUrl = if (first) draft.video?.watermarkedUrl?.takeIf { it.isNotEmpty() } else null,
                videoWidth = if (first) draft.video?.width ?: 0 else 0,
                videoHeight = if (first) draft.video?.height ?: 0 else 0,
                quotedWorkId = if (first) draft.quoteWorkId.takeIf { it.isNotEmpty() } else null,
                tusUploadId = if (first) draft.video?.tusUploadId?.takeIf { it.isNotEmpty() } else null,
            )
            val eventType = if (segments.size > 1) "thread_post" else "work.created"
            when (val receipt = runtime.malkuth.publish(eventType, payload, attachments)) {
                is Publication.Stored -> {
                    posted++
                    parentCid = receipt.cid
                }
                else -> {
                    failure = receipt
                    break
                }
            }
            if (failure != null) break
        }

        if (failure == null) {
            runtime.drafts.remove(draft.id)
            _state.update { it.copy(overlay = Overlay.None) }
            notice(
                when {
                    draft.scheduledAt.isNotEmpty() -> "Scheduled."
                    segments.size > 1 -> "Thread posted."
                    draft.replyTo != null -> "Reply posted."
                    else -> "Posted."
                },
            )
            loadContent()
            return@launch
        }

        val words = when (val f = failure) {
            is Publication.Refused -> f.message
            Publication.Unauthorized -> "Sign in again to post."
            else -> "The server could not be reached. Your draft is kept."
        }
        if (posted > 0) {
            // Part of the thread is on the server. What remains is kept as a draft that
            // continues the chain, so nothing already posted is posted twice.
            val remaining = segments.drop(posted)
            val continued = draft.copy(
                body = remaining.firstOrNull().orEmpty(),
                segments = remaining.drop(1),
                images = emptyList(),
                video = null,
                quoteWorkId = "",
                replyTo = null,
                continuesCid = parentCid.orEmpty(),
            )
            runtime.drafts.save(continued)
            composing { it.copy(draft = continued, posting = false, refusal = "Posted $posted of ${segments.size}. $words") }
            loadContent()
        } else {
            composing { it.copy(posting = false, refusal = words) }
        }
    }

    // ── Intents from a projected facet ────────────────────────────────────────

    /** Sends an intent a facet declared, and places whatever the server renders back. */
    fun dispatch(intent: FacetIntent) = viewModelScope.launch {
        when (intent) {
            is FacetIntent.Navigate -> route(intent.path)
            is FacetIntent.Seal -> sealAndSend(intent.convoId, intent.body, intent.origin)
            is FacetIntent.Tip -> openTip(intent.handle)
            is FacetIntent.Notice -> notice(intent.text)
            is FacetIntent.Quote -> composeQuote(intent.workId)
            is FacetIntent.Reply -> composeReply(intent.workId, intent.cid, intent.handle)

            is FacetIntent.Lane -> {
                if (intent.method == "GET" && liftsVision(intent)) {
                    openVision(ContentSource(intent.path, intent.values))
                    return@launch
                }
                if (intent.method == "GET" && liftsVisionComposer(intent)) {
                    openVisionComposer()
                    return@launch
                }
                // A conversation row asks for its thread into the web page's thread
                // pane. Here the thread is a wire of its own.
                if (intent.method == "GET" && intent.path.startsWith("/facets/messages_thread")) {
                    open(Wire.THREAD, queryValue(intent.path.substringAfter('?', ""), "c"))
                    return@launch
                }
                // A control that asks for a page (a vision card's comment button asks
                // for /work/{id}) is asking to go there. That is the Shell's move.
                if (intent.method == "GET" && route(intent.path)) return@launch
                if (intent.method == "POST" && intent.path == Endpoints.PROFILE_SAVE) {
                    // The profile editor saved. The website lands on the profile page;
                    // here the persona is read again, so the theme the server now
                    // holds reaches the frame, and the section is drawn afresh.
                    when (val result = laneCall(intent)) {
                        is LaneResult.Refused -> _state.update { it.copy(refusal = result.message.take(240)) }
                        is LaneResult.Unauthorized -> signOut()
                        is LaneResult.Unreachable -> notice("Could not reach the server.")
                        else -> {
                            refreshIdentity().join()
                            notice("Saved")
                            loadContent()
                        }
                    }
                    return@launch
                }
                when (val result = laneCall(intent)) {
                    is LaneResult.Rendered -> {
                        if (answersThreadPane(intent)) {
                            showThread(result.fragment)
                        } else {
                            emit(placement(intent, result.fragment))
                            if (intent.method != "GET" && intent.swap.startsWith("beforeend") && intent.origin.isNotEmpty()) {
                                // The web form resets itself after a successful send; the
                                // composer here is emptied the same way, and only then.
                                emit(SurfaceCommand.ClearComposer(intent.origin))
                            }
                        }
                    }

                    LaneResult.Accepted ->
                        // A decision that renders nothing back (accept or decline a contact
                        // request) changes the lists the page is made of; ask for them again.
                        if (intent.method != "GET") loadContent()

                    is LaneResult.Refused ->
                        _state.update { it.copy(refusal = result.message.take(240)) }

                    is LaneResult.Unauthorized -> signOut()
                    else -> Unit
                }
            }
        }
    }

    /** A control aimed at the web page's thread pane: its answer is a thread, or the gate before one. */
    private fun answersThreadPane(intent: FacetIntent.Lane): Boolean =
        intent.target.removePrefix("#") == "gnosis-thread" ||
            intent.path == Endpoints.MESSAGES_NEW ||
            intent.path == Endpoints.MESSAGES_NEW_NUMBER ||
            (intent.path == Endpoints.MESSAGES_SEND && intent.values.containsKey("to"))

    /**
     * Shows a thread the server just rendered — an open conversation, the prospect of
     * one, or the gate that stands before it — as the thread wire. The conversation
     * id, when the render carries one, is what a return to this wire re-reads by.
     */
    fun showThread(fragment: Fragment) {
        val convo = Regex("""data-convo-id="([^"]*)"""").find(fragment.html)?.groupValues?.get(1).orEmpty()
        val head = threadHeadOf(fragment.html)
        _state.update {
            it.copy(
                wire = Wire.THREAD,
                subject = convo,
                tabs = emptyList(),
                selectedTab = "",
                loading = false,
                threadTitle = head.first,
                threadSubtitle = head.second,
            )
        }
        lastRender[Wire.THREAD] = fragment
        emit(SurfaceCommand.Mount(fragment))
        emit(SurfaceCommand.OpenSealed)
    }

    /**
     * What the server's thread head says, for the Shell's title bar: the peer's name,
     * then their handle and the seal badge the server chose to show. The head is the
     * server's render; the bar repeats its words rather than deciding any of its own.
     */
    private fun threadHeadOf(html: String): Pair<String, String> {
        val head = Regex("""<header class="gn-thread-head"[\s\S]*?</header>""").find(html)?.value ?: return "" to ""
        fun span(cls: String) = Regex("""class="$cls"[^>]*>([^<]*)<""").find(head)?.groupValues?.get(1)?.trim().orEmpty()
        val name = span("gn-thread-name")
        val handle = span("gn-thread-handle")
        val seal = span("gn-lock-badge").ifEmpty { span("gn-plain-badge") }
            .replace("🔒", "").trim()
        val subtitle = listOf(handle, seal).filter { it.isNotEmpty() }.joinToString(" · ")
        return name to subtitle
    }

    /** Narrows the conversation list to [query], on the server. */
    fun searchConversations(query: String) {
        if (_state.value.wire != Wire.CHAT) return
        _state.update { it.copy(subject = query.trim()) }
        loadContent()
    }

    /** Starts, or returns to, a conversation with [handle]. The server decides which. */
    fun messageHandle(handle: String) = viewModelScope.launch {
        val clean = handle.trim().trimStart('@')
        if (clean.isEmpty()) return@launch
        _state.update { it.copy(loading = true) }
        when (val result = runtime.client.submit(Endpoints.MESSAGES_NEW, mapOf("handle" to clean))) {
            is LaneResult.Rendered -> showThread(result.fragment)
            is LaneResult.Refused -> {
                _state.update { it.copy(loading = false) }
                notice(result.message.take(160))
            }
            is LaneResult.Unauthorized -> signOut()
            else -> {
                _state.update { it.copy(loading = false) }
                notice("Could not reach the server.")
            }
        }
    }

    /**
     * Reaches somebody by their F33D3R Number — no handle, no phone number, no address
     * book. The server resolves it, applies the Number's own policy, and answers with
     * the conversation, a waiting request, or a gate that says nothing more than that.
     */
    fun contactByNumber(number: String, capability: String, note: String) = viewModelScope.launch {
        if (number.isBlank()) return@launch
        _state.update { it.copy(loading = true) }
        val fields = mutableMapOf("number" to number.trim())
        if (capability.isNotBlank()) fields["capability"] = capability.trim()
        if (note.isNotBlank()) fields["note"] = note.trim()
        when (val result = runtime.client.submit(Endpoints.MESSAGES_NEW_NUMBER, fields)) {
            is LaneResult.Rendered -> showThread(result.fragment)
            is LaneResult.Refused -> {
                _state.update { it.copy(loading = false) }
                notice(result.message.take(160))
            }
            is LaneResult.Unauthorized -> signOut()
            else -> {
                _state.update { it.copy(loading = false) }
                notice("Could not reach the server.")
            }
        }
    }

    /**
     * An intent raised inside the lifted surface. The same vocabulary, answered onto
     * the lifted plane — except a link out of it, which puts the surface down first.
     */
    fun dispatchOverlay(intent: FacetIntent) = viewModelScope.launch {
        when (intent) {
            is FacetIntent.Tip -> openTip(intent.handle)
            is FacetIntent.Notice -> notice(intent.text)
            is FacetIntent.Quote -> composeQuote(intent.workId)
            is FacetIntent.Reply -> composeReply(intent.workId, intent.cid, intent.handle)
            is FacetIntent.Seal -> Unit

            is FacetIntent.Navigate -> {
                val overlay = _state.value.overlay
                if (overlay is Overlay.Vision && intent.path.startsWith("/visions")) {
                    // The frame's own close control lands on the Visions page in the
                    // browser; here closing the frame is the whole of what it asks.
                    closeOverlay()
                    return@launch
                }
                closeOverlay()
                route(intent.path)
            }

            is FacetIntent.Lane -> {
                if (intent.method == "GET" && liftsVision(intent)) {
                    openVision(ContentSource(intent.path, intent.values))
                    return@launch
                }
                if (intent.method == "GET" && route(intent.path)) {
                    closeOverlay()
                    return@launch
                }
                when (val result = laneCall(intent)) {
                    is LaneResult.Rendered -> emitOverlay(placement(intent, result.fragment))
                    is LaneResult.Refused ->
                        _state.update { it.copy(refusal = result.message.take(240)) }

                    is LaneResult.Unauthorized -> signOut()
                    else -> Unit
                }
            }
        }
    }

    private suspend fun laneCall(intent: FacetIntent.Lane): LaneResult =
        if (intent.method == "GET") {
            runtime.client.read(ContentSource(intent.path, intent.values))
        } else {
            runtime.client.submit(intent.path, intent.values)
        }

    /**
     * Where the server's answer to a control goes. A control that asked to append
     * into a named container (a chat line into its log) is appended there; every
     * other answer replaces the facet it addresses itself as.
     */
    private fun placement(intent: FacetIntent.Lane, fragment: Fragment): SurfaceCommand {
        val container = intent.target.removePrefix("#")
        return when {
            intent.swap.startsWith("beforeend") && container.isNotEmpty() ->
                SurfaceCommand.Append(container, fragment)
            // A target that is a slot (an element id) rather than a Facet address takes
            // the render as its contents; a Facet address is replaced by its own render.
            intent.swap.startsWith("innerHTML") && container.isNotEmpty() &&
                !container.contains(':') && container != fragment.address ->
                SurfaceCommand.Place(container, fragment)
            else -> SurfaceCommand.Apply(fragment)
        }
    }

    private fun liftsVision(intent: FacetIntent.Lane): Boolean =
        intent.path.startsWith("/facets/vision/viewer") || intent.target == "#vision-viewer-slot"

    private fun liftsVisionComposer(intent: FacetIntent.Lane): Boolean =
        intent.path.startsWith("/facets/vision/composer") || intent.target == "#vision-composer-slot"

    /**
     * Turns an internal link into a wire.
     *
     * The paths are the server's routes, so the mapping reads them rather than
     * inventing a parallel scheme. It covers every page the website's navigation
     * reaches and every clean URL the server dispatches (`/{handle}`,
     * `/{handle}/followers`, `/{handle}/work/{id}`). A path with no wire opens
     * nothing and says so — better than opening the wrong thing.
     *
     * @return whether the path named a wire.
     */
    fun route(path: String): Boolean {
        val clean = path.substringBefore('?').substringBefore('#').trim('/')
        val query = path.substringAfter('?', "").substringBefore('#')
        val parts = clean.split('/')
        val first = parts[0]
        val second = parts.getOrElse(1) { "" }
        when {
            clean.isEmpty() -> open(Wire.PLAYGROUND)
            first == "u" && parts.size == 2 -> open(Wire.PROFILE, second)
            first == "u" && parts.size == 3 && parts[2] == "followers" -> open(Wire.FOLLOWERS, second)
            first == "u" && parts.size == 3 && parts[2] == "following" -> open(Wire.FOLLOWING, second)
            first == "profile" && parts.size == 1 -> open(Wire.PROFILE)
            first == "work" && parts.size == 2 -> open(Wire.WORK, second)
            first == "post" && parts.size == 2 -> open(Wire.WORK, second)
            first == "messages" && parts.size == 1 -> {
                val convo = queryValue(query, "c")
                if (convo.isEmpty()) open(Wire.CHAT) else open(Wire.THREAD, convo)
            }
            first == "notifications" && parts.size == 1 -> open(Wire.NOTIFICATIONS)
            first == "explore" && parts.size == 1 -> open(Wire.EXPLORE)
            first == "search" && parts.size == 1 ->
                open(Wire.SEARCH, queryValue(query, "q"), queryValue(query, "type"))
            first == "visions" && parts.size == 1 -> open(Wire.VISIONS)
            first == "visions" && second == "camera" -> openVisionComposer()
            first == "bookmarks" && parts.size == 1 -> open(Wire.BOOKMARKS)
            first == "music" && parts.size == 1 -> open(Wire.MUSIC, "", queryValue(query, "genre"))
            first == "golive" && parts.size == 1 -> goLive()
            first == "live" && parts.size == 1 -> open(Wire.LIVE)
            first == "live" && parts.size == 2 -> watchLive(second)
            first == "live" && parts.size == 3 && parts[2] == "broadcast" -> goLive()
            first == "create" && parts.size <= 2 -> open(Wire.CREATOR, second)
            first == "articles" && parts.size == 1 -> open(Wire.ARTICLES)
            first == "article" && parts.size == 2 -> open(Wire.ARTICLE, second)
            first == "achievements" && parts.size == 1 -> open(Wire.LEADERBOARD)
            first == "wallet" && parts.size <= 2 -> open(Wire.WALLET, second)
            first == "marketplace" && parts.size == 1 -> open(Wire.MARKETPLACE)
            first == "buyer-library" && parts.size == 1 -> open(Wire.LIBRARY)
            first == "shop" && parts.size == 2 -> open(Wire.SHOP, second)
            first == "analytics" && parts.size == 1 ->
                open(Wire.ANALYTICS, "", queryValue(query, "period"))
            first == "lists" && parts.size == 1 -> open(Wire.LISTS)
            first == "lists" && parts.size == 2 -> open(Wire.LIST, second)
            first == "settings" && parts.size <= 2 -> open(Wire.SETTINGS, second)
            first == "org" && second == "panel" -> open(Wire.ORG_PANEL)
            first == "admin" && parts.size <= 2 -> open(Wire.ADMIN, second)
            first == "sports" && parts.size == 1 -> open(Wire.SPORTS)
            first == "sports" && parts.size == 2 -> open(Wire.SPORTS, "", second)
            first == "sports" && parts.size == 3 -> open(Wire.SPORTS, "$second/${parts[2]}", second)
            first == "stocks" && parts.size == 2 -> open(Wire.STOCKS, second)
            first == "tag" && parts.size == 2 -> open(Wire.TAG, second)
            // The server's clean URLs: a bare handle is a profile, and what follows it
            // is that person's followers, following, or one of their works.
            parts.size == 1 && first !in RESERVED_ROOTS -> open(Wire.PROFILE, first)
            parts.size == 2 && second == "followers" && first !in RESERVED_ROOTS -> open(Wire.FOLLOWERS, first)
            parts.size == 2 && second == "following" && first !in RESERVED_ROOTS -> open(Wire.FOLLOWING, first)
            parts.size == 3 && second == "work" && first !in RESERVED_ROOTS -> open(Wire.WORK, parts[2])
            else -> return false
        }
        return true
    }

    private fun queryValue(query: String, key: String): String =
        query.split('&')
            .firstOrNull { it.substringBefore('=') == key }
            ?.substringAfter('=', "")
            ?.let { java.net.URLDecoder.decode(it, "UTF-8") }
            .orEmpty()

    fun clearRefusal() = _state.update { it.copy(refusal = "") }

    /** A short word in the Shell's notice slot, gone again on its own. */
    fun notice(text: String) {
        noticeJob?.cancel()
        _state.update { it.copy(notice = text) }
        noticeJob = viewModelScope.launch {
            delay(NOTICE_MILLIS)
            _state.update { if (it.notice == text) it.copy(notice = "") else it }
        }
    }

    // ── Lifted surfaces ───────────────────────────────────────────────────────

    /** The lifted surface's document is ready: hand it the render it was lifted for. */
    fun overlayReady() {
        overlayRender?.let { emitOverlay(SurfaceCommand.Mount(it)) }
        if (_state.value.overlay is Overlay.Vision) emitOverlay(SurfaceCommand.PlayVision)
    }

    /** Puts the lifted surface down and releases whatever it held open. */
    fun closeOverlay() {
        val overlay = _state.value.overlay
        overlayRender = null
        when (overlay) {
            is Overlay.Watch -> {
                heartbeatJob?.cancel()
                runtime.playback.stop()
            }

            is Overlay.Broadcast -> viewModelScope.launch { runtime.publisher.stop() }
            else -> Unit
        }
        _state.update { it.copy(overlay = Overlay.None) }
        if (overlay is Overlay.Vision || overlay is Overlay.VisionCompose) {
            // Every ring surface restates itself from the server once the viewer
            // closes: a ring that was just seen is re-rendered, not restyled.
            val s = _state.value
            if (s.wire == Wire.VISIONS || (s.wire == Wire.PLAYGROUND && s.selectedTab == "following")) {
                loadContent()
            }
        }
    }

    /**
     * The system back gesture with a surface lifted. A broadcast in progress is not
     * dismissed by a gesture — the End control is the one way out of it.
     *
     * @return whether the gesture was taken by a lifted surface.
     */
    fun backFromOverlay(): Boolean {
        return when (val overlay = _state.value.overlay) {
            Overlay.None -> false
            is Overlay.Broadcast -> {
                if (overlay.ended) closeOverlay()
                true
            }
            is Overlay.Watch -> {
                if (overlay.minimized) closeOverlay() else minimizeWatch()
                true
            }
            else -> {
                closeOverlay()
                true
            }
        }
    }

    // ── Visions ───────────────────────────────────────────────────────────────

    /**
     * Opens the sequential viewer at the frame [source] names, or steps to it when
     * the viewer is already lifted. Every step is a server round trip: the frame
     * carries its neighbours' addresses and this holds no sequence.
     */
    fun openVision(source: ContentSource) = viewModelScope.launch {
        when (val result = runtime.client.read(source)) {
            is LaneResult.Rendered -> {
                val html = result.fragment.html
                val scope = source.query["scope"]
                    ?: source.path.substringAfter("scope=", "").substringBefore('&').ifEmpty { "following" }
                overlayRender = result.fragment
                val already = _state.value.overlay is Overlay.Vision
                _state.update { it.copy(overlay = Overlay.Vision(visionAuthorOf(html), scope)) }
                if (already) {
                    emitOverlay(SurfaceCommand.Mount(result.fragment))
                    emitOverlay(SurfaceCommand.PlayVision)
                }
            }

            is LaneResult.Refused -> _state.update { it.copy(refusal = result.message.take(240)) }
            is LaneResult.Unauthorized -> signOut()
            else -> Unit
        }
    }

    /**
     * A swipe across the frame. Left and right press the frame's own next and previous
     * controls, so a swipe takes exactly the path a tap takes; down puts the frame away.
     */
    fun visionSwipe(swipe: Swipe) {
        when (swipe) {
            Swipe.LEFT -> emitOverlay(SurfaceCommand.Step("[data-vision-next]"))
            Swipe.RIGHT -> emitOverlay(SurfaceCommand.Step("[data-vision-prev]"))
            Swipe.DOWN -> closeOverlay()
        }
    }

    /** Shows or hides the chat over a live picture. The document's stylesheet does the rest. */
    fun setChatShown(shown: Boolean) =
        emitOverlay(SurfaceCommand.Attr("data-chat", if (shown) "on" else "off"))

    /** A private word back to a Vision's author: it lands in their messages, not on the Vision. */
    fun replyPrivately(handle: String, body: String) = viewModelScope.launch {
        if (body.isBlank() || handle.isEmpty()) return@launch
        when (val result = runtime.client.submit(Endpoints.MESSAGES_SEND, mapOf("to" to handle, "body" to body.trim()))) {
            is LaneResult.Rendered, LaneResult.Accepted -> notice("Sent to @$handle")
            is LaneResult.Refused -> notice(result.message.take(120))
            is LaneResult.Unauthorized -> signOut()
            else -> notice("The reply was not sent.")
        }
    }

    /** Mutes an author from the frame; the server decides what that does to every feed. */
    fun muteAuthor(handle: String) = viewModelScope.launch {
        when (val result = runtime.client.submit(Endpoints.EVENTS, mapOf("event_type" to "mute", "target_handle" to handle))) {
            is LaneResult.Rendered, LaneResult.Accepted -> {
                notice("Muted @$handle")
                closeOverlay()
            }
            is LaneResult.Refused -> notice(result.message.take(120))
            is LaneResult.Unauthorized -> signOut()
            else -> notice("Could not mute @$handle.")
        }
    }

    /**
     * Lifts the Vision composer. What this account may post is read off the server's
     * own composer render — the 18+ control is offered only because the server drew it.
     */
    fun openVisionComposer() = viewModelScope.launch {
        val rendered = runtime.client.read(Endpoints.visionComposer("media")) as? LaneResult.Rendered
        val canNsfw = rendered?.fragment?.html?.contains("name=\"nsfw\"") == true
        overlayRender = null
        _state.update { it.copy(overlay = Overlay.VisionCompose(canNsfw)) }
    }

    /** The server's render of the text Vision as it would be posted, for the composer's preview. */
    suspend fun previewArtboard(fields: Map<String, String>): Fragment? =
        (runtime.client.submit(Endpoints.VISION_ARTBOARD, fields) as? LaneResult.Rendered)?.fragment

    fun postVisionText(body: String, background: String, typeface: String, scale: String, align: String, nsfw: Boolean) =
        viewModelScope.launch {
            val fields = mutableMapOf(
                "content_type" to "text",
                "source" to "composer",
                "body" to body.trim(),
                "artboard_background" to background,
                "artboard_typeface" to typeface,
                "artboard_type_scale" to scale,
                "artboard_align" to align,
            )
            if (nsfw) fields["nsfw"] = "1"
            visionPosted(runtime.client.submit(Endpoints.VISIONS, fields))
        }

    fun postVisionPhoto(file: File, caption: String, nsfw: Boolean, width: Int, height: Int) =
        viewModelScope.launch {
            val fields = mutableMapOf(
                "content_type" to "image",
                "source" to "camera",
                "body" to caption.trim(),
            )
            if (width > 0) fields["width"] = width.toString()
            if (height > 0) fields["height"] = height.toString()
            if (nsfw) fields["nsfw"] = "1"
            _state.update { it.copy(loading = true) }
            val result = runtime.client.submitMultipart(Endpoints.VISIONS, fields, file, "media", "vision.jpg", "image/jpeg")
            file.delete()
            visionPosted(result)
        }

    /**
     * A clip goes up resumably and through the transcoder first; only the master the
     * transcoder names can be a Vision. [onUpload] reports where that stands.
     */
    fun postVisionClip(file: File, caption: String, nsfw: Boolean, onUpload: (UploadState) -> Unit) =
        viewModelScope.launch {
            _state.update { it.copy(loading = true) }
            when (val up = runtime.uploader.upload(file, "vision.mp4", onUpload)) {
                is UploadState.Transcoded -> {
                    val fields = mutableMapOf(
                        "content_type" to "video",
                        "source" to "camera",
                        "body" to caption.trim(),
                        "media_url" to up.masterUrl,
                    )
                    if (up.width > 0) fields["width"] = up.width.toString()
                    if (up.height > 0) fields["height"] = up.height.toString()
                    if (up.durationSecs > 0) fields["duration_secs"] = up.durationSecs.toString()
                    if (nsfw) fields["nsfw"] = "1"
                    file.delete()
                    visionPosted(runtime.client.submit(Endpoints.VISIONS, fields))
                }

                is UploadState.Duplicate -> {
                    file.delete()
                    _state.update { it.copy(loading = false) }
                    onUpload(up)
                    notice(
                        if (up.creatorHandle.isNotEmpty()) "This clip is already on F33D3R, by @${up.creatorHandle}."
                        else "This clip is already on F33D3R."
                    )
                }

                is UploadState.Refused -> {
                    _state.update { it.copy(loading = false) }
                    onUpload(up)
                    notice(up.message)
                }

                else -> _state.update { it.copy(loading = false) }
            }
        }

    private fun visionPosted(result: LaneResult) {
        _state.update { it.copy(loading = false) }
        when (result) {
            is LaneResult.Rendered -> {
                closeOverlay()
                notice("Vision posted. Gone in 24 hours.")
            }

            is LaneResult.Refused -> notice(
                errorTextOf(result.body, "vision-compose__error").ifEmpty { result.message.take(160) }
            )

            is LaneResult.Unauthorized -> signOut()
            LaneResult.Accepted -> {
                closeOverlay()
                notice("Vision posted.")
            }

            else -> notice("The Vision could not be posted.")
        }
    }

    // ── Live: broadcasting ────────────────────────────────────────────────────

    /**
     * Lifts the go-live sheet — or, when this account already holds an open stream,
     * its broadcast surface: the server answers /golive with whichever is true.
     */
    fun goLive() = viewModelScope.launch {
        when (val result = runtime.client.read(Endpoints.goLive)) {
            is LaneResult.Rendered -> {
                val html = result.fragment.html
                if (html.contains("live-bcast") && streamIdOf(html).isNotEmpty()) {
                    enterBroadcast(result.fragment, frontCamera = html.contains("data-facing=\"user\""))
                } else {
                    overlayRender = null
                    _state.update { it.copy(overlay = Overlay.GoLive(canNsfw = html.contains("name=\"nsfw\""))) }
                }
            }

            is LaneResult.Refused -> notice(result.message.take(160))
            is LaneResult.Unauthorized -> signOut()
            else -> notice("Could not reach the server.")
        }
    }

    /**
     * Opens the stream. The server creates it idle and answers with the broadcast
     * surface; it turns LIVE only when the media server reports frames arriving.
     */
    fun startBroadcast(title: String, description: String, frontCamera: Boolean, nsfw: Boolean, encoder: Boolean) =
        viewModelScope.launch {
            val current = _state.value.overlay as? Overlay.GoLive ?: return@launch
            _state.update { it.copy(loading = true) }
            val fields = mutableMapOf(
                "title" to title.trim(),
                "description" to description.trim(),
                "source" to if (encoder) "encoder" else "browser",
                "facing" to if (frontCamera) "user" else "environment",
            )
            if (nsfw) fields["nsfw"] = "1"
            when (val result = runtime.client.submit(Endpoints.LIVE_START, fields)) {
                is LaneResult.Rendered -> {
                    _state.update { it.copy(loading = false) }
                    val html = result.fragment.html
                    val rejection = errorTextOf(html, "live-go__error")
                    if (rejection.isNotEmpty() || streamIdOf(html).isEmpty()) {
                        _state.update { it.copy(overlay = current.copy(error = rejection.ifEmpty { "The broadcast could not be opened." })) }
                    } else {
                        enterBroadcast(result.fragment, frontCamera)
                    }
                }

                is LaneResult.Refused -> {
                    _state.update { it.copy(loading = false) }
                    val rejection = errorTextOf(result.body, "live-go__error")
                    _state.update { it.copy(overlay = current.copy(error = rejection.ifEmpty { result.message.take(200) })) }
                }

                is LaneResult.Unauthorized -> signOut()
                else -> {
                    _state.update { it.copy(loading = false, overlay = current.copy(error = "Could not reach the server.")) }
                }
            }
        }

    private fun enterBroadcast(fragment: Fragment, frontCamera: Boolean) {
        val html = fragment.html
        val id = streamIdOf(html)
        val already = _state.value.overlay is Overlay.Broadcast
        overlayRender = fragment
        _state.update {
            it.copy(
                overlay = Overlay.Broadcast(
                    streamId = id,
                    frontCamera = frontCamera,
                    source = if (html.contains("data-source=\"encoder\"")) "encoder" else "browser",
                    ended = html.contains("live-bcast--ended"),
                ),
            )
        }
        if (already) emitOverlay(SurfaceCommand.Mount(fragment))
    }

    /** Starts transmitting from this phone. Called by the broadcast surface once it holds the camera. */
    fun transmit() = viewModelScope.launch {
        val overlay = _state.value.overlay as? Overlay.Broadcast ?: return@launch
        if (overlay.ended || overlay.source != "browser") return@launch
        runtime.publisher.start(overlay.streamId, overlay.frontCamera)
    }

    /** Ends the broadcast for everyone. The server's re-rendered stage is the answer. */
    fun endBroadcast() = viewModelScope.launch {
        val overlay = _state.value.overlay as? Overlay.Broadcast ?: return@launch
        runtime.publisher.stop()
        when (val result = runtime.client.submit(Endpoints.liveEnd(overlay.streamId), emptyMap())) {
            is LaneResult.Rendered -> {
                overlayRender = result.fragment
                emitOverlay(SurfaceCommand.Apply(result.fragment))
                _state.update { it.copy(overlay = overlay.copy(ended = true)) }
            }

            is LaneResult.Refused -> notice(result.message.take(160))
            is LaneResult.Unauthorized -> signOut()
            else -> notice("The broadcast could not be ended.")
        }
    }

    // ── Live: watching ────────────────────────────────────────────────────────

    /**
     * Lifts the watch surface for [streamId]: the server's page around the picture,
     * the picture decoded natively, and the presence beat the page asks for.
     */
    fun watchLive(streamId: String) = viewModelScope.launch {
        when (val result = runtime.client.read(Endpoints.liveWatch(streamId))) {
            is LaneResult.Rendered -> {
                val html = result.fragment.html
                heartbeatJob?.cancel()
                overlayRender = result.fragment
                viewerToken = viewerTokenOf(html)
                val already = _state.value.overlay is Overlay.Watch
                _state.update {
                    it.copy(
                        overlay = Overlay.Watch(
                            streamId = streamId,
                            authorHandle = liveAuthorOf(html),
                            title = liveTitleOf(html),
                        ),
                    )
                }
                if (already) emitOverlay(SurfaceCommand.Mount(result.fragment))
                runtime.playback.play(runtime.session.origin.trimEnd('/') + Endpoints.liveMaster(streamId))
                heartbeatJob = viewModelScope.launch { beat(streamId) }
            }

            is LaneResult.Refused -> notice(result.message.take(160))
            is LaneResult.Unauthorized -> signOut()
            else -> notice("Could not reach the server.")
        }
    }

    /**
     * The presence beat the watch surface carries as hx-trigger="every 20s". The server
     * answers with the re-rendered tally — or, once the stream is over, with the
     * terminal player Facet, which carries no beat; the loop stops on that word.
     */
    private suspend fun beat(streamId: String) {
        while (viewModelScope.isActive) {
            delay(HEARTBEAT_MILLIS)
            val overlay = _state.value.overlay as? Overlay.Watch ?: return
            if (overlay.streamId != streamId) return
            val fields = if (viewerToken.isEmpty()) emptyMap() else mapOf("vt" to viewerToken)
            when (val result = runtime.client.submit(Endpoints.liveHeartbeat(streamId), fields)) {
                is LaneResult.Rendered -> {
                    emitOverlay(SurfaceCommand.Apply(result.fragment))
                    if (!result.fragment.html.contains("hx-post=\"/live/$streamId/heartbeat\"")) {
                        runtime.playback.stop()
                        return
                    }
                }

                is LaneResult.Unauthorized -> {
                    signOut()
                    return
                }

                else -> Unit
            }
        }
    }

    /** Puts the picture away and keeps the sound: the bar above the rail is the stream. */
    fun minimizeWatch() {
        val overlay = _state.value.overlay as? Overlay.Watch ?: return
        _state.update { it.copy(overlay = overlay.copy(minimized = true)) }
    }

    fun restoreWatch() {
        val overlay = _state.value.overlay as? Overlay.Watch ?: return
        _state.update { it.copy(overlay = overlay.copy(minimized = false)) }
    }

    /** A line into the stream's chat. The server renders it; the log receives the render. */
    fun sendLiveChat(body: String) = viewModelScope.launch {
        val streamId = when (val overlay = _state.value.overlay) {
            is Overlay.Watch -> overlay.streamId
            is Overlay.Broadcast -> overlay.streamId
            else -> return@launch
        }
        if (body.isBlank()) return@launch
        when (val result = runtime.client.submit(Endpoints.liveChat(streamId), mapOf("body" to body.trim()))) {
            is LaneResult.Rendered -> emitOverlay(SurfaceCommand.Append(FaLive.LIVE_CHAT, result.fragment))
            is LaneResult.Refused -> notice(result.message.take(160))
            is LaneResult.Unauthorized -> signOut()
            else -> Unit
        }
    }

    // ── Tips ──────────────────────────────────────────────────────────────────

    fun openTip(handle: String) = _state.update { it.copy(tipHandle = handle) }

    fun closeTip() = _state.update { it.copy(tipHandle = "") }

    /**
     * Sends AET to [handle]. The amount travels in hundredths, the unit the ledger
     * lane already takes from the web shell. The answer is the ledger's.
     */
    fun sendTip(handle: String, hundredths: Long) = viewModelScope.launch {
        if (hundredths <= 0) return@launch
        val fields = mapOf(
            "event_type" to "tip",
            "target_handle" to handle,
            "amount_aet" to hundredths.toString(),
        )
        when (val result = runtime.client.submit(Endpoints.EVENTS, fields)) {
            is LaneResult.Rendered, LaneResult.Accepted -> {
                closeTip()
                notice("Tip sent to @$handle")
            }

            is LaneResult.Refused -> notice(jsonErrorOf(result.message).ifEmpty { "The tip was not sent." })
            is LaneResult.Unauthorized -> signOut()
            else -> notice("Could not reach the server.")
        }
    }

    // ── Plumbing ──────────────────────────────────────────────────────────────

    private fun emit(command: SurfaceCommand) {
        viewModelScope.launch { _commands.emit(command) }
    }

    private fun emitOverlay(command: SurfaceCommand) {
        viewModelScope.launch { _overlayCommands.emit(command) }
    }

    private fun emitBoth(command: SurfaceCommand) {
        emit(command)
        emitOverlay(command)
    }

    companion object {
        /** The home lanes that are pages of their own rather than works surfaces. */
        const val LANE_MUSIC = "music"
        const val LANE_VISIONS = "visions"
        const val LANE_LIVE = "live"
        private val PAGE_LANES = setOf(LANE_MUSIC, LANE_VISIONS, LANE_LIVE)

        private const val NOTICE_MILLIS = 3_200L
        private const val HEARTBEAT_MILLIS = 20_000L

        /**
         * The home lanes, in the order the tab row shows them: the people you follow,
         * the ranked feed, then the three format lanes. Explore keeps Trending; the
         * timeline does not carry it.
         *
         * These are the Shell's chrome, not content: the server decides what each
         * surface contains, and this list only decides what the tab is called before
         * the reader taps it.
         */
        val CORE_SURFACES = listOf(
            SurfaceTab("following", "Following"),
            SurfaceTab("feed", "For you"),
            SurfaceTab(LANE_MUSIC, "Music"),
            SurfaceTab(LANE_VISIONS, "Visions"),
            SurfaceTab(LANE_LIVE, "Live"),
        )

        val EXPLORE_TABS = listOf(
            SurfaceTab("explore", "Explore"),
            SurfaceTab("news", "News"),
            SurfaceTab("sports", "Sports"),
            SurfaceTab("market", "Market"),
        )

        /** The profile page's own tab strip, surface for surface. */
        val PROFILE_TABS = listOf(
            SurfaceTab("profile_posts", "Posts"),
            SurfaceTab("profile_replies", "Replies"),
            SurfaceTab("profile_reposts", "Reposts"),
            SurfaceTab("profile_media", "Media"),
            SurfaceTab("profile_articles", "Articles"),
        )

        val SEARCH_TABS = listOf(
            SurfaceTab("top", "Top"),
            SurfaceTab("people", "People"),
            SurfaceTab("posts", "Posts"),
        )

        val ANALYTICS_TABS = listOf(
            SurfaceTab("7", "7 days"),
            SurfaceTab("30", "30 days"),
            SurfaceTab("90", "90 days"),
            SurfaceTab("all", "All time"),
        )

        val WALLET_TABS = listOf(
            SurfaceTab("overview", "Overview"),
            SurfaceTab("history", "History"),
            SurfaceTab("send", "Send"),
            SurfaceTab("governance", "Governance"),
        )

        val CREATOR_TABS = listOf(
            SurfaceTab("overview", "Overview"),
            SurfaceTab("content", "Content"),
            SurfaceTab("audience", "Audience"),
            SurfaceTab("earnings", "Earnings"),
            SurfaceTab("memberships", "Memberships"),
            SurfaceTab("music", "Music"),
            SurfaceTab("analytics", "Analytics"),
            SurfaceTab("settings", "Settings"),
        )

        /** The settings section that is dark or light and the accent theme. */
        const val SETTINGS_DISPLAY = "display"

        val SETTINGS_TABS = listOf(
            SurfaceTab("account", "Account"),
            SurfaceTab("security", "Security"),
            SurfaceTab("privacy", "Privacy"),
            SurfaceTab("notifications", "Notifications"),
            SurfaceTab(SETTINGS_DISPLAY, "Display"),
            SurfaceTab("contact", "Contact"),
            SurfaceTab("verification", "Verification"),
            SurfaceTab("realm", "Realm"),
            SurfaceTab("org", "Organisation"),
            SurfaceTab("resources", "Resources"),
            SurfaceTab("danger", "Danger zone"),
        )

        val ADMIN_TABS = listOf(
            SurfaceTab("overview", "Overview"),
            SurfaceTab("users", "Users"),
            SurfaceTab("moderation", "Moderation"),
            SurfaceTab("dmca", "DMCA"),
            SurfaceTab("org-verifications", "Orgs"),
            SurfaceTab("security", "Security"),
            SurfaceTab("treasury", "Treasury"),
            SurfaceTab("analytics", "Analytics"),
            SurfaceTab("celebrations", "Celebrations"),
            SurfaceTab("abraxas", "Abraxas"),
            SurfaceTab("devtools", "Dev tools"),
        )

        /** The genres the website's music browse lists, in its order. */
        val MUSIC_TABS = listOf(
            SurfaceTab("all", "All"),
            SurfaceTab("Hip-Hop", "Hip-Hop"),
            SurfaceTab("R&B", "R&B"),
            SurfaceTab("Electronic", "Electronic"),
            SurfaceTab("Pop", "Pop"),
            SurfaceTab("Rock", "Rock"),
            SurfaceTab("Jazz", "Jazz"),
            SurfaceTab("Afrobeats", "Afrobeats"),
            SurfaceTab("Ambient", "Ambient"),
        )

        /** The leagues the server polls. */
        val SPORTS_TABS = listOf(
            SurfaceTab("all", "All"),
            SurfaceTab("nfl", "NFL"),
            SurfaceTab("nba", "NBA"),
        )

        /** Wires whose subject, when a link carries one, names the section to open on. */
        val SECTIONED = setOf(Wire.SETTINGS, Wire.WALLET, Wire.CREATOR, Wire.ADMIN)

        /**
         * Wires that are menus first: many sections, each a page of its own. They open
         * on a vertical list of those sections, the way every phone settings screen
         * does, rather than on a tab strip that scrolls off the side of the screen.
         */
        val MENU_WIRES = setOf(Wire.SETTINGS, Wire.CREATOR, Wire.ADMIN)

        /**
         * First path segments the server owns outright, so a link to one is never
         * read as a handle. Mirrors the routes the server registers ahead of its
         * clean-URL profile dispatch.
         */
        val RESERVED_ROOTS = setOf(
            "api", "facets", "partials", "static", "media", "upload", "events", "login",
            "logout", "onboard", "forgot-password", "backup-codes", "kyc", "legal", "terms",
            "privacy", "dmca", "deactivated", "healthz", "metrics", "sw.js", "feed", "works",
            "react-video", "communities", "spheres", "spaces", "contact", "identity",
            "visions", "nsfw", "video", "library", "ledger", "verity", "ainsoph", "thessalon",
            "themis", "_atlas",
        )

        /**
         * Reads the pinned surfaces out of the tab row the server rendered.
         *
         * The row arrives as buttons carrying `data-surface` and their label; the native
         * tab row draws the same set. Reading them from the server's render is what
         * keeps the two in agreement — a hardcoded list here would drift the first time
         * someone pins a surface.
         */
        fun parseSurfaceTabs(html: String): List<SurfaceTab> =
            Regex("""data-surface="([^"]+)"[^>]*>([^<]*)<""")
                .findAll(html)
                .mapNotNull { match ->
                    val id = match.groupValues[1].trim()
                    val label = match.groupValues[2].trim()
                    if (id.isEmpty() || label.isEmpty()) null else SurfaceTab(id, label)
                }
                .toList()

        // The few names the native frame reads off a render — an address to beat, a
        // handle to title a bar with. Each is the server's own attribute or text,
        // taken as written; nothing is computed from them.

        fun streamIdOf(html: String): String =
            Regex("""data-stream-id="([^"]+)"""").find(html)?.groupValues?.get(1).orEmpty()

        fun viewerTokenOf(html: String): String =
            Regex(""""vt"\s*:\s*"([^"]*)"""").find(html)?.groupValues?.get(1).orEmpty()

        fun liveAuthorOf(html: String): String =
            Regex("""live-author__handle"[^>]*>@([^<]+)<""").find(html)?.groupValues?.get(1)?.trim().orEmpty()

        fun liveTitleOf(html: String): String =
            Regex("""live-titlebar__title"[^>]*>([^<]*)<""").find(html)?.groupValues?.get(1)?.trim().orEmpty()

        fun visionAuthorOf(html: String): String =
            Regex("""vision-viewer__handle"\s+href="/([^"?#]+)"""").find(html)?.groupValues?.get(1)?.trim().orEmpty()

        /** The text of the element carrying [cssClass], as the server wrote it. */
        fun errorTextOf(html: String, cssClass: String): String =
            Regex("""class="[^"]*\b${Regex.escape(cssClass)}\b[^"]*"[^>]*>([^<]*)<""")
                .find(html)?.groupValues?.get(1)?.trim().orEmpty()

        /** The `error` field of a JSON refusal, when the lane answered with one. */
        fun jsonErrorOf(text: String): String =
            Regex(""""error"\s*:\s*"([^"]*)"""").find(text)?.groupValues?.get(1).orEmpty()
    }
}
