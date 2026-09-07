package com.f33d3r.app.ui.shell

import androidx.activity.compose.BackHandler
import androidx.compose.animation.AnimatedVisibility
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.RowScope
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.statusBarsPadding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.border
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.Close
import androidx.compose.material.icons.outlined.DarkMode
import androidx.compose.material.icons.rounded.Add
import androidx.compose.material.icons.rounded.ArrowUpward
import androidx.compose.material.icons.rounded.MapsUgc
import androidx.compose.material.icons.rounded.Videocam
import androidx.compose.material3.DrawerValue
import androidx.compose.material3.Icon
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.ModalNavigationDrawer
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.rememberDrawerState
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import com.f33d3r.app.Runtime
import com.f33d3r.app.core.Wire
import com.f33d3r.app.seal.SealState
import com.f33d3r.app.ui.Overlay
import com.f33d3r.app.ui.ShellModel
import com.f33d3r.app.ui.ShellState
import com.f33d3r.app.ui.compose.Composer
import com.f33d3r.app.ui.vision.VisionComposer
import com.f33d3r.app.ui.vision.VisionViewer
import com.f33d3r.app.ui.live.BroadcastSurface
import com.f33d3r.app.ui.live.GoLiveSheet
import com.f33d3r.app.ui.live.WatchSurface
import com.f33d3r.app.ui.surface.FacetSurface
import com.f33d3r.app.ui.theme.Ink
import kotlinx.coroutines.launch

/**
 * The Shell: the persistent frame the application lives inside.
 *
 * It is native because it is the platform's job — the status bar inset, the back
 * gesture, the drawer, the bottom rail and the compose action are things Android
 * owns and should feel like Android. Everything the frame contains is the server's
 * render, delivered as facets and placed by the surface. The line between the two is
 * the whole architecture, and it is drawn exactly here.
 *
 * Some surfaces are lifted above the frame rather than placed inside it: a Vision
 * frame, the Vision composer, going live, a broadcast, a stream being watched. They
 * are drawn last, over everything, and the back gesture puts them down.
 */
@Composable
fun Shell(model: ShellModel, runtime: Runtime) {
    val session = runtime.session
    val state by model.state.collectAsStateWithLifecycle()
    val drawer = rememberDrawerState(DrawerValue.Closed)
    val scope = rememberCoroutineScope()
    var newMessage by remember { mutableStateOf(false) }

    BackHandler(enabled = state.overlay != Overlay.None) { model.backFromOverlay() }

    Box(Modifier.fillMaxSize()) {
        ModalNavigationDrawer(
            drawerState = drawer,
            // The drawer opens from the avatar. A drag anywhere across the timeline is a
            // lane swipe, which the surface owns; the drawer only takes a drag to close.
            gesturesEnabled = drawer.isOpen && state.overlay == Overlay.None,
            drawerContent = {
                PersonaDrawer(
                    identity = state.identity,
                    balance = state.balance,
                    connection = state.connection,
                    sealed = state.sealState == SealState.Unlocked,
                    density = state.density,
                    mode = state.mode,
                    onOpen = { wire, subject ->
                        scope.launch { drawer.close() }
                        model.open(wire, subject)
                    },
                    onDensity = model::toggleDensity,
                    onMode = model::toggleMode,
                    onSignOut = {
                        scope.launch { drawer.close() }
                        model.signOut()
                    },
                )
            },
        ) {
            Scaffold(
                containerColor = Ink.Surface,
                topBar = { ShellTopBar(state, model) { scope.launch { drawer.open() } } },
                bottomBar = {
                    Column {
                        if (state.connection != com.f33d3r.app.net.live.LiveState.LIVE) {
                            ConnectionNotice(state)
                        }
                        val watch = state.overlay as? Overlay.Watch
                        if (watch != null && watch.minimized) {
                            // The picture is put away; the stream is not. The bar is the stream.
                            SpacesRail(
                                title = watch.title.ifEmpty { "Live" },
                                detail = "@${watch.authorHandle} · live · tap to return",
                                onOpen = model::restoreWatch,
                                onLeave = model::closeOverlay,
                            )
                        }
                        BottomNav(
                            current = state.wire,
                            pendingWorks = state.pendingWorks,
                            notificationBadge = state.notificationBadge,
                            messageBadge = state.messageBadge,
                            onSelect = { model.open(it) },
                        )
                    }
                },
                floatingActionButton = {
                    // Composing belongs to the surfaces where something is composed:
                    // the timeline, explore, visions, the inbox, a profile. Not settings.
                    if (state.wire in COMPOSING_WIRES && state.overlay == Overlay.None) {
                        val onVisions = state.wire == Wire.VISIONS ||
                            (state.wire == Wire.PLAYGROUND && state.selectedTab == ShellModel.LANE_VISIONS)
                        val onLive = state.wire == Wire.PLAYGROUND && state.selectedTab == ShellModel.LANE_LIVE
                        ComposeAction(wire = state.wire, visions = onVisions, live = onLive) {
                            when {
                                onVisions -> model.openVisionComposer()
                                onLive -> model.goLive()
                                state.wire == Wire.CHAT || state.wire == Wire.THREAD -> newMessage = true
                                else -> model.openComposer()
                            }
                        }
                    }
                },
            ) { padding ->
                Box(
                    modifier = Modifier
                        .fillMaxSize()
                        .padding(padding)
                        .background(Ink.Surface),
                ) {
                    Column(Modifier.fillMaxSize()) {
                        if (state.wire == Wire.SETTINGS && state.selectedTab == ShellModel.SETTINGS_DISPLAY) {
                            // Dark or light is the frame's own choice, so the frame
                            // draws the switch; the accent theme below it is the
                            // server's profile editor.
                            ModeRow(mode = state.mode, onToggle = model::toggleMode)
                        }
                        FacetSurface(
                            session = session,
                            identity = state.identity,
                            density = state.density,
                            uiMode = state.mode,
                            commands = model.commands,
                            onIntent = model::dispatch,
                            onReady = { model.loadContent() },
                            onSealedBubbles = model::openSealed,
                            onPageForward = model::pageForward,
                            onSwipe = model::swipeLane,
                            modifier = Modifier.weight(1f).fillMaxWidth(),
                        )
                    }

                    if (state.wire in ShellModel.MENU_WIRES && state.selectedTab.isEmpty()) {
                        SectionMenu(wire = state.wire, sections = state.tabs, onSelect = model::selectTab)
                    }

                    if (state.loading) {
                        LinearProgressIndicator(
                            color = Ink.Accent,
                            trackColor = Ink.Hairline,
                            modifier = Modifier.fillMaxWidth().height(2.dp).align(Alignment.TopCenter),
                        )
                    }

                    if (state.emptyLane.isNotEmpty()) {
                        Text(
                            text = state.emptyLane,
                            style = MaterialTheme.typography.bodyLarge,
                            color = Ink.Tertiary,
                            modifier = Modifier.align(Alignment.Center).padding(32.dp),
                        )
                    }

                    AnimatedVisibility(
                        visible = state.pendingWorks > 0,
                        modifier = Modifier.align(Alignment.TopCenter).padding(top = 10.dp),
                    ) {
                        HeldWorksBanner(state.pendingWorks) { model.showHeldWorks() }
                    }

                    AnimatedVisibility(
                        visible = state.refusal.isNotEmpty(),
                        modifier = Modifier.align(Alignment.BottomCenter).padding(16.dp),
                    ) {
                        Refusal(state.refusal) { model.clearRefusal() }
                    }
                }
            }
        }

        // ── Lifted surfaces, drawn over the frame ──
        when (val overlay = state.overlay) {
            Overlay.None -> Unit
            is Overlay.Vision -> VisionViewer(model, session, state.identity, overlay)
            is Overlay.VisionCompose -> VisionComposer(model, session, state.identity, overlay)
            is Overlay.Compose -> Composer(model, session, state, overlay)
            is Overlay.GoLive -> GoLiveSheet(model, overlay, state.identity.handle, state.loading)
            is Overlay.Broadcast -> BroadcastSurface(model, session, state.identity, overlay, runtime.publisher)
            is Overlay.Watch -> if (!overlay.minimized) {
                WatchSurface(model, session, state.identity, overlay, runtime.playback)
            }
        }

        AnimatedVisibility(
            visible = state.notice.isNotEmpty(),
            modifier = Modifier.align(Alignment.TopCenter).statusBarsPadding().padding(top = TOP_BAR_HEIGHT + 8.dp),
        ) {
            Notice(state.notice)
        }

        if (state.tipHandle.isNotEmpty()) {
            TipSheet(
                handle = state.tipHandle,
                onDismiss = model::closeTip,
                onSend = { hundredths -> model.sendTip(state.tipHandle, hundredths) },
            )
        }
    }

    if (newMessage) {
        NewMessageSheet(
            onDismiss = { newMessage = false },
            onHandle = { handle ->
                newMessage = false
                model.messageHandle(handle)
            },
            onNumber = { number, capability, note ->
                newMessage = false
                model.contactByNumber(number, capability, note)
            },
        )
    }

}

/** The top bar each wire needs, and no more than that wire needs. */
@Composable
private fun ShellTopBar(state: ShellState, model: ShellModel, onAvatar: () -> Unit) {
    Column(Modifier.background(Ink.Surface)) {
        when (state.wire) {
            Wire.PLAYGROUND -> {
                HomeTopBar(
                    avatarUrl = state.identity.avatarUrl,
                    handle = state.identity.handle,
                    onAvatar = onAvatar,
                )
                WireTabs(
                    tabs = state.tabs,
                    selected = state.selectedTab,
                    onSelect = model::selectTab,
                    onAdd = { model.open(Wire.EXPLORE) },
                    menu = true,
                )
            }

            Wire.EXPLORE -> {
                // The explore bar is the top bar with the search pill where the title goes.
                Row(
                    modifier = Modifier
                        .fillMaxWidth()
                        .statusBarsPadding()
                        .height(TOP_BAR_HEIGHT)
                        .padding(horizontal = PAGE_PADDING),
                    verticalAlignment = Alignment.CenterVertically,
                ) {
                    PersonaAvatar(state.identity.avatarUrl, state.identity.handle, onClick = onAvatar)
                    Spacer(Modifier.width(12.dp))
                    SearchField("Search F33D3R", modifier = Modifier.weight(1f)) {
                        model.open(Wire.SEARCH)
                    }
                    Spacer(Modifier.width(4.dp))
                    // The gear is the way to settings as a whole; the menu, not one section.
                    SettingsAction { model.open(Wire.SETTINGS) }
                }
                WireTabs(state.tabs, state.selectedTab, model::selectTab)
            }

            Wire.CHAT -> {
                TopBar(
                    avatarUrl = state.identity.avatarUrl,
                    handle = state.identity.handle,
                    title = "Messages",
                    onAvatar = onAvatar,
                    // Your own Number lives where it is minted and retired: Settings → Contact.
                    trailing = { InboxFilter("Number") { model.open(Wire.SETTINGS, "contact") } },
                )
                Box(Modifier.padding(start = PAGE_PADDING, end = PAGE_PADDING, bottom = 8.dp)) {
                    // The list is narrowed on the server; the field holds only the draft.
                    QueryField(
                        initial = state.subject,
                        placeholder = "Search conversations",
                        onSubmit = model::searchConversations,
                        autofocus = false,
                    )
                }
            }

            Wire.THREAD -> {
                // Who the conversation is with, and whether it is sealed, as the
                // server's thread head states them; the head itself is not drawn
                // twice.
                TopBar(
                    avatarUrl = state.identity.avatarUrl,
                    handle = state.identity.handle,
                    title = state.threadTitle.ifEmpty { "Message" },
                    subtitle = state.threadSubtitle,
                    onAvatar = onAvatar,
                    onBack = { model.open(Wire.CHAT) },
                )
            }

            Wire.NOTIFICATIONS -> {
                TopBar(
                    avatarUrl = state.identity.avatarUrl,
                    handle = state.identity.handle,
                    title = "Notifications",
                    onAvatar = onAvatar,
                    trailing = { SettingsAction { model.open(Wire.SETTINGS, "notifications") } },
                )
            }

            Wire.SEARCH -> {
                // The search bar is the top bar with the query pill where the title goes.
                Row(
                    modifier = Modifier
                        .fillMaxWidth()
                        .statusBarsPadding()
                        .height(TOP_BAR_HEIGHT)
                        .padding(start = 4.dp, end = PAGE_PADDING),
                    verticalAlignment = Alignment.CenterVertically,
                ) {
                    BackAction { model.open(Wire.EXPLORE) }
                    Spacer(Modifier.width(4.dp))
                    QueryField(
                        initial = state.subject,
                        placeholder = "Search F33D3R",
                        modifier = Modifier.weight(1f),
                        onSubmit = model::search,
                    )
                }
                WireTabs(state.tabs, state.selectedTab, model::selectTab)
            }

            Wire.PROFILE -> {
                TopBar(
                    avatarUrl = state.identity.avatarUrl,
                    handle = state.identity.handle,
                    title = "@" + state.subject.ifEmpty { state.identity.handle },
                    onAvatar = onAvatar,
                    onBack = { model.open(Wire.PLAYGROUND) },
                )
                WireTabs(state.tabs, state.selectedTab, model::selectTab)
            }

            in ShellModel.MENU_WIRES -> {
                // A menu wire: the list of sections first, then one section at a time.
                // Back from a section is the list; back from the list is home.
                val section = state.tabs.firstOrNull { it.id == state.selectedTab }
                TopBar(
                    avatarUrl = state.identity.avatarUrl,
                    handle = state.identity.handle,
                    title = section?.label ?: state.wire.title,
                    // Inside a section the bar says which menu it belongs to; the menu
                    // itself needs no line under its name.
                    subtitle = if (section == null) "" else state.wire.title,
                    onAvatar = onAvatar,
                    onBack = { if (section == null) model.open(Wire.PLAYGROUND) else model.backToMenu() },
                )
            }

            else -> {
                // Every other wire is a website page: the page's own header is the
                // Shell's title bar, and its tab strip, when it has one, is the tab row.
                TopBar(
                    avatarUrl = state.identity.avatarUrl,
                    handle = state.identity.handle,
                    title = titleFor(state),
                    subtitle = subtitleFor(state),
                    onAvatar = onAvatar,
                    onBack = { model.open(Wire.PLAYGROUND) },
                )
                WireTabs(state.tabs, state.selectedTab, model::selectTab)
            }
        }
        Box(Modifier.fillMaxWidth().height(1.dp).background(Ink.Hairline))
    }
}

/** The wires the compose action is drawn on. */
private val COMPOSING_WIRES = setOf(
    Wire.PLAYGROUND, Wire.EXPLORE, Wire.VISIONS, Wire.CHAT, Wire.THREAD, Wire.PROFILE,
    Wire.NOTIFICATIONS, Wire.LIVE, Wire.SEARCH, Wire.TAG, Wire.WORK,
)

/** What the page is about, as the website's own header would say it. */
private fun titleFor(state: ShellState): String = when (state.wire) {
    Wire.FOLLOWERS, Wire.FOLLOWING, Wire.SHOP -> "@" + state.subject.ifEmpty { state.identity.handle }
    Wire.TAG -> "#" + state.subject
    Wire.STOCKS -> "$" + state.subject.uppercase()
    Wire.SPORTS -> state.subject.substringBefore('/').uppercase().ifEmpty { state.wire.title }
    else -> state.wire.title
}

private fun subtitleFor(state: ShellState): String = when (state.wire) {
    Wire.FOLLOWERS, Wire.FOLLOWING, Wire.SHOP -> state.wire.title
    else -> ""
}

/**
 * The compose action: a 56 accent circle with a white glyph. On the chat wire it
 * starts a conversation; on the Visions lane it opens the camera; on the Live lane
 * it goes live, and is red for it; everywhere else, a work.
 */
@Composable
private fun ComposeAction(wire: Wire, visions: Boolean, live: Boolean, onClick: () -> Unit) {
    Box(
        modifier = Modifier
            .size(56.dp)
            .clip(CircleShape)
            .background(if (live) Ink.Live else Ink.Accent)
            .clickable(onClick = onClick),
        contentAlignment = Alignment.Center,
    ) {
        Icon(
            imageVector = when {
                live -> Icons.Rounded.Videocam
                wire == Wire.CHAT -> Icons.Rounded.MapsUgc
                else -> Icons.Rounded.Add
            },
            contentDescription = when {
                live -> "Go live"
                visions -> "New Vision"
                wire == Wire.CHAT -> "New conversation"
                else -> "Compose"
            },
            tint = Ink.OnAccent,
            modifier = Modifier.size(24.dp),
        )
    }
}

/**
 * Dark or light: the one display choice the frame makes for itself, as a row in
 * the drawer's grammar — 52 tall, a 22 glyph, a 17 label, a switch in the accent.
 */
@Composable
private fun ModeRow(mode: String, onToggle: () -> Unit) {
    val dark = mode != com.f33d3r.app.net.Session.MODE_LIGHT
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .height(ROW_HEIGHT)
            .clickable(onClick = onToggle)
            .padding(horizontal = PAGE_PADDING),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Icon(
            imageVector = Icons.Outlined.DarkMode,
            contentDescription = null,
            tint = Ink.Primary,
            modifier = Modifier.size(BAR_GLYPH),
        )
        Spacer(Modifier.width(16.dp))
        Text(
            text = "Dark mode",
            style = MaterialTheme.typography.titleMedium,
            fontSize = 17.sp,
            fontWeight = FontWeight.SemiBold,
            color = Ink.Primary,
            modifier = Modifier.weight(1f),
        )
        Switch(
            checked = dark,
            onCheckedChange = { onToggle() },
            colors = accentSwitchColors(),
        )
    }
    Box(Modifier.fillMaxWidth().height(1.dp).background(Ink.Hairline))
}

/** Offers the works the server has rendered since the timeline was drawn: an accent pill. */
@Composable
private fun HeldWorksBanner(count: Int, onShow: () -> Unit) {
    Row(
        modifier = Modifier
            .height(36.dp)
            .clip(Pill)
            .background(Ink.Accent)
            .clickable(onClick = onShow)
            .padding(horizontal = 14.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Icon(
            imageVector = Icons.Rounded.ArrowUpward,
            contentDescription = null,
            tint = Ink.OnAccent,
            modifier = Modifier.size(16.dp),
        )
        Spacer(Modifier.width(6.dp))
        Text(
            text = if (count == 1) "1 new work" else "$count new works",
            style = MaterialTheme.typography.labelLarge,
            fontSize = 13.sp,
            fontWeight = FontWeight.SemiBold,
            color = Ink.OnAccent,
        )
    }
}

/**
 * The pill every notice from the frame is set in: the raised ground with a hairline
 * around it, so it stands off the page in the light as well as in the dark.
 */
@Composable
private fun NoticePill(
    modifier: Modifier = Modifier,
    content: @Composable RowScope.() -> Unit,
) {
    Row(
        modifier = modifier
            .clip(Pill)
            .background(Ink.Elevated)
            .border(1.dp, Ink.Hairline, Pill)
            .padding(start = 14.dp, end = 14.dp, top = 8.dp, bottom = 8.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        content()
    }
}

/** A short word from the Shell or from a facet's toast, in the Shell's own voice. */
@Composable
private fun Notice(text: String) {
    NoticePill {
        Text(
            text = text,
            style = MaterialTheme.typography.labelLarge,
            fontSize = 13.sp,
            fontWeight = FontWeight.SemiBold,
            color = Ink.Primary,
        )
    }
}

/**
 * What the server refused, in the server's words.
 *
 * The message is shown as it arrived rather than replaced with a generic one: the
 * server knows why it refused, and the reader is entitled to that reason.
 */
@Composable
private fun Refusal(message: String, onDismiss: () -> Unit) {
    NoticePill(modifier = Modifier.fillMaxWidth()) {
        Text(
            text = message,
            style = MaterialTheme.typography.bodyMedium,
            fontSize = 14.sp,
            color = Ink.Refused,
            modifier = Modifier.weight(1f),
        )
        Spacer(Modifier.width(8.dp))
        Box(
            modifier = Modifier
                .size(28.dp)
                .clip(CircleShape)
                .clickable(onClick = onDismiss),
            contentAlignment = Alignment.Center,
        ) {
            Icon(
                imageVector = Icons.Outlined.Close,
                contentDescription = "Dismiss",
                tint = Ink.Tertiary,
                modifier = Modifier.size(18.dp),
            )
        }
    }
}

/** Says plainly when the stream is not up, because then the surfaces are not live. */
@Composable
private fun ConnectionNotice(state: ShellState) {
    Box(
        modifier = Modifier.fillMaxWidth().padding(bottom = 8.dp),
        contentAlignment = Alignment.Center,
    ) {
        NoticePill {
            ConnectionDot(state.connection)
            Spacer(Modifier.width(8.dp))
            Text(
                text = when (state.connection) {
                    com.f33d3r.app.net.live.LiveState.CONNECTING -> "Reconnecting…"
                    else -> "Not live — surfaces will not update until the connection returns"
                },
                style = MaterialTheme.typography.labelLarge,
                fontSize = 13.sp,
                fontWeight = FontWeight.SemiBold,
                color = Ink.Tertiary,
            )
        }
    }
}
