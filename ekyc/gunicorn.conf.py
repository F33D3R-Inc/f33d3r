"""
gunicorn.conf.py — production server settings for ekyc.

One worker process, by design: liveness challenge sessions live in that
process's memory and must be seen by the request that redeems them. Concurrency
comes from threads; server.py caps simultaneous verifications with
EKYC_MAX_CONCURRENT, and the extra threads keep /health and challenge issuing
responsive while verifications run.
"""
import os

bind = f"0.0.0.0:{os.environ.get('PORT', '8099')}"
workers = 1
worker_class = "gthread"
threads = int(os.environ.get("EKYC_THREADS", "8"))
# A full verification (document OCR, barcode, faces, PAD on every frame) takes
# tens of seconds on the current CPU; feed-engine waits at most 45 s.
timeout = 90
graceful_timeout = 30
keepalive = 5
# Request lines and headers only; body size is enforced by Flask's
# MAX_CONTENT_LENGTH in server.py.
limit_request_line = 8190
accesslog = None
errorlog = "-"
loglevel = "info"
