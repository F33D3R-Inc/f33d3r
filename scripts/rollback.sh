#!/usr/bin/env bash
# ─────────────────────────────────────────────────────────────────────────────
# F33D3R Rollback — restore a pre-deploy backup: every database and the image
# every service was running.
#
# Usage:
#   bash scripts/rollback.sh <backup set> [--db-only] [--yes]
#
#   <backup set>  the directory deploy.sh --all printed, e.g.
#                 /var/backups/f33d3r/db/predeploy/20261116T031500Z
#                 There is no default. Rolling back to "the latest" without
#                 naming it is how the wrong state gets restored.
#   --db-only     restore only the databases (a scheduled set, which records
#                 no images, needs this)
#   --yes         do not ask for the typed confirmation (for a runbook that
#                 has already confirmed)
#
# What it does, in order, and it stops at the first failure:
#   1. checks the set is complete and intact, and that every image it records
#      is still tagged on this host;
#   2. takes a backup of the CURRENT databases (db/prerollback/<stamp>), so a
#      rollback can itself be undone;
#   3. stops every service except postgres;
#   4. restores every database in the set, strictly (infra/backup/restore.sh);
#   5. re-tags each service's recorded image back to the name compose runs;
#   6. starts everything without building, and runs deploy.sh's health check.
# ─────────────────────────────────────────────────────────────────────────────
set -euo pipefail

cd "$(dirname "$(realpath "$0")")/.."

[ -f .env.prod ] || { echo "[rollback] .env.prod not found in $(pwd)" >&2; exit 1; }
COMPOSE="docker compose -f docker-compose.prod.yml --env-file .env.prod"

GREEN="\033[0;32m"; YELLOW="\033[1;33m"; RED="\033[0;31m"; BOLD="\033[1m"; NC="\033[0m"
info() { echo -e "${GREEN}[rollback]${NC} $*"; }
warn() { echo -e "${YELLOW}[rollback]${NC} $*"; }
fail() { echo -e "${RED}[rollback]${NC} $*" >&2; exit 1; }

SET_HOST=""; DB_ONLY=0; YES=0
for arg in "$@"; do
  case "$arg" in
    --db-only) DB_ONLY=1 ;;
    --yes)     YES=1 ;;
    -*)        fail "unknown option $arg" ;;
    *)         [ -z "$SET_HOST" ] || fail "only one backup set may be named"; SET_HOST="$arg" ;;
  esac
done
[ -n "$SET_HOST" ] || fail "usage: bash scripts/rollback.sh <backup set directory> [--db-only] [--yes]
  The backup set is required; deploy.sh --all prints it."

command -v jq >/dev/null || fail "jq is required"

backup_run() { $COMPOSE run --rm --no-deps -T --entrypoint /bin/sh backup "$@"; }

# The backup set as the backup container sees it.
HOST_ROOT="$($COMPOSE config --format json | jq -er '.services.backup.volumes[] | select(.target == "/backups") | .source')"
SET_HOST="${SET_HOST%/}"
case "$SET_HOST" in
  "$HOST_ROOT"/db/*) ;;
  *) fail "$SET_HOST is not a backup set under $HOST_ROOT/db" ;;
esac
SET="/backups${SET_HOST#"$HOST_ROOT"}"
STAMP="$(basename "$SET_HOST")"

# ── 1. the set ────────────────────────────────────────────────────────────────
info "Checking $SET_HOST"
backup_run /backup/backup.sh verify "$SET" || fail "the backup set is not usable"

IMAGES=""
if [ "$DB_ONLY" -eq 0 ]; then
  IMAGES="$(backup_run -c 'cat "$1"' sh "$SET/images.txt")" \
    || fail "$SET_HOST records no images (a scheduled set?). Use --db-only to restore its databases alone."
  while read -r svc ref id tag; do
    [ -n "$svc" ] || continue
    [ "$tag" = "-" ] && continue
    docker image inspect "$tag" >/dev/null || fail "image $tag for $svc is no longer on this host"
  done <<< "$IMAGES"
fi

echo ""
echo -e "${BOLD}This replaces every database with the state of ${STAMP}${NC}"
[ "$DB_ONLY" -eq 1 ] || echo -e "${BOLD}and puts every service back on the image it ran then.${NC}"
echo "Everything written since then is replaced (a backup of it is taken first)."
if [ "$YES" -eq 0 ]; then
  read -r -p "Type the backup stamp ($STAMP) to proceed: " answer
  [ "$answer" = "$STAMP" ] || fail "not confirmed; nothing changed"
fi

# ── 2. a backup of now ────────────────────────────────────────────────────────
NOW="$(date -u +%Y%m%dT%H%M%SZ)"
info "Backing up the current databases to $HOST_ROOT/db/prerollback/$NOW"
backup_run /backup/backup.sh db "/backups/db/prerollback/$NOW" \
  || fail "could not back up the current state; nothing changed"

# ── 3. stop everything but postgres ───────────────────────────────────────────
mapfile -t OTHERS < <($COMPOSE config --services | grep -vxF postgres)
info "Stopping ${#OTHERS[@]} services (postgres stays up)"
$COMPOSE stop "${OTHERS[@]}"

# ── 4. databases ──────────────────────────────────────────────────────────────
info "Restoring databases from $SET_HOST"
backup_run /backup/restore.sh "$SET" || fail "database restore FAILED. Services are stopped.
  The state before this rollback is in $HOST_ROOT/db/prerollback/$NOW:
    bash scripts/rollback.sh $HOST_ROOT/db/prerollback/$NOW --db-only"

# ── 5. images ─────────────────────────────────────────────────────────────────
if [ "$DB_ONLY" -eq 0 ]; then
  while read -r svc ref id tag; do
    [ -n "$svc" ] || continue
    if [ "$tag" = "-" ]; then
      warn "$svc was not running at $STAMP; it keeps its current image"
      continue
    fi
    docker tag "$tag" "$ref"
    info "$svc → $ref = $tag"
  done <<< "$IMAGES"
fi

# ── 6. start + health ─────────────────────────────────────────────────────────
info "Starting every service (no build)"
$COMPOSE up -d --no-build
CHECK_TIMEOUT_SECONDS=600 bash ./deploy.sh --check || fail "services are up but the health check failed — see above"

info "Rollback to $STAMP complete."
info "To undo it (databases):  bash scripts/rollback.sh $HOST_ROOT/db/prerollback/$NOW --db-only"
