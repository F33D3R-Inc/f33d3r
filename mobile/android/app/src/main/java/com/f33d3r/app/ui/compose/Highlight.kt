package com.f33d3r.app.ui.compose

import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.AnnotatedString
import androidx.compose.ui.text.SpanStyle
import androidx.compose.ui.text.input.OffsetMapping
import androidx.compose.ui.text.input.TransformedText
import androidx.compose.ui.text.input.VisualTransformation

/** An @handle, a #tag or a link, as the server will read them out of the body. */
private val TOKEN = Regex("""(?<![\p{L}\p{N}_@#])[@#][\p{L}\p{N}_]{1,64}|https?://[^\s<>"']+""")

private val LINK = Regex("""https?://[^\s<>"']+""")

private val MENTION_AT_END = Regex("""(?<![\p{L}\p{N}_@#])@([\p{L}\p{N}_]{1,64})$""")

/**
 * Colours the tokens the server will link — mentions, tags, links — without
 * changing a character of what is typed. The mapping is the identity because the
 * text is the text; only the ink changes.
 */
class TokenInk(private val color: Color) : VisualTransformation {
    override fun filter(text: AnnotatedString): TransformedText {
        val builder = AnnotatedString.Builder(text)
        TOKEN.findAll(text.text).forEach { match ->
            builder.addStyle(SpanStyle(color = color), match.range.first, match.range.last + 1)
        }
        return TransformedText(builder.toAnnotatedString(), OffsetMapping.Identity)
    }
}

/** The @-token the caret is at the end of, without its @; null when the caret is elsewhere. */
fun mentionAtCursor(text: String, cursor: Int): String? {
    if (cursor <= 0 || cursor > text.length) return null
    // The token must end exactly at the caret: a caret in the middle of a word is not a request.
    if (cursor < text.length && (text[cursor].isLetterOrDigit() || text[cursor] == '_')) return null
    return MENTION_AT_END.find(text.substring(0, cursor))?.groupValues?.get(1)
}

/** Replaces the @-token ending at [cursor] with [handle], and returns the text and the new caret. */
fun completeMention(text: String, cursor: Int, handle: String): Pair<String, Int> {
    val head = text.substring(0, cursor)
    val match = MENTION_AT_END.find(head) ?: return text to cursor
    val replaced = head.substring(0, match.range.first) + "@" + handle + " "
    return (replaced + text.substring(cursor)) to replaced.length
}

fun linkCount(text: String): Int = LINK.findAll(text).count()

/**
 * Splits a body that runs past [limit] into posts that each fit, at paragraph
 * breaks first and at the last space before the limit when a paragraph alone is
 * too long. Nothing is dropped: the join of the parts is the body.
 */
fun splitIntoPosts(body: String, limit: Int): List<String> {
    val out = mutableListOf<String>()
    var current = StringBuilder()
    fun flush() {
        if (current.isNotBlank()) out += current.toString().trim()
        current = StringBuilder()
    }
    for (paragraph in body.split("\n\n")) {
        var piece = paragraph.trim()
        if (piece.isEmpty()) continue
        if (current.isNotEmpty() && current.length + 2 + piece.length > limit) flush()
        while (piece.length > limit) {
            if (current.isNotEmpty()) flush()
            val cut = piece.lastIndexOf(' ', limit - 1).takeIf { it > limit / 2 } ?: (limit - 1)
            out += piece.substring(0, cut).trim()
            piece = piece.substring(cut).trim()
        }
        if (current.isNotEmpty()) current.append("\n\n")
        current.append(piece)
    }
    flush()
    return out.ifEmpty { listOf(body.trim()) }
}
