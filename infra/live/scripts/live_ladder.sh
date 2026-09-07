#!/usr/bin/env bash
# live_ladder.sh — build one stream's ABR ladder.
#
# Started by the media server the moment a source becomes ready and killed when
# it stops. It pulls the source back out of the media server over loopback RTSP,
# encodes the ladder, and packages CMAF/fMP4 HLS onto the shared media volume,
# where the live edge serves it. Nothing here talks to a viewer.
#
# Argument 1: $MTX_PATH        e.g. live/9f1c…  (the stream id under the prefix)
# Argument 2: $MTX_SOURCE_TYPE e.g. rtmpConn | srtConn | webRTCSession
#
# ── what this script decides: nothing ────────────────────────────────────────
#
# This file used to carry its own copy of the rung table and pick its own
# encoder, and both were mistakes of the same kind. The rung table was a second
# copy of the one in feed-engine/internal/live/ladder.go, so a bitrate corrected
# in one and not the other produced a master playlist advertising a bandwidth
# the segments did not carry, silently. And the encoder choice was made by a
# process that can see exactly one broadcast — its own — on a box where the
# thing that actually runs out is shared. Every broadcast independently
# concluded it could afford three 1080p encodes, and on the fourth the box was
# oversubscribed by arithmetic nobody was doing.
#
# So this script now measures and executes, and decides nothing. It probes what
# the source really is, reports that to the control plane, and receives back the
# plan: which rungs, on which encoder, at what segment duration, with what
# thread ceiling. The server has every broadcast in view and is the only thing
# that can size one against the others.
#
# The ladder never upscales, and that rule lives in the server's ladder table.

set -euo pipefail

MTX_PATH="${1:?path required}"
SOURCE_TYPE="${2:-unknown}"
STREAM_ID="${MTX_PATH##*/}"

MEDIA_ROOT="${LIVE_MEDIA_ROOT:-/data/media/live}"
SCAN_ROOT="${LIVE_SCAN_ROOT:-/data/live-scan}"
RTSP_BASE="${LIVE_RTSP_BASE:-rtsp://127.0.0.1:8554}"
CONTROL_URL="${LIVE_CONTROL_URL:-http://feed-engine:8110}"
DVR_SEGMENTS="${LIVE_DVR_SEGMENTS:-12}"
SAMPLE_SECONDS="${LIVE_SAMPLE_SECONDS:-10}"

# A source that stops delivering is a dead source: the demuxer gives up after
# this many seconds of socket silence so the process exits instead of grinding
# on a stream that no longer exists.
SOURCE_TIMEOUT_SECONDS="${LIVE_SOURCE_TIMEOUT_SECONDS:-10}"
# How long the encoder gets to honour SIGTERM before it is killed outright.
SHUTDOWN_GRACE_SECONDS="${LIVE_SHUTDOWN_GRACE_SECONDS:-10}"
# Output rate used when the source does not declare one — see the frame rate
# section below. Browsers capture at 30 by default.
DEFAULT_FPS="${LIVE_DEFAULT_FPS:-30}"
# Ceiling on the encoded rate. A publisher may declare more than the ladder is
# willing to pay for; it is capped rather than refused.
MAX_FPS="${LIVE_MAX_FPS:-60}"

# ── backpressure thresholds ──────────────────────────────────────────────────
# ffmpeg reports the ratio of encoded media time to wall time as "speed". Below
# 1.0 the encoder is falling behind a source that keeps arriving, and the gap
# only ever widens: the media server's read queue fills, it starts discarding
# frames with "reader is too slow", the pulled RTSP sequence breaks, and the
# demuxer errors out. Every one of those is downstream of this number, and this
# number is visible seconds earlier.
#
# The floor is below 1.0 because the measurement is noisy at the start of a
# broadcast and around keyframes, and the count is high enough that a blip
# cannot trip it: only a sustained inability to keep up is worth interrupting a
# broadcast over.
PRESSURE_SPEED_FLOOR="${LIVE_PRESSURE_SPEED_FLOOR:-0.94}"
PRESSURE_SAMPLES="${LIVE_PRESSURE_SAMPLES:-12}"
PRESSURE_GRACE_SECONDS="${LIVE_PRESSURE_GRACE_SECONDS:-20}"

# ── playable deadline ────────────────────────────────────────────────────────
# How long the encoder gets, from launch, to put a master playlist and one
# rung's first segment on disk. An encoder that is running but has written
# nothing after this long is not going to: it is killed so the media server
# restarts the ladder, rather than left holding a lease over a stream nobody
# can watch. Generous, because the first segment needs a full segment of media
# plus the encoder's own start-up, and a hardware session can take a moment to
# open under load.
PLAYABLE_TIMEOUT_SECONDS="${LIVE_PLAYABLE_TIMEOUT_SECONDS:-45}"

OUT="${MEDIA_ROOT}/${STREAM_ID}"
SCAN="${SCAN_ROOT}/${STREAM_ID}"
SRC="${RTSP_BASE}/${MTX_PATH}"
PROGRESS="/tmp/live-progress-${STREAM_ID}"

log() { echo "[live-ladder ${STREAM_ID}] $*" >&2; }

# ── probe the source ─────────────────────────────────────────────────────────
# The publisher has just connected; the first keyframe may be a moment away.
# avg_frame_rate is read here as well as the geometry: it is the only frame rate
# field that can be trusted (see the frame rate section). codec_name is read
# because it decides whether the top rung can be passed through instead of
# re-encoded, which is the single largest saving available to this lane.
#
# ONE probe answers for every track. Video and audio used to be asked for in
# two separate ffprobe runs, and only the video one was retried: the audio one
# ran once, swallowed its own failure, and a transient RTSP hiccup on that
# single read turned into a whole broadcast packaged without sound while the
# log said nothing louder than "packaging video only". The tracks a source
# carries are declared together in its SDP, so they are read together, from
# the one probe that succeeded, and the same retry covers both.
#
# The fields are read as key=value and parsed by name, never by position.
# ffprobe emits -show_entries in the order the STREAM declares them, not the
# order they were asked for: requesting width,height,avg_frame_rate,codec_name
# against a real H.264 source returns "h264x1920x1080x30/1" — the codec first.
# Anything that splits that on position reads the codec as the width, and the
# geometry check then rejects a perfectly good 1080p source.
#
# compact output puts one stream per line, and only the fields a stream has:
# width/height appear on video lines alone, sample_rate/channels on audio
# lines alone. A line is selected by its codec_type and then read by key.
#
# The output is captured and then matched, never piped into "grep -q". That is
# not a style preference, it is a correctness requirement under the
# "set -o pipefail" this script runs with. "grep -q" exits the moment it finds
# a match; the producer still writing into the closed pipe takes SIGPIPE and
# exits non-zero; and pipefail then reports the whole pipeline as FAILED
# because the match was found too early. The success case is the one that
# breaks, which is why it survives casual testing — an interactive shell has no
# pipefail set and behaves correctly.
#
# This cost a real diagnosis: it is what made every ladder conclude the box had
# no hardware encoder while the identical command run by hand said otherwise.
# The same shape appeared three times in this file and all three are gone.

# stream_line TYPE — the first probe line describing a stream of that type.
# awk reads its whole input and exits only afterwards, so the producer is never
# left writing into a closed pipe.
stream_line() {
  printf '%s\n' "${probe}" | awk -F'|' -v want="codec_type=$1" '
    !found { for (i = 1; i <= NF; i++) if ($i == want) { print; found = 1; break } }'
}
# stream_field LINE KEY — one key=value field out of a compact stream line.
stream_field() {
  printf '%s\n' "$1" | awk -F'|' -v key="$2" '
    { for (i = 1; i <= NF; i++) { n = index($i, "="); if (substr($i, 1, n - 1) == key) { print substr($i, n + 1); exit } } }'
}

probe=""
vline=""
for attempt in 1 2 3 4 5 6 7 8 9 10; do
  if probe="$(ffprobe -v error -rtsp_transport tcp \
        -show_entries stream=codec_type,codec_name,width,height,avg_frame_rate,sample_rate,channels \
        -of compact=p=0:nk=0 "${SRC}" 2>/dev/null)" && [ -n "${probe}" ]; then
    vline="$(stream_line video)"
    WIDTH="$(stream_field "${vline}" width)"
    HEIGHT="$(stream_field "${vline}" height)"
    AVG_RATE="$(stream_field "${vline}" avg_frame_rate)"
    SRC_CODEC="$(stream_field "${vline}" codec_name)"
    # A source that has not yet delivered a keyframe answers with a zero
    # geometry rather than nothing; that is not a probe result either.
    if [ "${WIDTH:-0}" -gt 0 ] 2>/dev/null && [ "${HEIGHT:-0}" -gt 0 ] 2>/dev/null; then
      break
    fi
    probe=""
  fi
  log "probe attempt ${attempt} found no video stream yet"
  sleep 1
done

if [ -z "${probe}" ]; then
  log "FATAL: source carried no video stream after 10 attempts"
  exit 1
fi
if ! [ "${HEIGHT:-0}" -gt 0 ] 2>/dev/null || ! [ "${WIDTH:-0}" -gt 0 ] 2>/dev/null; then
  log "FATAL: probe returned an unusable geometry: ${WIDTH:-?}x${HEIGHT:-?}"
  exit 1
fi
SRC_CODEC="${SRC_CODEC:-unknown}"
log "source ${WIDTH}x${HEIGHT} ${SRC_CODEC} via ${SOURCE_TYPE}"

# Does the source carry audio? A video-only publisher is legitimate, and
# mapping an audio track that is not there would abort the mux. The answer
# comes from the same probe that just established the video, and what was
# found is said in the log — codec, rate, channels — so a broadcast that
# reaches viewers without sound can be traced to the publisher or to this
# ladder by reading one line.
HAS_AUDIO=0
aline="$(stream_line audio)"
if [ -n "${aline}" ]; then
  HAS_AUDIO=1
  SRC_AUDIO_CODEC="$(stream_field "${aline}" codec_name)"
  SRC_AUDIO_RATE="$(stream_field "${aline}" sample_rate)"
  SRC_AUDIO_CHANNELS="$(stream_field "${aline}" channels)"
  log "source audio: ${SRC_AUDIO_CODEC:-unknown} ${SRC_AUDIO_RATE:-?} Hz ${SRC_AUDIO_CHANNELS:-?}ch — transcoding to aac 48000 Hz 2ch 192k on every rung"
fi

# ── frame rate ───────────────────────────────────────────────────────────────
# WebRTC is variable framerate and carries no frame rate in its RTP stream. Read
# back out of the media server over RTSP, such a source reports
# avg_frame_rate=0/0 and an r_frame_rate that is pure noise — measured at
# 90000/1 (the RTP clock) on one sample of a live browser publish and 2/1 on the
# next. Handed 90000 to libx264 as the frame rate, every rung exceeds the H.264
# level macroblock ceiling and the VBV budget collapses to a few bits a frame,
# so the encoders never keep up, the media server drops the ladder as a slow
# reader, and nothing is ever written. That is the whole failure.
#
# avg_frame_rate is the only field that is either right or honestly absent:
# measured 30/1 and 60/1 for CFR H.264 arriving over RTMP and SRT, and 0/0 for
# VP8 arriving over WebRTC. r_frame_rate is not usable for either — it reports
# 120/1 for a 60 fps RTMP source.
#
# The encoded output is then made constant rate. Segment durations, the
# keyframe cadence that aligns the rungs, and the encoders' rate control all
# need a rate that does not move. A source that declares one keeps it, so the
# CFR ingest paths are untouched; a source that does not gets the default.
#
# FPS_DECLARED records which of those two happened, and it is reported to the
# control plane rather than kept here, because it is one of the conditions on
# passing the top rung through: a rung copied from a source whose real cadence
# is unknown cannot be guaranteed to stay in step with rungs being emitted at a
# rate the ladder invented.
FPS=""
FPS_DECLARED=false
if [ -n "${AVG_RATE:-}" ] && [ "${AVG_RATE}" != "0/0" ]; then
  num="${AVG_RATE%%/*}"
  den="${AVG_RATE##*/}"
  if [ "${den:-0}" -gt 0 ] 2>/dev/null && [ "${num:-0}" -gt 0 ] 2>/dev/null; then
    # Trust it only inside a band a real capture device can produce.
    if awk "BEGIN{r=${num}/${den}; exit !(r>=1 && r<=120)}"; then
      if awk "BEGIN{exit !(${num}/${den} > ${MAX_FPS})}"; then
        FPS="${MAX_FPS}"
        log "source declares ${AVG_RATE} fps, capped to ${MAX_FPS}"
      else
        FPS="${AVG_RATE}"
        FPS_DECLARED=true
      fi
    else
      log "source declares an implausible ${AVG_RATE} fps; ignoring it"
    fi
  fi
fi
if [ -z "${FPS}" ]; then
  FPS="${DEFAULT_FPS}"
  log "source declares no usable frame rate (avg_frame_rate=${AVG_RATE:-none}); encoding at ${FPS}"
fi
# The integer rate the control plane costs the encode against. The filter graph
# keeps the exact rational so a 30000/1001 source is not quantised to 30.
FPS_INT="$(awk "BEGIN{split(\"${FPS}\",p,\"/\"); d=(p[2]==\"\"?1:p[2]); printf \"%d\", int(p[1]/d + 0.5)}")"
log "output frame rate: ${FPS}"

# ── keyframe cadence of the publisher ────────────────────────────────────────
# This is measured for one reason: a rung that is copied rather than encoded is
# not cut where we ask for it, it is cut where the publisher put its keyframes.
# There is no option that changes that — a remux writes through the elementary
# stream it was given. So if the top rung is going to be copied, every other
# rung has to be forced to keyframe on the publisher's cadence, and the segment
# duration has to become that cadence, or the rungs break at different instants
# and every bitrate switch stalls on a player waiting for a boundary that is not
# there.
#
# It is measured rather than assumed because it is the publisher's setting, not
# ours: OBS defaults to 2 seconds, hardware encoders vary, and a source we
# guessed wrong about would produce exactly the misalignment being avoided.
#
# The measurement is the median gap between keyframes over a short window. A
# median rather than a mean: a scene cut inserts an extra keyframe and would
# drag a mean down, and the interval that matters is the regular one.
GOP_SECONDS=0
if [ "${SRC_CODEC}" = "h264" ]; then
  gop="$(ffprobe -v error -rtsp_transport tcp -select_streams v:0 \
          -show_entries packet=pts_time,flags -of csv=p=0 \
          -read_intervals "%+12" "${SRC}" 2>/dev/null |
        awk -F, '
          /K/ { if (last != "") { d = $1 - last; if (d > 0.05) gaps[n++] = d } last = $1 }
          END {
            if (n < 2) exit
            for (i = 0; i < n; i++) for (j = i+1; j < n; j++)
              if (gaps[j] < gaps[i]) { t = gaps[i]; gaps[i] = gaps[j]; gaps[j] = t }
            printf "%.3f", gaps[int(n/2)]
          }' || true)"
  if [ -n "${gop}" ]; then
    GOP_SECONDS="${gop}"
    log "publisher keyframe interval: ${GOP_SECONDS}s"
  else
    log "publisher keyframe interval could not be measured; the top rung will be encoded"
  fi
fi

# ── hardware encoder capability ──────────────────────────────────────────────
# Reported to the control plane, never decided here — but it can only be
# ANSWERED here, because this is the container that would have to run the
# encode.
#
# The answer is established by opening a session, not by reading the encoder
# list. Those are different questions and this box is the reason to know it: the
# image's ffmpeg lists h264_vaapi, h264_qsv and h264_vulkan, and not one of the
# three can encode a frame here. There is no Intel graphics device at all, so
# QSV is a name with no hardware under it; /dev/dri/renderD128 is the NVIDIA
# card under the proprietary driver, which publishes no VAAPI encode entrypoint,
# so VAAPI fails at device creation; and Pascal has no Vulkan video encode
# extension to expose. An encoder that is listed is a codec the binary was
# compiled against. An encoder that opens a session is one that works.
#
# The probe runs once per container, not once per broadcast: whether this box
# has a usable hardware encoder does not change between streams, and a probe
# holds a real session for as long as it runs — on a card with a fixed session
# ceiling that is capacity taken from a broadcast.
NVENC_PROBE_CACHE="/tmp/live-nvenc-capability"
# How long a NEGATIVE answer is trusted before the card is asked again.
NVENC_RETRY_SECONDS="${LIVE_NVENC_RETRY_SECONDS:-120}"

# The probe's failure is REPORTED, never swallowed. A capability check that
# fails silently is undiagnosable by construction: the only symptom is that the
# whole platform quietly encodes in software, which looks exactly like a box
# that has no card. Whatever the card or the driver says on the way down is the
# only thing that distinguishes "there is no GPU here" from "something went
# wrong for a moment", and it belongs in the log.
NVENC_PROBE_LOG="/tmp/live-nvenc-probe.log"
nvenc_probe_once() {
  if ffmpeg -hide_banner -loglevel error -f lavfi \
      -i "color=c=black:size=320x240:rate=30:duration=0.2" \
      -c:v h264_nvenc -f null - >/dev/null 2>"${NVENC_PROBE_LOG}"; then
    return 0
  fi
  log "hardware encode probe failed: $(tr '\n' ' ' < "${NVENC_PROBE_LOG}" 2>/dev/null | head -c 300)"
  return 1
}

nvenc_available() {
  # A positive answer is permanent: a card that opened a session is a card that
  # has one, and re-proving it every broadcast would spend a session to learn
  # something already known.
  if [ -r "${NVENC_PROBE_CACHE}" ] && [ "$(cat "${NVENC_PROBE_CACHE}" 2>/dev/null)" = "true" ]; then
    echo true
    return 0
  fi

  # A NEGATIVE answer is not permanent, and treating it as one was a real fault
  # in this script's first version. Four ladders starting in the same second all
  # probe at once, and a probe can fail for reasons that have nothing to do with
  # whether the box has a GPU: momentary session contention, a driver that has
  # not finished coming up with the container, a card busy with something else.
  # Cached forever, one such failure disabled hardware encoding for the life of
  # the container — and because the control plane learns this capability from
  # the ladders, one bad second put the WHOLE PLATFORM on the software encoder
  # until someone restarted it. That is exactly the shape of failure this lane
  # was rebuilt to stop: a transient fault latched into permanent degraded state
  # with nothing saying so.
  #
  # So a negative is held only long enough to stop every broadcast paying for a
  # probe, and then the card is asked again.
  if [ -r "${NVENC_PROBE_CACHE}" ]; then
    local age
    age=$(( $(date +%s) - $(stat -c %Y "${NVENC_PROBE_CACHE}" 2>/dev/null || echo 0) ))
    if [ "${age}" -lt "${NVENC_RETRY_SECONDS}" ]; then
      echo false
      return 0
    fi
  fi

  # Two attempts before concluding the box has no hardware encoder. One failure
  # is a bad moment; two, a second apart, is a real answer.
  local answer=false
  local encoders
  encoders="$(ffmpeg -hide_banner -encoders 2>/dev/null || true)"
  case "${encoders}" in
    *h264_nvenc*) : ;;
    *) log "this ffmpeg carries no h264_nvenc encoder"; encoders="" ;;
  esac
  if [ -n "${encoders}" ]; then
    if nvenc_probe_once; then
      answer=true
    else
      sleep 1
      if nvenc_probe_once; then
        answer=true
      fi
    fi
  fi

  # Written via a temporary file so two ladders starting together cannot read a
  # half-written answer.
  echo "${answer}" > "${NVENC_PROBE_CACHE}.$$" 2>/dev/null &&
    mv -f "${NVENC_PROBE_CACHE}.$$" "${NVENC_PROBE_CACHE}" 2>/dev/null || true
  echo "${answer}"
}
NVENC_AVAILABLE="$(nvenc_available)"
log "hardware encoder available: ${NVENC_AVAILABLE}"

# ── ask the control plane for the plan ───────────────────────────────────────
# One round trip, at the one moment when the geometry is known and no frame has
# been encoded yet. The control plane records the measurements and answers
# with the ladder. It does NOT mark the stream live here — this hook used to,
# and that put "live" on the row twelve to twenty-five seconds before a
# playlist existed, so every viewer's decoder started on a 404. The stream is
# announced as live further down, by the playable hook, once the first segment
# has been read back off disk.
#
# A refusal here is final. The control plane knows what else the box is
# carrying, and a plan it will not issue is a ladder that would have damaged a
# broadcast already running. Packaging anyway is the behaviour being replaced.
PLAN_FILE="/tmp/live-plan-${STREAM_ID}.json"
if ! curl -fsS -m 15 -X POST "${CONTROL_URL}/live/hook/ready" \
      -H "Content-Type: application/json" \
      -H "X-Internal-Key: ${INTERNAL_API_KEY}" \
      -o "${PLAN_FILE}" \
      -d "{\"stream_id\":\"${STREAM_ID}\",\"width\":${WIDTH},\"height\":${HEIGHT},\"protocol\":\"${SOURCE_TYPE}\",\"codec\":\"${SRC_CODEC}\",\"fps\":${FPS_INT},\"fps_declared\":${FPS_DECLARED},\"gop_seconds\":${GOP_SECONDS},\"nvenc_available\":${NVENC_AVAILABLE}}"; then
  log "FATAL: control plane issued no plan — refusing to package this stream"
  exit 1
fi

ENCODER="$(jq -r '.encoder' "${PLAN_FILE}")"
SEGMENT_SECONDS="$(jq -r '.segment_seconds' "${PLAN_FILE}")"
DEGRADED="$(jq -r '.degraded' "${PLAN_FILE}")"
PLAN_REASON="$(jq -r '.reason // ""' "${PLAN_FILE}")"
N="$(jq -r '.rungs | length' "${PLAN_FILE}")"

if [ -z "${ENCODER}" ] || [ "${ENCODER}" = "null" ] || ! [ "${N:-0}" -ge 1 ] 2>/dev/null; then
  log "FATAL: control plane returned an unusable plan: $(head -c 400 "${PLAN_FILE}")"
  exit 1
fi
if ! [ "${SEGMENT_SECONDS:-0}" -ge 1 ] 2>/dev/null; then
  log "FATAL: control plane returned an unusable segment duration: ${SEGMENT_SECONDS}"
  exit 1
fi
if [ "${DEGRADED}" = "true" ]; then
  log "DEGRADED: ${PLAN_REASON}"
fi

# The plan's rungs, one per line, as height:kbps:maxkbps:bufkbps:name:mode:threads.
mapfile -t RUNGS < <(jq -r '.rungs[] | "\(.height):\(.video_kbps):\(.max_kbps):\(.buf_kbps):\(.name):\(.mode):\(.threads)"' "${PLAN_FILE}")
log "plan: ${ENCODER} seg=${SEGMENT_SECONDS}s rungs=${RUNGS[*]}"

# atomic_writing keeps a half-written poster from ever being served.
ATOMIC=()
image2_help="$(ffmpeg -hide_banner -h muxer=image2 2>/dev/null || true)"
case "${image2_help}" in
  *atomic_writing*) ATOMIC=(-atomic_writing 1) ;;
esac

mkdir -p "${OUT}" "${SCAN}"
# The HLS muxer writes each rung into its own directory via %v. Those
# directories are created here, from the same plan that builds -var_stream_map,
# so the two can never disagree about which exist.
for rung in "${RUNGS[@]}"; do
  IFS=':' read -r _h _v _m _b rname _mode _t <<< "${rung}"
  mkdir -p "${OUT}/${rname}"
done
# The scanner consumes and deletes these frames from another container; the
# private scan volume is shared so the evidence must be removable by its reader.
chmod 0777 "${SCAN}" 2>/dev/null || log "could not relax permissions on ${SCAN}"

# ── build the ffmpeg invocation ──────────────────────────────────────────────
# One process, one decode, N scaled encodes plus the poster and the safety
# sampler. Splitting this into several processes would decode the source once
# per output for no benefit.
#
# A copied rung takes no branch of the filter graph at all: it is mapped from
# the input stream directly, which is the whole point — no scale, no colour
# conversion, no encode, and the picture the publisher sent arrives at the
# viewer having been through exactly one encoder rather than two.
ENCODED=0
COPY_RUNGS_PLANNED=0
for rung in "${RUNGS[@]}"; do
  IFS=':' read -r _h _v _m _b _n rmode _t <<< "${rung}"
  if [ "${rmode}" = "copy" ]; then
    COPY_RUNGS_PLANNED=$((COPY_RUNGS_PLANNED + 1))
  else
    ENCODED=$((ENCODED + 1))
  fi
done

SPLIT_LABELS=""
for i in $(seq 0 $((ENCODED - 1))); do
  SPLIT_LABELS="${SPLIT_LABELS}[s${i}]"
done
# The rate is normalised once, ahead of the split, so every encoded rung and the
# poster share one timeline and the forced keyframes land on the same frames.
FILTER="[0:v]fps=${FPS},split=$((ENCODED + 1))${SPLIT_LABELS}[sposter];"
ei=0
for rung in "${RUNGS[@]}"; do
  IFS=':' read -r rh _v _m _b _n rmode _t <<< "${rung}"
  if [ "${rmode}" = "copy" ]; then
    continue
  fi
  FILTER="${FILTER}[s${ei}]scale=-2:${rh}[v${ei}];"
  ei=$((ei + 1))
done
# One decode feeds the ladder, the poster and the safety sampler. The sample
# branch is split because a filter output may be consumed exactly once.
#
# The decimation comes BEFORE the scale, and the order is worth 0.19 cores per
# broadcast — measured, paired, in one load window: 2.543 cores against 2.352
# for an otherwise identical ladder. Scaling first meant resizing 1080 lines to
# 720 thirty times a second in order to keep one frame in three hundred, and
# throwing away the other two hundred and ninety-nine after paying for them.
# Selecting the frame first means that scale runs a tenth of a time a second.
# The frames chosen are the same frames either way: the fps filter picks by
# timestamp, and a picture is not changed by when it was resized.
FILTER="${FILTER}[sposter]fps=1/${SAMPLE_SECONDS},scale=-2:720,split=2[vposter][vsample]"

# -timeout bounds socket reads: when the publisher goes away the demuxer errors
# out and the process ends, rather than running on against a source that is no
# longer there. The RTSP stream carries real RTP timestamps, so no timestamp
# generation is asked for or wanted.
# -y is not optional. The poster and the sampler are outputs this command
# rewrites in place for the life of the broadcast, and runOnAvailableRestart
# means a ladder restarts against a directory its previous run already wrote.
# Without it ffmpeg refuses to open an existing poster.jpg and the WHOLE
# encode exits, taking every rung with it while the row still reads live.
FF=(ffmpeg -hide_banner -loglevel warning -nostdin -y
    -progress "${PROGRESS}")

# ── one clock for the whole ladder ───────────────────────────────────────────
# A copied rung and an encoded rung do not otherwise share a timeline, and one
# HLS muxer cannot segment two.
#
# The copied rung is written through carrying the publisher's own timestamps,
# which over RTSP are RTP timestamps starting at whatever the sender's clock
# happened to be — measured at 146045 on one broadcast. The encoded rungs go
# through the fps filter, which hands the encoder a timeline starting at zero.
# Muxed together the segmenter has two clocks, and what it produced was a
# copied rung cutting correctly every 2 seconds while the encoded rungs never
# reached a boundary at all: measured, on four live broadcasts, as ONE 23
# second segment per encoded rung against #EXT-X-TARGETDURATION:23. Every rung
# individually playable, the ladder as a whole unswitchable — the exact defect
# passthrough was supposed to avoid, arriving through a door I had not checked.
#
# -copyts keeps the input's own timestamps instead of rebasing each output
# independently, and -start_at_zero shifts that single shared timeline to begin
# at zero. Both branches then measure "2 seconds" from the same origin.
#
# It is applied only when a rung is actually being copied. An all-encoded
# ladder has one clock already, and every WebRTC broadcast is all-encoded —
# those sources carry the noisiest timestamps of any ingest here and there is
# no reason to hand them the demuxer's clock when nothing needs it.
#
# This is why the offline proof of this feature was not enough: a source FILE
# starts at zero, so both timelines coincide by accident and the ladder looks
# aligned. Only a live RTSP source has a clock that starts somewhere else.
if [ "${COPY_RUNGS_PLANNED}" -gt 0 ]; then
  FF+=(-copyts -start_at_zero)
fi

FF+=(-rtsp_transport tcp
    -timeout "$((SOURCE_TIMEOUT_SECONDS * 1000000))"
    -i "${SRC}")
FF+=(-filter_complex "${FILTER}")

# The scale filters are the one part of the graph that will spread across every
# core it is offered, and on the hardware path they are the only CPU cost left.
# Bounded to the ladder's own width so one broadcast's filtering cannot occupy
# the cores another broadcast was admitted on the promise of.
FF+=(-filter_threads "$((ENCODED + 1))")

VAR_MAP=""
ei=0
for i in $(seq 0 $((N - 1))); do
  IFS=':' read -r _h _v _m _b rname rmode _t <<< "${RUNGS[$i]}"
  if [ "${rmode}" = "copy" ]; then
    FF+=(-map "0:v:0")
  else
    FF+=(-map "[v${ei}]")
    ei=$((ei + 1))
  fi
  if [ "${HAS_AUDIO}" -eq 1 ]; then
    VAR_MAP="${VAR_MAP}v:${i},a:${i},name:${rname} "
  else
    VAR_MAP="${VAR_MAP}v:${i},name:${rname} "
  fi
done
if [ "${HAS_AUDIO}" -eq 1 ]; then
  for _ in $(seq 0 $((N - 1))); do
    # Every rung carries the same audio track: the picture degrades under a poor
    # connection, the microphone does not.
    FF+=(-map "0:a:0")
  done
else
  log "source carries no audio track; packaging video only"
fi

COPY_RUNGS=0
for i in $(seq 0 $((N - 1))); do
  IFS=':' read -r _h vkbps maxkbps bufkbps _n rmode rthreads <<< "${RUNGS[$i]}"
  if [ "${rmode}" = "copy" ]; then
    # Passed through untouched. No bitrate, no rate control, no frame rate mode:
    # every one of those is a property of an encode that is not happening.
    FF+=("-c:v:${i}" copy)
    COPY_RUNGS=$((COPY_RUNGS + 1))
    continue
  fi
  case "${ENCODER}" in
    h264_nvenc)
      FF+=("-c:v:${i}" h264_nvenc "-preset:v:${i}" p4 "-tune:v:${i}" ll
           "-profile:v:${i}" high "-rc:v:${i}" cbr)
      # forced-idr is not optional and is the whole reason an NVENC ladder
      # segments at all. -force_key_frames asks the encoder for a keyframe;
      # NVENC answers with a plain I-frame, which is a perfectly good picture
      # and is NOT an IDR. The HLS muxer splits on IDR and nothing else, so
      # without this it never finds a boundary: measured on four live NVENC
      # broadcasts as ONE 26 second segment per encoded rung, against a copied
      # rung cutting correctly every 2 seconds beside it. Each rung played;
      # the ladder could not be switched, which is the entire point of a ladder.
      #
      # -g pins the GOP to the segment duration as well, so the cadence holds
      # even in the gaps where the expression is not driving it.
      FF+=("-forced-idr:v:${i}" 1 "-g:v:${i}" "$((SEGMENT_SECONDS * FPS_INT))")
      ;;
    *)
      FF+=("-c:v:${i}" libx264 "-preset:v:${i}" veryfast "-tune:v:${i}" zerolatency
           "-profile:v:${i}" high)
      # The thread ceiling the control plane sized for this rung. Left to
      # itself libx264 builds its pool from the whole machine and one 1080p
      # rung will open a dozen threads; capped, it does the same encode without
      # sprawling into another broadcast's share. The zerolatency tune already
      # selects sliced threading, which is what makes a low cap safe — frame
      # threading would buy the throughput back by holding frames, and this
      # lane cannot spend that latency.
      if [ "${rthreads:-0}" -gt 0 ] 2>/dev/null; then
        FF+=("-threads:v:${i}" "${rthreads}")
      fi
      # The same GOP pinning as the hardware path. libx264 does honour
      # -force_key_frames with real IDRs, so this is belt and braces rather
      # than load-bearing — but a ladder whose two encoders disagree about
      # keyframe cadence is a ladder waiting to misalign.
      FF+=("-g:v:${i}" "$((SEGMENT_SECONDS * FPS_INT))")
      ;;
  esac
  FF+=("-b:v:${i}" "${vkbps}k" "-maxrate:v:${i}" "${maxkbps}k" "-bufsize:v:${i}" "${bufkbps}k")
  # Constant rate is the contract the segment durations depend on.
  FF+=("-fps_mode:v:${i}" cfr)
done

# yuv420p is the encoders' input format and must not be applied to a copied
# rung: -pix_fmt against a stream that is being remuxed is a request to convert
# a picture nothing is decoding.
if [ "${COPY_RUNGS}" -lt "${N}" ]; then
  ei=0
  for i in $(seq 0 $((N - 1))); do
    IFS=':' read -r _h _v _m _b _n rmode _t <<< "${RUNGS[$i]}"
    [ "${rmode}" = "copy" ] && continue
    FF+=("-pix_fmt:v:${i}" yuv420p)
  done
fi
# Aligned keyframes across every rung — without this a bitrate switch stalls.
# When a rung is copied this cadence is the publisher's own, measured above and
# handed back by the control plane as the segment duration, so the encoded rungs
# break on the same frames the copied one does.
FF+=(-force_key_frames "expr:gte(t,n_forced*${SEGMENT_SECONDS})")
if [ "${HAS_AUDIO}" -eq 1 ]; then
  FF+=(-c:a aac -b:a 192k -ac 2 -ar 48000)
fi

FF+=(-f hls
     -hls_time "${SEGMENT_SECONDS}"
     -hls_list_size "${DVR_SEGMENTS}"
     -hls_flags "delete_segments+independent_segments+program_date_time+temp_file"
     -hls_segment_type fmp4
     -hls_fmp4_init_filename "init.mp4"
     -master_pl_name "master.m3u8"
     -var_stream_map "${VAR_MAP% }"
     -hls_segment_filename "${OUT}/%v/seg_%05d.m4s"
     "${OUT}/%v/index.m3u8")

# Poster: the still the Facet shows before playback starts.
FF+=(-map "[vposter]" -f image2 -update 1 "${ATOMIC[@]}" "${OUT}/poster.jpg")
# Safety sampler: the same frame, into a private volume the edge cannot reach.
FF+=(-map "[vsample]" -f image2 -strftime 1 "${ATOMIC[@]}" "${SCAN}/%Y%m%d-%H%M%S.jpg")

log "encoding: ${VAR_MAP}"

# The encoder runs as a child rather than replacing this shell, so that the
# media server's stop signal is always answered: an encoder that is wedged is
# killed outright instead of being left holding a core, and the exit status is
# reported rather than swallowed.
#
# It is started under job control so that it lands in a process group of its
# own, and that is load-bearing. The media server stops this command with
# kill(-pgid, SIGINT) — a signal to the whole group — so without a group of its
# own the encoder receives that SIGINT directly, at the same moment this shell's
# trap sends it a second one. ffmpeg answers the first signal by finalising the
# outputs: it writes the fMP4 trailer, rewrites each rung's index.m3u8 with
# EXT-X-ENDLIST and rewrites master.m3u8. A second signal arriving during that
# window aborts it, and what is left on disk is a zero-length master.m3u8, a
# zero-length index.m3u8 for every rung and a zero-length final segment — the
# entire DVR tail of the broadcast destroyed at the moment it ends, measured on
# every stop before this. Owning the group means the stop is delivered once,
# here, in the way ffmpeg is documented to honour.
#
# It is niced, because the media server's own ingest threads share this
# container and they are the thing the encoder is reading from. Under pressure
# an encoder that outranks its own source starves the socket it is draining and
# turns a slow ladder into a dead one.
#
# Nothing is orphaned by this. The encoder's source read carries -timeout, so an
# encoder that outlives this shell has at most SOURCE_TIMEOUT_SECONDS of silence
# before its demuxer errors out and the process ends on its own.
: > "${PROGRESS}"
# The launch instant, as a file: the playable check below compares playlist
# mtimes against it, so a restarted ladder is never fooled by the playlists its
# previous run left in the same directory. The progress file cannot serve as
# that mark because ffmpeg rewrites it every second.
LAUNCH_MARK="${PROGRESS}.launch"
: > "${LAUNCH_MARK}"
set -m
nice -n 5 "${FF[@]}" &
FF_PID=$!
set +m

# ── what the microphone is actually delivering ───────────────────────────────
# The ladder can prove it mapped an audio track and encoded it; it could not
# say whether that track carried anything. "Video plays, no sound" then has
# two indistinguishable causes — a pipeline that dropped the audio, and a
# publisher whose microphone input is dead or muted at the source — and the
# only way to tell them apart was to pull segments off the volume and measure
# them by hand. This measures the source once, in parallel with the encoder,
# and says what it found. Peak and mean over the first seconds: digital
# silence reads about -91 dBFS, an idle room -40 to -60, speech -25 and up.
#
# Read from the media server, not from the ladder's output, so the number is
# about the publisher and nothing this script does can move it. Bounded by -t
# and the source timeout, run in the background so going live waits on
# nothing, and its own failure is reported rather than allowed to end the
# ladder: a measurement is never a reason to stop packaging a broadcast.
AUDIO_LEVEL_SECONDS="${LIVE_AUDIO_LEVEL_SECONDS:-5}"
measure_source_audio() {
  local report peak
  report="$(ffmpeg -hide_banner -nostdin -loglevel info \
      -rtsp_transport tcp -timeout "$((SOURCE_TIMEOUT_SECONDS * 1000000))" \
      -i "${SRC}" -t "${AUDIO_LEVEL_SECONDS}" -vn -map 0:a:0 -af volumedetect -f null - 2>&1 |
    awk '/volumedetect/ && (/mean_volume/ || /max_volume/) { sub(/^.*\] /, ""); printf "%s ", $0 } END { print "" }' || true)"
  report="${report% }"
  if [ -z "${report}" ]; then
    log "source audio level could not be measured in the first ${AUDIO_LEVEL_SECONDS}s"
    return 0
  fi
  log "source audio level over the first ${AUDIO_LEVEL_SECONDS}s: ${report}"
  peak="$(printf '%s\n' "${report}" | awk '{ for (i = 1; i <= NF; i++) if ($i == "max_volume:") print $(i + 1) }')"
  if [ -n "${peak}" ] && awk "BEGIN{exit !(${peak} < -40)}" 2>/dev/null; then
    log "source audio peaked below -40 dBFS in the first ${AUDIO_LEVEL_SECONDS}s: viewers will hear silence unless the publisher's microphone input is live"
  fi
}
LEVEL_PID=""
if [ "${HAS_AUDIO}" -eq 1 ]; then
  measure_source_audio &
  LEVEL_PID=$!
fi

STOP_REQUESTED=0
PRESSURE_RESTART=0

stop_encoder() {
  STOP_REQUESTED=1
  log "stop requested — signalling the encoder"
  kill -TERM "${FF_PID}" 2>/dev/null || true
  for _ in $(seq 1 "${SHUTDOWN_GRACE_SECONDS}"); do
    kill -0 "${FF_PID}" 2>/dev/null || return 0
    sleep 1
  done
  log "encoder did not stop within ${SHUTDOWN_GRACE_SECONDS}s — killing it"
  kill -KILL "${FF_PID}" 2>/dev/null || true
}
trap stop_encoder TERM INT

# ── backpressure watcher ─────────────────────────────────────────────────────
# The failure this watches for is the one that used to end broadcasts silently.
# When the box is oversubscribed the encoder cannot hold realtime; the media
# server's read queue fills; it logs "reader is too slow, discarding N frames"
# and starts dropping them; the RTSP sequence the encoder is reading breaks; the
# demuxer reports an I/O error and exits — and the row still says live, with
# nothing being written, for the rest of the broadcast.
#
# Every step of that is downstream of a speed below 1.0, which ffmpeg reports
# once a second and nothing was reading. This reads it, and when the encoder has
# been unable to keep up for long enough that it is not a blip, it tells the
# control plane. The control plane decides what happens next, because it is the
# only thing that knows whether there is a smaller ladder to fall back to: it
# answers either "restart" — at which point this exits, the media server starts
# a fresh ladder within seconds, and that ladder asks for and receives the
# reduced plan — or "continue", when this broadcast is already as small as the
# platform can make it and interrupting it would achieve nothing.
watch_pressure() {
  local below=0 speed line
  sleep "${PRESSURE_GRACE_SECONDS}"
  while kill -0 "${FF_PID}" 2>/dev/null; do
    sleep 1
    line="$(grep '^speed=' "${PROGRESS}" 2>/dev/null | tail -n 1 || true)"
    speed="${line#speed=}"
    speed="${speed%x}"
    speed="${speed// /}"
    case "${speed}" in
      ''|*[!0-9.]*) continue ;;
    esac
    if awk "BEGIN{exit !(${speed} < ${PRESSURE_SPEED_FLOOR})}"; then
      below=$((below + 1))
    else
      below=0
      continue
    fi
    [ "${below}" -lt "${PRESSURE_SAMPLES}" ] && continue
    below=0
    log "encoder holding only ${speed}x realtime — reporting backpressure"
    local verdict
    verdict="$(curl -fsS -m 10 -X POST "${CONTROL_URL}/live/hook/pressure" \
        -H "Content-Type: application/json" \
        -H "X-Internal-Key: ${INTERNAL_API_KEY}" \
        -d "{\"stream_id\":\"${STREAM_ID}\",\"speed\":${speed}}" 2>/dev/null || true)"
    if [ -z "${verdict}" ]; then
      log "control plane did not answer the pressure report; continuing"
      continue
    fi
    log "control plane: $(echo "${verdict}" | jq -r '.reason // ""')"
    if [ "$(echo "${verdict}" | jq -r '.restart')" = "true" ]; then
      # Touched before the signal so the exit path can tell an interruption we
      # asked for from a broadcast that simply ended.
      touch "${PROGRESS}.restart"
      kill -TERM "${FF_PID}" 2>/dev/null || true
      return 0
    fi
  done
}
watch_pressure &
WATCH_PID=$!

# ── announce the stream as playable ──────────────────────────────────────────
# This is the hook that marks the stream live, and it is sent at the first
# moment that is true for a viewer: the master playlist exists and at least one
# rung's playlist lists a segment. Both are read back off the volume the edge
# serves from, so what is being announced is what a player will fetch, not what
# the encoder was asked for.
#
# The muxer writes each playlist through a temporary file (hls_flags temp_file)
# and renames it into place, so a playlist that exists is a whole one; the
# check never sees a half-written index.
#
# An encoder that has not written a first segment by the deadline is killed.
# The exit path below then reports a non-zero encoder exit, this script exits
# non-zero, and the media server starts a fresh ladder — which re-probes,
# re-plans and tries again. What never happens is a lease held over a stream
# that nobody can watch while the row keeps quiet about it.
playable_written() {
  [ -s "${OUT}/master.m3u8" ] || return 1
  [ "${OUT}/master.m3u8" -nt "${LAUNCH_MARK}" ] || return 1
  local idx
  for idx in "${OUT}"/*/index.m3u8; do
    [ -s "${idx}" ] || continue
    [ "${idx}" -nt "${LAUNCH_MARK}" ] || continue
    if grep -q '^#EXTINF' "${idx}" 2>/dev/null; then
      return 0
    fi
  done
  return 1
}

announce_playable() {
  local waited=0
  while kill -0 "${FF_PID}" 2>/dev/null; do
    if playable_written; then
      log "first segment on disk after ${waited}s — announcing the stream as live"
      if ! curl -fsS -m 10 -X POST "${CONTROL_URL}/live/hook/playable" \
            -H "Content-Type: application/json" \
            -H "X-Internal-Key: ${INTERNAL_API_KEY}" \
            -o /dev/null \
            -d "{\"stream_id\":\"${STREAM_ID}\",\"width\":${WIDTH},\"height\":${HEIGHT},\"protocol\":\"${SOURCE_TYPE}\"}"; then
        # The segments are there and the edge is serving them; only the row is
        # behind. Said loudly, because until the control plane accepts this the
        # watch page keeps showing the idle wall over a working broadcast.
        log "control plane did not accept the playable hook; retrying"
        sleep 2
        continue
      fi
      return 0
    fi
    if [ "${waited}" -ge "${PLAYABLE_TIMEOUT_SECONDS}" ]; then
      log "FATAL: encoder running for ${waited}s and no playable segment on disk — killing it so the media server restarts the ladder"
      touch "${PROGRESS}.stalled"
      kill -TERM "${FF_PID}" 2>/dev/null || true
      return 1
    fi
    sleep 1
    waited=$((waited + 1))
  done
  return 1
}
announce_playable &
PLAYABLE_PID=$!

set +e
wait "${FF_PID}"
status=$?
# A trapped signal makes wait return early; keep waiting for the real exit.
while kill -0 "${FF_PID}" 2>/dev/null; do
  wait "${FF_PID}"
  status=$?
done
set -e

kill -TERM "${WATCH_PID}" 2>/dev/null || true
kill -TERM "${PLAYABLE_PID}" 2>/dev/null || true
if [ -n "${LEVEL_PID}" ]; then
  kill -TERM "${LEVEL_PID}" 2>/dev/null || true
fi
if [ -e "${PROGRESS}.restart" ]; then
  PRESSURE_RESTART=1
fi
STALLED=0
if [ -e "${PROGRESS}.stalled" ]; then
  STALLED=1
fi
rm -f "${PROGRESS}" "${PROGRESS}.restart" "${PROGRESS}.stalled" "${LAUNCH_MARK}" "${PLAN_FILE}" 2>/dev/null || true

# A stop we asked for is the ordinary end of a broadcast, not a failure, and no
# status is read from it. Two things make that status meaningless here: ffmpeg
# answers a stop signal by finalising its outputs and then exiting 255, which is
# neither success nor a signal death; and a trapped signal makes bash's own wait
# return 128+signum for the interruption rather than for the child. Anything that
# ends the encoder without us asking means the ladder stopped being produced
# while the source was still there, and is reported as such — the media server
# logs the exit and starts a fresh ladder.
if [ "${STOP_REQUESTED}" -eq 1 ]; then
  log "encoder stopped on request"
  exit 0
fi
if [ "${PRESSURE_RESTART}" -eq 1 ]; then
  # Deliberate, and deliberately non-zero: the media server restarts a ladder
  # that exits, and the replacement is what picks up the reduced plan. Exiting
  # zero here would end the ladder for the rest of the broadcast.
  log "restarting under the reduced plan"
  exit 75
fi
if [ "${STALLED}" -eq 1 ]; then
  # The encoder never produced a playable segment and was killed for it. The
  # media server restarts the ladder; the row was never marked live, so no
  # viewer was told to expect anything.
  log "FATAL: encoder produced no playable segment within ${PLAYABLE_TIMEOUT_SECONDS}s — restarting the ladder"
  exit 76
fi
if [ "${status}" -ne 0 ]; then
  log "FATAL: encoder exited ${status} — the ladder for this stream stopped being produced"
  exit "${status}"
fi
log "encoder finished cleanly"
