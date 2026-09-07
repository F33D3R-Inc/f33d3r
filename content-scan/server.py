"""
F33D3R content-scan brain — port 8097.

Endpoints:
  GET  /health            — liveness: the process is up
  GET  /ready             — readiness: the NSFW detector is loaded (503 when not)
  POST /scan/video        — fingerprint + dedup + lineage resolution
  POST /scan/video-dedup  — pre-transcode duplicate check (multipart)
  POST /scan/text         — clickbait detection
  POST /scan/image-dedup  — image SHA-256 + phash dedup check
  POST /v1/scan/video     — raw video scan without dedup lookup
  POST /v1/scan/image     — NSFW + gore image scan with a risk decision
  POST /v1/scan/risk      — full risk pipeline (post body + video/image)
  POST /v1/hashes/check   — banned-hash lookup by hash value
  POST /v1/hashes/check-media — layered banned-hash check over raw media bytes
  POST /v1/hashes/add     — banned-hash registration (X-Internal-Key)
  GET  /metrics           — Prometheus metrics

Fail-closed rule:
  When the NSFW detector cannot classify, every scan endpoint answers 503 with
  an explicit reason. It never answers 200 with a zero score. A caller that
  cannot get a verdict must hold its content unscanned; a caller that is handed
  a fabricated clean verdict cannot tell the difference, and that is exactly how
  unscanned content ends up recorded as clean.

/v1/scan/image response (the live lane's classifier contract):
  {
    "is_nsfw": bool, "nsfw_score": float, "labels": [str],
    "gore_score": float,
    "risk_level": "clean|age_gate|review|block",
    "recommendation": "approve|age_gate|human_review|auto_block",
    "signals": [str]
  }
"""

import base64
import hashlib
import logging
import os
import tempfile
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
_unavailable    = Counter("cs_detector_unavailable_total",
                          "Scans refused because a detector could not classify", ["kind"])

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
    # A registry the gate cannot reach must be answered as unreachable, not
    # waited on. Without this the connect blocks on an unreachable Postgres
    # until the caller's own deadline expires — measured, /v1/hashes/check-media
    # hung for the full 300 s video timeout — which stalls every upload instead
    # of returning the 503 the caller already knows how to act on.
    f"connect_timeout=5 "
    f"sslmode=disable"
)

_pg_conn = None
# Re-entrant on purpose. The lock serialises use of the one shared connection,
# and the reconnect path inside _pg_exec/_pg_fetch calls _get_pg() while already
# holding it. With a plain Lock that call deadlocks the thread against itself
# the first time Postgres drops the connection — measured: after one Postgres
# restart the request wedged forever holding the lock, every later registry read
# blocked behind it, and /v1/hashes/check-media stopped answering at all, which
# leaves the banned gate permanently unanswered rather than refusing.
_pg_lock = threading.RLock()


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

# Audio similarity lives in scanner alongside audio_fingerprint(), because the
# comparison and the producer of the value have to agree on its encoding, and
# because sweep.py needs the identical scoring — two copies of a metric drift
# apart and this one already spent its whole life wrong in both files.


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


def _closest_audio_match(audio_fp: str):
    """
    Best-scoring stored fingerprint for this audio, or None.
    Returns {"score", "row", "verdict"} where verdict is "block" or "review".

    A fingerprint that will not decode is logged with the row that holds it and
    skipped; it is never scored as 0.0. A query fingerprint that carries too
    little information (silence, a steady tone, a clip under ~8 s) disables the
    layer for this asset and says so, because "no audio duplicate" and "audio
    could not be judged" are different answers.
    """
    try:
        comparer = scanner.AudioFingerprintComparer(audio_fp)
    except scanner.AudioFingerprintError as e:
        logger.error("[content-scan] audio dedup skipped — query fingerprint "
                     "will not decode: %s", e)
        return None

    if not comparer.usable:
        logger.info("[content-scan] audio dedup skipped for this asset — %s",
                    comparer.reason)
        return None

    candidates = _pg_fetch(
        "SELECT * FROM media_fingerprints WHERE audio_fingerprint != '' LIMIT 2000",
    )
    best = None
    for row in candidates:
        try:
            score = comparer.score(row["audio_fingerprint"])
        except scanner.AudioFingerprintError as e:
            logger.error("[content-scan] media_fingerprints asset=%s holds an "
                         "audio fingerprint that will not decode: %s",
                         row.get("asset_id"), e)
            continue
        if score is None:
            continue
        if best is None or score > best["score"]:
            best = {"score": score, "row": row}
    if best is None:
        return None
    verdict = scanner.audio_match_verdict(best["score"])
    if verdict == "none":
        return None
    best["verdict"] = verdict
    return best


def _find_duplicate(sha256: str, audio_fp: str, phash: str):
    """
    Search the fingerprint store for a duplicate of the given asset.
    Returns (row_dict, duplicate_type, audio_near_match).

    duplicate_type is set only for a match the platform acts on — it drives
    lineage reattribution and post redirection downstream, so the review-band
    audio match is returned separately as audio_near_match and reported rather
    than acted on, mirroring the banned-phash registry's block/review radii.

    Priority: exact (SHA-256) > audio > visual.
    """
    # 1. Exact SHA-256 match — O(1) via unique index
    if sha256:
        rows = _pg_fetch(
            "SELECT * FROM media_fingerprints WHERE sha256 = %s AND sha256 != '' LIMIT 1",
            (sha256,),
        )
        if rows:
            return rows[0], "exact", None

    # 2. Audio fingerprint — bit similarity over aligned chromaprint frames
    audio_near = None
    if audio_fp:
        match = _closest_audio_match(audio_fp)
        if match and match["verdict"] == "block":
            return match["row"], "audio", None
        if match and match["verdict"] == "review":
            audio_near = match

    # 3. Perceptual hash (Hamming <= 12) — index-assisted prefix scan
    if phash:
        candidates = _pg_fetch(
            "SELECT * FROM media_fingerprints WHERE perceptual_hash != '' LIMIT 5000",
        )
        for row in candidates:
            if _phash_distance(phash, row["perceptual_hash"]) <= 12:
                return row, "visual", audio_near

    return None, None, audio_near


def _audio_near_match_report(near, context: str) -> dict:
    """
    Response block for a review-band audio match, plus the warning that records
    it. Reported only — the platform does not reattribute a work on this tier,
    because at 0.82-0.90 the audio is derived rather than identical and a wrong
    call here would take a legitimate creator's upload away from them.
    """
    row = near["row"]
    payload = {
        "score":            round(near["score"], 4),
        "original_handle":  row.get("uploader_handle", ""),
        "original_pial":    row.get("uploader_pial", ""),
        "original_post_id": row.get("post_id", ""),
    }
    logger.warning("[content-scan] %s: audio %.4f similar to @%s's asset %s — "
                   "below the %.2f block cut, reported for review not acted on",
                   context, near["score"], payload["original_handle"],
                   row.get("asset_id", ""), scanner.AUDIO_DUPLICATE_BLOCK_SCORE)
    return payload


# ── Banned audio registry: exact first, then the fuzzy radius ─────────────────
#
# The registry stores chromaprint's compressed base64 exactly as
# scanner.audio_fingerprint() produced it, and exact string equality still runs
# first — it is cheaper and it is certain. What equality cannot do is survive a
# container change: the same AAC stream rewrapped m4a → mka fingerprints
# differently and walks straight through an exact gate. scanner.scan_banned_audio
# scores the decoded frames instead, at the radii scanner.AUDIO_BANNED_* document
# and measure. Nothing stored changes.

_banned_registry_faults = Counter(
    "cs_banned_registry_faults_total",
    "Banned-registry rows that could not be enforced",
    ["hash_type", "fault"],
)


def _banned_audio_rows() -> list:
    """Every audio_fp row in the banned registry. Raises psycopg2.Error upward."""
    return _pg_fetch(
        "SELECT hash_value, category FROM banned_content_hashes WHERE hash_type = 'audio_fp'"
    )


def _scan_banned_audio(audio_fp: str, context: str):
    """
    Score one upload's audio fingerprint against the banned registry and report
    every way the scan fell short of a full answer.

    Returns a scanner.BannedAudioScan. Raises psycopg2.Error when the registry
    cannot be read and scanner.AudioFingerprintError when the fingerprint this
    process just produced will not decode — both are refusals to answer, and the
    caller turns them into a non-200, never into "not banned".

    A registry row that will not decode, or that holds audio too degenerate to
    match anything (silence, a tone, noise), is logged at ERROR naming the row
    and counted. It is skipped, never scored, and never allowed to abort the
    scan of the rest of the registry — one bad row must not disable the gate.
    """
    scan = scanner.scan_banned_audio(audio_fp, _banned_audio_rows())

    for row in scan.undecodable:
        _banned_registry_faults.labels(hash_type="audio_fp", fault="undecodable").inc()
        logger.error("[content-scan] %s: banned_content_hashes row hash_type=audio_fp "
                     "category=%s hash_value=%s… will not decode (%s) — this ban is NOT "
                     "being enforced and the row must be re-registered",
                     context, row["category"], row["hash_value"][:24], row["error"])
    for row in scan.unusable:
        _banned_registry_faults.labels(hash_type="audio_fp", fault="unusable").inc()
        logger.error("[content-scan] %s: banned_content_hashes row hash_type=audio_fp "
                     "category=%s hash_value=%s… cannot be matched — %s. This ban is NOT "
                     "being enforced: matching on it would refuse every equally "
                     "featureless upload on the platform, so the row is skipped and must "
                     "be replaced with a fingerprint of the actual material",
                     context, row["category"], row["hash_value"][:24], row["reason"])
    if not scan.query_usable:
        logger.info("[content-scan] %s: banned-audio fuzzy radius skipped — %s. Exact "
                    "fingerprint equality still applied, as did sha256 and the frame hash",
                    context, scan.query_reason)
    return scan


def _banned_audio_near(scan, context: str) -> dict:
    """
    near_match block for a review-band banned-audio score, plus the warning that
    records it. Reported and admitted — never blocked — for the same reason the
    banned phash registry admits its outer band: at 0.80-0.88 the audio is
    derived rather than identical, and refusing a creator on a derivation is the
    false-positive failure this gate has to avoid.
    """
    match = scan.match
    logger.warning("[content-scan] %s: audio %.4f similar to banned %s content "
                   "(registry %s…) — below the %.2f block cut, admitted and recorded "
                   "for review", context, match["score"], match["category"],
                   match["hash_value"][:24], scanner.AUDIO_BANNED_BLOCK_SCORE)
    return {"hash_type": "audio_fp",
            "score": round(match["score"], 4),
            "category": match["category"]}


# ── health ────────────────────────────────────────────────────────────────────

@app.route("/health")
def health():
    """Liveness. The process is up and answering; says nothing about models."""
    return jsonify({"status": "ok", "service": "content-scan"})


@app.route("/ready")
def ready():
    """
    Readiness. 200 only when the NSFW detector can classify, because that is the
    one model every scan endpoint depends on. 503 carries the exact load error so
    an operator does not have to guess which model is missing.
    """
    detector_ok, detector_err = scanner.detector_ready()
    body = {
        "service":     "content-scan",
        "device":      scanner.device(),
        "detectors": {
            "nsfw":       {"ready": detector_ok, "error": detector_err},
            "ocr":        {"ready": scanner.ocr_ready()},
            "transcribe": {"ready": scanner.transcriber_ready()},
        },
    }
    if not detector_ok:
        body["status"] = "unavailable"
        return jsonify(body), 503
    body["status"] = "ok"
    return jsonify(body)


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
        "audio_near_duplicate": {"score", "original_handle", "original_pial",
                                 "original_post_id"} | null,   # reported only
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
        match_row, dup_type, audio_near = _find_duplicate(sha256, audio_fp, phash)

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

        near_payload = None
        if audio_near:
            near_payload = _audio_near_match_report(audio_near, f"asset={asset_id}")
            _dupes_total.labels(kind="audio_near").inc()

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
            "audio_near_duplicate": near_payload,
        })

    except scanner.DetectorUnavailable as e:
        _unavailable.labels(kind="video").inc()
        logger.error("scan_video refused for %s — detector unavailable: %s", file_path, e)
        return jsonify({"error": "detector_unavailable", "detail": str(e)}), 503
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
    Returns: {duplicate_type, original_pial, original_handle, original_post_id,
              audio_near_duplicate}  — audio_near_duplicate is the review-band
    audio match, reported for a human and never a reason to refuse the upload.
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

        match_row, dup_type, audio_near = _find_duplicate(sha256, audio_fp, phash)

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

        # A near match against the uploader's own earlier work is their own
        # material and carries no signal, exactly as for the block tier above.
        near_payload = None
        if audio_near and audio_near["row"].get("uploader_handle") != uploader_handle:
            near_payload = _audio_near_match_report(audio_near, "pre-transcode")
            _dupes_total.labels(kind="audio_near").inc()

        _scan_duration.labels(kind="video-dedup").observe(time.time() - start)
        return jsonify({
            "duplicate_type":   dup_type,
            "original_pial":    original_pial,
            "original_handle":  original_handle,
            "original_post_id": original_post_id,
            "audio_near_duplicate": near_payload,
        })

    except scanner.DetectorUnavailable as e:
        _unavailable.labels(kind="video").inc()
        logger.error("scan_video_dedup refused for uploader @%s — detector unavailable: %s",
                     uploader_handle, e)
        return jsonify({"error": "detector_unavailable", "detail": str(e)}), 503
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
    except scanner.DetectorUnavailable as e:
        _unavailable.labels(kind="video").inc()
        logger.error("v1_scan_video refused for %s — detector unavailable: %s", file_path, e)
        return jsonify({"error": "detector_unavailable", "detail": str(e)}), 503
    except Exception as e:
        _errors_total.labels(kind="video").inc()
        logger.exception("v1_scan_video error for %s", file_path)
        return jsonify({"error": str(e)}), 500


# ── POST /v1/scan/image ───────────────────────────────────────────────────────

@app.route("/v1/scan/image", methods=["POST"])
def scan_image():
    """
    Single-image scan. This is the endpoint the live lane calls for every
    sampled keyframe, so its answer drives live_streams.scan_state.

    Request JSON:
      {"image_b64": "<base64>", "filename": "photo.jpg",
       "skip_nudity": bool (optional), "skip_gore": bool (optional)}

    Response JSON:
      {"is_nsfw", "nsfw_score", "labels",
       "gore_score", "risk_level", "recommendation", "signals"}

    503 when the detector cannot classify. The caller must treat that as
    "unscanned", never as "clean" — there is no 200 response from this endpoint
    that was not produced by a model actually looking at the pixels.
    """
    data = request.get_json(force=True, silent=True) or {}
    b64  = data.get("image_b64", "")
    name = data.get("filename", "image.jpg")
    skip_nudity = bool(data.get("skip_nudity", False))
    skip_gore   = bool(data.get("skip_gore", False))

    if not b64:
        return jsonify({"error": "image_b64 required"}), 400

    try:
        image_bytes = base64.b64decode(b64)
    except Exception:
        return jsonify({"error": "invalid base64"}), 400

    start = time.time()
    try:
        nsfw = scanner.scan_image_bytes(image_bytes, name)
    except scanner.DetectorUnavailable as e:
        _unavailable.labels(kind="image").inc()
        logger.error("scan_image refused — detector unavailable: %s", e)
        return jsonify({"error": "detector_unavailable", "detail": str(e)}), 503
    except Exception as e:
        _errors_total.labels(kind="image").inc()
        logger.exception("scan_image error")
        return jsonify({"error": str(e)}), 500

    # Gore is a pure colour statistic over the same pixels — no extra model, so
    # the live lane gets a violence signal at no additional load cost.
    gore = scanner.gore_score_image(image_bytes)

    risk = scanner.compute_risk(
        nudity_score = nsfw.get("nsfw_score", 0.0),
        gore_score   = gore,
        skip_nudity  = skip_nudity,
        skip_gore    = skip_gore,
    )

    result = {
        "is_nsfw":        nsfw.get("is_nsfw", False),
        "nsfw_score":     nsfw.get("nsfw_score", 0.0),
        "labels":         nsfw.get("labels", []),
        "gore_score":     gore,
        "risk_level":     risk["risk_level"],
        "recommendation": risk["recommendation"],
        "signals":        risk["signals"],
        "scan_version":   "2.1",
    }

    nsfw_label = "true" if result["is_nsfw"] else "false"
    _scans_total.labels(kind="image", is_nsfw=nsfw_label).inc()
    _risk_total.labels(level=risk["risk_level"]).inc()
    _scan_duration.labels(kind="image").observe(time.time() - start)
    return jsonify(result)


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

    # Perceptual hash through scanner, which is the only implementation of it
    # on this platform. Dedup and the banned-hash gate must agree on the value.
    phash = scanner.perceptual_image_hash(image_bytes) or ""

    # No audio fingerprint on a still image, so the audio near-match slot is
    # always empty here.
    match_row, dup_type, _audio_near = _find_duplicate(sha256, "", phash)

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
        "audio_near_duplicate": dict|null,
        "original_post_id": str,
        "banned_hash":      bool,
        "banned_category":  str,
        "banned_audio_near": dict|null   audio close to a banned registry entry
                                         but below the block cut — admitted,
                                         recorded, and routed to a human
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
        "audio_near_duplicate": None,
        "banned_hash":       False,
        "banned_category":   "",
        "banned_audio_near": None,
        "degraded":          [],
        "scan_version":      "2.1",
    }
    degraded = []

    try:
        # ── Text scan (post body) ─────────────────────────────────────────────
        if text:
            text_result = scanner.scan_text(text)
            result["clickbait_score"]   = text_result["clickbait_score"]
            result["hate_signals"]      = scanner.detect_hate_signals(text)
            result["violence_signals"]  = scanner.detect_violence_signals(text)
            result["self_harm_signals"] = scanner.detect_self_harm_signals(text)

        # ── Video file scan ───────────────────────────────────────────────────
        if file_path and not os.path.exists(file_path):
            # The caller asked for this file to be scanned and it is not there.
            # Falling through would return an approve carrying nudity_score 0.0
            # for media nothing ever opened, which is the exact shape of a
            # clean verdict. Say plainly that no scan happened.
            _errors_total.labels(kind="risk").inc()
            logger.error("scan_risk refused for post %s — media not found: %s", post_id, file_path)
            return jsonify({"error": "media_not_found", "detail": file_path}), 404

        if not file_path and not image_b64 and not text:
            return jsonify({"error": "one of file_path, image_b64 or text is required"}), 400

        if file_path:
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

            # Exact equality on the audio fingerprint is defeated by a container
            # change, so the same registry is scored by bit similarity behind
            # it. Same radii, same vocabulary as the upload gate in
            # hashes_check_media — one decision boundary for banned audio.
            if not banned and audio_fp:
                try:
                    audio_ban = _scan_banned_audio(audio_fp, f"post={post_id}")
                except scanner.AudioFingerprintError as e:
                    # The comparator could not read a fingerprint this process
                    # produced. The layer did not run; it is recorded as a
                    # detector that failed, which routes the post to a human
                    # rather than approving it on a gate that was not applied.
                    logger.error("[content-scan] post %s: banned-audio comparison failed "
                                 "— %s", post_id, e)
                    degraded.append("banned_audio_fp")
                    audio_ban = None
                if audio_ban is not None:
                    if audio_ban.registry_faults:
                        degraded.append("banned_audio_fp:registry")
                    if audio_ban.verdict == "block":
                        banned, ban_cat = True, audio_ban.match["category"]
                    elif audio_ban.verdict == "review":
                        result["banned_audio_near"] = _banned_audio_near(
                            audio_ban, f"post={post_id}")

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
            match_row, dup_type, audio_near = _find_duplicate(sha256, audio_fp, phash)
            if audio_near:
                result["audio_near_duplicate"] = _audio_near_match_report(
                    audio_near, f"post={post_id}")
                _dupes_total.labels(kind="audio_near").inc()
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
            if scanner.ocr_ready():
                result["ocr_text"] = scanner.extract_ocr_text(file_path)
            else:
                degraded.append("ocr")

            # Audio transcription — speech-to-text → text moderation pipeline.
            # This is the enterprise audio moderation approach: not raw waveform
            # classification, but transcript → same text stack as post body/OCR.
            if not scanner.transcriber_ready():
                degraded.append("transcribe")
                transcript = ""
            else:
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
            if not scanner.ocr_ready():
                degraded.append("ocr")
            else:
                import tempfile, os as _os
                tmp_path = None
                try:
                    with tempfile.NamedTemporaryFile(suffix=".jpg", delete=False) as tmp:
                        tmp.write(image_bytes)
                        tmp_path = tmp.name
                    result["ocr_text"] = scanner.extract_ocr_text(tmp_path)
                except Exception as e:
                    # OCR failed on content it was supposed to read. Recorded as
                    # a degraded scan so compute_risk sends it to a human rather
                    # than approving text nothing managed to read.
                    logger.warning("OCR failed for post %s: %s", post_id, e)
                    degraded.append("ocr")
                finally:
                    if tmp_path:
                        try:
                            _os.unlink(tmp_path)
                        except OSError:
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
            degraded    = degraded,
        )
        result["risk_level"]     = risk["risk_level"]
        result["recommendation"] = risk["recommendation"]
        result["signals"]        = risk["signals"]
        # The review-band audio match is a moderator-visible signal, not an
        # automatic verdict: it never moves the recommendation, exactly as the
        # banned-phash near-match band reports without blocking.
        if result["audio_near_duplicate"]:
            result["signals"].append(
                "audio_near_duplicate:%.4f" % result["audio_near_duplicate"]["score"])
        # A near match against the BANNED registry is not the same kind of
        # finding as a near-duplicate of another creator's work: it is content
        # close to material this platform refuses. It never auto-blocks — the
        # score is below the block cut precisely because the call is not certain
        # — but it always puts a human on it, the same ratchet the banned-phash
        # near band applies to a live frame. A verdict already more severe than
        # human_review is left alone; nothing here downgrades a block.
        if result["banned_audio_near"]:
            near = result["banned_audio_near"]
            result["signals"].append(
                "banned_audio_fp_near:%s:s%.4f" % (near["category"], near["score"]))
            if result["recommendation"] in ("approve", "age_gate"):
                result["recommendation"] = "human_review"
                result["risk_level"]     = "review"
        result["degraded"]       = degraded
        if degraded:
            logger.warning("[content-scan] post %s scanned with degraded detectors %s — "
                           "routed to human review rather than approved", post_id, degraded)

        _risk_total.labels(level=risk["risk_level"]).inc()
        _scan_duration.labels(kind="risk").observe(time.time() - start)

        # ── Callback to feed-engine ───────────────────────────────────────────
        if notify and post_id:
            threading.Thread(target=_notify_feed_engine, args=(post_id, dict(result)), daemon=True).start()

        return jsonify(result)

    except scanner.DetectorUnavailable as e:
        # Nothing classified this content. Answering 200 with nudity_score 0.0
        # would be indistinguishable from a clean verdict, so the caller is told
        # plainly that no scan happened and must hold the content unscanned.
        _unavailable.labels(kind="risk").inc()
        logger.error("scan_risk refused for post %s — detector unavailable: %s", post_id, e)
        return jsonify({"error": "detector_unavailable", "detail": str(e)}), 503
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

    Response: {"banned": bool, "category": str, "near_match": null|{...}}

    An audio_fp is checked exactly and then, if that misses, by the same fuzzy
    radius /v1/hashes/check-media applies. Answering this endpoint on string
    equality alone while the media endpoint matches perceptually would leave a
    caller holding a fingerprint with a weaker gate than a caller holding the
    file, and a weaker gate that reads as the same gate is the defect this
    radius exists to close.

    503 when the registry cannot be read or the fingerprint cannot be compared.
    A "not banned" answer on a failed lookup would be a verdict nothing produced.
    """
    data = request.get_json(force=True, silent=True) or {}
    hash_type  = data.get("hash_type", "")
    hash_value = data.get("hash_value", "")
    if not hash_type or not hash_value:
        return jsonify({"error": "hash_type and hash_value required"}), 400
    try:
        banned, category = _is_banned_hash(hash_type, hash_value)
        if banned or hash_type != "audio_fp":
            return jsonify({"banned": banned, "category": category, "near_match": None})

        scan = _scan_banned_audio(hash_value, "hash check")
        if scan.verdict == "block":
            return jsonify({"banned": True,
                            "category": scan.match["category"],
                            "near_match": {"hash_type": "audio_fp",
                                           "score": round(scan.match["score"], 4),
                                           "category": scan.match["category"]}})
        near = _banned_audio_near(scan, "hash check") if scan.verdict == "review" else None
        return jsonify({"banned": False, "category": "", "near_match": near})
    except psycopg2.Error as e:
        _errors_total.labels(kind="hash-check").inc()
        logger.exception("hashes_check: banned registry unreadable")
        return jsonify({"error": "registry_unavailable", "detail": str(e)}), 503
    except scanner.AudioFingerprintError as e:
        # The caller handed over something that is not a chromaprint
        # fingerprint, or one this build cannot decode. Either way no audio
        # comparison happened and the answer says so.
        _errors_total.labels(kind="hash-check").inc()
        logger.error("hashes_check: audio_fp could not be decoded for comparison: %s", e)
        return jsonify({"error": "audio_comparator_failed", "detail": str(e)}), 503


# ── POST /v1/hashes/check-media ───────────────────────────────────────────────

# A perceptual hash is matched by distance, never by string equality.
#
# Measured on this pipeline: re-encoding a PNG to JPEG q72, and resizing it to
# 80%, each move imagehash.phash by 2 bits out of 64. Requiring equality would
# therefore let a single re-encode through — the precise "gate that reads as
# enforced and catches nothing" this layer exists to close.
#
# Two radii, because the two consequences are not the same:
#
#   distance <= BLOCK  → treated as the same content and refused. Six bits of
#       64 is a random-collision probability around 4e-12 per comparison, so a
#       refusal here is a real match, not an accident.
#   BLOCK < distance <= REVIEW → reported as a near match and admitted, for a
#       human to judge. Low-detail media (a near-black frame, a flat colour
#       field) clusters in phash space, and refusing a creator on that would be
#       the false-positive failure of this gate.
#
# The clustering caveat is why the registry must never be fed the phash of a
# near-uniform frame: at these radii such an entry would refuse a large share
# of ordinary dark or blank media.
_BANNED_PHASH_BLOCK_DISTANCE  = 6
_BANNED_PHASH_REVIEW_DISTANCE = 12


def _closest_banned_phash(phash: str):
    """
    Nearest entry in the banned phash registry, or None when nothing is within
    the review radius. Returns {"distance", "category", "hash_value"}.

    banned_content_hashes is a small curated registry, not a content table, so
    this scans its phash rows rather than depending on an index the frozen
    schema does not carry.
    """
    if not phash:
        return None
    rows = _pg_fetch(
        "SELECT hash_value, category FROM banned_content_hashes WHERE hash_type = 'phash'"
    )
    best = None
    for row in rows:
        value = row.get("hash_value") or ""
        if not value:
            continue
        distance = _phash_distance(phash, value)
        if distance > _BANNED_PHASH_REVIEW_DISTANCE:
            continue
        if best is None or distance < best["distance"]:
            best = {"distance": distance, "category": row.get("category", ""),
                    "hash_value": value}
    return best


@app.route("/v1/hashes/check-media", methods=["POST"])
def hashes_check_media():
    """
    Layered banned-content check over raw media bytes. This is the endpoint
    feed-engine calls before it stores anything.

    feed-engine computes sha256 itself and checks it locally — that layer must
    work with this service down. What it cannot do is compute a perceptual hash:
    such a hash matches only when the same algorithm produced both sides, and
    the values in banned_content_hashes came from the imagehash/chromaprint
    pipeline in this process. So the perceptual layers live here and nowhere
    else, and feed-engine asks rather than computes.

    Request: multipart/form-data
      file — the media bytes
      kind — "image" | "video" | "audio" (decides which layers apply)

    Response 200:
      {
        "kind": "image|video|audio",
        "banned": bool,
        "hash_type": "sha256|phash|audio_fp|''",   which layer matched
        "category": "",                            registry category of the match
        "sha256": "...",
        "phash": "...",                            "" when not applicable/computed
        "audio_fp_present": bool,
        "checked":  ["sha256", "phash", ...],      layers that actually ran
        "degraded": ["phash:not_computed", ...],   layers that applied and failed
        "near_match":   null | one near-match block, the most specific one
        "near_matches": [ {"hash_type","category","distance"|"score"}, ... ]
        "audio_fuzzy_skipped": ""                  reason the audio fuzzy radius
                                                   did not run on this upload
      }

    A near match is admitted and recorded for a human, never blocked: for phash
    it is a Hamming distance in the outer radius, for audio_fp a similarity
    score in the band below the block cut.

    A non-empty "degraded" is not a clean verdict — part of the gate did not
    run, and the caller must record the media as unjudged rather than clean.

    503 when the registry could not be read. Answering "banned": false because
    a query failed would be a verdict nothing produced, and on this registry the
    failure direction is refusal, never admission.
    """
    kind = (request.form.get("kind") or "").strip().lower()
    if kind not in ("image", "video", "audio"):
        return jsonify({"error": "kind must be image, video or audio"}), 400

    file_obj = request.files.get("file")
    if file_obj is None:
        return jsonify({"error": "file field required"}), 400
    filename = file_obj.filename or "upload"

    result = {
        "kind":             kind,
        "banned":           False,
        "hash_type":        "",
        "category":         "",
        "sha256":           "",
        "phash":            "",
        "audio_fp_present": False,
        "checked":          [],
        "degraded":         [],
        "near_match":       None,
        "near_matches":     [],
        "audio_fuzzy_skipped": "",
    }

    start = time.time()
    tmp_path = None
    try:
        # The payload is spooled to disk rather than held in memory: a video is
        # gigabytes and this process also holds three models resident.
        suffix = os.path.splitext(filename)[1] or ""
        with tempfile.NamedTemporaryFile(suffix=suffix, delete=False) as tmp:
            tmp_path = tmp.name
        file_obj.save(tmp_path)
        if os.path.getsize(tmp_path) == 0:
            return jsonify({"error": "empty file"}), 400

        # ── Layer 1: exact bytes ──────────────────────────────────────────────
        result["sha256"] = scanner.sha256_file(tmp_path)
        result["checked"].append("sha256")
        banned, category = _is_banned_hash("sha256", result["sha256"])
        if banned:
            result["banned"]    = True
            result["hash_type"] = "sha256"
            result["category"]  = category
            logger.warning("[content-scan] banned sha256 %s (%s) on %s upload",
                           result["sha256"][:16], category, kind)
            return jsonify(result)

        # ── Layer 2: perceptual ───────────────────────────────────────────────
        audio_fp = ""
        if kind == "image":
            result["phash"] = scanner.perceptual_image_hash_file(tmp_path) or ""
            if result["phash"]:
                result["checked"].append("phash")
            else:
                result["degraded"].append("phash:not_computed")
        else:
            if kind == "video":
                result["phash"] = scanner.perceptual_video_hash(tmp_path) or ""
                if result["phash"]:
                    result["checked"].append("phash")
                else:
                    result["degraded"].append("phash:not_computed")
            # A file with no audio stream has nothing to fingerprint. That is
            # not a failed layer, and recording it as one would report every
            # silent upload as an unjudged one.
            if scanner.has_audio_stream(tmp_path):
                audio_fp = scanner.audio_fingerprint(tmp_path) or ""
                if audio_fp:
                    result["audio_fp_present"] = True
                    result["checked"].append("audio_fp")
                else:
                    result["degraded"].append("audio_fp:not_computed")

        closest = _closest_banned_phash(result["phash"])
        if closest and closest["distance"] <= _BANNED_PHASH_BLOCK_DISTANCE:
            result["banned"]    = True
            result["hash_type"] = "phash"
            result["category"]  = closest["category"]
            near = {"hash_type": "phash",
                    "distance": closest["distance"],
                    "category": closest["category"]}
            result["near_match"] = near
            result["near_matches"] = [near]
            logger.warning("[content-scan] banned phash %s within distance %d of %s (%s) on a "
                           "%s upload — the exact bytes were not in the registry",
                           result["phash"], closest["distance"], closest["hash_value"],
                           closest["category"], kind)
            return jsonify(result)

        # ── Layer 3: banned audio, exact then fuzzy ──────────────────────────
        # Exact string equality first: it is cheaper and it is certain. It is
        # also defeated by a container change — the same AAC stream rewrapped
        # m4a → mka fingerprints differently — so a bit-similarity radius over
        # the decoded frames runs behind it. Measured on this build, a
        # re-container scores 0.9961-0.9991 against the registered original and
        # every different work ever measured scored at most 0.7886.
        if audio_fp:
            banned, category = _is_banned_hash("audio_fp", audio_fp)
            if banned:
                result["banned"]    = True
                result["hash_type"] = "audio_fp"
                result["category"]  = category
                logger.warning("[content-scan] banned audio fingerprint (%s) on %s upload",
                               category, kind)
                return jsonify(result)

            audio_scan = _scan_banned_audio(audio_fp, f"{kind} upload")
            if not audio_scan.query_usable:
                # The upload's own audio carries too little information to be
                # fuzzy-matched — silence, a steady tone, broadband noise, or a
                # clip under ~7.9 s. Matching it would score high against every
                # equally featureless registry entry and refuse a large share of
                # ordinary quiet uploads, so it is not attempted. Said out loud
                # in the response rather than folded into "degraded": no re-scan
                # can change this answer, and every audio-bearing quiet upload
                # would otherwise be filed as unjudged forever.
                result["audio_fuzzy_skipped"] = audio_scan.query_reason
            if audio_scan.undecodable:
                result["degraded"].append(
                    "audio_fp:registry_rows_undecodable:%d" % len(audio_scan.undecodable))
            if audio_scan.unusable:
                result["degraded"].append(
                    "audio_fp:registry_rows_unusable:%d" % len(audio_scan.unusable))

            if audio_scan.verdict == "block":
                result["banned"]    = True
                result["hash_type"] = "audio_fp"
                result["category"]  = audio_scan.match["category"]
                near = {"hash_type": "audio_fp",
                        "score": round(audio_scan.match["score"], 4),
                        "category": audio_scan.match["category"]}
                result["near_match"] = near
                result["near_matches"].append(near)
                logger.warning("[content-scan] banned audio fingerprint at similarity "
                               "%.4f of registry %s… (%s) on a %s upload — the exact "
                               "fingerprint was not in the registry",
                               audio_scan.match["score"],
                               audio_scan.match["hash_value"][:24],
                               audio_scan.match["category"], kind)
                return jsonify(result)
            if audio_scan.verdict == "review":
                result["near_matches"].append(
                    _banned_audio_near(audio_scan, f"{kind} upload"))

        if closest:
            result["near_matches"].append({"hash_type": "phash",
                                           "distance": closest["distance"],
                                           "category": closest["category"]})
        # near_match carries the single most specific finding for callers that
        # read one; near_matches carries every one of them so a video that is
        # near-banned on both its frames and its audio loses neither. Audio
        # leads: a fuzzy fingerprint match survives re-encoding, where a phash
        # within the outer radius can be low-detail clustering.
        if result["near_matches"] and not result["near_match"]:
            result["near_match"] = result["near_matches"][0]
        for near in result["near_matches"]:
            if near["hash_type"] == "phash":
                logger.warning("[content-scan] %s upload within distance %d of banned %s "
                               "content — reported for review, not blocked",
                               kind, near["distance"], near["category"])

        if result["degraded"]:
            logger.warning("[content-scan] banned-hash layers %s could not be computed for a "
                           "%s upload — caller must hold it unjudged", result["degraded"], kind)
        return jsonify(result)

    except psycopg2.Error as e:
        # The registry itself could not be read. A "not banned" answer here
        # would be a verdict nothing produced.
        _errors_total.labels(kind="hash-check").inc()
        logger.exception("hashes_check_media: banned registry unreadable")
        return jsonify({"error": "registry_unavailable", "detail": str(e)}), 503
    except scanner.AudioFingerprintError as e:
        # The fingerprint this process produced seconds ago will not decode, so
        # the audio layer of the gate cannot run at all. That is a broken
        # comparator, not an absent match, and it is answered as one.
        _errors_total.labels(kind="hash-check").inc()
        logger.exception("hashes_check_media: the audio comparator could not read a "
                         "fingerprint this process just produced")
        return jsonify({"error": "audio_comparator_failed", "detail": str(e)}), 503
    except Exception as e:
        _errors_total.labels(kind="hash-check").inc()
        logger.exception("hashes_check_media error for %s upload", kind)
        return jsonify({"error": str(e)}), 500
    finally:
        _scan_duration.labels(kind="hash-check").observe(time.time() - start)
        if tmp_path:
            try:
                os.unlink(tmp_path)
            except OSError:
                pass


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


# ── warm-up ───────────────────────────────────────────────────────────────────

def warm_up():
    """
    Load every model into memory before the first request.

    The models are baked into the image at build time, so this is a load from
    local disk, not a download — a cold start is bounded and predictable rather
    than a multi-minute stall on the first frame that arrives.

    A model that will not load does NOT stop the process. A running service that
    answers /ready and every scan endpoint with an explicit 503 tells an operator
    exactly what is broken; a crash-looping container tells them nothing, and a
    caller cannot distinguish a missing container from a missing model. The error
    is logged at ERROR and repeated on every request — it is surfaced, not
    swallowed.
    """
    logger.info("content-scan warming up (device=%s)…", scanner.device())
    started = time.time()

    ok, err = scanner.detector_ready()
    if ok:
        logger.info("NudeNet detector ready")
    else:
        logger.error("NudeNet detector NOT ready: %s — every scan endpoint will "
                     "answer 503 until it loads. No content will be reported clean.", err)

    if scanner.ocr_ready():
        logger.info("EasyOCR reader ready")
    else:
        logger.error("EasyOCR reader NOT ready — scans that need OCR are reported "
                     "degraded and routed to human review, never approved.")

    if scanner.transcriber_ready():
        logger.info("faster-whisper transcriber ready")
    else:
        logger.error("faster-whisper NOT ready — scans that need a transcript are "
                     "reported degraded and routed to human review, never approved.")

    scanner._get_lgbm_model()
    logger.info("content-scan warm-up finished in %.1fs", time.time() - started)


# Runs on import so a gunicorn worker is warm before it accepts a request.
warm_up()


# ── entrypoint (direct execution — production runs gunicorn, see Dockerfile) ──

if __name__ == "__main__":
    port = int(os.environ.get("PORT", 8097))
    logger.info("content-scan listening on :%d", port)
    app.run(host="0.0.0.0", port=port, threaded=True)
