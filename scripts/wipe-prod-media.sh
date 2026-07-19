#!/usr/bin/env bash
# ─────────────────────────────────────────────────────────────────────────────
# wipe-prod-media.sh — Hard-reset all user media on the production server.
#
# WARNING: DESTRUCTIVE AND IRREVERSIBLE.
#
# What this does:
#   1. Stops transcoding and content-scan (active media writers)
#   2. Deletes all files inside the f33d3r_media Docker named volume
#   3. Nulls out all media-related columns in the works table
#   4. Clears video_raw_hashes (dedup table)
#   5. Restarts transcoding and content-scan
#
# What this does NOT do:
#   - It does not drop the Docker volume (keeps the volume, empties it)
#   - It does not touch user accounts, posts, or non-media work fields
#   - It does not wipe the content_scan_data volume (SQLite DB for fingerprints)
#     Pass --wipe-scan-db to also clear that.
#
# Usage (on prod server, from repo root):
#   bash scripts/wipe-prod-media.sh
#   bash scripts/wipe-prod-media.sh --wipe-scan-db
#
# ─────────────────────────────────────────────────────────────────────────────
set -euo pipefail

cd "$(dirname "$(realpath "$0")")/.."

COMPOSE="docker compose -f docker-compose.prod.yml --env-file .env.prod"

GREEN="\033[0;32m"; YELLOW="\033[1;33m"; RED="\033[0;31m"; BOLD="\033[1m"; NC="\033[0m"
info() { echo -e "${GREEN}[wipe-media]${NC} $*"; }
warn() { echo -e "${YELLOW}[wipe-media]${NC} $*"; }
err()  { echo -e "${RED}[wipe-media]${NC} $*" >&2; }

WIPE_SCAN_DB=0
if [[ "${1:-}" == "--wipe-scan-db" ]]; then
  WIPE_SCAN_DB=1
fi

echo -e "\n${RED}${BOLD}══════════════════════════════════════════════════════${NC}"
echo -e "${RED}${BOLD}  WARNING: PROD MEDIA WIPE — THIS CANNOT BE UNDONE    ${NC}"
echo -e "${RED}${BOLD}══════════════════════════════════════════════════════${NC}\n"
echo -e "  This will:"
echo -e "    • Delete ALL files in the f33d3r_media volume"
echo -e "    • Null media columns on ALL works rows"
echo -e "    • Clear video_raw_hashes (dedup table)"
if [ "$WIPE_SCAN_DB" = "1" ]; then
  echo -e "    • Wipe content_scan SQLite DB (--wipe-scan-db)"
fi
echo ""
read -r -p "  Type YES to confirm: " confirm
if [[ "$confirm" != "YES" ]]; then
  err "Aborted."
  exit 1
fi

# ── Step 1: Stop active media writers ────────────────────────────────────────
echo ""
info "Stopping transcoding and content-scan..."
$COMPOSE stop transcoding content-scan || true

# ── Step 2: Wipe media volume contents ───────────────────────────────────────
info "Wiping f33d3r_media volume contents..."
$COMPOSE run --rm --no-deps \
  -v f33d3r_media:/wipe_target \
  --entrypoint sh \
  caeor -c "rm -rf /wipe_target/* /wipe_target/.[!.]* 2>/dev/null || true"
info "Media volume cleared."

# ── Step 3: Null media columns in works table ─────────────────────────────────
info "Nulling media columns in works table..."
$COMPOSE exec -T postgres psql \
  -U "${POSTGRES_USER:-f33d3r}" \
  -d "${POSTGRES_DB:-f33d3r_feed}" \
  -c "
UPDATE works SET
  video_master_url      = NULL,
  video_watermarked_url = NULL,
  video_poster_url      = NULL,
  video_duration_secs   = NULL,
  video_width           = NULL,
  video_height          = NULL,
  voice_url             = NULL,
  voice_duration_secs   = NULL,
  media_urls            = '{}'
WHERE
  video_master_url IS NOT NULL
  OR video_watermarked_url IS NOT NULL
  OR video_poster_url IS NOT NULL
  OR voice_url IS NOT NULL
  OR media_urls != '{}';
"
info "Works media columns nulled."

# ── Step 4: Clear video_raw_hashes (dedup table) ──────────────────────────────
info "Clearing video_raw_hashes..."
$COMPOSE exec -T postgres psql \
  -U "${POSTGRES_USER:-f33d3r}" \
  -d "${POSTGRES_DB:-f33d3r_feed}" \
  -c "TRUNCATE video_raw_hashes;"
info "video_raw_hashes cleared."

# ── Step 5: Wipe content_scan SQLite DB (optional) ───────────────────────────
if [ "$WIPE_SCAN_DB" = "1" ]; then
  info "Wiping content_scan SQLite DB..."
  $COMPOSE run --rm --no-deps \
    -v content_scan_data:/scan_data \
    --entrypoint sh \
    content-scan -c "rm -f /scan_data/content_scan.db /scan_data/*.db 2>/dev/null || true"
  info "content_scan DB cleared."
fi

# ── Step 6: Restart services ──────────────────────────────────────────────────
info "Restarting transcoding and content-scan..."
$COMPOSE up -d transcoding content-scan
sleep 5

if $COMPOSE ps --status running transcoding 2>/dev/null | grep -q "transcoding"; then
  info "transcoding: running"
else
  warn "transcoding did not come back up — check: $COMPOSE logs transcoding"
fi

echo ""
info "Media wipe complete."
info "Run: bash deploy.sh --check"
