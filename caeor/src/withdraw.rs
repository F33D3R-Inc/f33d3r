//! Withdrawal: an asset leaves the platform, and its credit stays.
//!
//! The asset row, its raw hash and its private source are kept. Every
//! viewer-facing copy — the served derivatives and the download file — has its
//! bytes removed, and each derivative row records when. A job still encoding
//! the asset is cancelled, and whatever it writes afterwards is removed when it
//! stops (jobs::Reconciler::cancel).
//!
//! The database is updated first, so the instant this begins the read path
//! already refuses the asset. Byte removal comes second and is retryable: a
//! second withdrawal of a withdrawn asset finishes whatever the first could
//! not, and a failure is reported, never swallowed.

use std::io::ErrorKind;
use std::path::Path;

use anyhow::{Context, Result};
use serde::Serialize;
use sqlx::PgPool;
use uuid::Uuid;

use crate::assets::{self, DerivativeRow};
use crate::layout::{Layout, Location};

#[derive(Debug, Serialize)]
pub struct RemovalFailure {
    pub role: String,
    pub variant: String,
    pub path: String,
    pub error: String,
}

pub enum Outcome {
    NotFound,
    Done(Vec<RemovalFailure>),
}

async fn remove_path(abs: &Path) -> std::io::Result<()> {
    match tokio::fs::symlink_metadata(abs).await {
        Ok(m) if m.is_dir() => tokio::fs::remove_dir_all(abs).await,
        Ok(_) => tokio::fs::remove_file(abs).await,
        Err(e) if e.kind() == ErrorKind::NotFound => Ok(()),
        Err(e) => Err(e),
    }
}

/// Removes a file or directory at a location. Already gone is success.
pub async fn remove_location(layout: &Layout, loc: &Location) -> Result<()> {
    let abs = layout.abs(loc)?;
    remove_path(&abs)
        .await
        .with_context(|| format!("removing {}", abs.display()))
}

/// The bytes on disk a viewer-facing derivative occupies, or why caeor cannot
/// reach them.
fn derivative_target(layout: &Layout, d: &DerivativeRow) -> Result<std::path::PathBuf, String> {
    if let Some(loc) = d.location() {
        return layout.abs(&loc).map_err(|e| e.to_string());
    }
    // Served: an HLS derivative is its whole directory; anything else its file.
    let url = d.serves_prefix.as_deref().unwrap_or(&d.path);
    let url = url.trim_end_matches('/');
    layout.served_url_to_abs(url).ok_or_else(|| {
        format!("{} is outside the served media volume caeor holds; its bytes must be removed by the store that serves them", d.path)
    })
}

pub async fn withdraw(pool: &PgPool, layout: &Layout, id: Uuid, reason: &str) -> Result<Outcome> {
    let mut tx = pool.begin().await?;
    let exists: Option<String> = sqlx::query_scalar("SELECT status FROM assets WHERE id = $1 FOR UPDATE")
        .bind(id)
        .fetch_optional(&mut *tx)
        .await?;
    let Some(status) = exists else {
        tx.rollback().await?;
        return Ok(Outcome::NotFound);
    };
    if status != "withdrawn" {
        sqlx::query(
            "UPDATE assets SET status = 'withdrawn', withdrawn_at = NOW(), withdrawn_reason = $2,
                    updated_at = NOW() WHERE id = $1",
        )
        .bind(id)
        .bind(reason)
        .execute(&mut *tx)
        .await?;
    }
    sqlx::query(
        "UPDATE upload_jobs SET state = 'cancelled', finished_at = NOW(), updated_at = NOW(),
                last_error = 'asset withdrawn before dispatch'
          WHERE asset_id = $1 AND state = 'queued'",
    )
    .bind(id)
    .execute(&mut *tx)
    .await?;
    sqlx::query(
        "UPDATE upload_jobs SET state = 'cancelling', next_attempt_at = NOW(), updated_at = NOW()
          WHERE asset_id = $1 AND state = 'dispatched'",
    )
    .bind(id)
    .execute(&mut *tx)
    .await?;
    tx.commit().await?;
    tracing::info!(asset_id = %id, reason = %reason, "asset withdrawn");

    let mut failures = Vec::new();
    for d in assets::get_derivatives(pool, id).await? {
        if d.role == "source_private" || d.removed_at.is_some() {
            continue;
        }
        let res = match derivative_target(layout, &d) {
            Ok(abs) => remove_path(&abs).await.map_err(|e| format!("{}: {e}", abs.display())),
            Err(e) => Err(e),
        };
        match res {
            Ok(()) => {
                sqlx::query(
                    "UPDATE derivatives SET removed_at = NOW(), updated_at = NOW()
                      WHERE asset_id = $1 AND role = $2 AND variant = $3",
                )
                .bind(id)
                .bind(&d.role)
                .bind(&d.variant)
                .execute(pool)
                .await?;
            }
            Err(error) => {
                tracing::error!(asset_id = %id, role = %d.role, path = %d.path, error = %error,
                    "withdrawal could not remove a derivative's bytes");
                failures.push(RemovalFailure {
                    role: d.role.clone(),
                    variant: d.variant.clone(),
                    path: d.path.clone(),
                    error,
                });
            }
        }
    }
    // The asset's own served directory, which also holds anything a stopped
    // encode left half-written. Legacy assets have none.
    if failures.is_empty() {
        let dir = layout.media_dir.join(Layout::asset_rel(id));
        if let Err(e) = remove_path(&dir).await {
            failures.push(RemovalFailure {
                role: "asset_dir".into(),
                variant: String::new(),
                path: dir.display().to_string(),
                error: e.to_string(),
            });
        }
    }
    Ok(Outcome::Done(failures))
}
