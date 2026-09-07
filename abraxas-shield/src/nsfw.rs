/// NSFW detection — text signals (keyword matching) + optional visual ONNX inference.
///
/// Text detection is always active.
/// Visual detection requires the `visual-nsfw` Cargo feature AND a model file at MODEL_PATH.
/// Without the feature (default build), `load_detector()` always returns `None`.
use tracing::info;

// ── Text detection ─────────────────────────────────────────────────────────────

const EXPLICIT_TERMS: &[&str] = &[
    "nude", "naked", "nsfw", "explicit", "pornography", "pornographic",
    "onlyfans", "xxx", "18+", "adult content",
];

pub struct NsfwResult {
    pub nudity_score: f64,
    pub signals: Vec<String>,
}

pub fn detect_text_signals(text: &str) -> NsfwResult {
    if text.is_empty() {
        return NsfwResult { nudity_score: 0.0, signals: vec![] };
    }
    let lower = text.to_lowercase();
    let matched: Vec<String> = EXPLICIT_TERMS
        .iter()
        .filter(|&&t| lower.contains(t))
        .map(|&t| format!("nsfw_text:{}", t.replace(' ', "_")))
        .collect();
    if matched.is_empty() {
        NsfwResult { nudity_score: 0.0, signals: vec![] }
    } else {
        NsfwResult { nudity_score: 1.0, signals: matched }
    }
}

// ── Visual ONNX detection (feature-gated) ─────────────────────────────────────

pub struct NsfwVisualResult {
    pub nudity_score: f64,
    pub is_explicit:  bool,
    pub model:        String,
}

#[cfg(feature = "visual-nsfw")]
mod visual {
    use std::env;
    use std::path::Path;
    use std::sync::Mutex;

    use image::imageops::FilterType;
    use ort::session::Session;
    use ort::value::TensorRef;
    use tracing::warn;

    const DEFAULT_MODEL_PATH: &str = "/models/nsfw_model.onnx";
    const INPUT_SIZE: u32 = 224;

    pub struct NsfwOnnxDetector {
        pub(super) session: Mutex<Session>,
    }

    pub fn load_detector() -> Option<NsfwOnnxDetector> {
        let path = env::var("MODEL_PATH").unwrap_or_else(|_| DEFAULT_MODEL_PATH.to_string());
        if !Path::new(&path).exists() {
            return None;
        }
        match Session::builder().and_then(|mut b| b.commit_from_file(&path)) {
            Ok(s) => Some(NsfwOnnxDetector { session: Mutex::new(s) }),
            Err(e) => { warn!(err = %e, "NSFW model load failed"); None }
        }
    }

    pub fn detect_visual(det: &NsfwOnnxDetector, image_bytes: &[u8]) -> super::NsfwVisualResult {
        let img = match image::load_from_memory(image_bytes) {
            Ok(i) => i,
            Err(_) => return super::NsfwVisualResult { nudity_score: 0.0, is_explicit: false, model: "decode_err".into() },
        };
        let rgb  = img.resize_exact(INPUT_SIZE, INPUT_SIZE, FilterType::Triangle).to_rgb8();
        let size = INPUT_SIZE as usize;
        let mut data = Vec::<f32>::with_capacity(3 * size * size);
        for c in 0..3u8 {
            for y in 0..INPUT_SIZE {
                for x in 0..INPUT_SIZE {
                    data.push(rgb.get_pixel(x, y)[c as usize] as f32 / 255.0);
                }
            }
        }
        let array = match ndarray::Array::from_shape_vec([1usize, 3, size, size], data) {
            Ok(a) => a,
            Err(_) => return super::NsfwVisualResult { nudity_score: 0.0, is_explicit: false, model: "tensor_err".into() },
        };
        let tensor_ref = match TensorRef::from_array_view(array.view()) {
            Ok(t) => t,
            Err(_) => return super::NsfwVisualResult { nudity_score: 0.0, is_explicit: false, model: "tensor_err".into() },
        };
        let mut session = match det.session.lock() {
            Ok(s) => s,
            Err(_) => return super::NsfwVisualResult { nudity_score: 0.0, is_explicit: false, model: "lock_err".into() },
        };
        let outputs = match session.run(ort::inputs![tensor_ref]) {
            Ok(o) => o,
            Err(e) => { warn!(err = %e, "NSFW inference failed"); return super::NsfwVisualResult { nudity_score: 0.0, is_explicit: false, model: "infer_err".into() }; }
        };
        let unsafe_prob: f64 = match outputs[0].try_extract_tensor::<f32>() {
            Ok((_shape, flat)) if flat.len() >= 2 => {
                let e0 = flat[0].exp(); let e1 = flat[1].exp(); let s = e0 + e1;
                if s > 0.0 { (e1 / s) as f64 } else { 0.0 }
            }
            Ok((_shape, flat)) if flat.len() == 1 => flat[0].clamp(0.0, 1.0) as f64,
            _ => 0.0,
        };
        super::NsfwVisualResult { nudity_score: unsafe_prob, is_explicit: unsafe_prob > 0.7, model: "onnx_v1".into() }
    }
}

// Public API — same surface regardless of whether visual-nsfw is compiled in.

#[cfg(feature = "visual-nsfw")]
pub use visual::NsfwOnnxDetector;

#[cfg(not(feature = "visual-nsfw"))]
pub struct NsfwOnnxDetector;  // zero-size stub

pub fn load_detector() -> Option<NsfwOnnxDetector> {
    #[cfg(feature = "visual-nsfw")]
    {
        let det = visual::load_detector();
        if det.is_some() {
            info!("NSFW: text+visual mode (ONNX model loaded)");
        } else {
            info!("NSFW: text-only mode (no model file)");
        }
        return det;
    }
    #[cfg(not(feature = "visual-nsfw"))]
    {
        info!("NSFW: text-only mode (visual-nsfw feature not compiled)");
        None
    }
}

pub fn detect_visual(_detector: Option<&NsfwOnnxDetector>, _image_bytes: &[u8]) -> NsfwVisualResult {
    #[cfg(feature = "visual-nsfw")]
    if let Some(det) = _detector {
        return visual::detect_visual(det, _image_bytes);
    }
    NsfwVisualResult { nudity_score: 0.0, is_explicit: false, model: "none".into() }
}
