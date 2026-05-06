use anyhow::Result;
use config::{Config, File};
use serde::Deserialize;

#[derive(Debug, Clone, Deserialize)]
pub struct AppConfig {
    pub server: ServerConfig,
    pub aethyr: AethyrConfig,
    pub audio: AudioConfig,
    pub jung: JungConfig,
    pub mood: MoodConfig,
    pub velocity: VelocityConfig,
    pub safety: SafetyConfig,
}

#[derive(Debug, Clone, Deserialize)]
pub struct ServerConfig {
    pub host: String,
    pub port: u16,
}

#[derive(Debug, Clone, Deserialize)]
pub struct AethyrConfig {
    pub content_store_url: String,
    pub signal_push_timeout_ms: u64,
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

#[derive(Debug, Clone, Deserialize)]
pub struct JungConfig {
    pub vector_dim: usize,
    pub audio_weight: f64,
    pub behavioral_weight: f64,
}

#[derive(Debug, Clone, Deserialize)]
pub struct MoodConfig {
    pub major_key_valence_boost: f64,
    pub high_bpm_threshold: f64,
    pub low_bpm_threshold: f64,
}

#[derive(Debug, Clone, Deserialize)]
pub struct VelocityConfig {
    pub window_hours: f64,
    pub half_life_hours: f64,
}

#[derive(Debug, Clone, Deserialize)]
pub struct SafetyConfig {
    pub explicit_threshold: f64,
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
