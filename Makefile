APP_NAME=agentos

.PHONY: build test lint seed run-api run-worker docker-up docker-down migrate-up migrate-down help

help:
	@echo "Available targets:"
	@echo "  build       Build API and worker binaries"
	@echo "  test        Run Go tests"
	@echo "  lint        gofmt check + go vet"
	@echo "  seed        Seed idempotent demo data (requires migrate-up first)"
	@echo "  run-api     Run the API service"
	@echo "  run-worker  Run the worker service"
	@echo "  docker-up   Start local Postgres, Redis, and NATS"
	@echo "  docker-down Stop local services"
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
