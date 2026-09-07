#!/usr/bin/env bash
# ─────────────────────────────────────────────────────────────────────────────
# F33D3R Android — run the debug app on a local emulator (the Android Studio-free
# equivalent of Xcode's iPhone Simulator).
#
#   ./run-emulator.sh            build, boot the AVD, install, launch
#   ./run-emulator.sh --no-build install and launch what is already built
#
# What it assumes:
#   - An Android SDK at $ANDROID_HOME (default ~/Android/Sdk) with cmdline-tools,
#     platform-tools, emulator and the API 37 x86_64 google_apis system image.
#     Missing pieces are installed on first run.
#   - A JDK: JAVA_HOME, or the one bundled with Android Studio at /opt/android-studio.
#   - KVM (/dev/kvm readable) for hardware acceleration.
#   - A local nantar on :8081, which the debug build reaches at http://10.0.2.2:8081.
#     (docker compose -f docker-compose.local.yml up -d feed-engine)
# ─────────────────────────────────────────────────────────────────────────────
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export ANDROID_HOME="${ANDROID_HOME:-$HOME/Android/Sdk}"
export ANDROID_SDK_ROOT="$ANDROID_HOME"
export JAVA_HOME="${JAVA_HOME:-/opt/android-studio/jbr}"
export PATH="$ANDROID_HOME/platform-tools:$ANDROID_HOME/emulator:$ANDROID_HOME/cmdline-tools/latest/bin:$PATH"

AVD="${AVD:-f33d3r_pixel8_api37}"
IMAGE="system-images;android-37.0;google_apis;x86_64"
DEVICE="pixel_8"
APK="$HERE/app/build/outputs/apk/debug/app-debug.apk"
PKG="com.f33d3r.app.debug"

info() { printf '\033[0;32m[f33d3r-android]\033[0m %s\n' "$*"; }

# ── SDK pieces ───────────────────────────────────────────────────────────────
if ! command -v sdkmanager >/dev/null; then
  echo "cmdline-tools missing at $ANDROID_HOME/cmdline-tools/latest — install them from" >&2
  echo "https://developer.android.com/studio#command-line-tools-only and re-run." >&2
  exit 1
fi
if [ ! -f "$ANDROID_HOME/system-images/android-37.0/google_apis/x86_64/system.img" ]; then
  info "installing emulator, platform-tools and the API 37 system image (large download)"
  yes | sdkmanager --licenses >/dev/null
  sdkmanager "emulator" "platform-tools" "$IMAGE"
fi

# ── The virtual phone ────────────────────────────────────────────────────────
if ! avdmanager list avd 2>/dev/null | grep -q "Name: $AVD\$"; then
  info "creating AVD $AVD ($DEVICE, API 37)"
  echo no | avdmanager create avd -n "$AVD" -k "$IMAGE" -d "$DEVICE" >/dev/null
  CFG="$HOME/.android/avd/$AVD.avd/config.ini"
  set_kv() { grep -q "^$1=" "$CFG" && sed -i "s|^$1=.*|$1=$2|" "$CFG" || echo "$1=$2" >> "$CFG"; }
  set_kv hw.ramSize 4096
  set_kv vm.heapSize 512
  set_kv hw.cpu.ncore 4
  set_kv hw.gpu.enabled yes
  set_kv hw.gpu.mode host
  set_kv hw.keyboard yes
  set_kv hw.camera.front emulated
  set_kv hw.camera.back emulated
  set_kv disk.dataPartition.size 8G
fi

# ── Build ────────────────────────────────────────────────────────────────────
if [ "${1:-}" != "--no-build" ]; then
  info "building debug APK"
  (cd "$HERE" && ./gradlew :app:assembleDebug --console=plain -q)
fi

# ── Boot ─────────────────────────────────────────────────────────────────────
if ! adb devices | grep -q "^emulator-.*device$"; then
  info "booting $AVD"
  nohup emulator -avd "$AVD" -gpu host -no-boot-anim >/dev/null 2>&1 &
  adb wait-for-device
  until [ "$(adb shell getprop sys.boot_completed 2>/dev/null | tr -d '\r')" = "1" ]; do sleep 2; done
fi

# ── Install and launch ───────────────────────────────────────────────────────
info "installing $APK"
adb install -r "$APK" >/dev/null
adb shell monkey -p "$PKG" -c android.intent.category.LAUNCHER 1 >/dev/null 2>&1
info "launched $PKG — sign in against http://10.0.2.2:8081 (tap the URL on the sign-in screen to change it)"
