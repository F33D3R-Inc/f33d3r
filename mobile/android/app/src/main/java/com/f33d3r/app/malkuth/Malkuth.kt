package com.f33d3r.app.malkuth

import com.f33d3r.app.core.Endpoints
import com.f33d3r.app.net.FacetClient
import com.f33d3r.app.net.JsonAnswer
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.delay
import kotlinx.coroutines.withContext
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonObjectBuilder
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.jsonPrimitive
import kotlinx.serialization.json.put
import java.security.MessageDigest
import java.time.Instant
import java.time.format.DateTimeParseException
import java.util.UUID

/**
 * A Work as its author signs it: the nineteen fields of the server's
 * `workCanonicalPayload` (`feed-engine/internal/handler/work_event.go`), less the two
 * that are constants.
 *
 * Every field here is inside the content identifier, so every field here is something
 * the author is accountable for. `is_repost` and `repost_source_id` are hashed too, but
 * as the constants `false` and `null`: a repost is a reaction, never a signed work, and
 * the server refuses an envelope that sets them, so nothing here can.
 *
 * [timestampMs] is stamped when the work is signed, which is at send time — the server
 * accepts a work signed within the last hour. Body and poll options are normalised
 * ([WorkText]) when the work is signed.
 */
data class WorkPayload(
    /** The account the work is published as; the server requires the session's own. */
    val authorAccount: String,
    val body: String = "",
    /** The server's vocabulary: [CommentGating]. */
    val commentGating: String = CommentGating.EVERYONE,
    val kind: String = "post",
    val mediaUrls: List<String> = emptyList(),
    val parentCid: String? = null,
    /** RFC 3339; required on a poll, 5 minutes to 7 days after the work opens. */
    val pollEndsAt: String? = null,
    val pollOptions: List<String>? = null,
    val scheduledAt: String? = null,
    val subscriberOnly: Boolean = false,
    val tags: List<String> = emptyList(),
    val timestampMs: Long = 0,
    val videoDurationSecs: Long? = null,
    val videoMasterUrl: String? = null,
    val videoPosterUrl: String? = null,
    val voiceDurationSecs: Long? = null,
    val voiceUrl: String? = null,
) {
    /** What is actually signed: the text normalised, the clock read now. */
    fun preparedForSigning(nowMs: Long): WorkPayload = copy(
        body = WorkText.normalize(body),
        pollOptions = pollOptions?.map(WorkText::normalize),
        timestampMs = nowMs,
    )
}

/** Who may reply: `dbpkg.CommentGatings`, the closed vocabulary the server enforces. */
object CommentGating {
    const val EVERYONE = "everyone"
    const val FOLLOWERS = "followers"
    const val CIRCLE = "circle"
    const val VERIFIED = "verified"
    const val NONE = "none"

    /** In the server's order, with the labels the server gives them. */
    val ALL: List<Pair<String, String>> = listOf(
        EVERYONE to "Everyone",
        FOLLOWERS to "Your followers",
        CIRCLE to "Your circle (you follow each other)",
        VERIFIED to "Verified accounts",
        NONE to "Only you",
    )

    fun isValid(value: String): Boolean = ALL.any { it.first == value }
}

/**
 * The fields sent beside the signed payload: what the server's `parseWorkEnvelope`
 * reads outside the signature. Nothing else is sent — the downloadable file and the
 * upload binding are the media registry's, never the client's.
 */
data class WorkAttachments(
    val isNsfw: Boolean = false,
    val videoWidth: Int = 0,
    val videoHeight: Int = 0,
    val quotedWorkId: String? = null,
    val reactLayout: String? = null,
)

/** The closed vocabulary of create outcomes, as `X-Work-Create-State` names them. */
enum class WorkCreateState(val wire: String) {
    OK("ok"),
    DUPLICATE("duplicate"),
    TEXT_TOO_LONG("text_too_long"),
    TEXT_BLANK("text_blank"),
    INVALID_FIELD("invalid_field"),
    CITED_NOT_FOUND("cited_not_found"),
    REPLIES_RESTRICTED("replies_restricted"),
    POSTING_RESTRICTED("posting_restricted"),
    RATE_LIMITED("rate_limited"),
    SERVER_ERROR("server_error");

    companion object {
        /** The header when the server sent one, else the state its status stands for. */
        fun of(header: String?, status: Int): WorkCreateState =
            entries.firstOrNull { it.wire == header } ?: when (status) {
                200, 201 -> OK
                409 -> DUPLICATE
                422 -> TEXT_TOO_LONG
                400 -> INVALID_FIELD
                404 -> CITED_NOT_FOUND
                403 -> POSTING_RESTRICTED
                429 -> RATE_LIMITED
                else -> SERVER_ERROR
            }
    }
}

/**
 * What the server said about a Work it was asked to store.
 *
 * On success the server answers with the identifiers it assigned — the row id and the
 * content id it verified — and no render. What the timeline should show is the
 * timeline's next read; the receipt is only proof, and what a thread chains on.
 */
sealed interface Publication {
    data class Stored(val workId: String, val cid: String) : Publication
    data object Unauthorized : Publication

    /** Refused before or by the server, in words for the author. Nothing was written. */
    data class Refused(val message: String, val state: WorkCreateState? = null) : Publication

    /**
     * The server never answered, or answered that it broke: the work may or may not have
     * landed. Sending the same draft again is safe — see [Malkuth.publish].
     */
    data class Unconfirmed(val message: String) : Publication
}

/**
 * Malkuth — the lane that publishes a Work under the author's own key.
 *
 * A work is not "posted by whoever holds the session". It is hashed into a content
 * identifier, that identifier is signed by a key only this device can use, and the
 * server verifies the signature against the key authority before it stores anything.
 */
class Malkuth(private val client: FacetClient) {

    /**
     * Registers [account]'s public key with the key authority. Returns null when it
     * was registered, or the reason it was not.
     *
     * Required before the first publish, and repeated at every sign-in and launch: the
     * server verifies against the key it was last handed.
     */
    suspend fun registerDevice(account: String): String? = withContext(Dispatchers.IO) {
        val publicKey = try {
            DeviceKey(account).publicKeyB64()
        } catch (e: Exception) {
            return@withContext "This device could not make a signing key: ${e.message ?: e.javaClass.simpleName}"
        }
        val payload = buildJsonObject {
            put("public_key_b64", publicKey)
            put("algorithm", "ECDSA-P256")
        }.toString()
        when (val answer = client.exchangeJson(Endpoints.SIGNING_KEY_REGISTER, payload)) {
            is JsonAnswer.Ok -> null
            JsonAnswer.Unauthorized -> "Sign in again so this device can sign works."
            is JsonAnswer.Refused ->
                if (answer.code == 502 || answer.message.contains("key_authority_unavailable")) {
                    "The key authority isn't reachable, so this device can't sign works yet."
                } else {
                    "This device's signing key was not registered: ${answer.message.take(200)}"
                }
            is JsonAnswer.Unreachable -> "This device's signing key could not be registered: the server is unreachable."
        }
    }

    /** Destroys [account]'s signing key on this device. */
    fun forget(account: String) {
        if (account.isNotBlank()) DeviceKey(account).forget()
    }

    /**
     * Computes the content identifier for [payload].
     *
     * `media_urls` is sorted by UTF-8 byte, as Go's `sort.Strings` sorts it before
     * hashing: the set of attachments is the claim, not the order they were added in.
     */
    fun contentId(payload: WorkPayload): String {
        val digest = MessageDigest.getInstance("SHA-256")
            .digest(canonical(payload).toByteArray(Charsets.UTF_8))
        return "sha256:" + digest.joinToString("") { "%02x".format(it) }
    }

    internal fun canonical(payload: WorkPayload): String = CanonicalJson().obj {
        put("author_account", payload.authorAccount)
        put("body", payload.body)
        put("comment_gating", payload.commentGating)
        put("is_repost", false)
        put("kind", payload.kind)
        putStrings("media_urls", sortedByUtf8(payload.mediaUrls))
        putNullable("parent_cid", payload.parentCid)
        putNullable("poll_ends_at", payload.pollEndsAt)
        putNullableStrings("poll_options", payload.pollOptions)
        putNullable("repost_source_id", null as String?)
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

    /**
     * Signs and publishes a Work, returning the server's receipt for it.
     *
     * The work is validated, normalised and signed now — at send time — and those bytes
     * are what every retry sends: the server answers a re-delivered content id with the
     * work it already created, so resending can never make a second work. A transport
     * failure or a server fault is retried with the same bytes; a rate limit waits
     * longer first; a Duplicate and every other refusal are final. The one time a work
     * is signed again is when the server says its signature is too old, which proves the
     * first never landed.
     */
    suspend fun publish(
        eventType: String,
        draft: WorkPayload,
        attachments: WorkAttachments = WorkAttachments(),
        attempts: Int = 3,
    ): Publication = withContext(Dispatchers.IO) {
        var resigned = false
        var envelope = when (val signed = sign(eventType, draft, attachments)) {
            is Signed.Envelope -> signed
            is Signed.Refused -> return@withContext signed.refusal
        }
        var attempt = 0
        var last: Publication = Publication.Unconfirmed("not attempted")
        while (attempt < attempts.coerceAtLeast(1)) {
            val answer = client.exchangeJson(Endpoints.EVENTS, envelope.body)
            when (answer) {
                is JsonAnswer.Ok -> {
                    val receipt = answer.body as? JsonObject
                    val storedCid = receipt?.get("cid")?.jsonPrimitive?.content.orEmpty()
                    val workId = receipt?.get("work_id")?.jsonPrimitive?.content.orEmpty()
                    if (storedCid != envelope.cid || workId.isEmpty()) {
                        return@withContext Publication.Refused(
                            "The server stored the post under a different content id than was signed.",
                        )
                    }
                    return@withContext Publication.Stored(workId, storedCid)
                }
                JsonAnswer.Unauthorized -> return@withContext Publication.Unauthorized
                is JsonAnswer.Unreachable -> {
                    last = Publication.Unconfirmed("The server could not be reached.")
                    if (attempt < attempts - 1) delay(backoffMs(attempt))
                }
                is JsonAnswer.Refused -> {
                    val state = if (answer.state != null) WorkCreateState.of(answer.state, answer.code) else null
                    when {
                        // Not the create path's own answer: a proxy, or the server failing.
                        state == null && answer.code >= 500 ||
                            state == WorkCreateState.SERVER_ERROR -> {
                            last = Publication.Unconfirmed(answer.message)
                            if (attempt < attempts - 1) delay(backoffMs(attempt))
                        }
                        state == WorkCreateState.RATE_LIMITED -> {
                            last = Publication.Refused(answer.message, state)
                            if (attempt < attempts - 1) delay(backoffMs(attempt + 3))
                        }
                        state == WorkCreateState.INVALID_FIELD && invalidField(answer.message) == "timestamp_ms" -> {
                            if (resigned) {
                                return@withContext Publication.Refused(
                                    "This device's clock is wrong, so the post cannot be signed. " +
                                        "Set the date and time automatically and try again.",
                                    state,
                                )
                            }
                            resigned = true
                            envelope = when (val again = sign(eventType, draft, attachments)) {
                                is Signed.Envelope -> again
                                is Signed.Refused -> return@withContext again.refusal
                            }
                            continue
                        }
                        else -> return@withContext Publication.Refused(answer.message.take(400), state)
                    }
                }
            }
            attempt++
        }
        last
    }

    private sealed interface Signed {
        data class Envelope(val cid: String, val body: String) : Signed
        data class Refused(val refusal: Publication.Refused) : Signed
    }

    /** Validates, normalises, stamps, hashes and signs — in that order. */
    private fun sign(eventType: String, draft: WorkPayload, attachments: WorkAttachments): Signed {
        val payload = draft.preparedForSigning(System.currentTimeMillis())
        validate(payload)?.let { return Signed.Refused(Publication.Refused(it)) }
        val cid = contentId(payload)
        val signature = try {
            DeviceKey(payload.authorAccount).sign(cid)
        } catch (e: Exception) {
            return Signed.Refused(
                Publication.Refused("This device cannot sign: ${e.message ?: e.javaClass.simpleName}"),
            )
        }
        val body = buildJsonObject {
            put("event_type", eventType)
            put("cid", cid)
            put("signature", signature)
            put("is_nsfw", attachments.isNsfw)
            if (attachments.videoWidth > 0) put("video_width", attachments.videoWidth)
            if (attachments.videoHeight > 0) put("video_height", attachments.videoHeight)
            attachments.quotedWorkId?.let { put("quoted_work_id", it) }
            attachments.reactLayout?.let { put("react_layout", it) }
            put("payload", buildJsonObject {
                put("author_account", payload.authorAccount)
                put("body", payload.body)
                put("comment_gating", payload.commentGating)
                put("is_repost", false)
                put("kind", payload.kind)
                putJsonStrings("media_urls", sortedByUtf8(payload.mediaUrls))
                put("parent_cid", payload.parentCid)
                put("poll_ends_at", payload.pollEndsAt)
                if (payload.pollOptions == null) put("poll_options", null as String?)
                else putJsonStrings("poll_options", payload.pollOptions)
                put("repost_source_id", null as String?)
                put("scheduled_at", payload.scheduledAt)
                put("subscriber_only", payload.subscriberOnly)
                putJsonStrings("tags", payload.tags)
                put("timestamp_ms", payload.timestampMs)
                put("video_duration_secs", payload.videoDurationSecs)
                put("video_master_url", payload.videoMasterUrl)
                put("video_poster_url", payload.videoPosterUrl)
                put("voice_duration_secs", payload.voiceDurationSecs)
                put("voice_url", payload.voiceUrl)
            })
        }.toString()
        return Signed.Envelope(cid, body)
    }

    companion object {
        private val CID = Regex("^sha256:[0-9a-f]{64}$")
        private const val POLL_MIN_SECONDS = 5L * 60
        private const val POLL_MAX_SECONDS = 7L * 24 * 60 * 60

        /**
         * Why the server would refuse [payload], or null. Everything this client can
         * know is checked here, so a round trip is spent only on what only the server
         * knows.
         */
        fun validate(payload: WorkPayload): String? {
            runCatching { UUID.fromString(payload.authorAccount) }.onFailure {
                return "This device does not know which account it is signed in as. Sign in again."
            }
            if (!CommentGating.isValid(payload.commentGating)) return "Pick who can reply."
            payload.parentCid?.let { if (!CID.matches(it)) return "The post being answered has no valid content id." }
            if (payload.kind == "reply" && payload.parentCid == null) return "A reply needs the post it answers."
            if (!WorkText.isNormalized(payload.body)) return "The text could not be prepared for signing."
            val remaining = WorkText.remaining(payload.body)
            if (remaining < 0) return "Your post is ${-remaining} over the limit."
            payload.scheduledAt?.let { if (parseInstant(it) == null) return "The scheduled time is not a valid time." }
            return validatePoll(payload)
        }

        private fun validatePoll(payload: WorkPayload): String? {
            val options = payload.pollOptions
            if (options == null) {
                if (payload.kind == "poll") return "A poll needs options."
                if (payload.pollEndsAt != null) return "Only a poll has an end time."
                return null
            }
            if (payload.kind != "poll") return "Only a poll has options."
            if (options.size !in 2..4) return "A poll has 2 to 4 options."
            val seen = HashSet<String>()
            for (option in options) {
                if (!WorkText.isNormalized(option) || !WorkText.isSingleLine(option) || WorkText.isBlank(option)) {
                    return "Every option needs text, on one line."
                }
                if (WorkText.codePoints(option) > WorkText.POLL_OPTION_MAX_CODE_POINTS) {
                    return "Each option is at most ${WorkText.POLL_OPTION_MAX_CODE_POINTS} characters."
                }
                if (!seen.add(option.lowercase())) return "Options must differ."
            }
            val ends = payload.pollEndsAt?.let(::parseInstant) ?: return "A poll needs an end time."
            val opens = payload.scheduledAt?.let(::parseInstant) ?: Instant.ofEpochMilli(payload.timestampMs)
            val runs = ends.epochSecond - opens.epochSecond
            if (runs < POLL_MIN_SECONDS || runs > POLL_MAX_SECONDS) return "A poll runs from 5 minutes to 7 days."
            return null
        }

        private fun parseInstant(value: String): Instant? = try {
            java.time.OffsetDateTime.parse(value).toInstant()
        } catch (_: DateTimeParseException) {
            null
        }

        /** Go's `sort.Strings`: lexicographic by UTF-8 byte, not by UTF-16 unit. */
        internal fun sortedByUtf8(values: List<String>): List<String> =
            values.map { it to it.toByteArray(Charsets.UTF_8) }
                .sortedWith { a, b ->
                    val x = a.second
                    val y = b.second
                    var i = 0
                    while (i < x.size && i < y.size) {
                        val c = (x[i].toInt() and 0xFF) - (y[i].toInt() and 0xFF)
                        if (c != 0) return@sortedWith c
                        i++
                    }
                    x.size - y.size
                }
                .map { it.first }

        /** The field an `invalid_field` refusal names: `This post could not be sent: <field> — <detail>.` */
        internal fun invalidField(message: String): String? {
            val prefix = "This post could not be sent: "
            if (!message.startsWith(prefix)) return null
            val rest = message.removePrefix(prefix)
            val dash = rest.indexOf(" — ")
            return if (dash < 0) null else rest.substring(0, dash)
        }

        private fun backoffMs(attempt: Int): Long = 1000L shl attempt
    }
}

private fun JsonObjectBuilder.putJsonStrings(key: String, values: List<String>) {
    put(key, JsonArray(values.map { JsonPrimitive(it) }))
}
