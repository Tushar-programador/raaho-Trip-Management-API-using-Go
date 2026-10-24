# Load local settings if present (copy .env.example to .env).
-include .env
export

GO      ?= go
COMPOSE ?= docker compose

POSTGRES_USER     ?= trips
POSTGRES_PASSWORD ?= trips
API_DB_PASSWORD   ?= trip_api
# Inside the compose network the database is always postgres:5432.
DOCKER_TEST_DATABASE_URL     = postgres://$(POSTGRES_USER):$(POSTGRES_PASSWORD)@postgres:5432/trips_test?sslmode=disable
DOCKER_TEST_APP_DATABASE_URL = postgres://trip_api:$(API_DB_PASSWORD)@postgres:5432/trips_test?sslmode=disable

.DEFAULT_GOAL := help
.PHONY: help run build test test-unit test-integration test-race test-race-docker \
        lint fmt migrate-up migrate-down migrate-version db-up docker-up docker-down docker-logs clean

help: ## List available targets
	@grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "} {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

run: ## Run the API on the host (needs DATABASE_URL and a migrated database)
	$(GO) run ./cmd/api

build: ## Build bin/api and bin/migrate
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/api ./cmd/api
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/migrate ./cmd/migrate

test: ## Run all tests (integration tests are skipped unless TEST_DATABASE_URL is set)
	$(GO) test ./...

test-unit: ## Run unit tests only (no database needed)
	$(GO) test ./internal/...

test-integration: ## Run integration tests against TEST_DATABASE_URL
	@test -n "$(TEST_DATABASE_URL)" || (echo "TEST_DATABASE_URL is not set (see .env.example)" && exit 1)
	$(GO) test -count=1 -v ./tests/...

test-race: ## Run all tests with the race detector (requires cgo and a C compiler)
	CGO_ENABLED=1 $(GO) test -race -count=1 ./...

test-race-docker: db-up ## Run all tests with -race inside a golang container against the compose database
	docker run --rm --network trip-management_default \
		-v "$(CURDIR)":/src -w /src -v trip-management-gomod:/go/pkg/mod \
		-e TEST_DATABASE_URL="$(DOCKER_TEST_DATABASE_URL)" \
		-e TEST_APP_DATABASE_URL="$(DOCKER_TEST_APP_DATABASE_URL)" \
		golang:1.26 go test -race -count=1 ./...

lint: ## Check formatting and run go vet
	@unformatted="$$(gofmt -l .)"; if [ -n "$$unformatted" ]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi
	$(GO) vet ./...

fmt: ## Format all Go files
	gofmt -w .

migrate-up: ## Apply all pending migrations (as ADMIN_DATABASE_URL)
	$(GO) run ./cmd/migrate up

migrate-down: ## Roll back the most recent migration (as ADMIN_DATABASE_URL)
	$(GO) run ./cmd/migrate down

migrate-version: ## Print the schema version (via ADMIN_DATABASE_URL)
	$(GO) run ./cmd/migrate version

db-up: ## Start only PostgreSQL (and wait until it is healthy)
	$(COMPOSE) up -d --wait postgres

docker-up: ## Build and start postgres -> migrate -> api, waiting for health checks
	$(COMPOSE) up -d --build --wait

docker-down: ## Stop the stack (keeps the database volume)
	$(COMPOSE) down

docker-logs: ## Follow API logs
	$(COMPOSE) logs -f api

clean: ## Remove build output
	rm -rf bin coverage.out coverage.html
