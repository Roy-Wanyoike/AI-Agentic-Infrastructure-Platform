package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentos/internal/observability"
	"agentos/internal/queue"
)

func TestMetricsV2HandlerPrometheusDefault(t *testing.T) {
	m := observability.NewMetrics()
	m.IncRuns()
	m.ObserveHTTP("/v1/agents", "GET", 200, 0.05)
	q := queue.NewQueue()

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	metricsV2Handler(m, q).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	if ct := rr.Header().Get("Content-Type"); ct != observability.PrometheusFormat {
		t.Fatalf("Content-Type = %q, want %q", ct, observability.PrometheusFormat)
	}
	body := rr.Body.String()
	for _, want := range []string{
		"# HELP agentos_runs_total Total number of agent runs recorded.",
		"# TYPE agentos_runs_total counter",
		"agentos_runs_total 1",
		"# TYPE agentos_request_duration_seconds histogram",
		`agentos_request_duration_seconds_bucket{route="/v1/agents",method="GET",status="200",le="+Inf"} 1`,
		"# TYPE agentos_queue_length gauge",
		"agentos_queue_length 0",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
}

// TestMetricsV2HandlerJSONBackwardCompat pins the legacy JSON shape served at
// /metrics?format=json (counts, latency, queue_length) plus the additive
// "histograms" section.
func TestMetricsV2HandlerJSONBackwardCompat(t *testing.T) {
	m := observability.NewMetrics()
	m.Inc("runs_total")
	m.Observe("http_request_duration_seconds", 1.5)
	m.ObserveHistogram("agentos_request_duration_seconds", 0.3)

	req := httptest.NewRequest(http.MethodGet, "/metrics?format=json", nil)
	rr := httptest.NewRecorder()
	metricsV2Handler(m, nil).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}

	var payload struct {
		Counts      map[string]float64                        `json:"counts"`
		Latency     map[string]float64                        `json:"latency"`
		QueueLength float64                                   `json:"queue_length"`
		Histograms  map[string]observability.HistogramSummary `json:"histograms"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode json: %v; body=%s", err, rr.Body.String())
	}
	if payload.Counts["runs_total"] != 1 {
		t.Errorf("counts.runs_total = %v, want 1", payload.Counts["runs_total"])
	}
	if payload.Latency["http_request_duration_seconds"] != 1.5 {
		t.Errorf("latency.http_request_duration_seconds = %v, want 1.5", payload.Latency["http_request_duration_seconds"])
	}
	if payload.QueueLength != 0 {
		t.Errorf("queue_length = %v, want 0 (nil queue)", payload.QueueLength)
	}
	h, ok := payload.Histograms["agentos_request_duration_seconds"]
	if !ok || h.Count != 1 {
		t.Errorf("histograms section missing/incomplete: %+v", payload.Histograms)
	}
}

func TestMetricsV2HandlerMethodNotAllowed(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/metrics", nil)
	rr := httptest.NewRecorder()
	metricsV2Handler(observability.NewMetrics(), nil).ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusMethodNotAllowed)
	}
}
