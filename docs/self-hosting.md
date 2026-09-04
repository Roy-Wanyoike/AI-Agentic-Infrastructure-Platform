# Self-hosting AgentOS (Docker)

Production deployment for the AgentOS platform: API server, queue worker,
React dashboard, and the backing infrastructure (PostgreSQL, Redis, NATS) as a
single Docker Compose stack.

```
                host
  ┌────────────────────────────────────────────────────────┐
  │  web (nginx:80)          api (distroless, :8080)       │
  │   /            SPA        /healthz /readyz              │
  │   /api/v1 ───────▶ api ◀── AGENTOS_API (status calls)  │
  │   /v1 ───────────▶ ▲            │       ▲               │
  │                    │      worker│       │ (queue)       │
  │  :8080 (host)      │            ▼       ▼               │
  │                   │        postgres  redis  nats        │
  └────────────────────────────────────────────────────────┘
```

| Service   | Image                                  | Role |
|-----------|----------------------------------------|------|
| `web`     | `agentos/web` (nginx:alpine)           | Dashboard SPA + reverse proxy `/api/v1`, `/v1` → `api:8080` |
| `api`     | `agentos/api` (distroless, nonroot)    | HTTP API on `:8080` (`/healthz`, `/readyz`, `/v1`, `/api/v1`) |
| `worker`  | `agentos/worker` (distroless, nonroot) | Executes runs from the shared Redis queue |
| `migrate` | same image as `api`                    | One-shot SQL migration runner (compose profile `migrate`) |
| `postgres`| `postgres:16-alpine`                   | Persistence (schema via file-based migrations) |
| `redis`   | `redis:7-alpine` (AOF on)              | Shared task queue + distributed rate limiting |
| `nats`    | `nats:2-alpine` (JetStream)            | Event publishing + webhook delivery |

Zero-infrastructure mode still exists (no `DATABASE_URL` → in-memory stores),
but this guide targets the durable, multi-process deployment.

## Prerequisites

- Docker Engine 24+ and Docker Compose v2 (`docker compose version`)
- 2 vCPU / 2 GB RAM minimum (4 GB recommended once runs execute real models)
- Free host ports: `8080` (dashboard) and `8081` (API). Both are overridable.
- For the quickstart you need nothing else; for real model execution an
  OpenAI-compatible API key.

## Quickstart (one command)

```bash
cd agentos

# 1. Required secret (the secrets service refuses Postgres mode without it):
printf 'AGENTOS_SECRETS_MASTER_KEY=%s\n' "$(openssl rand -base64 32)" >> .env

# 2. Optional but recommended: change the database password
printf 'POSTGRES_PASSWORD=%s\n' "$(openssl rand -hex 16)" >> .env

# 3. Build, migrate, and start everything
make docker-up-prod        # == docker compose -f docker-compose.prod.yml --profile migrate up -d --build
```

Then:

- Dashboard: <http://localhost:8080> (register the first user on the login screen)
- API: <http://localhost:8081/v1> or through the dashboard origin
  <http://localhost:8080/api/v1> (same auth, same routes)
- Health: `curl -f http://localhost:8081/healthz`

`make docker-up-prod` includes the `migrate` one-shot service (profile
`migrate`), so the schema is applied on first boot. Later `up -d` invocations
without the profile just (re)start the long-running services.

## Configuration

Compose reads variables from the shell and from a `.env` file placed next to
`docker-compose.prod.yml`. Every variable below is optional unless marked
**required**; defaults in the compose file are evaluation-grade.

```dotenv
# --- required ---
AGENTOS_SECRETS_MASTER_KEY=base64-of-32-bytes   # openssl rand -base64 32

# --- strongly recommended ---
POSTGRES_PASSWORD=change-me
APP_ENV=production

# --- model provider (unset = deterministic offline mode) ---
OPENAI_API_KEY=sk-...
OPENAI_BASE_URL=https://api.openai.com/v1
AGENTOS_WORKER_MODEL=gpt-4o-mini

# --- recommended: worker → API status callbacks ---
AGENTOS_API_KEY=<API key created in the dashboard>

# --- host port overrides ---
AGENTOS_WEB_PORT=8080
AGENTOS_API_PORT=8081
```

### Environment variable matrix

Every variable the Go services read (evidence points at the code), plus the
compose-only knobs.

| Variable | Consumed by | Required | Default | Description |
|---|---|---|---|---|
| `AGENTOS_SECRETS_MASTER_KEY` | api | **Yes** (Postgres mode) | — | AES-256-GCM master key for encrypted org secrets; startup fails fast without it (`internal/secrets/crypto.go:49`, `cmd/api/main.go:140-144`). Must decode to 32 bytes. |
| `DATABASE_URL` | api, worker, migrate, seed | Yes in this stack | — (compose assembles it) | Postgres DSN; wins over `POSTGRES_*` (`internal/database/database.go:62`). Compose builds it from the `POSTGRES_*` defaults; URL-encode special characters in the password. |
| `POSTGRES_DB` | postgres container, DSN fallback | No | `agentos` | Database name (`internal/database/database.go:78`). |
| `POSTGRES_USER` | postgres container, DSN fallback | No | `agentos` | Database user (`database.go:73`). |
| `POSTGRES_PASSWORD` | postgres container, DSN fallback | No (recommended) | `agentos` | Database password (`database.go:77`). |
| `POSTGRES_HOST` / `POSTGRES_PORT` | api, worker | No | — / `5432` | Alternative to `DATABASE_URL` (`database.go:65-69`). Compose uses the URL form. |
| `POSTGRES_SSLMODE` | api, worker | No | `disable` | TLS mode for the DSN fallback (`database.go:82`). Use `require`+ for off-host databases. |
| `APP_ENV` | api, worker | No | `production` in compose | `production` = info-level JSON logs; `development` = debug (`internal/config/config.go:69`, `internal/logger/logger.go:7-13`). |
| `API_PORT` | api | No | `8080` (container-internal) | API listen port (`internal/config/config.go:70`). Compose maps host `${AGENTOS_API_PORT:-8081}` → `8080`. |
| `WORKER_PORT` | worker | No | `8081` | Log label only — the worker runs no HTTP server (`internal/config/config.go:71`, `cmd/worker/main.go:273`). |
| `AGENTOS_QUEUE` | api, worker | No (compose sets `redis`) | `memory` | Task queue backend: `memory` or `redis`. Redis mode fail-fast when Redis is unreachable (`internal/config/config.go:82`, `cmd/api/main.go:105-109`). |
| `REDIS_ADDR` | api, worker | With `AGENTOS_QUEUE=redis` | — (compose: `redis:6379`) | Redis endpoint `host:port` (`internal/config/config.go:96`, `internal/httpx/ratelimit.go:117`). |
| `REDIS_HOST` / `REDIS_PORT` | api, worker | No | — / `6379` | Fallback when `REDIS_ADDR` is unset (`internal/config/config.go:99-103`). |
| `REDIS_URL` | api rate limiter only | No | — | `redis://` URL alternative for the distributed rate limiter (`internal/httpx/ratelimit.go:120`). |
| `REDIS_QUEUE_KEY` | api, worker | No | `agentos:queue` | Redis list key holding tasks (`internal/config/config.go:85`). |
| `AGENTOS_NATS_URL` | api | No | — (noop publisher) | NATS for event publishing + webhook delivery; set but unreachable → in-memory fallback (`internal/events/publisher.go:12,181-199`). |
| `OPENAI_API_KEY` | worker + api eval runner | No | — (offline mode) | Enables live model execution against any OpenAI-compatible endpoint (`internal/models/provider_env.go:19`). |
| `OPENAI_BASE_URL` | worker + api eval runner | No | provider default | Base URL of the OpenAI-compatible API (`provider_env.go:23`). |
| `AGENTOS_WORKER_MODEL` | worker + api eval runner | No | per-agent config | Default/requested model id (`provider_env.go:27`). |
| `AGENTOS_FALLBACK_API_KEY` / `AGENTOS_FALLBACK_BASE_URL` | worker + eval runner | No | — | Failover endpoint after transient primary failures (`provider_env.go:31,35`). |
| `AGENTOS_PRICING_JSON` | api, worker | No | built-in table | USD-per-1M-token pricing overrides as JSON (`internal/models/pricing.go:33`). |
| `AGENTOS_EMBEDDING_BASE_URL` / `AGENTOS_EMBEDDING_MODEL` / `AGENTOS_EMBEDDING_API_KEY` | api (knowledge/RAG) | No | offline hash embedder | OpenAI-compatible `/embeddings` for RAG (`internal/knowledge/embedder.go:25-29`). |
| `AGENTOS_RATE_LIMIT_RPM` | api | No | `120` | Global rate limit, requests/minute; Redis-backed when `REDIS_ADDR` is set (`internal/httpx/ratelimit.go:104`). |
| `AGENTOS_CORS_ORIGINS` | api | No (recommended in production) | wildcard `*` | Comma-separated CORS allowlist (issue #55, `cmd/api/cors.go`). When set, `Access-Control-Allow-Origin` echoes ONLY listed origins (exact match, plus `Vary: Origin` and credentials-safe grants); unset/empty keeps the wildcard dev default. |
| `AGENTOS_WEBHOOK_SIGNING_KEY` | api | No | — | HMAC key for signed webhook deliveries (`cmd/api/main.go:255`). |
| `AGENTOS_SCHEDULER_POLL_INTERVAL` | api | No | code default | Scheduler trigger loop cadence, Go duration (`cmd/api/main.go:410`). |
| `AGENTOS_WORKFLOW_STALE_AFTER` | api, worker | No | code default | Staleness threshold for durable workflow recovery (`internal/workflows/durable.go:26`). |
| `AGENTOS_API` | worker | No (compose: `http://api:8080`) | `http://localhost:8080` | API base URL for worker status callbacks (`cmd/worker/main.go:226-229,277`). |
| `AGENTOS_API_KEY` | worker | Recommended | — | API key sent as `X-API-Key` on status callbacks (`cmd/worker/main.go:32,281`). Create a key in the dashboard; without it the callbacks 401. |
| `AGENTOS_API_PULL` | worker | No | `false` | Dev pull mode (poll `/v1/queue/pull` instead of consuming Redis). Leave unset in production (`cmd/worker/main.go:276`). |
| `AGENTOS_TOOL_SANDBOX` | worker | No | `off` | Process-isolated tool execution (`internal/sandbox/env.go:26`). Keep `off` in containers — `sandbox-exec` is not shipped in the distroless image. |
| `AGENTOS_SEED_PASSWORD` | `cmd/seed` (optional demo data) | No | built-in | Password for seeded demo login (`cmd/seed/main.go:43`). |
| `MIGRATIONS_DIR` | migrate | No | `./migrations` | Where the migration runner finds `NNN_name.sql` files (`cmd/migrate/main.go:36-42`); compose sets `/app/migrations`. |
| `AGENTOS_WEB_PORT` | compose only | No | `8080` | Host port for the dashboard (`80` inside the container). |
| `AGENTOS_API_PORT` | compose only | No | `8081` | Host port for direct API access (`8080` inside the container). |
| `JWT_SECRET` | api | **Yes in production** (`APP_ENV=production` refuses to boot without it) | built-in dev secret | HMAC signing secret for session tokens (issue #55, `cmd/api/prodguard.go`). When set it is honored in every environment; generate with `openssl rand -base64 48`. compose-prod enforces it with a `:?` guard. |
| `AGENTOS_BILLING_ENFORCEMENT` | — | — | — | **Not referenced by any code today** (verified by search). Reserved/declarative only; listed here so nobody wires it expecting behavior. |
| `VITE_API_URL` / `VITE_API_PREFIX` | web (build args) | No | `/api` / `v1` | API base + prefix baked into the dashboard bundle (`web/src/lib/api/client.ts:18-22`). Defaults give same-origin calls through the nginx proxy. Override with `--build-arg` for split-domain hosting. |
| `VITE_APP_NAME` / `VITE_ENV` | web (build args) | No | `AgentOS` / `production` | UI chrome labels. |

## Migrations

Migrations are **file-based** SQL (`migrations/NNN_*.sql`, applied in order by
`internal/database.LoadMigrations` / `ApplyMigrations` with a
`schema_migrations` ledger — idempotent, safe to re-run). The files are baked
into the `agentos/api` image at `/app/migrations`, and the one-shot `migrate`
service runs `/app/migrate --up` against Postgres:

```bash
# First boot (already included in make docker-up-prod):
docker compose -f docker-compose.prod.yml --profile migrate up -d

# Re-run after pulling a new version (upgrades, new migration files):
docker compose -f docker-compose.prod.yml --profile migrate run --rm migrate
# or: make docker-migrate
```

`cmd/migrate` resolves its DSN from `DATABASE_URL` first (container path),
falling back to the built-in localhost dev default, so `make migrate-up`
(`go run ./cmd/migrate --up`) keeps working unchanged for local development.

## Backup & restore

The database lives in the named volume `agentos_pgdata`. Everything else is
rebuildable state (Redis queue contents drain, NATS JetStream holds events,
the dashboard is static).

Backup:

```bash
# Consistent logical backup (run while the stack is up)
docker compose -f docker-compose.prod.yml exec -T postgres \
  pg_dump -U agentos -d agentos --format=custom \
  > "agentos-$(date +%Y%m%d-%H%M%S).dump"
```

Restore into a fresh volume:

```bash
docker compose -f docker-compose.prod.yml down            # stop writers
docker volume rm agentos_pgdata                            # destructive!
docker compose -f docker-compose.prod.yml up -d postgres   # empty DB
# restore the custom-format dump:
docker compose -f docker-compose.prod.yml exec -T postgres \
  pg_restore -U agentos -d agentos --clean --if-exists < agentos-20250101-000000.dump
docker compose -f docker-compose.prod.yml --profile migrate run --rm migrate
docker compose -f docker-compose.prod.yml up -d            # bring the stack back
```

Store dumps off-host (object storage, another machine). Test restores
periodically — a backup you have never restored is a hypothesis.

## Upgrades

```bash
git pull                      # or check out the release tag
make docker-build             # rebuild api/worker/web images
make docker-migrate           # apply any new migrations (idempotent)
docker compose -f docker-compose.prod.yml up -d   # roll the services
```

Order matters: migrate **before** restarting `api`/`worker` so new binaries
never meet an old schema. Rollback is `git checkout <previous-tag>` +
rebuild + restore the last pre-upgrade dump (migrations are additive by
convention but the runner has no down-migration for arbitrary versions).

## Reverse proxy & TLS

Terminate TLS one hop in front of the stack. The compose file intentionally
does not ship certificates; point your existing proxy (Caddy, nginx, cloud
LB) at the published ports:

```
example.com            → 127.0.0.1:8080   # dashboard (+ /api/v1, /v1 passthrough)
api.example.com        → 127.0.0.1:8081   # direct API for SDKs / agentosctl
```

Notes:

- Bind the published ports to loopback (`AGENTOS_WEB_PORT=127.0.0.1:8080`
  style values in `.env` do **not** work — edit the compose `ports` entries or
  use an override file) when everything should traverse the TLS proxy.
- SSE (run timelines) is served from `GET /api/v1/runs/{id}/events`;
  `deploy/nginx.conf` already disables proxy buffering and raises read
  timeouts. Replicate `proxy_buffering off;` + long `proxy_read_timeout` in
  any external proxy.
- Do not terminate TLS *at* the api container; the Go server expects plain
  HTTP behind a trusted network hop.

## Operations

```bash
docker compose -f docker-compose.prod.yml ps          # status + health
docker compose -f docker-compose.prod.yml logs -f api worker
make docker-down-prod                                 # stop everything (volumes kept)
docker compose -f docker-compose.prod.yml down -v     # ⚠ also deletes data
```

- **Healthchecks**: `api` is probed with the static `/app/healthcheck` Go
  binary against `/readyz` (distroless has no shell, so no `curl`-based
  checks); postgres/redis/nats use native probes; `web` uses busybox `wget`.
  The worker intentionally has no HTTP healthcheck — watch its logs.
- **Scaling runs**: `docker compose ... up -d --scale worker=2`. Workers share
  the Redis queue; workflow recovery uses `FOR UPDATE SKIP LOCKED`, so extra
  workers are safe.
- **Infrastructure ports**: postgres/redis/nats are *not* published to the
  host. Reach them with `docker compose -f docker-compose.prod.yml exec
  postgres psql -U agentos -d agentos` (or a temporary override file).

## Known limitations (honest list)

1. **JWT signing secret is not configurable** — `cmd/api/main.go:52-54`
   hardcodes a dev secret. Run the platform behind your network/TLS boundary;
   API keys are org-scoped and unaffected.
2. **`AGENTOS_BILLING_ENFORCEMENT` does nothing** — no code reads it today.
3. **Sandboxed tools (`AGENTOS_TOOL_SANDBOX=exec`) are unavailable in the
   worker image** — distroless ships no `sandbox-exec` binary. Run the worker
   on a VM/host image with the sandbox if you need process isolation.
4. **First-run admin**: any visitor can register the first account. Put the
   dashboard behind your proxy's access control until the org is claimed.
