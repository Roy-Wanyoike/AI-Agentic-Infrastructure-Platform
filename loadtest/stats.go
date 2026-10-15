// Package loadtest implements the AgentOS comprehensive load-testing suite
// (issue #125): sustained load, spike, soak, concurrency, workflow stress and
// agent lifecycle scenarios driven by the agentload CLI
// (loadtest/cmd/agentload).
//
// The package is black-box tooling: it talks to a running AgentOS API over
// HTTP only (stdlib net/http) and never imports internal packages. Reports
// are plain JSON so they can be recorded as baselines (agentload record) and
// gated in CI (agentload compare).
package loadtest

import (
	"math"
	"sort"
	"sync"
	"time"
)

// Percentile returns the nearest-rank percentile of an ascending slice.
// copies are not needed: the input MUST already be sorted ascending.
//
// Nearest-rank definition: for n samples and percentile p in (0,100],
// the result is sample[ceil(p/100*n)-1] (0-based index), clamped to [0,n-1].
// This is deterministic, allocation-free and matches the common
// "nearest-rank" method: P50 of 1..100 is 50, P95 is 95, P99 is 99.
// An empty slice returns 0.
func Percentile(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if p <= 0 {
		return sorted[0]
	}
	if p >= 100 {
		return sorted[n-1]
	}
	idx := int(math.Ceil(p/100*float64(n))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= n {
		idx = n - 1
	}
	return sorted[idx]
}

// Mean returns the arithmetic mean of the samples (0 for an empty slice).
func Mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}

// LinearSlope returns the least-squares slope (units of y per unit of x) of
// the (x,y) samples. It is the "growth trend line" used by the soak scenario
// to detect monotonic growth in process metrics. Both slices must have the
// same length; slices shorter than 2 samples (or a flat x axis) return 0.
func LinearSlope(xs, ys []float64) float64 {
	n := len(xs)
	if n != len(ys) || n < 2 {
		return 0
	}
	mx := Mean(xs)
	my := Mean(ys)
	var num, den float64
	for i := range xs {
		dx := xs[i] - mx
		num += dx * (ys[i] - my)
		den += dx * dx
	}
	if den == 0 {
		return 0
	}
	return num / den
}

// sortedCopy returns an ascending copy of the samples so percentile
// computation never mutates the collector's backing array.
func sortedCopy(xs []float64) []float64 {
	out := make([]float64, len(xs))
	copy(out, xs)
	sort.Float64s(out)
	return out
}

// collector accumulates latency samples (milliseconds) and error/status-code
// counts for one measured operation. It is safe for concurrent use.
//
// Latency is recorded for every completed HTTP exchange (success or failure,
// matching the issue #80 baseline harness semantics); "requests" is therefore
// len(samples) and the error rate is errors/requests. Transport failures are
// recorded with status -1. Each sample also carries its wall-clock timestamp
// (Unix seconds) so scenarios can compute rolling time windows (spike
// recovery) and first-half/second-half shifts (soak leak heuristics).
type collector struct {
	mu    sync.Mutex
	t0    float64
	lat   []float64 // milliseconds
	ts    []float64 // wall-clock seconds per sample
	isErr []bool
	codes map[int]int64
	errs  int64
	drops int64 // client-side queue drops (request never issued)
}

func newCollector() *collector {
	return &collector{codes: make(map[int]int64)}
}

// nowTS is the timestamp axis of every sample: Unix epoch seconds.
func nowTS() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// add records one completed request: status is the HTTP status code (or -1
// for transport errors); want is the expected status (<=0 accepts any).
func (c *collector) add(status, want int, latMs float64) {
	err := status < 0 || (want > 0 && status != want)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.t0 == 0 {
		c.t0 = nowTS()
	}
	c.lat = append(c.lat, latMs)
	c.ts = append(c.ts, nowTS())
	c.isErr = append(c.isErr, err)
	c.codes[status]++
	if err {
		c.errs++
	}
}

// addAt is add with an explicit timestamp (Unix seconds). Production code
// uses add; the explicit form exists for deterministic tests of the
// window/trend math.
func (c *collector) addAt(ts float64, status, want int, latMs float64) {
	err := status < 0 || (want > 0 && status != want)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.t0 == 0 {
		c.t0 = ts
	}
	c.lat = append(c.lat, latMs)
	c.ts = append(c.ts, ts)
	c.isErr = append(c.isErr, err)
	c.codes[status]++
	if err {
		c.errs++
	}
}

// drop counts a request that could not be issued because the dispatch queue
// was full (generator saturation, not a server error).
func (c *collector) drop() {
	c.mu.Lock()
	c.drops++
	c.mu.Unlock()
}

// merge folds another collector into this one (used to report several phases
// of one scenario under a single op).
func (c *collector) merge(o *collector) {
	o.mu.Lock()
	defer o.mu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.t0 == 0 {
		c.t0 = o.t0
	}
	c.lat = append(c.lat, o.lat...)
	c.ts = append(c.ts, o.ts...)
	c.isErr = append(c.isErr, o.isErr...)
	for k, v := range o.codes {
		c.codes[k] += v
	}
	c.errs += o.errs
	c.drops += o.drops
}

// stats returns the request/error totals and the P50/P95/P99/mean/max
// latency summary in milliseconds.
func (c *collector) stats() (requests, errors int64, p50, p95, p99, mean, max float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := sortedCopy(c.lat)
	return int64(len(s)), c.errs,
		Percentile(s, 50), Percentile(s, 95), Percentile(s, 99),
		Mean(s), maxOf(s)
}

// slice returns a copy of the recorded latency samples (ms) sorted ascending.
func (c *collector) slice() []float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return sortedCopy(c.lat)
}

// totals returns the raw counters (requests, errors, drops, status codes).
func (c *collector) totals() (requests, errors, drops int64, codes map[int]int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	codes = make(map[int]int64, len(c.codes))
	for k, v := range c.codes {
		codes[k] = v
	}
	return int64(len(c.lat)), c.errs, c.drops, codes
}

// timeline returns the samples with their timestamps (both unsorted copies).
func (c *collector) timeline() (lat, ts []float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]float64(nil), c.lat...), append([]float64(nil), c.ts...)
}

// totalsWindow returns requests and errors recorded with a timestamp in
// [from, to) (Unix seconds). Used for first/second-half soak comparisons.
func (c *collector) totalsWindow(from, to float64) (requests, errors int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.ts {
		if c.ts[i] >= from && c.ts[i] < to {
			requests++
			if c.isErr[i] {
				errors++
			}
		}
	}
	return requests, errors
}

// window returns the latencies whose timestamps fall in [now-keep, now],
// sorted ascending (a rolling window for spike recovery checks).
func window(xs, ts []float64, now, keep float64) []float64 {
	var out []float64
	for i := range xs {
		if ts[i] >= now-keep && ts[i] <= now {
			out = append(out, xs[i])
		}
	}
	sort.Float64s(out)
	return out
}

func maxOf(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	m := xs[0]
	for _, x := range xs[1:] {
		if x > m {
			m = x
		}
	}
	return m
}
