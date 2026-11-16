//! Subject scans: one work or vision, judged on its text, its media and its
//! author's behaviour.
//!
//! The media itself was gated by /v1/check when caeor stored it; a subject
//! scan waits for that verdict (an asset still `pending` holds the subject,
//! never approves it), then asks the worker to analyse each asset once — the
//! analysis is cached per asset — and the text. Behaviour (publishing rate,
//! repeated bodies, new-account bursts) is read from Abraxas's own intake
//! record, never from another brain's database.

use std::collections::HashSet;

use serde_json::{json, Value};
use sqlx::{Postgres, Transaction};
use uuid::Uuid;

use crate::api::AppState;
use crate::jobs::{self, JobError};
use crate::policy::{self, Behaviour, Decision};
use crate::review::{self, NewItem, Stage};
use crate::verdicts::{self, NewVerdict, Verdict};
use crate::worker::{Analysis, WorkerError};

#[derive(sqlx::FromRow)]
struct Subject {
    pial: Uuid,
    body: Option<String>,
    body_sha256: Option<String>,
    is_adult_creator: bool,
    account_created_at: Option<chrono::DateTime<chrono::Utc>>,
    unresolved_media: Vec<String>,
    received_at: chrono::DateTime<chrono::Utc>,
}

pub struct Assessment {
    pub pial: Uuid,
    pub decision: Decision,
    text: Option<Analysis>,
    /// Analyses computed by this assessment, to be cached per asset.
    fresh: Vec<(Uuid, Analysis)>,
    subject_kind: String,
    subject_id: String,
}

fn worker_err(e: WorkerError) -> JobError {
    JobError::Retry(e.to_string())
}

/// Judges a subject. `deep` is the ML review pass.
pub async fn assess(st: &AppState, kind: &str, id: &str, deep: bool) -> Result<Assessment, JobError> {
    let subject: Subject = sqlx::query_as(
        "SELECT pial, body, body_sha256, is_adult_creator, account_created_at, unresolved_media, received_at
           FROM subjects WHERE kind = $1 AND id = $2",
    )
    .bind(kind)
    .bind(id)
    .fetch_optional(&st.pool)
    .await?
    .ok_or_else(|| JobError::Escalate(format!("subject {kind}:{id} was never taken in")))?;

    let assets: Vec<Uuid> = sqlx::query_scalar(
        "SELECT asset_id FROM subject_assets WHERE subject_kind = $1 AND subject_id = $2 ORDER BY asset_id",
    )
    .bind(kind)
    .bind(id)
    .fetch_all(&st.pool)
    .await?;

    let mut asset_verdicts: Vec<(String, Verdict)> = Vec::new();
    let mut media: Vec<(String, Analysis)> = Vec::new();
    let mut fresh: Vec<(Uuid, Analysis)> = Vec::new();
    let mut opaque = 0usize;
    let mut unresolved = subject.unresolved_media.clone();
    for asset in &assets {
        let a = asset.to_string();
        let row: Option<(Option<String>, Option<String>)> =
            sqlx::query_as("SELECT kind, private_path FROM fingerprints WHERE asset_id = $1")
                .bind(asset)
                .fetch_optional(&st.pool)
                .await?;
        let Some((media_kind, private_path)) = row else {
            return Err(JobError::Retry(format!("asset {a} has not been through /v1/check yet")));
        };
        // An imported legacy fingerprint has no kind and no private path; the
        // asset must be backfilled (abraxas-shield backfill) to be analysed.
        let Some(media_kind) = media_kind else {
            unresolved.push(format!("legacy_unchecked:{a}"));
            continue;
        };
        let Some((v, _)) = verdicts::current_pool(&st.pool, "asset", &a).await? else {
            return Err(JobError::Retry(format!("asset {a} has no verdict yet")));
        };
        if v == Verdict::Pending {
            return Err(JobError::Retry(format!("asset {a} is still pending its integrity check")));
        }
        asset_verdicts.push((a.clone(), v));
        if matches!(v, Verdict::Block | Verdict::HumanReview) {
            continue;
        }
        if media_kind == "encrypted" {
            opaque += 1;
            continue;
        }
        let cached: Option<Value> = sqlx::query_scalar(
            "SELECT scores FROM scan_results
              WHERE subject_kind = 'asset' AND subject_id = $1 AND deep = $2
              ORDER BY id DESC LIMIT 1",
        )
        .bind(&a)
        .bind(deep)
        .fetch_optional(&st.pool)
        .await?;
        if let Some(analysis) = cached.and_then(|v| serde_json::from_value::<Analysis>(v).ok()) {
            media.push((a, analysis));
            continue;
        }
        let Some(rel) = private_path else {
            unresolved.push(format!("no_private_source:{a}"));
            continue;
        };
        let abs = crate::media::private_abs(&st.cfg.private_dir, &rel)
            .map_err(|e| JobError::Escalate(format!("asset {a}: recorded private path is invalid: {e}")))?;
        match st.worker.analyze_media(&abs, &media_kind, deep).await {
            Ok(analysis) => {
                fresh.push((*asset, analysis.clone()));
                media.push((a, analysis));
            }
            // The worker cannot read media it fingerprinted before: a person
            // looks, rather than the subject being judged without it.
            Err(WorkerError::Rejected(why)) => unresolved.push(format!("media_unreadable:{a}:{why}")),
            Err(e) => return Err(worker_err(e)),
        }
    }

    let text = match subject.body.as_deref().map(str::trim).filter(|b| !b.is_empty()) {
        Some(body) => Some(st.worker.analyze_text(body, deep).await.map_err(worker_err)?),
        // The body is released once a subject is final; a later rescan
        // (its media's verdict changed) reuses the text analysis on record.
        None => {
            let stored: Option<Value> = sqlx::query_scalar(
                "SELECT scores->'text' FROM scan_results
                  WHERE subject_kind = $1 AND subject_id = $2 AND scores ? 'text'
                  ORDER BY id DESC LIMIT 1",
            )
            .bind(kind)
            .bind(id)
            .fetch_optional(&st.pool)
            .await?;
            stored.filter(|v| !v.is_null()).and_then(|v| serde_json::from_value(v).ok())
        }
    };

    let behaviour = behaviour(st, kind, id, &subject).await?;
    let suppressed = st.suppression.get(&st.pool).await?;
    let decision = policy::decide(&policy::Inputs {
        text: text.as_ref(),
        media: &media,
        opaque_media: opaque,
        asset_verdicts: &asset_verdicts,
        behaviour: &behaviour,
        is_adult_creator: subject.is_adult_creator,
        unresolved_media: &unresolved,
        suppressed: &suppressed,
    });
    Ok(Assessment { pial: subject.pial, decision, text, fresh, subject_kind: kind.into(), subject_id: id.into() })
}

async fn behaviour(st: &AppState, kind: &str, id: &str, s: &Subject) -> sqlx::Result<Behaviour> {
    let (rate_1h, published_72h): (i64, i64) = sqlx::query_as(
        "SELECT COUNT(*) FILTER (WHERE received_at > $2 - INTERVAL '1 hour'),
                COUNT(*) FILTER (WHERE received_at > $2 - INTERVAL '72 hours')
           FROM subjects WHERE pial = $1 AND received_at <= $2",
    )
    .bind(s.pial)
    .bind(s.received_at)
    .fetch_one(&st.pool)
    .await?;
    let duplicate_body = match &s.body_sha256 {
        Some(h) => sqlx::query_scalar(
            "SELECT EXISTS (SELECT 1 FROM subjects
                             WHERE pial = $1 AND body_sha256 = $2 AND NOT (kind = $3 AND id = $4)
                               AND received_at BETWEEN $5 - INTERVAL '24 hours' AND $5)",
        )
        .bind(s.pial)
        .bind(h)
        .bind(kind)
        .bind(id)
        .bind(s.received_at)
        .fetch_one(&st.pool)
        .await?,
        None => false,
    };
    Ok(Behaviour {
        rate_1h,
        duplicate_body,
        account_age_hours: s
            .account_created_at
            .map(|c| (s.received_at - c).num_seconds().max(0) as f64 / 3600.0),
        published_72h,
    })
}

impl Assessment {
    /// Records what was measured: fresh per-asset analyses (the cache) and the
    /// subject's own scan row.
    pub async fn store(&self, tx: &mut Transaction<'_, Postgres>, st: &AppState, deep: bool) -> sqlx::Result<()> {
        for (asset, a) in &self.fresh {
            sqlx::query(
                "INSERT INTO scan_results (subject_kind, subject_id, deep, scan_version, scores, labels, signals, ocr_text, transcript)
                 VALUES ('asset', $1, $2, $3, $4, $5, '{}', $6, $7)",
            )
            .bind(asset.to_string())
            .bind(deep)
            .bind(&st.cfg.scan_version)
            .bind(serde_json::to_value(a).unwrap_or(Value::Null))
            .bind(&a.labels)
            .bind(&a.ocr_text)
            .bind(&a.transcript)
            .execute(&mut **tx)
            .await?;
        }
        sqlx::query(
            "INSERT INTO scan_results (subject_kind, subject_id, deep, scan_version, scores, labels, signals)
             VALUES ($1, $2, $3, $4, $5, $6, $7)",
        )
        .bind(&self.subject_kind)
        .bind(&self.subject_id)
        .bind(deep)
        .bind(&st.cfg.scan_version)
        .bind(json!({
            "decision": self.decision.scores,
            "text": self.text.as_ref().map(|t| serde_json::to_value(t).unwrap_or(Value::Null)),
        }))
        .bind(&self.decision.labels)
        .bind(&self.decision.signals)
        .execute(&mut **tx)
        .await?;
        Ok(())
    }
}

/// The subject_scan job.
pub async fn run_subject_scan(st: &AppState, r#ref: &str) -> Result<(), JobError> {
    let (kind, id) = r#ref
        .split_once(':')
        .ok_or_else(|| JobError::Escalate(format!("subject ref {ref:?} is not <kind>:<id>", ref = r#ref)))?;
    let a = assess(st, kind, id, false).await?;

    let mut tx = st.pool.begin().await?;
    a.store(&mut tx, st, false).await?;

    // A person's (or the deep pass's) ruling stands against a machine's
    // rescan — except a block: media found banned after the ruling still
    // takes the work down.
    let last_source: Option<String> = sqlx::query_scalar(
        "SELECT source FROM verdicts WHERE subject_kind = $1 AND subject_id = $2 ORDER BY id DESC LIMIT 1",
    )
    .bind(kind)
    .bind(id)
    .fetch_optional(&mut *tx)
    .await?;
    let ruled = matches!(last_source.as_deref(), Some("admin") | Some("ml_review"));
    if !ruled || a.decision.verdict == Verdict::Block {
        verdicts::write(&mut tx, NewVerdict {
            subject_kind: kind,
            subject_id: id,
            verdict: a.decision.verdict,
            signals: &a.decision.signals,
            scores: a.decision.scores.clone(),
            reason: &a.decision.reason,
            source: "pipeline",
            decided_by: "abraxas",
            asset_id: None,
            pial: Some(a.pial),
        })
        .await?;
        if a.decision.verdict == Verdict::HumanReview {
            review::open(&mut tx, NewItem {
                item_kind: "content",
                subject_kind: kind,
                subject_id: id,
                ownership_match: None,
                banned_id: None,
                category: None,
                report_id: None,
                dedup_key: format!("content:{kind}:{id}"),
                stage: Stage::MlReview,
                reason: &a.decision.reason,
                evidence: json!({ "signals": a.decision.signals, "scores": a.decision.scores }),
            })
            .await?;
        } else if a.decision.verdict.is_final() {
            release_body(&mut tx, kind, id).await?;
        }
    }
    tx.commit().await?;
    jobs::wake(&st.jobs_wake);
    Ok(())
}

/// Every subject carrying this asset is judged again: its media's verdict
/// changed (a recheck landed, a review was decided, a ban was confirmed).
pub async fn rescan_for_asset(tx: &mut Transaction<'_, Postgres>, asset: Uuid) -> sqlx::Result<()> {
    let refs: Vec<(String, String)> =
        sqlx::query_as("SELECT subject_kind, subject_id FROM subject_assets WHERE asset_id = $1")
            .bind(asset)
            .fetch_all(&mut **tx)
            .await?;
    let mut seen = HashSet::new();
    for (k, i) in refs {
        let r = format!("{k}:{i}");
        if seen.insert(r.clone()) {
            jobs::enqueue(tx, "subject_scan", &r, json!({})).await?;
        }
    }
    Ok(())
}

/// Drops a subject's body text once nobody needs to read it again. The hash
/// stays for duplicate detection; the text analysis stays on its scan row.
pub async fn release_body(tx: &mut Transaction<'_, Postgres>, kind: &str, id: &str) -> sqlx::Result<()> {
    if !matches!(kind, "work" | "vision") {
        return Ok(());
    }
    sqlx::query("UPDATE subjects SET body = NULL WHERE kind = $1 AND id = $2")
        .bind(kind)
        .bind(id)
        .execute(&mut **tx)
        .await?;
    Ok(())
}
