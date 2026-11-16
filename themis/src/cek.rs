use aes_gcm::{
    aead::{Aead, KeyInit},
    Aes256Gcm, Key, Nonce,
};
use anyhow::{anyhow, Result};
use base64::{engine::general_purpose::STANDARD as B64, Engine};
use hkdf::Hkdf;
use p256::{
    ecdh::diffie_hellman, elliptic_curve::sec1::ToEncodedPoint, pkcs8::DecodePrivateKey, PublicKey,
    SecretKey,
};
use rand::RngCore;
use serde::{Deserialize, Serialize};
use sha2::Sha256;
use zeroize::{Zeroize, Zeroizing};

const HKDF_INFO: &[u8] = b"themis-cek-wrap-v1";

/// Wire format for an ECIES-wrapped CEK.
/// Browser produces this; Themis consumes and re-produces it.
#[derive(Debug, Serialize, Deserialize)]
pub struct WrappedCek {
    pub ephemeral_pub: String, // base64 of uncompressed P-256 point (65 bytes)
    pub iv: String,            // base64 of 12-byte GCM nonce
    pub ciphertext: String,    // base64 of AES-256-GCM ciphertext (CEK + 16-byte tag)
}

/// Unwrap a CEK encrypted for `recipient_priv`. The plaintext key is returned
/// in a buffer that overwrites itself when dropped, so it never outlives the
/// re-wrap it exists for.
pub fn unwrap_cek(recipient_priv: &SecretKey, wrapped_json: &[u8]) -> Result<Zeroizing<Vec<u8>>> {
    let w: WrappedCek = serde_json::from_slice(wrapped_json)?;
    let ephemeral_pub_bytes = B64.decode(&w.ephemeral_pub)?;
    let ephemeral_pub = PublicKey::from_sec1_bytes(&ephemeral_pub_bytes)?;
    let iv_bytes = B64.decode(&w.iv)?;
    let ciphertext_bytes = B64.decode(&w.ciphertext)?;

    let shared = diffie_hellman(
        recipient_priv.to_nonzero_scalar(),
        ephemeral_pub.as_affine(),
    );
    let mut wrapping_key = [0u8; 32];
    let hk = Hkdf::<Sha256>::new(None, &shared.raw_secret_bytes()[..]);
    hk.expand(HKDF_INFO, &mut wrapping_key)
        .map_err(|_| anyhow!("HKDF expand failed"))?;

    let cipher = Aes256Gcm::new(&Key::<Aes256Gcm>::from(wrapping_key));
    let iv: [u8; 12] = iv_bytes
        .as_slice()
        .try_into()
        .map_err(|_| anyhow!("invalid IV length"))?;
    let nonce = Nonce::from(iv);
    let decrypted = cipher.decrypt(&nonce, ciphertext_bytes.as_ref());
    wrapping_key.zeroize();
    let cek = decrypted.map_err(|_| anyhow!("CEK decryption failed"))?;
    Ok(Zeroizing::new(cek))
}

/// Wrap a CEK for `recipient_pub` using a fresh ephemeral keypair.
pub fn wrap_cek_for(recipient_pub: &PublicKey, cek: &[u8]) -> Result<Vec<u8>> {
    let ephemeral_priv = SecretKey::random(&mut rand::thread_rng());
    let ephemeral_pub = ephemeral_priv.public_key();

    let shared = diffie_hellman(
        ephemeral_priv.to_nonzero_scalar(),
        recipient_pub.as_affine(),
    );
    let mut wrapping_key = [0u8; 32];
    let hk = Hkdf::<Sha256>::new(None, &shared.raw_secret_bytes()[..]);
    hk.expand(HKDF_INFO, &mut wrapping_key)
        .map_err(|_| anyhow!("HKDF expand failed"))?;

    let mut iv = [0u8; 12];
    rand::thread_rng().fill_bytes(&mut iv);

    let cipher = Aes256Gcm::new(&Key::<Aes256Gcm>::from(wrapping_key));
    let nonce = Nonce::from(iv);
    let encrypted = cipher.encrypt(&nonce, cek);
    wrapping_key.zeroize();
    let ciphertext = encrypted.map_err(|_| anyhow!("CEK encryption failed"))?;

    let pub_bytes = ephemeral_pub.to_encoded_point(false);
    let w = WrappedCek {
        ephemeral_pub: B64.encode(pub_bytes.as_bytes()),
        iv: B64.encode(iv),
        ciphertext: B64.encode(&ciphertext),
    };
    Ok(serde_json::to_vec(&w)?)
}

/// Parse Themis's ECDH private key from base64 PKCS8 DER.
fn parse_privkey(b64: &str) -> Result<SecretKey> {
    let der = Zeroizing::new(B64.decode(b64.trim())?);
    SecretKey::from_pkcs8_der(&der).map_err(|e| anyhow!("invalid PKCS8 key: {e}"))
}

const KEY_FILE: &str = "themis-ecdh.key";
/// Serialises first-boot key generation across replicas sharing one volume.
const KEYGEN_LOCK: i64 = 7331_0100;

/// Themis's ECDH private key. There is no ephemeral fallback: every listing's
/// content key is wrapped for this key, so a key that changed on restart would
/// strand every listing ever made.
///
/// `THEMIS_ECDH_PRIVKEY_B64` wins when set. Otherwise the key lives in
/// `THEMIS_ECDH_KEY_DIR/themis-ecdh.key` on its own volume: read when present,
/// generated once on first boot under a Postgres advisory lock (temp file 0600
/// → fsync → rename → fsync dir) so concurrent replicas never make two.
pub async fn load_privkey(pool: &sqlx::PgPool, env_b64: &str, key_dir: &str) -> Result<SecretKey> {
    if !env_b64.trim().is_empty() {
        return parse_privkey(env_b64);
    }
    if key_dir.trim().is_empty() {
        return Err(anyhow!("THEMIS_ECDH_PRIVKEY_B64 or THEMIS_ECDH_KEY_DIR is required"));
    }
    let dir = std::path::PathBuf::from(key_dir);
    {
        use std::os::unix::fs::DirBuilderExt;
        std::fs::DirBuilder::new()
            .recursive(true)
            .mode(0o700)
            .create(&dir)
            .map_err(|e| anyhow!("creating key dir {}: {e}", dir.display()))?;
    }
    let mut conn = pool.acquire().await?;
    sqlx::query("SELECT pg_advisory_lock($1)")
        .bind(KEYGEN_LOCK)
        .execute(&mut *conn)
        .await?;
    let out = load_or_create(&dir);
    sqlx::query("SELECT pg_advisory_unlock($1)")
        .bind(KEYGEN_LOCK)
        .execute(&mut *conn)
        .await?;
    out
}

fn load_or_create(dir: &std::path::Path) -> Result<SecretKey> {
    use p256::pkcs8::EncodePrivateKey;
    use std::io::Write;
    use std::os::unix::fs::OpenOptionsExt;

    let path = dir.join(KEY_FILE);
    if path.exists() {
        let raw = Zeroizing::new(
            std::fs::read_to_string(&path).map_err(|e| anyhow!("reading {}: {e}", path.display()))?,
        );
        return parse_privkey(&raw);
    }
    let key = SecretKey::random(&mut rand::rngs::OsRng);
    let der = key
        .to_pkcs8_der()
        .map_err(|e| anyhow!("encoding the new ECDH key: {e}"))?;
    let encoded = Zeroizing::new(format!("{}\n", B64.encode(der.as_bytes())));
    let tmp = dir.join(format!(".{KEY_FILE}.tmp"));
    let _ = std::fs::remove_file(&tmp);
    {
        let mut f = std::fs::OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o600)
            .open(&tmp)
            .map_err(|e| anyhow!("creating {}: {e}", tmp.display()))?;
        f.write_all(encoded.as_bytes())?;
        f.sync_all()?;
    }
    std::fs::rename(&tmp, &path).map_err(|e| anyhow!("installing {}: {e}", path.display()))?;
    std::fs::File::open(dir)?.sync_all()?;
    tracing::info!("generated Themis's ECDH key at {}", path.display());
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
    let key =
        p256::PublicKey::from_sec1_bytes(&bytes).map_err(|e| anyhow!("invalid public key: {e}"))?;
    Ok(key)
}
