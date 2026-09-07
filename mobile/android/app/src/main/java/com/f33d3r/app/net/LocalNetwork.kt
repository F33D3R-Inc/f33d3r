package com.f33d3r.app.net

import android.Manifest
import android.content.Context
import android.content.pm.PackageManager
import android.os.Build
import java.net.URI

/**
 * Whether an origin is on the local network, and whether reaching it needs asking.
 *
 * Android 16 put the local network behind a runtime permission: an app that talks to
 * a private address without holding it has its packets dropped, silently, and sees
 * only a connection that never completes. A local nantar is exactly such an address,
 * so the sign-in surface asks before the first request rather than after a timeout
 * that looks like the server being down.
 */
object LocalNetwork {

    const val PERMISSION = Manifest.permission.ACCESS_LOCAL_NETWORK

    /** True for loopback, the emulator's host alias, and the RFC 1918 private ranges. */
    fun isLocal(origin: String): Boolean {
        val host = runCatching { URI(origin).host }.getOrNull()?.lowercase() ?: return false
        return host == "localhost" ||
            host.endsWith(".local") ||
            host.startsWith("127.") ||
            host == "::1" || host == "[::1]" ||
            host.startsWith("10.") ||
            host.startsWith("192.168.") ||
            Regex("""^172\.(1[6-9]|2\d|3[01])\.""").containsMatchIn(host)
    }

    /** True when [origin] is local and this install has not yet been allowed to reach it. */
    fun needsPermission(context: Context, origin: String): Boolean =
        Build.VERSION.SDK_INT >= Build.VERSION_CODES.BAKLAVA &&
            isLocal(origin) &&
            context.checkSelfPermission(PERMISSION) != PackageManager.PERMISSION_GRANTED
}
