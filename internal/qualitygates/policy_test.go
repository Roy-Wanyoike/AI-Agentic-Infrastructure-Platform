package qualitygates

import (
	"errors"
	"testing"
	"time"
)

// validSecuritySuite returns a security_adversarial requirement that
// satisfies the structural fail-closed contract.
func validSecuritySuite() SuiteRequirement {
	return SuiteRequirement{
		Kind:               SuiteSecurityAdversarial,
		MaxSeverityAllowed: SevHigh,
		FailClosed:         true,
	}
}

func TestValidateAcceptsWellFormedPolicy(t *testing.T) {
	p := &GatePolicy{
		Name:   "release-default",
		Active: true,
		RequiredSuites: []SuiteRequirement{
			{Kind: SuiteEvaluations, MinPassRate: 0.8, MaxP95Ms: 5000, MaxAge: 24 * time.Hour},
			validSecuritySuite(),
		},
		Exceptions: []Exception{{
			Suite:      SuiteCanaryHealth,
			Reason:     "known flaky probe",
			ApprovedBy: "owner@x.io",
			ExpiresAt:  time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC),
		}},
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("want valid, got %v", err)
	}
}

func TestValidateRejections(t *testing.T) {
	base := func() *GatePolicy {
		return &GatePolicy{
			Name:           "p",
			RequiredSuites: []SuiteRequirement{{Kind: SuiteEvaluations, MinPassRate: 0.5}},
		}
	}
	cases := []struct {
		name   string
		mutate func(*GatePolicy)
	}{
		{"nil policy", func(p *GatePolicy) { *p = GatePolicy{}; p.Name = "x"; p.RequiredSuites = nil }},
		{"empty name", func(p *GatePolicy) { p.Name = "  " }},
		{"no suites", func(p *GatePolicy) { p.RequiredSuites = nil }},
		{"unknown kind", func(p *GatePolicy) { p.RequiredSuites[0].Kind = "chaos" }},
		{"duplicate kind", func(p *GatePolicy) {
			p.RequiredSuites = append(p.RequiredSuites, SuiteRequirement{Kind: SuiteEvaluations})
		}},
		{"coverage out of range", func(p *GatePolicy) { p.RequiredSuites[0].MinCoveragePct = 120 }},
		{"negative coverage", func(p *GatePolicy) { p.RequiredSuites[0].MinCoveragePct = -1 }},
		{"pass rate out of range", func(p *GatePolicy) { p.RequiredSuites[0].MinPassRate = 1.5 }},
		{"negative p95", func(p *GatePolicy) { p.RequiredSuites[0].MaxP95Ms = -5 }},
		{"negative max age", func(p *GatePolicy) { p.RequiredSuites[0].MaxAge = -time.Hour }},
		{"unknown severity", func(p *GatePolicy) { p.RequiredSuites[0].MaxSeverityAllowed = Severity("blocker") }},
		{"security not fail closed", func(p *GatePolicy) {
			s := validSecuritySuite()
			s.FailClosed = false
			p.RequiredSuites[0] = s
		}},
		{"security without explicit severity", func(p *GatePolicy) {
			p.RequiredSuites[0] = SuiteRequirement{Kind: SuiteSecurityAdversarial, FailClosed: true}
		}},
		{"exception without expiry", func(p *GatePolicy) {
			p.Exceptions = []Exception{{Reason: "r", ApprovedBy: "a"}}
		}},
		{"exception without reason", func(p *GatePolicy) {
			p.Exceptions = []Exception{{ApprovedBy: "a", ExpiresAt: time.Now().UTC().Add(time.Hour)}}
		}},
		{"exception without approver", func(p *GatePolicy) {
			p.Exceptions = []Exception{{Reason: "r", ExpiresAt: time.Now().UTC().Add(time.Hour)}}
		}},
		{"exception unknown suite", func(p *GatePolicy) {
			p.Exceptions = []Exception{{Suite: "chaos", Reason: "r", ApprovedBy: "a", ExpiresAt: time.Now().UTC().Add(time.Hour)}}
		}},
	}
	for _, tc := range cases {
		p := base()
		tc.mutate(p)
		if err := p.Validate(); err == nil {
			t.Errorf("%s: want rejection, got nil", tc.name)
		} else if !errors.Is(err, ErrInvalidPolicy) {
			t.Errorf("%s: error must wrap ErrInvalidPolicy, got %v", tc.name, err)
		}
	}
}

func TestEvaluationVersionDeterministicAndContentAddressed(t *testing.T) {
	p := &GatePolicy{
		ID:      "pol-1",
		OrgID:   "org-a",
		Name:    "default",
		Version: 3,
		Active:  true,
		RequiredSuites: []SuiteRequirement{
			{Kind: SuiteEvaluations, MinPassRate: 0.8, MaxAge: 24 * time.Hour},
			validSecuritySuite(),
		},
	}
	// Same content, suites listed in a different order: same version.
	reordered := *p
	reordered.RequiredSuites = []SuiteRequirement{p.RequiredSuites[1], p.RequiredSuites[0]}
	if p.EvaluationVersion() == "" || p.EvaluationVersion() != reordered.EvaluationVersion() {
		t.Fatalf("EvaluationVersion must be order-insensitive and non-empty: %q vs %q", p.EvaluationVersion(), reordered.EvaluationVersion())
	}

	// Any content change (version, threshold, exception) changes the hash.
	bumped := *p
	bumped.Version = 4
	if p.EvaluationVersion() == bumped.EvaluationVersion() {
		t.Fatal("version bump must change the evaluation version")
	}
	tightened := *p
	tightened.RequiredSuites = append([]SuiteRequirement(nil), p.RequiredSuites...) // deep copy: slice is shared
	tightened.RequiredSuites[0].MinPassRate = 0.9
	if p.EvaluationVersion() == tightened.EvaluationVersion() {
		t.Fatal("threshold change must change the evaluation version")
	}
	withEx := *p
	withEx.Exceptions = []Exception{{Reason: "r", ApprovedBy: "a", ExpiresAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)}}
	if p.EvaluationVersion() == withEx.EvaluationVersion() {
		t.Fatal("exception change must change the evaluation version")
	}

	// Repeated calls are stable (no hidden state).
	if p.EvaluationVersion() != p.EvaluationVersion() {
		t.Fatal("EvaluationVersion must be stable across calls")
	}
}
