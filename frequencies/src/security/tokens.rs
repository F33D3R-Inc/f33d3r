//! Short-lived signaling tokens.
//!
//! When Nantar authorizes a Tune In, Auralis mints a token that lets that
//! browser open the signaling socket for that Frequency as that session in
//! that role, once, for a minute. The token is what crosses the browser
//! boundary; the INTERNAL_API_KEY never does.
//!
//! Format: `base64url(claims-json) "." base64url(hmac-sha256(claims))`. Not a
//! JWT: no header, no algorithm negotiation, one key, one purpose. The estate
//! has no JWT library and this brain does not introduce one for a token only
//! it mints and only it verifies.
//!
//! Claims (§12): frequency_id, pial_id, session_id, role, permissions, iat,
//! exp, nonce. The nonce is burned in Redis on first use, so a captured token
//! opens nothing a second time.

use base64::{engine::general_purpose::URL_SAFE_NO_PAD as B64, Engine};
use chrono::Utc;
use hmac::{Hmac, Mac};
use serde::{Deserialize, Serialize};
use sha2::Sha256;
use uuid::Uuid;

use crate::domain::FrequencyRole;

type HmacSha256 = Hmac<Sha256>;

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct Claims {
    pub frequency_id: Uuid,
    /// Bare PIAL uuid.
    pub pial_id: String,
    pub session_id: Uuid,
    pub role: FrequencyRole,
    /// What the media plane may do for this session: "publish", "subscribe".
    pub permissions: Vec<String>,
    pub iat: i64,
    pub exp: i64,
    pub nonce: String,
    /// The node that minted it and owns the session. Signaling that lands on
    /// another node redirects there (Phase 6).
    pub node_id: String,
}

impl Claims {
    pub fn permissions_for(role: FrequencyRole) -> Vec<String> {
        let mut p = vec!["subscribe".to_string()];
        if role.speaks() {
            p.push("publish".to_string());
        }
        p
    }
}

/// Consumed by the signaling socket (Phase 3). Pinned by tests now so the
/// mint and the verify cannot drift apart before the socket exists.
#[cfg_attr(not(test), allow(dead_code))]
#[derive(Debug, thiserror::Error, PartialEq, Eq)]
pub enum TokenError {
    #[error("malformed token")]
    Malformed,
    #[error("bad signature")]
    BadSignature,
    #[error("token expired")]
    Expired,
    #[error("token not yet valid")]
    NotYetValid,
}

#[derive(Clone)]
pub struct Signer {
    key: Vec<u8>,
}

impl std::fmt::Debug for Signer {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("Signer(<redacted>)")
    }
}

impl Signer {
    /// A signer needs a secret. An empty one is refused at construction so
    /// `main` fails to boot rather than signing with nothing.
    pub fn new(secret: &str) -> anyhow::Result<Self> {
        if secret.trim().len() < 32 {
            anyhow::bail!("AURALIS_SIGNAL_SECRET must be set to at least 32 characters");
        }
        Ok(Self {
            key: secret.as_bytes().to_vec(),
        })
    }

    fn mac(&self, payload: &[u8]) -> Vec<u8> {
        let mut mac = HmacSha256::new_from_slice(&self.key).expect("hmac accepts any key length");
        mac.update(payload);
        mac.finalize().into_bytes().to_vec()
    }

    pub fn mint(&self, claims: &Claims) -> String {
        let payload = serde_json::to_vec(claims).expect("claims serialize");
        let sig = self.mac(&payload);
        format!("{}.{}", B64.encode(payload), B64.encode(sig))
    }

    /// Verify signature and time window. Nonce burning is the caller's job,
    /// because it needs Redis and this must stay pure. Consumed by the
    /// signaling socket (Phase 3); pinned by tests now.
    #[cfg_attr(not(test), allow(dead_code))]
    pub fn verify(&self, token: &str, now: i64) -> Result<Claims, TokenError> {
        if token.len() > 4096 {
            return Err(TokenError::Malformed);
        }
        let (p, s) = token.split_once('.').ok_or(TokenError::Malformed)?;
        let payload = B64.decode(p).map_err(|_| TokenError::Malformed)?;
        let sig = B64.decode(s).map_err(|_| TokenError::Malformed)?;
        let mut mac = HmacSha256::new_from_slice(&self.key).expect("hmac accepts any key length");
        mac.update(&payload);
        // Constant-time comparison; a byte-wise `==` would leak the prefix.
        mac.verify_slice(&sig)
            .map_err(|_| TokenError::BadSignature)?;
        let claims: Claims = serde_json::from_slice(&payload).map_err(|_| TokenError::Malformed)?;
        if now >= claims.exp {
            return Err(TokenError::Expired);
        }
        // A small skew allowance for a clock that is a few seconds behind.
        if now + 30 < claims.iat {
            return Err(TokenError::NotYetValid);
        }
        Ok(claims)
    }
}

pub fn new_nonce() -> String {
    Uuid::new_v4().simple().to_string()
}

pub fn now_unix() -> i64 {
    Utc::now().timestamp()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn signer() -> Signer {
        Signer::new("0123456789abcdef0123456789abcdef-test").unwrap()
    }

    fn claims(now: i64) -> Claims {
        Claims {
            frequency_id: Uuid::new_v4(),
            pial_id: "c0ffee00-0000-4000-8000-000000000001".into(),
            session_id: Uuid::new_v4(),
            role: FrequencyRole::Listener,
            permissions: Claims::permissions_for(FrequencyRole::Listener),
            iat: now,
            exp: now + 60,
            nonce: new_nonce(),
            node_id: "node-a".into(),
        }
    }

    #[test]
    fn round_trip() {
        let s = signer();
        let c = claims(1_000_000);
        let t = s.mint(&c);
        assert_eq!(s.verify(&t, 1_000_010).unwrap(), c);
    }

    #[test]
    fn expired_is_refused() {
        let s = signer();
        let t = s.mint(&claims(1_000_000));
        assert_eq!(s.verify(&t, 1_000_060), Err(TokenError::Expired));
    }

    #[test]
    fn a_different_key_is_refused() {
        let t = signer().mint(&claims(1_000_000));
        let other = Signer::new("ffffffffffffffffffffffffffffffff-other").unwrap();
        assert_eq!(other.verify(&t, 1_000_001), Err(TokenError::BadSignature));
    }

    /// The claim a browser would most like to edit. Changing one byte of the
    /// payload invalidates the signature; there is no way to be a host by
    /// asking.
    #[test]
    fn tampered_role_is_refused() {
        let s = signer();
        let mut c = claims(1_000_000);
        let t = s.mint(&c);
        c.role = FrequencyRole::Host;
        c.permissions = Claims::permissions_for(FrequencyRole::Host);
        let forged_payload = B64.encode(serde_json::to_vec(&c).unwrap());
        let sig = t.split_once('.').unwrap().1;
        let forged = format!("{forged_payload}.{sig}");
        assert_eq!(s.verify(&forged, 1_000_001), Err(TokenError::BadSignature));
    }

    #[test]
    fn short_secret_is_refused() {
        assert!(Signer::new("short").is_err());
        assert!(Signer::new("").is_err());
    }

    #[test]
    fn garbage_is_malformed_not_a_panic() {
        let s = signer();
        for bad in ["", ".", "a.b", "!!!.!!!", &"x".repeat(5000)] {
            assert!(matches!(
                s.verify(bad, 0),
                Err(TokenError::Malformed) | Err(TokenError::BadSignature)
            ));
        }
    }

    #[test]
    fn listeners_subscribe_and_speakers_publish() {
        assert_eq!(
            Claims::permissions_for(FrequencyRole::Listener),
            vec!["subscribe"]
        );
        assert_eq!(
            Claims::permissions_for(FrequencyRole::Speaker),
            vec!["subscribe", "publish"]
        );
        assert_eq!(
            Claims::permissions_for(FrequencyRole::Host),
            vec!["subscribe", "publish"]
        );
    }
}
