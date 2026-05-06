# ── Stage 1: build ─────────────────────────────────────────────────────────────
# D-026: never pin Go version. golang:alpine == latest stable on alpine base.
FROM golang:alpine AS builder

# git is required by `go mod tidy` to fetch modules from VCS.
RUN apk add --no-cache git ca-certificates

WORKDIR /app

# Copy everything FIRST. We can't pre-fetch deps from a stale go.sum because
# new requires in go.mod won't have entries yet (the Sprint 0 / S0.2 zap +
# prometheus deps were the trigger). Ship a self-healing build that always
# regenerates go.sum from the source.
COPY . .

# `go mod tidy` reconciles go.mod ↔ go.sum from the actual import set; this is
# idempotent when go.sum is already correct (cheap on repeat builds) and
# self-heals when go.mod gained a new require.  Then build.
RUN go mod tidy && \
    CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o feed-engine ./cmd/server

# ── Stage 2: minimal runtime ───────────────────────────────────────────────────
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
