//! Encryption at rest for the 2257 ID copies.
//!
//! 18 U.S.C. 2257 obliges the platform to keep a copy of each performer's
//! identification document. That copy is the most sensitive thing verity holds,
//! so it is never written in the clear: every image is sealed with AES-256-GCM
//! under `VERITY_RECORDS_KEY` before it reaches the database.
//!
//! * The key is 32 bytes, supplied base64-encoded. Verity refuses to start
//!   without it — there is no unencrypted mode to fall back to.
//! * Every image gets its own random 96-bit nonce from the OS CSPRNG.
//! * The additional authenticated data binds each ciphertext to the submission
//!   it belongs to and to its role (`front`, `back`, `portrait`). A blob copied
//!   onto another row, or swapped between roles, fails authentication instead
//!   of decrypting as somebody else's document.
//! * `key_id` is a one-way fingerprint of the key, stored beside each copy, so
//!   a rotation can tell which key sealed which row without the key ever being
//!   stored.

use base64::Engine as _;
use ring::aead::{Aad, LessSafeKey, Nonce, UnboundKey, AES_256_GCM, NONCE_LEN};
use ring::rand::{SecureRandom, SystemRandom};
use sha2::{Digest, Sha256};
use uuid::Uuid;

pub const RECORDS_KEY_ENV: &str = "VERITY_RECORDS_KEY";

/// One sealed image: the nonce it was sealed under and the ciphertext with its
/// 16-byte GCM tag appended.
pub struct Sealed {
    pub nonce: Vec<u8>,
    pub ciphertext: Vec<u8>,
}

pub struct RecordsKey {
    key: LessSafeKey,
    key_id: String,
    rng: SystemRandom,
}

impl RecordsKey {
    /// Loads and validates `VERITY_RECORDS_KEY`. Any failure is fatal to start-up.
    pub fn from_env() -> anyhow::Result<Self> {
        let raw = std::env::var(RECORDS_KEY_ENV).map_err(|_| {
            anyhow::anyhow!(
                "{RECORDS_KEY_ENV} must be set: it encrypts the 2257 ID copies at rest \
                 (32 random bytes, base64 — generate with `openssl rand -base64 32`)"
            )
        })?;
        let bytes = base64::engine::general_purpose::STANDARD
            .decode(raw.trim())
            .map_err(|e| anyhow::anyhow!("{RECORDS_KEY_ENV} is not valid base64: {e}"))?;
        if bytes.len() != 32 {
            anyhow::bail!(
                "{RECORDS_KEY_ENV} must decode to exactly 32 bytes for AES-256-GCM; it decodes to {}",
                bytes.len()
            );
        }

        let mut h = Sha256::new();
        h.update(b"verity-records-key-id-v1:");
        h.update(&bytes);
        let key_id = hex::encode(&h.finalize()[..8]);

        let unbound = UnboundKey::new(&AES_256_GCM, &bytes)
            .map_err(|_| anyhow::anyhow!("{RECORDS_KEY_ENV} was refused by AES-256-GCM"))?;

        Ok(Self {
            key: LessSafeKey::new(unbound),
            key_id,
            rng: SystemRandom::new(),
        })
    }

    pub fn key_id(&self) -> &str {
        &self.key_id
    }

    /// Seals one image of one submission under a fresh random nonce.
    pub fn seal(&self, submission_id: Uuid, role: &str, plaintext: &[u8]) -> anyhow::Result<Sealed> {
        let mut nonce = [0u8; NONCE_LEN];
        self.rng
            .fill(&mut nonce)
            .map_err(|_| anyhow::anyhow!("the OS random source failed to produce a nonce"))?;

        let aad = aad_for(submission_id, role);
        let mut in_out = plaintext.to_vec();
        self.key
            .seal_in_place_append_tag(
                Nonce::assume_unique_for_key(nonce),
                Aad::from(aad.as_bytes()),
                &mut in_out,
            )
            .map_err(|_| anyhow::anyhow!("AES-256-GCM seal failed"))?;

        Ok(Sealed {
            nonce: nonce.to_vec(),
            ciphertext: in_out,
        })
    }
}

/// The context every ciphertext is authenticated against. Versioned so the
/// binding can change without ambiguity about which rows use which form.
fn aad_for(submission_id: Uuid, role: &str) -> String {
    format!("verity-id-copy-v1|{submission_id}|{role}")
}
