//! Video transcoding via ffmpeg subprocess. Produces an HLS ladder
//! (variant playlists + master.m3u8 + poster frame) and uploads everything
//! to the media-derived bucket.
//!
//! Bitrate ladder by source resolution:
//!   ≥ 2160p   → 240, 360, 720, 1080, 2160
//!   ≥ 1080p   → 240, 360, 720, 1080
//!   ≥  720p   → 240, 360, 720
//!   < 720p    → source pass-through at one bitrate
//!
//! H.264 + AAC for maximum compatibility (Safari, iOS, Android, Chrome).
//! HEVC ladder added at T2 once we have GPU nodes.

use std::path::Path;
use std::process::Stdio;

use anyhow::{anyhow, bail, Context, Result};
use serde::{Deserialize, Serialize};
use tokio::process::Command;

use crate::storage::Storage;

#[derive(Debug, Serialize, Deserialize, Clone, Copy)]
pub struct Variant {
    pub name: &'static str,
    pub height: u32,
    pub video_kbps: u32,
    pub audio_kbps: u32,
    pub bandwidth: u32, // for master playlist, video+audio+overhead
}

const VARIANTS: &[Variant] = &[
    Variant { name: "240p",  height: 240,  video_kbps: 400,  audio_kbps: 64,  bandwidth: 500_000 },
    Variant { name: "360p",  height: 360,  video_kbps: 800,  audio_kbps: 96,  bandwidth: 950_000 },
    Variant { name: "720p",  height: 720,  video_kbps: 2800, audio_kbps: 128, bandwidth: 3_100_000 },
    Variant { name: "1080p", height: 1080, video_kbps: 5000, audio_kbps: 128, bandwidth: 5_500_000 },
    Variant { name: "2160p", height: 2160, video_kbps: 16000, audio_kbps: 192, bandwidth: 17_000_000 },
];

#[derive(Debug, Clone, Serialize)]
pub struct TranscodeOutput {
    pub asset_id: String,
    pub master_url: String,
    pub poster_url: String,
    pub variants: Vec<VariantOutput>,
    pub source_width: u32,
    pub source_height: u32,
    pub duration_seconds: f64,
}

#[derive(Debug, Clone, Serialize)]
pub struct VariantOutput {
    pub name: String,
    pub height: u32,
    pub playlist_url: String,
    pub bandwidth: u32,
}

/// Probe a video file with ffprobe for dimensions + duration.
/// We only call ffprobe — never let untrusted ffmpeg flags hit a shell.
pub async fn probe(path: &Path) -> Result<(u32, u32, f64)> {
    let out = Command::new("ffprobe")
        .args([
            "-v", "error",
            "-select_streams", "v:0",
            "-show_entries", "stream=width,height:format=duration",
            "-of", "json",
        ])
        .arg(path)
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .output()
        .await
        .context("ffprobe failed to spawn")?;

    if !out.status.success() {
        bail!("ffprobe exit {}: {}", out.status, String::from_utf8_lossy(&out.stderr));
    }

    let v: serde_json::Value = serde_json::from_slice(&out.stdout)?;
    let stream = v["streams"].get(0).ok_or_else(|| anyhow!("no video stream"))?;
    let width = stream["width"].as_u64().unwrap_or(0) as u32;
    let height = stream["height"].as_u64().unwrap_or(0) as u32;
    let duration_str = v["format"]["duration"].as_str().unwrap_or("0");
    let duration: f64 = duration_str.parse().unwrap_or(0.0);

    Ok((width, height, duration))
}

/// Choose which variants to produce given source height.
fn ladder_for(source_height: u32) -> Vec<Variant> {
    let mut out: Vec<Variant> = VARIANTS
        .iter()
        .filter(|v| v.height <= source_height)
        .copied()
        .collect();
    if out.is_empty() && source_height > 0 {
        // Source smaller than smallest variant — produce a single 240p anyway.
        out.push(VARIANTS[0]);
    }
    out
}

/// Generate a poster frame at 5% into the video.
async fn make_poster(input: &Path, duration: f64, dest: &Path) -> Result<()> {
    let pos = (duration * 0.05).max(0.5);
    let out = Command::new("ffmpeg")
        .args([
            "-y",
            "-ss", &format!("{pos:.2}"),
            "-i",
        ])
        .arg(input)
        .args([
            "-vframes", "1",
            "-q:v", "3",
            "-vf", "scale=-2:720",
        ])
        .arg(dest)
        .stdout(Stdio::null())
        .stderr(Stdio::piped())
        .output()
        .await
        .context("ffmpeg poster failed to spawn")?;
    if !out.status.success() {
        bail!("ffmpeg poster exit {}: {}", out.status, String::from_utf8_lossy(&out.stderr));
    }
    Ok(())
}

/// Transcode one variant into HLS segments under <out_dir>/<variant.name>/.
async fn transcode_variant(input: &Path, out_dir: &Path, variant: Variant) -> Result<()> {
    let dir = out_dir.join(variant.name);
    tokio::fs::create_dir_all(&dir).await?;

    let segment_filename = dir.join("seg_%03d.ts");
    let playlist = dir.join("index.m3u8");

    // Maintain even height; -2:HEIGHT keeps aspect ratio with even width.
    let scale = format!("scale=-2:{}", variant.height);
    let video_kbps = format!("{}k", variant.video_kbps);
    let video_max = format!("{}k", variant.video_kbps + variant.video_kbps / 8);
    let video_buf = format!("{}k", variant.video_kbps * 2);
    let audio_kbps = format!("{}k", variant.audio_kbps);

    let out = Command::new("ffmpeg")
        .args(["-y", "-i"])
        .arg(input)
        .args([
            "-map", "0:v:0",
            "-map", "0:a:0?",            // audio optional (videos with no audio)
            "-c:v", "libx264",
            "-preset", "veryfast",
            "-profile:v", "main",
            "-level", "4.1",
            "-pix_fmt", "yuv420p",
            "-vf", &scale,
            "-b:v", &video_kbps,
            "-maxrate", &video_max,
            "-bufsize", &video_buf,
            "-g", "48",
            "-keyint_min", "48",
            "-sc_threshold", "0",
            "-c:a", "aac",
            "-b:a", &audio_kbps,
            "-ac", "2",
            "-ar", "48000",
            "-f", "hls",
            "-hls_time", "4",
            "-hls_list_size", "0",
            "-hls_playlist_type", "vod",
            "-hls_segment_filename",
        ])
        .arg(&segment_filename)
        .arg(&playlist)
        .stdout(Stdio::null())
        .stderr(Stdio::piped())
        .output()
        .await
        .context("ffmpeg variant failed to spawn")?;

    if !out.status.success() {
        bail!("ffmpeg variant {} exit {}: {}", variant.name, out.status, String::from_utf8_lossy(&out.stderr));
    }

    Ok(())
}

/// Build master.m3u8 referencing each variant's index.m3u8.
fn make_master_playlist(variants: &[Variant]) -> String {
    let mut s = String::from("#EXTM3U\n#EXT-X-VERSION:3\n");
    for v in variants {
        s.push_str(&format!(
            "#EXT-X-STREAM-INF:BANDWIDTH={},RESOLUTION=x{},CODECS=\"avc1.4d401f,mp4a.40.2\",NAME=\"{}\"\n",
            v.bandwidth, v.height, v.name
        ));
        s.push_str(&format!("{}/index.m3u8\n", v.name));
    }
    s
}

/// Upload an HLS variant directory to S3 under <prefix>/<variant>/.
async fn upload_variant_dir(
    storage: &Storage,
    prefix: &str,
    variant: Variant,
    local_dir: &Path,
) -> Result<()> {
    let mut entries = tokio::fs::read_dir(local_dir).await?;
    while let Some(entry) = entries.next_entry().await? {
        let path = entry.path();
        let name = entry.file_name().to_string_lossy().to_string();
        let key = format!("{prefix}/{}/{}", variant.name, name);
        let ct = if name.ends_with(".m3u8") {
            "application/vnd.apple.mpegurl"
        } else {
            "video/mp2t"
        };
        storage
            .put_file(&storage.bucket_derived.clone(), &key, &path, ct)
            .await?;
    }
    Ok(())
}

/// Full pipeline: probe → poster → ladder transcode → upload → master playlist → upload.
pub async fn run(storage: &Storage, asset_id: &str, source: &Path) -> Result<TranscodeOutput> {
    let (width, height, duration) = probe(source).await?;
    if duration <= 0.0 {
        bail!("invalid duration");
    }
    if height < 144 {
        bail!("source height {height}p too small");
    }

    let work = tempfile::tempdir().context("temp work dir")?;
    let work_path = work.path().to_path_buf();

    // 1. Poster — non-fatal; some edge-case sources resist frame extraction.
    let poster_path = work_path.join("poster.jpg");
    let poster_ok = match make_poster(source, duration, &poster_path).await {
        Ok(()) => true,
        Err(e) => { warn!(%asset_id, ?e, "poster extraction failed (non-fatal)"); false }
    };

    // 2. Determine ladder + transcode each variant in parallel (bounded).
    let ladder = ladder_for(height);
    let mut futures: Vec<_> = Vec::new();
    for v in ladder.iter().copied() {
        let src = source.to_path_buf();
        let out = work_path.clone();
        futures.push(tokio::spawn(async move { transcode_variant(&src, &out, v).await }));
    }
    for fut in futures {
        fut.await.context("transcode task panicked")??;
    }

    // 3. Build master playlist
    let master_local = work_path.join("master.m3u8");
    tokio::fs::write(&master_local, make_master_playlist(&ladder)).await?;

    // 4. Upload everything under prefix posts/<asset_id>/
    let prefix = format!("posts/{asset_id}");

    if poster_ok {
        storage
            .put_file(
                &storage.bucket_derived.clone(),
                &format!("{prefix}/poster.jpg"),
                &poster_path,
                "image/jpeg",
            )
            .await?;
    }

    storage
        .put_file(
            &storage.bucket_derived.clone(),
            &format!("{prefix}/master.m3u8"),
            &master_local,
            "application/vnd.apple.mpegurl",
        )
        .await?;

    for v in ladder.iter() {
        let local_dir = work_path.join(v.name);
        upload_variant_dir(storage, &prefix, *v, &local_dir).await?;
    }

    // 5. Build response URLs
    let bucket = &storage.bucket_derived;
    let master_url = storage.public_url(bucket, &format!("{prefix}/master.m3u8"));
    let poster_url = if poster_ok {
        storage.public_url(bucket, &format!("{prefix}/poster.jpg"))
    } else {
        String::new()
    };
    let variants_out: Vec<VariantOutput> = ladder
        .iter()
        .map(|v| VariantOutput {
            name: v.name.into(),
            height: v.height,
            playlist_url: storage.public_url(bucket, &format!("{prefix}/{}/index.m3u8", v.name)),
            bandwidth: v.bandwidth,
        })
        .collect();

    Ok(TranscodeOutput {
        asset_id: asset_id.into(),
        master_url,
        poster_url,
        variants: variants_out,
        source_width: width,
        source_height: height,
        duration_seconds: duration,
    })
}
