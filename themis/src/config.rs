pub struct Config {
    pub database_url: String,
    pub port: String,
    pub nantar_url: String,
    pub elohim_veni_url: String,
    pub internal_api_key: String,
    pub mempool_ws_url: String,
    pub xrp_ws_url: String,
    pub themis_privkey_b64: String,
    pub confirmation_threshold_low: u32,
    pub confirmation_threshold_high: u32,
    pub ain_soph_url: String,
    pub verity_url: String,
}

impl Config {
    pub fn from_env() -> Self {
        Self {
            database_url: std::env::var("DATABASE_URL").expect("DATABASE_URL required"),
            port: std::env::var("PORT").unwrap_or_else(|_| "8100".into()),
            nantar_url: std::env::var("NANTAR_URL")
                .unwrap_or_else(|_| "http://feed-engine:8081".into()),
            elohim_veni_url: std::env::var("ELOHIM_VENI_URL")
                .unwrap_or_else(|_| "http://elohim-veni:8093".into()),
            internal_api_key: std::env::var("INTERNAL_API_KEY").unwrap_or_default(),
            mempool_ws_url: std::env::var("MEMPOOL_WS_URL")
                .unwrap_or_else(|_| "wss://mempool.space/api/v1/ws".into()),
            xrp_ws_url: std::env::var("XRP_WS_URL").unwrap_or_else(|_| "wss://xrpl.ws".into()),
            themis_privkey_b64: std::env::var("THEMIS_ECDH_PRIVKEY_B64").unwrap_or_default(),
            confirmation_threshold_low: std::env::var("CONF_THRESHOLD_LOW")
                .ok()
                .and_then(|v| v.parse().ok())
                .unwrap_or(1),
            confirmation_threshold_high: std::env::var("CONF_THRESHOLD_HIGH")
                .ok()
                .and_then(|v| v.parse().ok())
                .unwrap_or(3),
            ain_soph_url: std::env::var("AIN_SOPH_URL")
                .unwrap_or_else(|_| "http://ain-soph:8089".into()),
            verity_url: std::env::var("VERITY_URL").unwrap_or_else(|_| "http://verity:8095".into()),
        }
    }
}
