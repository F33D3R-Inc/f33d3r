//! `abraxas-shield backfill` — put every asset caeor holds from before the gate
//! existed through the gate, so legacy media is judged and enters the
//! ownership index as the original it is.
//!
//! Reads caeor's database (CAEOR_DATABASE_URL, read-only) for every asset with
//! a private source (`source_private` derivative; `caeor remark` creates one
//! for imported media), hashes the source from the read-only private volume,
//! and runs the same check /v1/check runs, with the asset's ORIGINAL upload
//! time — so a legacy original keeps its place ahead of later copies.
//!
//! Idempotent: an asset already fingerprinted perceptually is skipped. An
//! asset whose check the worker could not answer is recorded `pending` with a
//! durable recheck queued, exactly as an upload would be. Assets with no
//! private source are counted and listed: run `caeor remark` for them, then
//! run this again. Exits non-zero if any asset failed outright.

use std::path::Path;

use anyhow::{bail, Context, Result};
use chrono::{DateTime, Utc};
use sha2::{Digest, Sha256};
use tokio::io::AsyncReadExt;
use uuid::Uuid;

use crate::api::AppState;
use crate::check::{self, CheckRequest, Failure, Source};

async fn sha256_file(path: &Path) -> std::io::Result<String> {
    let mut f = tokio::fs::File::open(path).await?;
    let mut h = Sha256::new();
    let mut buf = vec![0u8; 1 << 20];
    loop {
        let n = f.read(&mut buf).await?;
        if n == 0 {
            break;
        }
        h.update(&buf[..n]);
    }
    Ok(hex::encode(h.finalize()))
}

pub async fn run(st: &AppState) -> Result<()> {
    let caeor = crate::db::connect_existing(&crate::config::import_source("CAEOR_DATABASE_URL")?, "caeor").await?;
    #[allow(clippy::type_complexity)]
    let rows: Vec<(Uuid, Uuid, String, Option<f64>, DateTime<Utc>, Option<String>)> = sqlx::query_as(
        "SELECT a.id, a.uploader_pial, a.kind, a.duration_seconds, a.created_at, d.path
           FROM assets a
           LEFT JOIN derivatives d
             ON d.asset_id = a.id AND d.role = 'source_private' AND d.store = 'private' AND d.removed_at IS NULL
          WHERE a.status IN ('ready', 'withdrawn')
          ORDER BY a.created_at, a.id",
    )
    .fetch_all(&caeor)
    .await
    .context("reading caeor's assets")?;

    let (mut done, mut checked, mut pending, mut blocked, mut held, mut credited) = (0, 0, 0, 0, 0, 0);
    let mut no_source: Vec<Uuid> = Vec::new();
    let mut failed: Vec<String> = Vec::new();
    let total = rows.len();
    for (asset, pial, kind, duration, created, path) in rows {
        let already: bool = sqlx::query_scalar(
            "SELECT EXISTS (SELECT 1 FROM fingerprints f
                             WHERE f.asset_id = $1
                               AND (f.perceptual_at IS NOT NULL OR f.kind = 'encrypted'))",
        )
        .bind(asset)
        .fetch_one(&st.pool)
        .await?;
        if already {
            done += 1;
            continue;
        }
        let Some(rel) = path else {
            no_source.push(asset);
            continue;
        };
        let abs = match crate::media::private_abs(&st.cfg.private_dir, &rel) {
            Ok(p) => p,
            Err(e) => {
                failed.push(format!("{asset}: private path {rel:?}: {e}"));
                continue;
            }
        };
        let sha = match sha256_file(&abs).await {
            Ok(s) => s,
            Err(e) => {
                failed.push(format!("{asset}: reading {}: {e}", abs.display()));
                continue;
            }
        };
        let req = CheckRequest {
            asset_id: asset,
            kind,
            uploader_pial: pial,
            sha256: sha,
            private_path: rel,
            duration,
            uploaded_at: Some(created),
        };
        match check::run(st, req, Source::Backfill).await {
            Ok(resp) => {
                checked += 1;
                match (resp.verdict, resp.reason.starts_with("not judged yet")) {
                    ("block", _) => blocked += 1,
                    ("review", true) => pending += 1,
                    ("review", false) => held += 1,
                    _ => {}
                }
                if resp.ownership.as_ref().and_then(|o| o.get("state")).and_then(|s| s.as_str()) == Some("credited") {
                    credited += 1;
                }
            }
            Err(Failure::Db(e)) => return Err(e).context("database failure during backfill; run it again"),
            Err(f) => failed.push(format!("{asset}: {}", crate::api::ApiError::from(f).detail)),
        }
        if (checked + failed.len()) % 100 == 0 {
            tracing::info!(checked, failed = failed.len(), total, "[backfill] progress");
        }
    }

    println!("caeor assets            {total}");
    println!("already fingerprinted   {done}");
    println!("checked now             {checked}  (blocked {blocked}, held for review {held}, pending worker {pending}, credited to an earlier original {credited})");
    println!("no private source       {}", no_source.len());
    for a in no_source.iter().take(50) {
        println!("  {a}  (run `caeor remark`, then backfill again)");
    }
    println!("failed                  {}", failed.len());
    for f in &failed {
        println!("  {f}");
    }
    if !failed.is_empty() {
        bail!("{} asset(s) could not be checked", failed.len());
    }
    Ok(())
}
