package com.f33d3r.app.ui.theme

import androidx.compose.material3.Typography
import androidx.compose.ui.text.TextStyle
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.LineHeightStyle
import androidx.compose.ui.unit.sp

/**
 * The Shell's type scale.
 *
 * Sized for the chrome only — tab labels, titles, badges, the drawer. The content
 * plane is typeset by the server's own stylesheet, so nothing here should try to
 * reach into it: a second type scale applied to server-rendered facets is how a
 * timeline ends up with two different body sizes.
 *
 * The scale, in sp / line height:
 *
 * - headlineMedium 24/30 bold — a profile name, an empty-state heading.
 * - titleLarge     20/26 bold — a screen title, a sheet title.
 * - titleMedium    15/20 bold — a row's primary line (display name, conversation name).
 * - titleSmall     15/20 semibold — a tab label.
 * - bodyLarge      16/22 regular — body copy.
 * - bodyMedium     14/19 regular — meta (handle · time · counts), a sheet subtitle.
 * - bodySmall      13/18 regular — a reply preview, a secondary meta line.
 * - labelLarge     13/18 semibold — a chip, a small label.
 * - labelSmall     11/14 bold — a count badge.
 *
 * No tracking anywhere: the wordmark's letter-spacing is the chrome's own, stated
 * where the wordmark is drawn, not a property of the scale.
 */
private val trim = LineHeightStyle(
    alignment = LineHeightStyle.Alignment.Center,
    trim = LineHeightStyle.Trim.None,
)

val F33d3rType = Typography(
    headlineMedium = TextStyle(
        fontFamily = FontFamily.SansSerif,
        fontWeight = FontWeight.Bold,
        fontSize = 24.sp,
        lineHeight = 30.sp,
        lineHeightStyle = trim,
    ),
    titleLarge = TextStyle(
        fontFamily = FontFamily.SansSerif,
        fontWeight = FontWeight.Bold,
        fontSize = 20.sp,
        lineHeight = 26.sp,
        lineHeightStyle = trim,
    ),
    titleMedium = TextStyle(
        fontFamily = FontFamily.SansSerif,
        fontWeight = FontWeight.Bold,
        fontSize = 15.sp,
        lineHeight = 20.sp,
        lineHeightStyle = trim,
    ),
    titleSmall = TextStyle(
        fontFamily = FontFamily.SansSerif,
        fontWeight = FontWeight.SemiBold,
        fontSize = 15.sp,
        lineHeight = 20.sp,
        lineHeightStyle = trim,
    ),
    bodyLarge = TextStyle(
        fontFamily = FontFamily.SansSerif,
        fontWeight = FontWeight.Normal,
        fontSize = 16.sp,
        lineHeight = 22.sp,
        lineHeightStyle = trim,
    ),
    bodyMedium = TextStyle(
        fontFamily = FontFamily.SansSerif,
        fontWeight = FontWeight.Normal,
        fontSize = 14.sp,
        lineHeight = 19.sp,
        lineHeightStyle = trim,
    ),
    bodySmall = TextStyle(
        fontFamily = FontFamily.SansSerif,
        fontWeight = FontWeight.Normal,
        fontSize = 13.sp,
        lineHeight = 18.sp,
        lineHeightStyle = trim,
    ),
    labelLarge = TextStyle(
        fontFamily = FontFamily.SansSerif,
        fontWeight = FontWeight.SemiBold,
        fontSize = 13.sp,
        lineHeight = 18.sp,
        lineHeightStyle = trim,
    ),
    labelSmall = TextStyle(
        fontFamily = FontFamily.SansSerif,
        fontWeight = FontWeight.Bold,
        fontSize = 11.sp,
        lineHeight = 14.sp,
        lineHeightStyle = trim,
    ),
)
