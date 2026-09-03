package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentos/internal/observability"
	"agentos/internal/runs"
	"agentos/internal/streaming"
)

// postRunEvent posts a worker-style status event to runEventsHandler.
func postRunEvent(t *testing.T, streamer *streaming.Service, runID, status, output string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"type":"status","name":"status.changed","payload":{"status":"` + status + `","output":"` + output + `"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/runs/"+runID+"/events", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	runEventsHandler(streamer).ServeHTTP(rr, req)
	return rr
}

// TestRunEventsCallbackCountsTerminalRun asserts the API-side path of issue
// #12: worker status callbacks that carry a terminal outcome are mirrored
// onto the runs service, which (a) moves the durable run row to its final
// status and (b) feeds agentos_runs_total exactly once per run (replays are
// deduped by the counter hook). Non-terminal statuses stay no-ops.
func TestRunEventsCallbackCountsTerminalRun(t *testing.T) {
	saved := runsServiceVar
	defer func() { runsServiceVar = saved }()

	m := observability.NewMetrics()
	rs := runs.NewService()
	rs.SetMetrics(m)
	runsServiceVar = rs

	run, err := rs.Create("org-1", "agent-1", "run through the callback")
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	streamer := streaming.NewService()

	// Non-terminal worker progress event: relayed, not counted.
	if rr := postRunEvent(t, streamer, run.ID, "RUNNING", ""); rr.Code != http.StatusNoContent {
		t.Fatalf("running event: expected 204, got %d", rr.Code)
	}
	counts, _ := m.Snapshot()
	if counts[observability.MetricRunsTotal] != 0 {
		t.Fatalf("running event counted: got %d, want 0", counts[observability.MetricRunsTotal])
	}

	// Terminal worker callback: mirrored onto the run row and counted once.
	if rr := postRunEvent(t, streamer, run.ID, "COMPLETED", "done"); rr.Code != http.StatusNoContent {
		t.Fatalf("completed event: expected 204, got %d", rr.Code)
	}
	got, ok := rs.Get(run.ID)
	if !ok || got.Status != runs.StatusCompleted {
		t.Fatalf("run row not mirrored to COMPLETED: %+v ok=%v", got, ok)
	}
	if got.Output != "done" {
		t.Fatalf("run output not mirrored: %q", got.Output)
	}
	counts, _ = m.Snapshot()
	if counts[observability.MetricRunsTotal] != 1 {
		t.Fatalf("after terminal callback got %d runs, want 1", counts[observability.MetricRunsTotal])
	}

	// A replayed callback (queue/at-least-once delivery) must not double count.
	if rr := postRunEvent(t, streamer, run.ID, "COMPLETED", "done"); rr.Code != http.StatusNoContent {
		t.Fatalf("replayed event: expected 204, got %d", rr.Code)
	}
	counts, _ = m.Snapshot()
	if counts[observability.MetricRunsTotal] != 1 {
		t.Fatalf("replayed callback double counted: got %d, want 1", counts[observability.MetricRunsTotal])
	}

	// The relay still stored the events on the stream (existing contract).
	if history := streamer.History(run.ID); len(history) != 3 {
		t.Fatalf("expected 3 relayed events, got %d", len(history))
	}
}

// TestRunEventsCallbackNilRunsServiceSafe asserts the mirror is nil-safe: with
// no runs service wired the handler keeps its legacy relay-only behavior.
func TestRunEventsCallbackNilRunsServiceSafe(t *testing.T) {
	saved := runsServiceVar
	defer func() { runsServiceVar = saved }()
	runsServiceVar = nil

	streamer := streaming.NewService()
	if rr := postRunEvent(t, streamer, "run-any", "COMPLETED", "out"); rr.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rr.Code)
	}
	if history := streamer.History("run-any"); len(history) != 1 {
		t.Fatalf("expected the relayed event in history, got %d", len(history))
	}
}

// TestRunEventsCallbackUnknownRunNoPanic asserts a terminal callback for a run
// the API process does not know is safe (no panic, 204, event relayed). In
// zero-infrastructure mode the terminal outcome is still this process's point
// of truth, so it is counted exactly once (same semantics as the worker's
// fire-and-forget path); in store mode the store lookup fails and nothing is
// counted (see internal/runs/metrics_test.go).
func TestRunEventsCallbackUnknownRunNoPanic(t *testing.T) {
	saved := runsServiceVar
	defer func() { runsServiceVar = saved }()

	m := observability.NewMetrics()
	rs := runs.NewService()
	rs.SetMetrics(m)
	runsServiceVar = rs

	streamer := streaming.NewService()
	if rr := postRunEvent(t, streamer, "run-ghost", "FAILED", ""); rr.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rr.Code)
	}
	counts, _ := m.Snapshot()
	if counts[observability.MetricRunsTotal] != 1 {
		t.Fatalf("unknown-run terminal callback not counted: got %d, want 1", counts[observability.MetricRunsTotal])
	}

	// Replays stay deduped.
	if rr := postRunEvent(t, streamer, "run-ghost", "FAILED", ""); rr.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rr.Code)
	}
	counts, _ = m.Snapshot()
	if counts[observability.MetricRunsTotal] != 1 {
		t.Fatalf("replayed unknown-run callback double counted: got %d, want 1", counts[observability.MetricRunsTotal])
	}
}
