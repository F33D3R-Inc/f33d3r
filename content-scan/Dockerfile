FROM python:latest

# ffmpeg + chromaprint (fpcalc) for video probing, audio extraction, and fingerprinting.
# libgl1 + libglib2.0-0 required by OpenCV/NudeNet ONNX runtime.
RUN apt-get update && apt-get install -y --no-install-recommends \
    ffmpeg \
    libchromaprint-tools \
    libgl1 \
    libglib2.0-0 \
    curl \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Data directory for SQLite fingerprint DB
RUN mkdir -p /data

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

# Pre-warm NudeNet model — avoids slow first-request download.
RUN python -c "\
from nudenet import NudeDetector; \
d = NudeDetector(); \
print('NudeNet model ready')" || echo "NudeNet pre-download skipped"

# Pre-warm EasyOCR model (~300MB) — English by default; downloads on first import.
# Falls back if network unavailable (model fetched on first OCR request instead).
RUN python -c "\
import easyocr; \
r = easyocr.Reader(['en'], gpu=False); \
print('EasyOCR model ready')" || echo "EasyOCR pre-download skipped"

# Pre-warm faster-whisper base model (~150MB, int8 quantized).
# Multilingual; used for audio transcript-based content moderation.
# Falls back if network unavailable (model fetched on first transcription request).
RUN python -c "\
from faster_whisper import WhisperModel; \
m = WhisperModel('base', device='cpu', compute_type='int8'); \
print('faster-whisper base model ready')" || echo "faster-whisper pre-download skipped"

COPY scanner.py .
COPY server.py .
COPY sweep.py .

EXPOSE 8097
CMD ["python", "server.py"]
