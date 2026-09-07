package com.f33d3r.app.ui.live

import android.Manifest
import android.content.Context
import android.content.pm.PackageManager
import android.net.ConnectivityManager
import android.net.NetworkCapabilities
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.camera.compose.CameraXViewfinder
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
import androidx.compose.foundation.layout.width
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.rounded.Close
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
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
import androidx.compose.ui.platform.LocalLifecycleOwner
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.core.content.ContextCompat
import com.f33d3r.app.ui.Overlay
import com.f33d3r.app.ui.ShellModel
import com.f33d3r.app.ui.capture.CameraRig
import com.f33d3r.app.ui.compose.ApplyButton
import com.f33d3r.app.ui.compose.SheetField
import com.f33d3r.app.ui.compose.SheetPanel
import com.f33d3r.app.ui.compose.SheetTitle
import com.f33d3r.app.ui.shell.Chip
import com.f33d3r.app.ui.theme.Ink
import kotlinx.coroutines.delay

/**
 * Go live — the setup sheet over the camera.
 *
 * Every setting is a visible chip and every check is visible before the button
 * enables: the camera and microphone are held, the network is up. What the sheet
 * offers is what the server's own go-live composer offers — a title, the camera
 * facing, this phone or an encoder, and 18+ only when the server drew that control.
 * Pressing the button opens the stream on the server; the stream goes LIVE when the
 * media server sees frames, and the server says so.
 */
@Composable
fun GoLiveSheet(
    model: ShellModel,
    overlay: Overlay.GoLive,
    handle: String,
    loading: Boolean,
) {
    val context = LocalContext.current
    val owner = LocalLifecycleOwner.current
    val rig = remember { CameraRig(context, owner) }

    var title by remember { mutableStateOf("") }
    var nsfw by remember { mutableStateOf(false) }
    var encoder by remember { mutableStateOf(false) }
    var camGranted by remember { mutableStateOf(granted(context, Manifest.permission.CAMERA)) }
    var micGranted by remember { mutableStateOf(granted(context, Manifest.permission.RECORD_AUDIO)) }
    var online by remember { mutableStateOf(isOnline(context)) }

    val ask = rememberLauncherForActivityResult(ActivityResultContracts.RequestMultiplePermissions()) { grants ->
        camGranted = grants[Manifest.permission.CAMERA] ?: camGranted
        micGranted = grants[Manifest.permission.RECORD_AUDIO] ?: micGranted
    }
    LaunchedEffect(Unit) {
        if (!camGranted || !micGranted) ask.launch(arrayOf(Manifest.permission.CAMERA, Manifest.permission.RECORD_AUDIO))
    }
    LaunchedEffect(Unit) {
        while (true) {
            online = isOnline(context)
            delay(3_000)
        }
    }
    LaunchedEffect(camGranted, encoder) {
        if (camGranted && !encoder) rig.open(front = rig.front, capture = false) else rig.close()
    }
    DisposableEffect(rig) { onDispose { rig.close() } }

    val ready = title.isNotBlank() && online && !loading && (encoder || (camGranted && micGranted))

    Box(Modifier.fillMaxSize().background(Color.Black)) {
        if (camGranted && !encoder) {
            rig.surfaceRequest?.let { request ->
                CameraXViewfinder(surfaceRequest = request, modifier = Modifier.fillMaxSize())
            }
        }

        Column(
            modifier = Modifier
                .fillMaxSize()
                .statusBarsPadding()
                .imePadding()
                .navigationBarsPadding(),
        ) {
            // ── HUD over the camera: close, the checks, the facing ──
            Row(
                modifier = Modifier
                    .fillMaxWidth()
                    .height(52.dp)
                    .padding(horizontal = 16.dp),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                HudGlyph(Icons.Rounded.Close, "Close", onClick = { model.closeOverlay() })
                Spacer(Modifier.weight(1f))
                if (!encoder) {
                    HudChip(label = if (rig.front) "Front" else "Rear", onClick = rig::flip)
                } else {
                    Box(Modifier.width(32.dp))
                }
            }
            Row(
                modifier = Modifier.padding(horizontal = 16.dp),
                horizontalArrangement = Arrangement.spacedBy(8.dp),
            ) {
                StatusPill(if (online) "Network good" else "No network", online)
                if (!encoder) StatusPill(if (micGranted) "Mic ready" else "Mic needed", micGranted)
                if (!encoder) StatusPill(if (camGranted) "Camera ready" else "Camera needed", camGranted)
            }

            Spacer(Modifier.weight(1f))

            // ── The sheet: the same panel every sheet in the Shell is drawn on ──
            SheetPanel {
                SheetTitle(
                    "Go live",
                    subtitle = if (encoder) {
                        "@$handle · the next screen gives you the server and stream key"
                    } else {
                        "@$handle · streaming from this phone"
                    },
                )
                SheetField(
                    value = title,
                    onValue = { if (it.length <= 120) title = it },
                    placeholder = "Stream title…",
                )
                Spacer(Modifier.height(12.dp))
                Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                    Chip(label = "Everyone", filled = true, onClick = {})
                    if (overlay.canNsfw) Chip(label = if (nsfw) "18+ ✓" else "18+", filled = nsfw, onClick = { nsfw = !nsfw })
                }
                Spacer(Modifier.height(8.dp))
                Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                    Chip(label = "This phone", filled = !encoder, onClick = { encoder = false })
                    Chip(label = "OBS encoder", filled = encoder, onClick = { encoder = true })
                }
                if (overlay.error.isNotEmpty()) {
                    Spacer(Modifier.height(12.dp))
                    Text(
                        text = overlay.error,
                        style = MaterialTheme.typography.bodyMedium,
                        fontSize = 14.sp,
                        color = Ink.Refused,
                    )
                }
                Spacer(Modifier.height(20.dp))
                ApplyButton(if (loading) "Opening…" else "Go live", enabled = ready) {
                    // The rig lets go of the camera here, before the server answers,
                    // so the publisher finds it free when the broadcast surface opens.
                    rig.close()
                    model.startBroadcast(title, "", rig.front, nsfw, encoder)
                }
            }
        }
    }
}

/** A check over the camera: white when it holds, refused ink when it does not. */
@Composable
private fun StatusPill(label: String, good: Boolean) {
    HudPill(label = label, tint = if (good) Color.White else Ink.Refused)
}

private fun granted(context: Context, permission: String): Boolean =
    ContextCompat.checkSelfPermission(context, permission) == PackageManager.PERMISSION_GRANTED

private fun isOnline(context: Context): Boolean {
    val cm = context.getSystemService(ConnectivityManager::class.java) ?: return false
    val caps = cm.getNetworkCapabilities(cm.activeNetwork) ?: return false
    return caps.hasCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
}
