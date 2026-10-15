# AgentOS Implementation Plan

> **Status (updated at docs reconciliation #154):** the plan below was written
> before the first commit; the repository has since delivered **phases 0–12 in
> full** plus the wave-9/10 programs (issues #100–#156), including the
> synthetic-assurance flagship (#118, closure evidence in
> [synthetic-assurance.md](synthetic-assurance.md)). Verified reality at this
> revision: **47 `internal/` packages, 50 Go test packages green, migrations
> 001–037, OpenAPI 182 paths / 531 schemas (222 registered routes under dual
> `/api/v1` + `/v1` mounts), deploy/ artifacts (k8s · helm · kustomize ·
> terraform), six-scenario load suite**. Completed items have been moved out of
> the roadmap into the **Delivered** summary; the remaining work is the
> **Planned** list. Sections 2–14 are kept as the original strategy record.

## 1. Current repository status

The workspace is a working platform, not a scaffold. Concretely:

- Two long-running processes (`cmd/api` :8080, `cmd/worker` — no HTTP
  listener) plus support binaries (`cmd/migrate`, `cmd/seed`,
  `cmd/agentosctl`, `cmd/healthcheck`, `cmd/sandbox-exec`).
- 47 domain packages under `internal/`; 50 Go test packages run green
  (`go test ./...`, race job in CI) with zero external infrastructure.
- Postgres migrations 001–037; every wired service runs dual-mode
  (in-memory or Postgres) through the same interface.
- HTTP contract: `api/openapi.yaml` (182 paths / 531 schemas),
  regression-tested by `internal/apispec`; 66 staged fragments under
  `api/fragments/`.
- Production artifacts: `deploy/kubernetes` (23 manifests),
  `deploy/helm/agentos` (chart + 18 templates), `deploy/kustomize` (base +
  dev/staging/production overlays), `deploy/terraform` (modules × 3
  environments), offline validators under `deploy/validation/`.
- Load suite: `loadtest/` (six scenarios, baselines, CI regression gate).
- Architecture walkthrough with per-claim pointers:
  [architecture.md](architecture.md).

## 1a. Delivered (phases 0–12 + wave programs)

| Plan phase | Delivered as | Evidence / issues |
|---|---|---|
| 0 — Foundation | Go module, config/logging, docker-compose (Postgres/Redis/NATS), migration runner, Makefile, CI gates | `cmd/migrate`, `migrations/001-005`, `Makefile`, `.github/workflows` |
| 1 — Auth & multi-tenancy | orgs/users/memberships, RBAC (OWNER/ADMIN/MEMBER/VIEWER), bcrypt + HMAC tokens, API keys, audit basics | `internal/auth`, `internal/organizations`, migrations 001/002; later: SSO/SCIM (#'s 019 migration), token revocation |
| 2 — Agent CRUD & versioning | agent CRUD + immutable versions + publish/rollback + lifecycle state machine + export/import/clone | `internal/agents`, `versions.go`, `lifecycle.go`, `portability.go` (#122), migrations 003/007/032 |
| 3 — Model abstraction | provider interface, OpenAI-compatible adapter, failover, router policies (#112), circuit breakers, response resilience (#128), pricing | `internal/models` (gateway.go, router_policies.go, circuitbreaker.go, resilience.go) |
| 4 — Agent runtime | bounded loop, state machine, step recording, checkpoints (#115/#37 migration), recovery, graceful shutdown (#107) | `internal/runtime`, `internal/runs` |
| 5 — Tooling & permissions | tool registry + schemas, permission tags, calculator/HTTP/database/search/code/webhook/JSON/file tools, sandbox | `internal/tools`, `internal/sandbox`, `cmd/sandbox-exec` |
| 6 — Async worker | queue with retries/DLQ/ops API (#116 dispatch plane memory\|nats\|hybrid), Redis queue, pull model, queue introspection | `internal/queue` (backend.go, dispatch.go, jetstream*.go), `cmd/worker` |
| 7 — Memory & run history | short/long memory, RAG + embeddings (#113, pgvector 033, hybrid RRF), run steps timeline, cost tracking (012) | `internal/memory`, `internal/knowledge` |
| 8 — Streaming & dashboard | SSE run events, React 19 dashboard (overview/agents/runs/knowledge/analytics), trace viewer (#126) | `internal/streaming`, `web/` |
| 9 — Workflows & approvals | DSL + validation (#109), control flow, parallel (#110), durable execution (013), approvals + enterprise oversight (#137) | `internal/workflows`, `internal/approvals` |
| 10 — Scheduler & webhooks | cron scheduler with catch-up (011), webhook deliveries + subscriptions (024), notification pipeline | `internal/scheduler`, `internal/webhooks`, `internal/notifications` |
| 11 — Production hardening | rate limits (#124), quotas/budgets/billing enforcement, CORS + JWT prod guard (#55), secrets (AES-GCM, 017), audit chain + compliance pack (#138), residency (#140) | `internal/limiter`, `internal/billing`, `cmd/api/cors.go`, `cmd/api/prodguard.go`, `internal/secrets`, `internal/audit`, `internal/compliance`, `internal/tenant` |
| 12 — Load testing & scale | #80 baseline harness + #125 six-scenario suite, baselines + regression comparator | `scripts/loadtest`, `loadtest/`, `docs/load-testing.md` |
| Beyond-plan programs | Marketplace + signing, connectors, MCP gateway/registry (#111/#82), A2A (#136), orchestration (#108), evaluations + adversarial suites (#153), canary + eval-gated promotion (#13/#51), gateway virtual keys (#139), agent keys + budgets, batch ops (#123), knowledge graph (#130-#132), simulation (#133), ReACT (#134), configgen (#129), deploy artifacts (#117), synthetic assurance (#118/#147-#156) | see [architecture.md](architecture.md) sections and [synthetic-assurance.md](synthetic-assurance.md) |

## 1b. Planned — honest gaps (kept visible)

1. Postgres stores for quality-gate policies/evidence and synth
   journeys/runs/evidence (registry + findings stores exist and ship;
   `PostgresFindingStore` is implemented but not yet wired into
   `cmd/api/main.go`).
2. Synth worker orchestration wiring (`BrowserWorker`/`APIWorker` into
   `cmd/worker`) and HTTP surfaces for journeys/runs/findings/GitHub sync
   (fragments are schema-only by design; route ownership is a recorded
   orchestrator decision).
3. NATS `nats|hybrid` dispatch as a documented production default — the
   `nats.go` client is already a direct dependency and the JetStream
   adapter is tested against an embedded broker; what remains is rollout
   (deploy defaults + ops runbook), not code.
4. MCP/A2A residency-guard wiring over the ready `tenant.Guard` seam
   (`internal/tenant/residency.go:451`).
5. CORS preset for the hosted portal playground (`AGENTOS_CORS_ORIGINS`
   allowlist exists; `docs/portal/` needs a documented origin).

## 2. Product strategy

AgentOS will be built as a modular monolith with clear internal boundaries and a worker-based execution model. The key decision is to keep the initial system deployable as:

- API service
- Worker service
- PostgreSQL
- Redis
- NATS or queue broker
- Web dashboard (delivered as React 19 + Vite + TypeScript in `web/`)

This delivers serious distributed-systems and backend engineering signal without prematurely creating a network of tiny services.

The product is not an AI chatbot. It is infrastructure for reliable AI agent execution.

## 3. Complete feature map and delivery strategy

AgentOS should be understood as a complete AI-agent infrastructure platform. The platform must support operational reliability, safety, cost visibility, governance, and multi-tenant execution, not just agent generation.

### 3.1 Platform feature categories

#### 3.1.1 Core platform
- dashboard with agent, run, latency, cost, queue, and workflow metrics
- quick actions for create agent, run agent, create workflow, add tool, review failures, usage, and API keys
- organization-level visibility and admin operations
- health, readiness, and operational status surfaces

#### 3.1.2 Agent lifecycle and versioning
- create, read, update, delete, archive, pause, duplicate, activate, export, and import agents
- agent states: DRAFT, ACTIVE, PAUSED, DISABLED, ARCHIVED
- versioned agent configuration with comparisons, deployment history, rollback, and publishing workflows
- reproducible behavior through immutable versions, notes, authors, timestamps, and config diffing

#### 3.1.3 Agent execution and runtime
- run lifecycle states: QUEUED, RUNNING, WAITING_APPROVAL, PAUSED, COMPLETED, FAILED, CANCELLED, TIMEOUT
- execution timeline showing model calls, tool usage, approvals, and final response
- runtime safeguards: max steps, max runtime, token caps, tool call limits, cancellation, retries, and loop detection
- trace IDs, model metadata, token usage, and cost attribution for each execution step

#### 3.1.4 Tools and permissions
- tool registry with name, description, version, schema, timeout, retry policy, and auth requirements
- built-in tool starting set: calculator, HTTP request, JSON transformation, webhook, and later DB, SaaS and search tools
- tool permissions, agent permissions, org policies, read/write restrictions, approval requirements, and sensitive-tool controls

#### 3.1.5 Memory and knowledge systems
- short-term memory: current context, task state, conversation state
- long-term memory: user preferences, customer notes, important facts, and historical context
- memory retrieval, updates, expiry, semantic search, and relevance scoring
- RAG pipeline with document ingestion, parsing, chunking, embeddings, vector retrieval, citations, and metadata filtering

#### 3.1.6 Model abstraction and routing
- provider-level configuration, model discovery, capabilities, rate limits, pricing, and availability
- model router for cost, latency, task complexity, fallback behavior, and provider health
- fallback strategies including retries, model failover, timeout fallback, and circuit breakers

#### 3.1.7 Workflows and automation
- workflow nodes: agent, tool, condition, delay, approval, webhook, transform, parallel branch, trigger, and end
- conditional logic and branching for business rules
- parallel execution and fan-out/fan-in patterns
- schedule-based and event-based automation

#### 3.1.8 Human-in-the-loop and governance
- approval queue with approve, reject, request changes, comments, expiration, and audit history
- policy engine to allow or deny actions by risk and sensitivity
- governance rules for PII, financial access, external communication, and dangerous tool execution

#### 3.1.9 Platform operations and reliability
- queue tasks with retries, backoff, priority, cancellation, worker health checks, task recovery, and DLQ handling
- Postgres/Redis/NATS-backed durable architecture with graceful shutdown and recovery
- real-time observability via logs, metrics, and traces
- secret management, API key lifecycle, rate limiting, and usage controls

#### 3.1.10 Enterprise and ecosystem features
- billing, usage metering, plan management, developer portal, SDKs, CLI, deployment environments, and integration connectors
- notifications, audit logs, export capabilities, and admin controls
- multi-agent systems, agent-to-agent delegation, and sandboxing for advanced workloads

### 3.2 Delivery strategy: MVP, production platform, enterprise

#### MVP (must ship first)
1. Authentication and session management
2. Organizations and RBAC
3. Agent CRUD and versioning
4. Agent runtime and run execution
5. Tool registry and permissions
6. Basic model abstraction
7. Queue and worker execution
8. Postgres-backed persistence
9. Redis-backed queue and caching
10. Basic dashboard and run history

#### Production platform
1. Memory and long-term state
2. RAG and knowledge bases
3. Workflow engine and approvals
4. Scheduler and webhooks
5. Usage analytics and cost management
6. Model routing and fallback
7. Observability, traces, and audit trails
8. API keys, rate limiting, quotas, and developer controls
9. Evaluation and regression testing

#### Enterprise / advanced platform
1. Billing and subscriptions
2. Deployment environments and release controls
3. CLI and SDKs
4. Integrations and connectors framework
5. Secrets management and governance
6. Agent marketplace and templates
7. Multi-agent orchestration
8. Sandbox execution and isolated tooling
9. Enterprise SSO/SCIM and compliance controls

### 3.3 Product thesis

The strongest differentiator for AgentOS is that it should not merely help someone build an AI agent. It should make that agent operationally reliable enough to function as a durable production service with proper governance, observability, retry semantics, and cost controls.

This product vision aligns with the backend and platform-first architecture already present in this repository: Go services, modular internals, queue-based worker execution, and tenant-aware boundaries.

## 4. Proposed architecture

```text
                       ┌──────────────────────┐
                       │  Web Dashboard (web/) │
                       └───────────┬──────────┘
                                   │ REST / SSE / WebSocket
                                   ▼
                       ┌──────────────────────┐
                       │   Go API Layer       │
                       └───────────┬──────────┘
                                   │
                 ┌─────────────────┼─────────────────┐
                 │                 │                 │
                 ▼                 ▼                 ▼
        ┌──────────────┐   ┌──────────────┐   ┌──────────────┐
        │ PostgreSQL   │   │    Redis     │   │    NATS      │
        └──────┬───────┘   └──────┬───────┘   └──────┬───────┘
               │                   │                   │
               │                   │                   ▼
               │                   │            ┌──────────────┐
               │                   │            │   Go Worker  │
               │                   │            │   Runtime    │
               │                   │            └──────┬───────┘
               │                   │                   │
               │                   │           ┌───────┼────────┐
               │                   │           ▼       ▼        ▼
               │                   │      Models   Tools    Memory
               │                   │
               └───────────────────┴────────────────────────────
```

### Architectural principles

1. Go owns the platform runtime and orchestration.
2. Models are abstracted behind an internal interface.
3. Tools are permissioned and individually registered.
4. Agent execution is asynchronous and durable.
5. Runs are immutable snapshots of execution state.
6. Multi-tenancy is enforced in application and query layers.
7. Security and observability are first-class concerns.
8. The system starts modular inside a monolith, not with many separate deployments.

## 4. Proposed repository structure

```text
agentos/
├── cmd/
│   ├── api/
│   └── worker/
├── internal/
│   ├── agents/
│   ├── runtime/
│   ├── models/
│   ├── tools/
│   ├── memory/
│   ├── workflows/
│   ├── scheduler/
│   ├── tasks/
│   ├── organizations/
│   ├── users/
│   ├── auth/
│   ├── permissions/
│   ├── usage/
│   ├── billing/
│   ├── audit/
│   ├── webhooks/
│   ├── queue/
│   ├── observability/
│   ├── database/
│   └── config/
├── pkg/
│   ├── logger/
│   ├── errors/
│   └── tracing/
├── web/
│   └── app/
├── migrations/
├── deployments/
│   ├── docker/
│   └── kubernetes/
├── terraform/
├── docs/
├── scripts/
├── tests/
├── docker-compose.yml
├── Makefile
├── go.mod
├── README.md
├── .env.example
└── .github/workflows/
```

## 5. Core domain model

### Organization
- id
- name
- status
- created_at
- updated_at

### User
- id
- organization_id
- email
- password_hash
- status
- created_at

### Membership / Role
- user_id
- organization_id
- role

### Agent
- id
- organization_id
- name
- description
- instructions
- model
- temperature
- max_steps
- max_runtime
- status
- current_version_id
- created_at
- updated_at

### AgentVersion
- id
- agent_id
- version
- instructions
- model_config
- config_hash
- created_at

### Tool
- id
- organization_id
- name
- description
- schema
- permission_tags
- timeout_ms
- enabled
- created_at

### Run
- id
- organization_id
- agent_id
- agent_version_id
- user_id
- status
- run_type
- input
- output
- started_at
- ended_at
- error
- created_at

### RunStep
- id
- run_id
- step_type
- status
- started_at
- completed_at
- input_meta
- output_meta
- error
- token_usage
- cost

### Task / TaskAttempt
- id
- queue_name
- payload
- status
- attempts
- max_attempts
- next_run_at
- dead_lettered
- created_at
- updated_at

### Memory
- id
- organization_id
- agent_id
- memory_type (short/long)
- content
- embedding
- metadata
- created_at

### Workflow / WorkflowRun
- id
- organization_id
- name
- definition
- state
- created_at

### UsageRecord / AuditLog / Webhook
- separate for observability, cost tracking, and eventing

## 6. API boundaries

### Auth
- POST /v1/auth/register
- POST /v1/auth/login
- POST /v1/auth/refresh
- POST /v1/auth/logout
- POST /v1/auth/api-keys

### Agents
- GET /v1/agents
- POST /v1/agents
- GET /v1/agents/:id
- PATCH /v1/agents/:id
- DELETE /v1/agents/:id
- POST /v1/agents/:id/runs
- GET /v1/agents/:id/runs

### Runs
- GET /v1/runs/:id
- GET /v1/runs/:id/steps
- POST /v1/runs/:id/cancel
- GET /v1/runs/:id/stream (SSE or WS)

### Tools
- GET /v1/tools
- POST /v1/tools
- GET /v1/tools/:id
- PATCH /v1/tools/:id

### Workflows
- GET /v1/workflows
- POST /v1/workflows
- GET /v1/workflows/:id
- POST /v1/workflows/:id/execute

### Admin / org
- GET /v1/organizations/:id/usage
- GET /v1/organizations/:id/audit-logs
- GET /v1/organizations/:id/metrics

## 7. Execution model

### Runtime execution loop

```text
Load agent version
  -> load context
  -> load memory
  -> call model
  -> decide tool or final response
  -> validate permission
  -> execute tool
  -> record step and result
  -> continue until max steps or timeout
```

### Required behavior
- max_steps enforced
- max_runtime enforced
- context cancellation supported
- tool timeout enforced
- invalid state transitions rejected
- retries for transient failures only
- model failure fallback path prepared
- no infinite loop

## 8. Security model

### Requirements
- password hashing with bcrypt/argon2
- API keys hashed and rotated
- RBAC centralization via permission service
- tenant isolation by organization_id on all resources
- rate limiting: IP, user, org, API key, agent
- input validation and output sanitization
- tool permission matrix enforced before execution
- audit logs for every sensitive action
- no secrets committed to repository

### RBAC roles
- OWNER
- ADMIN
- MEMBER
- VIEWER

### Permission examples
- agents.read
- agents.create
- agents.update
- agents.delete
- runs.read
- runs.execute
- runs.cancel
- tools.create
- workflows.execute
- organization.manage
- users.manage

## 9. Observability plan

### Metrics
- http_requests_total
- http_request_duration_seconds
- agent_runs_total
- agent_run_duration_seconds
- agent_run_failures_total
- tool_calls_total
- tool_call_duration_seconds
- queue_depth
- queue_latency
- model_requests_total
- model_tokens_total
- model_latency

### Tracing
Every request should trace through:
- HTTP request
- auth middleware
- database queries
- queue publish
- worker dequeue
- runtime execution
- model adapter call
- tool execution

### Logging
- structured JSON logs
- request_id included
- organization_id, user_id, run_id on relevant logs
- no raw secrets in logs

## 10. Phased implementation roadmap

**Status: phases 0-12 are DELIVERED.** The phase-by-phase plan that stood here
has been folded into the **Delivered** summary (section 1a) with package,
migration, and issue references; the residual work is the **Planned** list
(section 1b). The phase texts are preserved in git history
(`git log -- docs/agentos-implementation-plan.md`) for anyone who wants the
original sequencing rationale.

## 11. Risk areas

### 1. Runtime complexity
The biggest risk is not the UI. It is building a correct, safe, bounded agent execution loop that handles retries, cancellations, tool security, and state transitions cleanly.

Mitigation:
- implement state machine first
- ensure max step and timeout enforcement
- add race detection and integration tests early

### 2. Tool security
LLM-triggered tools are a serious production risk if not permission-scoped.

Mitigation:
- separate registry/service for tools
- explicit permission tags
- schema validation
- enforce allowlists by agent

### 3. Async reliability
Queue design and worker recovery can become fragile under partial failure.

Mitigation:
- persist tasks before processing
- use retries and DLQ
- design idempotency keys for external operations

### 4. Multi-tenancy drift
Cross-tenant leaks are a major business/legal risk.

Mitigation:
- central tenant context
- middleware-enforced filtering
- query-layer guardrails
- audit trail per resource access

## 12. Testing strategy

### Unit tests
- tool execution logic
- retry policies
- permission checks
- state transition validation
- cost calculation
- model routing policy

### Integration tests
- Postgres migrations and transactions
- Redis queue and rate limit behavior
- worker processing pipeline
- API request validation and auth

### End-to-end tests
- organization -> agent -> run -> worker -> result
- human approval flow
- queue retry and DLQ recovery
- tool call sequence with permission checks

### Race detection
Run this as part of CI:

```bash
go test -race ./...
```

This is non-negotiable for concurrency-heavy platform code.

## 13. Database and migration strategy

Use PostgreSQL as the source of truth. Keep Redis for queue and cache. Keep NATS or equivalent as asynchronous messaging. Use migration files under migrations/ and include:

- make migrate-up
- make migrate-down
- seed data only for local development

Do not use ad hoc database edits in production.

## 14. Implementation decision summary

This project should not begin by building dozens of microservices. It should begin with:

- one Go API service
- one Go worker service
- one PostgreSQL instance
- one Redis instance
- one queue transport
- one frontend app

The code should still be structured cleanly enough that the platform can later evolve into independent services when real scaling needs justify it.

## 15. Next actions

The platform phase of this plan is complete. Current next actions are the
**Planned** list in section 1b (Postgres stores for quality gates and synth
journeys/runs/evidence, synth worker orchestration + HTTP surfaces, NATS
production rollout defaults, MCP/A2A residency-guard wiring, portal CORS
preset, budget consumption endpoint, depth-bounded downward workflow
cascade). Architecture truth lives in
[architecture.md](architecture.md); flagship closure evidence in
[synthetic-assurance.md](synthetic-assurance.md).
