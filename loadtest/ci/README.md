# CI integration for the load suite (issue #125)

`.github/` is out of scope for the loadtest branch, so the workflow is provided
here verbatim: copy it to `.github/workflows/loadtest.yml` (one file, no other
changes needed). It is intentionally conservative:

* runs on every PR that touches request-path code, plus `workflow_dispatch`;
* boots the API in zero-infrastructure (in-memory) mode — no services needed;
* gates a short sustained run against the committed baseline
  (`loadtest/baselines/sustained-inmemory.json`, recorded by
  `loadtest/smoke.sh`) with the default gates: **p95 +10% or error rate +2pp →
  exit 1 → red PR**;
* uploads the JSON report + comparison as artifacts for triage.

The comparator is intentionally tolerant of runner noise in p95 (that is what
`-p95-tol` is for); keep the default 1.10 for the gate and only loosen it when
a machine-class change (e.g. new runner fleet) justifies re-recording the
baseline via `agentload record -o loadtest/baselines/sustained-inmemory.json`.

## Exact workflow to add

```yaml
# .github/workflows/loadtest.yml — copy from loadtest/ci/README.md (issue #125)
name: loadtest

on:
  workflow_dispatch:
  pull_request:
    paths:
      - "cmd/**"
      - "internal/**"
      - "loadtest/**"
      - "scripts/loadtest/**"

permissions:
  contents: read

jobs:
  load-gate:
    name: load suite (sustained gate)
    runs-on: ubuntu-latest
    timeout-minutes: 15
    steps:
      - uses: actions/checkout@v4

      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true

      - name: Build API + agentload
        run: |
          go build -o /tmp/agentos-api ./cmd/api
          go build -o /tmp/agentload ./loadtest/cmd/agentload

      - name: Start API (in-memory mode, rate limit off)
        run: |
          API_PORT=8080 AGENTOS_RATE_LIMIT_RPM=1000000 /tmp/agentos-api > /tmp/api.log 2>&1 &
          for i in $(seq 1 60); do
            curl -fsS http://127.0.0.1:8080/readyz && break
            sleep 0.5
          done
          curl -fsS http://127.0.0.1:8080/readyz

      - name: Unit tests (percentile math + comparator)
        run: go test ./loadtest/...

      - name: Run all six scenarios (short)
        run: |
          set -e
          for s in sustained spike soak concurrency workflow lifecycle; do
            case "$s" in
              sustained)   extra="-d 8s -warmup 2s -rps 60" ;;
              spike)       extra="-baseline-duration 4s -spike-duration 4s -recovery-timeout 12s -warmup 0s -rps 60" ;;
              soak)        extra="-d 12s -warmup 0s -rps 60 -sample-interval 3s" ;;
              concurrency) extra="-ramp-step 1s -max-workers 16 -warmup 0s -rps 60" ;;
              workflow)    extra="-d 6s -warmup 0s -rps 60" ;;
              lifecycle)   extra="-d 6s -warmup 0s -workers 4" ;;
            esac
            # -out writes the JSON report; the human summary goes to stdout.
            /tmp/agentload run -scenario "$s" -base http://127.0.0.1:8080 \
              -out /tmp/report-$s.json $extra | tail -n 25
          done

      - name: Gate against recorded baseline (p95 +10% / errors +2pp -> fail)
        run: |
          set -e
          /tmp/agentload run -scenario sustained -base http://127.0.0.1:8080 -json \
            -d 20s -warmup 3s -rps 60 > /tmp/current.json
          /tmp/agentload compare loadtest/baselines/sustained-inmemory.json /tmp/current.json \
            | tee /tmp/compare.txt

      - name: Upload artifacts
        if: always()
        uses: actions/upload-artifact@v4
        with:
          name: loadtest-reports
          path: |
            /tmp/report-*.json
            /tmp/current.json
            /tmp/compare.txt
            /tmp/api.log
          if-no-files-found: ignore
```

## Maintenance notes

* **Re-recording the baseline**: only on intentional performance changes or a
  new runner class, in the same environment the gate will run in:
  `bash loadtest/smoke.sh` (records + verifies) or
  `agentload record -o loadtest/baselines/sustained-inmemory.json -d 20s -rps 60`.
  Commit the JSON with a note in the PR.
* **p95 flake triage**: a red p95 gate on an untouched path usually means a
  noisy neighbor on the runner — re-run once via `workflow_dispatch` before
  investigating; two consecutive failures are real.
* The soak scenario's short variant (12s) is a smoke of the plumbing, not a
  leak hunt; a nightly `workflow_dispatch` job with `-d 30m -rps 100` is the
  recommended follow-up.
