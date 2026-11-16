package com.f33d3r.app.malkuth

import android.os.Build
import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyProperties
import android.security.keystore.StrongBoxUnavailableException
import android.util.Base64
import java.math.BigInteger
import java.security.KeyPairGenerator
import java.security.KeyStore
import java.security.PrivateKey
import java.security.Signature
import java.security.spec.ECGenParameterSpec

/**
 * This device's signing key for one account.
 *
 * The identity root is the person's, not a platform's: there is no Apple ID behind
 * it, no Google account, no phone number, and no email that can be taken away. What
 * makes it real on a phone is that the key proving it lives in hardware the phone
 * will not export — generated here, used here, and never transmitted. The server
 * receives the public half and nothing else, which is why a device can be revoked
 * without touching the identity, and the identity survives every device.
 *
 * One key per account on this device, kept under an alias named by the account id —
 * the id a signed work names as its `author_account` — as `malkuth.js` keeps
 * `signing_account_<id>`.
 *
 * The curve, the digest and the signature encoding are the server's, not this
 * client's choice: ECDSA over P-256, SHA-256, and IEEE P1363 (r‖s) because that is
 * what the verifier parses.
 */
class DeviceKey(account: String) {

    init {
        require(account.isNotBlank()) { "a signing key belongs to an account" }
    }

    private val alias = ALIAS_PREFIX + account

    /** True when this device already holds this account's signing key. */
    fun exists(): Boolean = keyStore().containsAlias(alias)

    /**
     * Returns the account's public key in the SPKI base64 form the key authority
     * stores, generating the keypair on first use.
     *
     * StrongBox is requested when the device has it — a separate security chip, so a
     * compromise of the main OS still cannot extract the key. Its absence is not an
     * error; the TEE-backed key is the ordinary case.
     */
    fun publicKeyB64(): String {
        val store = keyStore()
        val existing = store.getCertificate(alias)?.publicKey
        if (existing != null) return Base64.encodeToString(existing.encoded, Base64.NO_WRAP)

        // Earlier builds kept one device-wide key. A hardware key cannot be renamed or
        // exported, so it cannot become this account's; it is retired here, once, and
        // the account's own key takes its place. Nothing is lost: the key authority is
        // handed this account's key at every sign-in and launch, and verifies new
        // works against whichever key it was last handed.
        if (store.containsAlias(LEGACY_DEVICE_ALIAS)) store.deleteEntry(LEGACY_DEVICE_ALIAS)

        val generator = KeyPairGenerator.getInstance(
            KeyProperties.KEY_ALGORITHM_EC,
            ANDROID_KEYSTORE,
        )
        val pair = try {
            generator.initialize(spec(strongBox = Build.VERSION.SDK_INT >= Build.VERSION_CODES.P))
            generator.generateKeyPair()
        } catch (e: Exception) {
            // Only the absence of StrongBox falls back, to the TEE-backed key, which is
            // still non-exportable — the property that matters. Anything else is a
            // failure to make a key, and is reported as one.
            if (Build.VERSION.SDK_INT < Build.VERSION_CODES.P || e !is StrongBoxUnavailableException) throw e
            generator.initialize(spec(strongBox = false))
            generator.generateKeyPair()
        }

        return Base64.encodeToString(pair.public.encoded, Base64.NO_WRAP)
    }

    private fun spec(strongBox: Boolean): KeyGenParameterSpec =
        KeyGenParameterSpec.Builder(alias, KeyProperties.PURPOSE_SIGN)
            .setAlgorithmParameterSpec(ECGenParameterSpec("secp256r1"))
            .setDigests(KeyProperties.DIGEST_SHA256)
            .apply { if (strongBox && Build.VERSION.SDK_INT >= Build.VERSION_CODES.P) setIsStrongBoxBacked(true) }
            .build()

    /**
     * Signs the UTF-8 bytes of [cid] and returns the signature the server verifies:
     * base64url without padding, over the 64-byte r‖s form.
     *
     * Throws when the key is missing or the hardware refuses to sign; the caller
     * reports that rather than sending an unsigned work.
     */
    fun sign(cid: String): String {
        val key = keyStore().getKey(alias, null) as? PrivateKey
            ?: throw IllegalStateException("this device holds no signing key for the account")
        val der = Signature.getInstance("SHA256withECDSA").run {
            initSign(key)
            update(cid.toByteArray(Charsets.UTF_8))
            sign()
        }
        return Base64.encodeToString(derToP1363(der), Base64.URL_SAFE or Base64.NO_PADDING or Base64.NO_WRAP)
    }

    /** Destroys this account's key on this device. The identity is untouched; this device is not. */
    fun forget() {
        val store = keyStore()
        if (store.containsAlias(alias)) store.deleteEntry(alias)
    }

    private fun keyStore(): KeyStore = KeyStore.getInstance(ANDROID_KEYSTORE).apply { load(null) }

    private companion object {
        const val ANDROID_KEYSTORE = "AndroidKeyStore"
        const val ALIAS_PREFIX = "f33d3r.malkuth.signing.account."

        /** The device-wide alias earlier builds used. Only ever deleted. */
        const val LEGACY_DEVICE_ALIAS = "f33d3r.pial.signing.v1"
        const val COORDINATE_BYTES = 32

        /**
         * Converts the JCA's DER `SEQUENCE { INTEGER r, INTEGER s }` into the fixed-width
         * r‖s form the server parses.
         *
         * DER integers are signed and minimally encoded, so r and s arrive with leading
         * zero bytes stripped and sometimes a leading zero added. Both have to be
         * re-normalised to exactly 32 bytes: a signature that is 63 or 65 bytes long is
         * rejected outright by the verifier, which is the failure this conversion exists
         * to prevent.
         */
        fun derToP1363(der: ByteArray): ByteArray {
            var offset = 0
            require(der[offset++] == 0x30.toByte()) { "signature: not a DER sequence" }
            // Sequence length: short form for a P-256 signature, long form tolerated.
            if (der[offset].toInt() and 0xFF > 0x80) {
                offset += (der[offset].toInt() and 0x7F) + 1
            } else {
                offset++
            }

            fun readInteger(): BigInteger {
                require(der[offset++] == 0x02.toByte()) { "signature: expected DER integer" }
                val length = der[offset++].toInt() and 0xFF
                val bytes = der.copyOfRange(offset, offset + length)
                offset += length
                return BigInteger(bytes)
            }

            val r = readInteger()
            val s = readInteger()
            return fixedWidth(r) + fixedWidth(s)
        }

        fun fixedWidth(value: BigInteger): ByteArray {
            val bytes = value.toByteArray()
            val out = ByteArray(COORDINATE_BYTES)
            when {
                // BigInteger prepends a zero byte to keep the value positive.
                bytes.size > COORDINATE_BYTES ->
                    System.arraycopy(bytes, bytes.size - COORDINATE_BYTES, out, 0, COORDINATE_BYTES)

                else ->
                    System.arraycopy(bytes, 0, out, COORDINATE_BYTES - bytes.size, bytes.size)
            }
            return out
        }
    }
}
