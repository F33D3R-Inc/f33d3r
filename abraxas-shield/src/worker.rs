//! The client for content-scan, Abraxas's private ML worker.
//!
//! The worker computes; Abraxas decides. It is called only from here, over
//! HTTP, with X-Internal-Key, at CONTENT_SCAN_URL. Shapes, as content-scan
//! (server.py, fingerprint.py, audio_landmarks.py) produces them:
//!
//! ```text
//!   POST /v1/fingerprint  {path, kind}
//!     → {kind, sha256, duration?, phash?, video_segments?:[{t, phash}],
//!        audio?:{landmark_version, peaks, landmarks:[{hash, t}],
//!                robust_landmarks:[{hash, t}], chromaprint|null}}
//!     streamed; ~30 MB of JSON per hour of audio.
//!   POST /v1/analyze      {path?, kind, text?, deep}
//!     → {kind, deep, nsfw:{score, labels, label_scores, frames, tiles}|null,
//!        gore|null, ocr_text|null, transcript|null,
//!        toxicity|null, toxicity_signals:{hate[], violence[], self_harm[], sources[]},
//!        clickbait|null, clickbait_signals|null, spam|null, spam_signals|null,
//!        labels[], examined{}}
//! ```
//!
//! Wire conventions:
//!   * `path` is ABSOLUTE, under /data/media-private or /data/live-scan — the
//!     read-only volumes both containers mount at the same place (media.rs).
//!   * `phash` is 16 hex digits (imagehash's text form).
//!   * landmark `hash` is a positive int64 with a family tag in bits 60-62;
//!     `t` is the anchor's time in seconds. Landmarks are comparable only
//!     within one `landmark_version`.
//!   * null means NOT COMPUTED, never zero. Pictures (image, avatar, header,
//!     video, clip) must come back with nsfw and gore; sound (voice, audio)
//!     comes back with them null; text must come back with toxicity,
//!     clickbait and spam. Anything less is a contract failure, retried —
//!     never read as clean.
//!
//! Failure classes, because each one is handled differently upstream:
//!   Unavailable — connect failure, timeout, 5xx (503 detector_unavailable,
//!                 504 deadline_exceeded), 429. The media is pending; retried.
//!   Rejected    — 404 media_not_found or 422 (undecodable, missing stream):
//!                 no retry can read it. A person judges it.
//!   Contract    — 400, 401, 403, an unparseable answer, a missing score: the
//!                 two sides disagree about the contract (a deploy mismatch).
//!                 Retried, loudly, and escalated to a person if it persists.

use std::path::Path;

use serde::de::Error as _;
use serde::{Deserialize, Deserializer, Serialize};
use serde_json::Value;

use crate::hashing::parse_phash_hex;

#[derive(Debug)]
pub enum WorkerError {
    Unavailable(String),
    Rejected(String),
    Contract(String),
}

impl WorkerError {
    pub fn retryable(&self) -> bool {
        !matches!(self, WorkerError::Rejected(_))
    }
}

impl std::fmt::Display for WorkerError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            WorkerError::Unavailable(s) => write!(f, "worker unavailable: {s}"),
            WorkerError::Rejected(s) => write!(f, "worker rejected the media: {s}"),
            WorkerError::Contract(s) => write!(f, "worker contract violation: {s}"),
        }
    }
}

// ── Wire shapes ──────────────────────────────────────────────────────────────

fn de_phash<'de, D: Deserializer<'de>>(d: D) -> Result<i64, D::Error> {
    match Value::deserialize(d)? {
        Value::String(s) => parse_phash_hex(&s).ok_or_else(|| D::Error::custom(format!("phash {s:?} is not 16 hex digits"))),
        Value::Number(n) => n
            .as_i64()
            .or_else(|| n.as_u64().map(|u| u as i64))
            .ok_or_else(|| D::Error::custom("phash is not a 64-bit integer")),
        other => Err(D::Error::custom(format!("phash has type {other}"))),
    }
}

fn de_opt_phash<'de, D: Deserializer<'de>>(d: D) -> Result<Option<i64>, D::Error> {
    match Value::deserialize(d)? {
        Value::Null => Ok(None),
        Value::String(s) if s.is_empty() => Ok(None),
        v => de_phash(v).map(Some).map_err(D::Error::custom),
    }
}

#[derive(Debug, Clone, Deserialize)]
pub struct Segment {
    pub t: f64,
    #[serde(deserialize_with = "de_phash")]
    pub phash: i64,
}

#[derive(Debug, Clone, Deserialize)]
pub struct Landmark {
    pub hash: i64,
    pub t: f64,
}

#[derive(Debug, Clone, Deserialize)]
pub struct Audio {
    pub landmark_version: String,
    pub landmarks: Vec<Landmark>,
    pub robust_landmarks: Vec<Landmark>,
    #[serde(default)]
    pub chromaprint: Option<String>,
}

#[derive(Debug, Clone, Deserialize)]
pub struct Fingerprint {
    pub sha256: String,
    #[serde(default, deserialize_with = "de_opt_phash")]
    pub phash: Option<i64>,
    #[serde(default)]
    pub video_segments: Option<Vec<Segment>>,
    #[serde(default)]
    pub audio: Option<Audio>,
    #[serde(default)]
    pub duration: Option<f64>,
}

impl Fingerprint {
    fn validate(mut self) -> Result<Self, WorkerError> {
        self.sha256 = self.sha256.trim().to_ascii_lowercase();
        if !crate::media::is_sha256_hex(&self.sha256) {
            return Err(WorkerError::Contract(format!("sha256 {:?} is not 64 hex digits", self.sha256)));
        }
        if let Some(d) = self.duration {
            if !d.is_finite() || d < 0.0 {
                return Err(WorkerError::Contract(format!("duration {d} is not a non-negative number")));
            }
        }
        for s in self.video_segments.iter().flatten() {
            if !s.t.is_finite() || s.t < 0.0 {
                return Err(WorkerError::Contract(format!("video segment t={} is not a non-negative number", s.t)));
            }
        }
        if let Some(a) = &mut self.audio {
            for l in a.landmarks.iter().chain(a.robust_landmarks.iter()) {
                if !l.t.is_finite() || l.t < 0.0 || l.t > 2_000_000.0 {
                    return Err(WorkerError::Contract(format!("landmark t={} is outside 0..2e6 s", l.t)));
                }
            }
            let v = a.landmark_version.trim();
            if v.is_empty() || v.len() > 16 || !v.bytes().all(|c| c.is_ascii_alphanumeric() || c == b'_' || c == b'.') {
                return Err(WorkerError::Contract(format!("landmark_version {:?} is not a plain name", a.landmark_version)));
            }
            a.landmark_version = v.to_string();
            if a.chromaprint.as_deref().map(str::trim) == Some("") {
                a.chromaprint = None;
            }
        }
        Ok(self)
    }

    pub fn segments(&self) -> &[Segment] {
        self.video_segments.as_deref().unwrap_or(&[])
    }
}

#[derive(Debug, Clone, Deserialize, Serialize, Default)]
pub struct Nsfw {
    pub score: f64,
    #[serde(default)]
    pub labels: Vec<String>,
}

#[derive(Debug, Clone, Deserialize, Serialize, Default)]
pub struct ToxicitySignals {
    #[serde(default)]
    pub hate: Vec<String>,
    #[serde(default)]
    pub violence: Vec<String>,
    #[serde(default)]
    pub self_harm: Vec<String>,
    #[serde(default)]
    pub sources: Vec<String>,
}

#[derive(Debug, Clone, Deserialize, Serialize, Default)]
pub struct Analysis {
    #[serde(default)]
    pub nsfw: Option<Nsfw>,
    #[serde(default)]
    pub gore: Option<f64>,
    #[serde(default)]
    pub ocr_text: Option<String>,
    #[serde(default)]
    pub transcript: Option<String>,
    #[serde(default)]
    pub toxicity: Option<f64>,
    #[serde(default)]
    pub toxicity_signals: ToxicitySignals,
    #[serde(default)]
    pub clickbait: Option<f64>,
    #[serde(default)]
    pub clickbait_signals: Option<Vec<String>>,
    #[serde(default)]
    pub spam: Option<f64>,
    #[serde(default)]
    pub spam_signals: Option<Vec<String>>,
    /// Namespaced: "nsfw:<label>", "hate:<hit>", "violence:<hit>",
    /// "self_harm:<hit>", "clickbait:<signal>", "spam:<signal>".
    #[serde(default)]
    pub labels: Vec<String>,
}

/// What an analysis must contain to be used, by what was analysed.
#[derive(Debug, Clone, Copy, PartialEq)]
pub enum Needs {
    /// Image, avatar, header, video, clip: the picture classifiers must have run.
    Picture,
    /// Voice, audio: no picture, so nsfw and gore are legitimately null.
    Sound,
    /// Text: the language classifiers must have run.
    Text,
}

impl Needs {
    pub fn for_kind(kind: &str) -> Needs {
        match kind {
            "voice" | "audio" => Needs::Sound,
            "text" => Needs::Text,
            _ => Needs::Picture,
        }
    }
}

impl Analysis {
    pub fn nsfw_score(&self) -> Option<f64> {
        self.nsfw.as_ref().map(|n| n.score)
    }

    fn validate(mut self, needs: Needs) -> Result<Self, WorkerError> {
        for (name, v) in [
            ("nsfw.score", self.nsfw_score()),
            ("gore", self.gore),
            ("toxicity", self.toxicity),
            ("clickbait", self.clickbait),
            ("spam", self.spam),
        ] {
            if let Some(x) = v {
                if !x.is_finite() || !(0.0..=1.0).contains(&x) {
                    return Err(WorkerError::Contract(format!("{name}={x} is outside [0, 1]")));
                }
            }
        }
        let required: Vec<(&str, bool)> = match needs {
            Needs::Picture => vec![("nsfw", self.nsfw.is_some()), ("gore", self.gore.is_some())],
            Needs::Sound => vec![],
            Needs::Text => vec![
                ("toxicity", self.toxicity.is_some()),
                ("clickbait", self.clickbait.is_some()),
                ("spam", self.spam.is_some()),
            ],
        };
        let missing: Vec<&str> = required.iter().filter(|(_, ok)| !ok).map(|(n, _)| *n).collect();
        if !missing.is_empty() {
            return Err(WorkerError::Contract(format!(
                "analysis omitted {} — an uncomputed score is never read as zero",
                missing.join(", ")
            )));
        }
        self.labels.retain(|l| !l.trim().is_empty());
        for l in &mut self.labels {
            *l = l.trim().to_ascii_lowercase();
        }
        Ok(self)
    }
}

// ── Client ───────────────────────────────────────────────────────────────────

#[derive(Clone)]
pub struct Worker {
    http: reqwest::Client,
    base: String,
    key: String,
}

impl Worker {
    pub fn new(base: &str, key: &str, timeout: std::time::Duration) -> anyhow::Result<Self> {
        let http = reqwest::Client::builder()
            .connect_timeout(std::time::Duration::from_secs(5))
            .timeout(timeout)
            .build()?;
        Ok(Self { http, base: base.trim_end_matches('/').to_string(), key: key.to_string() })
    }

    async fn post<T: for<'de> Deserialize<'de>>(&self, route: &str, body: &Value) -> Result<T, WorkerError> {
        let resp = self
            .http
            .post(format!("{}{route}", self.base))
            .header("X-Internal-Key", &self.key)
            .json(body)
            .send()
            .await
            .map_err(|e| WorkerError::Unavailable(format!("{route}: {e}")))?;
        let status = resp.status();
        if status.is_success() {
            let bytes = resp
                .bytes()
                .await
                .map_err(|e| WorkerError::Unavailable(format!("{route}: reading the answer: {e}")))?;
            return serde_json::from_slice(&bytes)
                .map_err(|e| WorkerError::Contract(format!("{route}: answer does not parse: {e}")));
        }
        let detail = resp.text().await.unwrap_or_default();
        let detail: String = detail.chars().take(400).collect();
        let msg = format!("{route}: status {status}: {detail}");
        Err(match status.as_u16() {
            // The media is there and no decoder can read it as this kind.
            404 | 422 => WorkerError::Rejected(msg),
            429 => WorkerError::Unavailable(msg),
            // Malformed request, wrong key, path outside the roots: the two
            // sides disagree about the contract.
            400..=499 => WorkerError::Contract(msg),
            // 503 detector_unavailable, 504 deadline_exceeded, 500.
            _ => WorkerError::Unavailable(msg),
        })
    }

    pub async fn fingerprint(&self, path: &Path, kind: &str) -> Result<Fingerprint, WorkerError> {
        let body = serde_json::json!({ "path": path.to_string_lossy(), "kind": kind });
        let fp: Fingerprint = self.post("/v1/fingerprint", &body).await?;
        let fp = fp.validate()?;
        metrics::counter!("abraxas_worker_calls_total", "route" => "fingerprint").increment(1);
        Ok(fp)
    }

    pub async fn analyze_media(&self, path: &Path, kind: &str, deep: bool) -> Result<Analysis, WorkerError> {
        let body = serde_json::json!({ "path": path.to_string_lossy(), "kind": kind, "deep": deep });
        let a: Analysis = self.post("/v1/analyze", &body).await?;
        metrics::counter!("abraxas_worker_calls_total", "route" => "analyze_media").increment(1);
        a.validate(Needs::for_kind(kind))
    }

    pub async fn analyze_text(&self, text: &str, deep: bool) -> Result<Analysis, WorkerError> {
        let body = serde_json::json!({ "kind": "text", "text": text, "deep": deep });
        let a: Analysis = self.post("/v1/analyze", &body).await?;
        metrics::counter!("abraxas_worker_calls_total", "route" => "analyze_text").increment(1);
        a.validate(Needs::Text)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn fingerprint_parses_the_worker_shape() {
        let fp: Fingerprint = serde_json::from_str(
            r#"{"kind":"video","sha256":"AB00000000000000000000000000000000000000000000000000000000000000",
                "duration":12.0,
                "video_segments":[{"t":0,"phash":"ffffffffffffffff"},{"t":2.5,"phash":"0000000000000001"}],
                "audio":{"landmark_version":"lm1","peaks":240,
                         "landmarks":[{"hash":1152921504606863360,"t":1.25}],
                         "robust_landmarks":[],"chromaprint":null}}"#,
        )
        .unwrap();
        let fp = fp.validate().unwrap();
        assert_eq!(fp.segments()[0].phash, -1);
        assert_eq!(fp.segments()[1].phash, 1);
        assert_eq!(fp.audio.as_ref().unwrap().landmark_version, "lm1");
        assert!(fp.sha256.starts_with("ab"));
    }

    #[test]
    fn sound_may_lack_pictures_but_pictures_may_not() {
        let a: Analysis = serde_json::from_str(
            r#"{"kind":"audio","deep":false,"nsfw":null,"gore":null,"transcript":"hi",
                "toxicity":0.0,"toxicity_signals":{"hate":[],"violence":[],"self_harm":[],"sources":["transcript"]},
                "clickbait":null,"clickbait_signals":null,"spam":null,"spam_signals":null,"labels":[]}"#,
        )
        .unwrap();
        assert!(a.clone().validate(Needs::Sound).is_ok());
        assert!(matches!(a.validate(Needs::Picture), Err(WorkerError::Contract(_))));
        let p: Analysis = serde_json::from_str(
            r#"{"nsfw":{"score":0.8,"labels":["exposed_breast"],"label_scores":{},"frames":1,"tiles":0},
                "gore":0.1,"labels":["nsfw:exposed_breast"]}"#,
        )
        .unwrap();
        let p = p.validate(Needs::Picture).unwrap();
        assert_eq!(p.nsfw_score(), Some(0.8));
        assert_eq!(p.labels, vec!["nsfw:exposed_breast".to_string()]);
    }
}
