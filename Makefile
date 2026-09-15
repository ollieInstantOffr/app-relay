.PHONY: ui build dev test vet up down logs

ui:
	cd web && npm ci && npm run build

build: ui
	CGO_ENABLED=0 go build -o bin/relay ./cmd/relay

# Run the API locally against ./data (engines unavailable unless compose is up).
dev:
	RELAY_DEV=1 RELAY_DATA_DIR=./data RELAY_RUN_DIR=./data/run RELAY_LOG_DIR=./data/logs go run ./cmd/relay

test:
	go test ./...

vet:
	go vet ./...

up:
	docker compose up -d --build

down:
	docker compose down

logs:
	docker compose logs -f
