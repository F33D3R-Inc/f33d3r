"""
content-scan detectors — the models and text rules behind POST /v1/analyze.

Scores only. Nothing in this module decides: no thresholds turn a score into an
action, no risk level, no recommendation. abraxas-shield reads these scores
and decides.

Detectors:
  nsfw        NudeNet 3.4 (ONNX). Score = the strongest explicit-class
              detection across every examined frame; every class the model
              emitted is reported with its best score.
  gore        Red-dominance colour statistic per frame (no model). A proxy:
              the fraction of strongly red-dominant pixels, scaled.
  ocr         EasyOCR (English) over sampled frames. Deep pass only.
  transcript  faster-whisper `base`, multilingual. Fast pass: the first 300 s.
              Deep pass: the whole file, beam search 5.
  toxicity    Word-bounded phrase rules (hate, violence, self-harm) over every
              text surface: the supplied text, OCR text, transcript. Score 1.0
              on any match, 0.0 on none.
  clickbait   Rule-based feature score over the supplied text.
  spam        Content-intrinsic spam signals over the supplied text, with a
              weighted score. Posting-
              rate and duplicate-body spam need the posting history, which is
              abraxas's database, so they stay there.

Device:
  CONTENT_SCAN_DEVICE=cpu (default) or cuda. A cuda request the host cannot
  honour logs a WARNING naming the reason and runs that model on CPU.

Failure rule:
  A model that cannot load, a model that raises during inference, a frame that
  cannot be read — each raises a WorkerError. No function here returns a zero
  score, an empty transcript or an empty label list in place of a result it did
  not compute.
"""

import logging
import math
import os
import re
import subprocess
import threading
import time
from collections import Counter
from typing import Callable, List, Optional, Tuple

import numpy as np
from PIL import Image, UnidentifiedImageError

import media
from errors import DeadlineExceeded, DetectorUnavailable, MediaError

logger = logging.getLogger("content-scan")

# ── device ────────────────────────────────────────────────────────────────────

_DEVICE = os.environ.get("CONTENT_SCAN_DEVICE", "cpu").strip().lower()
if _DEVICE not in ("cpu", "cuda"):
    raise ValueError(f"CONTENT_SCAN_DEVICE={_DEVICE!r} is not a device this build "
                     f"understands; use 'cpu' or 'cuda'")


def device() -> str:
    return _DEVICE


def _onnx_providers() -> list:
    if _DEVICE != "cuda":
        return ["CPUExecutionProvider"]
    import onnxruntime
    available = onnxruntime.get_available_providers()
    if "CUDAExecutionProvider" in available:
        return ["CUDAExecutionProvider", "CPUExecutionProvider"]
    logger.warning("CONTENT_SCAN_DEVICE=cuda but onnxruntime exposes no "
                   "CUDAExecutionProvider (available: %s) — NudeNet runs on CPU", available)
    return ["CPUExecutionProvider"]


# ── model slots ───────────────────────────────────────────────────────────────

# How long a failed load is remembered before it is attempted again. A transient
# failure must not disable a detector for the life of the process; a hard
# failure must not be retried on every request.
_MODEL_RETRY_S = 30.0


class _ModelSlot:
    """
    One lazily loaded model. A load failure raises DetectorUnavailable naming
    the model, and is retried after a cooldown rather than latched.
    """

    def __init__(self, name: str, loader: Callable):
        self.name = name
        self._loader = loader
        self._model = None
        self._error = ""
        self._next_retry = 0.0
        self._lock = threading.Lock()

    def get(self):
        model = self._model
        if model is not None:
            return model
        with self._lock:
            if self._model is not None:
                return self._model
            now = time.monotonic()
            if now < self._next_retry:
                raise DetectorUnavailable(f"{self.name}: {self._error}")
            try:
                self._model = self._loader()
            except Exception as e:
                self._error = f"{type(e).__name__}: {e}"
                self._next_retry = now + _MODEL_RETRY_S
                logger.error("%s could not be loaded: %s", self.name, self._error)
                raise DetectorUnavailable(f"{self.name}: {self._error}") from e
            self._error = ""
            logger.info("%s loaded (device=%s)", self.name, _DEVICE)
            return self._model

    def ready(self) -> Tuple[bool, str]:
        try:
            self.get()
            return True, ""
        except DetectorUnavailable as e:
            return False, e.detail


def _load_nudenet():
    from nudenet import NudeDetector
    return NudeDetector(providers=_onnx_providers())


def _load_easyocr():
    import easyocr
    if _DEVICE == "cuda":
        try:
            return easyocr.Reader(["en"], gpu=True, verbose=False)
        except Exception as e:
            logger.warning("EasyOCR could not use CUDA (%s: %s) — loading on CPU",
                           type(e).__name__, e)
    return easyocr.Reader(["en"], gpu=False, verbose=False)


def _load_whisper():
    from faster_whisper import WhisperModel
    if _DEVICE == "cuda":
        try:
            return WhisperModel("base", device="cuda", compute_type="float16")
        except Exception as e:
            logger.warning("faster-whisper could not use CUDA (%s: %s) — loading on CPU int8",
                           type(e).__name__, e)
    return WhisperModel("base", device="cpu", compute_type="int8")


nudenet = _ModelSlot("nudenet", _load_nudenet)
easyocr_reader = _ModelSlot("easyocr", _load_easyocr)
whisper = _ModelSlot("faster-whisper", _load_whisper)

MODELS = (nudenet, easyocr_reader, whisper)

# EasyOCR's reader is not documented as safe for concurrent calls; one at a
# time. NudeNet's ONNX session and ctranslate2 behind whisper are.
_ocr_lock = threading.Lock()


# ── frame sampling ────────────────────────────────────────────────────────────

# Fast pass: 12 frames at the centres of 12 equal slices of the video — every
# part of the timeline is represented, at the cost of one keyframe seek each.
FAST_VIDEO_FRAMES = 12

# Deep pass: one frame per second, thinned evenly to at most 900 frames on
# videos longer than 15 minutes.
DEEP_FRAME_INTERVAL_S = 1.0
DEEP_MAX_FRAMES = 900

# Frames are rendered at 640 px wide: above NudeNet's 320 px input so nothing
# is lost to the model's own resize, and large enough for EasyOCR on captions.
FRAME_WIDTH = 640

# Animated images: the fast pass examines up to 4 frames, the deep pass up to
# 32, spread evenly. A still image is one frame either way.
FAST_IMAGE_FRAMES = 4
DEEP_IMAGE_FRAMES = 32

# Images are re-rendered to RGB JPEG for the models, longest side at most
# 2048 px. NudeNet reads its input with OpenCV, which cannot open GIF or every
# WebP/PNG variant PIL can; rendering once through PIL gives every detector the
# same pixels.
IMAGE_MAX_SIDE = 2048

# Deep pass full-frame decode: generous against software decode of long 4K.
DEEP_FRAMES_SECONDS_PER_MEDIA_SECOND = 3.0
FAST_FRAME_SEEK_TIMEOUT_S = 60.0


class Frame:
    __slots__ = ("t", "path")

    def __init__(self, t: Optional[float], path: str):
        self.t = t
        self.path = path


def _scale_filter() -> str:
    return f"scale='min({FRAME_WIDTH},iw)':-2"


def video_frames(path: str, duration_s: float, deep: bool, workdir: str) -> Tuple[List[Frame], int]:
    """
    Render sampled frames of the first video stream as JPEGs in `workdir`.
    Returns (frames, requested). A requested timestamp that yields no frame
    (a container whose stated duration overruns its last frame) is not an
    error; the counts say how many were examined. ffmpeg failing is.
    """
    if not deep:
        frames = []
        for i in range(FAST_VIDEO_FRAMES):
            t = duration_s * (i + 0.5) / FAST_VIDEO_FRAMES
            out = os.path.join(workdir, f"fast_{i:03d}.jpg")
            try:
                r = subprocess.run(
                    ["ffmpeg", "-nostdin", "-v", "error", "-ss", f"{t:.3f}",
                     "-protocol_whitelist", "file", "-i", media.ffmpeg_input(path),
                     "-map", "0:v:0", "-frames:v", "1", "-q:v", "3",
                     "-vf", _scale_filter(), "-y", out],
                    capture_output=True, timeout=FAST_FRAME_SEEK_TIMEOUT_S)
            except subprocess.TimeoutExpired as e:
                raise DeadlineExceeded(f"frame extraction at {t:.2f}s did not finish "
                                       f"within {e.timeout}s") from e
            if r.returncode != 0:
                raise MediaError(f"frame extraction at {t:.2f}s failed (ffmpeg exit "
                                 f"{r.returncode}): "
                                 + (r.stderr or b"").decode("utf-8", "replace")[-2000:],
                                 code="decode_failed")
            if os.path.exists(out) and os.path.getsize(out) > 0:
                frames.append(Frame(t, out))
        return frames, FAST_VIDEO_FRAMES

    interval = max(DEEP_FRAME_INTERVAL_S, duration_s / DEEP_MAX_FRAMES)
    requested = max(1, int(math.ceil(duration_s / interval)))
    pattern = os.path.join(workdir, "deep_%06d.jpg")
    args = ["-v", "error", "-protocol_whitelist", "file", "-i", media.ffmpeg_input(path),
            "-map", "0:v:0", "-an", "-sn", "-dn",
            "-vf", f"fps=fps={1.0 / interval:.6f},{_scale_filter()}",
            "-q:v", "3", "-y", pattern]
    with media.FFmpegRun(args, media.budget(duration_s, DEEP_FRAMES_SECONDS_PER_MEDIA_SECOND),
                         "deep frame sampling"):
        pass
    names = sorted(n for n in os.listdir(workdir) if n.startswith("deep_"))
    frames = [Frame(round(i * interval, 3), os.path.join(workdir, n)) for i, n in enumerate(names)]
    return frames, requested


def image_frames(path: str, deep: bool, workdir: str) -> Tuple[List[Frame], int]:
    """Render an image (each sampled frame of an animated one) to RGB JPEG."""
    limit = DEEP_IMAGE_FRAMES if deep else FAST_IMAGE_FRAMES
    try:
        with Image.open(path) as img:
            total = int(getattr(img, "n_frames", 1) or 1)
            count = min(total, limit)
            indices = sorted({int(total * (i + 0.5) / count) for i in range(count)}) if total > 1 else [0]
            frames = []
            for k, index in enumerate(indices):
                img.seek(index)
                rgb = img.convert("RGB")
                rgb.thumbnail((IMAGE_MAX_SIDE, IMAGE_MAX_SIDE))
                out = os.path.join(workdir, f"image_{k:03d}.jpg")
                rgb.save(out, "JPEG", quality=95)
                frames.append(Frame(None, out))
    except (UnidentifiedImageError, Image.DecompressionBombError, OSError, ValueError, EOFError) as e:
        raise MediaError(f"the image could not be decoded: {type(e).__name__}: {e}",
                         code="image_unreadable") from e
    return frames, len(indices)


def _evenly(items: list, limit: int) -> list:
    if len(items) <= limit:
        return list(items)
    return [items[int(len(items) * (i + 0.5) / limit)] for i in range(limit)]


# ── NSFW ──────────────────────────────────────────────────────────────────────

# NudeNet 3.x class names. Only these feed the score; every class is reported
# in label_scores so abraxas can weigh covered/suggestive classes itself.
NSFW_EXPLICIT_CLASSES = frozenset({
    "FEMALE_BREAST_EXPOSED",
    "FEMALE_GENITALIA_EXPOSED",
    "MALE_GENITALIA_EXPOSED",
    "ANUS_EXPOSED",
    "BUTTOCKS_EXPOSED",
})

# Detections below this confidence are not reported. NudeNet itself emits
# nothing under 0.20; the fast pass reports from 0.25, the deep pass reports
# everything the model emits.
NSFW_REPORT_FLOOR_FAST = 0.25
NSFW_REPORT_FLOOR_DEEP = 0.20

# Deep pass: each examined frame is ALSO classified as four overlapping
# quadrant crops (each 62.5 % of the frame's width and height). NudeNet scales
# its whole input to 320 px, so a small figure in a wide frame is a handful of
# pixels to it; a quadrant doubles that figure's size in the model's input.
# This is the stricter model pass the installed weights allow — NudeNet's
# larger 640 px model is not part of this image. Tiling costs 5× the inference
# per frame, so in video it runs on at most 120 evenly spread frames.
DEEP_TILE_FRACTION = 0.625
DEEP_TILED_VIDEO_FRAMES = 120


def _tiles(frame: Frame, workdir: str, k: int) -> List[str]:
    with Image.open(frame.path) as img:
        w, h = img.size
        tw, th = int(w * DEEP_TILE_FRACTION), int(h * DEEP_TILE_FRACTION)
        out = []
        for n, (x, y) in enumerate(((0, 0), (w - tw, 0), (0, h - th), (w - tw, h - th))):
            p = os.path.join(workdir, f"tile_{k:04d}_{n}.jpg")
            img.crop((x, y, x + tw, y + th)).save(p, "JPEG", quality=95)
            out.append(p)
    return out


def nsfw(frames: List[Frame], deep: bool, workdir: str, tiled_limit: Optional[int]) -> dict:
    """
    NudeNet over every frame (and, deep, quadrant tiles of up to `tiled_limit`
    frames; None = all). Returns {score, labels, label_scores, frames, tiles}.
    """
    if not frames:
        raise MediaError("no frame could be extracted — nothing was classified",
                         code="no_frames")
    detector = nudenet.get()
    floor = NSFW_REPORT_FLOOR_DEEP if deep else NSFW_REPORT_FLOOR_FAST
    best = {}
    tiled = set()
    if deep:
        chosen = frames if tiled_limit is None else _evenly(frames, tiled_limit)
        tiled = {id(f) for f in chosen}

    def classify(image_path: str, where: str):
        try:
            detections = detector.detect(image_path)
        except Exception as e:
            raise DetectorUnavailable(f"nudenet failed on {where}: {type(e).__name__}: {e}") from e
        for det in detections:
            score = float(det["score"])
            if score < floor:
                continue
            label = str(det["class"])
            if score > best.get(label, 0.0):
                best[label] = score

    tiles_examined = 0
    for k, frame in enumerate(frames):
        where = "the image" if frame.t is None else f"frame t={frame.t}"
        classify(frame.path, where)
        if id(frame) in tiled:
            for tile in _tiles(frame, workdir, k):
                classify(tile, f"a tile of {where}")
                tiles_examined += 1

    explicit = [best[c] for c in NSFW_EXPLICIT_CLASSES if c in best]
    return {
        "score": round(max(explicit), 4) if explicit else 0.0,
        "labels": sorted(c for c in NSFW_EXPLICIT_CLASSES if c in best),
        "label_scores": {k: round(v, 4) for k, v in sorted(best.items())},
        "frames": len(frames),
        "tiles": tiles_examined,
    }


# ── gore ──────────────────────────────────────────────────────────────────────


def _gore_frame(path: str) -> float:
    """
    Fraction of pixels that are strongly red-dominant (R > 1.5·G, R > 1.5·B,
    R > 80) at 160×120, scaled ×3 and capped at 1.0. A colour proxy for blood,
    not a model: it also fires on red lighting and red objects.
    """
    try:
        with Image.open(path) as img:
            arr = np.asarray(img.convert("RGB").resize((160, 120)), dtype=np.float32)
    except (UnidentifiedImageError, OSError, ValueError) as e:
        raise MediaError(f"gore statistic could not read a sampled frame: {e}",
                         code="decode_failed") from e
    r, g, b = arr[:, :, 0], arr[:, :, 1], arr[:, :, 2]
    dominant = ((r > 1.5 * g) & (r > 1.5 * b) & (r > 80)).mean()
    return float(min(dominant * 3.0, 1.0))


def gore(frames: List[Frame]) -> dict:
    if not frames:
        raise MediaError("no frame could be extracted — nothing was measured", code="no_frames")
    return {"score": round(max(_gore_frame(f.path) for f in frames), 4), "frames": len(frames)}


# ── OCR ───────────────────────────────────────────────────────────────────────

# EasyOCR costs ~1-2 s per frame on CPU; 48 evenly spread frames reads the
# captions and burned-in text of a long video without dominating the pass.
DEEP_OCR_MAX_FRAMES = 48


def ocr(frames: List[Frame]) -> dict:
    """Text read from up to DEEP_OCR_MAX_FRAMES frames, de-duplicated, in order."""
    reader = easyocr_reader.get()
    chosen = _evenly(frames, DEEP_OCR_MAX_FRAMES)
    seen, lines = set(), []
    for frame in chosen:
        try:
            with _ocr_lock:
                found = reader.readtext(frame.path, detail=0)
        except Exception as e:
            raise DetectorUnavailable(f"easyocr failed on frame t={frame.t}: "
                                      f"{type(e).__name__}: {e}") from e
        for text in found:
            text = str(text).strip()
            if text and text.lower() not in seen:
                seen.add(text.lower())
                lines.append(text)
    return {"text": "\n".join(lines), "frames": len(chosen)}


# ── transcript ────────────────────────────────────────────────────────────────

TRANSCRIBE_SAMPLE_RATE = 16000

# Whisper is fed 10-minute windows decoded straight from the file: memory stays
# at ~38 MB of float32 per window whatever the file's length.
TRANSCRIBE_WINDOW_S = 600.0

# The fast pass transcribes the first 5 minutes; the deep pass everything.
FAST_TRANSCRIPT_S = 300.0

# The budget for decoding each window to 16 kHz PCM (whisper's own inference is
# not under it). Decoding runs at hundreds of times real time; 1 s per media
# second only trips on a decoder that has stopped making progress.
TRANSCRIBE_DECODE_SECONDS_PER_MEDIA_SECOND = 1.0


def transcribe(path: str, duration_s: float, deep: bool) -> dict:
    """
    {text, seconds, complete}. `seconds` is how much of the file was
    transcribed; `complete` is whether that is all of it.
    """
    model = whisper.get()
    limit = duration_s if deep else min(duration_s, FAST_TRANSCRIPT_S)
    pieces = []
    start = 0.0
    while start < limit:
        length = min(TRANSCRIBE_WINDOW_S, limit - start)
        pcm = b"".join(media.pcm_chunks(
            path, TRANSCRIBE_SAMPLE_RATE, 60.0,
            media.budget(length, TRANSCRIBE_DECODE_SECONDS_PER_MEDIA_SECOND),
            start_s=start, length_s=length, sample_format="f32le"))
        audio = np.frombuffer(pcm, dtype="<f4")
        if audio.size:
            try:
                segments, _info = model.transcribe(audio, beam_size=5 if deep else 1,
                                                   vad_filter=True)
                for seg in segments:
                    text = seg.text.strip()
                    if text:
                        pieces.append(text)
            except Exception as e:
                raise DetectorUnavailable(f"faster-whisper failed on {start:.0f}s-"
                                          f"{start + length:.0f}s: {type(e).__name__}: {e}") from e
        start += length
    return {"text": " ".join(pieces), "seconds": round(limit, 3),
            "complete": limit >= duration_s}


# ── text: toxicity ────────────────────────────────────────────────────────────

_HATE_PHRASES = [
    # White supremacist
    "white power", "white genocide", "14 words", "heil", "seig heil", "sieg heil",
    "great replacement", "ethnic cleansing",
    # Genocide promotion
    "gas the", "death to all", "kill all", "exterminate the",
    # Dehumanisation
    "sub-human", "subhuman", "vermin", "cockroaches", "parasites are",
    # Extremist recruitment
    "join the movement", "race war", "day of the rope",
    # Sectarian violence promotion
    "death to jews", "death to muslims", "death to christians", "death to gays",
]

_VIOLENCE_PHRASES = [
    "i will kill you", "i'll kill you", "gonna kill you", "going to kill you",
    "i will hurt you", "i'll hurt you", "you're going to die", "youre going to die",
    "shoot up the", "bomb the", "blow up the", "mass shooting", "school shooting",
]

_SELF_HARM_PHRASES = [
    "kill yourself", "kys", "end your life", "end it all", "you should die",
    "go kill yourself", "commit suicide", "neck yourself",
]


def _compile_phrases(phrases):
    # Word-bounded on both sides. Plain substring matching — what these lists
    # were matched with before — fires inside other words ("kys" in "skyscraper",
    # "heil" in "Heilbronn") and reports text that says nothing of the kind.
    return [(p, re.compile(r"(?<!\w)" + re.escape(p) + r"(?!\w)")) for p in phrases]


_TOXICITY_RULES = {
    "hate": _compile_phrases(_HATE_PHRASES),
    "violence": _compile_phrases(_VIOLENCE_PHRASES),
    "self_harm": _compile_phrases(_SELF_HARM_PHRASES),
}


def _normalise(text: str) -> str:
    return text.lower().replace("’", "'").replace("‘", "'")


def toxicity(surfaces: dict) -> dict:
    """
    Phrase matches per category across every supplied text surface.

    `surfaces` maps a source name (text, ocr, transcript) to its text; each
    match is reported as "<source>:<phrase_slug>". `sources` lists the surfaces
    that were examined, so an empty category is read against what was looked at.

    `score` is what a phrase rule can honestly say: 1.0 when any surface
    contains a listed phrase, 0.0 when surfaces were examined and none does,
    None when there was no text surface at all — nothing was examined, so there
    is no score to give.
    """
    out = {category: [] for category in _TOXICITY_RULES}
    examined = []
    for source, text in surfaces.items():
        if text is None:
            continue
        examined.append(source)
        norm = _normalise(text)
        for category, rules in _TOXICITY_RULES.items():
            for phrase, rx in rules:
                if rx.search(norm):
                    out[category].append(f"{source}:{phrase.replace(' ', '_')}")
    out["sources"] = examined
    if not examined:
        out["score"] = None
    else:
        out["score"] = 1.0 if any(out[c] for c in _TOXICITY_RULES) else 0.0
    return out


# ── text: clickbait ───────────────────────────────────────────────────────────

_CLICKBAIT_PHRASES = [
    "you won't believe", "shocking", "this is why", "what happens next",
    "mind blown", "blew my mind", "this will", "secret revealed", "insane",
    "omg", "wtf", "unbelievable", "jaw dropping", "life changing", "must see",
    "going viral", "breaks the internet", "nobody is talking about", "wait for it",
    "they don't want you to know", "doctors hate", "one weird trick", "exposed",
    "banned", "censored", "this changes everything", "finally revealed",
    "gone wrong", "gone right", "reaction", "i can't believe", "look what",
    "you need to see this", "share before deleted", "watch before removed",
    "only real fans", "first to know", "exclusive", "limited time",
]
_CLICKBAIT_RULES = _compile_phrases(_CLICKBAIT_PHRASES)
_NUMBER_CLAIM = re.compile(
    r"\b\d+\s*(reasons?|things?|ways?|secrets?|facts?|tips?|tricks?|steps?|hacks?)\b")


def clickbait(text: str) -> dict:
    """
    Rule-based clickbait score in [0, 1] and the features that produced it.
    Weights: caps ratio ×2 (≤0.40), 0.08 per '!' (≤0.20), 0.12 per bait
    phrase (≤0.25), 0.15 for a "N things" claim, 0.04 per '?' (≤0.10).
    """
    norm = _normalise(text)
    alpha = [c for c in text if c.isalpha()]
    caps_ratio = sum(1 for c in alpha if c.isupper()) / len(alpha) if alpha else 0.0
    exclamations = text.count("!")
    questions = text.count("?")
    phrase_hits = sum(1 for _p, rx in _CLICKBAIT_RULES if rx.search(norm))
    number_claim = bool(_NUMBER_CLAIM.search(norm))

    score = (min(caps_ratio * 2.0, 0.4) + min(exclamations * 0.08, 0.2)
             + min(phrase_hits * 0.12, 0.25) + (0.15 if number_claim else 0.0)
             + min(questions * 0.04, 0.1))
    signals = []
    if caps_ratio > 0.3:
        signals.append("high_caps_ratio")
    if exclamations >= 2:
        signals.append("multiple_exclamations")
    if phrase_hits >= 2:
        signals.append("clickbait_keywords")
    if number_claim:
        signals.append("numbered_list_claim")
    if questions >= 2:
        signals.append("multiple_questions")
    return {"score": round(min(score, 1.0), 4), "signals": signals}


# ── text: spam ────────────────────────────────────────────────────────────────

_URL = re.compile(r"(?i)\bhttps?://\S+|\bwww\.\S+")
_SHORTENER = re.compile(r"(?i)\b(?:bit\.ly|tinyurl\.com|t\.co|goo\.gl|cutt\.ly|is\.gd|ow\.ly|"
                        r"rebrand\.ly|shorturl\.at|rb\.gy|tiny\.cc)/")
_REPEATED_CHAR = re.compile(r"(.)\1{7,}")
_HASHTAG = re.compile(r"(?<!\w)#\w+")
_MENTION = re.compile(r"(?<!\w)@\w+")
_PHONE = re.compile(r"(?<![\d\w])\+?\d(?:[\s().-]?\d){8,14}(?![\d\w])")
_CONTACT_RULES = _compile_phrases([
    "dm me", "dm for", "message me on", "add me on", "text me", "whatsapp", "telegram",
    "kik me", "snap me", "hit me up on",
])
_FINANCIAL_LURE_RULES = _compile_phrases([
    "send btc", "send eth", "send xrp", "double your", "giveaway", "airdrop",
    "guaranteed returns", "guaranteed profit", "investment opportunity", "free money",
    "cash app me", "recovery expert",
])

# Thresholds at which a count becomes a signal. These name a pattern; they do
# not judge it — abraxas weighs the signals together with posting history.
SPAM_MANY_LINKS = 3
SPAM_HASHTAGS = 10
SPAM_MENTIONS = 8
SPAM_REPEAT_MIN_TOKENS = 8
SPAM_REPEAT_TOKEN_SHARE = 0.4


# Score weights per signal, summed and capped at 1.0 — the same shape as the
# clickbait score. A financial lure is the strongest single content signal of
# spam on a payments platform; link and contact patterns are strong but common
# in honest posts; formatting noise is weak on its own and meaningful together.
SPAM_WEIGHTS = {
    "financial_lure": 0.5,
    "link_shortener": 0.3,
    "many_links": 0.3,
    "off_platform_contact": 0.3,
    "repeated_tokens": 0.3,
    "hashtag_stuffing": 0.25,
    "mention_stuffing": 0.25,
    "phone_number": 0.2,
    "repeated_characters": 0.15,
}


def spam(text: str) -> dict:
    """{score, signals}: content-intrinsic spam patterns and their weighted sum."""
    signals = spam_signals(text)
    score = min(1.0, sum(SPAM_WEIGHTS[s] for s in signals))
    return {"score": round(score, 4), "signals": signals}


def spam_signals(text: str) -> List[str]:
    """Content-intrinsic spam patterns in the text."""
    norm = _normalise(text)
    signals = []
    if len(_URL.findall(text)) >= SPAM_MANY_LINKS:
        signals.append("many_links")
    if _SHORTENER.search(text):
        signals.append("link_shortener")
    if _REPEATED_CHAR.search(text):
        signals.append("repeated_characters")
    tokens = re.findall(r"\w+", norm)
    if len(tokens) >= SPAM_REPEAT_MIN_TOKENS:
        _token, count = Counter(tokens).most_common(1)[0]
        if count / len(tokens) >= SPAM_REPEAT_TOKEN_SHARE:
            signals.append("repeated_tokens")
    if len(_HASHTAG.findall(text)) >= SPAM_HASHTAGS:
        signals.append("hashtag_stuffing")
    if len(_MENTION.findall(text)) >= SPAM_MENTIONS:
        signals.append("mention_stuffing")
    if _PHONE.search(text):
        signals.append("phone_number")
    if any(rx.search(norm) for _p, rx in _CONTACT_RULES):
        signals.append("off_platform_contact")
    if any(rx.search(norm) for _p, rx in _FINANCIAL_LURE_RULES):
        signals.append("financial_lure")
    return signals
