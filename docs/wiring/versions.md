# Wiring — Track 2-b (Agent versions + Deployments)

Everything in this track lives in NEW files; `cmd/api/main.go` is NOT edited by
track 2-b. The orchestrator applies the lines below.

## 1. Files added by this track

| File | Purpose |
|------|---------|
| `migrations/007_versions_deployments.sql` | `agent_versions` snapshot columns (`snapshot`, `status`, `published_at`, `published_by`) + `deployments` table + partial unique index `uq_deployments_one_healthy` (one healthy deployment per agent+environment). Idempotent (IF NOT EXISTS guards, 005 style). Auto-discovered — no registry edit needed. |
| `internal/agents/versions.go` | `VersionsService` (dual-mode), `ConfigVersion`, `VersionStore` interface, snapshot/publish/rollback semantics. `service.go`/`store.go` untouched. |
| `internal/agents/versions_store.go` | `pgVersionsStore` (Postgres `VersionStore`); tenant guard via `agents.organization_id` join on every statement. |
| `internal/deployments/service.go` | Real dual-mode `Service` (`NewService(resolver)` / `NewServiceWithStore(store, resolver)`), lifecycle requested→validated→deploying→healthy, `failed`, rollback. Replaces the old stub. |
| `internal/deployments/store.go` | `pgStore` (Postgres `Store`); every query scoped by `organization_id`. |
| `internal/auth/permissions_deployments.go` | `PermissionDeploymentsRead`/`Write`/`Deploy` + `init()` grants per the contract RBAC table. `service.go`/`middleware.go` untouched. |
| `cmd/api/versions.go` | `registerVersionsRoutes` + `registerDeploymentsRoutes` + handlers + local `writeJSONVD`/`writeErrorVD`/`readJSONVD` helpers. |
| `api/fragments/versions.yaml` | OpenAPI 3.1 merge fragment (9 paths, 13 schemas). |
| Tests | `internal/agents/versions_test.go`, `internal/deployments/service_test.go`, `internal/deployments/store_test.go`, `internal/auth/permissions_deployments_test.go`, `cmd/api/versions_test.go` (httptest over the real middleware chain). |

## 2. Exact constructor lines for `cmd/api/main.go`

### 2a. `app` struct — add two fields

```go
type app struct {
        // ...existing fields...
        versionsSvc    *agents.VersionsService
        deploymentsSvc *deployments.Service
}
```

### 2b. `newApp` — construct the services (both modes)

Postgres mode (inside `if db != nil`):

```go
a.versionsSvc = agents.NewVersionsServiceWithStore(a.agentsSvc, agents.NewVersionsPostgresStore(db))
a.deploymentsSvc = deployments.NewServiceWithStore(deployments.NewPostgresStore(db), a.versionsSvc)
```

In-memory mode (inside `else`):

```go
a.versionsSvc = agents.NewVersionsService(a.agentsSvc)
a.deploymentsSvc = deployments.NewService(a.versionsSvc)
```

Notes:

- `a.versionsSvc` MUST be constructed after `a.agentsSvc` (it snapshots live
  agent config through it).
- `a.versionsSvc` is passed as the deployments `VersionChecker` — it implements
  `deployments.VersionChecker` via `ResolvePublishedVersion`, so deployments can
  only target published versions. Passing `nil` skips the check (dev/test only).
- Imports to add: `agentos/internal/deployments` (`agents`/`auth`/`apikeys` are
  already imported).

### 2c. `routes()` — register the routes (order does not matter; Go 1.22
ServeMux picks the most specific pattern over the `/agents/` catch-all)

```go
registerVersionsRoutes(apiMux, a.versionsSvc, a.authSvc, a.apiKeysSvc)
registerDeploymentsRoutes(apiMux, a.deploymentsSvc, a.authSvc, a.apiKeysSvc)
```

## 3. Endpoints + RBAC

All routes mount on `apiMux` → served under BOTH `/v1` and `/api/v1`.

| Method & path | Permission | Response |
|---|---|---|
| `GET /agents/{id}/versions` | `agents.read` | `{"versions":[{version,snapshot,published_at,published_by,status}]}` |
| `POST /agents/{id}/versions/create` | `agents.write` | `{"version":3}` (201) |
| `POST /agents/{id}/versions/{version}/publish` | `agents.write` | `{"version":3}` |
| `POST /agents/{id}/rollback` body `{"target_version":2}` | `agents.write` | `{"current_version":2}` |
| `GET /deployments?agent_id=` | `deployments.read` | `{"deployments":[{id,agent_id,version,environment,status,health:{error_rate,last_check_at},created_at,updated_at}]}` |
| `POST /deployments/create` body `{"agent_id","version","environment"}` | `deployments.write` | `{"deployment":{...}}` (201, status `requested`) |
| `GET /deployments/{id}` | `deployments.read` | `{"deployment":{...}}` |
| `POST /deployments/{id}/promote` | `deployments.deploy` | `{"deployment":{...}}` (one step: requested→validated→deploying→healthy) |
| `POST /deployments/{id}/rollback` | `deployments.deploy` | `{"deployment":{...},"rolled_back_to_version":2}` |

Errors: `{"error":{"code","message"}}` — `AGENT_NOT_FOUND`/`VERSION_NOT_FOUND`/
`DEPLOYMENT_NOT_FOUND` (404), `VERSION_ARCHIVED`/`INVALID_STATE`/
`NO_PREVIOUS_HEALTHY` (409), `VALIDATION_ERROR`/`VERSION_NOT_PUBLISHED` (422),
`INVALID_REQUEST` (400). Auth failures remain the middleware's plain-text
401/403 (unchanged platform behavior).

## 4. Semantics implemented

- **Versions**: `create` snapshots the CURRENT agent config (draft, numbered
  max(legacy v1, config versions)+1). `publish` flips draft→published
  (idempotent; `published_at` never reset), archives the previously published
  version and re-points `agents.current_version_id`. Snapshot bytes are NEVER
  mutated once written. `rollback` re-points the agent to the target version,
  (re-)publishes it, archives the previously published one and restores the
  live config from the snapshot (only fields present in the snapshot are
  applied; unknown target → 404). Archived versions are revived only via
  rollback (direct publish → 409 `VERSION_ARCHIVED`).
- **Deployments**: rows are an append-only ledger of (agent, version,
  environment) lifecycles. `create` validates the version exists AND is
  published (via the versions service) → status `requested`. `promote` advances
  exactly one step; `healthy`/`failed` are terminal (promote on terminal → 409).
  Reaching healthy DEMOTES the previous healthy row of the same
  agent+environment (status→failed + `superseded_at` + `health.superseded_by`),
  which keeps the partial unique index `uq_deployments_one_healthy` satisfied —
  one healthy deployment per agent+environment, in-memory AND in Postgres.
  `rollback` re-points the environment to the PREVIOUS healthy deployment's
  version by creating a NEW healthy row for that version (idempotent when the
  environment already serves it; 409 `NO_PREVIOUS_HEALTHY` when none exists).
- **Multi-tenancy**: every service query takes `organization_id` from auth
  claims (`auth.ExtractClaims`); client-supplied org ids are ignored.
  Cross-tenant rows surface as 404.

## 5. Migration / TestApplyMigrations

`migrations/007_versions_deployments.sql` is auto-discovered. The wave-2
data-driven `internal/database/database_test.go` already carries the
version-7 matcher (`migrationMatchers[7] = "ALTER TABLE agent_versions ADD
COLUMN IF NOT EXISTS snapshot"`); unknown versions fall back to `(?s).*`, so no
further edit is needed there unless a later track re-pins exact versions.

## 6. Judgement calls / deviations (per contract "note it, don't deviate silently")

1. **Version endpoints reuse `agents.read`/`agents.write`.** The contract's
   RBAC table pins no dedicated versions permission for track 2-b, and versions
   are agent configuration — so list uses `agents.read`, create/publish/
   rollback use `agents.write` (OWNER/ADMIN/MEMBER; VIEWER read-only).
2. **Deployments permission mapping**: `deployments.write` guards
   `POST /deployments/create` (members may REQUEST a deployment),
   `deployments.deploy` guards `promote`/`rollback` (only OWNER/ADMIN may
   change what serves traffic). The contract pins the three permissions but
   not their per-endpoint assignment.
3. **`snapshot` is an embedded JSON object** in responses (it IS the agent
   config document), not a stringified JSON value.
4. **Deployment wire shape** carries exactly the contract fields
   (`id, agent_id, version, environment, status, health{error_rate,last_check_at},
   created_at, updated_at`). Server-side extras (`created_by`,
   `superseded_at`, `health.superseded_by`, `health.error`) stay out of the
   wire shape except `health.superseded_by`/`health.error` which appear
   (omitempty) on demoted/failed rows so history stays explainable.
5. **Rollback creates a NEW healthy deployment row** for the previous version
   rather than mutating history — matches the append-only ledger design and
   keeps `superseded_at` audit data. The response returns that new row plus
   `rolled_back_to_version` per the contract.
6. **No `failed` HTTP endpoint**: the contract defines `failed` as a lifecycle
   status but no `/fail` route. `Service.FailDeploymentCtx` exists for the
   future worker (tested in `internal/deployments`); it is intentionally NOT
   exposed over HTTP.
7. **Publish/rollback audit**: no audit trail entries are written by these
   handlers (contract does not require them; the audit service signature was
   left out of the registration functions to keep deps minimal). Easy to add
   later next to the `claims.UserID` fetches if the orchestrator wants it.

## 7. Verification

- `gofmt -w ./cmd ./internal` clean; `go vet` on touched packages clean.
- `go build ./...` green; `go test ./... -count=1` all packages green.
- Tests: version immutability on publish (snapshot bytes + `published_at`
  never change), rollback semantics (config restored, previous archived,
  idempotent no-op, unknown target), deployment lifecycle transitions (one
  promote = one step; terminal rejected), rollback picks previous healthy +
  single-healthy invariant, sqlmock store tests (tenant guards, NULL handling,
  RowsAffected checks), httptest handler tests (401/403/404/409/422, contract
  JSON shapes, cross-tenant isolation, route precedence vs the `/agents/`
  catch-all).
