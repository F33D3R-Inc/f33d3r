//! Verdicts: the current answer for a subject, and its history.
//!
//! Every verdict is written with its safety.events record in one transaction,
//! so nothing is decided that is not announced.

use serde_json::Value;
use sqlx::{PgPool, Postgres, Transaction};
use uuid::Uuid;

use crate::outbox::{self, Event, EventType};

#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord)]
pub enum Verdict {
    // Ordered by severity: a combination takes the max.
    Approve,
    AgeGate,
    Pending,
    HumanReview,
    Block,
}

impl Verdict {
    pub fn as_str(self) -> &'static str {
        match self {
            Verdict::Approve => "approve",
            Verdict::AgeGate => "age_gate",
            Verdict::Pending => "pending",
            Verdict::HumanReview => "human_review",
            Verdict::Block => "block",
        }
    }

    pub fn parse(s: &str) -> Option<Verdict> {
        Some(match s {
            "approve" => Verdict::Approve,
            "age_gate" => Verdict::AgeGate,
            "pending" => Verdict::Pending,
            "human_review" => Verdict::HumanReview,
            "block" => Verdict::Block,
            _ => return None,
        })
    }

    /// True for a verdict nobody has to look at again.
    pub fn is_final(self) -> bool {
        matches!(self, Verdict::Approve | Verdict::AgeGate | Verdict::Block)
    }
}

pub struct NewVerdict<'a> {
    pub subject_kind: &'a str,
    pub subject_id: &'a str,
    pub verdict: Verdict,
    pub signals: &'a [String],
    pub scores: Value,
    pub reason: &'a str,
    pub source: &'a str,
    pub decided_by: &'a str,
    pub asset_id: Option<Uuid>,
    pub pial: Option<Uuid>,
}

/// The newest verdict for a subject.
pub async fn current<'e, E>(exec: E, subject_kind: &str, subject_id: &str) -> sqlx::Result<Option<(Verdict, Vec<String>)>>
where
    E: sqlx::Executor<'e, Database = Postgres>,
{
    let row: Option<(String, Vec<String>)> = sqlx::query_as(
        "SELECT verdict, signals FROM verdicts
          WHERE subject_kind = $1 AND subject_id = $2
          ORDER BY id DESC LIMIT 1",
    )
    .bind(subject_kind)
    .bind(subject_id)
    .fetch_optional(exec)
    .await?;
    Ok(row.and_then(|(v, s)| Verdict::parse(&v).map(|v| (v, s))))
}

pub async fn current_pool(pool: &PgPool, subject_kind: &str, subject_id: &str) -> sqlx::Result<Option<(Verdict, Vec<String>)>> {
    current(pool, subject_kind, subject_id).await
}

/// Writes a verdict and its event. Returns false (and writes nothing) when the
/// subject already holds exactly this verdict with these signals: a rescan
/// that changes nothing is not news.
pub async fn write(tx: &mut Transaction<'_, Postgres>, v: NewVerdict<'_>) -> sqlx::Result<bool> {
    if let Some((cur, sigs)) = current(&mut **tx, v.subject_kind, v.subject_id).await? {
        let mut a = sigs.clone();
        let mut b = v.signals.to_vec();
        a.sort();
        b.sort();
        if cur == v.verdict && a == b {
            return Ok(false);
        }
    }
    sqlx::query(
        "INSERT INTO verdicts (subject_kind, subject_id, verdict, signals, scores, reason, source, decided_by)
         VALUES ($1, $2, $3, $4, $5, $6, $7, $8)",
    )
    .bind(v.subject_kind)
    .bind(v.subject_id)
    .bind(v.verdict.as_str())
    .bind(v.signals)
    .bind(&v.scores)
    .bind(v.reason)
    .bind(v.source)
    .bind(v.decided_by)
    .execute(&mut **tx)
    .await?;
    outbox::enqueue(
        tx,
        Event {
            kind: EventType::Verdict,
            subject_kind: v.subject_kind,
            subject_id: v.subject_id,
            asset_id: v.asset_id,
            pial: v.pial,
            verdict: v.verdict.as_str(),
            scores: v.scores,
            signals: v.signals,
            reason: v.reason,
            ownership: None,
            banned: None,
            review: None,
        },
    )
    .await?;
    metrics::counter!("abraxas_verdicts_total", "verdict" => v.verdict.as_str(), "source" => v.source.to_string()).increment(1);
    Ok(true)
}
