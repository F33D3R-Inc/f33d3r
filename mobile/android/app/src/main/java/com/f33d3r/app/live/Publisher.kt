package com.f33d3r.app.live

import android.content.Context
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.suspendCancellableCoroutine
import kotlinx.coroutines.withContext
import kotlinx.coroutines.withTimeoutOrNull
import org.webrtc.AudioSource
import org.webrtc.AudioTrack
import org.webrtc.Camera2Enumerator
import org.webrtc.CameraVideoCapturer
import org.webrtc.DataChannel
import org.webrtc.DefaultVideoDecoderFactory
import org.webrtc.DefaultVideoEncoderFactory
import org.webrtc.EglBase
import org.webrtc.IceCandidate
import org.webrtc.MediaConstraints
import org.webrtc.MediaStream
import org.webrtc.MediaStreamTrack
import org.webrtc.PeerConnection
import org.webrtc.PeerConnectionFactory
import org.webrtc.RtpParameters
import org.webrtc.RtpTransceiver
import org.webrtc.SdpObserver
import org.webrtc.SessionDescription
import org.webrtc.SurfaceTextureHelper
import org.webrtc.VideoSink
import org.webrtc.VideoSource
import org.webrtc.VideoTrack
import org.webrtc.audio.JavaAudioDeviceModule
import kotlin.coroutines.resume
import kotlin.coroutines.resumeWithException

/** Where the broadcast's own transmission stands. Stream status stays the server's. */
sealed interface PublishState {
    data object Idle : PublishState
    data object OpeningCamera : PublishState
    data object Connecting : PublishState
    data object Publishing : PublishState
    data class Failed(val reason: String) : PublishState
    data object Stopped : PublishState
}

/**
 * The browser-source broadcaster, natively.
 *
 * The web surface opens the camera through getUserMedia and publishes over WebRTC
 * with a single, complete WHIP offer. This is the same leg with the platform's own
 * camera and encoder underneath it: capture, one offer, one answer, publish. It
 * reports only the transmission's own state. Whether the broadcast is LIVE is the
 * server's answer — it turns live when the media server reports frames arriving —
 * and it reaches the stage as a Fragment, never from here.
 */
class Publisher(
    private val context: Context,
    private val whip: Whip,
) {

    private val _state = MutableStateFlow<PublishState>(PublishState.Idle)
    val state: StateFlow<PublishState> = _state.asStateFlow()

    private val _front = MutableStateFlow(false)
    /** Whether the front camera is capturing; the renderer mirrors accordingly. */
    val front: StateFlow<Boolean> = _front.asStateFlow()

    private val _muted = MutableStateFlow(false)
    val muted: StateFlow<Boolean> = _muted.asStateFlow()

    /** Shared GL context for the encoder and the on-screen renderer. */
    val eglBase: EglBase by lazy { EglBase.create() }

    private var factory: PeerConnectionFactory? = null
    private var audioModule: JavaAudioDeviceModule? = null
    private var capturer: CameraVideoCapturer? = null
    private var helper: SurfaceTextureHelper? = null
    private var videoSource: VideoSource? = null
    private var audioSource: AudioSource? = null
    private var videoTrack: VideoTrack? = null
    private var audioTrack: AudioTrack? = null
    private var peer: PeerConnection? = null
    private var resource = ""
    private var sink: VideoSink? = null
    private var gathered = CompletableDeferred<Unit>()

    /** Draws the local picture into [renderer]; one renderer at a time. */
    fun attach(renderer: VideoSink) {
        sink?.let { videoTrack?.removeSink(it) }
        sink = renderer
        videoTrack?.addSink(renderer)
    }

    fun detach() {
        sink?.let { videoTrack?.removeSink(it) }
        sink = null
    }

    /**
     * Opens the camera and publishes the stream. Suspends until the media server has
     * answered the offer or refused it; the state flow tells the surface what happened.
     */
    suspend fun start(streamId: String, frontCamera: Boolean) = withContext(Dispatchers.Default) {
        if (peer != null) stop()
        _state.value = PublishState.OpeningCamera
        try {
            val factory = factory ?: buildFactory().also { factory = it }
            openCamera(factory, frontCamera)
            _state.value = PublishState.Connecting

            gathered = CompletableDeferred()
            val config = PeerConnection.RTCConfiguration(emptyList()).apply {
                sdpSemantics = PeerConnection.SdpSemantics.UNIFIED_PLAN
                bundlePolicy = PeerConnection.BundlePolicy.MAXBUNDLE
                rtcpMuxPolicy = PeerConnection.RtcpMuxPolicy.REQUIRE
            }
            val pc = factory.createPeerConnection(config, observer)
                ?: error("this device cannot open a WebRTC connection")
            peer = pc
            val sendOnly = RtpTransceiver.RtpTransceiverInit(RtpTransceiver.RtpTransceiverDirection.SEND_ONLY)
            val videoTransceiver = pc.addTransceiver(videoTrack, sendOnly)
            pc.addTransceiver(audioTrack, sendOnly)
            preferH264(factory, videoTransceiver)

            val offer = pc.createOfferAwait()
            pc.setLocalAwait(offer)
            // Non-trickle ICE: the whole offer goes once, complete. A candidate that
            // never arrives must not hang the broadcast, so gathering is bounded.
            withTimeoutOrNull(GATHER_TIMEOUT_MILLIS) { gathered.await() }
            val local = pc.localDescription ?: error("no local description")

            val answer = whip.publish(streamId, local.description).getOrThrow()
            resource = answer.resource
            pc.setRemoteAwait(SessionDescription(SessionDescription.Type.ANSWER, answer.sdp))
            raiseSenderQuality(pc)
            _state.value = PublishState.Publishing
        } catch (t: Throwable) {
            releaseAll()
            _state.value = PublishState.Failed(t.message ?: "the broadcast could not be transmitted")
        }
    }

    /** Switches between the front and rear camera without renegotiating. */
    fun flip() {
        val cam = capturer ?: return
        cam.switchCamera(object : CameraVideoCapturer.CameraSwitchHandler {
            override fun onCameraSwitchDone(isFrontCamera: Boolean) {
                _front.value = isFrontCamera
            }

            override fun onCameraSwitchError(errorDescription: String?) = Unit
        })
    }

    fun setMuted(muted: Boolean) {
        audioTrack?.setEnabled(!muted)
        _muted.value = muted
    }

    /** Ends transmission and releases the camera. The stream itself is ended on the server. */
    suspend fun stop() = withContext(Dispatchers.Default) {
        val res = resource
        resource = ""
        if (res.isNotEmpty()) whip.teardown(res)
        releaseAll()
        _state.value = PublishState.Stopped
    }

    fun release() {
        releaseAll()
        factory?.dispose()
        factory = null
        audioModule?.release()
        audioModule = null
    }

    // ── Setup ─────────────────────────────────────────────────────────────────

    private fun buildFactory(): PeerConnectionFactory {
        initializeOnce(context)
        val adm = JavaAudioDeviceModule.builder(context)
            .setUseHardwareAcousticEchoCanceler(true)
            .setUseHardwareNoiseSuppressor(true)
            .createAudioDeviceModule()
        audioModule = adm
        return PeerConnectionFactory.builder()
            .setAudioDeviceModule(adm)
            .setVideoEncoderFactory(DefaultVideoEncoderFactory(eglBase.eglBaseContext, true, true))
            .setVideoDecoderFactory(DefaultVideoDecoderFactory(eglBase.eglBaseContext))
            .createPeerConnectionFactory()
    }

    private fun openCamera(factory: PeerConnectionFactory, frontCamera: Boolean) {
        val enumerator = Camera2Enumerator(context)
        val names = enumerator.deviceNames
        val name = names.firstOrNull { if (frontCamera) enumerator.isFrontFacing(it) else enumerator.isBackFacing(it) }
            ?: names.firstOrNull()
            ?: error("this device has no camera")
        val cam = enumerator.createCapturer(name, null) ?: error("the camera could not be opened")
        val texture = SurfaceTextureHelper.create("fa-capture", eglBase.eglBaseContext)
        val vsrc = factory.createVideoSource(cam.isScreencast)
        cam.initialize(texture, context, vsrc.capturerObserver)
        cam.startCapture(CAPTURE_WIDTH, CAPTURE_HEIGHT, CAPTURE_FPS)

        val vtrack = factory.createVideoTrack("fa-v0", vsrc)
        val asrc = factory.createAudioSource(MediaConstraints())
        val atrack = factory.createAudioTrack("fa-a0", asrc)
        atrack.setEnabled(!_muted.value)

        capturer = cam
        helper = texture
        videoSource = vsrc
        audioSource = asrc
        videoTrack = vtrack
        audioTrack = atrack
        _front.value = enumerator.isFrontFacing(name)
        sink?.let(vtrack::addSink)
    }

    /**
     * The same ceilings the web leg asks for: keep the resolution the camera captured
     * and drop frame rate first when the uplink cannot carry it.
     */
    /**
     * Puts H.264 first in the offer. The phone's hardware encoder produces it
     * natively, and it is the one codec the server can hand to viewers without
     * re-encoding: a VP8 broadcast forces every server to decode and encode it
     * again, which is the whole GPU bill. Left to its defaults libwebrtc offers
     * VP8 first and the media server takes the first codec it is offered.
     */
    private fun preferH264(factory: PeerConnectionFactory, transceiver: RtpTransceiver) {
        val codecs = factory.getRtpSenderCapabilities(MediaStreamTrack.MediaType.MEDIA_TYPE_VIDEO).codecs
        val h264 = codecs.filter { it.name.equals("H264", ignoreCase = true) }
        if (h264.isEmpty()) return
        transceiver.setCodecPreferences(h264 + codecs.filterNot { it.name.equals("H264", ignoreCase = true) })
    }

    private fun raiseSenderQuality(pc: PeerConnection) {
        pc.senders.forEach { sender ->
            val track = sender.track() ?: return@forEach
            val params = sender.parameters
            if (params.encodings.isEmpty()) return@forEach
            if (track.kind() == "video") {
                params.encodings[0].maxBitrateBps = VIDEO_MAX_BITRATE
                params.degradationPreference = RtpParameters.DegradationPreference.MAINTAIN_RESOLUTION
            } else {
                params.encodings[0].maxBitrateBps = AUDIO_MAX_BITRATE
            }
            sender.parameters = params
        }
    }

    private fun releaseAll() {
        runCatching { peer?.close() }
        runCatching { peer?.dispose() }
        peer = null
        runCatching { capturer?.stopCapture() }
        runCatching { capturer?.dispose() }
        capturer = null
        sink?.let { s -> runCatching { videoTrack?.removeSink(s) } }
        runCatching { videoTrack?.dispose() }
        videoTrack = null
        runCatching { audioTrack?.dispose() }
        audioTrack = null
        runCatching { videoSource?.dispose() }
        videoSource = null
        runCatching { audioSource?.dispose() }
        audioSource = null
        runCatching { helper?.dispose() }
        helper = null
    }

    private val observer = object : PeerConnection.Observer {
        override fun onIceGatheringChange(state: PeerConnection.IceGatheringState?) {
            if (state == PeerConnection.IceGatheringState.COMPLETE && !gathered.isCompleted) {
                gathered.complete(Unit)
            }
        }

        override fun onConnectionChange(newState: PeerConnection.PeerConnectionState?) {
            when (newState) {
                PeerConnection.PeerConnectionState.FAILED ->
                    _state.value = PublishState.Failed("the connection to the media server failed")

                PeerConnection.PeerConnectionState.CONNECTED ->
                    if (_state.value == PublishState.Connecting) _state.value = PublishState.Publishing

                else -> Unit
            }
        }

        override fun onSignalingChange(state: PeerConnection.SignalingState?) = Unit
        override fun onIceConnectionChange(state: PeerConnection.IceConnectionState?) = Unit
        override fun onIceConnectionReceivingChange(receiving: Boolean) = Unit
        override fun onIceCandidate(candidate: IceCandidate?) = Unit
        override fun onIceCandidatesRemoved(candidates: Array<out IceCandidate>?) = Unit
        override fun onAddStream(stream: MediaStream?) = Unit
        override fun onRemoveStream(stream: MediaStream?) = Unit
        override fun onDataChannel(channel: DataChannel?) = Unit
        override fun onRenegotiationNeeded() = Unit
    }

    // ── Suspending wrappers over the callback API ─────────────────────────────

    private suspend fun PeerConnection.createOfferAwait(): SessionDescription =
        suspendCancellableCoroutine { cont ->
            createOffer(object : SdpObserver {
                override fun onCreateSuccess(sdp: SessionDescription) { cont.resume(sdp) }
                override fun onCreateFailure(error: String?) {
                    cont.resumeWithException(IllegalStateException(error ?: "offer failed"))
                }
                override fun onSetSuccess() = Unit
                override fun onSetFailure(error: String?) = Unit
            }, MediaConstraints())
        }

    private suspend fun PeerConnection.setLocalAwait(sdp: SessionDescription) =
        suspendCancellableCoroutine { cont ->
            setLocalDescription(object : SdpObserver {
                override fun onSetSuccess() { cont.resume(Unit) }
                override fun onSetFailure(error: String?) {
                    cont.resumeWithException(IllegalStateException(error ?: "local description refused"))
                }
                override fun onCreateSuccess(sdp: SessionDescription?) = Unit
                override fun onCreateFailure(error: String?) = Unit
            }, sdp)
        }

    private suspend fun PeerConnection.setRemoteAwait(sdp: SessionDescription) =
        suspendCancellableCoroutine { cont ->
            setRemoteDescription(object : SdpObserver {
                override fun onSetSuccess() { cont.resume(Unit) }
                override fun onSetFailure(error: String?) {
                    cont.resumeWithException(IllegalStateException(error ?: "answer refused"))
                }
                override fun onCreateSuccess(sdp: SessionDescription?) = Unit
                override fun onCreateFailure(error: String?) = Unit
            }, sdp)
        }

    companion object {
        private const val CAPTURE_WIDTH = 1280
        private const val CAPTURE_HEIGHT = 720
        private const val CAPTURE_FPS = 30
        private const val GATHER_TIMEOUT_MILLIS = 4_000L
        private const val VIDEO_MAX_BITRATE = 6_000_000
        private const val AUDIO_MAX_BITRATE = 128_000

        @Volatile
        private var initialized = false

        private fun initializeOnce(context: Context) {
            if (initialized) return
            synchronized(this) {
                if (initialized) return
                PeerConnectionFactory.initialize(
                    PeerConnectionFactory.InitializationOptions.builder(context.applicationContext)
                        .createInitializationOptions()
                )
                initialized = true
            }
        }
    }
}
