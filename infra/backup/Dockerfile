FROM alpine:latest
RUN apk add --no-cache postgresql-client gzip curl bash && \
    curl -sSL https://dl.min.io/client/mc/release/linux-amd64/mc -o /usr/local/bin/mc && \
    chmod +x /usr/local/bin/mc
COPY backup.sh /usr/local/bin/backup.sh
COPY restore-test.sh /usr/local/bin/restore-test.sh
RUN chmod +x /usr/local/bin/backup.sh /usr/local/bin/restore-test.sh
CMD ["/usr/local/bin/backup.sh"]
