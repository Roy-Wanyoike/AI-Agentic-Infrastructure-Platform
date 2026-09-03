package main

import (
	"encoding/json"
	"net/http"

	"agentos/internal/observability"
	"agentos/internal/queue"
)

// ---------------------------------------------------------------------------
// /metrics v2 handler (Task 2-h, new file — cmd/api/handlers.go and main.go
// are orchestrator-owned).
//
// The handler keeps full backward compatibility with the JSON payload that
// cmd/api/handlers.go metricsHandler served and adds the Prometheus text
// exposition format as the new default:
//
//	GET /metrics              -> Prometheus text (Content-Type: text/plain; version=0.0.4)
//	GET /metrics?format=json  -> legacy JSON (counts/latency/queue_length)
//	                             plus additive "histograms" percentile summaries
//
// The orchestrator swaps the /metrics registration in cmd/api/main.go from
// metricsHandler to metricsV2Handler; see docs/wiring/2-h.md for the exact
// line. metricsHandler stays untouched for its existing tests.
// ---------------------------------------------------------------------------

// metricsV2Handler serves GET /metrics in Prometheus text format by default
// and in the legacy JSON shape when ?format=json is requested.
func metricsV2Handler(metrics *observability.Metrics, q *queue.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		counts, latency := metrics.Snapshot()

		if r.URL.Query().Get("format") == "json" {
			// Backward-compatible JSON payload: identical shape to the legacy
			// metricsHandler (counts, latency, queue_length) plus the additive
			// "histograms" section carrying bucketed p50/p95/p99 summaries.
			payload := map[string]any{
				"counts":       map[string]any{},
				"latency":      map[string]any{},
				"queue_length": 0,
				"histograms":   metrics.SnapshotSummaries(),
			}
			for k, v := range counts {
				payload["counts"].(map[string]any)[k] = v
			}
			for k, v := range latency {
				payload["latency"].(map[string]any)[k] = v
			}
			if q != nil {
				payload["queue_length"] = q.Length()
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(payload)
			return
		}

		queueLength := -1 // negative: omit the agentos_queue_length family
		if q != nil {
			queueLength = q.Length()
		}
		w.Header().Set("Content-Type", observability.PrometheusFormat)
		_, _ = w.Write([]byte(observability.RenderPrometheusText(counts, latency, metrics.SnapshotSummaries(), queueLength)))
	}
}
