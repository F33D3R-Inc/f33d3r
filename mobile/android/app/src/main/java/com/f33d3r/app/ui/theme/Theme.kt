package com.f33d3r.app.ui.theme

import androidx.compose.material3.ColorScheme
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.darkColorScheme
import androidx.compose.material3.lightColorScheme
import androidx.compose.runtime.Composable

/**
 * The Material scheme, read off the palette at composition time so that a change of
 * accent theme or mode — both of which [Ink.apply] resolves — reaches every Material
 * control the same way it reaches the chrome that reads [Ink] directly.
 *
 * Every slot a Material control can read is stated, so nothing falls through to the
 * library's baseline purple. The mapping is the same in both modes; the two builders
 * differ only in the defaults for slots that are not stated, of which there are none
 * that a control in this app reads.
 */
@Composable
fun F33d3rTheme(
    dark: Boolean = Ink.dark,
    content: @Composable () -> Unit,
) {
    val scheme = if (dark) darkColorScheme().inked() else lightColorScheme().inked()
    MaterialTheme(
        colorScheme = scheme,
        typography = F33d3rType,
        content = content,
    )
}

/**
 * Maps the palette onto Material's slots.
 *
 * - `surfaceTint` is the surface itself, so tonal elevation adds nothing: a scrolled
 *   top bar, a navigation bar or a card at elevation stays the ground, never an
 *   accent-tinted panel.
 * - Every `surfaceContainer*` is [Ink.Elevated], which is what a sheet reads for its
 *   container (`surfaceContainerLow` in Material 1.4), and the `scrim` is [Ink.Scrim],
 *   which the sheet and dialogs draw at their own alpha over the page.
 * - `secondaryContainer` — the selected indicator of a navigation bar, a filter chip,
 *   a segmented button — is the accent's soft tint rather than a second hue.
 * - The inverse slots (a snackbar) are a [Ink.Primary] fill with [Ink.OnPrimary] ink.
 */
private fun ColorScheme.inked(): ColorScheme = copy(
    primary = Ink.Accent,
    onPrimary = Ink.OnAccent,
    primaryContainer = Ink.AccentSoft,
    onPrimaryContainer = Ink.Primary,
    inversePrimary = Ink.AccentBright,
    secondary = Ink.AccentBright,
    onSecondary = Ink.OnAccent,
    secondaryContainer = Ink.AccentSoft,
    onSecondaryContainer = Ink.Primary,
    tertiary = Ink.Secondary,
    onTertiary = Ink.OnPrimary,
    tertiaryContainer = Ink.Elevated,
    onTertiaryContainer = Ink.Primary,
    background = Ink.Surface,
    onBackground = Ink.Primary,
    surface = Ink.Surface,
    onSurface = Ink.Primary,
    surfaceVariant = Ink.Elevated,
    onSurfaceVariant = Ink.Tertiary,
    surfaceTint = Ink.Surface,
    inverseSurface = Ink.Primary,
    inverseOnSurface = Ink.OnPrimary,
    error = Ink.Refused,
    onError = Ink.OnAccent,
    errorContainer = Ink.Elevated,
    onErrorContainer = Ink.Refused,
    outline = Ink.HairlineStrong,
    outlineVariant = Ink.Hairline,
    scrim = Ink.Scrim,
    surfaceBright = Ink.Elevated,
    surfaceDim = Ink.Sunk,
    surfaceContainer = Ink.Elevated,
    surfaceContainerHigh = Ink.Elevated,
    surfaceContainerHighest = Ink.Elevated,
    surfaceContainerLow = Ink.Elevated,
    surfaceContainerLowest = Ink.Elevated,
)
