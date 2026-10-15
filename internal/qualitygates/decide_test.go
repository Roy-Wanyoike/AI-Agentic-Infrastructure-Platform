package qualitygates

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// gateNow is the pinned decision instant (injected clock — no wall time).
var gateNow = time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)

func evalReq() SuiteRequirement {
	return SuiteRequirement{Kind: SuiteEvaluations, MinPassRate: 0.8, MaxP95Ms: 5000, MaxAge: 24 * time.Hour}
}
func journeyReq() SuiteRequirement {
	return SuiteRequirement{Kind: SuiteSyntheticJourneys, MinCoveragePct: 80, MinPassRate: 0.9}
}
func secReq() SuiteRequirement {
	return SuiteRequirement{Kind: SuiteSecurityAdversarial, MaxSeverityAllowed: SevHigh, FailClosed: true}
}

func completedRec(kind SuiteKind, id string, collected time.Time, mutate func(*SuiteResult)) EvidenceRecord {
	res := SuiteResult{Status: EvidenceCompleted, Cases: 100, Passed: 100, CoveragePct: 100, P95Ms: 100}
	if mutate != nil {
		mutate(&res)
	}
	return EvidenceRecord{
		ID: id, OrgID: "org-a", Suite: kind,
		CollectedAt: collected, Result: res,
		Subject: EvidenceSubject{AppVersion: "v42", AgentID: "agent-1", AgentVersion: 7},
	}
}

func exceptionOn(kind SuiteKind) Exception {
	return Exception{
		Suite: kind, Reason: "signed-off waiver", ApprovedBy: "owner@x.io",
		ApprovedAt: gateNow.Add(-time.Hour),
		ExpiresAt:  gateNow.Add(24 * time.Hour),
	}
}

func TestDecideAllSuitesPassPromotes(t *testing.T) {
	policy := &GatePolicy{ID: "p1", Version: 2, RequiredSuites: []SuiteRequirement{evalReq(), journeyReq(), secReq()}}
	evidence := []EvidenceRecord{
		completedRec(SuiteEvaluations, "ev-e", gateNow.Add(-time.Hour), nil),
		completedRec(SuiteSyntheticJourneys, "ev-j", gateNow.Add(-time.Hour), nil),
		completedRec(SuiteSecurityAdversarial, "ev-s", gateNow.Add(-time.Hour), nil),
	}
	d := Decide(policy, evidence, gateNow)
	if d.Recommendation != RecommendPromote {
		t.Fatalf("want promote, got %s (%+v)", d.Recommendation, d.CheckResults)
	}
	if d.PolicyID != "p1" || d.PolicyVersion != 2 || d.PolicyEvaluationVersion != policy.EvaluationVersion() {
		t.Fatalf("decision must pin the policy: %+v", d)
	}
	if !d.EvaluatedAt.Equal(gateNow.UTC()) {
		t.Fatalf("EvaluatedAt must be the injected now: %v", d.EvaluatedAt)
	}
	if len(d.CheckResults) != 3 {
		t.Fatalf("one check per required suite, got %d", len(d.CheckResults))
	}
	for _, cr := range d.CheckResults {
		if cr.Status != CheckPassed {
			t.Fatalf("want all passed, got %s (%s)", cr.Status, cr.Reason)
		}
	}
}

func TestDecideMissingEvidenceBlocks(t *testing.T) {
	policy := &GatePolicy{ID: "p1", Version: 1, RequiredSuites: []SuiteRequirement{evalReq(), secReq()}}
	evidence := []EvidenceRecord{completedRec(SuiteEvaluations, "ev-e", gateNow.Add(-time.Hour), nil)}
	d := Decide(policy, evidence, gateNow)
	if d.Recommendation != RecommendHold {
		t.Fatalf("missing required evidence must hold, got %s", d.Recommendation)
	}
	var sec *CheckResult
	for i := range d.CheckResults {
		if d.CheckResults[i].Suite == string(SuiteSecurityAdversarial) {
			sec = &d.CheckResults[i]
		}
	}
	if sec == nil || sec.Status != CheckBlocked || sec.Reason != "required suite has no evidence" {
		t.Fatalf("security check must be blocked with the documented reason: %+v", sec)
	}
}

func TestDecideStaleEvidenceBlocksPerPolicy(t *testing.T) {
	req := evalReq() // MaxAge 24h
	policy := &GatePolicy{ID: "p1", Version: 1, RequiredSuites: []SuiteRequirement{req}}
	old := completedRec(SuiteEvaluations, "ev-old", gateNow.Add(-25*time.Hour), nil)
	d := Decide(policy, []EvidenceRecord{old}, gateNow)
	if d.Recommendation != RecommendHold || d.CheckResults[0].Status != CheckStale {
		t.Fatalf("stale evidence must block: %+v", d)
	}
	// MaxAge 0 disables staleness: the same evidence passes.
	req.MaxAge = 0
	policy.RequiredSuites[0] = req
	d = Decide(policy, []EvidenceRecord{old}, gateNow)
	if d.Recommendation != RecommendPromote || d.CheckResults[0].Status != CheckPassed {
		t.Fatalf("max_age 0 must disable staleness: %+v", d)
	}
}

func TestDecideHardSecurityFailureAbortsRegardlessOfAggregates(t *testing.T) {
	// 100% pass rate does NOT excuse a critical finding: structural rule.
	policy := &GatePolicy{ID: "p1", Version: 1, RequiredSuites: []SuiteRequirement{secReq()}}
	evidence := []EvidenceRecord{completedRec(SuiteSecurityAdversarial, "ev-s", gateNow.Add(-time.Hour), func(r *SuiteResult) {
		r.Findings = []EvidenceFinding{{Severity: SevCritical, Title: "prompt injection exfiltrates secrets"}}
	})}
	d := Decide(policy, evidence, gateNow)
	if d.Recommendation != RecommendAbort {
		t.Fatalf("hard security failure must abort, got %s", d.Recommendation)
	}
	cr := d.CheckResults[0]
	if cr.Status != CheckFailed {
		t.Fatalf("check must be failed, got %s", cr.Status)
	}
	want := `hard security failure: finding "prompt injection exfiltrates secrets" severity critical exceeds max allowed high (aggregate scores ignored)`
	if cr.Reason != want {
		t.Fatalf("reason mismatch:\n got %q\nwant %q", cr.Reason, want)
	}
}

func TestDecideSecurityFailureNeverWaivableEvenWithException(t *testing.T) {
	// An exception that names the security suite exactly (and is unexpired)
	// must be structurally ignored: FailClosed ignores all exceptions.
	policy := &GatePolicy{
		ID: "p1", Version: 1,
		RequiredSuites: []SuiteRequirement{secReq()},
		Exceptions:     []Exception{exceptionOn(SuiteSecurityAdversarial)},
	}
	evidence := []EvidenceRecord{completedRec(SuiteSecurityAdversarial, "ev-s", gateNow.Add(-time.Hour), func(r *SuiteResult) {
		r.Findings = []EvidenceFinding{{Severity: SevCritical, Title: "ssrf to metadata"}}
	})}
	d := Decide(policy, evidence, gateNow)
	if d.Recommendation != RecommendAbort || d.CheckResults[0].Status != CheckFailed {
		t.Fatalf("security failure must ignore exceptions: %+v", d.CheckResults[0])
	}
	// A blanket exception ("") is ignored for security suites too.
	policy.Exceptions = []Exception{exceptionOn("")}
	d = Decide(policy, evidence, gateNow)
	if d.Recommendation != RecommendAbort {
		t.Fatalf("blanket waiver must never touch security suites: %+v", d)
	}
}

func TestDecideQualityFailureRollsBack(t *testing.T) {
	policy := &GatePolicy{ID: "p1", Version: 1, RequiredSuites: []SuiteRequirement{evalReq()}}
	evidence := []EvidenceRecord{completedRec(SuiteEvaluations, "ev-e", gateNow.Add(-time.Hour), func(r *SuiteResult) {
		r.Passed = 61 // pass_rate 0.61 < 0.8
	})}
	d := Decide(policy, evidence, gateNow)
	if d.Recommendation != RecommendRollback || d.CheckResults[0].Status != CheckFailed {
		t.Fatalf("threshold failure must rollback: %+v", d)
	}
	if d.CheckResults[0].Reason != "pass_rate 0.61 < 0.8" {
		t.Fatalf("reason mismatch: %q", d.CheckResults[0].Reason)
	}

	// p95 breach rolls back too.
	evidence[0] = completedRec(SuiteEvaluations, "ev-e", gateNow.Add(-time.Hour), func(r *SuiteResult) { r.P95Ms = 6000 })
	d = Decide(policy, evidence, gateNow)
	if d.Recommendation != RecommendRollback || d.CheckResults[0].Reason != "p95_ms 6000 > 5000" {
		t.Fatalf("p95 breach must rollback: %+v", d.CheckResults[0])
	}

	// Coverage shortfall rolls back (journeys requirement).
	jp := &GatePolicy{ID: "p1", Version: 1, RequiredSuites: []SuiteRequirement{journeyReq()}}
	je := []EvidenceRecord{completedRec(SuiteSyntheticJourneys, "ev-j", gateNow.Add(-time.Hour), func(r *SuiteResult) { r.CoveragePct = 62.5 })}
	d = Decide(jp, je, gateNow)
	if d.Recommendation != RecommendRollback || d.CheckResults[0].Reason != "coverage 62.5% < 80%" {
		t.Fatalf("coverage breach must rollback: %+v", d.CheckResults[0])
	}
}

func TestDecideExceptionsWaiveNonSecurityOnlyWhileUnexpired(t *testing.T) {
	policy := &GatePolicy{
		ID: "p1", Version: 1,
		RequiredSuites: []SuiteRequirement{evalReq()},
		Exceptions:     []Exception{exceptionOn(SuiteEvaluations)},
	}
	bad := []EvidenceRecord{completedRec(SuiteEvaluations, "ev-e", gateNow.Add(-time.Hour), func(r *SuiteResult) { r.Passed = 61 })}
	d := Decide(policy, bad, gateNow)
	if d.Recommendation != RecommendPromote || d.CheckResults[0].Status != CheckSkipped {
		t.Fatalf("unexpired exception must waive into promote: %+v", d)
	}
	if want := `pass_rate 0.61 < 0.8 (waived by exception "signed-off waiver" approved by owner@x.io, expires 2025-06-02T12:00:00Z)`; d.CheckResults[0].Reason != want {
		t.Fatalf("waiver reason mismatch:\n got %q\nwant %q", d.CheckResults[0].Reason, want)
	}

	// After expiry the same evidence rolls back.
	expired := *policy
	expired.Exceptions = []Exception{exceptionOn(SuiteEvaluations)}
	expired.Exceptions[0].ExpiresAt = gateNow.Add(-time.Second)
	d = Decide(&expired, bad, gateNow)
	if d.Recommendation != RecommendRollback || d.CheckResults[0].Status != CheckFailed {
		t.Fatalf("expired exception must not waive: %+v", d)
	}
}

func TestDecideInconclusiveEvidenceBlocks(t *testing.T) {
	policy := &GatePolicy{ID: "p1", Version: 1, RequiredSuites: []SuiteRequirement{evalReq()}}
	inc := completedRec(SuiteEvaluations, "ev-i", gateNow.Add(-time.Hour), nil)
	inc.Result.Status = EvidenceInconclusive
	d := Decide(policy, []EvidenceRecord{inc}, gateNow)
	if d.Recommendation != RecommendHold || d.CheckResults[0].Status != CheckInconclusive {
		t.Fatalf("inconclusive must block: %+v", d)
	}
	// Zero cases are inconclusive too (nothing to evaluate).
	empty := completedRec(SuiteEvaluations, "ev-0", gateNow.Add(-time.Hour), func(r *SuiteResult) { r.Cases = 0; r.Passed = 0 })
	d = Decide(policy, []EvidenceRecord{empty}, gateNow)
	if d.Recommendation != RecommendHold || d.CheckResults[0].Status != CheckInconclusive {
		t.Fatalf("zero-case evidence must be inconclusive: %+v", d)
	}
}

func TestDecideNewestEvidencePerKindWinsOrderIndependently(t *testing.T) {
	policy := &GatePolicy{ID: "p1", Version: 1, RequiredSuites: []SuiteRequirement{evalReq()}}
	old := completedRec(SuiteEvaluations, "ev-old", gateNow.Add(-2*time.Hour), func(r *SuiteResult) { r.Passed = 61 })
	newGood := completedRec(SuiteEvaluations, "ev-new", gateNow.Add(-time.Hour), nil)
	// Either input order: the NEWEST record decides.
	for _, order := range [][]EvidenceRecord{{old, newGood}, {newGood, old}} {
		d := Decide(policy, order, gateNow)
		if d.Recommendation != RecommendPromote || d.CheckResults[0].Status != CheckPassed {
			t.Fatalf("newest evidence must win regardless of input order: %+v", d.CheckResults[0])
		}
	}
}

func TestDecidePrecedenceAbortOverRollbackOverHold(t *testing.T) {
	policy := &GatePolicy{
		ID: "p1", Version: 1,
		RequiredSuites: []SuiteRequirement{evalReq(), secReq()},
	}
	evidence := []EvidenceRecord{
		completedRec(SuiteEvaluations, "ev-e", gateNow.Add(-time.Hour), func(r *SuiteResult) { r.Passed = 1 }),
		completedRec(SuiteSecurityAdversarial, "ev-s", gateNow.Add(-time.Hour), func(r *SuiteResult) {
			r.Findings = []EvidenceFinding{{Severity: SevCritical, Title: "c"}}
		}),
	}
	if d := Decide(policy, evidence, gateNow); d.Recommendation != RecommendAbort {
		t.Fatalf("abort must outrank rollback: %s", d.Recommendation)
	}
	// rollback outranks hold: eval fails AND security evidence missing.
	if d := Decide(policy, evidence[:1], gateNow); d.Recommendation != RecommendRollback {
		t.Fatalf("rollback must outrank hold: %s", d.Recommendation)
	}
	// hold when only evidence gaps exist.
	policy2 := &GatePolicy{ID: "p1", Version: 1, RequiredSuites: []SuiteRequirement{secReq()}}
	if d := Decide(policy2, nil, gateNow); d.Recommendation != RecommendHold {
		t.Fatalf("blocked-only must hold: %s", d.Recommendation)
	}
}

func TestDecideDeterministicPinned(t *testing.T) {
	// The issue contract: same policy + same evidence => byte-identical
	// decision. Pinned here including every reason string.
	policy := &GatePolicy{
		ID: "p1", Version: 3,
		RequiredSuites: []SuiteRequirement{journeyReq(), evalReq(), secReq()},
		Exceptions:     []Exception{exceptionOn(SuiteCanaryHealth)},
	}
	evidence := []EvidenceRecord{
		completedRec(SuiteEvaluations, "ev-e", gateNow.Add(-time.Hour), func(r *SuiteResult) { r.Passed = 61; r.P95Ms = 4200 }),
		completedRec(SuiteSyntheticJourneys, "ev-j", gateNow.Add(-2*time.Hour), func(r *SuiteResult) { r.Passed = 99; r.CoveragePct = 95 }),
		completedRec(SuiteSecurityAdversarial, "ev-s", gateNow.Add(-3*time.Hour), nil),
	}
	a := Decide(policy, evidence, gateNow)
	b := Decide(policy, evidence, gateNow)
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Fatalf("Decide must be deterministic:\n%s\n%s", ja, jb)
	}

	want := Decision{
		PolicyID:                "p1",
		PolicyVersion:           3,
		PolicyEvaluationVersion: policy.EvaluationVersion(),
		Recommendation:          RecommendRollback,
		EvaluatedAt:             gateNow.UTC(),
		CheckResults: []CheckResult{
			{Suite: "evaluations", Status: CheckFailed, Reason: "pass_rate 0.61 < 0.8"},
			{Suite: "security_adversarial", Status: CheckPassed, Reason: "evidence ev-s satisfied every threshold (pass_rate 1, coverage 100%, p95_ms 100)"},
			{Suite: "synthetic_journeys", Status: CheckPassed, Reason: "evidence ev-j satisfied every threshold (pass_rate 0.99, coverage 95%, p95_ms 100)"},
		},
	}
	if !reflect.DeepEqual(*a, want) {
		gotJSON, _ := json.MarshalIndent(a, "", "  ")
		wantJSON, _ := json.MarshalIndent(want, "", "  ")
		t.Fatalf("pinned decision drift:\n got %s\nwant %s", gotJSON, wantJSON)
	}
}

func TestDecideNilPolicyHoldsWithoutPanicking(t *testing.T) {
	d := Decide(nil, nil, gateNow)
	if d.Recommendation != RecommendHold || len(d.CheckResults) != 1 || d.CheckResults[0].Status != CheckBlocked {
		t.Fatalf("nil policy must hold defensively: %+v", d)
	}
}
