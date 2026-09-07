package com.f33d3r.app.ui.vision

import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.statusBarsPadding
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.rounded.MoreHoriz
import androidx.compose.material.icons.rounded.Paid
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import com.f33d3r.app.net.Identity
import com.f33d3r.app.net.Session
import com.f33d3r.app.ui.Overlay
import com.f33d3r.app.ui.ShellModel
import com.f33d3r.app.ui.live.HUD_LIFT
import com.f33d3r.app.ui.live.HudField
import com.f33d3r.app.ui.live.HudGlyph
import com.f33d3r.app.ui.surface.OverlaySurface
import com.f33d3r.app.ui.surface.SurfaceMode
import com.f33d3r.app.ui.theme.Ink

/**
 * The sequential Vision viewer, lifted over the timeline.
 *
 * The frame is the server's: one Vision, its place in the author's run, its
 * neighbours' addresses. What the phone adds is the phone's part — a swipe that
 * presses the frame's own step controls, a hold that pauses the clip, a swipe down
 * that puts the frame away — and one bar under it: a private reply that lands in
 * the author's messages, a tip, and a menu. No public likes, no repost, no count.
 */
@Composable
fun VisionViewer(
    model: ShellModel,
    session: Session,
    identity: Identity,
    overlay: Overlay.Vision,
) {
    Column(
        modifier = Modifier
            .fillMaxSize()
            .background(Color.Black)
            .statusBarsPadding(),
    ) {
        OverlaySurface(
            session = session,
            identity = identity,
            mode = SurfaceMode.VISION,
            commands = model.overlayCommands,
            onIntent = model::dispatchOverlay,
            onReady = model::overlayReady,
            onSwipe = model::visionSwipe,
            holdToPause = true,
            modifier = Modifier.weight(1f).fillMaxWidth(),
        )
        ReplyBar(
            authorHandle = overlay.authorHandle,
            onReply = { body -> model.replyPrivately(overlay.authorHandle, body) },
            onTip = { model.openTip(overlay.authorHandle) },
            onMute = { model.muteAuthor(overlay.authorHandle) },
        )
    }
}

@Composable
private fun ReplyBar(
    authorHandle: String,
    onReply: (String) -> Unit,
    onTip: () -> Unit,
    onMute: () -> Unit,
) {
    var draft by remember { mutableStateOf("") }
    var menu by remember { mutableStateOf(false) }
    fun send() {
        val text = draft.trim()
        if (text.isEmpty()) return
        onReply(text)
        draft = ""
    }
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .background(Color.Black)
            .imePadding()
            .navigationBarsPadding()
            .padding(horizontal = 16.dp, vertical = 10.dp),
        verticalAlignment = Alignment.CenterVertically,
        horizontalArrangement = Arrangement.spacedBy(8.dp),
    ) {
        // The bar's own ground is black, so the pill lifts off it in white rather than sinking in black.
        HudField(
            value = draft,
            onValue = { if (it.length <= 1000) draft = it },
            placeholder = if (authorHandle.isEmpty()) "Reply privately…" else "Reply privately to @$authorHandle…",
            onSend = ::send,
            enabled = authorHandle.isNotEmpty(),
            ground = HUD_LIFT,
            modifier = Modifier.weight(1f),
        )

        // The one accent on the picture: the money action.
        HudGlyph(
            icon = Icons.Rounded.Paid,
            contentDescription = "Tip",
            onClick = { if (authorHandle.isNotEmpty()) onTip() },
            size = 48.dp,
            ground = Ink.Accent,
            tint = Ink.OnAccent,
        )

        Box {
            HudGlyph(
                icon = Icons.Rounded.MoreHoriz,
                contentDescription = "More",
                onClick = { menu = true },
                size = 48.dp,
                ground = HUD_LIFT,
            )
            DropdownMenu(
                expanded = menu,
                onDismissRequest = { menu = false },
                shape = RoundedCornerShape(16.dp),
                containerColor = Ink.Elevated,
            ) {
                DropdownMenuItem(
                    text = {
                        Text(
                            text = if (authorHandle.isEmpty()) "Mute" else "Mute @$authorHandle",
                            style = MaterialTheme.typography.bodyLarge,
                            fontSize = 16.sp,
                            color = Ink.Primary,
                        )
                    },
                    enabled = authorHandle.isNotEmpty(),
                    onClick = { menu = false; onMute() },
                )
            }
        }
    }
}
