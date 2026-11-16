//! `caeor import` — the one-shot move of feed-engine's media registry into
//! caeor's authority.
//!
//! Reads canonical_media, video_raw_hashes and image_raw_hashes from
//! FEED_DATABASE_URL and writes assets, derivatives and raw_hashes here.
//! Published files are not moved: every existing served path stays valid and
//! is recorded as a derivative of its asset. A canonical_media row becomes the
//! asset with the SAME id, so feed-engine's references map one to one.
//!
//! Idempotent: assets, derivatives and hashes are inserted only when absent,
//! so a second run changes nothing and a run that was interrupted is finished
//! by running it again. It ends by counting what it should have written
//! against what exists, and exits non-zero on any shortfall.
//!
//! Imported image and video derivatives are recorded as NOT watermarked,
//! because they are not. The read path refuses them (resolve: servable=false)
//! until `caeor remark` has marked them in place.

use std::collections::{BTreeMap, HashMap, HashSet};

use anyhow::{bail, Context, Result};
use chrono::{DateTime, Utc};
use sqlx::postgres::PgPoolOptions;
use sqlx::{PgPool, Postgres, Transaction};
use uuid::Uuid;

use crate::assets::{self, Kind, NewAsset, NewDerivative};
use crate::ids::mark_handle;
use crate::layout::{Layout, Store};

#[derive(sqlx::FromRow)]
struct Canon {
    id: Uuid,
    pial: Option<Uuid>,
    account: Option<Uuid>,
    handle: String,
    image_url: String,
    master_url: String,
    poster_url: String,
    audio_url: String,
    asset_dir: String,
    watermarked_url: String,
    raw_sha256: String,
    width: i32,
    height: i32,
    created_at: DateTime<Utc>,
    withdrawn_at: Option<DateTime<Utc>>,
}

struct Plan {
    kind: Kind,
    derivatives: Vec<NewDerivative>,
}

#[derive(Default)]
struct Report {
    canonical_rows: usize,
    unattributed: Vec<Uuid>,
    no_media: Vec<Uuid>,
    by_kind: BTreeMap<&'static str, usize>,
    assets_created: usize,
    derivatives_planned: usize,
    derivatives_created: usize,
    derivative_path_conflicts: Vec<String>,
    missing_files: Vec<String>,
    outside_store: usize,
    hashes_seen: usize,
    hashes_mapped: usize,
    hashes_unmapped: Vec<String>,
    hashes_created: usize,
}

fn content_type_for(path: &str) -> &'static str {
    let ext = path.rsplit('.').next().unwrap_or("").to_ascii_lowercase();
    match ext.as_str() {
        "webp" => "image/webp",
        "jpg" | "jpeg" => "image/jpeg",
        "png" => "image/png",
        "gif" => "image/gif",
        "mp4" | "m4v" => "video/mp4",
        "mov" => "video/quicktime",
        "webm" => "video/webm",
        "m3u8" => "application/vnd.apple.mpegurl",
        "mp3" => "audio/mpeg",
        "m4a" => "audio/mp4",
        "ogg" | "opus" | "oga" => "audio/ogg",
        "wav" => "audio/wav",
        "aac" => "audio/aac",
        "flac" => "audio/flac",
        _ => "application/octet-stream",
    }
}

fn parent_dir(url: &str) -> &str {
    url.rsplit_once('/').map(|(d, _)| d).unwrap_or("")
}

/// The stem an image's -thumb/-feed/-full files share (feed-engine's rule,
/// migration 0034).
fn image_stem(url: &str) -> String {
    let no_ext = match url.rsplit_once('.') {
        Some((s, e)) if !e.contains('/') => s,
        _ => url,
    };
    for suffix in ["-thumb", "-feed", "-full"] {
        if let Some(s) = no_ext.strip_suffix(suffix) {
            return s.to_string();
        }
    }
    no_ext.to_string()
}

/// The legacy HLS directory a master URL sits in, without a trailing /clean.
fn video_dir(master_url: &str) -> Option<String> {
    let s = master_url
        .strip_suffix("/clean/master.m3u8")
        .or_else(|| master_url.strip_suffix("/master.m3u8"))?;
    Some(s.to_string())
}

struct Importer<'a> {
    layout: &'a Layout,
    report: Report,
}

impl Importer<'_> {
    /// True when the file exists; paths outside the served volume caeor holds
    /// cannot be checked and count as present but unverified.
    async fn exists(&mut self, url: &str) -> bool {
        match self.layout.served_url_to_abs(url) {
            Some(abs) => match tokio::fs::try_exists(&abs).await {
                Ok(true) => true,
                Ok(false) => false,
                Err(e) => {
                    tracing::error!(path = %abs.display(), error = %e, "checking a legacy file");
                    false
                }
            },
            None => {
                self.report.outside_store += 1;
                true
            }
        }
    }

    async fn file_size(&self, url: &str) -> Option<i64> {
        let abs = self.layout.served_url_to_abs(url)?;
        tokio::fs::metadata(&abs).await.ok().map(|m| m.len() as i64)
    }

    async fn served(&mut self, role: &'static str, url: &str, watermarked: bool) -> NewDerivative {
        if !self.exists(url).await {
            self.report.missing_files.push(url.to_string());
        }
        NewDerivative {
            role,
            variant: String::new(),
            store: Store::Served,
            path: url.to_string(),
            serves_prefix: None,
            watermarked,
            content_type: content_type_for(url).to_string(),
            width: None,
            height: None,
            bandwidth: None,
            byte_size: self.file_size(url).await,
        }
    }

    async fn plan(&mut self, c: &Canon) -> Result<Option<Plan>> {
        if !c.asset_dir.is_empty() || c.master_url.ends_with(".m3u8") {
            let dir = if !c.asset_dir.is_empty() {
                c.asset_dir.trim_end_matches('/').to_string()
            } else {
                video_dir(&c.master_url).unwrap_or_else(|| parent_dir(&c.master_url).to_string())
            };
            let mut candidates = vec![format!("{dir}/clean/master.m3u8"), format!("{dir}/master.m3u8")];
            if c.master_url.ends_with(".m3u8") {
                candidates.insert(0, c.master_url.clone());
            }
            let mut master = None;
            for m in &candidates {
                if self.exists(m).await {
                    master = Some(m.clone());
                    break;
                }
            }
            let master = match master {
                Some(m) => m,
                None => {
                    let m = candidates[0].clone();
                    self.report.missing_files.push(m.clone());
                    m
                }
            };
            let hls_dir = parent_dir(&master).to_string();
            let mut ds = Vec::new();
            let mut md = self.served("hls_master", &master, false).await;
            md.serves_prefix = Some(format!("{hls_dir}/"));
            if c.width > 0 && c.height > 0 {
                md.width = Some(c.width);
                md.height = Some(c.height);
            }
            ds.push(md);

            // Renditions, from the master playlist itself.
            if let Some(abs) = self.layout.served_url_to_abs(&master) {
                if let Ok(text) = tokio::fs::read_to_string(&abs).await {
                    let mut bandwidth = None;
                    for line in text.lines().map(str::trim) {
                        if let Some(attrs) = line.strip_prefix("#EXT-X-STREAM-INF:") {
                            bandwidth = attrs
                                .split(',')
                                .find_map(|kv| kv.strip_prefix("BANDWIDTH="))
                                .and_then(|v| v.parse::<i32>().ok());
                            continue;
                        }
                        if line.is_empty() || line.starts_with('#') || line.contains("://") || line.starts_with('/') {
                            continue;
                        }
                        let Some((name, _)) = line.split_once('/') else { continue };
                        let url = format!("{hls_dir}/{line}");
                        let mut v = self.served("hls_variant", &url, false).await;
                        v.variant = name.to_string();
                        v.serves_prefix = Some(format!("{hls_dir}/{name}/"));
                        v.bandwidth = bandwidth.take();
                        ds.push(v);
                    }
                }
            }
            let poster = if !c.poster_url.is_empty() {
                Some(c.poster_url.clone())
            } else {
                let p = format!("{hls_dir}/poster.jpg");
                if self.exists(&p).await { Some(p) } else { None }
            };
            if let Some(p) = poster {
                ds.push(self.served("poster", &p, false).await);
            }
            if !c.watermarked_url.is_empty() {
                // The legacy transcoder's moving-mark MP4: watermarked.
                ds.push(self.served("download", &c.watermarked_url, true).await);
            }
            return Ok(Some(Plan { kind: Kind::Video, derivatives: ds }));
        }
        if !c.master_url.is_empty() {
            let d = self.served("full", &c.master_url, false).await;
            return Ok(Some(Plan { kind: Kind::Video, derivatives: vec![d] }));
        }
        if !c.audio_url.is_empty() {
            let d = self.served("full", &c.audio_url, false).await;
            return Ok(Some(Plan { kind: Kind::Voice, derivatives: vec![d] }));
        }
        if !c.image_url.is_empty() {
            let stem = image_stem(&c.image_url);
            let mut ds = Vec::new();
            for (role, suffix) in [("thumb", "-thumb.webp"), ("feed", "-feed.webp"), ("full", "-full.webp")] {
                let url = format!("{stem}{suffix}");
                let is_registered = url == c.image_url;
                if is_registered || self.layout.served_url_to_abs(&url).is_some() && self.exists(&url).await {
                    ds.push(self.served(role, &url, false).await);
                }
            }
            if ds.is_empty() {
                // A single file from before derivatives existed.
                ds.push(self.served("full", &c.image_url, false).await);
            }
            return Ok(Some(Plan { kind: Kind::Image, derivatives: ds }));
        }
        Ok(None)
    }
}

async fn insert_derivative_if_absent(
    tx: &mut Transaction<'_, Postgres>,
    asset_id: Uuid,
    d: &NewDerivative,
) -> sqlx::Result<bool> {
    let done = sqlx::query(
        "INSERT INTO derivatives (asset_id, role, variant, store, path, serves_prefix, watermarked,
                                  content_type, width, height, bandwidth, byte_size)
         VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
         ON CONFLICT DO NOTHING",
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
    Ok(done.rows_affected() == 1)
}

fn is_sha256(s: &str) -> bool {
    s.len() == 64 && s.bytes().all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
}

pub async fn run(pool: &PgPool, layout: &Layout) -> Result<()> {
    let feed_url = std::env::var("FEED_DATABASE_URL").context("FEED_DATABASE_URL must be set for import")?;
    let feed = PgPoolOptions::new()
        .max_connections(2)
        .connect(&feed_url)
        .await
        .context("connecting to the feed database")?;

    let canon: Vec<Canon> = sqlx::query_as(
        "SELECT cm.id,
                COALESCE(cm.creator_pial_id, u.pial_id)                 AS pial,
                cm.creator_user_id                                      AS account,
                COALESCE(NULLIF(cm.creator_handle, ''), u.handle, '')   AS handle,
                cm.image_url, cm.master_url, cm.poster_url, cm.audio_url, cm.asset_dir,
                cm.watermarked_url, cm.raw_sha256, cm.width, cm.height,
                cm.created_at, cm.withdrawn_at
           FROM canonical_media cm
           LEFT JOIN users u ON u.id = cm.creator_user_id
          ORDER BY cm.created_at, cm.id",
    )
    .fetch_all(&feed)
    .await
    .context("reading canonical_media")?;

    let mut imp = Importer { layout, report: Report::default() };
    imp.report.canonical_rows = canon.len();

    // Where each legacy object lives, for mapping hashes to assets.
    let mut by_dir: HashMap<String, (Uuid, Uuid)> = HashMap::new();
    let mut by_path: HashMap<String, (Uuid, Uuid)> = HashMap::new();
    let mut by_stem: HashMap<String, (Uuid, Uuid)> = HashMap::new();
    let mut expected_assets: Vec<Uuid> = Vec::new();
    let mut expected_derivatives: Vec<(Uuid, &'static str, String)> = Vec::new();
    let mut hashes: Vec<(String, Uuid, Uuid, DateTime<Utc>)> = Vec::new();

    for c in &canon {
        let Some(pial) = c.pial else {
            imp.report.unattributed.push(c.id);
            continue;
        };
        let Some(plan) = imp.plan(c).await? else {
            imp.report.no_media.push(c.id);
            continue;
        };
        *imp.report.by_kind.entry(plan.kind.as_str()).or_default() += 1;
        expected_assets.push(c.id);

        let withdrawn = c.withdrawn_at.is_some();
        let asset = NewAsset {
            id: c.id,
            uploader_pial: pial,
            uploader_account: c.account,
            uploader_handle: mark_handle(&c.handle),
            kind: plan.kind,
            status: if withdrawn { "withdrawn" } else { "ready" },
            origin: "import",
            raw_sha256: Some(c.raw_sha256.clone()).filter(|s| is_sha256(s)),
            byte_size: None,
            width: (c.width > 0).then_some(c.width),
            height: (c.height > 0).then_some(c.height),
            created_at: Some(c.created_at),
            withdrawn_at: c.withdrawn_at,
            withdrawn_reason: withdrawn.then(|| "withdrawn in feed-engine before caeor held media".to_string()),
            // Imported media was on the platform before the integrity gate
            // existed; it is not put through it here.
            held_for: None,
        };
        let mut tx = pool.begin().await?;
        if assets::insert_asset(&mut tx, &asset, None, true).await?.is_some() {
            imp.report.assets_created += 1;
        }
        for d in &plan.derivatives {
            imp.report.derivatives_planned += 1;
            if insert_derivative_if_absent(&mut tx, c.id, d).await? {
                imp.report.derivatives_created += 1;
            }
            expected_derivatives.push((c.id, d.role, d.variant.clone()));
            by_path.insert(d.path.clone(), (c.id, pial));
            if let Some(p) = &d.serves_prefix {
                if d.role == "hls_master" {
                    let dir = p.trim_end_matches('/');
                    by_dir.insert(dir.to_string(), (c.id, pial));
                    if let Some(parent) = dir.strip_suffix("/clean") {
                        by_dir.insert(parent.to_string(), (c.id, pial));
                    }
                }
            }
        }
        tx.commit().await?;
        if !c.asset_dir.is_empty() {
            by_dir.insert(c.asset_dir.trim_end_matches('/').to_string(), (c.id, pial));
        }
        if !c.image_url.is_empty() {
            by_stem.insert(image_stem(&c.image_url), (c.id, pial));
        }
        if let Some(sha) = &asset.raw_sha256 {
            hashes.push((sha.clone(), c.id, pial, c.created_at));
        }
    }

    // ── raw hashes ───────────────────────────────────────────────────────────
    let canon_hash_count = hashes.len();
    let video_hashes: Vec<(String, DateTime<Utc>, Option<String>)> = sqlx::query_as(
        "SELECT v.sha256, v.created_at, w.video_master_url
           FROM video_raw_hashes v
           LEFT JOIN works w ON w.id::text = v.post_id",
    )
    .fetch_all(&feed)
    .await
    .context("reading video_raw_hashes")?;
    let image_hashes: Vec<(String, DateTime<Utc>, String)> =
        sqlx::query_as("SELECT sha256, created_at, media_url FROM image_raw_hashes")
            .fetch_all(&feed)
            .await
            .context("reading image_raw_hashes")?;
    imp.report.hashes_seen = video_hashes.len() + image_hashes.len();

    for (sha, at, master) in video_hashes {
        let target = master.as_deref().and_then(|m| {
            video_dir(m)
                .and_then(|d| by_dir.get(&d))
                .or_else(|| by_path.get(m))
                .copied()
        });
        match target {
            Some((asset, pial)) if is_sha256(&sha) => hashes.push((sha, asset, pial, at)),
            _ => imp.report.hashes_unmapped.push(sha),
        }
    }
    for (sha, at, url) in image_hashes {
        let target = by_path.get(&url).or_else(|| by_stem.get(&image_stem(&url))).copied();
        match target {
            Some((asset, pial)) if is_sha256(&sha) => hashes.push((sha, asset, pial, at)),
            _ => imp.report.hashes_unmapped.push(sha),
        }
    }
    // First seen wins: the earliest upload of the bytes keeps the credit.
    hashes.sort_by_key(|h| h.3);
    let mut mapped: HashSet<String> = HashSet::new();
    for (sha, asset, pial, at) in &hashes {
        if !mapped.insert(sha.clone()) {
            continue;
        }
        let mut tx = pool.begin().await?;
        if assets::claim_raw_hash(&mut tx, sha, *asset, *pial, Some(*at)).await? {
            imp.report.hashes_created += 1;
        }
        tx.commit().await?;
    }
    imp.report.hashes_mapped = mapped.len();

    // ── verification ─────────────────────────────────────────────────────────
    let present_assets: i64 = sqlx::query_scalar("SELECT COUNT(*) FROM assets WHERE id = ANY($1)")
        .bind(&expected_assets)
        .fetch_one(pool)
        .await?;
    let mut missing_derivatives = 0usize;
    for (asset, role, variant) in &expected_derivatives {
        let ok: bool = sqlx::query_scalar(
            "SELECT EXISTS (SELECT 1 FROM derivatives WHERE asset_id = $1 AND role = $2 AND variant = $3)",
        )
        .bind(asset)
        .bind(role)
        .bind(variant)
        .fetch_one(pool)
        .await?;
        if !ok {
            missing_derivatives += 1;
            imp.report
                .derivative_path_conflicts
                .push(format!("{asset} {role} {variant}"));
        }
    }
    let mapped_list: Vec<String> = mapped.into_iter().collect();
    let present_hashes: i64 = sqlx::query_scalar("SELECT COUNT(*) FROM raw_hashes WHERE sha256 = ANY($1)")
        .bind(&mapped_list)
        .fetch_one(pool)
        .await?;

    let r = &imp.report;
    println!("caeor import");
    println!("  canonical_media rows          {}", r.canonical_rows);
    println!("  imported as assets            {} (created this run: {})", expected_assets.len(), r.assets_created);
    for (k, n) in &r.by_kind {
        println!("    {k:<28}{n}");
    }
    println!("  skipped: no uploader PIAL     {}", r.unattributed.len());
    for id in r.unattributed.iter().take(20) {
        println!("    {id}");
    }
    println!("  skipped: names no media       {}", r.no_media.len());
    println!("  derivatives                   {} planned, {} created this run", r.derivatives_planned, r.derivatives_created);
    println!("  derivatives missing on disk   {}", r.missing_files.len());
    for p in r.missing_files.iter().take(20) {
        println!("    {p}");
    }
    println!("  paths outside /static/media/  {} (not verifiable by caeor)", r.outside_store);
    println!(
        "  raw hashes                    {} from canonical_media + {} from the raw-hash tables; \
         {} distinct mapped, {} created this run, {} unmapped",
        canon_hash_count, r.hashes_seen, r.hashes_mapped, r.hashes_created, r.hashes_unmapped.len()
    );
    println!("  VERIFY assets                 expected {} present {}", expected_assets.len(), present_assets);
    println!("  VERIFY derivatives            expected {} absent {}", expected_derivatives.len(), missing_derivatives);
    for c in r.derivative_path_conflicts.iter().take(20) {
        println!("    absent (path already owned by another asset): {c}");
    }
    println!("  VERIFY raw hashes             expected {} present {}", r.hashes_mapped, present_hashes);

    if present_assets as usize != expected_assets.len() {
        bail!("import verification failed: {} assets expected, {} present", expected_assets.len(), present_assets);
    }
    if present_hashes as usize != r.hashes_mapped {
        bail!("import verification failed: {} raw hashes expected, {} present", r.hashes_mapped, present_hashes);
    }
    if missing_derivatives > 0 {
        bail!(
            "import verification failed: {missing_derivatives} derivatives could not be recorded \
             (their served path is already another asset's); resolve the listed rows in feed-engine \
             and run again"
        );
    }
    println!("  OK");
    Ok(())
}
