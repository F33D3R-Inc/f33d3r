package com.f33d3r.app.push

import android.app.Notification
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.os.Build
import androidx.core.app.NotificationCompat
import androidx.core.app.NotificationManagerCompat
import androidx.core.app.ServiceCompat
import androidx.lifecycle.LifecycleService
import androidx.lifecycle.lifecycleScope
import com.f33d3r.app.F33d3rApp
import com.f33d3r.app.MainActivity
import com.f33d3r.app.R
import com.f33d3r.app.net.live.Mutation
import com.f33d3r.app.net.live.SignalKind
import kotlinx.coroutines.launch

/**
 * Holds FA Live open while the app is not in front.
 *
 * The platform has no push channel of its own here: nothing on the server issues
 * device tokens, so there is no message to be woken by. What exists is the stream,
 * and a stream only delivers while something is holding it — so this service is that
 * something, and it is honest about it: a visible, non-dismissable notification for
 * as long as it runs, started only when the person asks for it.
 *
 * It is not a background trick. It does not restart itself, it does not schedule
 * wakeups, and it stops the moment it is told to. When it is not running, messages
 * arrive when the app is next opened, which is the truthful behaviour rather than a
 * silent one.
 */
class FaLiveService : LifecycleService() {

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        super.onStartCommand(intent, flags, startId)

        if (intent?.action == ACTION_STOP) {
            stopSelf()
            return START_NOT_STICKY
        }

        ServiceCompat.startForeground(
            this,
            NOTIFICATION_ID,
            connectionNotification(),
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.UPSIDE_DOWN_CAKE) {
                ServiceInfo.FOREGROUND_SERVICE_TYPE_DATA_SYNC
            } else {
                0
            },
        )

        val runtime = (application as F33d3rApp).runtime
        runtime.live.connect()

        lifecycleScope.launch {
            runtime.live.mutations.collect { mutation ->
                // Only the signals that mean something arrived while nobody was looking
                // are surfaced. A re-rendered facet has no surface to land on right now,
                // and turning one into a notification would be inventing an event.
                when {
                    mutation is Mutation.Append &&
                        mutation.container == com.f33d3r.app.net.live.FaLive.GNOSIS_MESSAGES ->
                        notifyMessage()

                    mutation is Mutation.Signal &&
                        mutation.kind == SignalKind.NOTIFY_ARRIVED ->
                        notifyActivity(mutation.value)

                    else -> Unit
                }
            }
        }

        // Not sticky: the person started this, and the system restarting it silently
        // would be the app deciding to hold a connection nobody asked it to hold.
        return START_NOT_STICKY
    }

    /**
     * Says a sealed message arrived, and says nothing about what it contains.
     *
     * It cannot: the body is ciphertext until the app opens it with a key this service
     * would have to hold in order to preview it. Not previewing is the feature.
     */
    private fun notifyMessage() {
        val notification = NotificationCompat.Builder(this, F33d3rApp.CHANNEL_MESSAGES)
            .setSmallIcon(R.drawable.ic_launcher_foreground)
            .setContentTitle("New sealed message")
            .setContentText("Open F33D3R to read it")
            .setContentIntent(openShell())
            .setAutoCancel(true)
            .setCategory(NotificationCompat.CATEGORY_MESSAGE)
            .build()
        post(MESSAGE_NOTIFICATION_ID, notification)
    }

    private fun notifyActivity(count: String) {
        val notification = NotificationCompat.Builder(this, F33d3rApp.CHANNEL_ACTIVITY)
            .setSmallIcon(R.drawable.ic_launcher_foreground)
            .setContentTitle("New activity")
            .setContentText(if (count.isEmpty()) "Someone interacted with your work" else "$count unread")
            .setContentIntent(openShell())
            .setAutoCancel(true)
            .build()
        post(ACTIVITY_NOTIFICATION_ID, notification)
    }

    private fun post(id: Int, notification: Notification) {
        val manager = NotificationManagerCompat.from(this)
        if (!manager.areNotificationsEnabled()) return
        runCatching { manager.notify(id, notification) }
    }

    private fun connectionNotification(): Notification =
        NotificationCompat.Builder(this, F33d3rApp.CHANNEL_LIVE)
            .setSmallIcon(R.drawable.ic_launcher_foreground)
            .setContentTitle(getString(R.string.live_service_title))
            .setContentText(getString(R.string.live_service_text))
            .setContentIntent(openShell())
            .setOngoing(true)
            .setSilent(true)
            .setPriority(NotificationCompat.PRIORITY_LOW)
            .addAction(
                0,
                "Stop",
                PendingIntent.getService(
                    this,
                    1,
                    Intent(this, FaLiveService::class.java).setAction(ACTION_STOP),
                    PendingIntent.FLAG_IMMUTABLE,
                ),
            )
            .build()

    private fun openShell(): PendingIntent = PendingIntent.getActivity(
        this,
        0,
        Intent(this, MainActivity::class.java)
            .setFlags(Intent.FLAG_ACTIVITY_SINGLE_TOP or Intent.FLAG_ACTIVITY_CLEAR_TOP),
        PendingIntent.FLAG_IMMUTABLE,
    )

    companion object {
        const val ACTION_STOP = "com.f33d3r.app.LIVE_STOP"

        private const val NOTIFICATION_ID = 1
        private const val MESSAGE_NOTIFICATION_ID = 2
        private const val ACTIVITY_NOTIFICATION_ID = 3

        fun start(context: Context) {
            context.startForegroundService(Intent(context, FaLiveService::class.java))
        }

        fun stop(context: Context) {
            context.startService(
                Intent(context, FaLiveService::class.java).setAction(ACTION_STOP)
            )
        }
    }
}
