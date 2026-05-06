# ── Stage 1: build ─────────────────────────────────────────────────────────────
FROM rust:slim AS builder

RUN apt-get update && apt-get install -y --no-install-recommends \
    pkg-config \
    libssl-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Cache deps
COPY Cargo.toml Cargo.lock* ./
RUN mkdir src && echo "fn main(){}" > src/main.rs
RUN cargo build --release || true

COPY src ./src
RUN touch src/main.rs && cargo build --release

# ── Stage 2: runtime with ffmpeg ───────────────────────────────────────────────
# ffmpeg + ffprobe are required for the HLS ladder transcoder. The static
# bookworm packages are large (~150 MB) but adequate for T0–T1; switch to
# a stripped-down ffmpeg build when image size matters at T2+.
FROM debian:latest

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        ffmpeg \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY --from=builder /app/target/release/caeor .

VOLUME /data/media
EXPOSE 8086
ENV RUST_LOG=info

HEALTHCHECK --interval=10s --timeout=3s --retries=3 \
    CMD curl -f http://localhost:8086/health || exit 1

CMD ["./caeor"]
