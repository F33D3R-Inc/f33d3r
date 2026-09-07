#!/usr/bin/env bash
# Failure tests (§28, §26, §53) against the local compose stack.
#
#   1. SIGKILL Auralis mid-session: on restart the live Frequency is intact,
#      its lease is re-adopted, the outbox drains, and the session can end.
#   2. Redis restart mid-session: nobody is departed and the Frequency is not
#      ended on the first sweep (cold-Redis guard); a heartbeat repopulates
#      presence; the Frequency stays live.
#
#   INTERNAL_API_KEY=... frequencies/scripts/failure_tests.sh
set -euo pipefail

AURALIS_URL="${AURALIS_URL:-http://localhost:8108}"
KEY="${INTERNAL_API_KEY:?INTERNAL_API_KEY must be set}"
COMPOSE=(docker compose -f docker-compose.local.yml --env-file .env.local)
HOST_PIAL="c0ffee00-0000-4000-8000-00000000b001"
LISTENER_PIAL="c0ffee00-0000-4000-8000-00000000b002"

pass() { printf '  \033[32m✓\033[0m %s\n' "$1"; }
fail() { printf '  \033[31m✗\033[0m %s\n' "$1"; exit 1; }
call() {
  local method="$1" path="$2" pial="$3" body="${4:-}"
  curl -sS -X "$method" "$AURALIS_URL$path" -H "X-Internal-Key: $KEY" -H "X-Pial-Identity: $pial" \
    -H 'Content-Type: application/json' ${body:+-d "$body"}
}
wait_ready() {
  for _ in $(seq 1 45); do curl -sf "$AURALIS_URL/ready" >/dev/null 2>&1 && return 0; sleep 2; done
  fail "auralis did not become ready"
}
db() { PGPASSWORD=f33d3rdev psql -h localhost -U f33d3r -d f33d3r_frequencies -tAc "$1"; }

echo "── 1. SIGKILL Auralis mid-session"
C=$(call POST /v1/frequencies "$HOST_PIAL" '{"title":"Kill test"}'); FID=$(echo "$C" | jq -r .frequency.frequency.id)
call POST "/v1/frequencies/$FID/start" "$HOST_PIAL" | jq -e '.frequency.frequency.state=="live"' >/dev/null && pass "live $FID"
call POST "/v1/frequencies/$FID/tune_in" "$LISTENER_PIAL" | jq -e '.frequency.counts.listeners==1' >/dev/null && pass "listener in"
# Stop the drain from having published yet: kill within the same second as a write.
call POST "/v1/frequencies/$FID/request_mic" "$LISTENER_PIAL" '{"reason":"before the kill"}' >/dev/null
docker kill -s KILL f33d3r-auralis-1 >/dev/null && pass "SIGKILL sent"
sleep 1
"${COMPOSE[@]}" up -d auralis >/dev/null 2>&1
wait_ready && pass "restarted and ready"
LOG=$(docker logs f33d3r-auralis-1 2>&1 | grep "boot reconciliation done" | tail -1)
echo "$LOG" | jq -e '.adopted>=1 and .failed==0' >/dev/null && pass "boot reconciliation adopted the live lease ($(echo "$LOG" | jq -c '{adopted,failed,completed}'))" || fail "reconcile: $LOG"
[ "$(db "SELECT count(*) FROM frequencies WHERE id='$FID' AND state='live'")" = "1" ] && pass "frequency still live in postgres" || fail "state changed"
[ "$(db "SELECT count(*) FROM frequencies WHERE host_pial='pial:$HOST_PIAL' AND state IN ('starting','live','ending')")" = "1" ] && pass "no duplicate frequency object" || fail "duplicates"
sleep 3
[ "$(db "SELECT count(*) FROM frequency_events WHERE frequency_id='$FID' AND published_at IS NULL")" = "0" ] && pass "outbox drained after restart" || fail "unpublished rows remain"
call POST "/v1/frequencies/$FID/heartbeat" "$LISTENER_PIAL" | jq -e '.present==true' >/dev/null && pass "listener heartbeat accepted after restart"
call POST "/v1/frequencies/$FID/end" "$HOST_PIAL" | jq -e '.frequency.frequency.state=="ended"' >/dev/null && pass "host ended after restart"

echo "── 2. Redis restart mid-session"
C=$(call POST /v1/frequencies "$HOST_PIAL" '{"title":"Redis test"}'); FID=$(echo "$C" | jq -r .frequency.frequency.id)
call POST "/v1/frequencies/$FID/start" "$HOST_PIAL" | jq -e '.frequency.frequency.state=="live"' >/dev/null && pass "live $FID"
call POST "/v1/frequencies/$FID/tune_in" "$LISTENER_PIAL" | jq -e '.frequency.counts.listeners==1' >/dev/null && pass "listener in"
# Make the row old enough that a naive sweeper would call the host lost.
db "UPDATE frequencies SET updated_at = NOW() - INTERVAL '10 minutes' WHERE id='$FID'" >/dev/null
docker restart f33d3r-redis-1 >/dev/null && pass "redis restarted (all presence keys gone)"
for _ in $(seq 1 20); do docker exec f33d3r-redis-1 redis-cli ping 2>/dev/null | grep -q PONG && break; sleep 1; done
# One full sweep interval passes with Redis cold.
sleep 12
[ "$(db "SELECT state FROM frequencies WHERE id='$FID'")" = "live" ] && pass "not ended by the first sweep after a cold redis" || fail "ended: $(db "SELECT state, end_reason FROM frequencies WHERE id='$FID'")"
[ "$(db "SELECT count(*) FROM frequency_participants WHERE frequency_id='$FID' AND state='joined'")" = "2" ] && pass "nobody departed on cold evidence" || fail "participants departed"
grep -q "redis was cold" <(docker logs f33d3r-auralis-1 2>&1) && pass "sweeper logged the cold round" || fail "no cold-round log line"
call POST "/v1/frequencies/$FID/heartbeat" "$LISTENER_PIAL" | jq -e '.present==true and .counts.listeners==1' >/dev/null && pass "heartbeat repopulated presence"
call POST "/v1/frequencies/$FID/heartbeat" "$HOST_PIAL" | jq -e '.present==true' >/dev/null && pass "host heartbeat repopulated presence"
call POST "/v1/frequencies/$FID/end" "$HOST_PIAL" | jq -e '.frequency.frequency.state=="ended"' >/dev/null && pass "ended cleanly"

echo
echo "Failure tests: PASS"
