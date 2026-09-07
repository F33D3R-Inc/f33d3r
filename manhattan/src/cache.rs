//! The resolution cache: what makes `/v1/resolve` an in-memory answer.
//!
//! Every brain resolves names for every page it renders, so resolution is the
//! one read that scales with the whole estate's traffic rather than with any
//! one brain's. This cache answers it from memory and keeps the plane's oldest
//! rule while doing so: a revoked name NEVER resolves.
//!
//! # How it stays right
//!
//! Not by expiry. Migration 0009 makes Postgres announce every change that can
//! alter a resolution — a binding created, retired or re-pointed, a node's
//! status changed — on one NOTIFY channel, delivered at the instant the change
//! commits. `listen` below receives those and drops the named entry from this
//! replica's cache, so a rotated address stops resolving here at the same
//! moment a fresh read would stop seeing it. There is still a ceiling on how
//! long an entry may live, but it is a safety net for a listener that missed
//! something, not the mechanism.
//!
//! Two failure modes are handled rather than hoped away:
//!
//!   * The listener's connection drops. Notifications sent while it was down
//!     are gone. So whenever the listener reconnects, the whole cache is
//!     cleared, and while it is down the cache is bypassed entirely — every
//!     resolve goes to the database until the plane can hear again. A cache
//!     that cannot hear must not answer.
//!
//!   * A fill races an eviction. A request reads the old row, the revocation
//!     commits, the eviction arrives and finds nothing to evict, and THEN the
//!     request's stale answer is written into the cache. So every fill carries
//!     the moment its database read began, every eviction stamps the key with
//!     the moment it arrived, and a fill whose read began before the key's last
//!     eviction is refused. The stale answer is served once, to the request
//!     that read it, and never cached.
//!
//! # Shape
//!
//! Sharded by key hash so a hot name and a cold name never contend on one
//! lock; bounded per shard, with a shard that overflows emptied rather than
//! managed — the miss burst that follows is one shard's worth, and a cache
//! with no per-entry bookkeeping is a cache with nothing to get wrong.
//! Misses are cached too, briefly: an unbound name is the answer a scanner
//! asks for most, and it must cost the database nothing.

use std::collections::HashMap;
use std::hash::{Hash, Hasher};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Mutex;
use std::time::{Duration, Instant};

use sqlx::postgres::PgListener;
use sqlx::PgPool;

use crate::models::ResolveResp;

/// The channel migration 0009 announces on.
pub const CHANNEL: &str = "manhattan_resolution";

/// How long a hit may be served after it was filled, if no announcement has
/// touched it. A safety net, sized so that a listener that silently missed an
/// announcement bounds the damage to a minute.
const POSITIVE_TTL: Duration = Duration::from_secs(60);
/// A miss is cached for less: a name about to be bound is announced when it
/// is, so this only bounds the window if that announcement is lost.
const NEGATIVE_TTL: Duration = Duration::from_secs(5);

const SHARDS: usize = 256;

struct Entry {
    value: Option<ResolveResp>,
    filled_at: Instant,
}

#[derive(Default)]
struct Shard {
    entries: HashMap<String, Entry>,
    /// When each key was last announced. Consulted by `fill` to refuse a read
    /// that began before the announcement. Emptied whenever the shard is.
    evicted_at: HashMap<String, Instant>,
}

pub struct ResolveCache {
    shards: Vec<Mutex<Shard>>,
    per_shard: usize,
    /// False while the listener is not connected. A cache that cannot hear
    /// announcements answers nothing.
    hearing: AtomicBool,
}

/// What the cache knows about a name.
pub enum Lookup {
    Hit(Option<ResolveResp>),
    Miss,
}

impl ResolveCache {
    /// A cache holding at most `entries` resolutions across all shards. Zero
    /// disables it: every lookup misses and every fill is dropped.
    pub fn new(entries: usize) -> Self {
        ResolveCache {
            shards: (0..SHARDS).map(|_| Mutex::new(Shard::default())).collect(),
            per_shard: entries / SHARDS,
            hearing: AtomicBool::new(false),
        }
    }

    pub fn enabled(&self) -> bool {
        self.per_shard > 0
    }

    fn key(name: &str) -> String {
        name.trim().to_lowercase()
    }

    fn shard(&self, key: &str) -> &Mutex<Shard> {
        let mut h = std::collections::hash_map::DefaultHasher::new();
        key.hash(&mut h);
        &self.shards[(h.finish() as usize) % SHARDS]
    }

    pub fn get(&self, name: &str) -> Lookup {
        if !self.enabled() || !self.hearing.load(Ordering::Acquire) {
            return Lookup::Miss;
        }
        let key = Self::key(name);
        let shard = self.shard(&key).lock().unwrap_or_else(|e| e.into_inner());
        match shard.entries.get(&key) {
            Some(e) => {
                let ttl = if e.value.is_some() { POSITIVE_TTL } else { NEGATIVE_TTL };
                if e.filled_at.elapsed() < ttl {
                    metrics::counter!("manhattan_resolve_cache_total", "outcome" => "hit").increment(1);
                    Lookup::Hit(e.value.clone())
                } else {
                    metrics::counter!("manhattan_resolve_cache_total", "outcome" => "expired").increment(1);
                    Lookup::Miss
                }
            }
            None => {
                metrics::counter!("manhattan_resolve_cache_total", "outcome" => "miss").increment(1);
                Lookup::Miss
            }
        }
    }

    /// Records what a database read that began at `read_started` found.
    /// Refused when the key was announced after the read began — the read
    /// may predate the change the announcement carried.
    pub fn fill(&self, name: &str, read_started: Instant, value: Option<ResolveResp>) {
        if !self.enabled() || !self.hearing.load(Ordering::Acquire) {
            return;
        }
        let key = Self::key(name);
        let mut shard = self.shard(&key).lock().unwrap_or_else(|e| e.into_inner());
        if let Some(at) = shard.evicted_at.get(&key) {
            if *at >= read_started {
                metrics::counter!("manhattan_resolve_cache_total", "outcome" => "fill_refused").increment(1);
                return;
            }
        }
        if shard.entries.len() >= self.per_shard {
            shard.entries.clear();
            shard.evicted_at.clear();
            metrics::counter!("manhattan_resolve_cache_total", "outcome" => "shard_flushed").increment(1);
        }
        shard.entries.insert(
            key,
            Entry {
                value,
                filled_at: Instant::now(),
            },
        );
    }

    /// Drops a name, and remembers when, so a fill from an older read cannot
    /// put it back.
    pub fn evict(&self, name: &str) {
        let key = Self::key(name);
        let mut shard = self.shard(&key).lock().unwrap_or_else(|e| e.into_inner());
        shard.entries.remove(&key);
        if shard.evicted_at.len() >= self.per_shard.max(1) {
            // Bounded like the entries are. Losing an old eviction stamp only
            // matters for a read that began before it, which by now has long
            // since filled or been refused.
            shard.evicted_at.clear();
        }
        shard.evicted_at.insert(key, Instant::now());
        metrics::counter!("manhattan_resolve_cache_total", "outcome" => "evicted").increment(1);
    }

    pub fn clear(&self) {
        for shard in &self.shards {
            let mut s = shard.lock().unwrap_or_else(|e| e.into_inner());
            s.entries.clear();
            s.evicted_at.clear();
        }
    }

    /// How many resolutions are held right now, across all shards.
    pub fn len(&self) -> usize {
        self.shards
            .iter()
            .map(|s| s.lock().unwrap_or_else(|e| e.into_inner()).entries.len())
            .sum()
    }

    pub fn hearing(&self) -> bool {
        self.hearing.load(Ordering::Acquire)
    }

    fn set_hearing(&self, on: bool) {
        self.hearing.store(on, Ordering::Release);
        metrics::gauge!("manhattan_resolve_cache_hearing").set(if on { 1.0 } else { 0.0 });
    }
}

/// Runs for the life of the process: listens on the announcement channel and
/// keeps the cache honest. Returns only if the cache is disabled.
///
/// The listener reconnects on its own when its connection drops; every time it
/// does, the cache is cleared, because whatever was announced in between was
/// not heard. Between the drop and the reconnect the cache is bypassed.
pub async fn listen(pool: PgPool, cache: std::sync::Arc<ResolveCache>) {
    if !cache.enabled() {
        tracing::info!("resolution cache disabled (RESOLVE_CACHE_ENTRIES=0)");
        return;
    }
    let mut backoff = Duration::from_millis(200);
    loop {
        let mut listener = match PgListener::connect_with(&pool).await {
            Ok(l) => l,
            Err(e) => {
                tracing::error!(error = %e, "resolution listener cannot connect; cache bypassed");
                tokio::time::sleep(backoff).await;
                backoff = (backoff * 2).min(Duration::from_secs(10));
                continue;
            }
        };
        if let Err(e) = listener.listen(CHANNEL).await {
            tracing::error!(error = %e, "resolution listener cannot LISTEN; cache bypassed");
            tokio::time::sleep(backoff).await;
            backoff = (backoff * 2).min(Duration::from_secs(10));
            continue;
        }
        backoff = Duration::from_millis(200);
        // Anything cached before this point was cached while deaf.
        cache.clear();
        cache.set_hearing(true);
        tracing::info!(channel = CHANNEL, "resolution cache hearing announcements");

        loop {
            match listener.try_recv().await {
                Ok(Some(n)) => cache.evict(n.payload()),
                // The connection was lost and re-established underneath us:
                // announcements in between are gone, so is everything cached.
                Ok(None) => {
                    cache.clear();
                    tracing::warn!("resolution listener reconnected; cache cleared");
                }
                Err(e) => {
                    cache.set_hearing(false);
                    cache.clear();
                    tracing::error!(error = %e, "resolution listener lost; cache bypassed until it reconnects");
                    break;
                }
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn resp(name: &str) -> ResolveResp {
        ResolveResp {
            name: name.into(),
            node_id: uuid::Uuid::new_v4(),
            kind: "identity".into(),
            owner: "elohim-veni".into(),
            status: "active".into(),
        }
    }

    fn hearing_cache() -> ResolveCache {
        let c = ResolveCache::new(SHARDS * 4);
        c.set_hearing(true);
        c
    }

    #[test]
    fn a_cache_that_cannot_hear_answers_nothing_and_stores_nothing() {
        let c = ResolveCache::new(SHARDS * 4);
        c.fill("pial:a", Instant::now(), Some(resp("pial:a")));
        assert!(matches!(c.get("pial:a"), Lookup::Miss));
        assert_eq!(c.len(), 0);
        c.set_hearing(true);
        c.fill("pial:a", Instant::now(), Some(resp("pial:a")));
        assert!(matches!(c.get("PIAL:A"), Lookup::Hit(Some(_))), "case-folded like the names are");
    }

    #[test]
    fn a_fill_from_a_read_older_than_the_last_announcement_is_refused() {
        let c = hearing_cache();
        let read_started = Instant::now();
        std::thread::sleep(Duration::from_millis(2));
        c.evict("handle:bob");
        c.fill("handle:bob", read_started, Some(resp("handle:bob")));
        assert!(matches!(c.get("handle:bob"), Lookup::Miss), "the stale read was cached");
        std::thread::sleep(Duration::from_millis(2));
        c.fill("handle:bob", Instant::now(), None);
        assert!(matches!(c.get("handle:bob"), Lookup::Hit(None)), "a fresh read fills");
    }

    #[test]
    fn an_overflowing_shard_is_emptied_not_grown() {
        let c = ResolveCache::new(SHARDS); // one entry per shard
        c.set_hearing(true);
        for i in 0..SHARDS * 8 {
            c.fill(&format!("uuid:{i}"), Instant::now(), None);
        }
        assert!(c.len() <= SHARDS, "held {} entries with room for {SHARDS}", c.len());
    }

    #[test]
    fn zero_entries_disables_it() {
        let c = ResolveCache::new(0);
        c.set_hearing(true);
        c.fill("x", Instant::now(), None);
        assert!(!c.enabled());
        assert!(matches!(c.get("x"), Lookup::Miss));
    }
}
