/// AMP v1 cryptographic primitives.
///
/// Security properties:
///   Forward secrecy    — compromise of current keys does not expose past messages.
///   Break-in recovery  — future messages secure after partial compromise (via DH ratchet).
///   Authenticity       — all payloads bound to sender+recipient identities via AEAD AAD.
///   Deniability        — no long-term signing key; no transcript proof of conversation.
///
/// Algorithm choices:
///   Key exchange   X25519       — 128-bit security, Curve25519, fast on constrained devices
///   KDF            HKDF-SHA256  — standard, audited, composable
///   Hashing        BLAKE3       — faster than SHA-256, parallel, keyed variant for MACs
///   AEAD           ChaCha20-Poly1305 — constant-time, no timing channels, no hardware req
///   Identity hash  BLAKE3       — identity = BASE58(BLAKE3(public_key_bytes))
///
/// Post-quantum note:
///   The X25519 KEM can be replaced with ML-KEM-768 (CRYSTALS-Kyber) in AMP v2.
///   The design is hybrid-ready: just chain KDF outputs.

use anyhow::{anyhow, Result};
use base64::{engine::general_purpose::URL_SAFE_NO_PAD as BASE64, Engine};
use chacha20poly1305::{
    aead::{Aead, KeyInit, Payload},
    ChaCha20Poly1305, Key, Nonce,
};
use hkdf::Hkdf;
use rand::RngCore;
use sha2::Sha256;
use x25519_dalek::{PublicKey, StaticSecret};
use zeroize::{Zeroize, ZeroizeOnDrop};

pub const KEY_LEN:   usize = 32;
pub const NONCE_LEN: usize = 12;
pub const TAG_LEN:   usize = 16;

/// A 32-byte symmetric key. Zeroed in memory on drop.
#[derive(Clone, Zeroize, ZeroizeOnDrop)]
pub struct SymKey(pub [u8; KEY_LEN]);

impl SymKey {
    pub fn random() -> Self {
        let mut k = [0u8; KEY_LEN];
        rand::thread_rng().fill_bytes(&mut k);
        SymKey(k)
    }

    pub fn from_bytes(b: [u8; KEY_LEN]) -> Self { SymKey(b) }
    pub fn as_bytes(&self) -> &[u8; KEY_LEN] { &self.0 }
}

// ── Key generation ────────────────────────────────────────────────────────────

/// Generate a fresh X25519 keypair.
pub fn generate_keypair() -> (StaticSecret, PublicKey) {
    let secret = StaticSecret::random_from_rng(rand::thread_rng());
    let public = PublicKey::from(&secret);
    (secret, public)
}

// ── Identity derivation ───────────────────────────────────────────────────────

/// Derive the AMP identity string: BASE58( BLAKE3( public_key_bytes ) ).
/// Deterministic, collision-resistant, 43 characters in BASE58.
pub fn derive_identity(public_key: &PublicKey) -> String {
    let hash = blake3::hash(public_key.as_bytes());
    bs58::encode(hash.as_bytes()).into_string()
}

/// Constant-time identity equality check.
pub fn identity_eq(a: &str, b: &str) -> bool {
    use subtle::ConstantTimeEq;
    let ab = a.as_bytes();
    let bb = b.as_bytes();
    if ab.len() != bb.len() { return false; }
    ab.ct_eq(bb).into()
}

// ── Diffie-Hellman ────────────────────────────────────────────────────────────

/// X25519 DH: shared_secret = secret * their_public.
pub fn x25519_dh(secret: &StaticSecret, their_public: &PublicKey) -> [u8; 32] {
    secret.diffie_hellman(their_public).to_bytes()
}

// ── Key derivation (HKDF-SHA256) ──────────────────────────────────────────────

/// HKDF-SHA256. Returns `length` bytes of key material.
pub fn hkdf(ikm: &[u8], salt: &[u8], info: &[u8], length: usize) -> Result<Vec<u8>> {
    let h = Hkdf::<Sha256>::new(Some(salt), ikm);
    let mut out = vec![0u8; length];
    h.expand(info, &mut out).map_err(|_| anyhow!("HKDF output too long"))?;
    Ok(out)
}

/// BLAKE3 keyed hash — fast MAC alternative for chunk content addressing.
pub fn blake3_keyed(key: &[u8; 32], data: &[u8]) -> [u8; 32] {
    let mut hasher = blake3::Hasher::new_keyed(key);
    *hasher.update(data).finalize().as_bytes()
}

/// Content hash for a payload chunk — used for deduplication and integrity.
pub fn content_hash(data: &[u8]) -> [u8; 32] {
    *blake3::hash(data).as_bytes()
}

// ── Double Ratchet KDF chains ─────────────────────────────────────────────────

/// Root KDF: derive new root key + chain key from DH output.
/// RFC-compliant: HKDF with the old root key as salt.
pub fn kdf_rk(root_key: &[u8; 32], dh_out: &[u8; 32]) -> ([u8; 32], [u8; 32]) {
    let out = hkdf(dh_out, root_key, b"AMP_v1_RootKDF_v1", 64)
        .expect("HKDF never fails for 64 bytes");
    let mut new_rk = [0u8; 32];
    let mut ck     = [0u8; 32];
    new_rk.copy_from_slice(&out[..32]);
    ck.copy_from_slice(&out[32..]);
    (new_rk, ck)
}

/// Chain KDF: advance the KDF chain, producing a new chain key and a message key.
/// Uses BLAKE3 for speed — SHA-256 alternatives are drop-in replaceable here.
pub fn kdf_ck(chain_key: &[u8; 32]) -> ([u8; 32], [u8; 32]) {
    let new_ck = *blake3::Hasher::new_keyed(chain_key)
        .update(b"\x01")
        .finalize()
        .as_bytes();
    let mk = *blake3::Hasher::new_keyed(chain_key)
        .update(b"\x02")
        .finalize()
        .as_bytes();
    (new_ck, mk)
}

// ── AEAD encryption ───────────────────────────────────────────────────────────

/// Encrypt with ChaCha20-Poly1305.
/// Returns BASE64(12-byte nonce || ciphertext_with_16-byte tag).
/// `aad` binds the ciphertext to envelope metadata — tampering with AAD = auth failure.
pub fn aead_encrypt(key: &[u8; 32], plaintext: &[u8], aad: &[u8]) -> Result<String> {
    let k = Key::from_slice(key);
    let cipher = ChaCha20Poly1305::new(k);

    let mut nonce_bytes = [0u8; NONCE_LEN];
    rand::thread_rng().fill_bytes(&mut nonce_bytes);
    let nonce = Nonce::from_slice(&nonce_bytes);

    let ct = cipher
        .encrypt(nonce, Payload { msg: plaintext, aad })
        .map_err(|_| anyhow!("AEAD encryption failed"))?;

    let mut out = Vec::with_capacity(NONCE_LEN + ct.len());
    out.extend_from_slice(&nonce_bytes);
    out.extend_from_slice(&ct);
    Ok(BASE64.encode(&out))
}

/// Decrypt a payload produced by `aead_encrypt`.
pub fn aead_decrypt(key: &[u8; 32], payload_b64: &str, aad: &[u8]) -> Result<Vec<u8>> {
    let raw = BASE64
        .decode(payload_b64)
        .map_err(|_| anyhow!("base64 decode failed"))?;
    if raw.len() < NONCE_LEN + TAG_LEN {
        return Err(anyhow!("ciphertext too short to be valid"));
    }
    let (nonce_bytes, ct) = raw.split_at(NONCE_LEN);
    let k      = Key::from_slice(key);
    let cipher = ChaCha20Poly1305::new(k);
    let nonce  = Nonce::from_slice(nonce_bytes);

    cipher
        .decrypt(nonce, Payload { msg: ct, aad })
        .map_err(|_| anyhow!("AEAD authentication failed — wrong key, tampered ciphertext, or wrong AAD"))
}

// ── Chunk operations ──────────────────────────────────────────────────────────

pub const CHUNK_SIZE: usize = 64 * 1024; // 64 KiB per chunk

/// Split `data` into 64 KiB chunks. Last chunk may be smaller.
pub fn split_chunks(data: &[u8]) -> Vec<Vec<u8>> {
    data.chunks(CHUNK_SIZE)
        .map(|c| c.to_vec())
        .collect()
}

/// Derive a per-chunk key from the message key + chunk index.
/// Ensures each chunk has a unique key — limits blast radius of nonce reuse.
pub fn chunk_key(msg_key: &[u8; 32], chunk_index: u32) -> [u8; 32] {
    let mut idx_bytes = [0u8; 4];
    idx_bytes.copy_from_slice(&chunk_index.to_le_bytes());
    let out = hkdf(msg_key, &idx_bytes, b"AMP_v1_ChunkKey", 32).expect("HKDF");
    let mut k = [0u8; 32];
    k.copy_from_slice(&out);
    k
}

/// Encrypt a single chunk. Returns (content_hash_hex, encrypted_b64).
pub fn encrypt_chunk(msg_key: &[u8; 32], chunk_idx: u32, data: &[u8], aad: &[u8]) -> Result<(String, String)> {
    let ck   = chunk_key(msg_key, chunk_idx);
    let hash = hex::encode(&content_hash(data));
    let ct   = aead_encrypt(&ck, data, aad)?;
    Ok((hash, ct))
}

/// Decrypt a single chunk, verifying content hash.
pub fn decrypt_chunk(
    msg_key:    &[u8; 32],
    chunk_idx:  u32,
    ct_b64:     &str,
    aad:        &[u8],
    expected_hash: &str,
) -> Result<Vec<u8>> {
    let ck = chunk_key(msg_key, chunk_idx);
    let pt = aead_decrypt(&ck, ct_b64, aad)?;
    let actual_hash = hex::encode(&content_hash(&pt));
    if actual_hash != expected_hash {
        return Err(anyhow!("chunk content hash mismatch — data may be corrupted or tampered"));
    }
    Ok(pt)
}

// ── Proof-of-work (Hashcash-style anti-spam) ──────────────────────────────────

/// Verify that `nonce` makes BLAKE3(challenge || nonce) have `difficulty` leading zero bits.
/// Used for first-contact messages from unknown senders.
pub fn verify_pow(challenge: &[u8], nonce: u64, difficulty: u8) -> bool {
    let mut input = challenge.to_vec();
    input.extend_from_slice(&nonce.to_le_bytes());
    let hash = blake3::hash(&input);
    let bytes = hash.as_bytes();
    let full_bytes = (difficulty / 8) as usize;
    let rem_bits   = difficulty % 8;
    for i in 0..full_bytes {
        if bytes[i] != 0 { return false; }
    }
    if rem_bits > 0 && full_bytes < 32 {
        let mask = 0xFF_u8 << (8 - rem_bits);
        if bytes[full_bytes] & mask != 0 { return false; }
    }
    true
}

/// Compute a proof-of-work nonce. Typical difficulty=18 takes ~2ms on modern hardware.
pub fn compute_pow(challenge: &[u8], difficulty: u8) -> u64 {
    let mut nonce = 0u64;
    loop {
        if verify_pow(challenge, nonce, difficulty) { return nonce; }
        nonce += 1;
    }
}

mod hex {
    pub fn encode(bytes: &[u8]) -> String {
        bytes.iter().map(|b| format!("{:02x}", b)).collect()
    }
}

// ── Tests ─────────────────────────────────────────────────────────────────────

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn aead_roundtrip() {
        let key = [0x42u8; 32];
        let pt  = b"ngos use this for real ops";
        let aad = b"recipient:sender:msgid";
        let ct  = aead_encrypt(&key, pt, aad).unwrap();
        assert_eq!(aead_decrypt(&key, &ct, aad).unwrap(), pt);
    }

    #[test]
    fn aead_rejects_tampered_aad() {
        let key = [0x11u8; 32];
        let ct  = aead_encrypt(&key, b"secret", b"alice:bob").unwrap();
        assert!(aead_decrypt(&key, &ct, b"mallory:bob").is_err());
    }

    #[test]
    fn aead_rejects_bit_flip() {
        let key = [0x22u8; 32];
        let ct  = aead_encrypt(&key, b"payload", b"").unwrap();
        // Flip a byte in the ciphertext portion (after nonce)
        let mut raw = base64::engine::general_purpose::URL_SAFE_NO_PAD.decode(&ct).unwrap();
        raw[15] ^= 0xFF;
        let tampered = base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(&raw);
        assert!(aead_decrypt(&key, &tampered, b"").is_err());
    }

    #[test]
    fn kdf_chain_advances_uniquely() {
        let ck0 = [0u8; 32];
        let (ck1, mk1) = kdf_ck(&ck0);
        let (ck2, mk2) = kdf_ck(&ck1);
        assert_ne!(ck0, ck1);
        assert_ne!(ck1, ck2);
        assert_ne!(mk1, mk2);
        // Chain keys and message keys must be distinct
        assert_ne!(ck1, mk1);
    }

    #[test]
    fn chunk_keys_unique_per_index() {
        let mk = [0x55u8; 32];
        assert_ne!(chunk_key(&mk, 0), chunk_key(&mk, 1));
        assert_ne!(chunk_key(&mk, 1), chunk_key(&mk, 2));
    }

    #[test]
    fn identity_derivation_deterministic() {
        let (_, pk) = generate_keypair();
        assert_eq!(derive_identity(&pk), derive_identity(&pk));
        // Different keys must produce different identities
        let (_, pk2) = generate_keypair();
        assert_ne!(derive_identity(&pk), derive_identity(&pk2));
    }

    #[test]
    fn pow_verify_low_difficulty() {
        let challenge = b"amp_pow_challenge";
        let nonce = compute_pow(challenge, 8);
        assert!(verify_pow(challenge, nonce, 8));
        assert!(!verify_pow(challenge, nonce + 1, 8));
    }

    #[test]
    fn chunk_encrypt_decrypt_with_hash_check() {
        let mk  = SymKey::random();
        let data = vec![0xABu8; 1024];
        let aad  = b"envelope_aad";
        let (hash, ct) = encrypt_chunk(mk.as_bytes(), 0, &data, aad).unwrap();
        let recovered  = decrypt_chunk(mk.as_bytes(), 0, &ct, aad, &hash).unwrap();
        assert_eq!(recovered, data);
    }
}
