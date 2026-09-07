package com.f33d3r.app.ui.vision

import android.Manifest
import android.content.pm.PackageManager
import android.graphics.BitmapFactory
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.camera.compose.CameraXViewfinder
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
import androidx.compose.foundation.horizontalScroll
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
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.statusBarsPadding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.rounded.Cameraswitch
import androidx.compose.material.icons.rounded.Close
import androidx.compose.material.icons.rounded.PhotoCamera
import androidx.compose.material.icons.rounded.TextFields
import androidx.compose.material3.Icon
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
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.layout.ContentScale
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.LocalLifecycleOwner
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.compose.ui.viewinterop.AndroidView
import androidx.core.content.ContextCompat
import androidx.core.net.toUri
import androidx.media3.common.MediaItem
import androidx.media3.common.Player
import androidx.media3.exoplayer.ExoPlayer
import androidx.media3.ui.AspectRatioFrameLayout
import androidx.media3.ui.PlayerView
import coil3.compose.AsyncImage
import com.f33d3r.app.core.Fragment
import com.f33d3r.app.net.Identity
import com.f33d3r.app.net.Session
import com.f33d3r.app.ui.Overlay
import com.f33d3r.app.ui.ShellModel
import com.f33d3r.app.ui.capture.CameraRig
import com.f33d3r.app.ui.live.HudButton
import com.f33d3r.app.ui.live.HudChip
import com.f33d3r.app.ui.live.HudField
import com.f33d3r.app.ui.live.HudGlyph
import com.f33d3r.app.ui.live.HudPill
import com.f33d3r.app.ui.shell.RailButton
import com.f33d3r.app.ui.surface.FragmentPane
import com.f33d3r.app.ui.theme.Ink
import com.f33d3r.app.upload.UploadState
import kotlinx.coroutines.delay
import java.io.File

private enum class Mode(val label: String) { TEXT("Text"), PHOTO("Photo"), CLIP("Clip · 30s") }

/** The clip ceiling the mode chip promises. */
private const val CLIP_SECONDS = 30

/**
 * The preset keys the server's text composer offers, by name. The server resolves
 * each key to a look; nothing here knows what "ember" looks like, and the preview
 * beside these chips is the server's own render of the choice.
 */
private val BACKGROUNDS = listOf("void", "ember", "tide", "bloom", "pulp", "signal")
private val TYPEFACES = listOf("grotesk", "serif", "mono", "display")
private val ALIGNS = listOf("left", "center", "right")

/**
 * The camera-first Vision composer.
 *
 * Three modes, one composer: words on a server-rendered artboard, a photo, a clip.
 * The audience and the lifetime are stated as the server states them — everyone,
 * 24 hours — and the one gate the server may offer, 18+, is offered here only when
 * the server's own composer offered it. What is posted is posted to the same lane
 * the web composer and the web camera post to, and the confirmation is the server's.
 */
@Composable
fun VisionComposer(
    model: ShellModel,
    session: Session,
    identity: Identity,
    overlay: Overlay.VisionCompose,
) {
    val context = LocalContext.current
    val owner = LocalLifecycleOwner.current
    val rig = remember { CameraRig(context, owner) }

    var mode by remember { mutableStateOf(Mode.PHOTO) }
    var captured by remember { mutableStateOf<File?>(null) }
    var caption by remember { mutableStateOf("") }
    var nsfw by remember { mutableStateOf(false) }
    var uploadText by remember { mutableStateOf("") }
    var posting by remember { mutableStateOf(false) }
    var seconds by remember { mutableStateOf(0) }

    // Words on an artboard.
    var body by remember { mutableStateOf("") }
    var background by remember { mutableStateOf(BACKGROUNDS.first()) }
    var typeface by remember { mutableStateOf(TYPEFACES.first()) }
    var align by remember { mutableStateOf("center") }
    var artboard by remember { mutableStateOf<Fragment?>(null) }

    var camGranted by remember {
        mutableStateOf(ContextCompat.checkSelfPermission(context, Manifest.permission.CAMERA) == PackageManager.PERMISSION_GRANTED)
    }
    val ask = rememberLauncherForActivityResult(ActivityResultContracts.RequestMultiplePermissions()) { grants ->
        camGranted = grants[Manifest.permission.CAMERA] == true
    }
    LaunchedEffect(Unit) {
        if (!camGranted) ask.launch(arrayOf(Manifest.permission.CAMERA, Manifest.permission.RECORD_AUDIO))
    }

    // The camera is open exactly while a capture mode is showing the viewfinder.
    LaunchedEffect(mode, captured, camGranted) {
        if (mode != Mode.TEXT && captured == null && camGranted) rig.open(front = rig.front, capture = true) else rig.close()
    }
    DisposableEffect(rig) { onDispose { rig.close() } }

    // The clip stops itself at the ceiling the chip promised.
    LaunchedEffect(rig.recording) {
        seconds = 0
        if (!rig.recording) return@LaunchedEffect
        while (rig.recording && seconds < CLIP_SECONDS) {
            delay(1_000)
            seconds++
        }
        if (rig.recording) rig.stopClip()
    }

    // The artboard is the server's render of the words as they stand, asked for a
    // beat after the last change — the browser composer does the same.
    LaunchedEffect(mode, body, background, typeface, align) {
        if (mode != Mode.TEXT) return@LaunchedEffect
        delay(300)
        artboard = model.previewArtboard(
            mapOf(
                "body" to body,
                "artboard_background" to background,
                "artboard_typeface" to typeface,
                "artboard_type_scale" to "auto",
                "artboard_align" to align,
            ),
        )
    }

    Box(modifier = Modifier.fillMaxSize().background(Color.Black)) {

        // ── Stage ──
        when {
            mode == Mode.TEXT -> Column(Modifier.fillMaxSize().statusBarsPadding()) {
                Spacer(Modifier.height(56.dp))
                FragmentPane(
                    session = session,
                    identity = identity,
                    fragment = artboard,
                    modifier = Modifier.fillMaxWidth().weight(1f).padding(horizontal = 16.dp),
                )
                Spacer(Modifier.height(232.dp))
            }

            captured != null -> {
                val file = captured!!
                if (mode == Mode.CLIP) ClipPreview(file) else {
                    AsyncImage(
                        model = file,
                        contentDescription = "Your Vision",
                        contentScale = ContentScale.Fit,
                        modifier = Modifier.fillMaxSize(),
                    )
                }
            }

            camGranted -> rig.surfaceRequest?.let { request ->
                CameraXViewfinder(surfaceRequest = request, modifier = Modifier.fillMaxSize())
            }

            else -> Box(Modifier.fillMaxSize(), contentAlignment = Alignment.Center) {
                Text(
                    text = "Allow the camera to post a photo or clip Vision.",
                    color = Color.White,
                    style = MaterialTheme.typography.bodyLarge,
                    modifier = Modifier.padding(32.dp),
                )
            }
        }

        // ── Top ──
        Row(
            modifier = Modifier
                .fillMaxWidth()
                .statusBarsPadding()
                .height(52.dp)
                .padding(horizontal = 16.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            HudGlyph(Icons.Rounded.Close, "Close", onClick = { model.closeOverlay() })
            Spacer(Modifier.weight(1f))
            HudPill("New Vision")
            Spacer(Modifier.weight(1f))
            Box(Modifier.width(32.dp))
        }

        // ── Right rail: flip and the way into words ──
        if (captured == null && mode != Mode.TEXT && camGranted) {
            Column(
                modifier = Modifier
                    .align(Alignment.TopEnd)
                    .statusBarsPadding()
                    .padding(top = 64.dp, end = 16.dp),
                verticalArrangement = Arrangement.spacedBy(12.dp),
            ) {
                RailButton(Icons.Rounded.Cameraswitch, "flip", onClick = rig::flip)
                RailButton(Icons.Rounded.TextFields, "Aa", onClick = { mode = Mode.TEXT })
            }
        }

        // ── Bottom ──
        Column(
            modifier = Modifier
                .align(Alignment.BottomCenter)
                .fillMaxWidth()
                .imePadding()
                .navigationBarsPadding()
                .padding(bottom = 10.dp),
            horizontalAlignment = Alignment.CenterHorizontally,
        ) {
            if (uploadText.isNotEmpty()) {
                HudPill(uploadText, modifier = Modifier.padding(bottom = 8.dp))
            }

            when {
                mode == Mode.TEXT -> TextControls(
                    body = body, onBody = { body = it },
                    background = background, onBackground = { background = it },
                    typeface = typeface, onTypeface = { typeface = it },
                    align = align, onAlign = { align = it },
                    canNsfw = overlay.canNsfw, nsfw = nsfw, onNsfw = { nsfw = !nsfw },
                    onCamera = { mode = Mode.PHOTO },
                    postEnabled = body.isNotBlank() && !posting,
                    onPost = {
                        posting = true
                        model.postVisionText(body, background, typeface, "auto", align, nsfw)
                    },
                )

                captured != null -> CaptionControls(
                    caption = caption, onCaption = { caption = it },
                    canNsfw = overlay.canNsfw, nsfw = nsfw, onNsfw = { nsfw = !nsfw },
                    postEnabled = !posting,
                    onRetake = { captured?.delete(); captured = null; uploadText = "" },
                    onPost = {
                        val file = captured ?: return@CaptionControls
                        posting = true
                        if (mode == Mode.CLIP) {
                            model.postVisionClip(file, caption, nsfw) { state ->
                                uploadText = when (state) {
                                    is UploadState.Sending -> "Uploading… ${(state.fraction * 100).toInt()}%"
                                    UploadState.Processing -> "Processing the clip…"
                                    is UploadState.Refused -> { posting = false; state.message }
                                    is UploadState.Duplicate -> { posting = false; "" }
                                    else -> ""
                                }
                            }
                        } else {
                            val bounds = BitmapFactory.Options().apply { inJustDecodeBounds = true }
                            BitmapFactory.decodeFile(file.absolutePath, bounds)
                            model.postVisionPhoto(file, caption, nsfw, bounds.outWidth.coerceAtLeast(0), bounds.outHeight.coerceAtLeast(0))
                        }
                    },
                )

                else -> CaptureControls(
                    mode = mode, onMode = { mode = it },
                    canNsfw = overlay.canNsfw, nsfw = nsfw, onNsfw = { nsfw = !nsfw },
                    recording = rig.recording, seconds = seconds,
                    enabled = camGranted,
                    onShutter = {
                        if (mode == Mode.CLIP) {
                            if (rig.recording) {
                                rig.stopClip()
                            } else {
                                val file = File(context.cacheDir, "vision-${System.currentTimeMillis()}.mp4")
                                rig.startClip(file) { result ->
                                    result.onSuccess { captured = it }
                                        .onFailure { uploadText = "The clip could not be recorded." }
                                }
                            }
                        } else {
                            val file = File(context.cacheDir, "vision-${System.currentTimeMillis()}.jpg")
                            rig.takePhoto(file) { result ->
                                result.onSuccess { captured = it }
                                    .onFailure { uploadText = "The photo could not be taken." }
                            }
                        }
                    },
                )
            }
        }
    }
}

@Composable
private fun CaptureControls(
    mode: Mode,
    onMode: (Mode) -> Unit,
    canNsfw: Boolean,
    nsfw: Boolean,
    onNsfw: () -> Unit,
    recording: Boolean,
    seconds: Int,
    enabled: Boolean,
    onShutter: () -> Unit,
) {
    Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
        Mode.entries.forEach { m ->
            HudChip(label = m.label, filled = m == mode, onClick = { if (!recording) onMode(m) })
        }
    }
    Spacer(Modifier.height(8.dp))
    AudienceChips(canNsfw = canNsfw, nsfw = nsfw, onNsfw = onNsfw)
    Spacer(Modifier.height(16.dp))
    // Recording is the one thing the live red says on this surface.
    if (recording) {
        HudPill("0:%02d".format(seconds), tint = Ink.Live)
        Spacer(Modifier.height(8.dp))
    }
    Box(
        modifier = Modifier
            .size(74.dp)
            .clip(CircleShape)
            .border(4.dp, Color.White, CircleShape)
            .clickable(enabled = enabled, onClick = onShutter),
        contentAlignment = Alignment.Center,
    ) {
        if (mode == Mode.CLIP) {
            Box(
                modifier = Modifier
                    .size(if (recording) 28.dp else 58.dp)
                    .clip(if (recording) RoundedCornerShape(6.dp) else CircleShape)
                    .background(Ink.Live),
            )
        } else {
            Box(
                modifier = Modifier
                    .size(58.dp)
                    .clip(CircleShape)
                    .background(Color.White),
            )
        }
    }
}

@Composable
private fun AudienceChips(canNsfw: Boolean, nsfw: Boolean, onNsfw: () -> Unit) {
    Row(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalAlignment = Alignment.CenterVertically) {
        // What the server states about every Vision: who sees it and for how long.
        HudChip(label = "Everyone · 24h", onClick = {})
        if (canNsfw) HudChip(label = if (nsfw) "18+ ✓" else "18+", filled = nsfw, onClick = onNsfw)
    }
}

@Composable
private fun CaptionControls(
    caption: String,
    onCaption: (String) -> Unit,
    canNsfw: Boolean,
    nsfw: Boolean,
    onNsfw: () -> Unit,
    postEnabled: Boolean,
    onRetake: () -> Unit,
    onPost: () -> Unit,
) {
    AudienceChips(canNsfw = canNsfw, nsfw = nsfw, onNsfw = onNsfw)
    Spacer(Modifier.height(12.dp))
    Row(
        modifier = Modifier.fillMaxWidth().padding(horizontal = 16.dp),
        verticalAlignment = Alignment.CenterVertically,
        horizontalArrangement = Arrangement.spacedBy(8.dp),
    ) {
        HudChip(label = "Retake", onClick = onRetake)
        HudField(
            value = caption,
            onValue = { if (it.length <= 280) onCaption(it) },
            placeholder = "Say something…",
            modifier = Modifier.weight(1f),
        )
        HudButton("Post", enabled = postEnabled, onClick = onPost)
    }
}

@Composable
private fun TextControls(
    body: String,
    onBody: (String) -> Unit,
    background: String,
    onBackground: (String) -> Unit,
    typeface: String,
    onTypeface: (String) -> Unit,
    align: String,
    onAlign: (String) -> Unit,
    canNsfw: Boolean,
    nsfw: Boolean,
    onNsfw: () -> Unit,
    onCamera: () -> Unit,
    postEnabled: Boolean,
    onPost: () -> Unit,
) {
    Column(Modifier.fillMaxWidth().padding(horizontal = 16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
        HudField(
            value = body,
            onValue = { if (it.length <= 280) onBody(it) },
            placeholder = "Say something.",
            singleLine = false,
            modifier = Modifier.fillMaxWidth(),
        )
        ChipRow(BACKGROUNDS, background, onBackground)
        ChipRow(TYPEFACES, typeface, onTypeface)
        Row(
            modifier = Modifier.fillMaxWidth().horizontalScroll(rememberScrollState()),
            horizontalArrangement = Arrangement.spacedBy(8.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            ALIGNS.forEach { key -> HudChip(label = key, filled = key == align, onClick = { onAlign(key) }) }
            Spacer(Modifier.width(8.dp))
            HudChip(label = "Everyone · 24h", onClick = {})
            if (canNsfw) HudChip(label = if (nsfw) "18+ ✓" else "18+", filled = nsfw, onClick = onNsfw)
        }
        Row(verticalAlignment = Alignment.CenterVertically) {
            HudGlyph(Icons.Rounded.PhotoCamera, "Camera", onClick = onCamera, size = 40.dp)
            Spacer(Modifier.weight(1f))
            HudButton("Post", enabled = postEnabled, onClick = onPost)
        }
    }
}

@Composable
private fun ChipRow(keys: List<String>, selected: String, onSelect: (String) -> Unit) {
    Row(
        modifier = Modifier.horizontalScroll(rememberScrollState()),
        horizontalArrangement = Arrangement.spacedBy(8.dp),
    ) {
        keys.forEach { key -> HudChip(label = key, filled = key == selected, onClick = { onSelect(key) }) }
    }
}

/** The captured clip, looping, before it is posted. */
@Composable
private fun ClipPreview(file: File) {
    val context = LocalContext.current
    val player = remember(file) {
        ExoPlayer.Builder(context).build().apply {
            setMediaItem(MediaItem.fromUri(file.toUri()))
            repeatMode = Player.REPEAT_MODE_ALL
            prepare()
            playWhenReady = true
        }
    }
    DisposableEffect(player) { onDispose { player.release() } }
    AndroidView(
        factory = { ctx ->
            PlayerView(ctx).apply {
                useController = false
                resizeMode = AspectRatioFrameLayout.RESIZE_MODE_FIT
                this.player = player
            }
        },
        modifier = Modifier.fillMaxSize(),
    )
}
