# Wave 2 — API Contract & Parallel Work Rules

This document is the single source of truth for wave-2 feature tracks. Every agent
implements EXACTLY the endpoints/JSON shapes below. Do not invent alternative
paths or field names. If something is missing, note it in `docs/wiring/<track>.md`
instead of deviating silently.

## Ground rules for every track (NON-NEGOTIABLE)

1. Work ONLY inside your assigned worktree and branch. Never touch another track's files.
2. Go toolchain: `export PATH=/tmp/go/bin:$PATH` (Go 1.25 installed at /tmp/go).
3. NEVER edit these shared files (orchestrator owns them):
   - `cmd/api/main.go`, `cmd/api/handlers.go`, `cmd/api/agents.go`, `cmd/api/runs.go`
   - `api/openapi.yaml` (write your spec fragment to `api/fragments/<track>.yaml` instead)
   - `migrations/README.md` (orchestrator updates it)
   - `internal/auth/service.go`, `internal/auth/middleware.go` (add permissions in your own new file, see below)
   - `go.mod` / `go.sum` (exception: track 2-e may add `github.com/nats-io/nats.go` ONLY)
   - `web/` (exception: track 2-g owns all of `web/`)
4. NEVER commit directly to `main`. Commit to your track branch. Commit early and
   often — after the store works, after the service works, after handlers, after tests.
5. Follow the dual-mode service pattern used across the repo:
   `NewService()` (in-memory maps+mutex, zero-infrastructure mode) AND
   `NewServiceWithStore(NewPostgresStore(db))` (Postgres). Both must be tested
   (in-memory with unit tests, Postgres store with `sqlmock`).
6. Tests must NOT require running infrastructure. Use in-memory fakes, `sqlmock`,
   `miniredis` (all already in go.mod).
7. Handlers live in NEW files under `cmd/api/` named `<track>.go` with handler
   funcs + a route registration function (see "Wiring convention").
8. New RBAC permissions go in a NEW file `internal/auth/permissions_<track>.go`
   containing constants + an `init()` that appends them to `rolePermissions`
   (package-level map in `internal/auth/service.go`). Exact constant names and
   role grants are pinned below. Do NOT edit the map's definition file.
9. Every track runs before finishing: `gofmt -w ./cmd ./internal`, `go build ./...`,
   `go test ./...` — all green — and commits.
10. Append your record to `/home/z/my-project/worklog.md` (read it first; append
    only; use the standard Task ID template).

## Wiring convention

In `cmd/api/<track>.go` export a registration function taking explicit deps:

```go
// registerWorkflowsRoutes mounts all workflow + approval routes on apiMux.
func registerWorkflowsRoutes(apiMux *http.ServeMux, wfSvc *workflows.Service,
	apSvc *approvals.Service, runsSvc *runs.Service, queueSvc *queue.Queue,
	authSvc *auth.Service) { ... }
```

In `docs/wiring/<track>.md` document the exact constructor lines the orchestrator
must add to `newApp` / `routes()` in `cmd/api/main.go`, e.g.:

```go
a.wfSvc := workflows.NewServiceWithStore(workflows.NewPostgresStore(db)) // or workflows.NewService()
registerWorkflowsRoutes(apiMux, a.wfSvc, a.apSvc, a.runsSvc, a.queueSvc, a.authSvc)
```

Add your service fields to a NEW small struct in your own wiring file if needed —
do NOT edit the `app` struct in main.go. Prefer passing deps as function args.

## Shared conventions

- All routes mount on `apiMux` (they are automatically served under BOTH `/v1`
  and `/api/v1`). Write paths WITHOUT prefix, e.g. `/workflows`.
- Auth wrap pattern (copy from existing routes):
  `apiMux.Handle("/path", auth.RequireAuthOrAPIKey(a.authSvc, a.apiKeysSvc)(auth.RequirePermission(a.authSvc, auth.PermissionX)(http.HandlerFunc(h))))`
- JSON responses: small wrapper helpers may exist in `cmd/api/handlers.go` —
  if you need `writeJSON`/`readJSON`, define local unexported helpers in YOUR
  file with distinct names (e.g. `writeJSONTrack`) to avoid redeclaration.
- Errors: `{"error": {"code": "MACHINE_READABLE_CODE", "message": "..."}}`.
- IDs: UUID strings (google/uuid is available). Timestamps RFC3339 UTC.
- Multi-tenancy: scope every store query by `organization_id` taken from auth
  claims; never trust client-supplied org IDs.
- Slog structured logging with tenant/request context where available.
- Migration files: `migrations/NNN_<name>.sql` (number owned per track below).
  They are auto-discovered from the migrations dir at runtime — do not edit any registry.
  Follow the style of `migrations/005_persistence_hardening.sql` (idempotent guards).

## RBAC permissions pinned (constant name → string, roles granted)

| Track | Constants | String | OWNER | ADMIN | MEMBER | VIEWER |
|-------|-----------|--------|-------|-------|--------|--------|
| 2-a | PermissionWorkflowsRead | workflows.read | ✓ | ✓ | ✓ | ✓ |
| 2-a | PermissionWorkflowsWrite | workflows.write | ✓ | ✓ | ✓ | |
| 2-a | PermissionWorkflowsExecute | workflows.execute | ✓ | ✓ | ✓ | |
| 2-a | PermissionApprovalsRead | approvals.read | ✓ | ✓ | ✓ | ✓ |
| 2-a | PermissionApprovalsDecide | approvals.decide | ✓ | ✓ | | |
| 2-a | PermissionRunsControl | runs.control | ✓ | ✓ | ✓ | |
| 2-b | PermissionDeploymentsRead | deployments.read | ✓ | ✓ | ✓ | ✓ |
| 2-b | PermissionDeploymentsWrite | deployments.write | ✓ | ✓ | ✓ | |
| 2-b | PermissionDeploymentsDeploy | deployments.deploy | ✓ | ✓ | | |
| 2-c | PermissionPoliciesRead | policies.read | ✓ | ✓ | ✓ | ✓ |
| 2-c | PermissionPoliciesWrite | policies.write | ✓ | ✓ | | |
| 2-d | PermissionEvalsRead | evaluations.read | ✓ | ✓ | ✓ | ✓ |
| 2-d | PermissionEvalsWrite | evaluations.write | ✓ | ✓ | ✓ | |
| 2-e | PermissionWebhooksRead | webhooks.read | ✓ | ✓ | ✓ | ✓ |
| 2-e | PermissionWebhooksWrite | webhooks.write | ✓ | ✓ | ✓ | |
| 2-f | PermissionSchedulesRead | schedules.read | ✓ | ✓ | ✓ | ✓ |
| 2-f | PermissionSchedulesWrite | schedules.write | ✓ | ✓ | ✓ | |

Existing permissions you may reuse: `agents.read`, `agents.write`, `runs.read`,
`runs.execute` (`auth.PermissionAgentsRead` etc.).

## Endpoint contracts

### Track 2-a — Workflows + Approvals + Run control (migration 006)

`internal/workflows` (extend existing), `internal/approvals` (NEW package).

Workflow DSL (stored as JSONB, Go struct `DSL`):
```json
{
  "nodes": [{"id": "n1", "type": "agent|tool|condition|parallel|approval|delay|webhook",
             "name": "Planner", "config": {"agent_id": "...", "input": "{{input}}", "tool_id": "...", "timeout_seconds": 60}}],
  "edges": [{"from": "n1", "to": "n2", "condition": "on_success|on_failure|always"}]
}
```

Endpoints:
- `GET /workflows` → `{"workflows": [{"id","name","description","status","current_version","created_at","updated_at"}]}`
- `POST /workflows/create` body `{"name","description","dsl"}` → `{"workflow": {...as above, "dsl": ...}}` (status `draft`); runs validation first → 422 `{"errors":[{"code","message","node_id"}]}` if invalid
- `GET /workflows/{id}` → `{"workflow": {..., "versions":[{"version":1,"status":"published","created_at","dsl_snapshot"}]}}`
- `POST /workflows/{id}/validate` → `{"valid": true}` or 422 with errors
- `POST /workflows/{id}/publish` → publishes CURRENT dsl as next immutable version → `{"workflow", "version"}`
- `POST /workflows/{id}/execute` body `{"input": "..."}` → expands DAG: creates one run per agent/tool node (sequential for now; honor edges order; `parallel` node fans out) → `{"workflow_run_id","run_ids":[...],"status":"pending"}`
- `GET /workflow-runs/{id}` → `{"id","workflow_id","status","node_runs":[{"node_id","run_id","status","started_at","finished_at","error"}]}`
- Validation rules to implement: missing node refs in edges, unknown node type, cycles (DFS), empty graph, missing config per type (agent node requires agent_id, etc.)

Approvals:
- `GET /approvals?status=pending` → `{"approvals": [{"id","run_id","workflow_run_id","resource","action","reason","risk","status":"pending|approved|rejected|cancelled","requester","approver","created_at","decided_at"}]}`
- `GET /approvals/{id}` → single
- `POST /approvals/{id}/decide` body `{"decision":"approved|rejected","reason":"..."}` → sets approver from auth user, `decided_at` now; if approved and linked run is paused → resume it (call into runs service)

Run control (extend existing run detail route semantics — implement handler in YOUR file, orchestrator mounts):
- `POST /runs/{id}/cancel` → status `cancelled` (idempotent). `POST /runs/{id}/pause` → `paused`. `POST /runs/{id}/resume` → `pending` (re-enqueue if it was queued).
- New run status values allowed everywhere: `pending|running|paused|waiting_approval|completed|failed|cancelled|timeout`

DB (006): `workflows`, `workflow_versions`, `workflow_runs`, `workflow_node_runs`, `approvals` — all with `organization_id`, created_at/updated_at, proper FKs + indexes.

### Track 2-b — Agent versions + Deployments (migration 007)

`internal/agents` (extend with versions), `internal/deployments` (real service + store).

- `GET /agents/{id}/versions` → `{"versions": [{"version","snapshot","published_at","published_by","status":"draft|published|archived"}]}` (snapshot = full agent config JSON, immutable once published)
- `POST /agents/{id}/versions/create` → snapshot current config → `{"version": 3}`
- `POST /agents/{id}/versions/{version}/publish` → marks immutable → `{"version"}`
- `POST /agents/{id}/rollback` body `{"target_version": 2}` → marks target re-published/current → `{"current_version": 2}`
- `GET /deployments?agent_id=` → `{"deployments": [{"id","agent_id","version","environment":"development|staging|production","status":"requested|validated|deploying|healthy|failed","health":{"error_rate":0.0,"last_check_at"},"created_at","updated_at"}]}`
- `POST /deployments/create` body `{"agent_id","version","environment"}` → validates version exists+published → `{"deployment"}` (status `requested`)
- `GET /deployments/{id}` → single
- `POST /deployments/{id}/promote` → advance lifecycle one step (requested→validated→deploying→healthy) → `{"deployment"}` (a worker will do this later; for now promote is explicit)
- `POST /deployments/{id}/rollback` → re-points environment to previous healthy deployment's version → `{"deployment","rolled_back_to_version"}`

DB (007): `agent_versions`, `deployments` (+ unique partial index: one healthy deployment per agent+environment).

### Track 2-c — Policies + Rate limiting + Idempotency (migration 008)

`internal/policies` (NEW), `internal/httpx/ratelimit.go`, `internal/httpx/idempotency.go` (NEW files).

- `GET /policies` → `{"policies": [{"id","name","effect":"allow|deny","resource_type":"tool|agent|workflow|deployment|*","actions":["runs.execute","tools.call"],"conditions":{"tool_allowlist":["search"],"max_cost_cents":500,"environments":["development"],"require_approval":false},"priority":100,"enabled":true,"created_at","updated_at"}]}`
- `POST /policies/create` → validate conditions JSON → `{"policy"}`
- `PUT /policies/{id}` (full update) | `DELETE /policies/{id}` → `{"deleted": true}`
- `POST /policies/evaluate` body `{"subject":{"user_id","role","api_key_id"},"action":"tools.call","resource":{"type":"tool","id":"httpx","tenant_id":"..."},"context":{"estimated_cost_cents":50,"environment":"production"}}` → `{"decision":"allow|deny","matched_policy_id","reason"}` (highest priority deny wins over allow; default allow when no match)
- Rate limiting middleware: Redis token bucket (`go-redis` already present), keyed `ratelimit:{scope}:{id}` with sliding window; scope from route (auth, api, execute); falls back to in-memory `observability.RateLimiter` when Redis nil. Config via env `AGENTOS_RATE_LIMIT_RPM` (default 120). Middleware constructor: `httpx.NewRateLimitMiddleware(rdb *redis.Client, fallback *observability.RateLimiter, limit int, window time.Duration) func(http.Handler) http.Handler`. On limit: 429 + `Retry-After` header. Unit-test with miniredis.
- Idempotency middleware: `httpx.NewIdempotencyMiddleware(store IdempotencyStore)` — honors `Idempotency-Key` header on POST; stores response (status+body) for 24h; replays on retry with same key (200 with `X-Idempotent-Replay: true`). In-memory + Postgres store impls. Table `idempotency_keys` in 008.

DB (008): `policies`, `idempotency_keys`.

### Track 2-d — Evaluations (migration 009)

`internal/evaluations` (NEW).

- `GET /eval-datasets` → `{"datasets": [{"id","name","description","case_count","created_at"}]}`
- `POST /eval-datasets/create` body `{"name","description","cases":[{"id":"c1","input":"...","expected":"...","scorer":"exact|contains|regex|latency_under_ms|cost_under_cents","params":{"pattern":"^ok$","threshold_ms":1500,"threshold_cents":10}}]}` → `{"dataset"}`
- `GET /eval-datasets/{id}` → includes cases
- `POST /eval-datasets/{id}/run` body `{"agent_id"}` → executes each case through the runtime synchronously (bounded: max 50 cases, 30s per case) → `{"eval_run_id","status":"completed"}`
- `GET /eval-runs/{id}` → `{"id","dataset_id","agent_id","status","results":[{"case_id","output","passed","score":1.0,"latency_ms","cost_cents","error"}],"summary":{"pass_rate":0.83,"avg_latency_ms":420,"total_cost_cents":37,"by_scorer":{"exact":{"passed":5,"failed":1}}}}`
- `POST /eval-runs/compare` body `{"baseline_run_id","candidate_run_id"}` → `{"baseline":{summary},"candidate":{summary},"regressions":[{"case_id","baseline_passed":true,"candidate_passed":false}],"improvements":[...]}`

DB (009): `eval_datasets`, `eval_cases`, `eval_runs`, `eval_results`.

### Track 2-e — Events (NATS) + Outbound webhooks (migration 010)

`internal/events` (NEW), `internal/webhooks` (real service + store + delivery). MAY add `github.com/nats-io/nats.go` to go.mod.

Event model (`events.Event`): `{"id":"uuid","type":"execution.completed","tenant_id","project_id","timestamp","resource":{"type":"run","id"},"execution_id","trace_id","payload":{...}}`.
Event types (constants): `agent.created`, `agent.updated`, `run.started`, `run.completed`, `run.failed`, `run.cancelled`, `step.started`, `step.completed`, `approval.requested`, `approval.decided`, `deployment.completed`, `deployment.failed`, `webhook.received`.

- `events.Publisher` interface: `Publish(ctx, Event) error`. Impls: `NewNoopPublisher()`, `NewMemoryPublisher()` (ring buffer 1000, subscribers channel), `NewNATSPublisher(url string)` (JetStream; subject = event type with dots → `agentos.events.<type>`; if NATS unreachable at construction → return error, caller falls back to memory). `events.NewFromEnv()` reads `AGENTOS_NATS_URL` (empty → noop).
- `events.Subscriber` for memory/NATS with `Subscribe(types []string) (<-chan Event, func(), error)`.
- Webhooks endpoints:
  - `GET /webhooks` → `{"webhooks": [{"id","url","events":["run.failed"],"status":"active|disabled","secret_set":true,"created_at"}]}`
  - `POST /webhooks/create` body `{"url","events"}` → generates HMAC secret (returned ONCE: `"secret"`), stores only hash
  - `DELETE /webhooks/{id}` → `{"deleted": true}`
  - `GET /webhooks/{id}/deliveries?limit=50` → `{"deliveries": [{"id","event_type","status":"delivered|failed|retrying","attempts","last_status_code","latency_ms","error","created_at"}]}`
- Delivery worker: on matching event (subscribe to publisher), POST JSON `{"id","type","timestamp","payload"}` + `X-AgentOS-Signature: sha256=<hmac>` + `X-AgentOS-Event-Id`; retry with exponential backoff 1s/5s/30s, max 3 attempts, record every attempt. `http.Client{Timeout: 10s}`.

DB (010): `webhooks`, `webhook_deliveries`, `events` (append-only audit of published events).

### Track 2-f — Scheduler (migration 011)

`internal/scheduler` (real service + store + worker loop).

- `GET /schedules` → `{"schedules": [{"id","agent_id","input","kind":"once|recurring|cron","run_at","interval_seconds","cron_expr","timezone","status":"active|paused","next_run_at","last_run_id","created_at"}]}`
- `POST /schedules/create` body `{"agent_id","input","kind","run_at","interval_seconds","cron_expr","timezone"}` → validates (once requires run_at; recurring requires interval_seconds≥60; cron requires valid cron expr — implement a minimal 5-field cron parser, no new deps) → computes `next_run_at` → `{"schedule"}`
- `GET /schedules/{id}` | `POST /schedules/{id}/pause` | `POST /schedules/{id}/resume` | `DELETE /schedules/{id}`
- Worker loop: `scheduler.NewWorker(svc, runsSvc, queueSvc, pollInterval)` — every poll, `Due(ctx, now)` returns schedules to fire; create run + enqueue; update `next_run_at` (or complete for `once`); catch-up protection: never fire more than once per interval even after restart (store `last_fired_at`). Tests with fake clock.

DB (011): `schedules` (+ index on `(status, next_run_at)`).

### Track 2-g — Frontend (owns `web/` exclusively)

Build against the endpoint contracts above (all prefixed `/api/v1` via existing
client) + existing endpoints in `api/openapi.yaml` (read it in your worktree).
Priorities in order (commit after each):
1. **Run detail timeline**: fetch `/runs/{id}/steps`, render step list with status/tokens/cost; live-update from SSE `/runs/{id}/events`.
2. **Workflows**: list + detail + create (name/description/JSON DSL textarea with client-side basic validation displaying backend 422 errors) + execute button + workflow-run status view (`/workflow-runs/{id}`).
3. **Approvals page**: pending list + approve/reject actions.
4. **Evaluations**: datasets list/create + run view with results table + pass rate.
5. **Usage page**: cost/latency widgets from `/metrics` + `/runs` aggregates.
6. RBAC-aware nav (hide actions for viewer role), ⌘K command palette (navigate + create agent).
- Keep TypeScript strict-clean: `npm run build` MUST pass. Keep existing design language (App.css tokens). No new heavy deps without need (React Router already present).
- Respect the no-mock rule: every view consumes the real API; empty states for missing data.

### Track 2-h — Observability + DX (no migration)

- `internal/observability`: extend `Metrics` with histogram-style observation (bucketed p50/p95/p99) and a Prometheus text-format exposition handler `metricsHandler` producing `# HELP/# TYPE` lines (`agentos_runs_total`, `agentos_request_duration_seconds` etc.) at `GET /metrics` (keep existing JSON shape for backward compat via `?format=json`). No new deps.
- `cmd/seed` (NEW): creates demo org/user (email `demo@agentos.dev`, password from env or `demo-password`), 3 agents, 1 tool, 1 workflow (3 nodes), 2 completed runs with steps, 1 eval dataset. Clearly marked `demo-` prefixes. Idempotent (skip if exists).
- `Makefile`: add `seed`, `run-worker`, `docker-up`, `docker-down`, `migrate-up`, `migrate-down`, `lint` (gofmt+vet) targets if missing.
- `docs/architecture.md` (NEW): C4-ish overview matching ACTUAL current code (Go API + worker, Postgres/Redis/NATS optional, in-memory fallback mode, SSE, queue pull/push).
- README: sync "Quickstart" with `make docker-up && make migrate-up && make seed && make run-api && make run-worker`.

## Merge order (orchestrator)

2-c → 2-a → 2-b → 2-d → 2-e → 2-f → 2-h → 2-g (frontend last, against final API).
