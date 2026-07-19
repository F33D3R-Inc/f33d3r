"""
F33D3R content-scan brain — port 8097.

Endpoints:
  GET  /health
  POST /scan/video        — fingerprint + dedup + lineage resolution
  POST /scan/text         — clickbait detection
  POST /scan/image-dedup  — image SHA-256 + phash dedup check
  POST /v1/scan/image     — NSFW image scan (legacy path kept)
  POST /v1/scan/risk      — full risk pipeline (post body + video)
  GET  /metrics           — Prometheus metrics
"""

import base64
import hashlib
import io
import logging
import os
import time
import threading

import requests
from flask import Flask, request, jsonify
from prometheus_client import Counter, Histogram, generate_latest, CONTENT_TYPE_LATEST

import scanner

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s %(levelname)s %(name)s: %(message)s",
)
logger = logging.getLogger("content-scan")

app = Flask(__name__)

_scans_total    = Counter("cs_scans_total",       "Total scan requests",    ["kind", "is_nsfw"])
_scan_duration  = Histogram("cs_scan_duration_seconds", "Scan duration",     ["kind"])
_errors_total   = Counter("cs_errors_total",      "Scan errors",            ["kind"])
_dupes_total    = Counter("cs_duplicates_total",  "Duplicate detections",   ["kind"])
_risk_total     = Counter("cs_risk_decisions",    "Risk decisions by level", ["level"])

# Feed-engine callback for scan_complete events
_FEED_ENGINE_URL    = os.environ.get("FEED_ENGINE_URL", "http://feed-engine:8081")
_INTERNAL_API_KEY   = os.environ.get("INTERNAL_API_KEY", "")

# ── Postgres fingerprint store ───────────────────────────────────────────────
# media_fingerprints and banned_content_hashes live in f33d3r_feed Postgres.
# DB_* env vars are already wired in docker-compose.local.yml.

import psycopg2
import psycopg2.extras

_PG_DSN = (
    f"host={os.environ.get('DB_HOST','postgres')} "
    f"port={os.environ.get('DB_PORT','5432')} "
    f"dbname={os.environ.get('DB_NAME','f33d3r_feed')} "
    f"user={os.environ.get('DB_USER','f33d3r')} "
    f"password={os.environ.get('DB_PASSWORD','f33d3rdev')} "
    f"sslmode=disable"
)

_pg_conn = None
_pg_lock = threading.Lock()


def _get_pg():
    global _pg_conn
    with _pg_lock:
        if _pg_conn is None or _pg_conn.closed:
            _pg_conn = psycopg2.connect(_PG_DSN)
            _pg_conn.autocommit = True
            logger.info("content-scan connected to Postgres f33d3r_feed")
        return _pg_conn


def _pg_exec(sql: str, params=()):
    """Execute a write query against Postgres."""
    conn = _get_pg()
    with _pg_lock:
        try:
            with conn.cursor() as cur:
                cur.execute(sql, params)
        except psycopg2.OperationalError:
            # Reconnect once on broken pipe / server restart
            global _pg_conn
            _pg_conn = None
            conn = _get_pg()
            with conn.cursor() as cur:
                cur.execute(sql, params)


def _pg_fetch(sql: str, params=()) -> list:
    """Execute a read query and return list of dicts."""
    conn = _get_pg()
    with _pg_lock:
        try:
            with conn.cursor(cursor_factory=psycopg2.extras.RealDictCursor) as cur:
                cur.execute(sql, params)
                return [dict(r) for r in cur.fetchall()]
        except psycopg2.OperationalError:
            global _pg_conn
            _pg_conn = None
            conn = _get_pg()
            with conn.cursor(cursor_factory=psycopg2.extras.RealDictCursor) as cur:
                cur.execute(sql, params)
                return [dict(r) for r in cur.fetchall()]


# ── Similarity helpers ────────────────────────────────────────────────────────

def _audio_jaccard(fp_a: str, fp_b: str) -> float:
    """
    Jaccard similarity of the integer sets encoded in two chromaprint strings.
    Chromaprint fingerprints are comma-separated integers; Jaccard on the set
    of integers is a fast, CPU-friendly proxy for audio similarity.
    """
    try:
        set_a = set(int(x) for x in fp_a.split(",") if x.strip())
        set_b = set(int(x) for x in fp_b.split(",") if x.strip())
        if not set_a or not set_b:
            return 0.0
        return len(set_a & set_b) / len(set_a | set_b)
    except Exception:
        return 0.0


def _phash_distance(h_a: str, h_b: str) -> int:
    """Hamming distance between two hex perceptual hashes."""
    try:
        if len(h_a) != len(h_b):
            return 999
        a = int(h_a, 16)
        b = int(h_b, 16)
        xor = a ^ b
        return bin(xor).count("1")
    except Exception:
        return 999


def _find_duplicate(sha256: str, audio_fp: str, phash: str):
    """
    Search the fingerprint store for a duplicate of the given asset.
    Returns (row_dict, duplicate_type) or (None, None).

    Priority: exact (SHA-256) > audio > visual.
    """
    # 1. Exact SHA-256 match — O(1) via unique index
    if sha256:
        rows = _pg_fetch(
            "SELECT * FROM media_fingerprints WHERE sha256 = %s AND sha256 != '' LIMIT 1",
            (sha256,),
        )
        if rows:
            return rows[0], "exact"

    # 2. Audio fingerprint (Jaccard >= 0.85)
    if audio_fp:
        candidates = _pg_fetch(
            "SELECT * FROM media_fingerprints WHERE audio_fingerprint != '' LIMIT 2000",
        )
        best_score = 0.0
        best_row = None
        for row in candidates:
            score = _audio_jaccard(audio_fp, row["audio_fingerprint"])
            if score > best_score:
                best_score = score
                best_row = row
        if best_score >= 0.85 and best_row:
            return best_row, "audio"

    # 3. Perceptual hash (Hamming <= 12) — index-assisted prefix scan
    if phash:
        candidates = _pg_fetch(
            "SELECT * FROM media_fingerprints WHERE perceptual_hash != '' LIMIT 5000",
        )
        for row in candidates:
            if _phash_distance(phash, row["perceptual_hash"]) <= 12:
                return row, "visual"

    return None, None


# ── health ────────────────────────────────────────────────────────────────────

@app.route("/health")
def health():
    return jsonify({"status": "ok", "service": "content-scan"})


# ── POST /scan/video ──────────────────────────────────────────────────────────

@app.route("/scan/video", methods=["POST"])
def scan_video():
    """
    Request JSON:
    {
        "asset_id":        "uuid",
        "file_path":       "/absolute/path/to/video.mp4",
        "uploader_pial":   "uuid",
        "uploader_handle": "handle",
        "post_id":         "uuid"
    }

    Response JSON:
    {
        "asset_id":         "uuid",
        "duplicate_type":   "exact"|"audio"|"visual"|null,
        "original_pial":    "uuid"|"",
        "original_handle":  "handle"|"",
        "original_post_id": "uuid"|"",
        "is_nsfw":          bool,
        "nsfw_score":       float,
        "clickbait_signals": []
    }
    """
    data = request.get_json(force=True, silent=True) or {}
    file_path       = data.get("file_path", "")
    asset_id        = data.get("asset_id", "")
    uploader_pial   = data.get("uploader_pial", "")
    uploader_handle = data.get("uploader_handle", "")
    post_id         = data.get("post_id", "")

    if not file_path:
        return jsonify({"error": "file_path required"}), 400

    # Wait up to 60 s for the HLS master.m3u8 to appear — transcoding brain
    # writes it synchronously before returning the job URL, but network + FS
    # propagation can add a small lag.
    wait = 0
    while not __import__("os").path.exists(file_path) and wait < 60:
        time.sleep(2)
        wait += 2
    if not __import__("os").path.exists(file_path):
        logger.warning("[content-scan] file still absent after %ds: %s", wait, file_path)
        return jsonify({"error": "file_not_found_after_wait"}), 404

    start = time.time()
    try:
        scan = scanner.scan_video(file_path, asset_id)

        sha256   = scan.get("sha256") or ""
        audio_fp = scan.get("audio_fingerprint") or ""
        phash    = scan.get("perceptual_hash") or ""
        is_nsfw  = scan.get("is_nsfw", False)

        logger.info(
            "[content-scan] fingerprints: asset=%s sha256=%s audio=%s phash=%s",
            asset_id,
            sha256[:12] + "…" if sha256 else "(none)",
            "yes" if audio_fp else "(none)",
            phash[:8] + "…" if phash else "(none)",
        )

        # Duplicate detection
        match_row, dup_type = _find_duplicate(sha256, audio_fp, phash)

        original_pial    = ""
        original_handle  = ""
        original_post_id = ""
        if match_row and dup_type:
            original_pial    = match_row.get("uploader_pial", "")
            original_handle  = match_row.get("uploader_handle", "")
            original_post_id = match_row.get("post_id", "")
            _dupes_total.labels(kind=dup_type).inc()
            logger.info(
                "[content-scan] duplicate detected: asset=%s type=%s original=@%s",
                asset_id, dup_type, original_handle,
            )

        # Store this asset's fingerprints (only when it's not a duplicate).
        # ON CONFLICT on sha256: first uploader's record is preserved — never overwritten.
        if asset_id and not match_row and sha256:
            _pg_exec(
                """INSERT INTO media_fingerprints
                   (asset_id, post_id, uploader_pial, uploader_handle,
                    sha256, audio_fingerprint, perceptual_hash, is_nsfw)
                   VALUES (%s,%s,%s,%s,%s,%s,%s,%s)
                   ON CONFLICT (sha256) WHERE sha256 != '' DO NOTHING""",
                (asset_id, post_id, uploader_pial, uploader_handle,
                 sha256, audio_fp or "", phash or "", bool(is_nsfw)),
            )

        nsfw_label = "true" if is_nsfw else "false"
        _scans_total.labels(kind="video", is_nsfw=nsfw_label).inc()
        _scan_duration.labels(kind="video").observe(time.time() - start)

        return jsonify({
            "asset_id":         asset_id,
            "duplicate_type":   dup_type,
            "original_pial":    original_pial,
            "original_handle":  original_handle,
            "original_post_id": original_post_id,
            "is_nsfw":          is_nsfw,
            "nsfw_score":       scan.get("nsfw_score", 0.0),
            "clickbait_signals": [],
        })

    except Exception as e:
        _errors_total.labels(kind="video").inc()
        logger.exception("scan_video error for %s", file_path)
        return jsonify({"error": str(e)}), 500


# ── POST /scan/video-dedup ────────────────────────────────────────────────────

@app.route("/scan/video-dedup", methods=["POST"])
def scan_video_dedup():
    """
    Pre-transcoding duplicate check — accepts raw video bytes as multipart/form-data.
    Fingerprints the file and checks for duplicates WITHOUT storing the fingerprint
    (storage happens post-transcoding via /scan/video once the asset_id is known).
    Returns: {duplicate_type, original_pial, original_handle, original_post_id}
    """
    uploader_pial   = request.form.get("uploader_pial", "")
    uploader_handle = request.form.get("uploader_handle", "")

    if "file" not in request.files:
        return jsonify({"error": "file field required"}), 400

    file_obj    = request.files["file"]
    video_bytes = file_obj.read()
    if not video_bytes:
        return jsonify({"error": "empty file"}), 400

    import tempfile
    tmp_path = None
    try:
        suffix = os.path.splitext(file_obj.filename or "upload")[1] or ".mp4"
        with tempfile.NamedTemporaryFile(suffix=suffix, delete=False) as tmp:
            tmp.write(video_bytes)
            tmp_path = tmp.name

        start = time.time()
        scan     = scanner.scan_video(tmp_path, "")
        sha256   = scan.get("sha256") or ""
        audio_fp = scan.get("audio_fingerprint") or ""
        phash    = scan.get("perceptual_hash") or ""

        match_row, dup_type = _find_duplicate(sha256, audio_fp, phash)

        original_pial    = ""
        original_handle  = ""
        original_post_id = ""
        if match_row and dup_type:
            original_pial    = match_row.get("uploader_pial", "")
            original_handle  = match_row.get("uploader_handle", "")
            original_post_id = match_row.get("post_id", "")
            if original_handle == uploader_handle:
                original_pial = original_handle = original_post_id = ""
                dup_type = None
            else:
                _dupes_total.labels(kind=dup_type).inc()
                logger.info("[content-scan] pre-transcode dup type=%s original=@%s post=%s",
                            dup_type, original_handle, original_post_id)

        _scan_duration.labels(kind="video-dedup").observe(time.time() - start)
        return jsonify({
            "duplicate_type":   dup_type,
            "original_pial":    original_pial,
            "original_handle":  original_handle,
            "original_post_id": original_post_id,
        })

    except Exception as e:
        _errors_total.labels(kind="video").inc()
        logger.exception("scan_video_dedup error for uploader @%s", uploader_handle)
        return jsonify({"error": str(e)}), 500
    finally:
        if tmp_path:
            try:
                os.unlink(tmp_path)
            except Exception:
                pass


# ── POST /scan/text ───────────────────────────────────────────────────────────

@app.route("/scan/text", methods=["POST"])
def scan_text():
    """
    Request JSON: {"text": "...", "post_id": "uuid"}

    Response JSON:
    {
        "post_id":         "uuid",
        "clickbait_score": float,
        "is_clickbait":    bool,
        "signals":         list[str]
    }
    """
    data = request.get_json(force=True, silent=True) or {}
    text    = data.get("text", "")
    post_id = data.get("post_id", "")

    if not text:
        return jsonify({"error": "text required"}), 400

    start = time.time()
    try:
        result = scanner.scan_text(text)
        _scans_total.labels(kind="text", is_nsfw="false").inc()
        _scan_duration.labels(kind="text").observe(time.time() - start)
        return jsonify({
            "post_id":        post_id,
            "clickbait_score": result["clickbait_score"],
            "is_clickbait":    result["is_clickbait"],
            "signals":         result["signals"],
        })
    except Exception as e:
        _errors_total.labels(kind="text").inc()
        logger.exception("scan_text error for post %s", post_id)
        return jsonify({"error": str(e)}), 500


# ── POST /v1/scan/video (legacy path — kept for backward compat) ──────────────

@app.route("/v1/scan/video", methods=["POST"])
def v1_scan_video():
    """
    Legacy endpoint — returns raw scan result without dedup lookup.
    Old callers send {asset_id, file_path}.
    """
    data = request.get_json(force=True, silent=True) or {}
    file_path = data.get("file_path", "")
    asset_id  = data.get("asset_id", "")

    if not file_path:
        return jsonify({"error": "file_path required"}), 400

    start = time.time()
    try:
        result = scanner.scan_video(file_path, asset_id)
        nsfw_label = "true" if result.get("is_nsfw") else "false"
        _scans_total.labels(kind="video", is_nsfw=nsfw_label).inc()
        _scan_duration.labels(kind="video").observe(time.time() - start)
        return jsonify(result)
    except Exception as e:
        _errors_total.labels(kind="video").inc()
        logger.exception("v1_scan_video error for %s", file_path)
        return jsonify({"error": str(e)}), 500


# ── POST /v1/scan/image ───────────────────────────────────────────────────────

@app.route("/v1/scan/image", methods=["POST"])
def scan_image():
    """
    Request JSON: {"image_b64": "<base64>", "filename": "photo.jpg"}
    """
    data = request.get_json(force=True, silent=True) or {}
    b64  = data.get("image_b64", "")
    name = data.get("filename", "image.jpg")

    if not b64:
        return jsonify({"error": "image_b64 required"}), 400

    try:
        image_bytes = base64.b64decode(b64)
    except Exception:
        return jsonify({"error": "invalid base64"}), 400

    start = time.time()
    try:
        result = scanner.scan_image_bytes(image_bytes, name)
        nsfw_label = "true" if result.get("is_nsfw") else "false"
        _scans_total.labels(kind="image", is_nsfw=nsfw_label).inc()
        _scan_duration.labels(kind="image").observe(time.time() - start)
        return jsonify(result)
    except Exception as e:
        _errors_total.labels(kind="image").inc()
        logger.exception("scan_image error")
        return jsonify({"error": str(e)}), 500


# ── POST /scan/image-dedup ────────────────────────────────────────────────────

@app.route("/scan/image-dedup", methods=["POST"])
def scan_image_dedup():
    """
    Image duplicate detection via SHA-256 + perceptual hash.
    Called synchronously before a new image is accepted, so we can stop
    the upload and return the existing canonical URL to the composer.

    Request:  {"image_b64": "<base64>", "filename": "photo.jpg",
               "uploader_pial": "...", "uploader_handle": "...",
               "image_url": "/static/media/posts/uuid-feed.webp"}
    Response: {"is_duplicate": bool, "duplicate_type": "exact"|"visual"|null,
               "original_handle": "...", "original_pial": "...",
               "original_post_id": "...", "image_url": "..."}
    """
    data             = request.get_json(force=True, silent=True) or {}
    b64              = data.get("image_b64", "")
    uploader_pial    = data.get("uploader_pial", "")
    uploader_handle  = data.get("uploader_handle", "")
    image_url        = data.get("image_url", "")

    if not b64:
        return jsonify({"error": "image_b64 required"}), 400

    try:
        image_bytes = base64.b64decode(b64)
    except Exception:
        return jsonify({"error": "invalid base64"}), 400

    sha256 = hashlib.sha256(image_bytes).hexdigest()

    # Perceptual hash via imagehash (same library used by the video pipeline).
    phash = ""
    try:
        import imagehash
        from PIL import Image as PILImage
        img = PILImage.open(io.BytesIO(image_bytes))
        phash = str(imagehash.phash(img))
    except Exception:
        pass

    match_row, dup_type = _find_duplicate(sha256, "", phash)

    if match_row:
        _dupes_total.labels(kind="image_" + (dup_type or "unknown")).inc()
        return jsonify({
            "is_duplicate":   True,
            "duplicate_type": dup_type,
            "original_handle":  match_row.get("uploader_handle", ""),
            "original_pial":    match_row.get("uploader_pial", ""),
            "original_post_id": match_row.get("post_id", ""),
            "image_url":        match_row.get("image_url", ""),
        })

    # Not a duplicate — store fingerprint so future uploads are caught.
    # ON CONFLICT on sha256: first uploader's record is authoritative, never overwritten.
    _pg_exec(
        """INSERT INTO media_fingerprints
           (asset_id, post_id, uploader_pial, uploader_handle,
            sha256, perceptual_hash, is_nsfw, image_url)
           VALUES (%s, '', %s, %s, %s, %s, FALSE, %s)
           ON CONFLICT (sha256) WHERE sha256 != '' DO NOTHING""",
        (sha256, uploader_pial, uploader_handle, sha256, phash or "", image_url),
    )

    return jsonify({"is_duplicate": False, "duplicate_type": None})


# ── Banned hash helpers ───────────────────────────────────────────────────────

def _is_banned_hash(hash_type: str, hash_value: str) -> tuple:
    """Returns (True, category) if in banned_content_hashes, (False, '') otherwise."""
    if not hash_value:
        return False, ""
    rows = _pg_fetch(
        "SELECT category FROM banned_content_hashes WHERE hash_type = %s AND hash_value = %s LIMIT 1",
        (hash_type, hash_value),
    )
    if rows:
        return True, rows[0]["category"]
    return False, ""


def _notify_feed_engine(post_id: str, payload: dict):
    """Fire-and-forget: send scan_complete result to feed-engine internal endpoint."""
    if not _FEED_ENGINE_URL or not post_id:
        return
    try:
        url = f"{_FEED_ENGINE_URL}/api/internal/scan-complete"
        payload["post_id"] = post_id
        resp = requests.post(
            url,
            json=payload,
            headers={"X-Internal-Key": _INTERNAL_API_KEY},
            timeout=5,
        )
        if not resp.ok:
            logger.warning("[content-scan] feed-engine callback failed: %s %s", resp.status_code, resp.text[:200])
    except Exception as e:
        logger.warning("[content-scan] feed-engine callback error: %s", e)


# ── POST /v1/scan/risk ────────────────────────────────────────────────────────

@app.route("/v1/scan/risk", methods=["POST"])
def scan_risk():
    """
    Unified risk assessment endpoint — runs all detectors and returns aggregated decision.

    Request JSON:
    {
        "post_id":        "uuid",
        "asset_id":       "uuid",
        "file_path":      "/path/to/media" (optional),
        "image_b64":      "<base64>"        (optional),
        "text":           "post body text"  (optional),
        "uploader_pial":  "uuid",
        "uploader_handle": "handle",
        "notify_callback": true             (if true, POST scan_complete to feed-engine)
    }

    Response JSON:
    {
        "post_id":          "uuid",
        "risk_level":       "clean|age_gate|review|block",
        "recommendation":   "approve|age_gate|human_review|auto_block",
        "nudity_score":     float,
        "gore_score":       float,
        "clickbait_score":  float,
        "ocr_text":         str,
        "hate_signals":     list[str],
        "signals":          list[str],
        "is_duplicate":     bool,
        "duplicate_type":   str,
        "original_post_id": str,
        "banned_hash":      bool,
        "banned_category":  str
    }
    """
    data = request.get_json(force=True, silent=True) or {}
    post_id          = data.get("post_id", "")
    asset_id         = data.get("asset_id", "")
    file_path        = data.get("file_path", "")
    image_b64        = data.get("image_b64", "")
    text             = data.get("text", "")
    uploader_pial    = data.get("uploader_pial", "")
    uploader_handle  = data.get("uploader_handle", "")
    notify           = data.get("notify_callback", False)
    # skip_nudity: NSFW-verified creators (is_adult_creator) — nudity scan bypassed.
    # skip_gore:   Journalists / documentary / medical creators — gore scan bypassed.
    # Hard gates (CSAM/banned_hash, hate, violence, self-harm) always apply regardless.
    skip_nudity = bool(data.get("skip_nudity", False))
    skip_gore   = bool(data.get("skip_gore",   False))

    start = time.time()
    result = {
        "post_id":           post_id,
        "asset_id":          asset_id,
        "nudity_score":      0.0,
        "gore_score":        0.0,
        "clickbait_score":   0.0,
        "ocr_text":          "",
        "transcript":        "",
        "hate_signals":      [],
        "violence_signals":  [],
        "self_harm_signals": [],
        "signals":           [],
        "risk_level":        "clean",
        "recommendation":    "approve",
        "is_duplicate":      False,
        "duplicate_type":    "",
        "original_post_id":  "",
        "original_handle":   "",
        "original_pial":     "",
        "banned_hash":       False,
        "banned_category":   "",
        "scan_version":      "2.1",
    }

    try:
        # ── Text scan (post body) ─────────────────────────────────────────────
        if text:
            text_result = scanner.scan_text(text)
            result["clickbait_score"]   = text_result["clickbait_score"]
            result["hate_signals"]      = scanner.detect_hate_signals(text)
            result["violence_signals"]  = scanner.detect_violence_signals(text)
            result["self_harm_signals"] = scanner.detect_self_harm_signals(text)

        # ── Video file scan ───────────────────────────────────────────────────
        if file_path and __import__("os").path.exists(file_path):
            # Banned hash check — before any heavy scan
            video_scan = scanner.scan_video(file_path, asset_id)
            sha256   = video_scan.get("sha256", "")
            phash    = video_scan.get("perceptual_hash", "")
            audio_fp = video_scan.get("audio_fingerprint", "")

            banned, ban_cat = _is_banned_hash("sha256", sha256)
            if not banned and phash:
                banned, ban_cat = _is_banned_hash("phash", phash)
            if not banned and audio_fp:
                banned, ban_cat = _is_banned_hash("audio_fp", audio_fp)

            if banned:
                result["banned_hash"]     = True
                result["banned_category"] = ban_cat
                result["risk_level"]      = "block"
                result["recommendation"]  = "auto_block"
                result["signals"].append(f"banned_hash:{ban_cat}")
                _risk_total.labels(level="block").inc()
                if notify and post_id:
                    threading.Thread(target=_notify_feed_engine, args=(post_id, dict(result)), daemon=True).start()
                return jsonify(result)

            # Duplicate detection
            match_row, dup_type = _find_duplicate(sha256, audio_fp, phash)
            if match_row:
                result["is_duplicate"]     = True
                result["duplicate_type"]   = dup_type
                result["original_post_id"] = match_row.get("post_id", "")
                result["original_handle"]  = match_row.get("uploader_handle", "")
                result["original_pial"]    = match_row.get("uploader_pial", "")
            elif asset_id and sha256:
                _pg_exec(
                    """INSERT INTO media_fingerprints
                       (asset_id, post_id, uploader_pial, uploader_handle,
                        sha256, audio_fingerprint, perceptual_hash, is_nsfw)
                       VALUES (%s,%s,%s,%s,%s,%s,%s,%s)
                       ON CONFLICT (sha256) WHERE sha256 != '' DO NOTHING""",
                    (asset_id, post_id, uploader_pial, uploader_handle,
                     sha256, audio_fp or "", phash or "", False),
                )

            # NSFW / nudity score
            result["nudity_score"] = video_scan.get("nsfw_score", 0.0)

            # Gore heuristic
            result["gore_score"] = scanner.gore_score_video(file_path)

            # OCR on first frame — text embedded in video
            result["ocr_text"] = scanner.extract_ocr_text(file_path)

            # Audio transcription — speech-to-text → text moderation pipeline.
            # This is the enterprise audio moderation approach: not raw waveform
            # classification, but transcript → same text stack as post body/OCR.
            transcript = scanner.transcribe_audio(file_path)
            result["transcript"] = transcript[:2000] if transcript else ""  # truncate for storage
            if transcript:
                # Merge any transcript signals into top-level signal lists
                result["violence_signals"]  = list(set(result["violence_signals"]) | set(scanner.detect_violence_signals(transcript)))
                result["self_harm_signals"] = list(set(result["self_harm_signals"]) | set(scanner.detect_self_harm_signals(transcript)))

        # ── Image scan ────────────────────────────────────────────────────────
        elif image_b64:
            try:
                image_bytes = base64.b64decode(image_b64)
            except Exception:
                return jsonify({"error": "invalid base64"}), 400

            nsfw_result = scanner.scan_image_bytes(image_bytes)
            result["nudity_score"] = nsfw_result.get("nsfw_score", 0.0)
            result["gore_score"]   = scanner.gore_score_image(image_bytes)

            # OCR on image
            try:
                import tempfile, os as _os
                with tempfile.NamedTemporaryFile(suffix=".jpg", delete=False) as tmp:
                    tmp.write(image_bytes)
                    tmp_path = tmp.name
                result["ocr_text"] = scanner.extract_ocr_text(tmp_path)
                _os.unlink(tmp_path)
            except Exception:
                pass

        # ── Risk aggregation (frame + OCR + audio transcript + text) ─────────
        risk = scanner.compute_risk(
            nudity_score      = result["nudity_score"],
            gore_score        = result["gore_score"],
            clickbait_score   = result["clickbait_score"],
            hate_signals      = result["hate_signals"],
            ocr_text          = result["ocr_text"],
            transcript        = result["transcript"],
            violence_signals  = result["violence_signals"],
            self_harm_signals = result["self_harm_signals"],
            skip_nudity = skip_nudity,
            skip_gore   = skip_gore,
        )
        result["risk_level"]     = risk["risk_level"]
        result["recommendation"] = risk["recommendation"]
        result["signals"]        = risk["signals"]

        _risk_total.labels(level=risk["risk_level"]).inc()
        _scan_duration.labels(kind="risk").observe(time.time() - start)

        # ── Callback to feed-engine ───────────────────────────────────────────
        if notify and post_id:
            threading.Thread(target=_notify_feed_engine, args=(post_id, dict(result)), daemon=True).start()

        return jsonify(result)

    except Exception as e:
        _errors_total.labels(kind="risk").inc()
        logger.exception("scan_risk error for post %s", post_id)
        return jsonify({"error": str(e)}), 500


# ── POST /v1/hashes/check ─────────────────────────────────────────────────────

@app.route("/v1/hashes/check", methods=["POST"])
def hashes_check():
    """
    Check if a hash appears in the banned registry.
    Request JSON: {"hash_type": "sha256"|"phash"|"audio_fp", "hash_value": "..."}
    """
    data = request.get_json(force=True, silent=True) or {}
    hash_type  = data.get("hash_type", "")
    hash_value = data.get("hash_value", "")
    if not hash_type or not hash_value:
        return jsonify({"error": "hash_type and hash_value required"}), 400
    banned, category = _is_banned_hash(hash_type, hash_value)
    return jsonify({"banned": banned, "category": category})


# ── POST /v1/hashes/add ───────────────────────────────────────────────────────

@app.route("/v1/hashes/add", methods=["POST"])
def hashes_add():
    """
    Register a hash in the banned registry. Admin-only via X-Internal-Key.
    Request JSON: {"hash_type", "hash_value", "category", "note"}
    """
    key = request.headers.get("X-Internal-Key", "")
    if not _INTERNAL_API_KEY or key != _INTERNAL_API_KEY:
        return jsonify({"error": "forbidden"}), 403

    data = request.get_json(force=True, silent=True) or {}
    hash_type  = data.get("hash_type", "")
    hash_value = data.get("hash_value", "")
    category   = data.get("category", "unknown")
    note       = data.get("note", "")

    if not hash_type or not hash_value:
        return jsonify({"error": "hash_type and hash_value required"}), 400

    _pg_exec(
        """INSERT INTO banned_content_hashes (hash_type, hash_value, category, added_by, note)
           VALUES (%s, %s, %s, 'content-scan', %s)
           ON CONFLICT (hash_type, hash_value) DO NOTHING""",
        (hash_type, hash_value, category, note),
    )
    return jsonify({"ok": True})


# ── metrics ───────────────────────────────────────────────────────────────────

@app.route("/metrics")
def metrics():
    return generate_latest(), 200, {"Content-Type": CONTENT_TYPE_LATEST}


# ── entrypoint ────────────────────────────────────────────────────────────────

if __name__ == "__main__":
    port = int(os.environ.get("PORT", 8097))
    logger.info("Warming up NudeNet detector…")
    scanner._get_detector()
    logger.info("Pre-fitting LightGBM clickbait model…")
    scanner._get_lgbm_model()
    logger.info("content-scan listening on :%d", port)
    app.run(host="0.0.0.0", port=port, threaded=True)
