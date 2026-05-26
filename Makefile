.PHONY: help config up down tidy run build test lint

APP_BINARY := bin/app
COMPOSE    := docker compose -f deploy/docker-compose.yaml

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

run: ## Run the application
	go run ./cmd/app

build: ## Build the binary into bin/
	go build -o $(APP_BINARY) ./cmd/app

test: ## Run all tests
	go test ./...

lint: ## Run golangci-lint
	golangci-lint run
