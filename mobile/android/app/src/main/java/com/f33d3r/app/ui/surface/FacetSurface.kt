package com.f33d3r.app.ui.surface

import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberUpdatedState
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.viewinterop.AndroidView
import com.f33d3r.app.core.Fragment
import com.f33d3r.app.net.Identity
import com.f33d3r.app.net.Session
import com.f33d3r.app.ui.SurfaceCommand
import kotlinx.coroutines.flow.Flow

/**
 * The content plane of the Shell.
 *
 * Everything the reader is actually here for is drawn by the server and placed here.
 * The composable owns the surface's lifetime and its wiring to the command stream;
 * it does not own, transform, or inspect what the surface shows.
 */
@Composable
fun FacetSurface(
    session: Session,
    identity: Identity,
    density: String,
    uiMode: String,
    commands: Flow<SurfaceCommand>,
    onIntent: (FacetIntent) -> Unit,
    onReady: () -> Unit,
    onSealedBubbles: (List<PendingBubble>) -> Unit,
    onPageForward: (String) -> Unit,
    onSwipe: (Swipe) -> Unit,
    modifier: Modifier = Modifier,
) {
    val context = LocalContext.current
    val intent = rememberUpdatedState(onIntent)
    val ready = rememberUpdatedState(onReady)
    val paged = rememberUpdatedState(onPageForward)
    val swiped = rememberUpdatedState(onSwipe)

    val host = remember(identity.account) {
        FacetHost(
            context = context,
            session = session,
            onIntent = { intent.value(it) },
            onReady = { ready.value() },
            onPageForward = { paged.value(it) },
            mode = SurfaceMode.SURFACE,
            onSwipe = { swiped.value(it) },
        )
    }

    DisposableEffect(host) {
        host.load(identity.pial, identity.account, identity.handle, density, identity.theme, uiMode)
        onDispose { host.destroy() }
    }

    LaunchedEffect(host, density) { host.setDensity(density) }
    LaunchedEffect(host, uiMode) { host.setUiMode(uiMode) }
    LaunchedEffect(host, identity.theme) { host.setTheme(identity.theme) }

    LaunchedEffect(host, commands) {
        commands.collect { command ->
            when (command) {
                is SurfaceCommand.Mount -> host.mount(command.fragment)
                is SurfaceCommand.Extend -> host.extend(command.fragment)
                is SurfaceCommand.Apply -> host.apply(command.fragment)
                is SurfaceCommand.Append -> host.append(command.container, command.fragment)
                is SurfaceCommand.Place -> host.place(command.container, command.fragment)
                is SurfaceCommand.Remove -> host.remove(command.address)
                is SurfaceCommand.Reveal -> host.reveal(command.address, command.plaintext)
                is SurfaceCommand.SealedFailed ->
                    host.sealedFailed(command.address, command.message)

                is SurfaceCommand.ClearComposer -> host.clearComposer(command.address)
                is SurfaceCommand.Prepend ->
                    // Held works are inserted newest-last so the order the server ranked
                    // them in survives being put on screen in one go.
                    command.fragments.asReversed().forEach(host::prepend)

                SurfaceCommand.OpenSealed -> host.sealedBubbles(onSealedBubbles)
                is SurfaceCommand.Attr -> host.setAttr(command.name, command.value)
                SurfaceCommand.PlayVision, is SurfaceCommand.Step -> Unit
            }
        }
    }

    Box(modifier = modifier.fillMaxSize()) {
        AndroidView(
            factory = { host.view },
            modifier = Modifier.fillMaxSize(),
            onReset = null,
        )
    }
}

/**
 * A second content plane, for a surface the Shell lifts above the timeline: a Vision
 * frame, the chrome around a live picture. Same host, same runtime, same commands —
 * only the dress the host document gives the server's Facets differs, by [mode].
 *
 * It mounts nothing by itself. When its document is ready it says so, and the Shell
 * answers with the render it is holding for it.
 */
@Composable
fun OverlaySurface(
    session: Session,
    identity: Identity,
    mode: SurfaceMode,
    commands: Flow<SurfaceCommand>,
    onIntent: (FacetIntent) -> Unit,
    onReady: () -> Unit,
    onSwipe: ((Swipe) -> Unit)? = null,
    holdToPause: Boolean = false,
    modifier: Modifier = Modifier,
) {
    val context = LocalContext.current
    val intent = rememberUpdatedState(onIntent)
    val ready = rememberUpdatedState(onReady)
    val swiped = rememberUpdatedState(onSwipe)

    val host = remember(identity.account, mode) {
        FacetHost(
            context = context,
            session = session,
            onIntent = { intent.value(it) },
            onReady = { ready.value() },
            onPageForward = {},
            mode = mode,
            onSwipe = { swipe -> swiped.value?.invoke(swipe) },
            holdToPause = holdToPause,
        )
    }

    DisposableEffect(host) {
        host.load(
            identity.pial, identity.account, identity.handle,
            theme = identity.theme, uiMode = session.mode,
        )
        onDispose { host.destroy() }
    }

    LaunchedEffect(host, commands) {
        commands.collect { command ->
            when (command) {
                is SurfaceCommand.Mount -> host.mount(command.fragment)
                is SurfaceCommand.Apply -> host.apply(command.fragment)
                is SurfaceCommand.Append -> host.append(command.container, command.fragment)
                is SurfaceCommand.Place -> host.place(command.container, command.fragment)
                is SurfaceCommand.Remove -> host.remove(command.address)
                is SurfaceCommand.ClearComposer -> host.clearComposer(command.address)
                SurfaceCommand.PlayVision -> host.playVisionMedia()
                is SurfaceCommand.Step -> host.step(command.selector)
                is SurfaceCommand.Attr -> host.setAttr(command.name, command.value)
                // Paging, held works and sealed bubbles belong to the timeline and the
                // chat thread, neither of which is ever lifted into an overlay.
                is SurfaceCommand.Extend,
                is SurfaceCommand.Prepend,
                is SurfaceCommand.Reveal,
                is SurfaceCommand.SealedFailed,
                SurfaceCommand.OpenSealed -> Unit
            }
        }
    }

    Box(modifier = modifier.fillMaxSize()) {
        AndroidView(
            factory = { host.view },
            modifier = Modifier.fillMaxSize(),
            onReset = null,
        )
    }
}

/**
 * One server-rendered Facet, shown on its own — the artboard preview a composer
 * asks the server for, the confirmation a post is answered with.
 *
 * The pane re-mounts whenever it is handed a different render. It never draws a
 * stand-in of its own while waiting: an empty pane is the honest state between one
 * server answer and the next.
 */
@Composable
fun FragmentPane(
    session: Session,
    identity: Identity,
    fragment: Fragment?,
    onIntent: (FacetIntent) -> Unit = {},
    modifier: Modifier = Modifier,
) {
    val context = LocalContext.current
    val intent = rememberUpdatedState(onIntent)
    val host = remember(identity.account) {
        FacetHost(
            context = context,
            session = session,
            onIntent = { intent.value(it) },
            onReady = {},
            onPageForward = {},
            mode = SurfaceMode.PANE,
        )
    }

    DisposableEffect(host) {
        host.load(
            identity.pial, identity.account, identity.handle,
            theme = identity.theme, uiMode = session.mode,
        )
        onDispose { host.destroy() }
    }

    LaunchedEffect(host, fragment) {
        fragment?.let(host::mount)
    }

    Box(modifier = modifier) {
        AndroidView(
            factory = { host.view },
            modifier = Modifier.fillMaxSize(),
            onReset = null,
        )
    }
}
