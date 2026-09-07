//! HLS transcoding pipeline via ffmpeg subprocess.
//!
//! Produces an HLS ladder (variant playlists + master.m3u8 + poster frame)
//! for clean in-app playback, plus a single-file watermarked MP4 for downloads.
//!
//! Output layout under `{media_dir}/posts/{asset_id}/`:
//!   clean/           — HLS ladder (master.m3u8 + variant dirs)
//!   clean/poster.jpg
//!   watermarked.mp4  — single H.264+AAC MP4, moving Lissajous watermark burned in
//!
//! Bitrate ladder by source resolution:
//!   ≥ 2160p   → 240, 360, 720, 1080, 2160
//!   ≥ 1080p   → 240, 360, 720, 1080
//!   ≥  720p   → 240, 360, 720
//!   < 720p    → source pass-through at 240p
//!
//! H.264 + AAC for maximum compatibility (Safari, iOS, Android, Chrome).

use std::path::{Path, PathBuf};
use std::process::Stdio;

use anyhow::{anyhow, bail, Context, Result};
use serde::{Deserialize, Serialize};
use tokio::process::Command;
use tracing::warn;

/// Returns true when NVENC_ENABLED=true is set in the environment.
fn nvenc_enabled() -> bool {
    std::env::var("NVENC_ENABLED").as_deref() == Ok("true")
}

/// Strip characters that would break ffmpeg drawtext filter syntax.
/// Allows alphanumerics, underscore, dot, and hyphen only.
fn sanitize_for_drawtext(s: &str) -> String {
    s.chars()
        .filter(|c| c.is_alphanumeric() || *c == '_' || *c == '.' || *c == '-')
        .collect()
}

#[derive(Debug, Serialize, Deserialize, Clone, Copy)]
pub struct Variant {
    pub name: &'static str,
    pub height: u32,
    pub video_kbps: u32,
    pub audio_kbps: u32,
    pub bandwidth: u32,
}

const VARIANTS: &[Variant] = &[
    Variant {
        name: "240p",
        height: 240,
        video_kbps: 400,
        audio_kbps: 64,
        bandwidth: 500_000,
    },
    Variant {
        name: "360p",
        height: 360,
        video_kbps: 800,
        audio_kbps: 96,
        bandwidth: 950_000,
    },
    Variant {
        name: "720p",
        height: 720,
        video_kbps: 2800,
        audio_kbps: 128,
        bandwidth: 3_100_000,
    },
    Variant {
        name: "1080p",
        height: 1080,
        video_kbps: 5000,
        audio_kbps: 128,
        bandwidth: 5_500_000,
    },
    Variant {
        name: "2160p",
        height: 2160,
        video_kbps: 16000,
        audio_kbps: 192,
        bandwidth: 17_000_000,
    },
];

#[derive(Debug, Clone, Serialize)]
pub struct TranscodeOutput {
    pub asset_id: String,
    /// Clean HLS stream (no watermark) — served to the authenticated in-app player.
    pub master_url: String,
    /// Watermarked MP4 (single file, H.264+AAC) — served for downloads.
    /// Lissajous moving watermark with creator handle is burned into pixel data.
    /// Survives re-encoding, screen recording, and social platform reposting.
    pub watermarked_mp4_url: String,
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

pub async fn probe(path: &Path) -> Result<(u32, u32, f64)> {
    let out = Command::new("ffprobe")
        .args([
            "-v",
            "error",
            "-select_streams",
            "v:0",
            "-show_entries",
            "stream=width,height:format=duration",
            "-of",
            "json",
        ])
        .arg(path)
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .output()
        .await
        .context("ffprobe failed to spawn")?;

    if !out.status.success() {
        bail!(
            "ffprobe exit {}: {}",
            out.status,
            String::from_utf8_lossy(&out.stderr)
        );
    }

    let v: serde_json::Value = serde_json::from_slice(&out.stdout)?;
    let stream = v["streams"]
        .get(0)
        .ok_or_else(|| anyhow!("no video stream"))?;
    let width = stream["width"].as_u64().unwrap_or(0) as u32;
    let height = stream["height"].as_u64().unwrap_or(0) as u32;
    let duration_str = v["format"]["duration"].as_str().unwrap_or("0");
    let duration: f64 = duration_str.parse().unwrap_or(0.0);
    Ok((width, height, duration))
}

fn ladder_for(source_height: u32) -> Vec<Variant> {
    let mut out: Vec<Variant> = VARIANTS
        .iter()
        .filter(|v| v.height <= source_height)
        .copied()
        .collect();
    if out.is_empty() && source_height > 0 {
        out.push(VARIANTS[0]);
    }
    out
}

async fn make_poster(input: &Path, duration: f64, dest: &Path) -> Result<()> {
    let pos = (duration * 0.05).max(0.5);
    let out = Command::new("ffmpeg")
        .args(["-y", "-ss", &format!("{pos:.2}"), "-i"])
        .arg(input)
        .args(["-vframes", "1", "-q:v", "3", "-vf", "scale=-2:720"])
        .arg(dest)
        .stdout(Stdio::null())
        .stderr(Stdio::piped())
        .output()
        .await
        .context("ffmpeg poster failed to spawn")?;
    if !out.status.success() {
        bail!(
            "ffmpeg poster exit {}: {}",
            out.status,
            String::from_utf8_lossy(&out.stderr)
        );
    }
    Ok(())
}

/// Transcode one HLS variant — no watermark (clean pass only).
async fn transcode_variant(input: &Path, out_dir: &Path, variant: Variant) -> Result<()> {
    let dir = out_dir.join(variant.name);
    tokio::fs::create_dir_all(&dir).await?;

    let segment_filename = dir.join("seg_%03d.ts");
    let playlist = dir.join("index.m3u8");
    let scale = format!("scale=-2:{}", variant.height);

    let video_kbps = format!("{}k", variant.video_kbps);
    let video_max = format!("{}k", variant.video_kbps + variant.video_kbps / 8);
    let video_buf = format!("{}k", variant.video_kbps * 2);
    let audio_kbps = format!("{}k", variant.audio_kbps);

    let mut cmd = Command::new("ffmpeg");
    cmd.args(["-y", "-i"]).arg(input);
    cmd.args(["-map", "0:v:0", "-map", "0:a:0?"]);
    if nvenc_enabled() {
        cmd.args([
            "-c:v",
            "h264_nvenc",
            "-preset",
            "p4",
            "-profile:v",
            "high",
            "-pix_fmt",
            "yuv420p",
        ]);
    } else {
        cmd.args([
            "-c:v",
            "libx264",
            "-preset",
            "veryfast",
            "-profile:v",
            "main",
            "-level",
            "4.1",
            "-pix_fmt",
            "yuv420p",
        ]);
    }
    cmd.args([
        "-vf",
        &scale,
        "-b:v",
        &video_kbps,
        "-maxrate",
        &video_max,
        "-bufsize",
        &video_buf,
    ]);
    cmd.args(["-g", "48", "-keyint_min", "48"]);
    if !nvenc_enabled() {
        cmd.args(["-sc_threshold", "0"]);
    }
    cmd.args([
        "-c:a",
        "aac",
        "-b:a",
        &audio_kbps,
        "-ac",
        "2",
        "-ar",
        "48000",
    ]);
    cmd.args([
        "-f",
        "hls",
        "-hls_time",
        "4",
        "-hls_list_size",
        "0",
        "-hls_playlist_type",
        "vod",
        "-hls_segment_filename",
    ]);
    cmd.arg(&segment_filename).arg(&playlist);
    let out = cmd
        .stdout(Stdio::null())
        .stderr(Stdio::piped())
        .output()
        .await
        .context("ffmpeg variant failed to spawn")?;

    if !out.status.success() {
        bail!(
            "ffmpeg variant {} exit {}: {}",
            variant.name,
            out.status,
            String::from_utf8_lossy(&out.stderr)
        );
    }
    Ok(())
}

fn make_master_playlist(variants: &[Variant], src_width: u32, src_height: u32) -> String {
    let mut s = String::from("#EXTM3U\n#EXT-X-VERSION:3\n");
    for v in variants {
        // Compute scaled width preserving aspect ratio, rounded to nearest even number.
        // Matches FFmpeg scale=-2:{height} behaviour.
        let vw = if src_height > 0 {
            let w = (src_width as f64 / src_height as f64 * v.height as f64).round() as u32;
            ((w + 1) / 2) * 2 // round up to nearest even
        } else {
            (v.height * 16 / 9 / 2) * 2 // 16:9 fallback, even
        };
        s.push_str(&format!(
            "#EXT-X-STREAM-INF:BANDWIDTH={},RESOLUTION={}x{},CODECS=\"avc1.4d401f,mp4a.40.2\",NAME=\"{}\"\n",
            v.bandwidth, vw, v.height, v.name
        ));
        s.push_str(&format!("{}/index.m3u8\n", v.name));
    }
    s
}

/// Run the clean HLS pass: all ladder variants, no watermark.
async fn run_clean_pass(
    source: &Path,
    dest_dir: &Path,
    ladder: &[Variant],
    width: u32,
    height: u32,
) -> Result<()> {
    let work = tempfile::tempdir().context("temp work dir")?;
    let work_path = work.path().to_path_buf();

    let mut futures = Vec::new();
    for v in ladder.iter().copied() {
        let src = source.to_path_buf();
        let out = work_path.clone();
        futures.push(tokio::spawn(async move {
            transcode_variant(&src, &out, v).await
        }));
    }
    for fut in futures {
        fut.await.context("transcode task panicked")??;
    }

    let master_local = work_path.join("master.m3u8");
    tokio::fs::write(&master_local, make_master_playlist(ladder, width, height)).await?;

    tokio::fs::create_dir_all(dest_dir).await?;
    tokio::fs::copy(&master_local, dest_dir.join("master.m3u8")).await?;

    for v in ladder {
        let src_dir = work_path.join(v.name);
        let dst_dir = dest_dir.join(v.name);
        tokio::fs::create_dir_all(&dst_dir).await?;
        let mut entries = tokio::fs::read_dir(&src_dir).await?;
        while let Some(entry) = entries.next_entry().await? {
            tokio::fs::copy(entry.path(), dst_dir.join(entry.file_name())).await?;
        }
    }

    Ok(())
}

/// Burn a single-file watermarked MP4 using the highest-quality ladder variant.
///
/// The watermark is two drawtext layers stacked:
///   Line 1: @{creator_handle}
///   Line 2: f33d3r.com
///
/// Both lines drift together on a Lissajous (figure-8) path so they visit every
/// quadrant of the frame. The path uses two different sinusoidal periods
/// (20 s horizontal, 10 s vertical), guaranteeing the watermark cannot be
/// removed by static cropping. Font size scales with resolution (2.5% of height).
///
/// Drop shadow (shadowx/shadowy) provides readability on any background.
/// Opacity 0.6 (alpha channel) per spec.
///
/// DejaVu Sans is installed in the Docker image alongside ffmpeg.
/// fontfile must be an absolute path for ffmpeg's drawtext filter.
async fn burn_watermarked_mp4(
    input: &Path,
    top_variant: Variant,
    creator_handle: &str,
    dest: &Path,
) -> Result<()> {
    // Sanitize handle — drawtext uses single quotes; strip anything that could
    // escape the expression or inject ffmpeg filter syntax.
    let safe_handle = sanitize_for_drawtext(creator_handle);
    if safe_handle.is_empty() {
        // No handle: produce a clean copy at top quality.
        let scale_vf = format!("scale=-2:{}", top_variant.height);
        let mut cmd = Command::new("ffmpeg");
        cmd.args(["-y", "-i"]).arg(input);
        cmd.args(["-map", "0:v:0", "-map", "0:a:0?"]);
        if nvenc_enabled() {
            cmd.args([
                "-c:v",
                "h264_nvenc",
                "-preset",
                "p4",
                "-profile:v",
                "high",
                "-pix_fmt",
                "yuv420p",
            ]);
        } else {
            cmd.args([
                "-c:v",
                "libx264",
                "-preset",
                "veryfast",
                "-profile:v",
                "main",
                "-level",
                "4.1",
                "-pix_fmt",
                "yuv420p",
            ]);
        }
        cmd.args(["-vf", &scale_vf]);
        cmd.args(["-b:v", &format!("{}k", top_variant.video_kbps)]);
        cmd.args([
            "-maxrate",
            &format!("{}k", top_variant.video_kbps + top_variant.video_kbps / 8),
        ]);
        cmd.args(["-bufsize", &format!("{}k", top_variant.video_kbps * 2)]);
        cmd.args([
            "-c:a",
            "aac",
            "-b:a",
            &format!("{}k", top_variant.audio_kbps),
            "-ac",
            "2",
            "-ar",
            "48000",
        ]);
        cmd.args(["-movflags", "+faststart", "-f", "mp4"]).arg(dest);
        let out = cmd
            .stdout(Stdio::null())
            .stderr(Stdio::piped())
            .output()
            .await
            .context("ffmpeg watermarked mp4 (no handle) failed to spawn")?;
        if !out.status.success() {
            bail!(
                "ffmpeg watermarked mp4 exit {}: {}",
                out.status,
                String::from_utf8_lossy(&out.stderr)
            );
        }
        return Ok(());
    }

    // DejaVu Sans is available at this path in Debian after installing fonts-dejavu-core.
    let fontfile = "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf";

    // Lissajous position expressions:
    //   x: (w-tw)/2 + (w/3)*sin(2*PI*t/20)   — horizontal drift, period 20 s
    //   y: (h-th)/2 + (h/4)*sin(2*PI*t/10)   — vertical drift, period 10 s
    // The different periods produce a figure-8 / Lissajous path that visits
    // all four quadrants, making static cropping ineffective as removal.
    //
    // Font size = 2.5% of video height (scales with resolution).
    // Opacity: alpha=0.6 (white@0.6). Drop shadow: shadowx=2, shadowy=2.
    //
    // Two drawtext filters are chained: line 1 (@handle) and line 2 (f33d3r.com).
    // Line 2 is offset downward by fontsize+4 pixels so the two lines travel together.

    let line1 = format!(
        "drawtext=\
        fontfile={fontfile}:\
        text='@{handle}':\
        fontcolor=white@0.6:\
        fontsize='h*0.025':\
        shadowx=2:shadowy=2:shadowcolor=black@0.8:\
        x='(w-tw)/2+(w/3)*sin(2*3.14159265*t/20)':\
        y='(h-th)/2+(h/4)*sin(2*3.14159265*t/10)'",
        fontfile = fontfile,
        handle = safe_handle,
    );

    let line2 = format!(
        "drawtext=\
        fontfile={fontfile}:\
        text='f33d3r.com':\
        fontcolor=white@0.6:\
        fontsize='h*0.025':\
        shadowx=2:shadowy=2:shadowcolor=black@0.8:\
        x='(w-tw)/2+(w/3)*sin(2*3.14159265*t/20)':\
        y='(h-th)/2+(h/4)*sin(2*3.14159265*t/10)+h*0.025+4'",
        fontfile = fontfile,
    );

    let scale = format!("scale=-2:{}", top_variant.height);
    let vf_filter = format!("{scale},{line1},{line2}");

    let mut cmd = Command::new("ffmpeg");
    cmd.args(["-y", "-i"]).arg(input);
    cmd.args(["-map", "0:v:0", "-map", "0:a:0?"]);
    if nvenc_enabled() {
        cmd.args([
            "-c:v",
            "h264_nvenc",
            "-preset",
            "p4",
            "-profile:v",
            "high",
            "-pix_fmt",
            "yuv420p",
        ]);
    } else {
        cmd.args([
            "-c:v",
            "libx264",
            "-preset",
            "veryfast",
            "-profile:v",
            "main",
            "-level",
            "4.1",
            "-pix_fmt",
            "yuv420p",
        ]);
    }
    cmd.args(["-vf", &vf_filter]);
    cmd.args(["-b:v", &format!("{}k", top_variant.video_kbps)]);
    cmd.args([
        "-maxrate",
        &format!("{}k", top_variant.video_kbps + top_variant.video_kbps / 8),
    ]);
    cmd.args(["-bufsize", &format!("{}k", top_variant.video_kbps * 2)]);
    cmd.args([
        "-c:a",
        "aac",
        "-b:a",
        &format!("{}k", top_variant.audio_kbps),
        "-ac",
        "2",
        "-ar",
        "48000",
    ]);
    cmd.args(["-movflags", "+faststart", "-f", "mp4"]).arg(dest);
    let out = cmd
        .stdout(Stdio::null())
        .stderr(Stdio::piped())
        .output()
        .await
        .context("ffmpeg watermarked mp4 failed to spawn")?;

    if !out.status.success() {
        bail!(
            "ffmpeg watermarked mp4 exit {}: {}",
            out.status,
            String::from_utf8_lossy(&out.stderr)
        );
    }
    Ok(())
}

/// Full pipeline: probe → poster → clean HLS pass → watermarked MP4 → write to media filesystem.
///
/// Pass 1 (clean HLS): no watermark → `posts/{asset_id}/clean/` — served to the in-app player.
/// Pass 2 (watermarked MP4): Lissajous drawtext → `posts/{asset_id}/watermarked.mp4` — served
///   as the download file. The creator handle is burned into the pixel data at highest resolution.
///
/// `creator_handle` is burned into the watermarked pass via moving ffmpeg drawtext.
/// Pass an empty string to skip watermarking (watermarked.mp4 will be a clean copy).
pub async fn run(
    media_dir: &str,
    asset_id: &str,
    source: &Path,
    creator_handle: &str,
) -> Result<TranscodeOutput> {
    let (width, height, duration) = probe(source).await?;
    if duration <= 0.0 {
        bail!("invalid duration");
    }
    if height < 144 {
        bail!("source height {height}p too small");
    }

    let ladder = ladder_for(height);
    let base_dir = PathBuf::from(media_dir).join("posts").join(asset_id);
    tokio::fs::create_dir_all(&base_dir).await?;

    // 1. Poster frame — use the source directly (non-fatal)
    let poster_tmp = tempfile::Builder::new()
        .suffix(".jpg")
        .tempfile()
        .context("poster tempfile")?;
    let poster_path = poster_tmp.path().to_path_buf();
    let poster_ok = match make_poster(source, duration, &poster_path).await {
        Ok(()) => true,
        Err(e) => {
            warn!(%asset_id, ?e, "poster extraction failed (non-fatal)");
            false
        }
    };

    // 2. Clean HLS pass — all ladder variants, no watermark.
    let clean_dir = base_dir.join("clean");
    run_clean_pass(source, &clean_dir, &ladder, width, height)
        .await
        .context("clean transcode pass failed")?;

    // 3. Watermarked MP4 — single file at highest-quality variant.
    //    Lives alongside the clean/ directory as watermarked.mp4.
    let wm_mp4_path = base_dir.join("watermarked.mp4");
    let top_variant = *ladder
        .last()
        .expect("ladder always has at least one variant");
    burn_watermarked_mp4(source, top_variant, creator_handle, &wm_mp4_path)
        .await
        .context("watermarked mp4 burn failed")?;

    // 4. Copy poster into the clean directory.
    if poster_ok {
        tokio::fs::copy(&poster_path, clean_dir.join("poster.jpg")).await?;
    }

    // 5. Build public URLs (/static/media/... served by feed-engine).
    let clean_base = format!("/static/media/posts/{asset_id}/clean");
    let poster_url = if poster_ok {
        format!("{clean_base}/poster.jpg")
    } else {
        String::new()
    };
    let variants_out = ladder
        .iter()
        .map(|v| VariantOutput {
            name: v.name.into(),
            height: v.height,
            playlist_url: format!("{clean_base}/{}/index.m3u8", v.name),
            bandwidth: v.bandwidth,
        })
        .collect();

    Ok(TranscodeOutput {
        asset_id: asset_id.into(),
        master_url: format!("{clean_base}/master.m3u8"),
        watermarked_mp4_url: format!("/static/media/posts/{asset_id}/watermarked.mp4"),
        poster_url,
        variants: variants_out,
        source_width: width,
        source_height: height,
        duration_seconds: duration,
    })
}
