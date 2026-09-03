package runs

import (
	"agentos/internal/observability"
)

// ---------------------------------------------------------------------------
// agentos_runs_total wiring (issue #12).
//
// UpdateStatusCtx is the single point of truth every run status transition
// flows through: the worker process (cmd/worker) calls it via UpdateStatus for
// worker-executed runs — including the fire-and-forget path where the run row
// lives in the API process — and the API process reaches it through the run
// control plane (Cancel/Pause/Resume) and the worker status-callback mirror in
// cmd/api/handlers.go. When a run reaches a terminal status (completed,
// failed, cancelled, timeout — IsTerminalStatus) the service increments
// agentos_runs_total at most once per run ID, so queue retries, replays and
// idempotent control transitions never double count.
//
// The dependency is NIL-SAFE: metrics is optional and every code path tolerates
// a nil registry, so existing tests and zero-infrastructure deployments keep
// working without any wiring.
//
// Process wiring:
//   - cmd/worker/main.go creates its own Metrics registry and calls SetMetrics
//     on its runs service (worker-side point of truth for worker-executed
//     runs).
//   - cmd/api/main.go (orchestrator-owned file) must call
//     `a.runsSvc.SetMetrics(a.metricsSvc)` next to the existing
//     `a.runsSvc.SetStreamer(a.streamSvc)` line so API-side terminal
//     transitions (cancellations and mirrored worker callbacks) surface on
//     GET /metrics.
// ---------------------------------------------------------------------------

// SetMetrics attaches the process-wide Metrics registry used to maintain the
// agentos_runs_total counter. Passing nil disables the counter. Nil-safe on
// the receiver and safe to call at any time (concurrency-safe).
func (s *Service) SetMetrics(m *observability.Metrics) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.metrics = m
}

// countTerminalRun bumps agentos_runs_total the first time a run is observed
// reaching a terminal status. Non-terminal transitions (queued/pending,
// running, paused, waiting_approval) are ignored and a nil registry is a
// no-op. Callers must not hold s.mu when calling.
func (s *Service) countTerminalRun(id string, status RunStatus) {
	if !IsTerminalStatus(status) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.terminalCounted == nil {
		s.terminalCounted = make(map[string]bool)
	}
	if s.terminalCounted[id] {
		return
	}
	s.terminalCounted[id] = true
	if s.metrics != nil {
		s.metrics.IncRuns()
	}
}
