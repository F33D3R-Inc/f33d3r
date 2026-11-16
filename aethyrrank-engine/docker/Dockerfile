# ── Stage 1: build ────────────────────────────────────────────────────────────
FROM rust:latest AS builder

# cmake and SASL headers build rdkafka (cmake-build), as herald's Dockerfile does.
RUN apt-get update && apt-get install -y --no-install-recommends pkg-config libssl-dev cmake libsasl2-dev && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Cache dependency compilation separately from source
COPY Cargo.toml Cargo.lock* ./
RUN mkdir src && echo "fn main(){}" > src/main.rs && echo "" > src/lib.rs
RUN cargo build --release

# Build the real binary
COPY src        ./src
COPY config     ./config
# Embedded by src/migrate.rs (include_str!).
COPY migrations ./migrations
RUN touch src/main.rs src/lib.rs && cargo build --release

# ── Stage 2: minimal runtime ───────────────────────────────────────────────────
FROM debian:latest

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl libsasl2-2 \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY --from=builder /app/target/release/aethyrrank-engine .
COPY --from=builder /app/config ./config

EXPOSE 8080
ENV RUST_LOG=info

HEALTHCHECK --interval=10s --timeout=3s --retries=3 \
    CMD curl -f http://localhost:8080/health || exit 1

CMD ["./aethyrrank-engine"]
