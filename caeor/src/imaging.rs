//! Still images: decode, orient, size, mark, encode.
//!
//! Decoding to pixels and encoding fresh is what strips EXIF, GPS, maker
//! notes and every other metadata block: the encoder is handed pixels and
//! nothing else. The orientation EXIF carried is applied to the pixels first,
//! so a phone photo still stands the right way up without its metadata.

use std::io::Cursor;
use std::path::Path;

use anyhow::{bail, Context, Result};
use image::{imageops::FilterType, DynamicImage, ImageDecoder, ImageFormat, ImageReader, Limits};

use crate::assets::Kind;
use crate::mark::{self, ImageMark};

/// Largest image caeor will decode, per side. Anything larger is a
/// decompression bomb or a mistake.
const MAX_SIDE: u32 = 16_384;
/// Largest allocation a decode may make.
const MAX_ALLOC: u64 = 768 * 1024 * 1024;

/// Decodes upload bytes into upright pixels.
pub fn decode(bytes: &[u8]) -> Result<(DynamicImage, ImageFormat)> {
    let mut reader = ImageReader::new(Cursor::new(bytes))
        .with_guessed_format()
        .context("reading image header")?;
    let format = reader.format().context("not an image format caeor reads (jpeg, png, webp, gif)")?;
    let mut limits = Limits::default();
    limits.max_image_width = Some(MAX_SIDE);
    limits.max_image_height = Some(MAX_SIDE);
    limits.max_alloc = Some(MAX_ALLOC);
    reader.limits(limits);
    let mut decoder = reader.into_decoder().context("opening image decoder")?;
    let orientation = decoder.orientation().context("reading image orientation")?;
    let mut img = DynamicImage::from_decoder(decoder).context("decoding image")?;
    img.apply_orientation(orientation);
    if img.width() == 0 || img.height() == 0 {
        bail!("image has no pixels");
    }
    Ok((img, format))
}

/// The file extension a source of this format is kept under.
pub fn extension_for(format: ImageFormat) -> &'static str {
    match format {
        ImageFormat::Jpeg => "jpg",
        ImageFormat::Png => "png",
        ImageFormat::WebP => "webp",
        ImageFormat::Gif => "gif",
        _ => "img",
    }
}

/// One file made from an image.
pub struct Rendition {
    pub role: &'static str,
    pub file: &'static str,
    pub img: DynamicImage,
    pub watermarked: bool,
}

/// Shrinks to fit inside `max_w`×`max_h`, never enlarging.
fn fit_within(img: &DynamicImage, max_w: u32, max_h: u32) -> DynamicImage {
    if img.width() <= max_w && img.height() <= max_h {
        return img.clone();
    }
    img.resize(max_w, max_h, FilterType::Lanczos3)
}

fn crop_square(img: &DynamicImage) -> DynamicImage {
    let (w, h) = (img.width(), img.height());
    let side = w.min(h);
    img.crop_imm((w - side) / 2, (h - side) / 2, side, side)
}

fn crop_3x1(img: &DynamicImage) -> DynamicImage {
    let (w, h) = (img.width(), img.height());
    let target_h = w / 3;
    if target_h <= h && target_h > 0 {
        img.crop_imm(0, (h - target_h) / 2, w, target_h)
    } else {
        let target_w = (h * 3).min(w);
        img.crop_imm((w - target_w) / 2, 0, target_w, h)
    }
}

/// Every served rendition of an image of `kind`, marked as its kind requires.
///
///   image  → thumb 300² (corner mark), feed ≤800w, full ≤1200w (visible mark)
///   avatar → thumb 128², feed 256², full 512²   (identity: not marked)
///   header → thumb 600×200, feed 1200×400, full 2400×800 (identity: not marked)
pub async fn render(
    scratch: &Path,
    kind: Kind,
    img: DynamicImage,
    mark_text: &str,
    short_id: &str,
) -> Result<Vec<Rendition>> {
    match kind {
        Kind::Image => {
            let base = img;
            let sized = tokio::task::spawn_blocking(move || {
                let thumb = crop_square(&base).resize_exact(300, 300, FilterType::Lanczos3);
                let feed = fit_within(&base, 800, 2400);
                let full = fit_within(&base, 1200, 3600);
                (thumb, feed, full)
            })
            .await
            .context("resize task")?;
            let (thumb, feed, full) = sized;
            Ok(vec![
                Rendition {
                    role: "thumb",
                    file: "thumb.webp",
                    img: mark::mark_image(scratch, &thumb, ImageMark::Corner, mark_text, short_id).await?,
                    watermarked: true,
                },
                Rendition {
                    role: "feed",
                    file: "feed.webp",
                    img: mark::mark_image(scratch, &feed, ImageMark::Content, mark_text, short_id).await?,
                    watermarked: true,
                },
                Rendition {
                    role: "full",
                    file: "full.webp",
                    img: mark::mark_image(scratch, &full, ImageMark::Content, mark_text, short_id).await?,
                    watermarked: true,
                },
            ])
        }
        Kind::Avatar => {
            let out = tokio::task::spawn_blocking(move || {
                let sq = crop_square(&img);
                [(128u32, "thumb", "thumb.webp"), (256, "feed", "feed.webp"), (512, "full", "full.webp")]
                    .into_iter()
                    .map(|(px, role, file)| Rendition {
                        role,
                        file,
                        img: sq.resize_exact(px, px, FilterType::Lanczos3),
                        watermarked: false,
                    })
                    .collect::<Vec<_>>()
            })
            .await
            .context("resize task")?;
            Ok(out)
        }
        Kind::Header => {
            let out = tokio::task::spawn_blocking(move || {
                let banner = crop_3x1(&img);
                [
                    (600u32, 200u32, "thumb", "thumb.webp"),
                    (1200, 400, "feed", "feed.webp"),
                    (2400, 800, "full", "full.webp"),
                ]
                .into_iter()
                .map(|(w, h, role, file)| Rendition {
                    role,
                    file,
                    img: banner.resize_exact(w, h, FilterType::Lanczos3),
                    watermarked: false,
                })
                .collect::<Vec<_>>()
            })
            .await
            .context("resize task")?;
            Ok(out)
        }
        other => bail!("{} is not an image kind", other.as_str()),
    }
}

/// Encodes `img` in `format` and writes it to `dest` atomically: a reader of
/// `dest` sees the old file or the new one, never half of either. Returns the
/// byte count.
pub async fn write_atomic(img: DynamicImage, format: ImageFormat, dest: &Path) -> Result<u64> {
    let bytes = tokio::task::spawn_blocking(move || -> Result<Vec<u8>> {
        let mut out = Vec::new();
        let img = match format {
            // JPEG has no alpha channel.
            ImageFormat::Jpeg => DynamicImage::ImageRgb8(img.to_rgb8()),
            _ => img,
        };
        img.write_to(&mut Cursor::new(&mut out), format)
            .with_context(|| format!("encoding {format:?}"))?;
        Ok(out)
    })
    .await
    .context("encode task")??;
    write_bytes_atomic(&bytes, dest).await?;
    Ok(bytes.len() as u64)
}

/// Writes bytes to `dest` through a sibling temp file and a rename.
pub async fn write_bytes_atomic(bytes: &[u8], dest: &Path) -> Result<()> {
    let parent = dest.parent().context("destination has no parent directory")?;
    tokio::fs::create_dir_all(parent)
        .await
        .with_context(|| format!("creating {}", parent.display()))?;
    let name = dest
        .file_name()
        .and_then(|n| n.to_str())
        .context("destination has no file name")?;
    let tmp = parent.join(format!(".{name}.tmp-{}", uuid::Uuid::new_v4()));
    tokio::fs::write(&tmp, bytes)
        .await
        .with_context(|| format!("writing {}", tmp.display()))?;
    if let Err(e) = tokio::fs::rename(&tmp, dest).await {
        if let Err(e2) = tokio::fs::remove_file(&tmp).await {
            tracing::error!(path = %tmp.display(), error = %e2, "removing temp file after failed rename");
        }
        return Err(e).with_context(|| format!("renaming into {}", dest.display()));
    }
    Ok(())
}
