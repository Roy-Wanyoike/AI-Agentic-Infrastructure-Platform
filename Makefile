APP_NAME=agentos

.PHONY: build test lint seed run-api run-worker docker-up docker-down docker-build docker-up-prod docker-down-prod docker-migrate migrate-up migrate-down help

help:
	@echo "Available targets:"
	@echo "  build       Build API and worker binaries"
	@echo "  test        Run Go tests"
	@echo "  lint        gofmt check + go vet"
	@echo "  seed        Seed idempotent demo data (requires migrate-up first)"
	@echo "  run-api     Run the API service"
	@echo "  run-worker  Run the worker service"
	@echo "  docker-up    Start local Postgres, Redis, and NATS (dev stack)"
	@echo "  docker-down  Stop local services (dev stack)"
	@echo "  docker-build Build production images (api, worker, web)"
	@echo "  docker-up-prod  Start the production stack (docker-compose.prod.yml)"
	@echo "  docker-down-prod Stop the production stack"
	@echo "  docker-migrate  Run migrations against the production stack"
	@echo "  migrate-up  Apply database migrations"
	@echo "  migrate-down Roll back the last migration"

build:
	@go build ./...

test:
	@go test ./...

lint:
	@files="$$(gofmt -l ./cmd ./internal)"; \
	if [ -n "$$files" ]; then \
		echo "gofmt needed on:"; \
		echo "$$files"; \
		exit 1; \
	fi; \
	go vet ./...

seed:
	@go run ./cmd/seed

run-api:
	@go run ./cmd/api

run-worker:
	@go run ./cmd/worker

docker-up:
	@docker compose up -d

docker-down:
	@docker compose down

migrate-up:
	@go run ./cmd/migrate --up

migrate-down:
	@go run ./cmd/migrate --down

# --- Production deployment (Dockerfile.api/worker/web + docker-compose.prod.yml,
# see docs/self-hosting.md). The dev `docker-up`/`docker-down` targets above keep
# managing the infra-only docker-compose.yml and are intentionally untouched.

docker-build:
	@docker build -f Dockerfile.api -t agentos/api:latest .
	@docker build -f Dockerfile.worker -t agentos/worker:latest .
	@docker build -f Dockerfile.web -t agentos/web:latest .

docker-up-prod:
	@docker compose -f docker-compose.prod.yml --profile migrate up -d --build

docker-down-prod:
	@docker compose -f docker-compose.prod.yml down

docker-migrate:
	@docker compose -f docker-compose.prod.yml --profile migrate run --rm migrate
