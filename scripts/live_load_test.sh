#!/usr/bin/env bash
# F33D3R live-ladder load test — synthetic bots.
#
# Drives the *real* app surface, not a bypass: each bot signs up a fresh
# account (POST /api/v1/auth/signup), starts a broadcast (POST /live/start),
# reveals its own RTMP credentials (POST /live/{id}/encoder) and publishes a
# synthetic test pattern with ffmpeg. This is what exercises the capacity
# Governor's admission/backpressure/shed logic for real, including the
# measured 8-session NVENC ceiling on this box's GTX 1060 (LIVE_NVENC_SESSIONS
# in .env.local) — a plain curl against mediamtx's RTMP port would never touch
# any of that, since mediamtx authenticates ingest through feed-engine.
#
# Usage: scripts/live_load_test.sh [--publishers N] [--viewers M]
#          [--resolution WxH] [--fps N] [--duration SECONDS]
#
#   --publishers 3   under the 8-session NVENC ceiling: expect steady hardware
#                     encode, no shedding.
#   --publishers 9   deliberately over budget: expect the Governor to admit 8
#                     and cleanly reject or CPU-fallback the 9th, not stall.

set -euo pipefail

# Both go through Caddy: it proxies the app and routes the ladder's own
# files (m3u8/m4s/mp4/jpg) to live-edge, so app and edge share one origin,
# the same way a browser sees them. curl reads CURL_CA_BUNDLE as --cacert.
REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export CURL_CA_BUNDLE="${CURL_CA_BUNDLE:-$REPO_DIR/infra/caddy-root.crt}"
BASE_URL="${LIVE_LOAD_TEST_BASE_URL:-https://localhost:8443}"
EDGE_URL="${LIVE_LOAD_TEST_EDGE_URL:-$BASE_URL}"
PUBLISHERS=3
VIEWERS=0
RESOLUTION="1920x1080"
FPS=30
DURATION=60

usage() {
  grep '^#' "$0" | sed -e '1d' -e 's/^# \{0,1\}//'
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --publishers) PUBLISHERS="$2"; shift 2 ;;
    --viewers) VIEWERS="$2"; shift 2 ;;
    --resolution) RESOLUTION="$2"; shift 2 ;;
    --fps) FPS="$2"; shift 2 ;;
    --duration) DURATION="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage; exit 1 ;;
  esac
done

command -v jq >/dev/null || { echo "jq is required" >&2; exit 1; }
command -v ffmpeg >/dev/null || { echo "ffmpeg is required" >&2; exit 1; }

PIDS=()
BOT_LABELS=()

cleanup() {
  echo "── stopping ${#PIDS[@]} bot process(es) ──"
  for pid in "${PIDS[@]:-}"; do
    kill "$pid" >/dev/null 2>&1 || true
  done
  wait 2>/dev/null || true
}
trap cleanup EXIT INT TERM

run_id="$(date +%s)"

start_publisher() {
  local n="$1"
  local handle="loadbot${run_id}p${n}"

  local signup_resp token
  signup_resp="$(curl -sS -X POST "$BASE_URL/api/v1/auth/signup" \
    -H "Content-Type: application/json" \
    -d "$(jq -nc --arg h "$handle" --arg dn "Load Bot $n" '
      {handle:$h, password:"LoadTest!bot2026", display_name:$dn,
       date_of_birth:"1995-01-01", role_type:"creator", terms_accepted:true}')")"
  token="$(jq -r '.token // empty' <<<"$signup_resp")"
  if [[ -z "$token" ]]; then
    echo "publisher $n: signup failed — $signup_resp" >&2
    return 1
  fi

  local start_headers location stream_id
  start_headers="$(curl -sS -D - -o /dev/null -X POST "$BASE_URL/live/start" \
    -H "Authorization: Bearer $token" \
    --data-urlencode "title=Load test bot $n" \
    --data-urlencode "source=rtmp" \
    --data-urlencode "facing=environment")"
  location="$(grep -i '^location:' <<<"$start_headers" | sed -E 's/^[Ll]ocation: //' | tr -d '\r')"
  stream_id="$(sed -E 's#^/live/([^/?]+).*#\1#' <<<"$location")"
  if [[ -z "$stream_id" || "$stream_id" == "$location" ]]; then
    echo "publisher $n: /live/start did not return a broadcast redirect — $start_headers" >&2
    return 1
  fi

  local encoder_frag encoder_flat rtmp_server stream_key
  encoder_frag="$(curl -sS -X POST "$BASE_URL/live/$stream_id/encoder" \
    -H "Authorization: Bearer $token")"
  # The template spans id= and value= across separate lines, so this has to be
  # matched as one line, not line-by-line.
  encoder_flat="$(tr '\n' ' ' <<<"$encoder_frag")"
  rtmp_server="$(grep -o 'id="live-obs-server-[^"]*"[^>]*value="[^"]*"' <<<"$encoder_flat" \
    | sed -E 's/.*value="([^"]*)".*/\1/')"
  stream_key="$(grep -o 'id="live-obs-key-[^"]*"[^>]*value="[^"]*"' <<<"$encoder_flat" \
    | sed -E 's/.*value="([^"]*)".*/\1/' | sed 's/&amp;/\&/g')"
  if [[ -z "$rtmp_server" || -z "$stream_key" ]]; then
    echo "publisher $n: could not extract RTMP credentials — $encoder_frag" >&2
    return 1
  fi

  echo "publisher $n: stream $stream_id ($handle) → $rtmp_server/<key>"
  # This is the BOT'S OWN upload encode, not the server ladder under test — it
  # has to stay cheap on CPU (ultrafast/zerolatency/low bitrate) so N publisher
  # bots don't become the bottleneck themselves and confound what the ladder
  # actually did with the real source frames it received.
  ffmpeg -hide_banner -loglevel warning -nostdin -re \
    -f lavfi -i "testsrc=size=${RESOLUTION}:rate=${FPS}" \
    -f lavfi -i "sine=frequency=$((300 + n * 37))" \
    -c:v libx264 -preset ultrafast -tune zerolatency -b:v 4M -c:a aac -f flv -t "$DURATION" \
    "${rtmp_server}/${stream_key}" &
  PIDS+=("$!")
  BOT_LABELS+=("$stream_id")
}

start_viewer() {
  local n="$1" stream_id="$2"
  ( while true; do
      curl -sS -o /dev/null "$EDGE_URL/live/$stream_id/master.m3u8" || true
      sleep 2
    done ) &
  PIDS+=("$!")
  echo "viewer $n: polling $EDGE_URL/live/$stream_id/master.m3u8"
}

echo "── starting $PUBLISHERS publisher bot(s) ──"
for n in $(seq 1 "$PUBLISHERS"); do
  start_publisher "$n" || true
  sleep 1 # stagger signups so the auth rate limiter sees distinct requests, not a burst
done

if [[ "$VIEWERS" -gt 0 && ${#BOT_LABELS[@]} -gt 0 ]]; then
  echo "── starting $VIEWERS viewer bot(s) ──"
  for n in $(seq 1 "$VIEWERS"); do
    idx=$(( (n - 1) % ${#BOT_LABELS[@]} ))
    start_viewer "$n" "${BOT_LABELS[$idx]}"
  done
fi

echo "── ${#BOT_LABELS[@]}/$PUBLISHERS publisher(s) live — running for ${DURATION}s ──"
sleep "$DURATION"
echo "── duration elapsed, cleaning up ──"
