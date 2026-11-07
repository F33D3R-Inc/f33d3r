# MinIO server and mc client, built from the archived GitHub source.
# MinIO no longer publishes these images to Docker Hub or Quay.
FROM golang:alpine AS builder
RUN apk add --no-cache git
ARG MINIO_COMMIT=7aac2a2c5b7c882e68c1ce017d8256be2feea27f
ARG MC_COMMIT=77f82e18b5401a65958f1619df6ebb994634bd88
ENV CGO_ENABLED=0
WORKDIR /src/minio
RUN git init -q . && git remote add origin https://github.com/minio/minio.git && \
    git fetch -q --depth 1 origin "$MINIO_COMMIT" && git checkout -q FETCH_HEAD && \
    go build -tags kqueue -trimpath -ldflags "$(go run buildscripts/gen-ldflags.go)" -o /out/minio .
WORKDIR /src/mc
RUN git init -q . && git remote add origin https://github.com/minio/mc.git && \
    git fetch -q --depth 1 origin "$MC_COMMIT" && git checkout -q FETCH_HEAD && \
    go build -tags kqueue -trimpath -ldflags "$(go run buildscripts/gen-ldflags.go)" -o /out/mc .

FROM alpine:latest
RUN apk add --no-cache ca-certificates curl && \
    adduser -D -u 1000 minio && mkdir /data && chown minio:minio /data
COPY --from=builder /out/minio /out/mc /usr/local/bin/
USER minio
VOLUME /data
EXPOSE 9000 9001
ENTRYPOINT ["minio"]
