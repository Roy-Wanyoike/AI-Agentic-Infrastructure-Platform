package synth

// compare_test.go — issue #156 tests: release-version comparison delta
// math (completion, p50/p95 latency, assertion pass, console errors),
// threshold breaches + direction annotation, version-drift handling
// (drift forces Meaningful=false even on breached thresholds) and
// DETERMINISM — identical inputs produce byte-identical reports. All
// clock-free: runs carry fixed StartedAt/FinishedAt instants.

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// cmpRun builds a Run with pinned inputs and fixed start/finish instants.
func cmpRun(id, version string, status RunStatus, start, end time.Time, events ...StepEvent) Run {
	s, f := start, end
	return Run{
		ID:            id,
		OrgID:         "org-1",
		ProjectID:     "proj-1",
		JourneyID:     "j-1",
		JourneyVer:    3,
		EnvironmentID: "env-p",
		PersonaID:     "per-1",
		PersonaVer:    2,
		Status:        status,
		AppVersion:    version,
		StartedAt:     &s,
		FinishedAt:    &f,
		StepEvents:    events,
	}
}

func stampEvent(kind, status string, meta map[string]string) StepEvent {
	return StepEvent{Type: kind, Status: status, RedactedMeta: meta}
}

func TestRunComparisonDeltaMath(t *testing.T) {
	// Stable: 2 completed runs, latencies 1000ms/2000ms → p50=1000, p95=2000
	// (nearest rank). Candidate: 2 completed runs, 1500ms/3000ms.
	// Assertion pass: stable 1/2 = 50%, candidate 4/4 = 100% (+50pp).
	// Console errors: stable 1 across 2 runs = 0.5/run, candidate 0 (−0.5).
	base := time.Date(2025, 6, 2, 9, 0, 0, 0, time.UTC)
	stable := []Run{
		cmpRun("r1", "v1", RunCompleted, base, base.Add(1000*time.Millisecond),
			stampEvent("assertion", "failed", nil), stampEvent("console", "failed", nil)),
		cmpRun("r2", "v1", RunCompleted, base, base.Add(2000*time.Millisecond),
			stampEvent("assertion", "passed", nil)),
	}
	candidate := []Run{
		cmpRun("r3", "v2", RunCompleted, base, base.Add(1500*time.Millisecond),
			stampEvent("assertion", "passed", nil), stampEvent("assertion", "passed", nil)),
		cmpRun("r4", "v2", RunCompleted, base, base.Add(3000*time.Millisecond),
			stampEvent("assertion", "passed", nil), stampEvent("assertion", "passed", nil)),
	}
	rep, err := RunComparison(stable, candidate)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}

	if rep.StableVersion != "v1" || rep.CandidateVersion != "v2" {
		t.Fatalf("versions wrong: %q vs %q", rep.StableVersion, rep.CandidateVersion)
	}
	if rep.StableRuns != 2 || rep.CandidateRuns != 2 {
		t.Fatalf("run counts wrong: %d/%d", rep.StableRuns, rep.CandidateRuns)
	}
	if rep.StableCompletionPct != 100 || rep.CandidateCompletionPct != 100 || rep.JourneyCompletionDeltaPP != 0 {
		t.Fatalf("completion math wrong: %v vs %v (delta %v)",
			rep.StableCompletionPct, rep.CandidateCompletionPct, rep.JourneyCompletionDeltaPP)
	}
	ld := rep.LatencyDelta
	if ld.StableP50MS != 1000 || ld.StableP95MS != 2000 || ld.CandidateP50MS != 1500 || ld.CandidateP95MS != 3000 {
		t.Fatalf("percentiles wrong (nearest-rank): %+v", ld)
	}
	if ld.P50DeltaPct != 50 || ld.P95DeltaPct != 50 {
		t.Fatalf("relative latency deltas wrong: p50=%v p95=%v", ld.P50DeltaPct, ld.P95DeltaPct)
	}
	if rep.StableAssertionPassPct != 50 || rep.CandidateAssertionPassPct != 100 || rep.AssertionPassDeltaPP != 50 {
		t.Fatalf("assertion math wrong: %v -> %v (delta %v)",
			rep.StableAssertionPassPct, rep.CandidateAssertionPassPct, rep.AssertionPassDeltaPP)
	}
	if rep.StableConsoleErrorsPerRun != 0.5 || rep.CandidateConsoleErrorsPerRun != 0 || rep.ConsoleErrorDelta != -0.5 {
		t.Fatalf("console math wrong: %v -> %v (delta %v)",
			rep.StableConsoleErrorsPerRun, rep.CandidateConsoleErrorsPerRun, rep.ConsoleErrorDelta)
	}
	if len(rep.VersionDrift) != 0 {
		t.Fatalf("no drift expected, got %v", rep.VersionDrift)
	}
	// Breached thresholds: p50 50%>20%, p95 50%>30%, assertions 50pp>2pp,
	// console |−0.5|>=0.5 — completion delta 0pp is NOT a reason.
	want := []string{
		"p50 latency delta 50.0% exceeds 20.0% (regression)",
		"p95 latency delta 50.0% exceeds 30.0% (regression)",
		"assertion pass delta 50.00pp exceeds 2.00pp (improvement)",
		"console errors per run delta -0.50 exceeds 0.50 (improvement)",
	}
	if fmt.Sprint(rep.Reasons) != fmt.Sprint(want) {
		t.Fatalf("reasons wrong:\n got %v\nwant %v", rep.Reasons, want)
	}
	if !rep.Meaningful {
		t.Fatal("breached thresholds without drift must be meaningful")
	}
	if rep.Thresholds != DefaultComparisonThresholds {
		t.Fatal("default thresholds not carried on the report")
	}
}

func TestRunComparisonInconclusiveNotCompleted(t *testing.T) {
	// Inconclusive runs count as NOT completed (#148: explicitly not pass/fail).
	base := time.Date(2025, 6, 2, 9, 0, 0, 0, time.UTC)
	stable := []Run{cmpRun("s1", "v1", RunCompleted, base, base.Add(time.Second))}
	candidate := []Run{
		cmpRun("c1", "v2", RunInconclusive, base, base.Add(time.Second)),
		cmpRun("c2", "v2", RunInconclusive, base, base.Add(time.Second)),
	}
	rep, err := RunComparison(stable, candidate)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if rep.CandidateCompletionPct != 0 || rep.JourneyCompletionDeltaPP != -100 {
		t.Fatalf("inconclusive must not count as completion: %+v", rep)
	}
}

func TestRunComparisonVersionDriftForcesNotMeaningful(t *testing.T) {
	base := time.Date(2025, 6, 2, 9, 0, 0, 0, time.UTC)
	// Journey-version mismatch across sides.
	stable := []Run{cmpRun("s1", "v1", RunCompleted, base, base.Add(time.Second))}
	drifted := cmpRun("c1", "v2", RunCompleted, base, base.Add(10*time.Second))
	drifted.JourneyVer = 9 // different journey version → not the same input
	rep, err := RunComparison(stable, []Run{drifted})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if len(rep.VersionDrift) == 0 {
		t.Fatal("journey-version mismatch must be reported as drift")
	}
	if rep.Meaningful {
		t.Fatal("drift must force Meaningful=false even with breached thresholds")
	}

	// Mixed AppVersion within one side is drift too.
	mixed := []Run{cmpRun("c1", "v2", RunCompleted, base, base.Add(time.Second)), cmpRun("c2", "v3", RunCompleted, base, base.Add(time.Second))}
	if rep, err = RunComparison(stable, mixed); err != nil {
		t.Fatalf("compare: %v", err)
	}
	if len(rep.VersionDrift) == 0 || rep.Meaningful {
		t.Fatalf("mixed candidate versions must be drift: %v meaningful=%v", rep.VersionDrift, rep.Meaningful)
	}

	// Missing AppVersion anywhere is drift (evidence must pin its build).
	noVersion := cmpRun("c1", "", RunCompleted, base, base.Add(time.Second))
	if rep, err = RunComparison(stable, []Run{noVersion}); err != nil {
		t.Fatalf("compare: %v", err)
	}
	if len(rep.VersionDrift) == 0 || rep.Meaningful {
		t.Fatalf("missing AppVersion must be drift: %v meaningful=%v", rep.VersionDrift, rep.Meaningful)
	}
}

func TestRunComparisonDeterminism(t *testing.T) {
	base := time.Date(2025, 6, 2, 9, 0, 0, 0, time.UTC)
	mkStable := func() []Run {
		return []Run{
			cmpRun("s1", "v1", RunCompleted, base, base.Add(1200*time.Millisecond),
				stampEvent("assertion", "passed", nil), stampEvent("console", "failed", nil)),
			cmpRun("s2", "v1", RunCompleted, base, base.Add(2200*time.Millisecond),
				stampEvent("assertion", "failed", nil)),
			cmpRun("s3", "v1", RunFailed, base, base.Add(900*time.Millisecond)),
		}
	}
	mkCandidate := func() []Run {
		return []Run{
			cmpRun("c1", "v2", RunCompleted, base, base.Add(1800*time.Millisecond),
				stampEvent("assertion", "passed", nil)),
			cmpRun("c2", "v2", RunCompleted, base, base.Add(2600*time.Millisecond),
				stampEvent("assertion", "passed", nil), stampEvent("console", "failed", nil)),
		}
	}
	a, err := RunComparison(mkStable(), mkCandidate())
	if err != nil {
		t.Fatalf("compare a: %v", err)
	}
	for i := 0; i < 50; i++ {
		b, err := RunComparison(mkStable(), mkCandidate())
		if err != nil {
			t.Fatalf("compare b: %v", err)
		}
		if fmt.Sprintf("%#v", a) != fmt.Sprintf("%#v", b) {
			t.Fatalf("iteration %d: identical inputs must produce identical reports", i)
		}
	}
	// Reordering runs within a side must not change aggregate metrics
	// (percentiles sort a copy; counts are order-independent).
	flip, err := RunComparison([]Run{mkStable()[2], mkStable()[0], mkStable()[1]}, mkCandidate())
	if err != nil {
		t.Fatalf("compare flip: %v", err)
	}
	if fmt.Sprintf("%#v", a) != fmt.Sprintf("%#v", flip) {
		t.Fatal("run order within a side changed the report — nondeterministic")
	}
}

func TestRunComparisonZeroBaselineLatency(t *testing.T) {
	// Zero baseline makes relative latency deltas inapplicable (0), never a
	// fake 100%-improvement; the absolute report still carries both sides.
	base := time.Date(2025, 6, 2, 9, 0, 0, 0, time.UTC)
	// A zero-length interval satisfies !FinishedAt.Before(StartedAt), so the
	// stable p50 baseline is 0 → relative latency checks are inapplicable.
	stable2 := []Run{cmpRun("s2", "v1", RunCompleted, base, base)}
	candidate := []Run{cmpRun("c1", "v2", RunCompleted, base, base.Add(5*time.Second))}
	rep, err := RunComparisonWithThresholds(stable2, candidate, DefaultComparisonThresholds)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if rep.LatencyDelta.StableP50MS != 0 || rep.LatencyDelta.StableP95MS != 0 {
		t.Fatalf("expected zero stable baseline: %+v", rep.LatencyDelta)
	}
	if rep.LatencyDelta.P50DeltaPct != 0 || rep.LatencyDelta.P95DeltaPct != 0 {
		t.Fatalf("zero baseline must give no relative delta: %+v", rep.LatencyDelta)
	}
	for _, r := range rep.Reasons {
		if len(r) >= 4 && (r[:4] == "p50 " || r[:4] == "p95 ") {
			t.Fatalf("relative latency reasons must be skipped on zero baseline: %q", r)
		}
	}
}

func TestRunComparisonEmptySides(t *testing.T) {
	base := time.Date(2025, 6, 2, 9, 0, 0, 0, time.UTC)
	one := []Run{cmpRun("s1", "v1", RunCompleted, base, base.Add(time.Second))}
	if _, err := RunComparison(nil, one); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty stable must be ErrInvalid, got %v", err)
	}
	if _, err := RunComparison(one, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty candidate must be ErrInvalid, got %v", err)
	}
}

func TestRunInputsStampExtraction(t *testing.T) {
	base := time.Date(2025, 6, 2, 9, 0, 0, 0, time.UTC)
	r := cmpRun("r1", "v1", RunCompleted, base, base.Add(time.Second),
		stampEvent("action", "passed", nil),
		stampEvent("assertion", "passed", map[string]string{"fixture_set": "fx-7", "assertion_set": "as-2"}),
		stampEvent("console", "failed", map[string]string{"fixture_set": "fx-9"}), // later stamps ignored
	)
	in := RunInputs(r)
	if in.JourneyID != "j-1" || in.JourneyVersion != 3 || in.PersonaID != "per-1" || in.PersonaVersion != 2 || in.EnvironmentID != "env-p" {
		t.Fatalf("identity inputs wrong: %+v", in)
	}
	if in.FixtureSet != "fx-7" || in.AssertionSet != "as-2" {
		t.Fatalf("stamp extraction wrong: %+v", in)
	}
	// pinKey pins every dimension — a change in any one is visible.
	full := in.pinKey()
	other := in
	other.AssertionSet = "as-3"
	if other.pinKey() == full {
		t.Fatal("assertion-set change must change the pin key")
	}
	other = in
	other.PersonaVersion = 1
	if other.pinKey() == full {
		t.Fatal("persona-version change must change the pin key")
	}
}

func TestPercentileNearestRank(t *testing.T) {
	vals := []float64{4000, 1000, 3000, 2000} // unsorted on purpose
	if got := percentile(vals, 50); got != 2000 {
		t.Fatalf("p50 = %v, want 2000 (nearest-rank on sorted copy)", got)
	}
	if got := percentile(vals, 95); got != 4000 {
		t.Fatalf("p95 = %v, want 4000", got)
	}
	if got := percentile(vals, 0); got != 1000 {
		t.Fatalf("p0 clamps to the minimum, got %v", got)
	}
	if got := percentile(vals, 100); got != 4000 {
		t.Fatalf("p100 clamps to the maximum, got %v", got)
	}
	if got := percentile(nil, 50); got != 0 {
		t.Fatalf("empty percentile = %v, want 0", got)
	}
	// Input slice must not be mutated.
	if vals[0] != 4000 || vals[1] != 1000 {
		t.Fatalf("percentile mutated its input: %v", vals)
	}
}
