//! Where the media Abraxas judges lives, and the vocabulary for what it is.
//!
//! Abraxas never receives media bytes over HTTP. It is handed a path in one of
//! two read-only volumes, and it hands the worker the same absolute path —
//! both containers mount the volumes at the same place:
//!
//! ```text
//!   f33d3r_media_private → PRIVATE_DIR   (/data/media-private)  caeor's private store
//!   f33d3r_live_scan     → LIVE_SCAN_DIR (/data/live-scan)      sampled live keyframes
//! ```
//!
//! A path from a caller is RELATIVE to its store and must be plain: no root,
//! no `..`, no empty or `.` segment, only [A-Za-z0-9/._-]. caeor/src/layout.rs
//! applies the identical rule when it records a private path, so anything
//! caeor can name, Abraxas can resolve, and nothing else.

use std::path::{Component, Path, PathBuf};

use serde::{Deserialize, Serialize};

/// What an asset is, as caeor names it.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Kind {
    Image,
    Video,
    Voice,
    Audio,
    Clip,
    Avatar,
    Header,
    Encrypted,
}

impl Kind {
    pub fn as_str(self) -> &'static str {
        match self {
            Kind::Image => "image",
            Kind::Video => "video",
            Kind::Voice => "voice",
            Kind::Audio => "audio",
            Kind::Clip => "clip",
            Kind::Avatar => "avatar",
            Kind::Header => "header",
            Kind::Encrypted => "encrypted",
        }
    }

    pub fn parse(s: &str) -> Option<Kind> {
        Some(match s {
            "image" => Kind::Image,
            "video" => Kind::Video,
            "voice" => Kind::Voice,
            "audio" => Kind::Audio,
            "clip" => Kind::Clip,
            "avatar" => Kind::Avatar,
            "header" => Kind::Header,
            "encrypted" => Kind::Encrypted,
            _ => return None,
        })
    }

    /// Ciphertext the server cannot see. Only the exact-bytes layer applies.
    pub fn is_opaque(self) -> bool {
        self == Kind::Encrypted
    }

    /// Media whose re-upload is an ownership question. A shared meme avatar is
    /// not theft of anyone's work, and ciphertext has no perceptual content.
    pub fn ownership_eligible(self) -> bool {
        matches!(self, Kind::Image | Kind::Video | Kind::Voice | Kind::Audio | Kind::Clip)
    }
}

/// Validates a store-relative path. Mirrors caeor's `layout::check_rel`.
pub fn check_rel(rel: &str) -> Result<(), String> {
    if rel.is_empty() || rel.len() > 512 {
        return Err("path is empty or longer than 512 bytes".into());
    }
    if !rel
        .chars()
        .all(|c| c.is_ascii_alphanumeric() || matches!(c, '/' | '-' | '_' | '.'))
    {
        return Err(format!("path {rel:?} contains characters outside [A-Za-z0-9/._-]"));
    }
    if rel.split('/').any(|seg| seg.is_empty() || seg == "." || seg == "..") {
        return Err(format!("path {rel:?} has an empty or dot segment"));
    }
    for comp in Path::new(rel).components() {
        match comp {
            Component::Normal(_) => {}
            _ => return Err(format!("path {rel:?} is not a plain path inside its store")),
        }
    }
    Ok(())
}

/// The absolute path of a file in the private store.
pub fn private_abs(private_dir: &Path, rel: &str) -> Result<PathBuf, String> {
    check_rel(rel)?;
    Ok(private_dir.join(rel))
}

/// The absolute path of one sampled live keyframe. Both parts are single
/// segments: a stream id names a directory, a frame name a file in it.
pub fn live_frame_abs(live_dir: &Path, stream_id: &str, frame_name: &str) -> Result<PathBuf, String> {
    for (what, part) in [("stream_id", stream_id), ("frame_name", frame_name)] {
        check_rel(part)?;
        if part.contains('/') {
            return Err(format!("{what} must be a single path segment"));
        }
    }
    if !frame_name.ends_with(".jpg") {
        return Err("frame_name must name a .jpg keyframe".into());
    }
    Ok(live_dir.join(stream_id).join(frame_name))
}

/// The asset id a served media URL belongs to, by caeor's served layout:
/// `/static/media/a/<asset_id>/<file>`. Any other shape is not a caeor asset.
pub fn asset_id_from_served_url(url: &str) -> Option<uuid::Uuid> {
    let path = match url.find("/static/media/a/") {
        Some(i) => &url[i + "/static/media/a/".len()..],
        None => return None,
    };
    let id = path.split('/').next()?;
    uuid::Uuid::parse_str(id).ok()
}

pub fn is_sha256_hex(s: &str) -> bool {
    s.len() == 64 && s.bytes().all(|b| matches!(b, b'0'..=b'9' | b'a'..=b'f'))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn rejects_paths_that_climb_or_root() {
        assert!(check_rel("sources/a/source.mp4").is_ok());
        assert!(check_rel("/etc/passwd").is_err());
        assert!(check_rel("sources/../x").is_err());
        assert!(check_rel("sources//x").is_err());
        assert!(check_rel("a b").is_err());
    }

    #[test]
    fn served_url_names_its_asset() {
        let id = "0b3c8f0e-6a8e-4f53-9d55-3a6e5b0f2c11";
        let u = format!("/static/media/a/{id}/feed.webp");
        assert_eq!(asset_id_from_served_url(&u).map(|u| u.to_string()), Some(id.to_string()));
        assert!(asset_id_from_served_url("/static/media/posts/x.webp").is_none());
    }
}
