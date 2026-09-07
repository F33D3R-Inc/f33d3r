package com.f33d3r.app.ui.shell

import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.outlined.Article
import androidx.compose.material.icons.automirrored.outlined.HelpOutline
import androidx.compose.material.icons.automirrored.outlined.KeyboardArrowRight
import androidx.compose.material.icons.outlined.AccountBalance
import androidx.compose.material.icons.outlined.BarChart
import androidx.compose.material.icons.outlined.Build
import androidx.compose.material.icons.outlined.Celebration
import androidx.compose.material.icons.outlined.Contacts
import androidx.compose.material.icons.outlined.Dashboard
import androidx.compose.material.icons.outlined.Domain
import androidx.compose.material.icons.outlined.Gavel
import androidx.compose.material.icons.outlined.Group
import androidx.compose.material.icons.outlined.Lock
import androidx.compose.material.icons.outlined.MusicNote
import androidx.compose.material.icons.outlined.Notifications
import androidx.compose.material.icons.outlined.Palette
import androidx.compose.material.icons.outlined.Payments
import androidx.compose.material.icons.outlined.People
import androidx.compose.material.icons.outlined.Person
import androidx.compose.material.icons.outlined.Policy
import androidx.compose.material.icons.outlined.Security
import androidx.compose.material.icons.outlined.Settings
import androidx.compose.material.icons.outlined.Shield
import androidx.compose.material.icons.outlined.Star
import androidx.compose.material.icons.outlined.Verified
import androidx.compose.material.icons.outlined.Warning
import androidx.compose.material.icons.outlined.WorkspacePremium
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import com.f33d3r.app.core.Wire
import com.f33d3r.app.ui.SurfaceTab
import com.f33d3r.app.ui.theme.Ink

/**
 * A menu wire's front page: its sections as a vertical list.
 *
 * Icon, title, a line on what the section holds, a chevron — the shape every phone
 * settings screen has had for a decade, and the one a thumb can read without
 * scrolling sideways. The rows are the drawer's rows: 52 tall, a 22 outlined glyph,
 * a 17 label, a hairline between one and the next. The sections themselves are the
 * server's pages; this list only names them, in the server's order, and the words
 * beside each name say what the page is for.
 */
@Composable
fun SectionMenu(
    wire: Wire,
    sections: List<SurfaceTab>,
    onSelect: (String) -> Unit,
    modifier: Modifier = Modifier,
) {
    Column(
        modifier = modifier
            .fillMaxSize()
            .background(Ink.Surface)
            .verticalScroll(rememberScrollState()),
    ) {
        sections.forEachIndexed { index, section ->
            val detail = describe(wire, section.id)
            if (index > 0) {
                Box(Modifier.fillMaxWidth().height(1.dp).background(Ink.Hairline))
            }
            Row(
                modifier = Modifier
                    .fillMaxWidth()
                    .heightIn(min = ROW_HEIGHT)
                    .clickable { onSelect(section.id) }
                    .padding(horizontal = PAGE_PADDING, vertical = 12.dp),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                Icon(
                    imageVector = iconFor(wire, section.id),
                    contentDescription = null,
                    tint = Ink.Primary,
                    modifier = Modifier.size(BAR_GLYPH),
                )
                Spacer(Modifier.width(16.dp))
                Column(Modifier.weight(1f)) {
                    Text(
                        text = section.label,
                        style = MaterialTheme.typography.titleMedium,
                        fontSize = 17.sp,
                        fontWeight = FontWeight.SemiBold,
                        color = Ink.Primary,
                    )
                    if (detail.isNotEmpty()) {
                        Spacer(Modifier.height(2.dp))
                        Text(
                            text = detail,
                            style = MaterialTheme.typography.bodyMedium,
                            fontSize = 14.sp,
                            color = Ink.Tertiary,
                        )
                    }
                }
                Spacer(Modifier.width(12.dp))
                Icon(
                    imageVector = Icons.AutoMirrored.Outlined.KeyboardArrowRight,
                    contentDescription = null,
                    tint = Ink.Muted,
                    modifier = Modifier.size(20.dp),
                )
            }
        }
        Spacer(Modifier.height(96.dp))
    }
}

/** What each section is for, in a line. Sections with nothing to add stay a title. */
private fun describe(wire: Wire, id: String): String = when (wire) {
    Wire.SETTINGS -> when (id) {
        "account" -> "Your handle, display name, avatar and the personas you switch between."
        "security" -> "Password, two-factor authentication, backup codes and signed-in devices."
        "privacy" -> "Who sees your account, what content you see, and blocked or muted people."
        "notifications" -> "Which notifications you receive, here and as push."
        "display" -> "Dark or light, and the accent theme your profile carries everywhere."
        "contact" -> "Your F33D3R Numbers and contact links — how people reach you with nothing else."
        "verification" -> "Identity verification and the creator standing that comes with it."
        "realm" -> "Your realm, your XP, and what each level unlocks."
        "org" -> "An organisation account: members, roles and the official badge."
        "resources" -> "Help, terms, privacy policy and the DMCA process."
        "danger" -> "Deactivate or delete this account. Irreversible."
        else -> ""
    }

    Wire.CREATOR -> when (id) {
        "overview" -> "Today at a glance: earnings, new subscribers and what is under review."
        "content" -> "Everything you have published, gated or open, with its scan state."
        "audience" -> "Followers and subscribers, and how they found you."
        "earnings" -> "Tips, subscriptions and sales, paid straight to your wallets."
        "memberships" -> "Your subscription tiers and what each one unlocks."
        "music" -> "Your catalogue: tracks, releases and plays."
        "analytics" -> "Reach and engagement over time."
        "settings" -> "Creator settings: gating defaults, watermarks and payouts."
        else -> ""
    }

    else -> ""
}

private fun iconFor(wire: Wire, id: String): ImageVector = when (wire) {
    Wire.SETTINGS -> when (id) {
        "account" -> Icons.Outlined.Person
        "security" -> Icons.Outlined.Lock
        "privacy" -> Icons.Outlined.Shield
        "notifications" -> Icons.Outlined.Notifications
        "display" -> Icons.Outlined.Palette
        "contact" -> Icons.Outlined.Contacts
        "verification" -> Icons.Outlined.Verified
        "realm" -> Icons.Outlined.Star
        "org" -> Icons.Outlined.Domain
        "resources" -> Icons.AutoMirrored.Outlined.HelpOutline
        "danger" -> Icons.Outlined.Warning
        else -> Icons.Outlined.Settings
    }

    Wire.CREATOR -> when (id) {
        "overview" -> Icons.Outlined.Dashboard
        "content" -> Icons.AutoMirrored.Outlined.Article
        "audience" -> Icons.Outlined.People
        "earnings" -> Icons.Outlined.Payments
        "memberships" -> Icons.Outlined.WorkspacePremium
        "music" -> Icons.Outlined.MusicNote
        "analytics" -> Icons.Outlined.BarChart
        else -> Icons.Outlined.Settings
    }

    Wire.ADMIN -> when (id) {
        "overview" -> Icons.Outlined.Dashboard
        "users" -> Icons.Outlined.Group
        "moderation" -> Icons.Outlined.Gavel
        "dmca" -> Icons.Outlined.Policy
        "org-verifications" -> Icons.Outlined.Domain
        "security" -> Icons.Outlined.Security
        "treasury" -> Icons.Outlined.AccountBalance
        "analytics" -> Icons.Outlined.BarChart
        "celebrations" -> Icons.Outlined.Celebration
        "abraxas" -> Icons.Outlined.Shield
        "devtools" -> Icons.Outlined.Build
        else -> Icons.Outlined.Settings
    }

    else -> Icons.Outlined.Settings
}
