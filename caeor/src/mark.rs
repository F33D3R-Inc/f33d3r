//! Burned-in marks: the visible watermark on images, and the per-viewer
//! forensic layer on downloads.
//!
//! Text is drawn by ffmpeg's drawtext, the same renderer and font transcoding
//! uses for video, so a mark looks the same on every kind of media. Words
//! always reach drawtext through a `textfile` with expansion off: nothing in a
//! handle can be read as filter syntax or as a drawtext expansion, and the
//! bullet in the mark text needs no escaping.
//!
//! Pixels go in and out as PNG over pipes, so no intermediate file holds an
//! unmarked copy.

use std::path::{Path, PathBuf};
use std::process::Stdio;

use anyhow::{anyhow, bail, Context, Result};
use image::{DynamicImage, ImageFormat};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::process::Command;
use uuid::Uuid;

/// Installed by fonts-dejavu-core in caeor's image, as in transcoding's.
const FONT: &str = "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf";
const FONT_BOLD: &str = "/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf";

/// What a mark on an image looks like.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ImageMark {
    /// feed and full: a large semi-transparent mark across the middle, sized to
    /// the image, plus a small solid one in the corner. Cropping the corner
    /// leaves the middle; covering the middle ruins the picture.
    Content,
    /// thumb: a small corner mark only. A thumbnail is a pointer to the work.
    Corner,
}

/// A text file holding the words drawtext draws. Removed on drop.
struct TextFile(PathBuf);

impl TextFile {
    async fn new(scratch: &Path, words: &str) -> Result<Self> {
        let path = scratch.join(format!("mark-{}.txt", Uuid::new_v4()));
        tokio::fs::write(&path, words)
            .await
            .with_context(|| format!("writing mark text {}", path.display()))?;
        Ok(Self(path))
    }
    fn arg(&self) -> String {
        // Scratch paths are checked at boot to be [A-Za-z0-9/._-] only, so the
        // path needs no quoting inside the filter.
        self.0.display().to_string()
    }
}

impl Drop for TextFile {
    fn drop(&mut self) {
        if let Err(e) = std::fs::remove_file(&self.0) {
            if e.kind() != std::io::ErrorKind::NotFound {
                tracing::error!(path = %self.0.display(), error = %e, "removing mark text file");
            }
        }
    }
}

fn content_filters(main: &TextFile, corner: &TextFile) -> String {
    format!(
        "format=rgba,\
         drawtext=fontfile={FONT_BOLD}:textfile={main}:expansion=none:\
         fontsize='max(12,min(w/30,h/8))':fontcolor=white@0.32:\
         borderw=1:bordercolor=black@0.22:\
         x='(w-tw)/2':y='(h-th)/2',\
         drawtext=fontfile={FONT}:textfile={corner}:expansion=none:\
         fontsize='max(10,min(w/55,h/14))':fontcolor=white@0.62:\
         shadowx=1:shadowy=1:shadowcolor=black@0.7:\
         x='w-tw-w*0.02':y='h-th-h*0.02'",
        main = main.arg(),
        corner = corner.arg(),
    )
}

fn corner_filters(corner: &TextFile) -> String {
    format!(
        "format=rgba,\
         drawtext=fontfile={FONT}:textfile={corner}:expansion=none:\
         fontsize='max(9,min(w/20,h/10))':fontcolor=white@0.7:\
         shadowx=1:shadowy=1:shadowcolor=black@0.75:\
         x='w-tw-w*0.03':y='h-th-h*0.03'",
        corner = corner.arg(),
    )
}

fn forensic_image_filters(licence: &TextFile) -> String {
    format!(
        "format=rgba,\
         drawtext=fontfile={FONT}:textfile={licence}:expansion=none:\
         fontsize='max(10,min(w,h)*0.022)':fontcolor=white@0.45:\
         shadowx=1:shadowy=1:shadowcolor=black@0.6:\
         x='w*0.02':y='h-th-h*0.02'",
        licence = licence.arg(),
    )
}

/// Burns the visible watermark into an image.
///
/// `mark` is the full mark text (`@handle • f33d3r.com • short_id`);
/// `short_id` alone is used for the thumb's compact corner mark.
pub async fn mark_image(
    scratch: &Path,
    img: &DynamicImage,
    style: ImageMark,
    mark: &str,
    short_id: &str,
) -> Result<DynamicImage> {
    match style {
        ImageMark::Content => {
            let main = TextFile::new(scratch, mark).await?;
            let corner = TextFile::new(scratch, mark).await?;
            draw(img, &content_filters(&main, &corner)).await
        }
        ImageMark::Corner => {
            let corner = TextFile::new(scratch, &format!("f33d3r \u{2022} {short_id}")).await?;
            draw(img, &corner_filters(&corner)).await
        }
    }
}

/// Burns the per-viewer forensic layer into an (already watermarked) image.
pub async fn forensic_image(scratch: &Path, img: &DynamicImage, viewer: &str) -> Result<DynamicImage> {
    let licence = TextFile::new(scratch, &format!("Licensed to @{viewer}")).await?;
    draw(img, &forensic_image_filters(&licence)).await
}

/// Runs one image through ffmpeg with `filters`, PNG in and PNG out.
async fn draw(img: &DynamicImage, filters: &str) -> Result<DynamicImage> {
    let rgba = DynamicImage::ImageRgba8(img.to_rgba8());
    let mut png = Vec::new();
    rgba.write_to(&mut std::io::Cursor::new(&mut png), ImageFormat::Png)
        .context("encoding image for the mark pass")?;

    let mut child = Command::new("ffmpeg")
        .args(["-hide_banner", "-loglevel", "error", "-nostdin"])
        .args(["-f", "png_pipe", "-i", "pipe:0"])
        .args(["-vf", filters])
        .args(["-frames:v", "1", "-f", "image2pipe", "-c:v", "png", "pipe:1"])
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .kill_on_drop(true)
        .spawn()
        .context("spawning ffmpeg for the mark pass")?;

    let mut stdin = child.stdin.take().ok_or_else(|| anyhow!("ffmpeg stdin unavailable"))?;
    let mut stdout = child.stdout.take().ok_or_else(|| anyhow!("ffmpeg stdout unavailable"))?;
    let mut stderr = child.stderr.take().ok_or_else(|| anyhow!("ffmpeg stderr unavailable"))?;

    // Feed, drain and collect concurrently: a full pipe on either side would
    // otherwise deadlock the other.
    let writer = async move {
        stdin.write_all(&png).await.context("writing to ffmpeg")?;
        stdin.shutdown().await.context("closing ffmpeg stdin")?;
        Ok::<(), anyhow::Error>(())
    };
    let reader = async move {
        let mut out = Vec::new();
        stdout.read_to_end(&mut out).await.context("reading from ffmpeg")?;
        Ok::<Vec<u8>, anyhow::Error>(out)
    };
    let errors = async move {
        let mut err = Vec::new();
        stderr.read_to_end(&mut err).await.context("reading ffmpeg stderr")?;
        Ok::<Vec<u8>, anyhow::Error>(err)
    };
    let (w, out, err) = tokio::join!(writer, reader, errors);
    let status = child.wait().await.context("waiting for ffmpeg")?;
    let err = err?;
    if !status.success() {
        bail!("ffmpeg mark pass exit {status}: {}", String::from_utf8_lossy(&err));
    }
    w?;
    let out = out?;
    image::load_from_memory_with_format(&out, ImageFormat::Png).context("decoding the marked image")
}

/// Burns the per-viewer forensic layer into a watermarked MP4, writing `dest`.
///
/// Ported from feed-engine/internal/handler/download.go: "Licensed to @viewer",
/// bottom-right, static, 1.5% of height, 40% opacity. Static on purpose: the
/// moving creator mark is already in the file; this one names who took it.
pub async fn forensic_video(scratch: &Path, source: &Path, viewer: &str, dest: &Path) -> Result<()> {
    let licence = TextFile::new(scratch, &format!("Licensed to @{viewer}")).await?;
    let filter = format!(
        "drawtext=fontfile={FONT}:textfile={licence}:expansion=none:\
         fontcolor=white@0.4:fontsize='h*0.015':\
         shadowx=1:shadowy=1:shadowcolor=black@0.6:\
         x='w-tw-10':y='h-th-10'",
        licence = licence.arg(),
    );
    let out = Command::new("ffmpeg")
        .args(["-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-i"])
        .arg(source)
        .args(["-map", "0:v:0", "-map", "0:a:0?"])
        .args(["-vf", &filter])
        .args(["-c:v", "libx264", "-preset", "veryfast", "-profile:v", "main", "-level", "4.1"])
        .args(["-pix_fmt", "yuv420p", "-c:a", "copy", "-movflags", "+faststart", "-f", "mp4"])
        .arg(dest)
        .stdout(Stdio::null())
        .stderr(Stdio::piped())
        .kill_on_drop(true)
        .output()
        .await
        .context("spawning ffmpeg for the forensic layer")?;
    if !out.status.success() {
        bail!(
            "ffmpeg forensic layer exit {}: {}",
            out.status,
            String::from_utf8_lossy(&out.stderr)
        );
    }
    Ok(())
}

/// Copies an audio file with every metadata block removed (ID3, Vorbis
/// comments, encoder tags, embedded cover art). The stream itself is copied,
/// not re-encoded. Fails when the input is not audio ffmpeg can read.
pub async fn strip_audio_metadata(source: &Path, dest: &Path, container: &str) -> Result<()> {
    let out = Command::new("ffmpeg")
        .args(["-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-i"])
        .arg(source)
        .args(["-map", "0:a:0", "-map_metadata", "-1", "-map_chapters", "-1", "-c:a", "copy"])
        .args(["-f", container])
        .arg(dest)
        .stdout(Stdio::null())
        .stderr(Stdio::piped())
        .kill_on_drop(true)
        .output()
        .await
        .context("spawning ffmpeg to strip audio metadata")?;
    if !out.status.success() {
        bail!(
            "not a readable audio file (ffmpeg exit {}): {}",
            out.status,
            String::from_utf8_lossy(&out.stderr)
        );
    }
    Ok(())
}
