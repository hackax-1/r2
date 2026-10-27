# Multi-stage build for Free CDN Live Relay
FROM golang:1.26-alpine AS builder

WORKDIR /app

# Dependencies
COPY go.mod ./
RUN go mod download

# Build statically linked binary
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o cdn-relay .

# Production runtime container
FROM alpine:3.20

RUN apk add --no-cache ffmpeg ca-certificates tzdata

WORKDIR /app
COPY --from=builder /app/cdn-relay /app/cdn-relay
COPY config.json.example /app/config.json.example

EXPOSE 8888

ENTRYPOINT ["/app/cdn-relay"]
CMD ["-port=8888", "-autostart=false"]
