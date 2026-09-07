package com.f33d3r.app.ui.shell

import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.SolidColor
import androidx.compose.ui.graphics.StrokeCap
import androidx.compose.ui.graphics.StrokeJoin
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.graphics.vector.path
import androidx.compose.ui.unit.dp

/**
 * The Vision mark: a ring with a point at its centre — the glyph the web shell's
 * navigation draws for Visions, so the two clients share one mark. Stroked at the
 * same 2-of-24 weight as the outlined Material glyphs beside it on the rail, so the
 * five read as one set; the filled variant is the same ring with its centre solid,
 * which is how the rail says a destination is open.
 *
 * The black here is the vector's own template colour, never what is drawn: every
 * Icon that shows the mark tints it with the ink of the bar it sits in.
 */
private const val MARK_STROKE = 2f

val VisionMark: ImageVector by lazy {
    ImageVector.Builder(name = "VisionMark", defaultWidth = 24.dp, defaultHeight = 24.dp, viewportWidth = 24f, viewportHeight = 24f)
        .apply {
            path(stroke = SolidColor(Color.Black), strokeLineWidth = MARK_STROKE, strokeLineCap = StrokeCap.Round, strokeLineJoin = StrokeJoin.Round) {
                ring(12f, 12f, 10f)
            }
            path(stroke = SolidColor(Color.Black), strokeLineWidth = MARK_STROKE) {
                ring(12f, 12f, 3f)
            }
        }
        .build()
}

val VisionMarkFilled: ImageVector by lazy {
    ImageVector.Builder(name = "VisionMarkFilled", defaultWidth = 24.dp, defaultHeight = 24.dp, viewportWidth = 24f, viewportHeight = 24f)
        .apply {
            path(stroke = SolidColor(Color.Black), strokeLineWidth = MARK_STROKE) {
                ring(12f, 12f, 10f)
            }
            path(fill = SolidColor(Color.Black)) {
                ring(12f, 12f, 5f)
            }
        }
        .build()
}

/** A circle as four cubic arcs, since a vector path has no circle primitive. */
private fun androidx.compose.ui.graphics.vector.PathBuilder.ring(cx: Float, cy: Float, r: Float) {
    val k = 0.5522847f * r
    moveTo(cx + r, cy)
    curveTo(cx + r, cy + k, cx + k, cy + r, cx, cy + r)
    curveTo(cx - k, cy + r, cx - r, cy + k, cx - r, cy)
    curveTo(cx - r, cy - k, cx - k, cy - r, cx, cy - r)
    curveTo(cx + k, cy - r, cx + r, cy - k, cx + r, cy)
    close()
}
