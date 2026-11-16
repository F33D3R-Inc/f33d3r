//! Caeor's configuration. Read once at boot; a missing required value is a
//! boot failure, never a silent default that half-works.

use std::path::PathBuf;

use anyhow::{bail, Result};

use crate::layout::Layout;

#[derive(Clone, Debug)]
pub struct Config {
    pub port: String,
    pub database_url: String,
    pub layout: Layout,
    pub transcoding_url: String,
    /// The shared internal service key. Every caller of caeor presents it as
    /// X-Service-Key; caeor presents it to transcoding the same way.
    pub service_key: String,
    /// Abraxas, the integrity authority. Every upload is put to it before it
    /// may become ready; caeor presents service_key as X-Internal-Key.
    pub abraxas_url: String,
}

fn required(name: &str) -> Result<String> {
    match std::env::var(name) {
        Ok(v) if !v.trim().is_empty() => Ok(v),
        _ => bail!("{name} must be set"),
    }
}

fn or_default(name: &str, default: &str) -> String {
    std::env::var(name)
        .ok()
        .filter(|v| !v.trim().is_empty())
        .unwrap_or_else(|| default.to_string())
}

impl Config {
    pub fn from_env() -> Result<Self> {
        let media_dir = PathBuf::from(or_default("MEDIA_DIR", "/data/media"));
        let private_dir = PathBuf::from(or_default("PRIVATE_DIR", "/data/media-private"));
        if private_dir.starts_with(&media_dir) || media_dir.starts_with(&private_dir) {
            bail!(
                "PRIVATE_DIR ({}) and MEDIA_DIR ({}) must not contain one another: \
                 a private source inside the served store is a served source",
                private_dir.display(),
                media_dir.display()
            );
        }
        Ok(Self {
            port: or_default("PORT", "8086"),
            database_url: required("DATABASE_URL")?,
            layout: Layout { media_dir, private_dir },
            transcoding_url: or_default("TRANSCODING_URL", "http://transcoding:8085")
                .trim_end_matches('/')
                .to_string(),
            service_key: required("INTERNAL_API_KEY")?,
            // Required: without an integrity authority no upload may ever
            // become ready, so a missing URL is a boot failure, not a gate
            // that silently admits.
            abraxas_url: required("ABRAXAS_URL")?.trim_end_matches('/').to_string(),
        })
    }
}
