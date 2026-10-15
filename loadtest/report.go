package loadtest

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// ToolName / ToolVersion identify reports written by this suite (they appear
// in every JSON report and in recorded baselines).
const (
	ToolName    = "agentload"
	ToolVersion = "1.0.0"
)

// OpStats is the measured result of one operation (an HTTP call pattern)
// across a scenario run: request/error totals, throughput and the
// P50/P95/P99/mean/max latency summary in milliseconds.
type OpStats struct {
	Op          string        `json:"op"`
	Requests    int64         `json:"requests"`
	Errors      int64         `json:"errors"`
	Drops       int64         `json:"client_drops"`
	ErrorRate   float64       `json:"error_rate"`
	RPS         float64       `json:"rps"`
	P50Ms       float64       `json:"p50_ms"`
	P95Ms       float64       `json:"p95_ms"`
	P99Ms       float64       `json:"p99_ms"`
	MeanMs      float64       `json:"mean_ms"`
	MaxMs       float64       `json:"max_ms"`
	StatusCodes map[int]int64 `json:"status_codes,omitempty"`
}

// Totals aggregates the measured (non-setup) operations of a run.
type Totals struct {
	Requests  int64   `json:"requests"`
	Errors    int64   `json:"errors"`
	Drops     int64   `json:"client_drops"`
	ErrorRate float64 `json:"error_rate"`
	RPS       float64 `json:"rps"`
}

// Report is the full machine-readable result of one scenario run. It is the
// unit of baseline comparison (agentload compare): baselines are Reports and
// current runs are Reports.
type Report struct {
	Tool             string          `json:"tool"`
	ToolVersion      string          `json:"tool_version"`
	Scenario         string          `json:"scenario"`
	Base             string          `json:"base"`
	StartedAt        time.Time       `json:"started_at"`
	DurationSec      float64         `json:"duration_s"`
	Config           map[string]any  `json:"config"`
	Ops              []OpStats       `json:"ops"`
	SetupOps         []OpStats       `json:"setup_ops,omitempty"`
	Totals           Totals          `json:"totals"`
	Resources        *ResourceReport `json:"resources,omitempty"`
	Extra            map[string]any  `json:"extra,omitempty"`
	ClientGoroutines int             `json:"client_goroutines"`
}

// errorRate is errors/requests (0 when nothing was measured).
func errorRate(requests, errors int64) float64 {
	if requests <= 0 {
		return 0
	}
	return float64(errors) / float64(requests)
}

// opStatsFromCollector renders one measured operation. rps uses the measured
// wall time of the phase the op ran in.
func opStatsFromCollector(name string, c *collector, elapsed time.Duration) OpStats {
	requests, errs, drops, codes := c.totals()
	_, _, p50, p95, p99, mean, max := c.stats()
	st := OpStats{
		Op:          name,
		Requests:    requests,
		Errors:      errs,
		Drops:       drops,
		ErrorRate:   errorRate(requests, errs),
		P50Ms:       p50,
		P95Ms:       p95,
		P99Ms:       p99,
		MeanMs:      mean,
		MaxMs:       max,
		StatusCodes: codes,
	}
	if elapsed > 0 {
		st.RPS = float64(requests) / elapsed.Seconds()
	}
	return st
}

// reportBuilder assembles a Report while a scenario runs: one collector per
// measured op plus optional setup collectors (reported separately and
// excluded from the totals and from baseline comparison).
type reportBuilder struct {
	scenario string
	cfg      *Config
	started  time.Time
	ops      map[string]*collector
	opOrder  []string
	setup    map[string]*collector
}

func newReportBuilder(scenario string, cfg *Config) *reportBuilder {
	return &reportBuilder{
		scenario: scenario,
		cfg:      cfg,
		started:  time.Now(),
		ops:      make(map[string]*collector),
		setup:    make(map[string]*collector),
	}
}

// op returns (creating if needed) the collector of a measured op.
func (b *reportBuilder) op(name string) *collector {
	if c, ok := b.ops[name]; ok {
		return c
	}
	c := newCollector()
	b.ops[name] = c
	b.opOrder = append(b.opOrder, name)
	return c
}

// setupOp returns the collector of a one-time setup op (register/login/...).
func (b *reportBuilder) setupOp(name string) *collector {
	if c, ok := b.setup[name]; ok {
		return c
	}
	c := newCollector()
	b.setup["setup."+name] = c
	return c
}

// build renders the report. opsElapsed is the wall time of the measured
// phase(s) used for RPS normalization (zero falls back to time.Since start).
func (b *reportBuilder) build(opsElapsed time.Duration, resources *ResourceReport, extra map[string]any) *Report {
	if opsElapsed <= 0 {
		opsElapsed = time.Since(b.started)
	}
	rep := &Report{
		Tool:        ToolName,
		ToolVersion: ToolVersion,
		Scenario:    b.scenario,
		Base:        b.cfg.Base,
		StartedAt:   b.started,
		DurationSec: opsElapsed.Seconds(),
		Config:      b.cfg.configMap(),
		Resources:   resources,
		Extra:       extra,
	}
	var reqs, errs, drops int64
	for _, name := range b.opOrder {
		st := opStatsFromCollector(name, b.ops[name], opsElapsed)
		rep.Ops = append(rep.Ops, st)
		reqs += st.Requests
		errs += st.Errors
		drops += st.Drops
	}
	rep.Totals = Totals{
		Requests:  reqs,
		Errors:    errs,
		Drops:     drops,
		ErrorRate: errorRate(reqs, errs),
		RPS:       float64(reqs) / opsElapsed.Seconds(),
	}
	for _, name := range sortedKeys(b.setup) {
		rep.SetupOps = append(rep.SetupOps, opStatsFromCollector(name, b.setup[name], opsElapsed))
	}
	if rep.Resources != nil {
		rep.ClientGoroutines = rep.Resources.ClientGoroutines
	}
	return rep
}

func sortedKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// WriteJSON encodes the report indented (file and stdout artifact).
func (r *Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// WriteSummary renders the human-readable summary: a percentile table per op,
// totals, resource trends and scenario extras.
func (r *Report) WriteSummary(w io.Writer) {
	fmt.Fprintf(w, "AgentOS load test (issue #125) — scenario %s (%s v%s)\n", r.Scenario, r.Tool, r.ToolVersion)
	fmt.Fprintf(w, "base=%s duration=%.1fs started=%s\n\n", r.Base, r.DurationSec, r.StartedAt.Format(time.RFC3339))
	writeOpTable(w, "OP", r.Ops)
	if len(r.SetupOps) > 0 {
		fmt.Fprintln(w)
		writeOpTable(w, "SETUP OP (excluded from totals and comparison)", r.SetupOps)
	}
	fmt.Fprintf(w, "\ntotals: requests=%d errors=%d client_drops=%d error_rate=%.4f rps=%.1f\n",
		r.Totals.Requests, r.Totals.Errors, r.Totals.Drops, r.Totals.ErrorRate, r.Totals.RPS)
	if r.Resources != nil {
		writeResourceTable(w, r.Resources)
	}
	if len(r.Extra) > 0 {
		fmt.Fprintln(w, "\nscenario details:")
		for _, k := range sortedKeys(r.Extra) {
			fmt.Fprintf(w, "  %s: %v\n", k, r.Extra[k])
		}
	}
}

func writeOpTable(w io.Writer, title string, ops []OpStats) {
	fmt.Fprintf(w, "%s\n", title)
	fmt.Fprintf(w, "%-22s %10s %8s %8s %10s %10s %10s %10s %10s %8s\n",
		"OP", "REQUESTS", "ERRORS", "ERRRATE", "P50(ms)", "P95(ms)", "P99(ms)", "MEAN(ms)", "MAX(ms)", "RPS")
	for _, st := range ops {
		fmt.Fprintf(w, "%-22s %10d %8d %8.4f %10.2f %10.2f %10.2f %10.2f %10.2f %8.1f\n",
			st.Op, st.Requests, st.Errors, st.ErrorRate, st.P50Ms, st.P95Ms, st.P99Ms, st.MeanMs, st.MaxMs, st.RPS)
	}
}

func writeResourceTable(w io.Writer, rr *ResourceReport) {
	fmt.Fprintf(w, "\nresource trends (%d samples @ %.1fs, sample errors=%d, client goroutines=%d)\n",
		rr.Samples, rr.IntervalSec, rr.Errors, rr.ClientGoroutines)
	fmt.Fprintf(w, "%-46s %12s %12s %12s %12s\n", "FAMILY", "START", "END", "DELTA", "SLOPE/S")
	families := make([]string, 0, len(rr.Families))
	for f := range rr.Families {
		families = append(families, f)
	}
	sort.Strings(families)
	for _, f := range families {
		tr := rr.Families[f]
		fmt.Fprintf(w, "%-46s %12.2f %12.2f %12.2f %12.4f\n", f, tr.Start, tr.End, tr.Delta, tr.SlopePerSec)
	}
}

// scenarioTrend renders one Extra value as a human-readable string.
func trendString(tr ResourceTrend) string {
	return fmt.Sprintf("start=%.2f end=%.2f delta=%.2f slope/s=%.4f", tr.Start, tr.End, tr.Delta, tr.SlopePerSec)
}

// shortTrend is used by the soak verdict message.
func shortTrend(tr ResourceTrend) string {
	return strings.TrimSpace(fmt.Sprintf("delta=%.2f slope/s=%.4f", tr.Delta, tr.SlopePerSec))
}
