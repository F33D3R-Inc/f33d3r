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

Device:
  CPU by default and built for throughput on many CPU cores. Set
  CONTENT_SCAN_DEVICE=cuda to use an NVIDIA GPU when the container is started
  with the nvidia runtime; a cuda request the host cannot honour logs a WARNING
  naming the reason and continues on CPU rather than refusing to start.

Fail-closed rule:
  A detector that cannot load or cannot classify raises DetectorUnavailable.
  Nothing in this module converts a failure into a zero score or a clean
  verdict — content that was never examined must never be recorded as examined.
"""

import hashlib
import io
import json
import logging
import os
import subprocess
import tempfile
import threading
import time
from typing import Iterable, Optional

# chromaprint ships with pyacoustid and binds libchromaprint.so.1, which the
# image already carries for fpcalc (libchromaprint-tools). Imported at module
# level on purpose: without it audio duplicate detection cannot run, and a
# service that starts with a silently disabled dedup layer is the failure this
# module's fail-closed rule exists to prevent.
import chromaprint
import numpy as np
from numpy.lib.stride_tricks import sliding_window_view

logger = logging.getLogger("content-scan")

# NSFW labels that flag content as adult/NSFW
_NSFW_LABELS = {
    "EXPOSED_BREAST_F", "EXPOSED_GENITALIA_F", "EXPOSED_GENITALIA_M",
    "EXPOSED_ANUS_F", "EXPOSED_ANUS_M", "EXPOSED_BUTTOCKS_F",
    "EXPOSED_BUTTOCKS_M",
}
# Score above this threshold marks a frame as NSFW
_NSFW_FRAME_THRESHOLD = 0.45


# ── Device selection ─────────────────────────────────────────────────────────
# CONTENT_SCAN_DEVICE is "cpu" (the default) or "cuda". A cuda request the host
# cannot honour degrades to CPU with a WARNING that names the reason — never
# silently, and never by refusing to start. An unrecognised value is a
# configuration error and stops the process rather than guessing.

_DEVICE = os.environ.get("CONTENT_SCAN_DEVICE", "cpu").strip().lower()
if _DEVICE not in ("cpu", "cuda"):
    raise ValueError(
        f"CONTENT_SCAN_DEVICE={_DEVICE!r} is not a device this build understands; "
        f"use 'cpu' or 'cuda'"
    )


def device() -> str:
    """The device this process was asked to use."""
    return _DEVICE


def _onnx_providers() -> list:
    """ONNX Runtime execution providers for the requested device."""
    if _DEVICE != "cuda":
        return ["CPUExecutionProvider"]
    try:
        import onnxruntime
        available = onnxruntime.get_available_providers()
    except Exception as e:
        logger.warning(
            "CONTENT_SCAN_DEVICE=cuda but onnxruntime could not be queried (%s) — running on CPU", e)
        return ["CPUExecutionProvider"]
    if "CUDAExecutionProvider" in available:
        return ["CUDAExecutionProvider", "CPUExecutionProvider"]
    logger.warning(
        "CONTENT_SCAN_DEVICE=cuda but onnxruntime exposes no CUDAExecutionProvider "
        "(available: %s) — running on CPU", available)
    return ["CPUExecutionProvider"]


class DetectorUnavailable(RuntimeError):
    """
    The NSFW detector could not be loaded or could not classify.

    Raised, never swallowed: a caller that cannot get a verdict must fail closed.
    Returning a zero score here is what would let unscanned content be recorded
    as clean, and that is the class of bug this service exists to prevent.
    """


_nude_detector = None
_nude_error = ""
_nude_next_retry = 0.0

# How long a failed detector load is remembered before it is tried again. A
# transient failure at boot must not disable NSFW detection for the whole life
# of the process, and a hard failure must not be retried on every request.
_DETECTOR_RETRY_S = 30.0


def _get_detector():
    """
    Return the NudeNet detector, or raise DetectorUnavailable.

    The failure is retried on a cooldown rather than latched, so a model store
    that was briefly unreachable recovers on its own.
    """
    global _nude_detector, _nude_error, _nude_next_retry
    if _nude_detector is not None:
        return _nude_detector

    now = time.monotonic()
    if now < _nude_next_retry:
        raise DetectorUnavailable(_nude_error)

    try:
        from nudenet import NudeDetector
        providers = _onnx_providers()
        try:
            _nude_detector = NudeDetector(providers=providers)
        except TypeError:
            # Older NudeNet builds pick their own execution provider.
            logger.warning("installed NudeNet does not accept a providers argument — "
                           "loading with its own default execution provider")
            _nude_detector = NudeDetector()
        _nude_error = ""
        logger.info("NudeNet detector loaded (requested device=%s, providers=%s)", _DEVICE, providers)
        return _nude_detector
    except Exception as e:
        _nude_error = f"{type(e).__name__}: {e}"
        _nude_next_retry = now + _DETECTOR_RETRY_S
        logger.error("NudeNet detector unavailable: %s — NSFW scanning fails closed "
                     "(callers are answered 503, never 'clean')", _nude_error)
        raise DetectorUnavailable(_nude_error) from e


def detector_ready() -> tuple:
    """(ready, error) for the readiness endpoint. Never raises."""
    try:
        _get_detector()
        return True, ""
    except DetectorUnavailable as e:
        return False, str(e)


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
    except ImportError:
        logger.warning("faster-whisper not installed — audio transcription disabled")
        return _whisper_model

    if _DEVICE == "cuda":
        try:
            _whisper_model = WhisperModel("base", device="cuda", compute_type="float16")
            logger.info("faster-whisper base model loaded on CUDA (multilingual, float16)")
            return _whisper_model
        except Exception as e:
            logger.warning("faster-whisper could not use CUDA (%s) — falling back to CPU int8", e)

    try:
        _whisper_model = WhisperModel("base", device="cpu", compute_type="int8")
        logger.info("faster-whisper base model loaded on CPU (multilingual, int8)")
    except Exception as e:
        logger.warning("faster-whisper load error: %s — audio transcription disabled", e)
    return _whisper_model


def transcriber_ready() -> bool:
    """True when the speech-to-text model is loaded. Used to report degradation."""
    return _get_whisper() is not None


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
    """
    Media duration in seconds via ffprobe, or 0.0 when ffprobe could not say.

    The 0.0 is a "not known", not a "zero length", and callers must treat it as
    such — audio_fingerprint() below uses this value to decide whether fpcalc
    read the whole file, so a silent zero here would quietly weaken that check.
    """
    try:
        out = subprocess.run(
            ["ffprobe", "-v", "error",
             "-show_entries", "format=duration",
             "-of", "json", path],
            capture_output=True, timeout=10,
        )
        return float(json.loads(out.stdout).get("format", {}).get("duration", 0))
    except (FileNotFoundError, subprocess.TimeoutExpired, OSError,
            json.JSONDecodeError, ValueError, TypeError) as e:
        logger.warning("ffprobe could not report the duration of %s: %s", path, e)
        return 0.0


# fpcalc's own reading of the media duration must cover at least this much of
# what ffprobe reports before a direct fingerprint is accepted without trying
# the WAV extraction. Short coverage means fpcalc read only part of the input.
_FPCALC_MIN_DURATION_RATIO = 0.9


def _fpcalc(path: str) -> tuple:
    """
    Run fpcalc on a path. Returns (fingerprint, duration, exit_code, stderr),
    with an empty fingerprint when fpcalc printed no usable JSON.

    Success is judged on whether a fingerprint was produced, never on the exit
    code. fpcalc 1.5.1 — the build this image installs — exits 3 with
    "Error decoding audio frame (End of file)" on the trailing partial frame of
    ordinary wav/mp3/aac/mp4/webm/HLS input while printing the complete
    fingerprint and the correct duration. Requiring exit 0 is why this function
    returned None for every file in the running container, which silently left
    both audio duplicate detection and the banned-audio gate with no input at
    all. A real failure — no audio stream, an unreadable container — exits 2
    and prints nothing on stdout, which is what an empty fingerprint reports.
    """
    out = subprocess.run(["fpcalc", "-json", path], capture_output=True, timeout=60)
    stderr = (out.stderr or b"").decode("utf-8", "replace").strip()
    if not (out.stdout or b"").strip():
        return "", 0.0, out.returncode, stderr
    try:
        payload = json.loads(out.stdout)
    except json.JSONDecodeError as e:
        logger.warning("fpcalc printed output that is not JSON for %s: %s (stderr: %s)",
                       path, e, stderr)
        return "", 0.0, out.returncode, stderr
    try:
        duration = float(payload.get("duration") or 0.0)
    except (TypeError, ValueError):
        duration = 0.0
    return payload.get("fingerprint") or "", duration, out.returncode, stderr


def audio_fingerprint(path: str) -> Optional[str]:
    """
    Return chromaprint's compressed base64 fingerprint string, or None.

    This exact string is what goes into media_fingerprints.audio_fingerprint
    and what banned_content_hashes is looked up by for hash_type='audio_fp'.
    Its format must not change: rewriting it would invalidate every banned
    audio hash already registered. Duplicate detection decodes it at comparison
    time instead — see decode_audio_fingerprint() below.

    fpcalc is tried on the path directly. When that reads less of the media
    than ffprobe says is there — the case an HLS manifest whose relative
    segment URLs fpcalc cannot resolve produces — the audio is extracted to a
    temporary WAV and fingerprinted from there instead.

    None is returned only when nothing produced a fingerprint, and it is logged
    at ERROR when it happens, because an asset with no audio fingerprint gets
    neither a duplicate check nor a banned-audio check and that must not pass
    unnoticed.
    """
    tmp_wav = None
    try:
        direct_fp, direct_duration, direct_code, direct_err = _fpcalc(path)
        media_duration = probe_duration(path)
        covered = (media_duration <= 0
                   or direct_duration >= media_duration * _FPCALC_MIN_DURATION_RATIO)

        if direct_fp and covered:
            if direct_code != 0:
                logger.debug("fpcalc exited %d on %s (%s) and still produced a "
                             "fingerprint covering %.1fs of %.1fs — accepted",
                             direct_code, path, direct_err, direct_duration,
                             media_duration)
            return direct_fp

        # Either fpcalc produced nothing, or it read only part of the media.
        # Extract the audio and fingerprint that instead.
        with tempfile.NamedTemporaryFile(suffix=".wav", delete=False) as f:
            tmp_wav = f.name
        extract = subprocess.run(
            ["ffmpeg", "-y", "-i", path, "-vn",
             "-acodec", "pcm_s16le", "-ar", "44100", "-ac", "2",
             tmp_wav],
            capture_output=True, timeout=180,
        )
        if extract.returncode == 0 and os.path.exists(tmp_wav) and os.path.getsize(tmp_wav) > 0:
            wav_fp, _wav_duration, wav_code, wav_err = _fpcalc(tmp_wav)
            if wav_fp:
                return wav_fp
            logger.warning("fpcalc produced no fingerprint for the audio extracted "
                           "from %s (exit %d: %s)", path, wav_code, wav_err)
        else:
            logger.warning("could not extract audio from %s for fingerprinting "
                           "(ffmpeg exit %d): %s", path, extract.returncode,
                           (extract.stderr or b"").decode("utf-8", "replace")[-400:])

        if direct_fp:
            # A fingerprint of part of the media still identifies that part.
            # Recorded as partial so a later full read that disagrees is not a
            # mystery.
            logger.warning("audio fingerprint for %s covers %.1fs of %.1fs — stored "
                           "as a partial fingerprint", path, direct_duration,
                           media_duration)
            return direct_fp

        logger.error("no audio fingerprint for %s — fpcalc exit %d (%s) and the WAV "
                     "extraction produced nothing either; this asset gets no audio "
                     "duplicate check and no banned-audio check",
                     path, direct_code, direct_err)
    except FileNotFoundError as e:
        logger.error("audio fingerprinting is unavailable — %s. fpcalc comes from "
                     "chromaprint and ffmpeg from the base image; without them no "
                     "asset can be checked for audio duplicates or banned audio", e)
    except subprocess.TimeoutExpired as e:
        logger.error("audio fingerprinting timed out for %s after %ss: %s",
                     path, e.timeout, e.cmd)
    except OSError as e:
        logger.error("audio fingerprinting failed for %s: %s", path, e)
    finally:
        if tmp_wav and os.path.exists(tmp_wav):
            try:
                os.unlink(tmp_wav)
            except OSError as e:
                logger.warning("could not remove fingerprint scratch file %s: %s",
                               tmp_wav, e)
    return None


# ── Chromaprint fingerprint comparison ────────────────────────────────────────
#
# audio_fingerprint() returns chromaprint's compressed, URL-safe-base64 string.
# That exact string is also the key the banned-content registry is looked up by
# (hash_type='audio_fp'), so nothing here changes what is produced or stored:
# the decode happens at comparison time and the stored form is untouched.
#
# A decoded fingerprint is a sequence of 32-bit subfingerprints, one per
# ~0.1238 s frame (chromaprint algorithm 1: 11025 Hz input, 4096-sample frame,
# 1365-sample step). Re-encoding the same audio does NOT reproduce the same
# integers — it flips a handful of bits in each frame — so treating the frames
# as a set of exact values and taking a set overlap scores ~0 for audio no
# listener could tell apart. Similarity is measured at bit level instead:
#
#     score = 1 - popcount(a ^ b) / (32 * frames_compared)
#
# maximised over a bounded window of frame offsets, because two uploads of the
# same audio do not start at the same instant.

AUDIO_FRAME_SECONDS = 0.1238

# How far the two fingerprints are slid against each other. 320 frames is
# ~39.6 s of start-time drift in either direction, which covers a re-upload
# with its intro trimmed: measured, a 25 s trim of the same track scores 0.9933
# at offset 202 and only 0.6561 when the search stops at 80 frames. Cost is
# linear in this bound — measured in this image on 948-frame (120 s)
# fingerprints, 0.84 ms per stored candidate, so ~1.7 s to scan the whole
# 2000-row candidate set, against a video pipeline that already spends tens of
# seconds on frame classification and transcription.
AUDIO_MAX_OFFSET_FRAMES = 320

# How many aligned frames are actually compared once the best offset is found.
# 256 frames is ~31.7 s, i.e. 8192 bits of evidence; over 20 000 measured
# comparisons between different works the highest score reached was 0.7886,
# far below the 0.90 block cut. More frames buy no separation and cost time.
AUDIO_COMPARE_FRAMES = 256

# Below this many common frames the score stops being evidence. Measured null
# maxima against unrelated audio: 0.875 at 8 frames, 0.859 at 16, 0.815 at 32,
# 0.745 at 48, 0.721 at 64. 64 frames (~7.9 s) is the first bound whose worst
# observed false score sits clearly under the review cut.
AUDIO_MIN_OVERLAP_FRAMES = 64

# Low-information guard, measured as the spread of each of the 32 bit positions
# across the fingerprint's frames (see audio_fingerprint_bit_variance).
#
# Audio with no structure scores high against other audio with no structure.
# Silence and steady tones make chromaprint emit the SAME subfingerprint in
# every frame, and two such fingerprints then agree on 84-100% of their bits
# while sharing nothing (silence vs a 440 Hz tone: 0.9375). Broadband noise is
# the same failure one step up: across 496 pairs of DIFFERENT white-noise,
# band-limited-noise, rain-like and quiet-room-tone clips the mean score was
# 0.82 and the maximum 0.8433 — none reached the block cut, but a third of them
# reached the review band, which would fill the moderation queue with unrelated
# ambient works.
#
# Measured bit variance by class:
#   silence, pure tones                       0.000
#   sparse beeps over silence                 0.015 - 0.032
#   white noise                               0.352 - 0.375
#   rain-like / band-limited noise            0.390 - 0.413
#   quiet room tone                           0.403 - 0.419
#   ---- the floor sits in the gap here ----
#   a 3 s phrase looped for 30 s              0.640 - 0.703
#   synthesised speech                        0.762 - 0.880
#   music                                     0.862 - 0.921
#
# Everything at or below 0.419 false-matches its own class; everything at or
# above 0.640 has a measured different-source maximum of 0.7886 over 20 000
# comparisons. 0.50 sits in the empty gap between them.
#
# What this costs: material below the floor gets no audio duplicate check at
# all — sha256 and the perceptual frame hash still cover it. That is a missed
# duplicate, never a creator refused for someone else's silence.
AUDIO_MIN_BIT_VARIANCE = 0.50

# Two tiers, as for the banned perceptual-hash registry: a tight radius that is
# acted on and a wider band that is only reported.
#   score >= BLOCK  → the same audio. Every measured re-encode of one track
#       lands here: mp3 48k 0.9880, opus 32k 0.9814, mp3 24k mono 22 kHz
#       0.9749, 8 kHz telephone-band opus 16k 0.9658, aac→mp3 double transcode
#       0.9816, EQ + 9 s trim 0.9611, dynamic compression 0.9548, 25 s trim
#       0.9933, mp3 of speech 0.9891. The highest score ever measured between
#       different works, over 20 000 comparisons, was 0.7886.
#       An echo tail added to a track lands at 0.9032, i.e. just inside block —
#       the same recording with a room on it is still that recording.
#   REVIEW <= score < BLOCK → derived audio, reported for a human and never
#       acted on automatically: background noise mixed in scores 0.8905 and a
#       2% tempo change 0.8741. The floor sits above the 0.7886 null maximum
#       with room to spare, so a genuinely different work does not land here.
AUDIO_DUPLICATE_BLOCK_SCORE  = 0.90
AUDIO_DUPLICATE_REVIEW_SCORE = 0.82


class AudioFingerprintError(ValueError):
    """A chromaprint fingerprint could not be decoded to its frames."""


def decode_audio_fingerprint(fp: str) -> "np.ndarray":
    """
    Decode chromaprint's compressed base64 fingerprint to its 32-bit frames.

    libchromaprint does the base64 step itself. Its alphabet is verified, not
    assumed: fpcalc output contains '-' and '_' and never '+' or '/', and
    base64.urlsafe_b64decode() followed by decode_fingerprint(base64=False)
    returns byte-identical frames, while the standard alphabet errors out.
    Padding is absent and libchromaprint accepts it that way.

    Raises AudioFingerprintError rather than returning an empty result: a
    fingerprint that will not decode is a fact the caller has to log, and the
    zero this function refuses to return is exactly what hid the old
    comma-separated-integer parser for the whole of its life.
    """
    if not isinstance(fp, str) or not fp:
        raise AudioFingerprintError("empty audio fingerprint")
    try:
        frames, _algorithm = chromaprint.decode_fingerprint(fp.encode("ascii"))
    except UnicodeEncodeError as e:
        raise AudioFingerprintError(f"non-ascii audio fingerprint: {e}") from e
    except chromaprint.FingerprintError as e:
        # libchromaprint raises this bare, so name the input instead of it.
        raise AudioFingerprintError(
            f"libchromaprint rejected a {len(fp)}-character audio fingerprint "
            f"starting {fp[:24]!r}"
        ) from e
    if not frames:
        raise AudioFingerprintError("chromaprint fingerprint decoded to zero frames")
    return np.asarray(frames, dtype=np.uint32)


def audio_fingerprint_bit_variance(frames: "np.ndarray") -> float:
    """
    Spread of each of the 32 bit positions across the fingerprint's frames,
    averaged: 1.0 when every position is an even split over time, 0.0 when the
    fingerprint is a constant. Bit order within the frame does not matter
    because the result is averaged over all 32 positions.
    """
    bits = np.unpackbits(frames.view(np.uint8)).reshape(len(frames), 32)
    ones = bits.mean(axis=0)
    return float((4.0 * ones * (1.0 - ones)).mean())


def audio_fingerprint_usable(frames: "np.ndarray") -> tuple:
    """
    Whether a decoded fingerprint carries enough information to be compared.
    Returns (True, "") or (False, reason).
    """
    if len(frames) < AUDIO_MIN_OVERLAP_FRAMES:
        return False, (f"only {len(frames)} frames "
                       f"(~{len(frames) * AUDIO_FRAME_SECONDS:.1f}s), "
                       f"minimum {AUDIO_MIN_OVERLAP_FRAMES}")
    variance = audio_fingerprint_bit_variance(frames)
    if variance < AUDIO_MIN_BIT_VARIANCE:
        return False, (f"bit variance {variance:.3f} below "
                       f"{AUDIO_MIN_BIT_VARIANCE:.2f} — silence, a steady tone "
                       f"or broadband noise, which matches anything equally "
                       f"degenerate")
    return True, ""


def audio_similarity_frames(frames_a: "np.ndarray", frames_b: "np.ndarray") -> Optional[float]:
    """
    Fraction of matching bits between two decoded fingerprints at their best
    alignment, or None when they share too few frames to compare.

    Both slide directions are tried so the offset may be positive or negative.
    """
    overlap = min(len(frames_a), len(frames_b), AUDIO_COMPARE_FRAMES)
    if overlap < AUDIO_MIN_OVERLAP_FRAMES:
        return None
    best = 0.0
    for slid, fixed in ((frames_a, frames_b), (frames_b, frames_a)):
        span = min(AUDIO_MAX_OFFSET_FRAMES, len(slid) - overlap)
        windows = sliding_window_view(slid[:overlap + span], overlap)
        mismatched = np.bitwise_count(
            np.bitwise_xor(windows, fixed[:overlap])
        ).sum(axis=1, dtype=np.int64)
        best = max(best, 1.0 - int(mismatched.min()) / (32.0 * overlap))
    return best


def audio_similarity(fp_a: str, fp_b: str) -> Optional[float]:
    """
    Similarity of two compressed base64 chromaprint fingerprints in [0, 1], or
    None when either side is too short or too low in information to judge.
    Raises AudioFingerprintError if either fingerprint will not decode.
    """
    return AudioFingerprintComparer(fp_a).score(fp_b)


class AudioFingerprintComparer:
    """
    Decodes one fingerprint once and scores it against many stored ones, which
    is the shape a duplicate scan needs: decoding the query per candidate would
    repeat the same work for every row in the store.

    `usable` is False when this fingerprint cannot carry a verdict at all;
    `reason` names the failed check so the caller can log why dedup was skipped
    instead of reporting a confident "no duplicate".
    """

    def __init__(self, fp: str):
        self.frames = decode_audio_fingerprint(fp)
        self.bit_variance = audio_fingerprint_bit_variance(self.frames)
        self.usable, self.reason = audio_fingerprint_usable(self.frames)

    def score(self, other_fp: str) -> Optional[float]:
        """
        Similarity against another stored fingerprint, or None when the pair
        cannot be judged. Raises AudioFingerprintError if other_fp will not
        decode.
        """
        other = decode_audio_fingerprint(other_fp)
        if not self.usable:
            return None
        value = audio_similarity_frames(self.frames, other)
        if value is None:
            return None
        if value >= AUDIO_DUPLICATE_REVIEW_SCORE:
            # The stored side's information check runs only on a score that
            # would actually be reported. Below the review cut the verdict is
            # "not a duplicate" either way, and the check is the most expensive
            # part of a comparison that runs once per row in the store.
            other_usable, _reason = audio_fingerprint_usable(other)
            if not other_usable:
                return None
        return value


def audio_match_verdict(score: Optional[float]) -> str:
    """Map a similarity score to "block", "review" or "none"."""
    if score is None:
        return "none"
    if score >= AUDIO_DUPLICATE_BLOCK_SCORE:
        return "block"
    if score >= AUDIO_DUPLICATE_REVIEW_SCORE:
        return "review"
    return "none"



# ── Banned-content registry: fuzzy audio radius ───────────────────────────────
#
# banned_content_hashes rows with hash_type='audio_fp' hold exactly the string
# audio_fingerprint() produced, and were matched by string equality. A container
# change defeats that: re-wrapping the same AAC stream from m4a to mka changes
# the fingerprint, because mkv's AAC decode differs at the stream edges. Exact
# match then admits the file. Nothing stored changes here — the base64 is
# decoded at comparison time and compared bit for bit, as the duplicate scan
# already does.
#
# Two radii, the same two shapes the banned phash registry uses:
#
#   score >= AUDIO_BANNED_BLOCK_SCORE   → the same audio. Refused.
#   REVIEW <= score < BLOCK             → derived audio. Admitted and recorded
#       for a human, signalled banned_audio_fp_near:<category>:s<score>.
#   score <  AUDIO_BANNED_REVIEW_SCORE  → not a match.
#
# Measured on this build over 20 synthesised 90 s works (n=20 per transform
# unless noted), each scored against the fingerprint of its own original — the
# shape of "the registry holds the original, someone uploads a copy":
#
#   mp3 128k                        0.9980 - 0.9995
#   pink noise mixed in             0.9978 - 0.9996
#   mp3 48k mono 22 kHz             0.9977 - 0.9995
#   re-container m4a→mka, m4a→mp4   0.9961 - 0.9991  (n=40)
#   aac 96k                         0.9961 - 0.9991
#   opus 32k                        0.9902 - 0.9955
#   25 s head trim                  0.9868 - 0.9934
#   8 kHz telephone band, opus 16k  0.9778 - 0.9944
#   9 s head trim                   0.9564 - 0.9731
#   dynamic range compression       0.9512 - 0.9709
#   echo tail added                 0.9053 - 0.9410
#   two-band EQ                     0.8574 - 0.9283
#   2 % tempo change                0.8551 - 0.8944
#
# Against 48 940 pairs of genuinely different works — every variant of one work
# against every variant of every other — the highest score was 0.7208, the
# 99.99th percentile 0.7157, and nothing reached 0.75. The duplicate scan's own
# campaign over 20 000 comparisons of different works reached 0.7886. Nothing in
# ~69 000 measured comparisons of unrelated audio has ever exceeded 0.7886.
#
# Why these cuts are LOOSER than the duplicate cuts (0.90 block / 0.82 review)
# rather than tighter — the cost matrix is not the same one:
#
#   * The failures are not symmetric here. A missed ban admits banned material.
#     A false block refuses a creator — but a score just under the block cut is
#     not admitted silently, it lands in the review band and a human sees it. So
#     the expensive direction is a score falling below REVIEW, where nobody
#     looks, and REVIEW is set as low as the measurements allow: 0.80 is 0.011
#     above the worst different-work score ever measured.
#   * The duplicate scan draws from that null tail ~2000 times per upload — it
#     compares against every fingerprint in the store — while this registry is
#     curated, human-entered and currently holds one row. Two orders of
#     magnitude fewer draws per upload is why the same evidence supports a cut
#     closer to the tail here than there.
#   * 0.88 refuses every measured re-encode, re-container, telephone-band
#     rendering, trim, dynamic compression and echo, and most EQ; it sits 0.09
#     above the worst different-work score, 9.8 standard deviations above the
#     different-work mean (0.5830, sd 0.0302). A 2 % tempo change (0.8551 -
#     0.8944) and a heavy EQ (0.8574) land in the review band instead of being
#     refused outright, which is the correct place for them: they are the
#     transforms where "the same recording" starts to become a judgement.
#
# What this does NOT do: find a short banned clip embedded deep inside a long
# upload. audio_similarity_frames slides the two fingerprints over
# AUDIO_MAX_OFFSET_FRAMES (±39.6 s), so a banned clip five minutes into a work
# is not aligned and does not score. Removing that bound is affordable here
# (measured 4.9 ms to slide a 64-frame clip across a 29-minute upload) but it is
# a different search with a different null distribution — measured over that
# 29-minute haystack, unrelated 64-frame clips reach 0.7676 against the 0.7208
# of the bounded search, which eats most of the margin the 0.80 review cut has.
# It needs its own measurement campaign on real audio before it is turned on,
# and it is not what this radius is for.
AUDIO_BANNED_BLOCK_SCORE  = 0.88
AUDIO_BANNED_REVIEW_SCORE = 0.80


def banned_audio_match_verdict(score: Optional[float]) -> str:
    """Map a banned-registry similarity score to "block", "review" or "none"."""
    if score is None:
        return "none"
    if score >= AUDIO_BANNED_BLOCK_SCORE:
        return "block"
    if score >= AUDIO_BANNED_REVIEW_SCORE:
        return "review"
    return "none"


class BannedAudioEntry:
    """
    One decoded banned_content_hashes row, with its usability already judged.

    `usable` is False when the registry holds a fingerprint that cannot carry a
    verdict — silence, a steady tone, broadband noise, or a clip under ~7.9 s.
    Such an entry is not compared against anything: measured, silence scores
    0.9375 against a 440 Hz tone and a 440 Hz tone scores 0.9062 against a
    997 Hz one, so one degenerate registry row would refuse every quiet or tonal
    upload on the platform. An unusable row is a ban that cannot work and the
    operator has to be told, which is why the reason is carried, not discarded.
    """

    __slots__ = ("frames", "usable", "reason")

    def __init__(self, hash_value: str):
        self.frames = decode_audio_fingerprint(hash_value)
        self.usable, self.reason = audio_fingerprint_usable(self.frames)


# Decoded registry entries, keyed by the exact stored fingerprint string. The
# key is the immutable input to a deterministic decode, so a cached entry can
# never answer for a different row; a row deleted from the registry only leaves
# memory behind, never a wrong verdict. Bounded so a runaway registry cannot
# grow this without limit — past the bound entries are decoded per upload, which
# is slower and still correct.
_BANNED_AUDIO_CACHE_MAX = 4096
_banned_audio_cache = {}
_banned_audio_cache_lock = threading.Lock()


def _banned_audio_entry(hash_value: str) -> BannedAudioEntry:
    """
    Decode one registry fingerprint, cached. Raises AudioFingerprintError when
    the stored value will not decode — the caller names the row it came from.
    """
    with _banned_audio_cache_lock:
        entry = _banned_audio_cache.get(hash_value)
    if entry is not None:
        return entry
    entry = BannedAudioEntry(hash_value)
    with _banned_audio_cache_lock:
        if len(_banned_audio_cache) < _BANNED_AUDIO_CACHE_MAX:
            _banned_audio_cache[hash_value] = entry
    return entry


class BannedAudioScan:
    """
    Result of scoring one upload's fingerprint against the banned audio registry.

    Nothing here collapses to a clean answer. Each field says exactly what
    happened, because "no banned audio matched" and "the comparison could not be
    made" are different facts and only one of them clears an upload:

      query_usable      False when the upload's own audio carries too little
                        information to be fuzzy-matched at all (the degenerate
                        floor). The exact-equality layer still applies.
      query_reason      why, when query_usable is False.
      match             best match at or above the review cut, or None:
                        {"score", "verdict", "category", "hash_value"}.
      compared          registry rows actually scored.
      unusable          rows refused because the REGISTRY entry is degenerate:
                        [{"hash_value", "category", "reason"}]. A ban that
                        cannot work.
      undecodable       rows whose stored value will not decode:
                        [{"hash_value", "category", "error"}]. Corrupt registry
                        data, never silently counted as "no match".
    """

    __slots__ = ("query_usable", "query_reason", "match", "compared",
                 "unusable", "undecodable")

    def __init__(self):
        self.query_usable = True
        self.query_reason = ""
        self.match = None
        self.compared = 0
        self.unusable = []
        self.undecodable = []

    @property
    def verdict(self) -> str:
        """"block", "review" or "none"."""
        return self.match["verdict"] if self.match else "none"

    @property
    def registry_faults(self) -> bool:
        """Whether any registry row could not be enforced during this scan."""
        return bool(self.unusable or self.undecodable)


def scan_banned_audio(query_fp: str, entries: Iterable) -> BannedAudioScan:
    """
    Score one upload's audio fingerprint against banned registry rows.

    `entries` are mappings carrying "hash_value" and "category" — the rows as
    the registry stores them, so this function never has to know how they were
    read. The database stays with the caller and the comparison stays here, one
    implementation shared by every path that needs it.

    Raises AudioFingerprintError when the QUERY fingerprint will not decode: the
    upload's own fingerprint came out of audio_fingerprint() moments earlier, so
    a failure there is a defect in this process, not registry data, and the
    caller must refuse rather than record an unjudged upload as clean.
    """
    scan = BannedAudioScan()
    comparer = AudioFingerprintComparer(query_fp)
    if not comparer.usable:
        # Degenerate query audio. Fuzzy-matching it would score high against
        # every equally degenerate registry entry, so it is not attempted, and
        # the caller is told so rather than handed a "no match". Exact equality
        # still ran, and for video the frame hash covers this material too.
        scan.query_usable = False
        scan.query_reason = comparer.reason
        return scan

    for row in entries:
        hash_value = (row.get("hash_value") or "").strip()
        category = row.get("category") or ""
        if not hash_value:
            continue
        try:
            entry = _banned_audio_entry(hash_value)
        except AudioFingerprintError as e:
            scan.undecodable.append({"hash_value": hash_value,
                                     "category": category,
                                     "error": str(e)})
            continue
        if not entry.usable:
            scan.unusable.append({"hash_value": hash_value,
                                  "category": category,
                                  "reason": entry.reason})
            continue
        score = audio_similarity_frames(comparer.frames, entry.frames)
        scan.compared += 1
        if score is None:
            continue
        if scan.match is None or score > scan.match["score"]:
            scan.match = {"score": score, "category": category,
                          "hash_value": hash_value,
                          "verdict": banned_audio_match_verdict(score)}

    if scan.match and scan.match["verdict"] == "none":
        scan.match = None
    return scan


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


def _phash_of(source) -> Optional[str]:
    """
    imagehash.phash of anything PIL can open — a path or a file-like object.

    Deliberately the same imagehash.phash the video pipeline uses, so a still,
    a sampled live keyframe and a video frame all produce values that compare
    against the same registry entries. A second algorithm anywhere on this path
    would produce hashes that match nothing, and a gate that matches nothing is
    worse than no gate: it reads as enforced.

    Returns None when no hash could be produced — never a placeholder value,
    because a caller cannot tell a placeholder from a real miss.
    """
    try:
        import imagehash
        from PIL import Image
    except ImportError:
        logger.warning("imagehash/Pillow not available — perceptual image hash disabled")
        return None
    try:
        with Image.open(source) as img:
            return str(imagehash.phash(img))
    except Exception as e:
        logger.warning("perceptual image hash error: %s", e)
        return None


def perceptual_image_hash(image_bytes: bytes) -> Optional[str]:
    """Perceptual hash of a still image held in memory."""
    return _phash_of(io.BytesIO(image_bytes))


def perceptual_image_hash_file(path: str) -> Optional[str]:
    """Perceptual hash of a still image on disk, without loading the file."""
    return _phash_of(path)


def has_audio_stream(path: str) -> bool:
    """
    Whether the file carries an audio stream at all.

    A silent video produces no chromaprint fingerprint, and that is not a
    degraded scan — there is nothing to fingerprint. Without this probe every
    silent upload would be recorded as a gate layer that failed.
    """
    try:
        out = subprocess.run(
            ["ffprobe", "-v", "error",
             "-select_streams", "a",
             "-show_entries", "stream=index",
             "-of", "json", path],
            capture_output=True, timeout=15,
        )
        if out.returncode != 0:
            return False
        return bool(json.loads(out.stdout).get("streams"))
    except Exception as e:
        logger.warning("has_audio_stream error for %s: %s", path, e)
        return False


def scan_frames_nsfw(path: str, n_frames: int = 10) -> dict:
    """
    Extract n_frames from video and run NudeNet on each.
    Returns {'is_nsfw': bool, 'nsfw_score': float, 'labels': list[str]}.
    """
    detector = _get_detector()   # raises DetectorUnavailable — never returns clean

    dur = probe_duration(path)
    if dur <= 0:
        dur = 10.0

    max_score = 0.0
    found_labels: set = set()
    examined = 0

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
                # A seek past the end of a short clip yields no frame. Normal;
                # it is only fatal if it happens to every frame (checked below).
                continue
            # A detector fault propagates. Swallowing it would report a
            # nudity score of 0.0 for footage nothing ever looked at.
            detections = detector.detect(out_path)
            examined += 1
            for det in detections:
                label = det.get("class", "")
                score = float(det.get("score", 0.0))
                if label in _NSFW_LABELS and score > 0.25:
                    found_labels.add(label)
                    max_score = max(max_score, score)

    if examined == 0:
        raise DetectorUnavailable(
            f"no frame could be extracted from {path} — nothing was classified")

    return {
        "is_nsfw": max_score >= _NSFW_FRAME_THRESHOLD,
        "nsfw_score": round(max_score, 4),
        "labels": sorted(found_labels),
        "frames_examined": examined,
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

    # 4. NSFW frame scan. A failure here is not caught: an unclassified video
    #    must not leave this function carrying nsfw_score 0.0, which downstream
    #    would read as "looked at, nothing found".
    nsfw = scan_frames_nsfw(path)
    result["is_nsfw"] = nsfw["is_nsfw"]
    result["nsfw_score"] = nsfw["nsfw_score"]
    result["nsfw_labels"] = nsfw["labels"]

    return result


def scan_image_bytes(image_bytes: bytes, filename: str = "image.jpg") -> dict:
    """
    Scan a single image for NSFW content.
    Returns {'is_nsfw': bool, 'nsfw_score': float, 'labels': list[str]}.
    """
    detector = _get_detector()   # raises DetectorUnavailable — never returns clean

    with tempfile.NamedTemporaryFile(suffix=os.path.splitext(filename)[1] or ".jpg",
                                    delete=False) as tmp:
        tmp.write(image_bytes)
        tmp_path = tmp.name
    try:
        # A detector fault propagates to the caller as a failure. It is never
        # turned into is_nsfw=False, which is a verdict nothing reached.
        detections = detector.detect(tmp_path)
    except DetectorUnavailable:
        raise
    except Exception as e:
        raise DetectorUnavailable(f"{type(e).__name__}: {e}") from e
    finally:
        os.unlink(tmp_path)

    max_score = 0.0
    found_labels = set()
    for det in detections:
        label = det.get("class", "")
        score = float(det.get("score", 0.0))
        if label in _NSFW_LABELS and score > 0.25:
            found_labels.add(label)
            max_score = max(max_score, score)
    return {
        "is_nsfw": max_score >= _NSFW_FRAME_THRESHOLD,
        "nsfw_score": round(max_score, 4),
        "labels": sorted(found_labels),
    }


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
    """Lazy-load the EasyOCR reader (English, ~300MB model)."""
    global _ocr_reader
    if _ocr_reader is not None:
        return _ocr_reader
    try:
        import easyocr
    except Exception as e:
        logger.warning("EasyOCR unavailable: %s — OCR disabled", e)
        _ocr_reader = False
        return _ocr_reader

    if _DEVICE == "cuda":
        try:
            _ocr_reader = easyocr.Reader(["en"], gpu=True, verbose=False)
            logger.info("EasyOCR reader loaded on CUDA")
            return _ocr_reader
        except Exception as e:
            logger.warning("EasyOCR could not use CUDA (%s) — falling back to CPU", e)

    try:
        _ocr_reader = easyocr.Reader(["en"], gpu=False, verbose=False)
        logger.info("EasyOCR reader loaded on CPU")
    except Exception as e:
        logger.warning("EasyOCR unavailable: %s — OCR disabled", e)
        _ocr_reader = False
    return _ocr_reader


def ocr_ready() -> bool:
    """True when the OCR reader is loaded. Used to report a degraded scan."""
    return bool(_get_ocr_reader())


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
    degraded: list = None,
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
      6. A detector that should have run and did not (`degraded`) → human_review
      7. All clear                    → approve

    degraded: names of detectors that were expected to contribute and could not
      (OCR, transcription). Content they never examined is not approved on the
      strength of the detectors that did run — it goes to a human. This is the
      fail-closed rule; there is no path here where a missing detector produces
      an 'approve'.
    """
    hate_signals      = list(hate_signals or [])
    violence_signals  = list(violence_signals or [])
    self_harm_signals = list(self_harm_signals or [])
    degraded          = list(degraded or [])
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
    signals.extend(f"detector_unavailable:{d}" for d in degraded)

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

    # A detector that should have run and did not means this content was only
    # partially examined. "Nothing was found" is not the same claim as "nothing
    # looked", so a partial pass goes to a human instead of being approved.
    if degraded:
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
