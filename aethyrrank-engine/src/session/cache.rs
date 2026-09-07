//! Session Feature Cache
//!
//! Stores the feature vector used to score each (session, content) pair so
//! that the `/feedback` handler can recover it and perform an accurate LinUCB
//! update.
//!
//! ## Why this exists
//!
//! LinUCB's online update is:
//!
//! ```text
//!     A ← A + x·xᵀ
//!     b ← b + r·x
//! ```
//!
//! Without the *original* feature vector `x` from the scoring call, the
//! update degrades to a structureless reward signal — the model learns
//! "something was good" but not *which feature pattern* drove it.
//!
//! ## Design
//!
//! - In-process: `DashMap<key, entry>` with TTL-based expiry checked on write.
//! - In production: back this with Redis using the same key scheme.
//!   Replace `InProcessCache` with a `RedisCache` that implements the same
//!   `FeatureCache` trait.
//!
//! ## Key scheme
//!
//! ```text
//!     "{session_id}:{content_id}"
//! ```
//!
//! TTL: 3600 seconds (1 hour). Events arriving after TTL are silently dropped.

use dashmap::DashMap;
use std::time::{Duration, Instant};

const TTL_SECS: u64 = 3600;

#[derive(Clone)]
pub struct CacheEntry {
    pub feature_vector: Vec<f64>,
    pub surface: String,
    pub inserted_at: Instant,
}

pub struct SessionFeatureCache {
    inner: DashMap<String, CacheEntry>,
}

impl SessionFeatureCache {
    pub fn new() -> Self {
        Self {
            inner: DashMap::new(),
        }
    }

    /// Store the feature vector used to score `content_id` in `session_id`.
    pub fn put(&self, session_id: &str, content_id: &str, surface: &str, features: Vec<f64>) {
        let key = format!("{}:{}", session_id, content_id);
        self.inner.insert(
            key,
            CacheEntry {
                feature_vector: features,
                surface: surface.to_string(),
                inserted_at: Instant::now(),
            },
        );
        // Opportunistic eviction: remove expired entries on every 100th write
        if self.inner.len() % 100 == 0 {
            self.evict_expired();
        }
    }

    /// Retrieve the feature vector for this (session, content) pair.
    /// Returns `None` if not found or if TTL has expired.
    pub fn get(&self, session_id: &str, content_id: &str) -> Option<CacheEntry> {
        let key = format!("{}:{}", session_id, content_id);
        let entry = self.inner.get(&key)?;
        if entry.inserted_at.elapsed() > Duration::from_secs(TTL_SECS) {
            drop(entry);
            self.inner.remove(&key);
            return None;
        }
        Some(entry.clone())
    }

    fn evict_expired(&self) {
        let deadline = Duration::from_secs(TTL_SECS);
        self.inner.retain(|_, v| v.inserted_at.elapsed() < deadline);
    }

    pub fn len(&self) -> usize {
        self.inner.len()
    }
}

impl Default for SessionFeatureCache {
    fn default() -> Self {
        Self::new()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn put_and_get_roundtrip() {
        let cache = SessionFeatureCache::new();
        cache.put("sess1", "item1", "feed", vec![0.1, 0.2, 0.3]);
        let entry = cache.get("sess1", "item1").expect("should be present");
        assert_eq!(entry.feature_vector, vec![0.1, 0.2, 0.3]);
        assert_eq!(entry.surface, "feed");
    }

    #[test]
    fn missing_key_returns_none() {
        let cache = SessionFeatureCache::new();
        assert!(cache.get("no_session", "no_item").is_none());
    }

    #[test]
    fn different_sessions_are_independent() {
        let cache = SessionFeatureCache::new();
        cache.put("sess_a", "item1", "feed", vec![1.0]);
        cache.put("sess_b", "item1", "feed", vec![2.0]);
        let a = cache.get("sess_a", "item1").unwrap();
        let b = cache.get("sess_b", "item1").unwrap();
        assert_ne!(a.feature_vector, b.feature_vector);
    }
}
