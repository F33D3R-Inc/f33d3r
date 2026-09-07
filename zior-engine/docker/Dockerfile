FROM rust:latest AS builder
RUN apt-get update && apt-get install -y --no-install-recommends pkg-config libssl-dev && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY Cargo.toml Cargo.lock* ./
RUN mkdir src && echo "fn main(){}" > src/main.rs && echo "" > src/lib.rs
RUN cargo build --release 2>&1 | tail -3
COPY src    ./src
COPY config ./config
RUN touch src/main.rs src/lib.rs && cargo build --release

FROM debian:latest
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=builder /app/target/release/zior-engine .
COPY --from=builder /app/config ./config
EXPOSE 8082
ENV RUST_LOG=info
HEALTHCHECK --interval=10s --timeout=3s CMD curl -f http://localhost:8082/health || exit 1
CMD ["./zior-engine"]
