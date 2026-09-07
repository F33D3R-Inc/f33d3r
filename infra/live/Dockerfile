# F33D3R live ingest server.
#
# MediaMTX for ingest, plus the ffmpeg toolchain the ladder script runs, the
# curl its lifecycle hooks use and the jq it parses its encode plan with.
#
# ── why this is not the -ffmpeg image any more ───────────────────────────────
#
# bluenviron/mediamtx:latest-ffmpeg is Alpine, and its ffmpeg is built
# --enable-vaapi --enable-vulkan --enable-libx264 and nothing else. On this host
# that means every encode is software, and it means so despite the box holding a
# GPU that can do the job: the three hardware encoders that image advertises are
# all names with nothing behind them here.
#
#   h264_qsv     needs Intel graphics. lspci finds one display device in this
#                box and it is the NVIDIA card. There is no Intel GPU to use.
#   h264_vaapi   /dev/dri/renderD128 IS the NVIDIA card — DRIVER=nvidia in its
#                uevent, not an Intel iGPU as its device number suggests. The
#                proprietary driver publishes no VAAPI encode entrypoint, so
#                with the device passed through, vaInitialize fails outright:
#                "Failed to initialise VAAPI connection: -1".
#   h264_vulkan  Vulkan video encode needs Turing or newer. This is a GP106,
#                and the instance will not even create against this driver.
#
# What the box does have is NVENC, and the Alpine build has no nvenc support
# compiled in at all, so no runtime configuration could ever reach it. Debian's
# ffmpeg does, and it is built against headers this driver satisfies — the
# host's own newer ffmpeg is not, and refuses with "Driver does not support the
# required nvenc API version. Required: 13.1 Found: 13.0". That version pairing
# is the whole reason the base moved rather than the encoder being switched on.
#
# The MediaMTX binary is a static Go build and is copied straight across; only
# the userland around it changes.
FROM bluenviron/mediamtx:latest-ffmpeg AS mediamtx

FROM debian:stable-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
      bash curl ca-certificates ffmpeg jq \
    && rm -rf /var/lib/apt/lists/*

COPY --from=mediamtx /mediamtx /mediamtx

COPY scripts/ /scripts/
RUN chmod +x /scripts/*.sh

# Reaching the GPU also needs the driver's own libraries inside the container,
# which the NVIDIA container runtime injects when these are set. With no such
# runtime configured they are inert, the ladder's session probe fails, and it
# reports to the control plane that this box has no hardware encoder — which is
# then true of every plan the control plane issues. Nothing here asserts a
# capability; the probe establishes one.
ENV NVIDIA_VISIBLE_DEVICES=all \
    NVIDIA_DRIVER_CAPABILITIES=video,compute,utility

# ingest: 1935 RTMP · 8890/udp SRT · 8889 WebRTC-WHIP · 8189/udp WebRTC media
# internal: 8554 RTSP (ladder pull) · 9997 control API · 9998 metrics
EXPOSE 1935 8554 8889 8890/udp 8189/udp 9997 9998

ENTRYPOINT ["/mediamtx"]
CMD ["/mediamtx.yml"]
