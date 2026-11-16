"""
Bake every model into the image at build time.

Run from the Dockerfile. It fails loudly if any model cannot be loaded, so an
image that cannot analyse is never produced. The alternative — letting the
build skip a missing model and fetching it on the first request — turns a build
error into a multi-minute stall on the first request, at exactly the moment
nobody is watching the log.

Nothing here is an analysis. It only proves each model loads from local disk.
"""

import logging
import sys

logging.basicConfig(level=logging.INFO, format="%(levelname)s %(name)s: %(message)s")
log = logging.getLogger("prewarm")

import scanner  # noqa: E402  — configured logging first so model loads are visible

failures = []
for slot in scanner.MODELS:
    ok, err = slot.ready()
    if ok:
        log.info("%s: ready", slot.name)
    else:
        failures.append(f"{slot.name}: {err}")

if failures:
    for f in failures:
        log.error("pre-warm failure — %s", f)
    sys.exit(1)

log.info("all models baked into the image")
