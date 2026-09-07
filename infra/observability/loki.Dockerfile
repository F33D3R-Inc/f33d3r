FROM grafana/loki:latest AS loki-binary
FROM alpine:latest
# Loki is a fully static Go binary — no libc deps, runs on any base
COPY --from=loki-binary /usr/bin/loki /usr/bin/loki
RUN addgroup -S -g 10001 loki && \
    adduser -S -u 10001 -G loki loki && \
    mkdir -p /loki /etc/loki && \
    chown -R loki:loki /loki
USER 10001
EXPOSE 3100 9096
ENTRYPOINT ["/usr/bin/loki"]
