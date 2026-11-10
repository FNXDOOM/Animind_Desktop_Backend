.PHONY: help build run test vet fmt lint tidy clean migrate docker-up docker-down docker-logs cover test-cache

BINARY ?= build/animind-server
GO     ?= go

help: ## Show the available targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-14s %s\n", $$1, $$2}'

build: ## Compile the server
	@mkdir -p build
	$(GO) build -trimpath -o $(BINARY) ./cmd/server

run: build ## Build, migrate and run with the current environment
	$(BINARY)

migrate: build ## Apply pending migrations and exit
	$(BINARY) --migrate-only

test: ## Run the test suite (integration tests skip without POSTGRES_TEST_DATABASE_URL)
	$(GO) test ./...

test-db: ## Run the suite including Postgres integration tests (-p 1: they share one database)
	POSTGRES_TEST_DATABASE_URL=$(DATABASE_URL) $(GO) test -count=1 -p 1 ./...

test-cache: ## Run the suite including Postgres and Redis integration tests
	POSTGRES_TEST_DATABASE_URL=$(DATABASE_URL) REDIS_TEST_URL=$(REDIS_URL) $(GO) test -count=1 -p 1 ./...

cover: ## Report test coverage per package
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

vet: ## Run go vet
	$(GO) vet ./...

fmt: ## Format all Go source
	$(GO) fmt ./...

lint: ## Run golangci-lint (61 findings are known and not yet gated in CI; see .golangci.yml)
	golangci-lint run ./...

tidy: ## Sync go.mod and go.sum
	$(GO) mod tidy

clean: ## Remove build output and coverage files
	rm -rf build coverage.out

docker-up: ## Start the full stack: nginx, API, Postgres, Redis, Prometheus, Grafana
	docker compose up --build

docker-down: ## Stop the compose stack
	docker compose down

docker-logs: ## Follow the API container
	docker compose logs -f api
