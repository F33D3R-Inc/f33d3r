package com.f33d3r.app.ui.live

import android.content.Intent
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.statusBarsPadding
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.rounded.Cameraswitch
import androidx.compose.material.icons.rounded.Mic
import androidx.compose.material.icons.rounded.MicOff
import androidx.compose.material.icons.rounded.Share
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.compose.ui.viewinterop.AndroidView
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import com.f33d3r.app.live.PublishState
import com.f33d3r.app.live.Publisher
import com.f33d3r.app.net.Identity
import com.f33d3r.app.net.Session
import com.f33d3r.app.ui.Overlay
import com.f33d3r.app.ui.ShellModel
import com.f33d3r.app.ui.shell.RailButton
import com.f33d3r.app.ui.surface.OverlaySurface
import com.f33d3r.app.ui.surface.SurfaceMode
import com.f33d3r.app.ui.theme.Ink
import org.webrtc.RendererCommon
import org.webrtc.SurfaceViewRenderer

/**
 * Broadcasting.
 *
 * Underneath: the camera, drawn by the publisher. Over it: the server's stage — the
 * LIVE badge, the tally, the title, the status line, the chat — and, when the
 * broadcast ends, the server's summary. Beside it, in the thumb's reach: flip, mute,
 * share, and End. Everything the creator sees about the broadcast's state arrives
 * as a Fragment; the one thing said locally is whether this phone is transmitting.
 */
@Composable
fun BroadcastSurface(
    model: ShellModel,
    session: Session,
    identity: Identity,
    overlay: Overlay.Broadcast,
    publisher: Publisher,
) {
    val context = LocalContext.current
    val publish by publisher.state.collectAsStateWithLifecycle()
    val front by publisher.front.collectAsStateWithLifecycle()
    val muted by publisher.muted.collectAsStateWithLifecycle()
    var confirmEnd by remember { mutableStateOf(false) }
    val capturing = overlay.source == "browser" && !overlay.ended

    Box(Modifier.fillMaxSize().background(Color.Black)) {
        if (capturing) {
            val renderer = remember {
                SurfaceViewRenderer(context).apply {
                    init(publisher.eglBase.eglBaseContext, null)
                    setScalingType(RendererCommon.ScalingType.SCALE_ASPECT_FILL)
                    setEnableHardwareScaler(true)
                }
            }
            DisposableEffect(renderer) {
                publisher.attach(renderer)
                onDispose {
                    publisher.detach()
                    renderer.release()
                }
            }
            LaunchedEffect(front) { renderer.setMirror(front) }
            LaunchedEffect(overlay.streamId) { model.transmit() }
            AndroidView(factory = { renderer }, modifier = Modifier.fillMaxSize())
        }

        OverlaySurface(
            session = session,
            identity = identity,
            mode = SurfaceMode.BROADCAST,
            commands = model.overlayCommands,
            onIntent = model::dispatchOverlay,
            onReady = model::overlayReady,
            modifier = Modifier.fillMaxSize().statusBarsPadding(),
        )

        // What this phone's transmission is doing — the one fact that is local.
        val local = when (val p = publish) {
            PublishState.OpeningCamera -> "Opening the camera…"
            PublishState.Connecting -> "Connecting to the media server…"
            is PublishState.Failed -> "Camera ready, but the broadcast is not transmitting: ${p.reason}"
            else -> ""
        }
        if (capturing && local.isNotEmpty()) {
            HudPill(
                label = local,
                modifier = Modifier
                    .align(Alignment.TopCenter)
                    .statusBarsPadding()
                    .padding(top = 60.dp, start = 16.dp, end = 80.dp),
            )
        }

        if (!overlay.ended) {
            Column(
                modifier = Modifier
                    .align(Alignment.CenterEnd)
                    .padding(end = 16.dp, bottom = 40.dp),
                verticalArrangement = Arrangement.spacedBy(12.dp),
                horizontalAlignment = Alignment.CenterHorizontally,
            ) {
                if (capturing) {
                    RailButton(Icons.Rounded.Cameraswitch, "flip", onClick = publisher::flip)
                    // Muted is a state, not an error: the control inverts, white on black.
                    RailButton(
                        icon = if (muted) Icons.Rounded.MicOff else Icons.Rounded.Mic,
                        label = if (muted) "unmute" else "mic",
                        onClick = { publisher.setMuted(!muted) },
                        tint = if (muted) Color.Black else Color.White,
                        ground = if (muted) Color.White else HUD_GROUND,
                    )
                }
                RailButton(Icons.Rounded.Share, "share", onClick = {
                    val send = Intent(Intent.ACTION_SEND).apply {
                        type = "text/plain"
                        putExtra(Intent.EXTRA_TEXT, session.origin.trimEnd('/') + "/live/" + overlay.streamId)
                    }
                    context.startActivity(Intent.createChooser(send, "Share the stream"))
                })
            }
        }

        Column(
            modifier = Modifier
                .align(Alignment.BottomCenter)
                .fillMaxWidth()
                .imePadding()
                .navigationBarsPadding()
                .padding(bottom = 12.dp),
        ) {
            if (overlay.ended) {
                Row(Modifier.align(Alignment.CenterHorizontally)) {
                    HudButton("Leave", height = 48.dp) { model.closeOverlay() }
                }
            } else {
                LiveChatBar(onSend = model::sendLiveChat) {
                    HudButton("End", height = 48.dp) { confirmEnd = true }
                }
            }
            Spacer(Modifier.height(4.dp))
        }
    }

    if (confirmEnd) {
        AlertDialog(
            onDismissRequest = { confirmEnd = false },
            shape = RoundedCornerShape(28.dp),
            containerColor = Ink.Elevated,
            title = {
                Text(
                    text = "End this broadcast for everyone?",
                    style = MaterialTheme.typography.titleLarge,
                    fontSize = 20.sp,
                    fontWeight = FontWeight.Bold,
                    color = Ink.Primary,
                )
            },
            text = {
                Text(
                    text = "Your summary — peak audience and where it went — is shown once it ends.",
                    style = MaterialTheme.typography.bodyMedium,
                    fontSize = 14.sp,
                    color = Ink.Tertiary,
                )
            },
            confirmButton = {
                TextButton(onClick = { confirmEnd = false; model.endBroadcast() }) {
                    Text("End broadcast", fontSize = 15.sp, fontWeight = FontWeight.SemiBold, color = Ink.Refused)
                }
            },
            dismissButton = {
                TextButton(onClick = { confirmEnd = false }) {
                    Text("Keep going", fontSize = 15.sp, fontWeight = FontWeight.SemiBold, color = Ink.Primary)
                }
            },
        )
    }
}
