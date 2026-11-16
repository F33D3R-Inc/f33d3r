# F33D3R content-scan — the private ML worker of abraxas-shield: fingerprints
# and detector scores over files on the read-only media private volume. No
# database, no callbacks, no decisions.
#
# Python is pinned to 3.11 rather than :latest because every model stack in this
# image (onnxruntime, torch/torchvision behind EasyOCR, ctranslate2 behind
# faster-whisper) publishes wheels for it. On an unpinned :latest the
# interpreter moves ahead of those wheels and pip silently falls back to source
# builds that do not complete — a build that used to work stops working with no
# change to this file.
#
# The image is large (torch and its CUDA libraries dominate). That is the cost
# of a real classifier; the models are baked in so a cold start is a disk load,
# not a download.
FROM python:3.11-slim-bookworm

# ffmpeg + chromaprint (fpcalc): probing, frame sampling, shot detection, the
# 8 kHz decode behind the audio landmarks, and the full-length chromaprint.
# libgl1 + libglib2.0-0 are required by OpenCV behind NudeNet and EasyOCR.
# libgomp1 is the OpenMP runtime torch and ctranslate2 link against. curl serves
# the healthcheck.
RUN apt-get update && apt-get install -y --no-install-recommends \
    ffmpeg \
    libchromaprint-tools \
    libgl1 \
    libglib2.0-0 \
    libgomp1 \
    curl \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Model weights live under /models.
# Pointing every library's cache at a fixed path is what makes the pre-warm
# below land inside the image instead of in a throwaway home directory.
ENV EASYOCR_MODULE_PATH=/models/easyocr \
    HF_HOME=/models/hf \
    XDG_CACHE_HOME=/models/cache \
    PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1

RUN mkdir -p /models/easyocr /models/hf /models/cache

# ONNX Runtime build. The default CPU package has no CUDAExecutionProvider; to
# run NudeNet on a GPU, build with --build-arg ONNXRUNTIME_PACKAGE=onnxruntime-gpu.
# With the default, CONTENT_SCAN_DEVICE=cuda still accelerates EasyOCR and
# faster-whisper and logs a warning that NudeNet stayed on CPU.
ARG ONNXRUNTIME_PACKAGE=onnxruntime

# easyocr pulls torch, and PyPI's default wheel bundles CUDA — several GB that
# CONTENT_SCAN_DEVICE=cpu never executes. Install the CPU build first so pip
# resolves easyocr against it. For GPU, build with
# --build-arg TORCH_INDEX_URL=https://pypi.org/simple and ONNXRUNTIME_PACKAGE=onnxruntime-gpu.
ARG TORCH_INDEX_URL=https://download.pytorch.org/whl/cpu

COPY requirements.txt .
# Large wheels exceed pip's default 15s socket timeout on a slow link.
RUN pip install --no-cache-dir --timeout 120 --retries 10 \
      --index-url "${TORCH_INDEX_URL}" --extra-index-url https://pypi.org/simple torch torchvision \
 && pip install --no-cache-dir --timeout 120 --retries 10 -r requirements.txt \
 && pip install --no-cache-dir --timeout 120 --retries 10 "${ONNXRUNTIME_PACKAGE}"

COPY errors.py media.py scanner.py audio_landmarks.py fingerprint.py server.py prewarm.py ./

# Bake NudeNet, EasyOCR (~300 MB) and faster-whisper base (~150 MB) into the
# image. Three attempts absorb a flaky model host; after that the build FAILS.
# It does not fall through to a runtime download: an image that cannot classify
# must never be produced, because a classifier that is deployed but silent is
# indistinguishable from no classifier at all.
RUN set -eu; \
    ok=0; \
    for attempt in 1 2 3; do \
      if python /app/prewarm.py; then ok=1; break; fi; \
      echo "content-scan: model pre-warm attempt ${attempt} failed; retrying in 20s"; \
      sleep 20; \
    done; \
    if [ "$ok" != "1" ]; then \
      echo "content-scan: model pre-warm failed after 3 attempts — refusing to build an image that cannot scan"; \
      exit 1; \
    fi

EXPOSE 8097

# gunicorn, not the Werkzeug development server. One worker: each worker holds a
# full copy of NudeNet, EasyOCR and Whisper in memory, so a second worker doubles
# the resident set for no throughput gain. Threads carry the concurrency —
# ONNX Runtime, torch, ctranslate2 and numpy's FFT release the GIL. With the
# gthread worker the timeout is a liveness check on the worker process, not a
# limit on one request: a deep pass over a long file runs as long as its own
# per-stage deadlines (media.budget) allow, and abraxas's client timeout is the
# outer bound.
# exec keeps gunicorn as PID 1 so it receives SIGTERM directly; sh -c is only
# there so the bind address follows PORT instead of hardcoding it twice.
CMD ["sh", "-c", "exec gunicorn --bind 0.0.0.0:${PORT:-8097} --workers 1 --threads 8 --timeout 600 --graceful-timeout 30 --access-logfile - --error-logfile - server:app"]
