//! Hot transient state (§5).
//!
//! ```text
//! freq:{id}:presence:{pial}   STRING  role, TTL = presence_ttl
//! freq:{id}:listeners         SET     PIALs whose presence key is (was) alive
//! freq:{id}:speakers          SET     same, for speaking roles
//! freq:{id}:host_seen         STRING  unix seconds the host last touched
//! freq:{id}:lease             STRING  node_id, PX = lease_ttl
//! freq:notify:{pial}:{type}   STRING  Herald cooldown, EX = notify_cooldown
//! freq:rl:{scope}:{key}       STRING  fixed-window counter
//! freq:nonce:{nonce}          STRING  a signaling token that has been used
//! ```
//!
//! The sets are a fast index over presence keys, not truth: the sweeper
//! (`tasks::presence_sweep`) removes members whose presence key has expired
//! and marks them left in PostgreSQL. Redis runs with `allkeys-lru`, so any
//! of these can be evicted under memory pressure; every reader tolerates a
//! missing key as "not present / not held" and every writer re-creates.

use std::time::Duration;

use redis::aio::ConnectionManager;
use redis::{AsyncCommands, Script};

use crate::domain::FrequencyRole;
use crate::error::Result;
use crate::telemetry::metrics::Timed;
use uuid::Uuid;

#[derive(Clone)]
pub struct Hot {
    conn: ConnectionManager,
    node_id: String,
    presence_ttl: Duration,
    lease_ttl: Duration,
}

// Compare-and-expire / compare-and-delete, so a node can only renew or release
// a lease it holds. Two nodes racing for one Frequency can each SET NX, but
// only the winner's later renewals succeed.
const RENEW_IF_HELD: &str = r#"
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('PEXPIRE', KEYS[1], ARGV[2])
else
  return 0
end"#;

const RELEASE_IF_HELD: &str = r#"
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
else
  return 0
end"#;

// INCR then EXPIRE only on the first hit, atomically, so a window cannot be
// left without an expiry by a crash between the two.
const INCR_WINDOW: &str = r#"
local n = redis.call('INCR', KEYS[1])
if n == 1 then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
end
return n"#;

pub fn key_presence(id: Uuid, pial: &str) -> String {
    format!("freq:{id}:presence:{pial}")
}
pub fn key_listeners(id: Uuid) -> String {
    format!("freq:{id}:listeners")
}
pub fn key_speakers(id: Uuid) -> String {
    format!("freq:{id}:speakers")
}
pub fn key_host_seen(id: Uuid) -> String {
    format!("freq:{id}:host_seen")
}
pub fn key_lease(id: Uuid) -> String {
    format!("freq:{id}:lease")
}
/// Present while the sweeper has run recently. Its absence means Redis was
/// cold (restart, flush, eviction storm) and every presence key with it.
pub fn key_sweeper_armed() -> String {
    "freq:sweeper:armed".to_string()
}
pub fn key_notify(pial: &str, kind: &str) -> String {
    format!("freq:notify:{pial}:{kind}")
}
pub fn key_rate(scope: &str, key: &str) -> String {
    format!("freq:rl:{scope}:{key}")
}
/// Signaling nonces (Phase 3).
#[allow(dead_code)]
pub fn key_nonce(nonce: &str) -> String {
    format!("freq:nonce:{nonce}")
}

pub struct Members {
    pub listeners: Vec<String>,
    pub speakers: Vec<String>,
}

impl Hot {
    pub async fn connect(
        url: &str,
        node_id: String,
        presence_ttl: Duration,
        lease_ttl: Duration,
    ) -> Result<Self> {
        let client = redis::Client::open(url)?;
        let conn = ConnectionManager::new(client).await?;
        Ok(Self {
            conn,
            node_id,
            presence_ttl,
            lease_ttl,
        })
    }

    pub async fn ping(&self) -> Result<()> {
        let _t = Timed::redis("ping");
        let mut c = self.conn.clone();
        let _: String = redis::cmd("PING").query_async(&mut c).await?;
        Ok(())
    }

    // ── presence ─────────────────────────────────────────────────────────────

    /// Mark someone present in a role. Idempotent; called on join and on every
    /// heartbeat. Moves them between the two sets if their role changed.
    pub async fn touch(&self, id: Uuid, pial: &str, role: FrequencyRole) -> Result<()> {
        let _t = Timed::redis("touch");
        let mut c = self.conn.clone();
        let ttl_ms = self.presence_ttl.as_millis() as u64;
        let (add, remove) = if role.speaks() {
            (key_speakers(id), key_listeners(id))
        } else {
            (key_listeners(id), key_speakers(id))
        };
        let mut pipe = redis::pipe();
        pipe.atomic()
            .pset_ex(key_presence(id, pial), role.as_str(), ttl_ms)
            .ignore()
            .sadd(&add, pial)
            .ignore()
            .srem(&remove, pial)
            .ignore();
        if role == FrequencyRole::Host {
            pipe.set(key_host_seen(id), chrono::Utc::now().timestamp())
                .ignore();
        }
        pipe.query_async::<()>(&mut c).await?;
        Ok(())
    }

    /// Forget someone: presence key and both sets.
    pub async fn forget(&self, id: Uuid, pial: &str) -> Result<()> {
        let _t = Timed::redis("forget");
        let mut c = self.conn.clone();
        redis::pipe()
            .atomic()
            .del(key_presence(id, pial))
            .ignore()
            .srem(key_listeners(id), pial)
            .ignore()
            .srem(key_speakers(id), pial)
            .ignore()
            .query_async::<()>(&mut c)
            .await?;
        Ok(())
    }

    pub async fn present(&self, id: Uuid, pial: &str) -> Result<bool> {
        let _t = Timed::redis("present");
        let mut c = self.conn.clone();
        Ok(c.exists(key_presence(id, pial)).await?)
    }

    /// (listeners, speakers) currently in the sets.
    pub async fn counts(&self, id: Uuid) -> Result<(i64, i64)> {
        let _t = Timed::redis("counts");
        let mut c = self.conn.clone();
        let (l, s): (i64, i64) = redis::pipe()
            .scard(key_listeners(id))
            .scard(key_speakers(id))
            .query_async(&mut c)
            .await?;
        Ok((l, s))
    }

    pub async fn members(&self, id: Uuid) -> Result<Members> {
        let _t = Timed::redis("members");
        let mut c = self.conn.clone();
        let (listeners, speakers): (Vec<String>, Vec<String>) = redis::pipe()
            .smembers(key_listeners(id))
            .smembers(key_speakers(id))
            .query_async(&mut c)
            .await?;
        Ok(Members {
            listeners,
            speakers,
        })
    }

    /// Which of `pials` have a live presence key. One round trip.
    pub async fn alive(&self, id: Uuid, pials: &[String]) -> Result<Vec<bool>> {
        if pials.is_empty() {
            return Ok(Vec::new());
        }
        let _t = Timed::redis("alive");
        let mut c = self.conn.clone();
        let mut pipe = redis::pipe();
        for p in pials {
            pipe.exists(key_presence(id, p));
        }
        Ok(pipe.query_async(&mut c).await?)
    }

    /// Re-arm the sweeper and report whether it was armed before. `false`
    /// means Redis lost its keys since the last sweep: nobody's absence can
    /// be trusted this round, so the caller departs no one and lets one
    /// presence window pass (§26). The key outlives a presence TTL by one
    /// sweep so a healthy Redis never reads as cold.
    pub async fn arm_sweeper(&self, sweep_interval: Duration) -> Result<bool> {
        let _t = Timed::redis("arm_sweeper");
        let mut c = self.conn.clone();
        let ttl = self.presence_ttl + sweep_interval * 2;
        let previous: Option<String> = redis::cmd("SET")
            .arg(key_sweeper_armed())
            .arg(1)
            .arg("EX")
            .arg(ttl.as_secs().max(1))
            .arg("GET")
            .query_async(&mut c)
            .await?;
        Ok(previous.is_some())
    }

    /// Start the host-absence clock now. Used when there is no record of when
    /// the host was last seen, so a Frequency cannot be ended on a clock that
    /// never started.
    pub async fn mark_host_seen(&self, id: Uuid) -> Result<()> {
        let _t = Timed::redis("mark_host_seen");
        let mut c = self.conn.clone();
        let _: () = c
            .set(key_host_seen(id), chrono::Utc::now().timestamp())
            .await?;
        Ok(())
    }

    /// Unix seconds the host last touched, if known.
    pub async fn host_seen(&self, id: Uuid) -> Result<Option<i64>> {
        let _t = Timed::redis("host_seen");
        let mut c = self.conn.clone();
        Ok(c.get(key_host_seen(id)).await?)
    }

    /// Drop every key for a Frequency that has ended. Presence keys are found
    /// through the sets; anything the sets have lost expires on its own.
    pub async fn clear(&self, id: Uuid) -> Result<()> {
        let _t = Timed::redis("clear");
        let m = self.members(id).await?;
        let mut c = self.conn.clone();
        let mut pipe = redis::pipe();
        pipe.atomic();
        for p in m.listeners.iter().chain(m.speakers.iter()) {
            pipe.del(key_presence(id, p)).ignore();
        }
        pipe.del(key_listeners(id))
            .ignore()
            .del(key_speakers(id))
            .ignore()
            .del(key_host_seen(id))
            .ignore();
        pipe.query_async::<()>(&mut c).await?;
        Ok(())
    }

    // ── leases (§29) ─────────────────────────────────────────────────────────

    /// Try to become the media owner of a Frequency. True if this node now
    /// holds the lease (fresh or already ours).
    pub async fn acquire_lease(&self, id: Uuid) -> Result<bool> {
        let _t = Timed::redis("acquire_lease");
        let mut c = self.conn.clone();
        let ttl_ms = self.lease_ttl.as_millis() as u64;
        let set: Option<String> = redis::cmd("SET")
            .arg(key_lease(id))
            .arg(&self.node_id)
            .arg("NX")
            .arg("PX")
            .arg(ttl_ms)
            .query_async(&mut c)
            .await?;
        if set.is_some() {
            return Ok(true);
        }
        self.renew_lease(id).await
    }

    /// Extend a lease we hold. False means somebody else holds it, or it was
    /// evicted and re-acquired elsewhere; the caller stops serving media.
    pub async fn renew_lease(&self, id: Uuid) -> Result<bool> {
        let _t = Timed::redis("renew_lease");
        let mut c = self.conn.clone();
        let ttl_ms = self.lease_ttl.as_millis() as u64;
        let n: i64 = Script::new(RENEW_IF_HELD)
            .key(key_lease(id))
            .arg(&self.node_id)
            .arg(ttl_ms)
            .invoke_async(&mut c)
            .await?;
        Ok(n == 1)
    }

    pub async fn release_lease(&self, id: Uuid) -> Result<bool> {
        let _t = Timed::redis("release_lease");
        let mut c = self.conn.clone();
        let n: i64 = Script::new(RELEASE_IF_HELD)
            .key(key_lease(id))
            .arg(&self.node_id)
            .invoke_async(&mut c)
            .await?;
        Ok(n == 1)
    }

    pub async fn lease_holder(&self, id: Uuid) -> Result<Option<String>> {
        let _t = Timed::redis("lease_holder");
        let mut c = self.conn.clone();
        Ok(c.get(key_lease(id)).await?)
    }

    // ── limits and cooldowns ─────────────────────────────────────────────────

    /// Increment a fixed window and return the count after increment.
    pub async fn hit(&self, scope: &str, key: &str, window: Duration) -> Result<i64> {
        let _t = Timed::redis("rate_hit");
        let mut c = self.conn.clone();
        let n: i64 = Script::new(INCR_WINDOW)
            .key(key_rate(scope, key))
            .arg(window.as_millis() as u64)
            .invoke_async(&mut c)
            .await?;
        Ok(n)
    }

    /// True if a notification of this kind may go to this person now; sets the
    /// cooldown as a side effect so the next one within `ttl` is refused.
    pub async fn notify_allowed(&self, pial: &str, kind: &str, ttl: Duration) -> Result<bool> {
        let _t = Timed::redis("notify_allowed");
        let mut c = self.conn.clone();
        let set: Option<String> = redis::cmd("SET")
            .arg(key_notify(pial, kind))
            .arg(1)
            .arg("NX")
            .arg("EX")
            .arg(ttl.as_secs().max(1))
            .query_async(&mut c)
            .await?;
        Ok(set.is_some())
    }

    /// Burn a signaling nonce. True the first time, false ever after. Called
    /// by the signaling socket (Phase 3).
    #[allow(dead_code)]
    pub async fn burn_nonce(&self, nonce: &str, ttl: Duration) -> Result<bool> {
        let _t = Timed::redis("burn_nonce");
        let mut c = self.conn.clone();
        let set: Option<String> = redis::cmd("SET")
            .arg(key_nonce(nonce))
            .arg(1)
            .arg("NX")
            .arg("EX")
            .arg(ttl.as_secs().max(1))
            .query_async(&mut c)
            .await?;
        Ok(set.is_some())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn keys_are_namespaced_and_stable() {
        let id = Uuid::nil();
        assert_eq!(
            key_presence(id, "pial:x"),
            "freq:00000000-0000-0000-0000-000000000000:presence:pial:x"
        );
        assert_eq!(
            key_lease(id),
            "freq:00000000-0000-0000-0000-000000000000:lease"
        );
        assert_eq!(key_rate("join", "pial:x"), "freq:rl:join:pial:x");
        assert_eq!(
            key_notify("pial:x", "frequency_invite"),
            "freq:notify:pial:x:frequency_invite"
        );
    }
}
