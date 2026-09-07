/// Shield pipeline — orchestrates all detectors and produces a ShieldDecision.
use deadpool_postgres::Pool;
use serde::{Deserialize, Serialize};
use tracing::warn;

use crate::db::ContentSource;
use crate::{bot, clickbait, nsfw, spam, toxicity};
use crate::nsfw::NsfwOnnxDetector;

/// One content event off `content.events`. A single shape covers every lane:
/// `post.created` carries `work_id` (repeated as `post_id`), `vision.created`
/// carries `vision_id` and neither of the other two. Every id is optional so a
/// lane that does not use one still parses — a hard-required field here means
/// the whole event is dropped as unparseable and the content is never scanned.
#[derive(Debug, Deserialize, Clone)]
pub struct ContentEvent {
    #[serde(default)]
    pub post_id: String,
    /// Present when the content was created via the Malkuth signed-work path
    /// (works table).
    #[serde(default)]
    pub work_id: String,
    /// Present on `vision.created` — the ephemeral 24-hour Vision lane. A
    /// Vision has no id of its own: this carries the canonical
    /// `<author_pial>.<seq>` address, never a UUID.
    #[serde(default)]
    pub vision_id: String,
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
    #[serde(default)]
    #[allow(dead_code)]
    pub creator_type: Option<String>,
    /// URL of the primary media asset, Work lane spelling.
    #[serde(default)]
    pub media_url: Option<String>,
    /// Media asset URLs, Vision lane spelling.
    #[serde(default)]
    pub media_urls: Vec<String>,
}

impl ContentEvent {
    /// The row this event refers to and the lane it lives in.
    /// None when the event names no id this brain can route — the payload is
    /// unroutable and no amount of retrying will make it scannable.
    pub fn content_ref(&self) -> Option<(&str, ContentSource)> {
        if !self.vision_id.is_empty() {
            return Some((self.vision_id.as_str(), ContentSource::Visions));
        }
        if !self.work_id.is_empty() {
            return Some((self.work_id.as_str(), ContentSource::Works));
        }
        None
    }

    /// First media asset to put through visual inference, whichever lane
    /// spelling the producer used.
    #[allow(dead_code)]
    pub fn primary_media_url(&self) -> Option<&str> {
        self.media_url
            .as_deref()
            .filter(|u| !u.is_empty())
            .or_else(|| self.media_urls.iter().map(String::as_str).find(|u| !u.is_empty()))
    }
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
    content_id: &str,
    source: ContentSource,
    is_adult_creator: bool,
    scan_version: &str,
    detector: Option<&NsfwOnnxDetector>,
) -> ShieldDecision {
    let body = &event.body;
    let post_id = content_id;
    let pial_id = &event.pial_id;
    let suppressed = crate::db::load_suppressed_signals(pool).await;

    // ── All detectors run in parallel ────────────────────────────────────────
    let clickbait_res = clickbait::detect(body);
    let hate_sigs = toxicity::detect_hate(body);
    let violence_sigs = toxicity::detect_violence(body);
    let self_harm_sigs = toxicity::detect_self_harm(body);
    let nsfw_text_res = nsfw::detect_text_signals(body);
    let spam_res = spam::detect(pool, pial_id, body, content_id, source).await;
    let bot_res = bot::detect(pool, pial_id).await;

    // ── Visual NSFW detection (only when visual-nsfw feature compiled in) ────
    #[cfg(feature = "visual-nsfw")]
    let visual_nudity_score: f64 = if let Some(url) = event.primary_media_url() {
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

#[cfg(test)]
mod tests {
    use super::*;

    /// Exact payload from feed-engine publishVisionContentEvent. It carries no
    /// post_id and no work_id: a required field there drops the event as
    /// unparseable and the Vision is never scanned.
    const VISION_EVENT: &str = r#"{
        "event":"vision.created",
        "vision_id":"11111111-1111-1111-1111-111111111111.7",
        "pial_id":"22222222-2222-2222-2222-222222222222",
        "body":"hello",
        "media_urls":["https://cdn.example/a.jpg"],
        "has_media":true,
        "scan_state":"pending",
        "created_at":"2026-01-01T00:00:00Z"
    }"#;

    /// Exact payload from feed-engine publishContentEvent on the Work lane.
    const WORK_EVENT: &str = r#"{
        "event":"post.created",
        "post_id":"33333333-3333-3333-3333-333333333333",
        "work_id":"33333333-3333-3333-3333-333333333333",
        "pial_id":"22222222-2222-2222-2222-222222222222",
        "body":"hello",
        "has_media":false,
        "has_poll":false,
        "scan_state":"pending",
        "created_at":"2026-01-01T00:00:00Z"
    }"#;

    #[test]
    fn vision_event_parses_and_routes_to_the_vision_lane() {
        let ev: ContentEvent = serde_json::from_str(VISION_EVENT).expect("vision event must parse");
        let (id, source) = ev.content_ref().expect("vision event must be routable");
        assert_eq!(id, "11111111-1111-1111-1111-111111111111.7");
        assert_eq!(source, ContentSource::Visions);
        assert_eq!(ev.primary_media_url(), Some("https://cdn.example/a.jpg"));
    }

    #[test]
    fn work_event_parses_and_routes_to_the_work_lane() {
        let ev: ContentEvent = serde_json::from_str(WORK_EVENT).expect("work event must parse");
        let (id, source) = ev.content_ref().expect("work event must be routable");
        assert_eq!(id, "33333333-3333-3333-3333-333333333333");
        assert_eq!(source, ContentSource::Works);
        assert_eq!(ev.primary_media_url(), None);
    }

    #[test]
    fn event_naming_no_scannable_row_is_unroutable() {
        let ev: ContentEvent = serde_json::from_str(
            r#"{"event":"post.created","post_id":"44444444-4444-4444-4444-444444444444",
                "pial_id":"22222222-2222-2222-2222-222222222222","body":"x"}"#,
        )
        .expect("must parse");
        assert!(ev.content_ref().is_none());
    }
}
