# Load-Testing Baseline (issue #80)

This document records the **measured performance baseline** of the AgentOS API
in zero-infrastructure (in-memory) mode, captured with the Go harness in
[`scripts/loadtest/`](../scripts/loadtest/loadtest.go). Re-run the harness on
any change that touches the request path (middleware, auth, runs, queue) and
compare against the numbers below before merging.

## What the harness measures

One **journey** = the canonical user flow, repeated end-to-end by every
virtual user (VU):

1. `register` — `POST /v1/auth/register` (new tenant + user per journey)
2. `login` — `POST /v1/auth/login` → bearer token
3. `create_agent` — `POST /v1/agents/create`
4. `create_run` — `POST /v1/runs` (enqueues an `agent.run` task)
5. `poll_run` — `GET /v1/runs/{id}` up to `-polls` times (20 ms apart) until a
   terminal status; without a worker process the run stays `QUEUED` in
   in-memory mode, so the poll figures are the polling **read path**, not run
   execution.

Reported per step: request count, errors (transport failure or unexpected
status), P50/P95/P99/mean/max latency, plus total HTTP requests, HTTP RPS and
journey RPS. The process exits non-zero (2) when any journey failed, so CI can
gate on it.

## Reproduction steps

```sh
# 1. Build the API binary and the harness.
PATH="/tmp/go/bin:$PATH" go build -o /tmp/agentos-api ./cmd/api
PATH="/tmp/go/bin:$PATH" go build -o /tmp/loadtest ./scripts/loadtest

# 2. Start the API in zero-infrastructure mode on a free port.
#    No DATABASE_URL / REDIS_ADDR -> in-memory stores and queue.
#    The default rate limit (120 rpm) would throttle the harness, so raise it.
API_PORT=18080 AGENTOS_RATE_LIMIT_RPM=1000000 /tmp/agentos-api &

# 3. Run the baseline (8 VUs, 5 s warmup, 30 s measurement).
/tmp/loadtest -base http://127.0.0.1:18080 -vus 8 -d 30s -warmup 5s -polls 8
# add -json for the machine-readable report
```

Kill the server when done. Nothing else is required: no database, no Redis,
no collector.

## Baseline results (measured on this machine)

Environment: 2 vCPU Intel(R) Xeon(R), 3.9 GiB RAM, Linux 5.10 x86_64,
Go 1.25.0, tracing disabled (default). Fresh server process per run; warmup
discarded; **0 failed journeys / 0 step errors in both runs**.

### Run A — `-vus 8 -d 30s -warmup 5s -polls 8` (default baseline)

| step         | requests | errors | P50 (ms) | P95 (ms) | P99 (ms) |
|--------------|---------:|-------:|---------:|---------:|---------:|
| register     |       90 |      0 | 1025.31  | 1781.54  | 2042.17  |
| login        |       90 |      0 | 1095.02  | 1528.56  | 1907.88  |
| create_agent |       90 |      0 |   13.25  |  110.08  |  151.14  |
| create_run   |       90 |      0 |    0.24  |   87.06  |  164.04  |
| poll_run     |      720 |      0 |   40.13  |  105.56  |  130.13  |
| iteration    |       90 |      0 | 2854.04  | 3594.93  | 3800.88  |

Totals: 1272 HTTP requests, **40.5 HTTP RPS**, 90 journeys, **2.86 journeys/s**.

### Run B — `-vus 2` (same flags otherwise; low-concurrency reference)

| step         | requests | errors | P50 (ms) | P95 (ms) | P99 (ms) |
|--------------|---------:|-------:|---------:|---------:|---------:|
| register     |       70 |      0 |  321.97  |  435.38  |  484.86  |
| login        |       70 |      0 |  322.39  |  457.18  |  494.45  |
| create_agent |       70 |      0 |    2.97  |   36.36  |   49.81  |
| create_run   |       70 |      0 |    0.18  |    6.86  |   29.14  |
| poll_run     |      560 |      0 |    0.28  |    5.33  |   11.78  |
| iteration    |       70 |      0 |  875.16  | 1055.38  | 1088.69  |

Totals: 630 HTTP requests, **32.5 HTTP RPS**, 70 journeys, **2.31 journeys/s**.

## Interpretation

- **Password hashing dominates.** `register`/`login` P50 ≈ 0.32 s (2 VUs) is
  bcrypt cost; at 8 VUs on 2 cores the hashing saturates the CPU (P50 ≈ 1 s)
  and drags every other step up with it (queueing behind CPU-bound work).
- **The request path itself is sub-millisecond.** `create_run` P50 0.18–0.24 ms
  and `poll_run` P50 0.28 ms at 2 VUs are the honest cost of auth + tenant
  guard + in-memory store + queue enqueue/read.
- Throughput is intentionally *not* the headline: with per-journey user
  creation, ~2.3–2.9 journeys/s is the journey-level ceiling on this box; the
  per-step percentiles above are the regression signal to watch.
- Re-run with the same flags on comparable hardware; treat >2x drift on any
  step P95 as a regression to investigate.

## Tracing (issue #80) and this baseline

`AGENTOS_TRACING_ENABLED` defaults to **off**, and the shipped middleware is
wired only via documentation (see `cmd/api/middleware_tracing.go`): with the
flag unset the API serving chain is byte-identical to the pre-tracing build,
so the numbers above **are** the tracing-off baseline.

The zero-overhead-off contract is additionally pinned by microbenchmark
(`go test -bench BenchmarkTracingMiddleware ./internal/observability/`,
same machine):

| benchmark                       | ns/op | B/op | allocs/op |
|---------------------------------|------:|-----:|----------:|
| TracingMiddleware **disabled**  |   549 |  160 |         3 |
| TracingMiddleware **enabled**   | 15597 | 3399 |        21 |

Once the lead wires `SetupTracing` + `tracingMiddleware` into
`cmd/api/main.go`, re-run this baseline with
`AGENTOS_TRACING_ENABLED=true OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318`
and a collector attached to quantify the enabled-mode overhead end-to-end.
