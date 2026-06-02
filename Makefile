.PHONY: help config up down tidy run build migrate test lint

API_BINARY     := bin/api
MIGRATE_BINARY := bin/migrate
COMPOSE        := docker compose -f deploy/docker-compose.yaml

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

config: ## Bootstrap local config from the example (skipped if already exists)
	@test -f config/config.yaml || cp config/config.example.yaml config/config.yaml

up: ## Start local Postgres
	$(COMPOSE) up -d

down: ## Stop local Postgres and remove volumes
	$(COMPOSE) down -v

tidy: ## Resolve and lock module dependencies
	go mod tidy

run: ## Run the API server
	go run ./cmd/api

build: ## Build both binaries into bin/
	go build -o $(API_BINARY) ./cmd/api
	go build -o $(MIGRATE_BINARY) ./cmd/migrate

migrate: ## Apply all SQL migrations (run before `make run` on a fresh DB)
	go run ./cmd/migrate up

test: ## Run all tests
	go test ./...

lint: ## Run golangci-lint
	golangci-lint run
