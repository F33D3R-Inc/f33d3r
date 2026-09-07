package com.f33d3r.app.compose

import android.content.Context
import android.net.Uri
import android.provider.OpenableColumns
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import java.io.File

/**
 * A file the photo picker handed over, copied into the app's cache so the upload
 * lanes can read it as a plain file with a known name and type.
 *
 * The picker grants a one-shot content URI. Copying is what turns it into something
 * that can be hashed, sent resumably and retried after the grant is gone.
 */
data class PickedFile(
    val file: File,
    val name: String,
    val mimeType: String,
) {
    val isVideo: Boolean get() = mimeType.startsWith("video/")
    val isImage: Boolean get() = mimeType.startsWith("image/")

    companion object {
        suspend fun from(context: Context, uri: Uri): PickedFile? = withContext(Dispatchers.IO) {
            runCatching {
                val resolver = context.contentResolver
                val mime = resolver.getType(uri).orEmpty()
                val name = resolver.query(uri, arrayOf(OpenableColumns.DISPLAY_NAME), null, null, null)
                    ?.use { c -> if (c.moveToFirst()) c.getString(0) else null }
                    ?.takeIf { it.isNotBlank() }
                    ?: ("picked-" + System.currentTimeMillis() + extensionFor(mime))
                val target = File(context.cacheDir, "compose-" + System.currentTimeMillis() + "-" + name.filter { it.isLetterOrDigit() || it == '.' || it == '-' || it == '_' })
                resolver.openInputStream(uri)?.use { input ->
                    target.outputStream().use { output -> input.copyTo(output) }
                } ?: return@runCatching null
                PickedFile(target, name, mime)
            }.getOrNull()
        }

        private fun extensionFor(mime: String): String = when (mime) {
            "image/png" -> ".png"
            "image/gif" -> ".gif"
            "image/webp" -> ".webp"
            "video/mp4" -> ".mp4"
            "video/webm" -> ".webm"
            "video/quicktime" -> ".mov"
            else -> if (mime.startsWith("video/")) ".mp4" else ".jpg"
        }
    }
}
