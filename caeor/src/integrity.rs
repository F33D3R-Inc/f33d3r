//! The integrity gate: Abraxas decides whether an uploaded asset may exist on
//! the platform, and caeor carries the answer out.
//!
//! Every upload is inserted `held` with verdict 'pending' and put to Abraxas
//! (POST /v1/check) before anything of it can be served:
//!
//!   allow   → the asset leaves `held`; an ownership finding is recorded and
//!             exposed as the AssetView's credit_to
//!   review  → the asset stays `held`, servable to nobody, and is asked about
//!             again until a reviewer's decision turns the answer into allow
//!             or block
//!   block   → the asset is withdrawn with reason banned:<category>; its row,
//!             its private source and its fingerprint are kept as the record
//!   no answer (Abraxas unreachable, refusing, or saying something this build
//!             cannot read) → `held` with verdict 'unreachable', asked again
//!             with backoff. An unanswered check is never an allow.
//!
//! The release mechanism is a pull, owned here: the Reconciler re-asks Abraxas
//! about every held asset whose next check is due. POST /v1/check is
//! idempotent per asset_id and answers the asset's current standing, so a
//! review decided in Abraxas becomes allow or block on the next ask. A pull
//! cannot lose a callback; `POST /v1/assets/{id}/recheck` only brings a held
//! asset's next ask forward (feed-engine calls it when it sees review_decided
//! on safety.events), it never decides anything itself.

use std::sync::Arc;
use std::time::Duration;

use anyhow::{anyhow, bail, Context, Result};
use serde::{Deserialize, Serialize};
use sqlx::PgPool;
use tokio::sync::Notify;
use uuid::Uuid;

use crate::layout::Layout;
use crate::withdraw;

/// The prefix of every withdrawal the integrity authority caused.
pub const BANNED_PREFIX: &str = "banned:";

/// How long an inserted upload is left to the request that is asking about
/// it before the reconciler may ask too. Longer than CHECK_TIMEOUT plus an
/// image render, so the two never ask at once.
pub const INSERT_GRACE: Duration = Duration::from_secs(10 * 60);
/// How long one check may take. A long video is fingerprinted inside it.
const CHECK_TIMEOUT: Duration = Duration::from_secs(120);
/// How soon an asset under review is asked about again.
const REVIEW_RECHECK: Duration = Duration::from_secs(60);
/// Backoff ceiling for an asset Abraxas could not answer for.
const UNREACHABLE_MAX_BACKOFF_SECS: f64 = 300.0;
const TICK: Duration = Duration::from_secs(5);
const BATCH: i64 = 32;
/// Session advisory lock: one integrity reconciler per database.
const RECONCILER_LOCK: i64 = 0x_6361_656f_7269_6e74; // "caeorint"

// ── Wire: caeor → Abraxas ────────────────────────────────────────────────────

#[derive(Debug, Serialize)]
struct CheckRequest<'a> {
    asset_id: Uuid,
    kind: &'a str,
    uploader_pial: Uuid,
    sha256: &'a str,
    /// Relative to the private store, which Abraxas mounts read-only.
    private_path: &'a str,
    #[serde(skip_serializing_if = "Option::is_none")]
    duration: Option<f64>,
}

#[derive(Debug, Deserialize)]
struct CheckResponse {
    verdict: String,
    #[serde(default)]
    banned_match: Option<BannedMatch>,
    #[serde(default)]
    ownership: Option<Ownership>,
    fingerprint_id: String,
}

#[derive(Debug, Clone, Deserialize)]
pub struct BannedMatch {
    pub category: String,
    pub layer: String,
}

#[derive(Debug, Clone, Deserialize)]
pub struct Ownership {
    #[serde(default)]
    pub original_asset: Option<Uuid>,
    pub original_pial: Uuid,
    /// exact | derivative | excerpt
    pub kind: String,
    pub confidence: f64,
    #[serde(default)]
    pub offset_seconds: Option<f64>,
}

/// One answer from Abraxas, validated.
#[derive(Debug, Clone)]
pub enum Verdict {
    Allow { ownership: Option<Ownership>, fingerprint_id: String },
    Review { ownership: Option<Ownership>, fingerprint_id: String },
    Block { banned: BannedMatch, ownership: Option<Ownership>, fingerprint_id: String },
}

impl Verdict {
    fn word(&self) -> &'static str {
        match self {
            Verdict::Allow { .. } => "allow",
            Verdict::Review { .. } => "review",
            Verdict::Block { .. } => "block",
        }
    }
    fn parts(&self) -> (&Option<Ownership>, &str) {
        match self {
            Verdict::Allow { ownership, fingerprint_id }
            | Verdict::Review { ownership, fingerprint_id }
            | Verdict::Block { ownership, fingerprint_id, .. } => (ownership, fingerprint_id),
        }
    }
}

fn validate(r: CheckResponse) -> Result<Verdict> {
    let fingerprint_id = r.fingerprint_id.trim().to_string();
    if fingerprint_id.is_empty() || fingerprint_id.len() > 200 {
        bail!("Abraxas answered without a usable fingerprint_id");
    }
    if let Some(o) = &r.ownership {
        if !matches!(o.kind.as_str(), "exact" | "derivative" | "excerpt") {
            bail!("Abraxas answered an unknown ownership kind {:?}", o.kind);
        }
        if o.original_pial.is_nil() {
            bail!("Abraxas answered an ownership finding with no original owner");
        }
        if !o.confidence.is_finite() {
            bail!("Abraxas answered a non-finite ownership confidence");
        }
    }
    Ok(match r.verdict.as_str() {
        "allow" => Verdict::Allow { ownership: r.ownership, fingerprint_id },
        "review" => Verdict::Review { ownership: r.ownership, fingerprint_id },
        "block" => {
            let banned = r
                .banned_match
                .ok_or_else(|| anyhow!("Abraxas answered block without a banned_match"))?;
            let category = banned.category.trim();
            if category.is_empty()
                || category.len() > 64
                || !category.chars().all(|c| c.is_ascii_alphanumeric() || c == '_' || c == '-')
            {
                bail!("Abraxas answered block with an unusable category {:?}", banned.category);
            }
            Verdict::Block {
                banned: BannedMatch { category: category.to_string(), layer: banned.layer },
                ownership: r.ownership,
                fingerprint_id,
            }
        }
        other => bail!("Abraxas answered an unknown verdict {other:?}"),
    })
}

/// Caeor's client for Abraxas.
#[derive(Clone)]
pub struct Abraxas {
    http: reqwest::Client,
    base: String,
    key: String,
}

impl Abraxas {
    pub fn new(base: &str, key: &str) -> Result<Self> {
        let http = reqwest::Client::builder()
            .connect_timeout(Duration::from_secs(5))
            .timeout(CHECK_TIMEOUT)
            .build()
            .context("building the Abraxas client")?;
        Ok(Self { http, base: base.to_string(), key: key.to_string() })
    }

    /// Asks Abraxas about one asset. Any failure to get a readable answer is
    /// an error, which the caller records as 'unreachable' and holds on.
    async fn check(&self, req: &CheckRequest<'_>) -> Result<Verdict> {
        let resp = self
            .http
            .post(format!("{}/v1/check", self.base))
            .header("X-Internal-Key", &self.key)
            .json(req)
            .send()
            .await
            .context("sending the check to Abraxas")?;
        let status = resp.status();
        if !status.is_success() {
            let body = resp.text().await.unwrap_or_else(|e| format!("<unreadable body: {e}>"));
            bail!("Abraxas answered {status}: {}", body.chars().take(300).collect::<String>());
        }
        let r: CheckResponse = resp.json().await.context("decoding Abraxas's answer")?;
        validate(r)
    }
}

// ── Carrying out an answer ───────────────────────────────────────────────────

/// What became of an asset once an answer (or its absence) was carried out.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Gate {
    /// The asset left `held`.
    Released,
    /// The asset stays `held`: under review, or Abraxas did not answer.
    Held,
    /// The asset was withdrawn as banned.
    Banned,
}

/// Where an allowed asset goes from `held`.
#[derive(Debug, Clone, Copy)]
pub enum AllowTo {
    /// `processing`: the request that is asking is about to make the asset's
    /// derivatives itself (an image), and marks it ready when they exist.
    Processing,
    /// Whatever its work says: `processing` while an encode is open, `ready`
    /// when something servable exists, `failed` when nothing was ever made.
    Release,
}

/// The facts one check needs about an asset.
#[derive(sqlx::FromRow)]
struct Subject {
    kind: String,
    uploader_pial: Uuid,
    raw_sha256: Option<String>,
    duration_seconds: Option<f64>,
    private_path: Option<String>,
}

async fn load_subject(pool: &PgPool, id: Uuid) -> Result<Subject> {
    sqlx::query_as::<_, Subject>(
        "SELECT a.kind, a.uploader_pial, a.raw_sha256, a.duration_seconds,
                (SELECT d.path FROM derivatives d
                  WHERE d.asset_id = a.id AND d.role = 'source_private' AND d.variant = '') AS private_path
           FROM assets a WHERE a.id = $1",
    )
    .bind(id)
    .fetch_optional(pool)
    .await?
    .ok_or_else(|| anyhow!("asset {id} does not exist"))
}

/// Asks Abraxas about one held asset and carries the answer out.
/// `not_before` keeps a held outcome from being re-asked before then (the
/// image path renders while held, and is asked about again once it has).
pub async fn gate(
    pool: &PgPool,
    layout: &Layout,
    abraxas: &Abraxas,
    id: Uuid,
    allow_to: AllowTo,
    not_before: Option<Duration>,
) -> Result<Gate> {
    let s = load_subject(pool, id).await?;
    let outcome = match (&s.raw_sha256, &s.private_path) {
        (Some(sha), Some(path)) => {
            abraxas
                .check(&CheckRequest {
                    asset_id: id,
                    kind: &s.kind,
                    uploader_pial: s.uploader_pial,
                    sha256: sha,
                    private_path: path,
                    duration: s.duration_seconds,
                })
                .await
        }
        // An upload always has both; one without them cannot be judged, and
        // what cannot be judged is held, loudly.
        _ => Err(anyhow!("asset {id} has no recorded sha256 or private source to check")),
    };
    settle(pool, layout, id, outcome, allow_to, not_before).await
}

/// Records the ownership finding and fingerprint an answer carried. A finding
/// replaces an earlier one; no finding leaves an earlier one standing.
async fn record_facts(
    tx: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    id: Uuid,
    ownership: &Option<Ownership>,
    fingerprint_id: &str,
) -> Result<()> {
    sqlx::query("UPDATE assets SET fingerprint_id = $2, integrity_checked_at = NOW(), updated_at = NOW() WHERE id = $1")
        .bind(id)
        .bind(fingerprint_id)
        .execute(&mut **tx)
        .await
        .context("recording the fingerprint")?;
    if let Some(o) = ownership {
        sqlx::query(
            "UPDATE assets SET original_asset = $2, original_pial = $3, ownership_kind = $4,
                    ownership_confidence = $5, ownership_offset_seconds = $6, updated_at = NOW()
              WHERE id = $1",
        )
        .bind(id)
        .bind(o.original_asset)
        .bind(o.original_pial)
        .bind(&o.kind)
        .bind(o.confidence)
        .bind(o.offset_seconds)
        .execute(&mut **tx)
        .await
        .context("recording the ownership finding")?;
    }
    Ok(())
}

/// The SQL that takes a held asset out of `held` under AllowTo::Release.
const RELEASE_SQL: &str = "
    WITH next AS (
        SELECT a.id,
               CASE
                 WHEN EXISTS (SELECT 1 FROM upload_jobs j
                               WHERE j.asset_id = a.id AND j.state IN ('queued','dispatched','cancelling'))
                   THEN 'processing'
                 WHEN EXISTS (SELECT 1 FROM derivatives d
                               WHERE d.asset_id = a.id AND d.store = 'served' AND d.removed_at IS NULL)
                   THEN 'ready'
                 ELSE 'failed'
               END AS status
          FROM assets a WHERE a.id = $1 AND a.status = 'held'
    )
    UPDATE assets SET status = next.status,
           failure = CASE WHEN next.status = 'failed'
                          THEN 'released by the integrity check with nothing made to serve'
                          ELSE assets.failure END,
           integrity_verdict = 'allow', integrity_next_check_at = NULL, integrity_error = NULL,
           updated_at = NOW()
      FROM next WHERE assets.id = next.id
    RETURNING assets.status";

/// Carries out one answer, or the absence of one.
pub async fn settle(
    pool: &PgPool,
    layout: &Layout,
    id: Uuid,
    outcome: Result<Verdict>,
    allow_to: AllowTo,
    not_before: Option<Duration>,
) -> Result<Gate> {
    let floor = not_before.map(|d| d.as_secs_f64()).unwrap_or(0.0);
    let verdict = match outcome {
        Ok(v) => v,
        Err(e) => {
            let msg = format!("{e:#}");
            tracing::warn!(asset_id = %id, error = %msg, "Abraxas did not answer; the asset is held");
            sqlx::query(
                "UPDATE assets SET integrity_verdict = 'unreachable', integrity_error = $2,
                        integrity_attempts = integrity_attempts + 1,
                        integrity_next_check_at = NOW() + GREATEST(
                            LEAST(POWER(2, LEAST(integrity_attempts, 12)), $3), $4) * INTERVAL '1 second',
                        updated_at = NOW()
                  WHERE id = $1 AND status = 'held'",
            )
            .bind(id)
            .bind(msg.chars().take(1000).collect::<String>())
            .bind(UNREACHABLE_MAX_BACKOFF_SECS)
            .bind(floor)
            .execute(pool)
            .await
            .context("recording an unanswered check")?;
            return Ok(Gate::Held);
        }
    };

    if let Verdict::Block { banned, ownership, fingerprint_id } = &verdict {
        let reason = format!("{BANNED_PREFIX}{}", banned.category);
        // The withdrawal comes first: the instant it commits, nothing of the
        // asset can be served or downloaded, and a queued encode is cancelled.
        match withdraw::withdraw(pool, layout, id, &reason).await? {
            withdraw::Outcome::NotFound => bail!("asset {id} vanished before it could be withdrawn"),
            withdraw::Outcome::Done(failures) => {
                if !failures.is_empty() {
                    // Withdrawn and served nowhere; some bytes are still on
                    // disk. POST /v1/assets/{id}/withdraw finishes it.
                    tracing::error!(asset_id = %id, failures = ?failures,
                        "a banned asset is withdrawn but some of its bytes could not be removed; withdraw it again");
                }
            }
        }
        let mut tx = pool.begin().await?;
        record_facts(&mut tx, id, ownership, fingerprint_id).await?;
        sqlx::query(
            "UPDATE assets SET integrity_verdict = 'block', integrity_next_check_at = NULL,
                    integrity_error = NULL, updated_at = NOW() WHERE id = $1",
        )
        .bind(id)
        .execute(&mut *tx)
        .await
        .context("recording the block")?;
        tx.commit().await?;
        tracing::warn!(asset_id = %id, category = %banned.category, layer = %banned.layer,
            "asset refused by the integrity authority and withdrawn");
        return Ok(Gate::Banned);
    }

    let (ownership, fingerprint_id) = verdict.parts();
    let mut tx = pool.begin().await?;
    record_facts(&mut tx, id, ownership, fingerprint_id).await?;
    let gate = match &verdict {
        Verdict::Review { .. } => {
            sqlx::query(
                "UPDATE assets SET integrity_verdict = 'review', integrity_error = NULL,
                        integrity_next_check_at = NOW() + GREATEST($2, $3) * INTERVAL '1 second',
                        updated_at = NOW()
                  WHERE id = $1 AND status = 'held'",
            )
            .bind(id)
            .bind(REVIEW_RECHECK.as_secs_f64())
            .bind(floor)
            .execute(&mut *tx)
            .await
            .context("recording a review")?;
            Gate::Held
        }
        Verdict::Allow { .. } => {
            let moved = match allow_to {
                AllowTo::Processing => sqlx::query_scalar::<_, String>(
                    "UPDATE assets SET status = 'processing', integrity_verdict = 'allow',
                            integrity_next_check_at = NULL, integrity_error = NULL, updated_at = NOW()
                      WHERE id = $1 AND status = 'held' RETURNING status",
                )
                .bind(id)
                .fetch_optional(&mut *tx)
                .await
                .context("releasing an asset to processing")?,
                AllowTo::Release => sqlx::query_scalar::<_, String>(RELEASE_SQL)
                    .bind(id)
                    .fetch_optional(&mut *tx)
                    .await
                    .context("releasing a held asset")?,
            };
            if moved.is_none() {
                // Not held any more (failed while it was asked about): the
                // answer is recorded and the status is left as it is.
                sqlx::query("UPDATE assets SET integrity_verdict = 'allow', updated_at = NOW() WHERE id = $1 AND status <> 'held'")
                    .bind(id)
                    .execute(&mut *tx)
                    .await
                    .context("recording an allow")?;
            }
            Gate::Released
        }
        Verdict::Block { .. } => unreachable!("handled above"),
    };
    tx.commit().await?;
    tracing::info!(asset_id = %id, verdict = verdict.word(), "integrity answer carried out");
    Ok(gate)
}

// ── The reconciler ───────────────────────────────────────────────────────────

/// Re-asks Abraxas about every held asset whose next check is due.
#[derive(Clone)]
pub struct Reconciler {
    pub pool: PgPool,
    pub layout: Layout,
    pub abraxas: Abraxas,
    pub wake: Arc<Notify>,
}

impl Reconciler {
    pub fn spawn(self) {
        tokio::spawn(async move {
            loop {
                if let Err(e) = self.tick().await {
                    tracing::error!(error = %format!("{e:#}"), "integrity reconciler tick failed");
                }
                tokio::select! {
                    _ = tokio::time::sleep(TICK) => {}
                    _ = self.wake.notified() => {}
                }
            }
        });
    }

    async fn tick(&self) -> Result<()> {
        let mut lock_conn = self.pool.acquire().await.context("integrity reconciler connection")?;
        let locked: bool = sqlx::query_scalar("SELECT pg_try_advisory_lock($1)")
            .bind(RECONCILER_LOCK)
            .fetch_one(&mut *lock_conn)
            .await
            .context("taking the integrity reconciler lock")?;
        if !locked {
            return Ok(());
        }
        let outcome = self.tick_locked().await;
        let released: bool = sqlx::query_scalar("SELECT pg_advisory_unlock($1)")
            .bind(RECONCILER_LOCK)
            .fetch_one(&mut *lock_conn)
            .await
            .context("releasing the integrity reconciler lock")?;
        if !released {
            tracing::warn!("integrity reconciler lock was not held at release");
        }
        outcome
    }

    async fn tick_locked(&self) -> Result<()> {
        let due: Vec<Uuid> = sqlx::query_scalar(
            "SELECT id FROM assets
              WHERE status = 'held' AND integrity_next_check_at <= NOW()
              ORDER BY integrity_next_check_at, created_at
              LIMIT $1",
        )
        .bind(BATCH)
        .fetch_all(&self.pool)
        .await
        .context("reading held assets")?;
        for id in due {
            match gate(&self.pool, &self.layout, &self.abraxas, id, AllowTo::Release, None).await {
                Ok(Gate::Released) => {
                    tracing::info!(asset_id = %id, "held asset released by the integrity authority");
                }
                Ok(Gate::Banned) | Ok(Gate::Held) => {}
                Err(e) => {
                    // The answer could not be carried out (the database
                    // refused): the asset is still held and is asked about
                    // again at its next check.
                    tracing::error!(asset_id = %id, error = %format!("{e:#}"),
                        "carrying out an integrity answer failed; the asset stays held");
                    sqlx::query(
                        "UPDATE assets SET integrity_error = $2,
                                integrity_next_check_at = NOW() + INTERVAL '30 seconds', updated_at = NOW()
                          WHERE id = $1 AND status = 'held'",
                    )
                    .bind(id)
                    .bind(format!("{e:#}").chars().take(1000).collect::<String>())
                    .execute(&self.pool)
                    .await
                    .context("recording a failed integrity step")?;
                }
            }
        }
        Ok(())
    }
}

/// Brings a held asset's next check forward. False when the asset is not held.
pub async fn recheck_now(pool: &PgPool, id: Uuid) -> Result<bool> {
    let done = sqlx::query(
        "UPDATE assets SET integrity_next_check_at = NOW(), updated_at = NOW()
          WHERE id = $1 AND status = 'held' AND integrity_verdict <> 'pending'",
    )
    .bind(id)
    .execute(pool)
    .await
    .context("bringing a check forward")?;
    Ok(done.rows_affected() == 1)
}
