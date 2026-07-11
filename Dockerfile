# syntax=docker/dockerfile:1

# --- build stage -------------------------------------------------------------
# CGO is disabled (modernc.org/sqlite is pure Go), so the binary is fully
# static and needs no C toolchain or libc at runtime.
FROM golang:1.26-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

ARG VERSION=docker
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/hrg ./cmd/hrg

# --- runtime stage -----------------------------------------------------------
# Alpine + Chromium: the whole reason for a real base image instead of
# scratch is PDF export, which shells out to headless Chromium. HTML and
# Markdown exports work without it, so if you don't need PDF you could slim
# this down further.
FROM alpine:3.20

RUN apk add --no-cache \
      chromium \
      nss freetype harfbuzz ttf-freefont \
      ca-certificates tzdata

COPY --from=build /out/hrg /usr/local/bin/hrg

# Persistent state (SQLite DB, encryption key, generated exports) lives under
# /data; the manual resources.d is mounted separately, read-only by default.
WORKDIR /data
VOLUME ["/data"]
EXPOSE 8080

# Report healthy only when the app actually serves, not just that the port
# is open. /healthz needs no auth.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1

# Bind 0.0.0.0 inside the container; publish it to 127.0.0.1 on the host (see
# docker-compose.yml) to keep the network map off the LAN unless you mean to.
ENTRYPOINT ["hrg"]
CMD ["-db", "/data/hrg.db", \
     "-key", "/data/hrg.key", \
     "-resources", "/resources.d", \
     "-addr", "0.0.0.0:8080", \
     "serve"]
