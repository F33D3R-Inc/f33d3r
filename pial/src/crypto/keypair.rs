// F33D3R PIAL — Ed25519 Keypair Management
//
// Used by Elohim Veni to sign PIALTokens that brains can verify
// without calling Elohim Veni on every request.
//
// Key rotation: Elohim Veni publishes its current public key to the
// schema registry. Brains fetch it on startup and cache with 24h TTL.

use anyhow::{anyhow, Result};
use base64::{engine::general_purpose::STANDARD as B64, Engine};
use ed25519_dalek::{Signature, Signer, SigningKey, Verifier, VerifyingKey};
use rand::rngs::OsRng;

pub struct PIALSigningKey {
    inner: SigningKey,
}

pub struct PIALVerifyingKey {
    inner: VerifyingKey,
}

impl PIALSigningKey {
    /// Generate a new keypair from OS random source.
    pub fn generate() -> Self {
        let mut csprng = OsRng;
        Self {
            inner: SigningKey::generate(&mut csprng),
        }
    }

    /// Load from bytes (for persistence).
    pub fn from_bytes(bytes: &[u8]) -> Result<Self> {
        let arr: [u8; 32] = bytes.try_into().map_err(|_| anyhow!("Invalid key length"))?;
        Ok(Self {
            inner: SigningKey::from_bytes(&arr),
        })
    }

    pub fn to_bytes(&self) -> [u8; 32] {
        self.inner.to_bytes()
    }

    /// Get the corresponding verifying key for distribution.
    pub fn verifying_key(&self) -> PIALVerifyingKey {
        PIALVerifyingKey {
            inner: self.inner.verifying_key(),
        }
    }

    /// Sign a payload, returning base64-encoded signature.
    pub fn sign(&self, payload: &[u8]) -> String {
        let sig: Signature = self.inner.sign(payload);
        B64.encode(sig.to_bytes())
    }
}

impl PIALVerifyingKey {
    /// Load from base64-encoded string (from schema registry).
    pub fn from_base64(encoded: &str) -> Result<Self> {
        let bytes = B64.decode(encoded)?;
        let arr: [u8; 32] = bytes
            .try_into()
            .map_err(|_| anyhow!("Invalid verifying key length"))?;
        Ok(Self {
            inner: VerifyingKey::from_bytes(&arr).map_err(|e| anyhow!("{e}"))?,
        })
    }

    pub fn to_base64(&self) -> String {
        B64.encode(self.inner.as_bytes())
    }

    /// Verify a base64-encoded signature over a payload.
    pub fn verify(&self, payload: &[u8], signature_b64: &str) -> Result<()> {
        let sig_bytes = B64.decode(signature_b64)?;
        let arr: [u8; 64] = sig_bytes
            .try_into()
            .map_err(|_| anyhow!("Invalid signature length"))?;
        let sig = Signature::from_bytes(&arr);
        self.inner
            .verify(payload, &sig)
            .map_err(|e| anyhow!("Signature verification failed: {e}"))
    }
}

/// Build the canonical signing payload for a PIAL token.
/// Format: "<pial_id>|<capabilities>|<expires_at_unix>"
pub fn token_payload(pial_id: &str, capabilities: &[&str], expires_at_unix: i64) -> Vec<u8> {
    let caps = capabilities.join(",");
    format!("{pial_id}|{caps}|{expires_at_unix}")
        .into_bytes()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn sign_and_verify_roundtrip() {
        let key = PIALSigningKey::generate();
        let vk = key.verifying_key();
        let payload = b"test-pial-id|POSTING,MESSAGING|1735689600";
        let sig = key.sign(payload);
        vk.verify(payload, &sig).expect("signature should verify");
    }

    #[test]
    fn tampered_payload_fails() {
        let key = PIALSigningKey::generate();
        let vk = key.verifying_key();
        let sig = key.sign(b"original");
        assert!(vk.verify(b"tampered", &sig).is_err());
    }

    #[test]
    fn wrong_key_fails() {
        let key1 = PIALSigningKey::generate();
        let key2 = PIALSigningKey::generate();
        let payload = b"some payload";
        let sig = key1.sign(payload);
        assert!(key2.verifying_key().verify(payload, &sig).is_err());
    }
}
