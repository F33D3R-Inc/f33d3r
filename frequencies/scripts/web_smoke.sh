#!/usr/bin/env bash
# Web smoke for Frequencies Phase 2, through Nantar exactly as a browser would:
# sessions from /api/v1/auth, mutations as htmx form posts to /events with the
# CSRF Origin header, reads as pages and facets. Two accounts: a host and a
# listener. Requires curl, jq, and the local stack.
#
#   frequencies/scripts/web_smoke.sh            (defaults: https://localhost:8443, fadroid / Droid-Test-2026)
#
# Nantar is only reachable through Caddy (tls internal). curl reads
# CURL_CA_BUNDLE as --cacert; set it to the system bundle to run this
# against a real host.
set -eu

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
export CURL_CA_BUNDLE="${CURL_CA_BUNDLE:-$REPO_DIR/infra/caddy-root.crt}"
NANTAR="${NANTAR_URL:-https://localhost:8443}"
HOST_HANDLE="${HOST_HANDLE:-fadroid}"
HOST_PASS="${HOST_PASS:-Droid-Test-2026}"
LISTENER_HANDLE="${LISTENER_HANDLE:-freqlistener}"
LISTENER_PASS="${LISTENER_PASS:-Listener-Test-2026}"

pass() { printf '  \033[32m✓\033[0m %s\n' "$1"; }
fail() { printf '  \033[31m✗\033[0m %s\n' "$1"; exit 1; }

login() { # handle pass → token
  curl -sS -X POST "$NANTAR/api/v1/auth/login" -H 'Content-Type: application/json' -H "Origin: $NANTAR" \
    -d "{\"handle\":\"$1\",\"password\":\"$2\",\"device_name\":\"smoke\"}" | jq -r '.token // empty'
}
signup() { # handle pass
  curl -sS -X POST "$NANTAR/api/v1/auth/signup" -H 'Content-Type: application/json' -H "Origin: $NANTAR" \
    -d "{\"handle\":\"$1\",\"password\":\"$2\",\"display_name\":\"$1\",\"device_name\":\"smoke\",\"date_of_birth\":\"1990-01-01\",\"role_type\":\"user\",\"terms_accepted\":true}" | jq -r '.token // empty'
}
page() { # token path → body (full page)
  curl -sS "$NANTAR$2" -H "Authorization: Bearer $1"
}
facet() { # token path → body (HX-Request)
  curl -sS "$NANTAR$2" -H "Authorization: Bearer $1" -H 'HX-Request: true'
}
event() { # token event_type k=v... → body; sends an htmx form post
  local tok="$1" et="$2"; shift 2
  local args=(-d "event_type=$et")
  for kv in "$@"; do args+=(--data-urlencode "$kv"); done
  curl -sS -X POST "$NANTAR/events" -H "Authorization: Bearer $tok" -H "Origin: $NANTAR" -H 'HX-Request: true' \
    -H 'HX-Current-URL: '"$NANTAR${HX_URL:-/golive}" "${args[@]}"
}

echo "Web smoke against $NANTAR"

HT=$(login "$HOST_HANDLE" "$HOST_PASS"); [ -n "$HT" ] && pass "host session ($HOST_HANDLE)" || fail "host login failed"
LT=$(login "$LISTENER_HANDLE" "$LISTENER_PASS")
if [ -z "$LT" ]; then LT=$(signup "$LISTENER_HANDLE" "$LISTENER_PASS"); fi
[ -n "$LT" ] && pass "listener session ($LISTENER_HANDLE)" || fail "listener login/signup failed"

echo "pages"
GL=$(page "$HT" "/golive?mode=frequency")
echo "$GL" | grep -q 'frequency.go_live' && pass "/golive?mode=frequency renders the Frequency form" || fail "no frequency form on /golive"
echo "$GL" | grep -q 'href="/golive"' && echo "$GL" | grep -q 'mode=frequency' && pass "Video | Frequency tabs present" || fail "tabs missing"
echo "$GL" | grep -q 'id="frequency-dock"' && pass "Shell carries the #frequency-dock mount" || fail "no dock mount in Shell"
page "$HT" "/frequencies" | grep -q 'freq-lanes\|Live now' && pass "/frequencies lanes page renders" || fail "/frequencies failed"
code=$(curl -sS -o /dev/null -w '%{http_code}' "$NANTAR/spheres" -H "Authorization: Bearer $HT"); [ "$code" = "301" ] && pass "/spheres → 301" || fail "/spheres → $code"

echo "host goes live (audio)"
S=$(event "$HT" frequency.go_live "title=Smoke Frequency" "description=curl end to end" "visibility=public")
FID=$(echo "$S" | grep -o 'data-facet-id="facet:f33d3r:frequency:[0-9a-f-]*:stage"' | head -1 | sed -E 's/.*frequency:([0-9a-f-]+):stage.*/\1/')
[ -n "$FID" ] && pass "stage rendered for $FID" || fail "go_live did not render a stage: $(echo "$S" | head -c 400)"
echo "$S" | grep -q 'facet:f33d3r:frequency:'"$FID"':dock' && pass "host dock rendered OOB" || fail "no dock in go_live answer"
echo "$S" | grep -qi 'End Frequency' && pass "host controls show End Frequency" || fail "no End control"

echo "listener"
export HX_URL="/frequencies/$FID"
PV=$(facet "$LT" "/facets/frequency/$FID/preview")
echo "$PV" | grep -q 'frequency.tune_in' && echo "$PV" | grep -qi 'mic will be off' && pass "pre-join preview: speakers, mic-off line, Tune In" || fail "preview: $(echo "$PV" | head -c 300)"
T=$(event "$LT" frequency.tune_in "frequency_id=$FID")
echo "$T" | grep -q 'frequency:'"$FID"':dock' && pass "tune in answered with the dock" || fail "tune_in: $(echo "$T" | head -c 300)"
echo "$T" | grep -qi 'Request Mic\|request_mic' && pass "listener sees Request Mic" || fail "no Request Mic in dock"
LC=$(facet "$LT" "/facets/frequency/$FID/listener_count")
echo "$LC" | grep -q '1 listening' && pass "listener count facet: 1 listening" || fail "count: $LC"
R=$(event "$LT" frequency.request_mic "frequency_id=$FID" "reason=I run a node in Lagos")
echo "$R" | grep -qi 'requested\|withdraw' && pass "Request Mic → Requested state" || fail "request_mic: $(echo "$R" | head -c 300)"

echo "host moderates"
Q=$(facet "$HT" "/facets/frequency/$FID/requests")
echo "$Q" | grep -q 'Lagos' && pass "host queue shows the reason" || fail "queue: $(echo "$Q" | head -c 300)"
RID=$(echo "$Q" | grep -o 'name="request_id" value="[0-9a-f-]*"' | head -1 | grep -o '[0-9a-f-]\{36\}' || true)
[ -n "$RID" ] && pass "request id $RID" || fail "no request_id in queue markup"
A=$(event "$HT" frequency.approve "frequency_id=$FID" "request_id=$RID")
SG=$(facet "$HT" "/facets/frequency/$FID/speakers")
echo "$SG" | grep -qi "$LISTENER_HANDLE" && pass "speaker grid shows the new speaker" || fail "speakers: $(echo "$SG" | head -c 300)"
LC2=$(facet "$LT" "/facets/frequency/$FID/controls")
echo "$LC2" | grep -qi 'mute' && pass "speaker controls show mute" || fail "controls: $(echo "$LC2" | head -c 300)"

echo "host ends"
E=$(event "$HT" frequency.end "frequency_id=$FID")
echo "$E" | grep -qi 'ended' && pass "stage shows ended" || fail "end: $(echo "$E" | head -c 300)"
page "$LT" "/" | grep -q 'id="frequency-dock" class="freq-dock-slot"' && pass "listener's Shell dock is empty after end" || fail "dock still present"
page "$HT" "/frequencies" | grep -q "$FID" && pass "ended Frequency listed in lanes" || fail "not in lanes"

echo
echo "Web smoke: PASS ($FID)"
