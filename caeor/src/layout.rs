//! The one media layout. Every byte caeor or transcoding writes lands at a path
//! this module names, and nowhere else.
//!
//! ```text
//! SERVED STORE — the f33d3r_media volume
//!   mounted in caeor and transcoding at MEDIA_DIR (/data/media), read-write
//!   mounted in feed-engine at /app/web/static/media → URL /static/media/
//!   Every file here is a viewer-facing copy, so every image and video file
//!   here is watermarked. feed-engine serves a file only after asking caeor's
//!   resolve endpoint what it is.
//!
//!   a/<asset_id>/thumb.webp            image thumb   (small corner mark)
//!   a/<asset_id>/feed.webp             image feed    (visible mark)
//!   a/<asset_id>/full.webp             image full    (visible mark)
//!   a/<asset_id>/thumb.webp ...        avatar/header (identity, NOT marked)
//!   a/<asset_id>/poster.jpg            video poster  (static mark)
//!   a/<asset_id>/hls/master.m3u8       hls_master    (every rendition marked)
//!   a/<asset_id>/hls/<rendition>/      hls_variant   index.m3u8 + seg_NNN.ts
//!   a/<asset_id>/audio.<ext>           voice/audio   (metadata stripped)
//!   a/<asset_id>/blob.bin              encrypted     (ciphertext, opaque)
//!
//!   → URL: /static/media/a/<asset_id>/<file>
//!
//!   Media from before caeor kept its paths (/static/media/posts/...,
//!   /static/uploads/...); the import records them as derivatives of their
//!   assets and `caeor remark` watermarks them in place.
//!
//! PRIVATE STORE — the f33d3r_media_private volume
//!   mounted ONLY in caeor and transcoding at PRIVATE_DIR (/data/media-private).
//!   Not mounted in feed-engine, live-edge, mediamtx or content-scan, so no
//!   static mount anywhere can reach it. Paths recorded for this store are
//!   relative and never begin with '/', so no URL path can name one.
//!
//!   incoming/<upload_id>.part          an upload in flight
//!   sources/<asset_id>/source.<ext>    source_private: the clean original,
//!                                      kept for re-encodes, never served
//!   downloads/<asset_id>/download.mp4  the watermarked download file; reached
//!                                      only through GET /v1/assets/{id}/download
//!   scratch/                           transient work files
//! ```

use std::path::{Component, Path, PathBuf};

use anyhow::{bail, Result};
use serde::{Deserialize, Serialize};
use uuid::Uuid;

/// The URL prefix under which feed-engine serves the served store.
pub const SERVED_URL_PREFIX: &str = "/static/media/";

#[derive(Debug, Clone)]
pub struct Layout {
    pub media_dir: PathBuf,
    pub private_dir: PathBuf,
}

/// Which store a location lives in. Shared on the wire with transcoding.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Store {
    Served,
    Private,
}

impl Store {
    pub fn as_str(self) -> &'static str {
        match self {
            Store::Served => "served",
            Store::Private => "private",
        }
    }
}

/// A file or directory relative to the root of one store. This is the form
/// paths take between caeor and transcoding: neither side ever hands the other
/// an absolute path, and both reject anything that could climb out of a store.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Location {
    pub store: Store,
    pub rel: String,
}

impl Location {
    pub fn served(rel: impl Into<String>) -> Self {
        Self { store: Store::Served, rel: rel.into() }
    }
    pub fn private(rel: impl Into<String>) -> Self {
        Self { store: Store::Private, rel: rel.into() }
    }
}

/// A relative path is acceptable only when it is plain: no root, no `..`, no
/// empty or `.` segments, and only characters that need no quoting anywhere.
pub fn check_rel(rel: &str) -> Result<()> {
    if rel.is_empty() || rel.len() > 512 {
        bail!("relative path is empty or too long");
    }
    if !rel
        .chars()
        .all(|c| c.is_ascii_alphanumeric() || matches!(c, '/' | '-' | '_' | '.'))
    {
        bail!("relative path {rel:?} contains characters outside [A-Za-z0-9/._-]");
    }
    for comp in Path::new(rel).components() {
        match comp {
            Component::Normal(s) if s != "." && !s.is_empty() => {}
            _ => bail!("relative path {rel:?} is not a plain path inside its store"),
        }
    }
    if rel.split('/').any(|seg| seg.is_empty() || seg == "." || seg == "..") {
        bail!("relative path {rel:?} has an empty or dot segment");
    }
    Ok(())
}

impl Layout {
    pub fn abs(&self, loc: &Location) -> Result<PathBuf> {
        check_rel(&loc.rel)?;
        Ok(match loc.store {
            Store::Served => self.media_dir.join(&loc.rel),
            Store::Private => self.private_dir.join(&loc.rel),
        })
    }

    // ── served store ─────────────────────────────────────────────────────────

    pub fn asset_rel(asset_id: Uuid) -> String {
        format!("a/{asset_id}")
    }

    pub fn served_file(asset_id: Uuid, file: &str) -> Location {
        Location::served(format!("a/{asset_id}/{file}"))
    }

    pub fn hls_dir(asset_id: Uuid) -> Location {
        Location::served(format!("a/{asset_id}/hls"))
    }

    pub fn poster(asset_id: Uuid) -> Location {
        Self::served_file(asset_id, "poster.jpg")
    }

    // ── private store ────────────────────────────────────────────────────────

    pub fn incoming(upload_id: Uuid) -> Location {
        Location::private(format!("incoming/{upload_id}.part"))
    }

    pub fn source(asset_id: Uuid, ext: &str) -> Location {
        Location::private(format!("sources/{asset_id}/source.{ext}"))
    }

    pub fn download(asset_id: Uuid) -> Location {
        Location::private(format!("downloads/{asset_id}/download.mp4"))
    }

    pub fn scratch_dir(&self) -> PathBuf {
        self.private_dir.join("scratch")
    }

    /// Creates the fixed directories of both stores.
    pub fn prepare(&self) -> Result<()> {
        for d in [
            self.media_dir.join("a"),
            self.private_dir.join("incoming"),
            self.private_dir.join("sources"),
            self.private_dir.join("downloads"),
            self.private_dir.join("scratch"),
        ] {
            std::fs::create_dir_all(&d)
                .map_err(|e| anyhow::anyhow!("creating {}: {e}", d.display()))?;
        }
        Ok(())
    }

    /// Maps a served URL path (/static/media/…) to its file on the served
    /// volume. Paths outside /static/media/ live in stores caeor does not hold.
    pub fn served_url_to_abs(&self, url_path: &str) -> Option<PathBuf> {
        let rel = url_path.strip_prefix(SERVED_URL_PREFIX)?;
        check_rel(rel).ok()?;
        Some(self.media_dir.join(rel))
    }
}

/// The URL path feed-engine serves a served-store location at.
pub fn served_url(rel: &str) -> String {
    format!("{SERVED_URL_PREFIX}{rel}")
}

/// Every ancestor directory of a URL path, each ending in '/', nearest last.
/// `/static/media/a/x/hls/720p/seg_001.ts` → `/static/`, `/static/media/`, …,
/// `/static/media/a/x/hls/720p/`. resolve() matches these against
/// `derivatives.serves_prefix` so that an HLS segment finds its playlist.
pub fn ancestor_dirs(url_path: &str) -> Vec<String> {
    let mut out = Vec::new();
    let mut idx = 0;
    while let Some(pos) = url_path[idx..].find('/') {
        let end = idx + pos + 1;
        if end > 1 && end < url_path.len() {
            out.push(url_path[..end].to_string());
        }
        idx = end;
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn rel_paths_stay_inside() {
        assert!(check_rel("a/x/hls").is_ok());
        assert!(check_rel("../etc").is_err());
        assert!(check_rel("/abs").is_err());
        assert!(check_rel("a//b").is_err());
        assert!(check_rel("a/./b").is_err());
        assert!(check_rel("a/b c").is_err());
    }

    #[test]
    fn ancestors() {
        assert_eq!(
            ancestor_dirs("/static/media/a/x/hls/720p/seg.ts"),
            vec![
                "/static/",
                "/static/media/",
                "/static/media/a/",
                "/static/media/a/x/",
                "/static/media/a/x/hls/",
                "/static/media/a/x/hls/720p/"
            ]
        );
    }
}
