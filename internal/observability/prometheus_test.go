package observability

import (
	"strings"
	"testing"
)

func renderForTest(t *testing.T, counts map[string]int64, latency map[string]float64, summaries map[string]HistogramSummary, queueLength int) string {
	t.Helper()
	return RenderPrometheusText(counts, latency, summaries, queueLength)
}

// TestRenderPrometheusTextExactOutput pins the full text exposition format for
// a representative snapshot: histogram family (bucket/_sum/_count), counter,
// queue gauge, labelled gauge and labelled counter, in sorted family order.
func TestRenderPrometheusTextExactOutput(t *testing.T) {
	h := NewHistogram(nil)
	h.Observe(0.075)
	summary := h.Summary()

	labels := `agentos_request_duration_seconds{route="/v1/agents",method="GET",status="200"}`
	summaries := map[string]HistogramSummary{labels: summary}
	counts := map[string]int64{
		"agentos_runs_total": 2,
		`http_requests_total{route="/v1/agents",method="GET",status="200"}`: 1,
	}
	latency := map[string]float64{
		`http_request_duration_seconds{route="/v1/agents",method="GET",status="200"}`: 0.075,
	}

	want := `# HELP agentos_queue_length Number of tasks currently waiting in the run queue.
# TYPE agentos_queue_length gauge
agentos_queue_length 3
# HELP agentos_request_duration_seconds Duration of HTTP requests in seconds.
# TYPE agentos_request_duration_seconds histogram
agentos_request_duration_seconds_bucket{route="/v1/agents",method="GET",status="200",le="0.005"} 0
agentos_request_duration_seconds_bucket{route="/v1/agents",method="GET",status="200",le="0.01"} 0
agentos_request_duration_seconds_bucket{route="/v1/agents",method="GET",status="200",le="0.025"} 0
agentos_request_duration_seconds_bucket{route="/v1/agents",method="GET",status="200",le="0.05"} 0
agentos_request_duration_seconds_bucket{route="/v1/agents",method="GET",status="200",le="0.1"} 1
agentos_request_duration_seconds_bucket{route="/v1/agents",method="GET",status="200",le="0.25"} 1
agentos_request_duration_seconds_bucket{route="/v1/agents",method="GET",status="200",le="0.5"} 1
agentos_request_duration_seconds_bucket{route="/v1/agents",method="GET",status="200",le="1"} 1
agentos_request_duration_seconds_bucket{route="/v1/agents",method="GET",status="200",le="2.5"} 1
agentos_request_duration_seconds_bucket{route="/v1/agents",method="GET",status="200",le="5"} 1
agentos_request_duration_seconds_bucket{route="/v1/agents",method="GET",status="200",le="10"} 1
agentos_request_duration_seconds_bucket{route="/v1/agents",method="GET",status="200",le="+Inf"} 1
agentos_request_duration_seconds_sum{route="/v1/agents",method="GET",status="200"} 0.075
agentos_request_duration_seconds_count{route="/v1/agents",method="GET",status="200"} 1
# HELP agentos_runs_total Total number of agent runs recorded.
# TYPE agentos_runs_total counter
agentos_runs_total 2
# HELP http_request_duration_seconds Most recent HTTP request duration in seconds (last-value gauge).
# TYPE http_request_duration_seconds gauge
http_request_duration_seconds{route="/v1/agents",method="GET",status="200"} 0.075
# HELP http_requests_total HTTP requests processed, by route, method and status.
# TYPE http_requests_total counter
http_requests_total{route="/v1/agents",method="GET",status="200"} 1
`
	got := renderForTest(t, counts, latency, summaries, 3)
	if got != want {
		t.Fatalf("exposition mismatch\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderPrometheusTextEscapingAndSanitizing(t *testing.T) {
	counts := map[string]int64{
		`weird{label="a\"b\\c"}`: 1, // round-trip: parse unescapes, render re-escapes
		"runs total":             2, // space -> underscore
	}
	got := renderForTest(t, counts, nil, nil, -1)
	if !strings.Contains(got, `weird{label="a\"b\\c"} 1`+"\n") {
		t.Errorf("escaped label value not rendered correctly:\n%s", got)
	}
	if !strings.Contains(got, "runs_total 2\n") {
		t.Errorf("metric name not sanitized:\n%s", got)
	}
	if !strings.Contains(got, "# HELP runs_total AgentOS metric runs_total\n") {
		t.Errorf("generic HELP line missing:\n%s", got)
	}
}

// TestRenderPrometheusTextQueueGaugeOmitted pins the negative queueLength
// contract (no queue wired) and that empty input renders empty output.
func TestRenderPrometheusTextQueueGaugeOmitted(t *testing.T) {
	got := renderForTest(t, nil, nil, nil, -1)
	if got != "" {
		t.Errorf("empty snapshot with queueLength=-1 should render nothing, got:\n%s", got)
	}
	got = renderForTest(t, nil, nil, nil, 0)
	want := "# HELP agentos_queue_length Number of tasks currently waiting in the run queue.\n# TYPE agentos_queue_length gauge\nagentos_queue_length 0\n"
	if got != want {
		t.Errorf("queue-only render mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// TestRenderPrometheusTextFromMetrics renders from the live Metrics methods to
// verify the integration path used by the /metrics handler.
func TestRenderPrometheusTextFromMetrics(t *testing.T) {
	m := NewMetrics()
	m.IncRuns()
	m.ObserveHTTP("/v1/runs", "POST", 200, 0.5)
	counts, latency := m.Snapshot()
	got := renderForTest(t, counts, latency, m.SnapshotSummaries(), 2)

	for _, want := range []string{
		"# TYPE agentos_runs_total counter\nagentos_runs_total 1\n",
		"# TYPE agentos_request_duration_seconds histogram\n",
		`agentos_request_duration_seconds_bucket{route="/v1/runs",method="POST",status="200",le="0.5"} 1` + "\n",
		`http_request_duration_seconds{route="/v1/runs",method="POST",status="200"} 0.5` + "\n",
		"agentos_queue_length 2\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in output:\n%s", want, got)
		}
	}
}

func TestParseMetricKey(t *testing.T) {
	name, labels := parseMetricKey("plain_metric")
	if name != "plain_metric" || labels != nil {
		t.Errorf("plain key parsed to %q, %v", name, labels)
	}
	name, labels = parseMetricKey(`http_requests_total{route="/v1/agents",method="GET",status="200"}`)
	if name != "http_requests_total" {
		t.Errorf("name = %q", name)
	}
	if len(labels) != 3 || labels[0].name != "route" || labels[0].value != "/v1/agents" || labels[2].value != "200" {
		t.Errorf("labels = %v", labels)
	}
	// Malformed label section degrades to name-only instead of panicking.
	name, _ = parseMetricKey("broken{no_equals")
	if name != "broken" {
		t.Errorf("malformed key name = %q, want broken", name)
	}
}
