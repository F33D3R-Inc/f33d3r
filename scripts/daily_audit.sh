#!/usr/bin/env bash
# F33D3R Daily Audit — runs via cron, posts summary to @admin on the platform.
# Schedule: add to crontab with `crontab -e`:
#   0 8 * * * /home/hiiro/Downloads/f33d3r-local/scripts/daily_audit.sh >> /var/log/f33d3r-audit.log 2>&1

set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DB_CONTAINER="f33d3r-local-postgres-1"
FEED_URL="http://localhost:8081"
LOG_DATE="$(date -u '+%Y-%m-%d %H:%M UTC')"

# ── Brain health ──────────────────────────────────────────────────────────────

check_brain() {
  local name="$1" url="$2"
  local status
  status=$(curl -sf --max-time 3 "${url}" 2>/dev/null | python3 -c "import sys,json; d=json.load(sys.stdin); print('ok' if d.get('status')=='ok' else 'degraded')" 2>/dev/null || echo "offline")
  echo "${name}:${status}"
}

BRAIN_NANTAR=$(check_brain "Nantar" "${FEED_URL}/api/health")
BRAIN_AETHYRRANK=$(check_brain "AethyrRank" "http://localhost:8080/health")
BRAIN_VOVIN=$(check_brain "Vovin" "http://localhost:8092/health")
BRAIN_AINSOPH=$(check_brain "AinSoph" "http://localhost:8089/health")
BRAIN_ELOHIM=$(check_brain "ElohimVeni" "http://localhost:8093/health")
BRAIN_ZODACARE=$(check_brain "Zodacare" "http://localhost:8090/health")
BRAIN_VERITY=$(check_brain "Verity" "http://localhost:8095/health")
BRAIN_LEDGER=$(check_brain "Ledger" "http://localhost:8096/health")

# ── DB stats ──────────────────────────────────────────────────────────────────

run_sql() {
  docker exec "${DB_CONTAINER}" psql -U f33d3r -d f33d3r_feed -t -A -c "$1" 2>/dev/null || echo "0"
}

USERS_TOTAL=$(run_sql "SELECT COUNT(*) FROM users;")
USERS_NEW=$(run_sql "SELECT COUNT(*) FROM users WHERE created_at > NOW() - INTERVAL '24 hours';")
POSTS_TOTAL=$(run_sql "SELECT COUNT(*) FROM posts;")
POSTS_NEW=$(run_sql "SELECT COUNT(*) FROM posts WHERE created_at > NOW() - INTERVAL '24 hours';")
FOLLOWS_NEW=$(run_sql "SELECT COUNT(*) FROM follows WHERE created_at > NOW() - INTERVAL '24 hours';")
LIKES_NEW=$(run_sql "SELECT COUNT(*) FROM post_likes WHERE created_at > NOW() - INTERVAL '24 hours';")
SESSIONS_ACTIVE=$(run_sql "SELECT COUNT(*) FROM user_sessions WHERE expires_at > NOW();")
FAILED_AUTH=$(run_sql "SELECT COUNT(*) FROM user_sessions WHERE created_at > NOW() - INTERVAL '24 hours';")
REPORTS_PENDING=$(run_sql "SELECT COUNT(*) FROM content_reports WHERE status = 'pending';" 2>/dev/null || echo "0")
PIAL_ROOTS=$(run_sql "SELECT COUNT(*) FROM pial_roots;")

# DB size
DB_SIZE=$(docker exec "${DB_CONTAINER}" psql -U f33d3r -d f33d3r_feed -t -A \
  -c "SELECT pg_size_pretty(pg_database_size('f33d3r_feed'));" 2>/dev/null || echo "unknown")

# Vovin DB
MSG_PENDING=$(docker exec "${DB_CONTAINER}" psql -U f33d3r -d f33d3r_msg -t -A \
  -c "SELECT COUNT(*) FROM direct_messages WHERE expires_at > NOW();" 2>/dev/null || echo "0")

# ── Disk usage ────────────────────────────────────────────────────────────────

UPLOADS_SIZE=$(du -sh "${REPO_DIR}/feed-engine/web/static/uploads" 2>/dev/null | cut -f1 || echo "unknown")

# ── Security flags ────────────────────────────────────────────────────────────

FAILED_LOGINS=$(run_sql "SELECT COUNT(*) FROM user_sessions WHERE created_at > NOW() - INTERVAL '24 hours';" 2>/dev/null || echo "0")

# ── Build audit post body ──────────────────────────────────────────────────────

ONLINE_COUNT=0
OFFLINE_LIST=""
for brain in "${BRAIN_NANTAR}" "${BRAIN_AETHYRRANK}" "${BRAIN_VOVIN}" "${BRAIN_AINSOPH}" "${BRAIN_ELOHIM}" "${BRAIN_ZODACARE}" "${BRAIN_VERITY}" "${BRAIN_LEDGER}"; do
  name="${brain%%:*}"
  status="${brain##*:}"
  if [[ "${status}" == "ok" ]]; then
    ONLINE_COUNT=$((ONLINE_COUNT + 1))
  else
    OFFLINE_LIST="${OFFLINE_LIST} ${name}(${status})"
  fi
done

HEALTH_LINE="${ONLINE_COUNT}/8 brains online"
[[ -n "${OFFLINE_LIST}" ]] && HEALTH_LINE="${HEALTH_LINE} | OFFLINE:${OFFLINE_LIST}"

AUDIT_BODY="[DAILY AUDIT ${LOG_DATE}]

Brains: ${HEALTH_LINE}
Users: ${USERS_TOTAL} total (+${USERS_NEW} new 24h) | Sessions: ${SESSIONS_ACTIVE} active | PIAL: ${PIAL_ROOTS}
Posts: ${POSTS_TOTAL} total (+${POSTS_NEW} new 24h) | Likes: +${LIKES_NEW} | Follows: +${FOLLOWS_NEW}
Messages (pending relay): ${MSG_PENDING}
Uploads: ${UPLOADS_SIZE} on disk | DB: ${DB_SIZE}
Moderation queue: ${REPORTS_PENDING} pending reports"

echo "=== F33D3R DAILY AUDIT ${LOG_DATE} ==="
echo "${AUDIT_BODY}"
echo "======================================="

# ── Post to platform as @admin ────────────────────────────────────────────────
# Requires a valid admin session token set in environment: ADMIN_SESSION_TOKEN
# Set it with: export ADMIN_SESSION_TOKEN=<token from browser cookie>

if [[ -n "${ADMIN_SESSION_TOKEN:-}" ]]; then
  CSRF_TOKEN=$(curl -sf -c /tmp/audit_cookies.txt \
    -H "Cookie: f33d3r_session=${ADMIN_SESSION_TOKEN}" \
    "${FEED_URL}/compose" 2>/dev/null \
    | grep -o 'name="csrf_token" value="[^"]*"' \
    | grep -o 'value="[^"]*"' \
    | sed 's/value="//;s/"//' || echo "")

  if [[ -n "${CSRF_TOKEN}" ]]; then
    curl -sf -X POST "${FEED_URL}/api/post" \
      -H "Cookie: f33d3r_session=${ADMIN_SESSION_TOKEN}" \
      -d "csrf_token=${CSRF_TOKEN}&body=$(python3 -c "import urllib.parse,sys; print(urllib.parse.quote(sys.argv[1]))" "${AUDIT_BODY}")" \
      > /dev/null && echo "Audit posted to feed."
  else
    echo "Could not get CSRF token — audit not posted to feed."
  fi
else
  echo "ADMIN_SESSION_TOKEN not set — audit printed to log only."
  echo "Set it with: export ADMIN_SESSION_TOKEN=<token>"
fi
