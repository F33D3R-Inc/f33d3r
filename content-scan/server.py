"""
content-scan — the private ML worker of abraxas-shield. Port 8097.

It is called only by abraxas, over HTTP, with X-Internal-Key. It holds no
database, keeps no registry, calls nobody back and decides nothing: it reads a
file from the read-only media private volume and returns signals and scores.
abraxas stores them, compares them and decides.

Endpoints
─────────
  GET  /health          open. Liveness: the process answers.
  GET  /ready           open. Readiness: 200 when every model is loaded, 503
                        naming each one that is not. Docker's healthcheck.
  POST /v1/fingerprint  key. {path, kind} → identity signals (fingerprint.py).
  POST /v1/analyze      key. {path?, kind, text?, deep} → detector scores.
  GET  /metrics         key. Prometheus exposition.

Authentication
──────────────
Every route except /health and /ready requires X-Internal-Key equal to
INTERNAL_API_KEY, compared in constant time. The process refuses to start
without INTERNAL_API_KEY: a worker that would accept unauthenticated calls is
never brought up, not even briefly.

Paths
─────
`path` must be absolute and must resolve — after `..` and symlinks — inside
one of two read-only volumes, mounted at the same paths abraxas mounts them:
  MEDIA_PRIVATE_DIR  /data/media-private  f33d3r_media_private (caeor's store)
  LIVE_SCAN_DIR      /data/live-scan      f33d3r_live_scan (live keyframes)
Anything else is refused with 403. The process refuses to start when either
directory is absent.

Kinds
─────
image | avatar | header (all images) · video | clip · voice | audio, and for
/v1/analyze also text.

Errors
──────
Every failure is a non-2xx JSON body {"error": <code>, "detail": <text>}:
  400 bad_request · 401 unauthorized · 403 path_outside_media_root ·
  404 media_not_found · 422 media_unprocessable / decode_failed / no_audio_stream
  / no_video_stream / chromaprint_failed / … · 503 detector_unavailable ·
  504 deadline_exceeded · 500 internal_error.
There is no response that carries a score a detector did not compute.

POST /v1/analyze
────────────────
Request:  {"kind": <kind>, "path": "/data/…" (required unless kind is text),
           "text": "…" (optional; required for kind text), "deep": false}
Response (every score in [0, 1]; null means NOT COMPUTED, never zero):
  {
    "kind", "deep",
    "nsfw":       {score, labels[], label_scores{}, frames, tiles} | null (no picture),
    "gore":       float | null    (no picture),
    "ocr_text":   str | null      (deep, pictures only),
    "transcript": str | null      (files with an audio stream),
    "toxicity":   float | null    (null when there was no text surface at all),
    "toxicity_signals": {hate[], violence[], self_harm[], sources[]},
    "clickbait":  float | null,  "clickbait_signals": [str] | null   (no text supplied → null),
    "spam":       float | null,  "spam_signals":      [str] | null   (no text supplied → null),
    "labels":     [str]           every label above, namespaced,
    "examined":   {duration?, frames_requested?, frames?, nsfw_tiles?, ocr_frames?,
                   transcript_seconds?, transcript_complete?}
  }
deep=false is the upload-time pass: 12 frames across a video (4 across an
animated image), NudeNet + gore, first 5 minutes transcribed.
deep=true is the ML review pass: 1 frame/s (≤900), NudeNet on every frame plus
quadrant tiles on up to 120 of them (every frame of an image), gore, OCR on up
to 48 frames, the whole file transcribed with beam search.
"""

import hmac
import json
import logging
import os
import tempfile
import time

from flask import Flask, Response, jsonify, request
from prometheus_client import CONTENT_TYPE_LATEST, Counter, Histogram, generate_latest
from werkzeug.exceptions import HTTPException

import audio_landmarks
import fingerprint
import media
import scanner
from errors import BadRequest, MediaError, Unauthorized, WorkerError

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s: %(message)s")
logger = logging.getLogger("content-scan")

# ── configuration (refuse to start without it) ───────────────────────────────

_INTERNAL_API_KEY = os.environ.get("INTERNAL_API_KEY", "")
if not _INTERNAL_API_KEY:
    raise RuntimeError("INTERNAL_API_KEY is not set — content-scan accepts calls only from "
                       "abraxas-shield and refuses to start without the key")
_INTERNAL_API_KEY_BYTES = _INTERNAL_API_KEY.encode("utf-8")

MEDIA_ROOTS = media.media_roots_from_env()

ANALYZE_KINDS = fingerprint.KINDS + ("text",)

# Text beyond this is not a post body or a caption; it is refused rather than
# truncated, because a truncated scan reads as a scan of the whole.
MAX_TEXT_CHARS = 100_000

app = Flask(__name__)
app.config["MAX_CONTENT_LENGTH"] = 1 << 20

_OPEN_PATHS = frozenset({"/health", "/ready"})

# ── metrics ───────────────────────────────────────────────────────────────────

_requests = Counter("cs_requests_total", "Worker requests by endpoint, kind and outcome",
                    ["endpoint", "kind", "outcome"])
_duration = Histogram("cs_request_duration_seconds", "Worker request duration",
                      ["endpoint", "kind"],
                      buckets=(0.1, 0.5, 1, 2, 5, 10, 30, 60, 120, 300, 600, 1800, 3600))


# ── authentication ───────────────────────────────────────────────────────────


@app.before_request
def _require_internal_key():
    if request.path in _OPEN_PATHS:
        return None
    presented = request.headers.get("X-Internal-Key", "").encode("utf-8")
    if not hmac.compare_digest(presented, _INTERNAL_API_KEY_BYTES):
        raise Unauthorized("X-Internal-Key missing or wrong")
    return None


# ── error mapping ─────────────────────────────────────────────────────────────


@app.errorhandler(WorkerError)
def _worker_error(e: WorkerError):
    log = logger.warning if e.status < 500 else logger.error
    log("%s %s → %d %s: %s", request.method, request.path, e.status, e.code, e.detail)
    return jsonify({"error": e.code, "detail": e.detail}), e.status


@app.errorhandler(HTTPException)
def _http_error(e: HTTPException):
    return jsonify({"error": (e.name or "http_error").lower().replace(" ", "_"),
                    "detail": e.description or ""}), e.code


@app.errorhandler(Exception)
def _unexpected(e: Exception):
    logger.exception("%s %s → 500", request.method, request.path)
    return jsonify({"error": "internal_error", "detail": f"{type(e).__name__}: {e}"}), 500


# ── request parsing ───────────────────────────────────────────────────────────


def _body() -> dict:
    body = request.get_json(force=True, silent=True)
    if not isinstance(body, dict):
        raise BadRequest("request body must be a JSON object")
    return body


def _kind(body: dict, allowed) -> str:
    kind = body.get("kind")
    if kind not in allowed:
        raise BadRequest(f"kind must be one of {', '.join(allowed)}")
    return kind


def _observed(endpoint: str, kind: str, fn):
    """Run fn, recording its outcome and duration. Exceptions propagate unchanged."""
    started = time.monotonic()
    try:
        result = fn()
    except WorkerError as e:
        _requests.labels(endpoint, kind, e.code).inc()
        raise
    except Exception:
        _requests.labels(endpoint, kind, "internal_error").inc()
        raise
    finally:
        _duration.labels(endpoint, kind).observe(time.monotonic() - started)
    _requests.labels(endpoint, kind, "ok").inc()
    return result


# ── JSON streaming ────────────────────────────────────────────────────────────

# A long file's landmark lists run to hundreds of thousands of entries; they are
# serialised as they are written instead of being built into one string.
_FLUSH_CHARS = 1 << 16
_SERIES_BATCH = 4096


def _encode(obj):
    if isinstance(obj, audio_landmarks.LandmarkSeries):
        yield "["
        hashes, times, n = obj.hashes, obj.times, len(obj)
        for start in range(0, n, _SERIES_BATCH):
            stop = min(start + _SERIES_BATCH, n)
            part = ",".join(f'{{"hash":{hashes[i]},"t":{times[i]:.3f}}}' for i in range(start, stop))
            yield ("," + part) if start else part
        yield "]"
    elif isinstance(obj, dict):
        yield "{"
        for i, (key, value) in enumerate(obj.items()):
            yield ("," if i else "") + json.dumps(str(key)) + ":"
            yield from _encode(value)
        yield "}"
    elif isinstance(obj, (list, tuple)):
        yield "["
        for i, value in enumerate(obj):
            if i:
                yield ","
            yield from _encode(value)
        yield "]"
    else:
        yield json.dumps(obj)


def _stream_json(obj) -> Response:
    def chunks():
        buf, size = [], 0
        for piece in _encode(obj):
            buf.append(piece)
            size += len(piece)
            if size >= _FLUSH_CHARS:
                yield "".join(buf)
                buf, size = [], 0
        if buf:
            yield "".join(buf)
    return Response(chunks(), mimetype="application/json")


# ── health ────────────────────────────────────────────────────────────────────


@app.route("/health", methods=["GET"])
def health():
    return jsonify({"status": "ok", "service": "content-scan"})


@app.route("/ready", methods=["GET"])
def ready():
    models = {}
    all_ready = True
    for slot in scanner.MODELS:
        ok, err = slot.ready()
        models[slot.name] = {"ready": ok, "error": err}
        all_ready = all_ready and ok
    body = {"service": "content-scan", "device": scanner.device(),
            "status": "ok" if all_ready else "unavailable", "models": models}
    return jsonify(body), (200 if all_ready else 503)


@app.route("/metrics", methods=["GET"])
def metrics():
    return generate_latest(), 200, {"Content-Type": CONTENT_TYPE_LATEST}


# ── POST /v1/fingerprint ──────────────────────────────────────────────────────


@app.route("/v1/fingerprint", methods=["POST"])
def v1_fingerprint():
    body = _body()
    kind = _kind(body, fingerprint.KINDS)
    path = media.resolve_media_path(body.get("path"), MEDIA_ROOTS)
    result = _observed("fingerprint", kind, lambda: fingerprint.fingerprint(path, kind))
    logger.info("fingerprint kind=%s path=%s segments=%s landmarks=%s robust=%s",
                kind, path, len(result.get("video_segments") or []),
                len(result["audio"]["landmarks"]) if "audio" in result else "-",
                len(result["audio"]["robust_landmarks"]) if "audio" in result else "-")
    return _stream_json(result)


# ── POST /v1/analyze ──────────────────────────────────────────────────────────


def _analyze(kind: str, path, text, deep: bool) -> dict:
    result = {"kind": kind, "deep": deep, "nsfw": None, "gore": None, "ocr_text": None,
              "transcript": None}
    examined = {}
    surfaces = {}
    if text is not None:
        surfaces["text"] = text

    if path is not None:
        with tempfile.TemporaryDirectory(prefix="content-scan-") as workdir:
            frames = None
            tiled_limit = None
            if kind in fingerprint.IMAGE_KINDS:
                frames, requested = scanner.image_frames(path, deep, workdir)
            else:
                p = media.probe(path)
                duration = media.require_duration(p)
                examined["duration"] = round(duration, 3)
                if kind in fingerprint.VIDEO_KINDS:
                    if not p.has_video:
                        raise MediaError(f"kind {kind!r} but the file has no video stream",
                                         code="no_video_stream")
                    frames, requested = scanner.video_frames(path, duration, deep, workdir)
                    tiled_limit = scanner.DEEP_TILED_VIDEO_FRAMES
                elif not p.has_audio:
                    raise MediaError(f"kind {kind!r} but the file has no audio stream",
                                     code="no_audio_stream")
                if p.has_audio:
                    spoken = scanner.transcribe(path, duration, deep)
                    result["transcript"] = spoken["text"]
                    examined["transcript_seconds"] = spoken["seconds"]
                    examined["transcript_complete"] = spoken["complete"]

            if frames is not None:
                examined["frames_requested"] = requested
                result["nsfw"] = scanner.nsfw(frames, deep, workdir, tiled_limit)
                examined["frames"] = result["nsfw"]["frames"]
                examined["nsfw_tiles"] = result["nsfw"]["tiles"]
                result["gore"] = scanner.gore(frames)["score"]
                if deep:
                    read = scanner.ocr(frames)
                    result["ocr_text"] = read["text"]
                    examined["ocr_frames"] = read["frames"]
                    surfaces["ocr"] = read["text"]

    if result["transcript"] is not None:
        surfaces["transcript"] = result["transcript"]
    tox = scanner.toxicity(surfaces)
    result["toxicity"] = tox.pop("score")
    result["toxicity_signals"] = tox
    if text is not None:
        bait = scanner.clickbait(text)
        junk = scanner.spam(text)
        result["clickbait"], result["clickbait_signals"] = bait["score"], bait["signals"]
        result["spam"], result["spam_signals"] = junk["score"], junk["signals"]
    else:
        result["clickbait"] = result["clickbait_signals"] = None
        result["spam"] = result["spam_signals"] = None

    labels = []
    if result["nsfw"] is not None:
        labels += [f"nsfw:{label}" for label in result["nsfw"]["labels"]]
    for category in ("hate", "violence", "self_harm"):
        labels += [f"{category}:{hit}" for hit in tox[category]]
    if result["clickbait_signals"] is not None:
        labels += [f"clickbait:{s}" for s in result["clickbait_signals"]]
    if result["spam_signals"] is not None:
        labels += [f"spam:{s}" for s in result["spam_signals"]]
    result["labels"] = labels
    result["examined"] = examined
    return result


@app.route("/v1/analyze", methods=["POST"])
def v1_analyze():
    body = _body()
    kind = _kind(body, ANALYZE_KINDS)

    deep = body.get("deep", False)
    if not isinstance(deep, bool):
        raise BadRequest("deep must be a boolean")

    text = body.get("text")
    if text is not None:
        if not isinstance(text, str):
            raise BadRequest("text must be a string")
        if len(text) > MAX_TEXT_CHARS:
            raise BadRequest(f"text is {len(text)} characters; the limit is {MAX_TEXT_CHARS}")
        if not text.strip():
            text = None

    if kind == "text":
        if body.get("path") is not None:
            raise BadRequest("kind text takes no path")
        if text is None:
            raise BadRequest("kind text requires non-empty text")
        path = None
    else:
        path = media.resolve_media_path(body.get("path"), MEDIA_ROOTS)

    result = _observed("analyze", kind, lambda: _analyze(kind, path, text, deep))
    logger.info("analyze kind=%s deep=%s path=%s nsfw=%s gore=%s labels=%d",
                kind, deep, path,
                result["nsfw"]["score"] if result["nsfw"] else "-",
                result["gore"] if result["gore"] is not None else "-",
                len(result["labels"]))
    return jsonify(result)


# ── warm-up ───────────────────────────────────────────────────────────────────


def warm_up():
    """
    Load every model before the first request. The weights are baked into the
    image, so this is a disk load. A model that will not load does not stop the
    process: /ready answers 503 naming it and every request that needs it
    answers 503 with the load error, which tells an operator exactly what is
    broken where a crash loop would tell them nothing.
    """
    started = time.monotonic()
    for slot in scanner.MODELS:
        ok, err = slot.ready()
        if not ok:
            logger.error("%s NOT ready: %s — requests that need it answer 503", slot.name, err)
    logger.info("content-scan warm-up finished in %.1fs (device=%s, media roots=%s)",
                time.monotonic() - started, scanner.device(), ", ".join(MEDIA_ROOTS))


warm_up()


if __name__ == "__main__":
    port = int(os.environ.get("PORT", 8097))
    app.run(host="0.0.0.0", port=port, threaded=True)
