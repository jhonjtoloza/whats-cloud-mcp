BINARY_DIR   := bin
GATEWAY_BIN  := $(BINARY_DIR)/gateway
IMAGE        := whats-cloud-mcp/gateway:latest

# CGO stays off everywhere: the SQLite driver is pure Go, which is what allows
# the static distroless image.
export CGO_ENABLED := 0

.PHONY: help test test-race cover build run fmt vet lint tidy docker-build docker-up docker-down clean

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

docker-build: ## Build the container image
	docker build -t $(IMAGE) .

docker-up: ## Start the gateway with docker compose
	docker compose up -d --build

docker-down: ## Stop the gateway
	docker compose down

clean: ## Remove build artefacts
	rm -rf $(BINARY_DIR) coverage.out
