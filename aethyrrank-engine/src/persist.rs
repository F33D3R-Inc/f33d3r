//! LinUCB state in f33d3r_rank (schema: migrations/0001_rank_state.sql).
//!
//! The models are written by the ranking.events consumer in the same
//! transaction as the offsets they account for (learning::consumer), so
//! there is no periodic checkpoint: what is stored is always a state the
//! consumer reached, together with where it reached it.

use std::collections::HashMap;

use anyhow::{bail, Context, Result};
use nalgebra::{DMatrix, DVector};
use sqlx::{PgPool, Postgres, Transaction};

use crate::model_store::ModelStore;

/// A model row, checked: the shape matches its dim.
pub struct ModelRow {
    pub a_matrix: DMatrix<f64>,
    pub b_vector: DVector<f64>,
}

/// Decode a stored model. A row that does not decode is an error, never
/// skipped: skipping it would silently reset a person's learned preference.
pub fn decode(model_key: &str, a: serde_json::Value, b: serde_json::Value) -> Result<ModelRow> {
    let a_flat: Vec<f64> =
        serde_json::from_value(a).with_context(|| format!("model {model_key}: a_matrix"))?;
    let b_flat: Vec<f64> =
        serde_json::from_value(b).with_context(|| format!("model {model_key}: b_vector"))?;
    let dim = b_flat.len();
    if dim == 0 || a_flat.len() != dim * dim {
        bail!(
            "model {model_key}: a_matrix has {} values and b_vector {}; not a {dim}×{dim} model",
            a_flat.len(),
            dim
        );
    }
    // Column-major, which is how nalgebra iterates a DMatrix and so how it was written.
    Ok(ModelRow {
        a_matrix: DMatrix::from_column_slice(dim, dim, &a_flat),
        b_vector: DVector::from_vec(b_flat),
    })
}

/// Load every stored model into the store. Returns how many.
pub async fn restore_all(pool: &PgPool, store: &ModelStore) -> Result<usize> {
    let rows: Vec<(String, serde_json::Value, serde_json::Value)> =
        sqlx::query_as("SELECT model_key, a_matrix, b_vector FROM linucb_checkpoints")
            .fetch_all(pool)
            .await
            .context("reading linucb_checkpoints")?;
    let n = rows.len();
    for (key, a, b) in rows {
        let m = decode(&key, a, b)?;
        if m.b_vector.len() != store.dim() {
            bail!(
                "model {key} has dimension {} but the engine's feature vector has {}; \
                 a changed feature layout needs a migration of the stored models",
                m.b_vector.len(),
                store.dim()
            );
        }
        store.install(&key, m.a_matrix, m.b_vector);
    }
    Ok(n)
}

/// Write one model inside the batch transaction. `updates` is how many updates
/// and downdates this batch applied to it.
pub async fn write_model(
    tx: &mut Transaction<'_, Postgres>,
    model_key: &str,
    a_matrix: &DMatrix<f64>,
    b_vector: &DVector<f64>,
    updates: i64,
) -> Result<()> {
    let a: Vec<f64> = a_matrix.iter().copied().collect();
    let b: Vec<f64> = b_vector.iter().copied().collect();
    sqlx::query(
        "INSERT INTO linucb_checkpoints (model_key, dim, a_matrix, b_vector, update_count, updated_at)
         VALUES ($1, $2, $3, $4, $5, NOW())
         ON CONFLICT (model_key) DO UPDATE SET
             dim          = EXCLUDED.dim,
             a_matrix     = EXCLUDED.a_matrix,
             b_vector     = EXCLUDED.b_vector,
             update_count = linucb_checkpoints.update_count + EXCLUDED.update_count,
             updated_at   = NOW()",
    )
    .bind(model_key)
    .bind(b.len() as i32)
    .bind(serde_json::to_value(&a)?)
    .bind(serde_json::to_value(&b)?)
    .bind(updates)
    .execute(&mut **tx)
    .await
    .with_context(|| format!("writing model {model_key}"))?;
    Ok(())
}

/// The next offset to read per partition, as last committed.
pub async fn stored_offsets(pool: &PgPool, topic: &str) -> Result<HashMap<i32, i64>> {
    let rows: Vec<(i32, i64)> = sqlx::query_as(
        "SELECT partition, next_offset FROM ranking_consumer_offsets WHERE topic = $1",
    )
    .bind(topic)
    .fetch_all(pool)
    .await
    .context("reading ranking_consumer_offsets")?;
    Ok(rows.into_iter().collect())
}

pub async fn write_offset(
    tx: &mut Transaction<'_, Postgres>,
    topic: &str,
    partition: i32,
    next_offset: i64,
) -> Result<()> {
    sqlx::query(
        "INSERT INTO ranking_consumer_offsets (topic, partition, next_offset, updated_at)
         VALUES ($1, $2, $3, NOW())
         ON CONFLICT (topic, partition) DO UPDATE SET
             next_offset = EXCLUDED.next_offset, updated_at = NOW()",
    )
    .bind(topic)
    .bind(partition)
    .bind(next_offset)
    .execute(&mut **tx)
    .await
    .with_context(|| format!("writing offset of {topic}[{partition}]"))?;
    Ok(())
}
