//! Caeor, the media authority, as Themis needs it.
//!
//! A listing sells an encrypted body the seller uploaded. Themis does not take
//! the seller's word for that: the `content_url` must resolve through Caeor to
//! an asset of kind `encrypted` whose uploader is the seller. And a listing that
//! is deleted without ever selling has that asset withdrawn through Caeor. A
//! listing that sold keeps it: each buyer holds a key to those bytes.

use anyhow::{anyhow, Result};
use serde::Deserialize;
use uuid::Uuid;

use crate::pial_ref::PialRef;
use crate::AppState;

#[derive(Deserialize)]
struct Resolved {
    asset_id: Uuid,
    uploader_pial: Option<String>,
    kind: String,
    #[serde(default)]
    withdrawn: bool,
}

/// Why a content_url is not the seller's own encrypted asset.
#[derive(Debug)]
pub enum AssetRefusal {
    /// The answer is no.
    NotTheSellers(String),
    /// Caeor did not answer; nothing can be decided.
    Unavailable(String),
}

/// The Caeor asset a listing body names, if it is the seller's own encrypted
/// upload.
pub async fn seller_asset(state: &AppState, content_url: &str, seller: &PialRef) -> Result<Uuid, AssetRefusal> {
    let path = content_url.trim();
    if !path.starts_with('/') {
        return Err(AssetRefusal::NotTheSellers(
            "content_url must be the path of an uploaded encrypted file".into(),
        ));
    }
    let resp = state
        .http
        .get(format!("{}/v1/assets/resolve", state.cfg.caeor_url))
        .query(&[("path", path)])
        .header("X-Service-Key", &state.cfg.internal_api_key)
        .send()
        .await
        .map_err(|e| AssetRefusal::Unavailable(e.to_string()))?;
    if resp.status() == reqwest::StatusCode::NOT_FOUND {
        return Err(AssetRefusal::NotTheSellers("content_url is not an uploaded file".into()));
    }
    if !resp.status().is_success() {
        let status = resp.status();
        let body = resp.text().await.unwrap_or_default();
        return if status.is_server_error() {
            Err(AssetRefusal::Unavailable(format!("caeor {status}: {body}")))
        } else {
            Err(AssetRefusal::NotTheSellers(format!("caeor refused the path ({status})")))
        };
    }
    let r: Resolved = resp
        .json()
        .await
        .map_err(|e| AssetRefusal::Unavailable(format!("caeor answer unreadable: {e}")))?;
    if r.kind != "encrypted" {
        return Err(AssetRefusal::NotTheSellers("content_url is not an encrypted upload".into()));
    }
    if r.withdrawn {
        return Err(AssetRefusal::NotTheSellers("that upload has been withdrawn".into()));
    }
    let owner = r.uploader_pial.as_deref().and_then(|p| PialRef::parse(p).ok());
    if owner != Some(*seller) {
        return Err(AssetRefusal::NotTheSellers("that upload is not yours".into()));
    }
    Ok(r.asset_id)
}

/// Withdraws one asset through Caeor. Caeor answers a withdrawal it could not
/// finish with an error; that is retried on the next pass.
async fn withdraw(state: &AppState, asset_id: Uuid) -> Result<()> {
    let resp = state
        .http
        .post(format!("{}/v1/assets/{asset_id}/withdraw", state.cfg.caeor_url))
        .header("X-Service-Key", &state.cfg.internal_api_key)
        .json(&serde_json::json!({ "reason": "marketplace listing deleted by its seller" }))
        .send()
        .await?;
    if resp.status().is_success() || resp.status() == reqwest::StatusCode::NOT_FOUND {
        return Ok(());
    }
    Err(anyhow!("caeor withdraw {asset_id}: {} {}", resp.status(), resp.text().await.unwrap_or_default()))
}

/// Withdraws the asset of every deleted listing no buyer holds or is paying for.
pub async fn withdraw_delisted(state: &AppState) -> Result<()> {
    let due: Vec<(Uuid, Uuid)> = sqlx::query_as(
        "SELECT l.id, l.content_asset_id FROM listings l
          WHERE l.deleted_at IS NOT NULL AND l.asset_withdrawn_at IS NULL
            AND l.content_asset_id IS NOT NULL
            AND NOT EXISTS (SELECT 1 FROM purchases p
                             WHERE p.listing_id = l.id
                               AND p.status IN ('pending', 'confirmed', 'delivered'))
          LIMIT 50",
    )
    .fetch_all(&state.pool)
    .await?;
    for (listing, asset) in due {
        match withdraw(state, asset).await {
            Ok(()) => {
                sqlx::query("UPDATE listings SET asset_withdrawn_at = NOW() WHERE id = $1")
                    .bind(listing)
                    .execute(&state.pool)
                    .await?;
                tracing::info!(%listing, %asset, "deleted listing's encrypted asset withdrawn");
            }
            Err(e) => tracing::warn!(%listing, %asset, error = %e, "withdrawal of a deleted listing's asset will be retried"),
        }
    }
    Ok(())
}
