/// Toxicity detection — ported from content-scan/scanner.py.
/// Detects hate speech, violence threats, and self-harm encouragement via keyword patterns.

const HATE_PATTERNS: &[&str] = &[
    // White supremacist
    "white power",
    "white genocide",
    "14 words",
    "heil",
    "seig heil",
    "great replacement",
    "replace the",
    "ethnic cleansing",
    // Genocide promotion
    "gas the",
    "death to all",
    "kill all",
    "exterminate the",
    // Dehumanization
    "sub-human",
    "subhuman",
    "vermin",
    "cockroaches",
    "parasites are",
    // Extremist recruitment
    "join the movement",
    "race war",
    "day of the rope",
    // Sectarian violence promotion
    "death to jews",
    "death to muslims",
    "death to christians",
    "death to gays",
];

const VIOLENCE_PATTERNS: &[&str] = &[
    // Direct threats
    "i will kill you",
    "i'll kill you",
    "gonna kill you",
    "going to kill you",
    "i will hurt you",
    "i'll hurt you",
    "you're going to die",
    "youre going to die",
    // Mass violence
    "shoot up the",
    "bomb the",
    "blow up the",
    "mass shooting",
    "school shooting",
];

const SELF_HARM_PATTERNS: &[&str] = &[
    "kill yourself",
    "kys",
    "end your life",
    "end it all",
    "you should die",
    "go kill yourself",
    "commit suicide",
    "neck yourself",
];

fn match_patterns(text_lower: &str, patterns: &[&str], prefix: &str) -> Vec<String> {
    patterns
        .iter()
        .filter(|&&p| text_lower.contains(p))
        .map(|&p| {
            let slug = p.replace(' ', "_");
            let truncated = if slug.len() > 24 { &slug[..24] } else { &slug };
            format!("{}:{}", prefix, truncated)
        })
        .collect()
}

pub fn detect_hate(text: &str) -> Vec<String> {
    if text.is_empty() {
        return vec![];
    }
    let lower = text.to_lowercase();
    match_patterns(&lower, HATE_PATTERNS, "hate_keyword")
}

pub fn detect_violence(text: &str) -> Vec<String> {
    if text.is_empty() {
        return vec![];
    }
    let lower = text.to_lowercase();
    match_patterns(&lower, VIOLENCE_PATTERNS, "violence")
}

pub fn detect_self_harm(text: &str) -> Vec<String> {
    if text.is_empty() {
        return vec![];
    }
    let lower = text.to_lowercase();
    match_patterns(&lower, SELF_HARM_PATTERNS, "self_harm")
}
