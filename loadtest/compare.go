package loadtest

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
)

// Comparison tolerances (issue #125 contract): a scenario regresses when the
// current P95 exceeds baseline P95 by more than P95Tolerance (relative) or
// the error rate rises by more than ErrorTolerance (absolute percentage
// points). Defaults: +10% P95, +2pp error rate.
const (
	DefaultP95Tolerance   = 1.10
	DefaultErrorTolerance = 0.02
)

// setupOpPrefix marks one-time setup operations (setup.register, ...). They
// are excluded from the comparison gate: they run once per run (a handful of
// samples, dominated by bcrypt) and are not the throughput path.
const setupOpPrefix = "setup."

// Regression is one detected baseline violation.
type Regression struct {
	Op        string  `json:"op"`
	Kind      string  `json:"kind"` // "p95" or "error_rate"
	Baseline  float64 `json:"baseline"`
	Current   float64 `json:"current"`
	ChangePct float64 `json:"change_pct"`
	Detail    string  `json:"detail"`
}

// CompareReports gates a current report against a baseline report. Ops are
// matched by name (setup.* ops are skipped). Requests-level sanity: an op
// with zero requests in either report is skipped (nothing to compare).
// The returned slice is sorted by op name for deterministic output.
func CompareReports(baseline, current *Report, p95Tol, errTol float64) []Regression {
	base := opIndex(baseline)
	cur := opIndex(current)
	var out []Regression
	for _, name := range sortedKeys(base) {
		if isSetupOp(name) {
			continue
		}
		b, ok := base[name]
		if !ok || b.Requests == 0 {
			continue
		}
		c, ok := cur[name]
		if !ok {
			out = append(out, Regression{
				Op:     name,
				Kind:   "missing",
				Detail: "op present in baseline but missing in the current report",
			})
			continue
		}
		if c.Requests == 0 {
			continue
		}
		// P95 gate: current > baseline * tolerance (relative +10% by default).
		if c.P95Ms > b.P95Ms*p95Tol {
			pct := pctChange(b.P95Ms, c.P95Ms)
			out = append(out, Regression{
				Op: name, Kind: "p95",
				Baseline: b.P95Ms, Current: c.P95Ms, ChangePct: pct,
				Detail: fmt.Sprintf("p95 %.2fms -> %.2fms (%+.1f%%) exceeds +%.0f%% tolerance", b.P95Ms, c.P95Ms, pct, (p95Tol-1)*100),
			})
		}
		// Error-rate gate: +2 percentage points by default.
		if c.ErrorRate-b.ErrorRate > errTol {
			out = append(out, Regression{
				Op: name, Kind: "error_rate",
				Baseline: b.ErrorRate, Current: c.ErrorRate,
				ChangePct: (c.ErrorRate - b.ErrorRate) * 100,
				Detail:    fmt.Sprintf("error rate %.4f -> %.4f (+%.1fpp) exceeds %.0fpp tolerance", b.ErrorRate, c.ErrorRate, (c.ErrorRate-b.ErrorRate)*100, errTol*100),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Op != out[j].Op {
			return out[i].Op < out[j].Op
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

func isSetupOp(name string) bool {
	return len(name) >= len(setupOpPrefix) && name[:len(setupOpPrefix)] == setupOpPrefix
}

func opIndex(r *Report) map[string]OpStats {
	out := make(map[string]OpStats, len(r.Ops))
	for _, op := range r.Ops {
		out[op.Op] = op
	}
	return out
}

func pctChange(from, to float64) float64 {
	if from == 0 {
		return 0
	}
	return (to - from) / from * 100
}

// LoadReport reads a Report JSON written by run/record.
func LoadReport(path string) (*Report, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rep Report
	if err := json.Unmarshal(raw, &rep); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if rep.Tool != "" && rep.Tool != ToolName {
		return nil, fmt.Errorf("%s is not an %s report (tool=%q)", path, ToolName, rep.Tool)
	}
	return &rep, nil
}

// WriteComparison renders the comparison table and verdict. It returns the
// regressions so the CLI can pick the exit code.
func WriteComparison(w io.Writer, baseline, current *Report, regs []Regression, p95Tol, errTol float64) {
	fmt.Fprintf(w, "agentload compare (issue #125)\n")
	fmt.Fprintf(w, "baseline: %s (%s, %.1fs)  current: %s (%s, %.1fs)\n",
		baseline.Scenario, baseline.StartedAt.Format("2006-01-02 15:04:05"), baseline.DurationSec,
		current.Scenario, current.StartedAt.Format("2006-01-02 15:04:05"), current.DurationSec)
	fmt.Fprintf(w, "gates: p95 x%.2f, error_rate +%.0fpp\n\n", p95Tol, errTol*100)
	fmt.Fprintf(w, "%-22s %12s %12s %10s %10s %10s\n", "OP", "P95 BASE", "P95 CUR", "DP95(%)", "ERR BASE", "ERR CUR")
	base := opIndex(baseline)
	cur := opIndex(current)
	for _, name := range sortedKeys(base) {
		if isSetupOp(name) {
			continue
		}
		b := base[name]
		c, ok := cur[name]
		if !ok {
			fmt.Fprintf(w, "%-22s %12.2f %12s %10s %10.4f %10s  (missing)\n", name, b.P95Ms, "-", "-", b.ErrorRate, "-")
			continue
		}
		dpct := pctChange(b.P95Ms, c.P95Ms)
		fmt.Fprintf(w, "%-22s %12.2f %12.2f %10.1f %10.4f %10.4f\n", name, b.P95Ms, c.P95Ms, dpct, b.ErrorRate, c.ErrorRate)
	}
	if len(regs) == 0 {
		fmt.Fprintln(w, "\nPASS: no regressions detected")
		return
	}
	fmt.Fprintf(w, "\nFAIL: %d regression(s)\n", len(regs))
	for _, r := range regs {
		fmt.Fprintf(w, "  - [%s] %s: %s\n", r.Kind, r.Op, r.Detail)
	}
}
