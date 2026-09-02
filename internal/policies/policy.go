// Package policies implements the AgentOS governance layer: tenant-scoped
// policy records (allow/deny rules with conditions) plus the evaluation
// engine used to authorize actions before they execute.
//
// Evaluation semantics (Task 2-c contract):
//   - only enabled policies of the caller's organization participate;
//   - a policy matches when resource_type equals the request resource type
//     (or is "*"), the requested action is listed (an empty actions list
//     matches every action) and every specified condition holds;
//   - conditions are applicability predicates:
//     tool_allowlist (non-empty) -> the request tool must be listed;
//     environments   (non-empty) -> the request environment must be listed;
//     max_cost_cents (set >= 0)  -> the request is over budget, i.e.
//     estimated_cost_cents > max_cost_cents
//     (a budget-guard condition: it lets an
//     operator write one deny rule for
//     requests exceeding the cost cap; an
//     allow rule with the same condition
//     explicitly authorizes over-budget
//     requests);
//     require_approval           -> not a match predicate; when the winning
//     policy carries it the decision reason
//     notes that approval is required;
//   - the highest-priority matching policy wins; on a priority tie a deny
//     beats an allow (deny wins);
//   - when no policy matches the default decision is allow.
package policies

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Effect values and resource types accepted on policy records.
const (
	EffectAllow = "allow"
	EffectDeny  = "deny"

	ResourceTool       = "tool"
	ResourceAgent      = "agent"
	ResourceWorkflow   = "workflow"
	ResourceDeployment = "deployment"
	ResourceWildcard   = "*"
)

// ErrInvalidPolicy is returned when a policy record fails validation.
var ErrInvalidPolicy = errors.New("policies: invalid policy")

// Conditions restricts when a policy applies. A nil/zero Conditions matches
// every request for the policy's resource type and actions.
type Conditions struct {
	// ToolAllowlist restricts the policy to the listed tool ids/names.
	ToolAllowlist []string `json:"tool_allowlist,omitempty"`
	// MaxCostCents, when set (>= 0), restricts the policy to requests whose
	// estimated cost exceeds the cap (budget-guard semantics above).
	MaxCostCents *int64 `json:"max_cost_cents"`
	// Environments restricts the policy to the listed environments.
	Environments []string `json:"environments,omitempty"`
	// RequireApproval annotates the policy: when the winning policy has it,
	// the decision reason states that approval is required.
	RequireApproval bool `json:"require_approval"`
}

// Policy is one governance rule scoped to a single organization.
type Policy struct {
	ID             string     `json:"id"`
	OrganizationID string     `json:"-"` // tenant scope, never serialized
	Name           string     `json:"name"`
	Effect         string     `json:"effect"` // allow | deny
	ResourceType   string     `json:"resource_type"`
	Actions        []string   `json:"actions"`
	Conditions     Conditions `json:"conditions"`
	Priority       int        `json:"priority"`
	Enabled        bool       `json:"enabled"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// Validate checks the record invariants (name, effect, resource type,
// actions, conditions).
func (p *Policy) Validate() error {
	if p == nil {
		return fmt.Errorf("%w: policy is nil", ErrInvalidPolicy)
	}
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("%w: name is required", ErrInvalidPolicy)
	}
	switch strings.ToLower(strings.TrimSpace(p.Effect)) {
	case EffectAllow, EffectDeny:
	default:
		return fmt.Errorf("%w: effect must be allow or deny", ErrInvalidPolicy)
	}
	switch strings.ToLower(strings.TrimSpace(p.ResourceType)) {
	case ResourceTool, ResourceAgent, ResourceWorkflow, ResourceDeployment, ResourceWildcard:
	default:
		return fmt.Errorf("%w: resource_type must be one of tool|agent|workflow|deployment|*", ErrInvalidPolicy)
	}
	for _, action := range p.Actions {
		if strings.TrimSpace(action) == "" {
			return fmt.Errorf("%w: actions must not contain empty entries", ErrInvalidPolicy)
		}
	}
	if p.Conditions.MaxCostCents != nil && *p.Conditions.MaxCostCents < 0 {
		return fmt.Errorf("%w: max_cost_cents must be >= 0", ErrInvalidPolicy)
	}
	for _, tool := range p.Conditions.ToolAllowlist {
		if strings.TrimSpace(tool) == "" {
			return fmt.Errorf("%w: tool_allowlist must not contain empty entries", ErrInvalidPolicy)
		}
	}
	for _, env := range p.Conditions.Environments {
		if strings.TrimSpace(env) == "" {
			return fmt.Errorf("%w: environments must not contain empty entries", ErrInvalidPolicy)
		}
	}
	return nil
}

// Normalize lowercases effect/resource_type, trims list entries and stamps
// timestamps. It mutates and returns the receiver.
func (p *Policy) Normalize() *Policy {
	p.Effect = strings.ToLower(strings.TrimSpace(p.Effect))
	p.ResourceType = strings.ToLower(strings.TrimSpace(p.ResourceType))
	p.Name = strings.TrimSpace(p.Name)
	p.Actions = trimList(p.Actions)
	p.Conditions.ToolAllowlist = trimList(p.Conditions.ToolAllowlist)
	p.Conditions.Environments = trimList(p.Conditions.Environments)
	now := time.Now().UTC()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	p.UpdatedAt = now
	return p
}

func trimList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Subject identifies the caller for an evaluation request.
type Subject struct {
	UserID   string `json:"user_id"`
	Role     string `json:"role"`
	APIKeyID string `json:"api_key_id"`
}

// Resource is the target of the action being authorized.
type Resource struct {
	Type     string `json:"type"`
	ID       string `json:"id"`
	TenantID string `json:"tenant_id"`
}

// EvalContext carries request-scoped facts the conditions inspect.
type EvalContext struct {
	EstimatedCostCents int64  `json:"estimated_cost_cents"`
	Environment        string `json:"environment"`
	// Tool, when set, overrides resource.id for tool_allowlist matching.
	Tool string `json:"tool,omitempty"`
}

// EvaluateRequest is the POST /policies/evaluate request body.
type EvaluateRequest struct {
	Subject  Subject     `json:"subject"`
	Action   string      `json:"action"`
	Resource Resource    `json:"resource"`
	Context  EvalContext `json:"context"`
}

// Decision is the POST /policies/evaluate response body.
type Decision struct {
	Decision        string `json:"decision"` // allow | deny
	MatchedPolicyID string `json:"matched_policy_id"`
	Reason          string `json:"reason"`
}

// requestTool resolves the tool identity a policy's tool_allowlist matches
// against.
func (r EvaluateRequest) requestTool() string {
	if strings.TrimSpace(r.Context.Tool) != "" {
		return strings.TrimSpace(r.Context.Tool)
	}
	if strings.EqualFold(strings.TrimSpace(r.Resource.Type), ResourceTool) {
		return strings.TrimSpace(r.Resource.ID)
	}
	return ""
}

// matches reports whether policy p applies to req. Every condition that the
// policy specifies must hold (see package doc for per-condition semantics).
func matches(p *Policy, req EvaluateRequest) bool {
	if p == nil || !p.Enabled {
		return false
	}
	// Resource type: exact match or wildcard.
	if p.ResourceType != ResourceWildcard &&
		!strings.EqualFold(strings.TrimSpace(p.ResourceType), strings.TrimSpace(req.Resource.Type)) {
		return false
	}
	// Actions: empty list matches every action.
	if len(p.Actions) > 0 && !containsFold(p.Actions, req.Action) {
		return false
	}
	// tool_allowlist: the request must reference a listed tool.
	if len(p.Conditions.ToolAllowlist) > 0 {
		tool := req.requestTool()
		if tool == "" || !containsFold(p.Conditions.ToolAllowlist, tool) {
			return false
		}
	}
	// environments: the request environment must be listed.
	if len(p.Conditions.Environments) > 0 {
		if strings.TrimSpace(req.Context.Environment) == "" ||
			!containsFold(p.Conditions.Environments, req.Context.Environment) {
			return false
		}
	}
	// max_cost_cents: budget-guard condition — applies to requests whose
	// estimated cost exceeds the cap.
	if p.Conditions.MaxCostCents != nil && req.Context.EstimatedCostCents <= *p.Conditions.MaxCostCents {
		return false
	}
	return true
}

func containsFold(list []string, v string) bool {
	for _, item := range list {
		if strings.EqualFold(strings.TrimSpace(item), strings.TrimSpace(v)) {
			return true
		}
	}
	return false
}

// Evaluate runs the engine over the candidate policies (already scoped to one
// organization). Highest priority wins, deny wins ties, default allow.
func Evaluate(candidates []*Policy, req EvaluateRequest) Decision {
	type scored struct {
		policy *Policy
		index  int
	}
	var matched []scored
	for i, p := range candidates {
		if matches(p, req) {
			matched = append(matched, scored{policy: p, index: i})
		}
	}
	if len(matched) == 0 {
		return Decision{
			Decision: EffectAllow,
			Reason:   "no matching policy; default allow",
		}
	}
	// Sort: priority desc, deny before allow, stable order for determinism.
	sort.SliceStable(matched, func(i, j int) bool {
		a, b := matched[i].policy, matched[j].policy
		if a.Priority != b.Priority {
			return a.Priority > b.Priority
		}
		if (a.Effect == EffectDeny) != (b.Effect == EffectDeny) {
			return a.Effect == EffectDeny
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return matched[i].index < matched[j].index
	})
	winner := matched[0].policy
	reason := fmt.Sprintf("%s by policy %q (priority %d)", winner.Effect, winner.Name, winner.Priority)
	if winner.Conditions.RequireApproval {
		reason += "; approval required before execution"
	}
	if winner.Conditions.MaxCostCents != nil {
		reason += fmt.Sprintf("; estimated cost %d cents exceeds cap %d", req.Context.EstimatedCostCents, *winner.Conditions.MaxCostCents)
	}
	return Decision{
		Decision:        winner.Effect,
		MatchedPolicyID: winner.ID,
		Reason:          reason,
	}
}
