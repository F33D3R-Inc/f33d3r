# Build context is the REPO ROOT (see docker-compose.local.yml / docker-compose.prod.yml:
# `context: .` + `dockerfile: frequencies/docker/Dockerfile`). Every COPY below is written
# relative to that root, because this brain depends on the shared `libs/manhattan-client`
# crate by path and a Docker build cannot reach outside its context. The root
# .dockerignore whitelists `libs/` and each brain's own tree so the context stays small.

# ── Stage 1: build ─────────────────────────────────────────────────────────────
FROM rust:latest AS builder

# cmake + libsasl2-dev: rdkafka is built from source (`cmake-build`), the same
# way abraxas-shield builds it. No GPU, no media libraries: v1 audio is
# packet forwarding and needs neither (§40).
RUN apt-get update && apt-get install -y --no-install-recommends pkg-config libssl-dev cmake libsasl2-dev && rm -rf /var/lib/apt/lists/*

WORKDIR /app
# One target directory for both cargo invocations, so the dependency build
# below is what the source build reuses, and stage 2 has one path to copy from.
ENV CARGO_TARGET_DIR=/app/target

# Shared crates first: they change rarely and sit under the dependency cache.
COPY libs/manhattan-client ./libs/manhattan-client
COPY libs/manhattan-outbox ./libs/manhattan-outbox

# Manifest only, with a stub main, so dependencies compile into a cached layer
# that survives source edits.
COPY frequencies/Cargo.toml frequencies/Cargo.lock* ./frequencies/
RUN mkdir frequencies/src && echo 'fn main(){}' > frequencies/src/main.rs
RUN cargo build --release --manifest-path frequencies/Cargo.toml

# Migrations are embedded with include_str!, so they are part of the source
# build, not the dependency layer.
COPY frequencies/migrations ./frequencies/migrations
COPY frequencies/src ./frequencies/src
RUN touch frequencies/src/main.rs && cargo build --release --manifest-path frequencies/Cargo.toml

# ── Stage 2: minimal runtime ───────────────────────────────────────────────────
FROM debian:latest

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl libsasl2-2 \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY --from=builder /app/target/release/auralis .

EXPOSE 8108
ENV RUST_LOG=info

HEALTHCHECK --interval=10s --timeout=3s --retries=3 \
    CMD curl -f http://localhost:8108/health || exit 1

CMD ["./auralis"]
