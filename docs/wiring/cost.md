# Wiring — Track 3-b Cost tracking & pricing hook

How to wire the cost-tracking subsystem into `cmd/api/main.go` and
`cmd/worker/main.go` (the orchestrator performs the edits; this file
documents the exact lines). Migration `012_cost_tracking.sql` must be
applied (``make migrate-up``) **before** the new binaries run: the runs
store now writes `cost_cents` columns in every step insert.

## What gets mounted

`registerUsageCostsRoutes` (in `cmd/api/usage_costs.go`) mounts:

| Method | Path           | Permission   | Machine errors                                            |
|--------|----------------|--------------|-----------------------------------------------------------|
| GET    | `/usage/costs` | `usage.read` | 400 `INVALID_GROUP_BY`, 400 `INVALID_TIME_RANGE`, 503 `USAGE_UNAVAILABLE` |

The route is registered on `apiMux`, so it is served under both `/api/v1`
(canonical) and `/v1` (legacy alias). Auth wrap pattern matches every other
vertical (`auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(
auth.RequirePermission(authSvc, auth.PermissionUsageRead)(h))`). The
organization id always comes from the token claims, never from the client.

## Route registration (inside `routes()`)

```go
// cmd/api/main.go — routes() — BEFORE:
        registerPoliciesRoutes(apiMux, newPoliciesService(a.db), a.authSvc, a.apiKeysSvc)

// AFTER:
        registerPoliciesRoutes(apiMux, newPoliciesService(a.db), a.authSvc, a.apiKeysSvc)
        registerUsageCostsRoutes(apiMux, a.runsSvc, a.authSvc, a.apiKeysSvc) // wave-3 3-b
```

No new constructor is needed: the report aggregates `runs.cost_cents` via
`runs.Service.AggregateCostsCtx` on the **existing** `a.runsSvc` (Postgres
store path; falls back to in-memory aggregation with zero infrastructure).

## Eval-runner usage-source wiring (inside `newApp`)

The evaluations service prices each case from the runner's token usage
(`runtime.Run.Tokens`) through `models.ComputeCostCents`. It needs the
serving model, resolved via the optional `evaluations.UsageSource`
collaborator. Without this wiring every eval cost stays 0 (documented
offline behavior, never an error).

```go
// cmd/api/main.go — newApp() — BEFORE:
        if db != nil {
                a.evalSvc = evaluations.NewServiceWithStore(evaluations.NewPostgresStore(db), evaluations.Deps{
                        Agents: a.agentsSvc,
                        Runner: a.evalRunner,
                })
        } else {
                a.evalSvc = evaluations.NewService(evaluations.Deps{
                        Agents: a.agentsSvc,
                        Runner: a.evalRunner,
                })
        }

// AFTER: (identical, plus the pricing hook right after the if/else)
        if db != nil {
                a.evalSvc = evaluations.NewServiceWithStore(evaluations.NewPostgresStore(db), evaluations.Deps{
                        Agents: a.agentsSvc,
                        Runner: a.evalRunner,
                })
        } else {
                a.evalSvc = evaluations.NewService(evaluations.Deps{
                        Agents: a.agentsSvc,
                        Runner: a.evalRunner,
                })
        }
        // wave-3 3-b: price eval cases from reported token usage; the model
        // is resolved from the agent's configuration (best-effort: unknown
        // model -> 0 cents, never an error).
        a.evalSvc.AttachUsageSource(evaluations.UsageSourceFunc(func(orgID, agentID string) (string, bool) {
                agent, err := a.agentsSvc.GetAgentCtx(context.Background(), orgID, agentID)
                if err != nil {
                        return "", false
                }
                return agent.Model, true
        }))
```

Import to add in `cmd/api/main.go` (only for the strict-pricing-validation
snippet below; `evaluations` is already imported):

```go
"agentos/internal/models"
```

## Worker step costing (inside `cmd/worker/main.go`)

`runs.cost_cents` is bumped per step by the runs store, but the step's cost
value must be priced where the token usage is known — the worker's
`stepRecorderAdapter` (real costs only flow once the worker has a provider
configured via `AGENTOS_PROVIDER_API_KEY`-style env, as today):

```go
// cmd/worker/main.go — stepRecorderAdapter — BEFORE:
                if step.TokenUsage.TotalTokens > 0 {
                        rs.TokenUsage = map[string]any{
                                "prompt_tokens":     step.TokenUsage.PromptTokens,
                                "completion_tokens": step.TokenUsage.CompletionTokens,
                                "total_tokens":      step.TokenUsage.TotalTokens,
                        }
                }
                return runsService.RecordStep(ctx, run.OrganizationID, runID, rs)

// AFTER:
                if step.TokenUsage.TotalTokens > 0 {
                        rs.TokenUsage = map[string]any{
                                "prompt_tokens":     step.TokenUsage.PromptTokens,
                                "completion_tokens": step.TokenUsage.CompletionTokens,
                                "total_tokens":      step.TokenUsage.TotalTokens,
                        }
                        // wave-3 3-b: price model steps through the pricing hook
                        // (unknown model -> 0 cents, never an error).
                        if step.Type == runtime.StepTypeModel {
                                if agent, ok := runsService.AgentModelFor(run.AgentID); ok {
                                        rs.Cost = models.ComputeCostCents(agent,
                                                step.TokenUsage.PromptTokens, step.TokenUsage.CompletionTokens)
                                }
                        }
                }
                return runsService.RecordStep(ctx, run.OrganizationID, runID, rs)
```

Note: the adapter has the agents service (`agentsvc`) in scope in
`cmd/worker/main.go`; use `agentsvc.GetAgentCtx(context.Background(),
run.OrganizationID, run.AgentID)` and its `.Model` directly instead of the
illustrative `AgentModelFor` helper above. `rs.Cost` (USD cents) is then
persisted to BOTH `run_steps.cost_cents` and `run_steps.cost` and bumped
into `runs.cost_cents` atomically by the runs store's single-statement CTE.
Add `"agentos/internal/models"` to the worker imports.

## Pricing env knobs

| Env var               | Meaning                                                                                     |
|-----------------------|---------------------------------------------------------------------------------------------|
| `AGENTOS_PRICING_JSON` | Optional JSON array overriding/extending the built-in price table, e.g. `[{"model":"gpt-4o","input_per_m_tokens":2.5,"output_per_m_tokens":10}]` (USD per 1M tokens). Re-parsed automatically when the raw value changes; invalid JSON is ignored (built-ins stay effective — pricing never fails a run). |

Built-in table (`internal/models/pricing.go`): gpt-4o, gpt-4o-mini,
gpt-4.1, gpt-4.1-mini, gpt-4.1-nano, o3-mini, llama-3.1-8b-instruct,
llama-3.1-70b-instruct, mistral-small, mistral-small-latest. Matching is
case-insensitive; `vendor/model` ids (OpenRouter style) match their bare
suffix. **Unknown model → 0 cents, never an error** (documented contract).

Programmatic overrides (optional, for tests/config files):
`models.SetPricing([]models.ModelPricing{...})` (nil resets).

Optional strict startup validation (surface typos in the env override at
boot instead of silently falling back to built-ins):

```go
// cmd/api/main.go — main(), after cfg := config.Load() (optional):
        if raw := strings.TrimSpace(os.Getenv(models.PricingEnvVar)); raw != "" {
                if _, err := models.PricingFromJSON(raw); err != nil {
                        logr.Warn("invalid pricing override ignored", "env", models.PricingEnvVar, "error", err.Error())
                }
        }
```

## Migration

`migrations/012_cost_tracking.sql` (this track owns it). Auto-discovered by
`internal/database.LoadMigrations`; no registry edits needed. Additive and
idempotent: `cost_cents DOUBLE PRECISION NOT NULL DEFAULT 0` on
`run_steps` and `runs`, plus `idx_runs_org_created_cost`
(`organization_id, created_at` INCLUDE (`agent_id`, `cost_cents`)) for the
report's hot path. Existing `run_steps.cost` (migration 005) is kept and
written with the same value for backwards compatibility.

## Endpoint contract

`GET /v1/usage/costs?from=&to=&group_by=day|agent|model` → 200:

```json
{"total_cost_cents": 1.75, "series": [{"bucket": "2026-09-03", "cost_cents": 1.75, "runs": 4}]}
```

- `from`/`to`: RFC3339 or `YYYY-MM-DD` (UTC). Defaults: last 30 days ending
  now. Window is half-open `[from, to)`, capped at 366 days.
- `group_by=day` → `bucket` ("YYYY-MM-DD", UTC); `group_by=agent` →
  `agent_id`; `group_by=model` → `model` (resolved from the agents catalog;
  runs of deleted agents aggregate under an empty model label).
- `total_cost_cents` is the sum over `series`; `series` is `[]` (never
  null) when empty.
- OpenAPI: `api/fragments/cost.yaml` (`Usg*` prefix, standalone-valid).

## Tests

- `internal/models/pricing_test.go` — pricing math, built-in/override/env
  precedence, JSON validation, unknown-model and negative-token rules,
  vendor/model matching, SetPricing.
- `internal/runs/service_test.go` — in-memory cost accumulation, store-mode
  persistence + tenant guard, aggregate input validation, in-memory
  aggregation (day/agent), store-driven aggregation via fake store.
- `internal/runs/store_test.go` — sqlmock: atomic step insert + run-total
  bump CTE, tenant-guard rejection, cost total scan, the three aggregate
  queries (tenant guard + window + grouping SQL), store guard.
- `internal/evaluations/service_test.go` — real pricing assertions
  (AttachUsageSource + `models.ComputeCostCents`), offline/zero-cost paths.
- `internal/auth/permissions_usage_test.go` — `usage.read` grants.
- `cmd/api/usage_costs_test.go` — handler: group_by day/agent/model shapes,
  empty series, INVALID_GROUP_BY, INVALID_TIME_RANGE table, bare dates,
  auth required, tenant-scoped store call.

## Deviations / decisions

1. **Permission constant**: the contract sketch wrote `usage:read`, but
   every permission constant in `internal/auth` uses the dot convention
   (`agents.read`, `runs.read`, ...). The shipped constant is
   **`usage.read`** (`auth.PermissionUsageRead`), declared in NEW
   `internal/auth/permissions_usage.go` following the wave-2 per-track
   convention (`permissions_schedules.go` etc.: own file + `init()`
   append, `service.go` untouched). Grants: OWNER/ADMIN/MEMBER/VIEWER
   (read-only metering; matches how wave-2 granted read permissions).
   Flag to the orchestrator if the flat colon form is preferred instead.
2. **No changes to `internal/usage`**: the generic metering service there
   was not needed — the cost report aggregates `runs.cost_cents` directly
   in `internal/runs` (single source of truth, one tenant-scoped query).
3. **`INVALID_TIME_RANGE` beyond the contract**: the contract only pins
   `INVALID_GROUP_BY`. The handler also returns 400 `INVALID_TIME_RANGE`
   for unparsable/inverted windows and windows wider than 366 days (the
   service layer validates the same invariants).
4. **In-memory aggregation limitation**: `group_by=model` in
   zero-infrastructure mode yields empty model labels (no agents catalog
   in the runs service); Postgres resolves the model via a LEFT JOIN on
   `agents`. Documented in `internal/runs/cost.go`.
5. **Store owns the run-total bump**: `runs.cost_cents` is bumped inside
   the same atomic INSERT..SELECT/UPDATE CTE as the step row
   (`Store.InsertStep` contract, see `internal/runs/service.go`); the
   service only accumulates `TotalCostCents` itself in in-memory mode.
   Migration 005's `run_steps.cost` column is still written (same value)
   for backward compatibility.
6. **Day buckets are UTC** (`to_char(... AT TIME ZONE 'UTC')`) so reports
   are stable regardless of database session timezone.
