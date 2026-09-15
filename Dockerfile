# syntax=docker/dockerfile:1.7
#
# Relay images (one Dockerfile, three targets):
#   docker build --target relay   -t relay .          API, UI, MCP
#   docker build --target nginx   -t relay-nginx .    nginx + relay agent (PID 1)
#   docker build --target haproxy -t relay-haproxy .  haproxy + relay agent (PID 1)

# ---------------------------------------------------------------- web UI
FROM node:22-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm npm ci --no-audit --no-fund
COPY web/ ./
# vite writes to ../internal/webui/dist (see web/vite.config.ts).
RUN mkdir -p ../internal/webui/dist && npx vite build

# ---------------------------------------------------------------- Go binary
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY --from=web /src/internal/webui/dist ./internal/webui/dist
ARG VERSION=0.1.0
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/relay ./cmd/relay

# ---------------------------------------------------------------- relay (app)
FROM alpine:3.22 AS relay
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/relay /usr/local/bin/relay
ENV RELAY_DATA_DIR=/data \
    RELAY_RUN_DIR=/run/relay \
    RELAY_LOG_DIR=/var/log/relay \
    RELAY_LISTEN=:8181
VOLUME ["/data"]
EXPOSE 8181
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD wget -q -O /dev/null "http://127.0.0.1:${RELAY_LISTEN##*:}/healthz" || exit 1
ENTRYPOINT ["relay"]
CMD ["serve"]

# ---------------------------------------------------------------- nginx engine
FROM alpine:3.22 AS nginx
# Alpine's nginx is built with http_v3, http_v2, auth_request and stub_status;
# stream and geoip2 ship as dynamic modules loaded from /etc/nginx/modules.
RUN apk add --no-cache nginx nginx-mod-stream nginx-mod-http-geoip2 ca-certificates tzdata \
 && rm -f /etc/nginx/http.d/default.conf \
 && mkdir -p /etc/relay/nginx /run/nginx /var/cache/nginx /var/log/relay
COPY --from=build /out/relay /usr/local/bin/relay
ENV RELAY_DATA_DIR=/data \
    RELAY_RUN_DIR=/run/relay \
    RELAY_LOG_DIR=/var/log/relay
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD test -S /run/relay/nginx.sock || exit 1
ENTRYPOINT ["relay", "agent", "--engine", "nginx"]

# ---------------------------------------------------------------- haproxy engine
FROM haproxy:3.0-alpine AS haproxy
# The agent binds the runtime/master sockets in /run/relay and must be able to
# bind privileged frontend ports, so it runs as root like the nginx master.
USER root
RUN mkdir -p /etc/relay/haproxy /run/relay
COPY --from=build /out/relay /usr/local/bin/relay
ENV RELAY_DATA_DIR=/data \
    RELAY_RUN_DIR=/run/relay \
    RELAY_LOG_DIR=/var/log/relay
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD test -S /run/relay/haproxy.sock || exit 1
ENTRYPOINT ["relay", "agent", "--engine", "haproxy"]
CMD []
