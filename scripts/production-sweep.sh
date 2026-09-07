#!/usr/bin/env bash
# production-sweep.sh — run content lineage sweep on the production server
#
# Run this directly ON the Hetzner CCX23 server (not from your local machine).
# It executes sweep.py inside the running content-scan container against the
# production f33d3r_feed database and live media volume.
#
# Usage:
#   bash production-sweep.sh              # scan all unscanned videos
#   bash production-sweep.sh --dry-run   # preview matches without writing
#   bash production-sweep.sh --all       # reprocess everything (re-fingerprint)
#   bash production-sweep.sh --limit 50  # process first 50 only

set -euo pipefail

CONTAINER="f33d3r-local-content-scan-1"
DRY_RUN=""
ALL_FLAG=""
LIMIT_FLAG=""

for arg in "$@"; do
  case "$arg" in
    --dry-run) DRY_RUN="--dry-run" ;;
    --all)     ALL_FLAG="--all" ;;
    --limit*)  LIMIT_FLAG="$arg" ;;
  esac
done

echo "=== F33D3R Content Lineage Sweep ==="
echo "Container : $CONTAINER"
echo "Dry run   : ${DRY_RUN:-no}"
echo "Reprocess : ${ALL_FLAG:---}"
echo "Limit     : ${LIMIT_FLAG:--all-}"
echo ""

# Confirm the container is running
if ! docker ps --filter "name=${CONTAINER}" --filter "status=running" --format "{{.Names}}" | grep -q "$CONTAINER"; then
  echo "ERROR: Container $CONTAINER is not running."
  echo "Start it with: docker compose -f docker-compose.local.yml up -d content-scan"
  exit 1
fi

echo "Starting sweep..."
echo "────────────────────────────────────────"

docker exec "$CONTAINER" python sweep.py $DRY_RUN $ALL_FLAG $LIMIT_FLAG

echo "────────────────────────────────────────"
echo "Done. Any matched posts now have lineage_handle set in f33d3r_feed."
echo "The ◆ Content by @handle chip will appear on those posts immediately."
