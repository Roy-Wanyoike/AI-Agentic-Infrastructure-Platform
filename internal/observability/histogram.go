package observability

import (
	"math"
	"sort"
	"strconv"
	"sync"
)

// ---------------------------------------------------------------------------
// Bucketed histogram metrics (Task 2-h, additive extension).
//
// The original Metrics primitives keep their exact semantics: Inc() counts and
// Observe() stores the latest observed value per key (surfaced through
// Snapshot() and the legacy /metrics?format=json payload). This file adds a
// parallel histogram store: ObserveHistogram(name, value) records value into
// fixed cumulative buckets so p50/p95/p99 estimates can be computed and
// exported both in the JSON payload ("histograms") and in the Prometheus text
// exposition (agentos_request_duration_seconds_bucket{le="..."} families).
//
// No new module dependencies are introduced; everything is hand-rolled.
// ---------------------------------------------------------------------------

// Well-known AgentOS metric family names used by the Prometheus exposition.
// Callers that want the domain counters to appear on /metrics increment them
// through IncRuns / IncTools (see docs/wiring/2-h.md for the wiring notes).
const (
	MetricRunsTotal       = "agentos_runs_total"
	MetricToolsTotal      = "agentos_tools_total"
	MetricRequestDuration = "agentos_request_duration_seconds"
	MetricQueueLength     = "agentos_queue_length"
)

// DefaultBuckets mirrors the default bucket bounds of the Prometheus client
// libraries (seconds). Used when a histogram is created without explicit
// bounds. Bounds must be finite; +Inf is implicit and always added last.
var DefaultBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// Histogram is a fixed-upper-bound cumulative histogram with p50/p95/p99
// estimation. It is safe for concurrent use.
type Histogram struct {
	mu         sync.Mutex
	bounds     []float64 // finite upper bounds, ascending
	cumulative []uint64  // cumulative count per bound (len == len(bounds))
	sum        float64
	count      uint64
	min        float64
	max        float64
}

// NewHistogram returns a histogram with the given finite upper bounds. When
// bounds is empty the package defaults (DefaultBuckets) are used. The +Inf
// bucket is implicit and must not be included. Bounds are sanitized
// (non-finite values dropped) and sorted ascending.
func NewHistogram(bounds []float64) *Histogram {
	cleaned := make([]float64, 0, len(bounds))
	for _, b := range bounds {
		if math.IsNaN(b) || math.IsInf(b, 0) {
			continue
		}
		cleaned = append(cleaned, b)
	}
	if len(cleaned) == 0 {
		cleaned = append(cleaned, DefaultBuckets...)
	}
	sort.Float64s(cleaned)
	return &Histogram{
		bounds:     cleaned,
		cumulative: make([]uint64, len(cleaned)),
		min:        math.Inf(1),
		max:        math.Inf(-1),
	}
}

// Observe records one value. NaN values are ignored (they carry no ordering
// information); +/-Inf are counted but only ±Inf clamp min/max.
func (h *Histogram) Observe(value float64) {
	if h == nil || math.IsNaN(value) {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.count++
	h.sum += value
	if value < h.min {
		h.min = value
	}
	if value > h.max {
		h.max = value
	}
	for i, bound := range h.bounds {
		if value <= bound {
			h.cumulative[i]++
		}
	}
}

// Count returns the number of observations.
func (h *Histogram) Count() uint64 {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.count
}

// Sum returns the accumulated sum of all observations.
func (h *Histogram) Sum() float64 {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sum
}

// CumulativeBuckets returns a copy of the cumulative counts per finite bound.
func (h *Histogram) CumulativeBuckets() []uint64 {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]uint64, len(h.cumulative))
	copy(out, h.cumulative)
	return out
}

// BucketBounds returns a copy of the finite upper bounds.
func (h *Histogram) BucketBounds() []float64 {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]float64, len(h.bounds))
	copy(out, h.bounds)
	return out
}

// Quantile estimates the q-quantile (0 <= q <= 1) from the cumulative buckets
// using linear interpolation inside the bucket that contains the target rank,
// matching the approach of Prometheus histogram_quantile. With no observations
// it returns 0. When the target rank falls above the last finite bound the
// last finite bound is returned (the true value lies in the implicit +Inf
// bucket and cannot be interpolated without an upper edge).
func (h *Histogram) Quantile(q float64) float64 {
	if h == nil {
		return 0
	}
	if q < 0 {
		q = 0
	}
	if q > 1 {
		q = 1
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.count == 0 || len(h.bounds) == 0 {
		return 0
	}
	rank := q * float64(h.count)
	prevBound := 0.0
	var prevCum uint64
	for i, bound := range h.bounds {
		cum := h.cumulative[i]
		if float64(cum) < rank {
			prevBound = bound
			prevCum = cum
			continue
		}
		if cum == prevCum {
			// Empty bucket range: the rank sits exactly on the previous
			// cumulative boundary, so the previous bound is the estimate.
			return prevBound
		}
		frac := (rank - float64(prevCum)) / float64(cum-prevCum)
		return prevBound + frac*(bound-prevBound)
	}
	return prevBound
}

// HistogramSummary is the compact, JSON-friendly view of one histogram used by
// the /metrics?format=json payload (key "histograms") and by the Prometheus
// exposition renderer.
type HistogramSummary struct {
	Count   uint64            `json:"count"`
	Sum     float64           `json:"sum"`
	Min     float64           `json:"min"`
	Max     float64           `json:"max"`
	P50     float64           `json:"p50"`
	P95     float64           `json:"p95"`
	P99     float64           `json:"p99"`
	Buckets map[string]uint64 `json:"buckets"`
}

// Summary computes the summary view. min/max/p50/p95/p99 are 0 while the
// histogram is empty.
func (h *Histogram) Summary() HistogramSummary {
	if h == nil {
		return HistogramSummary{Buckets: map[string]uint64{}}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	summary := HistogramSummary{
		Count: h.count,
		Sum:   h.sum,
		P50:   h.quantileLocked(0.50),
		P95:   h.quantileLocked(0.95),
		P99:   h.quantileLocked(0.99),
	}
	if h.count > 0 {
		summary.Min = h.min
		summary.Max = h.max
	}
	summary.Buckets = make(map[string]uint64, len(h.cumulative)+1)
	for i, bound := range h.bounds {
		summary.Buckets[FormatBucketBound(bound)] = h.cumulative[i]
	}
	summary.Buckets[formatInfBucket] = h.count
	return summary
}

// quantileLocked is Quantile without re-acquiring the lock.
func (h *Histogram) quantileLocked(q float64) float64 {
	if h.count == 0 || len(h.bounds) == 0 {
		return 0
	}
	rank := q * float64(h.count)
	prevBound := 0.0
	var prevCum uint64
	for i, bound := range h.bounds {
		cum := h.cumulative[i]
		if float64(cum) < rank {
			prevBound = bound
			prevCum = cum
			continue
		}
		if cum == prevCum {
			return prevBound
		}
		frac := (rank - float64(prevCum)) / float64(cum-prevCum)
		return prevBound + frac*(bound-prevBound)
	}
	return prevBound
}

// ---------------------------------------------------------------------------
// Metrics extensions
// ---------------------------------------------------------------------------

// ObserveHistogram records one bucketed observation under the given metric
// key. Unlike Observe (which stores only the latest value for backward
// compatibility) this feeds the histogram store used for percentile
// estimation and Prometheus exposition. Histograms are created lazily with
// DefaultBuckets.
func (m *Metrics) ObserveHistogram(name string, value float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.histograms == nil {
		m.histograms = make(map[string]*Histogram)
	}
	h, ok := m.histograms[name]
	if !ok {
		h = NewHistogram(nil)
		m.histograms[name] = h
	}
	h.Observe(value)
}

// SnapshotSummaries returns the histogram summaries keyed by metric key. The
// returned map (and the bucket maps inside) are copies; callers may mutate
// them freely.
func (m *Metrics) SnapshotSummaries() map[string]HistogramSummary {
	if m == nil {
		return map[string]HistogramSummary{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]HistogramSummary, len(m.histograms))
	for name, h := range m.histograms {
		out[name] = h.Summary()
	}
	return out
}

// IncRuns increments the agentos_runs_total counter (agent runs recorded).
func (m *Metrics) IncRuns() {
	m.Inc(MetricRunsTotal)
}

// IncTools increments the agentos_tools_total counter (tool executions).
func (m *Metrics) IncTools() {
	m.Inc(MetricToolsTotal)
}

// FormatBucketBound renders a finite bucket bound the way Prometheus does in
// the le label (shortest round-trip float, e.g. 0.005 -> "0.005", 1 -> "1").
func FormatBucketBound(b float64) string {
	return strconv.FormatFloat(b, 'g', -1, 64)
}

const formatInfBucket = "+Inf"
