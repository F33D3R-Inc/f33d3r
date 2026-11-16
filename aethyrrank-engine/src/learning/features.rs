//! The context each work was ranked in, per viewer.
//!
//! A LinUCB update needs the feature vector x the work was *scored* with:
//! `A ← A + xxᵀ, b ← b + r·x`. /rank records that x here for every work it
//! ranks, under the viewer's account, the work, the surface and the model key
//! the viewer's model lives under. A ranking signal (learning::apply) names the
//! viewer and the work, and finds the newest context it was shown in.
//!
//! When there is no context — the work reached the viewer on a surface that is
//! not ranked (Following, a profile, a direct link), or longer ago than the TTL
//! — there is nothing honest to learn from, and the signal is skipped and
//! counted. A placeholder x would teach the model a pattern the viewer never saw.
//!
//! Bounded: each viewer keeps at most `MAX_PER_VIEWER` contexts (a few ranked
//! windows), each for at most `TTL`. The sweep removes what has expired.

use std::collections::VecDeque;
use std::time::{Duration, Instant};

use dashmap::DashMap;

/// How long a ranked context stays usable for learning.
pub const TTL: Duration = Duration::from_secs(6 * 3600);

/// Two full candidate windows (feed-engine ranks at most 120 per request).
pub const MAX_PER_VIEWER: usize = 240;

/// One work as one viewer was shown it.
#[derive(Clone, Debug)]
pub struct RankedContext {
    pub work_id: String,
    pub surface: String,
    /// `"{viewer_or_nexus_shard}:{surface}"` — the model this context trains.
    pub model_key: String,
    pub creator_account: String,
    pub creator_pial: Option<String>,
    pub features: Vec<f64>,
    /// 1-based position in the ranked output.
    pub position: usize,
    pub ranked_at: Instant,
}

impl RankedContext {
    fn live(&self, now: Instant) -> bool {
        now.duration_since(self.ranked_at) < TTL
    }

    fn same_slot(&self, other: &RankedContext) -> bool {
        self.work_id == other.work_id
            && self.surface == other.surface
            && self.model_key == other.model_key
    }
}

#[derive(Default)]
struct ViewerContexts {
    pial: Option<String>,
    /// Oldest first: a context re-ranked is moved to the back.
    entries: VecDeque<RankedContext>,
}

#[derive(Default)]
pub struct FeatureCache {
    by_viewer: DashMap<String, ViewerContexts>,
    viewer_by_pial: DashMap<String, String>,
}

impl FeatureCache {
    pub fn new() -> Self {
        Self::default()
    }

    /// Record the contexts of one /rank response for one viewer.
    pub fn record(&self, viewer_account: &str, viewer_pial: Option<&str>, contexts: Vec<RankedContext>) {
        let now = Instant::now();
        let mut viewer = self.by_viewer.entry(viewer_account.to_string()).or_default();
        if let Some(p) = viewer_pial.filter(|p| !p.is_empty()) {
            if viewer.pial.as_deref() != Some(p) {
                viewer.pial = Some(p.to_string());
                self.viewer_by_pial.insert(p.to_string(), viewer_account.to_string());
            }
        }
        for ctx in contexts {
            viewer.entries.retain(|e| !e.same_slot(&ctx));
            viewer.entries.push_back(ctx);
        }
        while viewer.entries.len() > MAX_PER_VIEWER
            || viewer.entries.front().is_some_and(|e| !e.live(now))
        {
            viewer.entries.pop_front();
        }
    }

    fn newest(&self, viewer_account: &str, pick: impl Fn(&RankedContext) -> bool) -> Option<RankedContext> {
        let now = Instant::now();
        let viewer = self.by_viewer.get(viewer_account)?;
        viewer
            .entries
            .iter()
            .rev()
            .find(|e| e.live(now) && pick(e))
            .cloned()
    }

    /// The newest context the viewer was shown this work in.
    pub fn for_work(&self, viewer_account: &str, work_id: &str) -> Option<RankedContext> {
        self.newest(viewer_account, |e| e.work_id == work_id)
    }

    /// The newest context the viewer was shown any work of this creator in.
    pub fn for_creator(&self, viewer_account: &str, creator_account: &str) -> Option<RankedContext> {
        self.newest(viewer_account, |e| e.creator_account == creator_account)
    }

    /// As `for_creator`, with both people named by PIAL (payment signals).
    pub fn for_creator_pial(&self, viewer_pial: &str, creator_pial: &str) -> Option<(String, RankedContext)> {
        let account = self.viewer_by_pial.get(viewer_pial)?.value().clone();
        let ctx = self.newest(&account, |e| e.creator_pial.as_deref() == Some(creator_pial))?;
        Some((account, ctx))
    }

    /// A deleted work is never shown again; nothing it was ranked in is kept.
    pub fn forget_work(&self, work_id: &str) {
        for mut viewer in self.by_viewer.iter_mut() {
            viewer.entries.retain(|e| e.work_id != work_id);
        }
    }

    /// Drop expired contexts and viewers left with none.
    pub fn sweep(&self) {
        let now = Instant::now();
        let mut gone_pials = Vec::new();
        self.by_viewer.retain(|_, v| {
            v.entries.retain(|e| e.live(now));
            if v.entries.is_empty() {
                if let Some(p) = v.pial.take() {
                    gone_pials.push(p);
                }
                return false;
            }
            true
        });
        for p in gone_pials {
            self.viewer_by_pial.remove(&p);
        }
    }

    /// Number of viewers holding contexts.
    pub fn viewers(&self) -> usize {
        self.by_viewer.len()
    }
}
