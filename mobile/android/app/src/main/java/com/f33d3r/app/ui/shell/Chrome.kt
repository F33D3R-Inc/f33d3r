package com.f33d3r.app.ui.shell

import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
import androidx.compose.foundation.horizontalScroll
import androidx.compose.foundation.interaction.MutableInteractionSource
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.IntrinsicSize
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.RowScope
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.statusBarsPadding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.BasicTextField
import androidx.compose.foundation.text.KeyboardActions
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.outlined.ArrowBack
import androidx.compose.material.icons.outlined.Add
import androidx.compose.material.icons.outlined.Check
import androidx.compose.material.icons.outlined.Close
import androidx.compose.material.icons.outlined.ExpandMore
import androidx.compose.material.icons.outlined.Search
import androidx.compose.material.icons.outlined.Settings
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.SwitchColors
import androidx.compose.material3.SwitchDefaults
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.SolidColor
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import coil3.compose.AsyncImage
import com.f33d3r.app.ui.SurfaceTab
import com.f33d3r.app.ui.theme.Ink

/*
 * The chrome's fixed measures. One place, so a bar on one wire is the same bar on
 * every other: 52 for a top bar, 44 for a tab row, 56 for the rail, 22 for a glyph in
 * a bar, 44 for anything a thumb has to hit, 32 for the avatar beside a title.
 */
val TOP_BAR_HEIGHT = 52.dp
val TAB_ROW_HEIGHT = 44.dp
val RAIL_HEIGHT = 56.dp
val ROW_HEIGHT = 52.dp
val PAGE_PADDING = 16.dp
val BAR_GLYPH = 22.dp
val TOUCH_TARGET = 44.dp
val BAR_AVATAR = 32.dp

/** The pill every chip, field and notice is cut to. */
val Pill = RoundedCornerShape(999.dp)

/** The wordmark's tracking: a fifth of an em at the wordmark's 20sp. */
private val WORDMARK_TRACKING = 4.sp

/** The persona's avatar, which is also the way into the drawer. */
@Composable
fun PersonaAvatar(
    avatarUrl: String,
    handle: String,
    size: Dp = BAR_AVATAR,
    ringed: Boolean = false,
    onClick: () -> Unit = {},
    modifier: Modifier = Modifier,
) {
    Box(
        modifier = modifier
            .size(size)
            .clip(CircleShape)
            .background(Ink.Elevated)
            // The ring is the one thing the accent says about an avatar: an unseen Vision.
            .then(if (ringed) Modifier.border(2.dp, Ink.Accent, CircleShape) else Modifier)
            .clickable(onClick = onClick),
        contentAlignment = Alignment.Center,
    ) {
        if (avatarUrl.isNotEmpty()) {
            AsyncImage(
                model = avatarUrl,
                contentDescription = handle,
                modifier = Modifier.size(size).clip(CircleShape),
            )
        } else {
            Text(
                text = handle.firstOrNull()?.uppercase() ?: "·",
                style = MaterialTheme.typography.titleMedium,
                fontSize = (size.value * 0.42f).sp,
                fontWeight = FontWeight.Bold,
                color = Ink.Tertiary,
            )
        }
    }
}

/** The wordmark, set the one way it is set everywhere: 20 bold, tracked a fifth of an em. */
@Composable
fun Wordmark(modifier: Modifier = Modifier) {
    Text(
        text = "F33D3R",
        style = MaterialTheme.typography.titleLarge,
        fontSize = 20.sp,
        fontWeight = FontWeight.Bold,
        letterSpacing = WORDMARK_TRACKING,
        color = Ink.Primary,
        maxLines = 1,
        modifier = modifier,
    )
}

/** A glyph in a bar: 22 tall, inside the 44 a thumb needs. */
@Composable
fun BarGlyph(
    icon: ImageVector,
    contentDescription: String?,
    onClick: (() -> Unit)? = null,
    tint: Color = Ink.Primary,
    modifier: Modifier = Modifier,
) {
    Box(
        modifier = modifier
            .size(TOUCH_TARGET)
            .clip(CircleShape)
            .then(if (onClick != null) Modifier.clickable(onClick = onClick) else Modifier),
        contentAlignment = Alignment.Center,
    ) {
        Icon(
            imageVector = icon,
            contentDescription = contentDescription,
            tint = tint,
            modifier = Modifier.size(BAR_GLYPH),
        )
    }
}

/** The switch colours every toggle in the chrome shares: the accent when on, nothing when off. */
@Composable
fun accentSwitchColors(): SwitchColors = SwitchDefaults.colors(
    checkedThumbColor = Ink.OnAccent,
    checkedTrackColor = Ink.Accent,
    checkedBorderColor = Ink.Accent,
    uncheckedThumbColor = Ink.Tertiary,
    uncheckedTrackColor = Ink.Elevated,
    uncheckedBorderColor = Ink.HairlineStrong,
)

/**
 * The tab row under the top bar.
 *
 * The selected tab is underlined rather than filled, and the line is exactly as wide
 * as the word above it, which is what leaves the whole row readable at a glance on a
 * black surface. Two or three tabs share the width between them; more than that
 * scroll sideways, the way a strip of lanes does.
 */
@Composable
fun WireTabs(
    tabs: List<SurfaceTab>,
    selected: String,
    onSelect: (String) -> Unit,
    onAdd: (() -> Unit)? = null,
    /**
     * When set, the open tab carries a chevron and a tap on it drops the whole list
     * down, so a lane that has scrolled off the side of the row is one tap away.
     */
    menu: Boolean = false,
    modifier: Modifier = Modifier,
) {
    if (tabs.isEmpty()) return
    val spread = tabs.size <= 3 && onAdd == null
    var dropped by remember { mutableStateOf(false) }
    Row(
        modifier = if (spread) {
            modifier.fillMaxWidth().height(TAB_ROW_HEIGHT)
        } else {
            modifier.fillMaxWidth().height(TAB_ROW_HEIGHT).horizontalScroll(rememberScrollState())
        },
        verticalAlignment = Alignment.CenterVertically,
    ) {
        tabs.forEach { tab ->
            val active = tab.id == selected
            val drops = menu && active
            Box(
                modifier = (if (spread) Modifier.weight(1f) else Modifier)
                    .height(TAB_ROW_HEIGHT)
                    .clickable(
                        interactionSource = remember { MutableInteractionSource() },
                        indication = null,
                    ) { if (drops) dropped = true else onSelect(tab.id) }
                    .padding(horizontal = PAGE_PADDING),
                contentAlignment = Alignment.Center,
            ) {
                Column(
                    horizontalAlignment = Alignment.CenterHorizontally,
                    modifier = Modifier.height(TAB_ROW_HEIGHT).width(IntrinsicSize.Max),
                ) {
                    Row(
                        verticalAlignment = Alignment.CenterVertically,
                        modifier = Modifier.weight(1f),
                    ) {
                        Text(
                            text = tab.label,
                            style = MaterialTheme.typography.titleMedium,
                            fontSize = 15.sp,
                            fontWeight = FontWeight.SemiBold,
                            color = if (active) Ink.Primary else Ink.Tertiary,
                            maxLines = 1,
                            overflow = TextOverflow.Ellipsis,
                        )
                        if (drops) {
                            Spacer(Modifier.width(2.dp))
                            Icon(
                                imageVector = Icons.Outlined.ExpandMore,
                                contentDescription = "Choose a lane",
                                tint = Ink.Tertiary,
                                modifier = Modifier.size(16.dp),
                            )
                        }
                    }
                    Box(
                        modifier = Modifier
                            .fillMaxWidth()
                            .height(3.dp)
                            .clip(Pill)
                            .background(if (active) Ink.Accent else Color.Transparent),
                    )
                }
                if (drops) {
                    DropdownMenu(
                        expanded = dropped,
                        onDismissRequest = { dropped = false },
                        containerColor = Ink.Elevated,
                    ) {
                        tabs.forEach { choice ->
                            val current = choice.id == selected
                            DropdownMenuItem(
                                text = {
                                    Text(
                                        text = choice.label,
                                        style = MaterialTheme.typography.bodyLarge,
                                        fontSize = 16.sp,
                                        fontWeight = if (current) FontWeight.SemiBold else FontWeight.Normal,
                                        color = Ink.Primary,
                                    )
                                },
                                trailingIcon = if (current) {
                                    {
                                        Icon(
                                            imageVector = Icons.Outlined.Check,
                                            contentDescription = null,
                                            tint = Ink.Accent,
                                            modifier = Modifier.size(18.dp),
                                        )
                                    }
                                } else {
                                    null
                                },
                                onClick = {
                                    dropped = false
                                    if (!current) onSelect(choice.id)
                                },
                            )
                        }
                    }
                }
            }
        }
        if (onAdd != null) {
            BarGlyph(
                icon = Icons.Outlined.Add,
                contentDescription = "Add a surface",
                onClick = onAdd,
                tint = Ink.Tertiary,
                modifier = Modifier.padding(end = 4.dp),
            )
        }
    }
}

/** The 40-tall pill a search field is cut from: a raised ground, a glyph, no frame. */
@Composable
private fun FieldPill(
    modifier: Modifier,
    onClick: (() -> Unit)? = null,
    content: @Composable RowScope.() -> Unit,
) {
    Row(
        modifier = modifier
            .fillMaxWidth()
            .height(40.dp)
            .clip(Pill)
            .background(Ink.Elevated)
            .then(if (onClick != null) Modifier.clickable(onClick = onClick) else Modifier)
            .padding(horizontal = 14.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Icon(
            imageVector = Icons.Outlined.Search,
            contentDescription = null,
            tint = Ink.Tertiary,
            modifier = Modifier.size(20.dp),
        )
        Spacer(Modifier.width(10.dp))
        content()
    }
}

/** The search field the explore wire carries; a tap opens the search wire. */
@Composable
fun SearchField(
    placeholder: String,
    modifier: Modifier = Modifier,
    onClick: () -> Unit,
) {
    FieldPill(modifier = modifier, onClick = onClick) {
        Text(
            text = placeholder,
            style = MaterialTheme.typography.bodyLarge,
            fontSize = 16.sp,
            color = Ink.Tertiary,
            maxLines = 1,
            overflow = TextOverflow.Ellipsis,
        )
    }
}

/** The top bar's three shapes: a wordmark, a title, or a title with a subtitle. */
@Composable
fun TopBar(
    avatarUrl: String,
    handle: String,
    title: String,
    subtitle: String = "",
    showWordmark: Boolean = false,
    onAvatar: () -> Unit,
    onBack: (() -> Unit)? = null,
    trailing: @Composable (() -> Unit)? = null,
) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .statusBarsPadding()
            .height(TOP_BAR_HEIGHT)
            .padding(horizontal = PAGE_PADDING),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        // The leading slot is the width of the avatar whichever it holds, so the
        // title sits at the same centre on a wire with a back arrow and one without.
        Box(Modifier.size(TOUCH_TARGET), contentAlignment = Alignment.CenterStart) {
            if (onBack != null) {
                BarGlyph(
                    icon = Icons.AutoMirrored.Outlined.ArrowBack,
                    contentDescription = "Back",
                    onClick = onBack,
                )
            } else {
                PersonaAvatar(avatarUrl = avatarUrl, handle = handle, onClick = onAvatar)
            }
        }

        Column(
            modifier = Modifier.weight(1f),
            horizontalAlignment = Alignment.CenterHorizontally,
        ) {
            if (showWordmark) {
                Wordmark()
            } else {
                Text(
                    text = title,
                    style = MaterialTheme.typography.titleLarge,
                    fontSize = 20.sp,
                    fontWeight = FontWeight.Bold,
                    color = Ink.Primary,
                    maxLines = 1,
                    overflow = TextOverflow.Ellipsis,
                )
                if (subtitle.isNotEmpty()) {
                    Text(
                        text = subtitle,
                        style = MaterialTheme.typography.bodyMedium,
                        fontSize = 13.sp,
                        color = Ink.Tertiary,
                        maxLines = 1,
                        overflow = TextOverflow.Ellipsis,
                    )
                }
            }
        }

        // The trailing slot is as wide as what it holds — a gear, or a filter pill —
        // and never narrower than the leading slot, so the title stays centred.
        Box(
            modifier = Modifier.widthIn(min = TOUCH_TARGET).height(TOUCH_TARGET),
            contentAlignment = Alignment.CenterEnd,
        ) {
            trailing?.invoke()
        }
    }
}

/**
 * The timeline's own top bar: the persona on the left, the wordmark in the middle,
 * nothing on the right.
 *
 * The avatar is the way into the drawer, on this wire as on every other. The bell
 * and the inbox are on the rail below; how tightly the timeline is set is a choice
 * about the persona's frame and lives in the drawer with the persona's other choices.
 */
@Composable
fun HomeTopBar(
    avatarUrl: String,
    handle: String,
    onAvatar: () -> Unit,
) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .statusBarsPadding()
            .height(TOP_BAR_HEIGHT)
            .padding(horizontal = PAGE_PADDING),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Box(Modifier.size(TOUCH_TARGET), contentAlignment = Alignment.CenterStart) {
            PersonaAvatar(avatarUrl = avatarUrl, handle = handle, onClick = onAvatar)
        }
        Box(Modifier.weight(1f), contentAlignment = Alignment.Center) {
            Wordmark()
        }
        Spacer(Modifier.size(TOUCH_TARGET))
    }
}

/**
 * A small pill naming a state; tapping it changes the state.
 *
 * Outlined by a hairline when it is a choice not taken; filled with the ink when it
 * is, with the ground showing through the label — white on black in the dark, black
 * on white in the light — so it reads in either mode.
 */
@Composable
fun Chip(
    label: String,
    onClick: () -> Unit,
    filled: Boolean = false,
    modifier: Modifier = Modifier,
) {
    Box(
        modifier = modifier
            .height(32.dp)
            .clip(Pill)
            .background(if (filled) Ink.Primary else Color.Transparent)
            .border(1.dp, if (filled) Ink.Primary else Ink.HairlineStrong, Pill)
            .clickable(onClick = onClick)
            .padding(horizontal = 12.dp),
        contentAlignment = Alignment.Center,
    ) {
        Text(
            text = label,
            style = MaterialTheme.typography.labelLarge,
            fontSize = 13.sp,
            fontWeight = FontWeight.SemiBold,
            color = if (filled) Ink.Surface else Ink.Primary,
            maxLines = 1,
        )
    }
}

/**
 * A round 48-point control for a thumb rail over a picture. It is drawn over media,
 * so its two colours are the two that stay legible over any picture: a black scrim
 * and white ink.
 */
@Composable
fun RailButton(
    icon: ImageVector,
    label: String,
    onClick: () -> Unit,
    tint: Color = Color.White,
    ground: Color = Color(0x80000000),
) {
    Column(horizontalAlignment = Alignment.CenterHorizontally) {
        Box(
            modifier = Modifier
                .size(48.dp)
                .clip(CircleShape)
                .background(ground)
                .clickable(onClick = onClick),
            contentAlignment = Alignment.Center,
        ) {
            Icon(imageVector = icon, contentDescription = label, tint = tint, modifier = Modifier.size(BAR_GLYPH))
        }
        Spacer(Modifier.height(4.dp))
        Text(
            text = label,
            style = MaterialTheme.typography.labelSmall,
            fontSize = 11.sp,
            fontWeight = FontWeight.Bold,
            color = Color.White,
        )
    }
}

/** The way back, for a bar that has no room for the full top bar. */
@Composable
fun BackAction(onClick: () -> Unit) {
    BarGlyph(
        icon = Icons.AutoMirrored.Outlined.ArrowBack,
        contentDescription = "Back",
        onClick = onClick,
    )
}

/**
 * A query being typed. What is typed is the reader's draft until they submit it;
 * the query the surface is showing is the wire's subject, and it comes from the
 * Shell, not from here.
 */
@Composable
fun QueryField(
    initial: String,
    placeholder: String,
    modifier: Modifier = Modifier,
    onSubmit: (String) -> Unit,
    autofocus: Boolean = true,
) {
    var draft by remember(initial) { mutableStateOf(initial) }
    // The website autofocuses its search input; an empty query here means the
    // reader came to type, so the keyboard is up before they reach for it.
    val focus = remember { FocusRequester() }
    LaunchedEffect(Unit) { if (autofocus && initial.isEmpty()) focus.requestFocus() }
    FieldPill(modifier = modifier) {
        Box(Modifier.weight(1f), contentAlignment = Alignment.CenterStart) {
            if (draft.isEmpty()) {
                Text(
                    text = placeholder,
                    style = MaterialTheme.typography.bodyLarge,
                    fontSize = 16.sp,
                    color = Ink.Tertiary,
                    maxLines = 1,
                    overflow = TextOverflow.Ellipsis,
                )
            }
            BasicTextField(
                value = draft,
                onValueChange = { draft = it },
                singleLine = true,
                textStyle = MaterialTheme.typography.bodyLarge.copy(color = Ink.Primary, fontSize = 16.sp),
                cursorBrush = SolidColor(Ink.Accent),
                keyboardOptions = KeyboardOptions(imeAction = ImeAction.Search),
                keyboardActions = KeyboardActions(onSearch = { onSubmit(draft) }),
                modifier = Modifier.fillMaxWidth().focusRequester(focus),
            )
        }
        if (draft.isNotEmpty()) {
            Spacer(Modifier.width(6.dp))
            Box(
                modifier = Modifier
                    .size(28.dp)
                    .clip(CircleShape)
                    .clickable { draft = "" },
                contentAlignment = Alignment.Center,
            ) {
                Icon(
                    imageVector = Icons.Outlined.Close,
                    contentDescription = "Clear",
                    tint = Ink.Tertiary,
                    modifier = Modifier.size(18.dp),
                )
            }
        }
    }
}

/** The gear the settings-bearing wires put in the top bar's trailing slot. */
@Composable
fun SettingsAction(onClick: () -> Unit) {
    BarGlyph(
        icon = Icons.Outlined.Settings,
        contentDescription = "Settings",
        onClick = onClick,
    )
}

/** The chat wire's inbox filter: a chip with a chevron, in the top bar's trailing slot. */
@Composable
fun InboxFilter(label: String, onClick: () -> Unit) {
    Row(
        modifier = Modifier
            .height(32.dp)
            .clip(Pill)
            .border(1.dp, Ink.HairlineStrong, Pill)
            .clickable(onClick = onClick)
            .padding(start = 12.dp, end = 8.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Text(
            text = label,
            style = MaterialTheme.typography.labelLarge,
            fontSize = 13.sp,
            fontWeight = FontWeight.SemiBold,
            color = Ink.Primary,
        )
        Spacer(Modifier.width(2.dp))
        Icon(
            imageVector = Icons.Outlined.ExpandMore,
            contentDescription = null,
            tint = Ink.Tertiary,
            modifier = Modifier.size(16.dp),
        )
    }
}

/**
 * The live rail that sits above the bottom navigation while a stream is put away.
 *
 * It stays on screen across every wire on purpose: a room you are in is not something
 * you leave by navigating, and the bar is what makes that true rather than implied.
 */
@Composable
fun SpacesRail(
    title: String,
    detail: String,
    onOpen: () -> Unit,
    onLeave: () -> Unit,
    modifier: Modifier = Modifier,
) {
    Column(modifier = modifier.fillMaxWidth().background(Ink.Surface)) {
        Box(Modifier.fillMaxWidth().height(1.dp).background(Ink.Hairline))
        Row(
            modifier = Modifier
                .fillMaxWidth()
                .height(ROW_HEIGHT)
                .clickable(onClick = onOpen)
                .padding(start = PAGE_PADDING, end = 4.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            // The one red thing on the frame: this stream is live.
            Box(Modifier.size(8.dp).clip(CircleShape).background(Ink.Live))
            Spacer(Modifier.width(12.dp))
            Column(Modifier.weight(1f), verticalArrangement = Arrangement.Center) {
                Text(
                    text = title,
                    style = MaterialTheme.typography.titleMedium,
                    fontSize = 15.sp,
                    fontWeight = FontWeight.Bold,
                    color = Ink.Primary,
                    maxLines = 1,
                    overflow = TextOverflow.Ellipsis,
                )
                Text(
                    text = detail,
                    style = MaterialTheme.typography.bodyMedium,
                    fontSize = 14.sp,
                    color = Ink.Tertiary,
                    maxLines = 1,
                    overflow = TextOverflow.Ellipsis,
                )
            }
            BarGlyph(
                icon = Icons.Outlined.Close,
                contentDescription = "Leave",
                onClick = onLeave,
                tint = Ink.Tertiary,
            )
        }
    }
}
