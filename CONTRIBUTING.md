# Contributing to AgentOS

Thanks for your interest in contributing. This document covers the dev setup, the quality gates every change must pass, and the PR conventions the project follows.

## Dev setup

Prerequisites:

- **Go 1.25+**
- **Node 22+** (only for the React 19 dashboard in `web/`)
- **Python 3** (only if you touch OpenAPI fragments — see below)
- Optional, for durable mode: Docker (Postgres, Redis, NATS via `docker-compose.yml`)

```bash
git clone https://github.com/Roy-Wanyoike/AI-Agentic-Infrastructure-Platform.git
cd AI-Agentic-Infrastructure-Platform
go build ./...

# zero-infrastructure mode (in-memory): nothing else to install
go run ./cmd/api                                   # API on :8080

# durable mode (Postgres + Redis + NATS)
make docker-up && make migrate-up && make seed
make run-api                                       # + make run-worker in a second terminal
```

The exact same binary runs in both modes — see [dual-mode requirement](#dual-mode-requirement-postgres--in-memory).

## Makefile targets

| Target | What it does |
|---|---|
| `make build` | Build API and worker binaries (`go build ./...`) |
| `make test` | Run the Go test suite |
| `make lint` | `gofmt` check on `./cmd ./internal` + `go vet ./...` |
| `make seed` | Seed idempotent demo data (requires `make migrate-up` first) |
| `make run-api` / `make run-worker` | Run the API / the worker |
| `make docker-up` / `make docker-down` | Start / stop local Postgres, Redis, and NATS |
| `make migrate-up` / `make migrate-down` | Apply / roll back the last migration |

## Dual-mode requirement (Postgres + in-memory)

AgentOS persists through Postgres in production and fully in-memory in dev and CI — same binary, same service interfaces. This is a hard rule for every change:

- **Features must work in both modes.** Any new store method gets both a Postgres implementation and an in-memory implementation with identical behavior: pagination edges, error shapes, and tenant guards included.
- **Tests must cover both where relevant.** Domain packages test against the in-memory stores; if your change is Postgres-specific (SQL, constraints, keyset ordering), add coverage that exercises that path too, or document precisely why the memory store is the behavioral reference.
- Never gate a feature on infrastructure. Anything that needs Postgres/Redis/NATS must either degrade to a working in-memory path or return an explicit, honest "not exposed" response — never a silent mock.

## Quality gates

Every PR must pass **all** of these locally before it is opened (CI runs the same set):

```bash
gofmt -l ./cmd ./internal      # must print nothing
go vet ./...
go build ./...
go test -count=1 ./...         # 38+ packages, 0 failures
```

Additional gates depending on what you touched:

- **OpenAPI fragments** (`api/fragments/*.yaml`): every local `$ref` must resolve inside the fragment you are adding or editing — run `python3 scripts/check_fragment_refs.py <fragment>` for each changed fragment.
- **Web changes** (`web/`): the dashboard must still build and lint cleanly:
  ```bash
  cd web && npm run build && npm run lint
  ```

## Commit format

Conventional Commits — one logical change per commit:

```text
<type>(<scope>): <short imperative summary>

[optional body: what and why, not how]
```

Common types: `feat`, `fix`, `docs`, `refactor`, `test`, `chore`, `perf`, `ci`. Reference the issue in the summary or body, e.g. `feat(secrets): one-time reveal endpoint (#25)`.

## Pull request rules

- **One issue per PR.** Disjoint tracks stay in disjoint PRs; if you discover unrelated work, file a new issue instead of expanding the diff.
- **The PR body must link the issue** with a closing keyword (`fixes #45` / `closes #45`) so the tracker stays honest.
- **All gates must pass.** A PR that is "almost done" waits — only verified work merges.
- Keep the diff focused: no drive-by refactors, no formatting-only churn inside functional changes.
- New HTTP surfaces ship contract-first: the OpenAPI path/fragment lands with (or before) the handler.

## Reporting issues

- **Bugs**: include the exact request/response (redact secrets), the mode you ran in (Postgres or in-memory), and what the logs or metrics showed.
- **Security**: do **not** open a public issue — follow [SECURITY.md](SECURITY.md) and disclose privately via GitHub Security Advisories.
