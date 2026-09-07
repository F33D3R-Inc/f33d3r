# Build context is the REPO ROOT (see docker-compose.local.yml / docker-compose.prod.yml). The COPY
# paths below are written relative to that root. A tight root .dockerignore keeps the context small.

# ── Stage 1: build the Go binary ─────────────────────────────────────────────────
# D-026: never pin Go version. golang:alpine == latest stable on alpine base.
FROM golang:alpine AS builder

# git is required by `go mod tidy` to fetch modules from VCS.
RUN apk add --no-cache git ca-certificates

WORKDIR /app

# Pre-fetch deps using go.sum as the cache key — only re-downloads on go.mod change.
COPY feed-engine/go.mod feed-engine/go.sum ./
RUN go mod download

# Copy source and build. No tidy needed — go.sum is authoritative.
COPY feed-engine/ .

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o feed-engine ./cmd/server

# ── Stage 3: minimal runtime ───────────────────────────────────────────────────
# D-026: alpine:latest, not pinned.
FROM alpine:latest

RUN apk add --no-cache ca-certificates tzdata wget

WORKDIR /app
COPY --from=builder /app/feed-engine .
COPY --from=builder /app/web ./web

EXPOSE 8081
ENV PORT=8081
ENV HOST=0.0.0.0

HEALTHCHECK --interval=10s --timeout=3s --retries=3 \
    CMD wget -qO- http://localhost:8081/api/health || exit 1

CMD ["./feed-engine"]
