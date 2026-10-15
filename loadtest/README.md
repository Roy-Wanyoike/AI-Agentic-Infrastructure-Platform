# AgentOS Load-Testing Suite (issue #125)

`agentload` (this package + `cmd/agentload`) is a comprehensive, black-box load
testing suite for a running AgentOS API. Like the issue #80 baseline harness
(`scripts/loadtest/`, still the one-journey reference), it is **pure stdlib,
HTTP-only tooling** — it never imports `internal/` packages and adds no module
dependencies. It extends the #80 baseline with six load profiles, resource
trend sampling from `/metrics`, JSON baselines and a CI regression gate.

```
loadtest/
├── cmd/agentload/     CLI: run | record | compare | list
├── scenario*.go       the six scenarios (sustained, spike, soak,
│                      concurrency, workflow, lifecycle)
├── env.go             shared pooled HTTP client + tenant/agent/run helpers
├── scenario.go        Scenario interface, registry, pacer + phase runners
├── stats.go           percentile/mean/slope math, latency collectors
├── metrics.go         Prometheus-text parser + ResourceSampler (/metrics)
├── report.go          Report schema, JSON + human summaries
├── compare.go         baseline comparator (p95 +10% / errors +2pp gates)
├── baselines/         recorded baselines (sustained-inmemory.json)
├── ci/README.md       exact GitHub Actions workflow to add (see below)
└── smoke.sh           end-to-end smoke: build -> boot :8080 -> 6 scenarios
                       -> record baseline -> compare gate
```

## Scenarios

| name           | shape                                                                                                                                                                                                                             | key extras in the report |
|----------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|--------------------------|
| `sustained`    | steady RPS at `POST /v1/runs` against one ACTIVE agent (+ unversioned `GET /healthz` control op at ~RPS/10)                                                                                                                        | P50/P95/P99/mean/max per op, RPS |
| `spike`        | baseline period → burst at `RPS x -spike-multiplier` (default 10x) for `-spike-duration` → recovery back at RPS until the rolling-window p95 returns within `-recovery-tolerance` of the baseline p95 (or `-recovery-timeout`)     | baseline/burst p95, recovered, recovery_time_s |
| `soak`         | long steady run while sampling `/metrics` every `-sample-interval`                                                                                                                                                                | leak_verdict (pass/suspected + reasons) from queue-length slope vs enqueue rate and first/second-half error/p95 shifts |
| `concurrency`  | closed-loop workers ramped geometrically (`-ramp-start`, `-ramp-factor`) per `-ramp-step` until the error rate crosses `-error-threshold`                                                                                          | per-step table, max_sustainable, knee, breaking_point |
| `workflow`     | concurrent `POST /v1/workflows/{id}/execute` of a published fan-out DAG (entry → N branches → join, `-fan-out`)                                                                                                                    | fan_out, dag_nodes |
| `lifecycle`    | agent churn: create → draft→building→active transitions → create run → delete (a run-free disposable agent; the AGENT_HAS_RUNS guard blocks deleting run-bearing agents by design)                                                | iterations, per-op latencies |

Every scenario also reports **resource trends** (start/end/delta + least-squares
slope per second) for each `/metrics` family (`agentos_queue_length`,
`agentos_runs_total`, `agentos_http_requests_total`, histograms, ...) and a
harness-side goroutine count. The API does not expose `/debug/pprof` or a
server goroutine/heap gauge (verified for #125), so goroutine/heap/FD leak
detection uses the metric-family trends above and is noted as a follow-up in
soak reports.

## Run

The API must be running (zero-infrastructure mode is enough):

```sh
# 1. Build + boot the API in in-memory mode (no DATABASE_URL / REDIS_ADDR).
PATH="/tmp/go/bin:$PATH" go build -o /tmp/agentos-api ./cmd/api
API_PORT=8080 AGENTOS_RATE_LIMIT_RPM=1000000 /tmp/agentos-api &

# 2. Build the CLI.
PATH="/tmp/go/bin:$PATH" go build -o /tmp/agentload ./loadtest/cmd/agentload

# 3. Run a scenario.
/tmp/agentload run -scenario sustained -base http://127.0.0.1:8080 -d 60s
/tmp/agentload run -scenario spike -rps 50 -spike-multiplier 10 -json -out /tmp/spike.json
/tmp/agentload list
```

Or the one-command end-to-end smoke (builds, boots on :8080, runs short
versions of all six scenarios, records the baseline, gates it):

```sh
make loadtest-smoke          # = bash loadtest/smoke.sh
```

Exit codes: `0` pass, `1` baseline regression (compare only), `2` usage or
setup error. Interrupts (SIGINT/SIGTERM) shut a run down gracefully:
generation stops, in-flight requests finish, the partial report is still
printed and written.

## Record + compare (regression gate)

```sh
# Record a baseline (writes the JSON report to -o AND the human summary to stdout).
/tmp/agentload record -o loadtest/baselines/sustained-inmemory.json -scenario sustained -d 30s

# Later: gate a current run against it. Exit 1 on regression.
/tmp/agentload compare loadtest/baselines/sustained-inmemory.json current.json
```

Gates (issue #125 contract), overridable via `-p95-tol` / `-err-tol`:

* **p95**: any op's current p95 > baseline p95 **+10%** (relative, `x1.10`) → FAIL
* **error rate**: current > baseline **+2 percentage points** (absolute) → FAIL
* ops missing in the current report → FAIL; `setup.*` ops and zero-request ops are excluded

Baselines are plain `agentload` Reports, so `compare` also accepts any `-json`
run output as `current`.

## Tuning

All scenarios share one flag set (`loadtest.RegisterFlags`); every scenario
validates the subset it needs (`loadtest.Config.Validate`). Defaults in
parentheses:

* common: `-base` (:8080), `-prefix` (/v1), `-d` (60s), `-warmup` (5s, 0 disables),
  `-rps` (100), `-workers` (16), `-timeout` (5s), `-queue-buffer` (4096),
  `-password`
* spike: `-spike-multiplier` (10), `-spike-duration` (30s), `-baseline-duration` (10s),
  `-recovery-timeout` (60s), `-recovery-tolerance` (1.2), `-recovery-window` (3s)
* soak: `-sample-interval` (5s), `-leak-slope-x` (1.5 — queue may grow up to
  enqueue rate × 1.5 before flagging)
* concurrency: `-ramp-start` (1), `-ramp-factor` (2), `-ramp-step` (3s),
  `-max-workers` (256), `-error-threshold` (0.05), `-knee-factor` (2.0)
* workflow: `-fan-out` (3)

Rules of thumb:

* **rps vs workers**: open-loop scenarios (sustained/spike/soak/workflow) pace
  by `-rps` and use `-workers` only as the generator pool; when the queue
  buffer fills, the excess is counted as `client_drops` (generator saturation)
  instead of silently distorting latency. Closed-loop scenarios (concurrency,
  lifecycle) ignore `-rps` and scale by worker count.
* **rate limiter**: the API defaults to 120 rpm (`AGENTOS_RATE_LIMIT_RPM`);
  anything above ~2 rps needs it raised on the server or every request 429s.
* **CI-sized runs**: `-d 10s -warmup 2s` keeps a full six-scenario pass under
  ~2 minutes (that is what `smoke.sh` does).

## Interpreting reports

Human summary: one latency table per op (requests, errors, error rate,
P50/P95/P99/mean/max, RPS), setup-op table (excluded from gates), totals, the
resource-trend table and scenario extras (spike recovery, soak verdict,
ramp steps, ...). `-json` emits the identical `Report` document.

* `client_drops > 0` — the generator could not keep up with its own pacing;
  lower `-rps` or raise `-workers` before trusting the latencies.
* soak `leak_verdict: suspected` — read `leak_reasons`: queue growing faster
  than the enqueue rate, or error rate / p95 climbing between run halves.
* concurrency — `max_sustainable` is the last worker count below the error
  threshold; `knee` is the first count whose p95 exceeded `-knee-factor` × the
  first step's p95. Re-run with `-ramp-start` near the knee for a fine-grained
  reading.
* In in-memory mode (no worker process) runs stay QUEUED: `create_run`
  measures the request path, not execution — same semantics as the #80
  baseline harness.
* **Labeled `/metrics` families are noisy in the resource table — by design of
  the server, not the harness.** The API's Prometheus exposition renders only
  ONE (randomly selected) label set per labeled family per scrape
  (`internal/observability/prometheus.go` keeps the first key seen per family
  in Go map iteration order), so the summed trend of
  `agentos_request_duration_seconds_*` / `http_requests_total` can move in
  either direction between samples. Treat those rows as informational; the
  authoritative resource signals are the **unlabeled** families —
  `agentos_queue_length` (the soak leak-detection input) and
  `agentos_runs_total` — plus the harness-measured latencies, which are
  sampled client-side and unaffected.

## CI

`ci/README.md` contains the exact GitHub Actions workflow YAML to add
(`.github/` is intentionally not touched by this branch). Make targets:
`make loadtest-sustained|loadtest-spike|loadtest-soak-short|loadtest-compare`
(plus `loadtest-workflow`, `loadtest-lifecycle`, `loadtest-concurrency`,
`loadtest-build`, `loadtest-smoke`).
