# AgentOS Architecture (current state)

This document describes what the codebase actually implements today. It is
updated with the code, not ahead of it — every statement below maps to an
existing package, route, migration, or test (spot-check pointers included).
Anything not fully shipped is labeled **partially implemented** or **planned**
in the [status appendix](#status-appendix) and expanded in
[synthetic-assurance.md](synthetic-assurance.md).

Verified counts (this revision): 47 packages under `internal/`, **50 Go test
packages** (46 in `internal/` + `cmd/api`, `cmd/worker`, `cmd/agentosctl`,
`loadtest`), migrations **001–037**, OpenAPI **182 paths / 531 schemas**
staged through **66 fragments**, **222 route registrations** across 52 route
files in `cmd/api`.

## Overview

AgentOS is a Go 1.25 modular monolith for multi-tenant AI-agent execution.
It ships as two long-running processes (HTTP API + worker), a React 19/Vite
dashboard, and a dual-mode persistence layer (Postgres or fully in-memory)
that is identical at the interface level. Redis and NATS are first-class
optional transports: Redis backs the task queue (`AGENTOS_QUEUE=redis`),
NATS backs both the run-dispatch plane (`AGENTOS_MESSAGING=nats|hybrid`)
and the domain-event bus. Production artifacts (Kubernetes/Helm/Kustomize/
Terraform) live under `deploy/` (issue #117), and a six-scenario load suite
lives under `loadtest/` (issue #125).

```text
                              ┌────────────────────────────────────────────┐
                              │   Clients: web SPA · CLI · SDK · CI · MCP  │
                              └───────┬───────────────────────┬────────────┘
                                      │ HTTPS / SSE           │ JSON-RPC
                                      ▼                       ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│  cmd/api :8080 (API_PORT)                                                   │
│   outer mux: /healthz /readyz (unversioned) + / → service info              │
│   /api/v1/* (canonical) and /v1/* (legacy) → StripPrefix → apiMux           │
│   apiMux: 222 registrations · 47 register*Routes helpers · Go 1.22 patterns │
│   middleware: tracing (AGENTOS_TRACING_ENABLED, default off) → metrics →    │
│               rate limit → CORS → auth.RequireAuthOrAPIKey → RBAC           │
└──────────────┬───────────────────────────────────────────────┬──────────────┘
               │ services (internal/*, 47 packages)            │
               ▼                                               ▼
┌──────────────────────────────┐   ┌───────────────────────────────────────┐
│ persistence (dual-mode)      │   │ transports                            │
│  Postgres mode: 26 services  │   │  task queue   AGENTOS_QUEUE=memory|   │
│  wired with tenant-guarded   │   │                redis                  │
│  stores (migrations 001-037) │   │  dispatch     AGENTOS_MESSAGING=      │
│  in-memory mode: same        │   │                memory|nats|hybrid     │
│  interfaces on maps          │   │  events bus   NATS JetStream or       │
│                              │   │                in-memory fallback     │
└──────────────────────────────┘   └───────────────────────────────────────┘
               ▲                                               ▲
               │ runs, callbacks, synth/evidence (seams ready) │
┌──────────────┴───────────────────────────────────────────────┴──────────────┐
│  cmd/worker — NO HTTP listener (deploy/ deliberately defines no HTTP probes)│
│   queue consumer (push/pull) · runtime.Runner bounded loop · scheduler loop │
│   webhook delivery · budgets/quota gates · graceful drain · recovery sweeper│
└─────────────────────────────────────────────────────────────────────────────┘
```

## Processes

| Command         | Purpose |
|-----------------|---------|
| `cmd/api`       | HTTP API on `API_PORT` (default `:8080`, `internal/config/config.go:70`): versioned routes, auth/RBAC, SSE streaming, metrics, admin surfaces |
| `cmd/worker`    | Run execution: consumes the task queue, drives `runtime.Runner`, scheduler + webhook delivery loops, budget/quota policy gates, stale-run recovery, graceful drain. Logs its configured `WORKER_PORT` (`:8081` default) but serves no HTTP — status flows back to the API via `POST /runs/{id}/events` |
| `cmd/migrate`   | Applies/rolls back `migrations/*.sql` (001–037, auto-discovered, tracked in `schema_migrations`) |
| `cmd/seed`      | Idempotent demo data (org/user/agents/workflow/runs/eval dataset) |
| `cmd/agentosctl`| CLI over the typed SDK (`internal/sdk`) — agents, runs, billing, secrets, marketplace, connectors, SSO, workflows, usage |
| `cmd/healthcheck` | Static probe binary for the distroless images |
| `cmd/sandbox-exec` | Process-isolated tool sandbox launcher (rlimits, env scrubbing) |

## API surface

- One internal `apiMux` is mounted twice (`cmd/api/main.go:677-681`):
  `/api/v1/...` (canonical, used by the web dashboard) and `/v1/...`
  (legacy). `/healthz` and `/readyz` stay unversioned; `GET /metrics` and
  every domain route are served under both prefixes.
- 222 `Handle`/`HandleFunc` registrations across 52 non-test files:
  `register*Routes` helpers (47 call sites in `main.go`, 55 definitions)
  plus a handful of inline registrations (core auth/agents/runs/queue).
- The contract is `api/openapi.yaml` — **182 paths / 531 schemas**,
  regression-tested by `internal/apispec` (dangling `$ref`, duplicate
  `operationId`, or a path without operations fails CI). Incremental
  surfaces are staged as schema/path fragments under `api/fragments/`
  (66 files) and merged by the orchestrator.

## Multi-tenancy and auth

- Every tenant-facing store query is scoped by `organization_id`; tenant
  identity is resolved from HMAC-signed tokens (`Authorization: Bearer`) or
  API keys (`X-API-Key`), never from client-supplied org IDs. Cross-tenant
  lookups are indistinguishable from missing records (404, no existence
  leak) — the rule is stated and enforced per store (e.g. `internal/synth/store.go`).
- Passwords are bcrypt-hashed; tokens are HMAC-SHA256 signed JSON claims
  with expiry (`internal/auth`). Single-use refresh + server-side
  revocation deny-list (SCIM deprovisioning cannot refresh).
- RBAC roles OWNER/ADMIN/MEMBER/VIEWER gate routes through
  `auth.RequireAuthOrAPIKey` + `auth.RequirePermission`; per-vertical
  permission catalogs live in `internal/auth/permissions_*.go` (including
  `permissions_synth.go`, `permissions_qualitygates.go`,
  `permissions_simulation.go`, `permissions_configgen.go`).
- Agent-scoped `ak_` keys carry tool allow-lists enforced at the runtime
  tool seam (`internal/apikeys/agent_keys.go`); model gateway `vk_` virtual
  keys carry model allow-lists + monthly spend caps (`internal/models/gateway.go`).
- Production guard: `APP_ENV=production` without `JWT_SECRET` refuses to
  boot (`cmd/api/prodguard.go`).

## Persistence: dual-mode everywhere

The house pattern: `NewService()` → in-memory maps; `NewServiceWithStore(...)`
→ Postgres store is the source of truth. **Both modes run the same service
code and the same tests** (in-memory unit tests, `sqlmock` store tests,
`miniredis` for the Redis queue, embedded `nats-server` for the JetStream
adapter).

Wired Postgres stores in `cmd/api/main.go` (Postgres mode): auth, api-keys,
organizations, audit (+ hash chain), residency policies, **synth registry**,
agents, runs, workflows (+ durable node store), approvals, agent versions,
deployments/canaries, scheduler, knowledge, memory, billing, SCIM identities,
marketplace (+ signing), connectors, MCP registry, A2A cards, evaluations
(+ adversarial), events, webhooks, model gateway, rate limiter.

**In-memory in both modes (documented exceptions):**

| Subsystem | Status |
|---|---|
| Quality gates (`internal/qualitygates`) | `NewInMemoryPolicyStore` + `NewInMemoryEvidenceStore` wired unconditionally (`cmd/api/main.go:389`); Postgres policy store is a pre-assigned follow-up (no migration yet) |
| Batch ops (`internal/batch`), simulation (`internal/simulation`), orchestration (`internal/orchestration`), configgen (`internal/configgen`) | In-memory services, no store seam wired |
| Synth journeys / runs / evidence bundles | Registry (`synth_*` migration 034) and findings (row-per-occurrence `synth_findings`, `internal/synth/store_findings.go`) have Postgres stores; the run/journey/evidence tracks default to in-memory (`internal/synth/findings.go:577`), with `PostgresFindingStore` implemented but not yet wired into `main.go` |
| Evidence artifact storage | `EvidenceStore` seam ready (in-memory impl; production object-storage backend pending) — `internal/synth/evidence_store.go` |
| SSO config store | In-memory config store; identities follow the auth store mode |

Schema: migrations **001–037** (`migrations/`), applied by `cmd/migrate`;
033 adds pgvector (`knowledge_chunks.embedding_vec`), 034 adds the synth
registry tables, 035 knowledge episodes, 036 rate limits, 037 run checkpoints.

## Event transport (three planes, honestly labeled)

1. **Task queue** — `AGENTOS_QUEUE=memory` (default) | `redis`
   (`internal/queue/backend.go`). Redis mode is wired into BOTH `cmd/api`
   and `cmd/worker` and tested against `miniredis`.
2. **Run-dispatch plane** — `AGENTOS_MESSAGING=memory` (default) | `nats` |
   `hybrid` (`internal/queue/dispatch.go:48`, worker side
   `cmd/worker/messaging.go`, issue #116):
   - `memory`: tasks flow through the platform queue (today's default).
   - `nats`: run creation publishes a `RunEnvelope` onto JetStream
     (`AGENTOS_TASKS` stream); workers consume durably.
   - `hybrid`: same as `nats` with automatic fallback to the memory queue
     when the broker is unavailable.
   - The real client adapter (`internal/queue/jetstream_nats.go`) targets
     `github.com/nats-io/nats.go` — **a direct dependency in `go.mod`
     (v1.53.1)** — and is verified against an embedded `nats-server` in
     `internal/queue/jetstream_nats_test.go`. The honest status: the code
     path is complete and tested, but `memory` remains the default and no
     deployment defaults/ops runbook ship with `nats` enabled yet (see
     [status appendix](#status-appendix)).
3. **Domain-event bus** — `events.NewFromEnv` (`cmd/api/main.go:438`):
   NATS JetStream publisher (`AGENTOS_EVENTS` stream, `agentos.events.<type>`
   subjects, `internal/events/nats.go`) when `AGENTOS_NATS_URL` is set and
   reachable; in-memory/no-op fallback otherwise. A Postgres-backed events
   store behind the same `AuditPublisher` chain powers `GET /events`.
   `internal/events/nats_embedded_test.go` tests the real broker path.

Webhook deliveries ride their own signed-HMAC retry worker
(`internal/webhooks`); notification subscriptions fan operational alerts
(run failures / approvals / canary rollbacks) over that pipeline.

## Run execution

- `POST /runs` validates the agent + org access, creates a `QUEUED` run and
  dispatches per the messaging plane above.
- The worker marks the run `RUNNING`, executes `runtime.Runner.RunWithID`
  (bounded: max steps, wall-clock + per-tool timeouts, loop detection,
  context cancellation), records each step through a `StepRecorder`, and
  posts transitions back to the API with retry/backoff. Without
  `OPENAI_API_KEY` the worker runs a deterministic offline mode so local
  development needs no model credentials.
- **Full lifecycle** (issue #115): `POST /runs/{id}/pause|resume|cancel`
  with an enforced state machine, durable per-step checkpoints
  (migration 037), a stale-run recovery sweeper, and `run.paused/resumed/
  cancelled` events. Pausing/cancelling a workflow-node run cascades
  best-effort UP to the owning workflow run
  (`internal/runs/lifecycle.go:540-604`); cascading DOWN from a workflow
  run to its node runs, and any configurable cascade depth, are **planned**.
- **Batch operations** (issue #123): `/batch/*` bulk runs/workflows/
  updates/cancels with per-item results, SSE progress, and partial-failure
  isolation (`cmd/api/batch.go`, `internal/batch`).
- **Run trace exposure** (issue #114): `GET /runs/{id}/trace` links the
  run's W3C trace; step-level trace hooks live in
  `internal/runtime/trace_hooks.go`.

## Model routing + resilience

- `models.Provider` abstraction; `models.NewOpenAIProvider` speaks any
  OpenAI-compatible HTTP API (`OPENAI_BASE_URL` targets OpenRouter/Groq/
  Ollama/vLLM; model via `AGENTOS_WORKER_MODEL`).
- **Failover**: `models.NewFailoverProvider(primary, fallback)` chain.
- **Advanced router** (issue #112): deterministic policies —
  cost / latency / complexity / round-robin / weighted — over a capability
  registry (`internal/models/router_policies.go`, `capabilities.go`), with
  per-provider circuit breakers (`circuitbreaker.go`).
- **LLM response resilience**: capability detection, JSON negotiation +
  bounded repair, reasoning-wrapper stripping, token-limit retry with
  backoff/jitter (`internal/models`, runtime resilience hooks).
- **Virtual keys**: `vk_` gateway keys with model allow-lists and enforced
  monthly spend caps — 402 `spend_limit_exceeded` before the provider call
  (`internal/models/gateway.go`); the worker consumes them through the
  `GuardedProvider` seam.
- **Embeddings** (issue #113): `AGENTOS_EMBEDDING_PROVIDER=openai|offline`
  (`internal/knowledge/embedding_provider.go:25`). `openai` = OpenAI-compatible
  `/v1/embeddings` client (batching, 429/5xx retry honoring `Retry-After`,
  per-model dimension table: `text-embedding-3-small` 1536d default,
  `text-embedding-3-large` 3072d, `ada-002` 1536d). `offline` (default, or
  any unknown value — fail-safe) = deterministic hash embedder.
  Vectors land in pgvector (migration 033) with HNSW search; hybrid
  keyword+vector fusion with RRF re-ranking (`internal/knowledge/hybrid.go`).

## Knowledge, memory, and the graph layer

- RAG: document ingestion → chunking → embeddings → semantic search with
  citations (`internal/knowledge`); HTTP surface `POST/GET
  /knowledge/documents`, `POST /knowledge/search`.
- **Knowledge graph** (issues #130–#132): ontology-first construction
  (deterministic + LLM schema generation with naming enforcement,
  `internal/knowledge/ontology.go`), episode ingestion with time-decay
  relevance and temporal queries (`episodes.go`, migration 035), tiered
  retrieval tools (`quick_search` / `panorama_search` / `insight_forge`)
  registered as agent tools (`retrieval_tools.go`), and graph lifecycle
  locks (read leases + exclusive write locks, `leases.go`). Status: engine
  complete and tested; no dedicated HTTP routes yet (the
  `knowledge-graph` fragment is schema-only) — **partially implemented**.
- Agent memory snippets (`GET/PUT /memory`) with the same provider-backed
  embedding path.

## Workflows and approvals

- JSON-DSL workflows (`internal/workflows/dsl.go`) with conditional
  execution (if/switch/loop/retry/wait-until over a safe stdlib expression
  engine, `conditions.go`), true parallel branches (all/any/race/majority,
  `parallel.go`), and `POST /workflows/validate` (issue #109).
- **Durable execution**: checkpointed node runs with recovery/watchdog
  (`durable.go`, `store_durable.go`, migration 013); the node timeline is
  exposed at `GET /workflow-runs/{id}/nodes`.
- Human-in-the-loop approvals (`internal/approvals`) with enterprise
  oversight (issue #137): delegation chains with expiry + lazy return,
  escalation SLAs with an idempotent sweep, two-person dual control
  (`dual_control_self_approval`), byte-deterministic evidence export.
- Cascade behavior: see [Run execution](#run-execution) — upward
  best-effort today; depth-bounded downward cascade **planned**.

## Synthetic assurance (flagship, issues #147–#156)

The safe-autonomous-testing subsystem: tenant-scoped registry (projects,
verified targets, environments with a **production read-only default**,
9 built-in personas) → isolated browser + API test workers (bounded state
machines, evidence capture with redaction) → versioned journeys with
mandatory explicit assertions → evidence bundles + finding lifecycle with
stable-fingerprint dedup and independent reproduction → GitHub findings
sync + regression-test generation → unified release quality gates
(evaluations + journeys + adversarial security + canary health).

Delivery status per stage — plus the #118 closure mapping with issue
references — lives in [synthetic-assurance.md](synthetic-assurance.md).
Short form: the registry and quality-gate surfaces are live HTTP
(`/synth/*`, `/quality-gates/*`); workers, journeys, findings, GitHub sync,
exploration, scheduling, and comparison are complete, tested Go engines
(`internal/synth`) whose HTTP/worker orchestration wiring is **partially
implemented** (fragments are schema-only by design; the Postgres findings
store is ready but unwired).

## Agent-to-agent surface

- **Orchestration** (issue #108): comm bus with org-scoped inboxes,
  delegation protocol with depth limits + approval routing, supervisor
  strategies (round-robin / least-loaded / capability-match) —
  `/orchestration/*` (in-memory state).
- **A2A protocol** (issue #136): Agent Card registry with canonical card
  JSON, governed inbound messages that create real runs, policy-gated
  outbound delegation with audited `delegation_denied` verdicts.
- **MCP gateway** (issues #111/#82): every tenant's tool registry over
  JSON-RPC 2.0 (`POST /mcp`), plus an org-scoped MCP client registry
  (`/mcp-servers`) with health-checked registration, toolset fingerprints +
  drift history, version pinning with rug-pull detection, and an opt-in
  prompt-injection arg scanner.
- **Residency** (issue #140): org region pinning with data-flow-aware
  endpoint classification, strict fail-closed mode, `residency_violation`
  audit events, posture surface. The `tenant.Guard` seam
  (`internal/tenant/residency.go:451`) is ready for MCP/A2A registries to
  consume — that wiring is **planned**.

## Other platform subsystems

| Area | Where | Notes |
|---|---|---|
| Rate limiting (issue #124) | `internal/limiter`, `/rate-limits` | org → agent → tool → model hierarchy, RPM/RPD/TPM sliding windows on Redis + in-memory, 429 `rate_limited` + Retry-After at run/tool/model seams |
| Budgets (wave-8) | `internal/billing/budgets.go`, `/budgets` | org/agent monthly limits on runs and cost, dry-run `GET /budgets/evaluate`, 402 `budget_exceeded` at run creation + worker pre-execution; a consumption-report endpoint is **planned** |
| Billing/meters (meters: issue #57) | `internal/billing` | plans, subscriptions, enforced quota gates, invoices reconciled with the runs cost ledger, usage meters + per-tenant margin, optional Stripe usage-record sync (no-op without a key), showback/chargeback (`/showback`) |
| Simulation (issue #133) | `internal/simulation`, `/simulations` | social/market/network adapters, deterministic seeded rounds, emergence heuristics (in-memory state) |
| ReACT (issue #134) | `internal/runtime/react.go` | `ReACTRunner` wraps the existing runtime — multi-tool steps, reflection rounds with quality self-assessment, long-output sectioning; policy/limiter seams untouched |
| Configgen (issue #129) | `internal/configgen`, `POST /config/generate` | natural-language → validated agent/workflow drafts, step-by-step generation with progress |
| Adversarial evals (issue #153) | `internal/evaluations/adversarial*.go` | prompt injection, permission bypass, cross-tenant probes, secret exposure — asserting enforcement at control boundaries, never model refusal |
| Compliance (issue #138) | `internal/compliance` | control mapping (SOC 2 TSC / ISO 42001 / EU AI Act / NIST AI RMF) with live evidence pointers, Annex-IV-style technical documentation, deterministic evidence bundles |
| Audit chain | `internal/audit/chain.go` | tamper-evident hash chain (`GET /audit/verify`) + deterministic JSONL evidence export with a WORM-anchor head hash |
| Sandbox | `internal/sandbox`, `cmd/sandbox-exec` | opt-in process isolation: rlimits, output caps, env scrubbing; database/search/code/webhook/JSON/file tools in `internal/tools` |

## Observability

- `GET /metrics` (auth + `runs.read`): Prometheus text 0.0.4 (hand-rolled
  in `internal/observability` — no client_golang dependency; bucketed
  p50/p95/p99 histograms) and `?format=json` legacy payload.
- **Tracing** (`AGENTOS_TRACING_ENABLED`, default off, garbage-off —
  `internal/observability/tracing.go:47`): when disabled the middleware
  returns the next handler unchanged at construction time
  (`cmd/api/middleware_tracing.go`), so the serving chain is byte-identical
  and the per-request cost is one atomic load. When enabled: OTel SDK with
  OTLP export, W3C propagation, HTTP server spans outermost (covering
  rate-limit rejections), runtime/tool trace hooks, run-trace exposure.
- `/healthz`, `/readyz` unversioned probes (the ones the deploy artifacts
  reference — the worker has no HTTP probes by design, `deploy/README.md`).
- **Load testing** (issues #80/#125): `loadtest/` — pure-stdlib `agentload`
  CLI (`run|record|compare|list`) with six scenarios (`sustained`, `spike`,
  `soak`, `concurrency`, `workflow`, `lifecycle`), `/metrics` resource-trend
  sampling, JSON baselines (`baselines/sustained-inmemory.json`) and a CI
  regression gate (p95 +10% / errors +2pp). `make loadtest-*` targets;
  methodology in `docs/load-testing.md`.

## Deployment topology

| Target | Artifacts |
|---|---|
| Dev | `docker-compose.yml` (Postgres 16, Redis 7, NATS 2.10) + `make run-api` / `make run-worker` / web dev server |
| Single-host prod | `docker-compose.prod.yml` — api + worker + web + data tier + one-shot migrate; distroless images (`Dockerfile.api/worker/web`), SPA proxy with SSE support (`deploy/nginx.conf`) |
| Kubernetes | `deploy/kubernetes/` — 23 manifests: Deployments (api/web/worker, HPAs, PDBs), Services, Ingress, StatefulSets (postgres/redis/nats), `migrate-job`, NetworkPolicies (default-deny data tier), ServiceAccounts, ConfigMap + secret template |
| Helm | `deploy/helm/agentos/` — chart with 18 templates (incl. optional OTEL sidecar ConfigMap), values-dev/prod |
| Kustomize | `deploy/kustomize/` — base (21 resources) + dev/staging/production overlays (replica/HPA/ingress/config patches) |
| Terraform | `deploy/terraform/` — modules (agentos-api, agentos-worker, database, redis, nats, aws/ecr + aws/irsa) × dev/staging/production environments |
| Validation | `deploy/validation/validate_*.py` — offline structural checks for all of the above |

Flow (prod): Ingress → web (SPA, `/api` same-origin proxy) → api `:8080`;
worker consumes the configured queue and calls the model provider; status
callbacks go worker → api (`AGENTOS_API` + API key); domain events flow
through NATS JetStream when configured. Reference layout and the
no-HTTP-probe rule for the worker are documented in `deploy/README.md`.

## Web dashboard and portal

`web/` is a React 19 + Vite + TypeScript app consuming the real API through
a typed client (`/api/v1` prefix) with SSE-driven run updates — including
the visual workflow builder (SVG DAG canvas), visual agent builder,
trace viewer, cost analytics, and ops/DLQ panels. `docs/portal/` is the
offline-generated developer portal (API reference over the full spec,
playground, SDK/CLI quickstarts); wiring a CORS preset for a hosted
playground origin (`AGENTOS_CORS_ORIGINS` allowlist exists in
`cmd/api/cors.go`) is **planned**.

## Local development loop

```bash
make docker-up     # Postgres, Redis, NATS (optional — zero-infra mode boots without it)
make migrate-up    # apply migrations 001-037
make seed          # demo org/user/agents/tool/workflow/runs/eval dataset
make run-api       # terminal 1
make run-worker    # terminal 2
cd web && npm run dev   # terminal 3 (dashboard)
```

`make lint` (gofmt + go vet) and `go test ./...` (50 packages, race job in
CI) are the correctness gates; tests never require running infrastructure.

## Status appendix

**Implemented** (highlights): dual-mode persistence across 26 wired stores;
222-route API under dual mounts; contract-first OpenAPI (182/531) with CI
validation; full run lifecycle + checkpoints + recovery; batch ops; Redis
queue; NATS domain-event bus with embedded-broker tests; model router +
circuit breakers + virtual keys; embeddings (openai|offline) + pgvector +
hybrid search; workflows (control flow, parallel, durable); approvals with
oversight; rate limiting; budgets + billing + showback; audit hash chain +
compliance packs; SSO/SCIM; marketplace signing; connectors; MCP gateway +
registry; A2A; orchestration; simulation; ReACT; configgen; deploy
artifacts (k8s/helm/kustomize/terraform); six-scenario load suite;
synthetic-assurance registry + findings Postgres stores and quality-gate
engine (details in [synthetic-assurance.md](synthetic-assurance.md)).

**Partially implemented**:
- Synth tracks beyond the registry (#148–#156): complete/tested engines,
  schema-only fragments; worker orchestration and HTTP surfaces for
  journeys/runs/findings/GitHub sync pending.
- `PostgresFindingStore` implemented, not wired into `cmd/api`.
- Knowledge-graph engine (ontology/episodes/tiered retrieval) without a
  dedicated HTTP surface.
- Workflow cascade: upward best-effort only.

**Planned**: Postgres stores for quality gates and synth journeys/runs/
evidence (+ object-storage evidence backend); NATS `nats|hybrid` as a
documented production default (adapter already complete); MCP/A2A
residency-guard wiring over the `tenant.Guard` seam; portal playground
CORS preset; budget consumption endpoint; depth-bounded downward workflow
cascade; soak leak detection via `/debug/pprof` (currently metric-trend
based — `loadtest/README.md`).
