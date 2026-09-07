package com.f33d3r.app.ui.theme

import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.setValue
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.lerp

/**
 * One of the server's accent themes, resolved to sRGB.
 *
 * The server's stylesheet states each theme as an OKLCH accent on `body[data-theme]`
 * and, in dark mode, a pair of surfaces. Android has no OKLCH, so the accents are
 * converted once, here, and the surfaces are the stylesheet's own hex values. The
 * list is the server's `AllThemes()` in the server's order.
 */
class AccentTheme(
    val id: String,
    val name: String,
    val vibe: String,
    val accent: Color,
    val darkSurface: Color,
    val darkElevated: Color,
)

/** The six themes the server offers, as the profile editor lists them. */
val ACCENT_THEMES = listOf(
    AccentTheme("void", "Void", "Deep Space", Color(0xFF855DD7), Color(0xFF05050A), Color(0xFF0D0D18)),
    AccentTheme("aurora", "Aurora", "Flow State", Color(0xFF00A49D), Color(0xFF030F14), Color(0xFF071520)),
    AccentTheme("sakura", "Sakura", "Soft Power", Color(0xFFE9609E), Color(0xFF0E060E), Color(0xFF160C14)),
    AccentTheme("obsidian", "Obsidian", "Sharp Edge", Color(0xFF5981E0), Color(0xFF07071A), Color(0xFF0E0E24)),
    AccentTheme("moss", "Moss", "Root System", Color(0xFF399E43), Color(0xFF040C05), Color(0xFF091409)),
    AccentTheme("dusk", "Dusk", "Golden Hour", Color(0xFFD0901E), Color(0xFF0E0A04), Color(0xFF18130A)),
)

/**
 * The Shell's palette, resolved from the platform's own design tokens.
 *
 * These are not new colours. Each one is the sRGB resolution of a custom property in
 * the server's stylesheet — the light `:root` block, the `[data-mode="dark"]` block,
 * and the `body[data-theme]` accent rules — so the native chrome and the projected
 * facets are the same surface rather than two surfaces that happen to look similar.
 *
 * Where the native chrome sets type smaller than the content plane does, the two
 * quietest inks are lifted just far enough to clear the contrast floor for that
 * size: meta text (Tertiary) holds 4.5:1 and placeholder text (Muted) holds 3:1 on
 * every ground the server can state, in both modes. The hairlines are the server's
 * hue at a strength that reads on a phone panel: 9% for a divider, 14% for an
 * outline. Everything else is the stylesheet's own value.
 *
 * The palette is state, not constants: the accent theme is the persona's, stated by
 * the server on the shell it renders, and the mode is the frame's own choice. When
 * either changes, [apply] resolves the palette again and every reader recomposes.
 * The initial values are the dark "void" resolution so the first frame before the
 * server has stated anything is the same ground the window and splash were drawn on.
 */
object Ink {
    // --bg, --bg-elev, --bg-sunk
    var Surface by mutableStateOf(Color(0xFF05050A)); private set
    var Elevated by mutableStateOf(Color(0xFF0D0D18)); private set
    var Sunk by mutableStateOf(Color(0xFF050507)); private set

    // --ink through --ink-5
    var Primary by mutableStateOf(Color(0xFFF3F2EE)); private set
    var Secondary by mutableStateOf(Color(0xFFC8C5BE)); private set
    var Tertiary by mutableStateOf(Color(0xFF8F8C85)); private set
    var Muted by mutableStateOf(Color(0xFF6E6B65)); private set
    var Faint by mutableStateOf(Color(0xFF3E3C37)); private set

    // --hairline, --hairline-strong
    var Hairline by mutableStateOf(Color(0x17FFFFFF)); private set
    var HairlineStrong by mutableStateOf(Color(0x24FFFFFF)); private set

    // --accent, --accent-2, --accent-soft
    var Accent by mutableStateOf(Color(0xFF855DD7)); private set
    var AccentBright by mutableStateOf(Color(0xFF9B78DD)); private set
    var AccentSoft by mutableStateOf(Color(0xFF25193D)); private set

    /** The ink that stays legible on top of the accent (--accent-ink). */
    var OnAccent by mutableStateOf(Color(0xFFFFFFFF)); private set

    /**
     * The ink that stays legible on top of a [Primary] fill — a filled chip, a
     * snackbar, an inverted button. It is the ground the fill sits on: the dark
     * surface in dark mode, white in light mode.
     */
    var OnPrimary by mutableStateOf(Color(0xFF05050A)); private set

    /** The glass the bottom bars sit on, over content that scrolls beneath them. */
    var Glass by mutableStateOf(Color(0xEB05050A)); private set

    /**
     * What a modal sheet or dialog dims the page with, before its own alpha. Dark
     * mode sinks the page; light mode inks it, since a pale scrim over a pale ground
     * would not read as a dim at all.
     */
    var Scrim by mutableStateOf(Color(0xFF050507)); private set

    /** Whether the frame is in dark mode; the theme composable reads it. */
    var dark by mutableStateOf(true); private set

    /** The accent theme in force, by the server's id. */
    var theme by mutableStateOf("void"); private set

    /** Live, positive, and refused — the three states the chrome must state plainly. */
    val Live = Color(0xFFFF3B5C)
    val Positive = Color(0xFF3FCF8E)
    val Refused = Color(0xFFFF6B6B)

    /**
     * Resolves the palette for [themeId] in [mode] ("dark" or "light"). An unknown
     * theme id is the server's default, as the server's own stylesheet would read it.
     */
    fun apply(themeId: String, mode: String) {
        val accentTheme = ACCENT_THEMES.firstOrNull { it.id == themeId } ?: ACCENT_THEMES.first()
        val isDark = mode != MODE_LIGHT
        theme = accentTheme.id
        dark = isDark
        Accent = accentTheme.accent
        AccentBright = lerp(accentTheme.accent, Color.White, 0.15f)
        if (isDark) {
            Surface = accentTheme.darkSurface
            Elevated = accentTheme.darkElevated
            Sunk = Color(0xFF050507)
            Primary = Color(0xFFF3F2EE)
            Secondary = Color(0xFFC8C5BE)
            Tertiary = Color(0xFF8F8C85)
            Muted = Color(0xFF6E6B65)
            Faint = Color(0xFF3E3C37)
            Hairline = Color(0x17FFFFFF)
            HairlineStrong = Color(0x24FFFFFF)
            AccentSoft = lerp(accentTheme.accent, accentTheme.darkSurface, 0.72f)
            OnAccent = Color(0xFFFFFFFF)
            OnPrimary = accentTheme.darkSurface
            Glass = accentTheme.darkSurface.copy(alpha = 0.92f)
            Scrim = Color(0xFF050507)
        } else {
            Surface = Color(0xFFFAFAF7)
            Elevated = Color(0xFFFFFFFF)
            Sunk = Color(0xFFF3F2EE)
            Primary = Color(0xFF14120C)
            Secondary = Color(0xFF3A3730)
            Tertiary = Color(0xFF66625B)
            Muted = Color(0xFF8A867F)
            Faint = Color(0xFFC4C0B8)
            Hairline = Color(0x1714120C)
            HairlineStrong = Color(0x2414120C)
            AccentSoft = lerp(accentTheme.accent, Color.White, 0.86f)
            OnAccent = Color(0xFFFFFFFF)
            OnPrimary = Color(0xFFFFFFFF)
            Glass = Color(0xEBFAFAF7)
            Scrim = Color(0xFF14120C)
        }
    }

    const val MODE_DARK = "dark"
    const val MODE_LIGHT = "light"
}
