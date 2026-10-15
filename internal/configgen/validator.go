package configgen

import (
	"errors"
	"sort"
	"strings"

	"agentos/internal/runtime"
	"agentos/internal/workflows"
)

// ErrEmptyDescription is returned by both generators when the caller submits
// a blank description: there is nothing to translate.
var ErrEmptyDescription = errors.New("configgen: description is required")

// ValidateAgentConfig checks an agent draft against the same invariants the
// agents service enforces at creation time (name/instructions/model are
// required — internal/agents.Service.CreateAgentCtx) plus configgen-specific
// rules: tool names must exist in the catalog, the policy effect must be
// allow/deny with a non-empty allowlist, and the runtime draft must parse
// through runtime.ParseRuntimeConfig (the exact seam the runner uses). It
// returns the list of problems; an empty slice means the draft is valid.
func ValidateAgentConfig(draft AgentConfigDraft, catalog ToolCatalog) []string {
	issues := make([]string, 0)

	if strings.TrimSpace(draft.Name) == "" {
		issues = append(issues, "name is required")
	}
	if strings.TrimSpace(draft.Model) == "" {
		issues = append(issues, "model is required")
	}
	if strings.TrimSpace(draft.SystemInstructions) == "" {
		issues = append(issues, "system_instructions are required")
	}

	catalogNames := catalogNames(catalog)
	seen := map[string]bool{}
	for _, t := range draft.Tools {
		name := strings.TrimSpace(t.Tool)
		switch {
		case name == "":
			issues = append(issues, "tools: empty tool name")
		case seen[name]:
			issues = append(issues, "tools: duplicate tool "+name)
		case len(catalogNames) > 0 && !catalogNames[name]:
			issues = append(issues, "tools: unknown catalog tool "+name)
		}
		seen[name] = true
	}

	switch strings.ToLower(strings.TrimSpace(draft.Policy.Effect)) {
	case "", "allow", "deny":
	default:
		issues = append(issues, "policy: effect must be allow or deny")
	}
	allowSeen := map[string]bool{}
	for _, tool := range draft.Policy.ToolAllowlist {
		tool = strings.TrimSpace(tool)
		if tool == "" {
			issues = append(issues, "policy: tool_allowlist contains an empty entry")
			continue
		}
		if allowSeen[tool] {
			issues = append(issues, "policy: tool_allowlist duplicates "+tool)
		}
		allowSeen[tool] = true
	}

	if draft.Runtime.MaxSteps < 0 {
		issues = append(issues, "runtime: max_steps must be >= 0")
	}
	if draft.Runtime.ConversationHistory.MaxTokens < 0 {
		issues = append(issues, "runtime: conversation_history.max_tokens must be >= 0")
	}
	if s := draft.Runtime.ConversationHistory.Strategy; s != "" &&
		s != runtime.StrategySlidingWindow &&
		s != runtime.StrategySummarizeOlder &&
		s != runtime.StrategyDropOldest {
		issues = append(issues, "runtime: unknown conversation_history strategy "+string(s))
	}
	tb := draft.Runtime.TokenBudget
	if tb.MaxTotalTokens < 0 || tb.MaxPromptTokens < 0 || tb.MaxCompletionTokens < 0 {
		issues = append(issues, "runtime: token_budget limits must be >= 0")
	}
	if tb.AlertThreshold < 0 || tb.AlertThreshold > 1 {
		issues = append(issues, "runtime: token_budget.alert_threshold must be within (0, 1]")
	}
	// Round-trip through the runner's own parser: if the runtime would reject
	// this document at run time, the draft is invalid NOW.
	doc := draft.Runtime.RuntimeDocument()
	if _, err := runtime.ParseRuntimeConfig([]byte(doc)); err != nil {
		issues = append(issues, "runtime: runtime_config rejected by runtime.ParseRuntimeConfig: "+err.Error())
	}
	return issues
}

// catalogNames snapshots a ToolCatalog into a name set (nil-safe: nil yields
// nil, which disables the catalog cross-check).
func catalogNames(catalog ToolCatalog) map[string]bool {
	if catalog == nil {
		return nil
	}
	names := make(map[string]bool)
	for _, info := range catalog.List() {
		names[info.Name] = true
	}
	return names
}

// ValidateAndRepairAgentDraft validates the draft and repairs every fixable
// problem with safe deterministic defaults, so a generator can NEVER return
// an invalid draft (issue #129 gate: every generated config validates).
// Unrepairable fields cannot exist here — every field has a safe default —
// so the repair pass is total.
func ValidateAndRepairAgentDraft(draft AgentConfigDraft) AgentConfigDraft {
	// Structure repairs (mirror agents.Service creation rules).
	if strings.TrimSpace(draft.Name) == "" {
		draft.Name = deriveName(draft.Description)
	}
	if strings.TrimSpace(draft.Model) == "" {
		draft.Model = selectModel(draft.Description)
	}
	if strings.TrimSpace(draft.SystemInstructions) == "" {
		draft.SystemInstructions = buildSystemInstructions(draft.Description, draft.Intent)
	}

	// Tools: drop empty/duplicate names (validator rules), keeping declaration
	// order. Catalog existence was cross-checked at selection time; here the
	// repair only guarantees structural well-formedness.
	seen := map[string]bool{}
	tools := make([]ToolBinding, 0, len(draft.Tools))
	for _, t := range draft.Tools {
		name := strings.TrimSpace(t.Tool)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		tools = append(tools, ToolBinding{Tool: name, Reason: strings.TrimSpace(t.Reason)})
	}
	draft.Tools = tools

	// Policy: allow effect with exactly the selected tools.
	if strings.TrimSpace(draft.Policy.Effect) == "" {
		draft.Policy.Effect = "allow"
	}
	allowSeen := map[string]bool{}
	allow := make([]string, 0, len(draft.Policy.ToolAllowlist))
	for _, tool := range draft.Policy.ToolAllowlist {
		tool = strings.TrimSpace(tool)
		if tool == "" || allowSeen[tool] {
			continue
		}
		allowSeen[tool] = true
		allow = append(allow, tool)
	}
	sort.Strings(allow)
	draft.Policy.ToolAllowlist = allow

	// Runtime: safe defaults everywhere (see buildRuntimeDraft's table).
	if draft.Runtime.MaxSteps <= 0 {
		draft.Runtime.MaxSteps = runtime.DefaultMaxSteps
	}
	if draft.Runtime.ConversationHistory.Strategy != runtime.StrategySlidingWindow &&
		draft.Runtime.ConversationHistory.Strategy != runtime.StrategySummarizeOlder &&
		draft.Runtime.ConversationHistory.Strategy != runtime.StrategyDropOldest {
		draft.Runtime.ConversationHistory.Strategy = runtime.StrategySlidingWindow
	}
	if draft.Runtime.ConversationHistory.MaxTokens < 0 {
		draft.Runtime.ConversationHistory.MaxTokens = runtime.DefaultHistoryMaxTokens
	}
	if draft.Runtime.ConversationHistory.MaxTokens == 0 {
		draft.Runtime.ConversationHistory.MaxTokens = runtime.DefaultHistoryMaxTokens
	}
	tb := draft.Runtime.TokenBudget
	if tb.MaxTotalTokens < 0 {
		tb.MaxTotalTokens = 0
	}
	if tb.MaxPromptTokens < 0 {
		tb.MaxPromptTokens = 0
	}
	if tb.MaxCompletionTokens < 0 {
		tb.MaxCompletionTokens = 0
	}
	if tb.AlertThreshold <= 0 || tb.AlertThreshold > 1 {
		tb.AlertThreshold = runtime.DefaultAlertThreshold
	}
	draft.Runtime.TokenBudget = tb

	// Post-condition: the repaired draft must pass validation. This cannot
	// fire (every field has a total repair above) but the guard documents the
	// never-invalid contract and fails loudly if a future field regresses.
	if issues := ValidateAgentConfig(draft, nil); len(issues) > 0 {
		panic("configgen: repaired agent draft still invalid: " + strings.Join(issues, "; "))
	}
	return draft
}

// ValidateWorkflowDSL validates a workflow draft by delegating to the
// workflows service's own validator (workflows.ValidateDSL) — the exact rules
// POST /workflows enforces at creation time. An empty result means the graph
// is valid.
func ValidateWorkflowDSL(draft WorkflowDSLDraft) []workflows.ValidationError {
	return workflows.ValidateDSL(draft.DSL)
}

// ValidateAndRepairWorkflowDraft validates the graph and, if it could not
// pass validation (only possible for caller-assembled drafts — generated
// graphs are valid by construction), falls back to the minimal valid
// generated chain, recording the substitution as an assumption. The returned
// draft is always valid: the guard below fails loudly otherwise.
func ValidateAndRepairWorkflowDraft(draft WorkflowDSLDraft) WorkflowDSLDraft {
	if errs := ValidateWorkflowDSL(draft); len(errs) > 0 {
		draft.DSL = buildWorkflowGraph(false, false, false)
		draft.Assumptions = append(draft.Assumptions,
			"generated graph was replaced with the minimal valid chain because the assembled graph failed validation")
	}
	if errs := ValidateWorkflowDSL(draft); len(errs) > 0 {
		panic("configgen: repaired workflow draft still invalid")
	}
	return draft
}
