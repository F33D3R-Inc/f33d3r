.PHONY: run run-dev build tidy docker docker-up clean test

## Run locally (requires Go 1.22+)
run:
	AETHYRRANK_URL=http://localhost:8080 \
	SHOW_SCORES=false \
	go run ./cmd/server

## Run with score overlay visible
run-dev:
	AETHYRRANK_URL=http://localhost:8080 \
	SHOW_SCORES=true \
	DEV_MODE=true \
	go run ./cmd/server

## Build binary
build:
	go build -o feed-engine ./cmd/server

## Refresh go.sum after dependency changes (uses Docker — no local Go needed).
## Run this once after pulling Sprint 0 / S0.2.
tidy:
	docker run --rm -v "$$(pwd)":/app -w /app golang:1.26.2-alpine \
		sh -c "go mod tidy && go mod download"

## Run unit tests (uses Docker if no local Go).
test:
	@if command -v go >/dev/null 2>&1; then \
		go test ./... ; \
	else \
		docker run --rm -v "$$(pwd)":/app -w /app golang:latest go test ./... ; \
	fi

## Compile-check every package without producing binaries (fast).
check:
	@if command -v go >/dev/null 2>&1; then \
		go build ./... ; \
	else \
		docker run --rm -v "$$(pwd)":/app -w /app golang:latest go build ./... ; \
	fi

## Build Docker image
docker:
	docker build -t feed-engine:latest .

## Start both feed-engine + aethyrrank-engine via docker-compose
docker-up:
	docker-compose up --build

## Clean build artifacts
clean:
	rm -f feed-engine
