# syntax=docker/dockerfile:1
#
# yt-fifo: YouTube audio as raw PCM for Snapcast.
#
# Two stages. The Go build produces a static binary; the runtime is alpine:edge
# because yt-dlp-ejs (yt-dlp's YouTube challenge-solver scripts) exists only
# there. The image is also consumed by ghcr.io/jagjordi/home-audio, which copies
# /usr/local/bin/yt-fifo and /etc/yt-dlp.conf out of it, so keep those paths.

FROM golang:1.27-alpine AS build

ARG VERSION=dev
WORKDIR /src
COPY go.mod main.go ./
COPY internal ./internal
# CGO off: fully static, so it also runs unchanged in the home-audio image.
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/yt-fifo .

FROM alpine:edge

LABEL org.opencontainers.image.title="yt-fifo" \
      org.opencontainers.image.description="YouTube audio as raw PCM for Snapcast, with a queue and HTTP API" \
      org.opencontainers.image.source="https://github.com/jagjordi/yt-fifo" \
      org.opencontainers.image.licenses="MIT"

# yt-dlp-core + yt-dlp-ejs instead of the yt-dlp metapackage, which also drags
# in a second ffmpeg dependency chain and deno. yt-dlp-ejs (the YouTube
# challenge-solver scripts) needs a JavaScript runtime and offers one
# subpackage per runtime; name the quickjs one (2 MB) explicitly, otherwise
# apk's default pick for the virtual is deno (106 MB). yt-dlp.conf then tells
# yt-dlp to use it. ffmpeg decodes whatever yt-dlp picks (opus/webm, m4a, HLS)
# to s16le.
RUN set -eux; \
    apk add --no-cache \
        ffmpeg \
        yt-dlp-core yt-dlp-ejs yt-dlp-ejs-rt-quickjs \
        ca-certificates tzdata; \
    if apk info -e deno nodejs >/dev/null; then echo "unexpected JS runtime installed" >&2; exit 1; fi; \
    mkdir -p /pipe

COPY yt-dlp.conf /etc/yt-dlp.conf
COPY --from=build /out/yt-fifo /usr/local/bin/yt-fifo

# Defaults for the standalone container. PORT is what the healthcheck probes
# and what the binary listens on when YT_FIFO_LISTEN is unset. The output
# defaults to a FIFO under /pipe so a snapserver reading pipe:///.../ytaudio
# from the same host directory picks it up; set YT_FIFO_OUTPUT=tcp://host:port
# to feed a snapserver tcp:// source instead.
ENV PORT=8080 \
    YT_FIFO_OUTPUT=/pipe/ytaudio

EXPOSE 8080
VOLUME ["/pipe"]

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -q -O /dev/null "http://127.0.0.1:${PORT}/health" || exit 1

ENTRYPOINT ["/usr/local/bin/yt-fifo"]
CMD ["serve"]
