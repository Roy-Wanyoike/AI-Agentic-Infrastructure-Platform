package configgen

// generator_test.go — issue #129 heuristic generator coverage: table-driven
// NL→config mapping (support-inbox → categorize/draft tooling, research →
// search, data → database_query, …), the 5-step progress ordering, model/LLM
// seam injection (fake LLM + failing/blank LLM fallback), determinism, and
// blank-description rejection.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"agentos/internal/tools"
	"agentos/internal/workflows"
)

// newTestGenerator wires the REAL default tools catalog (tools.DefaultRegistry)
// with no LLM: the deterministic heuristic path under test.
func newTestGenerator(t *testing.T) *AgentConfigGenerator {
	t.Helper()
	return NewAgentConfigGenerator(tools.DefaultRegistry(), nil)
}

// --- table-driven heuristic mapping -------------------------------------------

func TestHeuristicAgentGeneration(t *testing.T) {
	tests := []struct {
		name           string
		description    string
		wantIntent     string
		wantTools      []string
		wantModel      string
		wantMaxSteps   int
		wantStrategy   string
		wantTokenCap   int
		noInstructions bool
	}{
		{
			name:         "support inbox maps to categorize/draft tooling",
			description:  "An agent that triages our support inbox: categorize tickets and draft replies for the team",
			wantIntent:   intentSupportInbox,
			wantTools:    []string{"json_transform"}, // categorize/ticket/support keywords
			wantMaxSteps: 10,
			wantStrategy: "sliding_window",
		},
		{
			name:         "research agent gets search",
			description:  "Research assistant that scans the latest competitor documentation and summarizes findings",
			wantIntent:   intentResearch,
			wantTools:    []string{"search", "file_storage"}, // search/scan + report keywords
			wantMaxSteps: 10,
			wantStrategy: "summarize_older", // "summarizes" mentions summarization
		},
		{
			name:         "data agent gets database_query",
			description:  "Query the orders database and export a daily revenue report",
			wantIntent:   intentData,
			wantTools:    []string{"database_query", "file_storage"}, // database/query/export + report
			wantModel:    "gpt-4o-mini",
			wantMaxSteps: 10,
			wantStrategy: "sliding_window",
		},
		{
			name:         "cost-sensitive ops agent caps the token budget",
			description:  "Monitor deployment alerts and notify the oncall channel; keep spend within budget",
			wantIntent:   intentOps,
			wantTools:    []string{"webhook"}, // notify keyword
			wantMaxSteps: 10,
			wantStrategy: "sliding_window",
			wantTokenCap: 100000,
		},
		{
			name:         "simple fast agent gets a small step budget",
			description:  "A simple fast agent that does quick arithmetic totals on invoices",
			wantIntent:   "",                     // no intent keyword fires
			wantTools:    []string{"calculator"}, // arithmetic/totals/invoices keywords
			wantModel:    "gpt-4o-mini",
			wantMaxSteps: 5,
			wantStrategy: "sliding_window",
		},
		{
			name:         "complex multi-step agent upgrades model and steps",
			description:  "A complex multi-step orchestrator that analyzes incidents across services and sends a deep postmortem",
			wantIntent:   intentOps,
			wantTools:    []string{"webhook"}, // sends keyword
			wantModel:    "gpt-4o",
			wantMaxSteps: 20,
			wantStrategy: "sliding_window",
		},
		{
			name:           "plain description stays generic",
			description:    "Help the team write weekly updates",
			wantIntent:     "",
			wantTools:      []string{},
			wantMaxSteps:   10,
			wantStrategy:   "sliding_window",
			noInstructions: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := newTestGenerator(t)
			draft, err := g.Generate(context.Background(), tc.description)
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			if draft.Intent != tc.wantIntent {
				t.Errorf("intent = %q, want %q", draft.Intent, tc.wantIntent)
			}
			if tc.wantModel != "" && draft.Model != tc.wantModel {
				t.Errorf("model = %q, want %q", draft.Model, tc.wantModel)
			}
			if draft.Runtime.MaxSteps != tc.wantMaxSteps {
				t.Errorf("max_steps = %d, want %d", draft.Runtime.MaxSteps, tc.wantMaxSteps)
			}
			if string(draft.Runtime.ConversationHistory.Strategy) != tc.wantStrategy {
				t.Errorf("strategy = %q, want %q", draft.Runtime.ConversationHistory.Strategy, tc.wantStrategy)
			}
			if tc.wantTokenCap != 0 && draft.Runtime.TokenBudget.MaxTotalTokens != tc.wantTokenCap {
				t.Errorf("token cap = %d, want %d", draft.Runtime.TokenBudget.MaxTotalTokens, tc.wantTokenCap)
			}
			got := make([]string, 0, len(draft.Tools))
			for _, tool := range draft.Tools {
				got = append(got, tool.Tool)
			}
			if strings.Join(got, ",") != strings.Join(tc.wantTools, ",") {
				t.Errorf("tools = %v, want %v (reasons: %v)", got, tc.wantTools, draft.Tools)
			}
			if draft.Name == "" {
				t.Error("name is empty")
			}
			if draft.SystemInstructions == "" {
				t.Error("system instructions are empty")
			}
			if !strings.Contains(draft.SystemInstructions, tc.description) {
				t.Error("system instructions do not restate the goal")
			}
			if draft.Policy.Effect != "allow" {
				t.Errorf("policy effect = %q, want allow", draft.Policy.Effect)
			}
		})
	}
}

func TestHeuristicAgentToolsAreCatalogTools(t *testing.T) {
	g := newTestGenerator(t)
	catalog := tools.DefaultRegistry()
	known := map[string]bool{}
	for _, info := range catalog.List() {
		known[info.Name] = true
	}
	for _, desc := range []string{
		"categorize support tickets and draft email replies",
		"search the docs, query the database and run python scripts",
		"upload attachments, post webhooks, fetch urls and calculate totals",
	} {
		draft, err := g.Generate(context.Background(), desc)
		if err != nil {
			t.Fatalf("Generate(%q): %v", desc, err)
		}
		for _, tool := range draft.Tools {
			if !known[tool.Tool] {
				t.Errorf("description %q produced non-catalog tool %q", desc, tool.Tool)
			}
			if tool.Reason == "" {
				t.Errorf("tool %q selected without a documented reason", tool.Tool)
			}
		}
	}
}

// --- policy + runtime shape ----------------------------------------------------

func TestGeneratedPolicyMirrorsTools(t *testing.T) {
	g := newTestGenerator(t)
	draft, err := g.Generate(context.Background(), "search the knowledge base and query the database for records")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	want := map[string]bool{}
	for _, tool := range draft.Tools {
		want[tool.Tool] = true
	}
	if len(want) == 0 {
		t.Fatal("expected tools to be selected")
	}
	if len(draft.Policy.ToolAllowlist) != len(want) {
		t.Fatalf("allowlist %v does not mirror tools %v", draft.Policy.ToolAllowlist, want)
	}
	for _, tool := range draft.Policy.ToolAllowlist {
		if !want[tool] {
			t.Errorf("allowlist entry %q is not a selected tool", tool)
		}
	}
	for i := 1; i < len(draft.Policy.ToolAllowlist); i++ {
		if draft.Policy.ToolAllowlist[i-1] > draft.Policy.ToolAllowlist[i] {
			t.Fatalf("allowlist not sorted: %v", draft.Policy.ToolAllowlist)
		}
	}
}

// --- progress ordering ----------------------------------------------------------

func TestAgentGenerationProgressOrdering(t *testing.T) {
	g := newTestGenerator(t)
	var steps []int
	var partials []AgentConfigDraft
	g.OnProgress(func(step, total int, partial any) {
		if total != GenerationSteps {
			t.Errorf("total = %d, want %d", total, GenerationSteps)
		}
		draft, ok := partial.(AgentConfigDraft)
		if !ok {
			t.Fatalf("partial is %T, want AgentConfigDraft", partial)
		}
		steps = append(steps, step)
		partials = append(partials, draft)
	})
	if _, err := g.Generate(context.Background(), "categorize support tickets and draft replies"); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	want := []int{StepStructure, StepTools, StepPolicy, StepRuntime, StepValidate}
	if len(steps) != len(want) {
		t.Fatalf("got %d callbacks (%v), want %d", len(steps), steps, len(want))
	}
	for i, s := range want {
		if steps[i] != s {
			t.Errorf("steps[%d] = %d, want %d", i, steps[i], s)
		}
	}
	// Partial progress is cumulative: the final snapshot is the returned draft.
	last := partials[len(partials)-1]
	if last.Runtime.MaxSteps == 0 || last.SystemInstructions == "" {
		t.Errorf("final partial snapshot is not the complete draft: %+v", last)
	}
	// Each partial gains structure step by step: step 1 already has a name,
	// step 2 already has tools.
	if partials[0].Name == "" {
		t.Error("step-1 partial is missing the derived name")
	}
	if partials[0].Policy.Effect != "" {
		t.Error("step-1 partial should not carry a policy yet")
	}
	if partials[1].Policy.Effect != "" {
		t.Error("step-2 partial should not carry a policy yet")
	}
	if partials[2].Policy.Effect == "" {
		t.Error("step-3 partial is missing the policy")
	}
}

func TestWorkflowGenerationProgressOrdering(t *testing.T) {
	g := NewWorkflowConfigGenerator(nil)
	var steps []int
	g.OnProgress(func(step, total int, partial any) {
		if _, ok := partial.(WorkflowDSLDraft); !ok {
			t.Fatalf("partial is %T, want WorkflowDSLDraft", partial)
		}
		steps = append(steps, step)
	})
	if _, err := g.Generate(context.Background(), "publish the weekly report after review"); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	want := []int{StepStructure, StepTools, StepPolicy, StepRuntime, StepValidate}
	if len(steps) != len(want) {
		t.Fatalf("got %d callbacks (%v), want %d", len(steps), steps, len(want))
	}
	for i, s := range want {
		if steps[i] != s {
			t.Errorf("steps[%d] = %d, want %d", i, steps[i], s)
		}
	}
}

// --- blank input -----------------------------------------------------------------

func TestGenerateBlankDescriptionRejected(t *testing.T) {
	g := newTestGenerator(t)
	for _, blank := range []string{"", "   ", "\n\t"} {
		if _, err := g.Generate(context.Background(), blank); !errors.Is(err, ErrEmptyDescription) {
			t.Errorf("Generate(%q) = %v, want ErrEmptyDescription", blank, err)
		}
	}
	wg := NewWorkflowConfigGenerator(nil)
	if _, err := wg.Generate(context.Background(), ""); !errors.Is(err, ErrEmptyDescription) {
		t.Errorf("workflow Generate(\"\") = %v, want ErrEmptyDescription", err)
	}
}

// --- determinism -----------------------------------------------------------------

func TestHeuristicGenerationIsDeterministic(t *testing.T) {
	g := newTestGenerator(t)
	desc := "triage support tickets, categorize them, search the docs and calculate refund totals"
	first, err := g.Generate(context.Background(), desc)
	if err != nil {
		t.Fatalf("first Generate: %v", err)
	}
	for i := 0; i < 10; i++ {
		again, err := g.Generate(context.Background(), desc)
		if err != nil {
			t.Fatalf("Generate #%d: %v", i, err)
		}
		if fmt.Sprint(again) != fmt.Sprint(first) {
			t.Fatalf("generation #%d diverged:\n%+v\nvs\n%+v", i, again, first)
		}
	}
}

// --- LLM seam injection -----------------------------------------------------------

// fakeLLM is a deterministic ConfigLLM fake: it records the prompt it was
// given and replies with a canned payload.
type fakeLLM struct {
	prompt   string
	response string
	calls    int
}

func (f *fakeLLM) Generate(ctx context.Context, prompt string) string {
	f.calls++
	f.prompt = prompt
	return f.response
}

func TestFakeLLMDraftIsUsed(t *testing.T) {
	fake := &fakeLLM{response: `{
                "name": "Refund Bot",
                "model": "gpt-4o",
                "system_instructions": "Handle refund requests politely.",
                "tools": [{"tool": "calculator", "reason": "refund math"}]
        }`}
	g := NewAgentConfigGenerator(tools.DefaultRegistry(), fake)
	draft, err := g.Generate(context.Background(), "process refund requests")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("LLM calls = %d, want 1", fake.calls)
	}
	if !strings.Contains(fake.prompt, "process refund requests") {
		t.Errorf("prompt does not carry the description: %q", fake.prompt)
	}
	if draft.Name != "Refund Bot" || draft.Model != "gpt-4o" {
		t.Errorf("LLM-authored fields lost: %+v", draft)
	}
	if draft.SystemInstructions != "Handle refund requests politely." {
		t.Errorf("system instructions = %q", draft.SystemInstructions)
	}
	if len(draft.Tools) != 1 || draft.Tools[0].Tool != "calculator" {
		t.Errorf("tools = %+v, want [calculator]", draft.Tools)
	}
	// Heuristic steps still fill the parts the model did not author.
	if draft.Policy.Effect != "allow" || len(draft.Policy.ToolAllowlist) != 1 {
		t.Errorf("policy not derived: %+v", draft.Policy)
	}
	if draft.Runtime.MaxSteps <= 0 {
		t.Errorf("runtime not derived: %+v", draft.Runtime)
	}
}

func TestFakeLLMUnknownToolsDropped(t *testing.T) {
	fake := &fakeLLM{response: `{"name":"X","model":"gpt-4o-mini","system_instructions":"hi",
                "tools":[{"tool":"make_coffee"},{"tool":"search"}]}`}
	g := NewAgentConfigGenerator(tools.DefaultRegistry(), fake)
	draft, err := g.Generate(context.Background(), "something")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for _, tool := range draft.Tools {
		if tool.Tool == "make_coffee" {
			t.Error("non-catalog tool survived selection")
		}
	}
	if len(draft.Tools) != 1 || draft.Tools[0].Tool != "search" {
		t.Errorf("tools = %+v, want only search", draft.Tools)
	}
}

func TestFakeLLMBlankResponseFallsBackToHeuristics(t *testing.T) {
	fake := &fakeLLM{response: ""}
	g := NewAgentConfigGenerator(tools.DefaultRegistry(), fake)
	draft, err := g.Generate(context.Background(), "categorize support tickets and draft replies")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("LLM calls = %d, want 1", fake.calls)
	}
	if draft.Intent != intentSupportInbox {
		t.Errorf("intent = %q, want heuristic support_inbox", draft.Intent)
	}
	if draft.Model != "gpt-4o-mini" || draft.Name == "" {
		t.Errorf("heuristic structure not applied: %+v", draft)
	}
}

func TestFakeLLMGarbageResponseFallsBackToHeuristics(t *testing.T) {
	fake := &fakeLLM{response: "I am sorry, I cannot answer in JSON."}
	g := NewAgentConfigGenerator(tools.DefaultRegistry(), fake)
	draft, err := g.Generate(context.Background(), "triage support tickets")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if draft.Intent != intentSupportInbox {
		t.Errorf("intent = %q, want heuristic support_inbox", draft.Intent)
	}
	if len(draft.Tools) == 0 {
		t.Error("heuristic tools not applied after unparseable LLM output")
	}
}

// compile-time check: *fakeLLM implements the seam.
var _ ConfigLLM = (*fakeLLM)(nil)

// --- workflow generation -----------------------------------------------------------

func TestWorkflowGenerationChainShapes(t *testing.T) {
	tests := []struct {
		name        string
		description string
		wantTypes   []workflows.NodeType
	}{
		{
			name:        "research → approval → send (webhook)",
			description: "Every Monday, research the latest industry docs, then publish the digest to the team webhook after manager approval",
			wantTypes: []workflows.NodeType{
				workflows.NodeAgent, workflows.NodeApproval, workflows.NodeWebhook, workflows.NodeEnd,
			},
		},
		{
			name:        "delay chain",
			description: "Wait and send the scheduled report later",
			wantTypes: []workflows.NodeType{
				workflows.NodeAgent, workflows.NodeDelay, workflows.NodeTool, workflows.NodeEnd,
			},
		},
		{
			name:        "minimal chain",
			description: "Summarize the daily tickets",
			wantTypes: []workflows.NodeType{
				workflows.NodeAgent, workflows.NodeTool, workflows.NodeEnd,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWorkflowConfigGenerator(nil)
			draft, err := g.Generate(context.Background(), tc.description)
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			if errs := ValidateWorkflowDSL(draft); len(errs) > 0 {
				t.Fatalf("generated workflow invalid: %+v", errs)
			}
			if len(draft.DSL.Nodes) != len(tc.wantTypes) {
				t.Fatalf("nodes = %d (%v), want %d", len(draft.DSL.Nodes), nodeTypes(draft.DSL), len(tc.wantTypes))
			}
			for i, n := range draft.DSL.Nodes {
				if n.Type != tc.wantTypes[i] {
					t.Errorf("nodes[%d].type = %q, want %q", i, n.Type, tc.wantTypes[i])
				}
			}
			// The chain is linear: every node except the first has exactly one
			// incoming edge, in declaration order.
			incoming := map[string]int{}
			for _, e := range draft.DSL.Edges {
				incoming[e.To]++
			}
			for i, n := range draft.DSL.Nodes {
				want := 0
				if i > 0 {
					want = 1
				}
				if incoming[n.ID] != want {
					t.Errorf("node %q has %d incoming edges, want %d", n.ID, incoming[n.ID], want)
				}
			}
		})
	}
}

func nodeTypes(d workflows.DSL) []string {
	out := make([]string, 0, len(d.Nodes))
	for _, n := range d.Nodes {
		out = append(out, string(n.Type))
	}
	return out
}

func TestWorkflowGraphIsAcyclicAndForwardOnly(t *testing.T) {
	g := NewWorkflowConfigGenerator(nil)
	draft, err := g.Generate(context.Background(), "approve and publish the report to the webhook")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	index := map[string]int{}
	for i, n := range draft.DSL.Nodes {
		index[n.ID] = i
	}
	for _, e := range draft.DSL.Edges {
		if index[e.From] >= index[e.To] {
			t.Errorf("edge %s→%s is not forward-only", e.From, e.To)
		}
		if e.Condition != workflows.EdgeOnSuccess {
			t.Errorf("edge %s→%s uses condition %q, want on_success", e.From, e.To, e.Condition)
		}
	}
	if _, err := workflows.TopoOrder(draft.DSL); err != nil {
		t.Errorf("TopoOrder failed on generated graph: %v", err)
	}
}
