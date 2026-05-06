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
    pub safety: SafetyConfig,
    pub bandit: BanditConfig,
    pub model_store: ModelStoreConfig,
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
pub struct SafetyConfig {
    pub default_epsilon: f64,
    pub sfw_epsilon: f64,
    pub hard_block_score: f64,
}

#[derive(Debug, Clone, Deserialize)]
pub struct BanditConfig {
    pub alpha_ucb: f64,
    pub feature_dim: usize,
    pub exploration_count: usize,
    pub cold_start_explore_count: usize,
}

#[derive(Debug, Clone, Deserialize)]
pub struct ModelStoreConfig {
    pub surfaces: Vec<String>,
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
