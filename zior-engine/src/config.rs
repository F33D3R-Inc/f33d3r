use anyhow::Result;
use config::{Config, File};
use serde::Deserialize;

#[derive(Debug, Clone, Deserialize)]
pub struct AppConfig {
    pub server: ServerConfig,
    pub audio: AudioConfig,
}

#[derive(Debug, Clone, Deserialize)]
pub struct ServerConfig {
    pub host: String,
    pub port: u16,
}

#[derive(Debug, Clone, Deserialize)]
pub struct AudioConfig {
    pub max_upload_bytes: usize,
    pub target_sample_rate: u32,
    pub fft_window_size: usize,
    pub hop_size: usize,
    pub min_duration_secs: f64,
    pub max_duration_secs: f64,
}

/// The shared X-Internal-Key every caller but /health and /metrics must
/// present. Required: Zior refuses to start without it.
pub fn internal_api_key() -> Result<String> {
    match std::env::var("INTERNAL_API_KEY") {
        Ok(v) if !v.trim().is_empty() => Ok(v),
        _ => anyhow::bail!("INTERNAL_API_KEY is not set; Zior refuses to start without it"),
    }
}

impl AppConfig {
    pub fn load() -> Result<Self> {
        let cfg = Config::builder()
            .add_source(File::with_name("config/default"))
            .add_source(config::Environment::with_prefix("ZIOR").separator("__"))
            .build()?;
        Ok(cfg.try_deserialize()?)
    }
}
