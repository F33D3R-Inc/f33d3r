FROM python:latest

# Python 3.11 required: insightface/onnxruntime don't support 3.14 yet
# build-essential + cmake needed to compile insightface Cython extensions
RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential \
    cmake \
    tesseract-ocr \
    tesseract-ocr-eng \
    libgl1 \
    libglib2.0-0 \
    libsm6 \
    libxext6 \
    libopenblas-dev \
    libjpeg-dev \
    libpng-dev \
    libtiff-dev \
    curl \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY requirements.txt .
# Install numpy first — insightface build depends on it being present
RUN pip install --no-cache-dir numpy
RUN pip install --no-cache-dir -r requirements.txt

# Pre-download InsightFace buffalo_sc model so startup is instant
RUN python -c "\
from insightface.app import FaceAnalysis; \
fa = FaceAnalysis(name='buffalo_sc', providers=['CPUExecutionProvider']); \
fa.prepare(ctx_id=0, det_size=(320, 320)); \
print('InsightFace buffalo_sc ready')" || echo "Model pre-download skipped — will fetch on first request"

COPY nexus_biometric.py .
COPY mrz.py .
COPY server.py .
EXPOSE 8099
CMD ["python", "server.py"]
