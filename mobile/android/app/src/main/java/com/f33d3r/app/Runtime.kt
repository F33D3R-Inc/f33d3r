package com.f33d3r.app

import android.content.Context
import com.f33d3r.app.compose.DraftStore
import com.f33d3r.app.live.HlsPlayback
import com.f33d3r.app.live.Publisher
import com.f33d3r.app.live.Whip
import com.f33d3r.app.net.FacetClient
import com.f33d3r.app.net.Http
import com.f33d3r.app.net.Session
import com.f33d3r.app.net.live.FaLive
import com.f33d3r.app.pial.DeviceKey
import com.f33d3r.app.pial.Malkuth
import com.f33d3r.app.seal.Gnosis
import com.f33d3r.app.upload.VideoUploader
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.plus
import okhttp3.OkHttpClient

/**
 * The parts of the client that outlive any screen.
 *
 * FA Live in particular: the connection is the application, so it is opened once for
 * the session and belongs to the process, not to whatever surface happens to be
 * visible. Tying it to a screen would tear it down on every navigation and reconnect
 * on every return — which the server would see as connection churn and answer by
 * evicting the device's own live stream.
 */
class Runtime(context: Context) {

    /** The application context, for the parts that need a platform handle: camera, media. */
    val context: Context = context.applicationContext

    /** Lives as long as the process; cancelled only when the process ends. */
    val scope = CoroutineScope(SupervisorJob() + kotlinx.coroutines.Dispatchers.Default)

    val session: Session = Session(context)
    val http: OkHttpClient = Http.client(session)
    val client: FacetClient = FacetClient(session, http)
    val live: FaLive = FaLive(session, scope)
    val gnosis: Gnosis = Gnosis(session, client)
    val deviceKey: DeviceKey = DeviceKey()
    val malkuth: Malkuth = Malkuth(client, deviceKey)
    val whip: Whip = Whip(session, http)
    val uploader: VideoUploader = VideoUploader(this.context, session, client, http)

    /** The composer's drafts, kept on this device only. */
    val drafts: DraftStore = DraftStore(this.context)

    /**
     * The broadcast publisher and the live decoder. Created on first use and kept:
     * a broadcast in progress and a stream playing in the mini-player both have to
     * survive every navigation inside the Shell, exactly as the connection does.
     */
    val publisher: Publisher by lazy { Publisher(this.context, whip) }
    val playback: HlsPlayback by lazy { HlsPlayback(this.context, session) }

    init {
        session.restore()
    }
}
