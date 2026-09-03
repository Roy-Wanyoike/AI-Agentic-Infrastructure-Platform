# Wave-3 API Contract (binding)

Wave-3 dispatches five parallel tracks. This document is the binding contract:
endpoints, JSON shapes, migration numbers, file ownership, and ground rules.
It exists so tracks can be built in parallel without collisions and merged
without rework.

## Ground rules (same as wave-2, all binding)

1. **Never edit** `cmd/api/main.go`, `cmd/worker/main.go`, or
   `api/openapi.yaml`. Deliver exact wiring lines in `docs/wiring/<track>.md`
   instead — the orchestrator applies them at integration time.
2. **OpenAPI**: new paths go in a standalone-valid OpenAPI 3.1 fragment at
   `api/fragments/<track>.yaml`. Every operation carries
   `x-required-permission`. Shared components use a track-unique schema
   prefix (e.g. `Usg*`, `Mem*`, `Knb*`, `Vdf*`). All local `$ref`s must
   resolve inside the fragment. The orchestrator merges fragments into
   `api/openapi.yaml`.
3. **Migration numbers are pinned**: 3-b → `012_cost_tracking.sql`,
   3-c → `013_durable_workflows.sql`, 3-d → `014_memory_knowledge.sql`.
   Follow the style of `migrations/006_workflows_approvals.sql`
   (`CREATE TABLE IF NOT EXISTS`, idempotent, forward-only).
4. **Dual-mode persistence**: every service keeps
   `NewService()` (in-memory, zero-infra) and gains/keeps
   `NewServiceWithStore(db *sql.DB)` (Postgres). Mirror the store patterns in
   `internal/evaluations/store.go` / `internal/deployments/store.go`.
5. **Tests are mandatory** and use the existing conventions: `sqlmock` for
   stores, `miniredis` for Redis, table-driven service tests, no docker, no
   external services. Gate: `gofmt -l`, `go vet ./...`,
   `go build ./...`, `go test -count=1 ./...` all green.
6. **RBAC + tenancy**: every endpoint sits behind
   `auth.RequireAuthOrAPIKey` + `auth.RequirePermission`; every store query
   filters by `org_id`. Machine error codes are UPPER_SNAKE in the standard
   error envelope.
7. **Zero new runtime dependencies** unless essential; if `go.mod` changes,
   justify it in the wiring doc (test-only deps like an embedded broker are
   acceptable when documented).
8. Update `docs/wiring/<track>.md` with: exact `main.go` import + constructor
   + route-registration lines (BEFORE/AFTER style like wave-2 docs), endpoint
   table with permissions, and a "Deviations" section for anything you had to
   assume.
9. **File ownership** below is exclusive. If you believe you must touch a
   file you do not own, stop and document the need in your wiring doc — do
   not edit it.

## File ownership map

| Area | Owner |
|---|---|
| `internal/config`, `internal/queue`, `internal/events`, README env table | **3-a only** |
| `internal/usage`, `internal/runs`, `internal/evaluations`, `internal/models/pricing.go` (new), `migrations/012*` | **3-b only** |
| `internal/workflows`, `internal/orchestration`, `migrations/013*` | **3-c only** |
| `internal/memory`, `internal/knowledge` (new), `migrations/014*` | **3-d only** |
| `web/**`, `cmd/api/versions.go`, `cmd/api/versions_test.go`, versions service file(s) | **3-e only** |
| `internal/observability` | nobody (read-only) |
| `cmd/api/handlers.go` | nobody (read-only; add NEW handler files in `cmd/api/` per track instead) |

Note: handler files under `cmd/api/` that you own are NEW files (e.g.
`cmd/api/usage_costs.go` owned by 3-b). Do not edit `handlers.go` — route
registration happens in `main.go` via the wiring doc.

## Track 3-a — Redis queue wiring + events NATS verification

Branch: `feat/w3-redis-queue` · Fragment: none · Migrations: none

1. `internal/config`: add queue mode selection
   (`AGENTOS_QUEUE=memory|redis`, default `memory`, plus
   `REDIS_ADDR`/`REDIS_QUEUE_KEY` knobs consistent with `.env.example`).
   Expose a config-driven constructor helper in `internal/queue`
   (e.g. `queue.NewFromConfig(cfg)` returning `queue.Queue`) so `main.go`
   wiring is a one-liner. `RedisQueue` (`queue.NewRedisQueue(addr)`) is
   already implemented and tested — do not rewrite it; wire it.
2. Tests: constructor selection with `miniredis` (redis mode round-trips one
   task through the queue interface), memory default, invalid mode error.
3. `internal/events` NATS path: verify the publish/subscribe wiring against
   an embedded or in-memory NATS server if a test-only dependency is
   reasonable (`nats-server/v2` as a test dep is acceptable if documented);
   otherwise unit-test the subject naming + payload shape and write a manual
   verification script in the wiring doc. Document which you did.
4. README: add the new env vars to the env table section (README env table
   is 3-a's; no other README edits).
5. `docs/wiring/redis-queue.md`: exact `main.go` lines for API + worker
   (BEFORE/AFTER), graceful-shutdown note (drain/close Redis client).

## Track 3-b — Cost tracking & pricing hook

Branch: `feat/w3-cost-tracking` · Fragment: `api/fragments/cost.yaml` ·
Migration: `012_cost_tracking.sql`

1. `internal/models/pricing.go` (NEW): `ModelPricing{Model string;
   InputPerMTokens, OutputPerMTokens float64}` (USD per 1M tokens) +
   `ComputeCostCents(model string, promptTokens, completionTokens int)
   float64` with a small built-in table for common OpenAI-compatible models
   and env/JSON override (`AGENTOS_PRICING_JSON`). Unknown model → 0 cents +
   documented behavior (never fail a run for pricing).
2. `internal/runs`: run steps already record usage where available. Add
   `cost_cents` (double precision) to run steps and runs totals in migration
   `012`; extend the runs store (sqlmock tests) to persist/sum it; add
   `TotalCostCents` to run summary JSON (additive, snake_case).
3. `internal/evaluations`: after each case, compute cost from the completion
   usage via the pricing hook and set `CostCents` (currently pinned at 0 —
   update `service_test.go` accordingly: the test asserting `want 0` must
   become a real assertion against computed pricing). Wire an optional
   provider/usage source via a setter (e.g. `AttachUsageSource`) so the eval
   runner can compute real costs when the API process has a provider
   configured — wiring lines in the wiring doc (you do not edit `main.go`).
4. New endpoint `GET /v1/usage/costs?from=&to=&group_by=day|agent|model`
   (behind `usage.read`): aggregates `cost_cents` from runs for the org over
   the window. Response shape (snake_case):
   `{"total_cost_cents": 0, "series": [{"bucket": "2026-09-03",
   "agent_id": "…", "model": "…", "cost_cents": 0, "runs": 0}]}`
   (`bucket` present when `group_by=day`; `agent_id`/`model` present for the
   other groupings; unknown `group_by` → 400 `INVALID_GROUP_BY`).
   Handler in NEW file `cmd/api/usage_costs.go` (3-b-owned) + sqlmock-backed
   aggregate tests.
5. Fragment `api/fragments/cost.yaml`: path `/usage/costs`, components
   prefix `Usg*`, `x-required-permission: usage:read` (match the actual
   permission constant name you find in `internal/auth`).
6. `docs/wiring/cost.md`: constructor + registration lines, pricing env
   knobs, eval-runner provider wiring lines.

## Track 3-c — Durable workflow execution

Branch: `feat/w3-durable-workflows` · Fragment:
`api/fragments/durable-workflows.yaml` · Migration:
`013_durable_workflows.sql`

Problem: workflow node execution is currently "queued runs" — a worker
crash loses in-flight workflow runs; node state is not checkpointed
idempotently; there is no recovery or stale-run watchdog.

1. Migration `013`: harden `workflow_runs`/`workflow_node_runs` (add
   `attempt`, `locked_at`, `heartbeat_at`, `finished_at`, `error_code`
   columns as needed; `CREATE TABLE IF NOT EXISTS` + additive
   `ALTER TABLE ... ADD COLUMN IF NOT EXISTS` only).
2. Make node transitions checkpoint synchronously and idempotently: a node
   run that already reached a terminal state is never re-executed on replay
   (idempotency keyed by `workflow_run_id` + node id + attempt).
3. Recovery: `workflows.Service` gains
   `RecoverIncompleteWorkflowRuns(ctx, orgID string) (recovered int, err
   error)` — finds non-terminal `workflow_runs` whose `heartbeat_at` is
   older than the staleness threshold (config knob
   `AGENTOS_WORKFLOW_STALE_AFTER`, default 5m), marks orphaned node runs
   failed with `NODE_ORPHANED`, and re-enqueues the next pending node via the
   existing queue interface. Must be safe to call concurrently (row-level
   locking via `SELECT ... FOR UPDATE SKIP LOCKED` in the Postgres store;
   sqlmock tests for the queries + service-level tests for the state
   machine).
4. Watchdog: same recovery pass times out runs exceeding their deadline
   (`timeout` status, machine code `WORKFLOW_RUN_TIMEOUT`).
5. Endpoint `GET /v1/workflow-runs/{id}/nodes` (behind
   `workflows:read`): returns the checkpointed node timeline (id, node_id,
   status, attempt, started/finished, error_code). Response snake_case:
   `{"nodes": [{"id": "…", "node_id": "…", "status": "succeeded",
   "attempt": 1, "started_at": "…", "finished_at": "…",
   "error_code": null}]}`. Handler in NEW file
   `cmd/api/workflow_run_nodes.go`.
6. Fragment with `Wf3*` component prefix; `docs/wiring/durable-workflows.md`
   with the `cmd/worker` recovery-loop lines (startup + ticker).

## Track 3-d — Memory persistence + knowledge/RAG foundation

Branch: `feat/w3-memory-knowledge` · Fragment:
`api/fragments/knowledge.yaml` · Migration: `014_memory_knowledge.sql`

1. Migration `014`: `memory_snippets` (org_id, agent_id nullable, scope
   short_term|long_term, content, importance, expires_at, embedding jsonb
   nullable, created/updated) and `knowledge_documents` + 
   `knowledge_chunks` (org_id, document_id, ordinal, content, embedding
   jsonb nullable, source, metadata jsonb). No pgvector requirement —
   embeddings stored as JSON float arrays; similarity computed in Go.
2. `internal/memory`: keep the in-memory service, add a Postgres store
   (sqlmock tests) + embedding-aware retrieval (cosine similarity over the
   org-scoped candidate set; short-term expiry honored).
3. `internal/knowledge` (NEW package): document ingestion pipeline —
   create document → chunk (≈800 chars, 15% overlap, on chunk boundaries) →
   embed chunks via an OpenAI-compatible `/embeddings` HTTP client
   (`AGENTOS_EMBEDDING_BASE_URL`/`AGENTOS_EMBEDDING_MODEL`, own minimal
   client in the package — do NOT modify `internal/models`) → store chunks.
   Retrieval: `Search(ctx, orgID, query, k)` → top-k chunks with score +
   document citation. Offline mode: no embedding config → deterministic
   hashing-based pseudo-embeddings so search still works in tests/dev
   (documented deviation).
4. Endpoints (handler files `cmd/api/knowledge.go`, `cmd/api/memory.go`):
   - `POST /v1/knowledge/documents` (`knowledge:write`) → 201
   - `GET /v1/knowledge/documents` (`knowledge:read`) → list
   - `POST /v1/knowledge/search` (`knowledge:read`) →
     `{"results": [{"document_id": "…", "chunk_ordinal": 3, "content": "…",
     "score": 0.83, "citation": "…"}]}`
   - `GET /v1/memory?agent_id=` / `PUT /v1/memory` (`memory:read` /
     `memory:write`) → org/agent-scoped snippets CRUD-light
5. Permission constants: if `internal/auth` lacks `knowledge:*` /
   `memory:*` permissions, document the closest existing permissions you
   used instead (do not edit `internal/auth` — orchestrator decision needed;
   suggested fallback: `tools:read`/`tools:write` documented in wiring doc).
6. Fragment with `Knb*`/`Mem*` prefixes; `docs/wiring/knowledge.md`.

## Track 3-e — Version diff API + missing frontend views

Branch: `feat/w3-versions-ui` · Fragment:
`api/fragments/versions-diff.yaml` · Migrations: none

1. Backend: `GET /v1/agents/{agentID}/versions/diff?from={n}&to={m}` —
   structured, field-level diff between two published agent versions.
   Locate the versions service (grep `versions` under `internal/`; wave-2
   shipped versions under migration 007). Response snake_case:
   `{"agent_id": "…", "from": 3, "to": 4,
   "fields": [{"field": "model", "from": "gpt-4o", "to": "gpt-4o-mini",
   "changed": true},
              {"field": "system_prompt", "from": "…", "to": "…",
   "changed": true},
              {"field": "tools", "from": ["a"], "to": ["a","b"],
   "changed": true}]}`
   Comparable fields: model, system_prompt, temperature/params, tools,
   description. Handler + test in `cmd/api/versions.go` /
   `cmd/api/versions_test.go` (3-e-owned); service method in the versions
   service file(s) you find. 404 `VERSION_NOT_FOUND` for unknown from/to.
2. Frontend (React 19 + Vite + TS, zero new deps, follow 2-g patterns in
   `web/src/views/` + `lib/api/` + `lib/hooks.ts` + RBAC gating via
   `lib/rbac.ts`; no mocks — real API calls only):
   - **Versions & deployments view**: version list, publish, diff viewer
     (side-by-side for changed fields against the new diff endpoint),
     deployments per environment with promote/rollback (RBAC: write-gated)
   - **Policies view**: list + evaluate form (input JSON → decision,
     matched policy) with OWNER/ADMIN gating matching the API
   - **Schedules view**: list + create (cron, agent, input), pause/resume
   - **Webhooks view**: list + create (URL, events, secret shown once),
     delivery list with status
   - Nav entries behind the same RBAC capability props as wave-2; keep the
     demo-section discipline (no fake data).
3. Gates: `npm run build` + `npm run lint` green; Go gates green for the
   diff endpoint; fragment valid + refs resolve.
4. `docs/wiring/versions-diff.md`: registration lines + response contract
   notes for the frontend.

## Merge order (orchestrator)

3-a → 3-b → 3-c → 3-d → 3-e (backend before frontend; 3-e's diff endpoint
lands with the UI that consumes it). Fragments merged into
`api/openapi.yaml` at integration; wiring docs applied to `main.go` /
`cmd/worker/main.go` at integration.
