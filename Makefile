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

# --- Load testing suite (issue #125, see loadtest/README.md) ---------------
# Black-box load scenarios against a RUNNING AgentOS API (default
# http://127.0.0.1:8080 in zero-infrastructure mode; boot one with
# `API_PORT=8080 AGENTOS_RATE_LIMIT_RPM=1000000 go run ./cmd/api`).
# Targets below are purely additive to the dev/deploy targets above.

AGENTLOAD ?= /tmp/agentload
LOADTEST_BASE ?= http://127.0.0.1:8080
LOADTEST_BASELINE ?= loadtest/baselines/sustained-inmemory.json

.PHONY: loadtest-build loadtest-sustained loadtest-spike loadtest-soak-short loadtest-concurrency loadtest-workflow loadtest-lifecycle loadtest-compare loadtest-smoke

loadtest-build:
	@go build -o $(AGENTLOAD) ./loadtest/cmd/agentload

loadtest-sustained: loadtest-build
	@$(AGENTLOAD) run -scenario sustained -base $(LOADTEST_BASE)

loadtest-spike: loadtest-build
	@$(AGENTLOAD) run -scenario spike -base $(LOADTEST_BASE)

loadtest-soak-short:
	@go run ./loadtest/cmd/agentload run -scenario soak -base $(LOADTEST_BASE) -d 60s

loadtest-concurrency:
	@go run ./loadtest/cmd/agentload run -scenario concurrency -base $(LOADTEST_BASE) -ramp-step 2s -max-workers 64

loadtest-workflow:
	@go run ./loadtest/cmd/agentload run -scenario workflow -base $(LOADTEST_BASE)

loadtest-lifecycle:
	@go run ./loadtest/cmd/agentload run -scenario lifecycle -base $(LOADTEST_BASE)

# Gate a current JSON report against the recorded baseline.
# Usage: make loadtest-compare CURRENT=/tmp/current.json [BASELINE=...]
# Exits 1 on regression (p95 +10% or error rate +2pp).
loadtest-compare: loadtest-build
	@if [ -z "$(CURRENT)" ]; then \
		echo "usage: make loadtest-compare CURRENT=path/to/report.json [BASELINE=$(LOADTEST_BASELINE)]"; \
		exit 2; \
	fi
	@$(AGENTLOAD) compare $(BASELINE) $(CURRENT)

# Full end-to-end smoke: builds the API + CLI, boots :8080, runs short versions
# of all six scenarios, records loadtest/baselines/sustained-inmemory.json and
# gates a fresh run against it.
loadtest-smoke:
	@bash loadtest/smoke.sh
