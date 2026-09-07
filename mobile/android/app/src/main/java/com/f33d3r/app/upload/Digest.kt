package com.f33d3r.app.upload

import java.io.File
import java.security.MessageDigest

/**
 * The SHA-256 of a file, streamed.
 *
 * The digest is the server's primary key for both duplicate detection and the banned
 * content gate, so it is computed here before a byte is sent — that is the whole
 * point of the pre-check: a file already on the platform, or one that will be
 * refused, costs one hash instead of a full upload over a phone connection.
 *
 * It reads in chunks because a captured video is routinely larger than the heap this
 * process is allowed to hold.
 */
internal object Digest {

    private const val CHUNK_BYTES = 1 shl 16

    fun sha256(file: File): String {
        val digest = MessageDigest.getInstance("SHA-256")
        file.inputStream().use { stream ->
            val buffer = ByteArray(CHUNK_BYTES)
            while (true) {
                val read = stream.read(buffer)
                if (read <= 0) break
                digest.update(buffer, 0, read)
            }
        }
        return digest.digest().joinToString("") { "%02x".format(it) }
    }
}
