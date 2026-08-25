APP_NAME=agentos

.PHONY: build test run-api run-worker docker-up docker-down migrate-up migrate-down help

help:
	@echo "Available targets:"
	@echo "  build       Build API and worker binaries"
	@echo "  test        Run Go tests"
	@echo "  run-api     Run the API service"
	@echo "  run-worker  Run the worker service"
	@echo "  docker-up   Start local Postgres, Redis, and NATS"
	@echo "  docker-down Stop local services"

build:
	@go build ./...

test:
	@go test ./...

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
