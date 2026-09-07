package com.f33d3r.app.net

import android.content.Context
import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyProperties
import android.util.Base64
import java.security.KeyStore
import javax.crypto.Cipher
import javax.crypto.KeyGenerator
import javax.crypto.SecretKey
import javax.crypto.spec.GCMParameterSpec

/**
 * Storage for the values that must never leave the device in the clear: the session
 * token and the account's sealed-messaging private key.
 *
 * Every value is sealed under an AES-256-GCM key that lives in the Android Keystore
 * and is not extractable — the ciphertext in SharedPreferences is useless off the
 * device, and useless to another app on it. This is custody, not obfuscation: the
 * private key the Gnosis lane depends on is exactly the thing the server promises it
 * never holds, so the client must not hold it anywhere weaker.
 */
class SecureStore(context: Context) {

    private val prefs = context.applicationContext
        .getSharedPreferences("f33d3r.secure", Context.MODE_PRIVATE)

    fun put(name: String, value: String?) {
        if (value == null) {
            prefs.edit().remove(name).apply()
            return
        }
        val cipher = Cipher.getInstance(TRANSFORMATION).apply { init(Cipher.ENCRYPT_MODE, key()) }
        val ct = cipher.doFinal(value.toByteArray(Charsets.UTF_8))
        val blob = cipher.iv + ct
        prefs.edit().putString(name, Base64.encodeToString(blob, Base64.NO_WRAP)).apply()
    }

    fun get(name: String): String? {
        val stored = prefs.getString(name, null) ?: return null
        return try {
            val blob = Base64.decode(stored, Base64.NO_WRAP)
            if (blob.size <= IV_BYTES) return null
            val iv = blob.copyOfRange(0, IV_BYTES)
            val ct = blob.copyOfRange(IV_BYTES, blob.size)
            val cipher = Cipher.getInstance(TRANSFORMATION).apply {
                init(Cipher.DECRYPT_MODE, key(), GCMParameterSpec(TAG_BITS, iv))
            }
            String(cipher.doFinal(ct), Charsets.UTF_8)
        } catch (_: Exception) {
            // A value that will not open is a value that is gone: the keystore key was
            // invalidated (device credential removed, app data restored to another
            // device). Report absence so the caller re-authenticates rather than
            // limping along with a corrupt secret.
            prefs.edit().remove(name).apply()
            null
        }
    }

    fun clear() = prefs.edit().clear().apply()

    private fun key(): SecretKey {
        val ks = KeyStore.getInstance(ANDROID_KEYSTORE).apply { load(null) }
        (ks.getEntry(KEY_ALIAS, null) as? KeyStore.SecretKeyEntry)?.let { return it.secretKey }

        val generator = KeyGenerator.getInstance(KeyProperties.KEY_ALGORITHM_AES, ANDROID_KEYSTORE)
        generator.init(
            KeyGenParameterSpec.Builder(
                KEY_ALIAS,
                KeyProperties.PURPOSE_ENCRYPT or KeyProperties.PURPOSE_DECRYPT,
            )
                .setBlockModes(KeyProperties.BLOCK_MODE_GCM)
                .setEncryptionPaddings(KeyProperties.ENCRYPTION_PADDING_NONE)
                .setKeySize(256)
                .build()
        )
        return generator.generateKey()
    }

    private companion object {
        const val ANDROID_KEYSTORE = "AndroidKeyStore"
        const val KEY_ALIAS = "f33d3r.secure.v1"
        const val TRANSFORMATION = "AES/GCM/NoPadding"
        const val IV_BYTES = 12
        const val TAG_BITS = 128
    }
}
