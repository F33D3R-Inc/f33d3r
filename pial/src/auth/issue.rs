// F33D3R PIAL — Token Issuance
//
// Elohim Veni issues signed PIALTokens that brains can verify locally
// without calling Elohim Veni on every request.
//
// Token TTL: 5 minutes (short enough to keep capability state fresh,
// long enough to absorb burst traffic without hammering Elohim Veni).

use crate::crypto::keypair::{token_payload, PIALSigningKey};
use crate::models::{CapabilityKind, PIALToken};
use chrono::{Duration, Utc};
use uuid::Uuid;

pub const TOKEN_TTL_SECONDS: i64 = 300; // 5 minutes

/// Issue a signed PIALToken for a given PIAL root.
/// Called by Elohim Veni only — never by any other brain.
pub fn issue_token(
    pial_id: Uuid,
    capabilities: Vec<CapabilityKind>,
    signing_key: &PIALSigningKey,
) -> PIALToken {
    let issued_at  = Utc::now();
    let expires_at = issued_at + Duration::seconds(TOKEN_TTL_SECONDS);

    let cap_strs: Vec<String> = capabilities.iter().map(|c| c.to_string()).collect();
    let cap_refs: Vec<&str>   = cap_strs.iter().map(|s| s.as_str()).collect();
    let payload = token_payload(&pial_id.to_string(), &cap_refs, expires_at.timestamp());

    let signature = signing_key.sign(&payload);

    PIALToken {
        pial_id,
        capabilities,
        issued_at,
        expires_at,
        signature,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::crypto::keypair::PIALSigningKey;

    #[test]
    fn issue_token_has_correct_capabilities() {
        let key = PIALSigningKey::generate();
        let pial = Uuid::new_v4();
        let caps = vec![CapabilityKind::Posting, CapabilityKind::Messaging];
        let token = issue_token(pial, caps.clone(), &key);
        assert_eq!(token.pial_id, pial);
        assert_eq!(token.capabilities, caps);
        assert!(!token.is_expired());
        assert!(!token.signature.is_empty());
    }
}
