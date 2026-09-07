package com.f33d3r.app.ui.shell

import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.interaction.MutableInteractionSource
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.offset
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.ChatBubbleOutline
import androidx.compose.material.icons.outlined.Home
import androidx.compose.material.icons.outlined.Notifications
import androidx.compose.material.icons.outlined.Search
import androidx.compose.material.icons.rounded.ChatBubble
import androidx.compose.material.icons.rounded.Home
import androidx.compose.material.icons.rounded.Notifications
import androidx.compose.material.icons.rounded.Search
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.remember
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import com.f33d3r.app.core.Wire
import com.f33d3r.app.ui.theme.Ink

/** One destination in the bottom rail, and the two glyphs that say whether it is open. */
private data class Rail(
    val wire: Wire,
    val label: String,
    val idle: ImageVector,
    val active: ImageVector,
)

/**
 * Home, Explore, Visions, Notifications, Messages: the five places a thumb goes
 * without thinking. Visions holds the middle because it is the surface no other
 * network has; the wallet and everything else the persona owns are in the drawer.
 */
private val RAILS = listOf(
    Rail(Wire.PLAYGROUND, "Home", Icons.Outlined.Home, Icons.Rounded.Home),
    Rail(Wire.EXPLORE, "Explore", Icons.Outlined.Search, Icons.Rounded.Search),
    Rail(Wire.VISIONS, "Visions", VisionMark, VisionMarkFilled),
    Rail(Wire.NOTIFICATIONS, "Notifications", Icons.Outlined.Notifications, Icons.Rounded.Notifications),
    Rail(Wire.CHAT, "Messages", Icons.Outlined.ChatBubbleOutline, Icons.Rounded.ChatBubble),
)

/** The wires each rail glyph stands for when one of its inner pages is open. */
private fun railFor(wire: Wire): Wire = when (wire) {
    Wire.SEARCH, Wire.TAG, Wire.STOCKS, Wire.SPORTS, Wire.ARTICLES, Wire.ARTICLE, Wire.MARKETPLACE -> Wire.EXPLORE
    Wire.THREAD -> Wire.CHAT
    else -> wire
}

/**
 * The bottom rail: five glyphs on a hairline, the open one filled in.
 *
 * No labels. The glyphs are the ones every phone already taught — a house, a lens, a
 * bell, a bubble — and the fifth is the Vision mark the top of the timeline repeats.
 * All five are drawn in the one ink; the open one is the solid variant of its
 * outline, which is the whole of the rail's selected state. The rail carries the
 * three counts the server pushes for its destinations: works held back on the
 * timeline, unread notifications, unread messages.
 */
@Composable
fun BottomNav(
    current: Wire,
    pendingWorks: Int,
    notificationBadge: String,
    messageBadge: Int,
    onSelect: (Wire) -> Unit,
    modifier: Modifier = Modifier,
) {
    val open = railFor(current)
    Column(modifier = modifier.fillMaxWidth().background(Ink.Surface)) {
        Box(Modifier.fillMaxWidth().height(1.dp).background(Ink.Hairline))
        Row(
            modifier = Modifier
                .fillMaxWidth()
                .navigationBarsPadding()
                .height(RAIL_HEIGHT),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            RAILS.forEach { rail ->
                val selected = open == rail.wire
                Box(
                    modifier = Modifier
                        .weight(1f)
                        .fillMaxWidth()
                        .height(RAIL_HEIGHT)
                        .clickable(
                            interactionSource = remember { MutableInteractionSource() },
                            indication = null,
                        ) { onSelect(rail.wire) },
                    contentAlignment = Alignment.Center,
                ) {
                    Icon(
                        imageVector = if (selected) rail.active else rail.idle,
                        contentDescription = rail.label,
                        tint = Ink.Primary,
                        modifier = Modifier.size(BAR_GLYPH),
                    )
                    when (rail.wire) {
                        Wire.PLAYGROUND -> if (pendingWorks > 0 && !selected) Dot()
                        Wire.NOTIFICATIONS -> if (notificationBadge.isNotEmpty()) Count(notificationBadge)
                        Wire.CHAT -> if (messageBadge > 0) Count(messageBadge.coerceAtMost(99).toString())
                        else -> Unit
                    }
                }
            }
        }
    }
}

/** Something new behind this glyph, without saying how much. */
@Composable
private fun Dot() {
    Box(
        modifier = Modifier
            .offset(x = 8.dp, y = (-8).dp)
            .size(8.dp)
            .clip(CircleShape)
            .background(Ink.Accent),
    )
}

/** The count the server pushed for this glyph: 11 bold on the accent, sat on the glyph's shoulder. */
@Composable
private fun Count(text: String) {
    Box(
        modifier = Modifier
            .offset(x = 10.dp, y = (-9).dp)
            .height(16.dp)
            .widthIn(min = 16.dp)
            .clip(Pill)
            .background(Ink.Accent)
            .padding(horizontal = 4.dp),
        contentAlignment = Alignment.Center,
    ) {
        Text(
            text = text,
            style = MaterialTheme.typography.labelSmall,
            fontSize = 11.sp,
            fontWeight = FontWeight.Bold,
            color = Ink.OnAccent,
            maxLines = 1,
        )
    }
}
