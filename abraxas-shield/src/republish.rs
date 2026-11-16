//! `abraxas-shield republish <kind>:<id> [<kind>:<id> ...]`
//!
//! Re-announces a subject's CURRENT verdict on safety.events as a fresh event.
//!
//! Verdicts are published once, when they change. A consumer that missed one,
//! or that dead-lettered one because of a bug on its side, has no way to get it
//! back by re-reading the topic: the record it kept is the record it could not
//! apply. The authority is the source of truth, so the recovery is the
//! authority stating that truth again, through the same outbox and relay as
//! every other event, with a new seq so per-subject ordering on the consumer
//! side accepts it.
//!
//! Only the verdict is re-announced. Ownership credits and banned matches are
//! decisions with their own events and side effects; replaying those is not
//! this tool's job.
//!
//! Scores are passed through `numeric_scores`: every consumer decodes scores as
//! numbers, and verdict rows recorded before that rule was enforced may carry
//! other values.

use anyhow::{bail, Context};
use serde_json::{Map, Value};
use sqlx::PgPool;
use uuid::Uuid;

use crate::outbox::{self, Event, EventType};

/// Keeps only the numeric entries of a scores object.
fn numeric_scores(scores: &Value) -> Value {
    let mut out = Map::new();
    if let Some(obj) = scores.as_object() {
        for (k, v) in obj {
            if v.is_number() {
                out.insert(k.clone(), v.clone());
            }
        }
    }
    Value::Object(out)
}

pub async fn run(pool: &PgPool, refs: &[String]) -> anyhow::Result<()> {
    if refs.is_empty() {
        bail!("usage: abraxas-shield republish <kind>:<id> [<kind>:<id> ...] (kind: work, vision, asset, stream)");
    }
    let mut republished = 0usize;
    for r in refs {
        let (kind, id) = r
            .split_once(':')
            .with_context(|| format!("{r:?} is not <kind>:<id>"))?;
        if !matches!(kind, "work" | "vision" | "asset" | "stream") {
            bail!("{r:?}: kind must be work, vision, asset or stream");
        }

        let mut tx = pool.begin().await?;
        let latest: Option<(String, Vec<String>, Value, String)> = sqlx::query_as(
            "SELECT verdict, signals, scores, reason FROM verdicts
              WHERE subject_kind = $1 AND subject_id = $2 ORDER BY id DESC LIMIT 1",
        )
        .bind(kind)
        .bind(id)
        .fetch_optional(&mut *tx)
        .await?;
        let Some((verdict, signals, scores, reason)) = latest else {
            bail!("{r}: no verdict recorded for this subject");
        };

        // Who the subject belongs to, where Abraxas knows it.
        let pial: Option<Uuid> = if matches!(kind, "work" | "vision") {
            sqlx::query_scalar("SELECT pial FROM subjects WHERE kind = $1 AND id = $2")
                .bind(kind)
                .bind(id)
                .fetch_optional(&mut *tx)
                .await?
        } else {
            None
        };
        let asset_id = if kind == "asset" {
            Some(Uuid::parse_str(id).with_context(|| format!("{r}: asset id is not a UUID"))?)
        } else {
            None
        };

        outbox::enqueue(
            &mut tx,
            Event {
                kind: EventType::Verdict,
                subject_kind: kind,
                subject_id: id,
                asset_id,
                pial,
                verdict: &verdict,
                scores: numeric_scores(&scores),
                signals: &signals,
                reason: &reason,
                ownership: None,
                banned: None,
                review: None,
            },
        )
        .await?;
        tx.commit().await?;
        println!("republished {r}: {verdict}");
        republished += 1;
    }
    println!("{republished} verdict(s) queued on safety.events; the running server's relay publishes them");
    Ok(())
}
