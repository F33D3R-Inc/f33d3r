//! Shared application state.
//!
//! `#[derive(Clone)]`, handed to axum with `.with_state()`, the way lore and
//! herald do it. Every member is itself a cheap clone (pools, connection
//! managers, an `Arc`).

use std::sync::Arc;

use axum::extract::FromRef;
use manhattan_client::Manhattan;

use crate::auth::{ActorContext, InternalApiKey};
use crate::config::Config;
use crate::repository::redis::Hot;
use crate::security::tokens::Signer;

#[derive(Clone)]
pub struct AppState {
    pub pool: sqlx::PgPool,
    /// Redis: presence, live sets, leases, rate limits, cooldowns.
    pub hot: Hot,
    /// The naming plane. Auralis resolves identities through it and owns none.
    pub manhattan: Manhattan,
    pub herald_client: reqwest::Client,
    pub internal_api_key: InternalApiKey,
    pub signer: Signer,
    pub config: Arc<Config>,
}

impl FromRef<AppState> for InternalApiKey {
    fn from_ref(s: &AppState) -> Self {
        s.internal_api_key.clone()
    }
}

impl FromRef<AppState> for ActorContext {
    fn from_ref(s: &AppState) -> Self {
        ActorContext {
            key: s.internal_api_key.clone(),
            manhattan: s.manhattan.clone(),
        }
    }
}
