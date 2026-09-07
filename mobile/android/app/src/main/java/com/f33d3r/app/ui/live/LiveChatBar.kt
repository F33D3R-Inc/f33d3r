package com.f33d3r.app.ui.live

import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.BasicTextField
import androidx.compose.foundation.text.KeyboardActions
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.SolidColor
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.text.TextStyle
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp

/*
 * The HUD over a picture — a live stream, a Vision, the camera — is the one place
 * the chrome draws a colour that is not an ink: black at 55% under white, so the
 * controls read on any frame in either mode. Nothing here reaches for a third hue.
 */

/** Black at 55%: the ground of every pill drawn over a picture. */
internal val HUD_GROUND = Color(0x8C000000)

/** White at 60%: a placeholder over a picture. */
internal val HUD_PLACEHOLDER = Color(0x99FFFFFF)

/** White at 12%: a pill's fill where the ground under it is already black. */
internal val HUD_LIFT = Color(0x1FFFFFFF)

/**
 * "Say something…" over a live picture, with whatever the surface puts beside it:
 * the viewer's quick-tip chips, the broadcaster's End control.
 *
 * The line goes to the server; the server renders it and the log receives the render.
 * Nothing is drawn into the log from here.
 */
@Composable
fun LiveChatBar(
    onSend: (String) -> Unit,
    modifier: Modifier = Modifier,
    trailing: @Composable () -> Unit = {},
) {
    var draft by remember { mutableStateOf("") }
    fun send() {
        val text = draft.trim()
        if (text.isEmpty()) return
        onSend(text)
        draft = ""
    }
    Row(
        modifier = modifier.fillMaxWidth().padding(horizontal = 16.dp),
        verticalAlignment = Alignment.CenterVertically,
        horizontalArrangement = Arrangement.spacedBy(8.dp),
    ) {
        HudField(
            value = draft,
            onValue = { if (it.length <= 280) draft = it },
            placeholder = "Say something…",
            onSend = ::send,
            modifier = Modifier.weight(1f),
        )
        trailing()
    }
}

/** A quick-tip chip: one tap, one amount, no wallet detour. */
@Composable
fun QuickTipChip(amount: Int, onClick: () -> Unit) {
    HudChip(label = "$amount", onClick = onClick)
}

/**
 * A 48-tall line of text over a picture: white on black at 55%, a bold white Send
 * once there is something to send. The keyboard's own Send does the same.
 */
@Composable
internal fun HudField(
    value: String,
    onValue: (String) -> Unit,
    placeholder: String,
    onSend: (() -> Unit)? = null,
    modifier: Modifier = Modifier,
    enabled: Boolean = true,
    singleLine: Boolean = true,
    ground: Color = HUD_GROUND,
) {
    Row(
        modifier = modifier
            .then(if (singleLine) Modifier.height(48.dp) else Modifier.heightIn(min = 48.dp))
            .clip(if (singleLine) CircleShape else RoundedCornerShape(16.dp))
            .background(ground)
            .padding(horizontal = 16.dp, vertical = if (singleLine) 0.dp else 12.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        BasicTextField(
            value = value,
            onValueChange = onValue,
            singleLine = singleLine,
            enabled = enabled,
            textStyle = TextStyle(color = Color.White, fontSize = 16.sp, lineHeight = 22.sp),
            cursorBrush = SolidColor(Color.White),
            keyboardOptions = KeyboardOptions(imeAction = if (onSend != null) ImeAction.Send else ImeAction.Default),
            keyboardActions = KeyboardActions(onSend = { onSend?.invoke() }),
            modifier = Modifier.weight(1f),
            decorationBox = { field ->
                if (value.isEmpty()) {
                    Text(
                        text = placeholder,
                        style = MaterialTheme.typography.bodyLarge,
                        fontSize = 16.sp,
                        color = HUD_PLACEHOLDER,
                        maxLines = 1,
                        overflow = TextOverflow.Ellipsis,
                    )
                }
                field()
            },
        )
        if (onSend != null && value.isNotBlank()) {
            Spacer(Modifier.width(8.dp))
            Text(
                text = "Send",
                style = MaterialTheme.typography.labelLarge,
                fontSize = 15.sp,
                fontWeight = FontWeight.Bold,
                color = Color.White,
                modifier = Modifier
                    .clip(CircleShape)
                    .clickable(onClick = onSend)
                    .padding(horizontal = 4.dp, vertical = 8.dp),
            )
        }
    }
}

/**
 * A 32-tall pill over a picture: white 13 semibold on black at 55%. Filled, it
 * inverts — black on white — which is how a chosen mode states itself on a frame.
 */
@Composable
internal fun HudChip(
    label: String,
    onClick: () -> Unit,
    filled: Boolean = false,
    tint: Color = Color.White,
    modifier: Modifier = Modifier,
) {
    Box(
        modifier = modifier
            .height(32.dp)
            .clip(CircleShape)
            .background(if (filled) Color.White else HUD_GROUND)
            .clickable(onClick = onClick)
            .padding(horizontal = 12.dp),
        contentAlignment = Alignment.Center,
    ) {
        Text(
            text = label,
            style = MaterialTheme.typography.labelLarge,
            fontSize = 13.sp,
            fontWeight = FontWeight.SemiBold,
            color = if (filled) Color.Black else tint,
            maxLines = 1,
        )
    }
}

/** A word over a picture that is not a control: the same pill, nothing to tap. */
@Composable
internal fun HudPill(label: String, modifier: Modifier = Modifier, tint: Color = Color.White, ground: Color = HUD_GROUND) {
    Box(
        modifier = modifier
            .height(32.dp)
            .clip(CircleShape)
            .background(ground)
            .padding(horizontal = 12.dp),
        contentAlignment = Alignment.Center,
    ) {
        Text(
            text = label,
            style = MaterialTheme.typography.labelLarge,
            fontSize = 13.sp,
            fontWeight = FontWeight.SemiBold,
            color = tint,
            maxLines = 1,
        )
    }
}

/** A 32 round glyph over a picture — the close, the more — on black at 55%. */
@Composable
internal fun HudGlyph(
    icon: ImageVector,
    contentDescription: String,
    onClick: () -> Unit,
    modifier: Modifier = Modifier,
    size: androidx.compose.ui.unit.Dp = 32.dp,
    ground: Color = HUD_GROUND,
    tint: Color = Color.White,
) {
    Box(
        modifier = modifier
            .size(size)
            .clip(CircleShape)
            .background(ground)
            .clickable(onClick = onClick),
        contentAlignment = Alignment.Center,
    ) {
        Icon(imageVector = icon, contentDescription = contentDescription, tint = tint, modifier = Modifier.size(20.dp))
    }
}

/** The one primary action over a picture: a white pill, black bold 15, 40 tall. */
@Composable
internal fun HudButton(label: String, enabled: Boolean = true, height: androidx.compose.ui.unit.Dp = 40.dp, onClick: () -> Unit) {
    Box(
        modifier = Modifier
            .height(height)
            .clip(CircleShape)
            .background(if (enabled) Color.White else HUD_LIFT)
            .clickable(enabled = enabled, onClick = onClick)
            .padding(horizontal = 20.dp),
        contentAlignment = Alignment.Center,
    ) {
        Text(
            text = label,
            style = MaterialTheme.typography.labelLarge,
            fontSize = 15.sp,
            fontWeight = FontWeight.Bold,
            color = if (enabled) Color.Black else HUD_PLACEHOLDER,
        )
    }
}
