# Build the API, then ship it with ffmpeg: local storage mode needs ffprobe for
# track discovery, ffmpeg for subtitle extraction, audio variants and HLS remuxing.
FROM golang:1.27-alpine AS build
WORKDIR /src

# Dependencies are resolved before the source is copied so a code-only change
# reuses the module layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/animind-server ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache ffmpeg ca-certificates tzdata \
 && addgroup -S animind && adduser -S animind -G animind

WORKDIR /app
COPY --from=build /out/animind-server /app/animind-server

# 3001 is the API and the watch-party socket, both on one port.
EXPOSE 3001
# 3002 is the Prometheus listener. It is unauthenticated: publish it only to a network a
# scraper can reach, never to the internet. The compose stack leaves it unmapped.
EXPOSE 3002

# The binary reads its own configuration from the environment and needs no
# writable root filesystem beyond /mnt/anime and a temp directory for HLS.
USER animind
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD wget -qO- http://127.0.0.1:3001/health || exit 1

ENTRYPOINT ["/app/animind-server"]
