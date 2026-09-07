//! sealcore — Gnosis sealed-mode crypto core.
//!
//! One audited primitive set, compiled to WASM for the browser unsealer. The
//! server never links this; it only stores the opaque outputs.
//!
//! Scheme:
//!   * Per message: a random 32-byte AES-256-GCM content key (CK) encrypts the
//!     body once.
//!   * Per recipient: an ephemeral X25519 key does ECDH with the recipient's
//!     public key; HKDF-SHA256 derives a wrap key; AES-256-GCM seals CK to that
//!     recipient. Only the holder of the recipient private key can unwrap CK.
//!   * Account key custody: the X25519 private key is wrapped under a key the
//!     browser derives at login (and, separately, under the backup code via
//!     Argon2id). The wrapped blob is all the server ever sees.
//!
//! Logic lives in `*_impl` fns returning `Result<_, String>` so the crypto is
//! unit-testable natively; the `#[wasm_bindgen]` wrappers just map the error to
//! a JsValue for the browser.

use aes_gcm::aead::{Aead, KeyInit};
use aes_gcm::{Aes256Gcm, Key, Nonce};
use argon2::Argon2;
use base64::engine::general_purpose::STANDARD as B64;
use base64::Engine;
use hkdf::Hkdf;
use rand_core::{OsRng, RngCore};
use serde::{Deserialize, Serialize};
use sha2::Sha256;
use wasm_bindgen::prelude::*;
use x25519_dalek::{PublicKey, StaticSecret};

const HKDF_INFO: &[u8] = b"sealcore-v1-x25519-wrap";

// ── wire types ──────────────────────────────────────────────────────────────

#[derive(Deserialize)]
struct RecipientIn {
    account: String,
    pub_b64: String,
}

#[derive(Serialize)]
struct SealedKeyOut {
    recipient_account: String,
    eph_pub_b64: String,
    sealed_b64: String,
    sealed_nonce_b64: String,
}

#[derive(Serialize)]
struct Envelope {
    body_ct_b64: String,
    body_nonce_b64: String,
    sealed: Vec<SealedKeyOut>,
}

#[derive(Serialize)]
struct Keypair {
    priv_b64: String,
    pub_b64: String,
}

#[derive(Serialize)]
struct Wrapped {
    ct_b64: String,
    nonce_b64: String,
}

// ── helpers (String errors, native-testable) ────────────────────────────────

fn rand_bytes<const N: usize>() -> [u8; N] {
    let mut b = [0u8; N];
    OsRng.fill_bytes(&mut b);
    b
}

fn b64d(s: &str) -> Result<Vec<u8>, String> {
    B64.decode(s.trim()).map_err(|e| format!("base64: {e}"))
}

fn arr32(v: &[u8], what: &str) -> Result<[u8; 32], String> {
    v.try_into().map_err(|_| format!("{what}: expected 32 bytes"))
}

fn aes_seal(key: &[u8; 32], plaintext: &[u8]) -> Result<(Vec<u8>, [u8; 12]), String> {
    let cipher = Aes256Gcm::new(Key::<Aes256Gcm>::from_slice(key));
    let nonce = rand_bytes::<12>();
    let ct = cipher
        .encrypt(Nonce::from_slice(&nonce), plaintext)
        .map_err(|_| "aes encrypt failed".to_string())?;
    Ok((ct, nonce))
}

fn aes_open(key: &[u8; 32], nonce: &[u8], ct: &[u8]) -> Result<Vec<u8>, String> {
    if nonce.len() != 12 {
        return Err("nonce: expected 12 bytes".to_string());
    }
    let cipher = Aes256Gcm::new(Key::<Aes256Gcm>::from_slice(key));
    cipher
        .decrypt(Nonce::from_slice(nonce), ct)
        .map_err(|_| "aes decrypt failed (wrong key or tampered)".to_string())
}

fn derive_wrap_key(secret: &StaticSecret, their_pub: &PublicKey) -> [u8; 32] {
    let shared = secret.diffie_hellman(their_pub);
    let hk = Hkdf::<Sha256>::new(None, shared.as_bytes());
    let mut okm = [0u8; 32];
    hk.expand(HKDF_INFO, &mut okm).expect("hkdf expand");
    okm
}

// ── core logic ──────────────────────────────────────────────────────────────

pub fn generate_keypair_impl() -> Result<String, String> {
    let secret = StaticSecret::random_from_rng(OsRng);
    let public = PublicKey::from(&secret);
    let kp = Keypair {
        priv_b64: B64.encode(secret.to_bytes()),
        pub_b64: B64.encode(public.as_bytes()),
    };
    serde_json::to_string(&kp).map_err(|e| e.to_string())
}

pub fn seal_message_impl(recipients_json: &str, plaintext: &str) -> Result<String, String> {
    let recipients: Vec<RecipientIn> =
        serde_json::from_str(recipients_json).map_err(|e| format!("recipients: {e}"))?;
    if recipients.is_empty() {
        return Err("no recipients".to_string());
    }
    let ck = rand_bytes::<32>();
    let (body_ct, body_nonce) = aes_seal(&ck, plaintext.as_bytes())?;

    let mut sealed = Vec::with_capacity(recipients.len());
    for r in &recipients {
        let their_pub = PublicKey::from(arr32(&b64d(&r.pub_b64)?, "recipient pub")?);
        let eph = StaticSecret::random_from_rng(OsRng);
        let eph_pub = PublicKey::from(&eph);
        let wrap_key = derive_wrap_key(&eph, &their_pub);
        let (sct, snonce) = aes_seal(&wrap_key, &ck)?;
        sealed.push(SealedKeyOut {
            recipient_account: r.account.clone(),
            eph_pub_b64: B64.encode(eph_pub.as_bytes()),
            sealed_b64: B64.encode(sct),
            sealed_nonce_b64: B64.encode(snonce),
        });
    }
    let env = Envelope {
        body_ct_b64: B64.encode(body_ct),
        body_nonce_b64: B64.encode(body_nonce),
        sealed,
    };
    serde_json::to_string(&env).map_err(|e| e.to_string())
}

pub fn open_message_impl(
    my_priv_b64: &str,
    eph_pub_b64: &str,
    sealed_b64: &str,
    sealed_nonce_b64: &str,
    body_ct_b64: &str,
    body_nonce_b64: &str,
) -> Result<String, String> {
    let secret = StaticSecret::from(arr32(&b64d(my_priv_b64)?, "my priv")?);
    let eph_pub = PublicKey::from(arr32(&b64d(eph_pub_b64)?, "eph pub")?);
    let wrap_key = derive_wrap_key(&secret, &eph_pub);

    let ck_vec = aes_open(&wrap_key, &b64d(sealed_nonce_b64)?, &b64d(sealed_b64)?)?;
    let ck = arr32(&ck_vec, "content key")?;

    let body = aes_open(&ck, &b64d(body_nonce_b64)?, &b64d(body_ct_b64)?)?;
    String::from_utf8(body).map_err(|_| "plaintext not utf-8".to_string())
}

fn derive_backup_key_impl(code: &str, salt_b64: &str) -> Result<String, String> {
    let salt = b64d(salt_b64)?;
    let mut out = [0u8; 32];
    Argon2::default()
        .hash_password_into(code.as_bytes(), &salt, &mut out)
        .map_err(|e| format!("argon2: {e}"))?;
    Ok(B64.encode(out))
}

fn wrap_with_key_impl(key_b64: &str, plaintext_b64: &str) -> Result<String, String> {
    let key = arr32(&b64d(key_b64)?, "wrap key")?;
    let (ct, nonce) = aes_seal(&key, &b64d(plaintext_b64)?)?;
    let w = Wrapped {
        ct_b64: B64.encode(ct),
        nonce_b64: B64.encode(nonce),
    };
    serde_json::to_string(&w).map_err(|e| e.to_string())
}

fn unwrap_with_key_impl(key_b64: &str, ct_b64: &str, nonce_b64: &str) -> Result<String, String> {
    let key = arr32(&b64d(key_b64)?, "wrap key")?;
    let pt = aes_open(&key, &b64d(nonce_b64)?, &b64d(ct_b64)?)?;
    Ok(B64.encode(pt))
}

// ── exported WASM API (maps String error -> JsValue) ────────────────────────

fn js(r: Result<String, String>) -> Result<String, JsValue> {
    r.map_err(|e| JsValue::from_str(&e))
}

/// Generate a fresh X25519 identity keypair (base64).
#[wasm_bindgen]
pub fn generate_keypair() -> Result<String, JsValue> {
    js(generate_keypair_impl())
}

/// Generate a random 16-byte salt (base64) for Argon2id / key derivation.
#[wasm_bindgen]
pub fn generate_salt() -> String {
    B64.encode(rand_bytes::<16>())
}

/// Seal a plaintext message to a set of recipients. `recipients_json` is
/// `[{"account":"...","pub_b64":"..."}]`. Returns an Envelope JSON.
#[wasm_bindgen]
pub fn seal_message(recipients_json: &str, plaintext: &str) -> Result<String, JsValue> {
    js(seal_message_impl(recipients_json, plaintext))
}

/// Open a sealed message addressed to me. Returns the plaintext.
#[wasm_bindgen]
pub fn open_message(
    my_priv_b64: &str,
    eph_pub_b64: &str,
    sealed_b64: &str,
    sealed_nonce_b64: &str,
    body_ct_b64: &str,
    body_nonce_b64: &str,
) -> Result<String, JsValue> {
    js(open_message_impl(
        my_priv_b64,
        eph_pub_b64,
        sealed_b64,
        sealed_nonce_b64,
        body_ct_b64,
        body_nonce_b64,
    ))
}

/// Argon2id: derive a 32-byte key (base64) from a backup code + salt.
#[wasm_bindgen]
pub fn derive_backup_key(code: &str, salt_b64: &str) -> Result<String, JsValue> {
    js(derive_backup_key_impl(code, salt_b64))
}

/// Wrap a secret under a 32-byte symmetric key. Returns `{ct_b64, nonce_b64}`.
#[wasm_bindgen]
pub fn wrap_with_key(key_b64: &str, plaintext_b64: &str) -> Result<String, JsValue> {
    js(wrap_with_key_impl(key_b64, plaintext_b64))
}

/// Unwrap a secret sealed with `wrap_with_key`. Returns base64 of the plaintext.
#[wasm_bindgen]
pub fn unwrap_with_key(key_b64: &str, ct_b64: &str, nonce_b64: &str) -> Result<String, JsValue> {
    js(unwrap_with_key_impl(key_b64, ct_b64, nonce_b64))
}

// ── native roundtrip tests (`cargo test`) ───────────────────────────────────

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::Value;

    fn kp() -> (String, String) {
        let j: Value = serde_json::from_str(&generate_keypair_impl().unwrap()).unwrap();
        (
            j["priv_b64"].as_str().unwrap().to_string(),
            j["pub_b64"].as_str().unwrap().to_string(),
        )
    }

    fn open(env: &Value, idx: usize, priv_k: &str) -> Result<String, String> {
        let s = &env["sealed"][idx];
        open_message_impl(
            priv_k,
            s["eph_pub_b64"].as_str().unwrap(),
            s["sealed_b64"].as_str().unwrap(),
            s["sealed_nonce_b64"].as_str().unwrap(),
            env["body_ct_b64"].as_str().unwrap(),
            env["body_nonce_b64"].as_str().unwrap(),
        )
    }

    #[test]
    fn seal_open_roundtrip_single() {
        let (bpriv, bpub) = kp();
        let recips = format!(r#"[{{"account":"B","pub_b64":"{bpub}"}}]"#);
        let env: Value = serde_json::from_str(&seal_message_impl(&recips, "hello sealed").unwrap()).unwrap();
        assert_eq!(open(&env, 0, &bpriv).unwrap(), "hello sealed");
    }

    #[test]
    fn group_each_recipient_opens() {
        let (bpriv, bpub) = kp();
        let (cpriv, cpub) = kp();
        let recips =
            format!(r#"[{{"account":"B","pub_b64":"{bpub}"}},{{"account":"C","pub_b64":"{cpub}"}}]"#);
        let env: Value = serde_json::from_str(&seal_message_impl(&recips, "group msg").unwrap()).unwrap();
        assert_eq!(open(&env, 0, &bpriv).unwrap(), "group msg");
        assert_eq!(open(&env, 1, &cpriv).unwrap(), "group msg");
    }

    #[test]
    fn wrong_key_fails() {
        let (_bpriv, bpub) = kp();
        let (wrong_priv, _) = kp();
        let recips = format!(r#"[{{"account":"B","pub_b64":"{bpub}"}}]"#);
        let env: Value = serde_json::from_str(&seal_message_impl(&recips, "secret").unwrap()).unwrap();
        assert!(open(&env, 0, &wrong_priv).is_err());
    }

    #[test]
    fn backup_wrap_roundtrip() {
        let (priv_b64, _) = kp();
        let salt = generate_salt();
        let key = derive_backup_key_impl("correct horse battery staple", &salt).unwrap();
        let wrapped: Value = serde_json::from_str(&wrap_with_key_impl(&key, &priv_b64).unwrap()).unwrap();
        let key2 = derive_backup_key_impl("correct horse battery staple", &salt).unwrap();
        let out = unwrap_with_key_impl(
            &key2,
            wrapped["ct_b64"].as_str().unwrap(),
            wrapped["nonce_b64"].as_str().unwrap(),
        )
        .unwrap();
        assert_eq!(out, priv_b64);
    }
}
