BINARY_DIR   := bin
GATEWAY_BIN  := $(BINARY_DIR)/gateway
IMAGE        := whats-cloud-mcp/gateway:latest
# The published image. CI builds and pushes it; the server only pulls.
REGISTRY_IMAGE := ghcr.io/jhonjtoloza/whats-cloud-mcp
IMAGE_TAG      ?= latest
COMPOSE_DEV    := -f docker-compose.yml -f docker-compose.dev.yml

# CGO stays off everywhere: the SQLite driver is pure Go, which is what allows
# the static distroless image.
export CGO_ENABLED := 0

.PHONY: help test test-race cover build run fmt vet lint tidy docker-build docker-pull docker-up docker-down docker-deploy docker-dev clean

help: ## Show this help
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

test: ## Run the whole test suite
	go test ./...

test-race: ## Run the test suite with the race detector (needs CGO)
	CGO_ENABLED=1 go test -race ./...

cover: ## Run tests and report coverage
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

build: ## Build the gateway (the only binary; the MCP server is mounted inside it)
	go build -trimpath -ldflags="-s -w" -o $(GATEWAY_BIN) ./cmd/gateway

run: ## Run the gateway locally
	go run ./cmd/gateway

fmt: ## Format the source tree
	gofmt -w .

vet: ## Run go vet
	go vet ./...

lint: fmt vet ## Format, vet and fail on anything left unformatted
	@test -z "$$(gofmt -l .)" || { echo "unformatted files:"; gofmt -l .; exit 1; }

tidy: ## Tidy go.mod / go.sum
	go mod tidy

docker-build: ## Build the container image locally (host architecture only)
	docker build -t $(IMAGE) .

docker-pull: ## Pull the published image from ghcr.io
	docker compose pull

docker-up: ## Start the gateway from the published image
	docker compose up -d

docker-down: ## Stop the gateway
	docker compose down

docker-deploy: ## Pull the published image and restart — the redeploy on the server
	docker compose pull
	docker compose up -d

docker-dev: ## Build from source and start (local development override)
	docker compose $(COMPOSE_DEV) up -d --build

clean: ## Remove build artefacts
	rm -rf $(BINARY_DIR) coverage.out
