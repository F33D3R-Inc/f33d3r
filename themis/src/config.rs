use anyhow::{bail, Result};

pub struct Config {
    pub database_url: String,
    pub nantar_url: String,
    pub elohim_veni_url: String,
    pub internal_api_key: String,
    pub mempool_api_url: String,
    pub xrp_ws_url: String,
    /// The ECDH private key as base64 PKCS8 DER, when the deployment supplies it.
    pub themis_privkey_b64: String,
    /// The directory holding the persisted ECDH key file, when the deployment
    /// lets Themis generate and keep its own. One of the two is required.
    pub themis_key_dir: String,
    pub confirmation_threshold_low: u32,
    pub confirmation_threshold_high: u32,
    pub ain_soph_url: String,
    pub verity_url: String,
    /// Caeor: the media authority every encrypted listing body must belong to.
    pub caeor_url: String,
    /// Sitra Achra brokers, for the commerce.events relay.
    pub kafka_brokers: String,
}

fn required(name: &str) -> Result<String> {
    match std::env::var(name) {
        Ok(v) if !v.trim().is_empty() => Ok(v),
        _ => bail!("{name} is required"),
    }
}

impl Config {
    pub fn from_env() -> Result<Self> {
        let cfg = Self {
            database_url: required("DATABASE_URL")?,
            nantar_url: std::env::var("NANTAR_URL")
                .unwrap_or_else(|_| "http://feed-engine:8081".into()),
            elohim_veni_url: std::env::var("ELOHIM_VENI_URL")
                .unwrap_or_else(|_| "http://elohim-veni:8093".into()),
            // Themis moves value. A brain that cannot tell a peer from a
            // stranger must not start.
            internal_api_key: required("INTERNAL_API_KEY")?,
            mempool_api_url: std::env::var("MEMPOOL_API_URL")
                .unwrap_or_else(|_| "https://mempool.space/api".into()),
            xrp_ws_url: std::env::var("XRP_WS_URL").unwrap_or_else(|_| "wss://xrpl.ws".into()),
            themis_privkey_b64: std::env::var("THEMIS_ECDH_PRIVKEY_B64").unwrap_or_default(),
            themis_key_dir: std::env::var("THEMIS_ECDH_KEY_DIR").unwrap_or_default(),
            confirmation_threshold_low: std::env::var("CONF_THRESHOLD_LOW")
                .ok()
                .and_then(|v| v.parse().ok())
                .unwrap_or(2),
            confirmation_threshold_high: std::env::var("CONF_THRESHOLD_HIGH")
                .ok()
                .and_then(|v| v.parse().ok())
                .unwrap_or(3),
            ain_soph_url: std::env::var("AIN_SOPH_URL")
                .unwrap_or_else(|_| "http://ain-soph:8089".into()),
            verity_url: std::env::var("VERITY_URL").unwrap_or_else(|_| "http://verity:8095".into()),
            caeor_url: required("CAEOR_URL")?,
            // commerce.events is how feed-engine's subscription projection
            // learns anything; without a broker it would silently go stale.
            kafka_brokers: required("KAFKA_BROKERS")?,
        };
        if cfg.themis_privkey_b64.trim().is_empty() && cfg.themis_key_dir.trim().is_empty() {
            bail!(
                "THEMIS_ECDH_PRIVKEY_B64 or THEMIS_ECDH_KEY_DIR is required: every listing's \
                 content key is wrapped for Themis's ECDH key, and a key that changes on restart \
                 strands every listing ever made"
            );
        }
        Ok(cfg)
    }
}
