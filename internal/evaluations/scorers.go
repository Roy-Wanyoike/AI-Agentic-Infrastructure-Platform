// Package evaluations implements the evaluation subsystem: datasets of test
// cases, synchronous bounded runs that execute every case through the agent
// runtime, per-case scoring, run summaries, and baseline/candidate run
// comparison.
//
// All tenant-facing operations are scoped by organization_id (multi-tenant
// guard), IDs are UUIDs (except case IDs which are caller supplied, e.g.
// "c1"), and timestamps are RFC3339 UTC at the API boundary.
package evaluations

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Scorer is the name of the evaluation strategy applied to one case output.
type Scorer string

const (
	// ScorerExact passes when the output equals the expected string byte for byte.
	ScorerExact Scorer = "exact"
	// ScorerContains passes when the output contains the expected substring.
	ScorerContains Scorer = "contains"
	// ScorerRegex passes when the output matches (re.MatchString semantics)
	// the params.pattern regular expression.
	ScorerRegex Scorer = "regex"
	// ScorerLatencyUnderMs passes when the measured case latency is at or
	// under params.threshold_ms (threshold is inclusive: "must not exceed").
	ScorerLatencyUnderMs Scorer = "latency_under_ms"
	// ScorerCostUnderCents passes when the recorded case cost is at or under
	// params.threshold_cents (threshold is inclusive).
	ScorerCostUnderCents Scorer = "cost_under_cents"
)

// Valid reports whether the scorer name is one of the supported kinds.
func (s Scorer) Valid() bool {
	switch s {
	case ScorerExact, ScorerContains, ScorerRegex, ScorerLatencyUnderMs, ScorerCostUnderCents:
		return true
	}
	return false
}

// Params carries the optional per-scorer tuning knobs. Thresholds are
// pointers so missing values can be distinguished from zero at validation
// time (a zero threshold is always a misconfiguration).
type Params struct {
	// Pattern is the regular expression for the regex scorer.
	Pattern string `json:"pattern,omitempty"`
	// ThresholdMS is the inclusive latency ceiling in milliseconds for
	// latency_under_ms.
	ThresholdMS *float64 `json:"threshold_ms,omitempty"`
	// ThresholdCents is the inclusive cost ceiling in cents for
	// cost_under_cents.
	ThresholdCents *float64 `json:"threshold_cents,omitempty"`
}

// Validate checks the scorer/params combination of a case definition so
// misconfigured cases are rejected at dataset creation instead of failing
// mysteriously at run time.
func (p Params) Validate(scorer Scorer) error {
	switch scorer {
	case ScorerExact, ScorerContains:
		return nil
	case ScorerRegex:
		if strings.TrimSpace(p.Pattern) == "" {
			return errors.New("regex scorer requires params.pattern")
		}
		if _, err := regexp.Compile(p.Pattern); err != nil {
			return fmt.Errorf("invalid regex pattern %q: %w", p.Pattern, err)
		}
		return nil
	case ScorerLatencyUnderMs:
		if p.ThresholdMS == nil || *p.ThresholdMS <= 0 {
			return errors.New("latency_under_ms scorer requires params.threshold_ms > 0")
		}
		return nil
	case ScorerCostUnderCents:
		if p.ThresholdCents == nil || *p.ThresholdCents <= 0 {
			return errors.New("cost_under_cents scorer requires params.threshold_cents > 0")
		}
		return nil
	default:
		return fmt.Errorf("unknown scorer %q", scorer)
	}
}

// Score evaluates one case output. latencyMS is the measured wall-clock
// duration of the case execution in milliseconds; costCents is the recorded
// cost in cents (0 when the runtime does not expose cost). It returns the
// normalized score (1.0 pass / 0.0 fail for every scorer today) and whether
// the case passed. A returned error means the scorer itself was misconfigured
// (e.g. an uncompilable pattern that bypassed validation); the engine records
// the error and fails the case.
func (c Case) Score(output string, latencyMS, costCents float64) (float64, bool, error) {
	switch c.Scorer {
	case ScorerExact:
		if output == c.Expected {
			return 1.0, true, nil
		}
		return 0.0, false, nil
	case ScorerContains:
		if strings.Contains(output, c.Expected) {
			return 1.0, true, nil
		}
		return 0.0, false, nil
	case ScorerRegex:
		re, err := regexp.Compile(c.Params.Pattern)
		if err != nil {
			return 0.0, false, fmt.Errorf("scorer regex: %w", err)
		}
		if re.MatchString(output) {
			return 1.0, true, nil
		}
		return 0.0, false, nil
	case ScorerLatencyUnderMs:
		if c.Params.ThresholdMS == nil {
			return 0.0, false, errors.New("scorer latency_under_ms: missing threshold_ms")
		}
		if latencyMS <= *c.Params.ThresholdMS {
			return 1.0, true, nil
		}
		return 0.0, false, nil
	case ScorerCostUnderCents:
		if c.Params.ThresholdCents == nil {
			return 0.0, false, errors.New("scorer cost_under_cents: missing threshold_cents")
		}
		if costCents <= *c.Params.ThresholdCents {
			return 1.0, true, nil
		}
		return 0.0, false, nil
	default:
		return 0.0, false, fmt.Errorf("unknown scorer %q", c.Scorer)
	}
}
