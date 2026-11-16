use serde::Serialize;

/// What Zior hears in one audio file. Zior keeps none of it: feed-engine
/// stores the whole answer on the work (feed-engine/internal/zior/client.go
/// analyzeResponse, db.SetWorkAudioAnalysis).
#[derive(Debug, Serialize)]
pub struct AnalyzeResponse {
    pub processing_ms: u64,
    /// Decoded length of the audio.
    pub duration_secs: f64,
    /// The audio's position on the 8 Jung axes, in `AXIS_NAMES` order
    /// (`jung::axes`). Exactly 8 floats by type — the contract with
    /// feed-engine's `jung.Vector`, which rejects any other length.
    pub topic_vector: [f32; 8],
    pub bpm: f32,
    /// "A minor", "C# major", …
    pub key: String,
    pub mood: String,
    /// Genre-free display string ("128 BPM · A minor · intense · …").
    pub descriptor: String,
    /// Listening contexts the audio suggests (workout, late night, …).
    pub context_tags: Vec<String>,
}
