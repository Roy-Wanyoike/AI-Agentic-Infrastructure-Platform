package tools

// policy.go — the per-tool policy gate (issue #111). The PRIMARY enforcement
// point for tool calls remains the runtime seam: the runner evaluates every
// tool invocation through *policies.Enforcer.AuthorizeToolCall before the
// tool executes (internal/runtime/runtime.go checkToolPolicy; the worker
// stamps org/environment via policies.WithRunScope). Tools executed through
// the runner therefore need no extra wiring.
//
// This file adds an OPT-IN defense-in-depth gate for tools that are also
// reachable outside a run (direct registry use, MCP adapters): each new
// catalog tool consults ToolPolicyGate (when wired) before doing work, and
// surfaces the request-scoped facts the policy engine's conditions expect —
// resource type "tool", action "tools.call", context.tool for tool_allowlist,
// context.environment from the run scope and context.estimated_cost_cents
// taken from the tool's "estimated_cost_cents" argument (max_cost_cents
// condition, internal/policies policy.go).
//
// internal/policies is imported read-only (no edits to that package); the
// gate accepts the platform Evaluator interface so *policies.Service,
// policies.HTTPEvaluator, policies.EvaluatorFunc and *policies.Enforcer
// adapters all satisfy it.

import (
	"context"
	"errors"
	"fmt"

	"agentos/internal/policies"
)

// ToolPolicyDenied is returned when the policy decision is deny (or
// require_approval routed as a denial by the caller's flow). The message
// carries the engine's reason verbatim.
var ToolPolicyDenied = errors.New("tools: denied by policy")

// ToolPolicyEvaluator is the decision source of the gate. It is satisfied by
// *policies.Service, policies.HTTPEvaluator and policies.EvaluatorFunc.
type ToolPolicyEvaluator interface {
	EvaluateCtx(ctx context.Context, orgID string, req policies.EvaluateRequest) (policies.Decision, error)
}

// ToolPolicyGate evaluates one tool invocation against the organization's
// policies BEFORE the tool does any work. A nil gate is a no-op (the runtime
// seam stays the authoritative enforcement point); a wired gate fails CLOSED:
// a failing decision source denies the call.
type ToolPolicyGate struct {
	evaluator ToolPolicyEvaluator
}

// NewToolPolicyGate returns a gate over the given evaluator. A nil evaluator
// yields a nil-effect gate that reports Enabled() == false.
func NewToolPolicyGate(evaluator ToolPolicyEvaluator) *ToolPolicyGate {
	if evaluator == nil {
		return nil
	}
	return &ToolPolicyGate{evaluator: evaluator}
}

// Enabled reports whether the gate will actually evaluate anything.
func (g *ToolPolicyGate) Enabled() bool {
	return g != nil && g.evaluator != nil
}

// Allow evaluates the tools.call action for the named tool on behalf of the
// organization carried by the run scope in ctx (policies.WithRunScope). An
// empty scope organization fails closed: without a tenant there is no policy
// set to consult and no safe default to assume. estimatedCostCents feeds the
// max_cost_cents condition (0 for tools without a cost model).
func (g *ToolPolicyGate) Allow(ctx context.Context, tool string, estimatedCostCents int64) error {
	if !g.Enabled() {
		return nil
	}
	scope := policies.RunScopeFromContext(ctx)
	if scope.OrganizationID == "" {
		return fmt.Errorf("%w: no organization scope in context (policies cannot be evaluated)", ToolPolicyDenied)
	}
	req := policies.EvaluateRequest{
		Action: policies.ActionToolCall,
		Resource: policies.Resource{
			Type:     policies.ResourceTool,
			ID:       tool,
			TenantID: scope.OrganizationID,
		},
		Context: policies.EvalContext{
			EstimatedCostCents: estimatedCostCents,
			Environment:        scope.Environment,
			Tool:               tool,
		},
	}
	decision, err := g.evaluator.EvaluateCtx(ctx, scope.OrganizationID, req)
	if err != nil {
		// Fail closed: a broken decision source must never open the gate.
		return fmt.Errorf("%w: policy evaluation failed: %v", ToolPolicyDenied, err)
	}
	if !decision.Allowed() {
		return fmt.Errorf("%w: %s", ToolPolicyDenied, decision.Reason)
	}
	return nil
}

// toolEstimatedCostCents reads the optional "estimated_cost_cents" tool
// argument (JSON number or numeric string). Absent/garbage values resolve to
// 0 — tools without a cost model never invent one.
func toolEstimatedCostCents(value any) int64 {
	switch v := value.(type) {
	case float64:
		if v > 0 {
			return int64(v)
		}
	case int64:
		if v > 0 {
			return v
		}
	case int:
		if v > 0 {
			return int64(v)
		}
	case string:
		var n int64
		neg := false
		s := v
		if len(s) > 0 && s[0] == '-' {
			neg = true
			s = s[1:]
		}
		for _, ch := range s {
			if ch < '0' || ch > '9' {
				return 0
			}
			n = n*10 + int64(ch-'0')
		}
		if neg {
			n = -n
		}
		if n > 0 {
			return n
		}
	}
	return 0
}
