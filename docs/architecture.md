# AgentOS Architecture (current state)

This document describes what the codebase actually implements today. It is
updated with the code, not ahead of it — every statement below maps to an
existing package, command, or route.

## Overview

AgentOS is a Go 1.25 modular monolith for multi-tenant AI-agent execution. It
ships as two long-running processes (API server and worker), a Postgres
persistence layer with an automatic in-memory fallback, and a React/Vite web
dashboard. Redis and NATS are available through docker-compose; the code
currently consumes Redis only through the optional queue implementation and
does not consume NATS.

```text
                         ┌──────────────────────────────────────────────────────────┐
                         │                         Client                           │
                         │        (web dashboard · curl · Prometheus scraper)       │
                         └───────┬──────────────────────────────┬───────────────────┘
                                 │ HTTPS / SSE                  │ GET /metrics
                                 ▼                              ▼
┌─────────────────────────────────────────────────┐   ┌──────────────────────────────────┐
│                   cmd/api :8080                 │   │  GET /metrics                    │
│                                                 │   │  - default: Prometheus text      │
│  outer mux (/healthz /readyz /)                 │   │    text/plain; version=0.0.4     │
│   ├─ /api/v1/*  ─┐                              │   │  - ?format=json: legacy JSON     │
│   └─ /v1/*     ──┤ StripPrefix → apiMux         │   │    (+ histograms percentiles)    │
│                  ▼                              │   └──────────────────────────────────┘
│  apiMux (same handlers under both prefixes)     │
│   ├─ /auth/register /auth/login                 │
│   ├─ /agents /agents/create /agents/{id}        │
│   ├─ /runs (GET list / POST create)             │
│   ├─ /runs/{id} /runs/{id}/steps /runs/{id}/events (SSE)
│   ├─ /queue/pull   (worker pull model)          │
│   └─ /metrics      (auth + runs.read)           │
│                                                 │
│  middleware: CORS · auth.RequireAuthOrAPIKey ·  │
│              auth.RequirePermission (RBAC)      │
└───────────────┬─────────────────────────────────┘
                │ services (internal/*)
                ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│ auth · organizations · apikeys · agents · runs · workflows · queue ·        │
│ streaming · audit · usage · memory · models · runtime · tools · scheduler · │
│ notifications · webhooks · secrets · billing · deployments · observability  │
│                                                                             │
│  dual-mode pattern per service:                                             │
│    NewService()                 → in-memory maps + mutex (zero-infra mode)  │
│    NewServiceWithStore(store)   → Postgres store is the source of truth     │
└──────────────┬──────────────────────────────────────────────┬───────────────┘
               │ lib/pq                                       │ optional
               ▼                                              ▼
┌──────────────────────────┐                    ┌────────────────────────────┐
│  Postgres (source of     │                    │  Redis (optional)          │
│  truth, migrations       │                    │  queue.RedisQueue impl     │
│  001-005 via cmd/migrate)│                    │  (not wired into cmd yet)  │
└──────────────────────────┘                    └────────────────────────────┘

┌───────────────────────────────────────┐
│            cmd/worker :8081           │
│  queue loop (in-memory push model)    │
│  or /queue/pull polling when          │
│  AGENTOS_API_PULL=true (pull model)   │
│  runtime.Runner:                      │
│    bounded model-in-the-loop          │
│    tools registry (calculator, http)  │
│    step recorder → runs.RecordStep    │
│  models.Provider: OpenAI-compatible   │
│  (+ FailoverProvider when a fallback  │
│   key is configured; offline          │
│   deterministic mode without keys)    │
│  status events → POST /runs/{id}/events
└───────────────────────────────────────┘
```

## Processes

| Command       | Default port | Purpose |
|---------------|--------------|---------|
| `cmd/api`     | 8080 (`API_PORT`)  | HTTP API, auth, metrics, SSE streaming |
| `cmd/worker`  | 8081 (`WORKER_PORT`) | Executes queued runs through the runtime loop |
| `cmd/migrate` | — | Applies/rolls back SQL migrations from `migrations/*.sql` (auto-discovered, tracked in `schema_migrations`) |
| `cmd/seed`    | — | Idempotent demo data for local development (requires migrations) |

## Multi-tenancy and auth

- Every tenant-facing store query is scoped by `organization_id`; tenant
  identity is resolved from HMAC-signed tokens (`Authorization: Bearer`) or
  API keys (`X-API-Key`), never from client-supplied org IDs.
- Passwords are bcrypt-hashed (`golang.org/x/crypto/bcrypt`); tokens are
  HMAC-SHA256 signed JSON claims with expiry (`internal/auth`).
- RBAC roles OWNER/ADMIN/MEMBER/VIEWER gate routes through
  `auth.RequireAuthOrAPIKey` + `auth.RequirePermission` middleware
  (`agents.read`, `agents.write`, `runs.read`, `runs.execute`, ...).
- API keys are stored hashed with a visibility prefix (`api_keys` table).

## Persistence: two modes, same code paths

1. **Postgres mode** — when `DATABASE_URL` (or `POSTGRES_*` variables) is set
   and reachable, `cmd/api` builds every service with
   `NewServiceWithStore(NewPostgresStore(db))`. Tables: organizations, users,
   agents, agent_versions, tools, tool_permissions, runs, run_steps,
   workflows, notifications, api_keys, organization_memberships, audit_logs,
   usage_records (migrations 001-005, idempotent SQL).
2. **Zero-infrastructure mode** — when no DSN is configured or the ping
   fails, the same services run on in-memory maps so the platform boots and
   serves traffic with no external dependencies. A dev API key is created for
   worker polling convenience in this mode only.

Both modes are covered by tests (in-memory unit tests, `sqlmock` for stores,
`miniredis` for the Redis queue).

## Run execution

- `POST /runs` requires an agent id and organization access, creates a
  `QUEUED` run and enqueues a task (`{run_id, agent_id, input}`) on the
  in-memory queue.
- The worker consumes the task (push model) or polls `GET /queue/pull`
  (`AGENTOS_API_PULL=true`), marks the run `RUNNING`, executes it through
  `runtime.Runner.RunWithID`, and posts status transitions back to the API
  (`POST /runs/{id}/events`) with retry/backoff.
- The runtime loop is bounded: max steps (10), max runtime (60s), per-tool
  timeout (10s), loop detection (identical tool call ≥3×), and context
  cancellation. Each model/tool step is recorded through a `StepRecorder`
  into `run_steps` (tenant-scoped, sequential `step_index`), which powers
  `GET /runs/{id}/steps` and the run timeline.
- Without `OPENAI_API_KEY` the worker runs in a deterministic offline mode
  (math expressions route to the calculator tool; other inputs get a canned
  completion) so local development needs no model credentials.

## Model provider layer

- `models.Provider` is the abstraction; `models.NewOpenAIProvider` speaks any
  OpenAI-compatible HTTP API (`OPENAI_BASE_URL` targets OpenRouter/Groq/
  Ollama/vLLM, model via `AGENTOS_WORKER_MODEL`).
- `models.NewFailoverProvider(primary, fallback)` chains providers
  (`AGENTOS_FALLBACK_API_KEY` / `AGENTOS_FALLBACK_BASE_URL`).

## Queue

- `queue.Queue` — in-memory task queue (used by the API and worker by
  default), with a worker loop and status/attempts bookkeeping.
- `queue.RedisQueue` — Redis-backed implementation of the same interface
  (`agentos:queue` list); implemented and tested but not yet wired into the
  commands.
- Pull model — `GET /queue/pull` (behind auth + `runs.execute`) lets external
  workers dequeue tasks; used by `cmd/worker` when `AGENTOS_API_PULL=true`.

## Streaming

`internal/streaming` is an in-process pub/sub with per-run history. The API
exposes it at `GET /runs/{id}/events`:

- `Accept: text/event-stream` → live SSE (`data: {...}\n\n`, flushed per
  event) after replaying history;
- otherwise → JSON history (compat).

Status changes (including worker callbacks) and step recordings publish
events through this channel.

## Observability

- `GET /metrics` (auth + `runs.read`):
  - default: Prometheus text exposition version 0.0.4
    (`Content-Type: text/plain; version=0.0.4`), hand-rolled in
    `internal/observability` — no client_golang dependency. Families:
    `agentos_runs_total`, `agentos_tools_total`,
    `agentos_request_duration_seconds_bucket{le=...}` (+`_sum`/`_count`),
    `http_requests_total{route,method,status}`, `agentos_queue_length`.
  - `?format=json`: the original JSON payload (`counts`, `latency`,
    `queue_length`) plus an additive `histograms` section with
    count/sum/min/max/p50/p95/p99 per series.
- Bucketed percentiles come from fixed cumulative histograms (default
  Prometheus bounds); p50/p95/p99 are linearly interpolated between bucket
  bounds the way `histogram_quantile` does.
- `observability.MetricsMiddleware` records per-route request counts and
  durations; `observability.RateLimiter` and `observability.Quota` provide
  fixed-window primitives (used by other packages; wiring notes in
  `docs/wiring/`).
- `/healthz` and `/readyz` are unauthenticated liveness/readiness probes.

## API surface

All versioned routes are mounted twice: `/api/v1/...` (canonical, used by the
web dashboard) and `/v1/...` (legacy). The contract lives in
`api/openapi.yaml`; per-track additions are collected under `api/fragments/`
and `docs/wiring/`.

## Web dashboard

`web/` is a React 19 + Vite + TypeScript app consuming the real API through a
typed client (`/api/v1` prefix) with SSE-driven run updates. `npm run build`
is part of the verification gate.

## Local development loop

```bash
make docker-up     # Postgres, Redis, NATS
make migrate-up    # apply migrations
make seed          # demo org/user/agents/tool/workflow/runs/eval dataset
make run-api       # terminal 1
make run-worker    # terminal 2
cd web && npm run dev   # terminal 3 (dashboard)
```

`make lint` (gofmt + go vet) and `go test ./...` are the correctness gates;
tests never require running infrastructure.
