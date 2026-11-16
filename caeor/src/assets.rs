//! Asset and derivative records: the rows, the reads, and the view every
//! endpoint answers with.

use chrono::{DateTime, Utc};
use serde::Serialize;
use sqlx::{PgPool, Postgres, Transaction};
use uuid::Uuid;

use crate::db::is_unique_violation;
use crate::ids::new_short_id;
use crate::layout::{served_url, Location, Store};

// ── Vocabulary ───────────────────────────────────────────────────────────────

/// Kinds a caller may upload. `clip` is made from a replay, not uploaded.
pub const UPLOAD_KINDS: &[&str] = &["image", "video", "voice", "audio", "avatar", "header", "encrypted"];

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Kind {
    Image,
    Video,
    Voice,
    Audio,
    Avatar,
    Header,
    Encrypted,
    Clip,
}

impl Kind {
    pub fn parse(s: &str) -> Option<Self> {
        Some(match s {
            "image" => Kind::Image,
            "video" => Kind::Video,
            "voice" => Kind::Voice,
            "audio" => Kind::Audio,
            "avatar" => Kind::Avatar,
            "header" => Kind::Header,
            "encrypted" => Kind::Encrypted,
            "clip" => Kind::Clip,
            _ => return None,
        })
    }
    pub fn as_str(self) -> &'static str {
        match self {
            Kind::Image => "image",
            Kind::Video => "video",
            Kind::Voice => "voice",
            Kind::Audio => "audio",
            Kind::Avatar => "avatar",
            Kind::Header => "header",
            Kind::Encrypted => "encrypted",
            Kind::Clip => "clip",
        }
    }
    /// Content kinds carry the visible watermark on every viewer-facing copy.
    /// Avatars and headers are identity, not content.
    pub fn is_visibly_marked(self) -> bool {
        matches!(self, Kind::Image | Kind::Video | Kind::Clip)
    }
    /// Kinds whose raw bytes earn credit in raw_hashes. Identity images and
    /// ciphertext do not: re-using one's own avatar is not a repost, and the
    /// same plaintext never encrypts to the same bytes twice.
    pub fn is_credited_by_hash(self) -> bool {
        matches!(self, Kind::Image | Kind::Video | Kind::Voice | Kind::Audio)
    }
}

// ── Rows ─────────────────────────────────────────────────────────────────────

#[derive(Debug, Clone, sqlx::FromRow)]
pub struct AssetRow {
    pub id: Uuid,
    pub short_id: String,
    pub uploader_pial: Uuid,
    pub uploader_account: Option<Uuid>,
    pub uploader_handle: String,
    pub kind: String,
    pub status: String,
    pub origin: String,
    pub raw_sha256: Option<String>,
    pub byte_size: Option<i64>,
    pub width: Option<i32>,
    pub height: Option<i32>,
    pub duration_seconds: Option<f64>,
    pub failure: Option<String>,
    pub created_at: DateTime<Utc>,
    pub withdrawn_at: Option<DateTime<Utc>>,
    pub withdrawn_reason: Option<String>,
    pub integrity_verdict: Option<String>,
    pub original_asset: Option<Uuid>,
    pub original_pial: Option<Uuid>,
    pub ownership_kind: Option<String>,
}

impl AssetRow {
    pub fn kind(&self) -> Kind {
        // The CHECK constraint admits only parseable kinds.
        Kind::parse(&self.kind).unwrap_or(Kind::Encrypted)
    }
    pub fn is_withdrawn(&self) -> bool {
        self.status == "withdrawn"
    }
}

const ASSET_COLUMNS: &str = "id, short_id, uploader_pial, uploader_account, uploader_handle, kind, status, \
     origin, raw_sha256, byte_size, width, height, duration_seconds, failure, created_at, \
     withdrawn_at, withdrawn_reason, integrity_verdict, original_asset, original_pial, ownership_kind";

#[derive(Debug, Clone, sqlx::FromRow)]
pub struct DerivativeRow {
    pub role: String,
    pub variant: String,
    pub store: String,
    pub path: String,
    pub serves_prefix: Option<String>,
    pub watermarked: bool,
    pub content_type: String,
    pub width: Option<i32>,
    pub height: Option<i32>,
    pub bandwidth: Option<i32>,
    pub byte_size: Option<i64>,
    pub removed_at: Option<DateTime<Utc>>,
}

const DERIVATIVE_COLUMNS: &str = "role, variant, store, path, serves_prefix, watermarked, \
     content_type, width, height, bandwidth, byte_size, removed_at";

impl DerivativeRow {
    pub fn is_served(&self) -> bool {
        self.store == "served"
    }
    pub fn location(&self) -> Option<Location> {
        match self.store.as_str() {
            "private" => Some(Location::private(self.path.clone())),
            _ => None,
        }
    }
}

/// A derivative about to be written.
#[derive(Debug, Clone)]
pub struct NewDerivative {
    pub role: &'static str,
    pub variant: String,
    pub store: Store,
    /// served: the URL path. private: relative to the private store.
    pub path: String,
    pub serves_prefix: Option<String>,
    pub watermarked: bool,
    pub content_type: String,
    pub width: Option<i32>,
    pub height: Option<i32>,
    pub bandwidth: Option<i32>,
    pub byte_size: Option<i64>,
}

impl NewDerivative {
    /// A served file at a served-store location.
    pub fn served_file(role: &'static str, loc: &Location, watermarked: bool, content_type: &str) -> Self {
        Self {
            role,
            variant: String::new(),
            store: Store::Served,
            path: served_url(&loc.rel),
            serves_prefix: None,
            watermarked,
            content_type: content_type.to_string(),
            width: None,
            height: None,
            bandwidth: None,
            byte_size: None,
        }
    }
    /// A file in the private store.
    pub fn private_file(role: &'static str, loc: &Location, watermarked: bool, content_type: &str) -> Self {
        Self {
            role,
            variant: String::new(),
            store: Store::Private,
            path: loc.rel.clone(),
            serves_prefix: None,
            watermarked,
            content_type: content_type.to_string(),
            width: None,
            height: None,
            bandwidth: None,
            byte_size: None,
        }
    }
    pub fn dims(mut self, w: u32, h: u32) -> Self {
        self.width = Some(w as i32);
        self.height = Some(h as i32);
        self
    }
    pub fn size(mut self, bytes: u64) -> Self {
        self.byte_size = Some(bytes as i64);
        self
    }
}

/// A new asset's identity and facts, before it is inserted.
#[derive(Debug, Clone)]
pub struct NewAsset {
    pub id: Uuid,
    pub uploader_pial: Uuid,
    pub uploader_account: Option<Uuid>,
    pub uploader_handle: String,
    pub kind: Kind,
    pub status: &'static str,
    pub origin: &'static str,
    pub raw_sha256: Option<String>,
    pub byte_size: Option<i64>,
    pub width: Option<i32>,
    pub height: Option<i32>,
    pub created_at: Option<DateTime<Utc>>,
    pub withdrawn_at: Option<DateTime<Utc>>,
    pub withdrawn_reason: Option<String>,
    /// Some for an upload inserted `held` while Abraxas is asked about it: the
    /// asset is recorded with verdict 'pending', and the reconciler will not
    /// ask again before this much time has passed (the answer the request
    /// itself is waiting for comes first).
    pub held_for: Option<std::time::Duration>,
}

// ── Writes ───────────────────────────────────────────────────────────────────

/// Inserts an asset under a fresh short id, retrying the (rare) collision.
/// Returns the short id. With `idempotent`, an asset that already exists under
/// this id is left untouched and Ok(None) is returned.
pub async fn insert_asset(
    tx: &mut Transaction<'_, Postgres>,
    a: &NewAsset,
    short_id: Option<&str>,
    idempotent: bool,
) -> sqlx::Result<Option<String>> {
    for _ in 0..8 {
        let sid = short_id.map(str::to_string).unwrap_or_else(new_short_id);
        let sql = format!(
            "INSERT INTO assets (id, short_id, uploader_pial, uploader_account, uploader_handle, kind, \
                                 status, origin, raw_sha256, byte_size, width, height, created_at, \
                                 withdrawn_at, withdrawn_reason, integrity_verdict, integrity_next_check_at)
             VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,COALESCE($13, NOW()),$14,$15,
                     CASE WHEN $16::float8 IS NULL THEN NULL ELSE 'pending' END,
                     CASE WHEN $16::float8 IS NULL THEN NULL ELSE NOW() + $16::float8 * INTERVAL '1 second' END)
             {}",
            if idempotent { "ON CONFLICT (id) DO NOTHING" } else { "" }
        );
        // A savepoint, so a short-id collision does not abort the caller's
        // transaction.
        let mut sp = sqlx::Acquire::begin(&mut **tx).await?;
        let res = sqlx::query(&sql)
            .bind(a.id)
            .bind(&sid)
            .bind(a.uploader_pial)
            .bind(a.uploader_account)
            .bind(&a.uploader_handle)
            .bind(a.kind.as_str())
            .bind(a.status)
            .bind(a.origin)
            .bind(&a.raw_sha256)
            .bind(a.byte_size)
            .bind(a.width)
            .bind(a.height)
            .bind(a.created_at)
            .bind(a.withdrawn_at)
            .bind(&a.withdrawn_reason)
            .bind(a.held_for.map(|d| d.as_secs_f64()))
            .execute(&mut *sp)
            .await;
        match res {
            Ok(done) => {
                sp.commit().await?;
                if done.rows_affected() == 0 {
                    return Ok(None);
                }
                return Ok(Some(sid));
            }
            Err(e) if is_unique_violation(&e, "assets_short_id_key") && short_id.is_none() => {
                sp.rollback().await?;
                continue;
            }
            Err(e) => return Err(e),
        }
    }
    Err(sqlx::Error::Protocol("could not allocate a unique short id in 8 attempts".into()))
}

/// Writes a derivative, replacing an earlier one in the same role/variant.
pub async fn upsert_derivative(
    tx: &mut Transaction<'_, Postgres>,
    asset_id: Uuid,
    d: &NewDerivative,
) -> sqlx::Result<()> {
    sqlx::query(
        "INSERT INTO derivatives (asset_id, role, variant, store, path, serves_prefix, watermarked,
                                  content_type, width, height, bandwidth, byte_size)
         VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
         ON CONFLICT (asset_id, role, variant) DO UPDATE
            SET store = EXCLUDED.store, path = EXCLUDED.path,
                serves_prefix = EXCLUDED.serves_prefix, watermarked = EXCLUDED.watermarked,
                content_type = EXCLUDED.content_type, width = EXCLUDED.width,
                height = EXCLUDED.height, bandwidth = EXCLUDED.bandwidth,
                byte_size = EXCLUDED.byte_size, removed_at = NULL, updated_at = NOW()",
    )
    .bind(asset_id)
    .bind(d.role)
    .bind(&d.variant)
    .bind(d.store.as_str())
    .bind(&d.path)
    .bind(&d.serves_prefix)
    .bind(d.watermarked)
    .bind(&d.content_type)
    .bind(d.width)
    .bind(d.height)
    .bind(d.bandwidth)
    .bind(d.byte_size)
    .execute(&mut **tx)
    .await?;
    Ok(())
}

/// Records `sha256` as first seen in `asset_id`. Returns false when the hash
/// was already credited to an earlier asset.
pub async fn claim_raw_hash(
    tx: &mut Transaction<'_, Postgres>,
    sha256: &str,
    asset_id: Uuid,
    uploader_pial: Uuid,
    created_at: Option<DateTime<Utc>>,
) -> sqlx::Result<bool> {
    let done = sqlx::query(
        "INSERT INTO raw_hashes (sha256, asset_id, uploader_pial, created_at)
         VALUES ($1, $2, $3, COALESCE($4, NOW()))
         ON CONFLICT (sha256) DO NOTHING",
    )
    .bind(sha256)
    .bind(asset_id)
    .bind(uploader_pial)
    .bind(created_at)
    .execute(&mut **tx)
    .await?;
    Ok(done.rows_affected() == 1)
}

// ── Reads ────────────────────────────────────────────────────────────────────

pub async fn get_asset(pool: &PgPool, id: Uuid) -> sqlx::Result<Option<AssetRow>> {
    sqlx::query_as::<_, AssetRow>(&format!("SELECT {ASSET_COLUMNS} FROM assets WHERE id = $1"))
        .bind(id)
        .fetch_optional(pool)
        .await
}

pub async fn get_derivatives(pool: &PgPool, id: Uuid) -> sqlx::Result<Vec<DerivativeRow>> {
    sqlx::query_as::<_, DerivativeRow>(&format!(
        "SELECT {DERIVATIVE_COLUMNS} FROM derivatives WHERE asset_id = $1 ORDER BY role, variant"
    ))
    .bind(id)
    .fetch_all(pool)
    .await
}

/// The asset first credited with these exact bytes.
#[derive(Debug, Clone, Serialize, sqlx::FromRow)]
pub struct DuplicateOf {
    pub asset_id: Uuid,
    pub short_id: String,
    pub uploader_pial: Uuid,
    pub kind: String,
    pub status: String,
    pub created_at: DateTime<Utc>,
    /// Why the original was withdrawn, if it was. Not part of the duplicate
    /// answer: a banned original is answered as banned, never as a duplicate.
    #[serde(skip)]
    pub withdrawn_reason: Option<String>,
}

impl DuplicateOf {
    /// True when the original was refused by the integrity authority.
    pub fn is_banned(&self) -> bool {
        self.status == "withdrawn"
            && self.withdrawn_reason.as_deref().is_some_and(|r| r.starts_with(crate::integrity::BANNED_PREFIX))
    }
}

pub async fn find_by_raw_hash(pool: &PgPool, sha256: &str) -> sqlx::Result<Option<DuplicateOf>> {
    sqlx::query_as::<_, DuplicateOf>(
        "SELECT a.id AS asset_id, a.short_id, a.uploader_pial, a.kind, a.status, a.created_at,
                a.withdrawn_reason
           FROM raw_hashes r JOIN assets a ON a.id = r.asset_id
          WHERE r.sha256 = $1",
    )
    .bind(sha256)
    .fetch_optional(pool)
    .await
}

/// What a served path is.
#[derive(Debug, Clone, sqlx::FromRow)]
pub struct Resolved {
    pub asset_id: Uuid,
    pub short_id: String,
    pub uploader_pial: Uuid,
    pub uploader_account: Option<Uuid>,
    pub kind: String,
    pub status: String,
    pub role: String,
    pub variant: String,
    pub watermarked: bool,
    pub content_type: String,
    pub removed_at: Option<DateTime<Utc>>,
}

/// Looks a served URL path up: first as a derivative's own path, then as a
/// file under an HLS derivative's directory (a segment under its playlist),
/// nearest directory first. Only the served store is ever consulted, so a
/// private source cannot be resolved by any path.
pub async fn resolve_path(pool: &PgPool, path: &str, ancestors: &[String]) -> sqlx::Result<Option<Resolved>> {
    const SELECT: &str = "SELECT a.id AS asset_id, a.short_id, a.uploader_pial, a.uploader_account, a.kind,
                a.status, d.role, d.variant, d.watermarked, d.content_type, d.removed_at
           FROM derivatives d JOIN assets a ON a.id = d.asset_id";
    if let Some(r) = sqlx::query_as::<_, Resolved>(&format!(
        "{SELECT} WHERE d.store = 'served' AND d.path = $1"
    ))
    .bind(path)
    .fetch_optional(pool)
    .await?
    {
        return Ok(Some(r));
    }
    if ancestors.is_empty() {
        return Ok(None);
    }
    sqlx::query_as::<_, Resolved>(&format!(
        "{SELECT} WHERE d.store = 'served' AND d.serves_prefix = ANY($1)
          ORDER BY length(d.serves_prefix) DESC LIMIT 1"
    ))
    .bind(ancestors)
    .fetch_optional(pool)
    .await
}

// ── The view ─────────────────────────────────────────────────────────────────

#[derive(Debug, Serialize)]
pub struct DerivativeView {
    pub role: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub variant: String,
    /// The served URL path. Absent for private roles (source, download):
    /// those are never addressed by path.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub url: Option<String>,
    pub watermarked: bool,
    pub content_type: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub width: Option<i32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub height: Option<i32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub bandwidth: Option<i32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub bytes: Option<i64>,
    pub removed: bool,
}

#[derive(Debug, Default, Serialize)]
pub struct Urls {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub thumb: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub feed: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub full: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub hls_master: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub poster: Option<String>,
}

#[derive(Debug, Serialize)]
pub struct JobView {
    pub job_id: Uuid,
    pub purpose: String,
    pub state: String,
    pub attempts: i32,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub last_error: Option<String>,
}

/// Who these bytes are credited to when the integrity authority found them to
/// be (or derive from) somebody else's asset. The caller shows the original
/// owner wherever this asset is shown.
#[derive(Debug, Serialize)]
pub struct CreditView {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub original_asset: Option<Uuid>,
    pub original_pial: Uuid,
    /// exact | derivative | excerpt
    pub kind: String,
}

#[derive(Debug, Serialize)]
pub struct AssetView {
    pub asset_id: Uuid,
    pub short_id: String,
    pub kind: String,
    pub status: String,
    pub origin: String,
    pub uploader_pial: Uuid,
    pub uploader_account: Option<Uuid>,
    pub uploader_handle: String,
    pub raw_sha256: Option<String>,
    pub bytes: Option<i64>,
    pub width: Option<i32>,
    pub height: Option<i32>,
    pub duration_seconds: Option<f64>,
    pub created_at: DateTime<Utc>,
    pub withdrawn: bool,
    pub withdrawn_at: Option<DateTime<Utc>>,
    pub withdrawn_reason: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub failure: Option<String>,
    /// What Abraxas last said: pending | allow | review | block | unreachable.
    /// Absent for assets that predate the integrity gate.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub integrity_verdict: Option<String>,
    /// The original owner to credit, when these bytes are somebody else's.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub credit_to: Option<CreditView>,
    /// The URL a work shows: feed for images, the HLS master for video, the
    /// file itself for audio. None until ready, and after withdrawal.
    pub primary_url: Option<String>,
    pub urls: Urls,
    /// True when a download file exists (video: the watermarked MP4; image:
    /// the watermarked full). Whether a viewer may have it is feed-engine's
    /// per-work decision.
    pub downloadable: bool,
    /// True when every served image/video derivative carries the watermark.
    pub fully_watermarked: bool,
    pub derivatives: Vec<DerivativeView>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub job: Option<JobView>,
}

pub fn build_view(a: &AssetRow, ds: &[DerivativeRow], job: Option<JobView>) -> AssetView {
    let live = |d: &&DerivativeRow| d.is_served() && d.removed_at.is_none() && !a.is_withdrawn();
    let url_of = |role: &str| {
        ds.iter()
            .filter(live)
            .find(|d| d.role == role)
            .map(|d| d.path.clone())
    };
    let mut urls = Urls {
        thumb: url_of("thumb"),
        feed: url_of("feed"),
        full: url_of("full"),
        hls_master: url_of("hls_master"),
        poster: url_of("poster"),
    };
    if a.status != "ready" {
        urls = Urls::default();
    }
    let primary_url = urls
        .hls_master
        .clone()
        .or_else(|| urls.feed.clone())
        .or_else(|| urls.full.clone());

    let downloadable = a.status == "ready"
        && match a.kind() {
            Kind::Video | Kind::Clip => ds
                .iter()
                .any(|d| d.role == "download" && d.removed_at.is_none()),
            Kind::Image | Kind::Voice | Kind::Audio => urls.full.is_some(),
            Kind::Avatar | Kind::Header | Kind::Encrypted => false,
        };

    let fully_watermarked = !a.kind().is_visibly_marked()
        || ds
            .iter()
            .filter(|d| d.is_served() && d.removed_at.is_none() && d.role != "download")
            .all(|d| d.watermarked);

    AssetView {
        asset_id: a.id,
        short_id: a.short_id.clone(),
        kind: a.kind.clone(),
        status: a.status.clone(),
        origin: a.origin.clone(),
        uploader_pial: a.uploader_pial,
        uploader_account: a.uploader_account,
        uploader_handle: a.uploader_handle.clone(),
        raw_sha256: a.raw_sha256.clone(),
        bytes: a.byte_size,
        width: a.width,
        height: a.height,
        duration_seconds: a.duration_seconds,
        created_at: a.created_at,
        withdrawn: a.is_withdrawn(),
        withdrawn_at: a.withdrawn_at,
        withdrawn_reason: a.withdrawn_reason.clone(),
        failure: a.failure.clone(),
        integrity_verdict: a.integrity_verdict.clone(),
        credit_to: match (a.original_pial, &a.ownership_kind) {
            (Some(original_pial), Some(kind)) => Some(CreditView {
                original_asset: a.original_asset,
                original_pial,
                kind: kind.clone(),
            }),
            _ => None,
        },
        primary_url,
        urls,
        downloadable,
        fully_watermarked,
        derivatives: ds
            .iter()
            .map(|d| DerivativeView {
                role: d.role.clone(),
                variant: d.variant.clone(),
                url: if d.is_served() && d.role != "download" { Some(d.path.clone()) } else { None },
                watermarked: d.watermarked,
                content_type: d.content_type.clone(),
                width: d.width,
                height: d.height,
                bandwidth: d.bandwidth,
                bytes: d.byte_size,
                removed: d.removed_at.is_some(),
            })
            .collect(),
        job,
    }
}

pub async fn load_view(pool: &PgPool, id: Uuid) -> sqlx::Result<Option<AssetView>> {
    let Some(a) = get_asset(pool, id).await? else {
        return Ok(None);
    };
    let ds = get_derivatives(pool, id).await?;
    let job = sqlx::query_as::<_, (Uuid, String, String, i32, Option<String>)>(
        "SELECT id, purpose, state, attempts, last_error FROM upload_jobs
          WHERE asset_id = $1 ORDER BY created_at DESC LIMIT 1",
    )
    .bind(id)
    .fetch_optional(pool)
    .await?
    .map(|(job_id, purpose, state, attempts, last_error)| JobView {
        job_id,
        purpose,
        state,
        attempts,
        last_error,
    });
    Ok(Some(build_view(&a, &ds, job)))
}
