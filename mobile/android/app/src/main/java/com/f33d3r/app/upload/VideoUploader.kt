package com.f33d3r.app.upload

import android.content.Context
import com.f33d3r.app.core.Endpoints
import com.f33d3r.app.net.FacetClient
import com.f33d3r.app.net.Session
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.delay
import kotlinx.coroutines.withContext
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import okhttp3.OkHttpClient
import java.io.File

/**
 * A captured clip on its way to being a platform video.
 *
 * Bytes go up resumably; then the transcoder has to say the video exists. Only the
 * transcoded master is a thing a Vision or a Work can name, so this waits for that
 * answer and reports the server's states as the server gives them — including the
 * one where the clip is already on the platform and belongs to someone else.
 */
class VideoUploader(
    context: Context,
    session: Session,
    private val client: FacetClient,
    http: OkHttpClient,
) {

    private val tus = TusSession(context, session, http)

    suspend fun upload(
        file: File,
        filename: String,
        onState: (UploadState) -> Unit,
    ): UploadState = withContext(Dispatchers.IO) {
        val id = tus.open(file, filename)
            ?: return@withContext UploadState.Refused("The upload could not be started.")

        var offset = tus.offset(id) ?: 0L
        var failures = 0
        val total = file.length()
        onState(UploadState.Sending(offset, total))
        while (offset < total) {
            val next = tus.send(id, file, offset) { sent, all -> onState(UploadState.Sending(sent, all)) }
            if (next == null) {
                failures++
                if (failures > SEND_ATTEMPTS) {
                    return@withContext UploadState.Refused("The connection dropped too many times. Try again.")
                }
                delay(RETRY_MILLIS)
                offset = tus.offset(id) ?: offset
                continue
            }
            offset = next
        }

        onState(UploadState.Processing)
        repeat(POLL_ATTEMPTS) {
            val status = client.readJson(Endpoints.tusStatus(id)) as? JsonObject
            when (status?.text("status")) {
                "ready" -> {
                    val out = status["output"] as? JsonObject ?: return@repeat
                    val master = out.text("master_url")
                    if (master.isEmpty()) return@repeat
                    tus.forget(file)
                    return@withContext UploadState.Transcoded(
                        masterUrl = master,
                        posterUrl = out.text("poster_url"),
                        durationSecs = out.text("duration_seconds").toFloatOrNull()?.toLong() ?: 0L,
                        width = out.text("source_width").toIntOrNull() ?: 0,
                        height = out.text("source_height").toIntOrNull() ?: 0,
                        tusUploadId = id,
                    )
                }

                "duplicate" -> {
                    val orig = status["original"] as? JsonObject
                    tus.forget(file)
                    return@withContext UploadState.Duplicate(
                        masterUrl = orig?.text("master_url").orEmpty(),
                        posterUrl = orig?.text("poster_url").orEmpty(),
                        workId = orig?.text("work_id").orEmpty(),
                        creatorHandle = orig?.text("creator_handle").orEmpty(),
                        creatorName = orig?.text("creator_name").orEmpty(),
                        bySelf = false,
                    )
                }

                "failed", "error", "rejected" -> {
                    tus.forget(file)
                    return@withContext UploadState.Refused(
                        status.text("error").ifEmpty { "The video could not be processed." }
                    )
                }

                else -> Unit
            }
            delay(POLL_MILLIS)
        }
        UploadState.Refused("The video is still processing. Try posting it again in a moment.")
    }

    private fun JsonObject.text(key: String): String =
        this[key]?.jsonPrimitive?.content.orEmpty()

    private companion object {
        const val SEND_ATTEMPTS = 3
        const val RETRY_MILLIS = 1_500L
        const val POLL_MILLIS = 2_000L
        const val POLL_ATTEMPTS = 150
    }
}
