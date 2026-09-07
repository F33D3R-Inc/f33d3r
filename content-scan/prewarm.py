"""
Bake every model into the image at build time.

Run from the Dockerfile. It fails loudly if any model cannot be loaded, so an
image that cannot scan is never produced. The alternative — letting the build
skip a missing model and fetching it on the first request — turns a build error
into a multi-minute stall on the first piece of live content, at exactly the
moment nobody is watching the log.

Nothing here is a scan. It only proves each model loads from local disk.
"""

import logging
import sys

logging.basicConfig(level=logging.INFO, format="%(levelname)s %(name)s: %(message)s")
log = logging.getLogger("prewarm")

import scanner  # noqa: E402  — configured logging first so model loads are visible

failures = []

# NudeNet — the NSFW detector. Without it there is no visual classification at
# all, so its absence is fatal to the build.
try:
    scanner._get_detector()
    log.info("NudeNet detector: ready")
except Exception as e:
    failures.append(f"NudeNet detector: {type(e).__name__}: {e}")

# EasyOCR — reads text burned into images and video frames (~300 MB of weights).
if scanner.ocr_ready():
    log.info("EasyOCR reader: ready")
else:
    failures.append("EasyOCR reader: failed to load")

# faster-whisper — speech to text, feeding the same text moderation stack.
if scanner.transcriber_ready():
    log.info("faster-whisper transcriber: ready")
else:
    failures.append("faster-whisper transcriber: failed to load")

# LightGBM clickbait model — trained in-process from a fixed synthetic set.
if scanner._get_lgbm_model():
    log.info("LightGBM clickbait model: ready")
else:
    failures.append("LightGBM clickbait model: failed to train")

if failures:
    for f in failures:
        log.error("pre-warm failure — %s", f)
    sys.exit(1)

log.info("all models baked into the image")
