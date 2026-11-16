//! Abraxas's configuration. Read once at boot; a missing required value is a
//! boot failure, never a silent default that half-works.

use std::path::PathBuf;
use std::time::Duration;

use anyhow::{bail, Result};

#[derive(Clone, Debug)]
pub struct Config {
    pub port: String,
    /// f33d3r_integrity, the only database Abraxas writes.
    pub database_url: String,
    pub kafka_brokers: String,
    /// content-scan, Abraxas's private ML worker.
    pub content_scan_url: String,
    /// How long one worker call may take. Fingerprinting an hour of video is
    /// minutes of work; a short timeout here would turn long uploads into
    /// permanent "worker unavailable" rechecks.
    pub worker_timeout: Duration,
    pub elohim_veni_url: String,
    /// The shared internal key: required of every caller, presented to the
    /// worker and to elohim-veni.
    pub internal_key: String,
    /// caeor's private media store, mounted read-only.
    pub private_dir: PathBuf,
    /// The live lane's sampled-keyframe volume, mounted read-only.
    pub live_scan_dir: PathBuf,
    pub scan_version: String,
    pub job_concurrency: usize,
}

fn required(name: &str) -> Result<String> {
    match std::env::var(name) {
        Ok(v) if !v.trim().is_empty() => Ok(v.trim().to_string()),
        _ => bail!("{name} must be set"),
    }
}

fn or_default(name: &str, default: &str) -> String {
    std::env::var(name)
        .ok()
        .map(|v| v.trim().to_string())
        .filter(|v| !v.is_empty())
        .unwrap_or_else(|| default.to_string())
}

fn parsed<T: std::str::FromStr>(name: &str, default: T) -> Result<T> {
    match std::env::var(name) {
        Ok(v) if !v.trim().is_empty() => match v.trim().parse() {
            Ok(n) => Ok(n),
            Err(_) => bail!("{name}={v:?} does not parse"),
        },
        _ => Ok(default),
    }
}

/// A source database read only by `abraxas-shield import`. Required there and
/// nowhere else, so it is read on demand rather than at boot.
pub fn import_source(name: &str) -> Result<String> {
    required(name)
}

impl Config {
    pub fn from_env() -> Result<Self> {
        let job_concurrency: usize = parsed("JOB_CONCURRENCY", 4)?;
        if job_concurrency == 0 {
            bail!("JOB_CONCURRENCY must be at least 1");
        }
        let worker_timeout_secs: u64 = parsed("WORKER_TIMEOUT_SECS", 900)?;
        if worker_timeout_secs < 10 {
            bail!("WORKER_TIMEOUT_SECS must be at least 10");
        }
        Ok(Self {
            port: or_default("PORT", "8109"),
            database_url: required("DATABASE_URL")?,
            kafka_brokers: or_default("KAFKA_BROKERS", "sitra-achra:9092"),
            content_scan_url: required("CONTENT_SCAN_URL")?.trim_end_matches('/').to_string(),
            worker_timeout: Duration::from_secs(worker_timeout_secs),
            elohim_veni_url: required("ELOHIM_VENI_URL")?.trim_end_matches('/').to_string(),
            internal_key: required("INTERNAL_API_KEY")?,
            private_dir: PathBuf::from(or_default("PRIVATE_DIR", "/data/media-private")),
            live_scan_dir: PathBuf::from(or_default("LIVE_SCAN_DIR", "/data/live-scan")),
            scan_version: or_default("SCAN_VERSION", "4.0"),
            job_concurrency,
        })
    }
}
