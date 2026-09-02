# AgentOS API Contract

Human-readable summary of the frozen API contract. The machine-readable
source of truth is [`api/openapi.yaml`](../api/openapi.yaml) (OpenAPI 3.1).
This document reflects the API **as implemented today** in `cmd/api` —
including documented limitations — so the frontend and worker can be built
against the real contract, not an aspirational one.

---

## 1. Base paths

| Base path | Status |
|---|---|
| `/api/v1/...` | **Canonical** (versioned REST API) |
| `/v1/...` | Legacy alias — same routes, served identically (worker + existing clients still use it) |
| `/healthz`, `/readyz`, `/`, `/metrics`-style platform routes | Unversioned; `/healthz` and `/readyz` live at the server root |

> Note: today `cmd/api` registers only the `/v1/...` routes. A parallel
> change adds the `/api/v1` alias so both prefixes serve the same handlers.
> Clients must target `/api/v1`.

## 2. Authentication

Two schemes, both accepted on every protected endpoint:

1. **Bearer token** — `Authorization: Bearer <token>`.
   `<token>` is the HMAC-SHA256 JWT-style token (24h expiry) returned by
   `POST /auth/login` (or created at registration time server-side). Claims:
   `user_id`, `organization_id`, `email`, `role`, `exp`.
2. **API key** — `X-API-Key: ak_...` header. Keys authenticate as the key's
   organization with the **OWNER** role. A dev key is created at API startup
   and logged. Browser `EventSource` clients (which cannot set headers) may
   pass `?api_key=<key>` as a legacy fallback.

### RBAC

Roles: `OWNER`, `ADMIN`, `MEMBER`, `VIEWER`. Permissions:
`agents.read`, `agents.write`, `runs.read`, `runs.execute`, `users.manage`,
`organization.manage`.

| Role | Permissions |
|---|---|
| OWNER | all |
| ADMIN | all except `organization.manage` |
| MEMBER | `agents.read`, `runs.read`, `runs.execute` |
| VIEWER | `agents.read`, `runs.read` |

Every operation below lists its required permission (also in the OpenAPI spec
via `x-required-permission`).

## 3. Endpoint table

| Method & path | Permission | Success | Notes |
|---|---|---|---|
| `POST /api/v1/auth/register` | — (public) | 201 | Creates org + first OWNER user. Response includes the user record **including PasswordHash (bcrypt digest)** — known wart, treat as opaque. |
| `POST /api/v1/auth/login` | — (public) | 200 | `{"token": "..."}` |
| `GET /api/v1/agents` | `agents.read` | 200 | JSON **array** of agents. Optional `?organization_id=` (defaults to caller's org). **Unpaginated.** |
| `POST /api/v1/agents/create` | `agents.write` | 201 | Creates agent (status `DRAFT`) + initial AgentVersion. |
| `GET /api/v1/agents/{id}` | `agents.read` | 200 | Agent JSON. |
| `PATCH /api/v1/agents/{id}` | `agents.read`* | 200 | **Not implemented:** the shared detail handler has no method switch — PATCH returns the agent unchanged, no mutation, no `agents.write` enforcement. |
| `DELETE /api/v1/agents/{id}` | `agents.read`* | 200 | **Not implemented:** same as PATCH — returns the agent unchanged, deletes nothing. |
| `GET /api/v1/runs` | `runs.read` | 200 | Envelope `{"runs":[...]}`. **Unpaginated; currently lists all orgs' runs** (org filter is future work). |
| `POST /api/v1/runs` | `runs.execute` | 201 | Creates run (`QUEUED`) + enqueues `agent.run` task. `{"run_id":"...","status":"queued"}`. |
| `GET /api/v1/runs/{id}` | `runs.read` | 200 | Run JSON. |
| `GET /api/v1/runs/{id}/events` | `runs.read` | 200 | SSE stream (with `Accept: text/event-stream`) or JSON history envelope. See §5. |
| `POST /api/v1/runs/{id}/events` | `runs.read`* | 204 | Worker event ingestion `{"type","name","payload"}`. Registered under the `runs.read` gate today; API keys (OWNER) pass. |
| `GET /api/v1/queue/pull` | `runs.execute` | 200 / 204 | Worker pull model: Task JSON or **204** when the queue is empty. `POST` accepted as alias. |
| `GET /api/v1/metrics` | `runs.read` | 200 | Metrics snapshot (§6). |
| `GET /healthz` | — (public) | 200 | Literal body `ok`. |
| `GET /readyz` | — (public) | 200 | Literal body `ready`. |
| `GET /` | — (public) | 200 | `{"service":"agentos-api","status":"running"}`. |

\* documented reality, flagged for review in the OpenAPI spec.

**Pagination:** list endpoints are **unpaginated today** (they return the full
result set). This is a documented limitation that will change in a future
minor version.

## 4. Field naming (important!)

Service structs carry **no JSON tags**, so resource payloads serialize with
**PascalCase** keys (this is reality, not a choice — do not invent camelCase
in clients):

```json
{
  "ID": "agent-1",
  "OrganizationID": "org-1",
  "Name": "Support Agent",
  "Description": "Demo customer support agent",
  "Instructions": "Answer simple questions",
  "Model": "gpt-4o-mini",
  "Status": "DRAFT",
  "CurrentVersionID": "version-agent-1-1",
  "CreatedAt": "2025-01-01T12:00:00.123456789Z",
  "UpdatedAt": "2025-01-01T12:00:00.123456789Z"
}
```

Request bodies, by contrast, use **snake_case** (`organization_id`, `agent_id`,
`input`, ...) because the handler request structs declare explicit json tags.
The SSE/history event payloads are **snake_case** too (`run_id`, `type`,
`name`, `payload`, `created_at`) — they are hand-built maps in the handler.

### Domain shapes (exact)

- **Agent**: `ID, OrganizationID, Name, Description, Instructions, Model, Status(DRAFT|ACTIVE|DISABLED), CurrentVersionID, CreatedAt, UpdatedAt`
- **AgentVersion** (domain-only today, no HTTP route yet): `ID, AgentID, Version(int), Instructions, Model, CreatedAt`
- **Run**: `ID, OrganizationID, AgentID, Input, Output, Status(QUEUED|RUNNING|COMPLETED|FAILED), CreatedAt, UpdatedAt`
- **Task** (queue): `ID, Type, Payload(map), Status(queued|running|completed|dead_letter), Attempts, CreatedAt, UpdatedAt, LastError`

## 5. SSE event protocol (`GET /runs/{id}/events`)

Request (`Accept: text/event-stream` switches from JSON history to SSE):

```bash
curl -N -H "Accept: text/event-stream" \
  "http://localhost:8080/api/v1/runs/<run_id>/events?api_key=<key>"
```

Response headers:

```
Content-Type: text/event-stream
Cache-Control: no-cache
Connection: keep-alive
```

Protocol:

- Frames are `data: <JSON>\n\n` — **no `event:` field is emitted**; clients
  dispatch on the `name` field inside the JSON.
- Buffered history is replayed first, then live events follow. The hardened
  `internal/streaming` service subscribes before snapshotting history and
  deduplicates by `created_at`, so nothing is lost or duplicated.
- Comment keep-alive pings (`: ping`) are written every ~15 s of idleness.
- The stream terminates on client disconnect.

Event frame payload (`RunEvent`):

```json
{
  "run_id": "3f6c2b62-2b57-4f8e-9e4e-2f4d3b6c9a10",
  "type": "status",
  "name": "status.changed",
  "payload": {"status": "COMPLETED", "output": "The result of 2+2 is 4", "ts": "2025-01-01T12:00:05Z"},
  "created_at": "2025-01-01T12:00:05.987654321Z"
}
```

Event catalog today:

| `type` | `name` | `payload` | Source |
|---|---|---|---|
| `status` | `status.changed` | `{"status":"QUEUED\|RUNNING\|COMPLETED\|FAILED","output"?,"ts"?}` | `runs.Service.UpdateStatus`, worker callbacks (`POST /runs/{id}/events`) |

Without the SSE `Accept` header the same endpoint returns JSON history:
`{"run_id":"...","events":[RunEvent, ...]}`.

## 6. Error model

Target structured error (internal/httpx, Content-Type `application/json`):

```json
{
  "error": {
    "code": "not_found",
    "message": "agent not found",
    "request_id": "1b8f0c47e51d2a3c4d5e6f708192a3b4"
  }
}
```

Stable codes: `bad_request`, `unauthorized`, `forbidden`, `not_found`,
`method_not_allowed`, `internal_error`. `request_id` echoes the
`X-Request-ID` correlation header (see §8).

**Migration note:** existing handlers still emit `http.Error`-style
`text/plain` bodies (e.g. `agent not found`). The structured model above is
the contract going forward; adoption in handlers is a follow-up step (the
helpers exist in `internal/httpx/errors.go`).

## 7. Worked examples (curl)

```bash
BASE=http://localhost:8080/api/v1

# 1. Register (public) — 201
curl -s -X POST $BASE/auth/register -H 'Content-Type: application/json' \
  -d '{"organization":"Acme AI","email":"owner@acme.dev","password":"s3cret-password"}'
# {"organization":{"ID":"org-1","Name":"Acme AI"},"user":{"ID":"user-1", ...,"Role":"OWNER",...}}

# 2. Login (public) — 200
TOKEN=$(curl -s -X POST $BASE/auth/login -H 'Content-Type: application/json' \
  -d '{"email":"owner@acme.dev","password":"s3cret-password"}' | python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])')

# 3. Create agent (agents.write) — 201 (PascalCase response)
curl -s -X POST $BASE/agents/create -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"Support Agent","description":"Demo customer support agent","instructions":"Answer simple questions","model":"gpt-4o-mini"}'

# 4. List agents (agents.read) — 200 (bare JSON array)
curl -s $BASE/agents -H "Authorization: Bearer $TOKEN"

# 5. Create run (runs.execute) — 201 (snake_case response!)
curl -s -X POST $BASE/runs -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"agent_id":"agent-1","input":"What is 2+2?"}'
# {"run_id":"3f6c2b62-...","status":"queued"}

# 6. Get run + stream events
curl -s $BASE/runs/3f6c2b62-2b57-4f8e-9e4e-2f4d3b6c9a10 -H "Authorization: Bearer $TOKEN"
curl -N -H "Accept: text/event-stream" "http://localhost:8080/api/v1/runs/3f6c2b62-2b57-4f8e-9e4e-2f4d3b6c9a10/events?api_key=<key>"

# 7. Worker pull (runs.execute) — Task JSON or 204
curl -s http://localhost:8080/api/v1/queue/pull -H "X-API-Key: ak_..."

# 8. Metrics (runs.read)
curl -s $BASE/metrics -H "Authorization: Bearer $TOKEN"
```

## 8. HTTP middleware stack (production wiring)

`internal/httpx` (stdlib-only) provides: request correlation, the JSON error
model, CORS, panic recovery and timeouts. `internal/observability` provides
the request-metrics middleware.

**Coordinator action — replace the hand-rolled CORS block in
`cmd/api/main.go` (the `handler := http.HandlerFunc(func(w, r) { ...CORS... })`
assignment) with the following ready-to-paste snippet.** `logr` and
`metricsService` already exist in `main()`; only the import of
`agentos/internal/httpx` is new. Order matters — outermost first:

```go
import (
        // ... existing imports ...
        "agentos/internal/httpx"
)

        // handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { ... })  // <- DELETE this CORS block
        handler := httpx.Chain(mux,
                httpx.RequestID,                                 // 1. X-Request-ID correlation (outermost)
                httpx.Recovery(logr),                            // 2. panic -> 500 JSON + structured log
                httpx.Timeout(30*time.Second),                   // 3. per-request timeout
                observability.MetricsMiddleware(metricsService), // 4. count/duration by route/method/status
                httpx.CORS(httpx.DefaultCORSOptions()),          // 5. CORS (innermost, wraps the mux)
        )
```

Notes for the coordinator:

- **SSE vs timeout:** `httpx.Timeout` uses `http.TimeoutHandler`, which kills
  long-lived responses after the duration. `/v1/runs/{id}/events` streams are
  long-lived by design. Recommended: keep `30s` for now (dev), and in the
  follow-up that adopts `httpx` per-route, exclude the SSE route (e.g. mount
  the events handler outside the Timeout wrapper) or raise the duration.
  `httpx.Timeout(0)` disables the wrapper.
- **CORS placement:** with the mandated order, preflight OPTIONS traverse
  correlation/recovery/metrics before being answered (204) — correct but it
  counts preflights under the `unmatched` route. If desired later, move
  `httpx.CORS` to position 1 (outermost) to short-circuit preflights;
  behavior remains valid either way.
- **`httpx.CORS` config for production:** replace
  `httpx.DefaultCORSOptions()` with explicit origins, e.g.
  `httpx.CORSOptions{AllowedOrigins: []string{"https://app.agentos.dev"},
  AllowCredentials: true}`. The default is the permissive dev profile
  (`*` origins; methods GET/POST/PATCH/DELETE/PUT/OPTIONS; headers
  Content-Type, Authorization, X-API-Key, api_key, X-Request-ID, Accept).
- **Error adoption (follow-up):** handlers should switch `http.Error(...)`
  calls to `httpx.ErrUnauthorized/ErrForbidden/ErrNotFound/ErrBadRequest/`
  `ErrInternal` (or `httpx.WriteError` for custom statuses) so every error is
  the JSON envelope with `request_id`.
- **Metrics route labels:** `ServeMux` patterns are not introspectable from
  outer middleware, so label requests via
  `observability.RouteName("/api/v1/agents", handler)` /
  `observability.RequestWithRouteName(r, "/api/v1/agents")` when refactoring
  handlers. Untagged routes are recorded as `unmatched`. Keys look like:
  `http_requests_total{route="/v1/agents",method="GET",status="200"}`.
- **Correlation:** `httpx.RequestID` accepts/creates `X-Request-ID`, echoes it
  on responses, and exposes it to handlers via
  `httpx.RequestIDFromContext(ctx)`; `httpx.Recovery` and the error model read
  the same ID.

## 9. Client integration checklist (frontend / worker)

- Base URL `/api/v1`; do **not** hardcode `/v1`.
- Send `Authorization: Bearer <token>` or `X-API-Key`; for `EventSource` use
  `?api_key=`.
- Expect PascalCase resource fields; snake_case request bodies and event
  payloads (§4).
- Dispatch SSE on the `name` field (`status.changed`), not an `event:` line.
- Treat list responses as unpaginated arrays/envelopes (§3).
- Read `X-Request-ID` from responses for support/debug correlation.
- Handle `204` from `queue/pull` as "no work available".
