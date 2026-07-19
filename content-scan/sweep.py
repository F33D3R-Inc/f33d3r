"""
Media sweep — backfill content fingerprints and scan_state for all existing posts.

Two modes:

1. Fingerprint / lineage sweep (default)
   Iterates all video posts, fingerprints each, detects cross-user duplicates,
   writes lineage_handle/lineage_pial back to PostgreSQL.

   python sweep.py                    # process unscanned only
   python sweep.py --all              # reprocess everything
   python sweep.py --limit 100        # process first N
   python sweep.py --dry-run          # report without writing

2. Risk backfill (--scan-risk)
   Calls /v1/scan/risk for each post that is still in 'pending_scan' state
   (or all posts with --all). Content-scan returns a recommendation which
   triggers a scan_complete callback to feed-engine, transitioning scan_state.

   python sweep.py --scan-risk                    # pending_scan posts only
   python sweep.py --scan-risk --all              # every post (re-scan)
   python sweep.py --scan-risk --limit 50         # first 50 pending
   python sweep.py --scan-risk --dry-run          # report only
"""

from __future__ import annotations

import argparse
import glob
import logging
import os
import sqlite3
import sys
import time
from typing import Optional

import psycopg2
import psycopg2.extras
import requests

import scanner

CONTENT_SCAN_URL = os.environ.get("CONTENT_SCAN_URL", "http://localhost:8097")

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s %(levelname)s sweep: %(message)s",
)
logger = logging.getLogger("sweep")

# ── configuration ─────────────────────────────────────────────────────────────

MEDIA_ROOT   = os.environ.get("MEDIA_ROOT", "/data/media")
DB_HOST      = os.environ.get("DB_HOST", "postgres")
DB_PORT      = int(os.environ.get("DB_PORT", "5432"))
DB_NAME      = os.environ.get("DB_NAME", "f33d3r_feed")
DB_USER      = os.environ.get("DB_USER", "f33d3r")
DB_PASSWORD  = os.environ.get("DB_PASSWORD", "")
SQLITE_PATH  = os.environ.get("CONTENT_SCAN_DB", "/data/content_scan.db")

# Similarity thresholds (mirror server.py constants)
_AUDIO_JACCARD_THRESHOLD = 0.85
_PHASH_HAMMING_THRESHOLD = 12

# ── SQLite helpers ─────────────────────────────────────────────────────────────

_SCHEMA = """
CREATE TABLE IF NOT EXISTS media_fingerprints (
  asset_id          TEXT PRIMARY KEY,
  post_id           TEXT NOT NULL DEFAULT '',
  uploader_pial     TEXT NOT NULL DEFAULT '',
  uploader_handle   TEXT NOT NULL DEFAULT '',
  sha256            TEXT NOT NULL DEFAULT '',
  audio_fingerprint TEXT NOT NULL DEFAULT '',
  perceptual_hash   TEXT NOT NULL DEFAULT '',
  scanned_at        INTEGER NOT NULL DEFAULT 0,
  is_nsfw           INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_fp_sha256 ON media_fingerprints(sha256) WHERE sha256 != '';
CREATE INDEX IF NOT EXISTS idx_fp_phash  ON media_fingerprints(perceptual_hash) WHERE perceptual_hash != '';
"""


def _open_sqlite() -> sqlite3.Connection:
    os.makedirs(os.path.dirname(SQLITE_PATH) if os.path.dirname(SQLITE_PATH) else ".", exist_ok=True)
    conn = sqlite3.connect(SQLITE_PATH, check_same_thread=False, timeout=30)
    conn.row_factory = sqlite3.Row
    conn.executescript(_SCHEMA)
    conn.commit()
    return conn


def _sqlite_fetch(conn: sqlite3.Connection, sql: str, params=()) -> list:
    cur = conn.execute(sql, params)
    return cur.fetchall()


def _sqlite_exec(conn: sqlite3.Connection, sql: str, params=()):
    conn.execute(sql, params)
    conn.commit()


# ── similarity helpers (mirrors server.py) ────────────────────────────────────

def _audio_jaccard(fp_a: str, fp_b: str) -> float:
    try:
        set_a = set(int(x) for x in fp_a.split(",") if x.strip())
        set_b = set(int(x) for x in fp_b.split(",") if x.strip())
        if not set_a or not set_b:
            return 0.0
        return len(set_a & set_b) / len(set_a | set_b)
    except Exception:
        return 0.0


def _phash_distance(h_a: str, h_b: str) -> int:
    try:
        if len(h_a) != len(h_b):
            return 999
        a = int(h_a, 16)
        b = int(h_b, 16)
        return bin(a ^ b).count("1")
    except Exception:
        return 999


def _find_duplicate(sqlite_conn: sqlite3.Connection, sha256: str, audio_fp: str, phash: str) -> tuple[Optional[dict], Optional[str]]:
    """
    Search the fingerprint store for a duplicate.
    Returns (matched_row_dict, duplicate_type) or (None, None).
    Priority: exact (SHA-256) > audio > visual.
    """
    # 1. Exact SHA-256 match
    if sha256:
        rows = _sqlite_fetch(
            sqlite_conn,
            "SELECT * FROM media_fingerprints WHERE sha256 = ? AND sha256 != '' LIMIT 1",
            (sha256,),
        )
        if rows:
            return dict(rows[0]), "exact"

    # 2. Audio fingerprint (Jaccard >= 0.85)
    if audio_fp:
        candidates = _sqlite_fetch(
            sqlite_conn,
            "SELECT * FROM media_fingerprints WHERE audio_fingerprint != '' LIMIT 2000",
        )
        best_score = 0.0
        best_row = None
        for row in candidates:
            score = _audio_jaccard(audio_fp, row["audio_fingerprint"])
            if score > best_score:
                best_score = score
                best_row = row
        if best_score >= _AUDIO_JACCARD_THRESHOLD and best_row:
            return dict(best_row), "audio"

    # 3. Perceptual hash (Hamming <= 12)
    if phash:
        candidates = _sqlite_fetch(
            sqlite_conn,
            "SELECT * FROM media_fingerprints WHERE perceptual_hash != '' LIMIT 5000",
        )
        for row in candidates:
            if _phash_distance(phash, row["perceptual_hash"]) <= _PHASH_HAMMING_THRESHOLD:
                return dict(row), "visual"

    return None, None


# ── file resolution ────────────────────────────────────────────────────────────

def _resolve_video_file(asset_id: str, master_url: str) -> Optional[str]:
    """
    Resolve the video path on disk.

    Priority:
    1. master_url → /static/media/... → /data/media/... master.m3u8 (HLS, preferred).
       ffmpeg and fpcalc can read HLS directly; sha256_video_stream decodes the audio.
    2. Fallback: look for original.mp4 / source.mp4 / any *.mp4 in the asset dir.
    """
    # 1. HLS master.m3u8 — convert the /static/media/... URL to a filesystem path
    if master_url:
        # Strip known URL prefixes to get the relative media path
        relative = master_url
        for prefix in ("/static/media/", "/media/", "media/"):
            if relative.startswith(prefix):
                relative = relative[len(prefix):]
                break
        # relative is now e.g. "posts/{asset_id}/master.m3u8"
        candidate = os.path.join(MEDIA_ROOT, relative)
        if os.path.exists(candidate):
            return candidate

        # Try the directory of the URL for any .mp4 fallback
        url_dir = os.path.dirname(relative)
        candidate_dir = os.path.join(MEDIA_ROOT, url_dir)
        for name in ("original.mp4", "source.mp4"):
            c = os.path.join(candidate_dir, name)
            if os.path.exists(c):
                return c
        mp4s = glob.glob(os.path.join(candidate_dir, "*.mp4"))
        if mp4s:
            return mp4s[0]

    # 2. Fallback: check {MEDIA_ROOT}/{asset_id}/
    if asset_id:
        asset_dir = os.path.join(MEDIA_ROOT, asset_id)
        for name in ("master.m3u8", "original.mp4", "source.mp4"):
            c = os.path.join(asset_dir, name)
            if os.path.exists(c):
                return c
        mp4s = glob.glob(os.path.join(asset_dir, "*.mp4"))
        if mp4s:
            return mp4s[0]

    return None


# ── PostgreSQL helpers ────────────────────────────────────────────────────────

def _open_pg() -> psycopg2.extensions.connection:
    return psycopg2.connect(
        host=DB_HOST,
        port=DB_PORT,
        dbname=DB_NAME,
        user=DB_USER,
        password=DB_PASSWORD,
        cursor_factory=psycopg2.extras.RealDictCursor,
    )


def _fetch_posts(pg_conn, limit: int, skip_existing: bool) -> list:
    """
    Fetch all posts that have video content, ordered oldest-first.

    Each row returns:
      post_id, author_handle, author_pial, master_url, canonical_media_id,
      created_at, lineage_handle (to detect already-processed rows).
    """
    lineage_filter = "AND p.lineage_handle = ''" if skip_existing else ""
    limit_clause   = f"LIMIT {limit}" if limit > 0 else ""

    sql = f"""
        SELECT
            p.id                                    AS post_id,
            u.handle                                AS author_handle,
            u.pial_id::text                         AS author_pial,
            COALESCE(p.video_master_url, cm.master_url, '')  AS master_url,
            COALESCE(p.canonical_media_id::text, '')         AS canonical_media_id,
            p.created_at,
            p.lineage_handle
        FROM posts p
        JOIN users u ON u.id = p.author_id
        LEFT JOIN canonical_media cm ON cm.id = p.canonical_media_id
        WHERE (
            p.video_master_url IS NOT NULL
            AND p.video_master_url != ''
        )
        {lineage_filter}
        AND p.deleted_at IS NULL
        ORDER BY p.created_at ASC
        {limit_clause}
    """
    # deleted_at may not exist in older schemas — handle gracefully
    try:
        with pg_conn.cursor() as cur:
            cur.execute(sql)
            return cur.fetchall()
    except psycopg2.errors.UndefinedColumn:
        pg_conn.rollback()
        sql_no_deleted = sql.replace("AND p.deleted_at IS NULL", "")
        with pg_conn.cursor() as cur:
            cur.execute(sql_no_deleted)
            return cur.fetchall()


def _update_post_lineage(pg_conn, post_id: str, lineage_pial: str, lineage_handle: str, dry_run: bool):
    if dry_run:
        logger.info("[dry-run] would UPDATE posts SET lineage_pial=%s, lineage_handle=%s WHERE id=%s",
                    lineage_pial, lineage_handle, post_id)
        return
    with pg_conn.cursor() as cur:
        cur.execute(
            "UPDATE posts SET lineage_pial = %s, lineage_handle = %s WHERE id = %s",
            (lineage_pial, lineage_handle, post_id),
        )
    pg_conn.commit()


# ── Risk backfill ─────────────────────────────────────────────────────────────

def _fetch_risk_posts(pg_conn, limit: int, reprocess_all: bool) -> list:
    """Fetch posts that need a scan_state decision."""
    state_filter = "" if reprocess_all else "AND COALESCE(p.scan_state, 'clean') = 'pending_scan'"
    limit_clause = f"LIMIT {limit}" if limit > 0 else ""
    sql = f"""
        SELECT
            p.id                                           AS post_id,
            u.handle                                       AS author_handle,
            u.pial_id::text                                AS author_pial,
            COALESCE(p.video_master_url, cm.master_url, '') AS master_url,
            COALESCE(p.canonical_media_id::text, '')        AS canonical_media_id,
            COALESCE(p.body, '')                            AS body,
            COALESCE(p.scan_state, 'pending_scan')          AS scan_state
        FROM posts p
        JOIN users u ON u.id = p.author_id
        LEFT JOIN canonical_media cm ON cm.id = p.canonical_media_id
        WHERE TRUE
        {state_filter}
        ORDER BY p.created_at ASC
        {limit_clause}
    """
    try:
        with pg_conn.cursor() as cur:
            cur.execute(sql)
            return cur.fetchall()
    except psycopg2.errors.UndefinedColumn:
        pg_conn.rollback()
        # scan_state column may not exist in very old schemas
        sql_fallback = sql.replace(state_filter, "")
        with pg_conn.cursor() as cur:
            cur.execute(sql_fallback)
            return cur.fetchall()


def run_risk_backfill(limit: int = 0, reprocess_all: bool = False, dry_run: bool = False):
    """
    Call /v1/scan/risk for each post needing a scan_state decision.
    Content-scan fires a notify_callback → feed-engine transitions scan_state.
    """
    logger.info(
        "Starting risk backfill: limit=%s reprocess_all=%s dry_run=%s scan_url=%s",
        limit or "all", reprocess_all, dry_run, CONTENT_SCAN_URL,
    )

    try:
        pg_conn = _open_pg()
    except Exception as e:
        logger.error("Cannot connect to PostgreSQL: %s", e)
        sys.exit(1)

    sqlite_conn = _open_sqlite()
    posts = _fetch_risk_posts(pg_conn, limit, reprocess_all)
    total = len(posts)
    logger.info("Found %d posts to risk-scan", total)

    done = 0
    errors = 0

    for idx, row in enumerate(posts, start=1):
        post_id       = str(row["post_id"])
        author_handle = row["author_handle"] or ""
        author_pial   = row["author_pial"] or ""
        master_url    = row["master_url"] or ""
        asset_id      = row["canonical_media_id"] or post_id
        body          = row["body"] or ""
        current_state = row["scan_state"]

        prefix = f"[{idx}/{total}] @{author_handle} {post_id[:8]} ({current_state})"

        file_path = _resolve_video_file(asset_id, master_url) or ""

        if dry_run:
            logger.info("%s: [dry-run] would call /v1/scan/risk file_path=%s body_len=%d",
                        prefix, file_path or "(none)", len(body))
            continue

        payload = {
            "post_id":          post_id,
            "asset_id":         asset_id,
            "text":             body,
            "uploader_pial":    author_pial,
            "uploader_handle":  author_handle,
            "notify_callback":  True,
        }
        if file_path:
            payload["file_path"] = file_path

        try:
            resp = requests.post(
                f"{CONTENT_SCAN_URL}/v1/scan/risk",
                json=payload,
                timeout=120,
            )
            if resp.ok:
                r = resp.json()
                logger.info("%s: → %s (%s)", prefix, r.get("recommendation"), r.get("risk_level"))
                done += 1
            else:
                logger.warning("%s: /v1/scan/risk returned %d", prefix, resp.status_code)
                errors += 1
        except Exception as e:
            logger.error("%s: request error: %s", prefix, e)
            errors += 1

        time.sleep(0.1)  # avoid hammering the scanner

    pg_conn.close()
    sqlite_conn.close()
    logger.info("Risk backfill complete. total=%d scanned=%d errors=%d", total, done, errors)
    return {"total": total, "done": done, "errors": errors}


# ── main sweep ────────────────────────────────────────────────────────────────

def run_sweep(limit: int = 0, reprocess_all: bool = False, dry_run: bool = False):
    logger.info(
        "Starting sweep: limit=%s reprocess_all=%s dry_run=%s media_root=%s db=%s/%s",
        limit or "all", reprocess_all, dry_run, MEDIA_ROOT, DB_HOST, DB_NAME,
    )

    try:
        pg_conn = _open_pg()
    except Exception as e:
        logger.error("Cannot connect to PostgreSQL: %s", e)
        sys.exit(1)

    sqlite_conn = _open_sqlite()

    posts = _fetch_posts(pg_conn, limit, skip_existing=not reprocess_all)
    total = len(posts)
    logger.info("Found %d posts to process", total)

    matched = 0
    skipped = 0
    errors  = 0

    for idx, row in enumerate(posts, start=1):
        post_id        = str(row["post_id"])
        author_handle  = row["author_handle"] or ""
        author_pial    = row["author_pial"] or ""
        master_url     = row["master_url"] or ""
        asset_id       = row["canonical_media_id"] or post_id  # fallback to post_id as asset key

        prefix = f"[{idx}/{total}] @{author_handle} - {asset_id}"

        # Check if already fingerprinted in SQLite (skip unless --all)
        if not reprocess_all:
            existing = _sqlite_fetch(
                sqlite_conn,
                "SELECT asset_id FROM media_fingerprints WHERE asset_id = ? LIMIT 1",
                (asset_id,),
            )
            if existing:
                logger.debug("%s: already in SQLite — skipping", prefix)
                skipped += 1
                continue

        # Resolve file path
        file_path = _resolve_video_file(asset_id, master_url)
        if not file_path:
            logger.warning("%s: no video file found (master_url=%s) — skipping", prefix, master_url or "none")
            skipped += 1
            continue

        # Run fingerprint pipeline
        try:
            scan = scanner.scan_video(file_path, asset_id=asset_id)
        except Exception as e:
            logger.error("%s: scan_video error: %s", prefix, e)
            errors += 1
            continue

        if scan.get("error"):
            logger.warning("%s: scan_video reported error: %s", prefix, scan["error"])
            skipped += 1
            continue

        sha256   = scan.get("sha256") or ""
        audio_fp = scan.get("audio_fingerprint") or ""
        phash    = scan.get("perceptual_hash") or ""
        is_nsfw  = scan.get("is_nsfw", False)

        # Check for duplicates in SQLite
        match_row, dup_type = _find_duplicate(sqlite_conn, sha256, audio_fp, phash)

        if match_row and dup_type:
            matched_scanned_at = match_row.get("scanned_at", 0)
            # created_at epoch for this post
            post_epoch = int(row["created_at"].timestamp()) if row["created_at"] else int(time.time())

            # Only mark lineage if the matched record was scanned (inserted) earlier —
            # meaning the original uploader's fingerprint existed first.
            # scanned_at for the original record should be <= this post's created_at epoch.
            if matched_scanned_at <= post_epoch or dup_type == "exact":
                original_pial   = match_row.get("uploader_pial", "")
                original_handle = match_row.get("uploader_handle", "")

                logger.info("%s: matched %s -> @%s", prefix, dup_type, original_handle)
                matched += 1

                # Only write lineage when the original uploader differs from this
            # post's author — same as the real-time media.go check.
            if original_handle and original_handle != author_handle:
                _update_post_lineage(pg_conn, post_id, original_pial, original_handle, dry_run)
            else:
                logger.info(
                    "%s: matched %s but matched record is newer (scanned_at=%s > post epoch=%s) — this may be original",
                    prefix, dup_type, matched_scanned_at, post_epoch,
                )
        else:
            logger.info("%s: no match", prefix)

        # Store this post's fingerprints in SQLite (upsert — idempotent)
        if not dry_run and asset_id:
            _sqlite_exec(
                sqlite_conn,
                """INSERT OR REPLACE INTO media_fingerprints
                   (asset_id, post_id, uploader_pial, uploader_handle,
                    sha256, audio_fingerprint, perceptual_hash, scanned_at, is_nsfw)
                   VALUES (?,?,?,?,?,?,?,?,?)""",
                (asset_id, post_id, author_pial, author_handle,
                 sha256, audio_fp, phash, int(time.time()), int(is_nsfw)),
            )

    pg_conn.close()
    sqlite_conn.close()

    logger.info(
        "Sweep complete. total=%d matched=%d skipped=%d errors=%d",
        total, matched, skipped, errors,
    )
    return {"total": total, "matched": matched, "skipped": skipped, "errors": errors}


# ── CLI entry point ────────────────────────────────────────────────────────────

def _parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description="Backfill video fingerprints, lineage, and scan_state into PostgreSQL."
    )
    p.add_argument("--scan-risk", dest="scan_risk", action="store_true",
                   help="Risk backfill mode: call /v1/scan/risk for pending_scan posts")
    p.add_argument("--all",     dest="reprocess_all", action="store_true",
                   help="Reprocess all posts (--scan-risk: re-scan all; default: unscanned only)")
    p.add_argument("--limit",   type=int, default=0,
                   help="Process at most N posts (0 = all)")
    p.add_argument("--dry-run", dest="dry_run", action="store_true",
                   help="Report what would happen without writing to PostgreSQL")
    return p.parse_args()


if __name__ == "__main__":
    args = _parse_args()
    if args.scan_risk:
        result = run_risk_backfill(
            limit=args.limit,
            reprocess_all=args.reprocess_all,
            dry_run=args.dry_run,
        )
        sys.exit(0 if result["errors"] == 0 else 1)
    else:
        result = run_sweep(
            limit=args.limit,
            reprocess_all=args.reprocess_all,
            dry_run=args.dry_run,
        )
        sys.exit(0 if result["errors"] == 0 else 1)
