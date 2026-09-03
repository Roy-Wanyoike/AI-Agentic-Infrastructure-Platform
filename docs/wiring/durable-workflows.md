# Wave-3 track 3-c wiring — Durable workflow execution

Everything in this document is additive: no line of `cmd/api/main.go`,
`cmd/worker/main.go` or `api/openapi.yaml` was touched by the track (per the
contract). Apply the snippets below at integration time.

Shipped by the track:

| Deliverable | File |
|---|---|
| Migration 013 (additive hardening, idempotent) | `migrations/013_durable_workflows.sql` |
| Idempotent checkpoint state machine | `internal/workflows/durable.go` |
| Recovery pass + watchdog + recovery worker loop | `internal/workflows/recovery.go` |
| Postgres durable-execution store (SKIP LOCKED) | `internal/workflows/store_durable.go` |
| `GET /v1/workflow-runs/{id}/nodes` handler | `cmd/api/workflow_run_nodes.go` |
| OpenAPI fragment (`Wf3*` prefix) | `api/fragments/durable-workflows.yaml` |

## 1. Endpoint table

| Method & path | Permission constant | x-required-permission | Notes |
|---|---|---|---|
| `GET /api/v1/workflow-runs/{id}/nodes` | `auth.PermissionWorkflowsRead` | `workflows.read` | Checkpointed node timeline, one entry per (node, attempt), oldest first. Same permission the wave-2 workflow routes use. |

Response shape (snake_case, contract-pinned):

```json
{"nodes": [{"id": "…", "node_id": "…", "status": "completed", "attempt": 1,
            "started_at": "…", "finished_at": "…", "error_code": null}]}
```

Errors use the standard envelope: `404 WORKFLOW_RUN_NOT_FOUND` (also for
runs of another tenant — cross-tenant access is never a 403),
`503 WORKFLOWS_UNAVAILABLE` (service not wired), `401`/`403` from the RBAC
middleware.

## 2. API wiring (`cmd/api/main.go`)

### 2.1 Route registration — in `func (a *app) routes()`

BEFORE:

```go
	registerWorkflowsRoutes(apiMux, a.wfSvc, a.apSvc, a.runsSvc, a.queueSvc, a.authSvc, a.apiKeysSvc)
	registerVersionsRoutes(apiMux, a.versionsSvc, a.authSvc, a.apiKeysSvc)
```

AFTER:

```go
	registerWorkflowsRoutes(apiMux, a.wfSvc, a.apSvc, a.runsSvc, a.queueSvc, a.authSvc, a.apiKeysSvc)
	registerWorkflowRunNodeRoutes(apiMux, a.wfSvc, a.authSvc, a.apiKeysSvc) // wave-3 3-c: checkpointed node timeline
	registerVersionsRoutes(apiMux, a.versionsSvc, a.authSvc, a.apiKeysSvc)
```

No import changes needed (`cmd/api/workflow_run_nodes.go` is `package main`).

### 2.2 (Optional) durability knobs on the API-side service

The recovery pass is worker-side; the API service only needs the knobs when
you want `ExecuteWorkflow` to stamp wall-clock deadlines
(`deadline_at`) on newly created runs. In `newApp`:

BEFORE (store mode, ~line 95):

```go
		a.wfSvc = workflows.NewServiceWithStore(workflows.NewPostgresStore(db))
```

AFTER:

```go
		a.wfSvc = workflows.NewServiceWithOptions(workflows.NewPostgresStore(db),
			workflows.WithStaleAfter(workflows.StaleAfterFromEnv()),
			workflows.WithDefaultRunDeadline(30*time.Minute)) // watchdog budget per run (0 disables)
```

(Same substitution for the in-memory branch `workflows.NewService()` →
`workflows.NewServiceWithOptions(nil, …)` if desired.)

## 3. Worker wiring (`cmd/worker/main.go`) — recovery loop

### 3.1 Imports

BEFORE:

```go
import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
	...
)
```

AFTER (add `errors`, `os/signal`, `syscall` and the workflows package):

```go
import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	...

	"agentos/internal/workflows"
)
```

### 3.2 Recovery loop (startup pass + ticker) — in `func main()`

BEFORE:

```go
	worker := queue.NewWorker(workQueue, processTask)

	logr.Info("agentos worker starting", "port", cfg.Worker.Port, "env", cfg.Env)
```

AFTER:

```go
	worker := queue.NewWorker(workQueue, processTask)

	// Wave-3 track 3-c: durable workflow recovery. One startup pass, then a
	// sweep every DefaultRecoveryInterval (1m). The pass times out runs past
	// their deadline_at (status timeout / WORKFLOW_RUN_TIMEOUT), orphans the
	// pending/running node checkpoints of stale runs (NODE_ORPHANED) and
	// re-enqueues their next pending node through workQueue. Safe to run in
	// several workers: Postgres candidates are selected FOR UPDATE SKIP
	// LOCKED and every transition is a guarded conditional UPDATE.
	wfSvc := workflows.NewServiceWithOptions(nil, workflows.WithStaleAfter(workflows.StaleAfterFromEnv()))
	recoveryCtx, recoveryStop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer recoveryStop()
	go func() {
		if err := workflows.NewRecoveryWorker(wfSvc, workflows.DefaultRecoveryInterval).Run(recoveryCtx); err != nil && !errors.Is(err, context.Canceled) {
			logr.Warn("workflow recovery loop stopped", "error", err)
		}
	}()

	logr.Info("agentos worker starting", "port", cfg.Worker.Port, "env", cfg.Env)
```

Notes:

- `workflows.NewServiceWithOptions(nil, …)` = in-memory service. **For a
  Postgres deployment the recovery loop MUST share the database**, otherwise
  it cannot see or repair orphaned rows:
  `workflows.NewServiceWithOptions(workflows.NewPostgresStore(db), workflows.WithStaleAfter(workflows.StaleAfterFromEnv()))`
  with the same `*sql.DB` the API uses.
- The empty org id in `RecoveryWorker.RunOnce` sweeps **every** tenant. This
  is intentional and internal-only (worker process; the HTTP surface never
  exposes it).

### 3.3 (Recommended) checkpoint-aware node execution in `processTask`

Workflow node tasks carry `workflow_run_id` + `node_id` in their payload
(wave-2 `enqueue` shape, unchanged). Wire the checkpoint API so a task replay
never re-executes finished work:

BEFORE (top of `processTask`, after payload decoding):

```go
		runID, _ := task.Payload["run_id"].(string)
		agentID, _ := task.Payload["agent_id"].(string)
		input, _ := task.Payload["input"].(string)
		if runID == "" || agentID == "" || input == "" {
			return fmt.Errorf("task payload missing run_id, agent_id or input")
		}
```

AFTER:

```go
		runID, _ := task.Payload["run_id"].(string)
		agentID, _ := task.Payload["agent_id"].(string)
		input, _ := task.Payload["input"].(string)
		if runID == "" || agentID == "" || input == "" {
			return fmt.Errorf("task payload missing run_id, agent_id or input")
		}

		// Wave-3 3-c: durable checkpointing of workflow node execution.
		workflowRunID, _ := task.Payload["workflow_run_id"].(string)
		nodeID, _ := task.Payload["node_id"].(string)
		var checkpoint *workflows.NodeRun
		if workflowRunID != "" && nodeID != "" {
			orgID, _ := task.Payload["organization_id"].(string)
			nr, nerr := wfSvc.BeginNodeRun(ctx, orgID, workflowRunID, nodeID, runID)
			switch {
			case errors.Is(nerr, workflows.ErrNodeRunTerminal):
				return nil // replayed task: this attempt is already finished
			case nerr != nil:
				return nerr
			}
			checkpoint = nr
		}
```

and on completion (next to the existing `runsService.UpdateStatus(...StatusCompleted...)`):

```go
		if checkpoint != nil {
			if run.Status == string(runs.StatusFailed) {
				_ = wfSvc.FinishNodeRun(ctx, orgIDOf(checkpoint), checkpoint.ID, workflows.RunStatusFailed, "NODE_FAILED")
			} else {
				_ = wfSvc.FinishNodeRun(ctx, orgIDOf(checkpoint), checkpoint.ID, workflows.RunStatusCompleted, "")
			}
		}
```

`BeginNodeRun` also heartbeats the parent workflow run, and long node
executions should call `wfSvc.HeartbeatNodeRun(ctx, orgID, checkpoint.ID)`
between steps to keep the lease alive (staleness threshold below).

## 4. Knobs

| Knob | Default | Effect |
|---|---|---|
| `AGENTOS_WORKFLOW_STALE_AFTER` | `5m` | Staleness threshold for the recovery pass (`workflows.StaleAfterFromEnv()`; Go duration, e.g. `90s`). A non-terminal run whose `heartbeat_at` (or `updated_at` for legacy rows) is older than this is orphaned + re-kicked. Also the node-lease window for `BeginNodeRun` re-claims. |
| `workflows.WithStaleAfter(d)` | `workflows.DefaultStaleAfter` (5m) | Programmatic override of the same knob (service option). |
| `workflows.WithDefaultRunDeadline(d)` | 0 (disabled) | Stamps `deadline_at = now + d` on newly executed runs; the watchdog transitions overdue runs to `timeout` + `WORKFLOW_RUN_TIMEOUT`. |
| `workflows.NewRecoveryWorker(svc, interval)` | `workflows.DefaultRecoveryInterval` (1m) | Recovery loop cadence (startup pass always runs once). |

`AGENTOS_WORKFLOW_STALE_AFTER` is read by the workflows package directly
(`workflows.StaleAfterFromEnv`) — `internal/config` is owned by track 3-a and
was not touched. Invalid/missing values fall back to the 5m default.

## 5. Migration

Apply `migrations/013_durable_workflows.sql` before deploying. It is fully
idempotent (`ADD COLUMN IF NOT EXISTS` / `CREATE … IF NOT EXISTS`,
forward-only) and hardens the existing `workflow_runs` / `workflow_node_runs`
tables from migration 006:

- `workflow_runs`: `attempt`, `locked_at`, `heartbeat_at`, `finished_at`,
  `deadline_at`, `error_code` + index `(status, heartbeat_at)`.
- `workflow_node_runs`: `attempt`, `locked_at`, `heartbeat_at`, `error_code`,
  unique arbiter `(workflow_run_id, node_id, attempt)` (the idempotency key:
  checkpoint inserts use `ON CONFLICT … DO NOTHING` against it) + index
  `(organization_id, status)`.

Pre-existing rows stay valid (attempt defaults to 0; the recovery queries
COALESCE `heartbeat_at` onto `updated_at` for legacy rows).

## 6. Recovery semantics (what the pass guarantees)

`workflows.Service.RecoverIncompleteWorkflowRuns(ctx, orgID) (recovered int, err error)`:

1. **Watchdog**: non-terminal runs past `deadline_at` → status `timeout`,
   `error_code = WORKFLOW_RUN_TIMEOUT`, their pending/running node checkpoints
   are failed with the same code. Runs over deadline are never re-kicked.
2. **Stale sweep** (`heartbeat_at`/`updated_at` older than the threshold):
   the run is claimed (`attempt++`, fresh lease, `FOR UPDATE SKIP LOCKED`
   candidate selection in Postgres), its pending/running node checkpoints are
   failed with `NODE_ORPHANED`, structural nodes converge, approval gates are
   re-materialized unless a pending approval exists (fail-safe: a gate is
   never bypassed), and the next pending agent/tool node is re-enqueued
   through the existing queue interface.
3. **Convergence**: runs whose every node is terminal finalize immediately
   (`completed`, or `failed` with the node's machine code — a genuine node
   failure fails the run fail-fast, mirroring the executor).

Delivery is at-least-once: a replayed task hits `BeginNodeRun`, which returns
`ErrNodeRunTerminal` for already-terminal attempts (never re-executes work)
and starts a fresh attempt row after `NODE_ORPHANED`. All queries filter by
`organization_id` except the worker-only whole-tenant sweep (`orgID = ""`).

## 7. OpenAPI

`api/fragments/durable-workflows.yaml` is standalone-valid OpenAPI 3.1;
components are prefixed `Wf3*` (no collision with wave-2 `Wf*`), the operation
carries `x-required-permission: workflows.read`, and all local `$ref`s
resolve inside the fragment (verified). Merge `paths` + `components` into
`api/openapi.yaml` per the contract.

## 8. Deviations / judgement calls

1. **`deadline_at` column**: the contract lists `attempt/locked_at/
   heartbeat_at/finished_at/error_code`; migration 013 also adds
   `deadline_at` ("as needed") — the watchdog needs a per-run budget
   (`WithDefaultRunDeadline` stamps it; `SetWorkflowRunDeadline` can pin it).
2. **Timeline `status` vocabulary**: the contract sketch shows
   `"status": "succeeded"`; the endpoint emits the existing workflow status
   vocabulary (`completed`, `failed`, `pending`, `running`,
   `waiting_approval`, `timeout`) to stay consistent with
   `GET /workflow-runs/{id}` and the pinned 2-a statuses. Machine failure
   detail lives in `error_code`.
3. **Whole-tenant sweep**: `RecoverIncompleteWorkflowRuns(ctx, "")` queries
   all tenants (worker-only path, documented in §3.2); every other store
   query is org-scoped per the contract's tenancy rule.
4. **Queue usage**: the engine carries the concrete `*queue.Queue` exactly
   like the wave-2 executor (`enqueue` helper); the queue package was not
   modified and no new interface was introduced.
5. **`go.mod` unchanged** — zero new dependencies (sqlmock was already a
   test dependency of the repo).
6. **Fail-fast on genuine node failure**: `FinishNodeRun(failed)` finalizes
   the parent run immediately (not only at full convergence), matching the
   executor's and the recovery pass's fail-fast semantics; `NODE_ORPHANED`
   failures never finalize a run.
7. **Env knob read in the workflows package** instead of `internal/config`
   (3-a-owned) — exposed as `workflows.StaleAfterFromEnv()`.

## 9. Tests

- `internal/workflows/durable_test.go`: 19 service-level tests — checkpoint
  state machine (first attempt / terminal replay refusal / fresh in-flight vs
  stale re-claim / orphan restart), heartbeat, deadline + env knob, recovery
  (orphan + re-enqueue, watchdog timeout, waiting_approval fail-safe,
  resume-after-decision, genuine-failure finalization, tenant scoping,
  worker loop), store-mode round trip + recovery pass (sqlmock).
- `internal/workflows/store_durable_test.go`: 11 sqlmock tests for the
  Postgres durable surface (checkpoint insert conflict, claim, heartbeat,
  guarded terminal transitions, orphan pass, claim-from-statuses, SKIP LOCKED
  listings incl. tenant filter, rollback-on-scan-error, nil-DB guard).
- `cmd/api/workflow_run_nodes_test.go`: 4 handler tests (auth, wire shape
  incl. `error_code: null`, tenant guard + VIEWER read, unknown run 404).
- Gates: `gofmt -l ./cmd ./internal` clean, `go vet ./...` clean,
  `go build ./...` OK, `go test -count=1 ./...` all green.
