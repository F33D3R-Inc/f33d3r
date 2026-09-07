use std::env;

pub struct Config {
    pub kafka_brokers: String,
    pub database_url: String,
    pub scan_version: String,
}

impl Config {
    pub fn from_env() -> Self {
        Config {
            kafka_brokers: env::var("KAFKA_BROKERS")
                .unwrap_or_else(|_| "sitra-achra:9092".to_string()),
            database_url: env::var("DATABASE_URL")
                .expect("DATABASE_URL must be set"),
            scan_version: env::var("SCAN_VERSION")
                .unwrap_or_else(|_| "3.0".to_string()),
        }
    }
}
