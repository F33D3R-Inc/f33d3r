package com.f33d3r.app.ui.live

import android.content.Intent
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.statusBarsPadding
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.ChatBubbleOutline
import androidx.compose.material.icons.rounded.ChatBubble
import androidx.compose.material.icons.rounded.Close
import androidx.compose.material.icons.rounded.Paid
import androidx.compose.material.icons.rounded.Share
import androidx.compose.material.icons.rounded.VolumeOff
import androidx.compose.material.icons.rounded.VolumeUp
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.unit.dp
import androidx.compose.ui.viewinterop.AndroidView
import androidx.media3.exoplayer.ExoPlayer
import androidx.media3.ui.AspectRatioFrameLayout
import androidx.media3.ui.PlayerView
import com.f33d3r.app.live.HlsPlayback
import com.f33d3r.app.net.Identity
import com.f33d3r.app.net.Session
import com.f33d3r.app.ui.Overlay
import com.f33d3r.app.ui.ShellModel
import com.f33d3r.app.ui.shell.RailButton
import com.f33d3r.app.ui.surface.OverlaySurface
import com.f33d3r.app.ui.surface.SurfaceMode
import com.f33d3r.app.ui.surface.Swipe
import com.f33d3r.app.ui.theme.Ink

/** The three amounts that sit by the keyboard: a tip without a wallet detour. */
private val QUICK_TIPS = listOf(5, 20, 50)

/**
 * Watching.
 *
 * Clean video, decoded natively and letterboxed at its own aspect. Over it, the
 * server's page around the picture: title and LIVE badge, author and follow, the
 * tally, the chat as bubbles. In the thumb's reach: tip, share, chat on or off,
 * sound. Under the thumb: a line into the chat and three quick tips. Swipe down and
 * the picture goes away while the sound keeps playing in the bar above the rail.
 */
@Composable
fun WatchSurface(
    model: ShellModel,
    session: Session,
    identity: Identity,
    overlay: Overlay.Watch,
    playback: HlsPlayback,
) {
    val context = LocalContext.current
    var chatShown by remember { mutableStateOf(true) }
    var soundOn by remember { mutableStateOf(true) }

    Box(Modifier.fillMaxSize().background(Color.Black)) {
        LivePicture(playback.player)

        OverlaySurface(
            session = session,
            identity = identity,
            mode = SurfaceMode.WATCH,
            commands = model.overlayCommands,
            onIntent = model::dispatchOverlay,
            onReady = model::overlayReady,
            onSwipe = { swipe -> if (swipe == Swipe.DOWN) model.minimizeWatch() },
            modifier = Modifier.fillMaxSize().statusBarsPadding(),
        )

        HudGlyph(
            icon = Icons.Rounded.Close,
            contentDescription = "Close",
            onClick = { model.closeOverlay() },
            modifier = Modifier
                .align(Alignment.TopEnd)
                .statusBarsPadding()
                .padding(top = 10.dp, end = 16.dp),
        )

        Column(
            modifier = Modifier
                .align(Alignment.CenterEnd)
                .padding(end = 16.dp, bottom = 40.dp),
            verticalArrangement = Arrangement.spacedBy(12.dp),
            horizontalAlignment = Alignment.CenterHorizontally,
        ) {
            // The one accent on the picture: the money action.
            RailButton(
                icon = Icons.Rounded.Paid,
                label = "tip",
                onClick = { model.openTip(overlay.authorHandle) },
                tint = Ink.OnAccent,
                ground = Ink.Accent,
            )
            RailButton(Icons.Rounded.Share, "share", onClick = {
                val send = Intent(Intent.ACTION_SEND).apply {
                    type = "text/plain"
                    putExtra(Intent.EXTRA_TEXT, session.origin.trimEnd('/') + "/live/" + overlay.streamId)
                }
                context.startActivity(Intent.createChooser(send, "Share the stream"))
            })
            RailButton(
                icon = if (chatShown) Icons.Rounded.ChatBubble else Icons.Outlined.ChatBubbleOutline,
                label = if (chatShown) "chat off" else "chat on",
                onClick = {
                    chatShown = !chatShown
                    model.setChatShown(chatShown)
                },
            )
            RailButton(
                icon = if (soundOn) Icons.Rounded.VolumeUp else Icons.Rounded.VolumeOff,
                label = if (soundOn) "sound" else "muted",
                onClick = {
                    soundOn = !soundOn
                    playback.setMuted(!soundOn)
                },
            )
        }

        Column(
            modifier = Modifier
                .align(Alignment.BottomCenter)
                .fillMaxWidth()
                .imePadding()
                .navigationBarsPadding()
                .padding(bottom = 12.dp),
        ) {
            LiveChatBar(onSend = model::sendLiveChat) {
                Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                    QUICK_TIPS.forEach { amount ->
                        QuickTipChip(amount) { model.sendTip(overlay.authorHandle, amount * 100L) }
                    }
                }
            }
            Spacer(Modifier.height(4.dp))
        }
    }
}

/** The decoder's output, at the stream's own aspect on black. */
@Composable
private fun LivePicture(player: ExoPlayer) {
    AndroidView(
        factory = { ctx ->
            PlayerView(ctx).apply {
                useController = false
                resizeMode = AspectRatioFrameLayout.RESIZE_MODE_FIT
                setShutterBackgroundColor(android.graphics.Color.BLACK)
                this.player = player
            }
        },
        onRelease = { it.player = null },
        modifier = Modifier.fillMaxSize(),
    )
}
