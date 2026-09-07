package com.f33d3r.app.ui.shell

import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.statusBarsPadding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.outlined.Article
import androidx.compose.material.icons.automirrored.outlined.ListAlt
import androidx.compose.material.icons.outlined.BarChart
import androidx.compose.material.icons.outlined.Bookmark
import androidx.compose.material.icons.outlined.DarkMode
import androidx.compose.material.icons.outlined.Domain
import androidx.compose.material.icons.outlined.Edit
import androidx.compose.material.icons.outlined.EmojiEvents
import androidx.compose.material.icons.outlined.MusicNote
import androidx.compose.material.icons.outlined.Person
import androidx.compose.material.icons.outlined.Settings
import androidx.compose.material.icons.outlined.Shield
import androidx.compose.material.icons.outlined.Storefront
import androidx.compose.material.icons.outlined.ViewAgenda
import androidx.compose.material.icons.outlined.Videocam
import androidx.compose.material.icons.outlined.Wallet
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import com.f33d3r.app.core.Wire
import com.f33d3r.app.net.Identity
import com.f33d3r.app.net.Session
import com.f33d3r.app.net.live.LiveState
import com.f33d3r.app.ui.theme.Ink

/**
 * One row of the drawer. [shown] is the same gate the website's navigation applies
 * to the same link — the server states the standing in the shell it renders, and
 * this row reads it rather than deciding it.
 */
private data class DrawerEntry(
    val wire: Wire,
    val label: String,
    val icon: ImageVector,
    val subject: String = "",
    val shown: (Identity) -> Boolean = { true },
)

/**
 * The website's navigation, in its order, less the five wires on the bottom rail.
 * What is left is what the persona owns: its page, its saves, its catalogue, its
 * studio, its money.
 */
private val PRIMARY = listOf(
    DrawerEntry(Wire.PROFILE, "Profile", Icons.Outlined.Person),
    DrawerEntry(Wire.BOOKMARKS, "Bookmarks", Icons.Outlined.Bookmark),
    DrawerEntry(Wire.MUSIC, "Music", Icons.Outlined.MusicNote),
    DrawerEntry(Wire.GOLIVE, "Go Live", Icons.Outlined.Videocam),
    DrawerEntry(Wire.CREATOR, "Creator Studio", Icons.Outlined.Edit) { it.creator || it.admin },
    DrawerEntry(Wire.ARTICLES, "Articles", Icons.AutoMirrored.Outlined.Article),
    DrawerEntry(Wire.LEADERBOARD, "Leaderboard", Icons.Outlined.EmojiEvents),
    DrawerEntry(Wire.WALLET, "Wallet", Icons.Outlined.Wallet, "overview"),
    DrawerEntry(Wire.MARKETPLACE, "Market", Icons.Outlined.Storefront) { it.verified },
    DrawerEntry(Wire.ANALYTICS, "Analytics", Icons.Outlined.BarChart),
    DrawerEntry(Wire.LISTS, "Lists", Icons.AutoMirrored.Outlined.ListAlt),
)

private val SECONDARY = listOf(
    // The menu wires open on their section list; a link that names a section still
    // lands on that section.
    DrawerEntry(Wire.SETTINGS, "Settings and privacy", Icons.Outlined.Settings),
    DrawerEntry(Wire.ORG_PANEL, "Org Panel", Icons.Outlined.Domain) { it.official.isNotEmpty() },
    DrawerEntry(Wire.ADMIN, "Admin", Icons.Outlined.Shield) { it.admin },
)

/** The drawer's width: the page less a strip of the timeline, which the scrim keeps in view. */
private val DRAWER_WIDTH = 300.dp

/**
 * The persona sheet.
 *
 * It opens from the avatar because that is what it is about: which persona is acting,
 * what that persona holds, and where its own surfaces are. The balance and the
 * connection state are shown here rather than in the top bar — they are facts about
 * the identity, not about whatever wire happens to be open. So is the timeline's
 * density: how the persona likes its frame set is a choice it makes once, here.
 */
@Composable
fun PersonaDrawer(
    identity: Identity,
    balance: String,
    connection: LiveState,
    sealed: Boolean,
    density: String,
    mode: String,
    onOpen: (Wire, String) -> Unit,
    onDensity: () -> Unit,
    onMode: () -> Unit,
    onSignOut: () -> Unit,
) {
    Column(
        modifier = Modifier
            .fillMaxHeight()
            .width(DRAWER_WIDTH)
            .background(Ink.Surface)
            .statusBarsPadding()
            .navigationBarsPadding()
            .verticalScroll(rememberScrollState()),
    ) {
        // ── The persona ──
        Column(Modifier.padding(horizontal = PAGE_PADDING)) {
            Spacer(Modifier.height(16.dp))
            PersonaAvatar(
                avatarUrl = identity.avatarUrl,
                handle = identity.handle,
                size = 56.dp,
                onClick = { onOpen(Wire.PROFILE, identity.handle) },
            )

            Spacer(Modifier.height(12.dp))
            Text(
                text = identity.displayName.ifEmpty { identity.handle },
                style = MaterialTheme.typography.titleLarge,
                fontSize = 20.sp,
                fontWeight = FontWeight.Bold,
                color = Ink.Primary,
                maxLines = 1,
                overflow = TextOverflow.Ellipsis,
                modifier = Modifier.clickable { onOpen(Wire.PROFILE, identity.handle) },
            )
            Spacer(Modifier.height(2.dp))
            Text(
                text = "@${identity.handle}",
                style = MaterialTheme.typography.bodyLarge,
                fontSize = 15.sp,
                color = Ink.Tertiary,
                maxLines = 1,
                overflow = TextOverflow.Ellipsis,
            )

            // The two lists every profile has, one tap from the persona's name. The
            // counts are the server's to state and it states them on the pages themselves.
            Spacer(Modifier.height(12.dp))
            Row(verticalAlignment = Alignment.CenterVertically) {
                CountLink("Following") { onOpen(Wire.FOLLOWING, identity.handle) }
                Spacer(Modifier.width(16.dp))
                CountLink("Followers") { onOpen(Wire.FOLLOWERS, identity.handle) }
            }

            // What the frame knows about the connection and the keys, in one quiet line.
            Spacer(Modifier.height(12.dp))
            Row(verticalAlignment = Alignment.CenterVertically) {
                ConnectionDot(connection)
                Spacer(Modifier.width(8.dp))
                Text(
                    text = when (connection) {
                        LiveState.LIVE -> "Live"
                        LiveState.CONNECTING -> "Connecting"
                        LiveState.DROPPED -> "Offline"
                    },
                    style = MaterialTheme.typography.bodyMedium,
                    fontSize = 14.sp,
                    color = Ink.Tertiary,
                )
                Meta(" · ")
                Text(
                    text = if (sealed) "Sealed" else "Unsealed",
                    style = MaterialTheme.typography.bodyMedium,
                    fontSize = 14.sp,
                    color = if (sealed) Ink.Secondary else Ink.Muted,
                )
                if (balance.isNotEmpty()) {
                    Meta(" · ")
                    Text(
                        text = balance,
                        style = MaterialTheme.typography.bodyMedium,
                        fontSize = 14.sp,
                        fontWeight = FontWeight.SemiBold,
                        color = Ink.Primary,
                    )
                }
            }
            Spacer(Modifier.height(12.dp))
        }

        // ── The persona's own surfaces ──
        PRIMARY.filter { it.shown(identity) }.forEach { entry ->
            DrawerRow(entry.label, entry.icon) { onOpen(entry.wire, entry.subject) }
        }

        Section()

        // ── The frame's choices ──
        DrawerRow(
            label = "Dark mode",
            icon = Icons.Outlined.DarkMode,
            toggled = mode != Session.MODE_LIGHT,
            onClick = onMode,
        )
        DrawerRow(
            label = "Compact timeline",
            icon = Icons.Outlined.ViewAgenda,
            toggled = density == Session.DENSITY_COMPACT,
            onClick = onDensity,
        )

        Section()

        SECONDARY.filter { it.shown(identity) }.forEach { entry ->
            DrawerRow(entry.label, entry.icon) { onOpen(entry.wire, entry.subject) }
        }

        Section()

        DrawerRow(label = "Sign out", ink = Ink.Refused, onClick = onSignOut)
        Spacer(Modifier.height(16.dp))
    }
}

/** A hairline with breathing room, between one group of rows and the next. */
@Composable
private fun Section() {
    Spacer(Modifier.height(8.dp))
    Box(Modifier.fillMaxWidth().height(1.dp).background(Ink.Hairline))
    Spacer(Modifier.height(8.dp))
}

/** One of the two list links under the handle. */
@Composable
private fun CountLink(label: String, onClick: () -> Unit) {
    Text(
        text = label,
        style = MaterialTheme.typography.bodyLarge,
        fontSize = 15.sp,
        fontWeight = FontWeight.SemiBold,
        color = Ink.Primary,
        modifier = Modifier.clickable(onClick = onClick),
    )
}

@Composable
private fun Meta(text: String) {
    Text(
        text = text,
        style = MaterialTheme.typography.bodyMedium,
        fontSize = 14.sp,
        color = Ink.Muted,
    )
}

/**
 * A drawer row: 52 tall, a 22 outlined glyph, a 17 label, and — when the row is a
 * choice the frame keeps — a switch that shows the accent when the choice is on.
 */
@Composable
private fun DrawerRow(
    label: String,
    icon: ImageVector? = null,
    toggled: Boolean? = null,
    ink: Color = Ink.Primary,
    onClick: () -> Unit,
) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .height(ROW_HEIGHT)
            .clickable(onClick = onClick)
            .padding(horizontal = PAGE_PADDING),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        if (icon != null) {
            Icon(
                imageVector = icon,
                contentDescription = null,
                tint = ink,
                modifier = Modifier.size(BAR_GLYPH),
            )
            Spacer(Modifier.width(16.dp))
        }
        Text(
            text = label,
            style = MaterialTheme.typography.titleMedium,
            fontSize = 17.sp,
            fontWeight = FontWeight.SemiBold,
            color = ink,
            maxLines = 1,
            overflow = TextOverflow.Ellipsis,
            modifier = Modifier.weight(1f),
        )
        if (toggled != null) {
            Switch(
                checked = toggled,
                onCheckedChange = { onClick() },
                colors = accentSwitchColors(),
            )
        }
    }
}

/** The one indicator that says whether the application currently exists. */
@Composable
fun ConnectionDot(state: LiveState, size: Dp = 8.dp) {
    Box(
        modifier = Modifier
            .size(size)
            .background(
                color = when (state) {
                    LiveState.LIVE -> Ink.Positive
                    LiveState.CONNECTING -> Ink.Accent
                    LiveState.DROPPED -> Ink.Muted
                },
                shape = CircleShape,
            ),
    )
}
