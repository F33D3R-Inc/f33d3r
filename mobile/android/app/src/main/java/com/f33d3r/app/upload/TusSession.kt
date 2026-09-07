package com.f33d3r.app.upload

import android.content.Context
import android.util.Base64
import com.f33d3r.app.net.Session
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody
import okio.BufferedSink
import okio.source
import java.io.File
import java.io.IOException

/**
 * A resumable upload, as the tus protocol defines it and this server implements it.
 *
 * Resumability is the point on a phone. A three-minute video on a moving train will
 * lose its connection, and an upload that starts over from zero each time never
 * finishes. So the offset is the server's to state — asked for with HEAD, corrected
 * from the 409 the server sends on a mismatch — and the client's job is only to send
 * from wherever the server says it already has bytes.
 *
 * The session id survives the process, because the other thing that kills an upload
 * is the app being backgrounded and reaped mid-send.
 */
class TusSession(
    context: Context,
    private val session: Session,
    private val http: OkHttpClient,
) {

    private val remembered = context.applicationContext
        .getSharedPreferences("f33d3r.tus", Context.MODE_PRIVATE)

    /**
     * Creates a session for [file], or reuses the one this device already has for it.
     *
     * Reuse is keyed on the file's own path and length: the same capture resumed after
     * a restart is the same upload, and starting a second session for it would send
     * the whole file again and leave an abandoned one on the server.
     */
    fun open(file: File, filename: String): String? {
        rememberedId(file)?.let { existing ->
            // The server is the authority on whether that session still exists. A
            // remembered id it has forgotten (a restart, an expiry) is not resumable.
            if (offset(existing) != null) return existing
            forget(file)
        }

        val metadata = "filename " + Base64.encodeToString(
            filename.toByteArray(Charsets.UTF_8),
            Base64.NO_WRAP,
        )

        val request = Request.Builder()
            .url(session.origin.trimEnd('/') + "/upload/tus/")
            .post(EMPTY)
            .header("Tus-Resumable", TUS_VERSION)
            .header("Upload-Length", file.length().toString())
            .header("Upload-Metadata", metadata)
            .build()

        return runCatching {
            http.newCall(request).execute().use { response ->
                if (response.code != 201) return@use null
                // The id is the last segment of Location, which is the server's address
                // for this upload — never reconstructed from anything the client knows.
                val location = response.header("Location").orEmpty()
                location.substringAfterLast('/').takeIf { it.isNotEmpty() }
            }
        }.getOrNull()?.also { remember(file, it) }
    }

    /** Asks the server how many bytes it already holds, or null if it has no session. */
    fun offset(uploadId: String): Long? {
        val request = Request.Builder()
            .url(session.origin.trimEnd('/') + "/upload/tus/" + uploadId)
            .head()
            .header("Tus-Resumable", TUS_VERSION)
            .build()

        return runCatching {
            http.newCall(request).execute().use { response ->
                if (!response.isSuccessful) return@use null
                response.header("Upload-Offset")?.toLongOrNull()
            }
        }.getOrNull()
    }

    /**
     * Sends the file from [from] to its end, reporting progress as it goes.
     *
     * One PATCH carries the remainder rather than a chunk per request: the body is
     * streamed off disk, so a long upload costs one connection and the progress the
     * caller sees is the progress actually on the wire. A drop mid-send is recovered
     * by asking for the offset again and calling this with the new one.
     *
     * Returns the offset the server confirms, or null when the send failed.
     */
    fun send(
        uploadId: String,
        file: File,
        from: Long,
        onProgress: (sent: Long, total: Long) -> Unit,
    ): Long? {
        val total = file.length()
        if (from >= total) return from

        val body = object : RequestBody() {
            override fun contentType() = OFFSET_OCTET_STREAM

            override fun contentLength(): Long = total - from

            override fun writeTo(sink: BufferedSink) {
                file.inputStream().use { stream ->
                    var skipped = 0L
                    while (skipped < from) {
                        val jumped = stream.skip(from - skipped)
                        if (jumped <= 0) throw IOException("could not resume at offset $from")
                        skipped += jumped
                    }
                    val source = stream.source()
                    val buffer = okio.Buffer()
                    var sent = from
                    while (true) {
                        val read = source.read(buffer, WINDOW_BYTES)
                        if (read == -1L) break
                        sink.write(buffer, read)
                        sent += read
                        onProgress(sent, total)
                    }
                }
            }
        }

        val request = Request.Builder()
            .url(session.origin.trimEnd('/') + "/upload/tus/" + uploadId)
            .patch(body)
            .header("Tus-Resumable", TUS_VERSION)
            .header("Upload-Offset", from.toString())
            .build()

        return runCatching {
            http.newCall(request).execute().use { response ->
                when (response.code) {
                    // The server took the bytes and says where it now stands.
                    204 -> response.header("Upload-Offset")?.toLongOrNull() ?: total

                    // Offset conflict: the server holds a different amount than we
                    // assumed. It says how much, and that answer is authoritative.
                    409 -> response.header("Upload-Offset")?.toLongOrNull()

                    else -> null
                }
            }
        }.getOrNull()
    }

    /** Forgets a completed or dead session so its id is never resumed by mistake. */
    fun forget(file: File) {
        remembered.edit().remove(key(file)).apply()
    }

    private fun rememberedId(file: File): String? = remembered.getString(key(file), null)

    private fun remember(file: File, uploadId: String) {
        remembered.edit().putString(key(file), uploadId).apply()
    }

    /** Length is part of the key so an edited file is never resumed as the original. */
    private fun key(file: File) = "tus:" + file.absolutePath + ":" + file.length()

    private companion object {
        const val TUS_VERSION = "1.0.0"

        /** How much is moved per write, so progress is reported often enough to read. */
        const val WINDOW_BYTES = 64L * 1024

        val OFFSET_OCTET_STREAM = "application/offset+octet-stream".toMediaType()
        val EMPTY: RequestBody = ByteArray(0).let {
            okhttp3.RequestBody.create(null, it)
        }
    }
}
