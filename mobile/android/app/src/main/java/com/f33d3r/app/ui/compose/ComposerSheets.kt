package com.f33d3r.app.ui.compose

import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.Delete
import androidx.compose.material3.DatePicker
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.material3.TimeInput
import androidx.compose.material3.rememberDatePickerState
import androidx.compose.material3.rememberTimePickerState
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import com.f33d3r.app.compose.WorkDraft
import com.f33d3r.app.ui.SurfaceTab
import com.f33d3r.app.ui.theme.Ink
import java.time.Instant
import java.time.LocalDate
import java.time.LocalTime
import java.time.OffsetDateTime
import java.time.ZoneId
import java.time.ZoneOffset
import java.time.ZonedDateTime
import java.time.format.DateTimeFormatter

/** The reply rules the server offers, in the server's words and the composer's. */
internal val REPLY_RULES = listOf(
    WorkDraft.REPLY_EVERYONE to "Everyone can reply",
    WorkDraft.REPLY_FOLLOWERS to "Followers can reply",
    WorkDraft.REPLY_CIRCLE to "Close circle can reply",
    WorkDraft.REPLY_NONE to "No replies",
)

internal fun replyRuleLabel(rule: String): String =
    REPLY_RULES.firstOrNull { it.first == rule }?.second ?: REPLY_RULES.first().second

/** Who may reply. */
@Composable
fun ReplyRuleSheet(current: String, onPick: (String) -> Unit, onDismiss: () -> Unit) {
    ShellSheet(onDismiss) {
        SheetTitle("Who can reply")
        REPLY_RULES.forEach { (rule, label) ->
            OptionRow(label = label, detail = "", selected = rule == current) { onPick(rule) }
        }
    }
}

/** Which pinned interest lane the work is tagged into, if any. */
@Composable
fun LaneSheet(lanes: List<SurfaceTab>, current: String, onPick: (String) -> Unit, onDismiss: () -> Unit) {
    ShellSheet(onDismiss) {
        SheetTitle(
            "Lane",
            subtitle = "A lane tag decides where the work shows on the timeline. Hashtags in the words count too.",
        )
        OptionRow(label = "None", detail = "Following and For you only", selected = current.isEmpty()) { onPick("") }
        lanes.forEach { lane ->
            OptionRow(label = lane.label, detail = "#${lane.id}", selected = lane.id == current) { onPick(lane.id) }
        }
    }
}

/**
 * When to post. The time is kept as the server takes it — RFC 3339 with the
 * device's offset — and shown back in the device's own zone.
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun ScheduleSheet(current: String, onPick: (String) -> Unit, onDismiss: () -> Unit) {
    val zone = ZoneId.systemDefault()
    val existing = parseSchedule(current)?.withZoneSameInstant(zone)
    val dateState = rememberDatePickerState(
        initialSelectedDateMillis = (existing?.toLocalDate() ?: LocalDate.now(zone).plusDays(1))
            .atStartOfDay(ZoneOffset.UTC).toInstant().toEpochMilli(),
    )
    val timeState = rememberTimePickerState(
        initialHour = existing?.hour ?: 9,
        initialMinute = existing?.minute ?: 0,
        is24Hour = false,
    )
    var complaint by remember { mutableStateOf("") }

    ShellSheet(onDismiss) {
        Column(Modifier.verticalScroll(rememberScrollState())) {
            SheetTitle("Schedule", subtitle = "Posted at this time, in this phone's zone.")
            DatePicker(state = dateState, showModeToggle = false, title = null, headline = null)
            Spacer(Modifier.height(8.dp))
            Box(Modifier.fillMaxWidth(), contentAlignment = Alignment.Center) { TimeInput(state = timeState) }
            if (complaint.isNotEmpty()) {
                Text(complaint, style = MaterialTheme.typography.bodyMedium, fontSize = 14.sp, color = Ink.Refused)
                Spacer(Modifier.height(8.dp))
            }
            Spacer(Modifier.height(8.dp))
            ApplyButton("Apply") {
                val millis = dateState.selectedDateMillis
                if (millis == null) {
                    complaint = "Pick a day."
                    return@ApplyButton
                }
                val day = Instant.ofEpochMilli(millis).atZone(ZoneOffset.UTC).toLocalDate()
                val at = ZonedDateTime.of(day, LocalTime.of(timeState.hour, timeState.minute), zone)
                if (!at.isAfter(ZonedDateTime.now(zone).plusMinutes(1))) {
                    complaint = "Pick a time in the future."
                    return@ApplyButton
                }
                onPick(at.toOffsetDateTime().format(DateTimeFormatter.ISO_OFFSET_DATE_TIME))
            }
            if (current.isNotEmpty()) {
                Spacer(Modifier.height(4.dp))
                SheetCancel("Post now instead") { onPick("") }
            }
        }
    }
}

internal fun parseSchedule(value: String): ZonedDateTime? =
    runCatching { OffsetDateTime.parse(value).toZonedDateTime() }.getOrNull()

/** "Sep 7 · 9:30 AM", in the device's zone. */
internal fun scheduleLabel(value: String): String =
    parseSchedule(value)?.withZoneSameInstant(ZoneId.systemDefault())
        ?.format(DateTimeFormatter.ofPattern("MMM d · h:mm a")) ?: "Schedule"

/**
 * The one gate the server offers a work — subscribers only — and, for a thread,
 * whether the first post stays free as a preview. Money is never touched here:
 * what a subscription costs is the creator's shop page, and what has been earned is
 * the wallet the server keeps.
 */
@Composable
fun MonetizeSheet(
    subscriberOnly: Boolean,
    previewFirst: Boolean,
    isThread: Boolean,
    balance: String,
    onApply: (subscriberOnly: Boolean, previewFirst: Boolean) -> Unit,
    onDismiss: () -> Unit,
) {
    var gated by remember { mutableStateOf(subscriberOnly) }
    var preview by remember { mutableStateOf(previewFirst) }
    ShellSheet(onDismiss) {
        SheetTitle(
            if (isThread) "Monetize this thread" else "Monetize this post",
            subtitle = buildString {
                append("Subscribers pay you directly, into your wallet.")
                if (balance.isNotEmpty()) append(" Wallet · ").append(balance)
            },
        )
        OptionRow(label = "Free", detail = "Everyone can read it", selected = !gated) { gated = false }
        OptionRow(label = "Subscribers only", detail = "Tips stay on either way", selected = gated) { gated = true }
        if (isThread && gated) {
            SheetRule()
            OptionRow(
                label = "First post free as a preview",
                detail = if (preview) "The rest is gated" else "Every post is gated",
                selected = preview,
            ) { preview = !preview }
        }
        Spacer(Modifier.height(16.dp))
        ApplyButton("Apply") { onApply(gated, gated && preview) }
        SheetCancel(onClick = onDismiss)
    }
}

/** The drafts this device holds; tapping one opens it, the bin removes it. */
@Composable
fun DraftsSheet(
    drafts: List<WorkDraft>,
    onOpen: (WorkDraft) -> Unit,
    onDelete: (String) -> Unit,
    onDismiss: () -> Unit,
) {
    ShellSheet(onDismiss) {
        SheetTitle("Drafts", subtitle = "Kept on this phone, not on the server.")
        if (drafts.isEmpty()) {
            Text("Nothing kept on this phone.", style = MaterialTheme.typography.bodyMedium, fontSize = 14.sp, color = Ink.Tertiary)
        }
        Column(Modifier.verticalScroll(rememberScrollState())) {
            drafts.forEachIndexed { index, draft ->
                if (index > 0) SheetRule()
                Row(
                    modifier = Modifier
                        .fillMaxWidth()
                        .heightIn(min = 52.dp)
                        .clickable { onOpen(draft) }
                        .padding(vertical = 12.dp),
                    verticalAlignment = Alignment.CenterVertically,
                ) {
                    Column(Modifier.weight(1f)) {
                        Text(
                            text = draftPreview(draft),
                            style = MaterialTheme.typography.bodyLarge,
                            fontSize = 16.sp,
                            lineHeight = 22.sp,
                            color = Ink.Primary,
                            maxLines = 2,
                            overflow = TextOverflow.Ellipsis,
                        )
                        Text(
                            text = draftDetail(draft),
                            style = MaterialTheme.typography.bodyMedium,
                            fontSize = 14.sp,
                            color = Ink.Tertiary,
                            maxLines = 1,
                            overflow = TextOverflow.Ellipsis,
                        )
                    }
                    Spacer(Modifier.width(12.dp))
                    Icon(
                        imageVector = Icons.Outlined.Delete,
                        contentDescription = "Delete draft",
                        tint = Ink.Tertiary,
                        modifier = Modifier
                            .size(44.dp)
                            .clip(CircleShape)
                            .clickable { onDelete(draft.id) }
                            .padding(11.dp),
                    )
                }
            }
        }
    }
}

private fun draftPreview(draft: WorkDraft): String {
    val words = draft.body.trim().ifEmpty { draft.segments.firstOrNull { it.isNotBlank() }?.trim().orEmpty() }
    return when {
        words.isNotEmpty() -> words
        draft.video != null -> "A clip"
        draft.images.isNotEmpty() -> "${draft.images.size} photo${if (draft.images.size == 1) "" else "s"}"
        else -> "Empty"
    }
}

private fun draftDetail(draft: WorkDraft): String {
    val parts = mutableListOf<String>()
    if (draft.isThread) parts += "thread · ${draft.segments.size + 1}"
    if (draft.images.isNotEmpty()) parts += "${draft.images.size} photo${if (draft.images.size == 1) "" else "s"}"
    if (draft.video != null) parts += "clip"
    if (draft.replyTo != null) parts += "reply to @${draft.replyTo.handle}"
    if (draft.quoteWorkId.isNotEmpty()) parts += "quote"
    if (draft.scheduledAt.isNotEmpty()) parts += scheduleLabel(draft.scheduledAt)
    parts += savedAgo(draft.savedAt)
    return parts.joinToString(" · ")
}

private fun savedAgo(savedAt: Long): String {
    if (savedAt <= 0) return "unsaved"
    val minutes = (System.currentTimeMillis() - savedAt) / 60_000
    return when {
        minutes < 1 -> "just now"
        minutes < 60 -> "${minutes}m ago"
        minutes < 60 * 24 -> "${minutes / 60}h ago"
        else -> "${minutes / (60 * 24)}d ago"
    }
}
