package com.f33d3r.app.ui.capture

import android.Manifest
import android.annotation.SuppressLint
import android.content.Context
import android.content.pm.PackageManager
import androidx.camera.core.CameraSelector
import androidx.camera.core.ImageCapture
import androidx.camera.core.ImageCaptureException
import androidx.camera.core.Preview
import androidx.camera.core.SurfaceRequest
import androidx.camera.lifecycle.ProcessCameraProvider
import androidx.camera.lifecycle.awaitInstance
import androidx.camera.video.FallbackStrategy
import androidx.camera.video.FileOutputOptions
import androidx.camera.video.Quality
import androidx.camera.video.QualitySelector
import androidx.camera.video.Recorder
import androidx.camera.video.Recording
import androidx.camera.video.VideoCapture
import androidx.camera.video.VideoRecordEvent
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.setValue
import androidx.core.content.ContextCompat
import androidx.lifecycle.LifecycleOwner
import java.io.File

/**
 * The phone's camera, for the two creator surfaces that begin with it: composing a
 * Vision and setting up a broadcast.
 *
 * It opens a preview and, when asked, a still and a clip capture. It writes files
 * and hands them back; what a file becomes — a Vision, a Work — is decided by the
 * server when the file is posted. The broadcast itself does not use this rig: WebRTC
 * holds the camera for that leg, so the rig is closed before the publisher opens.
 */
class CameraRig(
    private val context: Context,
    private val owner: LifecycleOwner,
) {

    /** The surface the viewfinder draws; null until the camera is open. */
    var surfaceRequest by mutableStateOf<SurfaceRequest?>(null)
        private set

    var front by mutableStateOf(false)
        private set

    var recording by mutableStateOf(false)
        private set

    private var provider: ProcessCameraProvider? = null
    private var withCapture = false
    private var activeRecording: Recording? = null

    private val preview = Preview.Builder().build().also { p ->
        p.setSurfaceProvider { request -> surfaceRequest = request }
    }

    private val imageCapture = ImageCapture.Builder()
        .setCaptureMode(ImageCapture.CAPTURE_MODE_MINIMIZE_LATENCY)
        .build()

    private val recorder = Recorder.Builder()
        .setQualitySelector(
            QualitySelector.from(Quality.HD, FallbackStrategy.lowerQualityOrHigherThan(Quality.SD))
        )
        .build()

    private val videoCapture = VideoCapture.withOutput(recorder)

    /** Opens the camera facing [front], with still and clip capture when [capture]. */
    suspend fun open(front: Boolean, capture: Boolean) {
        this.front = front
        this.withCapture = capture
        val p = provider ?: ProcessCameraProvider.awaitInstance(context).also { provider = it }
        bind(p)
    }

    fun flip() {
        val p = provider ?: return
        front = !front
        bind(p)
    }

    /** Writes one JPEG to [file] and reports when it is on disk. */
    fun takePhoto(file: File, onDone: (Result<File>) -> Unit) {
        val options = ImageCapture.OutputFileOptions.Builder(file).build()
        imageCapture.takePicture(
            options,
            ContextCompat.getMainExecutor(context),
            object : ImageCapture.OnImageSavedCallback {
                override fun onImageSaved(outputFileResults: ImageCapture.OutputFileResults) {
                    onDone(Result.success(file))
                }

                override fun onError(exception: ImageCaptureException) {
                    onDone(Result.failure(exception))
                }
            },
        )
    }

    /**
     * Records a clip into [file] until [stopClip] or the recorder finishes on its own.
     * Sound is recorded only when the microphone permission is held; a clip without
     * it is still a clip.
     */
    @SuppressLint("MissingPermission")
    fun startClip(file: File, onFinished: (Result<File>) -> Unit) {
        if (activeRecording != null) return
        val hasMic = ContextCompat.checkSelfPermission(context, Manifest.permission.RECORD_AUDIO) ==
            PackageManager.PERMISSION_GRANTED
        var pending = recorder.prepareRecording(context, FileOutputOptions.Builder(file).build())
        if (hasMic) pending = pending.withAudioEnabled()
        recording = true
        activeRecording = pending.start(ContextCompat.getMainExecutor(context)) { event ->
            if (event is VideoRecordEvent.Finalize) {
                activeRecording = null
                recording = false
                if (event.hasError()) {
                    onFinished(Result.failure(IllegalStateException("recording failed (${event.error})")))
                } else {
                    onFinished(Result.success(file))
                }
            }
        }
    }

    fun stopClip() {
        activeRecording?.stop()
    }

    /** Releases the camera. Safe to call more than once. */
    fun close() {
        activeRecording?.stop()
        activeRecording = null
        recording = false
        provider?.unbindAll()
        surfaceRequest = null
    }

    private fun bind(p: ProcessCameraProvider) {
        val selector = if (front) CameraSelector.DEFAULT_FRONT_CAMERA else CameraSelector.DEFAULT_BACK_CAMERA
        p.unbindAll()
        if (!withCapture) {
            p.bindToLifecycle(owner, selector, preview)
            return
        }
        // Three use cases at once is more than some devices will hold; a device that
        // refuses gets the still capture, and its clip control says so by being absent.
        try {
            p.bindToLifecycle(owner, selector, preview, imageCapture, videoCapture)
        } catch (_: IllegalArgumentException) {
            p.bindToLifecycle(owner, selector, preview, imageCapture)
        }
    }
}
