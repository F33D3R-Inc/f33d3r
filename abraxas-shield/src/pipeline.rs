/// Shield pipeline — orchestrates all detectors and produces a ShieldDecision.
use deadpool_postgres::Pool;
use serde::{Deserialize, Serialize};
use tracing::warn;

use crate::{bot, clickbait, nsfw, spam, toxicity};
use crate::nsfw::NsfwOnnxDetector;

#[derive(Debug, Deserialize, Clone)]
pub struct ContentEvent {
    pub post_id: String,
    /// Present when the content was created via the Malkuth signed-work path
    /// (works table). Absent / empty for legacy posts-table content.
    #[serde(default)]
    pub work_id: String,
    pub pial_id: String,
    pub body: String,
    #[serde(default)]
    #[allow(dead_code)]
    pub has_media: bool,
    #[serde(default)]
    #[allow(dead_code)]
    pub has_poll: bool,
    #[serde(default)]
    pub scan_state: String,
    #[allow(dead_code)]
    pub creator_type: Option<String>,
    /// Optional URL of the primary media asset attached to this post.
    /// When present and a visual detector is loaded, Shield will fetch
    /// the image and run ONNX inference on it.
    #[serde(default)]
    pub media_url: Option<String>,
}

#[derive(Debug, Serialize, Clone)]
pub struct ShieldDecision {
    pub post_id: String,
    pub pial_id: String,
    /// approve | age_gate | human_review | auto_block
    pub recommendation: String,
    /// clean | age_gate | review | block
    pub risk_level: String,
    pub signals: Vec<String>,
    pub nudity_score: f64,
    pub gore_score: f64,
    pub clickbait_score: f64,
    pub hate_signals: Vec<String>,
    pub violence_signals: Vec<String>,
    pub self_harm_signals: Vec<String>,
    pub scan_version: String,
}

fn make_decision(
    recommendation: &str,
    risk_level: &str,
    signals: Vec<String>,
    nudity_score: f64,
    clickbait_score: f64,
    hate_signals: Vec<String>,
    violence_signals: Vec<String>,
    self_harm_signals: Vec<String>,
    post_id: &str,
    pial_id: &str,
    scan_version: &str,
) -> ShieldDecision {
    ShieldDecision {
        post_id: post_id.to_string(),
        pial_id: pial_id.to_string(),
        recommendation: recommendation.to_string(),
        risk_level: risk_level.to_string(),
        signals,
        nudity_score,
        gore_score: 0.0, // Phase 3.1
        clickbait_score,
        hate_signals,
        violence_signals,
        self_harm_signals,
        scan_version: scan_version.to_string(),
    }
}

/// Fetch image bytes from a URL using a blocking reqwest client.
/// Only compiled when the visual-nsfw feature is enabled.
#[cfg(feature = "visual-nsfw")]
async fn fetch_image_bytes(url: &str) -> Option<Vec<u8>> {
    let url = url.to_string();
    tokio::task::spawn_blocking(move || {
        match reqwest::blocking::get(&url) {
            Ok(resp) => {
                if resp.status().is_success() {
                    resp.bytes().ok().map(|b| b.to_vec())
                } else {
                    warn!(url = %url, status = %resp.status(), "Non-success fetching media for NSFW scan");
                    None
                }
            }
            Err(e) => {
                warn!(url = %url, err = %e, "Failed to fetch media for NSFW scan");
                None
            }
        }
    })
    .await
    .unwrap_or(None)
}

/// Run the full detection pipeline for a content event.
/// `detector` is `None` when no ONNX model is loaded (text-only mode).
/// Decision priority matches Python compute_risk exactly.
pub async fn run(
    pool: &Pool,
    event: &ContentEvent,
    is_adult_creator: bool,
    scan_version: &str,
    detector: Option<&NsfwOnnxDetector>,
) -> ShieldDecision {
    let body = &event.body;
    let post_id = &event.post_id;
    let pial_id = &event.pial_id;
    let suppressed = crate::db::load_suppressed_signals(pool).await;

    // ── All detectors run in parallel ────────────────────────────────────────
    let clickbait_res = clickbait::detect(body);
    let hate_sigs = toxicity::detect_hate(body);
    let violence_sigs = toxicity::detect_violence(body);
    let self_harm_sigs = toxicity::detect_self_harm(body);
    let nsfw_text_res = nsfw::detect_text_signals(body);
    let spam_res = spam::detect(pool, pial_id, body).await;
    let bot_res = bot::detect(pool, pial_id).await;

    // ── Visual NSFW detection (only when visual-nsfw feature compiled in) ────
    #[cfg(feature = "visual-nsfw")]
    let visual_nudity_score: f64 = if let Some(url) = &event.media_url {
        if detector.is_some() {
            match fetch_image_bytes(url).await {
                Some(bytes) => nsfw::detect_visual(detector, &bytes).nudity_score,
                None => 0.0,
            }
        } else { 0.0 }
    } else { 0.0 };
    #[cfg(not(feature = "visual-nsfw"))]
    let visual_nudity_score: f64 = 0.0;

    // Take the higher of text and visual scores.
    let nudity_score = nsfw_text_res.nudity_score.max(visual_nudity_score);
    let clickbait_score = clickbait_res.score;

    // Build the combined signals list.
    let mut signals: Vec<String> = Vec::new();
    if nudity_score >= 0.70 {
        signals.push("nudity_high".to_string());
    } else if nudity_score >= 0.45 {
        signals.push("nudity_moderate".to_string());
    }
    if visual_nudity_score > 0.0 {
        signals.push(format!("visual_nudity:{:.2}", visual_nudity_score));
    }
    if clickbait_score >= 0.70 {
        signals.push("clickbait_high".to_string());
    } else if clickbait_score >= 0.55 {
        signals.push("clickbait_moderate".to_string());
    }
    signals.extend(clickbait_res.signals.iter().cloned());
    signals.extend(nsfw_text_res.signals.iter().cloned());
    signals.extend(hate_sigs.iter().cloned());
    signals.extend(violence_sigs.iter().cloned());
    signals.extend(self_harm_sigs.iter().cloned());
    signals.extend(spam_res.signals.iter().cloned());
    signals.extend(bot_res.signals.iter().cloned());

    // Remove admin-suppressed signals (confirmed false positives via feedback loop).
    let hate_sigs: Vec<String> = hate_sigs.into_iter().filter(|s| !suppressed.contains(s)).collect();
    let violence_sigs: Vec<String> = violence_sigs.into_iter().filter(|s| !suppressed.contains(s)).collect();
    let self_harm_sigs: Vec<String> = self_harm_sigs.into_iter().filter(|s| !suppressed.contains(s)).collect();

    // ── Decision priority — same as Python compute_risk ───────────────────────

    // 1. Hate signals → auto_block (hard gate, no exceptions)
    if !hate_sigs.is_empty() {
        return make_decision(
            "auto_block",
            "block",
            signals,
            nudity_score,
            clickbait_score,
            hate_sigs,
            violence_sigs,
            self_harm_sigs,
            post_id,
            pial_id,
            scan_version,
        );
    }

    // 2. Violence / self-harm → human_review (hard gate)
    if !violence_sigs.is_empty() || !self_harm_sigs.is_empty() {
        return make_decision(
            "human_review",
            "review",
            signals,
            nudity_score,
            clickbait_score,
            hate_sigs,
            violence_sigs,
            self_harm_sigs,
            post_id,
            pial_id,
            scan_version,
        );
    }

    // 3. Adult creator → age_gate all their content
    //    (hard gates above still apply; this comes before nudity thresholds)
    if is_adult_creator {
        return make_decision(
            "age_gate",
            "age_gate",
            signals,
            nudity_score,
            clickbait_score,
            hate_sigs,
            violence_sigs,
            self_harm_sigs,
            post_id,
            pial_id,
            scan_version,
        );
    }

    // 4. High NSFW score → age_gate
    if nudity_score >= 0.70 {
        return make_decision(
            "age_gate",
            "age_gate",
            signals,
            nudity_score,
            clickbait_score,
            hate_sigs,
            violence_sigs,
            self_harm_sigs,
            post_id,
            pial_id,
            scan_version,
        );
    }

    // 5. Moderate NSFW → human_review
    if nudity_score >= 0.45 {
        return make_decision(
            "human_review",
            "review",
            signals,
            nudity_score,
            clickbait_score,
            hate_sigs,
            violence_sigs,
            self_harm_sigs,
            post_id,
            pial_id,
            scan_version,
        );
    }

    // 6. Clickbait high → human_review
    if clickbait_score >= 0.70 {
        return make_decision(
            "human_review",
            "review",
            signals,
            nudity_score,
            clickbait_score,
            hate_sigs,
            violence_sigs,
            self_harm_sigs,
            post_id,
            pial_id,
            scan_version,
        );
    }

    // 7. Bot suspected → human_review
    if bot_res.is_suspected_bot {
        return make_decision(
            "human_review",
            "review",
            signals,
            nudity_score,
            clickbait_score,
            hate_sigs,
            violence_sigs,
            self_harm_sigs,
            post_id,
            pial_id,
            scan_version,
        );
    }

    // 8. Spam high rate → human_review
    if spam_res.is_spam {
        return make_decision(
            "human_review",
            "review",
            signals,
            nudity_score,
            clickbait_score,
            hate_sigs,
            violence_sigs,
            self_harm_sigs,
            post_id,
            pial_id,
            scan_version,
        );
    }

    // 9. All clear → approve
    make_decision(
        "approve",
        "clean",
        signals,
        nudity_score,
        clickbait_score,
        hate_sigs,
        violence_sigs,
        self_harm_sigs,
        post_id,
        pial_id,
        scan_version,
    )
}
