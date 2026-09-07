FROM golang:latest AS builder
WORKDIR /app
COPY . .
RUN go build -o help-center .

FROM debian:stable-slim
RUN apt-get update && apt-get install -y --no-install-recommends wget && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=builder /app/help-center .
COPY templates/ templates/
# docs/ is mounted as a volume — do NOT copy it into the image
EXPOSE 8080
CMD ["./help-center"]
