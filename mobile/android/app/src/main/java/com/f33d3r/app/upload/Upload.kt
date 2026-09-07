package com.f33d3r.app.upload

import java.io.File

/** What kind of thing is being uploaded, and which lane the server has for it. */
enum class MediaKind {
    IMAGE,
    VIDEO,
    VOICE,
    AVATAR,
    HEADER,
    VISION,
}

/** A file on its way to the server, with everything the lanes need to send it. */
data class Upload(
    val file: File,
    val kind: MediaKind,
    val filename: String = file.name,
    /** Set for video once the server has issued a resumable session. */
    val tusId: String = "",
    val bytesSent: Long = 0,
)

/**
 * Where an upload has got to.
 *
 * The states are the server's, not invented here: `Duplicate` and `SelfDuplicate` are
 * distinct because the server distinguishes them, and `Processing` exists because a
 * video is not finished when its bytes arrive — it is finished when the transcoder
 * says so.
 */
sealed interface UploadState {

    data object Idle : UploadState

    /** Hashing before sending, so a duplicate costs a digest and not an upload. */
    data class Hashing(val kind: MediaKind) : UploadState

    data class Sending(val sent: Long, val total: Long) : UploadState {
        val fraction: Float get() = if (total <= 0) 0f else (sent.toFloat() / total).coerceIn(0f, 1f)
    }

    /** Bytes are in; the transcoder is still working. */
    data object Processing : UploadState

    /** An image or voice note is stored and addressable. */
    data class Stored(val url: String) : UploadState

    /** A video is transcoded and addressable. */
    data class Transcoded(
        val masterUrl: String,
        val posterUrl: String,
        val durationSecs: Long,
        val width: Int,
        val height: Int,
        /** Carried into the work envelope, outside the signature. */
        val tusUploadId: String,
    ) : UploadState

    /**
     * This file is already on the platform, uploaded by someone else.
     *
     * The server returns the canonical media so the work can cite the original
     * instead of storing a second copy. Deciding what to do with that is the
     * person's, so this state carries the facts and no action.
     */
    data class Duplicate(
        val masterUrl: String,
        val posterUrl: String,
        val workId: String,
        val creatorHandle: String,
        val creatorName: String,
        val bySelf: Boolean,
    ) : UploadState

    /** The server refused, in its own words. */
    data class Refused(val message: String) : UploadState
}
