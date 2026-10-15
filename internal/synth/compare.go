package synth

// compare.go — issue #156: release-version comparison. ReleaseComparator
// answers "did the candidate build change the product experience?" by
// comparing runs of the SAME versioned inputs — journey, persona,
// environment, fixture set and assertion set are pinned via
// VersionedRunInputs (extracted from run records; workers stamp the
// fixture/assertion set refs into step-event RedactedMeta). Any input or
// AppVersion drift is reported in VersionDrift and forces Meaningful=false:
// a comparison across different journeys/personas/fixtures is never
// presented as evidence.
//
// Metric deltas (candidate − stable):
//   - JourneyCompletionDeltaPP: journey completion rate, percentage points
//   - LatencyDelta: run duration p50/p95 in ms, absolute + relative (%)
//   - AssertionPassDeltaPP: deterministic assertion pass rate, pp
//   - ConsoleErrorDelta: console errors per run (absolute)
//
// Meaningful-difference thresholds (DefaultComparisonThresholds) — a
// difference is MEANINGFUL when ANY of:
//   - |completion delta|            >= 5 percentage points
//   - |p50 latency relative delta|  >= 20 %  (baseline > 0)
//   - |p95 latency relative delta|  >= 30 %  (baseline > 0)
//   - |assertion pass delta|        >= 2 percentage points
//   - |console errors per run|      >= 0.5
// Direction is annotated in Reasons (regression vs improvement). A zero
// latency baseline makes the relative check inapplicable (documented, not
// silently treated as zero drift).
//
// Determinism: percentile uses nearest-rank on a sorted copy; reports
// contain no map-iteration-dependent output — identical inputs produce
// identical reports (pinned by tests).

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// RunInputsKey pins the exact versioned inputs a run executed with.
// Two runs are comparable ONLY when every field matches.
type RunInputsKey struct {
	JourneyID      string
	JourneyVersion int
	PersonaID      string
	PersonaVersion int
	EnvironmentID  string
	FixtureSet     string // fixture set ref (workers stamp into step-event RedactedMeta)
	AssertionSet   string // assertion set ref (same stamping convention)
}

// pinKey renders the canonical comparison key.
func (v RunInputsKey) pinKey() string {
	return fmt.Sprintf("%s@%d|%s@%d|%s|%s|%s",
		v.JourneyID, v.JourneyVersion, v.PersonaID, v.PersonaVersion,
		v.EnvironmentID, v.FixtureSet, v.AssertionSet)
}

// RunInputs extracts the RunInputsKey a run recorded. Fixture and
// assertion set references are read from the first non-empty
// RedactedMeta["fixture_set"] / ["assertion_set"] step-event stamp.
func RunInputs(r Run) RunInputsKey {
	in := RunInputsKey{
		JourneyID:      r.JourneyID,
		JourneyVersion: r.JourneyVer,
		PersonaID:      r.PersonaID,
		PersonaVersion: r.PersonaVer,
		EnvironmentID:  r.EnvironmentID,
	}
	for _, e := range r.StepEvents {
		if in.FixtureSet == "" {
			in.FixtureSet = e.RedactedMeta["fixture_set"]
		}
		if in.AssertionSet == "" {
			in.AssertionSet = e.RedactedMeta["assertion_set"]
		}
		if in.FixtureSet != "" && in.AssertionSet != "" {
			break
		}
	}
	return in
}

// sideInputs pins one comparison side: the shared inputs plus per-run drift
// entries for runs that do not match them.
func sideInputs(runs []Run, side string, drift *[]string) RunInputsKey {
	base := RunInputs(runs[0])
	baseKey := base.pinKey()
	for _, r := range runs[1:] {
		if k := RunInputs(r).pinKey(); k != baseKey {
			*drift = append(*drift, fmt.Sprintf("%s run %s inputs %q differ from pinned %q", side, r.ID, k, baseKey))
		}
	}
	return base
}

// ComparisonThresholds are the meaningful-difference thresholds
// (documented on DefaultComparisonThresholds).
type ComparisonThresholds struct {
	CompletionDeltaPP     float64 // percentage points
	LatencyP50RelPct      float64 // relative % (baseline > 0)
	LatencyP95RelPct      float64 // relative % (baseline > 0)
	AssertionPassDeltaPP  float64 // percentage points
	ConsoleErrorPerRunAbs float64 // errors per run, absolute
}

// DefaultComparisonThresholds are the documented meaningful-difference
// thresholds: 5pp completion, 20%/30% p50/p95 latency, 2pp assertions,
// 0.5 console errors per run.
var DefaultComparisonThresholds = ComparisonThresholds{
	CompletionDeltaPP:     5,
	LatencyP50RelPct:      20,
	LatencyP95RelPct:      30,
	AssertionPassDeltaPP:  2,
	ConsoleErrorPerRunAbs: 0.5,
}

// LatencyDelta compares run-duration percentiles (milliseconds).
type LatencyDelta struct {
	StableP50MS    float64
	StableP95MS    float64
	CandidateP50MS float64
	CandidateP95MS float64
	// P50DeltaPct/P95DeltaPct are relative deltas
	// (candidate − stable)/stable × 100; inapplicable (0) on a zero baseline.
	P50DeltaPct float64
	P95DeltaPct float64
}

// ComparisonReport is the release-version comparison verdict.
type ComparisonReport struct {
	StableVersion    string
	CandidateVersion string
	StableRuns       int
	CandidateRuns    int

	StableCompletionPct    float64
	CandidateCompletionPct float64
	// JourneyCompletionDeltaPP = candidate − stable (percentage points).
	JourneyCompletionDeltaPP float64

	LatencyDelta LatencyDelta

	StableAssertionPassPct    float64
	CandidateAssertionPassPct float64
	AssertionPassDeltaPP      float64

	StableConsoleErrorsPerRun    float64
	CandidateConsoleErrorsPerRun float64
	ConsoleErrorDelta            float64

	// VersionDrift lists every input/version mismatch that makes the
	// comparison non-evidence (non-empty forces Meaningful=false).
	VersionDrift []string
	// Meaningful is true when at least one documented threshold is breached
	// AND there is no version drift.
	Meaningful bool
	// Reasons lists the breached thresholds in deterministic order with
	// direction (regression/improvement).
	Reasons    []string
	Thresholds ComparisonThresholds
}

// RunComparison compares stable vs candidate runs under the documented
// default thresholds. Errors (ErrInvalid) only for empty sides; version
// drift is DATA (VersionDrift + Meaningful=false), never silently ignored.
func RunComparison(stable, candidate []Run) (ComparisonReport, error) {
	return RunComparisonWithThresholds(stable, candidate, DefaultComparisonThresholds)
}

// RunComparisonWithThresholds is RunComparison with explicit thresholds.
func RunComparisonWithThresholds(stable, candidate []Run, th ComparisonThresholds) (ComparisonReport, error) {
	if len(stable) == 0 || len(candidate) == 0 {
		return ComparisonReport{}, fmt.Errorf("%w: both stable and candidate runs are required", ErrInvalid)
	}
	rep := ComparisonReport{Thresholds: th}

	var drift []string
	stableIn := sideInputs(stable, "stable", &drift)
	candIn := sideInputs(candidate, "candidate", &drift)
	if stableIn.pinKey() != candIn.pinKey() {
		drift = append(drift, fmt.Sprintf("pinned inputs differ: stable %q vs candidate %q", stableIn.pinKey(), candIn.pinKey()))
	}
	// Version consistency within and across sides.
	stableVersion := versionOf(stable, "stable", &drift)
	candVersion := versionOf(candidate, "candidate", &drift)

	sm := aggregateSideMetrics(stable)
	cm := aggregateSideMetrics(candidate)

	rep.StableVersion, rep.CandidateVersion = stableVersion, candVersion
	rep.StableRuns, rep.CandidateRuns = sm.runs, cm.runs
	rep.StableCompletionPct, rep.CandidateCompletionPct = sm.completionPct, cm.completionPct
	rep.JourneyCompletionDeltaPP = cm.completionPct - sm.completionPct
	rep.LatencyDelta = LatencyDelta{
		StableP50MS:    sm.p50,
		StableP95MS:    sm.p95,
		CandidateP50MS: cm.p50,
		CandidateP95MS: cm.p95,
	}
	if sm.p50 > 0 {
		rep.LatencyDelta.P50DeltaPct = (cm.p50 - sm.p50) / sm.p50 * 100
	}
	if sm.p95 > 0 {
		rep.LatencyDelta.P95DeltaPct = (cm.p95 - sm.p95) / sm.p95 * 100
	}
	rep.StableAssertionPassPct, rep.CandidateAssertionPassPct = sm.assertionPct, cm.assertionPct
	rep.AssertionPassDeltaPP = cm.assertionPct - sm.assertionPct
	rep.StableConsoleErrorsPerRun, rep.CandidateConsoleErrorsPerRun = sm.consolePerRun, cm.consolePerRun
	rep.ConsoleErrorDelta = cm.consolePerRun - sm.consolePerRun
	rep.VersionDrift = drift

	// Meaningful-difference evaluation (deterministic order).
	abs := func(f float64) float64 { return math.Abs(f) }
	if abs(rep.JourneyCompletionDeltaPP) >= th.CompletionDeltaPP {
		rep.Reasons = append(rep.Reasons, fmt.Sprintf("journey completion delta %.2fpp exceeds %.2fpp (%s)",
			rep.JourneyCompletionDeltaPP, th.CompletionDeltaPP, direction(rep.JourneyCompletionDeltaPP < 0)))
	}
	if sm.p50 > 0 && abs(rep.LatencyDelta.P50DeltaPct) >= th.LatencyP50RelPct {
		rep.Reasons = append(rep.Reasons, fmt.Sprintf("p50 latency delta %.1f%% exceeds %.1f%% (%s)",
			rep.LatencyDelta.P50DeltaPct, th.LatencyP50RelPct, direction(rep.LatencyDelta.P50DeltaPct > 0)))
	}
	if sm.p95 > 0 && abs(rep.LatencyDelta.P95DeltaPct) >= th.LatencyP95RelPct {
		rep.Reasons = append(rep.Reasons, fmt.Sprintf("p95 latency delta %.1f%% exceeds %.1f%% (%s)",
			rep.LatencyDelta.P95DeltaPct, th.LatencyP95RelPct, direction(rep.LatencyDelta.P95DeltaPct > 0)))
	}
	if abs(rep.AssertionPassDeltaPP) >= th.AssertionPassDeltaPP {
		rep.Reasons = append(rep.Reasons, fmt.Sprintf("assertion pass delta %.2fpp exceeds %.2fpp (%s)",
			rep.AssertionPassDeltaPP, th.AssertionPassDeltaPP, direction(rep.AssertionPassDeltaPP < 0)))
	}
	if abs(rep.ConsoleErrorDelta) >= th.ConsoleErrorPerRunAbs {
		rep.Reasons = append(rep.Reasons, fmt.Sprintf("console errors per run delta %.2f exceeds %.2f (%s)",
			rep.ConsoleErrorDelta, th.ConsoleErrorPerRunAbs, direction(rep.ConsoleErrorDelta > 0)))
	}
	rep.Meaningful = len(rep.Reasons) > 0 && len(drift) == 0
	return rep, nil
}

// direction annotates a delta: worse = regression.
func direction(worse bool) string {
	if worse {
		return "regression"
	}
	return "improvement"
}

// versionOf checks AppVersion consistency within one side; mixed or empty
// versions are drift (the first run's version is reported).
func versionOf(runs []Run, side string, drift *[]string) string {
	v := runs[0].AppVersion
	for _, r := range runs[1:] {
		if r.AppVersion != v {
			*drift = append(*drift, fmt.Sprintf("%s side mixes AppVersion %q and %q", side, v, r.AppVersion))
		}
	}
	if strings.TrimSpace(v) == "" {
		*drift = append(*drift, fmt.Sprintf("%s runs carry no AppVersion", side))
	}
	return v
}

// aggregateSideMetrics aggregates one comparison side. Completion counts
// RunCompleted (inconclusive runs count as not completed — explicitly NOT
// pass/fail per #148); latency uses completed wall-clock durations
// (FinishedAt−StartedAt, runs missing either endpoint are excluded);
// assertion pass rate counts step events of type "assertion"; console
// errors are step events of type "console" with status "failed".
type sideMetrics struct {
	runs          int
	completed     int
	completionPct float64
	p50           float64
	p95           float64
	assertionPct  float64
	consolePerRun float64
}

func aggregateSideMetrics(runs []Run) sideMetrics {
	m := sideMetrics{runs: len(runs)}
	var durs []float64
	assertTotal, assertPass := 0, 0
	var console float64
	for _, r := range runs {
		if r.Status == RunCompleted {
			m.completed++
		}
		if r.StartedAt != nil && r.FinishedAt != nil && !r.FinishedAt.Before(*r.StartedAt) {
			durs = append(durs, r.FinishedAt.Sub(*r.StartedAt).Seconds()*1000)
		}
		for _, e := range r.StepEvents {
			switch {
			case e.Type == "assertion":
				assertTotal++
				if e.Status == "passed" {
					assertPass++
				}
			case e.Type == "console" && e.Status == "failed":
				console++
			}
		}
	}
	if m.runs > 0 {
		m.completionPct = float64(m.completed) / float64(m.runs) * 100
		m.consolePerRun = console / float64(m.runs)
	}
	if assertTotal > 0 {
		m.assertionPct = float64(assertPass) / float64(assertTotal) * 100
	}
	m.p50 = percentile(durs, 50)
	m.p95 = percentile(durs, 95)
	return m
}

// percentile computes the nearest-rank percentile of vals (0 for empty).
// Deterministic: sorts a copy; rank = ceil(p/100 × n), clamped to [1, n].
func percentile(vals []float64, p float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	s := append([]float64(nil), vals...)
	sort.Float64s(s)
	rank := int(math.Ceil(p / 100 * float64(len(s))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(s) {
		rank = len(s)
	}
	return s[rank-1]
}
