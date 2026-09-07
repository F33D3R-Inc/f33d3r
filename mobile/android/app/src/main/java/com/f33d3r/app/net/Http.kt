package com.f33d3r.app.net

import android.webkit.CookieManager
import okhttp3.Cookie
import okhttp3.HttpUrl
import okhttp3.Interceptor
import okhttp3.OkHttpClient
import okhttp3.Response
import java.util.concurrent.TimeUnit

/**
 * The one HTTP client the app uses, and the one place the session cookie is applied.
 *
 * The server's authority is a cookie it minted, so the client's job is custody and
 * replay: attach it to every request, notice when the server replaces it, and mirror
 * it into the WebView cookie store so a projected fragment's own asset requests
 * (avatars, posters, encrypted media) are made by the same signed-in identity. A
 * second cookie store is how a surface ends up rendering as a stranger.
 */
object Http {

    const val SESSION_COOKIE = "f33d3r_session"

    /** Identifies the client to the server; also what the session list shows as the device. */
    const val USER_AGENT = "F33D3R-Android/1.0 (FA Live; Facet projection surface)"

    private const val CALL_TIMEOUT_SECONDS = 30L
    private const val STREAM_READ_TIMEOUT_SECONDS = 0L // FA Live holds the connection open

    fun client(session: Session): OkHttpClient =
        OkHttpClient.Builder()
            .connectTimeout(15, TimeUnit.SECONDS)
            .readTimeout(CALL_TIMEOUT_SECONDS, TimeUnit.SECONDS)
            .writeTimeout(CALL_TIMEOUT_SECONDS, TimeUnit.SECONDS)
            .followRedirects(true)
            // A network interceptor, not an application one: sign-in answers with a
            // redirect, and the session cookie rides on that first hop. An application
            // interceptor sees only the last hop of a redirect chain and would miss the
            // cookie every time; on the network path every hop carries the cookie the
            // hop before it minted.
            .addNetworkInterceptor(SessionInterceptor(session))
            .build()

    /**
     * A client with no read timeout, for the FA Live stream.
     *
     * The stream is idle by design between mutations; timing it out would tear down a
     * healthy connection and make the app reconnect for no reason.
     */
    fun streamClient(session: Session): OkHttpClient =
        client(session).newBuilder()
            .readTimeout(STREAM_READ_TIMEOUT_SECONDS, TimeUnit.SECONDS)
            .retryOnConnectionFailure(true)
            .build()

    /**
     * Copies the session cookie into the WebView store for [origin].
     *
     * Called before a surface loads. Without it the projected fragment renders, but
     * every URL inside it is fetched anonymously and the gated ones come back 403.
     */
    fun syncCookieToWebView(origin: String, token: String?) {
        val manager = CookieManager.getInstance()
        manager.setAcceptCookie(true)
        val secure = origin.startsWith("https://")
        val value = buildString {
            append(SESSION_COOKIE).append('=').append(token.orEmpty())
            append("; Path=/")
            if (secure) append("; Secure")
            append("; SameSite=Lax")
        }
        manager.setCookie(origin, value)
        manager.flush()
    }

    private class SessionInterceptor(private val session: Session) : Interceptor {
        override fun intercept(chain: Interceptor.Chain): Response {
            val request = chain.request().newBuilder()
                .header("User-Agent", USER_AGENT)
                // The server proves every mutation same-origin by the Origin the
                // caller stamps on it. A browser stamps its document's origin; this
                // client's document is the origin it is signed in to, so it says so.
                .header("Origin", session.origin.trimEnd('/'))
                .apply {
                    session.token?.let { header("Cookie", "$SESSION_COOKIE=$it") }
                }
                .build()

            val response = chain.proceed(request)
            captureSessionCookie(response, request.url)
            return response
        }

        /**
         * Adopts a session cookie the server set on this response.
         *
         * Sign-in, sign-out and rotation all arrive this way, so the token is taken
         * from whatever the server last said rather than only from the login call —
         * that is what keeps a rotated session from being replayed with a stale value.
         */
        private fun captureSessionCookie(response: Response, url: HttpUrl) {
            val setCookies = response.headers("Set-Cookie")
            if (setCookies.isEmpty()) return
            for (header in setCookies) {
                val cookie = Cookie.parse(url, header) ?: continue
                if (cookie.name != SESSION_COOKIE) continue
                if (cookie.value.isEmpty() || cookie.expiresAt <= System.currentTimeMillis()) {
                    session.token = null
                } else {
                    session.token = cookie.value
                }
            }
        }
    }
}
