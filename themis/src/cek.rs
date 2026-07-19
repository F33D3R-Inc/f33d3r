use anyhow::{anyhow, Result};
use base64::{engine::general_purpose::STANDARD as B64, Engine};
use p256::{
    ecdh::diffie_hellman,
    elliptic_curve::sec1::ToEncodedPoint,
    pkcs8::DecodePrivateKey,
    PublicKey, SecretKey,
};
use hkdf::Hkdf;
use sha2::Sha256;
use aes_gcm::{
    aead::{Aead, KeyInit},
    Aes256Gcm, Key, Nonce,
};
use rand::RngCore;
use serde::{Deserialize, Serialize};
use zeroize::Zeroize;

const HKDF_INFO: &[u8] = b"themis-cek-wrap-v1";

/// Wire format for an ECIES-wrapped CEK.
/// Browser produces this; Themis consumes and re-produces it.
#[derive(Debug, Serialize, Deserialize)]
pub struct WrappedCek {
    pub ephemeral_pub: String,  // base64 of uncompressed P-256 point (65 bytes)
    pub iv:            String,  // base64 of 12-byte GCM nonce
    pub ciphertext:    String,  // base64 of AES-256-GCM ciphertext (CEK + 16-byte tag)
}

/// Unwrap a CEK encrypted for `recipient_priv`.
pub fn unwrap_cek(recipient_priv: &SecretKey, wrapped_json: &[u8]) -> Result<Vec<u8>> {
    let w: WrappedCek = serde_json::from_slice(wrapped_json)?;
    let ephemeral_pub_bytes = B64.decode(&w.ephemeral_pub)?;
    let ephemeral_pub = PublicKey::from_sec1_bytes(&ephemeral_pub_bytes)?;
    let iv_bytes = B64.decode(&w.iv)?;
    let ciphertext_bytes = B64.decode(&w.ciphertext)?;

    let shared = diffie_hellman(recipient_priv.to_nonzero_scalar(), ephemeral_pub.as_affine());
    let mut wrapping_key = [0u8; 32];
    let hk = Hkdf::<Sha256>::new(None, shared.raw_secret_bytes().as_slice());
    hk.expand(HKDF_INFO, &mut wrapping_key)
        .map_err(|_| anyhow!("HKDF expand failed"))?;

    let cipher = Aes256Gcm::new(Key::<Aes256Gcm>::from_slice(&wrapping_key));
    if iv_bytes.len() != 12 {
        return Err(anyhow!("invalid IV length"));
    }
    let nonce = Nonce::from_slice(&iv_bytes);
    let cek = cipher.decrypt(nonce, ciphertext_bytes.as_ref())
        .map_err(|_| anyhow!("CEK decryption failed"))?;

    wrapping_key.zeroize();
    Ok(cek)
}

/// Wrap a CEK for `recipient_pub` using a fresh ephemeral keypair.
pub fn wrap_cek_for(recipient_pub: &PublicKey, cek: &[u8]) -> Result<Vec<u8>> {
    let ephemeral_priv = SecretKey::random(&mut rand::thread_rng());
    let ephemeral_pub = ephemeral_priv.public_key();

    let shared = diffie_hellman(ephemeral_priv.to_nonzero_scalar(), recipient_pub.as_affine());
    let mut wrapping_key = [0u8; 32];
    let hk = Hkdf::<Sha256>::new(None, shared.raw_secret_bytes().as_slice());
    hk.expand(HKDF_INFO, &mut wrapping_key)
        .map_err(|_| anyhow!("HKDF expand failed"))?;

    let mut iv = [0u8; 12];
    rand::thread_rng().fill_bytes(&mut iv);

    let cipher = Aes256Gcm::new(Key::<Aes256Gcm>::from_slice(&wrapping_key));
    let nonce = Nonce::from_slice(&iv);
    let ciphertext = cipher.encrypt(nonce, cek)
        .map_err(|_| anyhow!("CEK encryption failed"))?;

    wrapping_key.zeroize();

    let pub_bytes = ephemeral_pub.to_encoded_point(false);
    let w = WrappedCek {
        ephemeral_pub: B64.encode(pub_bytes.as_bytes()),
        iv:            B64.encode(iv),
        ciphertext:    B64.encode(&ciphertext),
    };
    Ok(serde_json::to_vec(&w)?)
}

/// Load Themis's ECDH private key from a base64-encoded PKCS8 DER string.
/// Returns the SecretKey.
pub fn load_privkey(b64: &str) -> Result<SecretKey> {
    if b64.is_empty() {
        // Generate ephemeral key for dev — log a warning
        tracing::warn!("THEMIS_ECDH_PRIVKEY_B64 not set — using ephemeral key (dev only)");
        return Ok(SecretKey::random(&mut rand::thread_rng()));
    }
    let der = B64.decode(b64)?;
    let key = SecretKey::from_pkcs8_der(&der)
        .map_err(|e| anyhow!("invalid PKCS8 key: {e}"))?;
    Ok(key)
}

/// Export a public key as uncompressed SEC1 bytes, base64-encoded.
pub fn pubkey_to_b64(pub_key: &p256::PublicKey) -> String {
    let pt = pub_key.to_encoded_point(false);
    B64.encode(pt.as_bytes())
}

/// Parse a public key from base64-encoded uncompressed SEC1 bytes.
pub fn pubkey_from_b64(b64: &str) -> Result<p256::PublicKey> {
    let bytes = B64.decode(b64)?;
    let key = p256::PublicKey::from_sec1_bytes(&bytes)
        .map_err(|e| anyhow!("invalid public key: {e}"))?;
    Ok(key)
}
