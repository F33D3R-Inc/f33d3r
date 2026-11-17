//! Policy: the one place scores become a verdict.
//!
//! The worker measures (nsfw, gore, toxicity, clickbait, spam, labels); this
//! module decides. Every cut is here, with the reason for it, and the same
//! function decides the first pass and the ML deep pass — the deep pass only
//! brings more evidence (OCR, transcript, stronger models), never a different
//! rulebook.
//!
//! Severity order: block > human_review > age_gate > approve. A `human_review`
//! outcome is not a person's job yet: it opens an ml_review item, and only
//! what the deep pass still cannot settle reaches an admin.

use std::collections::HashSet;

use serde_json::{json, Map, Value};

use crate::verdicts::Verdict;
use crate::worker::Analysis;

// ── Cuts ─────────────────────────────────────────────────────────────────────

/// Nudity at or above this is adult content: age-gated, not removed. Nudity is
/// lawful on this platform; the gate exists so it is never shown to someone
/// who has not opted in.
pub const NSFW_AGE_GATE: f64 = 0.70;
/// Between this and NSFW_AGE_GATE the classifier is unsure whether it is
/// looking at nudity at all. Gating on a guess hides lawful work from most of
/// its audience; approving on one shows nudity to people who did not opt in.
/// Neither error is cheap, so the band goes to review.
pub const NSFW_REVIEW: f64 = 0.45;
/// Graphic violence is never auto-removed (news, documentary, history) and
/// never auto-approved above this: a person decides context.
pub const GORE_REVIEW: f64 = 0.60;
/// Toxicity is the worker's phrase rule over every text surface (typed text,
/// OCR, transcript): 1.0 when a listed phrase is present, 0.0 when surfaces
/// were examined and none is. A listed HATE phrase removes the content without
/// a person — the one text class treated that way. Violence and self-harm
/// phrases are context (news, recovery stories, fiction) and go to review.
pub const TOXICITY_REVIEW: f64 = 0.60;
pub const CLICKBAIT_REVIEW: f64 = 0.70;
pub const SPAM_REVIEW: f64 = 0.80;

/// Publishing rate (works + visions per hour, per PIAL) above which the
/// account's output is reviewed. Ten an hour is a busy human; twenty is a
/// script.
pub const RATE_REVIEW_PER_HOUR: i64 = 10;
pub const RATE_HIGH_PER_HOUR: i64 = 20;

// ── Inputs ───────────────────────────────────────────────────────────────────

#[derive(Debug, Default, Clone)]
pub struct Behaviour {
    /// Subjects this PIAL published in the last hour, this one included.
    pub rate_1h: i64,
    /// The same body text from this PIAL in the last 24 hours.
    pub duplicate_body: bool,
    /// Account age at publication, when the event carried it.
    pub account_age_hours: Option<f64>,
    /// Subjects this PIAL published in the last 72 hours.
    pub published_72h: i64,
}

pub struct Inputs<'a> {
    pub text: Option<&'a Analysis>,
    /// (asset id, analysis). Encrypted media has none and is listed in
    /// `opaque_media` instead.
    pub media: &'a [(String, Analysis)],
    pub opaque_media: usize,
    /// The integrity verdicts of the media this subject carries.
    pub asset_verdicts: &'a [(String, Verdict)],
    pub behaviour: &'a Behaviour,
    pub is_adult_creator: bool,
    pub unresolved_media: &'a [String],
    /// Signals reviewers have overturned often enough to stop trusting
    /// (see review::suppressed_signals). Suppression only ever lowers a
    /// review trigger; it never lifts a block.
    pub suppressed: &'a HashSet<String>,
}

pub struct Decision {
    pub verdict: Verdict,
    pub signals: Vec<String>,
    /// Numbers only: every value is a score in [0, 1]. Consumers of
    /// safety.events and the review API decode scores as numbers, so anything
    /// that is not a measurement lives in `labels` or `signals`.
    pub scores: Value,
    /// What the detectors named (sorted, distinct), e.g. "nsfw:exposed_breast".
    pub labels: Vec<String>,
    pub reason: String,
}

fn max_of(media: &[(String, Analysis)], f: impl Fn(&Analysis) -> Option<f64>) -> f64 {
    media.iter().filter_map(|(_, a)| f(a)).fold(0.0, f64::max)
}

pub fn decide(i: &Inputs<'_>) -> Decision {
    let mut signals: Vec<String> = Vec::new();
    let mut blocks: Vec<String> = Vec::new();
    let mut reviews: Vec<String> = Vec::new();

    // Integrity verdicts of the carried media come first: banned media blocks
    // the work carrying it, media under review holds it.
    for (asset, v) in i.asset_verdicts {
        match v {
            Verdict::Block => blocks.push(format!("asset_blocked:{asset}")),
            Verdict::HumanReview => reviews.push(format!("asset_under_review:{asset}")),
            _ => {}
        }
    }
    for u in i.unresolved_media {
        reviews.push(format!("media_unresolved:{u}"));
    }

    let nsfw = max_of(i.media, |a| a.nsfw_score());
    let gore = max_of(i.media, |a| a.gore);
    let text = i.text;
    let all: Vec<&Analysis> = text.into_iter().chain(i.media.iter().map(|(_, a)| a)).collect();
    // Toxicity covers OCR and transcripts as well as typed text: the worker
    // scores every surface it read, inside whichever analysis read it.
    let toxicity = all.iter().filter_map(|a| a.toxicity).fold(0.0, f64::max);
    let clickbait = text.and_then(|t| t.clickbait).unwrap_or(0.0);
    let spam = all.iter().filter_map(|a| a.spam).fold(0.0, f64::max);
    let hate: Vec<&String> = all.iter().flat_map(|a| a.toxicity_signals.hate.iter()).collect();
    let violence = all.iter().any(|a| !a.toxicity_signals.violence.is_empty());
    let self_harm = all.iter().any(|a| !a.toxicity_signals.self_harm.is_empty());
    let labels: HashSet<String> = all.iter().flat_map(|a| a.labels.iter().cloned()).collect();

    if !hate.is_empty() {
        let mut h: Vec<&str> = hate.iter().map(|s| s.as_str()).collect();
        h.sort();
        h.dedup();
        blocks.push(format!("hate:{}", h.join("|")));
    }

    let mut review = |sig: String| {
        if !i.suppressed.contains(sig.split(':').next().unwrap_or(&sig)) {
            reviews.push(sig);
        } else {
            signals.push(format!("suppressed:{sig}"));
        }
    };
    if violence {
        review("violence".into());
    }
    if self_harm {
        review("self_harm".into());
    }
    if hate.is_empty() && toxicity >= TOXICITY_REVIEW && !violence && !self_harm {
        review(format!("toxicity:{toxicity:.2}"));
    }
    if gore >= GORE_REVIEW {
        review(format!("gore:{gore:.2}"));
    }
    if clickbait >= CLICKBAIT_REVIEW {
        review(format!("clickbait:{clickbait:.2}"));
    }
    if spam >= SPAM_REVIEW {
        review(format!("spam:{spam:.2}"));
    }
    let b = i.behaviour;
    if b.rate_1h > RATE_HIGH_PER_HOUR {
        review(format!("rate_high:{}", b.rate_1h));
    } else if b.rate_1h > RATE_REVIEW_PER_HOUR {
        review(format!("rate_moderate:{}", b.rate_1h));
    }
    if b.duplicate_body {
        review("duplicate_body".into());
    }
    if let Some(age) = b.account_age_hours {
        // A day-old account publishing in bulk is the bot signature; an
        // established account doing the same is caught by the rate cut above.
        if (age < 24.0 && b.published_72h > 5) || (age < 72.0 && b.published_72h > 15) {
            review(format!("new_account_burst:{}", b.published_72h));
        }
    }
    // Nudity: the uncertain band is a review — unless the creator is a
    // declared adult creator. Their uncertain nudity is age-gated instead of
    // held for a person, because 18+ media is what they declared they post.
    if nsfw >= NSFW_REVIEW && nsfw < NSFW_AGE_GATE && !i.is_adult_creator {
        review(format!("nsfw_uncertain:{nsfw:.2}"));
    }
    if i.opaque_media > 0 {
        signals.push(format!("opaque_media:{}", i.opaque_media));
    }

    // Only 18+ material is age-gated: nudity in the media. Being a declared
    // adult creator never gates a post by itself — an adult creator's text, or
    // a photo with no nudity in it, is an ordinary post. It only moves the
    // uncertain nudity band from review to age-gate (above).
    let age_gate = nsfw >= NSFW_AGE_GATE || (i.is_adult_creator && nsfw >= NSFW_REVIEW);
    if nsfw >= NSFW_AGE_GATE {
        signals.push(format!("nsfw:{nsfw:.2}"));
    }
    if i.is_adult_creator {
        signals.push("adult_creator".into());
    }

    let (verdict, reason) = if !blocks.is_empty() {
        (Verdict::Block, format!("blocked: {}", blocks.join(", ")))
    } else if !reviews.is_empty() {
        (Verdict::HumanReview, format!("review: {}", reviews.join(", ")))
    } else if age_gate {
        (Verdict::AgeGate, "adult content".to_string())
    } else {
        (Verdict::Approve, "clean".to_string())
    };
    signals.extend(blocks);
    signals.extend(reviews);
    signals.sort();
    signals.dedup();

    let mut scores = Map::new();
    scores.insert("nsfw".into(), json!(nsfw));
    scores.insert("gore".into(), json!(gore));
    scores.insert("toxicity".into(), json!(toxicity));
    scores.insert("clickbait".into(), json!(clickbait));
    scores.insert("spam".into(), json!(spam));
    let mut labels: Vec<String> = labels.into_iter().collect();
    labels.sort();

    Decision { verdict, signals, scores: Value::Object(scores), labels, reason }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn text(tox: f64, hate: &[&str]) -> Analysis {
        Analysis {
            toxicity: Some(tox),
            toxicity_signals: crate::worker::ToxicitySignals {
                hate: hate.iter().map(|s| s.to_string()).collect(),
                ..Default::default()
            },
            clickbait: Some(0.0),
            spam: Some(0.0),
            ..Default::default()
        }
    }

    fn run(t: Option<&Analysis>, media: &[(String, Analysis)], adult: bool) -> Decision {
        let s = HashSet::new();
        let b = Behaviour::default();
        decide(&Inputs {
            text: t,
            media,
            opaque_media: 0,
            asset_verdicts: &[],
            behaviour: &b,
            is_adult_creator: adult,
            unresolved_media: &[],
            suppressed: &s,
        })
    }

    #[test]
    fn hate_blocks_and_other_toxicity_reviews() {
        assert_eq!(run(Some(&text(1.0, &["text:slur"])), &[], false).verdict, Verdict::Block);
        assert_eq!(run(Some(&text(1.0, &[])), &[], false).verdict, Verdict::HumanReview);
        assert_eq!(run(Some(&text(0.0, &[])), &[], false).verdict, Verdict::Approve);
    }

    #[test]
    fn nudity_is_gated_and_its_uncertain_band_reviewed() {
        let m = |n: f64| vec![("a".to_string(), Analysis {
            nsfw: Some(crate::worker::Nsfw { score: n, labels: vec![] }),
            gore: Some(0.0),
            ..Default::default()
        })];
        assert_eq!(run(None, &m(0.9), false).verdict, Verdict::AgeGate);
        assert_eq!(run(None, &m(0.5), false).verdict, Verdict::HumanReview);
        assert_eq!(run(None, &m(0.5), true).verdict, Verdict::AgeGate);
        assert_eq!(run(None, &m(0.1), false).verdict, Verdict::Approve);
        // Being an adult creator is not 18+ material: their photo with no
        // nudity, and their text, are ordinary posts.
        assert_eq!(run(None, &m(0.1), true).verdict, Verdict::Approve);
        assert_eq!(run(Some(&text(0.0, &[])), &[], true).verdict, Verdict::Approve);
    }
}
