//! Applying one ranking signal inside a batch transaction.
//!
//! Changes are made to working copies of the models (`Working`), which the
//! consumer writes and commits with the batch and only then installs in the
//! ModelStore that /rank reads. A batch that fails to commit leaves nothing
//! behind, in the database or in memory.

use std::collections::HashMap;

use anyhow::{Context, Result};
use nalgebra::DVector;
use sqlx::{Postgres, Transaction};

use crate::bandit::linucb::{reward_from_event, LinUcbModel};
use crate::learning::features::{FeatureCache, RankedContext};
use crate::learning::signal::{Effect, RankingSignal, Subject};
use crate::model_store::ModelStore;

/// The models one batch has changed, and how many updates each took.
#[derive(Default)]
pub struct Working {
    pub models: HashMap<String, (LinUcbModel, i64)>,
    /// Works deleted in this batch, forgotten by the cache once it commits.
    pub forgotten: Vec<String>,
}

impl Working {
    fn model(&mut self, store: &ModelStore, key: &str) -> &mut (LinUcbModel, i64) {
        self.models
            .entry(key.to_string())
            .or_insert_with(|| (store.working_copy(key), 0))
    }
}

/// What became of one signal, for the outcome metric.
#[derive(Debug, Clone, Copy)]
pub enum Outcome {
    Learned,
    Undone,
    Forgotten,
    /// The same event was applied before.
    Duplicate,
    /// A standing reaction that already stood.
    AlreadyStanding,
    /// An undo of a reaction that was never learned (it predates this intake,
    /// or it had no context when it was given).
    NothingToUndo,
    /// The viewer was never ranked this work (or creator) within the TTL.
    NoContext,
}

impl Outcome {
    pub fn label(self) -> &'static str {
        match self {
            Outcome::Learned => "learned",
            Outcome::Undone => "undone",
            Outcome::Forgotten => "forgotten",
            Outcome::Duplicate => "duplicate",
            Outcome::AlreadyStanding => "already_standing",
            Outcome::NothingToUndo => "nothing_to_undo",
            Outcome::NoContext => "no_context",
        }
    }
}

/// Position discount: a reaction to the first work on a page says less about
/// the features than one the viewer scrolled to. 1 at position 1.
fn position_discount(position: usize) -> f64 {
    1.0 / (1.0 + (position.max(1) as f64).ln())
}

pub async fn apply(
    tx: &mut Transaction<'_, Postgres>,
    cache: &FeatureCache,
    store: &ModelStore,
    working: &mut Working,
    s: &RankingSignal,
) -> Result<Outcome> {
    let fresh = sqlx::query(
        "INSERT INTO ranking_events_applied (event_id) VALUES ($1) ON CONFLICT (event_id) DO NOTHING",
    )
    .bind(s.event_id)
    .execute(&mut **tx)
    .await
    .context("recording the applied event")?
    .rows_affected()
        == 1;
    if !fresh {
        return Ok(Outcome::Duplicate);
    }

    let viewer = s.viewer_account.as_deref().unwrap_or_default();
    // The id a standing reaction is stored under: the work, or the account followed.
    let target = match s.action.subject() {
        Subject::Creator => s.target_account.as_deref().unwrap_or_default(),
        _ => s.work_id.as_deref().unwrap_or_default(),
    };

    match s.action.effect() {
        Effect::Forget => {
            working.forgotten.push(target.to_string());
            Ok(Outcome::Forgotten)
        }

        Effect::Undo(signal) => {
            let row: Option<(String, serde_json::Value, f64)> = sqlx::query_as(
                "DELETE FROM linucb_reactions
                  WHERE viewer_account = $1 AND target_id = $2 AND signal = $3
              RETURNING model_key, features, reward",
            )
            .bind(viewer)
            .bind(target)
            .bind(signal)
            .fetch_optional(&mut **tx)
            .await
            .context("taking back a standing reaction")?;
            let Some((model_key, features, reward)) = row else {
                return Ok(Outcome::NothingToUndo);
            };
            let x: Vec<f64> = serde_json::from_value(features)
                .with_context(|| format!("stored features of {signal} on {target}"))?;
            let x = DVector::from_vec(x);
            let entry = working.model(store, &model_key);
            entry.0.downdate(&x, reward);
            entry.1 += 1;
            Ok(Outcome::Undone)
        }

        Effect::Learn { event, standing } => {
            let found: Option<(String, RankedContext)> = match s.action.subject() {
                Subject::Work => cache.for_work(viewer, target).map(|c| (viewer.to_string(), c)),
                Subject::Creator => cache.for_creator(viewer, target).map(|c| (viewer.to_string(), c)),
                Subject::PaidCreator => cache.for_creator_pial(
                    &s.viewer_pial,
                    s.creator_pial.as_deref().unwrap_or_default(),
                ),
            };
            let Some((viewer, ctx)) = found else {
                return Ok(Outcome::NoContext);
            };
            if ctx.features.len() != store.dim() {
                anyhow::bail!(
                    "cached features of {} have {} dimensions, the model {}",
                    ctx.work_id,
                    ctx.features.len(),
                    store.dim()
                );
            }
            let reward = reward_from_event(&event) * position_discount(ctx.position);

            if let Some(signal) = standing {
                let stood = sqlx::query(
                    "INSERT INTO linucb_reactions
                         (viewer_account, target_id, signal, model_key, features, reward)
                     VALUES ($1, $2, $3, $4, $5, $6)
                     ON CONFLICT (viewer_account, target_id, signal) DO NOTHING",
                )
                .bind(&viewer)
                .bind(target)
                .bind(signal)
                .bind(&ctx.model_key)
                .bind(serde_json::to_value(&ctx.features)?)
                .bind(reward)
                .execute(&mut **tx)
                .await
                .context("recording a standing reaction")?
                .rows_affected()
                    == 0;
                if stood {
                    return Ok(Outcome::AlreadyStanding);
                }
            }

            let x = DVector::from_vec(ctx.features.clone());
            let entry = working.model(store, &ctx.model_key);
            entry.0.update(&x, reward);
            entry.1 += 1;
            Ok(Outcome::Learned)
        }
    }
}
