# ── Stage 1: build ────────────────────────────────────────────────────────────
FROM rust:latest AS builder

# FEATURES build arg controls optional Cargo features at build time.
# Default (empty) = text-only NSFW, no ONNX runtime, runs on any CPU.
# Pass --build-arg FEATURES=visual-nsfw on AVX2-capable hosts (Hetzner CCX33+)
# to enable full ONNX-based visual NSFW scanning.
ARG FEATURES=""

RUN apt-get update && apt-get install -y --no-install-recommends \
    pkg-config libssl-dev cmake libsasl2-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Cache dependency compilation separately from source
COPY Cargo.toml Cargo.lock* ./
RUN mkdir src && echo "fn main(){}" > src/main.rs
RUN if [ -n "$FEATURES" ]; then cargo build --release --features "$FEATURES"; else cargo build --release; fi

# Build the real binary
COPY src ./src
RUN touch src/main.rs && \
    if [ -n "$FEATURES" ]; then cargo build --release --features "$FEATURES"; else cargo build --release; fi

# Locate and stage the ONNX Runtime shared library that ort's download-binaries
# feature fetched into OUT_DIR during build. The .so lives under the ort build
# output directory — use find to handle any version in the path.
RUN find /usr/local/cargo/registry -name "libonnxruntime*.so*" -exec cp {} /app/ \; 2>/dev/null || true
RUN find /app/target -name "libonnxruntime*.so*" -exec cp {} /app/ \; 2>/dev/null || true

# ── Stage 2: minimal runtime ───────────────────────────────────────────────────
FROM debian:latest

RUN apt-get update \
    && apt-get install -y --no-install-recommends libssl3 libsasl2-2 ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY --from=builder /app/target/release/abraxas-shield .

# Copy the ONNX Runtime shared library if present (absent in text-only deploys).
COPY --from=builder /app/libonnxruntime*.so* /usr/local/lib/
RUN ldconfig

ENV RUST_LOG=info

CMD ["./abraxas-shield"]
