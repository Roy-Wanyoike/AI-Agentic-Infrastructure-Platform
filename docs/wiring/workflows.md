# Wiring — Workflows + Approvals + Run control (wave-2 track 2-a)

Exact lines for the orchestrator to paste into `cmd/api/main.go`. Everything
below is additive; `main.go` itself is owned by the orchestrator, so this
track never edited it.

## 1. Imports (top of `cmd/api/main.go`)

```go
import (
        // ... existing imports ...
        "agentos/internal/approvals"
        "agentos/internal/workflows"
)
```

(`apikeys`, `auth`, `queue` and `runs` are already imported by `main.go`.)

## 2. `app` struct fields (optional style)

The registration function takes every dependency as an argument, so fields are
optional. If you prefer fields on `app` (orchestrator-owned struct):

```go
type app struct {
        // ... existing fields ...
        wfSvc *workflows.Service
        apSvc *approvals.Service
}
```

Otherwise keep the two services as locals in `newApp`/`routes()` and pass them
through — see §5 for a wiring-file alternative.

## 3. Constructor lines in `newApp` (dual mode)

```go
if db != nil {
        // ... existing Postgres stores ...
        a.wfSvc = workflows.NewServiceWithStore(workflows.NewPostgresStore(db))
        a.apSvc = approvals.NewServiceWithStore(approvals.NewPostgresStore(db))
} else {
        // ... existing in-memory services ...
        a.wfSvc = workflows.NewService()
        a.apSvc = approvals.NewService()
}
```

No `SetEngine` / `SetRunController` calls are required: the registration
function (§4) wires `workflows.Engine{Runs, Queue, Approvals}` and
`approvals.RunController` for you when both `runsSvc` and `queueSvc` are
present. If you wire them yourself anyway, the calls are idempotent:

```go
a.wfSvc.SetEngine(workflows.Engine{Runs: a.runsSvc, Queue: a.queueSvc, Approvals: a.apSvc})
a.apSvc.SetRunController(a.runsSvc)
```

## 4. Route registration in `routes()`

Paste after the existing registrations (anywhere before the `/` catch-all):

```go
registerWorkflowsRoutes(apiMux, a.wfSvc, a.apSvc, a.runsSvc, a.queueSvc, a.authSvc, a.apiKeysSvc)
```

Note: **run control routes are included** — this call also mounts
`POST /runs/{id}/cancel`, `POST /runs/{id}/pause` and `POST /runs/{id}/resume`
with the `runs.control` permission. They are registered as Go 1.22 method
patterns (`POST /runs/{id}/cancel`), which are more specific than the legacy
`/runs/` subtree handler already registered in `main.go`, so they take
precedence automatically; the legacy `/runs/{id}`, `/runs/{id}/steps` and
`/runs/{id}/events` behavior is unchanged.

If you did NOT add fields to `app` (§2), a tiny struct local to a new wiring
file works too:

```go
// cmd/api/workflows_wiring.go (example)
type workflowsDeps struct {
        wfSvc *workflows.Service
        apSvc *approvals.Service
}
```

## 5. Endpoints mounted (all under `/api/v1` and legacy `/v1`)

| Route | Permission | Notes |
|---|---|---|
| `GET /workflows` | workflows.read | summary shape (no dsl) |
| `POST /workflows/create` | workflows.write | validates first → 422 `{"errors":[{"code","message","node_id"}]}` |
| `GET /workflows/{id}` | workflows.read | includes `dsl` + immutable `versions` |
| `POST /workflows/{id}/validate` | workflows.read | `{"valid":true}` or 422 errors |
| `POST /workflows/{id}/publish` | workflows.write | `{"workflow","version"}` |
| `POST /workflows/{id}/execute` | workflows.execute | `{"workflow_run_id","run_ids":[...],"status"}` |
| `GET /workflow-runs/{id}` | workflows.read | `{"id","workflow_id","status","node_runs":[...]}` |
| `GET /approvals?status=` | approvals.read | status filter optional |
| `GET /approvals/{id}` | approvals.read | `{"approval":{...}}` |
| `POST /approvals/{id}/decide` | approvals.decide | approve resumes the linked paused run |
| `POST /runs/{id}/cancel` | runs.control | idempotent |
| `POST /runs/{id}/pause` | runs.control | idempotent |
| `POST /runs/{id}/resume` | runs.control | idempotent; pending→pending no-op |

Auth on every route: `auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(auth.RequirePermission(authSvc, <permission>)(handler))`.
The tenant is always `auth.ExtractClaims(...).OrganizationID`; client-supplied
organization ids are ignored. Cross-tenant access surfaces as **404**.

## 6. Deviations / judgement calls (documented per the ground rules)

1. `POST /workflows/{id}/validate` requires `workflows.read` (not write): it
   is non-mutating, so VIEWERs can validate.
2. `POST /workflows/{id}/execute` returns **200** (not 201/202): the run
   expansion is accepted and enqueued; the contract pinned the body, not the
   status.
3. Error codes are uppercase machine strings: `VALIDATION_ERROR`,
   `WORKFLOW_NOT_FOUND`, `WORKFLOW_RUN_NOT_FOUND`, `APPROVAL_NOT_FOUND`,
   `RUN_NOT_FOUND`, `INVALID_DECISION` (422), `ALREADY_DECIDED` (409),
   `INVALID_STATE` (409), `ENGINE_NOT_WIRED` (503), `INVALID_REQUEST` (400),
   `INTERNAL_ERROR` (500). DSL-validation failures keep the contract-exact
   body `{"errors":[{"code","message","node_id"}]}` (not the error envelope).
4. Run control responses serialize the existing `runs.Run` struct, which has
   no JSON tags → PascalCase keys (`ID`, `Status`, ...) matching the existing
   `GET /runs/{id}` behavior; statuses are the new lowercase vocabulary
   (`cancelled`, `paused`, `pending`).
5. The `queueSvc` parameter of `registerWorkflowsRoutes` is accepted for
   signature parity with the wiring convention (and is what allows the
   function to self-wire `workflows.Engine`); handlers never enqueue directly.
6. Migration 006 (`migrations/006_workflows_approvals.sql`) and the
   `internal/database` version-6 expectation were part of commit 93fad62 —
   nothing to add here.

## 7. Verification

- `go build ./...` green, `go test ./...` green (26 packages).
- `cmd/api/workflows_test.go` covers 401s, VIEWER 403s, the OWNER happy path
  (create → validate → publish → execute → workflow-run → decide → linked run
  resumed), 422 validation bodies, cross-tenant 404s, run-control transitions
  and the 503 engine-not-wired path.
- OpenAPI fragment: `api/fragments/workflows.yaml` (13 paths/operations, 29
  schemas, standalone YAML validated).
