# syntax=docker/dockerfile:1

# ---- build: static Go binary ------------------------------------------------
FROM golang:1.26-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/yt-converter ./cmd/server

# static ffmpeg/ffprobe (multi-arch); Debian's ffmpeg package drags in ~450MB
# of X11/SDL/pulse libraries we never use
FROM mwader/static-ffmpeg:7.1.1 AS ffmpeg

# yt-dlp needs node >= 22 for YouTube's JS challenges; Debian ships 20, which
# yt-dlp reports as "unsupported" (the old node:20 image had the same problem)
FROM node:22-trixie-slim AS node

# ---- runtime ------------------------------------------------------------------
FROM debian:trixie-slim

# yt-dlp is still the only reliable YouTube extractor, so it lives in an
# isolated venv. curl_cffi enables --impersonate.
ARG YTDLP_VERSION=2026.10.26
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates libstdc++6 python3 python3-venv tini \
 && python3 -m venv /opt/yt \
 && /opt/yt/bin/pip install --no-cache-dir --upgrade pip \
 && /opt/yt/bin/pip install --no-cache-dir "yt-dlp[default]==${YTDLP_VERSION}" curl_cffi \
 && apt-get clean && rm -rf /var/lib/apt/lists/* /root/.cache \
 && useradd --system --uid 10001 --user-group --home-dir /app --shell /usr/sbin/nologin app \
 && mkdir -p /app/data /app/cache /app/cookies \
 && chown -R app:app /app

COPY --from=ffmpeg /ffmpeg /ffprobe /usr/local/bin/
COPY --from=node /usr/local/bin/node /usr/local/bin/node
COPY --from=build /out/yt-converter /usr/local/bin/yt-converter
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh

ENV PATH=/opt/yt/bin:$PATH \
    PORT=9999 \
    DATA_DIR=/app/data \
    CACHE_DIR=/app/cache \
    COOKIES_FILE=/app/cookies/cookies.txt \
    HOME=/app \
    PYTHONDONTWRITEBYTECODE=1

WORKDIR /app
EXPOSE 9999
VOLUME ["/app/data", "/app/cache"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
  CMD ["yt-converter", "-healthcheck"]

# starts as root only to fix volume ownership / optionally update yt-dlp,
# then drops to the unprivileged "app" user and execs tini as PID 1
ENTRYPOINT ["docker-entrypoint.sh"]
CMD ["yt-converter"]
