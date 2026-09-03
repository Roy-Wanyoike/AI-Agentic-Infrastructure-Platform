# Wiring — Track 2-e: Events (NATS) + Outbound Webhooks

Everything in this file describes edits the **orchestrator** applies to
`cmd/api/main.go` (and, if desired, `cmd/worker/main.go`, `docker-compose.yml`).
No shared file was edited by track 2-e; `registerWebhooksRoutes` and friends
live in `cmd/api/webhooks.go`, the services in `internal/events` and
`internal/webhooks`.

## 1. Publisher construction (in `newApp`, after the `db` if/else block)

```go
// Events publisher (track 2-e): AGENTOS_NATS_URL unset -> NoopPublisher;
// set + NATS reachable -> NATS JetStream; set + unreachable -> MemoryPublisher
// fallback. Never fails: the platform keeps running with zero infrastructure.
var publisher events.Publisher = events.NewFromEnv()
if db != nil {
    // Append-only audit of every published event (migration 010 `events`
    // table). Publish fails closed when the audit insert fails.
    publisher = events.NewAuditPublisher(events.NewPostgresStore(db), publisher)
}
```

Imports: `agentos/internal/events`.

`events.NewFromEnv()` reads `AGENTOS_NATS_URL` (constant
`events.EnvNATSURL`). Semantics:

| AGENTOS_NATS_URL      | Result                                                    |
|-----------------------|-----------------------------------------------------------|
| unset / empty         | `events.NewNoopPublisher()` (Publish validates + discards) |
| set, NATS reachable   | `events.NewNATSPublisher(url)` (JetStream stream `AGENTOS_EVENTS`, subjects `agentos.events.<event.type>`) |
| set, NATS down        | `events.NewMemoryPublisher()` fallback (ring buffer 1000 + subscriber channels) |

`NewNATSPublisher` returns an error when unreachable (2s connect timeout);
`NewFromEnv` catches it and falls back — the error path is covered by
`TestNewFromEnvFallsBackToMemoryWhenNATSDown` / `TestNATSPublisherUnreachableReturnsError`.

All three implementations satisfy both `events.Publisher` and
`events.Subscriber` (`Subscribe(types []string) (<-chan Event, func(), error)`).

## 2. Webhook service + delivery worker (in `newApp`)

```go
// Webhooks (track 2-e): dual-mode service; in Postgres mode the webhook and
// delivery records survive restarts (migration 010 tables).
var whSvc *webhooks.Service
if db != nil {
    whSvc = webhooks.NewServiceWithStore(webhooks.NewPostgresStore(db))
} else {
    whSvc = webhooks.NewService()
}
// Optional: derive endpoint secrets from a deployment-specific key instead of
// the dev default (webhooks.DefaultSigningKey). Changing the key invalidates
// secrets already handed out for webhooks created under the previous key.
whSvc.SetSigningKey(os.Getenv("AGENTOS_WEBHOOK_SIGNING_KEY"))

// Delivery worker: subscribes to the publisher, POSTs each matching event to
// the tenant's active webhooks, retries with 1s/5s/30s backoff (max 3
// attempts), records every attempt. client=nil -> http.Client{Timeout: 10s}.
whWorker := webhooks.NewWorker(whSvc, publisher.(events.Subscriber), nil, logr)
go func() {
    if err := whWorker.Run(context.Background()); err != nil && !errors.Is(err, context.Canceled) {
        logr.Warn("webhook delivery worker stopped", "error", err.Error())
    }
}()
```

Imports: `agentos/internal/webhooks` (plus `context`, `errors`, `os` already
in main.go's import set or trivially added).

Notes:

- `publisher.(events.Subscriber)` is safe for every implementation
  `NewFromEnv` can return (Noop/Memory/NATS all implement `Subscribe`; the
  noop variant returns a channel that never receives).
- Start the worker in EXACTLY ONE process (the API is recommended because the
  in-memory service/mode only knows about webhooks created in the same
  process; with Postgres mode a separate worker process would also work).
- The worker is started explicitly here — it is NOT wired into main.go by
  track 2-e (shared file); the orchestrator applies the lines above.

## 3. Routes (in `routes()`, alongside the other `apiMux.Handle` lines)

```go
registerWebhooksRoutes(apiMux, whSvc, a.authSvc, a.apiKeysSvc, a.auditSvc)
```

If the fields are preferred over locals, stash `publisher`/`whSvc` on the
`app` struct (owned by main.go) or pass through whatever mechanism the
orchestrator chooses — `registerWebhooksRoutes` takes explicit deps only.

## 4. Endpoints (all under `/api/v1` + legacy `/v1`)

| Method | Path                         | Permission       | Response |
|--------|------------------------------|------------------|----------|
| GET    | `/webhooks`                  | `webhooks.read`  | `{"webhooks":[{id,url,events,status,secret_set,created_at}]}` |
| POST   | `/webhooks/create`           | `webhooks.write` | `{"webhook":{...},"secret":"..."}` (secret returned ONCE) |
| DELETE | `/webhooks/{id}`             | `webhooks.write` | `{"deleted":true}` (foreign/unknown id → 404) |
| GET    | `/webhooks/{id}/deliveries`  | `webhooks.read`  | `{"deliveries":[{id,event_type,status,attempts,last_status_code,latency_ms,error,created_at}]}` `?limit=50` default, cap 500 |

Body for create: `{"url":"https://...", "events":["run.failed", ...]}`
(empty/omitted `events` = subscribe to ALL types). Validation → 422
`VALIDATION_ERROR`; malformed JSON → 400 `INVALID_REQUEST`.

RBAC grants (registered in `internal/auth/permissions_webhooks.go` via
`init()`; `service.go`/`middleware.go` untouched):

- `auth.PermissionWebhooksRead` = `webhooks.read` → OWNER, ADMIN, MEMBER, VIEWER
- `auth.PermissionWebhooksWrite` = `webhooks.write` → OWNER, ADMIN, MEMBER

## 5. How other services publish events

After the publisher is reachable where needed (stash it on `app`, pass it as a
constructor dep, or inject via a setter like the existing
`runsSvc.SetStreamer` pattern):

```go
evt := events.NewEvent(events.EventRunCompleted, orgID, "run", runID,
    map[string]any{"status": "completed"})
if err := publisher.Publish(r.Context(), evt); err != nil {
    logr.Warn("publish event failed", "error", err.Error())
}
```

Contract event types (constants in `internal/events/events.go`):
`agent.created`, `agent.updated`, `run.started`, `run.completed`,
`run.failed`, `run.cancelled`, `step.started`, `step.completed`,
`approval.requested`, `approval.decided`, `deployment.completed`,
`deployment.failed`, `webhook.received`.

Delivery payload (pinned by the contract, produced by the worker):

```json
{"id":"<event uuid>","type":"run.failed","timestamp":"2025-01-01T00:00:00Z","payload":{...}}
```

Headers on every attempt: `X-AgentOS-Signature: sha256=<hex hmac-sha256(secret, body)>`,
`X-AgentOS-Event-Id: <event uuid>`, `Content-Type: application/json`.
Receivers verify with `webhooks.VerifyPayload(secret, body, signatureHeader)`.

## 6. Migration

`migrations/010_events_webhooks.sql` (owned by track 2-e): tables `webhooks`,
`webhook_deliveries` (FK CASCADE into webhooks), `events` (append-only audit)
+ tenant-scoped indexes. `internal/database/database_test.go`
TestApplyMigrations was bumped to expect version **10** (the file was at 5 in
this worktree; if the merged tree already expects >= 10, the orchestrator
keeps the higher expectation and simply keeps the line).

## 7. Env vars / compose

| Variable | Effect |
|----------|--------|
| `AGENTOS_NATS_URL` | `nats://localhost:4222` locally / `nats://nats:4222` in compose. Unset → noop publisher (no deliveries, no NATS traffic). |
| `AGENTOS_WEBHOOK_SIGNING_KEY` | Optional. Key for deriving per-endpoint HMAC secrets. Dev default `agentos-dev-webhook-signing-key`. |

Note: `.env.example`'s `NATS_URL` is NOT read by this package — the contract
pins `AGENTOS_NATS_URL`; add the latter to compose (`api:`/`worker:`
environment) when enabling NATS.

## 8. Judgement calls / deviations (contract §Track 2-e)

1. **Secret storage scheme.** The contract says the create response returns
   the secret "returned ONCE" and "stores only hash". To keep deliveries
   working after restarts without persisting the raw secret, the secret is
   derived deterministically per endpoint:
   `secret = hex(HMAC-SHA256(signingKey, "webhook:"+webhookID))` with
   `signingKey = AGENTOS_WEBHOOK_SIGNING_KEY` (dev default). The stored value
   is `SHA-256(secret)` (`secret_hash`). Consequence: rotating the signing key
   invalidates previously issued secrets (documented on `SetSigningKey`).
   `webhooks.RandomSecret()` exists for callers that want an explicit random
   secret instead.
2. **Delivery record granularity.** One record per (webhook, event) pair —
   created on the first attempt and UPDATED on retries (attempts /
   last_status_code / latency_ms / error always mirror the latest attempt).
   The contract's delivery fields map 1:1; this matches "records every
   attempt" via an attempts counter + upsert instead of one row per attempt.
3. **Backoff schedule** is exactly 1s/5s/30s with max 3 attempts (the 30s
   entry only applies if `WithMaxAttempts` is raised above the contract's 3).
   Attempts made before `ctx` cancellation are recorded as `retrying`.
4. **HTTP status handling:** 2xx → `delivered`; anything else (3xx/4xx/5xx,
   transport error) retries then `failed`.
5. **`secret_set`** is `true` whenever `secret_hash` is non-empty (it always
   is for records created through the API).
6. **Delivery listing cap:** `limit` default 50 (contract), hard cap 500,
   invalid values → 400 `INVALID_REQUEST`.
7. **NATS subject:** `agentos.events.<type>` with dots preserved, e.g.
   `run.failed` → `agentos.events.run.failed` (JetStream wildcard
   `agentos.events.>` covered by stream `AGENTOS_EVENTS`).
8. **Audit publisher fails closed:** if the `events` audit insert fails,
   `Publish` returns the error and the event is not fanned out. In
   zero-infrastructure mode (db == nil) there is no audit store and the
   decorator is not installed.

## 9. Test coverage summary (no running infra required)

- `internal/events`: model/JSON shape/constants, noop + memory pub/sub
  (fan-out, filtering, ring cap 1000, cancel, concurrency, close),
  `NewFromEnv` noop default + NATS-down fallback, NATS constructor error on
  unreachable endpoint, subject mapping, audit publisher (persist+forward,
  fail-closed, pass-through), sqlmock pg audit store.
- `internal/webhooks`: secret returned once + hash-only storage, create
  validation, tenant scoping, event matching (wildcard + disabled skip),
  HMAC signing known vector, delivery secret == created secret, in-memory
  delivery records, worker end-to-end with memory publisher, signed payload
  assertion, retry-then-deliver (1s/5s backoff, fake client), give-up after
  max attempts, event/tenant/status filtering, ctx-cancel mid-backoff,
  transport error recording, sqlmock store CRUD + delivery upsert/list +
  nil-db guard.
- `cmd/api`: 401 unauthenticated (all 4 endpoints), VIEWER read-only (403 on
  write, 200 on reads), create → 201 with one-time secret + hash-only
  storage, validation 422/400 codes, cross-tenant delete/deliveries 404,
  limit parsing (truncate/`abc`/`0`), 405 matrix, `X-API-Key` auth path.
- Migration: TestApplyMigrations applies 010 against sqlmock.
