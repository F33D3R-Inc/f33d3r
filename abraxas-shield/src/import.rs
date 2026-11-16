//! `abraxas-shield import` — the one-shot move of every integrity record the
//! platform already holds into f33d3r_integrity.
//!
//! Sources (read only):
//!   FEED_DATABASE_URL    f33d3r_feed: banned_content_hashes, media_fingerprints,
//!                        content_scan_results, content_moderation_log,
//!                        csam_scan_log, scan_feedback; users / identity_facts
//!                        to turn handles into PIALs.
//!   SAFETY_DATABASE_URL  f33d3r_safety (zodacare): content_reports.
//!
//! Idempotent: every imported row carries an import_ref naming its source row
//! (or keeps its source id), and is inserted only when absent; a row that
//! cannot be carried over is recorded in import_rejects with the reason. A
//! second run changes nothing; an interrupted run is finished by running it
//! again; a reject fixed at the source is imported by the next run and its
//! reject record cleared.
//!
//! It ends by VERIFYING: for every source table, imported + rejected must equal
//! the source count. Any shortfall exits non-zero with the table that is short.
//!
//! Nothing imported is announced on safety.events: feed-engine already applied
//! every state these rows describe. Works feed-engine still holds at
//! human_review become admin_review items, so nothing waits unreviewed.

use std::collections::{HashMap, HashSet};

use anyhow::{bail, Context, Result};
use chrono::{DateTime, Utc};
use serde_json::json;
use sqlx::PgPool;
use uuid::Uuid;

use crate::hashing::{parse_phash_hex, phash_bands};

const PAGE: i64 = 2000;

async fn reject(pool: &PgPool, source: &str, id: &str, reason: &str) -> Result<()> {
    sqlx::query(
        "INSERT INTO import_rejects (source, source_id, reason) VALUES ($1, $2, $3)
         ON CONFLICT (source, source_id) DO UPDATE SET reason = EXCLUDED.reason, recorded_at = NOW()",
    )
    .bind(source)
    .bind(id)
    .bind(reason)
    .execute(pool)
    .await?;
    Ok(())
}

async fn accepted(pool: &PgPool, source: &str, id: &str) -> Result<()> {
    sqlx::query("DELETE FROM import_rejects WHERE source = $1 AND source_id = $2")
        .bind(source)
        .bind(id)
        .execute(pool)
        .await?;
    Ok(())
}

fn category_ok(c: &str) -> bool {
    c.len() >= 2 && c.len() <= 32 && c.starts_with(|x: char| x.is_ascii_lowercase()) && c.chars().all(|x| x.is_ascii_lowercase() || x == '_')
}

fn is_vision_ref(id: &str) -> bool {
    id.rsplit_once('.')
        .map(|(p, n)| Uuid::parse_str(p).is_ok() && n.parse::<i64>().map(|n| n > 0).unwrap_or(false))
        .unwrap_or(false)
}

/// Handle → PIAL through feed-engine's account projection, confirmed against
/// identity_facts (the identities feed-engine knows exist).
struct Identities {
    by_handle: HashMap<String, Uuid>,
    known: HashSet<Uuid>,
}

impl Identities {
    async fn load(feed: &PgPool) -> Result<Self> {
        let rows: Vec<(String, Uuid)> =
            sqlx::query_as("SELECT lower(handle), pial_id FROM users WHERE pial_id IS NOT NULL AND handle <> ''")
                .fetch_all(feed)
                .await
                .context("reading users")?;
        let known: Vec<Uuid> = sqlx::query_scalar("SELECT pial_id FROM identity_facts")
            .fetch_all(feed)
            .await
            .context("reading identity_facts")?;
        Ok(Self { by_handle: rows.into_iter().collect(), known: known.into_iter().collect() })
    }

    /// A PIAL from a stored PIAL text, falling back to the handle.
    fn resolve(&self, pial_text: &str, handle: &str) -> Option<Uuid> {
        let direct = Uuid::parse_str(pial_text.trim()).ok().filter(|p| self.known.contains(p));
        direct.or_else(|| {
            self.by_handle
                .get(&handle.trim().trim_start_matches('@').to_ascii_lowercase())
                .copied()
                .filter(|p| self.known.contains(p))
        })
    }
}

pub async fn run(pool: &PgPool) -> Result<()> {
    let feed = crate::db::connect_existing(&crate::config::import_source("FEED_DATABASE_URL")?, "feed").await?;
    let safety = crate::db::connect_existing(&crate::config::import_source("SAFETY_DATABASE_URL")?, "safety").await?;
    let ids = Identities::load(&feed).await?;
    tracing::info!(handles = ids.by_handle.len(), identities = ids.known.len(), "[import] identity map loaded");

    banned(pool, &feed).await?;
    fingerprints(pool, &feed, &ids).await?;
    scans(pool, &feed).await?;
    moderation_history(pool, &feed).await?;
    evidence(pool, &feed, &ids).await?;
    feedback(pool, &feed).await?;
    reports(pool, &safety).await?;
    open_human_review(pool, &feed).await?;

    verify(pool, &feed, &safety).await
}

// ── banned_content_hashes → banned_hashes ────────────────────────────────────

async fn banned(pool: &PgPool, feed: &PgPool) -> Result<()> {
    const SRC: &str = "feed.banned_content_hashes";
    let rows: Vec<(Uuid, String, String, String, String, DateTime<Utc>, String)> = sqlx::query_as(
        "SELECT id, hash_type, hash_value, category, added_by, added_at, note FROM banned_content_hashes ORDER BY added_at, id",
    )
    .fetch_all(feed)
    .await
    .context("reading banned_content_hashes")?;
    for (id, t, v, category, by, at, note) in rows {
        let sid = id.to_string();
        let r#ref = format!("{SRC}:{id}");
        if !category_ok(&category) {
            reject(pool, SRC, &sid, &format!("category {category:?} is not a lowercase word")).await?;
            continue;
        }
        let (layer, sha, ph, ch) = match t.as_str() {
            "sha256" if crate::media::is_sha256_hex(&v.trim().to_ascii_lowercase()) => ("sha256", Some(v.trim().to_ascii_lowercase()), None, None),
            "phash" => match parse_phash_hex(&v) {
                Some(h) => ("phash", None, Some(h), None),
                None => {
                    reject(pool, SRC, &sid, "phash is not 16 hex digits").await?;
                    continue;
                }
            },
            // Carried exactly as stored; a value that will not decode is
            // reported as an unenforced ban by the registry on every refresh.
            "audio_fp" => ("chromaprint", None, None, Some(v.trim().to_string())),
            _ => {
                reject(pool, SRC, &sid, &format!("hash_type {t:?} with value {v:?} is not importable")).await?;
                continue;
            }
        };
        let n = sqlx::query(
            "INSERT INTO banned_hashes (layer, sha256, phash, chromaprint, category, added_by, note, origin, import_ref, added_at)
             VALUES ($1, $2, $3, $4, $5, $6, $7, 'import', $8, $9) ON CONFLICT DO NOTHING",
        )
        .bind(layer)
        .bind(&sha)
        .bind(ph)
        .bind(&ch)
        .bind(&category)
        .bind(&by)
        .bind(&note)
        .bind(&r#ref)
        .bind(at)
        .execute(pool)
        .await?
        .rows_affected();
        let present: bool = sqlx::query_scalar("SELECT EXISTS (SELECT 1 FROM banned_hashes WHERE import_ref = $1)")
            .bind(&r#ref)
            .fetch_one(pool)
            .await?;
        if n == 1 || present {
            accepted(pool, SRC, &sid).await?;
        } else {
            // The same value is already banned (added before the import, or
            // listed twice in the source): the ban exists, recorded once.
            reject(pool, SRC, &sid, "already_present: the same value is already an active ban").await?;
        }
    }
    Ok(())
}

// ── media_fingerprints → fingerprints ────────────────────────────────────────

async fn fingerprints(pool: &PgPool, feed: &PgPool, ids: &Identities) -> Result<()> {
    const SRC: &str = "feed.media_fingerprints";
    let mut after: Option<(DateTime<Utc>, Uuid)> = None;
    let (mut unattributed, mut total) = (0usize, 0usize);
    loop {
        #[allow(clippy::type_complexity)]
        let rows: Vec<(Uuid, String, String, String, String, String, String, String, DateTime<Utc>)> = sqlx::query_as(
            "SELECT id, asset_id, post_id, uploader_pial, uploader_handle, sha256, audio_fingerprint, perceptual_hash, scanned_at
               FROM media_fingerprints
              WHERE ($1::timestamptz IS NULL OR (scanned_at, id) > ($1, $2))
              ORDER BY scanned_at, id LIMIT $3",
        )
        .bind(after.map(|a| a.0))
        .bind(after.map(|a| a.1))
        .bind(PAGE)
        .fetch_all(feed)
        .await
        .context("reading media_fingerprints")?;
        if rows.is_empty() {
            break;
        }
        after = rows.last().map(|r| (r.8, r.0));
        for (id, asset, post, pial_text, handle, sha, audio, phash, at) in rows {
            total += 1;
            let sid = id.to_string();
            let r#ref = format!("{SRC}:{id}");
            let sha = Some(sha.trim().to_ascii_lowercase()).filter(|s| crate::media::is_sha256_hex(s));
            let ph = if phash.trim().is_empty() { None } else { parse_phash_hex(&phash) };
            if !phash.trim().is_empty() && ph.is_none() {
                reject(pool, SRC, &sid, "perceptual_hash is not 16 hex digits").await?;
                continue;
            }
            let chroma = Some(audio.trim().to_string()).filter(|s| !s.is_empty());
            if sha.is_none() && ph.is_none() && chroma.is_none() {
                reject(pool, SRC, &sid, "no usable sha256, phash or audio fingerprint").await?;
                continue;
            }
            let pial = ids.resolve(&pial_text, &handle);
            if pial.is_none() {
                unattributed += 1;
            }
            let asset_uuid = Uuid::parse_str(asset.trim()).ok();
            let mut tx = pool.begin().await?;
            let taken: bool = match asset_uuid {
                Some(a) => sqlx::query_scalar("SELECT EXISTS (SELECT 1 FROM fingerprints WHERE asset_id = $1 AND import_ref IS DISTINCT FROM $2)")
                    .bind(a)
                    .bind(&r#ref)
                    .fetch_one(&mut *tx)
                    .await?,
                None => false,
            };
            let fp: Option<Uuid> = sqlx::query_scalar(
                "INSERT INTO fingerprints
                   (asset_id, uploader_pial, kind, origin, sha256, phash, chromaprint, ownership_eligible,
                    import_ref, legacy_asset_ref, legacy_post_id, created_at)
                 VALUES ($1, $2, NULL, 'import', $3, $4, $5, TRUE, $6, $7, $8, $9)
                 ON CONFLICT (import_ref) DO NOTHING RETURNING id",
            )
            .bind(if taken { None } else { asset_uuid })
            .bind(pial)
            .bind(&sha)
            .bind(ph)
            .bind(&chroma)
            .bind(&r#ref)
            .bind(Some(asset.clone()).filter(|a| !a.is_empty()))
            .bind(Some(post.clone()).filter(|p| !p.is_empty()))
            .bind(at)
            .fetch_optional(&mut *tx)
            .await?;
            // The image phash enters the visual index at t = 0 so legacy
            // images are originals new uploads are credited against.
            if let (Some(fp), Some(h)) = (fp, ph) {
                let seg: i64 = sqlx::query_scalar(
                    "INSERT INTO video_segments (fingerprint_id, asset_id, t, phash) VALUES ($1, $2, 0, $3) RETURNING id",
                )
                .bind(fp)
                .bind(if taken { None } else { asset_uuid })
                .bind(h)
                .fetch_one(&mut *tx)
                .await?;
                for b in phash_bands(h) {
                    sqlx::query("INSERT INTO segment_bands (band, segment_id) VALUES ($1, $2)")
                        .bind(b)
                        .bind(seg)
                        .execute(&mut *tx)
                        .await?;
                }
            }
            tx.commit().await?;
            accepted(pool, SRC, &sid).await?;
        }
    }
    tracing::info!(total, unattributed, "[import] media_fingerprints carried over (unattributed: no PIAL resolvable from uploader_pial or handle)");
    Ok(())
}

// ── content_scan_results → scan_results + verdicts ───────────────────────────

fn map_recommendation(r: &str) -> Option<&'static str> {
    Some(match r {
        "approve" => "approve",
        "age_gate" => "age_gate",
        "human_review" => "human_review",
        "auto_block" | "block" => "block",
        "pending" | "pending_scan" => "pending",
        _ => return None,
    })
}

async fn scans(pool: &PgPool, feed: &PgPool) -> Result<()> {
    const SRC: &str = "feed.content_scan_results";
    #[allow(clippy::type_complexity)]
    let rows: Vec<(String, String, f64, f64, f64, String, String, Vec<String>, String, String, Vec<String>, DateTime<Utc>, String)> = sqlx::query_as(
        "SELECT post_id, scan_version, nudity_score, gore_score, clickbait_score, ocr_text, transcript,
                hate_signals, risk_level, recommendation, signals, scanned_at, reviewed_by
           FROM content_scan_results ORDER BY scanned_at",
    )
    .fetch_all(feed)
    .await
    .context("reading content_scan_results")?;
    for (post, version, nudity, gore, clickbait, ocr, transcript, hate, risk, rec, signals, at, reviewed_by) in rows {
        let kind = if is_vision_ref(&post) {
            "vision"
        } else if Uuid::parse_str(&post).is_ok() {
            "work"
        } else {
            reject(pool, SRC, &post, "post_id is neither a work uuid nor a vision address").await?;
            continue;
        };
        let Some(verdict) = map_recommendation(&rec) else {
            reject(pool, SRC, &post, &format!("recommendation {rec:?} has no verdict equivalent")).await?;
            continue;
        };
        let r#ref = format!("{SRC}:{post}");
        let mut tx = pool.begin().await?;
        sqlx::query(
            "INSERT INTO scan_results (subject_kind, subject_id, scan_version, scores, labels, signals, ocr_text, transcript, origin, import_ref, created_at)
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'import', $9, $10) ON CONFLICT (import_ref) DO NOTHING",
        )
        .bind(kind)
        .bind(&post)
        .bind(&version)
        .bind(json!({ "decision": { "nsfw": nudity, "gore": gore, "clickbait": clickbait }, "risk_level": risk }))
        .bind(&hate)
        .bind(&signals)
        .bind(Some(ocr).filter(|s| !s.is_empty()))
        .bind(Some(transcript).filter(|s| !s.is_empty()))
        .bind(&r#ref)
        .bind(at)
        .execute(&mut *tx)
        .await?;
        sqlx::query(
            "INSERT INTO verdicts (subject_kind, subject_id, verdict, signals, scores, reason, source, decided_by, import_ref, created_at)
             VALUES ($1, $2, $3, $4, $5, $6, 'import', $7, $8, $9) ON CONFLICT (import_ref) DO NOTHING",
        )
        .bind(kind)
        .bind(&post)
        .bind(verdict)
        .bind(&signals)
        .bind(json!({ "nsfw": nudity, "gore": gore, "clickbait": clickbait }))
        .bind(format!("imported from feed-engine (risk {risk})"))
        .bind(if reviewed_by.is_empty() { "abraxas-legacy".to_string() } else { reviewed_by.clone() })
        .bind(&r#ref)
        .bind(at)
        .execute(&mut *tx)
        .await?;
        tx.commit().await?;
        accepted(pool, SRC, &post).await?;
    }
    Ok(())
}

// ── content_moderation_log → moderation_history ─────────────────────────────

async fn moderation_history(pool: &PgPool, feed: &PgPool) -> Result<()> {
    let rows: Vec<(Uuid, String, String, String, String, String, DateTime<Utc>)> = sqlx::query_as(
        "SELECT id, post_id, from_state, to_state, reason, actor, created_at FROM content_moderation_log ORDER BY created_at",
    )
    .fetch_all(feed)
    .await
    .context("reading content_moderation_log")?;
    for (id, post, from, to, reason, actor, at) in rows {
        sqlx::query(
            "INSERT INTO moderation_history (id, content_id, from_state, to_state, reason, actor, created_at)
             VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT (id) DO NOTHING",
        )
        .bind(id)
        .bind(&post)
        .bind(&from)
        .bind(&to)
        .bind(&reason)
        .bind(&actor)
        .bind(at)
        .execute(pool)
        .await?;
    }
    Ok(())
}

// ── csam_scan_log → csam_evidence_log ────────────────────────────────────────

async fn evidence(pool: &PgPool, feed: &PgPool, ids: &Identities) -> Result<()> {
    const SRC: &str = "feed.csam_scan_log";
    let rows: Vec<(Uuid, String, String, String, String, f32, DateTime<Utc>)> = sqlx::query_as(
        "SELECT id, media_url, pial_id, content_type, result, score, scanned_at FROM csam_scan_log ORDER BY scanned_at",
    )
    .fetch_all(feed)
    .await
    .context("reading csam_scan_log")?;
    for (id, media, pial_text, ctype, result, score, at) in rows {
        let sid = id.to_string();
        if !matches!(result.as_str(), "flagged" | "review" | "pending_scan" | "clean" | "error") {
            reject(pool, SRC, &sid, &format!("result {result:?} is not an evidence result")).await?;
            continue;
        }
        let pial = ids.resolve(&pial_text, "");
        sqlx::query(
            "INSERT INTO csam_evidence_log (surface, subject_ref, pial, legacy_pial, media_kind, result, score, import_ref, recorded_at)
             VALUES ('import', $1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT (import_ref) DO NOTHING",
        )
        .bind(&media)
        .bind(pial)
        .bind(if pial.is_none() && !pial_text.is_empty() { Some(pial_text.clone()) } else { None })
        .bind(if ctype.is_empty() { "image".to_string() } else { ctype.clone() })
        .bind(&result)
        .bind(score)
        .bind(format!("{SRC}:{id}"))
        .bind(at)
        .execute(pool)
        .await?;
        accepted(pool, SRC, &sid).await?;
    }
    Ok(())
}

// ── scan_feedback → signal_feedback ──────────────────────────────────────────

async fn feedback(pool: &PgPool, feed: &PgPool) -> Result<()> {
    const SRC: &str = "feed.scan_feedback";
    let rows: Vec<(Uuid, String, Vec<String>, String, String, DateTime<Utc>)> = sqlx::query_as(
        "SELECT id, post_id, signals, admin_decision, actioned_by, created_at FROM scan_feedback ORDER BY created_at",
    )
    .fetch_all(feed)
    .await
    .context("reading scan_feedback")?;
    for (id, post, signals, decision, by, at) in rows {
        let sid = id.to_string();
        if !matches!(decision.as_str(), "approve" | "age_gate" | "block") || signals.is_empty() {
            reject(pool, SRC, &sid, &format!("decision {decision:?} or empty signals cannot train suppression")).await?;
            continue;
        }
        sqlx::query(
            "INSERT INTO signal_feedback (subject_ref, signals, decision, decided_by, import_ref, created_at)
             VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (import_ref) DO NOTHING",
        )
        .bind(format!("work:{post}"))
        .bind(&signals)
        .bind(&decision)
        .bind(&by)
        .bind(format!("{SRC}:{id}"))
        .bind(at)
        .execute(pool)
        .await?;
        accepted(pool, SRC, &sid).await?;
    }
    Ok(())
}

// ── zodacare content_reports → reports + report_filings ──────────────────────

async fn reports(pool: &PgPool, safety: &PgPool) -> Result<()> {
    const SRC: &str = "safety.content_reports";
    #[allow(clippy::type_complexity)]
    let rows: Vec<(Uuid, Option<Uuid>, Option<Uuid>, String, String, String, Option<String>, String, Option<String>, Option<DateTime<Utc>>, bool, DateTime<Utc>)> = sqlx::query_as(
        "SELECT id, reporter_pial_id, reported_pial_id, content_id, content_type, reason, detail, status,
                resolved_by, resolved_at, escalated, created_at
           FROM content_reports ORDER BY created_at, id",
    )
    .fetch_all(safety)
    .await
    .context("reading content_reports")?;
    for (id, reporter, reported, content, ctype, reason, detail, status, resolved_by, resolved_at, escalated, at) in rows {
        let sid = id.to_string();
        let r#ref = format!("{SRC}:{id}");
        let already: bool = sqlx::query_scalar("SELECT EXISTS (SELECT 1 FROM report_filings WHERE import_ref = $1)")
            .bind(&r#ref)
            .fetch_one(pool)
            .await?;
        if already {
            accepted(pool, SRC, &sid).await?;
            continue;
        }
        let subject = match ctype.as_str() {
            "post" | "work" if Uuid::parse_str(&content).is_ok() => Some(("work", content.clone())),
            "vision" if is_vision_ref(&content) => Some(("vision", content.clone())),
            "asset" | "media" if Uuid::parse_str(&content).is_ok() => Some(("asset", content.clone())),
            "user" | "account" | "profile" => reported.map(|p| ("account", p.to_string())),
            "stream" | "live" if crate::media::check_rel(&content).is_ok() && !content.contains('/') => Some(("stream", content.clone())),
            _ => None,
        };
        let Some((sk, subject_id)) = subject else {
            reject(pool, SRC, &sid, &format!("content_type {ctype:?} with content_id {content:?} names no subject")).await?;
            continue;
        };
        let (new_status, stage) = match status.as_str() {
            "pending" => ("open", if escalated { "admin_review" } else { "queue" }),
            "reviewing" => ("open", "admin_review"),
            "actioned" => ("resolved", "queue"),
            "dismissed" => ("dismissed", "queue"),
            _ => {
                reject(pool, SRC, &sid, &format!("status {status:?} is not a report status")).await?;
                continue;
            }
        };
        let closed_at = if new_status == "open" { None } else { Some(resolved_at.unwrap_or(at)) };
        let mut tx = pool.begin().await?;
        // Open reports on the same subject merge into one case; closed ones
        // stay the cases they were.
        let case: Option<Uuid> = if new_status == "open" {
            sqlx::query_scalar("SELECT id FROM reports WHERE subject_kind = $1 AND subject_id = $2 AND kind = 'content' AND status = 'open'")
                .bind(sk)
                .bind(&subject_id)
                .fetch_optional(&mut *tx)
                .await?
        } else {
            None
        };
        let case = match case {
            Some(c) => c,
            None => {
                sqlx::query_scalar(
                    "INSERT INTO reports (id, subject_kind, subject_id, kind, reason, detail, reported_pial, status, stage,
                                          action, resolved_by, resolved_at, escalated_at, origin, created_at, updated_at)
                     VALUES ($1, $2, $3, 'content', $4, $5, $6, $7, $8, $9, $10, $11, $12, 'import', $13, $13)
                     ON CONFLICT (id) DO UPDATE SET updated_at = reports.updated_at RETURNING id",
                )
                .bind(id)
                .bind(sk)
                .bind(&subject_id)
                .bind(&reason)
                .bind(&detail)
                .bind(reported)
                .bind(new_status)
                .bind(stage)
                .bind(if new_status == "open" { None } else { Some("none") })
                .bind(&resolved_by)
                .bind(closed_at)
                .bind(if stage == "admin_review" { Some(at) } else { None })
                .bind(at)
                .fetch_one(&mut *tx)
                .await?
            }
        };
        sqlx::query(
            "INSERT INTO report_filings (report_id, reporter_pial, reason, detail, import_ref, filed_at)
             VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT DO NOTHING",
        )
        .bind(case)
        .bind(reporter)
        .bind(&reason)
        .bind(&detail)
        .bind(&r#ref)
        .bind(at)
        .execute(&mut *tx)
        .await?;
        let landed: bool = sqlx::query_scalar("SELECT EXISTS (SELECT 1 FROM report_filings WHERE import_ref = $1)")
            .bind(&r#ref)
            .fetch_one(&mut *tx)
            .await?;
        if !landed {
            // The same person filed the same open case twice in zodacare.
            tx.rollback().await?;
            reject(pool, SRC, &sid, "already_present: the reporter's filing on this case is already recorded").await?;
            continue;
        }
        if new_status == "open" && stage == "admin_review" {
            sqlx::query(
                "INSERT INTO review_queue (item_kind, subject_kind, subject_id, report_id, dedup_key, stage, reason, evidence, origin, admin_at, created_at)
                 VALUES ('report', $1, $2, $3, $4, 'admin_review', $5, '{}', 'import', NOW(), $6)
                 ON CONFLICT (dedup_key) WHERE stage <> 'decided' DO NOTHING",
            )
            .bind(sk)
            .bind(&subject_id)
            .bind(case)
            .bind(format!("report:{case}"))
            .bind(format!("report (content): {reason}"))
            .bind(at)
            .execute(&mut *tx)
            .await?;
        }
        tx.commit().await?;
        accepted(pool, SRC, &sid).await?;
    }
    Ok(())
}

// ── what feed-engine still holds for a person ────────────────────────────────

async fn open_human_review(pool: &PgPool, feed: &PgPool) -> Result<()> {
    let held: Vec<(Uuid, DateTime<Utc>)> = sqlx::query_as(
        "SELECT id, created_at FROM works WHERE scan_state = 'human_review' AND deleted_at IS NULL ORDER BY created_at",
    )
    .fetch_all(feed)
    .await
    .context("reading works held at human_review")?;
    let mut opened = 0u64;
    for (id, at) in &held {
        opened += sqlx::query(
            "INSERT INTO review_queue (item_kind, subject_kind, subject_id, dedup_key, stage, reason, evidence, origin, admin_at, created_at)
             VALUES ('content', 'work', $1, $2, 'admin_review', 'held at human_review before the integrity plane existed', '{}', 'import', NOW(), $3)
             ON CONFLICT (dedup_key) WHERE stage <> 'decided' DO NOTHING",
        )
        .bind(id.to_string())
        .bind(format!("content:work:{id}"))
        .bind(at)
        .execute(pool)
        .await?
        .rows_affected();
    }
    tracing::info!(held = held.len(), opened, "[import] works held at human_review are in the admin queue");
    Ok(())
}

// ── verification ─────────────────────────────────────────────────────────────

async fn verify(pool: &PgPool, feed: &PgPool, safety: &PgPool) -> Result<()> {
    let checks: [(&str, &PgPool, &str, &str); 7] = [
        ("feed.banned_content_hashes", feed, "SELECT COUNT(*) FROM banned_content_hashes", "SELECT COUNT(*) FROM banned_hashes WHERE import_ref LIKE 'feed.banned_content_hashes:%'"),
        ("feed.media_fingerprints", feed, "SELECT COUNT(*) FROM media_fingerprints", "SELECT COUNT(*) FROM fingerprints WHERE import_ref LIKE 'feed.media_fingerprints:%'"),
        ("feed.content_scan_results", feed, "SELECT COUNT(*) FROM content_scan_results", "SELECT COUNT(*) FROM scan_results WHERE import_ref LIKE 'feed.content_scan_results:%'"),
        ("feed.content_moderation_log", feed, "SELECT COUNT(*) FROM content_moderation_log", "SELECT COUNT(*) FROM moderation_history"),
        ("feed.csam_scan_log", feed, "SELECT COUNT(*) FROM csam_scan_log", "SELECT COUNT(*) FROM csam_evidence_log WHERE import_ref LIKE 'feed.csam_scan_log:%'"),
        ("feed.scan_feedback", feed, "SELECT COUNT(*) FROM scan_feedback", "SELECT COUNT(*) FROM signal_feedback WHERE import_ref LIKE 'feed.scan_feedback:%'"),
        ("safety.content_reports", safety, "SELECT COUNT(*) FROM content_reports", "SELECT COUNT(*) FROM report_filings WHERE import_ref LIKE 'safety.content_reports:%'"),
    ];
    let mut short = Vec::new();
    println!("{:<30} {:>10} {:>10} {:>10}", "source", "source", "imported", "rejected");
    for (name, src, count_src, count_dst) in checks {
        let s: i64 = sqlx::query_scalar(count_src).fetch_one(src).await.with_context(|| format!("counting {name}"))?;
        let d: i64 = sqlx::query_scalar(count_dst).fetch_one(pool).await?;
        let r: i64 = sqlx::query_scalar("SELECT COUNT(*) FROM import_rejects WHERE source = $1").bind(name).fetch_one(pool).await?;
        println!("{name:<30} {s:>10} {d:>10} {r:>10}");
        if d + r != s {
            short.push(format!("{name}: {s} in the source, {d} imported + {r} rejected"));
        }
    }
    if !short.is_empty() {
        bail!("import is incomplete — run it again; if it stays short, read import_rejects:\n  {}", short.join("\n  "));
    }
    println!("import verified: every source row is imported or recorded in import_rejects with its reason");
    Ok(())
}
