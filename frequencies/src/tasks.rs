//! Background loops: boot reconciliation, presence sweep and host grace,
//! lease renewal, idempotency pruning, gauges.
//!
//! Each loop is its own task; each tick is wrapped so a failure is logged
//! and the loop continues. None of them assumes it is the only node.

use std::time::Duration;

use metrics::{counter, gauge};
use tracing::{error, info, warn};

use crate::api::ops::{self, Cause};
use crate::domain::permissions::{self, HostLossOutcome, Reconcile};
use crate::domain::{FrequencyRole, FrequencyState};
use crate::error::Result;
use crate::repository::postgres as pg;
use crate::state::AppState;
use crate::telemetry::metrics as m;

/// §28: after a restart, inspect every open Frequency and put it in a state
/// this process can serve. Runs before the listener binds.
pub async fn reconcile_on_boot(s: &AppState) -> Result<()> {
    let open = pg::list_open(&s.pool).await?;
    let now = chrono::Utc::now();
    let cause = Cause::service("boot-reconcile");
    let mut failed = 0;
    let mut completed = 0;
    let mut adopted = 0;

    for f in open {
        let age = (now - f.updated_at).to_std().unwrap_or_default();
        match permissions::reconcile_on_boot(f.state, age, s.config.starting_timeout) {
            Reconcile::FailStarting => {
                ops::fail_starting(s, &f, &cause, "starting_timeout").await?;
                failed += 1;
            }
            Reconcile::CompleteEnding => {
                ops::complete_ending(
                    s,
                    &f,
                    &cause,
                    f.end_reason.as_deref().unwrap_or("host_ended"),
                )
                .await?;
                completed += 1;
            }
            Reconcile::Leave => {
                if f.state == FrequencyState::Live {
                    // Reclaim media ownership where nobody holds it. With one
                    // node this is always us; with several, the holder keeps
                    // it and this node serves control only.
                    if s.hot.acquire_lease(f.id).await? {
                        adopted += 1;
                        if f.media_node.as_deref() != Some(s.config.node_id.as_str()) {
                            let mut conn = s.pool.acquire().await?;
                            let p = pg::Patch {
                                media_node: Some(Some(s.config.node_id.clone())),
                                ..Default::default()
                            };
                            pg::cas_update(&mut conn, f.id, f.version, &p).await?;
                        }
                    }
                }
            }
        }
    }
    info!(failed, completed, adopted, "boot reconciliation done");
    Ok(())
}

pub fn spawn_all(s: AppState) {
    tokio::spawn(presence_loop(s.clone()));
    tokio::spawn(lease_loop(s.clone()));
    tokio::spawn(prune_loop(s));
}

async fn presence_loop(s: AppState) {
    let mut tick = tokio::time::interval(s.config.presence_sweep_interval);
    loop {
        tick.tick().await;
        if let Err(e) = presence_sweep(&s).await {
            error!(error = %e, "presence sweep failed");
        }
    }
}

/// One sweep (§26, §27). For every live Frequency: anyone whose presence key
/// has expired is departed; a host who has been gone past the grace period
/// hands over to a present co-host or ends the session.
pub async fn presence_sweep(s: &AppState) -> Result<()> {
    let live = pg::list_live(&s.pool, 10_000).await?;
    let cause = Cause::service("presence-sweep");
    let (mut listeners_total, mut speakers_total) = (0i64, 0i64);

    // A cold Redis has no presence keys for anyone, present or not. Departing
    // on that evidence would empty every live Frequency and end the older
    // ones. So this round only re-arms; the next one, a presence window
    // later, sees who actually came back.
    let armed = s.hot.arm_sweeper(s.config.presence_sweep_interval).await?;
    if !armed {
        warn!(
            live = live.len(),
            "presence sweep: redis was cold; departing nobody this round"
        );
        for f in &live {
            s.hot.mark_host_seen(f.id).await?;
        }
        counter!(m::PRESENCE_SWEPT_TOTAL, "outcome" => "cold_skip").increment(1);
        return Ok(());
    }

    for f in &live {
        let mut conn = s.pool.acquire().await?;
        let joined = pg::joined(&mut conn, f.id).await?;
        let names: Vec<String> = joined.iter().map(|p| p.pial.clone()).collect();
        let alive = s.hot.alive(f.id, &names).await?;

        let mut cohost_present = false;
        let mut host_present = false;
        for (p, present) in joined.iter().zip(alive.iter().copied()) {
            if present {
                match p.role {
                    FrequencyRole::Host => host_present = true,
                    FrequencyRole::CoHost => cohost_present = true,
                    _ => {}
                }
                if p.role.speaks() {
                    speakers_total += 1
                } else {
                    listeners_total += 1
                }
                continue;
            }
            if p.role == FrequencyRole::Host {
                // The host is handled by the grace rule below, not swept.
                continue;
            }
            let mut tx = s.pool.begin().await?;
            ops::depart(s, &mut tx, f, &p.pial, &cause, "presence_expired").await?;
            tx.commit().await?;
            counter!(m::PRESENCE_SWEPT_TOTAL, "outcome" => "departed").increment(1);
        }

        if !host_present {
            let seen = s.hot.host_seen(f.id).await?;
            let absent_for = match seen {
                Some(ts) => {
                    Duration::from_secs((chrono::Utc::now().timestamp() - ts).max(0) as u64)
                }
                // No record: the clock starts now, never from a guess. The
                // host gets a full grace period from this moment.
                None => {
                    s.hot.mark_host_seen(f.id).await?;
                    Duration::ZERO
                }
            };
            match permissions::on_host_absent(absent_for, s.config.host_grace, cohost_present) {
                HostLossOutcome::KeepWaiting => {}
                HostLossOutcome::ContinueUnderCoHost => {
                    counter!(m::HOST_LOSS_TOTAL, "outcome" => "cohost_continues").increment(1);
                }
                HostLossOutcome::EndFrequency => {
                    warn!(frequency_id = %f.id, absent_secs = absent_for.as_secs(), "host lost; ending frequency");
                    counter!(m::HOST_LOSS_TOTAL, "outcome" => "ended").increment(1);
                    ops::end(s, f.id, &cause, "host_lost", FrequencyState::Ended).await?;
                }
            }
        }
    }

    gauge!(m::FREQUENCIES_ACTIVE).set(live.len() as f64);
    gauge!(m::LISTENERS_CURRENT).set(listeners_total as f64);
    gauge!(m::SPEAKERS_CURRENT).set(speakers_total as f64);
    gauge!(m::PARTICIPANTS_CURRENT).set((listeners_total + speakers_total) as f64);
    Ok(())
}

async fn lease_loop(s: AppState) {
    let mut tick = tokio::time::interval(s.config.lease_ttl / 3);
    loop {
        tick.tick().await;
        if let Err(e) = renew_leases(&s).await {
            error!(error = %e, "lease renewal failed");
        }
    }
}

/// §29: renew every lease this node holds. A renewal that fails means the
/// lease was evicted or taken; the Frequency is still live and its control
/// plane is still served, but this node no longer owns its media.
pub async fn renew_leases(s: &AppState) -> Result<()> {
    let live = pg::list_live(&s.pool, 10_000).await?;
    let mut held = 0;
    for f in live
        .iter()
        .filter(|f| f.media_node.as_deref() == Some(s.config.node_id.as_str()))
    {
        if s.hot.renew_lease(f.id).await? {
            held += 1;
        } else if s.hot.acquire_lease(f.id).await? {
            // Evicted (allkeys-lru) and nobody else took it: ours again.
            held += 1;
        } else {
            counter!(m::LEASE_LOST_TOTAL).increment(1);
            warn!(frequency_id = %f.id, "lease held elsewhere; this node no longer owns its media");
        }
    }
    gauge!(m::LEASE_HELD).set(held as f64);
    Ok(())
}

async fn prune_loop(s: AppState) {
    let mut tick = tokio::time::interval(Duration::from_secs(3600));
    loop {
        tick.tick().await;
        match pg::idempotent_prune(&s.pool, 24).await {
            Ok(n) if n > 0 => info!(pruned = n, "idempotency keys pruned"),
            Ok(_) => {}
            Err(e) => error!(error = %e, "idempotency prune failed"),
        }
    }
}
