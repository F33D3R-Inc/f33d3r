"""
content-scan scanner — CPU-only media analysis for F33D3R.

Runs on Hetzner CCX23 (no GPU). Pipeline per uploaded video:
  1. SHA-256 of decoded audio stream — content-based, not file-based.
     Hashing master.m3u8 manifests is unreliable because relative segment
     paths make identical videos hash differently or accidentally equal.
     We decode audio via ffmpeg (mono 16 kHz PCM) so the hash is the same
     regardless of container, codec, or segment naming.
  2. Audio chromaprint fingerprint (fpcalc / chromaprint).
     Tries the path directly first, then extracts to a temp WAV for HLS
     paths where fpcalc cannot resolve relative segment URLs.
  3. Perceptual frame hash (imagehash phash) for visual duplicate detection.
  4. NSFW frame classification (NudeNet ONNX — CPU, ~80ms/frame).

Text analysis:
  5. Clickbait detection — TF-IDF features + LightGBM (rule-based fallback).
  6. Hate/violence/self-harm text signals on post body, OCR text, and audio transcript.

Audio content moderation (scan_version 2.1+):
  7. transcribe_audio — faster-whisper base model (CPU, int8 quantized, multilingual).
     Speech-to-text → same text moderation stack as post body and OCR text.
     This is how Meta, ByteDance, and X Corp do audio moderation at scale:
     audio → transcript → text pipeline, NOT raw waveform classifiers.

CSAM policy:
  Known CSAM is detected via PhotoDNA-style SHA-256/phash hash matching against
  the banned_hashes registry. The hash list must be sourced from NCMEC/IWF.
  DO NOT attempt to train or run a CSAM classifier — possession of training
  material is illegal. Hash matching is the only safe and legal approach.
  Legal adult content (nudity_score >= 0.70) → age_gate, not block.

No GPU, no heavy transformers. Built for throughput on many CPU cores.
"""

import hashlib
import json
import logging
import os
import subprocess
import tempfile
from typing import Optional

logger = logging.getLogger("content-scan")

# NSFW labels that flag content as adult/NSFW
_NSFW_LABELS = {
    "EXPOSED_BREAST_F", "EXPOSED_GENITALIA_F", "EXPOSED_GENITALIA_M",
    "EXPOSED_ANUS_F", "EXPOSED_ANUS_M", "EXPOSED_BUTTOCKS_F",
    "EXPOSED_BUTTOCKS_M",
}
# Score above this threshold marks a frame as NSFW
_NSFW_FRAME_THRESHOLD = 0.45

_nude_detector = None


def _get_detector():
    global _nude_detector
    if _nude_detector is None:
        try:
            from nudenet import NudeDetector
            _nude_detector = NudeDetector()
            logger.info("NudeNet detector loaded (CPU ONNX)")
        except Exception as e:
            logger.warning("NudeNet unavailable: %s — NSFW detection disabled", e)
            _nude_detector = False
    return _nude_detector


# ── helpers ───────────────────────────────────────────────────────────────────

def sha256_file(path: str) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(65536), b""):
            h.update(chunk)
    return h.hexdigest()


def sha256_video_stream(path: str) -> str:
    """
    Hash the decoded audio stream for content-based dedup.

    Produces the same fingerprint for the same source audio regardless of
    container format, codec parameters, or HLS segment naming.  Mono 16 kHz
    PCM is a deterministic decode target — ffmpeg will produce identical bytes
    for identical source audio across re-encodes at the same settings.

    Returns empty string on failure (caller should fall back gracefully).
    """
    try:
        proc = subprocess.run(
            ["ffmpeg", "-y", "-i", path,
             "-vn",                     # audio-only — fast, no video decode
             "-ac", "1",               # mono
             "-ar", "16000",           # 16 kHz — enough for fingerprint fidelity
             "-f", "s16le",            # raw signed 16-bit LE PCM
             "pipe:1"],
            capture_output=True,
            timeout=300,
        )
        if proc.returncode == 0 and proc.stdout:
            return hashlib.sha256(proc.stdout).hexdigest()
        if proc.stderr:
            logger.debug("sha256_video_stream ffmpeg stderr: %s", proc.stderr[-500:].decode(errors="replace"))
    except subprocess.TimeoutExpired:
        logger.warning("sha256_video_stream timed out for %s", path)
    except Exception as e:
        logger.warning("sha256_video_stream error: %s", e)
    return ""


# ── Audio transcription (faster-whisper, CPU int8) ───────────────────────────

_whisper_model = None
_whisper_loaded = False


def _get_whisper():
    """Lazy-load faster-whisper base model (multilingual, int8 quantized, CPU)."""
    global _whisper_model, _whisper_loaded
    if _whisper_loaded:
        return _whisper_model
    _whisper_loaded = True
    try:
        from faster_whisper import WhisperModel
        _whisper_model = WhisperModel("base", device="cpu", compute_type="int8")
        logger.info("faster-whisper base model loaded (multilingual, int8)")
    except ImportError:
        logger.warning("faster-whisper not installed — audio transcription disabled")
    except Exception as e:
        logger.warning("faster-whisper load error: %s", e)
    return _whisper_model


def transcribe_audio(path: str, max_duration_s: float = 300.0) -> str:
    """
    Transcribe audio from a video/audio file using faster-whisper.

    Returns transcript text or "" on any failure. Skips files longer than
    max_duration_s (default 5 min) to keep scan latency bounded.

    Works with HLS master.m3u8 by extracting audio to a temp WAV first,
    same strategy as audio_fingerprint().
    """
    model = _get_whisper()
    if model is None:
        return ""

    tmp_wav = None
    try:
        dur = probe_duration(path)
        if dur > max_duration_s:
            logger.info("transcription skipped: duration %.1fs > limit %.1fs (%s)", dur, max_duration_s, path)
            return ""

        with tempfile.NamedTemporaryFile(suffix=".wav", delete=False) as f:
            tmp_wav = f.name

        r = subprocess.run(
            ["ffmpeg", "-y", "-i", path,
             "-vn", "-acodec", "pcm_s16le", "-ar", "16000", "-ac", "1",
             tmp_wav],
            capture_output=True, timeout=60,
        )
        if r.returncode != 0 or not os.path.exists(tmp_wav) or os.path.getsize(tmp_wav) == 0:
            return ""

        segments, _ = model.transcribe(tmp_wav, beam_size=1, vad_filter=True)
        transcript = " ".join(seg.text.strip() for seg in segments)
        return transcript.strip()

    except subprocess.TimeoutExpired:
        logger.warning("audio extraction timeout for %s", path)
        return ""
    except Exception as e:
        logger.warning("transcribe_audio error for %s: %s", path, e)
        return ""
    finally:
        if tmp_wav:
            try:
                os.unlink(tmp_wav)
            except OSError:
                pass


def probe_duration(path: str) -> float:
    """Return video duration via ffprobe, default 0."""
    try:
        out = subprocess.run(
            ["ffprobe", "-v", "error",
             "-show_entries", "format=duration",
             "-of", "json", path],
            capture_output=True, timeout=10,
        )
        return float(json.loads(out.stdout).get("format", {}).get("duration", 0))
    except Exception:
        return 0.0


def audio_fingerprint(path: str) -> Optional[str]:
    """
    Return chromaprint fingerprint string or None.

    Tries fpcalc directly first.  For HLS paths (master.m3u8) fpcalc cannot
    resolve relative segment URLs, so we extract audio to a temp WAV via
    ffmpeg and fingerprint that instead.
    """
    tmp_wav = None
    try:
        # Direct attempt — works for mp4/webm and sometimes HLS
        out = subprocess.run(
            ["fpcalc", "-json", path],
            capture_output=True, timeout=60,
        )
        if out.returncode == 0:
            fp = json.loads(out.stdout).get("fingerprint")
            if fp:
                return fp
        # fpcalc returned non-zero or empty fingerprint — try WAV extraction
        with tempfile.NamedTemporaryFile(suffix=".wav", delete=False) as f:
            tmp_wav = f.name
        extract = subprocess.run(
            ["ffmpeg", "-y", "-i", path, "-vn",
             "-acodec", "pcm_s16le", "-ar", "44100", "-ac", "2",
             tmp_wav],
            capture_output=True, timeout=180,
        )
        if extract.returncode == 0 and os.path.exists(tmp_wav) and os.path.getsize(tmp_wav) > 0:
            out2 = subprocess.run(["fpcalc", "-json", tmp_wav], capture_output=True, timeout=60)
            if out2.returncode == 0:
                return json.loads(out2.stdout).get("fingerprint")
    except FileNotFoundError:
        logger.warning("fpcalc not found — install chromaprint for audio fingerprinting")
    except Exception as e:
        logger.warning("audio_fingerprint error: %s", e)
    finally:
        if tmp_wav and os.path.exists(tmp_wav):
            try:
                os.unlink(tmp_wav)
            except Exception:
                pass
    return None


def perceptual_video_hash(path: str, n_frames: int = 8) -> Optional[str]:
    """
    Extract n_frames keyframes and compute average perceptual hash.
    Returns hex string representing the video's visual fingerprint.
    """
    try:
        import imagehash
        from PIL import Image
    except ImportError:
        logger.warning("imagehash/Pillow not available — perceptual hash disabled")
        return None

    dur = probe_duration(path)
    if dur <= 0:
        return None

    hashes = []
    with tempfile.TemporaryDirectory() as td:
        for i in range(n_frames):
            ts = dur * (i + 0.5) / n_frames
            out_path = os.path.join(td, f"f{i:03d}.jpg")
            subprocess.run(
                ["ffmpeg", "-y", "-ss", f"{ts:.2f}", "-i", path,
                 "-vframes", "1", "-q:v", "3", "-vf", "scale=160:-2", out_path],
                capture_output=True, timeout=15,
            )
            if os.path.exists(out_path):
                try:
                    h = imagehash.phash(Image.open(out_path))
                    hashes.append(h)
                except Exception:
                    pass

    if not hashes:
        return None
    # Average the hashes by taking majority vote per bit
    bits = len(hashes[0].hash.flatten())
    counts = [sum(1 for h in hashes if h.hash.flatten()[b]) for b in range(bits)]
    avg_bits = [c > len(hashes) / 2 for c in counts]
    import numpy as np
    final = imagehash.ImageHash(np.array(avg_bits).reshape(hashes[0].hash.shape))
    return str(final)


def scan_frames_nsfw(path: str, n_frames: int = 10) -> dict:
    """
    Extract n_frames from video and run NudeNet on each.
    Returns {'is_nsfw': bool, 'nsfw_score': float, 'labels': list[str]}.
    """
    detector = _get_detector()
    if not detector:
        return {"is_nsfw": False, "nsfw_score": 0.0, "labels": []}

    dur = probe_duration(path)
    if dur <= 0:
        dur = 10.0

    max_score = 0.0
    found_labels: set = set()

    with tempfile.TemporaryDirectory() as td:
        for i in range(n_frames):
            ts = dur * (i + 0.5) / n_frames
            out_path = os.path.join(td, f"f{i:03d}.jpg")
            subprocess.run(
                ["ffmpeg", "-y", "-ss", f"{ts:.2f}", "-i", path,
                 "-vframes", "1", "-q:v", "3", "-vf", "scale=640:-2", out_path],
                capture_output=True, timeout=15,
            )
            if not os.path.exists(out_path):
                continue
            try:
                detections = detector.detect(out_path)
                for det in detections:
                    label = det.get("class", "")
                    score = float(det.get("score", 0.0))
                    if label in _NSFW_LABELS and score > 0.25:
                        found_labels.add(label)
                        max_score = max(max_score, score)
            except Exception as e:
                logger.debug("NudeNet frame error: %s", e)

    return {
        "is_nsfw": max_score >= _NSFW_FRAME_THRESHOLD,
        "nsfw_score": round(max_score, 4),
        "labels": sorted(found_labels),
    }


# ── public API ────────────────────────────────────────────────────────────────

def scan_video(path: str, asset_id: str = "") -> dict:
    """
    Full scan pipeline for a video file:
      - SHA-256 exact hash
      - Audio chromaprint fingerprint
      - Perceptual video hash (for near-duplicate detection)
      - NSFW frame analysis (NudeNet)

    Returns a dict with all results; safe to store as JSON.
    """
    result = {
        "asset_id": asset_id,
        "sha256": None,
        "audio_fingerprint": None,
        "perceptual_hash": None,
        "is_nsfw": False,
        "nsfw_score": 0.0,
        "nsfw_labels": [],
        "scan_version": "1.1",
        "error": None,
    }

    if not path or not os.path.exists(path):
        result["error"] = "file_not_found"
        return result

    # 1. Content-based audio stream hash (format-independent duplicate detection).
    #    sha256_video_stream decodes the audio via ffmpeg so the hash reflects
    #    actual content, not container/manifest structure.
    result["sha256"] = sha256_video_stream(path)
    if not result["sha256"]:
        logger.warning("sha256_video_stream produced no hash for %s — falling back to file hash", path)
        try:
            result["sha256"] = sha256_file(path)
        except Exception as e:
            logger.warning("sha256_file fallback error: %s", e)

    # 2. Audio fingerprint (for duplicate detection + creator lineage)
    result["audio_fingerprint"] = audio_fingerprint(path)

    # 3. Perceptual video hash
    result["perceptual_hash"] = perceptual_video_hash(path)

    # 4. NSFW frame scan
    try:
        nsfw = scan_frames_nsfw(path)
        result["is_nsfw"] = nsfw["is_nsfw"]
        result["nsfw_score"] = nsfw["nsfw_score"]
        result["nsfw_labels"] = nsfw["labels"]
    except Exception as e:
        logger.warning("NSFW scan error: %s", e)

    return result


def scan_image_bytes(image_bytes: bytes, filename: str = "image.jpg") -> dict:
    """
    Scan a single image for NSFW content.
    Returns {'is_nsfw': bool, 'nsfw_score': float, 'labels': list[str]}.
    """
    detector = _get_detector()
    if not detector:
        return {"is_nsfw": False, "nsfw_score": 0.0, "labels": []}

    result = {"is_nsfw": False, "nsfw_score": 0.0, "labels": []}
    with tempfile.NamedTemporaryFile(suffix=os.path.splitext(filename)[1] or ".jpg",
                                    delete=False) as tmp:
        tmp.write(image_bytes)
        tmp_path = tmp.name
    try:
        detections = detector.detect(tmp_path)
        max_score = 0.0
        found_labels = set()
        for det in detections:
            label = det.get("class", "")
            score = float(det.get("score", 0.0))
            if label in _NSFW_LABELS and score > 0.25:
                found_labels.add(label)
                max_score = max(max_score, score)
        result["is_nsfw"] = max_score >= _NSFW_FRAME_THRESHOLD
        result["nsfw_score"] = round(max_score, 4)
        result["labels"] = sorted(found_labels)
    except Exception as e:
        logger.warning("NudeNet image error: %s", e)
    finally:
        os.unlink(tmp_path)
    return result


# ── Text scan: clickbait detection ────────────────────────────────────────────

# Clickbait keyword patterns — hand-tuned, no external training data needed.
_CLICKBAIT_KEYWORDS = {
    "you won't believe", "shocking", "this is why", "what happens next",
    "mind blown", "blew my mind", "this will", "secret revealed", "insane",
    "omg", "wtf", "unbelievable", "jaw dropping", "life changing", "must see",
    "going viral", "breaks the internet", "nobody is talking about", "wait for it",
    "they don't want you to know", "doctors hate", "one weird trick", "exposed",
    "banned", "censored", "this changes everything", "finally revealed",
    "gone wrong", "gone right", "reaction", "i can't believe", "look what",
    "you need to see this", "share before deleted", "watch before removed",
    "only real fans", "first to know", "exclusive", "limited time",
}

_lgbm_model = None
_lgbm_available = False

try:
    import lightgbm as lgb  # noqa: F401
    _lgbm_available = True
except ImportError:
    _lgbm_available = False


def _extract_text_features(text: str) -> dict:
    """Extract rule-based features from text for clickbait detection."""
    import re
    if not text:
        return {
            "caps_ratio": 0.0,
            "exclamation_count": 0,
            "question_count": 0,
            "keyword_hits": 0,
            "word_count": 0,
            "avg_word_len": 0.0,
            "has_number_claim": False,
        }
    words = text.split()
    alpha_chars = [c for c in text if c.isalpha()]
    caps_ratio = sum(1 for c in alpha_chars if c.isupper()) / max(len(alpha_chars), 1)
    exclamation_count = text.count("!")
    question_count = text.count("?")
    text_lower = text.lower()
    keyword_hits = sum(1 for kw in _CLICKBAIT_KEYWORDS if kw in text_lower)
    avg_word_len = sum(len(w) for w in words) / max(len(words), 1)
    has_number_claim = bool(re.search(
        r'\b\d+\s*(reasons?|things?|ways?|secrets?|facts?|tips?|tricks?|steps?|hacks?)\b',
        text_lower,
    ))
    return {
        "caps_ratio": round(caps_ratio, 4),
        "exclamation_count": exclamation_count,
        "question_count": question_count,
        "keyword_hits": keyword_hits,
        "word_count": len(words),
        "avg_word_len": round(avg_word_len, 2),
        "has_number_claim": has_number_claim,
    }


def _rule_based_clickbait_score(features: dict) -> float:
    """
    Compute a clickbait score 0.0–1.0 from features using hand-tuned weights.
    Used when LightGBM is unavailable.
    """
    score = 0.0
    score += min(features["caps_ratio"] * 2.0, 0.4)
    score += min(features["exclamation_count"] * 0.08, 0.2)
    score += min(features["keyword_hits"] * 0.12, 0.25)
    if features["has_number_claim"]:
        score += 0.15
    score += min(features["question_count"] * 0.04, 0.1)
    return round(min(score, 1.0), 4)


def _get_lgbm_model():
    """Lazy-load/train a tiny LightGBM clickbait model from synthetic data."""
    global _lgbm_model
    if not _lgbm_available:
        return None
    if _lgbm_model is not None:
        return _lgbm_model

    try:
        import lightgbm as lgb
        import numpy as np

        # Synthetic training set — feature order:
        # [caps_ratio, excl_count, q_count, kw_hits, word_count, avg_word_len, number_claim]
        X_train = np.array([
            # Clickbait
            [0.8, 3, 1, 4, 8,  4.0, 1],
            [0.6, 2, 0, 3, 6,  5.0, 1],
            [0.5, 1, 2, 5, 12, 4.5, 0],
            [0.4, 3, 0, 2, 5,  4.0, 1],
            [0.7, 2, 1, 3, 7,  4.0, 0],
            [0.3, 2, 3, 4, 9,  3.8, 1],
            # Borderline
            [0.2, 1, 1, 1, 15, 5.5, 0],
            [0.3, 0, 2, 2, 10, 4.8, 1],
            [0.25, 1, 0, 1, 20, 6.0, 0],
            # Not clickbait
            [0.05, 0, 0, 0, 25, 6.5, 0],
            [0.03, 0, 1, 0, 30, 7.0, 0],
            [0.1,  0, 0, 0, 50, 6.0, 0],
            [0.02, 0, 0, 1, 40, 7.2, 0],
            [0.08, 1, 0, 0, 35, 5.5, 0],
        ], dtype=np.float32)
        y_train = np.array([1,1,1,1,1,1, 1,1,0, 0,0,0,0,0], dtype=np.float32)

        ds = lgb.Dataset(X_train, label=y_train)
        params = {
            "objective": "binary",
            "metric": "binary_logloss",
            "num_leaves": 8,
            "learning_rate": 0.1,
            "verbosity": -1,
        }
        _lgbm_model = lgb.train(params, ds, num_boost_round=30)
        logger.info("LightGBM clickbait model trained on synthetic data")
    except Exception as e:
        logger.warning("LightGBM model training failed: %s — using rule-based fallback", e)
        _lgbm_model = False
    return _lgbm_model


_ocr_reader = None


def _get_ocr_reader():
    """Lazy-load EasyOCR reader (English). CPU-only, ~300MB model."""
    global _ocr_reader
    if _ocr_reader is None:
        try:
            import easyocr
            _ocr_reader = easyocr.Reader(["en"], gpu=False, verbose=False)
            logger.info("EasyOCR reader loaded (CPU)")
        except Exception as e:
            logger.warning("EasyOCR unavailable: %s — OCR disabled", e)
            _ocr_reader = False
    return _ocr_reader


def extract_ocr_text(path: str) -> str:
    """
    Run OCR on image or first frame of video.
    Returns extracted text string (empty on failure).
    """
    reader = _get_ocr_reader()
    if not reader:
        return ""

    target = path
    tmp_frame = None

    # For video: extract the first keyframe
    if path.lower().endswith((".mp4", ".webm", ".mkv", ".m3u8", ".ts")):
        import tempfile
        tmp = tempfile.NamedTemporaryFile(suffix=".jpg", delete=False)
        tmp_frame = tmp.name
        tmp.close()
        ret = subprocess.run(
            ["ffmpeg", "-y", "-i", path, "-vframes", "1", "-q:v", "3", tmp_frame],
            capture_output=True, timeout=15,
        )
        if ret.returncode != 0 or not os.path.exists(tmp_frame):
            return ""
        target = tmp_frame

    try:
        results = reader.readtext(target, detail=0)
        return " ".join(results).strip()
    except Exception as e:
        logger.debug("OCR error: %s", e)
        return ""
    finally:
        if tmp_frame and os.path.exists(tmp_frame):
            try:
                os.unlink(tmp_frame)
            except Exception:
                pass


# ── Gore/violence heuristics ──────────────────────────────────────────────────
# Pure color-statistics approach — no heavy model, CPU-only, <10ms/frame.
# High red saturation in centre of frame is a proxy for blood/gore.
# False positive rate is acceptable here — gore triggers human_review, not auto_block.

_GORE_RED_RATIO_THRESHOLD = 0.28


def _gore_score_frame(path: str) -> float:
    """
    Returns 0.0–1.0 gore proxy score based on red-channel dominance.
    Frames where R > 1.5×G and R > 1.5×B and mean(R) > 80 score high.
    """
    try:
        import numpy as np
        from PIL import Image
        img = Image.open(path).convert("RGB").resize((160, 120))
        arr = np.array(img, dtype=np.float32)
        r, g, b = arr[:, :, 0], arr[:, :, 1], arr[:, :, 2]
        dominant_red = ((r > 1.5 * g) & (r > 1.5 * b) & (r > 80)).mean()
        return float(min(dominant_red * 3.0, 1.0))
    except Exception:
        return 0.0


def gore_score_video(path: str, n_frames: int = 6) -> float:
    """Sample n_frames and return max gore proxy score."""
    dur = probe_duration(path)
    if dur <= 0:
        dur = 10.0

    max_score = 0.0
    with tempfile.TemporaryDirectory() as td:
        for i in range(n_frames):
            ts = dur * (i + 0.5) / n_frames
            out_path = os.path.join(td, f"g{i:03d}.jpg")
            subprocess.run(
                ["ffmpeg", "-y", "-ss", f"{ts:.2f}", "-i", path,
                 "-vframes", "1", "-q:v", "5", "-vf", "scale=160:-2", out_path],
                capture_output=True, timeout=10,
            )
            if os.path.exists(out_path):
                max_score = max(max_score, _gore_score_frame(out_path))
    return round(max_score, 4)


def gore_score_image(image_bytes: bytes) -> float:
    """Run gore heuristic on a raw image given as bytes."""
    try:
        import io
        with tempfile.NamedTemporaryFile(suffix=".jpg", delete=False) as tmp:
            tmp.write(image_bytes)
            tmp_path = tmp.name
        score = _gore_score_frame(tmp_path)
        os.unlink(tmp_path)
        return score
    except Exception:
        return 0.0


# ── Text signal detection — hate, violence, self-harm ────────────────────────
#
# These run on: post body text, OCR text from images/video frames,
# AND audio transcript. Same pipeline for all three surfaces.

_HATE_PATTERNS = [
    # White supremacist
    "white power", "white genocide", "14 words", "heil", "seig heil",
    "great replacement", "replace the", "ethnic cleansing",
    # Genocide promotion
    "gas the", "death to all", "kill all", "exterminate the",
    # Dehumanization
    "sub-human", "subhuman", "vermin", "cockroaches", "parasites are",
    # Extremist recruitment
    "join the movement", "race war", "day of the rope",
    # Sectarian violence promotion
    "death to jews", "death to muslims", "death to christians", "death to gays",
]

_VIOLENCE_PATTERNS = [
    # Direct threats
    "i will kill you", "i'll kill you", "gonna kill you", "going to kill you",
    "i will hurt you", "i'll hurt you", "you're going to die", "youre going to die",
    # Mass violence
    "shoot up the", "bomb the", "blow up the",
    "mass shooting", "school shooting",
]

_SELF_HARM_PATTERNS = [
    "kill yourself", "kys", "end your life", "end it all",
    "you should die", "go kill yourself", "commit suicide",
    "neck yourself",
]


def detect_hate_signals(text: str) -> list:
    """Return matched hate signal labels from text (post body, OCR, or transcript)."""
    if not text:
        return []
    text_lower = text.lower()
    hits = []
    for pattern in _HATE_PATTERNS:
        if pattern in text_lower:
            hits.append(f"hate_keyword:{pattern.replace(' ', '_')[:24]}")
    return hits


def detect_violence_signals(text: str) -> list:
    """Return matched violence threat signal labels from text."""
    if not text:
        return []
    text_lower = text.lower()
    return [
        f"violence:{p.replace(' ', '_')[:24]}"
        for p in _VIOLENCE_PATTERNS
        if p in text_lower
    ]


def detect_self_harm_signals(text: str) -> list:
    """Return matched self-harm encouragement labels from text."""
    if not text:
        return []
    text_lower = text.lower()
    return [
        f"self_harm:{p.replace(' ', '_')[:24]}"
        for p in _SELF_HARM_PATTERNS
        if p in text_lower
    ]


# ── Risk aggregation ─────────────────────────────────────────────────────────

def compute_risk(
    nudity_score: float = 0.0,
    gore_score: float = 0.0,
    clickbait_score: float = 0.0,
    hate_signals: list = None,
    ocr_text: str = "",
    transcript: str = "",
    violence_signals: list = None,
    self_harm_signals: list = None,
    is_duplicate: bool = False,
    skip_nudity: bool = False,
    skip_gore: bool = False,
) -> dict:
    """
    Aggregate all signal scores into a unified risk decision.

    skip_nudity=True: set for Verity-verified NSFW creators (is_adult_creator).
      Nudity score thresholds are bypassed — they are verified adults whose
      explicit content is expected and gated at the viewer level, not the post level.

    skip_gore=True: set for documentary/journalist/scientist creators
      (creator_type = journalist | scientist | medical).
      Gore score thresholds are bypassed — graphic content is inherent to their work
      (war footage, surgery, field research). They are still responsible for all
      other content rules.

    HARD GATES — always enforced regardless of any skip flag:
      • banned_hash (CSAM/NCMEC registry) — caught in server.py before this function
      • hate_signals (slurs, genocide promotion, incitement) → auto_block
      • violence_signals (direct threats: "I will kill you", "bomb the") → human_review
      • self_harm_signals (suicide encouragement) → human_review

    Decision priority (highest to lowest):
      1. Hate speech                  → auto_block   [hard gate, no skip]
      2. Violence threats / self-harm → human_review [hard gate, no skip]
      3. High gore   (skip_gore=False only)          → human_review
      4. Nudity ≥ 0.70 (skip_nudity=False only)      → age_gate
      5. Moderate nudity/gore/clickbait (respective skips)  → human_review
      6. All clear                    → approve
    """
    hate_signals      = list(hate_signals or [])
    violence_signals  = list(violence_signals or [])
    self_harm_signals = list(self_harm_signals or [])
    signals = []

    # Check OCR text for all signal types
    if ocr_text:
        hate_signals      = list(set(hate_signals) | set(detect_hate_signals(ocr_text)))
        violence_signals  = list(set(violence_signals) | set(detect_violence_signals(ocr_text)))
        self_harm_signals = list(set(self_harm_signals) | set(detect_self_harm_signals(ocr_text)))

    # Check audio transcript for all signal types
    if transcript:
        transcript_hate      = detect_hate_signals(transcript)
        transcript_violence  = detect_violence_signals(transcript)
        transcript_self_harm = detect_self_harm_signals(transcript)
        hate_signals      = list(set(hate_signals) | {f"transcript:{s}" for s in transcript_hate})
        violence_signals  = list(set(violence_signals) | {f"transcript:{s}" for s in transcript_violence})
        self_harm_signals = list(set(self_harm_signals) | {f"transcript:{s}" for s in transcript_self_harm})

    # Build signals list — always record raw detections for audit trail.
    # :creator_skip suffix marks signals that were detected but not acted on.
    if nudity_score >= 0.70:
        signals.append("nudity_high" + (":creator_skip" if skip_nudity else ""))
    elif nudity_score >= 0.45:
        signals.append("nudity_moderate" + (":creator_skip" if skip_nudity else ""))

    if gore_score >= 0.55:
        signals.append("gore_high" + (":creator_skip" if skip_gore else ""))
    elif gore_score >= 0.28:
        signals.append("gore_moderate" + (":creator_skip" if skip_gore else ""))

    if clickbait_score >= 0.70:
        signals.append("clickbait_high")
    elif clickbait_score >= 0.55:
        signals.append("clickbait_moderate")

    signals.extend(hate_signals)
    signals.extend(violence_signals)
    signals.extend(self_harm_signals)

    # ── Hard gates — no skip flag overrides these ─────────────────────────────
    if hate_signals:
        return {"risk_level": "block", "recommendation": "auto_block", "signals": signals}

    if violence_signals or self_harm_signals:
        return {"risk_level": "review", "recommendation": "human_review", "signals": signals}

    # ── Gore threshold (skipped for journalists / documentary / medical) ───────
    if not skip_gore and gore_score >= 0.55:
        return {"risk_level": "review", "recommendation": "human_review", "signals": signals}

    # ── Nudity threshold (skipped for NSFW-verified creators) ─────────────────
    if not skip_nudity:
        if nudity_score >= 0.70:
            # Legal adult content — age gate, not block.
            # CSAM is caught earlier via banned_hashes (NCMEC/PhotoDNA registry).
            return {"risk_level": "age_gate", "recommendation": "age_gate", "signals": signals}
        if nudity_score >= 0.45:
            return {"risk_level": "review", "recommendation": "human_review", "signals": signals}

    # ── Moderate gore (skipped for documentary creators) ─────────────────────
    if not skip_gore and gore_score >= 0.28:
        return {"risk_level": "review", "recommendation": "human_review", "signals": signals}

    if clickbait_score >= 0.70:
        return {"risk_level": "review", "recommendation": "human_review", "signals": signals}

    return {"risk_level": "clean", "recommendation": "approve", "signals": signals}


def scan_text(text: str) -> dict:
    """
    Clickbait detection for post bodies and titles.

    Returns:
        {
            "clickbait_score": float (0.0–1.0),
            "is_clickbait": bool,
            "signals": list[str]   — human-readable explanation tokens
        }
    """
    features = _extract_text_features(text)
    signals = []

    model = _get_lgbm_model()
    if model and _lgbm_available:
        try:
            import numpy as np
            feat_vec = np.array([[
                features["caps_ratio"],
                float(features["exclamation_count"]),
                float(features["question_count"]),
                float(features["keyword_hits"]),
                float(features["word_count"]),
                features["avg_word_len"],
                float(features["has_number_claim"]),
            ]], dtype=np.float32)
            score = float(model.predict(feat_vec)[0])
        except Exception as e:
            logger.debug("LightGBM predict failed: %s — using rule fallback", e)
            score = _rule_based_clickbait_score(features)
    else:
        score = _rule_based_clickbait_score(features)

    if features["caps_ratio"] > 0.3:
        signals.append("high_caps_ratio")
    if features["exclamation_count"] >= 2:
        signals.append("multiple_exclamations")
    if features["keyword_hits"] >= 2:
        signals.append("clickbait_keywords")
    if features["has_number_claim"]:
        signals.append("numbered_list_claim")
    if features["question_count"] >= 2:
        signals.append("multiple_questions")

    return {
        "clickbait_score": round(score, 4),
        "is_clickbait": score >= 0.55,
        "signals": signals,
    }
