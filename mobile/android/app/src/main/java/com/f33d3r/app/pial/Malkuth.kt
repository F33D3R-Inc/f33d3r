package com.f33d3r.app.pial

import com.f33d3r.app.core.Endpoints
import com.f33d3r.app.net.FacetClient
import com.f33d3r.app.net.JsonAnswer
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.jsonPrimitive
import kotlinx.serialization.json.put
import java.security.MessageDigest

/**
 * A Work as its author signed it.
 *
 * Every field here is inside the content identifier, so every field here is something
 * the author is accountable for. Anything the server derives — a watermarked video
 * URL, transcoder dimensions, the resolved id of a quoted work — is deliberately
 * outside it: those are not the author's claims and must not be able to invalidate
 * the author's signature when the server recomputes them.
 */
data class WorkPayload(
    val authorPial: String,
    val body: String = "",
    val commentGating: String = "everyone",
    val isRepost: Boolean = false,
    val kind: String = "post",
    val mediaUrls: List<String> = emptyList(),
    val parentCid: String? = null,
    val pollEndsAt: String? = null,
    val pollOptions: List<String>? = null,
    val repostSourceId: String? = null,
    val scheduledAt: String? = null,
    val subscriberOnly: Boolean = false,
    val tags: List<String> = emptyList(),
    val timestampMs: Long = System.currentTimeMillis(),
    val videoDurationSecs: Long? = null,
    val videoMasterUrl: String? = null,
    val videoPosterUrl: String? = null,
    val voiceDurationSecs: Long? = null,
    val voiceUrl: String? = null,
)

/** Server-derived facts sent alongside the envelope, deliberately outside the signature. */
data class WorkAttachments(
    val isNsfw: Boolean = false,
    val videoWatermarkedUrl: String? = null,
    val videoWidth: Int = 0,
    val videoHeight: Int = 0,
    val quotedWorkId: String? = null,
    val reactLayout: String? = null,
    val tusUploadId: String? = null,
)

/**
 * What the server said about a Work it was asked to store.
 *
 * On success the server answers with the identifiers it assigned — the row id and
 * the content id it verified — and no render. What the timeline should show is the
 * timeline's next read; the receipt is only proof, and what a thread chains on.
 */
sealed interface Publication {
    data class Stored(val workId: String, val cid: String) : Publication
    data object Unauthorized : Publication

    /** Refused, in the server's words. */
    data class Refused(val message: String) : Publication
    data object Unreachable : Publication
}

/**
 * Malkuth — the lane that publishes a Work under the author's own key.
 *
 * A work is not "posted by whoever holds the session". It is hashed into a content
 * identifier, that identifier is signed by a key only this device can use, and the
 * server verifies the signature against the key authority before it stores anything.
 * The session says which persona is asking; the signature says who actually wrote it,
 * and those are different claims.
 */
class Malkuth(
    private val client: FacetClient,
    private val deviceKey: DeviceKey = DeviceKey(),
) {

    /**
     * Registers this device's public key with the key authority.
     *
     * Idempotent from the caller's side, and required before the first publish: the
     * server verifies against the registered key, so an unregistered device produces
     * signatures nothing can check.
     */
    suspend fun registerDevice(): Boolean = withContext(Dispatchers.IO) {
        val payload = buildJsonObject {
            put("public_key_b64", deviceKey.publicKeyB64())
            put("algorithm", "ECDSA-P256")
        }.toString()
        client.postJson(Endpoints.PIAL_SIGNING_KEY, payload) != null
    }

    /**
     * Computes the content identifier for [payload].
     *
     * `media_urls` is sorted because the server sorts it before hashing: the set of
     * attachments is the claim, not the order the composer happened to add them in.
     */
    fun contentId(payload: WorkPayload): String {
        val canonical = CanonicalJson().obj {
            put("author_pial", payload.authorPial)
            put("body", payload.body)
            put("comment_gating", payload.commentGating)
            put("is_repost", payload.isRepost)
            put("kind", payload.kind)
            putStrings("media_urls", payload.mediaUrls.sorted())
            putNullable("parent_cid", payload.parentCid)
            putNullable("poll_ends_at", payload.pollEndsAt)
            putNullableStrings("poll_options", payload.pollOptions)
            putNullable("repost_source_id", payload.repostSourceId)
            putNullable("scheduled_at", payload.scheduledAt)
            put("subscriber_only", payload.subscriberOnly)
            putStrings("tags", payload.tags)
            put("timestamp_ms", payload.timestampMs)
            putNullable("video_duration_secs", payload.videoDurationSecs)
            putNullable("video_master_url", payload.videoMasterUrl)
            putNullable("video_poster_url", payload.videoPosterUrl)
            putNullable("voice_duration_secs", payload.voiceDurationSecs)
            putNullable("voice_url", payload.voiceUrl)
        }
        val digest = MessageDigest.getInstance("SHA-256")
            .digest(canonical.toByteArray(Charsets.UTF_8))
        return "sha256:" + digest.joinToString("") { "%02x".format(it) }
    }

    /**
     * Signs and publishes a Work, returning the server's receipt for it.
     *
     * The client asserts nothing about the outcome. The server answers with the ids
     * it assigned, and nothing else; the timeline shows the work when the timeline
     * is next read from the server. There is no locally composed card to reconcile,
     * because none was made.
     */
    suspend fun publish(
        eventType: String,
        payload: WorkPayload,
        attachments: WorkAttachments = WorkAttachments(),
    ): Publication = withContext(Dispatchers.IO) {
        val cid = contentId(payload)
        val signature = deviceKey.sign(cid)
            ?: return@withContext Publication.Refused("This device cannot sign — key unavailable.")

        val envelope = buildJsonObject {
            put("event_type", eventType)
            put("cid", cid)
            put("signature", signature)
            put("is_nsfw", attachments.isNsfw)
            put("video_watermarked_url", attachments.videoWatermarkedUrl)
            put("video_width", attachments.videoWidth)
            put("video_height", attachments.videoHeight)
            put("quoted_work_id", attachments.quotedWorkId)
            put("react_layout", attachments.reactLayout)
            put("payload", buildJsonObject {
                put("author_pial", payload.authorPial)
                put("body", payload.body)
                put("comment_gating", payload.commentGating)
                put("is_repost", payload.isRepost)
                put("kind", payload.kind)
                putJsonStrings("media_urls", payload.mediaUrls.sorted())
                put("parent_cid", payload.parentCid)
                put("poll_ends_at", payload.pollEndsAt)
                if (payload.pollOptions == null) put("poll_options", null as String?)
                else putJsonStrings("poll_options", payload.pollOptions)
                put("repost_source_id", payload.repostSourceId)
                put("scheduled_at", payload.scheduledAt)
                put("subscriber_only", payload.subscriberOnly)
                putJsonStrings("tags", payload.tags)
                put("timestamp_ms", payload.timestampMs)
                put("video_duration_secs", payload.videoDurationSecs)
                put("video_master_url", payload.videoMasterUrl)
                put("video_poster_url", payload.videoPosterUrl)
                put("voice_duration_secs", payload.voiceDurationSecs)
                put("voice_url", payload.voiceUrl)
                // Carried in the payload but excluded from the signed canonical form, so
                // resuming an upload can never invalidate the author's signature.
                attachments.tusUploadId?.let { put("tus_upload_id", it) }
            })
        }.toString()

        when (val answer = client.exchangeJson(Endpoints.EVENTS, envelope)) {
            is JsonAnswer.Ok -> {
                val receipt = answer.body as? kotlinx.serialization.json.JsonObject
                val storedCid = receipt?.get("cid")?.jsonPrimitive?.content.orEmpty().ifEmpty { cid }
                val workId = receipt?.get("work_id")?.jsonPrimitive?.content.orEmpty()
                Publication.Stored(workId, storedCid)
            }
            JsonAnswer.Unauthorized -> Publication.Unauthorized
            is JsonAnswer.Refused -> Publication.Refused(answer.message.take(240))
            is JsonAnswer.Unreachable -> Publication.Unreachable
        }
    }
}

private fun kotlinx.serialization.json.JsonObjectBuilder.putJsonStrings(
    key: String,
    values: List<String>,
) {
    put(key, kotlinx.serialization.json.JsonArray(values.map { kotlinx.serialization.json.JsonPrimitive(it) }))
}
