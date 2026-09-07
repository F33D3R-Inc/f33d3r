package com.f33d3r.app.seal

import com.f33d3r.app.core.Endpoints
import com.f33d3r.app.core.Fragment
import com.f33d3r.app.net.FacetClient
import com.f33d3r.app.net.LaneResult
import com.f33d3r.app.net.Session
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.boolean
import kotlinx.serialization.json.buildJsonArray
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import kotlinx.serialization.json.put

/** The state of this device's ability to read and write sealed messages. */
sealed interface SealState {
    /** This device holds the account's private key and can read sealed threads. */
    data object Unlocked : SealState

    /** The key exists on the server but this device cannot open it without a sign-in. */
    data object Locked : SealState

    /** The account has no messaging identity yet. */
    data object Absent : SealState
}

/** A recipient whose pinned key has changed since this device last sealed to them. */
data class KeyChange(val account: String, val previousPubB64: String, val currentPubB64: String)

/**
 * The Gnosis lane: sealed messaging, held to its promise.
 *
 * The server is a blind relay here. It stores ciphertext and per-recipient wrapped
 * keys, and it stores this account's own private key only in a form it cannot open.
 * Everything that would break that promise happens in this file, on this device:
 * key generation, wrapping, unwrapping, sealing and opening.
 *
 * The private key is held in the Android Keystore rather than anywhere the projected
 * fragments can reach. A sealed bubble arrives as ciphertext in HTML, its plaintext
 * is produced here, and only the plaintext crosses back — so the surface that
 * displays a message never holds the key that opened it.
 */
class Gnosis(
    private val session: Session,
    private val client: FacetClient,
) {

    /**
     * Establishes this device's messaging identity for [account].
     *
     * Called with the key derived from the password at sign-in, which is the only
     * moment the identity can be unlocked: there is no separate recovery, by design.
     * If the server holds a wrapped key that this derivation opens, it is adopted; if
     * the unwrap fails — which is what a password reset looks like from here — a fresh
     * identity is provisioned and the server records the rotation.
     */
    suspend fun establish(account: String, handle: String, password: String): SealState =
        withContext(Dispatchers.IO) {
            session.sealedPrivateKey(account)?.let { return@withContext SealState.Unlocked }

            val kek = SealCore.deriveBackupKey(password, SealCore.saltFromHandle(handle))
            val bootstrap = client.readJson(Endpoints.GNOSIS_BOOTSTRAP)?.jsonObject
                ?: return@withContext SealState.Locked

            val has = (bootstrap["has"] as? JsonPrimitive)?.boolean ?: false
            val wrappedPriv = bootstrap["wrapped_priv"]?.jsonPrimitive?.content.orEmpty()
            val wrapNonce = bootstrap["wrap_nonce"]?.jsonPrimitive?.content.orEmpty()

            val existing = if (has && wrappedPriv.isNotEmpty()) {
                runCatching { SealCore.unwrapWithKey(kek, wrappedPriv, wrapNonce) }.getOrNull()
            } else {
                null
            }

            if (existing != null) {
                session.storeSealedPrivateKey(account, existing)
                return@withContext SealState.Unlocked
            }

            val keypair = SealCore.generateKeypair()
            val wrapped = SealCore.wrapWithKey(kek, keypair.privB64)
            val payload = buildJsonObject {
                put("pub_b64", keypair.pubB64)
                put("wrapped_priv", wrapped.ctB64)
                put("wrap_nonce", wrapped.nonceB64)
            }.toString()

            when (client.submitJson(Endpoints.GNOSIS_PROVISION, payload)) {
                is LaneResult.Accepted, is LaneResult.Rendered -> {
                    session.storeSealedPrivateKey(account, keypair.privB64)
                    SealState.Unlocked
                }
                // The key directory refused the registration. Storing the private key
                // now would leave this device holding an identity nobody can seal to.
                else -> SealState.Absent
            }
        }

    /** Whether this device can currently read sealed threads for [account]. */
    fun state(account: String): SealState =
        if (session.sealedPrivateKey(account) != null) SealState.Unlocked else SealState.Locked

    /**
     * Opens one sealed bubble.
     *
     * Takes the inert ciphertext attributes the server rendered and returns the
     * plaintext. Failure is reported as null rather than as an exception with the
     * ciphertext attached, because the surface's only correct response either way is
     * to say the bubble cannot be read.
     */
    fun open(
        account: String,
        ephPubB64: String,
        sealedKeyB64: String,
        sealedNonceB64: String,
        bodyCtB64: String,
        bodyNonceB64: String,
    ): String? {
        val priv = session.sealedPrivateKey(account) ?: return null
        return runCatching {
            SealCore.openMessage(
                myPrivB64 = priv,
                ephPubB64 = ephPubB64,
                sealedB64 = sealedKeyB64,
                sealedNonceB64 = sealedNonceB64,
                bodyCtB64 = bodyCtB64,
                bodyNonceB64 = bodyNonceB64,
            )
        }.getOrNull()
    }

    /**
     * Reads the conversation's key directory.
     *
     * Scoped to a conversation the caller belongs to — the server enforces that, which
     * is what stops the directory being a harvestable list of every public key on the
     * platform.
     */
    suspend fun directory(convoId: String): List<Recipient> = withContext(Dispatchers.IO) {
        val array = client.readJson(
            Endpoints.GNOSIS_DIRECTORY,
            mapOf("c" to convoId),
        ) as? JsonArray ?: return@withContext emptyList()

        array.mapNotNull { entry ->
            val obj = entry as? JsonObject ?: return@mapNotNull null
            val account = obj["account"]?.jsonPrimitive?.content.orEmpty()
            val pub = obj["pub_b64"]?.jsonPrimitive?.content.orEmpty()
            if (account.isEmpty() || pub.isEmpty()) null else Recipient(account, pub)
        }
    }

    /**
     * Checks each recipient's key against the one this device pinned on first contact.
     *
     * A changed key is usually a new device, and is sometimes an interception. This
     * client cannot tell which, so it does not decide: it reports the changes and lets
     * the person sending the message decide, which is the only honest answer a
     * trust-on-first-use scheme can give.
     */
    fun keyChanges(recipients: List<Recipient>): List<KeyChange> =
        recipients.mapNotNull { recipient ->
            val pinned = session.pinnedKey(recipient.account) ?: return@mapNotNull null
            if (pinned == recipient.pubB64) null
            else KeyChange(recipient.account, pinned, recipient.pubB64)
        }

    /** Records the recipients' keys as the ones this device now trusts. */
    fun pin(recipients: List<Recipient>) {
        recipients.forEach { session.pinKey(it.account, it.pubB64) }
    }

    /**
     * Seals [text] to the conversation and hands the envelope to the relay.
     *
     * Returns the sender's own bubble as the server rendered it — still sealed, since
     * the server has no plaintext to render. The surface opens it the same way it
     * opens any other bubble.
     */
    suspend fun send(convoId: String, recipients: List<Recipient>, text: String): LaneResult =
        withContext(Dispatchers.IO) {
            if (recipients.isEmpty()) {
                return@withContext LaneResult.Refused(0, "No encryption keys for the recipients yet.")
            }
            val envelope = runCatching { SealCore.sealMessage(recipients, text) }
                .getOrElse { return@withContext LaneResult.Refused(0, "Could not seal this message.") }

            val payload = buildJsonObject {
                put("c", convoId)
                put("envelope", buildJsonObject {
                    put("body_ct_b64", envelope.bodyCtB64)
                    put("body_nonce_b64", envelope.bodyNonceB64)
                    put("sealed", buildJsonArray {
                        envelope.sealed.forEach { key ->
                            add(
                                buildJsonObject {
                                    put("recipient_account", key.recipientAccount)
                                    put("eph_pub_b64", key.ephPubB64)
                                    put("sealed_b64", key.sealedB64)
                                    put("sealed_nonce_b64", key.sealedNonceB64)
                                }
                            )
                        }
                    })
                })
            }.toString()

            when (val result = client.submitJson(Endpoints.GNOSIS_SEND_SEALED, payload)) {
                is LaneResult.Rendered -> result
                is LaneResult.Accepted -> LaneResult.Accepted
                else -> result
            }
        }

    /** True when [html] is a bubble the server could not render because it is sealed. */
    fun isSealed(html: String): Boolean = html.contains("data-sealed=\"1\"")

    /** Reads the ciphertext attributes off a sealed bubble fragment. */
    fun ciphertextOf(fragment: Fragment): SealedBubble? {
        fun attr(name: String): String? =
            Regex("""$name\s*=\s*"([^"]*)"""").find(fragment.html)?.groupValues?.get(1)

        return SealedBubble(
            ephPubB64 = attr("data-eph") ?: return null,
            sealedKeyB64 = attr("data-sealed-key") ?: return null,
            sealedNonceB64 = attr("data-sealed-nonce") ?: return null,
            bodyCtB64 = attr("data-body-ct") ?: return null,
            bodyNonceB64 = attr("data-body-nonce") ?: return null,
        )
    }
}

/** The inert ciphertext the server renders in place of a sealed message body. */
data class SealedBubble(
    val ephPubB64: String,
    val sealedKeyB64: String,
    val sealedNonceB64: String,
    val bodyCtB64: String,
    val bodyNonceB64: String,
)
