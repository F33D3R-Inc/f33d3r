package com.f33d3r.app.pial

import android.os.Build
import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyProperties
import android.util.Base64
import java.math.BigInteger
import java.security.KeyPairGenerator
import java.security.KeyStore
import java.security.PrivateKey
import java.security.Signature
import java.security.spec.ECGenParameterSpec

/**
 * This device's PIAL signing key.
 *
 * The identity root is the person's, not a platform's: there is no Apple ID behind
 * it, no Google account, no phone number, and no email that can be taken away. What
 * makes it real on a phone is that the key proving it lives in hardware the phone
 * will not export — generated here, used here, and never transmitted. The server
 * receives the public half and nothing else, which is why a device can be revoked
 * without touching the identity, and the identity survives every device.
 *
 * The curve, the digest and the signature encoding are the server's, not this
 * client's choice: ECDSA over P-256, SHA-256, and IEEE P1363 (r‖s) because that is
 * what the verifier parses.
 */
class DeviceKey {

    /** True when this device already holds a PIAL signing key. */
    fun exists(): Boolean = keyStore().containsAlias(ALIAS)

    /**
     * Returns the device's public key in the SPKI base64 form the key authority
     * stores, generating the keypair on first use.
     *
     * StrongBox is requested when the device has it — a separate security chip, so a
     * compromise of the main OS still cannot extract the key. Its absence is not an
     * error; the TEE-backed key is the ordinary case.
     */
    fun publicKeyB64(): String {
        val existing = keyStore().getCertificate(ALIAS)?.publicKey
        if (existing != null) return Base64.encodeToString(existing.encoded, Base64.NO_WRAP)

        val generator = KeyPairGenerator.getInstance(
            KeyProperties.KEY_ALGORITHM_EC,
            ANDROID_KEYSTORE,
        )
        val spec = KeyGenParameterSpec.Builder(ALIAS, KeyProperties.PURPOSE_SIGN)
            .setAlgorithmParameterSpec(ECGenParameterSpec("secp256r1"))
            .setDigests(KeyProperties.DIGEST_SHA256)
            .apply {
                if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.P) {
                    setIsStrongBoxBacked(true)
                }
            }
            .build()

        val pair = try {
            generator.initialize(spec)
            generator.generateKeyPair()
        } catch (_: Exception) {
            // No StrongBox on this device. Fall back to the TEE-backed key, which is
            // still non-exportable — the property that matters.
            val fallback = KeyGenParameterSpec.Builder(ALIAS, KeyProperties.PURPOSE_SIGN)
                .setAlgorithmParameterSpec(ECGenParameterSpec("secp256r1"))
                .setDigests(KeyProperties.DIGEST_SHA256)
                .build()
            generator.initialize(fallback)
            generator.generateKeyPair()
        }

        return Base64.encodeToString(pair.public.encoded, Base64.NO_WRAP)
    }

    /**
     * Signs the UTF-8 bytes of [cid] and returns the signature the server verifies:
     * base64url without padding, over the 64-byte r‖s form.
     */
    fun sign(cid: String): String? {
        val key = keyStore().getKey(ALIAS, null) as? PrivateKey ?: return null
        return runCatching {
            val der = Signature.getInstance("SHA256withECDSA").run {
                initSign(key)
                update(cid.toByteArray(Charsets.UTF_8))
                sign()
            }
            Base64.encodeToString(derToP1363(der), Base64.URL_SAFE or Base64.NO_PADDING or Base64.NO_WRAP)
        }.getOrNull()
    }

    /** Destroys the device's signing key. The identity is untouched; this device is not. */
    fun forget() {
        runCatching { keyStore().deleteEntry(ALIAS) }
    }

    private fun keyStore(): KeyStore = KeyStore.getInstance(ANDROID_KEYSTORE).apply { load(null) }

    private companion object {
        const val ANDROID_KEYSTORE = "AndroidKeyStore"
        const val ALIAS = "f33d3r.pial.signing.v1"
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
