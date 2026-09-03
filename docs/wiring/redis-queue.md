# Wiring — Track 3-a: Redis queue + NATS events verification

Everything in this file describes edits the **orchestrator** applies to
`cmd/api/main.go` and `cmd/worker/main.go`. Track 3-a edited NO shared file;
all code lives in `internal/config` (queue knobs), `internal/queue`
(`NewFromConfig` + `Backend`) and `internal/events` (tests only).

## 0. What shipped

| Piece | Where |
|---|---|
| Queue mode selection | `internal/config`: `Config.Queue` (`AGENTOS_QUEUE`, `REDIS_ADDR`/`REDIS_HOST`+`REDIS_PORT`, `REDIS_QUEUE_KEY`) |
| Config-driven constructor | `internal/queue.NewFromConfig(cfg)` — one-liner backend selection |
| Storage-agnostic contract | `internal/queue.Backend` interface (satisfied by both `*Queue` and `*RedisQueue`) |
| Drain/close | `(*Queue).Close()` / `(*RedisQueue).Close()` — closes the Redis client; no-op in memory mode |
| NATS verification | `internal/events/nats_embedded_test.go` — real publish/subscribe against an embedded JetStream server (Option A, see §5) |

`RedisQueue` itself is UNCHANGED (contract: "already implemented and tested —
do not rewrite it; wire it"). The only touches in `queue.go` are the `redis`
delegation field on `*Queue` and the `Close` method.

## 1. `cmd/api/main.go` — BEFORE/AFTER

Imports: **no changes needed** — `agentos/internal/config`,
`agentos/internal/queue` and `os` are already imported.

### BEFORE (in `newApp`, the `a := &app{...}` literal)

```go
a := &app{
    cfg:        cfg,
    logr:       logr,
    db:         db,
    queueSvc:   queue.NewQueue(),
    metricsSvc: observability.NewMetrics(),
    streamSvc:  streaming.NewService(),
}
```

### AFTER

```go
// Task queue (track 3-a): AGENTOS_QUEUE selects the backend —
//   memory (default): in-process queue, zero infrastructure
//   redis:            shared Redis list across every API/worker process
// The constructor pings Redis and FAILS when AGENTOS_QUEUE=redis and Redis
// is unreachable: a silent memory fallback would split the task flow
// (producers enqueueing in memory, consumers reading Redis).
queueSvc, err := queue.NewFromConfig(cfg)
if err != nil {
    logr.Error("queue backend init failed", "error", err)
    os.Exit(1)
}

a := &app{
    cfg:        cfg,
    logr:       logr,
    db:         db,
    queueSvc:   queueSvc,
    metricsSvc: observability.NewMetrics(),
    streamSvc:  streaming.NewService(),
}
```

### BEFORE (in `main()`, after `application := newApp(cfg, logr, db)`)

```go
application := newApp(cfg, logr, db)
```

### AFTER (graceful shutdown: drain/close the Redis client)

```go
application := newApp(cfg, logr, db)
// Track 3-a: release the Redis connection on exit. Queue contents live in
// Redis and survive the process; Close only tears down the client (a no-op
// in memory mode, so it is safe unconditionally).
defer func() { _ = application.queueSvc.Close() }()
```

`newApp` keeps its `(cfg, logr, db) *app` signature — the constructor error is
handled with `os.Exit(1)` (fail-fast). If the orchestrator prefers an error
return from `newApp`, the same four lines move into `main()` unchanged.

## 2. `cmd/worker/main.go` — BEFORE/AFTER

Imports: **no changes needed** — `agentos/internal/config`,
`agentos/internal/queue` and `os` are already imported.

### BEFORE (line ~143)

```go
workQueue := queue.NewQueue()
```

### AFTER

```go
// Task queue (track 3-a): same AGENTOS_QUEUE selection as the API so both
// processes cooperate on one task flow (redis mode) or stay self-contained
// (memory mode, the default).
workQueue, err := queue.NewFromConfig(cfg)
if err != nil {
    logr.Error("queue backend init failed", "error", err)
    os.Exit(1)
}
defer func() { _ = workQueue.Close() }()
```

(`err` is already declared in `main` — `:=` is legal because `workQueue` is
new on the left-hand side.)

Note: the pre-existing `AGENTOS_API_PULL=true` pull loop bypasses the local
queue and stays as-is; with `AGENTOS_QUEUE=redis` it is unnecessary because
API and worker already share the Redis list (that is the point of the mode).

## 3. Env knobs

| Variable | Default | Effect |
|----------|---------|--------|
| `AGENTOS_QUEUE` | `memory` | `memory` → in-process queue (zero infrastructure); `redis` → shared Redis list. Unknown values fail startup with a clear error. Whitespace/case tolerated (`" Redis "` works). |
| `REDIS_ADDR` | — | Redis endpoint `host:port`, required in redis mode. Falls back to `REDIS_HOST:REDIS_PORT` (port default `6379`), matching `.env.example`. Also consumed by the wave-2 rate limiter (`internal/httpx`). |
| `REDIS_QUEUE_KEY` | `agentos:queue` | Redis list key for queued tasks (`queue.DefaultQueueKey`). All cooperating processes MUST agree on it. |
| `REDIS_HOST` / `REDIS_PORT` | — / `6379` | Legacy `.env.example` knobs; only used when `REDIS_ADDR` is unset. |

`.env.example` was NOT edited (not owned by 3-a). Suggested integration
addition, applied by the orchestrator if desired:

```
AGENTOS_QUEUE=memory
# REDIS_QUEUE_KEY=agentos:queue   # optional; default matches queue.DefaultQueueKey
```

`REDIS_HOST`/`REDIS_PORT` are already present there and are honored as the
fallback endpoint.

## 4. Runtime semantics (what redis mode changes)

- `Enqueue`/`Dequeue`/`Peek`/`Length`/`Requeue` hit the Redis list
  (`RPUSH`/`LPOP`/`LINDEX`/`LLEN`), so N API replicas and M workers share one
  task flow. `MarkStarted`/`MarkFailed`/`Ack` mutate the in-hand `*Task`
  (existing `RedisQueue` semantics, wired unchanged).
- Task JSON is the existing `queue.Task` envelope (`ID`, `Type`, `Payload`,
  `Status`, `Attempts`, timestamps) — unchanged on the wire.
- `queue.NewWorker(workQueue, handle)` and every other `*queue.Queue`
  consumer (handlers, `workflows.Engine`, `scheduler.NewWorker`) keep working
  without edits: `NewFromConfig` returns `*queue.Queue` which delegates to the
  Redis backend (see deviation D1).
- Startup fails fast (`os.Exit(1)`) when redis mode is requested but Redis is
  unreachable, or when `AGENTOS_QUEUE` holds an unknown value, or when redis
  mode is set without any address.

## 5. NATS events verification — choice made: **Option A (embedded server)**

The wave-3 contract allows `nats-server/v2` as a test-only dependency when
documented; it was reasonable and chosen, because the NATS path in
`internal/events` (JetStream stream setup, publish-ack, push consumers) had
only compile-time + connection-error coverage so far.

- **Test dependency:** `github.com/nats-io/nats-server/v2 v2.14.5`, imported
  ONLY by `internal/events/nats_embedded_test.go` (`_test.go` file — never
  linked into the API/worker binaries). `go.mod` justify-per-rule-7:
  single module, pure Go, no cgo, no external broker/docker needed.
- **Why v2.14.5 and not v2.14.6:** v2.14.6 declares `go 1.26.0` in its
  go.mod, which forces the module's `go` directive above the project's pinned
  Go 1.25 toolchain. v2.14.5 is the newest release that supports `go 1.25.0`.
- **What is verified for real** (`internal/events/nats_embedded_test.go`,
  embedded JetStream server with a `t.TempDir()` store):
  - `TestNATSPublisherEmbeddedRoundTrip` — `NewNATSPublisher` connects and
    creates stream `AGENTOS_EVENTS`; publish waits for the JetStream ack; a
    filtered subscriber receives the event with envelope fields intact; an
    independent core-NATS tap proves the pinned subject
    (`agentos.events.run.failed`) and the standard JSON envelope keys
    (`id`, `type`, `tenant_id`, `timestamp`); malformed payloads are skipped;
    foreign event types are filtered by subject.
  - `TestNATSPublisherEmbeddedSubscribeAllAndCancel` — empty types
    subscription becomes one consumer on `agentos.events.>` and receives
    multiple event types; `cancel()` unsubscribes + closes the channel and is
    idempotent; publishing after cancel does not panic.
- Subject naming and payload shape were already unit-tested
  (`TestSubjectFor`, `TestEventJSONShape`) and remain green.

Manual verification against real infrastructure (optional, compose):

```bash
docker compose up -d redis nats
AGENTOS_QUEUE=redis REDIS_ADDR=localhost:6379 make run-api &
AGENTOS_QUEUE=redis REDIS_ADDR=localhost:6379 make run-worker &
redis-cli llen agentos:queue          # grows when runs are created
nats sub --server nats://localhost:4222 "agentos.events.>"   # events stream
```

## 6. Deviations / judgement calls

1. **`NewFromConfig` returns `*queue.Queue`, not an interface.** The contract
   said "returning `queue.Queue`" (an interface). In this codebase
   `queue.Queue` is a concrete struct consumed directly by every call site
   (`app.queueSvc`, `queue.NewWorker`, `workflows.Engine`,
   `scheduler.NewWorker`) — an interface return would break those signatures
   and the "one-liner wiring" goal (and `cmd/api/main.go` must not be edited).
   The storage-agnostic contract exists as **`queue.Backend`**
   (compile-time-checked for both implementations); `*Queue` satisfies it and
   internally delegates to `*RedisQueue` when configured. `RedisQueue` is
   wired, not rewritten.
2. **Fail-fast, no silent fallback.** `AGENTOS_QUEUE=redis` with unreachable
   Redis aborts startup. Reason: a silent memory fallback splits the task
   flow between processes (producers in memory, consumers on Redis). This
   mirrors `events.NewNATSPublisher` returning an error so callers can decide.
3. **Mode normalization.** `AGENTOS_QUEUE` is trimmed and matched
   case-insensitively (`"Redis "` → redis) — operator-friendly; unknown values
   still fail with an error naming both supported modes.
4. **`nats-server/v2 v2.14.5` pinned** (test-only) instead of latest v2.14.6 —
   see §5. Without the pin, `go get` silently bumped the module to
   `go 1.26.0`, breaking the Go 1.25 toolchain gate.
5. **`.env.example` untouched** (not in the 3-a ownership map); fallback
   `REDIS_HOST:REDIS_PORT` keeps the existing file working as-is. Suggested
   additions listed in §3.
6. **Redis-backed `MarkStarted/MarkFailed/Ack` act on the in-hand task
   pointer** (pre-existing `RedisQueue` behavior, kept per the contract's
   "do not rewrite it"): status transitions are visible to the worker
   handling the task; the Redis list itself only holds queued tasks. Tests
   document this so it is a deliberate semantic, not a surprise.

## 7. Test coverage summary (no docker / external services)

- `internal/config` (`config_queue_test.go`): memory default, redis mode +
  addr/key knobs, `REDIS_ADDR` precedence over `REDIS_HOST:REDIS_PORT`,
  port default 6379.
- `internal/queue` (`backend_test.go`): default → memory (unset + explicit),
  redis round-trip of one task through the `Backend` interface against
  miniredis (enqueue → length → key existence → peek → dequeue →
  MarkStarted → Ack → Close), default key `agentos:queue`, mode
  normalization, invalid mode error, redis-without-addr error, fail-fast when
  Redis is down, worker process-next + retry/requeue through the Redis
  backend.
- `internal/events` (`nats_embedded_test.go`): the two embedded-server tests
  described in §5 (existing suite unchanged and still green).
