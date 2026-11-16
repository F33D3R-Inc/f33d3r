//! `caeor remark` — watermarks imported media in place.
//!
//! Media from before caeor was published clean. Every viewer-facing copy must
//! carry the mark, and every published path must keep working, so remark
//! rewrites each unmarked served file at its own path:
//!
//!   image  — each unmarked thumb/feed/full is marked in its own format and
//!            atomically replaced. The largest clean file is first copied to
//!            the private store as the asset's source_private.
//!   video, HLS — a `remark` job re-ladders the asset into its existing HLS
//!            directory, replacing it whole and atomically. The source is the
//!            legacy watermarked MP4 when there is one (renditions inherit its
//!            moving mark), otherwise the clean ladder itself with the mark
//!            drawn, plus a new download file in the private store.
//!   video, single file — the clean file is copied to the private store as
//!            source_private, then a job burns the moving mark into the file
//!            at its own path.
//!
//! Remark only queues and writes; the running caeor's reconciler executes the
//! video jobs. It is idempotent: marked derivatives are skipped, and an asset
//! with an open job is not queued twice.

use anyhow::{Context, Result};
use image::ImageFormat;
use sqlx::PgPool;
use uuid::Uuid;

use crate::assets::{self, DerivativeRow, NewDerivative};
use crate::ids::mark_text;
use crate::imaging;
use crate::jobs::{self, JobRequest, MarkSpec};
use crate::layout::{Layout, Location, SERVED_URL_PREFIX};
use crate::mark::{self, ImageMark};

#[derive(Default)]
struct Tally {
    images_marked: usize,
    files_marked: usize,
    video_jobs: usize,
    already_queued: usize,
    unreachable: Vec<String>,
    failed: Vec<String>,
}

fn served_rel(url: &str) -> Option<String> {
    url.strip_prefix(SERVED_URL_PREFIX).map(str::to_string)
}

pub async fn run(pool: &PgPool, layout: &Layout) -> Result<()> {
    let ids: Vec<Uuid> = sqlx::query_scalar(
        "SELECT a.id FROM assets a
          WHERE a.status = 'ready' AND a.kind IN ('image','video','clip')
            AND EXISTS (SELECT 1 FROM derivatives d
                         WHERE d.asset_id = a.id AND d.store = 'served' AND NOT d.watermarked
                           AND d.removed_at IS NULL AND d.role <> 'download')
          ORDER BY a.created_at",
    )
    .fetch_all(pool)
    .await
    .context("listing unmarked assets")?;

    let mut t = Tally::default();
    for id in ids {
        let Some(a) = assets::get_asset(pool, id).await? else { continue };
        let ds = assets::get_derivatives(pool, id).await?;
        let res = if a.kind == "image" {
            remark_image(pool, layout, &a, &ds, &mut t).await
        } else {
            remark_video(pool, layout, &a, &ds, &mut t).await
        };
        if let Err(e) = res {
            let msg = format!("{id}: {e:#}");
            tracing::error!(error = %msg, "remark failed for an asset");
            t.failed.push(msg);
        }
    }

    println!("caeor remark");
    println!("  images marked in place        {} ({} files)", t.images_marked, t.files_marked);
    println!("  video jobs queued             {}", t.video_jobs);
    println!("  video already queued          {}", t.already_queued);
    println!("  unreachable (outside store)   {}", t.unreachable.len());
    for u in t.unreachable.iter().take(20) {
        println!("    {u}");
    }
    println!("  failed                        {}", t.failed.len());
    for f in t.failed.iter().take(20) {
        println!("    {f}");
    }
    if !t.failed.is_empty() {
        anyhow::bail!("{} assets could not be re-marked; see above, fix, and run again", t.failed.len());
    }
    Ok(())
}

/// Copies a clean served file to the private store as the asset's source,
/// once. Returns the private location.
async fn keep_source(
    pool: &PgPool,
    layout: &Layout,
    asset: Uuid,
    ds: &[DerivativeRow],
    clean_url: &str,
) -> Result<Location> {
    if let Some(d) = ds.iter().find(|d| d.role == "source_private") {
        return Ok(Location::private(d.path.clone()));
    }
    let src = layout
        .served_url_to_abs(clean_url)
        .context("source is outside the served store")?;
    let ext = src
        .extension()
        .and_then(|e| e.to_str())
        .filter(|e| e.chars().all(|c| c.is_ascii_alphanumeric()))
        .unwrap_or("bin")
        .to_ascii_lowercase();
    let loc = Layout::source(asset, &ext);
    let dest = layout.abs(&loc)?;
    if let Some(parent) = dest.parent() {
        tokio::fs::create_dir_all(parent).await?;
    }
    tokio::fs::copy(&src, &dest)
        .await
        .with_context(|| format!("copying {} to the private store", src.display()))?;
    let size = tokio::fs::metadata(&dest).await?.len();
    let ctype = ds
        .iter()
        .find(|d| d.path == clean_url)
        .map(|d| d.content_type.clone())
        .unwrap_or_else(|| "application/octet-stream".into());
    let mut tx = pool.begin().await?;
    assets::upsert_derivative(&mut tx, asset, &NewDerivative::private_file("source_private", &loc, false, &ctype).size(size))
        .await?;
    tx.commit().await?;
    Ok(loc)
}

async fn remark_image(
    pool: &PgPool,
    layout: &Layout,
    a: &assets::AssetRow,
    ds: &[DerivativeRow],
    t: &mut Tally,
) -> Result<()> {
    let unmarked: Vec<&DerivativeRow> = ds
        .iter()
        .filter(|d| d.is_served() && !d.watermarked && d.removed_at.is_none())
        .collect();
    // The largest clean rendition is the best source there is.
    let best = ["full", "feed", "thumb"]
        .iter()
        .find_map(|r| unmarked.iter().find(|d| d.role == *r))
        .copied();
    let Some(best) = best else { return Ok(()) };
    if layout.served_url_to_abs(&best.path).is_none() {
        t.unreachable.push(format!("{} {}", a.id, best.path));
        return Ok(());
    }
    keep_source(pool, layout, a.id, ds, &best.path).await?;

    let text = mark_text(&a.uploader_handle, &a.short_id);
    let scratch = layout.scratch_dir();
    for d in unmarked {
        let Some(abs) = layout.served_url_to_abs(&d.path) else {
            t.unreachable.push(format!("{} {}", a.id, d.path));
            continue;
        };
        let bytes = tokio::fs::read(&abs)
            .await
            .with_context(|| format!("reading {}", abs.display()))?;
        let (img, format) = tokio::task::spawn_blocking(move || imaging::decode(&bytes))
            .await
            .context("decode task")??;
        // Written back in the format its extension promises.
        let out_format = ImageFormat::from_path(&abs).unwrap_or(format);
        let style = if d.role == "thumb" { ImageMark::Corner } else { ImageMark::Content };
        let marked = mark::mark_image(&scratch, &img, style, &text, &a.short_id).await?;
        let size = imaging::write_atomic(marked, out_format, &abs).await?;
        sqlx::query(
            "UPDATE derivatives SET watermarked = TRUE, byte_size = $4, updated_at = NOW()
              WHERE asset_id = $1 AND role = $2 AND variant = $3",
        )
        .bind(a.id)
        .bind(&d.role)
        .bind(&d.variant)
        .bind(size as i64)
        .execute(pool)
        .await?;
        t.files_marked += 1;
    }
    t.images_marked += 1;
    Ok(())
}

async fn remark_video(
    pool: &PgPool,
    layout: &Layout,
    a: &assets::AssetRow,
    ds: &[DerivativeRow],
    t: &mut Tally,
) -> Result<()> {
    let open: bool = sqlx::query_scalar(
        "SELECT EXISTS (SELECT 1 FROM upload_jobs WHERE asset_id = $1
                         AND state IN ('queued','dispatched','cancelling'))",
    )
    .bind(a.id)
    .fetch_one(pool)
    .await?;
    if open {
        t.already_queued += 1;
        return Ok(());
    }

    let master = ds.iter().find(|d| d.role == "hls_master" && d.removed_at.is_none());
    let req = if let Some(master) = master {
        let Some(hls_rel) = master
            .serves_prefix
            .as_deref()
            .and_then(|p| served_rel(p.trim_end_matches('/')))
        else {
            t.unreachable.push(format!("{} {}", a.id, master.path));
            return Ok(());
        };
        let marked_download = ds
            .iter()
            .find(|d| d.role == "download" && d.watermarked && d.removed_at.is_none() && d.is_served())
            .and_then(|d| served_rel(&d.path));
        let poster = ds
            .iter()
            .find(|d| d.role == "poster" && d.removed_at.is_none())
            .and_then(|d| served_rel(&d.path))
            .unwrap_or_else(|| format!("{hls_rel}/poster.jpg"));
        let (source, source_marked, download) = match marked_download {
            Some(rel) => (Location::served(rel), true, None),
            None => {
                // No marked file exists: encode from the clean ladder's best
                // rendition. Its own playlist, not the master: a master read
                // as input offers its lowest rendition's stream first.
                let top = ds
                    .iter()
                    .filter(|d| d.role == "hls_variant" && d.removed_at.is_none())
                    .max_by_key(|d| d.height.unwrap_or(0).max(d.bandwidth.unwrap_or(0) / 10_000))
                    .unwrap_or(master);
                let src_rel = served_rel(&top.path).context("ladder outside the served store")?;
                (Location::served(src_rel), false, Some(Layout::download(a.id)))
            }
        };
        JobRequest {
            job_id: Uuid::new_v4(),
            asset_id: a.id,
            source,
            clip: None,
            mark: MarkSpec { handle: a.uploader_handle.clone(), short_id: a.short_id.clone(), source_marked },
            hls: Some(Location::served(hls_rel)),
            poster: Some(Location::served(poster)),
            download,
        }
    } else {
        let Some(full) = ds.iter().find(|d| d.role == "full" && !d.watermarked && d.removed_at.is_none()) else {
            return Ok(());
        };
        let Some(rel) = served_rel(&full.path) else {
            t.unreachable.push(format!("{} {}", a.id, full.path));
            return Ok(());
        };
        // The marked file is H.264/AAC in an MP4 container, written at the
        // file's own path: only paths whose extension promises that may take it.
        let ext = rel.rsplit('.').next().unwrap_or("").to_ascii_lowercase();
        if !matches!(ext.as_str(), "mp4" | "m4v" | "mov") {
            t.unreachable.push(format!(
                "{} {} (a .{ext} path cannot hold the marked MP4; it stays refused by the read path)",
                a.id, full.path
            ));
            return Ok(());
        }
        let source = keep_source(pool, layout, a.id, ds, &full.path).await?;
        JobRequest {
            job_id: Uuid::new_v4(),
            asset_id: a.id,
            source,
            clip: None,
            mark: MarkSpec { handle: a.uploader_handle.clone(), short_id: a.short_id.clone(), source_marked: false },
            hls: None,
            poster: None,
            download: Some(Location::served(rel)),
        }
    };
    let mut tx = pool.begin().await?;
    jobs::enqueue(&mut tx, "remark", &req).await?;
    tx.commit().await?;
    t.video_jobs += 1;
    Ok(())
}
