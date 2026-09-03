# Scheduler (Track 2-f) — Wiring Guide

Everything in `internal/scheduler` + `cmd/api/schedules.go` is implemented and
tested on `feat/wave2-scheduler`. **No shared file was edited** — the lines
below are what the orchestrator applies to light everything up.

## 1. Endpoints (live after wiring)

| Method | Path                      | Permission        | Response envelope |
|--------|---------------------------|-------------------|-------------------|
| GET    | `/schedules`              | `schedules.read`  | `{"schedules":[...]}` |
| POST   | `/schedules/create`       | `schedules.write` | `{"schedule":{...}}` (201) |
| GET    | `/schedules/{id}`         | `schedules.read`  | `{"schedule":{...}}` |
| POST   | `/schedules/{id}/pause`   | `schedules.write` | `{"schedule":{...}}` |
| POST   | `/schedules/{id}/resume`  | `schedules.write` | `{"schedule":{...}}` |
| DELETE | `/schedules/{id}`         | `schedules.write` | `{"deleted":true}` |

All routes are registered on `apiMux`, so they serve under BOTH `/api/v1`
(canonical) and `/v1` (legacy) exactly like the existing routes. Error shape:
`{"error":{"code":"MACHINE_READABLE_CODE","message":"..."}}` with codes
`VALIDATION_ERROR` (422), `SCHEDULE_NOT_FOUND` (404), `INVALID_STATE` /
`SCHEDULE_COMPLETED` (409), `INVALID_REQUEST` (400), `INTERNAL_ERROR` (500).
Auth chain per route: `auth.RequireAuthOrAPIKey(a.authSvc, a.apiKeysSvc)` →
`auth.RequirePermission(...)` (same pattern as main.go:99–127).

## 2. `cmd/api/main.go` wiring (4 edits, all owned by the orchestrator)

**(a) import block** — add:

```go
"agentos/internal/scheduler"
```

**(b) `app` struct (main.go:34–48)** — add one field:

```go
schedSvc *scheduler.Service
```

**(c) `newApp` (inside `if db != nil { ... }` around main.go:62–70)** — add:

```go
a.schedSvc = scheduler.NewServiceWithStore(scheduler.NewPostgresStore(db))
```

and inside the `else` branch (in-memory mode, around main.go:71–86) add:

```go
a.schedSvc = scheduler.NewService()
```

**(d) `routes()` (after the `/runs/` handle, around main.go:125)** — add:

```go
registerSchedulesRoutes(apiMux, a.schedSvc, a.authSvc, a.apiKeysSvc, a.auditSvc)
```

**(e) Scheduler trigger loop in the API process (recommended place).** The
worker needs a runs service and a queue to create/enqueue runs; the API
process already has both wired (`a.runsSvc`, `a.queueSvc`) and, with Postgres,
every schedule created through the API is visible to the loop. In `main()`
after `application := newApp(cfg, logr, db)` (main.go:181):

```go
schedPoll := scheduler.DefaultPollInterval // 30s; override via env below
if v := strings.TrimSpace(os.Getenv("AGENTOS_SCHEDULER_POLL_INTERVAL")); v != "" {
    if d, err := time.ParseDuration(v); err == nil && d > 0 {
        schedPoll = d
    }
}
schedWorker := scheduler.NewWorker(application.schedSvc, application.runsSvc, application.queueSvc, schedPoll)
go schedWorker.Run(context.Background())
logr.Info("scheduler trigger worker started", "poll_interval", schedPoll.String())
```

(`context`, `strings`, `os`, `time` are already imported in main.go.)

## 3. `cmd/worker/main.go` wiring (alternative: run the loop in the worker)

The worker binary currently builds **in-memory** `runs.Service` +
`queue.Queue` (cmd/worker/main.go:143–144). Firing the loop there only sees
schedules stored in the worker's own scheduler service, so it MUST be
Postgres-backed to observe API-created schedules. Edits (all owned by the
orchestrator):

**(a) imports** — add:

```go
"context"
"database/sql"

"agentos/internal/database"
"agentos/internal/scheduler"
```

**(b) after `runsService := runs.NewService()` (cmd/worker/main.go:144)** — add:

```go
// Scheduler trigger loop: Postgres-backed so API-created schedules are
// visible; falls back to in-memory when no DATABASE_URL is set (nothing
// to fire in that mode, since schedules cannot be shared).
var schedSvc *scheduler.Service
if dsn := database.DSNFromEnv(); dsn != "" {
    if db, err := database.Connect(dsn); err == nil {
        defer db.Close()
        schedSvc = scheduler.NewServiceWithStore(scheduler.NewPostgresStore(db))
    } else {
        logr.Warn("scheduler: database unavailable, scheduler loop disabled", "error", err.Error())
    }
}
if schedSvc == nil {
    schedSvc = scheduler.NewService()
}
schedPoll := scheduler.DefaultPollInterval
if v := strings.TrimSpace(os.Getenv("AGENTOS_SCHEDULER_POLL_INTERVAL")); v != "" {
    if d, err := time.ParseDuration(v); err == nil && d > 0 {
        schedPoll = d
    }
}
schedWorker := scheduler.NewWorker(schedSvc, runsService, workQueue, schedPoll)
go schedWorker.Run(context.Background())
logr.Info("scheduler trigger worker started", "poll_interval", schedPoll.String())
```

> **Pick ONE place to run the loop** (API process §2-e or worker §3) — running
> both against the same database is also *safe* (the conditional
> `ClaimForFire` UPDATE makes firing at-most-once across concurrent workers),
> but wasteful. The API process placement is recommended because it needs no
> new imports in cmd/worker and already holds `runsSvc`/`queueSvc`.

## 4. Environment variables

| Var | Default | Meaning |
|-----|---------|---------|
| `AGENTOS_SCHEDULER_POLL_INTERVAL` | `30s` | Worker loop tick (Go duration, e.g. `10s`, `1m`) |
| `DATABASE_URL` / `POSTGRES_*` | – | When set, the Postgres store is used (`migrations/011_scheduler.sql` must be applied — auto-discovered by `internal/database` on startup) |

## 5. Semantics / firing contract

- `Due(ctx, now)` = `status='active' AND next_run_at <= now` (hot path indexed
  by `idx_schedules_status_next_run (status, next_run_at)`).
- Each due schedule is **claimed before firing** via a conditional UPDATE
  (`ClaimForFire`: re-checks `status='active' AND next_run_at <= fired_at`).
  This is the catch-up protection: a restart or a second worker can never
  consume the same due slot twice. `last_fired_at` records the claimed
  instant (survives restarts in Postgres mode).
- After a successful claim: `runs.CreateRunCtx(ctx, schedule.OrganizationID,
  schedule.AgentID, schedule.Input)` (same call shape as
  `createRunHandler`), then `queue.Enqueue("agent.run", {organization_id,
  agent_id, input, run_id, schedule_id, trigger:"schedule"})` — exactly the
  payload `cmd/worker`'s `processTask` consumes.
- `kind=once` → status `completed` after its single firing (terminal);
  `kind=recurring` → `next_run_at = fire_time + interval` (no burst of
  catch-up fires after downtime); `kind=cron` → next matching wall-clock
  minute in the schedule's IANA timezone (DST-aware).
- If run creation fails after the claim, the slot is still consumed
  (at-most-once preferred over duplicated runs — documented trade-off).
- Pausing preserves `next_run_at`; resuming an overdue schedule fires ONCE on
  the next poll, then resumes the cadence.

## 6. Judgement calls / deviations (per contract ground rule 3)

1. **`POST /schedules/{id}/pause|resume` conflict code** — contract pins no
   code; a second pause (or pause of a completed once-schedule) returns
   **409 `INVALID_STATE` / `SCHEDULE_COMPLETED`** instead of 200-idempotent.
2. **Additive response fields** — `last_fired_at` and `updated_at` are
   returned in addition to the contract's field list (needed to observe
   catch-up protection; supersets are backward compatible).
3. **`scheduler.ValidationError` + sentinel errors both map to 422**
   `VALIDATION_ERROR`; malformed JSON is 400 `INVALID_REQUEST`.
4. **`time.LoadLocation`** requires the IANA tzdata to be resolvable — on
   minimal containers without `/usr/share/zoneinfo` only `UTC` and
   fixed-offset names validate; if that becomes a problem, import
   `time/tzdata` in main.go (one line, orchestrator-owned; not added here to
   avoid touching shared files).
5. **Cron semantics** follow Vixie cron: 5 fields (min hour dom month dow),
   Sunday=0, dom/dow OR-rule when both are restricted, `N/step` expands to
   `N-max/step`, expressions that can never fire (e.g. `0 0 31 2 *`) are
   rejected at create with 422 ("never fires within 5 years").
6. **No `GET /schedules?status=` filter** — the contract lists none; the
   in-memory list returns active/paused/completed rows for the tenant.

## 7. OpenAPI

`api/fragments/scheduler.yaml` (paths + schemas, YAML-parse verified) is to
be merged into `api/openapi.yaml` by the orchestrator; it references the
main spec's `securitySchemes` (`bearerToken`, `apiKey`) and shared responses
(`Unauthorized`, `Forbidden`).
