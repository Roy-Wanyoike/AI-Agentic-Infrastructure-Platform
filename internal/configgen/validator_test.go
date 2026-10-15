package configgen

// validator_test.go — issue #129 gate: EVERY generated config must validate.
// Covers ValidateAgentConfig / ValidateAndRepairAgentDraft against the
// services' own rules (runtime.ParseRuntimeConfig round-trip, workflows
// ValidateDSL reuse), repair of model-authored garbage, and the never-invalid
// post-condition for both generators across a description matrix.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"agentos/internal/runtime"
	"agentos/internal/tools"
	"agentos/internal/workflows"
)

// fakeCatalog is a minimal ToolCatalog stub.
type fakeCatalog struct{ names []string }

func (f fakeCatalog) List() []tools.ToolInfo {
	out := make([]tools.ToolInfo, 0, len(f.names))
	for _, n := range f.names {
		out = append(out, tools.ToolInfo{Name: n})
	}
	return out
}

func TestValidateAgentConfigAcceptsGeneratedDraft(t *testing.T) {
	g := NewAgentConfigGenerator(tools.DefaultRegistry(), nil)
	draft, err := g.Generate(context.Background(), "categorize support tickets, search the docs and draft replies")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if issues := ValidateAgentConfig(draft, tools.DefaultRegistry()); len(issues) != 0 {
		t.Fatalf("generated draft invalid: %v", issues)
	}
}

func TestValidateAgentConfigDetectsProblems(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*AgentConfigDraft)
		wantIssue string
	}{
		{"blank name", func(d *AgentConfigDraft) { d.Name = " " }, "name is required"},
		{"blank model", func(d *AgentConfigDraft) { d.Model = "" }, "model is required"},
		{"blank instructions", func(d *AgentConfigDraft) { d.SystemInstructions = "" }, "system_instructions are required"},
		{"unknown tool", func(d *AgentConfigDraft) { d.Tools = []ToolBinding{{Tool: "make_coffee"}} }, "unknown catalog tool"},
		{"empty tool name", func(d *AgentConfigDraft) { d.Tools = []ToolBinding{{Tool: "  "}} }, "empty tool name"},
		{"duplicate tool", func(d *AgentConfigDraft) {
			d.Tools = []ToolBinding{{Tool: "search"}, {Tool: "search"}}
		}, "duplicate tool"},
		{"bad effect", func(d *AgentConfigDraft) { d.Policy.Effect = "maybe" }, "effect must be allow or deny"},
		{"empty allowlist entry", func(d *AgentConfigDraft) {
			d.Policy.Effect = "allow"
			d.Policy.ToolAllowlist = []string{"search", " "}
		}, "empty entry"},
		{"negative max steps", func(d *AgentConfigDraft) { d.Runtime.MaxSteps = -1 }, "max_steps must be >= 0"},
		{"unknown strategy", func(d *AgentConfigDraft) {
			d.Runtime.ConversationHistory.Strategy = "yolo"
		}, "unknown conversation_history strategy"},
		{"negative budget", func(d *AgentConfigDraft) { d.Runtime.TokenBudget.MaxTotalTokens = -5 }, "token_budget limits must be >= 0"},
		{"bad alert threshold", func(d *AgentConfigDraft) { d.Runtime.TokenBudget.AlertThreshold = 1.5 }, "alert_threshold must be within (0, 1]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewAgentConfigGenerator(tools.DefaultRegistry(), nil)
			draft, err := g.Generate(context.Background(), "categorize support tickets")
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			tc.mutate(&draft)
			issues := ValidateAgentConfig(draft, tools.DefaultRegistry())
			found := false
			for _, issue := range issues {
				if strings.Contains(issue, tc.wantIssue) {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("issues %v do not contain %q", issues, tc.wantIssue)
			}
		})
	}
}

func TestValidateAgentConfigNilCatalogSkipsCrossCheck(t *testing.T) {
	g := NewAgentConfigGenerator(nil, nil)
	draft, err := g.Generate(context.Background(), "categorize support tickets")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	draft.Tools = []ToolBinding{{Tool: "anything_goes"}}
	if issues := ValidateAgentConfig(draft, nil); len(issues) != 0 {
		t.Fatalf("nil catalog should disable the cross-check, got %v", issues)
	}
}

func TestRepairAgentDraftNeverInvalid(t *testing.T) {
	// Model-authored garbage + an empty structure: the repair pass must turn
	// this into a valid draft, not an error.
	garbage := AgentConfigDraft{
		Description: "an agent",
		Name:        " ",
		Model:       "",
		Tools:       []ToolBinding{{Tool: ""}, {Tool: "search"}, {Tool: "search"}},
		Policy:      PolicyDraft{Effect: "", ToolAllowlist: []string{"search", " ", "search"}},
		Runtime: RuntimeDraft{
			MaxSteps:            -3,
			ConversationHistory: runtime.ConversationHistoryConfig{Strategy: "nope", MaxTokens: -1},
			TokenBudget:         runtime.TokenBudgetConfig{MaxTotalTokens: -1, AlertThreshold: 9},
		},
	}
	repaired := ValidateAndRepairAgentDraft(garbage)
	if issues := ValidateAgentConfig(repaired, nil); len(issues) != 0 {
		t.Fatalf("repaired draft invalid: %v", issues)
	}
	if repaired.Name == "" || repaired.Model == "" || repaired.SystemInstructions == "" {
		t.Fatalf("structure not repaired: %+v", repaired)
	}
	if len(repaired.Tools) != 1 || repaired.Tools[0].Tool != "search" {
		t.Errorf("tools not deduplicated: %+v", repaired.Tools)
	}
	if got := repaired.Policy.Effect; got != "allow" {
		t.Errorf("effect = %q, want allow", got)
	}
	if repaired.Runtime.MaxSteps != runtime.DefaultMaxSteps {
		t.Errorf("max_steps = %d, want default", repaired.Runtime.MaxSteps)
	}
	if repaired.Runtime.ConversationHistory.Strategy != runtime.StrategySlidingWindow {
		t.Errorf("strategy = %q, want sliding_window", repaired.Runtime.ConversationHistory.Strategy)
	}
	if repaired.Runtime.TokenBudget.AlertThreshold != runtime.DefaultAlertThreshold {
		t.Errorf("alert threshold = %v, want default", repaired.Runtime.TokenBudget.AlertThreshold)
	}
}

func TestRuntimeDocumentRoundTripsThroughRuntimeParser(t *testing.T) {
	g := NewAgentConfigGenerator(tools.DefaultRegistry(), nil)
	draft, err := g.Generate(context.Background(), "long-running conversation agent that summarizes chats within budget")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	cfg, err := runtime.ParseRuntimeConfig([]byte(draft.Runtime.RuntimeDocument()))
	if err != nil {
		t.Fatalf("runtime rejected the generated runtime_config: %v", err)
	}
	if cfg.ConversationHistory.Strategy != draft.Runtime.ConversationHistory.Strategy {
		t.Errorf("strategy round-trip mismatch: %q vs %q", cfg.ConversationHistory.Strategy, draft.Runtime.ConversationHistory.Strategy)
	}
	if cfg.TokenBudget.MaxTotalTokens != draft.Runtime.TokenBudget.MaxTotalTokens {
		t.Errorf("token cap round-trip mismatch")
	}
	// The document must marshal to the documented runtime_config shape.
	var raw map[string]any
	if err := json.Unmarshal([]byte(draft.Runtime.RuntimeDocument()), &raw); err != nil {
		t.Fatalf("runtime document is not JSON: %v", err)
	}
	if _, ok := raw["conversation_history"]; !ok {
		t.Error("runtime document missing conversation_history")
	}
}

// TestEveryGeneratedAgentConfigValidates is the issue #129 validity gate:
// a matrix of descriptions × generator configurations must all produce drafts
// that pass ValidateAgentConfig with the REAL catalog.
func TestEveryGeneratedAgentConfigValidates(t *testing.T) {
	descriptions := []string{
		"an agent that triages the support inbox, categorizes tickets and drafts replies",
		"research new AI papers and summarize them weekly",
		"watch the deployments dashboard and notify oncall on incidents via webhook",
		"query the analytics database and export monthly reports",
		"run python scripts to clean uploaded attachment files",
		"a simple calculator helper for invoice totals",
		"complex multi-step competitor analysis with screenshots (vision)",
		"",
		"     ",
		"just be helpful",
		"deal with 订单数据库 and 发票 billing invoices", // unicode keywords
	}
	for _, withLLM := range []bool{false, true} {
		for _, desc := range descriptions {
			name := "llm=" + boolLabel(withLLM) + "/" + shortLabel(desc)
			t.Run(name, func(t *testing.T) {
				var llm ConfigLLM
				if withLLM {
					llm = &fakeLLM{response: `{"name":"M","model":"gpt-4o","system_instructions":"hi",
						"tools":[{"tool":"search"},{"tool":"bogus"}]}`}
				}
				g := NewAgentConfigGenerator(tools.DefaultRegistry(), llm)
				draft, err := g.Generate(context.Background(), desc)
				if desc == "" || strings.TrimSpace(desc) == "" {
					if err == nil {
						t.Fatal("blank description must be rejected")
					}
					return
				}
				if err != nil {
					t.Fatalf("Generate: %v", err)
				}
				if issues := ValidateAgentConfig(draft, tools.DefaultRegistry()); len(issues) != 0 {
					t.Fatalf("generated draft invalid: %v — draft %+v", issues, draft)
				}
			})
		}
	}
}

// TestEveryGeneratedWorkflowValidates is the workflow half of the gate: all
// generated graphs pass workflows.ValidateDSL.
func TestEveryGeneratedWorkflowValidates(t *testing.T) {
	descriptions := []string{
		"publish the weekly report after manager approval",
		"research docs then notify the webhook",
		"wait a day and send the scheduled digest",
		"summarize tickets",
		"approve expense requests, then post them to the finance webhook",
	}
	for _, desc := range descriptions {
		t.Run(shortLabel(desc), func(t *testing.T) {
			g := NewWorkflowConfigGenerator(nil)
			draft, err := g.Generate(context.Background(), desc)
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			if errs := ValidateWorkflowDSL(draft); len(errs) > 0 {
				t.Fatalf("generated workflow invalid: %+v", errs)
			}
			// Structural extras: acyclic, forward-only, topo-orderable.
			if _, err := workflows.TopoOrder(draft.DSL); err != nil {
				t.Fatalf("TopoOrder: %v", err)
			}
		})
	}
}

func TestValidateWorkflowDSLDetectsInvalidGraph(t *testing.T) {
	draft := WorkflowDSLDraft{
		Name: "broken",
		DSL: workflows.DSL{
			Nodes: []workflows.Node{{ID: "a", Type: workflows.NodeAgent}},
			Edges: []workflows.Edge{{From: "a", To: "ghost", Condition: workflows.EdgeOnSuccess}},
		},
	}
	errs := ValidateWorkflowDSL(draft)
	if len(errs) == 0 {
		t.Fatal("dangling edge must be reported")
	}
	// Repair falls back to the minimal valid chain.
	repaired := ValidateAndRepairWorkflowDraft(draft)
	if errs := ValidateWorkflowDSL(repaired); len(errs) != 0 {
		t.Fatalf("repaired workflow invalid: %+v", errs)
	}
	if len(repaired.Assumptions) == 0 {
		t.Error("repair must record an assumption note")
	}
}

func boolLabel(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// shortLabel makes a readable, deterministic subtest name from a description.
func shortLabel(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 24 {
		s = s[:24]
	}
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "blank"
	}
	return b.String()
}
