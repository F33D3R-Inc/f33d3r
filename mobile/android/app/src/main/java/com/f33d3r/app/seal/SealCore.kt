package com.f33d3r.app.seal

import android.util.Base64
import org.bouncycastle.crypto.agreement.X25519Agreement
import org.bouncycastle.crypto.digests.SHA256Digest
import org.bouncycastle.crypto.generators.Argon2BytesGenerator
import org.bouncycastle.crypto.generators.HKDFBytesGenerator
import org.bouncycastle.crypto.params.Argon2Parameters
import org.bouncycastle.crypto.params.HKDFParameters
import org.bouncycastle.crypto.params.X25519PrivateKeyParameters
import org.bouncycastle.crypto.params.X25519PublicKeyParameters
import java.security.SecureRandom
import javax.crypto.Cipher
import javax.crypto.spec.GCMParameterSpec
import javax.crypto.spec.SecretKeySpec

/** One recipient of a sealed message: the persona, and the key that opens for it. */
data class Recipient(val account: String, val pubB64: String)

/** A recipient's copy of the content key, sealed so only they can take it. */
data class SealedKey(
    val recipientAccount: String,
    val ephPubB64: String,
    val sealedB64: String,
    val sealedNonceB64: String,
)

/** A sealed message: one encrypted body, and one wrapped content key per recipient. */
data class Envelope(
    val bodyCtB64: String,
    val bodyNonceB64: String,
    val sealed: List<SealedKey>,
)

/** An X25519 identity keypair, base64-encoded as the wire carries it. */
data class Keypair(val privB64: String, val pubB64: String)

/** A secret sealed under a symmetric key. */
data class Wrapped(val ctB64: String, val nonceB64: String)

/**
 * The sealed-mode crypto core, native.
 *
 * This is a port of the `sealcore` crate the browser runs as WASM, and it is a port
 * in the strict sense: same primitives, same parameters, same HKDF info string, same
 * base64 alphabet. It has to be, because both clients seal to the same recipients and
 * open the same stored blobs — a message this app sealed must open in the browser
 * tomorrow, and a message the browser sealed last year must open here.
 *
 * The scheme:
 *  - one random AES-256-GCM content key encrypts the body, once;
 *  - per recipient, an ephemeral X25519 key does ECDH against their public key,
 *    HKDF-SHA256 derives a wrapping key, and the content key is sealed to it;
 *  - the account's own private key is wrapped under a key derived at sign-in, and
 *    only that wrapped blob is ever given to the server.
 *
 * Nothing here talks to the network, and no function accepts a key it did not
 * receive from its caller: the server is a blind relay and this file is what makes
 * that claim true on this device.
 */
object SealCore {

    /** Domain separation for the wrap key. Changing this string breaks every existing message. */
    private val HKDF_INFO = "sealcore-v1-x25519-wrap".toByteArray(Charsets.UTF_8)

    private const val KEY_BYTES = 32
    private const val NONCE_BYTES = 12
    private const val TAG_BITS = 128

    // Argon2id at the reference parameters: 19 MiB, two passes, one lane. These are
    // the `argon2` crate's defaults, which the browser derives with — a different
    // cost here would derive a different key and silently fail to unwrap.
    private const val ARGON2_MEMORY_KIB = 19456
    private const val ARGON2_ITERATIONS = 2
    private const val ARGON2_PARALLELISM = 1

    private val random = SecureRandom()

    /** Generates a fresh X25519 identity keypair. */
    fun generateKeypair(): Keypair {
        val priv = X25519PrivateKeyParameters(random)
        return Keypair(
            privB64 = encode(priv.encoded),
            pubB64 = encode(priv.generatePublicKey().encoded),
        )
    }

    /** Generates a 16-byte salt for key derivation. */
    fun generateSalt(): String = encode(randomBytes(16))

    /**
     * Seals [plaintext] to every recipient.
     *
     * The body is encrypted once under a fresh content key; each recipient gets that
     * key wrapped to them alone. A group message therefore costs one body and N small
     * wraps, and no recipient learns another's wrapping key.
     */
    fun sealMessage(recipients: List<Recipient>, plaintext: String): Envelope {
        require(recipients.isNotEmpty()) { "no recipients" }

        val contentKey = randomBytes(KEY_BYTES)
        val (bodyCt, bodyNonce) = aesSeal(contentKey, plaintext.toByteArray(Charsets.UTF_8))

        val sealed = recipients.map { recipient ->
            val theirPub = X25519PublicKeyParameters(decodeExact(recipient.pubB64, "recipient pub"), 0)
            val ephemeral = X25519PrivateKeyParameters(random)
            val wrapKey = deriveWrapKey(ephemeral, theirPub)
            val (keyCt, keyNonce) = aesSeal(wrapKey, contentKey)
            SealedKey(
                recipientAccount = recipient.account,
                ephPubB64 = encode(ephemeral.generatePublicKey().encoded),
                sealedB64 = encode(keyCt),
                sealedNonceB64 = encode(keyNonce),
            )
        }

        return Envelope(encode(bodyCt), encode(bodyNonce), sealed)
    }

    /**
     * Opens a message sealed to the holder of [myPrivB64].
     *
     * Throws when the ciphertext was not sealed to this key or has been altered —
     * AES-GCM authenticates, so a failure here means the bubble is not readable by
     * this identity, never that it is readable but wrong.
     */
    fun openMessage(
        myPrivB64: String,
        ephPubB64: String,
        sealedB64: String,
        sealedNonceB64: String,
        bodyCtB64: String,
        bodyNonceB64: String,
    ): String {
        val mine = X25519PrivateKeyParameters(decodeExact(myPrivB64, "my priv"), 0)
        val ephPub = X25519PublicKeyParameters(decodeExact(ephPubB64, "eph pub"), 0)
        val wrapKey = deriveWrapKey(mine, ephPub)

        val contentKey = aesOpen(wrapKey, decode(sealedNonceB64), decode(sealedB64))
        require(contentKey.size == KEY_BYTES) { "content key: expected $KEY_BYTES bytes" }

        val body = aesOpen(contentKey, decode(bodyNonceB64), decode(bodyCtB64))
        return String(body, Charsets.UTF_8)
    }

    /**
     * Argon2id: derives the 32-byte key that wraps the account's private key.
     *
     * The input is the sign-in password (or a backup code) and a public per-account
     * salt. The cost is deliberately high because this derivation is the only thing
     * standing between a stolen wrapped blob and the identity inside it.
     */
    fun deriveBackupKey(code: String, saltB64: String): String {
        val params = Argon2Parameters.Builder(Argon2Parameters.ARGON2_id)
            .withVersion(Argon2Parameters.ARGON2_VERSION_13)
            .withMemoryAsKB(ARGON2_MEMORY_KIB)
            .withIterations(ARGON2_ITERATIONS)
            .withParallelism(ARGON2_PARALLELISM)
            .withSalt(decode(saltB64))
            .build()
        val generator = Argon2BytesGenerator().apply { init(params) }
        val out = ByteArray(KEY_BYTES)
        generator.generateBytes(code.toByteArray(Charsets.UTF_8), out)
        return encode(out)
    }

    /**
     * The per-account salt: SHA-256 of the lowercased handle.
     *
     * Public and deterministic by design — it only has to be unique per account so
     * two people with the same password derive different keys.
     */
    fun saltFromHandle(handle: String): String {
        val digest = java.security.MessageDigest.getInstance("SHA-256")
        return encode(digest.digest(handle.lowercase().toByteArray(Charsets.UTF_8)))
    }

    /** Wraps a base64 secret under a 32-byte symmetric key. */
    fun wrapWithKey(keyB64: String, plaintextB64: String): Wrapped {
        val key = decodeExact(keyB64, "wrap key")
        val (ct, nonce) = aesSeal(key, decode(plaintextB64))
        return Wrapped(encode(ct), encode(nonce))
    }

    /** Unwraps a secret sealed with [wrapWithKey], returning its base64 form. */
    fun unwrapWithKey(keyB64: String, ctB64: String, nonceB64: String): String {
        val key = decodeExact(keyB64, "wrap key")
        return encode(aesOpen(key, decode(nonceB64), decode(ctB64)))
    }

    // ── primitives ────────────────────────────────────────────────────────────

    private fun deriveWrapKey(
        secret: X25519PrivateKeyParameters,
        theirPublic: X25519PublicKeyParameters,
    ): ByteArray {
        val shared = ByteArray(X25519Agreement().agreementSize)
        X25519Agreement().apply {
            init(secret)
            calculateAgreement(theirPublic, shared, 0)
        }
        // Salt is absent, matching the browser's HKDF::new(None, ..) — RFC 5869 then
        // uses a zero-filled salt of the hash length.
        val hkdf = HKDFBytesGenerator(SHA256Digest())
        hkdf.init(HKDFParameters(shared, null, HKDF_INFO))
        val okm = ByteArray(KEY_BYTES)
        hkdf.generateBytes(okm, 0, okm.size)
        shared.fill(0)
        return okm
    }

    private fun aesSeal(key: ByteArray, plaintext: ByteArray): Pair<ByteArray, ByteArray> {
        val nonce = randomBytes(NONCE_BYTES)
        val cipher = Cipher.getInstance("AES/GCM/NoPadding").apply {
            init(Cipher.ENCRYPT_MODE, SecretKeySpec(key, "AES"), GCMParameterSpec(TAG_BITS, nonce))
        }
        return cipher.doFinal(plaintext) to nonce
    }

    private fun aesOpen(key: ByteArray, nonce: ByteArray, ciphertext: ByteArray): ByteArray {
        require(nonce.size == NONCE_BYTES) { "nonce: expected $NONCE_BYTES bytes" }
        val cipher = Cipher.getInstance("AES/GCM/NoPadding").apply {
            init(Cipher.DECRYPT_MODE, SecretKeySpec(key, "AES"), GCMParameterSpec(TAG_BITS, nonce))
        }
        return cipher.doFinal(ciphertext)
    }

    private fun randomBytes(count: Int) = ByteArray(count).also(random::nextBytes)

    private fun encode(bytes: ByteArray): String = Base64.encodeToString(bytes, Base64.NO_WRAP)

    private fun decode(b64: String): ByteArray = Base64.decode(b64.trim(), Base64.DEFAULT)

    private fun decodeExact(b64: String, what: String): ByteArray {
        val bytes = decode(b64)
        require(bytes.size == KEY_BYTES) { "$what: expected $KEY_BYTES bytes" }
        return bytes
    }
}
