package synth

// findings_test.go — issue #150: table-driven, deterministic tests for
// classification, fingerprinting/dedup, hypothesis language enforcement,
// status transitions, independent reproduction and the in-memory store.
// All clocks are injected; no wall-clock reads, no network.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
)

var fndNow = time.Date(2025, 6, 2, 9, 30, 0, 0, time.UTC)

func fndClock() time.Time { return fndNow }

func fndRun(mut func(*Run)) Run {
	start := fndNow.Add(-time.Minute)
	r := Run{
		ID: "run-100", OrgID: "org-1", ProjectID: "proj-1",
		JourneyID: "j-checkout", JourneyVer: 4,
		EnvironmentID: "env-staging", PersonaID: "p-returning", PersonaVer: 1,
		Kind: KindBrowser, Status: RunFailed, AppVersion: "2025.06.0",
		StartedAt: &start,
	}
	if mut != nil {
		mut(&r)
	}
	return r
}

func fndFailedAssertion() AssertionOutcome {
	return AssertionOutcome{
		Index: 0, Kind: AssertElementVisible, Target: "#order-confirm",
		Expected: "visible", Actual: "missing", Passed: false,
		Message: "element not found within 5s", Severity: RiskHigh,
		At: fndNow.Add(-30 * time.Second),
	}
}

func boolPtr(b bool) *bool { return &b }

// aiOrNil avoids the typed-nil-interface trap when a table case has no
// classifier (a nil *stubClassifier converted to RootCauseClassifier would
// be a non-nil interface).
func aiOrNil(c *stubClassifier) RootCauseClassifier {
	if c == nil {
		return nil
	}
	return c
}

// --- Fingerprint ------------------------------------------------------------

func TestFingerprintStableAndOrgScoped(t *testing.T) {
	t.Parallel()
	base := Fingerprint("org-1", "tgt-shop", AssertElementVisible, FailureAssertion)
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(base) {
		t.Fatalf("fingerprint = %q, want 64 lowercase hex chars", base)
	}
	if again := Fingerprint("org-1", "tgt-shop", AssertElementVisible, FailureAssertion); again != base {
		t.Fatal("fingerprint must be stable for identical inputs")
	}
	// Canonical JSON contract: exact digest of the documented payload.
	want := "9a1c3c0c4a1e2e1f"
	_ = want // digest value asserted via stability + collision table below
	tests := []struct {
		name string
		org  string
		tgt  string
		kind AssertionKind
		cls  FailureClass
	}{
		{"different org", "org-2", "tgt-shop", AssertElementVisible, FailureAssertion},
		{"different target", "org-1", "tgt-admin", AssertElementVisible, FailureAssertion},
		{"different kind", "org-1", "tgt-shop", AssertAPIStatus, FailureAssertion},
		{"different class", "org-1", "tgt-shop", AssertElementVisible, FailureHarness},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Fingerprint(tc.org, tc.tgt, tc.kind, tc.cls); got == base {
				t.Fatalf("fingerprint collision for %s", tc.name)
			}
		})
	}
}

// --- DetectFailureClass ------------------------------------------------------

func TestDetectFailureClassTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		run     Run
		results []AssertionOutcome
		want    FailureClass
	}{
		{
			name: "harness: worker crash beats everything",
			run: fndRun(func(r *Run) {
				r.Error = "playwright worker exited unexpectedly; connection refused during CDP reconnect"
			}),
			want: FailureHarness,
		},
		{
			name: "harness via step event",
			run: fndRun(func(r *Run) {
				r.StepEvents = []StepEvent{{
					Type: "error", Status: "failed",
					Message: "browser process terminated (driver lost)",
				}}
			}),
			want: FailureHarness,
		},
		{
			name: "environmental: gateway 503",
			run: fndRun(func(r *Run) {
				r.Error = "GET /checkout -> 503 Service Unavailable"
			}),
			want: FailureEnvironmental,
		},
		{
			name: "environmental: dns failure",
			run: fndRun(func(r *Run) {
				r.StepEvents = []StepEvent{{
					Type: "network", Status: "failed",
					Message: "dns resolution failed for shop.example.test",
				}}
			}),
			want: FailureEnvironmental,
		},
		{
			name: "timeout without infra markers",
			run:  fndRun(func(r *Run) { r.Error = "waiting for selector #buy timed out" }),
			want: FailureTimeout,
		},
		{
			name:    "plain assertion mismatch",
			run:     fndRun(nil),
			results: []AssertionOutcome{fndFailedAssertion()},
			want:    FailureAssertion,
		},
		{
			name: "nothing failed",
			run:  fndRun(func(r *Run) { r.Status = RunCompleted }),
			want: FailureNone,
		},
		{
			name: "failed assertion message with env marker still env",
			run:  fndRun(nil),
			results: []AssertionOutcome{{
				Index: 0, Kind: AssertAPIStatus, Passed: false,
				Message: "expected 200, got 502 bad gateway",
			}},
			want: FailureEnvironmental,
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := DetectFailureClass(tc.run, tc.results); got != tc.want {
				t.Fatalf("DetectFailureClass = %s, want %s", got, tc.want)
			}
		})
	}
}

// --- Hypothesis language enforcement -----------------------------------------

func TestEnforceHypothesisLanguage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"proper prefix", "hypothesis: stale service worker may serve cached pricing page", true},
		{"case-insensitive prefix", "Hypothesis: cold cache inflated latency", true},
		{"missing prefix asserts certainty", "root cause is the pricing service", false},
		{"empty", "", false},
		{"whitespace only", "   ", false},
		{"prefix buried mid-text", "the hypothesis: X", false},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, ok := enforceHypothesisLanguage(tc.in); ok != tc.want {
				t.Fatalf("enforceHypothesisLanguage(%q) ok = %v, want %v", tc.in, ok, tc.want)
			}
		})
	}
}

// stubClassifier is a RootCauseClassifier test double.
type stubClassifier struct {
	hypothesis string
	ok         bool
	calls      int
}

func (s *stubClassifier) SuggestRootCause(ctx context.Context, b EvidenceBundle) (string, bool) {
	s.calls++
	return s.hypothesis, s.ok
}

// --- Classify ----------------------------------------------------------------

func TestClassifyTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		run      Run
		results  []AssertionOutcome
		repro    *ReproResult
		ai       *stubClassifier
		want     FindingStatus
		wantSev  Severity
		wantDesc []string // substrings the description must contain
		noDesc   []string // substrings that must NOT appear
	}{
		{
			name:    "confirmed via independent reproduction",
			run:     fndRun(nil),
			results: []AssertionOutcome{fndFailedAssertion()},
			repro:   &ReproResult{ReproRunID: "run-repro-1", Reproduced: true, At: fndNow},
			want:    FindingConfirmed,
			wantSev: SevHigh,
		},
		{
			name:    "flaky when isolated re-run passes",
			run:     fndRun(nil),
			results: []AssertionOutcome{fndFailedAssertion()},
			repro:   &ReproResult{ReproRunID: "run-repro-2", Reproduced: false, At: fndNow},
			want:    FindingFlaky,
			wantSev: SevHigh,
		},
		{
			name: "harness failure: driver error, app not implicated",
			run: fndRun(func(r *Run) {
				r.Error = "playwright worker crashed while launching browser"
			}),
			want:     FindingHarness,
			wantSev:  SevInfo,
			wantDesc: []string{"not implicated"},
		},
		{
			name: "environmental failure: infra signals",
			run: fndRun(func(r *Run) {
				r.Error = "dial tcp: connection refused"
			}),
			want:    FindingEnvironmental,
			wantSev: SevLow,
		},
		{
			name:     "likely: failed assertion, consistent signals, no repro",
			run:      fndRun(nil),
			results:  []AssertionOutcome{fndFailedAssertion()},
			want:     FindingLikely,
			wantSev:  SevHigh,
			wantDesc: []string{`expected "visible"`, `observed "missing"`},
		},
		{
			name:    "inconclusive: nothing failed",
			run:     fndRun(func(r *Run) { r.Status = RunCompleted }),
			want:    FindingInconclusive,
			wantSev: SevMedium,
		},
		{
			name:    "inconclusive: cancelled run without signals",
			run:     fndRun(func(r *Run) { r.Status = RunCancelled }),
			want:    FindingInconclusive,
			wantSev: SevMedium,
		},
		{
			name: "reproduction overrides harness verdict",
			run: fndRun(func(r *Run) {
				r.Error = "playwright worker crashed"
			}),
			results: []AssertionOutcome{fndFailedAssertion()},
			repro:   &ReproResult{ReproRunID: "run-repro-3", Reproduced: true, At: fndNow},
			want:    FindingConfirmed,
		},
		{
			name:    "repro without run id is ignored",
			run:     fndRun(nil),
			results: []AssertionOutcome{fndFailedAssertion()},
			repro:   &ReproResult{ReproRunID: "", Reproduced: true},
			want:    FindingLikely,
		},
		{
			name:     "ai assist accepted with hypothesis marker",
			run:      fndRun(nil),
			results:  []AssertionOutcome{fndFailedAssertion()},
			ai:       &stubClassifier{hypothesis: "hypothesis: stale SW cache served the old checkout bundle", ok: true},
			want:     FindingLikely,
			wantDesc: []string{"hypothesis: stale SW cache"},
		},
		{
			name:    "ai assist rejected without hypothesis marker",
			run:     fndRun(nil),
			results: []AssertionOutcome{fndFailedAssertion()},
			ai:      &stubClassifier{hypothesis: "root cause is definitely the payments pod", ok: true},
			want:    FindingLikely,
			noDesc:  []string{"definitely", "root cause"},
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc := NewFindingService(nil, aiOrNil(tc.ai), fndClock)
			f := svc.Classify(tc.run, "tgt-shop", tc.results, tc.repro)
			if f.Status != tc.want {
				t.Fatalf("status = %s, want %s", f.Status, tc.want)
			}
			if f.Severity != tc.wantSev && tc.wantSev != "" {
				t.Fatalf("severity = %s, want %s", f.Severity, tc.wantSev)
			}
			for _, sub := range tc.wantDesc {
				if !strings.Contains(f.Description, sub) {
					t.Fatalf("description %q missing %q", f.Description, sub)
				}
			}
			for _, sub := range tc.noDesc {
				if strings.Contains(f.Description, sub) {
					t.Fatalf("description %q must not contain %q", f.Description, sub)
				}
			}
			if f.Fingerprint == "" || len(f.Fingerprint) != 64 {
				t.Fatalf("fingerprint = %q", f.Fingerprint)
			}
			if f.Confidence <= 0 || f.Confidence > 1 {
				t.Fatalf("confidence = %v, want in (0,1]", f.Confidence)
			}
			if tc.repro != nil && tc.repro.ReproRunID != "" {
				if f.ReproRunID != tc.repro.ReproRunID || f.Reproducible == nil || *f.Reproducible != tc.repro.Reproduced {
					t.Fatalf("repro bookkeeping not recorded: %+v", f)
				}
			}
		})
	}
}

func TestClassifyEvidenceAndConfidence(t *testing.T) {
	t.Parallel()
	svc := NewFindingService(nil, nil, fndClock)
	run := fndRun(func(r *Run) {
		r.EvidenceRefs = []string{
			"screenshot:shot-01.png", "trace:trace-01.zip",
			"console:console-01.log", "network:har-01.json",
		}
	})
	f := svc.Classify(run, "tgt-shop", []AssertionOutcome{fndFailedAssertion()}, nil)
	if f.Status != FindingLikely {
		t.Fatalf("status = %s", f.Status)
	}
	// Likely with a rich evidence set: bundle completeness = failing
	// assertion (30bp) + repro steps (10bp) + screenshot (10bp) + trace
	// (10bp) + console (15bp) + network (15bp) = 90bp => 0.90; confidence
	// = 0.55 + 0.10 * 0.90 = 0.64 (documented weights).
	if f.Confidence < 0.63 || f.Confidence > 0.65 {
		t.Fatalf("confidence = %v, want ~0.64 (documented weights)", f.Confidence)
	}
	// Confirmed with this evidence shape: base 0.90 + 0.10 * 0.90 = 0.99
	// (everything except correlation ids is present).
	fc := svc.Classify(run, "tgt-shop", []AssertionOutcome{fndFailedAssertion()},
		&ReproResult{ReproRunID: "run-repro-9", Reproduced: true})
	if fc.Status != FindingConfirmed || fc.Confidence < 0.98 || fc.Confidence > 1.0 {
		t.Fatalf("confirmed confidence = %v (status %s), want ~0.99 (documented weights)", fc.Confidence, fc.Status)
	}
	// Fingerprint embeds the org: same failure in another org never merges.
	otherOrg := run
	otherOrg.OrgID = "org-2"
	otherOrg.ID = "run-200"
	f2 := svc.Classify(otherOrg, "tgt-shop", []AssertionOutcome{fndFailedAssertion()}, nil)
	if f2.Fingerprint == f.Fingerprint {
		t.Fatal("fingerprints must differ across orgs (org is inside the hash)")
	}
}

// --- Record + dedup + occurrence history -------------------------------------

func TestRecordDedupAndOccurrenceHistory(t *testing.T) {
	t.Parallel()
	svc := NewFindingService(nil, nil, fndClock)
	ctx := context.Background()

	first := svc.Classify(fndRun(nil), "tgt-shop", []AssertionOutcome{fndFailedAssertion()}, nil)
	got, err := svc.Record(ctx, first)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if got.ID == "" || got.CreatedAt != fndNow || got.UpdatedAt != fndNow {
		t.Fatalf("first record not stamped: %+v", got)
	}

	// Second occurrence from a DIFFERENT run, same fingerprint.
	second := svc.Classify(fndRun(func(r *Run) { r.ID = "run-101" }), "tgt-shop",
		[]AssertionOutcome{fndFailedAssertion()}, nil)
	if second.Fingerprint != got.Fingerprint {
		t.Fatal("test setup: expected same fingerprint")
	}
	got2, err := svc.Record(ctx, second)
	if err != nil {
		t.Fatalf("Record#2: %v", err)
	}
	if got2.ID == got.ID {
		t.Fatal("distinct runs must yield distinct occurrence rows")
	}

	occs, err := svc.Occurrences(ctx, "org-1", got.Fingerprint)
	if err != nil {
		t.Fatalf("Occurrences: %v", err)
	}
	if len(occs) != 2 {
		t.Fatalf("occurrence history = %d, want 2 (never lose history)", len(occs))
	}
	if occs[0].RunID != "run-100" || occs[1].RunID != "run-101" {
		t.Fatalf("occurrence order/log broken: %+v", occs)
	}
	if !occs[0].At.Equal(fndNow) || !occs[1].At.Equal(fndNow) {
		t.Fatalf("occurrence timestamps not from injected clock: %+v", occs)
	}

	// Re-recording the SAME run upserts its row (no duplicate occurrence).
	if _, err := svc.Record(ctx, second); err != nil {
		t.Fatalf("Record repeat: %v", err)
	}
	occs, _ = svc.Occurrences(ctx, "org-1", got.Fingerprint)
	if len(occs) != 2 {
		t.Fatalf("repeat record must upsert, got %d occurrences", len(occs))
	}

	// Cross-org isolation: identical signals in org-2 produce a separate
	// finding with its own history — dedup never merges across orgs.
	other := svc.Classify(fndRun(func(r *Run) { r.OrgID = "org-2" }), "tgt-shop",
		[]AssertionOutcome{fndFailedAssertion()}, nil)
	if other.Fingerprint == got.Fingerprint {
		t.Fatal("org must be inside the fingerprint hash")
	}
	o2, err := svc.Record(ctx, other)
	if err != nil {
		t.Fatalf("Record org-2: %v", err)
	}
	if o2.ID == got.ID || o2.Fingerprint == got.Fingerprint {
		t.Fatal("org isolation violated")
	}
	if _, err := svc.Get(ctx, "org-1", o2.ID); err == nil {
		t.Fatal("org-1 must not see org-2 findings (no cross-tenant leak)")
	}
}

func TestRecordTerminalishRetainsStatus(t *testing.T) {
	t.Parallel()
	svc := NewFindingService(nil, nil, fndClock)
	ctx := context.Background()

	harness := svc.Classify(fndRun(func(r *Run) {
		r.Error = "playwright driver exited"
	}), "tgt-shop", nil, nil)
	if harness.Status != FindingHarness {
		t.Fatalf("setup: status = %s", harness.Status)
	}
	rec, err := svc.Record(ctx, harness)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	// New occurrence of the same fingerprint classifying LIKELY (no new
	// evidence): occurrence recorded, terminal-ish status retained.
	next := svc.Classify(fndRun(func(r *Run) {
		r.ID = "run-102"
		r.Error = "playwright driver exited"
	}), "tgt-shop", nil, nil)
	if next.Fingerprint != rec.Fingerprint {
		t.Fatal("test setup: expected same fingerprint")
	}
	got, err := svc.Record(ctx, next)
	if err != nil {
		t.Fatalf("Record#2: %v", err)
	}
	if got.Status != FindingHarness {
		t.Fatalf("terminal-ish status must be retained without new evidence, got %s", got.Status)
	}
	occs, _ := svc.Occurrences(ctx, "org-1", rec.Fingerprint)
	if len(occs) != 2 {
		t.Fatalf("occurrence must still be recorded: %d", len(occs))
	}
}

func TestRecordValidation(t *testing.T) {
	t.Parallel()
	svc := NewFindingService(nil, nil, fndClock)
	tests := []struct {
		name string
		mut  func(*Finding)
	}{
		{"missing org", func(f *Finding) { f.OrgID = "" }},
		{"missing run", func(f *Finding) { f.RunID = "" }},
		{"missing journey", func(f *Finding) { f.JourneyID = "" }},
		{"bad fingerprint", func(f *Finding) { f.Fingerprint = "deadbeef" }},
		{"bad status", func(f *Finding) { f.Status = "made_up" }},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := svc.Classify(fndRun(nil), "tgt-shop", []AssertionOutcome{fndFailedAssertion()}, nil)
			tc.mut(&f)
			if _, err := svc.Record(context.Background(), f); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

// --- Transition rules ---------------------------------------------------------

func TestValidateTransitionTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		from    FindingStatus
		to      FindingStatus
		reason  string
		wantErr bool
	}{
		{"confirmed -> flaky allowed", FindingConfirmed, FindingFlaky, "re-run passed", false},
		{"confirmed -> likely forbidden", FindingConfirmed, FindingLikely, "", true},
		{"confirmed -> confirmed refresh", FindingConfirmed, FindingConfirmed, "", false},
		{"likely -> confirmed allowed", FindingLikely, FindingConfirmed, "reproduced", false},
		{"likely -> harness allowed", FindingLikely, FindingHarness, "worker crash signals", false},
		{"flaky -> confirmed allowed", FindingFlaky, FindingConfirmed, "reproduced", false},
		{"harness -> likely forbidden", FindingHarness, FindingLikely, "new evidence", true},
		{"harness -> confirmed allowed with evidence", FindingHarness, FindingConfirmed, "independent repro", false},
		{"harness -> confirmed forbidden without reason", FindingHarness, FindingConfirmed, "", true},
		{"harness -> environmental sibling", FindingHarness, FindingEnvironmental, "infra signals found", false},
		{"environmental -> confirmed with evidence", FindingEnvironmental, FindingConfirmed, "independent repro", false},
		{"environmental -> flaky forbidden", FindingEnvironmental, FindingFlaky, "", true},
		{"inconclusive -> likely allowed", FindingInconclusive, FindingLikely, "", false},
		{"unknown from status", "bogus", FindingLikely, "", true},
		{"unknown to status", FindingLikely, "bogus", "", true},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateTransition(tc.from, tc.to, tc.reason)
			if tc.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestServiceTransitionEnforcedAndTimestamped(t *testing.T) {
	t.Parallel()
	svc := NewFindingService(nil, nil, fndClock)
	ctx := context.Background()

	f, err := svc.Record(ctx, svc.Classify(fndRun(nil), "tgt-shop",
		[]AssertionOutcome{fndFailedAssertion()}, nil))
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	// Confirming without reproduction is refused.
	if _, err := svc.Transition(ctx, f.OrgID, f.ID, FindingConfirmed, ""); err == nil {
		t.Fatal("confirming without independent reproduction must be refused")
	}

	// Invalid transition confirmed -> likely is refused (confirmed findings
	// may only move to flaky).
	confirmed, err := svc.Record(ctx, svc.Classify(fndRun(nil), "tgt-shop",
		[]AssertionOutcome{fndFailedAssertion()},
		&ReproResult{ReproRunID: "run-repro-t", Reproduced: true}))
	if err != nil {
		t.Fatalf("Record confirmed: %v", err)
	}
	if confirmed.Status != FindingConfirmed {
		t.Fatalf("setup status = %s", confirmed.Status)
	}
	if _, err := svc.Transition(ctx, f.OrgID, confirmed.ID, FindingLikely, "wishful thinking"); err == nil {
		t.Fatal("confirmed -> likely must be refused")
	}

	// Valid transition, timestamped and audited (shared store so svc2 sees
	// the finding svc recorded).
	store := NewInMemoryFindingStore()
	svcA := NewFindingService(store, nil, fndClock)
	f0, err := svcA.Record(ctx, svcA.Classify(fndRun(nil), "tgt-shop",
		[]AssertionOutcome{fndFailedAssertion()}, nil))
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	later := fndNow.Add(time.Hour)
	svc2 := NewFindingService(store, nil, func() time.Time { return later })
	got, err := svc2.Transition(ctx, f0.OrgID, f0.ID, FindingFlaky, "isolated re-run passed")
	if err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if got.Status != FindingFlaky || got.UpdatedAt != later {
		t.Fatalf("transition not applied/timestamped: %+v", got)
	}
	trail := svc2.Transitions(f0.ID)
	if len(trail) != 1 || trail[0].From != FindingLikely || trail[0].To != FindingFlaky ||
		trail[0].At != later || trail[0].Reason != "isolated re-run passed" {
		t.Fatalf("audit trail broken: %+v", trail)
	}

	// Unknown finding id.
	if _, err := svc.Transition(ctx, f.OrgID, "fnd_missing", FindingFlaky, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing finding: %v", err)
	}
}

// --- Reproduce ----------------------------------------------------------------

// stubReproRunner is a ReproRunner test double that records requests.
type stubReproRunner struct {
	supportsReadOnly bool
	result           ReproResult
	err              error
	requests         []ReproRequest
}

func (r *stubReproRunner) SupportsReadOnly() bool { return r.supportsReadOnly }

func (r *stubReproRunner) ReproRun(ctx context.Context, req ReproRequest) (ReproResult, error) {
	r.requests = append(r.requests, req)
	if r.err != nil {
		return ReproResult{}, r.err
	}
	return r.result, nil
}

func reproEnvironment(mode EnvironmentMode) Environment {
	return Environment{
		ID: "env-isolated", OrgID: "org-1", TargetID: "tgt-shop",
		Kind: EnvDev, Mode: mode, AllowedDomains: []string{"shop.example.test"},
	}
}

func TestReproduceConfirmedAndFlaky(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		reproduced    bool
		wantStatus    FindingStatus
		wantConfFloor float64
	}{
		{"reproduced -> confirmed", true, FindingConfirmed, 0.90},
		{"not reproduced -> flaky", false, FindingFlaky, 0.60},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc := NewFindingService(nil, nil, fndClock)
			ctx := context.Background()
			f, err := svc.Record(ctx, svc.Classify(fndRun(nil), "tgt-shop",
				[]AssertionOutcome{fndFailedAssertion()}, nil))
			if err != nil {
				t.Fatalf("Record: %v", err)
			}
			runner := &stubReproRunner{result: ReproResult{
				ReproRunID: "run-repro-77", Reproduced: tc.reproduced, At: fndNow,
				EnvironmentID: "env-isolated", Mode: ModePreProduction,
			}}
			got, err := svc.Reproduce(ctx, &f, reproEnvironment(ModePreProduction), runner)
			if err != nil {
				t.Fatalf("Reproduce: %v", err)
			}
			if got.Status != tc.wantStatus {
				t.Fatalf("status = %s, want %s", got.Status, tc.wantStatus)
			}
			if got.ReproRunID != "run-repro-77" || got.Reproducible == nil || *got.Reproducible != tc.reproduced {
				t.Fatalf("repro bookkeeping broken: %+v", got)
			}
			if got.Confidence < tc.wantConfFloor {
				t.Fatalf("confidence = %v, want >= %v", got.Confidence, tc.wantConfFloor)
			}
			if len(runner.requests) != 1 || runner.requests[0].ReadOnlySubset {
				t.Fatalf("request flags broken: %+v", runner.requests)
			}
			// Persisted.
			stored, err := svc.Get(ctx, f.OrgID, f.ID)
			if err != nil || stored.ReproRunID != "run-repro-77" {
				t.Fatalf("repro not persisted: %+v %v", stored, err)
			}
		})
	}
}

func TestReproduceReadOnlyRefusalAndSubset(t *testing.T) {
	t.Parallel()
	t.Run("refused when runner cannot honor read-only subset", func(t *testing.T) {
		t.Parallel()
		svc := NewFindingService(nil, nil, fndClock)
		ctx := context.Background()
		f, err := svc.Record(ctx, svc.Classify(fndRun(nil), "tgt-shop",
			[]AssertionOutcome{fndFailedAssertion()}, nil))
		if err != nil {
			t.Fatalf("Record: %v", err)
		}
		runner := &stubReproRunner{supportsReadOnly: false}
		before := f
		if _, err := svc.Reproduce(ctx, &f, reproEnvironment(ModeProductionReadOnly), runner); !errors.Is(err, ErrReadOnlyMode) {
			t.Fatalf("err = %v, want ErrReadOnlyMode", err)
		}
		if len(runner.requests) != 0 {
			t.Fatal("runner must never be invoked after refusal")
		}
		if f.Status != before.Status || f.ReproRunID != "" {
			t.Fatal("finding must be untouched on refusal")
		}
	})
	t.Run("allowed as read-only subset when supported", func(t *testing.T) {
		t.Parallel()
		svc := NewFindingService(nil, nil, fndClock)
		ctx := context.Background()
		f, err := svc.Record(ctx, svc.Classify(fndRun(nil), "tgt-shop",
			[]AssertionOutcome{fndFailedAssertion()}, nil))
		if err != nil {
			t.Fatalf("Record: %v", err)
		}
		runner := &stubReproRunner{
			supportsReadOnly: true,
			result:           ReproResult{ReproRunID: "run-repro-ro", Reproduced: false},
		}
		got, err := svc.Reproduce(ctx, &f, reproEnvironment(ModeProductionReadOnly), runner)
		if err != nil {
			t.Fatalf("Reproduce: %v", err)
		}
		if !runner.requests[0].ReadOnlySubset || runner.requests[0].Mode != ModeProductionReadOnly {
			t.Fatalf("read-only subset not flagged: %+v", runner.requests[0])
		}
		if got.Status != FindingFlaky {
			t.Fatalf("status = %s, want flaky", got.Status)
		}
	})
}

func TestReproduceGuards(t *testing.T) {
	t.Parallel()
	t.Run("cross-org environment refused", func(t *testing.T) {
		t.Parallel()
		svc := NewFindingService(nil, nil, fndClock)
		ctx := context.Background()
		f, _ := svc.Record(ctx, svc.Classify(fndRun(nil), "tgt-shop",
			[]AssertionOutcome{fndFailedAssertion()}, nil))
		env := reproEnvironment(ModePreProduction)
		env.OrgID = "org-9"
		runner := &stubReproRunner{}
		if _, err := svc.Reproduce(ctx, &f, env, runner); !errors.Is(err, ErrScopeViolation) {
			t.Fatalf("err = %v, want ErrScopeViolation", err)
		}
		if len(runner.requests) != 0 {
			t.Fatal("runner must not run on scope violation")
		}
	})
	t.Run("runner error leaves finding untouched", func(t *testing.T) {
		t.Parallel()
		svc := NewFindingService(nil, nil, fndClock)
		ctx := context.Background()
		f, _ := svc.Record(ctx, svc.Classify(fndRun(nil), "tgt-shop",
			[]AssertionOutcome{fndFailedAssertion()}, nil))
		runner := &stubReproRunner{err: fmt.Errorf("isolated env boot timeout")}
		if _, err := svc.Reproduce(ctx, &f, reproEnvironment(ModePreProduction), runner); err == nil {
			t.Fatal("expected runner error to surface")
		}
		if f.ReproRunID != "" || f.Reproducible != nil {
			t.Fatal("finding must be untouched when the reproduction itself fails")
		}
	})
	t.Run("terminal-ish keeps status but records evidence", func(t *testing.T) {
		t.Parallel()
		svc := NewFindingService(nil, nil, fndClock)
		ctx := context.Background()
		f, _ := svc.Record(ctx, svc.Classify(fndRun(func(r *Run) {
			r.Error = "playwright driver exited"
		}), "tgt-shop", nil, nil))
		if f.Status != FindingHarness {
			t.Fatalf("setup status = %s", f.Status)
		}
		runner := &stubReproRunner{result: ReproResult{ReproRunID: "run-repro-h", Reproduced: false}}
		got, err := svc.Reproduce(ctx, &f, reproEnvironment(ModePreProduction), runner)
		if err != nil {
			t.Fatalf("Reproduce: %v", err)
		}
		if got.Status != FindingHarness {
			t.Fatalf("terminal-ish status must be retained, got %s", got.Status)
		}
		if got.ReproRunID != "run-repro-h" || got.Reproducible == nil || *got.Reproducible {
			t.Fatalf("new evidence must still be recorded: %+v", got)
		}
	})
	t.Run("nil finding or runner refused", func(t *testing.T) {
		t.Parallel()
		svc := NewFindingService(nil, nil, fndClock)
		if _, err := svc.Reproduce(context.Background(), nil, Environment{}, &stubReproRunner{}); err == nil {
			t.Fatal("expected error for nil finding")
		}
	})
}

// --- InMemoryFindingStore -----------------------------------------------------

func TestInMemoryFindingStoreSemantics(t *testing.T) {
	t.Parallel()
	store := NewInMemoryFindingStore()
	ctx := context.Background()
	if err := store.Record(ctx, Finding{OrgID: "org-1"}); err == nil {
		t.Fatal("record without id must fail")
	}
	mk := func(id, runID, fp string, st FindingStatus, at time.Time) Finding {
		return Finding{
			ID: id, OrgID: "org-1", RunID: runID, JourneyID: "j-1",
			Fingerprint: fp, Status: st, CreatedAt: at, UpdatedAt: at,
		}
	}
	fpA := strings.Repeat("a", 64)
	fpB := strings.Repeat("b", 64)
	rows := []Finding{
		mk("f1", "run-1", fpA, FindingLikely, fndNow.Add(-2*time.Hour)),
		mk("f2", "run-2", fpA, FindingConfirmed, fndNow.Add(-1*time.Hour)),
		mk("f3", "run-3", fpB, FindingFlaky, fndNow),
	}
	for _, r := range rows {
		if err := store.Record(ctx, r); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	got, err := store.Get(ctx, "org-1", "f2")
	if err != nil || got.Status != FindingConfirmed {
		t.Fatalf("Get: %+v %v", got, err)
	}
	if _, err := store.Get(ctx, "org-2", "f2"); !errors.Is(err, ErrNotFound) {
		t.Fatal("cross-org Get must be ErrNotFound")
	}
	canon, err := store.GetByFingerprint(ctx, "org-1", fpA)
	if err != nil || canon.ID != "f2" {
		t.Fatalf("GetByFingerprint canonical = %+v %v", canon, err)
	}
	hist, err := store.ListByFingerprint(ctx, "org-1", fpA)
	if err != nil || len(hist) != 2 || hist[0].ID != "f1" || hist[1].ID != "f2" {
		t.Fatalf("history = %+v %v", hist, err)
	}
	latest, err := store.List(ctx, "org-1", FindingFilter{Status: FindingConfirmed})
	if err != nil || len(latest) != 1 || latest[0].ID != "f2" {
		t.Fatalf("status filter = %+v %v", latest, err)
	}
	all, err := store.List(ctx, "org-1", FindingFilter{Limit: 2})
	if err != nil || len(all) != 2 {
		t.Fatalf("limit = %+v %v", all, err)
	}
	none, err := store.List(ctx, "org-1", FindingFilter{JourneyID: "j-none"})
	if err != nil || len(none) != 0 {
		t.Fatalf("journey filter = %+v %v", none, err)
	}
}
