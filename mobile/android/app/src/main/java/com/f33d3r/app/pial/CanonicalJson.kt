package com.f33d3r.app.pial

/**
 * The canonical JSON writer the content identifier is computed over.
 *
 * A CID is a hash of bytes, so "the same payload" has to mean the same bytes on
 * every client that will ever sign one. The server recomputes the hash with Go's
 * `encoding/json` and `SetEscapeHTML(false)`, and rejects the work if it differs by a
 * byte — so this writer reproduces that encoder's rules rather than using a general
 * JSON library whose formatting is free to change:
 *
 *  - keys appear in the order written, never sorted at write time;
 *  - no whitespace anywhere;
 *  - only `"`, `\` and the C0 control characters are escaped, with `\n`, `\r` and
 *    `\t` in short form and the rest as `\u00xx` in lowercase hex — `<`, `>` and `&`
 *    are written literally, which is what disabling HTML escaping means;
 *  - U+2028 and U+2029 are escaped, as Go escapes them unconditionally;
 *  - absent values are `null`, and absent lists are `[]`, never omitted.
 */
internal class CanonicalJson {

    private val out = StringBuilder()
    private var first = true

    fun obj(build: CanonicalJson.() -> Unit): String {
        out.append('{')
        build()
        out.append('}')
        return out.toString()
    }

    fun put(key: String, value: String) {
        comma(); writeKey(key); writeString(value)
    }

    fun put(key: String, value: Boolean) {
        comma(); writeKey(key); out.append(if (value) "true" else "false")
    }

    fun put(key: String, value: Long) {
        comma(); writeKey(key); out.append(value)
    }

    fun putNullable(key: String, value: String?) {
        comma(); writeKey(key)
        if (value == null) out.append("null") else writeString(value)
    }

    fun putNullable(key: String, value: Long?) {
        comma(); writeKey(key)
        if (value == null) out.append("null") else out.append(value)
    }

    fun putStrings(key: String, values: List<String>) {
        comma(); writeKey(key); writeArray(values)
    }

    fun putNullableStrings(key: String, values: List<String>?) {
        comma(); writeKey(key)
        if (values == null) out.append("null") else writeArray(values)
    }

    private fun writeArray(values: List<String>) {
        out.append('[')
        values.forEachIndexed { index, value ->
            if (index > 0) out.append(',')
            writeString(value)
        }
        out.append(']')
    }

    private fun comma() {
        if (first) first = false else out.append(',')
    }

    private fun writeKey(key: String) {
        writeString(key)
        out.append(':')
    }

    private fun writeString(value: String) {
        out.append('"')
        var index = 0
        while (index < value.length) {
            val ch = value[index]
            when {
                ch == '"' -> out.append("\\\"")
                ch == '\\' -> out.append("\\\\")
                ch == '\n' -> out.append("\\n")
                ch == '\r' -> out.append("\\r")
                ch == '\t' -> out.append("\\t")
                ch.code < 0x20 -> out.append(String.format("\\u%04x", ch.code))
                // Go escapes the line and paragraph separators even with HTML escaping
                // off, because they are statement terminators in older JavaScript.
                ch.code == 0x2028 || ch.code == 0x2029 ->
                    out.append(String.format("\\u%04x", ch.code))

                else -> out.append(ch)
            }
            index++
        }
        out.append('"')
    }
}
