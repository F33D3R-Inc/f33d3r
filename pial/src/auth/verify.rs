// F33D3R PIAL — Token Verification
//
// All brains (not just Elohim Veni) use this to verify PIALTokens locally
// without a network call. The Elohim Veni verifying key is fetched from
// the schema registry on startup and cached in memory.

use crate::crypto::keypair::{token_payload, PIALVerifyingKey};
use crate::models::{CapabilityKind, PIALToken};
use anyhow::{anyhow, Result};

/// Verify a PIALToken's signature and expiry.
/// Returns Ok(()) if the token is valid, Err if invalid or expired.
pub fn verify_token(token: &PIALToken, verifying_key: &PIALVerifyingKey) -> Result<()> {
    if token.is_expired() {
        return Err(anyhow!("PIAL token expired at {}", token.expires_at));
    }

    let cap_strs: Vec<String> = token.capabilities.iter().map(|c| c.to_string()).collect();
    let cap_refs: Vec<&str>   = cap_strs.iter().map(|s| s.as_str()).collect();
    let payload = token_payload(
        &token.pial_id.to_string(),
        &cap_refs,
        token.expires_at.timestamp(),
    );

    verifying_key.verify(&payload, &token.signature)
}

/// Verify a token AND check that it includes a specific capability.
pub fn verify_capability(
    token: &PIALToken,
    required: &CapabilityKind,
    verifying_key: &PIALVerifyingKey,
) -> Result<()> {
    verify_token(token, verifying_key)?;
    if token.capabilities.contains(required) {
        Ok(())
    } else {
        Err(anyhow!(
            "PIAL token does not include capability: {}",
            required
        ))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::auth::issue::issue_token;
    use crate::crypto::keypair::PIALSigningKey;
    use uuid::Uuid;

    fn setup() -> (PIALToken, PIALVerifyingKey) {
        let key = PIALSigningKey::generate();
        let vk = key.verifying_key();
        let token = issue_token(
            Uuid::new_v4(),
            vec![CapabilityKind::Posting, CapabilityKind::Messaging],
            &key,
        );
        (token, vk)
    }

    #[test]
    fn valid_token_verifies() {
        let (token, vk) = setup();
        assert!(verify_token(&token, &vk).is_ok());
    }

    #[test]
    fn capability_present_passes() {
        let (token, vk) = setup();
        assert!(verify_capability(&token, &CapabilityKind::Posting, &vk).is_ok());
    }

    #[test]
    fn capability_absent_fails() {
        let (token, vk) = setup();
        assert!(verify_capability(&token, &CapabilityKind::NsfwAccess, &vk).is_err());
    }

    #[test]
    fn wrong_verifying_key_fails() {
        let (token, _) = setup();
        let other_key = PIALSigningKey::generate();
        let other_vk = other_key.verifying_key();
        assert!(verify_token(&token, &other_vk).is_err());
    }
}
