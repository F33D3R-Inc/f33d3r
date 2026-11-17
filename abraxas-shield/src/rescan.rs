//! `abraxas-shield rescan <kind>:<id> [<kind>:<id> ...]`
//!
//! Queues a fresh subject scan, so a work or Vision is judged again under the
//! policy as it stands now. Used when the policy changes and subjects judged
//! under the old one must be brought in line.
//!
//! The scan goes through the ordinary subject_scan job, so the verdict it
//! reaches is recorded and published like any other: if it differs from the
//! subject's last verdict, a new verdict event goes out on safety.events. A
//! consumer applies it only where the subject's current state was itself set
//! by automation; a person's decision is never overwritten by a re-scan.

use anyhow::{bail, Context};
use serde_json::json;
use sqlx::PgPool;

use crate::jobs;

pub async fn run(pool: &PgPool, refs: &[String]) -> anyhow::Result<()> {
    if refs.is_empty() {
        bail!("usage: abraxas-shield rescan <kind>:<id> [<kind>:<id> ...] (kind: work or vision)");
    }
    let mut queued = 0usize;
    for r in refs {
        let (kind, id) = r
            .split_once(':')
            .with_context(|| format!("{r:?} is not <kind>:<id>"))?;
        if !matches!(kind, "work" | "vision") {
            bail!("{r:?}: kind must be work or vision");
        }
        let known: bool = sqlx::query_scalar("SELECT EXISTS (SELECT 1 FROM subjects WHERE kind = $1 AND id = $2)")
            .bind(kind)
            .bind(id)
            .fetch_one(pool)
            .await?;
        if !known {
            bail!("{r}: Abraxas has never taken this subject in, so there is nothing to scan again");
        }
        let mut tx = pool.begin().await?;
        jobs::enqueue(&mut tx, "subject_scan", r, json!({})).await?;
        tx.commit().await?;
        println!("queued a fresh scan of {r}");
        queued += 1;
    }
    println!("{queued} scan(s) queued; the running server's job loop takes them");
    Ok(())
}
