//! The job contract with caeor. Mirrors caeor/src/jobs.rs and the Location
//! type in caeor/src/layout.rs; change both or neither.
//!
//! Caeor is the only caller. It owns the durable record of every job; this
//! service keeps an in-memory view of the jobs it is running and nothing else.
//! Paths cross the wire as locations relative to one of two stores, never as
//! absolute paths, and every one is checked before it touches the disk.

use std::path::{Component, Path, PathBuf};

use anyhow::{bail, Result};
use serde::{Deserialize, Serialize};
use uuid::Uuid;

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Store {
    /// The f33d3r_media volume: viewer-facing copies, all of them marked.
    Served,
    /// The f33d3r_media_private volume: clean sources and download files.
    Private,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Location {
    pub store: Store,
    pub rel: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ClipSpec {
    pub replay_file: String,
    pub start_seconds: u32,
    pub length_seconds: u32,
    pub title: String,
    pub host_handle: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct MarkSpec {
    pub handle: String,
    pub short_id: String,
    pub source_marked: bool,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct JobRequest {
    pub job_id: Uuid,
    pub asset_id: Uuid,
    pub source: Location,
    #[serde(default)]
    pub clip: Option<ClipSpec>,
    pub mark: MarkSpec,
    #[serde(default)]
    pub hls: Option<Location>,
    #[serde(default)]
    pub poster: Option<Location>,
    #[serde(default)]
    pub download: Option<Location>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct RenditionOut {
    pub name: String,
    pub width: u32,
    pub height: u32,
    pub bandwidth: u32,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct JobOutput {
    pub source_width: u32,
    pub source_height: u32,
    pub duration_seconds: f64,
    pub renditions: Vec<RenditionOut>,
    pub hls_written: bool,
    pub poster_written: bool,
    pub download_written: bool,
    pub download_bytes: Option<u64>,
}

/// Where the two stores and the replays are mounted in this container.
#[derive(Debug, Clone)]
pub struct Roots {
    pub media_dir: PathBuf,
    pub private_dir: PathBuf,
    pub replay_dir: PathBuf,
}

impl Roots {
    pub fn scratch(&self) -> PathBuf {
        self.private_dir.join("scratch")
    }

    pub fn abs(&self, loc: &Location) -> Result<PathBuf> {
        check_rel(&loc.rel)?;
        Ok(match loc.store {
            Store::Served => self.media_dir.join(&loc.rel),
            Store::Private => self.private_dir.join(&loc.rel),
        })
    }
}

/// Same rule as caeor's layout::check_rel: a plain relative path of
/// [A-Za-z0-9/._-] with no empty, `.` or `..` segment.
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
    if rel.split('/').any(|seg| seg.is_empty() || seg == "." || seg == "..") {
        bail!("relative path {rel:?} has an empty or dot segment");
    }
    if !Path::new(rel)
        .components()
        .all(|c| matches!(c, Component::Normal(_)))
    {
        bail!("relative path {rel:?} is not a plain path inside its store");
    }
    Ok(())
}

impl JobRequest {
    /// Everything a job may be refused for, checked before any work starts.
    pub fn validate(&self) -> Result<()> {
        for loc in [Some(&self.source), self.hls.as_ref(), self.poster.as_ref(), self.download.as_ref()]
            .into_iter()
            .flatten()
        {
            check_rel(&loc.rel)?;
        }
        if self.hls.is_none() && self.poster.is_none() && self.download.is_none() {
            bail!("a job must write at least one output");
        }
        if let Some(h) = &self.hls {
            if h.store != Store::Served {
                bail!("the HLS ladder is a viewer-facing copy and belongs in the served store");
            }
        }
        if let Some(p) = &self.poster {
            if p.store != Store::Served {
                bail!("the poster belongs in the served store");
            }
        }
        if let Some(c) = &self.clip {
            if self.source.store != Store::Private {
                bail!("a clip's drawn source belongs in the private store");
            }
            let bare = Path::new(&c.replay_file).file_name().and_then(|n| n.to_str());
            if bare != Some(c.replay_file.as_str()) || !c.replay_file.ends_with(".mp4") {
                bail!("replay_file must be a bare .mp4 file name");
            }
            check_rel(&c.replay_file)?;
            if c.length_seconds == 0 || c.length_seconds > MAX_CLIP_SECONDS {
                bail!("clip length must be 1..={MAX_CLIP_SECONDS} seconds");
            }
        }
        if !self.mark.short_id.chars().all(|c| c.is_ascii_alphanumeric()) || self.mark.short_id.len() != 8 {
            bail!("short_id must be 8 alphanumeric characters");
        }
        Ok(())
    }
}

pub const MAX_CLIP_SECONDS: u32 = 140;
