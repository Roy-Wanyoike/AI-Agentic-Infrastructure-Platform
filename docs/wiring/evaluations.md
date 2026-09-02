# Wiring — Track 2-d Evaluations

How to wire the evaluations subsystem into `cmd/api/main.go` (the
orchestrator performs the edit; this file documents the exact lines).

## What gets mounted

`registerEvaluationsRoutes` (in `cmd/api/evaluations.go`) mounts:

| Method | Path                       | Permission            |
|--------|----------------------------|-----------------------|
| GET    | `/eval-datasets`           | `evaluations.read`    |
| POST   | `/eval-datasets/create`    | `evaluations.write`   |
| GET    | `/eval-datasets/{id}`      | `evaluations.read`    |
| POST   | `/eval-datasets/{id}/run`  | `evaluations.write`   |
| GET    | `/eval-runs/{id}`          | `evaluations.read`    |
| POST   | `/eval-runs/compare`       | `evaluations.write`   |

All routes are registered on `apiMux`, so they are served under both
`/api/v1` (canonical) and `/v1` (legacy alias). Auth wrap pattern matches
`cmd/api/main.go` lines 99-127
(`auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(auth.RequirePermission(authSvc, perm)(h))`).

## Service construction (inside `newApp`)

The evaluations service needs **two collaborators passed in**: the agents
service (tenant guard: the run target agent must belong to the caller's
organization) and an eval runner (an `evaluations.AgentRunner`, satisfied by
`*runtime.Runner`).

```go
// cmd/api/main.go — app struct additions (fields only, orchestrator-owned):
type app struct {
    // ... existing fields ...
    evalSvc    *evaluations.Service
    evalRunner *runtime.Runner
}

// newApp — build the runner once, before the branches. Offline mode uses the
// deterministic no-provider runner; attach runtime.WithProvider(...) when a
// model backend is configured (same env wiring as cmd/worker).
a.evalRunner = runtime.NewRunnerWithOptions(a.agentsSvc, nil /* tools.Registry */)

// Postgres branch (db != nil):
a.evalSvc = evaluations.NewServiceWithStore(evaluations.NewPostgresStore(db), evaluations.Deps{
    Agents: a.agentsSvc,
    Runner: a.evalRunner,
})

// In-memory branch (db == nil):
a.evalSvc = evaluations.NewService(evaluations.Deps{
    Agents: a.agentsSvc,
    Runner: a.evalRunner,
})
```

Optional tuning: `Deps.CaseTimeout` bounds each case (default 30s) and
`evaluations.MaxCasesPerDataset` (50) caps dataset size.

## Route registration (inside `routes()`)

```go
registerEvaluationsRoutes(apiMux, a.evalSvc, a.authSvc, a.apiKeysSvc)
```

## Imports to add in main.go

```go
"agentos/internal/evaluations"
"agentos/internal/runtime" // if not already imported
```

## Migration

`migrations/009_evaluations.sql` (this track owns it). Auto-discovered by
`internal/database.LoadMigrations`; no registry edits needed. Tables:
`eval_datasets`, `eval_cases` (PK `(dataset_id, case_id)`, `position`
ordering), `eval_runs`, `eval_results` (`scorer` + `case_index`
denormalized for by_scorer aggregation), all `organization_id`-scoped with
tenant indexes, idempotent guards per the 005 style.

## Contract notes / known limitations

1. **Cost is recorded as 0.** `runtime.Run` exposes token usage only; there
   is no pricing hook yet. When one exists, `Service.runCase` in
   `internal/evaluations/service.go` is the single place to feed real
   `cost_cents` into results/summaries (currently `const costCents = 0.0`).
2. **Synchronous run endpoint.** `POST /eval-datasets/{id}/run` executes up
   to 50 cases × 30s worst case before responding. Note the server currently
   sets `WriteTimeout: 10s` in `cmd/api/main.go`; large datasets against a
   slow provider may hit that ceiling. Options for the orchestrator: keep
   datasets small, raise the write timeout, or move execution behind the
   queue later (out of scope for 2-d, which follows the synchronous contract).
3. **Compare semantics.** Runs are matched by `case_id` intersection; cases
   missing from either run are ignored (never counted as regressions).
   Thresholds are inclusive (`latency <= threshold_ms`,
   `cost <= threshold_cents`); regex uses Go `regexp.MatchString` (search,
   case-sensitive) semantics.
4. **RBAC grants** (pinned by the wave-2 contract):
   `evaluations.read` → OWNER/ADMIN/MEMBER/VIEWER,
   `evaluations.write` → OWNER/ADMIN/MEMBER. Registered via `init()` in
   `internal/auth/permissions_evaluations.go`; `service.go`/`middleware.go`
   untouched.
5. **`TestApplyMigrations` was made data-driven** (in
   `internal/database/database_test.go`): sqlmock expectations are now derived
   from the discovered migration set instead of a hard-coded version list, so
   adding migrations 006-011 across wave-2 tracks cannot break it. This is
   the only edit outside track-owned files (test-only, no production code).
6. **Request/latency bound.** If the client disconnects mid-run, remaining
   cases fail fast with `context canceled` recorded per case; the run still
   persists as `completed` with per-case errors.
