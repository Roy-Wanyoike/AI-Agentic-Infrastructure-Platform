package qualitygates

// canary_bridge.go — the seam between quality-gate decisions and the
// EXISTING canary machinery (issue #152).
//
// This package never mutates live traffic itself and never reaches into
// internal/deployments state: a Decision is a RECOMMENDATION. The
// orchestrator (cmd wiring / a future controller) implements the narrow
// CanaryAdvisor below and ApplyDecision converts a Decision into the
// advisor calls. The adapter shape mirrors the documented-reuse bridges in
// internal/deployments/policy.go (EvalSampleSourceFunc et al): deployments
// defines ITS sampling seams without importing evaluations; qualitygates
// defines ITS application seam without owning deployment transitions.
//
// Mapping onto the existing machinery (internal/deployments/canary.go):
//
//	promote   -> PromoteCanary   (canary becomes stable; weight -> 0)
//	rollback  -> AbortCanary     (canary cleared; STABLE keeps serving
//	                            100% — deployments' documented revert-
//	                            to-baseline semantics, see policy.go
//	                            decideCanary: "Rollback reverts to the
//	                            baseline: the canary config is cleared
//	                            and the stable version keeps serving")
//	abort     -> AbortCanary     (same transition: stable keeps serving;
//	                            the difference is intent — hard security
//	                            failure vs proven regression — recorded
//	                            on the Decision and in the audit trail)
//	hold      -> no call         (the split keeps serving while evidence
//	                            accrues; deployments' evidence gate)

import (
	"context"
	"fmt"

	"agentos/internal/deployments"
)

// CanaryAdvisor is the narrow interface the orchestrator implements to
// apply a quality-gate recommendation to a live canary. The two methods
// are exactly the transitions the existing machinery supports — the bridge
// deliberately does NOT widen the deployments surface.
type CanaryAdvisor interface {
	// PromoteCanary promotes the active canary of agent+environment to
	// stable (deployments.PromoteCanary semantics).
	PromoteCanary(ctx context.Context, orgID, agentID, environment string) error
	// AbortCanary clears the active canary of agent+environment, leaving
	// the stable version serving 100% (deployments.AbortCanary semantics;
	// used for BOTH rollback and abort recommendations — see mapping).
	AbortCanary(ctx context.Context, orgID, agentID, environment string) error
}

// AdvisorResult reports what ApplyDecision did.
type AdvisorResult struct {
	// Applied is true when a transition was invoked (false for hold).
	Applied bool
	// Action is the recommendation that drove the outcome.
	Action Recommendation
	// Invoked is the advisor method used ("promote" | "abort" | "" for
	// hold) — surfaced for logs/audit.
	Invoked string
}

// ApplyDecision converts a Decision into CanaryAdvisor calls (the
// orchestrator wiring point). hold is a deliberate no-op (the canary keeps
// running while evidence accrues); promote invokes PromoteCanary; abort
// and rollback both invoke AbortCanary (documented mapping above). The
// Decision is never nil-tolerated: a nil decision returns an error rather
// than guessing.
func ApplyDecision(ctx context.Context, advisor CanaryAdvisor, orgID, agentID, environment string, decision *Decision) (*AdvisorResult, error) {
	if advisor == nil {
		return nil, fmt.Errorf("qualitygates: apply decision requires a CanaryAdvisor")
	}
	if decision == nil {
		return nil, fmt.Errorf("qualitygates: apply decision requires a Decision")
	}
	switch decision.Recommendation {
	case RecommendPromote:
		if err := advisor.PromoteCanary(ctx, orgID, agentID, environment); err != nil {
			return nil, err
		}
		return &AdvisorResult{Applied: true, Action: RecommendPromote, Invoked: "promote"}, nil
	case RecommendRollback, RecommendAbort:
		if err := advisor.AbortCanary(ctx, orgID, agentID, environment); err != nil {
			return nil, err
		}
		return &AdvisorResult{Applied: true, Action: decision.Recommendation, Invoked: "abort"}, nil
	case RecommendHold:
		return &AdvisorResult{Applied: false, Action: RecommendHold, Invoked: ""}, nil
	default:
		return nil, fmt.Errorf("qualitygates: unknown recommendation %q", decision.Recommendation)
	}
}

// CanaryAdvisorFunc adapts plain functions to CanaryAdvisor (the
// EvalSampleSourceFunc house pattern).
type CanaryAdvisorFunc struct {
	Promote func(ctx context.Context, orgID, agentID, environment string) error
	Abort   func(ctx context.Context, orgID, agentID, environment string) error
}

// PromoteCanary implements CanaryAdvisor.
func (f CanaryAdvisorFunc) PromoteCanary(ctx context.Context, orgID, agentID, environment string) error {
	if f.Promote == nil {
		return nil
	}
	return f.Promote(ctx, orgID, agentID, environment)
}

// AbortCanary implements CanaryAdvisor.
func (f CanaryAdvisorFunc) AbortCanary(ctx context.Context, orgID, agentID, environment string) error {
	if f.Abort == nil {
		return nil
	}
	return f.Abort(ctx, orgID, agentID, environment)
}

// DeploymentsAdvisor adapts the EXISTING deployments service to
// CanaryAdvisor (import-only bridge: internal/deployments is NOT modified
// by this track). It resolves the agent+environment to the current HEALTHY
// deployment row via deployments.CanaryStatusCtx — the same row the
// promote/abort lifecycle treats as "what serves traffic".
type DeploymentsAdvisor struct {
	svc *deployments.Service
}

// NewDeploymentsAdvisor returns the bridge for one deployments service.
func NewDeploymentsAdvisor(svc *deployments.Service) *DeploymentsAdvisor {
	return &DeploymentsAdvisor{svc: svc}
}

// PromoteCanary implements CanaryAdvisor.
func (a *DeploymentsAdvisor) PromoteCanary(ctx context.Context, orgID, agentID, environment string) error {
	if a == nil || a.svc == nil {
		return fmt.Errorf("qualitygates: deployments advisor not wired")
	}
	status, err := a.svc.CanaryStatusCtx(ctx, orgID, agentID, environment)
	if err != nil {
		return err
	}
	_, err = a.svc.PromoteCanaryCtx(ctx, orgID, status.DeploymentID)
	return err
}

// AbortCanary implements CanaryAdvisor.
func (a *DeploymentsAdvisor) AbortCanary(ctx context.Context, orgID, agentID, environment string) error {
	if a == nil || a.svc == nil {
		return fmt.Errorf("qualitygates: deployments advisor not wired")
	}
	status, err := a.svc.CanaryStatusCtx(ctx, orgID, agentID, environment)
	if err != nil {
		return err
	}
	_, err = a.svc.AbortCanaryCtx(ctx, orgID, status.DeploymentID)
	return err
}
