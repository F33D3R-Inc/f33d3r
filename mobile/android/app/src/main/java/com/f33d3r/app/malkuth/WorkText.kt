package com.f33d3r.app.malkuth

import java.text.Normalizer

/**
 * What a work's text is allowed to be: the client half of
 * `feed-engine/internal/worktext` (normalizer version 1), mirrored rule for rule from
 * `F33WorkText` in `malkuth.js`, the reference client, and from the iOS `WorkText`.
 *
 * The body and every poll option are inside the signed content address, so the
 * server never rewrites them: it refuses any text that is not already what
 * [normalize] makes of it. The composer therefore signs `normalize(body)`, and the
 * counter measures [weightedLength] of that same normalised text, because that is the
 * length the server limits.
 *
 * Everything here works on Unicode code points, as Go's `range` over a string does —
 * never on UTF-16 chars, which would split an emoji in two.
 */
object WorkText {

    const val NORMALIZER_VERSION = 1

    /** The one length limit of a body, in weighted units. */
    const val MAX_WEIGHTED_LENGTH = 2000

    /** The stored-size bound of a body, in code points, independent of weight. */
    const val MAX_CODE_POINTS = 10_000

    /** What one link weighs, whatever its length. */
    const val URL_WEIGHT = 23

    /** The longest a poll option may be, in code points. */
    const val POLL_OPTION_MAX_CODE_POINTS = 80

    /**
     * Normalizer v1, preceded by NFC as `malkuth.js` applies it:
     *
     *  1. NFC.
     *  2. CRLF and lone CR become LF.
     *  3. Control characters other than LF and TAB, bidi controls and invisible
     *     characters are removed. ZWJ and ZWNJ are kept.
     *  4. A line holding only whitespace becomes empty, and runs of empty lines
     *     collapse to one.
     *  5. Leading and trailing whitespace is trimmed.
     */
    fun normalize(text: String): String {
        val nfc = Normalizer.normalize(text, Normalizer.Form.NFC)
        val kept = StringBuilder(nfc.length)
        var i = 0
        while (i < nfc.length) {
            val cp = nfc.codePointAt(i)
            i += Character.charCount(cp)
            when {
                cp == 0x0D -> {
                    // CRLF and a lone CR are both one LF.
                    if (i < nfc.length && nfc[i] == '\n') i++
                    kept.append('\n')
                }
                !isStripped(cp) -> kept.appendCodePoint(cp)
            }
        }

        val lines = ArrayList<String>()
        var blank = false
        for (line in kept.split('\n')) {
            if (allSpace(line)) {
                if (!blank) lines.add("")
                blank = true
                continue
            }
            blank = false
            lines.add(line)
        }
        return trimSpace(lines.joinToString("\n"))
    }

    /** Whether [text] is already what [normalize] makes of it. */
    fun isNormalized(text: String): Boolean = normalize(text) == text

    /** Whether [text] has nothing a reader can see: no letter, number, punctuation or symbol. */
    fun isBlank(text: String): Boolean {
        var i = 0
        while (i < text.length) {
            val cp = text.codePointAt(i)
            if (isVisible(cp)) return false
            i += Character.charCount(cp)
        }
        return true
    }

    /** Whether a normalised value fits a one-line field (a poll option, a tag). */
    fun isSingleLine(text: String): Boolean = text.indexOf('\n') < 0

    /** The number of code points, the unit the server's rune bounds use. */
    fun codePoints(text: String): Int = text.codePointCount(0, text.length)

    /**
     * The length the body limit is measured in. Each link weighs [URL_WEIGHT];
     * everything else weighs what its code points weigh.
     *
     * A link is what Go's `https?://[^\s"'<>\x00-\x1f]+` matches, where `\s` is RE2's
     * ASCII class — the server is the verifier, so its pattern is the one implemented.
     * Sentence punctuation at a link's end is not the link, and is weighed as text.
     */
    fun weightedLength(text: String): Int {
        val cps = text.codePoints().toArray()
        var n = 0
        var last = 0
        var i = 0
        while (i < cps.size) {
            val schemeEnd = schemeEnd(cps, i)
            if (schemeEnd < 0) { i++; continue }
            var end = schemeEnd
            while (end < cps.size && isUrlCodePoint(cps[end])) end++
            if (end == schemeEnd) { i++; continue }
            var linkEnd = end
            while (linkEnd > i && isUrlTrailing(cps[linkEnd - 1])) linkEnd--
            n += weight(cps, last, i) + URL_WEIGHT
            last = linkEnd
            i = end
        }
        return n + weight(cps, last, cps.size)
    }

    /**
     * Room left under both of the server's bounds — the weighted limit and the stored
     * size — for [text] as it will be signed. Negative when over.
     */
    fun remaining(text: String): Int {
        val signed = normalize(text)
        return minOf(
            MAX_WEIGHTED_LENGTH - weightedLength(signed),
            MAX_CODE_POINTS - codePoints(signed),
        )
    }

    // ── Rules, by code point ──────────────────────────────────────────────────

    internal fun isStripped(cp: Int): Boolean = when {
        cp == 0x0A || cp == 0x09 -> false
        cp < 0x20 || cp == 0x7F || cp in 0x80..0x9F -> true // C0, DEL, C1
        cp == 0x061C || cp == 0x200E || cp == 0x200F ||
            cp in 0x202A..0x202E || cp in 0x2066..0x2069 -> true // bidi
        cp == 0x00AD || cp == 0x180E || cp == 0x200B ||
            cp in 0x2060..0x2064 || cp in 0x206A..0x206F ||
            cp == 0x3164 || cp == 0xFFA0 || cp == 0xFEFF ||
            cp in 0xFFF9..0xFFFB -> true // invisible
        else -> false
    }

    /** Go's `unicode.IsSpace`. */
    internal fun isSpace(cp: Int): Boolean =
        cp in 0x09..0x0D || cp == 0x20 || cp == 0x85 || cp == 0xA0 || cp == 0x1680 ||
            cp in 0x2000..0x200A || cp == 0x2028 || cp == 0x2029 || cp == 0x202F ||
            cp == 0x205F || cp == 0x3000

    private fun allSpace(line: String): Boolean {
        var i = 0
        while (i < line.length) {
            val cp = line.codePointAt(i)
            if (!isSpace(cp)) return false
            i += Character.charCount(cp)
        }
        return true
    }

    private fun trimSpace(text: String): String {
        var start = 0
        while (start < text.length) {
            val cp = text.codePointAt(start)
            if (!isSpace(cp)) break
            start += Character.charCount(cp)
        }
        var end = text.length
        while (end > start) {
            val cp = text.codePointBefore(end)
            if (!isSpace(cp)) break
            end -= Character.charCount(cp)
        }
        return text.substring(start, end)
    }

    private fun isVisible(cp: Int): Boolean = when (Character.getType(cp)) {
        Character.UPPERCASE_LETTER.toInt(), Character.LOWERCASE_LETTER.toInt(),
        Character.TITLECASE_LETTER.toInt(), Character.MODIFIER_LETTER.toInt(),
        Character.OTHER_LETTER.toInt(),
        Character.DECIMAL_DIGIT_NUMBER.toInt(), Character.LETTER_NUMBER.toInt(),
        Character.OTHER_NUMBER.toInt(),
        Character.CONNECTOR_PUNCTUATION.toInt(), Character.DASH_PUNCTUATION.toInt(),
        Character.START_PUNCTUATION.toInt(), Character.END_PUNCTUATION.toInt(),
        Character.INITIAL_QUOTE_PUNCTUATION.toInt(), Character.FINAL_QUOTE_PUNCTUATION.toInt(),
        Character.OTHER_PUNCTUATION.toInt(),
        Character.MATH_SYMBOL.toInt(), Character.CURRENCY_SYMBOL.toInt(),
        Character.MODIFIER_SYMBOL.toInt(), Character.OTHER_SYMBOL.toInt() -> true
        else -> false
    }

    internal fun codePointWeight(cp: Int): Int {
        val type = Character.getType(cp)
        return when {
            type == Character.NON_SPACING_MARK.toInt() || type == Character.ENCLOSING_MARK.toInt() -> 0
            cp == 0x200C || cp == 0x200D || cp in 0xFE00..0xFE0F || cp in 0xE0100..0xE01EF ||
                cp in 0x1F3FB..0x1F3FF || cp in 0xE0020..0xE007F -> 0
            cp <= 0x10FF || cp in 0x2000..0x200D || cp in 0x2010..0x201F || cp in 0x2032..0x2037 -> 1
            else -> 2
        }
    }

    private fun weight(cps: IntArray, from: Int, to: Int): Int {
        var n = 0
        for (k in from until to) n += codePointWeight(cps[k])
        return n
    }

    /** The index just past `http://` or `https://` starting at [i], or -1. */
    private fun schemeEnd(cps: IntArray, i: Int): Int {
        fun matches(literal: String): Boolean {
            if (i + literal.length > cps.size) return false
            for (k in literal.indices) if (cps[i + k] != literal[k].code) return false
            return true
        }
        return when {
            matches("https://") -> i + 8
            matches("http://") -> i + 7
            else -> -1
        }
    }

    /** `[^\s"'<>\x00-\x1f]` with RE2's ASCII `\s`. */
    private fun isUrlCodePoint(cp: Int): Boolean =
        !(cp in 0x00..0x20 || cp == 0x22 || cp == 0x27 || cp == 0x3C || cp == 0x3E)

    /** `.,;:!?)'"` */
    private fun isUrlTrailing(cp: Int): Boolean =
        cp == 0x2E || cp == 0x2C || cp == 0x3B || cp == 0x3A || cp == 0x21 ||
            cp == 0x3F || cp == 0x29 || cp == 0x27 || cp == 0x22
}
