#!/usr/bin/env bash
# The first milestone (§56), exercised against a running Auralis through its
# internal API exactly as Nantar will call it: the host creates and starts a
# Frequency, a listener tunes in, the count is authoritative, the listener asks
# for the mic with a reason, the host sees the request, approves it, the role
# is Speaker server-side, the host ends, the object is a durable ended row, and
# every event is in the outbox and on the topic.
#
#   AURALIS_URL=http://localhost:8108 INTERNAL_API_KEY=... frequencies/scripts/milestone1.sh
#
# Exits non-zero on the first assertion that fails. Needs curl, jq, and (for
# the topic check) docker with the sitra-achra container running.
set -euo pipefail

AURALIS_URL="${AURALIS_URL:-http://localhost:8108}"
KEY="${INTERNAL_API_KEY:?INTERNAL_API_KEY must be set}"
HOST_PIAL="${HOST_PIAL:-c0ffee00-0000-4000-8000-00000000a001}"
LISTENER_PIAL="${LISTENER_PIAL:-c0ffee00-0000-4000-8000-00000000a002}"

pass() { printf '  \033[32m✓\033[0m %s\n' "$1"; }
fail() { printf '  \033[31m✗\033[0m %s\n' "$1"; exit 1; }

call() { # method path pial [json]
  local method="$1" path="$2" pial="$3" body="${4:-}"
  if [ -n "$body" ]; then
    curl -sS -X "$method" "$AURALIS_URL$path" -H "X-Internal-Key: $KEY" -H "X-Pial-Identity: $pial" \
      -H "X-Request-ID: m1-$RANDOM" -H 'Content-Type: application/json' -d "$body"
  else
    curl -sS -X "$method" "$AURALIS_URL$path" -H "X-Internal-Key: $KEY" -H "X-Pial-Identity: $pial" \
      -H "X-Request-ID: m1-$RANDOM"
  fi
}

echo "Milestone 1 against $AURALIS_URL"

echo "health"
H=$(curl -sS "$AURALIS_URL/health"); echo "$H" | jq -e '.status=="ok"' >/dev/null && pass "health ok (schema v$(echo "$H" | jq .schema_version))" || fail "health: $H"
curl -sS "$AURALIS_URL/ready" | jq -e '.ready==true' >/dev/null && pass "ready" || fail "not ready"

echo "unauthenticated is refused"
code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$AURALIS_URL/v1/frequencies" -H 'Content-Type: application/json' -d '{"title":"x"}')
[ "$code" = "401" ] && pass "no key → 401" || fail "no key → $code"
code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$AURALIS_URL/v1/frequencies" -H "X-Internal-Key: $KEY" -H 'Content-Type: application/json' -d '{"title":"x"}')
[ "$code" = "401" ] && pass "key without identity → 401" || fail "key without identity → $code"

echo "host creates a Frequency"
C=$(call POST /v1/frequencies "$HOST_PIAL" '{"title":"Late Night Tech Talk","description":"fees, nodes, and why Lagos matters","language":"en"}')
FID=$(echo "$C" | jq -r '.frequency.frequency.id'); [ "$FID" != "null" ] || fail "create: $C"
echo "$C" | jq -e '.frequency.frequency.state=="draft"' >/dev/null && pass "created $FID as draft" || fail "state: $C"

echo "a browser cannot make itself host"
code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$AURALIS_URL/v1/frequencies/$FID/end" -H "X-Internal-Key: $KEY" -H "X-Pial-Identity: $LISTENER_PIAL")
[ "$code" = "403" ] && pass "listener End → 403" || fail "listener End → $code"

echo "host starts"
S=$(call POST "/v1/frequencies/$FID/start" "$HOST_PIAL")
echo "$S" | jq -e '.frequency.frequency.state=="live"' >/dev/null && pass "live" || fail "start: $S"
echo "$S" | jq -e '.session.token|length>40' >/dev/null && pass "host session token minted (role $(echo "$S" | jq -r .session.role))" || fail "no session: $S"
S2=$(call POST "/v1/frequencies/$FID/start" "$HOST_PIAL")
echo "$S2" | jq -e '.frequency.frequency.state=="live"' >/dev/null && pass "second Start is idempotent" || fail "second start: $S2"

echo "listener tunes in"
T=$(call POST "/v1/frequencies/$FID/tune_in" "$LISTENER_PIAL")
echo "$T" | jq -e '.frequency.viewer.role=="listener" and .frequency.counts.listeners==1' >/dev/null && pass "listener admitted, count 1" || fail "tune_in: $T"
echo "$T" | jq -e '.session.permissions==["subscribe"]' >/dev/null && pass "listener token: subscribe only" || fail "perms: $T"
echo "$T" | jq -e '.frequency.viewer.can_request_mic==true and .frequency.viewer.can_end==false' >/dev/null && pass "viewer abilities from the matrix" || fail "abilities: $T"

echo "listener requests the mic, with a reason"
R=$(call POST "/v1/frequencies/$FID/request_mic" "$LISTENER_PIAL" '{"reason":"I run a node in Lagos, can speak to fees"}')
RID=$(echo "$R" | jq -r .request.id); [ "$RID" != "null" ] || fail "request_mic: $R"
pass "request $RID queued"
R2=$(call POST "/v1/frequencies/$FID/request_mic" "$LISTENER_PIAL" '{"reason":"again"}')
echo "$R2" | jq -e ".created==false and .request.id==\"$RID\"" >/dev/null && pass "second request is the same request" || fail "dup: $R2"

echo "host sees the request"
V=$(call GET "/v1/frequencies/$FID" "$HOST_PIAL")
echo "$V" | jq -e ".frequency.requests[0].id==\"$RID\" and (.frequency.requests[0].reason|test(\"Lagos\"))" >/dev/null && pass "host view has the request with its reason" || fail "view: $V"
echo "$V" | jq -e '.frequency.present_pial_ids|length==2' >/dev/null && pass "fan-out list has host and listener" || fail "present: $V"
echo "$V" | jq -e '.frequency.counts.participants==2 and .frequency.counts.speakers==0 and .frequency.counts.listeners==1' >/dev/null && pass "counts: host + 0 speakers, 1 listening, 2 present" || fail "counts: $V"

echo "listener cannot approve"
code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$AURALIS_URL/v1/frequencies/$FID/requests/$RID/approve" -H "X-Internal-Key: $KEY" -H "X-Pial-Identity: $LISTENER_PIAL")
[ "$code" = "403" ] && pass "listener approve → 403" || fail "listener approve → $code"

echo "host approves"
A=$(call POST "/v1/frequencies/$FID/requests/$RID/approve" "$HOST_PIAL")
echo "$A" | jq -e '.frequency.requests|length==0' >/dev/null && pass "queue empty" || fail "approve: $A"
echo "$A" | jq -e '.frequency.speakers|map(select(.role=="speaker"))|length==1' >/dev/null && pass "role is Speaker server-side" || fail "speakers: $A"
echo "$A" | jq -e '.frequency.counts.speakers==1 and .frequency.counts.listeners==0' >/dev/null && pass "counts moved: 1 speaker, 0 listeners" || fail "counts: $A"

echo "speaker heartbeat"
HB=$(call POST "/v1/frequencies/$FID/heartbeat" "$LISTENER_PIAL")
echo "$HB" | jq -e '.role=="speaker" and .present==true' >/dev/null && pass "heartbeat answers role speaker" || fail "heartbeat: $HB"

echo "host ends"
E=$(call POST "/v1/frequencies/$FID/end" "$HOST_PIAL")
echo "$E" | jq -e '.frequency.frequency.state=="ended" and .frequency.frequency.end_reason=="host_ended"' >/dev/null && pass "ended, reason host_ended" || fail "end: $E"
E2=$(call POST "/v1/frequencies/$FID/end" "$HOST_PIAL")
echo "$E2" | jq -e '.frequency.frequency.state=="ended"' >/dev/null && pass "second End is idempotent" || fail "end2: $E2"
code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$AURALIS_URL/v1/frequencies/$FID/tune_in" -H "X-Internal-Key: $KEY" -H "X-Pial-Identity: $LISTENER_PIAL")
[ "$code" = "409" ] && pass "tune in after end → 409" || fail "tune in after end → $code"

echo "events"
EV=$(call GET "/v1/frequencies/$FID/events" "$HOST_PIAL")
for t in frequency.created frequency.started frequency.listener_joined frequency.speaker_requested frequency.speaker_approved frequency.ended; do
  echo "$EV" | jq -e ".events|map(.event_type)|index(\"$t\")!=null" >/dev/null && pass "$t persisted" || fail "$t missing: $(echo "$EV" | jq -c '.events|map(.event_type)')"
done
echo "$EV" | jq -e '.events|all(.schema_version=="1.0" and (.event_id|length==36))' >/dev/null && pass "every event carries the full envelope" || fail "envelope"

sleep 2
H2=$(curl -sS "$AURALIS_URL/health")
echo "$H2" | jq -e '.events_unpublished==0' >/dev/null && pass "outbox drained to Sitra Achra (unpublished 0)" || fail "outbox backlog: $(echo "$H2" | jq .events_unpublished)"

if docker ps --format '{{.Names}}' 2>/dev/null | grep -q sitra-achra; then
  ON_TOPIC=$(timeout 8 docker exec f33d3r-sitra-achra-1 rpk topic consume frequency.events -o start -f '%v\n' 2>/dev/null | grep -c "\"frequency_id\":\"$FID\"" || true)
  [ "${ON_TOPIC:-0}" -ge 6 ] && pass "$ON_TOPIC events for this Frequency on topic frequency.events" || fail "only ${ON_TOPIC:-0} events on the topic"
fi

echo "discovery lanes"
call GET "/v1/frequencies?lane=ended" "$HOST_PIAL" | jq -e ".items|map(.frequency.id)|index(\"$FID\")!=null" >/dev/null && pass "appears in lane=ended" || fail "ended lane"
call GET "/v1/frequencies?lane=live" "$HOST_PIAL" | jq -e ".items|map(.frequency.id)|index(\"$FID\")==null" >/dev/null && pass "absent from lane=live" || fail "live lane"

echo
echo "Milestone 1 control plane: PASS ($FID)"
