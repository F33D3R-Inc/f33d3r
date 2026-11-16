use anyhow::Result;
use config::{Config, File};
use serde::Deserialize;

#[derive(Debug, Clone, Deserialize)]
pub struct AppConfig {
    pub server: ServerConfig,
    pub pipeline: PipelineConfig,
    pub prerank: PrerankConfig,
    pub scoring: ScoringConfig,
    pub velocity: VelocityConfig,
    pub revenue: RevenueConfig,
    pub bandit: BanditConfig,
    pub learning: LearningConfig,
    pub network: NetworkConfig,
}

#[derive(Debug, Clone, Deserialize)]
pub struct ServerConfig {
    pub host: String,
    pub port: u16,
}

#[derive(Debug, Clone, Deserialize)]
pub struct PipelineConfig {
    pub revenue_weight_cap: f64,
    pub explore_weight_cap: f64,
    pub neural_base_weight: f64,
    pub aesq_constraint_weight: f64,
}

#[derive(Debug, Clone, Deserialize)]
pub struct PrerankConfig {
    pub top_k: usize,
    pub min_fast_score: f64,
}

#[derive(Debug, Clone, Deserialize)]
pub struct ScoringConfig {
    pub weights: WeightConfig,
    pub freshness: FreshnessConfig,
    pub quality: QualityConfig,
}

#[derive(Debug, Clone, Deserialize)]
pub struct WeightConfig {
    pub w_alignment: f64,
    pub w_expansion: f64,
    pub w_shadow: f64,
    pub w_quality: f64,
    pub w_freshness: f64,
}

#[derive(Debug, Clone, Deserialize)]
pub struct FreshnessConfig {
    pub half_life_hours: f64,
}

#[derive(Debug, Clone, Deserialize)]
pub struct QualityConfig {
    pub w_like: f64,
    pub w_share: f64,
    pub w_comment: f64,
    pub w_save: f64,
    pub w_view_time: f64,
}

#[derive(Debug, Clone, Deserialize)]
pub struct VelocityConfig {
    pub boost_multiplier: f64,
    pub promotion_threshold: f64,
    pub decay_threshold: f64,
}

#[derive(Debug, Clone, Deserialize)]
pub struct RevenueConfig {
    pub w_conversion: f64,
    pub w_creator_rate: f64,
    pub w_ltv: f64,
    pub max_adjustment: f64,
}

#[derive(Debug, Clone, Deserialize)]
pub struct BanditConfig {
    pub feature_dim: usize,
    pub exploration_count: usize,
    pub cold_start_explore_count: usize,
}

#[derive(Debug, Clone, Deserialize)]
pub struct NetworkConfig {
    /// Score boost added to in-network content (creator is followed by the user).
    /// Mirrors X's in-network pool priority — social graph content floats up.
    pub network_boost: f64,
}

/// How much the viewer's learned LinUCB preference moves a work's score.
#[derive(Debug, Clone, Deserialize)]
pub struct LearningConfig {
    /// Weight on θᵀx, the reward the viewer's model expects for this work's
    /// feature vector (clamped to [0, 1]). See config/default.toml for why it
    /// is the value it is.
    pub learned_weight: f64,
}

/// Process settings that come only from the environment. Every one is
/// required: AethyrRank owns the ranking state and its intake, and a process
/// that could not keep either would only pretend to rank.
#[derive(Debug, Clone)]
pub struct Runtime {
    /// f33d3r_rank. Created at boot when it does not exist.
    pub database_url: String,
    /// Sitra Achra brokers, for ranking.events and its dead-letter topic.
    pub kafka_brokers: String,
    /// The shared X-Internal-Key every caller must present.
    pub internal_api_key: String,
}

impl Runtime {
    pub fn from_env() -> Result<Self> {
        fn required(name: &str) -> Result<String> {
            match std::env::var(name) {
                Ok(v) if !v.trim().is_empty() => Ok(v),
                _ => anyhow::bail!("{name} is not set; AethyrRank refuses to start without it"),
            }
        }
        Ok(Self {
            database_url: required("DATABASE_URL")?,
            kafka_brokers: required("KAFKA_BROKERS")?,
            internal_api_key: required("INTERNAL_API_KEY")?,
        })
    }
}

impl AppConfig {
    pub fn load() -> Result<Self> {
        let cfg = Config::builder()
            .add_source(File::with_name("config/default"))
            .add_source(config::Environment::with_prefix("APP").separator("__"))
            .build()?;
        Ok(cfg.try_deserialize()?)
    }
}
