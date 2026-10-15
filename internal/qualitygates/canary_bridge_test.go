package qualitygates

import (
	"context"
	"errors"
	"testing"
)

// canary_bridge_test.go — the Decision->advisor mapping and the
// deployments import-only bridge wiring (issue #152).

func TestApplyDecisionMapping(t *testing.T) {
	ctx := context.Background()
	var promoteCalls, abortCalls int
	advisor := CanaryAdvisorFunc{
		Promote: func(ctx context.Context, orgID, agentID, environment string) error {
			promoteCalls++
			return nil
		},
		Abort: func(ctx context.Context, orgID, agentID, environment string) error {
			abortCalls++
			return nil
		},
	}

	// promote -> PromoteCanary.
	res, err := ApplyDecision(ctx, advisor, "org-a", "agent-1", "production", &Decision{Recommendation: RecommendPromote})
	if err != nil || !res.Applied || res.Invoked != "promote" || promoteCalls != 1 || abortCalls != 0 {
		t.Fatalf("promote mapping wrong: %+v %v", res, err)
	}

	// rollback AND abort -> AbortCanary (deployments' revert-to-baseline
	// semantics; intent is recorded on the Decision, not the transition).
	for _, rec := range []Recommendation{RecommendRollback, RecommendAbort} {
		res, err = ApplyDecision(ctx, advisor, "org-a", "agent-1", "production", &Decision{Recommendation: rec})
		if err != nil || !res.Applied || res.Invoked != "abort" {
			t.Fatalf("%s mapping wrong: %+v %v", rec, res, err)
		}
	}
	if abortCalls != 2 {
		t.Fatalf("rollback+abort must each invoke AbortCanary: %d", abortCalls)
	}

	// hold -> deliberate no-op (the split keeps serving while evidence
	// accrues; mirrors the deployments evidence gate).
	res, err = ApplyDecision(ctx, advisor, "org-a", "agent-1", "production", &Decision{Recommendation: RecommendHold})
	if err != nil || res.Applied || res.Invoked != "" {
		t.Fatalf("hold must be a no-op: %+v %v", res, err)
	}

	// Contract violations error instead of guessing.
	if _, err := ApplyDecision(ctx, advisor, "org-a", "agent-1", "production", nil); err == nil {
		t.Fatal("nil decision must error")
	}
	if _, err := ApplyDecision(ctx, nil, "org-a", "agent-1", "production", &Decision{Recommendation: RecommendHold}); err == nil {
		t.Fatal("nil advisor must error")
	}
	if _, err := ApplyDecision(ctx, advisor, "org-a", "agent-1", "production", &Decision{Recommendation: Recommendation(" Deploy ")}); err == nil {
		t.Fatal("unknown recommendation must error")
	}
}

func TestApplyDecisionPropagatesAdvisorErrors(t *testing.T) {
	boom := errors.New("boom")
	advisor := CanaryAdvisorFunc{
		Abort: func(ctx context.Context, orgID, agentID, environment string) error { return boom },
	}
	if _, err := ApplyDecision(context.Background(), advisor, "org-a", "agent-1", "production", &Decision{Recommendation: RecommendAbort}); !errors.Is(err, boom) {
		t.Fatalf("advisor errors must propagate: %v", err)
	}
}

func TestDeploymentsAdvisorNilGuards(t *testing.T) {
	// The import-only bridge must fail loudly (not panic) when the
	// deployments service is not wired — the orchestrator owns the real
	// wiring (documented in canary_bridge.go).
	unwired := NewDeploymentsAdvisor(nil)
	if err := unwired.PromoteCanary(context.Background(), "org-a", "agent-1", "production"); err == nil {
		t.Fatal("unwired advisor must error on promote")
	}
	if err := unwired.AbortCanary(context.Background(), "org-a", "agent-1", "production"); err == nil {
		t.Fatal("unwired advisor must error on abort")
	}
	var nilAdvisor *DeploymentsAdvisor
	if err := nilAdvisor.PromoteCanary(context.Background(), "org-a", "agent-1", "production"); err == nil {
		t.Fatal("nil advisor must error, not panic")
	}

	// Compile-time conformance: the bridge satisfies the narrow interface.
	var _ CanaryAdvisor = NewDeploymentsAdvisor(nil)
	var _ CanaryAdvisor = CanaryAdvisorFunc{}
}
