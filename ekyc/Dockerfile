# Base pinned by tag and digest: the exact image this service was verified on
# (Python 3.14.10, Debian 13 trixie). Python 3.14 is the newest release
# onnxruntime ships wheels for.
FROM python:3.14.10-trixie@sha256:8a6b0a0ed63fb9d4ea59ee320591109f14205bbe7a787c51d9f72ad2a2f7e89c

# Tesseract reads the document front and (through PassportEye) the MRZ.
# Every Python package installs from a prebuilt wheel, so no compiler is needed.
RUN apt-get update && apt-get install -y --no-install-recommends \
    tesseract-ocr \
    tesseract-ocr-eng \
    libglib2.0-0 \
    curl \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir --root-user-action=ignore -r requirements.txt

# ── Models, baked in at build time and checksum-verified ─────────────────────
# Every model is licensed for commercial use.
# YuNet 2026may (MIT) and SFace 2021dec (Apache-2.0): OpenCV Zoo, pinned to
#   repository commit 48b10fa505aeb51fbd127fc60a5c660040e823b7.
# MiniFASNetV2 / MiniFASNetV1SE: Silent-Face-Anti-Spoofing weights (Minivision,
#   Apache-2.0), ONNX export from yakhyo/face-anti-spoofing release "weights"
#   (Apache-2.0, repository commit 01a38903f4f2680bb59d32a994a6c7b5ae56f49b).
ENV EKYC_MODEL_DIR=/opt/ekyc-models
RUN set -eu; \
    mkdir -p /opt/ekyc-models/face /opt/ekyc-models/pad; \
    zoo=https://media.githubusercontent.com/media/opencv/opencv_zoo/48b10fa505aeb51fbd127fc60a5c660040e823b7/models; \
    curl -fsSL -o /opt/ekyc-models/face/face_detection_yunet_2026may.onnx \
      "$zoo/face_detection_yunet/face_detection_yunet_2026may.onnx"; \
    curl -fsSL -o /opt/ekyc-models/face/face_recognition_sface_2021dec.onnx \
      "$zoo/face_recognition_sface/face_recognition_sface_2021dec.onnx"; \
    curl -fsSL -o /opt/ekyc-models/pad/MiniFASNetV2.onnx \
      https://github.com/yakhyo/face-anti-spoofing/releases/download/weights/MiniFASNetV2.onnx; \
    curl -fsSL -o /opt/ekyc-models/pad/MiniFASNetV1SE.onnx \
      https://github.com/yakhyo/face-anti-spoofing/releases/download/weights/MiniFASNetV1SE.onnx; \
    printf '%s\n' \
      "ebafce4e3c118d6554634be5c27ab333b4c047a9a8c3faf1d7cf93101c22f0f0  /opt/ekyc-models/face/face_detection_yunet_2026may.onnx" \
      "0ba9fbfa01b5270c96627c4ef784da859931e02f04419c829e83484087c34e79  /opt/ekyc-models/face/face_recognition_sface_2021dec.onnx" \
      "b32929adc2d9c34b9486f8c4c7bc97c1b69bc0ea9befefc380e4faae4e463907  /opt/ekyc-models/pad/MiniFASNetV2.onnx" \
      "ebab7f90c7833fbccd46d3a555410e78d969db5438e169b6524be444862b3676  /opt/ekyc-models/pad/MiniFASNetV1SE.onnx" \
      | sha256sum -c -

COPY mrz.py aamva.py frontocr.py faces.py liveness.py server.py wsgi.py gunicorn.conf.py ./
EXPOSE 8099
CMD ["gunicorn", "--config", "gunicorn.conf.py", "wsgi:app"]
