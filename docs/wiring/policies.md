# Wiring — Track 2-c: Policies + Rate limiting + Idempotency

Everything in this track lives in NEW files; no shared file was edited except
`internal/database/database_test.go` (see "Shared-file touch" below). This doc
gives the orchestrator the exact lines to add to `cmd/api/main.go`.

## Files delivered

| File | Purpose |
|------|---------|
| `internal/policies/policy.go` | Policy/Conditions records, validation, evaluation engine |
| `internal/policies/service.go` | Dual-mode service (in-memory + Store) |
| `internal/policies/store.go` | Store interface + Postgres implementation |
| `internal/policies/*_test.go` | Engine table tests, CRUD org-scoping, sqlmock store tests |
| `internal/auth/permissions_policies.go` | `PermissionPoliciesRead/Write` + `init()` role grants |
| `internal/httpx/ratelimit.go` | Redis sliding-window rate limiter middleware |
| `internal/httpx/idempotency.go` | Idempotency-Key middleware + memory/Postgres stores |
| `cmd/api/policies.go` | Handlers + `registerPoliciesRoutes` + `newPoliciesService` |
| `migrations/008_policies.sql` | `policies`, `idempotency_keys` tables |
| `api/fragments/policies.yaml` | OpenAPI 3.1 fragment (merge into `api/openapi.yaml`) |

## 1. Policy routes

In `(*app).routes()` (cmd/api/main.go, inside the `apiMux` section, e.g. after
the `/metrics` registration at line ~126):

```go
registerPoliciesRoutes(apiMux, newPoliciesService(a.db), a.authSvc, a.apiKeysSvc)
```

`newPoliciesService(db *sql.DB)` returns the Postgres-backed service when
`db != nil`, the in-memory service otherwise (zero-infrastructure mode). No
`app` struct change is needed.

Routes registered (all under `/v1` and `/api/v1` via the existing mount):

- `GET /policies` — `policies.read`
- `POST /policies/create` — `policies.write`
- `GET /policies/{id}` — `policies.read`
- `PUT /policies/{id}` — `policies.write`
- `DELETE /policies/{id}` — `policies.write`
- `POST /policies/evaluate` — `policies.read` (read-only operation)

RBAC grants (registered by `internal/auth/permissions_policies.go` `init()`):
`policies.read` → OWNER/ADMIN/MEMBER/VIEWER; `policies.write` → OWNER/ADMIN.

## 2. Rate limiting (global) + idempotency (POST /runs)

In `main()` / `routes()` — recommended wiring:

```go
// imports: net/http, "agentos/internal/httpx", "agentos/internal/observability"

// Rate limit: Redis when REDIS_ADDR/REDIS_URL is set, in-memory fallback
// otherwise. AGENTOS_RATE_LIMIT_RPM (default 120) sets requests/minute.
limit, window := httpx.RateLimitFromEnv()
rateLimit := httpx.NewRateLimitMiddleware(
    httpx.RedisClientFromEnv(),                     // *redis.Client or nil
    observability.NewRateLimiter(limit, window),    // in-memory fallback
    limit, window,
)

// Idempotency: Postgres store when db is available, in-memory otherwise.
idempotent := httpx.NewIdempotencyMiddleware(httpx.NewIdempotencyStoreFromDB(db))
```

Apply globally in `routes()` (outermost, before CORS so 429s are JSON):

```go
return rateLimit(corsMiddleware(mux))
```

Apply idempotency to the run-creation route (auth outermost so 401s are never
cached; the middleware passes GETs through untouched):

```go
apiMux.Handle("/runs",
    auth.RequireAuthOrAPIKey(a.authSvc, a.apiKeysSvc)(
        idempotent(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            // existing method switch: listRunsHandler / createRunHandler
        }))))
```

The same `idempotent` wrapper can be applied to any stateful POST route
(e.g. `/policies/create`, `/agents/create`). Requests carrying
`Idempotency-Key` are stored (status+body, 24h) and replayed with
`X-Idempotent-Replay: true`; 5xx responses are never stored.

### Optional per-scope buckets

The limiter key is `ratelimit:{scope}:{id}`; scope defaults to `api` and id is
the API key / bearer token hash / client IP. To give a route family its own
bucket (e.g. `execute` for run creation), wrap the limiter with a scope
injection (scope wrapper outermost):

```go
scopedExecute := func(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        next.ServeHTTP(w, r.WithContext(httpx.WithRateLimitScope(r.Context(), "execute")))
    })
}
// per-route: scopedExecute(rateLimit(handler))
```

## 3. Environment variables

| Variable | Default | Meaning |
|----------|---------|---------|
| `AGENTOS_RATE_LIMIT_RPM` | `120` | Requests per window (window = 1 minute) |
| `REDIS_ADDR` / `REDIS_URL` | empty | Redis for the distributed limiter; empty → in-memory fallback |

`internal/config` keeps its env helpers unexported, so the rate-limit env
contract lives in `httpx.RateLimitFromEnv()` as the contract anticipated.

## 4. Migration

`migrations/008_policies.sql` (owned by 2-c): `policies` (tenant-scoped,
JSONB actions/conditions, priority/enabled, org FK + cascade) and
`idempotency_keys` (PK `(organization_id, key, scope)`, 24h `expires_at`,
org FK + cascade, expiry index). Auto-discovered by the existing runner;
no registry edit.

## 5. Evaluation semantics (implementation notes for reviewers)

- A policy **matches** when: enabled, resource type equals the request
  resource type (or `*`), action listed (empty list = all actions), and every
  specified condition holds.
- Conditions are applicability predicates:
  - `tool_allowlist` (non-empty): request tool must be in the list
    (`context.tool` overrides `resource.id`);
  - `environments` (non-empty): `context.environment` must be in the list;
  - `max_cost_cents` (set ≥ 0): budget-guard — matches requests with
    `estimated_cost_cents > max_cost_cents` (so one deny rule blocks
    over-budget requests; an allow rule with the same condition explicitly
    authorizes them);
  - `require_approval`: not a predicate; the winning policy's decision reason
    notes "approval required before execution".
- Resolution: highest `priority` wins; ties → deny beats allow; then earliest
  `created_at`, then id (deterministic). No match → `allow` with
  `matched_policy_id: ""` and reason `no matching policy; default allow`.

## 6. Deviations / judgement calls (explicit, not silent)

1. `POST /policies/evaluate` requires `policies.read` (the contract pins
   permissions for read/write but not for evaluate; evaluate mutates nothing).
2. Error responses use the structured envelope
   `{"error":{"code","message"}}`; codes: `bad_request` (400),
   `invalid_policy` (422), `not_found` (404), `rate_limited` (429),
   `internal_error` (500). The contract's workflows track uses 422 for
   validation; policies follow the same convention.
3. The 429 body reuses `httpx.WriteError` (adds `request_id` when the
   RequestID middleware is present) — a superset of the pinned error shape.
4. Idempotency records are keyed by `(owner, key, scope=path)`; owner is the
   authenticated organization when a wrapper injects
   `httpx.WithIdempotencyOwner`, else a SHA-256 prefix of the caller's API
   key / bearer token, else client IP. Duplicate concurrent requests with the
   same key are not coalesced (single-flight is a future hardening).
5. `internal/database/database_test.go` (not on the shared do-not-edit list)
   gained one expectation entry for migration 008 in `TestApplyMigrations`
   — required because the test applies every file `LoadMigrations` finds.
   Tracks 2-a/2-b will need the same one-line addition for 006/007.
6. Rate limiter identity is derived from credentials/IP headers rather than
   the auth package (avoids coupling `httpx` → `auth`); the global middleware
   therefore also protects unauthenticated routes (`/auth/login`), keyed by
   IP. `httpx.WithRateLimitIdentity` / `WithIdempotencyOwner` let auth-aware
   wrappers pin a stronger identity.
7. `X-RateLimit-Limit` / `X-RateLimit-Remaining` headers are emitted in
   addition to the required `Retry-After`.

## 7. Verification

- `gofmt -l ./cmd ./internal` → empty
- `go build ./...` → green
- `go test ./...` → green (no infrastructure required: miniredis for Redis,
  sqlmock for Postgres stores, in-memory services elsewhere)
