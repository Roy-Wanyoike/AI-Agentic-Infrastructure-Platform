package loadtest

import (
	"context"
	"io"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ParsePrometheusText parses the Prometheus TEXT exposition format (version
// 0.0.4) that GET /v1/metrics serves. It returns one value per metric family:
// for families that carry labels (e.g. agentos_http_requests_total{...}) the
// sample values are SUMMED so callers see the process-wide total. Comment
// (# HELP/# TYPE) and blank lines are ignored; malformed sample lines are
// skipped (the parser must be lenient: it samples a live endpoint mid-run).
//
// Histogram families appear under their suffixed names
// (..._bucket/_sum/_count) exactly as they are rendered.
func ParsePrometheusText(body string) map[string]float64 {
	out := make(map[string]float64)
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// sample line: name{labels} value [timestamp]
		name := line
		if sp := strings.IndexAny(line, " \t"); sp >= 0 {
			name = strings.TrimSpace(line[:sp])
			line = strings.TrimSpace(line[sp+1:])
		} else {
			continue // no value: not a sample line
		}
		// Strip labels: family name is everything before '{'.
		if brace := strings.IndexByte(name, '{'); brace >= 0 {
			name = name[:brace]
		}
		if name == "" {
			continue
		}
		// Value is the second field; a trailing timestamp (optional) is the
		// third and is ignored.
		valueField := line
		if sp := strings.IndexAny(line, " \t"); sp >= 0 {
			valueField = line[:sp]
		}
		v, err := strconv.ParseFloat(valueField, 64)
		if err != nil {
			continue // lenient: skip unparsable values
		}
		out[name] += v
	}
	return out
}

// FamilyNames returns the sorted family names of a parsed snapshot
// (deterministic reports and tests).
func FamilyNames(m map[string]float64) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// resourceSample is one snapshot of the server's metric families.
type resourceSample struct {
	at    time.Time
	vals  map[string]float64
	goros int // harness-side goroutines (client health, not the server)
}

// ResourceSampler polls GET {prefix}/metrics on an interval for the lifetime
// of a scenario and records per-family trend lines. It is safe for concurrent
// use: scenarios read Report() only after the sampling context is cancelled.
type ResourceSampler struct {
	interval time.Duration
	url      string
	token    string
	client   *http.Client

	mu      sync.Mutex
	samples []resourceSample
	errs    int
}

// NewResourceSampler builds a sampler polling url every interval with an
// optional bearer token. Interval <= 0 defaults to 5s.
func NewResourceSampler(client *http.Client, url, token string, interval time.Duration) *ResourceSampler {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return &ResourceSampler{interval: interval, url: url, token: token, client: client}
}

// Run samples until ctx is cancelled. It is blocking; run it on its own
// goroutine. The first sample is taken immediately so start/end deltas are
// well defined even for short scenarios.
func (s *ResourceSampler) Run(ctx context.Context) {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	s.sampleOnce()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sampleOnce()
		}
	}
}

func (s *ResourceSampler) sampleOnce() {
	req, err := http.NewRequest(http.MethodGet, s.url, nil)
	if err != nil {
		s.fail()
		return
	}
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		s.fail()
		return
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK {
		s.fail()
		return
	}
	s.mu.Lock()
	s.samples = append(s.samples, resourceSample{
		at:    time.Now(),
		vals:  ParsePrometheusText(string(body)),
		goros: runtime.NumGoroutine(),
	})
	s.mu.Unlock()
}

func (s *ResourceSampler) fail() {
	s.mu.Lock()
	s.errs++
	s.mu.Unlock()
}

// goroutineCount is reported per sample; the server does not expose a
// runtime goroutine gauge or /debug/pprof (verified for issue #125), so
// server-side leak signals come from the /metrics families, while the
// harness-side count (runtime.NumGoroutine) watches the generator itself.

// ResourceTrend is the start/end/delta + per-second growth line of one metric
// family across a scenario.
type ResourceTrend struct {
	Start       float64 `json:"start"`
	End         float64 `json:"end"`
	Delta       float64 `json:"delta"`
	SlopePerSec float64 `json:"slope_per_s"`
}

// ResourceReport summarises the sampled metrics of one scenario.
type ResourceReport struct {
	IntervalSec float64                  `json:"interval_s"`
	Samples     int                      `json:"samples"`
	Errors      int                      `json:"errors"`
	Families    map[string]ResourceTrend `json:"families"`
	// ClientGoroutines is the last harness-side goroutine count (leak watch
	// on the generator itself).
	ClientGoroutines int `json:"client_goroutines"`
}

// Report summarises the recorded samples. With fewer than 2 samples the
// report carries the single snapshot (or errors) and zero-valued trends.
func (s *ResourceSampler) Report(interval time.Duration) *ResourceReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	rep := &ResourceReport{
		IntervalSec: interval.Seconds(),
		Families:    make(map[string]ResourceTrend),
		Errors:      s.errs,
	}
	if len(s.samples) == 0 {
		return rep
	}
	first := s.samples[0]
	last := s.samples[len(s.samples)-1]
	rep.Samples = len(s.samples)
	rep.ClientGoroutines = last.goros
	families := make(map[string]struct{})
	for _, smp := range s.samples {
		for f := range smp.vals {
			families[f] = struct{}{}
		}
	}
	t0 := first.at
	for f := range families {
		xs := make([]float64, 0, len(s.samples))
		ys := make([]float64, 0, len(s.samples))
		for _, smp := range s.samples {
			if v, ok := smp.vals[f]; ok {
				xs = append(xs, smp.at.Sub(t0).Seconds())
				ys = append(ys, v)
			}
		}
		if len(ys) == 0 {
			continue
		}
		tr := ResourceTrend{Start: ys[0], End: ys[len(ys)-1], Delta: ys[len(ys)-1] - ys[0]}
		tr.SlopePerSec = LinearSlope(xs, ys)
		rep.Families[f] = tr
	}
	return rep
}
