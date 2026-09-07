# syntax=docker/dockerfile:1
# ── Stage 1: build ─────────────────────────────────────────────────────────────
FROM rust:latest AS builder

RUN apt-get update && apt-get install -y --no-install-recommends \
    pkg-config libssl-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Pre-build deps — cached unless Cargo.toml / Cargo.lock changes
COPY Cargo.toml Cargo.lock* ./
RUN mkdir src && echo "fn main(){}" > src/main.rs
RUN --mount=type=cache,id=cargo-registry,target=/usr/local/cargo/registry \
    --mount=type=cache,id=caeor-target,target=/app/target \
    cargo build --release || true

# Build source — only recompiles changed .rs files
COPY src ./src
RUN --mount=type=cache,id=cargo-registry,target=/usr/local/cargo/registry \
    --mount=type=cache,id=caeor-target,target=/app/target \
    touch src/main.rs && cargo build --release && \
    cp target/release/caeor /usr/local/bin/caeor

# ── Stage 2: minimal runtime (no ffmpeg — image-only brain) ───────────────────
FROM debian:latest

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /usr/local/bin/caeor /usr/local/bin/caeor

VOLUME /data/media
EXPOSE 8086
ENV RUST_LOG=info

HEALTHCHECK --interval=10s --timeout=3s --retries=3 \
    CMD curl -f http://localhost:8086/health || exit 1

CMD ["caeor"]
