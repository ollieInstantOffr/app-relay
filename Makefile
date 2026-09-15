.PHONY: ui build dev test vet up down logs

ui:
	cd web && npm ci && npm run build

VERSION := $(shell sh scripts/version.sh 2>/dev/null || echo dev)
COMMIT := $(shell git rev-parse HEAD 2>/dev/null)

build: ui
	CGO_ENABLED=0 go build -ldflags "-X main.version=$(VERSION) -X main.commit=$(COMMIT)" -o bin/relay ./cmd/relay

# Run the API locally against ./data (engines unavailable unless compose is up).
dev:
	RELAY_DEV=1 RELAY_DATA_DIR=./data RELAY_RUN_DIR=./data/run RELAY_LOG_DIR=./data/logs go run ./cmd/relay

test:
	go test ./...

vet:
	go vet ./...

# Builds with the version derived from git history (scripts/version.sh).
up:
	RELAY_VERSION=$(VERSION) RELAY_COMMIT=$(COMMIT) docker compose up -d --build

down:
	docker compose down

logs:
	docker compose logs -f
