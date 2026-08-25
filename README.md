# AgentOS

AgentOS is a Go-based AI agent infrastructure platform foundation. The goal is to provide a reliable, multi-tenant platform for agent lifecycle management, execution, tooling, and observability while staying modular enough to evolve into a production-grade system.

## Current scope

This repository contains the Phase 0 foundation:

- Go module and application structure
- API and worker entrypoints
- environment configuration
- structured logging
- local Docker Compose stack for Postgres, Redis, and NATS
- CI workflow skeleton
- developer tooling and docs

## Repository layout

```text
agentos/
├── cmd/
│   ├── api/
│   └── worker/
├── internal/
│   ├── config/
│   └── logger/
├── migrations/
├── .github/
├── docs/
├── docker-compose.yml
├── .env.example
├── .gitignore
├── Makefile
├── README.md
├── go.mod
└── ...
```

## Quick start

```bash
cp .env.example .env
make docker-up
make build
make test
make run-api
```

In a second terminal:

```bash
make run-worker
```

## Services

- API: http://localhost:8080
- Worker: local process for asynchronous agent execution
- Postgres: localhost:5432
- Redis: localhost:6379
- NATS: localhost:4222

## Health endpoints

- API: http://localhost:8080/healthz
- API: http://localhost:8080/readyz

## Roadmap

The project follows the implementation plan in [docs/agentos-implementation-plan.md](docs/agentos-implementation-plan.md). The immediate next milestone is Phase 1: authentication and multi-tenancy.

## Contributing

Keep the codebase modular and keep behavior explicit. Prefer typed configuration, structured logging, and clear service boundaries.
# AI-Agentic-Infrastructure-Platform
