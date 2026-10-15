# Synthetic Assurance — Flagship Closure Evidence (#118)

Issue #118 is the flagship epic: **safe autonomous testing of production
software** — a tenant registers a scope, verifies ownership of the target,
runs against a read-only-by-default production posture, captures evidence,
files findings, loops them through GitHub, and gates releases on the
results. Waves 9–10 delivered the subsystem across issues **#147–#156**
(plus the release-gate bridge to earlier canary/eval work). This document is
the closure evidence: each promise mapped to the package, route, migration,
schema fragment, and tests that prove it — with honest status labels for the
seams that are wired but not yet orchestrated.

Subsystems outside this file that the gates consume: adversarial
evaluations (#153, `internal/evaluations/adversarial*.go`) and canary
deployments (#13/#51, `internal/deployments/canary.go` + `policy.go`).

## Promise → delivered component

| # | Promise | Delivered | Proof | Status |
|---|---|---|---|---|
| 1 | **Scope registration** — tenant-scoped registry for projects, targets, environments, personas | `internal/synth` registry service; 21 routes under `/synth/*` (`cmd/api/synth.go`); tables `synth_projects`, `synth_targets`, `synth_environments`, `synth_personas` | migration **034**; `internal/synth/store.go` (org-guarded SQL); `cmd/api/synth_test.go`; fragment `api/fragments/synth-registry.yaml` | **Implemented** (live HTTP + Postgres) |
| 2 | **Target ownership verification** — targets execute only when VERIFIED | challenge/confirm flow: `POST /synth/targets/{id}/verify`; `AssertExecutionAllowed` denies ALL actions on unverified targets | `internal/synth/service.go` (`ErrTargetUnverified`, scope-guard seam); handler `synthTargetVerifyHandler` | **Implemented** |
| 3 | **Read-only production default** | production-kind environments default to `production_read_only` and can never be downgraded to `pre_production`; enabling production tests requires OWNER/ADMIN (`synthProductionGate` in `cmd/api/synth.go`); write journeys run ONLY in release-comparison mode with an isolated synthetic account | `internal/synth/service.go` invariants; `internal/synth/schedule.go` (issue #156, `ErrReadOnlyMode` server-side re-check) | **Implemented** |
| 4 | **Isolated test workers** (browser + API) with bounded execution | `internal/synth/worker.go` + `worker_api.go`: `RunStateMachine` (pending→queued→running→terminal, illegal transitions fail loud), per-run step budget (default 200), per-step timeout, wall-clock cap, in-flight cap, rate cap, kill switch, context-cancel → `inconclusive` (never silently lost); BrowserWorker via pluggable `BrowserDriver` seam (real `npx playwright` adapter in `playwright_cli.go`); APIWorker with pooled client, SSRF guard against non-allowlisted hosts, redacted request/response metadata | `worker_test.go`, `worker_api_test.go` (contract fakes); SSRF + redaction assertions in-package | **Implemented (engine)** — orchestration wiring into `cmd/worker` and HTTP surface **pending** (see below) |
| 5 | **Versioned journeys + explicit assertions** — model confidence can never pass a run | `internal/synth/journeys.go` (issue #149): versioned specs, enforced draft→reviewed→scheduled→retired lifecycle, mandatory assertions evaluated **worker-side** against observations (deterministic; skipped ≠ pass), capability coverage mapping, write-op flags + cleanup steps, `VersionedRunInputs` for reproducibility; 9 built-in personas (`personas.go`, keys pinned in `service.go`) | `journeys_test.go`, `personas_test.go`; schema fragment `api/fragments/synth-journeys.yaml` (schema-only by design) | **Implemented (engine)** — HTTP surface pending |
| 6 | **Evidence bundles + finding lifecycle** | `internal/synth/evidence.go` + `evidence_store.go` (issue #150): versioned bundles (`schema_version`), redaction engine (cookies/bearer/JWT/API keys stripped before bundling; callers redact metadata, store treats payloads as opaque); `findings.go`: confirmed/likely/flaky/harness/environmental/inconclusive taxonomy, SHA-256 fingerprint over org+target+assertion-kind+failure-class (org INSIDE the hash — dedup never merges tenants), append-only occurrences, independent reproduction (never replaying production writes) | `evidence_test.go`, `findings_test.go`, `fixtures_test.go` (synthetic-only fixture guards); fragment `synth-evidence.yaml` | **Implemented (engine)** |
| 7 | **Durable findings storage** | `PostgresFindingStore` (`internal/synth/store_findings.go`): row-per-occurrence over `synth_findings` (migration 034, `ux_synth_findings_fp` unique index); canonical finding = latest row; every query org-scoped | `store_findings_test.go` | **Implemented — not yet wired** into `cmd/api/main.go` (in-memory default at `findings.go:577`); wiring is a one-line follow-up |
| 8 | **GitHub findings loop** | `internal/synth/github.go` + `github_sync.go` (issue #151): fingerprint as idempotency key (first sync creates, re-syncs update/no-op), allow-listed repos, least-privilege client, secret scrubbing, webhook linkback; `regression.go`: confirmed findings generate draft regression journeys (born `JourneyDraft`, require approval) + fix verification | `github_test.go`, `github_sync_test.go` | **Implemented (engine)** — REST surface (`POST /synth/findings/{id}/github-sync`, webhook) explicitly deferred to an orchestrator decision (`synth-github.yaml` header) |
| 9 | **Unified release quality gates** | `internal/qualitygates` (issue #152): versioned policies over evaluations + synth journeys + adversarial security suites + canary health (`canary_bridge.go`); pure deterministic `Decide` → promote/hold/abort/rollback (`decide.go`); tamper-evident evidence records (`evidence.go`); time-bounded audited exceptions with forced-actor approver; security suites fail-closed; 7 live routes under `/quality-gates/*` (`cmd/api/qualitygates.go`) | `decide_test.go`, `policy_test.go`, `evidence_test.go`, `canary_bridge_test.go`, `cmd/api/qualitygates_test.go`; fragment `quality-gates.yaml` | **Implemented** (live HTTP; stores in-memory — Postgres policy store is the pre-assigned follow-up, see architecture.md) |
| 10 | **Safety-bounded autonomous exploration** | `internal/synth/explore.go` (issue #155): `ExplorePlanner` walks a REGISTERED app (verified target + pinned environment) via deterministic scripted explorer + optional pluggable AI explorer; **every** proposed action re-checked server-side by `ScopeEnforcer.CheckScope`; candidates are drafts requiring approval; untrusted page content is data-only (`SanitizeUntrusted`) — it can propose, never authorize | `explore_test.go`; fragment `synth-explore.yaml` | **Implemented (engine)** |
| 11 | **Continuous production journeys + release-version comparison** | `internal/synth/schedule.go` + `compare.go` (issue #156): `ProductionScheduler` turns reviewed journeys into recurring/deployment-triggered runs (verified production targets only, quiet windows, timezones, kill switches); `ReleaseComparator` answers "did the candidate build change the experience?" by comparing runs of the SAME `VersionedRunInputs` — any journey/persona/fixture/app-version drift lands in `VersionDrift` and forces `Meaningful=false` | `schedule_test.go`, `compare_test.go` | **Implemented (engine)** |

## Adversarial evals + canary (gate inputs)

- Adversarial evaluations (#153): prompt-injection, permission-bypass,
  cross-tenant, and secret-exposure suites that assert **enforcement at
  control boundaries** (a probe passes only when the system denies), never
  model refusal — `internal/evaluations/adversarial.go`,
  `adversarial_store.go`, wired into the API with dedicated permissions.
- Canary health (#13 + #51 policy): weighted traffic split with
  deterministic stickiness, eval-gated auto-promotion/rollback
  (`internal/deployments/canary.go`, `policy.go`) — the quality-gate
  `canary_bridge.go` consumes the same health signals.

## Verification record

- `internal/synth` and `internal/qualitygates` are part of the 50-package
  green suite (16 test files in `internal/synth` alone: every subsystem
  file above carries a `_test.go`).
- Wire shapes are pinned twice: `internal/synth/types.go` (cross-track Go
  contract, unmodifiable by sibling tracks) and schema-only OpenAPI
  fragments (`synth-registry.yaml` merged into the spec — the
  `/synth/*` paths exist at `api/openapi.yaml:9403-9929`;
  `synth-journeys/workers/evidence/github/explore` publish shared
  component schemas with no paths, so tracks cannot drift).
- Registry persistence proven by migration 034 + `store_test.go`;
  findings persistence proven by `store_findings_test.go`.

## Honest remaining work (kept visible, not hidden)

1. **Worker orchestration**: `BrowserWorker`/`APIWorker` are not yet driven
   by `cmd/worker` (the orchestrator wiring is called out in
   `worker.go`'s ownership header). Until wired, journeys run through the
   engine and tests, not the live loop.
2. **HTTP surfaces for journeys/runs/findings/GitHub sync/explore**:
   fragments are schema-only by orchestrator direction; route ownership
   (registry track vs. new tracks) is a recorded follow-up decision.
3. **Store wiring**: `PostgresFindingStore` → `cmd/api`; Postgres stores
   for journeys, runs, and evidence (registry + findings tables already
   exist); object-storage backend behind the `EvidenceStore` seam.
4. **Quality-gate Postgres policy/evidence stores** (in-memory in both API
   modes today; migration to be co-assigned with the store work).
5. **Portal/surface work**: no dashboard views for synth runs yet.

None of the above narrows the safety contract: registration, verification,
the read-only production default, evidence redaction, and the
deterministic gate decision are enforced in shipped code paths — the
pending items are orchestration and persistence reach, not semantics.
