# syntax=docker/dockerfile:1
# ── Stage 1: build ─────────────────────────────────────────────────────────────
FROM rust:latest AS builder

WORKDIR /app

# Pre-build deps — cached unless Cargo.toml / Cargo.lock changes
COPY Cargo.toml Cargo.lock* ./
RUN mkdir src && echo "fn main(){}" > src/main.rs
RUN --mount=type=cache,id=cargo-registry,target=/usr/local/cargo/registry \
    --mount=type=cache,id=transcoding-target,target=/app/target \
    cargo build --release || true

# Build source — only recompiles changed .rs files
COPY src ./src
RUN --mount=type=cache,id=cargo-registry,target=/usr/local/cargo/registry \
    --mount=type=cache,id=transcoding-target,target=/app/target \
    touch src/main.rs && cargo build --release && \
    cp target/release/transcoding /usr/local/bin/transcoding

# ── Stage 2: runtime with ffmpeg ──────────────────────────────────────────────
FROM debian:latest

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl ffmpeg fonts-dejavu-core \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /usr/local/bin/transcoding /usr/local/bin/transcoding

EXPOSE 8085
ENV RUST_LOG=info

HEALTHCHECK --interval=10s --timeout=3s --retries=3 \
    CMD curl -f http://localhost:8085/health || exit 1

CMD ["transcoding"]
