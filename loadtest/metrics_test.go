package loadtest

import (
	"testing"
	"time"
)

// fixtureProm is a representative /v1/metrics body in the Prometheus text
// exposition format the API serves (observability.RenderPrometheusText):
// HELP/TYPE headers, labeled counter families, an unlabeled gauge and a
// histogram family with _bucket/_sum/_count suffixes.
const fixtureProm = `# HELP agentos_runs_total Total number of agent runs recorded.
# TYPE agentos_runs_total counter
agentos_runs_total 42
# HELP agentos_tools_total Total number of tool executions recorded.
# TYPE agentos_tools_total counter
agentos_tools_total 7
# TYPE agentos_queue_length gauge
agentos_queue_length 12
# TYPE agentos_http_requests_total counter
agentos_http_requests_total{route="/v1/runs",method="POST",status="201"} 30
agentos_http_requests_total{route="/v1/runs",method="POST",status="409"} 2
agentos_http_requests_total{route="/healthz",method="GET",status="200"} 10
# TYPE agentos_http_request_duration_seconds histogram
agentos_http_request_duration_seconds_bucket{route="/v1/runs",le="0.01"} 25
agentos_http_request_duration_seconds_bucket{route="/v1/runs",le="+Inf"} 32
agentos_http_request_duration_seconds_sum{route="/v1/runs"} 0.42
agentos_http_request_duration_seconds_count{route="/v1/runs"} 32
`

func TestParsePrometheusText(t *testing.T) {
	m := ParsePrometheusText(fixtureProm)

	// Unlabeled families parse verbatim.
	if got := m["agentos_runs_total"]; got != 42 {
		t.Errorf("agentos_runs_total = %v, want 42", got)
	}
	if got := m["agentos_tools_total"]; got != 7 {
		t.Errorf("agentos_tools_total = %v, want 7", got)
	}
	if got := m["agentos_queue_length"]; got != 12 {
		t.Errorf("agentos_queue_length = %v, want 12", got)
	}
	// Labeled families are summed (process-wide totals).
	if got := m["agentos_http_requests_total"]; got != 42 { // 30+2+10
		t.Errorf("agentos_http_requests_total = %v, want 42", got)
	}
	// Histogram families keep their suffixes.
	if got := m["agentos_http_request_duration_seconds_count"]; got != 32 {
		t.Errorf("..._count = %v, want 32", got)
	}
	if got := m["agentos_http_request_duration_seconds_sum"]; got != 0.42 {
		t.Errorf("..._sum = %v, want 0.42", got)
	}
}

func TestParsePrometheusTextLenient(t *testing.T) {
	m := ParsePrometheusText("" +
		"blank_line_above_is_fine\n" +
		"# a comment\n" +
		"valid_metric 3\n" +
		"no_value_field\n" +
		"bad_value notanumber\n" +
		"trailing_timestamp 5 1700000000000\n" +
		"  spaced_metric   2.5  \n" +
		"empty_labels{} 4\n")
	if m["valid_metric"] != 3 {
		t.Errorf("valid_metric = %v, want 3", m["valid_metric"])
	}
	if _, ok := m["no_value_field"]; ok {
		t.Error("line without a value must be skipped")
	}
	if _, ok := m["bad_value"]; ok {
		t.Error("unparsable value must be skipped")
	}
	if m["trailing_timestamp"] != 5 {
		t.Errorf("timestamp suffix must be ignored, got %v", m["trailing_timestamp"])
	}
	if m["spaced_metric"] != 2.5 {
		t.Errorf("spaced_metric = %v, want 2.5", m["spaced_metric"])
	}
	if m["empty_labels"] != 4 {
		t.Errorf("empty_labels = %v, want 4", m["empty_labels"])
	}
	if len(ParsePrometheusText("")) != 0 {
		t.Error("empty body must parse to an empty map")
	}
}

// TestSoakVerdictHeuristics pins the leak heuristics: a healthy run passes,
// runaway queue growth / climbing errors / climbing p95 are flagged.
func TestSoakVerdictHeuristics(t *testing.T) {
	cfg := DefaultConfig()

	// 20s of steady load at 20 req/s against a fresh collector; the helper
	// synthesizes the timestamp axis so the test is deterministic.
	base := nowTS()
	newPrimary := func() *collector {
		primary := newCollector()
		for i := 0; i < 400; i++ { // 400 samples @ 20/s = 20s
			ts := base + float64(i)/20.0
			primary.addAt(ts, 201, 201, 1.0)
		}
		return primary
	}

	t.Run("healthy passes", func(t *testing.T) {
		rr := &ResourceReport{Families: map[string]ResourceTrend{}}
		// Queue grows exactly at the enqueue rate: not a leak.
		rr.Families["agentos_queue_length"] = ResourceTrend{SlopePerSec: 20}
		verdict, reasons := soakVerdict(cfg, rr, newPrimary(), base, 20_000_000_000)
		if verdict != "pass" || len(reasons) != 0 {
			t.Fatalf("verdict = %q reasons=%v, want pass with no reasons", verdict, reasons)
		}
	})

	t.Run("runaway queue growth flagged", func(t *testing.T) {
		rr := &ResourceReport{Families: map[string]ResourceTrend{}}
		// Queue grows 3x the enqueue rate: the process retains work it was
		// never offered.
		rr.Families["agentos_queue_length"] = ResourceTrend{SlopePerSec: 60}
		verdict, reasons := soakVerdict(cfg, rr, newPrimary(), base, 20_000_000_000)
		if verdict != "suspected" || len(reasons) == 0 {
			t.Fatalf("verdict = %q reasons=%v, want suspected", verdict, reasons)
		}
	})

	t.Run("climbing error rate flagged", func(t *testing.T) {
		primary := newCollector()
		for i := 0; i < 400; i++ {
			ts := base + float64(i)/20.0
			if ts >= base+10 { // second half fails
				primary.addAt(ts, 500, 201, 1.0)
			}
		}
		rr := &ResourceReport{Families: map[string]ResourceTrend{}}
		verdict, reasons := soakVerdict(cfg, rr, primary, base, 20_000_000_000)
		if verdict != "suspected" || len(reasons) == 0 {
			t.Fatalf("verdict = %q reasons=%v, want suspected", verdict, reasons)
		}
	})

	t.Run("climbing p95 flagged", func(t *testing.T) {
		primary := newCollector()
		for i := 0; i < 400; i++ {
			ts := base + float64(i)/20.0
			lat := 1.0
			if ts >= base+10 { // second half is 100x slower
				lat = 100
			}
			primary.addAt(ts, 201, 201, lat)
		}
		rr := &ResourceReport{Families: map[string]ResourceTrend{}}
		verdict, reasons := soakVerdict(cfg, rr, primary, base, 20_000_000_000)
		if verdict != "suspected" || len(reasons) == 0 {
			t.Fatalf("verdict = %q reasons=%v, want suspected", verdict, reasons)
		}
	})
}

// TestResourceSamplerReport pins the trend-line summary over synthetic
// samples (start/end/delta/slope).
func TestResourceSamplerReport(t *testing.T) {
	s := NewResourceSampler(nil, "http://127.0.0.1:1/v1/metrics", "", 0)
	now := nowTS()
	s.mu.Lock()
	s.samples = []resourceSample{
		{at: time.UnixMilli(int64((now) * 1e3)), vals: map[string]float64{"gauge": 10, "counter": 100}, goros: 5},
		{at: time.UnixMilli(int64((now + 1) * 1e3)), vals: map[string]float64{"gauge": 20, "counter": 300}, goros: 7},
		{at: time.UnixMilli(int64((now + 2) * 1e3)), vals: map[string]float64{"gauge": 30, "counter": 500}, goros: 9},
	}
	s.mu.Unlock()

	rep := s.Report(1_000_000_000)
	if rep.Samples != 3 {
		t.Fatalf("samples = %d, want 3", rep.Samples)
	}
	if rep.ClientGoroutines != 9 {
		t.Fatalf("client goroutines = %d, want 9", rep.ClientGoroutines)
	}
	g := rep.Families["gauge"]
	if g.Start != 10 || g.End != 30 || g.Delta != 20 {
		t.Fatalf("gauge trend = %+v, want start 10 end 30 delta 20", g)
	}
	if g.SlopePerSec != 10 {
		t.Fatalf("gauge slope = %v, want 10", g.SlopePerSec)
	}
	c := rep.Families["counter"]
	if c.Start != 100 || c.End != 500 || c.Delta != 400 || c.SlopePerSec != 200 {
		t.Fatalf("counter trend = %+v, want slope 200", c)
	}

	// A family that appears only in later samples still trends.
	s.mu.Lock()
	s.samples = append(s.samples, resourceSample{
		at:   time.UnixMilli(int64((now + 3) * 1e3)),
		vals: map[string]float64{"late_family": 5}, goros: 9,
	})
	s.mu.Unlock()
	rep = s.Report(1_000_000_000)
	if tr, ok := rep.Families["late_family"]; !ok || tr.Start != 5 || tr.End != 5 {
		t.Fatalf("late_family trend = %+v ok=%v, want start=end=5", tr, ok)
	}
}
