package com.f33d3r.app.ui.compose

import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ColumnScope
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.BasicTextField
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.rounded.Check
import androidx.compose.material3.BottomSheetDefaults
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.ModalBottomSheet
import androidx.compose.material3.Text
import androidx.compose.material3.rememberModalBottomSheetState
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.SolidColor
import androidx.compose.ui.text.TextStyle
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import com.f33d3r.app.ui.theme.Ink

/**
 * The one grammar every sheet in the Shell is written in.
 *
 * A raised surface with a 28 radius at the top and the standard handle; a 20 bold
 * title with an optional 14 line under it; 52-tall option rows that state their
 * choice with an accent check; a full-width 48 accent pill for the one primary
 * action; a plain word to cancel. The tip sheet, the new-message sheet, the go-live
 * panel and the composer's own sheets all draw from here, which is what makes them
 * read as one product rather than five.
 */

/** The sheet's top radius. */
internal val SHEET_RADIUS = 28.dp

/** A modal sheet on [Ink.Elevated], with the frame every sheet shares. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
internal fun ShellSheet(onDismiss: () -> Unit, content: @Composable ColumnScope.() -> Unit) {
    val sheet = rememberModalBottomSheetState(skipPartiallyExpanded = true)
    ModalBottomSheet(
        onDismissRequest = onDismiss,
        sheetState = sheet,
        shape = RoundedCornerShape(topStart = SHEET_RADIUS, topEnd = SHEET_RADIUS),
        containerColor = Ink.Elevated,
        contentColor = Ink.Primary,
        dragHandle = { SheetHandle() },
    ) {
        Column(
            modifier = Modifier
                .fillMaxWidth()
                .imePadding()
                .navigationBarsPadding()
                .padding(horizontal = 16.dp),
        ) {
            content()
            Spacer(Modifier.height(20.dp))
        }
    }
}

/** The standard drag handle, in the palette's faint ink. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
internal fun SheetHandle() {
    BottomSheetDefaults.DragHandle(color = Ink.Faint)
}

/**
 * The sheet's frame for a sheet that is not modal — the go-live panel sits over the
 * camera on its own, with the same surface, radius and handle as every other sheet.
 */
@Composable
internal fun SheetPanel(modifier: Modifier = Modifier, content: @Composable ColumnScope.() -> Unit) {
    Column(
        modifier = modifier
            .fillMaxWidth()
            .clip(RoundedCornerShape(topStart = SHEET_RADIUS, topEnd = SHEET_RADIUS))
            .background(Ink.Elevated),
        horizontalAlignment = Alignment.CenterHorizontally,
    ) {
        SheetHandle()
        Column(Modifier.fillMaxWidth().padding(horizontal = 16.dp)) {
            content()
            Spacer(Modifier.height(20.dp))
        }
    }
}

/** 20 bold, and under it, when there is one, a 14 line in tertiary ink. */
@Composable
internal fun SheetTitle(text: String, subtitle: String = "") {
    Text(
        text = text,
        style = MaterialTheme.typography.titleLarge,
        fontSize = 20.sp,
        fontWeight = FontWeight.Bold,
        color = Ink.Primary,
    )
    if (subtitle.isNotEmpty()) {
        Spacer(Modifier.height(4.dp))
        Text(
            text = subtitle,
            style = MaterialTheme.typography.bodyMedium,
            fontSize = 14.sp,
            color = Ink.Tertiary,
        )
    }
    Spacer(Modifier.height(12.dp))
}

/** A 52-tall row naming one choice; the chosen one carries an accent check. */
@Composable
internal fun OptionRow(label: String, detail: String, selected: Boolean, onClick: () -> Unit) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .heightIn(min = 52.dp)
            .clickable(onClick = onClick)
            .padding(vertical = 8.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Column(Modifier.weight(1f)) {
            Text(
                text = label,
                style = MaterialTheme.typography.bodyLarge,
                fontSize = 16.sp,
                color = Ink.Primary,
                maxLines = 1,
                overflow = TextOverflow.Ellipsis,
            )
            if (detail.isNotEmpty()) {
                Text(
                    text = detail,
                    style = MaterialTheme.typography.bodyMedium,
                    fontSize = 14.sp,
                    color = Ink.Tertiary,
                    maxLines = 1,
                    overflow = TextOverflow.Ellipsis,
                )
            }
        }
        if (selected) {
            Spacer(Modifier.width(12.dp))
            Icon(
                imageVector = Icons.Rounded.Check,
                contentDescription = "Selected",
                tint = Ink.Accent,
                modifier = Modifier.size(22.dp),
            )
        }
    }
}

/** The one primary action: a full-width 48 accent pill. */
@Composable
internal fun ApplyButton(label: String, enabled: Boolean = true, onClick: () -> Unit) {
    Box(
        modifier = Modifier
            .fillMaxWidth()
            .height(48.dp)
            .clip(CircleShape)
            .background(if (enabled) Ink.Accent else Ink.Faint)
            .clickable(enabled = enabled, onClick = onClick),
        contentAlignment = Alignment.Center,
    ) {
        Text(
            text = label,
            style = MaterialTheme.typography.labelLarge,
            fontSize = 15.sp,
            fontWeight = FontWeight.Bold,
            color = if (enabled) Ink.OnAccent else Ink.Muted,
        )
    }
}

/** The way out: a plain word, no frame, centred under the primary action. */
@Composable
internal fun SheetCancel(label: String = "Cancel", onClick: () -> Unit) {
    Box(
        modifier = Modifier
            .fillMaxWidth()
            .height(44.dp)
            .clip(CircleShape)
            .clickable(onClick = onClick),
        contentAlignment = Alignment.Center,
    ) {
        Text(
            text = label,
            style = MaterialTheme.typography.labelLarge,
            fontSize = 15.sp,
            fontWeight = FontWeight.SemiBold,
            color = Ink.Primary,
        )
    }
}

/** A hairline-edged 48-tall field on the sheet's own surface. */
@Composable
internal fun SheetField(
    value: String,
    onValue: (String) -> Unit,
    placeholder: String,
    keyboard: KeyboardOptions = KeyboardOptions.Default,
    singleLine: Boolean = true,
    enabled: Boolean = true,
    trailing: @Composable (() -> Unit)? = null,
) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .heightIn(min = 48.dp)
            .clip(RoundedCornerShape(12.dp))
            .background(Ink.Elevated)
            .border(1.dp, Ink.HairlineStrong, RoundedCornerShape(12.dp))
            .padding(horizontal = 14.dp, vertical = 12.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        BasicTextField(
            value = value,
            onValueChange = onValue,
            singleLine = singleLine,
            enabled = enabled,
            keyboardOptions = keyboard,
            textStyle = TextStyle(color = Ink.Primary, fontSize = 16.sp, lineHeight = 22.sp),
            cursorBrush = SolidColor(Ink.Accent),
            modifier = Modifier.weight(1f),
            decorationBox = { field ->
                if (value.isEmpty()) {
                    Text(
                        text = placeholder,
                        style = MaterialTheme.typography.bodyLarge,
                        fontSize = 16.sp,
                        color = Ink.Muted,
                        maxLines = 1,
                        overflow = TextOverflow.Ellipsis,
                    )
                }
                field()
            },
        )
        if (trailing != null) {
            Spacer(Modifier.width(8.dp))
            trailing()
        }
    }
}

/** A 1px rule between rows on a sheet. */
@Composable
internal fun SheetRule() {
    Box(Modifier.fillMaxWidth().height(1.dp).background(Ink.Hairline))
}
