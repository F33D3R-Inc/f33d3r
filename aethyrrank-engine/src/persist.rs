/// LinUCB model persistence.
///
/// Checkpoints the per-surface A matrix and b vector to Postgres on a 60-second
/// interval and restores them on startup, giving the bandit continuity across restarts.
///
/// Schema (created automatically):
///
///   linucb_checkpoints (
///     surface      TEXT PRIMARY KEY,
///     a_matrix     JSONB  -- flat column-major f64 array, length = dim²
///     b_vector     JSONB  -- flat f64 array, length = dim
///     update_count BIGINT
///     updated_at   TIMESTAMPTZ
///   )
use std::sync::Arc;

use anyhow::Result;
use nalgebra::{DMatrix, DVector};
use sqlx::PgPool;
use tracing::{info, warn};

use crate::model_store::ModelStore;

const CREATE_TABLE: &str = r#"
CREATE TABLE IF NOT EXISTS linucb_checkpoints (
    surface      TEXT        PRIMARY KEY,
    a_matrix     JSONB       NOT NULL,
    b_vector     JSONB       NOT NULL,
    update_count BIGINT      NOT NULL DEFAULT 0,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
)
"#;

pub async fn create_pool(database_url: &str) -> Result<PgPool> {
    let pool = PgPool::connect(database_url).await?;
    Ok(pool)
}

pub async fn ensure_schema(pool: &PgPool) -> Result<()> {
    sqlx::query(CREATE_TABLE).execute(pool).await?;
    Ok(())
}

/// Restore all checkpointed surfaces into the model store. Returns the number restored.
pub async fn restore_all(pool: &PgPool, store: &ModelStore) -> Result<usize> {
    let rows = sqlx::query_as::<_, CheckpointRow>(
        "SELECT surface, a_matrix, b_vector FROM linucb_checkpoints",
    )
    .fetch_all(pool)
    .await?;

    let mut restored = 0usize;
    for row in rows {
        let a_flat: Vec<f64> = match serde_json::from_value(row.a_matrix) {
            Ok(v) => v,
            Err(e) => {
                warn!("corrupt a_matrix for surface '{}': {}", row.surface, e);
                continue;
            }
        };
        let b_flat: Vec<f64> = match serde_json::from_value(row.b_vector) {
            Ok(v) => v,
            Err(e) => {
                warn!("corrupt b_vector for surface '{}': {}", row.surface, e);
                continue;
            }
        };

        let dim = b_flat.len();
        if a_flat.len() != dim * dim || dim == 0 {
            warn!(
                "dimension mismatch for surface '{}' — skipping",
                row.surface
            );
            continue;
        }

        // nalgebra DMatrix::from_column_slice expects column-major layout, which is
        // how we serialise (iter() on DMatrix is column-major).
        let a_matrix = DMatrix::from_column_slice(dim, dim, &a_flat);
        let b_vector = DVector::from_vec(b_flat);
        store.load_checkpoint(&row.surface, a_matrix, b_vector);
        info!("restored LinUCB checkpoint for surface '{}'", row.surface);
        restored += 1;
    }
    Ok(restored)
}

/// Flush all surfaces to Postgres (upsert). Called by the background loop and on shutdown.
pub async fn flush_all(pool: &PgPool, store: &ModelStore) -> Result<()> {
    let snapshots = store.snapshot_all();
    for (surface, a_matrix, b_vector) in snapshots {
        // Flatten column-major (nalgebra default storage order).
        let a_flat: Vec<f64> = a_matrix.iter().cloned().collect();
        let b_flat: Vec<f64> = b_vector.iter().cloned().collect();
        let a_json = serde_json::to_value(&a_flat)?;
        let b_json = serde_json::to_value(&b_flat)?;

        sqlx::query(
            r#"INSERT INTO linucb_checkpoints (surface, a_matrix, b_vector, updated_at)
               VALUES ($1, $2, $3, NOW())
               ON CONFLICT (surface) DO UPDATE SET
                   a_matrix     = EXCLUDED.a_matrix,
                   b_vector     = EXCLUDED.b_vector,
                   update_count = linucb_checkpoints.update_count + 1,
                   updated_at   = NOW()"#,
        )
        .bind(&surface)
        .bind(a_json)
        .bind(b_json)
        .execute(pool)
        .await?;
    }
    Ok(())
}

/// Spawn a background task that checkpoints every `interval_secs` seconds.
pub fn start_checkpoint_loop(pool: PgPool, store: Arc<ModelStore>, interval_secs: u64) {
    tokio::spawn(async move {
        let mut ticker = tokio::time::interval(std::time::Duration::from_secs(interval_secs));
        // First tick fires immediately; skip it so we don't flush an empty model on cold start
        // before any real updates have been applied.
        ticker.tick().await;
        loop {
            ticker.tick().await;
            match flush_all(&pool, &store).await {
                Ok(()) => info!(
                    "LinUCB checkpoint saved ({} surfaces)",
                    store.surfaces().len()
                ),
                Err(e) => warn!("LinUCB checkpoint failed: {e}"),
            }
        }
    });
}

// ── sqlx FromRow helper ────────────────────────────────────────────────────────

#[derive(sqlx::FromRow)]
struct CheckpointRow {
    surface: String,
    a_matrix: serde_json::Value,
    b_vector: serde_json::Value,
}
