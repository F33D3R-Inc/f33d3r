package com.f33d3r.app.net

import android.content.Context
import androidx.compose.runtime.Immutable
import com.f33d3r.app.BuildConfig
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow

/**
 * The signed-in identity, as the server reports it.
 *
 * Three names, three different things, and the client keeps them apart because the
 * server does: [pial] is the person, [account] is the persona that person is
 * currently acting as, and [handle] is the name bound to that persona. Message
 * sealing is per-account; notifications and balance are per-PIAL.
 */
@Immutable
data class Identity(
    val pial: String,
    val account: String,
    val handle: String,
    val displayName: String = "",
    val avatarUrl: String = "",
    val verified: Boolean = false,
    /** The account may publish as a creator: the Create wire is theirs. */
    val creator: Boolean = false,
    /** The account holds the admin or founder role. */
    val admin: Boolean = false,
    /** "business" or "government" when the account represents an organisation. */
    val official: String = "",
    /** The accent theme on the persona's profile, as the server stamps it on its shell. */
    val theme: String = "",
) {
    val isSignedIn: Boolean get() = account.isNotEmpty() && handle.isNotEmpty()

    companion object {
        val NONE = Identity(pial = "", account = "", handle = "")
    }
}

/**
 * Session state and the origin every request is made against.
 *
 * The session token is the whole of the client's authority: it is minted by the
 * server at sign-in, stored sealed in the Keystore, and replayed as the cookie the
 * server already recognises. The client derives nothing from it and never mints one.
 */
class Session(context: Context) {

    private val store = SecureStore(context)
    private val prefs = context.applicationContext
        .getSharedPreferences("f33d3r.session", Context.MODE_PRIVATE)

    private val _identity = MutableStateFlow(Identity.NONE)
    val identity: StateFlow<Identity> = _identity.asStateFlow()

    /**
     * The origin this install talks to. Defaults to the build's origin; an operator
     * running a local nantar can point the app at it without a rebuild.
     */
    var origin: String
        get() = prefs.getString(KEY_ORIGIN, null)?.takeIf { it.isNotBlank() } ?: BuildConfig.ORIGIN
        set(value) {
            prefs.edit().putString(KEY_ORIGIN, value.trimEnd('/')).apply()
        }

    var token: String?
        get() = store.get(KEY_TOKEN)
        set(value) = store.put(KEY_TOKEN, value)

    /**
     * The server's cache-busting stamp for its static assets, read off the shell it
     * renders. The server serves its stylesheets as immutable for a year and keys
     * them by this stamp; a surface that asked for them unstamped would keep the copy
     * it first downloaded through every deploy since.
     */
    var assetVersion: String
        get() = prefs.getString(KEY_ASSET_VERSION, "").orEmpty()
        set(value) {
            prefs.edit().putString(KEY_ASSET_VERSION, value).apply()
        }

    /**
     * The home lane the Shell was last on. Navigation state of the native frame — the
     * timeline opens on the lane the reader left, the way a tab bar remembers its tab —
     * and nothing about what that lane contains, which stays the server's.
     */
    var homeLane: String
        get() = prefs.getString(KEY_HOME_LANE, "").orEmpty()
        set(value) {
            prefs.edit().putString(KEY_HOME_LANE, value).apply()
        }

    /** How tightly the timeline is set: "card" or "compact". A projection choice of the frame. */
    var density: String
        get() = prefs.getString(KEY_DENSITY, DENSITY_CARD) ?: DENSITY_CARD
        set(value) {
            prefs.edit().putString(KEY_DENSITY, value).apply()
        }

    /**
     * "dark" or "light": which of the stylesheet's two palettes the frame is drawn in.
     * A projection choice of this device, like density; the accent theme within the
     * palette is the persona's and comes from the server with the identity.
     */
    var mode: String
        get() = prefs.getString(KEY_MODE, MODE_DARK) ?: MODE_DARK
        set(value) {
            prefs.edit().putString(KEY_MODE, value).apply()
        }

    /** The account's unwrapped X25519 messaging private key, if this device holds it. */
    fun sealedPrivateKey(account: String): String? = store.get(sealKey(account))

    fun storeSealedPrivateKey(account: String, privB64: String) = store.put(sealKey(account), privB64)

    /**
     * Records a recipient's public key on first contact so a later change is visible.
     * Trust-on-first-use is the only defence a blind relay can offer against a key
     * swap, and it only works if the previous key is remembered.
     */
    fun pinnedKey(account: String): String? = prefs.getString(pinKey(account), null)

    fun pinKey(account: String, pubB64: String) {
        prefs.edit().putString(pinKey(account), pubB64).apply()
    }

    fun adopt(identity: Identity) {
        _identity.value = identity
        prefs.edit()
            .putString(KEY_PIAL, identity.pial)
            .putString(KEY_ACCOUNT, identity.account)
            .putString(KEY_HANDLE, identity.handle)
            .putString(KEY_DISPLAY, identity.displayName)
            .putString(KEY_AVATAR, identity.avatarUrl)
            .putBoolean(KEY_VERIFIED, identity.verified)
            .putString(KEY_THEME, identity.theme)
            .apply()
    }

    /** Restores the last known identity so the shell draws before the network answers. */
    fun restore(): Identity {
        val restored = Identity(
            pial = prefs.getString(KEY_PIAL, "").orEmpty(),
            account = prefs.getString(KEY_ACCOUNT, "").orEmpty(),
            handle = prefs.getString(KEY_HANDLE, "").orEmpty(),
            displayName = prefs.getString(KEY_DISPLAY, "").orEmpty(),
            avatarUrl = prefs.getString(KEY_AVATAR, "").orEmpty(),
            verified = prefs.getBoolean(KEY_VERIFIED, false),
            theme = prefs.getString(KEY_THEME, "").orEmpty(),
        )
        _identity.value = restored
        return restored
    }

    /**
     * Drops every trace of the session, including the sealed key material.
     *
     * Sign-out must not leave a private key behind that the next person to hold the
     * phone could unlock: the account's messaging identity is recoverable from the
     * server's wrapped copy at the next password sign-in, so destroying the local
     * copy costs nothing and closes the window.
     */
    fun forget() {
        store.clear()
        prefs.edit().clear().apply()
        _identity.value = Identity.NONE
    }

    private fun sealKey(account: String) = "seal.priv.$account"
    private fun pinKey(account: String) = "seal.pin.$account"

    companion object {
        const val DENSITY_CARD = "card"
        const val DENSITY_COMPACT = "compact"
        const val MODE_DARK = "dark"
        const val MODE_LIGHT = "light"

        private const val KEY_HOME_LANE = "home.lane"
        private const val KEY_DENSITY = "home.density"
        private const val KEY_MODE = "frame.mode"
        private const val KEY_THEME = "identity.theme"
        private const val KEY_ORIGIN = "origin"
        private const val KEY_ASSET_VERSION = "assets.version"
        private const val KEY_TOKEN = "session.token"
        private const val KEY_PIAL = "identity.pial"
        private const val KEY_ACCOUNT = "identity.account"
        private const val KEY_HANDLE = "identity.handle"
        private const val KEY_DISPLAY = "identity.display"
        private const val KEY_AVATAR = "identity.avatar"
        private const val KEY_VERIFIED = "identity.verified"
    }
}
