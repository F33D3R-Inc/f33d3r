"""
F33D3R Transcoding Brain — ffmpeg video processing service.

Endpoints:
  GET  /health                    — health check
  POST /v1/transcode              — queue a transcode job
  GET  /v1/jobs/<job_id>          — job status + output URLs
  POST /v1/thumbnail              — extract thumbnail from video

Transcode pipeline:
  Input: any format (mp4, mov, avi, mkv, webm, flv, etc.)
  Output:
    - H.264/AAC mp4 (baseline, web-optimised, fast-start)
    - HLS segments + playlist (adaptive streaming) [future]
    - Thumbnail JPEG at 0:00:01

Environment:
  PORT           — listen port (default 8098)
  MEDIA_DIR      — input media directory (shared volume with Caeor)
  OUTPUT_DIR     — output directory for transcoded files
  MAX_CONCURRENT — max simultaneous ffmpeg jobs (default 2)
"""

import os
import uuid
import subprocess
import threading
import time
import json
from pathlib import Path
from flask import Flask, request, jsonify

app = Flask(__name__)

PORT       = int(os.environ.get("PORT", "8098"))
MEDIA_DIR  = Path(os.environ.get("MEDIA_DIR",  "/data/media"))
OUTPUT_DIR = Path(os.environ.get("OUTPUT_DIR", "/data/transcoded"))
MAX_JOBS   = int(os.environ.get("MAX_CONCURRENT", "2"))

OUTPUT_DIR.mkdir(parents=True, exist_ok=True)

# In-memory job registry (replace with Redis for multi-instance)
jobs: dict[str, dict] = {}
semaphore = threading.Semaphore(MAX_JOBS)


def ffmpeg_transcode(job_id: str, input_path: Path, output_path: Path, thumb_path: Path):
    """Run ffmpeg synchronously in a background thread."""
    job = jobs[job_id]
    job["status"] = "processing"
    job["started_at"] = time.time()

    with semaphore:
        try:
            # ── H.264/AAC mp4 (web-optimised, fast-start moov atom) ──────────
            cmd_video = [
                "ffmpeg", "-y", "-i", str(input_path),
                "-c:v", "libx264",
                "-preset", "fast",           # balance speed vs compression
                "-crf", "23",               # quality (18=high, 28=low)
                "-maxrate", "8M",
                "-bufsize", "16M",
                "-c:a", "aac",
                "-b:a", "128k",
                "-movflags", "+faststart",  # moov atom at start for streaming
                "-vf", "scale=trunc(iw/2)*2:trunc(ih/2)*2",  # ensure even dims
                str(output_path),
            ]
            result = subprocess.run(cmd_video, capture_output=True, timeout=900)
            if result.returncode != 0:
                raise RuntimeError(result.stderr.decode())

            # ── Thumbnail (1 second in, 1280px wide) ─────────────────────────
            cmd_thumb = [
                "ffmpeg", "-y", "-i", str(input_path),
                "-ss", "00:00:01",
                "-vframes", "1",
                "-vf", "scale=1280:-1",
                str(thumb_path),
            ]
            subprocess.run(cmd_thumb, capture_output=True, timeout=60)

            # ── Get video metadata ────────────────────────────────────────────
            probe = subprocess.run([
                "ffprobe", "-v", "quiet", "-print_format", "json",
                "-show_streams", "-show_format", str(output_path),
            ], capture_output=True, timeout=30)
            meta = json.loads(probe.stdout) if probe.returncode == 0 else {}
            duration = float(meta.get("format", {}).get("duration", 0))
            video_stream = next(
                (s for s in meta.get("streams", []) if s.get("codec_type") == "video"),
                {}
            )

            job["status"]    = "complete"
            job["output_url"]  = f"/static/transcoded/{output_path.name}"
            job["thumb_url"]   = f"/static/transcoded/{thumb_path.name}"
            job["duration_s"]  = round(duration)
            job["width"]       = video_stream.get("width", 0)
            job["height"]      = video_stream.get("height", 0)
            job["codec"]       = video_stream.get("codec_name", "")
            job["completed_at"] = time.time()

        except Exception as e:
            job["status"] = "failed"
            job["error"]  = str(e)[:500]
            job["completed_at"] = time.time()


@app.get("/health")
def health():
    active = sum(1 for j in jobs.values() if j.get("status") == "processing")
    return jsonify({"status": "ok", "active_jobs": active, "max_concurrent": MAX_JOBS})


@app.post("/v1/transcode")
def transcode():
    data = request.get_json(force=True, silent=True) or {}
    input_rel = data.get("input_path", "")      # relative to MEDIA_DIR
    file_id   = data.get("file_id", "")          # Caeor file ID

    if not input_rel and not file_id:
        return jsonify({"error": "input_path or file_id required"}), 400

    # Resolve input
    if input_rel:
        input_path = MEDIA_DIR / input_rel
    else:
        # Caeor stores files as /data/media/<file_id>
        input_path = MEDIA_DIR / file_id
    if not input_path.exists():
        return jsonify({"error": f"input not found: {input_path}"}), 404

    job_id   = str(uuid.uuid4())
    out_name = f"{job_id}.mp4"
    thumb_name = f"{job_id}_thumb.jpg"
    output_path = OUTPUT_DIR / out_name
    thumb_path  = OUTPUT_DIR / thumb_name

    jobs[job_id] = {
        "job_id":    job_id,
        "status":    "queued",
        "input":     str(input_path),
        "queued_at": time.time(),
    }

    t = threading.Thread(
        target=ffmpeg_transcode,
        args=(job_id, input_path, output_path, thumb_path),
        daemon=True,
    )
    t.start()
    return jsonify({"job_id": job_id, "status": "queued"}), 202


@app.get("/v1/jobs/<job_id>")
def job_status(job_id: str):
    job = jobs.get(job_id)
    if not job:
        return jsonify({"error": "job not found"}), 404
    return jsonify(job)


@app.post("/v1/thumbnail")
def thumbnail():
    """Extract a thumbnail from an already-uploaded video."""
    data = request.get_json(force=True, silent=True) or {}
    input_rel = data.get("input_path", "")
    if not input_rel:
        return jsonify({"error": "input_path required"}), 400

    input_path = MEDIA_DIR / input_rel
    if not input_path.exists():
        return jsonify({"error": "input not found"}), 404

    thumb_name = f"{uuid.uuid4()}_thumb.jpg"
    thumb_path = OUTPUT_DIR / thumb_name

    cmd = [
        "ffmpeg", "-y", "-i", str(input_path),
        "-ss", "00:00:01", "-vframes", "1",
        "-vf", "scale=1280:-1",
        str(thumb_path),
    ]
    result = subprocess.run(cmd, capture_output=True, timeout=60)
    if result.returncode != 0:
        return jsonify({"error": result.stderr.decode()[:300]}), 500

    return jsonify({"thumb_url": f"/static/transcoded/{thumb_name}"})


if __name__ == "__main__":
    app.run(host="0.0.0.0", port=PORT, threaded=True)
