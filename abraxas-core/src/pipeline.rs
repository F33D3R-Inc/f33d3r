use deadpool_postgres::Pool;
use serde::Deserialize;
use tracing::warn;

/// Ranking event published by Nantar on the `ranking.events` topic.
///
/// Nantar serialises these fields (see feed-engine/internal/handler/post.go):
///   event       — always "post.interaction"
///   event_type  — like | unlike | repost | unrepost | post | view
///   post_id     — UUID string
///   actor_pial  — publisher PIAL UUID
///   created_at  — RFC3339 timestamp
///
/// `actor_pial` is the canonical field name from Nantar. `pial_id` is accepted
/// as an alias for forward-compat with any future producer variant.
#[derive(Deserialize)]
pub struct RankingEvent {
    pub event_type: String,
    pub post_id: String,
    /// PIAL of the user who triggered this event.
    /// Accepts both `actor_pial` (current Nantar) and `pial_id` (alias).
    #[serde(alias = "actor_pial")]
    pub pial_id: Option<String>,
}

impl RankingEvent {
    /// Returns the resolved PIAL string if present.
    pub fn resolved_pial(&self) -> Option<&str> {
        self.pial_id.as_deref()
    }
}

/// Map an event_type to (signal_type, weight).
/// Returns None for unknown/unmapped types (caller should skip + warn).
fn signal_weight(event_type: &str) -> Option<(&'static str, f64)> {
    match event_type {
        "like"          => Some(("like",         1.0)),
        "unlike"        => Some(("unlike",       -0.5)),
        "repost"        => Some(("repost",        3.0)),
        "unrepost"      => Some(("unrepost",     -1.0)),
        "post"          => Some(("post_created",  0.5)),
        "view"          => Some(("view",          0.1)),
        _               => None,
    }
}

pub async fn process(
    pool: &Pool,
    event: RankingEvent,
) -> Result<(), Box<dyn std::error::Error>> {
    let pial = match event.resolved_pial() {
        Some(p) => p.to_owned(),
        None => {
            warn!(
                event_type = %event.event_type,
                post_id    = %event.post_id,
                "Ranking event has no PIAL — skipping"
            );
            return Ok(());
        }
    };

    let (signal_type, weight) = match signal_weight(&event.event_type) {
        Some(sw) => sw,
        None => {
            warn!(
                event_type = %event.event_type,
                "Unknown ranking event_type — skipping"
            );
            return Ok(());
        }
    };

    crate::db::record_signal(pool, &event.post_id, signal_type, &pial, weight).await?;
    crate::db::update_post_score(pool, &event.post_id).await?;

    Ok(())
}
