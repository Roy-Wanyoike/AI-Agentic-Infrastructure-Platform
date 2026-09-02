package observability

import (
	"math"
	"sync"
	"testing"
)

// TestHistogramQuantilesExactBounds observes every integer 1..10 once with
// unit-width buckets [1..10]; quantiles land exactly on bucket edges.
func TestHistogramQuantilesExactBounds(t *testing.T) {
	bounds := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	h := NewHistogram(bounds)
	for v := 1.0; v <= 10; v++ {
		h.Observe(v)
	}
	if got := h.Count(); got != 10 {
		t.Fatalf("count = %d, want 10", got)
	}
	if got := h.Sum(); got != 55 {
		t.Fatalf("sum = %v, want 55", got)
	}
	cases := []struct {
		q     float64
		want  float64
		label string
	}{
		{0.50, 5.0, "p50"},
		{0.95, 9.5, "p95"},
		{0.99, 9.9, "p99"},
		{0.00, 0.0, "p0"},
		{1.00, 10.0, "p100"},
	}
	for _, tc := range cases {
		if got := h.Quantile(tc.q); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("%s (q=%v) = %v, want %v", tc.label, tc.q, got, tc.want)
		}
	}
}

// TestHistogramQuantilesDefaultBuckets mixes in-bucket and beyond-range values.
func TestHistogramQuantilesDefaultBuckets(t *testing.T) {
	h := NewHistogram(nil)
	for _, v := range []float64{0.001, 0.3, 2.0, 30} {
		h.Observe(v)
	}
	cumulative := h.CumulativeBuckets()
	bounds := h.BucketBounds()
	if len(cumulative) != len(DefaultBuckets) || len(bounds) != len(DefaultBuckets) {
		t.Fatalf("bucket count = %d/%d, want %d", len(cumulative), len(bounds), len(DefaultBuckets))
	}
	wantCum := []uint64{1, 1, 1, 1, 1, 1, 2, 2, 3, 3, 3}
	for i, want := range wantCum {
		if cumulative[i] != want {
			t.Fatalf("cumulative[%d] (le=%v) = %d, want %d", i, bounds[i], cumulative[i], want)
		}
	}
	if got := h.Quantile(0.50); math.Abs(got-0.5) > 1e-9 {
		t.Errorf("p50 = %v, want 0.5", got)
	}
	// Rank 3.8 falls into the implicit +Inf bucket: clamped to last bound.
	if got := h.Quantile(0.95); got != 10 {
		t.Errorf("p95 = %v, want 10 (clamp to last finite bound)", got)
	}
	if got := h.Quantile(0.99); got != 10 {
		t.Errorf("p99 = %v, want 10 (clamp to last finite bound)", got)
	}
	summary := h.Summary()
	if summary.Min != 0.001 || summary.Max != 30 {
		t.Errorf("min/max = %v/%v, want 0.001/30", summary.Min, summary.Max)
	}
	if summary.Count != 4 {
		t.Errorf("summary count = %d, want 4", summary.Count)
	}
	if summary.Buckets["+Inf"] != 4 {
		t.Errorf("+Inf bucket = %d, want 4", summary.Buckets["+Inf"])
	}
}

func TestHistogramEmpty(t *testing.T) {
	h := NewHistogram(nil)
	if h.Count() != 0 || h.Sum() != 0 {
		t.Fatalf("empty histogram count/sum = %d/%v, want 0/0", h.Count(), h.Sum())
	}
	for _, q := range []float64{0, 0.5, 0.95, 0.99, 1} {
		if got := h.Quantile(q); got != 0 {
			t.Errorf("empty Quantile(%v) = %v, want 0", q, got)
		}
	}
	summary := h.Summary()
	if summary.Count != 0 || summary.P50 != 0 || summary.Min != 0 || summary.Max != 0 {
		t.Errorf("empty summary = %+v, want zeroed", summary)
	}
	if summary.Buckets[formatInfBucket] != 0 {
		t.Errorf("empty +Inf bucket = %d, want 0", summary.Buckets[formatInfBucket])
	}
}

func TestHistogramNaNIgnoredAndBoundsSanitized(t *testing.T) {
	h := NewHistogram([]float64{math.Inf(1), 1, math.NaN(), 0.5})
	h.Observe(math.NaN())
	h.Observe(0.4)
	if h.Count() != 1 {
		t.Fatalf("count = %d, want 1 (NaN ignored)", h.Count())
	}
	if len(h.BucketBounds()) != 2 || h.BucketBounds()[0] != 0.5 || h.BucketBounds()[1] != 1 {
		t.Fatalf("bounds = %v, want [0.5 1] (non-finite dropped, sorted)", h.BucketBounds())
	}
}

// TestMetricsObserveKeepsLatestValue pins the legacy Observe/Snapshot
// semantics: last value wins, counts untouched, no histogram side effects.
func TestMetricsObserveKeepsLatestValue(t *testing.T) {
	m := NewMetrics()
	m.Observe("latency", 1.0)
	m.Observe("latency", 2.0)
	m.Inc("counter")
	counts, latency := m.Snapshot()
	if latency["latency"] != 2.0 {
		t.Errorf("latency[latency] = %v, want 2.0 (latest wins)", latency["latency"])
	}
	if counts["counter"] != 1 {
		t.Errorf("counts[counter] = %d, want 1", counts["counter"])
	}
	if summaries := m.SnapshotSummaries(); len(summaries) != 0 {
		t.Errorf("Observe must not create histograms, got %v", summaries)
	}
}

// TestMetricsObserveHistogramAndSummaries covers the new bucketed path.
func TestMetricsObserveHistogramAndSummaries(t *testing.T) {
	m := NewMetrics()
	m.ObserveHistogram("tool_duration_seconds", 0.2)
	m.ObserveHistogram("tool_duration_seconds", 0.8)
	m.ObserveHistogram("eval_duration_seconds", 1.5)

	summaries := m.SnapshotSummaries()
	s, ok := summaries["tool_duration_seconds"]
	if !ok {
		t.Fatalf("missing summary for tool_duration_seconds in %v", summaries)
	}
	if s.Count != 2 || math.Abs(s.Sum-1.0) > 1e-9 {
		t.Errorf("summary count/sum = %d/%v, want 2/1.0", s.Count, s.Sum)
	}
	// Observations {0.2, 0.8} with default buckets: cumulative hits 1 at le=0.25
	// and 2 at le=1, so interpolation yields p50=0.25 and p95=0.95.
	if math.Abs(s.P50-0.25) > 1e-9 {
		t.Errorf("p50 = %v, want 0.25", s.P50)
	}
	if math.Abs(s.P95-0.95) > 1e-9 {
		t.Errorf("p95 = %v, want 0.95", s.P95)
	}
	if _, ok := summaries["eval_duration_seconds"]; !ok {
		t.Errorf("missing summary for eval_duration_seconds")
	}
}

// TestMetricsObserveHTTPRecordsBothViews pins the dual recording: legacy
// latest-value gauge key plus the new agentos_request_duration_seconds
// histogram with the same labels.
func TestMetricsObserveHTTPRecordsBothViews(t *testing.T) {
	m := NewMetrics()
	m.ObserveHTTP("/v1/agents", "GET", 200, 0.075)

	_, latency := m.Snapshot()
	wantLatencyKey := "http_request_duration_seconds{route=\"/v1/agents\",method=\"GET\",status=\"200\"}"
	if latency[wantLatencyKey] != 0.075 {
		t.Errorf("latency key %q missing/wrong: %v", wantLatencyKey, latency)
	}

	summaries := m.SnapshotSummaries()
	wantHistogramKey := "agentos_request_duration_seconds{route=\"/v1/agents\",method=\"GET\",status=\"200\"}"
	s, ok := summaries[wantHistogramKey]
	if !ok {
		t.Fatalf("missing histogram %q in %v", wantHistogramKey, summaries)
	}
	if s.Count != 1 {
		t.Errorf("histogram count = %d, want 1", s.Count)
	}
	if s.Buckets["+Inf"] != 1 || s.Buckets["0.1"] != 1 {
		t.Errorf("buckets wrong: %v", s.Buckets)
	}
}

func TestMetricsDomainCounters(t *testing.T) {
	m := NewMetrics()
	m.IncRuns()
	m.IncTools()
	m.IncTools()
	counts, _ := m.Snapshot()
	if counts[MetricRunsTotal] != 1 {
		t.Errorf("%s = %d, want 1", MetricRunsTotal, counts[MetricRunsTotal])
	}
	if counts[MetricToolsTotal] != 2 {
		t.Errorf("%s = %d, want 2", MetricToolsTotal, counts[MetricToolsTotal])
	}
}

func TestHistogramConcurrentObserve(t *testing.T) {
	h := NewHistogram(nil)
	var wg sync.WaitGroup
	const workers, perWorker = 8, 250
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perWorker; j++ {
				h.Observe(0.05)
			}
		}()
	}
	wg.Wait()
	if got := h.Count(); got != workers*perWorker {
		t.Fatalf("count = %d, want %d", got, workers*perWorker)
	}
	if got, want := h.Sum(), float64(workers*perWorker)*0.05; math.Abs(got-want) > 1e-6 {
		t.Errorf("sum = %v, want %v", got, want)
	}
}

func TestFormatBucketBound(t *testing.T) {
	cases := map[float64]string{
		0.005: "0.005",
		0.1:   "0.1",
		1:     "1",
		2.5:   "2.5",
		10:    "10",
	}
	for in, want := range cases {
		if got := FormatBucketBound(in); got != want {
			t.Errorf("FormatBucketBound(%v) = %q, want %q", in, got, want)
		}
	}
}
