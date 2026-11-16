//! The video pipeline: one job from caeor in, marked outputs out.
//!
//! Every viewer-facing output carries the mark. There is no clean ladder:
//!
//!   HLS ladder — every rendition has the moving watermark drawn into its
//!                pixels: `@handle • f33d3r.com • <short_id>` drifting on a
//!                Lissajous path (20 s horizontal, 10 s vertical) so it visits
//!                every part of the frame and no static crop removes it.
//!                Screen-recording a stream records the mark.
//!   poster     — a static mark in the corner.
//!   download   — a single H.264/AAC MP4 at the top rendition, same moving
//!                mark. Written where caeor says: the private store for new
//!                media (reached only through caeor's download endpoint), or
//!                a legacy file's own served path when caeor re-marks it.
//!
//! When the source already carries the moving mark (a legacy watermarked
//! download being re-laddered), renditions inherit it rather than getting a
//! second one drawn over it.
//!
//! The clean source stays where caeor put it, in the private store, for
//! future re-encodes. Nothing here copies it anywhere served.
//!
//! Every output is written to a staging name and moved into place only after
//! every encode has succeeded, so a reader sees the old outputs or the new
//! ones and a failed or interrupted job leaves the served tree untouched. A
//! re-run of the same job starts by clearing its own staging.
//!
//! Ladder by source height:
//!   ≥ 2160p → 240, 360, 720, 1080, 2160
//!   ≥ 1080p → 240, 360, 720, 1080
//!   ≥  720p → 240, 360, 720
//!   <  720p → 240 (and every rung at or below the source)

use std::path::{Path, PathBuf};
use std::process::Stdio;

use anyhow::{anyhow, bail, Context, Result};
use tokio::process::Command;
use uuid::Uuid;

use crate::job::{JobOutput, JobRequest, RenditionOut, Roots};

const FONT: &str = "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf";
const AUDIOGRAM_FONT: &str = "/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf";

fn nvenc_enabled() -> bool {
    std::env::var("NVENC_ENABLED").as_deref() == Ok("true")
}

/// Characters a handle may contain and nothing that could be read as filter
/// syntax. Caeor sends handles already reduced to this; this is the check at
/// the boundary.
fn sanitize_for_drawtext(s: &str) -> String {
    s.chars()
        .filter(|c| c.is_ascii_alphanumeric() || *c == '_' || *c == '.' || *c == '-')
        .collect()
}

#[derive(Debug, Clone, Copy)]
struct Variant {
    name: &'static str,
    height: u32,
    video_kbps: u32,
    audio_kbps: u32,
    bandwidth: u32,
}

const VARIANTS: &[Variant] = &[
    Variant { name: "240p", height: 240, video_kbps: 400, audio_kbps: 64, bandwidth: 500_000 },
    Variant { name: "360p", height: 360, video_kbps: 800, audio_kbps: 96, bandwidth: 950_000 },
    Variant { name: "720p", height: 720, video_kbps: 2800, audio_kbps: 128, bandwidth: 3_100_000 },
    Variant { name: "1080p", height: 1080, video_kbps: 5000, audio_kbps: 128, bandwidth: 5_500_000 },
    Variant { name: "2160p", height: 2160, video_kbps: 16000, audio_kbps: 192, bandwidth: 17_000_000 },
];

fn ladder_for(source_height: u32) -> Vec<Variant> {
    let mut out: Vec<Variant> = VARIANTS.iter().filter(|v| v.height <= source_height).copied().collect();
    if out.is_empty() && source_height > 0 {
        out.push(VARIANTS[0]);
    }
    out
}

/// Scaled width for a rendition, matching ffmpeg's scale=-2:{height}.
fn width_for(src_w: u32, src_h: u32, height: u32) -> u32 {
    if src_h == 0 {
        return (height * 16 / 9 / 2) * 2;
    }
    let w = (src_w as f64 / src_h as f64 * height as f64).round() as u32;
    w.div_ceil(2) * 2
}

async fn run_ffmpeg(mut cmd: Command, what: &str) -> Result<()> {
    let out = cmd
        .stdout(Stdio::null())
        .stderr(Stdio::piped())
        .kill_on_drop(true)
        .output()
        .await
        .with_context(|| format!("ffmpeg {what} failed to spawn"))?;
    if !out.status.success() {
        bail!("ffmpeg {what} exit {}: {}", out.status, String::from_utf8_lossy(&out.stderr));
    }
    Ok(())
}

pub async fn probe(path: &Path) -> Result<(u32, u32, f64)> {
    let out = Command::new("ffprobe")
        .args(["-v", "error", "-select_streams", "v:0"])
        .args(["-show_entries", "stream=width,height:format=duration", "-of", "json"])
        .arg(path)
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .kill_on_drop(true)
        .output()
        .await
        .context("ffprobe failed to spawn")?;
    if !out.status.success() {
        bail!("ffprobe exit {}: {}", out.status, String::from_utf8_lossy(&out.stderr));
    }
    let v: serde_json::Value = serde_json::from_slice(&out.stdout)?;
    let stream = v["streams"].get(0).ok_or_else(|| anyhow!("no video stream"))?;
    let width = stream["width"].as_u64().ok_or_else(|| anyhow!("no video width"))? as u32;
    let height = stream["height"].as_u64().ok_or_else(|| anyhow!("no video height"))? as u32;
    let duration: f64 = v["format"]["duration"]
        .as_str()
        .ok_or_else(|| anyhow!("no duration"))?
        .parse()
        .context("duration does not parse")?;
    Ok((width, height, duration))
}

/// The words a mark draws, held in a file so nothing in them is filter syntax.
struct MarkFile(PathBuf);

impl MarkFile {
    async fn new(scratch: &Path, job: Uuid, tag: &str, words: &str) -> Result<Self> {
        let p = scratch.join(format!("{job}-{tag}.txt"));
        tokio::fs::write(&p, words).await.with_context(|| format!("writing {}", p.display()))?;
        Ok(Self(p))
    }
    fn arg(&self) -> String {
        self.0.display().to_string()
    }
}

impl Drop for MarkFile {
    fn drop(&mut self) {
        if let Err(e) = std::fs::remove_file(&self.0) {
            if e.kind() != std::io::ErrorKind::NotFound {
                tracing::error!(path = %self.0.display(), error = %e, "removing a mark text file");
            }
        }
    }
}

/// The moving mark: one line, sized to the smaller side so it fits portrait
/// and landscape alike, drifting so it stays wholly inside the frame.
fn moving_mark(text: &MarkFile) -> String {
    format!(
        "drawtext=fontfile={FONT}:textfile={t}:expansion=none:\
         fontcolor=white@0.6:fontsize='max(12,min(w,h)*0.035)':\
         shadowx=2:shadowy=2:shadowcolor=black@0.8:\
         x='(w-tw)/2+((w-tw)/2)*0.9*sin(2*PI*t/20)':\
         y='(h-th)/2+((h-th)/2)*0.85*sin(2*PI*t/10)'",
        t = text.arg()
    )
}

/// The poster's static mark, bottom-right.
fn static_mark(text: &MarkFile) -> String {
    format!(
        "drawtext=fontfile={FONT}:textfile={t}:expansion=none:\
         fontcolor=white@0.75:fontsize='max(14,min(w,h)*0.035)':\
         shadowx=2:shadowy=2:shadowcolor=black@0.8:\
         x='w-tw-min(w,h)*0.03':y='h-th-min(w,h)*0.03'",
        t = text.arg()
    )
}

fn video_codec_args(cmd: &mut Command) {
    if nvenc_enabled() {
        cmd.args(["-c:v", "h264_nvenc", "-preset", "p4", "-profile:v", "high", "-pix_fmt", "yuv420p"]);
    } else {
        cmd.args(["-c:v", "libx264", "-preset", "veryfast", "-profile:v", "main", "-level", "4.1", "-pix_fmt", "yuv420p"]);
    }
}

fn rate_args(cmd: &mut Command, v: Variant) {
    cmd.args(["-b:v", &format!("{}k", v.video_kbps)]);
    cmd.args(["-maxrate", &format!("{}k", v.video_kbps + v.video_kbps / 8)]);
    cmd.args(["-bufsize", &format!("{}k", v.video_kbps * 2)]);
    cmd.args(["-c:a", "aac", "-b:a", &format!("{}k", v.audio_kbps), "-ac", "2", "-ar", "48000"]);
}

/// One rendition of the ladder, marked.
async fn encode_rendition(input: PathBuf, dir: PathBuf, v: Variant, mark: Option<String>) -> Result<()> {
    let out_dir = dir.join(v.name);
    tokio::fs::create_dir_all(&out_dir).await?;
    let filter = match mark {
        Some(m) => format!("scale=-2:{},{m}", v.height),
        None => format!("scale=-2:{}", v.height),
    };
    let mut cmd = Command::new("ffmpeg");
    cmd.args(["-hide_banner", "-nostdin", "-y", "-i"]).arg(&input);
    cmd.args(["-map", "0:v:0", "-map", "0:a:0?"]);
    video_codec_args(&mut cmd);
    cmd.args(["-vf", &filter]);
    rate_args(&mut cmd, v);
    cmd.args(["-g", "48", "-keyint_min", "48"]);
    if !nvenc_enabled() {
        cmd.args(["-sc_threshold", "0"]);
    }
    cmd.args(["-f", "hls", "-hls_time", "4", "-hls_list_size", "0", "-hls_playlist_type", "vod"]);
    cmd.arg("-hls_segment_filename").arg(out_dir.join("seg_%03d.ts"));
    cmd.arg(out_dir.join("index.m3u8"));
    run_ffmpeg(cmd, &format!("rendition {}", v.name)).await
}

fn master_playlist(ladder: &[Variant], w: u32, h: u32) -> String {
    let mut s = String::from("#EXTM3U\n#EXT-X-VERSION:3\n");
    for v in ladder {
        s.push_str(&format!(
            "#EXT-X-STREAM-INF:BANDWIDTH={},RESOLUTION={}x{},CODECS=\"avc1.4d401f,mp4a.40.2\",NAME=\"{}\"\n{}/index.m3u8\n",
            v.bandwidth,
            width_for(w, h, v.height),
            v.height,
            v.name,
            v.name
        ));
    }
    s
}

async fn make_poster(input: &Path, duration: f64, mark: &MarkFile, dest: &Path) -> Result<()> {
    let pos = (duration * 0.05).max(0.5).min((duration - 0.1).max(0.0));
    let filter = format!("scale=-2:720,{}", static_mark(mark));
    let mut cmd = Command::new("ffmpeg");
    cmd.args(["-hide_banner", "-nostdin", "-y", "-ss", &format!("{pos:.2}"), "-i"]).arg(input);
    cmd.args(["-vframes", "1", "-q:v", "3", "-vf", &filter, "-c:v", "mjpeg", "-f", "image2", "-update", "1"]).arg(dest);
    run_ffmpeg(cmd, "poster").await
}

async fn make_download(input: &Path, top: Variant, mark: Option<String>, dest: &Path) -> Result<()> {
    let filter = match mark {
        Some(m) => format!("scale=-2:{},{m}", top.height),
        None => format!("scale=-2:{}", top.height),
    };
    let mut cmd = Command::new("ffmpeg");
    cmd.args(["-hide_banner", "-nostdin", "-y", "-i"]).arg(input);
    cmd.args(["-map", "0:v:0", "-map", "0:a:0?"]);
    video_codec_args(&mut cmd);
    cmd.args(["-vf", &filter]);
    rate_args(&mut cmd, top);
    cmd.args(["-movflags", "+faststart", "-f", "mp4"]).arg(dest);
    run_ffmpeg(cmd, "download").await
}

/// Draws a stretch of a replay as a video: its waveform under the room's
/// title, crediting the host. The words go through a file.
async fn make_audiogram(roots: &Roots, job: Uuid, req: &crate::job::ClipSpec, dest: &Path) -> Result<()> {
    let replay = roots.replay_dir.join(&req.replay_file);
    if !tokio::fs::try_exists(&replay).await.context("checking the replay")? {
        bail!("no such replay: {}", req.replay_file);
    }
    let short: String = req.title.chars().take(60).collect();
    let host = sanitize_for_drawtext(&req.host_handle);
    let words = if host.is_empty() {
        format!("{short}\non f33d3r")
    } else {
        format!("{short}\n@{host} on f33d3r")
    };
    let title = MarkFile::new(&roots.scratch(), job, "title", &words).await?;
    let filter = format!(
        "color=c=0x0B0B0F:s=1280x720:r=30[bg];\
         [0:a]showwaves=s=1280x320:mode=cline:rate=30:colors=0xF97316,format=rgba[wave];\
         [bg][wave]overlay=0:300:shortest=1,\
         drawtext=fontfile={AUDIOGRAM_FONT}:textfile={}:x=(w-text_w)/2:y=90:fontsize=44:\
         fontcolor=white:line_spacing=18:expansion=none,format=yuv420p[v]",
        title.arg()
    );
    let mut cmd = Command::new("ffmpeg");
    cmd.args(["-hide_banner", "-nostdin", "-loglevel", "error", "-y"])
        .args(["-ss", &req.start_seconds.to_string()])
        .args(["-t", &req.length_seconds.to_string()])
        .arg("-i")
        .arg(&replay)
        .args(["-filter_complex", &filter])
        .args(["-map", "[v]", "-map", "0:a"])
        .args(["-c:v", "libx264", "-preset", "veryfast", "-crf", "24"])
        .args(["-c:a", "aac", "-b:a", "128k", "-shortest", "-movflags", "+faststart", "-f", "mp4"])
        .arg(dest);
    run_ffmpeg(cmd, "audiogram").await
}

/// A sibling of `final_path` named for this job: `.<name>.<tag>-<job>`.
fn sibling(final_path: &Path, tag: &str, job: Uuid) -> Result<PathBuf> {
    let parent = final_path.parent().context("output has no parent directory")?;
    let name = final_path
        .file_name()
        .and_then(|n| n.to_str())
        .context("output has no file name")?;
    Ok(parent.join(format!(".{name}.{tag}-{job}")))
}

async fn remove_any(p: &Path) -> Result<()> {
    match tokio::fs::symlink_metadata(p).await {
        Ok(m) if m.is_dir() => tokio::fs::remove_dir_all(p).await.with_context(|| format!("removing {}", p.display())),
        Ok(_) => tokio::fs::remove_file(p).await.with_context(|| format!("removing {}", p.display())),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(e) => Err(e).with_context(|| format!("inspecting {}", p.display())),
    }
}

/// Moves a staged file or directory into place. A directory that already
/// exists is moved aside first and removed after, so the swap is two renames.
async fn commit(staged: &Path, final_path: &Path, job: Uuid) -> Result<()> {
    let is_dir = tokio::fs::metadata(staged).await?.is_dir();
    if is_dir && tokio::fs::try_exists(final_path).await? {
        let old = sibling(final_path, "old", job)?;
        remove_any(&old).await?;
        tokio::fs::rename(final_path, &old)
            .await
            .with_context(|| format!("moving {} aside", final_path.display()))?;
        tokio::fs::rename(staged, final_path)
            .await
            .with_context(|| format!("moving {} into place", final_path.display()))?;
        remove_any(&old).await?;
    } else {
        tokio::fs::rename(staged, final_path)
            .await
            .with_context(|| format!("moving {} into place", final_path.display()))?;
    }
    Ok(())
}

/// Rendition directory names a master playlist lists.
async fn listed_renditions(master: &Path) -> Result<Vec<String>> {
    let text = match tokio::fs::read_to_string(master).await {
        Ok(t) => t,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(Vec::new()),
        Err(e) => return Err(e).with_context(|| format!("reading {}", master.display())),
    };
    Ok(text
        .lines()
        .map(str::trim)
        .filter(|l| !l.is_empty() && !l.starts_with('#') && !l.contains("://") && !l.starts_with('/'))
        .filter_map(|l| l.split_once('/').map(|(d, _)| d.to_string()))
        .filter(|d| crate::job::check_rel(d).is_ok())
        .collect())
}

/// Moves a staged ladder into place.
///
/// A ladder directory that does not exist yet is one rename. One that does
/// (a legacy asset being re-marked) may share its directory with other files
/// — a legacy master sat beside the asset's download file — so only the
/// ladder's own entries are replaced: each rendition directory is swapped,
/// the master is renamed over, and renditions the old master listed that the
/// new ladder lacks are removed. Nothing else in the directory is touched.
async fn commit_ladder(stage: &Path, final_dir: &Path, renditions: &[RenditionOut], job: Uuid) -> Result<()> {
    if !tokio::fs::try_exists(final_dir).await? {
        return commit(stage, final_dir, job).await;
    }
    let old = listed_renditions(&final_dir.join("master.m3u8")).await?;
    for r in renditions {
        commit(&stage.join(&r.name), &final_dir.join(&r.name), job).await?;
    }
    commit(&stage.join("master.m3u8"), &final_dir.join("master.m3u8"), job).await?;
    for name in old {
        if !renditions.iter().any(|r| r.name == name) {
            remove_any(&final_dir.join(&name)).await?;
        }
    }
    remove_any(stage).await
}

/// Runs one job to completion.
pub async fn run(roots: &Roots, req: &JobRequest) -> Result<JobOutput> {
    let job = req.job_id;
    let source = roots.abs(&req.source)?;
    let scratch = roots.scratch();
    tokio::fs::create_dir_all(&scratch).await?;

    // A clip's source is drawn from the replay the first time; a re-run of the
    // job finds it already in the private store.
    if let Some(clip) = &req.clip {
        if !tokio::fs::try_exists(&source).await? {
            if let Some(parent) = source.parent() {
                tokio::fs::create_dir_all(parent).await?;
            }
            let staged = sibling(&source, "stage", job)?;
            remove_any(&staged).await?;
            if let Err(e) = make_audiogram(roots, job, clip, &staged).await {
                remove_any(&staged).await?;
                return Err(e);
            }
            commit(&staged, &source, job).await?;
        }
    }
    if !tokio::fs::try_exists(&source).await? {
        bail!("source {} does not exist", req.source.rel);
    }

    let (width, height, duration) = probe(&source).await?;
    if duration <= 0.0 {
        bail!("invalid duration");
    }
    if height < 144 {
        bail!("source height {height}p too small");
    }
    let ladder = ladder_for(height);
    let top = *ladder.last().context("empty ladder")?;

    let handle = sanitize_for_drawtext(&req.mark.handle);
    let short_id = sanitize_for_drawtext(&req.mark.short_id);
    let words = if handle.is_empty() {
        format!("f33d3r.com \u{2022} {short_id}")
    } else {
        format!("@{handle} \u{2022} f33d3r.com \u{2022} {short_id}")
    };
    let mark_file = MarkFile::new(&scratch, job, "mark", &words).await?;
    let moving = if req.mark.source_marked { None } else { Some(moving_mark(&mark_file)) };

    // ── encode everything into staging ──────────────────────────────────────
    let mut staged: Vec<(PathBuf, PathBuf)> = Vec::new();
    let result: Result<JobOutput> = async {
        let mut renditions = Vec::new();
        if let Some(hls) = &req.hls {
            let final_dir = roots.abs(hls)?;
            if let Some(parent) = final_dir.parent() {
                tokio::fs::create_dir_all(parent).await?;
            }
            let stage = sibling(&final_dir, "stage", job)?;
            remove_any(&stage).await?;
            tokio::fs::create_dir_all(&stage).await?;
            staged.push((stage.clone(), final_dir));
            let mut tasks = Vec::new();
            for v in ladder.iter().copied() {
                tasks.push(tokio::spawn(encode_rendition(source.clone(), stage.clone(), v, moving.clone())));
            }
            for t in tasks {
                t.await.context("rendition task panicked")??;
            }
            tokio::fs::write(stage.join("master.m3u8"), master_playlist(&ladder, width, height)).await?;
            renditions = ladder
                .iter()
                .map(|v| RenditionOut {
                    name: v.name.to_string(),
                    width: width_for(width, height, v.height),
                    height: v.height,
                    bandwidth: v.bandwidth,
                })
                .collect();
        }
        if let Some(poster) = &req.poster {
            let final_path = roots.abs(poster)?;
            if let Some(parent) = final_path.parent() {
                tokio::fs::create_dir_all(parent).await?;
            }
            let stage = sibling(&final_path, "stage", job)?;
            remove_any(&stage).await?;
            staged.push((stage.clone(), final_path));
            make_poster(&source, duration, &mark_file, &stage).await?;
        }
        let mut download_bytes = None;
        if let Some(dl) = &req.download {
            let final_path = roots.abs(dl)?;
            if let Some(parent) = final_path.parent() {
                tokio::fs::create_dir_all(parent).await?;
            }
            let stage = sibling(&final_path, "stage", job)?;
            remove_any(&stage).await?;
            staged.push((stage.clone(), final_path));
            make_download(&source, top, moving.clone(), &stage).await?;
            download_bytes = Some(tokio::fs::metadata(&stage).await?.len());
        }
        Ok(JobOutput {
            source_width: width,
            source_height: height,
            duration_seconds: duration,
            renditions,
            hls_written: req.hls.is_some(),
            poster_written: req.poster.is_some(),
            download_written: req.download.is_some(),
            download_bytes,
        })
    }
    .await;

    match result {
        Ok(out) => {
            // Every encode succeeded: move it all into place. The ladder is
            // first in `staged` when there is one.
            for (i, (stage, final_path)) in staged.iter().enumerate() {
                if i == 0 && req.hls.is_some() {
                    commit_ladder(stage, final_path, &out.renditions, job).await?;
                } else {
                    commit(stage, final_path, job).await?;
                }
            }
            Ok(out)
        }
        Err(e) => {
            for (stage, _) in &staged {
                if let Err(e2) = remove_any(stage).await {
                    tracing::error!(error = %format!("{e2:#}"), "clearing staging after a failed job");
                }
            }
            Err(e)
        }
    }
}
