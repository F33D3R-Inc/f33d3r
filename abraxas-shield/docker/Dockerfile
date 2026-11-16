# Build context is the REPO ROOT (`context: .` + `dockerfile: abraxas-shield/docker/Dockerfile`
# in both compose files): Abraxas depends on libs/manhattan-client and
# libs/manhattan-outbox by path, and a build cannot reach outside its context.
# The root .dockerignore whitelists `libs/` and `abraxas-shield/`.

# ── Stage 1: build ─────────────────────────────────────────────────────────────
FROM rust:latest AS builder

# cmake builds librdkafka (rdkafka's cmake-build feature).
RUN apt-get update && apt-get install -y --no-install-recommends \
    pkg-config libssl-dev cmake \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
ENV CARGO_TARGET_DIR=/app/target

# Shared crates first: they change rarely and sit under the dependency cache.
COPY libs/manhattan-client ./libs/manhattan-client
COPY libs/manhattan-outbox ./libs/manhattan-outbox

# Manifest only, with a stub main, so dependencies compile into a cached layer.
COPY abraxas-shield/Cargo.toml abraxas-shield/Cargo.lock ./abraxas-shield/
RUN mkdir abraxas-shield/src && echo 'fn main(){}' > abraxas-shield/src/main.rs
RUN --mount=type=cache,id=cargo-registry,target=/usr/local/cargo/registry \
    --mount=type=cache,id=abraxas-target,target=/app/target \
    cargo build --release --locked --manifest-path abraxas-shield/Cargo.toml

# Migrations are embedded with include_str!, so they belong to the source build.
COPY abraxas-shield/migrations ./abraxas-shield/migrations
COPY abraxas-shield/src ./abraxas-shield/src
RUN --mount=type=cache,id=cargo-registry,target=/usr/local/cargo/registry \
    --mount=type=cache,id=abraxas-target,target=/app/target \
    touch abraxas-shield/src/main.rs && cargo build --release --locked --manifest-path abraxas-shield/Cargo.toml && \
    cp /app/target/release/abraxas-shield /usr/local/bin/abraxas-shield

# ── Stage 2: runtime ───────────────────────────────────────────────────────────
FROM debian:latest

RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /usr/local/bin/abraxas-shield /usr/local/bin/abraxas-shield

EXPOSE 8109
ENV RUST_LOG=info

HEALTHCHECK --interval=10s --timeout=3s --retries=3 \
    CMD curl -f http://localhost:8109/health || exit 1

CMD ["abraxas-shield"]
