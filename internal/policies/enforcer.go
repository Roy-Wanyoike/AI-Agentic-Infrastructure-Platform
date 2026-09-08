package policies

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// EnforcementEnvVar is the feature-flag environment variable gating the
// governance enforcement seams (issue #75): run creation and runtime tool
// invocation consult matching policies only when enforcement is enabled.
const EnforcementEnvVar = "AGENTOS_POLICY_ENFORCEMENT"

// DefaultEnvironment is the environment assumed when a caller does not state
// one. It mirrors the deployments vocabulary (development|staging|production)
// whose default read models answer for "production" (e.g. the canary status
// endpoint), so environment-scoped policies behave the same everywhere.
const DefaultEnvironment = "production"

// Canonical action names used by the enforcement seams. They reuse the
// action vocabulary the engine and its tests already pin ("runs.execute",
// "tools.call") so one policy record governs both the HTTP evaluate endpoint
// and the enforcement paths.
const (
	// ActionRunExecute is evaluated before a run is accepted (run creation).
	ActionRunExecute = "runs.execute"
	// ActionToolCall is evaluated before one tool invocation executes.
	ActionToolCall = "tools.call"
)

// EnforcementFromEnv reports whether enforcement is requested. Default ON
// (issue #75): only the explicit spellings parsed by strconv.ParseBool as
// false disable it, and unreadable garbage is treated as ON — a typo must
// never silently open the governance gate. (The mirrored billing flag
// AGENTOS_BILLING_ENFORCEMENT defaults OFF because it changes pricing
// behavior; policy enforcement is the documented default and every
// no-policy organization keeps the default-allow outcome.)
func EnforcementFromEnv() bool {
	v := strings.TrimSpace(os.Getenv(EnforcementEnvVar))
	if v == "" {
		return true
	}
	on, err := strconv.ParseBool(v)
	if err != nil {
		return true
	}
	return on
}

// runScopeContextKey carries the per-run facts (tenant + environment) the
// execution engine needs for policy checks. Workers stamp it from the task
// payload before invoking the runner; the runtime reads it at the tool seam.
type runScopeContextKey struct{}

// RunScope carries the tenant/environment facts of one executing run.
type RunScope struct {
	OrganizationID string
	Environment    string
}

// WithRunScope returns a context carrying the run scope for policy checks.
func WithRunScope(ctx context.Context, scope RunScope) context.Context {
	return context.WithValue(ctx, runScopeContextKey{}, scope)
}

// RunScopeFromContext resolves the run scope from ctx (zero value when
// absent). An empty Environment is normalized to DefaultEnvironment so
// environment-scoped policies match the platform default.
func RunScopeFromContext(ctx context.Context) RunScope {
	scope, _ := ctx.Value(runScopeContextKey{}).(RunScope)
	if strings.TrimSpace(scope.Environment) == "" {
		scope.Environment = DefaultEnvironment
	}
	return scope
}

// Evaluator is the decision source the Enforcer consults. *Service
// implements it; HTTPEvaluator implements it for processes that hold no
// durable policy state (the pull-mode worker).
type Evaluator interface {
	EvaluateCtx(ctx context.Context, orgID string, req EvaluateRequest) (Decision, error)
}

// EvaluatorFunc adapts a plain function to Evaluator (the StepRecorderFunc
// precedent) so tests and embedded callers can inject canned decisions
// without constructing a Service.
type EvaluatorFunc func(ctx context.Context, orgID string, req EvaluateRequest) (Decision, error)

// EvaluateCtx implements Evaluator.
func (f EvaluatorFunc) EvaluateCtx(ctx context.Context, orgID string, req EvaluateRequest) (Decision, error) {
	if f == nil {
		return disabledDecision(), nil
	}
	return f(ctx, orgID, req)
}

// Enforcer adapts the policy engine to the two enforcement seams (issue #75):
//
//   - run creation: AuthorizeRunCreation gates POST /runs before the run row
//     exists; denies surface as 403 policy_denied and require_approval
//     decisions route the run into the approvals flow;
//   - tool invocation: AuthorizeToolCall gates every runtime tool call; a
//     denied tool is never invoked and the denial is visible on the step.
//
// The zero-risk contract: a nil *Enforcer, a nil decision source, or a
// disabled flag all degrade to the default-allow Decision so unwired callers
// keep byte-identical legacy behavior. When the enforcer IS enabled but the
// decision source fails, Authorize* returns the error and the caller decides
// how to fail closed (503 on run creation, tool denial at the runtime seam).
type Enforcer struct {
	evaluator Evaluator
	enabled   bool
}

// NewEnforcer returns an enforcer over the service, honoring
// AGENTOS_POLICY_ENFORCEMENT (default ON). The service may be nil (disabled).
func NewEnforcer(svc *Service) *Enforcer {
	return &Enforcer{evaluator: svc, enabled: EnforcementFromEnv()}
}

// NewEnforcerWithEvaluator returns an enforcer over any decision source
// (service or HTTP evaluator), honoring AGENTOS_POLICY_ENFORCEMENT.
func NewEnforcerWithEvaluator(evaluator Evaluator) *Enforcer {
	return &Enforcer{evaluator: evaluator, enabled: EnforcementFromEnv()}
}

// NewEnforcerWithFlag returns an enforcer with an explicit on/off state,
// bypassing the environment flag (test and embedded-Runtime seam).
func NewEnforcerWithFlag(evaluator Evaluator, enabled bool) *Enforcer {
	return &Enforcer{evaluator: evaluator, enabled: enabled}
}

// Enabled reports whether enforcement is active. A nil enforcer is disabled.
func (e *Enforcer) Enabled() bool {
	return e != nil && e.enabled && e.evaluator != nil
}

// disabledDecision is the explicit outcome of an unwired/disabled seam.
func disabledDecision() Decision {
	return Decision{
		Decision: EffectAllow,
		Reason:   "policy enforcement disabled",
	}
}

// AuthorizeRunCreation evaluates the run-creation action for one agent run
// before the run row is created. environment defaults to DefaultEnvironment
// when blank. The returned error (a failing decision source) is the caller's
// fail-closed signal; every other outcome is a concrete Decision.
func (e *Enforcer) AuthorizeRunCreation(ctx context.Context, orgID, agentID, environment string, estimatedCostCents int64) (Decision, error) {
	if !e.Enabled() {
		return disabledDecision(), nil
	}
	if strings.TrimSpace(environment) == "" {
		environment = DefaultEnvironment
	}
	req := EvaluateRequest{
		Action: ActionRunExecute,
		Resource: Resource{
			Type:     ResourceAgent,
			ID:       agentID,
			TenantID: orgID,
		},
		Context: EvalContext{
			EstimatedCostCents: estimatedCostCents,
			Environment:        environment,
		},
	}
	return e.evaluator.EvaluateCtx(ctx, orgID, req)
}

// AuthorizeToolCall evaluates one tool invocation inside a run. The tool is
// matched by resource type "tool" and Context.Tool so tool_allowlist
// conditions resolve against the invoked tool name.
func (e *Enforcer) AuthorizeToolCall(ctx context.Context, orgID, tool, environment string) (Decision, error) {
	if !e.Enabled() {
		return disabledDecision(), nil
	}
	if strings.TrimSpace(environment) == "" {
		environment = DefaultEnvironment
	}
	req := EvaluateRequest{
		Action: ActionToolCall,
		Resource: Resource{
			Type:     ResourceTool,
			ID:       tool,
			TenantID: orgID,
		},
		Context: EvalContext{
			Environment: environment,
			Tool:        tool,
		},
	}
	return e.evaluator.EvaluateCtx(ctx, orgID, req)
}

// DenyMessage renders the operator-facing message for a denied action. It
// carries the machine reason (policy name/priority + condition context) the
// engine produced so API consumers can show WHY the action was refused.
func DenyMessage(action string, d Decision) string {
	return fmt.Sprintf("%s denied by policy: %s", action, d.Reason)
}
