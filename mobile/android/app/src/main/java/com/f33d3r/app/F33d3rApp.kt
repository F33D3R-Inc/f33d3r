package com.f33d3r.app

import android.app.Application
import android.app.NotificationChannel
import android.app.NotificationManager
import android.os.Build
import android.webkit.WebView

class F33d3rApp : Application() {

    lateinit var runtime: Runtime
        private set

    override fun onCreate() {
        super.onCreate()
        runtime = Runtime(this)

        // Each process gets its own WebView data directory. Without this a second
        // process touching a WebView takes down the first.
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.P) {
            WebView.setDataDirectorySuffix("f33d3r")
        }

        createChannels()
    }

    /**
     * The channels the person can silence independently.
     *
     * Separated by what they interrupt for, not by which part of the app emitted
     * them: a sealed message and a like are not the same interruption, and one
         * channel for both means silencing the noise silences the message too.
     */
    private fun createChannels() {
        val manager = getSystemService(NotificationManager::class.java) ?: return
        manager.createNotificationChannel(
            NotificationChannel(
                CHANNEL_MESSAGES,
                "Messages",
                NotificationManager.IMPORTANCE_HIGH,
            ).apply { description = "Sealed messages addressed to you" }
        )
        manager.createNotificationChannel(
            NotificationChannel(
                CHANNEL_ACTIVITY,
                "Activity",
                NotificationManager.IMPORTANCE_DEFAULT,
            ).apply { description = "Follows, likes, reposts and mentions" }
        )
        manager.createNotificationChannel(
            NotificationChannel(
                CHANNEL_LIVE,
                "Live connection",
                NotificationManager.IMPORTANCE_LOW,
            ).apply {
                description = "Shown while the app holds its live connection in the background"
                setShowBadge(false)
            }
        )
    }

    companion object {
        const val CHANNEL_MESSAGES = "messages"
        const val CHANNEL_ACTIVITY = "activity"
        const val CHANNEL_LIVE = "live"
    }
}
