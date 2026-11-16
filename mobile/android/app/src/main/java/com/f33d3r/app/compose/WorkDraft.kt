package com.f33d3r.app.compose

import com.f33d3r.app.malkuth.CommentGating
import com.f33d3r.app.malkuth.WorkText
import kotlinx.serialization.Serializable
import java.util.UUID

/**
 * A video the transcoder has finished with, as the server named it.
 *
 * Every field is the server's: the master and poster it wrote, the dimensions and
 * duration it measured. The composer copies them into the work envelope and asserts
 * nothing of its own about the file. [creditedTo] names the original uploader when the
 * clip is somebody else's, already on F33D3R; the work is then a credited reference.
 */
@Serializable
data class VideoAttachment(
    val masterUrl: String,
    val posterUrl: String = "",
    val durationSecs: Long = 0,
    val width: Int = 0,
    val height: Int = 0,
    val creditedTo: String = "",
)

/** The work a reply is to, as the card that offered the reply named it. */
@Serializable
data class ReplyTarget(
    val workId: String,
    val cid: String,
    val handle: String,
)

/**
 * What the composer has collected so far, and what is saved between openings.
 *
 * A draft is a device convenience — the same one the web composer keeps in the
 * browser — and never leaves the phone. Nothing in it is a claim about a work:
 * the work exists only once the server has verified the signature and stored it.
 * Attachments are stored as the URLs the server already assigned them, so a draft
 * resumed tomorrow cites the same stored files rather than re-uploading.
 */
@Serializable
data class WorkDraft(
    val id: String = UUID.randomUUID().toString(),
    val savedAt: Long = 0,
    val body: String = "",
    /** Continuations of a thread, in order. Empty when the draft is a single work. */
    val segments: List<String> = emptyList(),
    /** Stored photo URLs, as the upload lane returned them. */
    val images: List<String> = emptyList(),
    val video: VideoAttachment? = null,
    /** Who may reply: one of [CommentGating]'s values, the server's vocabulary. */
    val replyRule: String = CommentGating.EVERYONE,
    /** Subscribers only: the one audience gate the server offers a work. */
    val subscriberOnly: Boolean = false,
    /** In a gated thread, the first post stays free as a preview. */
    val previewFirst: Boolean = false,
    /** RFC 3339, or empty to post now. */
    val scheduledAt: String = "",
    /** A pinned interest lane's id; the server indexes it as a tag. Empty for none. */
    val lane: String = "",
    val nsfw: Boolean = false,
    /** The id of a work this one quotes; the server renders the embed. */
    val quoteWorkId: String = "",
    val replyTo: ReplyTarget? = null,
    /**
     * The content id of the last post of a thread that was cut short mid-way. The
     * next post continues that chain rather than starting a new one.
     */
    val continuesCid: String = "",
) {
    /** True when there is anything worth keeping. */
    val hasContent: Boolean
        get() = body.isNotBlank() || segments.any { it.isNotBlank() } ||
            images.isNotEmpty() || video != null

    val isThread: Boolean get() = segments.isNotEmpty()

    /**
     * Every body in posting order, as typed, blanks dropped. Each is normalised when
     * it is signed ([com.f33d3r.app.malkuth.WorkPayload.preparedForSigning]).
     */
    val bodies: List<String>
        get() = (listOf(body) + segments).filter { !WorkText.isBlank(WorkText.normalize(it)) }

    companion object {

        /** How many photos a work carries at most. */
        const val MAX_IMAGES = 4

        fun blank() = WorkDraft()
    }
}
