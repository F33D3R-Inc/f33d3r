// F33D3R PIAL — Token Revocation
//
// Short-lived tokens (5 min TTL) minimize revocation complexity.
// The revocation list covers the gap between a ban action and token expiry.
//
// Implementation: in-memory hashset of revoked pial_ids with expiry cleanup.
// In production: backed by Redis with TTL matching token TTL.

use chrono::{DateTime, Utc};
use std::collections::HashMap;
use std::sync::{Arc, RwLock};
use uuid::Uuid;

#[derive(Debug, Clone)]
struct RevocationEntry {
    revoked_at: DateTime<Utc>,
    reason:     String,
}

/// Thread-safe in-memory revocation list.
/// Brains check this after signature verification for immediate bans.
#[derive(Clone, Default)]
pub struct RevocationList {
    entries: Arc<RwLock<HashMap<Uuid, RevocationEntry>>>,
}

impl RevocationList {
    pub fn new() -> Self {
        Self::default()
    }

    /// Revoke a PIAL immediately. All tokens issued before this call become invalid.
    pub fn revoke(&self, pial_id: Uuid, reason: &str) {
        let mut list = self.entries.write().expect("revocation lock poisoned");
        list.insert(
            pial_id,
            RevocationEntry {
                revoked_at: Utc::now(),
                reason: reason.to_string(),
            },
        );
    }

    /// Restore a PIAL (unban). Removes from revocation list.
    pub fn restore(&self, pial_id: &Uuid) {
        let mut list = self.entries.write().expect("revocation lock poisoned");
        list.remove(pial_id);
    }

    /// Check if a PIAL is currently revoked.
    pub fn is_revoked(&self, pial_id: &Uuid) -> bool {
        let list = self.entries.read().expect("revocation lock poisoned");
        list.contains_key(pial_id)
    }

    /// Clean up entries older than the token TTL (they've expired anyway).
    pub fn cleanup_expired(&self, token_ttl_seconds: i64) {
        use chrono::Duration;
        let cutoff = Utc::now() - Duration::seconds(token_ttl_seconds);
        let mut list = self.entries.write().expect("revocation lock poisoned");
        list.retain(|_, entry| entry.revoked_at >= cutoff);
    }

    pub fn count(&self) -> usize {
        self.entries.read().expect("revocation lock poisoned").len()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn revoke_and_check() {
        let list = RevocationList::new();
        let id = Uuid::new_v4();
        assert!(!list.is_revoked(&id));
        list.revoke(id, "ban test");
        assert!(list.is_revoked(&id));
    }

    #[test]
    fn restore_removes_revocation() {
        let list = RevocationList::new();
        let id = Uuid::new_v4();
        list.revoke(id, "test");
        list.restore(&id);
        assert!(!list.is_revoked(&id));
    }

    #[test]
    fn different_pials_are_independent() {
        let list = RevocationList::new();
        let a = Uuid::new_v4();
        let b = Uuid::new_v4();
        list.revoke(a, "ban a");
        assert!(list.is_revoked(&a));
        assert!(!list.is_revoked(&b));
    }
}
