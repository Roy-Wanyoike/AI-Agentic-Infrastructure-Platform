package synth

// regression_test.go — issue #151 tests for the regression-generation loop
// and fix verification. No network, no runner side effects: fakes only,
// everything deterministic (generated ids are pure functions of the inputs).

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// confirmedFindingWithSteps is the canonical confirmed finding with fully
// structured repro steps.
func confirmedFindingWithSteps() *Finding {
	yes := true
	return &Finding{
		ID:           "F-7",
		OrgID:        "org1",
		RunID:        "R-1",
		JourneyID:    "J-2",
		AssertionIx:  0,
		Status:       FindingConfirmed,
		Severity:     SevCritical,
		Title:        "Cart total drops the discount",
		Description:  "Total ignores applied coupon; api_key: sk-live-9 leaked into the repro notes",
		Fingerprint:  "fp-777",
		ReproSteps:   "1. navigate https://staging.shop.dev/cart\n2. fill #coupon SAVE10\n3. click button[type=submit]\n4. wait 2",
		EvidenceRefs: []string{"evidence://runs/R-1/trace.zip"},
		ReproRunID:   "R-2",
		Reproducible: &yes,
		Confidence:   0.95,
	}
}

// ---------------------------------------------------------------------------
// Candidate generation
// ---------------------------------------------------------------------------

func TestGenerateRegressionTestDeterministicConversion(t *testing.T) {
	j, err := GenerateRegressionTest(confirmedFindingWithSteps(), RegressionOptions{OrgID: "org1", ProjectID: "P-1", EnvironmentID: "E-1", PersonaID: "PS-1"})
	if err != nil {
		t.Fatalf("GenerateRegressionTest: %v", err)
	}

	// REQUIRED invariants: draft + regression source, never scheduled.
	if j.Status != JourneyDraft {
		t.Fatalf("candidate must be born draft, got %q", j.Status)
	}
	if j.Source != SourceRegression {
		t.Fatalf("source = %q, want %q", j.Source, SourceRegression)
	}

	// Deterministic id: same inputs → same id; different fingerprint → different id.
	j2, err := GenerateRegressionTest(confirmedFindingWithSteps(), RegressionOptions{OrgID: "org1"})
	if err != nil {
		t.Fatal(err)
	}
	if j.ID != j2.ID || j.ID == "" {
		t.Fatalf("id not deterministic: %q vs %q", j.ID, j2.ID)
	}
	j3, _ := GenerateRegressionTest(func() *Finding { f := confirmedFindingWithSteps(); f.Fingerprint = "fp-778"; return f }(), RegressionOptions{OrgID: "org1"})
	if j3.ID == j.ID {
		t.Fatal("different fingerprints must yield different ids")
	}

	// 1:1 structured conversion with renumbered indices.
	want := []struct {
		action, target, value string
		write                 bool
	}{
		{"navigate", "https://staging.shop.dev/cart", "", false},
		{"fill", "#coupon", "SAVE10", true},
		{"click", "button[type=submit]", "", true},
		{"wait", "2", "", false},
	}
	if len(j.Steps) != len(want) {
		t.Fatalf("steps = %d, want %d", len(j.Steps), len(want))
	}
	for i, w := range want {
		s := j.Steps[i]
		if s.Index != i+1 || s.Action != w.action || s.Target != w.target || s.Value != w.value || s.WriteOp != w.write {
			t.Errorf("step %d = %+v, want %v", i, s, w)
		}
	}
	if NeedsAuthoring(j) {
		t.Error("fully structured repro must not be tagged needs_authoring")
	}

	// Risk mapping + scrubbing + linkback tags + explicit placeholder criterion.
	if j.Risk != RiskCritical {
		t.Fatalf("risk = %q, want critical", j.Risk)
	}
	if strings.Contains(j.Name, "sk-live-9") || len(j.Steps) > 0 && strings.Contains(j.Steps[1].Value, "sk-live-9") {
		t.Error("finding text must be scrubbed in the candidate")
	}
	hasFindingTag, hasFPTag := false, false
	for _, tag := range j.Tags {
		switch tag {
		case "finding:F-7":
			hasFindingTag = true
		case "fingerprint:fp-777":
			hasFPTag = true
		}
	}
	if !hasFindingTag || !hasFPTag {
		t.Fatalf("linkback tags missing: %v", j.Tags)
	}
	if len(j.Assertions) != 1 || j.Assertions[0].Target != "needs_authoring" || j.Assertions[0].Severity != RiskCritical {
		t.Fatalf("placeholder assertion wrong: %+v", j.Assertions)
	}
	if len(j.Preconditions) < 2 || !strings.Contains(j.Preconditions[0], "fp-777") {
		t.Fatalf("preconditions wrong: %v", j.Preconditions)
	}
	if !strings.Contains(j.Preconditions[1], "ISOLATED") {
		t.Fatalf("isolation precondition missing: %v", j.Preconditions)
	}
}

func TestGenerateRegressionTestRequiresConfirmed(t *testing.T) {
	for _, status := range []FindingStatus{FindingLikely, FindingFlaky, FindingHarness, FindingEnvironmental, FindingInconclusive} {
		f := confirmedFindingWithSteps()
		f.Status = status
		if _, err := GenerateRegressionTest(f, RegressionOptions{OrgID: "org1"}); err == nil {
			t.Errorf("status %q must not generate a candidate", status)
		}
	}
}

func TestGenerateRegressionTestNeedsAuthoring(t *testing.T) {
	// Fully unstructured repro → single placeholder step + tag.
	f := confirmedFindingWithSteps()
	f.ReproSteps = "Open the app, log in, watch the cart total explode (see trace)."
	j, err := GenerateRegressionTest(f, RegressionOptions{OrgID: "org1"})
	if err != nil {
		t.Fatal(err)
	}
	if !NeedsAuthoring(j) {
		t.Fatal("expected needs_authoring tag")
	}
	if len(j.Steps) != 1 || j.Steps[0].Target != "needs_authoring" {
		t.Fatalf("placeholder step wrong: %+v", j.Steps)
	}
	if !strings.Contains(j.Steps[0].Hint, "watch the cart total") {
		t.Fatalf("placeholder must carry the original notes for the author: %+v", j.Steps[0])
	}

	// Mixed: structured head + unstructured tail → converted steps PLUS marker.
	f2 := confirmedFindingWithSteps()
	f2.ReproSteps = "1. navigate https://staging.shop.dev/cart\nthen poke around until the total changes"
	j2, err := GenerateRegressionTest(f2, RegressionOptions{OrgID: "org1"})
	if err != nil {
		t.Fatal(err)
	}
	if !NeedsAuthoring(j2) {
		t.Fatal("partial conversion must be tagged needs_authoring")
	}
	if len(j2.Steps) != 2 || j2.Steps[0].Action != "navigate" || j2.Steps[1].Target != "needs_authoring" {
		t.Fatalf("mixed conversion wrong: %+v", j2.Steps)
	}

	// Garbage actions and missing targets stay unstructured.
	f3 := confirmedFindingWithSteps()
	f3.ReproSteps = "1. teleport somewhere\n2. click\n3. assert #total 42"
	j3, err := GenerateRegressionTest(f3, RegressionOptions{OrgID: "org1"})
	if err != nil {
		t.Fatal(err)
	}
	if !NeedsAuthoring(j3) {
		t.Fatal("garbage repro must be tagged needs_authoring")
	}
	if len(j3.Steps) != 2 || j3.Steps[0].Action != "assert" {
		t.Fatalf("only the structured assert should convert: %+v", j3.Steps)
	}
}

func TestGenerateRegressionTestValidation(t *testing.T) {
	if _, err := GenerateRegressionTest(nil, RegressionOptions{OrgID: "org1"}); err == nil {
		t.Error("nil finding must be refused")
	}
	if _, err := GenerateRegressionTest(confirmedFindingWithSteps(), RegressionOptions{OrgID: "org2"}); err == nil {
		t.Error("org mismatch must be refused")
	}
	if _, err := GenerateRegressionTest(confirmedFindingWithSteps(), RegressionOptions{}); err == nil {
		t.Error("missing options org must be refused")
	}
	f := confirmedFindingWithSteps()
	f.Fingerprint = ""
	if _, err := GenerateRegressionTest(f, RegressionOptions{OrgID: "org1"}); err == nil {
		t.Error("missing fingerprint must be refused")
	}
}

func TestRiskForSeverity(t *testing.T) {
	cases := map[Severity]RiskClass{
		SevInfo: RiskLow, SevLow: RiskLow, SevMedium: RiskMedium,
		SevHigh: RiskHigh, SevCritical: RiskCritical,
		Severity("bogus"): RiskHigh, // unknown → fail safe to stricter gates
	}
	for sev, want := range cases {
		if got := riskForSeverity(sev); got != want {
			t.Errorf("riskForSeverity(%q) = %q, want %q", sev, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Approval gate — candidates stay draft until explicitly approved
// ---------------------------------------------------------------------------

func TestCandidateStaysDraftUntilApproved(t *testing.T) {
	gate := NewInMemoryApprovalGate()
	j, err := GenerateRegressionTest(confirmedFindingWithSteps(), RegressionOptions{OrgID: "org1"})
	if err != nil {
		t.Fatal(err)
	}

	// Not approved → promotion refused, candidate STAYS draft.
	if err := PromoteRegressionCandidate(gate, j); !errors.Is(err, ErrNotApproved) {
		t.Fatalf("expected ErrNotApproved, got %v", err)
	}
	if j.Status != JourneyDraft {
		t.Fatalf("unapproved candidate must stay draft, got %q", j.Status)
	}

	// Explicit approval → promotion to reviewed (scheduling tracks' entry state).
	if err := gate.Approve(j.ID); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if err := PromoteRegressionCandidate(gate, j); err != nil {
		t.Fatalf("promotion after approval: %v", err)
	}
	if j.Status != JourneyReviewed || j.Source != SourceRegression {
		t.Fatalf("promoted state wrong: status=%q source=%q", j.Status, j.Source)
	}

	// Already-promoted candidates are not re-promotable.
	if err := PromoteRegressionCandidate(gate, j); err == nil {
		t.Fatal("re-promotion must be refused")
	}

	// Non-regression journeys are refused even when "approved".
	authored := &Journey{ID: "J-9", Status: JourneyDraft, Source: "authored"}
	if err := gate.Approve("J-9"); err != nil {
		t.Fatal(err)
	}
	if err := PromoteRegressionCandidate(gate, authored); err == nil {
		t.Fatal("non-regression source must be refused")
	}
	if authored.Status != JourneyDraft {
		t.Fatal("refused journey must stay untouched")
	}

	// An approval-only gate (cannot answer "was it approved?") is rejected —
	// promotion must be verifiable, not honor-system.
	if err := PromoteRegressionCandidate(approvalOnlyGate{}, j); err == nil {
		t.Fatal("approve-only gate must be refused")
	}
	if err := gate.Approve(""); err == nil {
		t.Fatal("empty journey id approval must be refused")
	}
}

// approvalOnlyGate satisfies ApprovalGate but cannot report state.
type approvalOnlyGate struct{}

func (approvalOnlyGate) Approve(string) error { return nil }

// ---------------------------------------------------------------------------
// Fix verification
// ---------------------------------------------------------------------------

// recordingRunner captures the journey it was handed and returns a scripted
// result. It never touches production anything — by construction.
type recordingRunner struct {
	gotJourney *Journey
	result     RunnerResult
	err        error
}

func (r *recordingRunner) RunJourney(_ context.Context, j *Journey) (RunnerResult, error) {
	r.gotJourney = j
	return r.result, r.err
}

func TestVerifyFixOutcomes(t *testing.T) {
	f := confirmedFindingWithSteps()

	// Pass: completed run, evidence flows back, finding linked.
	pass := &recordingRunner{result: RunnerResult{RunID: "run-1", Status: RunCompleted, EvidenceRefs: []string{"evidence://runs/run-1/trace.zip"}}}
	res, err := VerifyFix(context.Background(), f, pass)
	if err != nil {
		t.Fatalf("VerifyFix: %v", err)
	}
	if res.Outcome != VerifyPass || res.FindingID != "F-7" || res.Fingerprint != "fp-777" ||
		res.RunID != "run-1" || len(res.EvidenceRefs) != 1 || res.JourneyID == "" {
		t.Fatalf("pass result wrong: %+v", res)
	}
	// The runner received the deterministic regression candidate.
	if pass.gotJourney == nil || pass.gotJourney.Source != SourceRegression ||
		pass.gotJourney.Status != JourneyDraft || pass.gotJourney.ID != res.JourneyID {
		t.Fatalf("runner received wrong journey: %+v", pass.gotJourney)
	}

	// Fail: the defect still reproduces.
	fail := &recordingRunner{result: RunnerResult{RunID: "run-2", Status: RunFailed, Error: "total mismatch: 100 != 90"}}
	res, err = VerifyFix(context.Background(), f, fail)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != VerifyFail || !strings.Contains(res.Detail, "total mismatch") {
		t.Fatalf("fail result wrong: %+v", res)
	}

	// Harness failure (runner error) → Inconclusive, never Fail.
	harness := &recordingRunner{err: errors.New("browser crashed")}
	res, err = VerifyFix(context.Background(), f, harness)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != VerifyInconclusive || !strings.Contains(res.Detail, "browser crashed") {
		t.Fatalf("harness result wrong: %+v", res)
	}

	// Lost worker (cancelled) → Inconclusive.
	lost := &recordingRunner{result: RunnerResult{RunID: "run-3", Status: RunCancelled}}
	res, err = VerifyFix(context.Background(), f, lost)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != VerifyInconclusive {
		t.Fatalf("non-terminal status must be inconclusive, got %q", res.Outcome)
	}
}

func TestVerifyFixValidation(t *testing.T) {
	runner := &recordingRunner{}
	if _, err := VerifyFix(context.Background(), nil, runner); err == nil {
		t.Error("nil finding refused")
	}
	f := confirmedFindingWithSteps()
	if _, err := VerifyFix(context.Background(), f, nil); err == nil {
		t.Error("nil runner refused")
	}
	f.Status = FindingLikely
	if _, err := VerifyFix(context.Background(), f, runner); err == nil {
		t.Error("non-confirmed finding refused")
	}
	if runner.gotJourney != nil {
		t.Error("runner must not be invoked for rejected findings")
	}
}
