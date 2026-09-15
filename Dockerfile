# syntax=docker/dockerfile:1.7
#
# Relay image:
#   docker build --target relay -t relay .     API, UI, MCP (+ agent binary)
#
# nginx and HAProxy run the official images (docker-compose.yml). On start the
# relay container copies its static binary to the relay-bin volume
# (/opt/relay/bin/relay) and the engine containers run it as `relay agent`.

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
# Set by `make up` and the in-app upgrader: the version derived from git history
# (scripts/version.sh) and the commit Relay is built from.
ARG RELAY_VERSION=
ARG RELAY_COMMIT=
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${RELAY_VERSION:-dev} -X main.commit=${RELAY_COMMIT}" -o /out/relay ./cmd/relay

# ---------------------------------------------------------------- relay (app)
FROM alpine:3.22 AS relay
RUN apk add --no-cache ca-certificates tzdata && mkdir -p /opt/relay/bin
COPY --from=build /out/relay /usr/local/bin/relay
ENV RELAY_DATA_DIR=/data \
    RELAY_RUN_DIR=/run/relay \
    RELAY_LOG_DIR=/var/log/relay \
    RELAY_LISTEN=:8181 \
    RELAY_AGENT_BIN_DIR=/opt/relay/bin
VOLUME ["/data"]
EXPOSE 8181
# Follows the admin UI port from Settings → General (not only RELAY_LISTEN).
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD ["relay", "healthcheck"]
ENTRYPOINT ["relay"]
CMD ["serve"]
