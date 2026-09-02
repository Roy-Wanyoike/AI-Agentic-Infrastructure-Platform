package policies

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func costPtr(v int64) *int64 { return &v }

// mkPolicy builds a minimal valid policy for engine tests.
func mkPolicy(id, name, effect, resourceType string, actions []string, priority int, conds Conditions) *Policy {
	return &Policy{
		ID:           id,
		Name:         name,
		Effect:       effect,
		ResourceType: resourceType,
		Actions:      actions,
		Conditions:   conds,
		Priority:     priority,
		Enabled:      true,
	}
}

func TestEvaluateDenyWinsOverAllowAtSamePriority(t *testing.T) {
	allow := mkPolicy("p-allow", "Allow tools", EffectAllow, ResourceWildcard, nil, 100, Conditions{})
	deny := mkPolicy("p-deny", "Deny tools", EffectDeny, ResourceWildcard, nil, 100, Conditions{})
	// Order in the candidate list must not matter.
	for _, candidates := range [][]*Policy{{allow, deny}, {deny, allow}} {
		got := Evaluate(candidates, EvaluateRequest{
			Action:   "tools.call",
			Resource: Resource{Type: ResourceTool, ID: "httpx"},
		})
		if got.Decision != EffectDeny {
			t.Fatalf("expected deny to win, got %+v", got)
		}
		if got.MatchedPolicyID != "p-deny" {
			t.Fatalf("expected deny policy id, got %q", got.MatchedPolicyID)
		}
	}
}

func TestEvaluateHighestPriorityWins(t *testing.T) {
	lowDeny := mkPolicy("p-low", "Low deny", EffectDeny, ResourceTool, []string{"tools.call"}, 10, Conditions{})
	highAllow := mkPolicy("p-high", "High allow", EffectAllow, ResourceTool, []string{"tools.call"}, 500, Conditions{})
	got := Evaluate([]*Policy{lowDeny, highAllow}, EvaluateRequest{
		Action:   "tools.call",
		Resource: Resource{Type: ResourceTool, ID: "httpx"},
	})
	if got.Decision != EffectAllow || got.MatchedPolicyID != "p-high" {
		t.Fatalf("expected highest priority allow, got %+v", got)
	}
}

func TestEvaluateTableDriven(t *testing.T) {
	policies := []*Policy{
		{
			ID: "p-deny-prod", Name: "No prod tools", Effect: EffectDeny,
			ResourceType: ResourceTool, Actions: []string{"tools.call"},
			Conditions: Conditions{Environments: []string{"production"}}, Priority: 100, Enabled: true,
		},
		{
			ID: "p-deny-expensive", Name: "Cost cap", Effect: EffectDeny,
			ResourceType: ResourceWildcard, Actions: []string{},
			Conditions: Conditions{MaxCostCents: costPtr(500)}, Priority: 90, Enabled: true,
		},
		{
			ID: "p-allow-search", Name: "Allow search", Effect: EffectAllow,
			ResourceType: ResourceTool, Actions: []string{"tools.call"},
			Conditions: Conditions{ToolAllowlist: []string{"search"}}, Priority: 80, Enabled: true,
		},
		{
			ID: "p-deny-disabled", Name: "Disabled", Effect: EffectDeny,
			ResourceType: ResourceWildcard, Actions: []string{}, Priority: 1000, Enabled: false,
		},
		{
			ID: "p-approval", Name: "Workflow approval", Effect: EffectAllow,
			ResourceType: ResourceWorkflow, Actions: []string{"workflows.execute"},
			Conditions: Conditions{RequireApproval: true}, Priority: 50, Enabled: true,
		},
	}

	tests := []struct {
		name        string
		req         EvaluateRequest
		want        string
		wantMatched string
		wantReason  string // substring; empty = skip
	}{
		{
			name: "tool in production is denied by environment condition",
			req: EvaluateRequest{
				Action:   "tools.call",
				Resource: Resource{Type: ResourceTool, ID: "httpx"},
				Context:  EvalContext{Environment: "production"},
			},
			want:        EffectDeny,
			wantMatched: "p-deny-prod",
			wantReason:  "deny by policy",
		},
		{
			name: "tool in development falls through to allowlist policy for non-search tool",
			req: EvaluateRequest{
				Action:   "tools.call",
				Resource: Resource{Type: ResourceTool, ID: "httpx"},
				Context:  EvalContext{Environment: "development"},
			},
			want: EffectAllow,
		},
		{
			name: "search tool in development matches allowlist policy",
			req: EvaluateRequest{
				Action:   "tools.call",
				Resource: Resource{Type: ResourceTool, ID: "search"},
				Context:  EvalContext{Environment: "development"},
			},
			want:        EffectAllow,
			wantMatched: "p-allow-search",
		},
		{
			name: "over-budget request is denied regardless of environment",
			req: EvaluateRequest{
				Action:   "tools.call",
				Resource: Resource{Type: ResourceTool, ID: "search"},
				Context:  EvalContext{Environment: "development", EstimatedCostCents: 501},
			},
			want:        EffectDeny,
			wantMatched: "p-deny-expensive",
			wantReason:  "exceeds cap 500",
		},
		{
			name: "at-cap request is not over budget",
			req: EvaluateRequest{
				Action:   "tools.call",
				Resource: Resource{Type: ResourceTool, ID: "search"},
				Context:  EvalContext{Environment: "development", EstimatedCostCents: 500},
			},
			want:        EffectAllow,
			wantMatched: "p-allow-search",
		},
		{
			name: "disabled policies never match",
			req: EvaluateRequest{
				Action:   "agents.invoke",
				Resource: Resource{Type: ResourceAgent, ID: "a-1"},
			},
			want: EffectAllow,
		},
		{
			name: "no matching policy defaults to allow with reason",
			req: EvaluateRequest{
				Action:   "deployments.create",
				Resource: Resource{Type: ResourceDeployment, ID: "d-1"},
			},
			want:       EffectAllow,
			wantReason: "no matching policy; default allow",
		},
		{
			name: "wildcard resource type policy catches agent requests over budget",
			req: EvaluateRequest{
				Action:   "runs.execute",
				Resource: Resource{Type: ResourceAgent, ID: "agent-1"},
				Context:  EvalContext{EstimatedCostCents: 900},
			},
			want:        EffectDeny,
			wantMatched: "p-deny-expensive",
		},
		{
			name: "workflow execution carries approval annotation in reason",
			req: EvaluateRequest{
				Action:   "workflows.execute",
				Resource: Resource{Type: ResourceWorkflow, ID: "wf-1"},
			},
			want:        EffectAllow,
			wantMatched: "p-approval",
			wantReason:  "approval required",
		},
		{
			name: "context.tool overrides resource id for allowlist matching",
			req: EvaluateRequest{
				Action:   "tools.call",
				Resource: Resource{Type: ResourceTool, ID: "httpx"},
				Context:  EvalContext{Tool: "search", Environment: "development"},
			},
			want:        EffectAllow,
			wantMatched: "p-allow-search",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Evaluate(policies, tc.req)
			if got.Decision != tc.want {
				t.Fatalf("decision = %q, want %q (full: %+v)", got.Decision, tc.want, got)
			}
			if tc.wantMatched != "" && got.MatchedPolicyID != tc.wantMatched {
				t.Fatalf("matched_policy_id = %q, want %q", got.MatchedPolicyID, tc.wantMatched)
			}
			if tc.wantReason != "" && !strings.Contains(got.Reason, tc.wantReason) {
				t.Fatalf("reason %q does not contain %q", got.Reason, tc.wantReason)
			}
		})
	}
}

func TestEvaluateTieBreakDeterministic(t *testing.T) {
	// Equal priority, equal effect, equal created_at -> first candidate wins.
	first := mkPolicy("p-a", "First", EffectDeny, ResourceWildcard, nil, 10, Conditions{})
	second := mkPolicy("p-b", "Second", EffectDeny, ResourceWildcard, nil, 10, Conditions{})
	first.CreatedAt = now
	second.CreatedAt = now
	got := Evaluate([]*Policy{first, second}, EvaluateRequest{Action: "tools.call"})
	if got.MatchedPolicyID != "p-a" {
		t.Fatalf("expected stable winner p-a, got %q", got.MatchedPolicyID)
	}
}

// now pins created_at ordering tests to a deterministic instant.
var now = time.Now().UTC()

func TestPolicyValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Policy)
		wantErr bool
	}{
		{"valid minimal", func(p *Policy) {}, false},
		{"missing name", func(p *Policy) { p.Name = " " }, true},
		{"bad effect", func(p *Policy) { p.Effect = "maybe" }, true},
		{"bad resource type", func(p *Policy) { p.ResourceType = "lambda" }, true},
		{"empty action entry", func(p *Policy) { p.Actions = []string{"runs.execute", " "} }, true},
		{"negative max cost", func(p *Policy) { p.Conditions.MaxCostCents = costPtr(-1) }, true},
		{"empty allowlist entry", func(p *Policy) { p.Conditions.ToolAllowlist = []string{""} }, true},
		{"empty environment entry", func(p *Policy) { p.Conditions.Environments = []string{""} }, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := mkPolicy("id", "n", EffectDeny, ResourceTool, nil, 0, Conditions{})
			tc.mutate(p)
			err := p.Validate()
			if tc.wantErr && err == nil {
				t.Fatal("expected validation error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestServiceCRUDOrgScoping(t *testing.T) {
	svc := NewService()
	ctx := context.Background()

	created, err := svc.CreatePolicyCtx(ctx, "org-1", &Policy{
		Name: "Deny prod tools", Effect: EffectDeny, ResourceType: ResourceTool,
		Actions: []string{"tools.call"}, Conditions: Conditions{Environments: []string{"production"}},
		Priority: 100, Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreatePolicyCtx: %v", err)
	}
	if created.ID == "" || created.OrganizationID != "org-1" || created.CreatedAt.IsZero() {
		t.Fatalf("created policy not populated: %+v", created)
	}

	// Listing is tenant-scoped.
	if _, err := svc.CreatePolicyCtx(ctx, "org-2", &Policy{
		Name: "Other org", Effect: EffectAllow, ResourceType: ResourceWildcard, Enabled: true,
	}); err != nil {
		t.Fatalf("CreatePolicyCtx org-2: %v", err)
	}
	list1, err := svc.ListPoliciesCtx(ctx, "org-1")
	if err != nil {
		t.Fatalf("ListPoliciesCtx: %v", err)
	}
	if len(list1) != 1 || list1[0].ID != created.ID {
		t.Fatalf("org-1 list leaked: %+v", list1)
	}

	// Cross-tenant get/delete surfaces as not found.
	if _, err := svc.GetPolicyCtx(ctx, "org-2", created.ID); !errors.Is(err, ErrPolicyNotFound) {
		t.Fatalf("expected ErrPolicyNotFound for cross-tenant get, got %v", err)
	}
	if err := svc.DeletePolicyCtx(ctx, "org-2", created.ID); !errors.Is(err, ErrPolicyNotFound) {
		t.Fatalf("expected ErrPolicyNotFound for cross-tenant delete, got %v", err)
	}

	// Full update replaces fields and bumps updated_at.
	updated, err := svc.UpdatePolicyCtx(ctx, "org-1", created.ID, &Policy{
		Name: "Allow prod tools", Effect: EffectAllow, ResourceType: ResourceTool,
		Actions: []string{"tools.call"}, Priority: 200, Enabled: true,
	})
	if err != nil {
		t.Fatalf("UpdatePolicyCtx: %v", err)
	}
	if updated.Effect != EffectAllow || updated.Priority != 200 {
		t.Fatalf("update not applied: %+v", updated)
	}
	if !updated.UpdatedAt.After(created.UpdatedAt) && !updated.UpdatedAt.Equal(created.UpdatedAt) {
		t.Fatalf("updated_at should advance: %v -> %v", created.UpdatedAt, updated.UpdatedAt)
	}

	// Evaluation reflects the update.
	decision, err := svc.EvaluateCtx(ctx, "org-1", EvaluateRequest{
		Action:   "tools.call",
		Resource: Resource{Type: ResourceTool, ID: "httpx"},
		Context:  EvalContext{Environment: "production"},
	})
	if err != nil {
		t.Fatalf("EvaluateCtx: %v", err)
	}
	if decision.Decision != EffectAllow || decision.MatchedPolicyID != created.ID {
		t.Fatalf("unexpected decision: %+v", decision)
	}

	if err := svc.DeletePolicyCtx(ctx, "org-1", created.ID); err != nil {
		t.Fatalf("DeletePolicyCtx: %v", err)
	}
	if _, err := svc.GetPolicyCtx(ctx, "org-1", created.ID); !errors.Is(err, ErrPolicyNotFound) {
		t.Fatalf("expected not found after delete, got %v", err)
	}
}

func TestServiceCreateValidation(t *testing.T) {
	svc := NewService()
	if _, err := svc.CreatePolicyCtx(context.Background(), "org-1", &Policy{
		Name: "Bad", Effect: "maybe", ResourceType: ResourceTool,
	}); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("expected ErrInvalidPolicy, got %v", err)
	}
	if _, err := svc.CreatePolicyCtx(context.Background(), "", &Policy{
		Name: "No org", Effect: EffectDeny, ResourceType: ResourceTool,
	}); err == nil {
		t.Fatal("expected org id requirement error")
	}
}
