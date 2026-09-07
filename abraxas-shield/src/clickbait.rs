/// Clickbait detection — ported from content-scan/scanner.py.
/// Rule-based only (no LightGBM); architecture is ready for a model in a later phase.
use regex::Regex;
use std::sync::OnceLock;

static NUMBER_CLAIM_RE: OnceLock<Regex> = OnceLock::new();

fn number_claim_re() -> &'static Regex {
    NUMBER_CLAIM_RE.get_or_init(|| {
        Regex::new(r"(?i)\b\d+\s*(reasons?|things?|ways?|secrets?|facts?|tips?|tricks?|steps?|hacks?)\b")
            .expect("number claim regex is valid")
    })
}

const CLICKBAIT_KEYWORDS: &[&str] = &[
    "you won't believe",
    "shocking",
    "this is why",
    "what happens next",
    "mind blown",
    "blew my mind",
    "this will",
    "secret revealed",
    "insane",
    "omg",
    "wtf",
    "unbelievable",
    "jaw dropping",
    "life changing",
    "must see",
    "going viral",
    "breaks the internet",
    "nobody is talking about",
    "wait for it",
    "they don't want you to know",
    "doctors hate",
    "one weird trick",
    "exposed",
    "banned",
    "censored",
    "this changes everything",
    "finally revealed",
    "gone wrong",
    "gone right",
    "reaction",
    "i can't believe",
    "look what",
    "you need to see this",
    "share before deleted",
    "watch before removed",
    "only real fans",
    "first to know",
    "exclusive",
    "limited time",
];

#[allow(dead_code)]
pub struct ClickbaitResult {
    pub score: f64,
    pub is_clickbait: bool,
    pub signals: Vec<String>,
}

struct TextFeatures {
    caps_ratio: f64,
    exclamation_count: usize,
    question_count: usize,
    keyword_hits: usize,
    /// Mirrors the Python feature set; will be used when the ML model is added.
    #[allow(dead_code)]
    word_count: usize,
    has_number_claim: bool,
}

fn extract_features(text: &str) -> TextFeatures {
    if text.is_empty() {
        return TextFeatures {
            caps_ratio: 0.0,
            exclamation_count: 0,
            question_count: 0,
            keyword_hits: 0,
            word_count: 0,
            has_number_claim: false,
        };
    }

    let alpha_chars: Vec<char> = text.chars().filter(|c| c.is_alphabetic()).collect();
    let upper_count = alpha_chars.iter().filter(|c| c.is_uppercase()).count();
    let caps_ratio = if alpha_chars.is_empty() {
        0.0
    } else {
        upper_count as f64 / alpha_chars.len() as f64
    };

    let exclamation_count = text.chars().filter(|&c| c == '!').count();
    let question_count = text.chars().filter(|&c| c == '?').count();
    let word_count = text.split_whitespace().count();

    let text_lower = text.to_lowercase();
    let keyword_hits = CLICKBAIT_KEYWORDS
        .iter()
        .filter(|&&kw| text_lower.contains(kw))
        .count();

    let has_number_claim = number_claim_re().is_match(&text_lower);

    TextFeatures {
        caps_ratio,
        exclamation_count,
        question_count,
        keyword_hits,
        word_count,
        has_number_claim,
    }
}

fn rule_based_score(f: &TextFeatures) -> f64 {
    let mut score = 0.0_f64;
    score += (f.caps_ratio * 2.0).min(0.4);
    score += ((f.exclamation_count as f64) * 0.08).min(0.2);
    score += ((f.keyword_hits as f64) * 0.12).min(0.25);
    if f.has_number_claim {
        score += 0.15;
    }
    score += ((f.question_count as f64) * 0.04).min(0.1);
    score.min(1.0)
}

pub fn detect(text: &str) -> ClickbaitResult {
    let f = extract_features(text);
    let score = (rule_based_score(&f) * 10000.0).round() / 10000.0;

    let mut signals = Vec::new();
    if f.caps_ratio > 0.3 {
        signals.push("high_caps_ratio".to_string());
    }
    if f.exclamation_count >= 2 {
        signals.push("multiple_exclamations".to_string());
    }
    if f.keyword_hits >= 2 {
        signals.push("clickbait_keywords".to_string());
    }
    if f.has_number_claim {
        signals.push("numbered_list_claim".to_string());
    }
    if f.question_count >= 2 {
        signals.push("multiple_questions".to_string());
    }

    ClickbaitResult {
        score,
        is_clickbait: score >= 0.55,
        signals,
    }
}

