#!/usr/bin/env bash
# loadtest/smoke.sh — end-to-end smoke of the AgentOS load suite (issue #125).
#
# Does exactly this, with no external dependencies beyond Go and curl:
#
#   1. build ./cmd/api (zero-infrastructure in-memory mode) and ./loadtest/cmd/agentload
#   2. start the API on :${SMOKE_PORT:-8080} with the rate limit disabled
#   3. wait for GET /readyz
#   4. run SHORT versions of all six scenarios (sustained, spike, soak,
#      concurrency, workflow, lifecycle)
#   5. record a REAL sustained baseline (default 20s) into
#      loadtest/baselines/sustained-inmemory.json
#   6. gate a fresh short sustained run against the baseline
#      (`agentload compare`, p95 +10% / error +2pp -> exit 1)
#   7. kill the API
#
# Exit codes: 0 smoke passed, non-zero on any setup/scenario/gate failure.
# Artifacts (JSON reports, API log) land in /tmp/agentload-smoke/.
#
# Usage:
#   bash loadtest/smoke.sh
#   SMOKE_PORT=18080 SMOKE_BASELINE_D=30s bash loadtest/smoke.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

SMOKE_PORT="${SMOKE_PORT:-8080}"
BASE="http://127.0.0.1:${SMOKE_PORT}"
ART="/tmp/agentload-smoke"
GO="${GO:-go}"
command -v "$GO" >/dev/null || { PATH="/tmp/go/bin:$PATH"; GO=go; }

API_BIN="${ART}/agentos-api"
AGENTLOAD_BIN="${ART}/agentload"
API_LOG="${ART}/api.log"
BASELINE="${ROOT}/loadtest/baselines/sustained-inmemory.json"

# Short scenario knobs (full-size defaults live in loadtest.DefaultConfig and
# loadtest/README.md "Tuning").
SMOKE_WARMUP="${SMOKE_WARMUP:-2s}"
SMOKE_SUSTAINED_D="${SMOKE_SUSTAINED_D:-8s}"
SMOKE_BASELINE_D="${SMOKE_BASELINE_D:-20s}"   # the REAL recorded baseline window (contract: 20-30s)
SMOKE_BASELINE_WARMUP="${SMOKE_BASELINE_WARMUP:-3s}"
SMOKE_SPIKE_BASELINE="${SMOKE_SPIKE_BASELINE:-4s}"
SMOKE_SPIKE_DURATION="${SMOKE_SPIKE_DURATION:-4s}"
SMOKE_SOAK_D="${SMOKE_SOAK_D:-12s}"
SMOKE_RAMP_STEP="${SMOKE_RAMP_STEP:-1s}"
SMOKE_MAX_WORKERS="${SMOKE_MAX_WORKERS:-16}"
SMOKE_WORKFLOW_D="${SMOKE_WORKFLOW_D:-6s}"
SMOKE_LIFECYCLE_D="${SMOKE_LIFECYCLE_D:-6s}"
SMOKE_RPS="${SMOKE_RPS:-60}"

mkdir -p "$ART" "$(dirname "$BASELINE")"

log() { printf '\n=== %s ===\n' "$*"; }

API_PID=""
cleanup() {
  if [ -n "$API_PID" ] && kill -0 "$API_PID" 2>/dev/null; then
    kill "$API_PID" 2>/dev/null || true
    wait "$API_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

log "1/6 build cmd/api + loadtest/cmd/agentload"
"$GO" build -o "$API_BIN" ./cmd/api
"$GO" build -o "$AGENTLOAD_BIN" ./loadtest/cmd/agentload
"$AGENTLOAD_BIN" list

log "2/6 start API on :${SMOKE_PORT} (in-memory mode, rate limit off)"
# No DATABASE_URL/REDIS_ADDR -> in-memory stores + memory queue. The default
# 120 rpm limiter would 429 the generator, so it is disabled for the smoke.
API_PORT="$SMOKE_PORT" AGENTOS_RATE_LIMIT_RPM=1000000 "$API_BIN" >"$API_LOG" 2>&1 &
API_PID=$!

log "3/6 wait for /readyz"
ready=0
for _ in $(seq 1 60); do
  if curl -fsS "$BASE/readyz" >/dev/null 2>&1; then ready=1; break; fi
  if ! kill -0 "$API_PID" 2>/dev/null; then
    echo "API process died during startup; log tail:" >&2
    tail -20 "$API_LOG" >&2
    exit 1
  fi
  sleep 0.5
done
[ "$ready" = "1" ] || { echo "API never became ready" >&2; tail -20 "$API_LOG" >&2; exit 1; }
echo "ready: $(curl -fsS "$BASE/readyz")"

run_scenario() { # name, json-artifact, extra flags...
  local name="$1" json="$2"; shift 2
  echo "--- scenario: $name $*"
  local rc=0
  # -out writes the JSON report; the human summary goes to stdout.
  "$AGENTLOAD_BIN" run -scenario "$name" -base "$BASE" -out "$json" "$@" || rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "smoke: scenario $name failed (exit $rc)" >&2
    exit 1
  fi
}

log "4/6 short runs of all six scenarios (rps=${SMOKE_RPS})"
run_scenario sustained     "$ART/sustained.json"  -d "$SMOKE_SUSTAINED_D"   -warmup "$SMOKE_WARMUP" -rps "$SMOKE_RPS"
run_scenario spike         "$ART/spike.json"      -baseline-duration "$SMOKE_SPIKE_BASELINE" -spike-duration "$SMOKE_SPIKE_DURATION" -recovery-timeout 12s -warmup 0s -rps "$SMOKE_RPS"
run_scenario soak          "$ART/soak.json"       -d "$SMOKE_SOAK_D"        -warmup 0s -rps "$SMOKE_RPS" -sample-interval 3s
run_scenario concurrency   "$ART/concurrency.json" -ramp-step "$SMOKE_RAMP_STEP" -max-workers "$SMOKE_MAX_WORKERS" -warmup 0s -rps "$SMOKE_RPS"
run_scenario workflow      "$ART/workflow.json"   -d "$SMOKE_WORKFLOW_D"    -warmup 0s -rps "$SMOKE_RPS"
run_scenario lifecycle     "$ART/lifecycle.json"  -d "$SMOKE_LIFECYCLE_D"   -warmup 0s -workers 4

log "5/6 record REAL sustained baseline (${SMOKE_BASELINE_D} -> loadtest/baselines/sustained-inmemory.json)"
"$AGENTLOAD_BIN" record -o "$BASELINE" -scenario sustained -base "$BASE" \
  -d "$SMOKE_BASELINE_D" -warmup "$SMOKE_BASELINE_WARMUP" -rps "$SMOKE_RPS" \
  >"$ART/baseline-run.txt"
tail -8 "$ART/baseline-run.txt"
[ -s "$BASELINE" ] || { echo "smoke: baseline not written" >&2; exit 1; }

log "6/6 gate a fresh sustained run against the recorded baseline (agentload compare)"
"$AGENTLOAD_BIN" run -scenario sustained -base "$BASE" -json \
  -d "$SMOKE_SUSTAINED_D" -warmup "$SMOKE_WARMUP" -rps "$SMOKE_RPS" \
  >"$ART/current.json"
set +e
"$AGENTLOAD_BIN" compare "$BASELINE" "$ART/current.json" | tee "$ART/compare.txt"
gate=${PIPESTATUS[0]}
set -e
if [ "$gate" -ne 0 ] && [ "$gate" -ne 1 ]; then
  echo "smoke: compare exited $gate (usage/setup error)" >&2
  exit 1
fi
if [ "$gate" -eq 1 ]; then
  echo "smoke: FAIL — current run regressed against the fresh baseline (see $ART/compare.txt)" >&2
  exit 1
fi

log "smoke PASSED (baseline: $BASELINE)"
