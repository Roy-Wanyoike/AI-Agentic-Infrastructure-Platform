# AgentOS

AgentOS is a Go-based AI-agent infrastructure platform foundation designed for reliable, multi-tenant agent orchestration. The codebase intentionally starts as a modular monolith with clear boundaries so it can evolve into a larger distributed platform without unnecessary early complexity.

## Current status

The repository now includes a verified platform foundation spanning the first product milestones:

- authentication and tenant-aware token validation
- RBAC permissions for OWNER/ADMIN/MEMBER/VIEWER roles
- organization and membership management
- agent creation, versioning, and runtime execution
- tool registry with calculator tool support
- queue primitives for async task flow
- workflow execution skeleton
- scheduler base model
- observability metrics
- API key issuance and revocation
- audit logging and usage tracking
- webhook event support
- notification primitives
- database and configuration helpers

## Repository layout

```text
agentos/
├── cmd/
│   ├── api/
│   └── worker/
├── internal/
│   ├── agents/
│   ├── apikeys/
│   ├── audit/
│   ├── auth/
│   ├── config/
│   ├── database/
│   ├── logger/
│   ├── notifications/
│   ├── observability/
│   ├── organizations/
│   ├── queue/
│   ├── runtime/
│   ├── scheduler/
│   ├── tenant/
│   ├── tools/
│   ├── usage/
│   ├── webhooks/
│   └── workflows/
├── migrations/
├── .github/
├── docs/
├── docker-compose.yml
├── .env.example
├── .gitignore
├── Makefile
├── README.md
├── go.mod
└── go.sum
```

## Quick start

```bash
cp .env.example .env
make docker-up      # Postgres, Redis, NATS
make migrate-up     # apply database migrations
make seed           # idempotent demo data (org, agents, tool, workflow, runs)
make run-api        # API on :8080
```

In a second terminal, start the worker (run execution):

```bash
make run-worker
```

In a third terminal, run the web dashboard dev server:

```bash
cd web && npm install && npm run dev
```

The seeder creates a demo login you can use right away:
`demo@agentos.dev` / `demo-password` (override with `AGENTOS_SEED_PASSWORD`
before running `make seed`). Platform docs: [docs/architecture.md](docs/architecture.md).

## Services

- API: http://localhost:8080
- Worker: local process for asynchronous agent execution
- Postgres: localhost:5432
- Redis: localhost:6379
- NATS: localhost:4222

## Health endpoints

- API: http://localhost:8080/healthz
- API: http://localhost:8080/readyz

## Verification

The project is validated with:

```bash
PATH="/tmp/go/bin:$PATH" /tmp/go/bin/go test ./...
```

This is the current correctness gate for the repository.

## Roadmap

The implementation follows the plan in [docs/agentos-implementation-plan.md](docs/agentos-implementation-plan.md). The next milestone is persistence and durable data access, followed by API authorization enforcement for real tenant-isolated resource access.

### Product scope and delivery phases

AgentOS is being built as an AI-agent infrastructure platform, not just as a single-agent builder. The project intentionally prioritizes delivery by layer:

- Phase 1: MVP core infrastructure — auth, orgs, RBAC, agent CRUD, versions, model abstraction, runtime, tools, runs, queue, dashboard basics.
- Phase 2: Production platform — memory, workflows, scheduler, webhooks, evaluation, usage analytics, cost tracking, observability, approval flows.
- Phase 3: Enterprise platform — billing, secrets, policy engine, deployment environments, governance, multi-agent orchestration, governance and compliance controls.

The core differentiators are reliability, multi-tenancy, observability, and operational safety: agent execution should be production-grade, not just demo-friendly.

## Contributing

Keep the codebase modular, test-driven, and explicit about tenant boundaries, retries, auth, and observability. The project is intentionally designed for gradual evolution from a solid platform foundation into a production-grade system.
