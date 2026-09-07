#!/usr/bin/env bash
# live_teardown.sh — one stream stopped publishing.
#
# Run by the media server on the not-ready edge, after the ladder process has
# been signalled. It tells the application server the broadcast is over and
# sweeps ladder output that no longer has a stream behind it.
#
# Argument 1: $MTX_PATH

set -uo pipefail

MTX_PATH="${1:?path required}"
STREAM_ID="${MTX_PATH##*/}"

MEDIA_ROOT="${LIVE_MEDIA_ROOT:-/data/media/live}"
SCAN_ROOT="${LIVE_SCAN_ROOT:-/data/live-scan}"
CONTROL_URL="${LIVE_CONTROL_URL:-http://feed-engine:8110}"
RETENTION_MINUTES="${LIVE_RETENTION_MINUTES:-120}"

log() { echo "[live-teardown ${STREAM_ID}] $*" >&2; }

if ! curl -fsS -m 10 -X POST "${CONTROL_URL}/live/hook/notready" \
      -H "Content-Type: application/json" \
      -H "X-Internal-Key: ${INTERNAL_API_KEY}" \
      -d "{\"stream_id\":\"${STREAM_ID}\"}" >/dev/null; then
  # The reconcile loop closes the row within two passes, so this is reported
  # rather than retried here.
  log "control plane did not accept the notready hook; leaving it to reconciliation"
fi

# Ladder output outlives the broadcast briefly so a viewer mid-segment is not
# cut off, then goes. Retention is bounded, never unbounded.
if [ -d "${MEDIA_ROOT}" ]; then
  find "${MEDIA_ROOT}" -mindepth 1 -maxdepth 1 -type d -mmin "+${RETENTION_MINUTES}" \
    -exec rm -rf {} + 2>/dev/null || log "sweep of ${MEDIA_ROOT} reported errors"
fi
# Sampled frames are evidence and are consumed by the scanner; anything left
# behind by a crash is swept on the same schedule.
if [ -d "${SCAN_ROOT}" ]; then
  find "${SCAN_ROOT}" -mindepth 1 -maxdepth 1 -type d -mmin "+${RETENTION_MINUTES}" \
    -exec rm -rf {} + 2>/dev/null || log "sweep of ${SCAN_ROOT} reported errors"
fi

log "teardown complete"
