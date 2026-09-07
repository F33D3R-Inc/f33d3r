# Build context is the REPO ROOT (see docker-compose.local.yml / docker-compose.prod.yml:
# `context: .` + `dockerfile: themis/Dockerfile`). Every COPY below is written relative to
# that root, because this brain depends on the shared `libs/manhattan-client`
# crate by path and a Docker build cannot reach outside its context. The root
# .dockerignore whitelists `libs/` and each brain's own tree so the context stays small.

# ── Stage 1: build ─────────────────────────────────────────────────────────────
FROM rust:latest AS builder

RUN apt-get update && apt-get install -y --no-install-recommends pkg-config libssl-dev && rm -rf /var/lib/apt/lists/*

WORKDIR /app
# One target directory for both cargo invocations, so the dependency build
# below is what the source build reuses, and stage 2 has one path to copy from.
ENV CARGO_TARGET_DIR=/app/target

# Shared crates first: they change rarely and sit under the dependency cache.
COPY libs/manhattan-client ./libs/manhattan-client
COPY libs/manhattan-outbox ./libs/manhattan-outbox

# Manifest only, with a stub main, so dependencies compile into a cached layer
# that survives source edits.
COPY themis/Cargo.toml themis/Cargo.lock* ./themis/
RUN mkdir themis/src && echo 'fn main(){}' > themis/src/main.rs
RUN cargo build --release --manifest-path themis/Cargo.toml

COPY themis/src ./themis/src
RUN touch themis/src/main.rs && cargo build --release --manifest-path themis/Cargo.toml

# ── Stage 2: minimal runtime ───────────────────────────────────────────────────
FROM debian:latest

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY --from=builder /app/target/release/themis .

EXPOSE 8100
ENV RUST_LOG=info

HEALTHCHECK --interval=10s --timeout=3s --retries=3 \
    CMD curl -f http://localhost:8100/health || exit 1

CMD ["./themis"]
