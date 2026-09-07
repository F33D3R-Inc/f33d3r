package com.f33d3r.app.live

import android.content.Context
import androidx.media3.common.MediaItem
import androidx.media3.common.util.UnstableApi
import androidx.media3.datasource.okhttp.OkHttpDataSource
import androidx.media3.exoplayer.ExoPlayer
import androidx.media3.exoplayer.hls.HlsMediaSource
import com.f33d3r.app.net.Http
import com.f33d3r.app.net.Session

/**
 * The live picture, decoded natively.
 *
 * The watch surface's own Facets — title, badge, tally, author, chat — are the
 * server's and are projected over this. The decoder is the platform's job, exactly
 * as it is the browser's on the web: it plays the ladder the server names for the
 * stream and remembers nothing about the stream. It outlives the overlay so the
 * sound continues when the picture is put away in the mini-player.
 */
@androidx.annotation.OptIn(UnstableApi::class)
class HlsPlayback(context: Context, session: Session) {

    private val dataSource = OkHttpDataSource.Factory(Http.client(session))
        .setUserAgent(Http.USER_AGENT)

    val player: ExoPlayer = ExoPlayer.Builder(context.applicationContext).build()

    private var current = ""

    /** Plays [url], a live HLS master. Re-issuing the same url leaves playback alone. */
    fun play(url: String) {
        if (url == current && player.isPlaying) return
        current = url
        val source = HlsMediaSource.Factory(dataSource).createMediaSource(MediaItem.fromUri(url))
        player.setMediaSource(source)
        player.prepare()
        player.playWhenReady = true
    }

    fun stop() {
        current = ""
        player.stop()
        player.clearMediaItems()
    }

    fun setMuted(muted: Boolean) {
        player.volume = if (muted) 0f else 1f
    }

    fun release() {
        stop()
        player.release()
    }
}
