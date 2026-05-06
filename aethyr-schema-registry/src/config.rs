use anyhow::Result;
use serde::Deserialize;

#[derive(Debug, Clone, Deserialize)]
pub struct AppConfig {
    pub server: ServerConfig,
    pub database: DatabaseConfig,
    pub registry: RegistryConfig,
}

#[derive(Debug, Clone, Deserialize)]
pub struct ServerConfig {
    pub host: String,
    pub port: u16,
}

#[derive(Debug, Clone, Deserialize)]
pub struct DatabaseConfig {
    pub url: String,
    pub max_connections: u32,
    pub connect_timeout: u64,
}

#[derive(Debug, Clone, Deserialize)]
pub struct RegistryConfig {
    pub stale_threshold_secs: i64,
    pub strict_compatibility: bool,
}

impl AppConfig {
    pub fn load() -> Result<Self> {
        let cfg = config::Config::builder()
            .add_source(config::File::with_name("config/default").required(false))
            .add_source(config::Environment::with_prefix("REGISTRY").separator("__"))
            // DATABASE_URL override
            .set_override_option("database.url", std::env::var("DATABASE_URL").ok())?
            .set_override_option("server.port", std::env::var("PORT").ok())?
            .build()?;

        Ok(cfg.try_deserialize()?)
    }
}
