package com.f33d3r.app.ui.compose

import androidx.activity.compose.BackHandler
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.PickVisualMediaRequest
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
import androidx.compose.foundation.gestures.detectDragGesturesAfterLongPress
import androidx.compose.foundation.horizontalScroll
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.IntrinsicSize
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxHeight
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
import androidx.compose.foundation.text.BasicTextField
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.Article
import androidx.compose.material.icons.outlined.AttachMoney
import androidx.compose.material.icons.outlined.Image
import androidx.compose.material.icons.outlined.MusicNote
import androidx.compose.material.icons.outlined.Videocam
import androidx.compose.material.icons.rounded.Close
import androidx.compose.material.icons.rounded.DragHandle
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableFloatStateOf
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.mutableStateMapOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.rememberUpdatedState
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.alpha
import androidx.compose.ui.draw.clip
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.focus.onFocusChanged
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.SolidColor
import androidx.compose.ui.graphics.graphicsLayer
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.input.pointer.pointerInput
import androidx.compose.ui.layout.ContentScale
import androidx.compose.ui.layout.onSizeChanged
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.TextRange
import androidx.compose.ui.text.TextStyle
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.TextFieldValue
import androidx.compose.ui.text.buildAnnotatedString
import androidx.compose.ui.text.withStyle
import androidx.compose.ui.text.SpanStyle
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.compose.ui.zIndex
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import coil3.compose.AsyncImage
import com.f33d3r.app.compose.Mention
import com.f33d3r.app.compose.PickedFile
import com.f33d3r.app.compose.VideoAttachment
import com.f33d3r.app.compose.WorkDraft
import com.f33d3r.app.core.Fragment
import com.f33d3r.app.core.Wire
import com.f33d3r.app.net.Session
import com.f33d3r.app.ui.Overlay
import com.f33d3r.app.ui.ShellModel
import com.f33d3r.app.ui.ShellState
import com.f33d3r.app.ui.shell.Chip
import com.f33d3r.app.ui.shell.PersonaAvatar
import com.f33d3r.app.ui.surface.FragmentPane
import com.f33d3r.app.ui.theme.Ink
import com.f33d3r.app.upload.UploadState
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch

private enum class Sheet { NONE, REPLY, LANE, SCHEDULE, MONETIZE, DRAFTS }

/** Black at 60%, drawn only over a picture, under a white glyph. */
private val OVER_MEDIA = Color(0x99000000)

/**
 * The work composer, lifted over the frame.
 *
 * It collects words, attachments and the few settings the server offers a work —
 * who may reply, subscribers only, when, which lane, 18+ — and hands them to the
 * model to sign and send. It renders no preview of the work it is about to create:
 * the work does not exist until the server has verified the signature and stored
 * it, and when it does, the server renders it. The one preview here, the quoted
 * work, is the server's own render of a work that already exists.
 *
 * What is typed is kept as a draft on this device, continuously, so nothing is
 * lost to a call or the back gesture.
 */
@Composable
fun Composer(model: ShellModel, session: Session, state: ShellState, overlay: Overlay.Compose) {
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    val identity = state.identity
    val drafts by model.drafts.collectAsStateWithLifecycle()

    var draft by remember(overlay.draft) { mutableStateOf(overlay.draft) }
    var bodyField by remember(overlay.draft) {
        mutableStateOf(TextFieldValue(overlay.draft.body, TextRange(overlay.draft.body.length)))
    }
    var saved by remember(overlay.draft) { mutableStateOf(overlay.draft.savedAt > 0) }
    var uploading by remember { mutableIntStateOf(0) }
    var uploadText by remember { mutableStateOf("") }
    var note by remember { mutableStateOf("") }
    var mentions by remember { mutableStateOf<List<Mention>>(emptyList()) }
    var quote by remember { mutableStateOf<Fragment?>(null) }
    var focused by remember { mutableIntStateOf(0) }
    // The post that should take the keyboard next: a segment just added, or the one
    // a removed segment hands back to. -1 when nothing is owed focus.
    var focusOwed by remember { mutableIntStateOf(-1) }
    var sheet by remember { mutableStateOf(Sheet.NONE) }
    val bodyFocus = remember { FocusRequester() }

    fun setBody(value: TextFieldValue) {
        bodyField = value
        if (value.text != draft.body) draft = draft.copy(body = value.text)
    }

    // Every post in order: the body first, then the thread's continuations.
    val posts = listOf(bodyField.text) + draft.segments
    val focusedText = posts.getOrElse(focused) { bodyField.text }
    val overLimit = posts.any { it.length > WorkDraft.BODY_LIMIT }
    val remaining = WorkDraft.BODY_LIMIT - focusedText.length
    val canPost = !overlay.posting && uploading == 0 && !overLimit && draft.hasContent

    fun cancel() {
        if (draft.hasContent) model.saveDraft(draft) else model.discardDraft(draft.id)
        model.closeOverlay()
    }
    BackHandler { cancel() }

    // The keyboard is up on open: one tap to typing, no format chosen up front.
    LaunchedEffect(overlay.draft.id) { bodyFocus.requestFocus() }

    // Kept as a draft a beat after every change; forgotten again once emptied.
    LaunchedEffect(draft) {
        delay(800)
        if (draft.hasContent) {
            model.saveDraft(draft)
            saved = true
        } else if (saved) {
            model.discardDraft(draft.id)
            saved = false
        }
    }

    // @-completion: the server's handles for the token under the caret.
    LaunchedEffect(bodyField.text, bodyField.selection, focused) {
        val token = if (focused == 0) mentionAtCursor(bodyField.text, bodyField.selection.end) else null
        if (token == null) {
            mentions = emptyList()
            return@LaunchedEffect
        }
        delay(250)
        mentions = model.mentionSuggestions(token)
    }

    LaunchedEffect(draft.quoteWorkId) {
        quote = if (draft.quoteWorkId.isEmpty()) null else model.quotePreview(draft.quoteWorkId)
    }

    val pickImages = rememberLauncherForActivityResult(
        ActivityResultContracts.PickMultipleVisualMedia(WorkDraft.MAX_IMAGES),
    ) { uris ->
        if (uris.isEmpty()) return@rememberLauncherForActivityResult
        scope.launch {
            val room = (WorkDraft.MAX_IMAGES - draft.images.size).coerceAtLeast(0)
            for (uri in uris.take(room)) {
                val picked = PickedFile.from(context, uri) ?: continue
                uploading++
                when (val result = model.uploadImage(picked)) {
                    is UploadState.Stored -> draft = draft.copy(images = draft.images + result.url)
                    is UploadState.Refused -> note = result.message
                    else -> Unit
                }
                uploading--
            }
        }
    }

    val pickVideo = rememberLauncherForActivityResult(ActivityResultContracts.PickVisualMedia()) { uri ->
        uri ?: return@rememberLauncherForActivityResult
        scope.launch {
            val picked = PickedFile.from(context, uri) ?: return@launch
            uploading++
            val result = model.uploadVideo(picked) { progress ->
                uploadText = when (progress) {
                    is UploadState.Sending -> "Uploading… ${(progress.fraction * 100).toInt()}%"
                    UploadState.Processing -> "Processing the clip…"
                    else -> ""
                }
            }
            uploadText = ""
            when (result) {
                is UploadState.Transcoded -> draft = draft.copy(
                    video = VideoAttachment(
                        masterUrl = result.masterUrl,
                        posterUrl = result.posterUrl,
                        durationSecs = result.durationSecs,
                        width = result.width,
                        height = result.height,
                        tusUploadId = result.tusUploadId,
                    ),
                )

                is UploadState.Duplicate -> {
                    val who = if (result.creatorHandle.isNotEmpty()) ", by @${result.creatorHandle}" else ""
                    if (result.masterUrl.isNotEmpty()) {
                        draft = draft.copy(video = VideoAttachment(masterUrl = result.masterUrl, posterUrl = result.posterUrl))
                        note = "This clip is already on F33D3R$who. The work will cite the original."
                    } else {
                        note = "This clip is already on F33D3R$who."
                    }
                }

                is UploadState.Refused -> note = result.message
                else -> Unit
            }
            uploading--
        }
    }

    fun reorder(from: Int, to: Int) {
        val list = posts.toMutableList()
        val item = list.removeAt(from)
        list.add(to, item)
        draft = draft.copy(body = list[0], segments = list.drop(1))
        bodyField = TextFieldValue(list[0], TextRange(list[0].length))
        focused = to
    }

    fun setSegment(index: Int, text: String) {
        draft = draft.copy(segments = draft.segments.toMutableList().also { it[index - 1] = text })
    }

    fun removeSegment(index: Int) {
        if (index == 0) return
        draft = draft.copy(segments = draft.segments.toMutableList().also { it.removeAt(index - 1) })
        focused = (index - 1).coerceAtLeast(0)
        focusOwed = focused
    }

    fun addSegment() {
        draft = draft.copy(segments = draft.segments + "")
        focused = draft.segments.size
        focusOwed = focused
    }

    fun splitBody() {
        val parts = splitIntoPosts(bodyField.text, WorkDraft.BODY_LIMIT)
        draft = draft.copy(body = parts[0], segments = parts.drop(1) + draft.segments)
        bodyField = TextFieldValue(parts[0], TextRange(parts[0].length))
    }

    Box(
        Modifier
            .fillMaxSize()
            .background(Ink.Surface)
            .statusBarsPadding(),
    ) {
        Column(Modifier.fillMaxSize()) {

            // ── Cancel · drafts / saved / thread · Post ──
            Row(
                modifier = Modifier
                    .fillMaxWidth()
                    .height(52.dp)
                    .padding(horizontal = 16.dp),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                Box(
                    modifier = Modifier
                        .height(44.dp)
                        .clip(CircleShape)
                        .clickable(onClick = ::cancel)
                        .padding(horizontal = 4.dp),
                    contentAlignment = Alignment.Center,
                ) {
                    Text(
                        text = "Cancel",
                        style = MaterialTheme.typography.labelLarge,
                        fontSize = 15.sp,
                        fontWeight = FontWeight.SemiBold,
                        color = Ink.Primary,
                    )
                }
                Spacer(Modifier.weight(1f))
                when {
                    draft.isThread -> Text(
                        "Thread · ${posts.size}",
                        style = MaterialTheme.typography.labelLarge,
                        fontSize = 13.sp,
                        fontWeight = FontWeight.SemiBold,
                        color = Ink.Tertiary,
                    )
                    saved -> Chip(label = "Saved to drafts", onClick = { sheet = Sheet.DRAFTS })
                    drafts.isNotEmpty() -> Chip(label = "Drafts ${drafts.size}", onClick = { sheet = Sheet.DRAFTS })
                }
                Spacer(Modifier.weight(1f))
                Box(
                    modifier = Modifier
                        .height(36.dp)
                        .clip(CircleShape)
                        .background(if (canPost) Ink.Accent else Ink.Faint)
                        .clickable(enabled = canPost) { model.publish(draft.copy(body = bodyField.text)) }
                        .padding(horizontal = 18.dp),
                    contentAlignment = Alignment.Center,
                ) {
                    Text(
                        text = when {
                            overlay.posting -> "Posting…"
                            draft.isThread -> "Post all"
                            else -> "Post"
                        },
                        style = MaterialTheme.typography.labelLarge,
                        fontSize = 15.sp,
                        fontWeight = FontWeight.Bold,
                        color = if (canPost) Ink.OnAccent else Ink.Muted,
                    )
                }
            }

            // ── The work ──
            Column(
                modifier = Modifier
                    .weight(1f)
                    .fillMaxWidth()
                    .verticalScroll(rememberScrollState())
                    .padding(horizontal = 16.dp),
            ) {
                draft.replyTo?.let { target ->
                    Text(
                        text = buildAnnotatedString {
                            append("Replying to ")
                            withStyle(SpanStyle(color = Ink.Accent)) { append("@${target.handle}") }
                        },
                        style = MaterialTheme.typography.bodyMedium,
                        fontSize = 14.sp,
                        color = Ink.Tertiary,
                        modifier = Modifier.padding(start = 52.dp, top = 8.dp, bottom = 4.dp),
                    )
                }

                if (!draft.isThread) {
                    Row(verticalAlignment = Alignment.Top) {
                        PersonaAvatar(identity.avatarUrl, identity.handle, size = 40.dp)
                        Spacer(Modifier.width(12.dp))
                        Column(Modifier.weight(1f)) {
                            AuthorLine(identity.handle, draft.subscriberOnly) { sheet = Sheet.MONETIZE }
                            BodyField(
                                value = bodyField,
                                onValue = ::setBody,
                                placeholder = if (draft.replyTo != null) "Post your reply" else "What's happening?",
                                modifier = Modifier
                                    .fillMaxWidth()
                                    .focusRequester(bodyFocus)
                                    .onFocusChanged { if (it.isFocused) focused = 0 },
                            )
                        }
                    }
                } else {
                    AuthorLine(identity.handle, draft.subscriberOnly, modifier = Modifier.padding(start = 52.dp)) { sheet = Sheet.MONETIZE }
                    ThreadList(
                        posts = posts,
                        bodyField = bodyField,
                        onBody = ::setBody,
                        onSegment = ::setSegment,
                        onRemove = ::removeSegment,
                        onReorder = ::reorder,
                        focused = focused,
                        onFocus = { focused = it },
                        focusOwed = focusOwed,
                        onFocusPaid = { focusOwed = -1 },
                        bodyFocus = bodyFocus,
                        avatarUrl = identity.avatarUrl,
                        handle = identity.handle,
                        gatedFrom = if (draft.subscriberOnly) (if (draft.previewFirst) 1 else 0) else Int.MAX_VALUE,
                    )
                }

                Column(Modifier.padding(start = 52.dp)) {
                    if (draft.images.isNotEmpty() || draft.video != null) {
                        Spacer(Modifier.height(10.dp))
                        MediaStrip(
                            origin = session.origin,
                            images = draft.images,
                            video = draft.video,
                            onRemoveImage = { url -> draft = draft.copy(images = draft.images - url) },
                            onRemoveVideo = { draft = draft.copy(video = null) },
                        )
                    }
                    if (uploadText.isNotEmpty()) {
                        Spacer(Modifier.height(6.dp))
                        Text(uploadText, style = MaterialTheme.typography.bodyMedium, fontSize = 14.sp, color = Ink.Tertiary)
                    }
                    if (draft.quoteWorkId.isNotEmpty()) {
                        Spacer(Modifier.height(10.dp))
                        QuoteCard(session, state, quote) { draft = draft.copy(quoteWorkId = "") }
                    }
                    val links = linkCount(bodyField.text)
                    if (links > 0) {
                        Spacer(Modifier.height(6.dp))
                        Text(
                            "$links link${if (links == 1) "" else "s"} found · preview will show",
                            style = MaterialTheme.typography.bodyMedium,
                            fontSize = 14.sp,
                            color = Ink.Tertiary,
                        )
                    }
                    if (overLimit) {
                        Spacer(Modifier.height(8.dp))
                        Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(10.dp)) {
                            Text(
                                "Over the ${WorkDraft.BODY_LIMIT} character limit",
                                style = MaterialTheme.typography.bodyMedium,
                                fontSize = 14.sp,
                                color = Ink.Refused,
                            )
                            if (bodyField.text.length > WorkDraft.BODY_LIMIT) {
                                Chip(label = "Split into thread", filled = true, onClick = ::splitBody)
                            }
                        }
                    }
                }
                Spacer(Modifier.height(24.dp))
            }

            // ── Above the keyboard ──
            Column(
                modifier = Modifier
                    .fillMaxWidth()
                    .imePadding()
                    .navigationBarsPadding(),
            ) {
                val refusal = overlay.refusal.ifEmpty { note }
                if (refusal.isNotEmpty()) {
                    HeadsUp(refusal) { note = "" }
                }
                if (mentions.isNotEmpty()) {
                    Row(
                        modifier = Modifier
                            .fillMaxWidth()
                            .horizontalScroll(rememberScrollState())
                            .padding(horizontal = 16.dp, vertical = 6.dp),
                        horizontalArrangement = Arrangement.spacedBy(8.dp),
                    ) {
                        mentions.forEach { mention ->
                            Chip(label = "@${mention.handle}", onClick = {
                                val (text, caret) = completeMention(bodyField.text, bodyField.selection.end, mention.handle)
                                setBody(TextFieldValue(text, TextRange(caret)))
                            })
                        }
                    }
                }
                Row(
                    modifier = Modifier
                        .fillMaxWidth()
                        .horizontalScroll(rememberScrollState())
                        .padding(horizontal = 16.dp, vertical = 8.dp),
                    horizontalArrangement = Arrangement.spacedBy(8.dp),
                ) {
                    Chip(label = replyRuleLabel(draft.replyRule) + " ▾", onClick = { sheet = Sheet.REPLY })
                    val lanes = model.lanes
                    if (lanes.isNotEmpty()) {
                        val laneLabel = lanes.firstOrNull { it.id == draft.lane }?.label
                        Chip(
                            label = if (laneLabel == null) "Add lane · none ▾" else "Lane · $laneLabel ▾",
                            filled = laneLabel != null,
                            onClick = { sheet = Sheet.LANE },
                        )
                    }
                    Chip(
                        label = if (draft.scheduledAt.isEmpty()) "Schedule" else scheduleLabel(draft.scheduledAt) + " ▾",
                        filled = draft.scheduledAt.isNotEmpty(),
                        onClick = { sheet = Sheet.SCHEDULE },
                    )
                    Chip(label = if (draft.nsfw) "18+ ✓" else "18+", filled = draft.nsfw, onClick = { draft = draft.copy(nsfw = !draft.nsfw) })
                }
                Box(Modifier.fillMaxWidth().height(1.dp).background(Ink.Hairline))
                Row(
                    modifier = Modifier
                        .fillMaxWidth()
                        .height(52.dp)
                        .padding(horizontal = 8.dp),
                    verticalAlignment = Alignment.CenterVertically,
                ) {
                    Tool(Icons.Outlined.Image, "photo", enabled = draft.images.size < WorkDraft.MAX_IMAGES) {
                        pickImages.launch(PickVisualMediaRequest(ActivityResultContracts.PickVisualMedia.ImageOnly))
                    }
                    Tool(Icons.Outlined.Videocam, "video", enabled = draft.video == null) {
                        pickVideo.launch(PickVisualMediaRequest(ActivityResultContracts.PickVisualMedia.VideoOnly))
                    }
                    Tool(Icons.Outlined.MusicNote, "track") { model.leaveComposerFor(Wire.MUSIC) }
                    Tool(Icons.Outlined.Article, "article") { model.leaveComposerFor(Wire.ARTICLES) }
                    Tool(Icons.Outlined.AttachMoney, "gate", on = draft.subscriberOnly) { sheet = Sheet.MONETIZE }
                    Spacer(Modifier.width(4.dp))
                    Chip(label = "+ thread", onClick = ::addSegment)
                    Spacer(Modifier.weight(1f))
                    Text(
                        text = "$remaining",
                        style = MaterialTheme.typography.bodyMedium,
                        fontSize = 14.sp,
                        color = if (remaining < 0) Ink.Refused else Ink.Tertiary,
                        modifier = Modifier.padding(end = 8.dp),
                    )
                }
            }
        }
    }

    when (sheet) {
        Sheet.NONE -> Unit
        Sheet.REPLY -> ReplyRuleSheet(
            current = draft.replyRule,
            onPick = { draft = draft.copy(replyRule = it); sheet = Sheet.NONE },
            onDismiss = { sheet = Sheet.NONE },
        )
        Sheet.LANE -> LaneSheet(
            lanes = model.lanes,
            current = draft.lane,
            onPick = { draft = draft.copy(lane = it); sheet = Sheet.NONE },
            onDismiss = { sheet = Sheet.NONE },
        )
        Sheet.SCHEDULE -> ScheduleSheet(
            current = draft.scheduledAt,
            onPick = { draft = draft.copy(scheduledAt = it); sheet = Sheet.NONE },
            onDismiss = { sheet = Sheet.NONE },
        )
        Sheet.MONETIZE -> MonetizeSheet(
            subscriberOnly = draft.subscriberOnly,
            previewFirst = draft.previewFirst,
            isThread = draft.isThread,
            balance = state.balance,
            onApply = { gated, preview ->
                draft = draft.copy(subscriberOnly = gated, previewFirst = preview)
                sheet = Sheet.NONE
            },
            onDismiss = { sheet = Sheet.NONE },
        )
        Sheet.DRAFTS -> DraftsSheet(
            drafts = drafts,
            onOpen = { chosen ->
                sheet = Sheet.NONE
                if (chosen.id == draft.id) return@DraftsSheet
                if (draft.hasContent) model.saveDraft(draft) else model.discardDraft(draft.id)
                model.openComposer(chosen)
            },
            onDelete = { id ->
                model.discardDraft(id)
                if (id == draft.id) saved = false
            },
            onDismiss = { sheet = Sheet.NONE },
        )
    }
}

@Composable
private fun AuthorLine(handle: String, subscriberOnly: Boolean, modifier: Modifier = Modifier, onAudience: () -> Unit) {
    Row(modifier = modifier.padding(bottom = 4.dp), verticalAlignment = Alignment.CenterVertically) {
        Text(
            text = "@$handle",
            style = MaterialTheme.typography.titleMedium,
            fontSize = 15.sp,
            fontWeight = FontWeight.Bold,
            color = Ink.Primary,
        )
        Spacer(Modifier.width(8.dp))
        Chip(label = if (subscriberOnly) "Subscribers ▾" else "Everyone ▾", filled = subscriberOnly, onClick = onAudience)
    }
}

@Composable
private fun BodyField(
    value: TextFieldValue,
    onValue: (TextFieldValue) -> Unit,
    placeholder: String,
    modifier: Modifier = Modifier,
) {
    BasicTextField(
        value = value,
        onValueChange = onValue,
        textStyle = TextStyle(
            color = Ink.Primary,
            fontSize = 16.sp,
            lineHeight = 22.sp,
        ),
        cursorBrush = SolidColor(Ink.Accent),
        visualTransformation = remember { TokenInk(Ink.Accent) },
        modifier = modifier,
        decorationBox = { field ->
            if (value.text.isEmpty()) {
                Text(placeholder, style = MaterialTheme.typography.bodyLarge, fontSize = 16.sp, lineHeight = 22.sp, color = Ink.Muted)
            }
            field()
        },
    )
}

/**
 * The thread as a numbered chain. The row being written is bright, the others are
 * dimmed; long-press the handle to drag a post to another place in the chain.
 */
@Composable
private fun ThreadList(
    posts: List<String>,
    bodyField: TextFieldValue,
    onBody: (TextFieldValue) -> Unit,
    onSegment: (Int, String) -> Unit,
    onRemove: (Int) -> Unit,
    onReorder: (Int, Int) -> Unit,
    focused: Int,
    onFocus: (Int) -> Unit,
    focusOwed: Int,
    onFocusPaid: () -> Unit,
    bodyFocus: FocusRequester,
    avatarUrl: String,
    handle: String,
    gatedFrom: Int,
) {
    var dragging by remember { mutableIntStateOf(-1) }
    var dragOffset by remember { mutableFloatStateOf(0f) }
    val heights = remember { mutableStateMapOf<Int, Int>() }
    val reorder by rememberUpdatedState(onReorder)

    Column {
        posts.forEachIndexed { index, text ->
            val isDragged = dragging == index
            val requester = if (index == 0) bodyFocus else remember { FocusRequester() }
            LaunchedEffect(focusOwed, index) {
                if (focusOwed == index) {
                    requester.requestFocus()
                    onFocusPaid()
                }
            }
            Row(
                modifier = Modifier
                    .fillMaxWidth()
                    .height(IntrinsicSize.Min)
                    .onSizeChanged { heights[index] = it.height }
                    .zIndex(if (isDragged) 1f else 0f)
                    .graphicsLayer { translationY = if (isDragged) dragOffset else 0f }
                    .alpha(if (focused == index || isDragged) 1f else 0.6f),
            ) {
                Column(Modifier.width(40.dp).fillMaxHeight(), horizontalAlignment = Alignment.CenterHorizontally) {
                    PersonaAvatar(avatarUrl, handle, size = 40.dp)
                    if (index < posts.lastIndex) {
                        Box(
                            Modifier
                                .padding(top = 4.dp)
                                .width(1.dp)
                                .weight(1f)
                                .background(Ink.HairlineStrong),
                        )
                    }
                }
                Spacer(Modifier.width(12.dp))
                Column(Modifier.weight(1f).padding(bottom = 16.dp)) {
                    Row(verticalAlignment = Alignment.CenterVertically) {
                        Text(
                            text = "${index + 1}",
                            style = MaterialTheme.typography.labelLarge,
                            fontSize = 13.sp,
                            fontWeight = FontWeight.SemiBold,
                            color = Ink.Tertiary,
                        )
                        if (index >= gatedFrom) {
                            Spacer(Modifier.width(8.dp))
                            Chip(label = "subs only", onClick = {})
                        }
                        Spacer(Modifier.weight(1f))
                        if (posts.size > 1) {
                            Icon(
                                imageVector = Icons.Rounded.DragHandle,
                                contentDescription = "Drag to reorder",
                                tint = if (isDragged) Ink.Accent else Ink.Tertiary,
                                modifier = Modifier
                                    .size(28.dp)
                                    .pointerInput(Unit) {
                                        detectDragGesturesAfterLongPress(
                                            onDragStart = { dragging = index; dragOffset = 0f },
                                            onDragEnd = { dragging = -1; dragOffset = 0f },
                                            onDragCancel = { dragging = -1; dragOffset = 0f },
                                            onDrag = { change, amount ->
                                                change.consume()
                                                val at = dragging
                                                if (at < 0) return@detectDragGesturesAfterLongPress
                                                dragOffset += amount.y
                                                val next = heights[at + 1]
                                                val prev = heights[at - 1]
                                                if (next != null && dragOffset > next / 2f) {
                                                    reorder(at, at + 1)
                                                    dragging = at + 1
                                                    dragOffset -= next
                                                } else if (prev != null && dragOffset < -prev / 2f) {
                                                    reorder(at, at - 1)
                                                    dragging = at - 1
                                                    dragOffset += prev
                                                }
                                            },
                                        )
                                    }
                                    .padding(3.dp),
                            )
                        }
                        if (index > 0) {
                            Icon(
                                imageVector = Icons.Rounded.Close,
                                contentDescription = "Remove this post",
                                tint = Ink.Tertiary,
                                modifier = Modifier
                                    .size(28.dp)
                                    .clip(CircleShape)
                                    .clickable { onRemove(index) }
                                    .padding(5.dp),
                            )
                        }
                    }
                    if (index == 0) {
                        BodyField(
                            value = bodyField,
                            onValue = onBody,
                            placeholder = "What's happening?",
                            modifier = Modifier
                                .fillMaxWidth()
                                .focusRequester(bodyFocus)
                                .onFocusChanged { if (it.isFocused) onFocus(0) },
                        )
                    } else {
                        BasicTextField(
                            value = text,
                            onValueChange = { onSegment(index, it) },
                            textStyle = TextStyle(
                                color = Ink.Primary,
                                fontSize = 16.sp,
                                lineHeight = 22.sp,
                            ),
                            cursorBrush = SolidColor(Ink.Accent),
                            visualTransformation = remember { TokenInk(Ink.Accent) },
                            modifier = Modifier
                                .fillMaxWidth()
                                .focusRequester(requester)
                                .onFocusChanged { if (it.isFocused) onFocus(index) },
                            decorationBox = { field ->
                                if (text.isEmpty()) {
                                    Text("Continue your thread…", style = MaterialTheme.typography.bodyLarge, fontSize = 16.sp, lineHeight = 22.sp, color = Ink.Muted)
                                }
                                field()
                            },
                        )
                    }
                }
            }
        }
    }
}

@Composable
private fun MediaStrip(
    origin: String,
    images: List<String>,
    video: VideoAttachment?,
    onRemoveImage: (String) -> Unit,
    onRemoveVideo: () -> Unit,
) {
    Row(
        modifier = Modifier.horizontalScroll(rememberScrollState()),
        horizontalArrangement = Arrangement.spacedBy(8.dp),
    ) {
        images.forEach { url ->
            Tile(onRemove = { onRemoveImage(url) }) {
                AsyncImage(
                    model = absolute(origin, url),
                    contentDescription = "Attached photo",
                    contentScale = ContentScale.Crop,
                    modifier = Modifier.fillMaxSize(),
                )
            }
        }
        video?.let { clip ->
            Tile(onRemove = onRemoveVideo) {
                if (clip.posterUrl.isNotEmpty()) {
                    AsyncImage(
                        model = absolute(origin, clip.posterUrl),
                        contentDescription = "Attached clip",
                        contentScale = ContentScale.Crop,
                        modifier = Modifier.fillMaxSize(),
                    )
                } else {
                    Box(Modifier.fillMaxSize().background(Ink.Elevated), contentAlignment = Alignment.Center) {
                        Icon(Icons.Rounded.Videocam, contentDescription = "Attached clip", tint = Ink.Tertiary)
                    }
                }
                if (clip.durationSecs > 0) {
                    Text(
                        text = "%d:%02d".format(clip.durationSecs / 60, clip.durationSecs % 60),
                        style = MaterialTheme.typography.labelLarge,
                        fontSize = 11.sp,
                        fontWeight = FontWeight.Bold,
                        color = Color.White,
                        modifier = Modifier
                            .align(Alignment.BottomStart)
                            .padding(6.dp)
                            .clip(CircleShape)
                            .background(OVER_MEDIA)
                            .padding(horizontal = 7.dp, vertical = 2.dp),
                    )
                }
            }
        }
    }
}

@Composable
private fun Tile(onRemove: () -> Unit, content: @Composable androidx.compose.foundation.layout.BoxScope.() -> Unit) {
    Box(
        modifier = Modifier
            .size(104.dp)
            .clip(RoundedCornerShape(12.dp))
            .background(Ink.Elevated),
    ) {
        content()
        Icon(
            imageVector = Icons.Rounded.Close,
            contentDescription = "Remove",
            tint = Color.White,
            modifier = Modifier
                .align(Alignment.TopEnd)
                .padding(4.dp)
                .size(24.dp)
                .clip(CircleShape)
                .background(OVER_MEDIA)
                .clickable(onClick = onRemove)
                .padding(4.dp),
        )
    }
}

/** The quoted work, as the server rendered it; nothing about it is assembled here. */
@Composable
private fun QuoteCard(session: Session, state: ShellState, fragment: Fragment?, onRemove: () -> Unit) {
    Box(
        modifier = Modifier
            .fillMaxWidth()
            .clip(RoundedCornerShape(16.dp))
            .border(1.dp, Ink.Hairline, RoundedCornerShape(16.dp))
            .background(Ink.Elevated),
    ) {
        if (fragment == null) {
            Text(
                "Loading the quoted work…",
                style = MaterialTheme.typography.bodyMedium,
                fontSize = 14.sp,
                color = Ink.Tertiary,
                modifier = Modifier.padding(16.dp),
            )
        } else {
            FragmentPane(
                session = session,
                identity = state.identity,
                fragment = fragment,
                modifier = Modifier.fillMaxWidth().height(180.dp),
            )
        }
        Icon(
            imageVector = Icons.Rounded.Close,
            contentDescription = "Remove the quote",
            tint = Ink.Secondary,
            modifier = Modifier
                .align(Alignment.TopEnd)
                .padding(6.dp)
                .size(28.dp)
                .clip(CircleShape)
                .background(Ink.Elevated)
                .border(1.dp, Ink.Hairline, CircleShape)
                .clickable(onClick = onRemove)
                .padding(5.dp),
        )
    }
}

/** A quiet word from the server — a refusal, an upload that did not take. */
@Composable
private fun HeadsUp(text: String, onDismiss: () -> Unit) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .padding(horizontal = 16.dp, vertical = 6.dp)
            .clip(RoundedCornerShape(12.dp))
            .border(1.dp, Ink.Hairline, RoundedCornerShape(12.dp))
            .background(Ink.Elevated)
            .padding(horizontal = 12.dp, vertical = 9.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Text(text, style = MaterialTheme.typography.bodyMedium, fontSize = 14.sp, color = Ink.Refused, modifier = Modifier.weight(1f))
        Icon(
            imageVector = Icons.Rounded.Close,
            contentDescription = "Dismiss",
            tint = Ink.Tertiary,
            modifier = Modifier
                .size(24.dp)
                .clip(CircleShape)
                .clickable(onClick = onDismiss)
                .padding(3.dp),
        )
    }
}

@Composable
private fun Tool(
    icon: ImageVector,
    label: String,
    enabled: Boolean = true,
    on: Boolean = false,
    onClick: () -> Unit,
) {
    Box(
        modifier = Modifier
            .size(44.dp)
            .clip(CircleShape)
            .clickable(enabled = enabled, onClick = onClick),
        contentAlignment = Alignment.Center,
    ) {
        Icon(
            imageVector = icon,
            contentDescription = label,
            tint = when {
                !enabled -> Ink.Faint
                on -> Ink.Accent
                else -> Ink.Secondary
            },
            modifier = Modifier.size(22.dp),
        )
    }
}

private fun absolute(origin: String, url: String): String =
    if (url.startsWith("http://") || url.startsWith("https://")) url else origin.trimEnd('/') + url
