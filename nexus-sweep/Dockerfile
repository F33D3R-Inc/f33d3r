FROM rust:latest AS builder
WORKDIR /build
COPY Cargo.toml ./
COPY src ./src
RUN cargo build --release

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=builder /build/target/release/nexus-sweep /usr/local/bin/nexus-sweep
ENV RUST_LOG=info
CMD ["nexus-sweep"]
