package com.f33d3r.app.live

import com.f33d3r.app.core.Endpoints
import com.f33d3r.app.net.Http
import com.f33d3r.app.net.Session
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody

/**
 * WHIP, as this server proxies it.
 *
 * The phone never learns the media server's address or the stream's publish
 * credential. The SDP offer goes to the same-origin endpoint the session already
 * has authority on; the server attaches the credential, forwards the offer, and
 * hands the answer back with a same-origin resource path for the teardown. This
 * class carries those two calls and holds nothing between them.
 */
class Whip(
    private val session: Session,
    private val http: OkHttpClient = Http.client(session),
) {

    /** The media server's answer, and the same-origin address of the session it opened. */
    data class Answer(val sdp: String, val resource: String)

    suspend fun publish(streamId: String, offerSdp: String): Result<Answer> =
        withContext(Dispatchers.IO) {
            runCatching {
                val request = Request.Builder()
                    .url(session.origin.trimEnd('/') + Endpoints.liveWhip(streamId))
                    .post(offerSdp.toRequestBody(SDP))
                    .build()
                http.newCall(request).execute().use { response ->
                    val body = response.body.string()
                    if (response.code != 201 && response.code != 200) {
                        error("ingest refused (${response.code}): ${body.trim()}")
                    }
                    if (body.isBlank()) error("the media server answered with no SDP")
                    Answer(sdp = body, resource = response.header("Location").orEmpty())
                }
            }
        }

    /**
     * Releases the WebRTC session at the media server. Best effort: the media server
     * also notices a closed peer, so a failure here costs nothing but tidiness.
     */
    suspend fun teardown(resource: String) = withContext(Dispatchers.IO) {
        if (resource.isEmpty()) return@withContext
        runCatching {
            val request = Request.Builder()
                .url(session.origin.trimEnd('/') + resource)
                .delete()
                .build()
            http.newCall(request).execute().close()
        }
        Unit
    }

    private companion object {
        val SDP = "application/sdp".toMediaType()
    }
}
