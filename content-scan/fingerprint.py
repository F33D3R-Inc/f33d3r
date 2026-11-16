"""
POST /v1/fingerprint — identity signals for one file. No scores, no verdicts,
no storage: abraxas holds every registry and does every comparison.

  sha256          of the file's bytes, every kind.
  duration        seconds, from the container, every kind except image.
  phash           imagehash.phash (64-bit DCT hash, 16 hex digits) of the image
                  (kinds image, avatar, header) — first frame of an animated one.
  video_segments  [{t, phash}] for video and clip — see VIDEO SEGMENTS below.
  audio           {landmarks, robust_landmarks, chromaprint, landmark_version,
                  peaks} for voice, audio, and any video/clip that carries an
                  audio stream. See audio_landmarks.py for the landmark design.

VIDEO SEGMENTS — decision: one phash per detected shot boundary, falling back
to a fixed 2 s grid only when the whole video has no cut.

  Shot boundaries are where the picture itself changes, so they land on the
  same content instants in every copy of a video: a re-encode, a bitrate or
  resolution change, a re-mux, or a trim all move WHERE in the file a cut is,
  never WHICH frames sit either side of it. A fixed grid is the opposite: trim
  0.7 s off the head and every grid sample lands on a different frame. Shot
  phashes are therefore what lets a matcher align an excerpt or a trimmed
  re-upload against the original — the sequence of shot hashes and the gaps
  between them is a signature of the edit.

  A video with no cut at all (one static shot, a screen recording, a talking
  head) has no boundaries to align on, and one phash would describe minutes of
  footage. Those get a 2 s grid instead, which describes the whole length.

  Detection runs on a 128×128 greyscale rendition of every frame with ffmpeg's
  scene score (mean absolute difference, normalised) and a 0.30 threshold — the
  conventional cut threshold: hard cuts score 0.4-1.0, camera motion and
  lighting drift stay well under 0.2. Frame 0 always opens the first segment.
  The phash of a 128×128 greyscale frame equals, to within re-sampling noise,
  the phash of the full frame, because imagehash.phash itself reduces every
  input to 32×32 greyscale before its DCT.
"""

import base64
import json
import subprocess
import threading
from typing import Optional

import imagehash
from PIL import Image, UnidentifiedImageError

import audio_landmarks
import media
from errors import DeadlineExceeded, MediaError

# Kinds as caeor and abraxas name them. Avatars and headers are images and are
# fingerprinted exactly as images are. "encrypted" is not accepted: ciphertext
# has no perceptual content, and its sha256 is abraxas's to take itself.
IMAGE_KINDS = ("image", "avatar", "header")
VIDEO_KINDS = ("video", "clip")
AUDIO_KINDS = ("voice", "audio")
KINDS = IMAGE_KINDS + VIDEO_KINDS + AUDIO_KINDS

# ── image ─────────────────────────────────────────────────────────────────────


def image_phash(path: str) -> str:
    """imagehash.phash of the image's first frame. Raises MediaError if PIL cannot read it."""
    try:
        with Image.open(path) as img:
            img.seek(0)
            return str(imagehash.phash(img))
    except (UnidentifiedImageError, Image.DecompressionBombError, OSError, ValueError) as e:
        raise MediaError(f"the image could not be decoded: {type(e).__name__}: {e}",
                         code="image_unreadable") from e


# ── video segments ────────────────────────────────────────────────────────────

SCENE_THRESHOLD = 0.30
GRID_SECONDS = 2
SEGMENT_FRAME_SIZE = 128
_SEGMENT_FRAME_BYTES = SEGMENT_FRAME_SIZE * SEGMENT_FRAME_SIZE

# Every frame of the video is decoded for cut detection. Software decode of
# 1080p H.264 runs at several times real time on one core; 2 s per media second
# covers 4K and slow codecs and still catches a decoder that has stalled.
SEGMENT_SECONDS_PER_MEDIA_SECOND = 2.0


def _phash_frames(path: str, filters: str, deadline_s: float, what: str) -> list:
    """
    Run one ffmpeg pass that emits SEGMENT_FRAME_SIZE² greyscale frames and
    showinfo timestamps; return [{t, phash}] in order. Frames are hashed as
    they arrive, so memory does not grow with the video.
    """
    args = ["-v", "info", "-protocol_whitelist", "file", "-i", media.ffmpeg_input(path),
            "-map", "0:v:0", "-an", "-sn", "-dn",
            "-vf", filters, "-fps_mode", "vfr", "-f", "rawvideo", "pipe:1"]
    hashes = []
    with media.FFmpegRun(args, deadline_s, what, capture_pattern=media.SHOWINFO_PTS) as run:
        while True:
            frame = run.read_exact(_SEGMENT_FRAME_BYTES)
            if not frame:
                break
            if len(frame) != _SEGMENT_FRAME_BYTES:
                raise MediaError(f"{what}: ffmpeg emitted a truncated frame", code="decode_failed")
            img = Image.frombytes("L", (SEGMENT_FRAME_SIZE, SEGMENT_FRAME_SIZE), frame)
            hashes.append(str(imagehash.phash(img)))
    times = [float(m.group(2)) for m in run.captured]
    if len(times) != len(hashes):
        raise MediaError(f"{what}: ffmpeg reported {len(times)} frame timestamps for "
                         f"{len(hashes)} frames", code="decode_failed")
    return [{"t": round(t, 3), "phash": h} for t, h in zip(times, hashes)]


def video_segments(path: str, duration_s: float) -> list:
    """One phash per shot (frame 0 plus every cut), or a 2 s grid when there is no cut."""
    deadline = media.budget(duration_s, SEGMENT_SECONDS_PER_MEDIA_SECOND)
    reduce = f"scale={SEGMENT_FRAME_SIZE}:{SEGMENT_FRAME_SIZE}:flags=area,format=gray"
    shots = _phash_frames(
        path,
        f"{reduce},select='eq(n,0)+gt(scene,{SCENE_THRESHOLD:.2f})',showinfo",
        deadline, "shot detection")
    if not shots:
        raise MediaError("the video stream decoded to zero frames", code="video_empty")
    if len(shots) > 1:
        return shots
    return _phash_frames(path, f"fps=fps=1/{GRID_SECONDS},{reduce},showinfo",
                         deadline, "2 s segment grid")


# ── chromaprint ───────────────────────────────────────────────────────────────

# Chromaprint's default algorithm resamples to 11025 Hz and steps 1365 samples
# per sub-fingerprint; its first frame needs a full 4096-sample window plus a
# 16-frame filter history, which is the start-up the coverage check allows for.
CHROMAPRINT_FRAME_SECONDS = 1365.0 / 11025.0
_CHROMAPRINT_STARTUP_FRAMES = 20

# fpcalc must account for the whole decoded duration, less this tolerance:
# 2 s of edge effects plus 1 % for decoders that disagree on a container's
# trailing partial frame.
_CHROMAPRINT_COVERAGE_SLACK_S = 2.0
_CHROMAPRINT_COVERAGE_SLACK_RATIO = 0.01

# fpcalc decodes and resamples the whole file; this is generous against that.
CHROMAPRINT_SECONDS_PER_MEDIA_SECOND = 0.5


def _chromaprint_frames(fp: str) -> int:
    """
    Sub-fingerprint count from the compressed fingerprint's header: byte 0 is
    the algorithm, bytes 1-3 the frame count, big-endian. The string is
    URL-safe base64 without padding; 8 characters decode to the 6 leading bytes.
    """
    try:
        head = base64.urlsafe_b64decode(fp[:8])
    except (ValueError, TypeError) as e:
        raise MediaError(f"fpcalc returned a fingerprint that is not URL-safe base64: {e}",
                         code="chromaprint_failed") from e
    if len(head) < 4:
        raise MediaError("fpcalc returned a fingerprint too short to carry a header",
                         code="chromaprint_failed")
    return int.from_bytes(head[1:4], "big")


def chromaprint(path: str, duration_s: float) -> Optional[str]:
    """
    Chromaprint compressed fingerprint over the WHOLE file (`-length 0`), or
    None when the audio is too short for chromaprint to emit a single frame —
    fpcalc's own "Empty fingerprint" answer, which is a fact about the audio and
    not a failure. Every other fpcalc failure raises.

    Success is judged on the fingerprint printed, never on the exit code:
    fpcalc 1.5.1 exits 3 with "Error decoding audio frame (End of file)" on the
    trailing partial frame of ordinary files while printing a complete result.
    """
    timeout = media.budget(duration_s, CHROMAPRINT_SECONDS_PER_MEDIA_SECOND)
    try:
        out = subprocess.run(["fpcalc", "-length", "0", "-json", path],
                             capture_output=True, timeout=timeout)
    except subprocess.TimeoutExpired as e:
        raise DeadlineExceeded(f"fpcalc did not finish within {timeout:.0f}s") from e
    except FileNotFoundError as e:
        raise MediaError("fpcalc is not installed in this image", code="chromaprint_failed") from e
    stderr = (out.stderr or b"").decode("utf-8", "replace").strip()
    stdout = (out.stdout or b"").strip()
    if not stdout:
        if "Empty fingerprint" in stderr:
            return None
        raise MediaError(f"fpcalc exited {out.returncode} with no fingerprint: {stderr[-2000:]}",
                         code="chromaprint_failed")
    try:
        fp = json.loads(stdout).get("fingerprint") or ""
    except json.JSONDecodeError as e:
        raise MediaError(f"fpcalc printed output that is not JSON: {e}",
                         code="chromaprint_failed") from e
    if not fp:
        raise MediaError(f"fpcalc printed JSON without a fingerprint (exit {out.returncode}): "
                         f"{stderr[-2000:]}", code="chromaprint_failed")
    return fp


def _check_chromaprint_coverage(fp: str, decoded_s: float):
    """Refuse a chromaprint that stops short of the decoded audio."""
    covered = (_chromaprint_frames(fp) + _CHROMAPRINT_STARTUP_FRAMES) * CHROMAPRINT_FRAME_SECONDS
    allowed = _CHROMAPRINT_COVERAGE_SLACK_S + decoded_s * _CHROMAPRINT_COVERAGE_SLACK_RATIO
    if decoded_s - covered > allowed:
        raise MediaError(f"chromaprint covers {covered:.1f}s of {decoded_s:.1f}s of decoded "
                         f"audio — fpcalc stopped early", code="chromaprint_partial")


# ── audio ─────────────────────────────────────────────────────────────────────


def audio_signals(path: str, duration_s: float) -> dict:
    """
    Landmarks and chromaprint for the first audio stream. fpcalc runs on its
    own thread while the landmark decode streams, so the two full-length passes
    overlap instead of adding.
    """
    box = {}

    def run_fpcalc():
        try:
            box["fp"] = chromaprint(path, duration_s)
        except BaseException as e:  # carried to the request thread and re-raised there
            box["error"] = e

    worker = threading.Thread(target=run_fpcalc, name="fpcalc", daemon=True)
    worker.start()
    try:
        landmarks = audio_landmarks.extract(path, duration_s)
    finally:
        worker.join()
    if "error" in box:
        raise box["error"]

    fp = box["fp"]
    if fp is not None:
        _check_chromaprint_coverage(fp, landmarks.duration_s)
    return {
        "landmark_version": audio_landmarks.LANDMARK_VERSION,
        "peaks": landmarks.peaks,
        "landmarks": landmarks.classic,
        "robust_landmarks": landmarks.robust,
        "chromaprint": fp,
    }


# ── entry point ───────────────────────────────────────────────────────────────


def fingerprint(path: str, kind: str) -> dict:
    """Identity signals for a confined path. Raises a WorkerError on any failure."""
    result = {"kind": kind, "sha256": media.sha256_file(path)}
    if kind in IMAGE_KINDS:
        result["phash"] = image_phash(path)
        return result

    p = media.probe(path)
    duration = media.require_duration(p)
    result["duration"] = round(duration, 3)
    if kind in VIDEO_KINDS:
        if not p.has_video:
            raise MediaError(f"kind {kind!r} but the file has no video stream",
                             code="no_video_stream")
        result["video_segments"] = video_segments(path, duration)
        if p.has_audio:
            result["audio"] = audio_signals(path, duration)
        return result

    if not p.has_audio:
        raise MediaError(f"kind {kind!r} but the file has no audio stream",
                         code="no_audio_stream")
    result["audio"] = audio_signals(path, duration)
    return result
