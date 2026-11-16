# syntax=docker/dockerfile:1
# Build context is the REPO ROOT (`context: .` + `dockerfile: caeor/docker/Dockerfile`
# in both compose files): caeor depends on libs/manhattan-client and
# libs/manhattan-outbox by path, and a build cannot reach outside its context.
# The root .dockerignore whitelists `libs/` and `caeor/`.

# ── Stage 1: build ─────────────────────────────────────────────────────────────
FROM rust:latest AS builder

RUN apt-get update && apt-get install -y --no-install-recommends \
    pkg-config libssl-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
ENV CARGO_TARGET_DIR=/app/target

# Shared crates first: they change rarely and sit under the dependency cache.
COPY libs/manhattan-client ./libs/manhattan-client
COPY libs/manhattan-outbox ./libs/manhattan-outbox

# Manifest only, with a stub main, so dependencies compile into a cached layer.
COPY caeor/Cargo.toml caeor/Cargo.lock ./caeor/
RUN mkdir caeor/src && echo 'fn main(){}' > caeor/src/main.rs
RUN --mount=type=cache,id=cargo-registry,target=/usr/local/cargo/registry \
    --mount=type=cache,id=caeor-target,target=/app/target \
    cargo build --release --manifest-path caeor/Cargo.toml

# Migrations are embedded with include_str!, so they belong to the source build.
COPY caeor/migrations ./caeor/migrations
COPY caeor/src ./caeor/src
RUN --mount=type=cache,id=cargo-registry,target=/usr/local/cargo/registry \
    --mount=type=cache,id=caeor-target,target=/app/target \
    touch caeor/src/main.rs && cargo build --release --manifest-path caeor/Cargo.toml && \
    cp /app/target/release/caeor /usr/local/bin/caeor

# ── Stage 2: runtime ───────────────────────────────────────────────────────────
# ffmpeg and the DejaVu fonts are the same packages transcoding/Dockerfile
# installs: caeor draws the image watermark and the per-viewer forensic layer
# with the same renderer and font transcoding uses for video, and strips audio
# metadata with it.
FROM debian:latest

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl ffmpeg fonts-dejavu-core \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /usr/local/bin/caeor /usr/local/bin/caeor

VOLUME /data/media
VOLUME /data/media-private
EXPOSE 8086
ENV RUST_LOG=info

HEALTHCHECK --interval=10s --timeout=3s --retries=3 \
    CMD curl -f http://localhost:8086/health || exit 1

CMD ["caeor"]
