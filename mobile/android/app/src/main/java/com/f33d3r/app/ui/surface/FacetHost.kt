package com.f33d3r.app.ui.surface

import android.annotation.SuppressLint
import android.content.Context
import android.os.SystemClock
import android.view.MotionEvent
import android.view.ViewConfiguration
import android.webkit.JavascriptInterface
import android.webkit.WebResourceRequest
import android.webkit.WebSettings
import android.webkit.WebView
import android.webkit.WebViewClient
import com.f33d3r.app.core.Fragment
import com.f33d3r.app.net.Http
import com.f33d3r.app.net.Session
import org.json.JSONArray
import kotlin.math.abs

/** A sealed bubble on screen, waiting for a key this surface does not hold. */
data class PendingBubble(
    val address: String,
    val ephPubB64: String,
    val sealedKeyB64: String,
    val sealedNonceB64: String,
    val bodyCtB64: String,
    val bodyNonceB64: String,
)

/**
 * What a surface is being used as. The host document reads it as `data-fa-mode`
 * and dresses the server's Facets for that use: a page in a column, a Vision frame
 * on black, the chrome around a natively decoded live picture, or one Facet alone.
 */
enum class SurfaceMode(val attr: String, val autoplay: Boolean) {
    SURFACE("surface", autoplay = false),
    VISION("vision", autoplay = true),
    WATCH("watch", autoplay = false),
    BROADCAST("broadcast", autoplay = false),
    PANE("pane", autoplay = true),
}

/** A swipe the reader made across a surface, classified. */
enum class Swipe { LEFT, RIGHT, DOWN }

/**
 * The projection surface: where server-rendered facets are put on screen.
 *
 * This is the client's whole rendering responsibility, and the boundary is exact.
 * The surface receives complete HTML fragments and places them by their facet
 * address. It builds no markup, holds no application state, and makes no decision
 * about what a control does — a tap becomes an intent, the intent goes to the server,
 * and what comes back is a fragment this surface puts where the server addressed it.
 *
 * The Shell around it is native Compose. That division is deliberate: the frame,
 * navigation, and gestures are the platform's job and belong in Kotlin, while what
 * the frame contains is the server's render and belongs to the server. The gestures
 * this class recognises — a lane swipe, a Vision step, a hold — are of that kind: each
 * ends in the same intent a tap on the server's own control would have sent.
 */
@SuppressLint("SetJavaScriptEnabled")
class FacetHost(
    context: Context,
    private val session: Session,
    private val onIntent: (FacetIntent) -> Unit,
    private val onReady: () -> Unit,
    private val onPageForward: (String) -> Unit,
    private val mode: SurfaceMode = SurfaceMode.SURFACE,
    private val onSwipe: ((Swipe) -> Unit)? = null,
    /** Hold to pause the frame's media; used by the Vision viewer. */
    private val holdToPause: Boolean = false,
) {

    private var density: String = ""

    val view: WebView = GestureWebView(context).apply {
        setBackgroundColor(android.graphics.Color.TRANSPARENT)
        isVerticalScrollBarEnabled = mode == SurfaceMode.SURFACE
        overScrollMode = WebView.OVER_SCROLL_NEVER

        settings.apply {
            javaScriptEnabled = true
            domStorageEnabled = false
            // The surface renders what the server sent; it does not go and get more on
            // its own, and it never navigates. Everything it needs arrives through the
            // lanes, which carry the session.
            allowFileAccess = false
            allowContentAccess = false
            javaScriptCanOpenWindowsAutomatically = false
            setSupportMultipleWindows(false)
            // A Vision frame's clip plays on arrival, as it does in the web viewer. A
            // timeline's media waits for a tap.
            mediaPlaybackRequiresUserGesture = !mode.autoplay
            mixedContentMode = WebSettings.MIXED_CONTENT_NEVER_ALLOW
            cacheMode = WebSettings.LOAD_DEFAULT
            useWideViewPort = true
            loadWithOverviewMode = false
            textZoom = 100
        }

        webViewClient = object : WebViewClient() {
            /**
             * Refuses every navigation.
             *
             * A projection surface that followed a link would load a page — shell,
             * sidebars and all — inside a frame that already has a native one. Links are
             * intercepted by the runtime and forwarded as intents; anything that reaches
             * here is something the Shell has not been asked to handle, and is dropped.
             */
            override fun shouldOverrideUrlLoading(
                view: WebView,
                request: WebResourceRequest,
            ): Boolean = true
        }

        addJavascriptInterface(Bridge(), "FAHost")
    }

    private var ready = false
    private var pendingMount: String? = null

    /**
     * Loads the host document. Called once per surface.
     *
     * [theme] is the persona's accent theme, stamped on the body the way the server
     * stamps its own; [uiMode] is "dark" or "light", stamped on the root the way the
     * stylesheet keys its two palettes. Both are attributes the stylesheet reads, so
     * the projected facets are dressed exactly as the browser would dress them.
     */
    fun load(
        pial: String,
        account: String,
        handle: String,
        density: String = "",
        theme: String = "",
        uiMode: String = "dark",
    ) {
        this.density = density
        Http.syncCookieToWebView(session.origin, session.token)
        val runtime = view.context.assets.open("fa-runtime.js")
            .bufferedReader().use { it.readText() }
        val document = view.context.assets.open("facet_host.html")
            .bufferedReader().use { it.readText() }
            .replace("__FA_RUNTIME__", runtime)
            // The server's stamp when the shell has been read; until then a stamp of
            // this launch, so an unread server never hands back a year-old stylesheet.
            .replace("__ASSET_VER__", session.assetVersion.ifEmpty { "launch-" + LAUNCH_STAMP }.escapeAttribute())
            .replace("__FA_MODE__", mode.attr)
            .replace("__FA_UIMODE__", uiMode.escapeAttribute())
            .replace("__FA_THEME__", theme.ifEmpty { "void" }.escapeAttribute())
            .replace("__FA_DENSITY__", density.escapeAttribute())
            .replace("__PIAL__", pial.escapeAttribute())
            .replace("__ACCOUNT__", account.escapeAttribute())
            .replace("__HANDLE__", handle.escapeAttribute())

        // The base URL is the origin, so the stylesheet, avatars, posters and gated
        // media inside a fragment resolve and are fetched with the session cookie.
        view.loadDataWithBaseURL(session.origin, document, "text/html", "utf-8", null)
    }

    /** Replaces the content plane with [fragment]. */
    fun mount(fragment: Fragment) {
        val html = fragment.html.toJsString()
        if (!ready) {
            pendingMount = html
            return
        }
        eval("FA.mount($html)")
    }

    /** Adds the next page of a paged facet to the end of the content plane. */
    fun extend(fragment: Fragment) = eval("FA.extend(${fragment.html.toJsString()})")

    /** Replaces the facet at its own address with its new render. */
    fun apply(fragment: Fragment) =
        eval("FA.apply(${fragment.address.toJsString()}, ${fragment.html.toJsString()})")

    /** Appends a rendered facet into [container], when that container is on screen. */
    fun append(container: String, fragment: Fragment) =
        eval("FA.append(${container.toJsString()}, ${fragment.html.toJsString()}); __faPinLog(${container.toJsString()})")

    /** Puts a render inside the slot [container] names, replacing what the slot held. */
    fun place(container: String, fragment: Fragment) =
        eval("__faPlace(${container.toJsString()}, ${fragment.html.toJsString()})")

    /** Inserts a facet the reader has now asked to see. */
    fun prepend(fragment: Fragment) = eval("FA.prepend(${fragment.html.toJsString()})")

    /** Removes the facet at [address]. */
    fun remove(address: String) = eval("FA.remove(${address.toJsString()})")

    /** Asks whether this surface currently holds [address]. */
    fun holds(address: String, answer: (Boolean) -> Unit) =
        view.evaluateJavascript("FA.holds(${address.toJsString()})") { answer(it == "true") }

    /** Puts decrypted plaintext into a sealed bubble. */
    fun reveal(address: String, plaintext: String) =
        eval("FA.reveal(${address.toJsString()}, ${plaintext.toJsString()})")

    /** Marks a sealed bubble as unreadable by this identity. */
    fun sealedFailed(address: String, message: String) =
        eval("FA.sealedFailed(${address.toJsString()}, ${message.toJsString()})")

    /** Empties the composer once the server has accepted what it held. */
    fun clearComposer(address: String) = eval("FA.clearComposer(${address.toJsString()})")

    /** Sets how tightly the timeline is drawn: "card" or "compact". */
    fun setDensity(value: String) {
        density = value
        eval("document.documentElement.setAttribute('data-density', ${value.toJsString()})")
    }

    /** Sets which of the stylesheet's palettes the document is drawn in: "dark" or "light". */
    fun setUiMode(value: String) =
        eval("document.documentElement.setAttribute('data-mode', ${value.toJsString()})")

    /** Sets the persona's accent theme on the body, as the server's shell does. */
    fun setTheme(value: String) =
        eval("document.body.setAttribute('data-theme', ${value.ifEmpty { "void" }.toJsString()})")

    /** Sets an attribute on the host document, for the stylesheet to read. */
    fun setAttr(name: String, value: String) =
        eval("document.documentElement.setAttribute(${name.toJsString()}, ${value.toJsString()})")

    /** Presses the server-rendered control [selector] names, if the surface holds one. */
    fun step(selector: String) = eval("__faStep(${selector.toJsString()})")

    /** Starts the current Vision frame's clip, if the frame has one. */
    fun playVisionMedia() = eval("__faVisionPlay()")

    /** Lists the sealed bubbles awaiting decryption on this surface. */
    fun sealedBubbles(answer: (List<PendingBubble>) -> Unit) {
        view.evaluateJavascript("FA.sealedBubbles()") { raw ->
            answer(parseBubbles(raw))
        }
    }

    /** The paging cursor the server wrote into the last facet on this surface. */
    fun nextCursor(answer: (String) -> Unit) {
        view.evaluateJavascript("FA.nextCursor()") { raw ->
            answer(raw.unquoteJsString())
        }
    }

    fun destroy() {
        view.removeJavascriptInterface("FAHost")
        view.loadUrl("about:blank")
        view.destroy()
    }

    private fun eval(script: String) {
        view.post { view.evaluateJavascript(script, null) }
    }

    /**
     * The WebView, watching the touches it is already receiving.
     *
     * It consumes nothing on its own account. A drag is classified once the finger
     * lifts, and only a drag that began outside anything that scrolls sideways is
     * reported as a lane swipe — the ring rail and a tab strip keep their own
     * gesture. A hold pauses the frame's media and, on release, is swallowed so
     * the lift is not also read as a tap on the step control underneath.
     */
    private inner class GestureWebView(context: Context) : WebView(context) {
        private val slop = ViewConfiguration.get(context).scaledTouchSlop
        private val swipeMin = (72 * resources.displayMetrics.density)
        private val dropMin = (96 * resources.displayMetrics.density)

        private var downX = 0f
        private var downY = 0f
        private var downAt = 0L
        private var sidewaysUnderFinger = false
        private var holding = false
        private val hold = Runnable {
            holding = true
            evaluateJavascript("__faMediaHold()", null)
        }

        override fun dispatchTouchEvent(event: MotionEvent): Boolean {
            when (event.actionMasked) {
                MotionEvent.ACTION_DOWN -> {
                    downX = event.x
                    downY = event.y
                    downAt = SystemClock.uptimeMillis()
                    sidewaysUnderFinger = false
                    if (onSwipe != null) probeSideways(event.x, event.y)
                    if (holdToPause) postDelayed(hold, HOLD_MILLIS)
                }

                MotionEvent.ACTION_MOVE -> {
                    if (holdToPause && !holding &&
                        (abs(event.x - downX) > slop || abs(event.y - downY) > slop)
                    ) {
                        removeCallbacks(hold)
                    }
                }

                MotionEvent.ACTION_UP -> {
                    removeCallbacks(hold)
                    if (holding) {
                        holding = false
                        evaluateJavascript("__faMediaRelease()", null)
                        event.action = MotionEvent.ACTION_CANCEL
                        return super.dispatchTouchEvent(event)
                    }
                    classify(event.x - downX, event.y - downY)
                }

                MotionEvent.ACTION_CANCEL -> {
                    removeCallbacks(hold)
                    if (holding) {
                        holding = false
                        evaluateJavascript("__faMediaRelease()", null)
                    }
                }
            }
            return super.dispatchTouchEvent(event)
        }

        private fun probeSideways(x: Float, y: Float) {
            // CSS pixels, not device pixels: the viewport is device-width at scale 1.
            val cssScale = if (scale > 0f) scale else resources.displayMetrics.density
            val script = "__faHScrollAt(${x / cssScale}, ${y / cssScale})"
            evaluateJavascript(script) { sidewaysUnderFinger = it == "true" }
        }

        private fun classify(dx: Float, dy: Float) {
            val report = onSwipe ?: return
            if (SystemClock.uptimeMillis() - downAt > SWIPE_WINDOW_MILLIS) return
            val ax = abs(dx)
            val ay = abs(dy)
            when {
                ax > swipeMin && ax > ay * SWIPE_SLOPE && !sidewaysUnderFinger ->
                    report(if (dx < 0) Swipe.LEFT else Swipe.RIGHT)

                dy > dropMin && ay > ax * SWIPE_SLOPE && scrollY == 0 ->
                    report(Swipe.DOWN)
            }
        }
    }

    private inner class Bridge {
        @JavascriptInterface
        fun intent(raw: String) {
            val intent = FacetIntent.parse(raw) ?: return
            view.post { onIntent(intent) }
        }

        @JavascriptInterface
        fun ready() {
            view.post {
                if (ready) return@post
                ready = true
                pendingMount?.let { html ->
                    pendingMount = null
                    view.evaluateJavascript("FA.mount($html)", null)
                }
                onReady()
            }
        }

        @JavascriptInterface
        fun measured(height: Int) {
            // Reported on every content change. Nothing acts on it yet; it exists so the
            // Shell can size a surface it does not scroll itself.
        }

        @JavascriptInterface
        fun pageForward(cursor: String) {
            if (cursor.isEmpty()) return
            view.post { onPageForward(cursor) }
        }
    }

    private companion object {
        /** One stamp per process, for surfaces loaded before the server's own is known. */
        val LAUNCH_STAMP: String = System.currentTimeMillis().toString(36)
        const val HOLD_MILLIS = 320L
        const val SWIPE_WINDOW_MILLIS = 700L
        const val SWIPE_SLOPE = 1.6f

        /** Escapes a value for placement inside a double-quoted HTML attribute. */
        fun String.escapeAttribute(): String =
            replace("&", "&amp;").replace("\"", "&quot;").replace("<", "&lt;")

        /**
         * Encodes a Kotlin string as a JavaScript string literal.
         *
         * Fragments are arbitrary HTML that routinely contains quotes, backslashes and
         * newlines, and one unescaped character would turn a fragment into a syntax
         * error that silently drops the mutation. `</script` is broken up because the
         * evaluated script is parsed as script.
         */
        fun String.toJsString(): String {
            val out = StringBuilder(length + 16)
            out.append('"')
            for (ch in this) {
                when {
                    ch == '"' -> out.append("\\\"")
                    ch == '\\' -> out.append("\\\\")
                    ch == '\n' -> out.append("\\n")
                    ch == '\r' -> out.append("\\r")
                    ch == '\t' -> out.append("\\t")
                    // `<`, `>` and `&` are escaped so a fragment can never close the
                    // script element it is being evaluated inside.
                    ch == '<' -> out.append("\\u003c")
                    ch == '>' -> out.append("\\u003e")
                    ch == '&' -> out.append("\\u0026")
                    // U+2028 and U+2029 terminate a line in JavaScript but not in HTML,
                    // so an unescaped one truncates the literal.
                    ch.code == 0x2028 || ch.code == 0x2029 || ch.code < 0x20 ->
                        out.append("\\u%04x".format(ch.code))

                    else -> out.append(ch)
                }
            }
            out.append('"')
            return out.toString()
        }

        /** evaluateJavascript hands back a JSON-encoded value; this takes the string out. */
        fun String.unquoteJsString(): String = runCatching {
            org.json.JSONTokener(this).nextValue() as? String
        }.getOrNull().orEmpty()

        fun parseBubbles(raw: String): List<PendingBubble> = runCatching {
            val json = raw.unquoteJsString().ifEmpty { return emptyList() }
            val array = JSONArray(json)
            (0 until array.length()).mapNotNull { index ->
                val obj = array.optJSONObject(index) ?: return@mapNotNull null
                PendingBubble(
                    address = obj.optString("address"),
                    ephPubB64 = obj.optString("eph"),
                    sealedKeyB64 = obj.optString("sealedKey"),
                    sealedNonceB64 = obj.optString("sealedNonce"),
                    bodyCtB64 = obj.optString("bodyCt"),
                    bodyNonceB64 = obj.optString("bodyNonce"),
                ).takeIf { it.address.isNotEmpty() && it.bodyCtB64.isNotEmpty() }
            }
        }.getOrElse { emptyList() }
    }
}
