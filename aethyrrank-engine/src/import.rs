//! One-shot import of the LinUCB checkpoints AethyrRank used to keep in
//! feed-engine's database.
//!
//!     FEED_DATABASE_URL=postgres://…/f33d3r_feed \
//!     DATABASE_URL=postgres://…/f33d3r_rank \
//!         aethyrrank-engine import-feed-checkpoints
//!
//! Idempotent. A model already present in f33d3r_rank is kept as it is — it is
//! learning from ranking.events and is newer than the old checkpoint. Every
//! source row is decoded and checked first; the run then verifies, by key, that
//! every valid source model exists in f33d3r_rank, and records each verified key
//! in f33d3r_feed.linucb_checkpoints_exported. That table is what the staged
//! feed-engine migration that drops linucb_checkpoints checks before it runs.
//!
//! Exit status is non-zero when a source row does not decode or a verified
//! count does not match; the rows concerned are printed.

use anyhow::{bail, Context, Result};
use sqlx::postgres::PgPoolOptions;

use crate::{db, migrate, persist};

pub async fn run() -> Result<()> {
    let feed_url = std::env::var("FEED_DATABASE_URL")
        .context("FEED_DATABASE_URL (the f33d3r_feed database) is not set")?;
    let rank_url =
        std::env::var("DATABASE_URL").context("DATABASE_URL (the f33d3r_rank database) is not set")?;

    let feed = PgPoolOptions::new()
        .max_connections(2)
        .connect(&feed_url)
        .await
        .context("connecting to f33d3r_feed")?;
    let rank = db::connect(&rank_url, 2).await?;
    migrate::run(&rank).await?;

    let exists: bool =
        sqlx::query_scalar("SELECT to_regclass('public.linucb_checkpoints') IS NOT NULL")
            .fetch_one(&feed)
            .await
            .context("looking for f33d3r_feed.linucb_checkpoints")?;
    if !exists {
        println!("f33d3r_feed has no linucb_checkpoints table: nothing to import");
        return Ok(());
    }

    let rows: Vec<(String, serde_json::Value, serde_json::Value, i64)> = sqlx::query_as(
        "SELECT surface, a_matrix, b_vector, update_count FROM linucb_checkpoints ORDER BY surface",
    )
    .fetch_all(&feed)
    .await
    .context("reading f33d3r_feed.linucb_checkpoints")?;
    let source = rows.len();

    let mut invalid = Vec::new();
    let mut valid = Vec::new();
    for (key, a, b, count) in rows {
        match persist::decode(&key, a, b) {
            Ok(m) => valid.push((key, m, count)),
            Err(e) => invalid.push(format!("{e:#}")),
        }
    }

    let mut inserted = 0u64;
    let mut tx = rank.begin().await?;
    for (key, m, count) in &valid {
        let a: Vec<f64> = m.a_matrix.iter().copied().collect();
        let b: Vec<f64> = m.b_vector.iter().copied().collect();
        inserted += sqlx::query(
            "INSERT INTO linucb_checkpoints (model_key, dim, a_matrix, b_vector, update_count)
             VALUES ($1, $2, $3, $4, $5)
             ON CONFLICT (model_key) DO NOTHING",
        )
        .bind(key)
        .bind(b.len() as i32)
        .bind(serde_json::to_value(&a)?)
        .bind(serde_json::to_value(&b)?)
        .bind(*count)
        .execute(&mut *tx)
        .await
        .with_context(|| format!("importing model {key}"))?
        .rows_affected();
    }
    tx.commit().await.context("committing the import")?;

    let keys: Vec<String> = valid.iter().map(|(k, _, _)| k.clone()).collect();
    let present: Vec<String> =
        sqlx::query_scalar("SELECT model_key FROM linucb_checkpoints WHERE model_key = ANY($1)")
            .bind(&keys)
            .fetch_all(&rank)
            .await
            .context("verifying the import")?;

    sqlx::query(
        "CREATE TABLE IF NOT EXISTS linucb_checkpoints_exported (
             model_key   TEXT        PRIMARY KEY,
             exported_at TIMESTAMPTZ NOT NULL DEFAULT NOW())",
    )
    .execute(&feed)
    .await
    .context("creating f33d3r_feed.linucb_checkpoints_exported")?;
    sqlx::query(
        "INSERT INTO linucb_checkpoints_exported (model_key)
         SELECT unnest($1::text[]) ON CONFLICT (model_key) DO NOTHING",
    )
    .bind(&present)
    .execute(&feed)
    .await
    .context("recording the exported keys")?;

    println!("source rows:            {source}");
    println!("valid:                  {}", valid.len());
    println!("imported this run:      {inserted}");
    println!("already in f33d3r_rank: {}", present.len() as u64 - inserted.min(present.len() as u64));
    println!("verified present:       {}", present.len());
    println!("invalid (not imported): {}", invalid.len());
    for line in &invalid {
        println!("  {line}");
    }
    if present.len() != valid.len() {
        let missing: Vec<&String> = keys.iter().filter(|k| !present.contains(k)).collect();
        bail!("{} valid model(s) are not in f33d3r_rank after the import: {missing:?}", missing.len());
    }
    if !invalid.is_empty() {
        bail!(
            "{} source row(s) do not decode and were not imported; repair or delete them in \
             f33d3r_feed.linucb_checkpoints and run the import again",
            invalid.len()
        );
    }
    println!("import verified");
    Ok(())
}
