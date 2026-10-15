// Package configgen implements LLM-driven auto-configuration (issue #129):
// natural-language descriptions are translated into AgentOS configuration
// drafts — agent configs and workflow DSL graphs — through a step-by-step
// generation pipeline with per-step progress reporting.
//
// SAFETY CONTRACT: generation is DRAFT-ONLY. The generators never create or
// mutate any platform resource; callers review the returned draft and create
// the real resources through the normal endpoints (POST /agents/create,
// POST /workflows). Every generated draft is validated before it is returned
// (see validator.go) so a draft can never be structurally invalid.
//
// The LLM seam is deliberately narrow (ConfigLLM, a single Generate call
// returning raw text). The default implementation is the deterministic
// HeuristicGenerator behavior built into the generators (keyword/intent
// mapping documented in heuristicTools), so tests and offline deployments
// need no model at all; when a real models.Provider is wired, its text is
// parsed as JSON and — if it does not parse or omits required fields — the
// generator falls back to the same deterministic heuristics rather than
// emitting an unvalidated draft.
package configgen

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"agentos/internal/models"
	"agentos/internal/runtime"
	"agentos/internal/tools"
	"agentos/internal/workflows"
)

// Generation steps, exposed through ProgressCallback so a UI can render
// partial progress while a complex config is being generated. Total is always
// GenerationSteps.
const (
	// StepStructure selects the agent identity/structure (name, model,
	// intent classification) — or, for workflows, the node chain skeleton.
	StepStructure = 1
	// StepTools configures the tool batch (catalog tool selection + binding).
	StepTools = 2
	// StepPolicy drafts the policy/permissions (tool allowlist + effect).
	StepPolicy = 3
	// StepRuntime derives the runtime parameters (react step budget,
	// conversation history, token budget).
	StepRuntime = 4
	// StepValidate validates the assembled draft and finalizes it.
	StepValidate = 5

	// GenerationSteps is the total number of generation steps reported to
	// ProgressCallback (issue #129: step-by-step generation).
	GenerationSteps = 5
)

// ConfigLLM is the narrow LLM seam for config generation (issue #129): one
// method, prompt in, raw text out. An empty result means "no model content"
// and makes every generator fall back to its deterministic heuristics. Errors
// are intentionally not part of the seam: a model outage degrades generation
// to the heuristic path instead of failing the draft request — the caller
// always receives a VALID draft.
type ConfigLLM interface {
	// Generate returns the raw model completion for prompt, or "" when the
	// model is unavailable / produced nothing usable.
	Generate(ctx context.Context, prompt string) string
}

// ProviderLLM adapts a models.Provider to the narrow ConfigLLM seam. It uses
// temperature 0 and maps provider failures to "" so the generator's
// deterministic fallback applies.
type ProviderLLM struct {
	// Provider is the underlying completion backend; nil makes Generate
	// always return "".
	Provider models.Provider
}

// Generate implements ConfigLLM on top of models.Provider.Complete.
func (l *ProviderLLM) Generate(ctx context.Context, prompt string) string {
	if l == nil || l.Provider == nil {
		return ""
	}
	resp, err := l.Provider.Complete(ctx, models.CompletionRequest{
		Prompt:      prompt,
		Temperature: 0,
	})
	if err != nil || resp == nil {
		return ""
	}
	return resp.Text
}

// ProgressCallback receives per-step partial progress during generation:
// step is 1-based (StepStructure..StepValidate), total is GenerationSteps and
// partial is the in-progress draft snapshot (AgentConfigDraft or
// WorkflowDSLDraft, depending on the generator). Callbacks must be cheap and
// must not mutate the partial value.
type ProgressCallback func(step, total int, partial any)

// ToolBinding is one catalog tool the generated agent should be allowed to
// call, with the reason the heuristic (or model) selected it — reviewers see
// WHY a tool was proposed, not just what.
type ToolBinding struct {
	// Tool is the catalog tool name (tools.Registry key, e.g. "search").
	Tool string `json:"tool"`
	// Reason documents why the tool was selected for this agent.
	Reason string `json:"reason,omitempty"`
}

// AgentConfigDraft is the reviewable agent configuration a generator
// produces (issue #129). It is a DRAFT: nothing here is persisted or applied;
// the caller reviews it and creates the agent via POST /agents/create
// (name/instructions/model) plus the tool/policy/runtime surfaces it wants to
// adopt. It intentionally mirrors shapes the platform already consumes:
//   - Model/SystemInstructions map onto agents.Agent fields;
//   - Tools references tools.Registry catalog names;
//   - Runtime embeds runtime.RuntimeConfig (conversation_history +
//     token_budget) plus the ReAct step-budget knob.
type AgentConfigDraft struct {
	// Name is a short human-readable agent name derived from the description.
	Name string `json:"name"`
	// Description restates the caller's intent (stored as agents.Description).
	Description string `json:"description,omitempty"`
	// Model is the provider model id (e.g. "gpt-4o-mini").
	Model string `json:"model"`
	// SystemInstructions is the generated system prompt.
	SystemInstructions string `json:"system_instructions"`
	// Tools lists the proposed catalog tool bindings (possibly empty — an
	// agent needs no tools).
	Tools []ToolBinding `json:"tools,omitempty"`
	// Policy is the proposed tool policy draft.
	Policy PolicyDraft `json:"policy"`
	// Runtime carries the react/history/token-budget knobs.
	Runtime RuntimeDraft `json:"runtime"`
	// Intent is the heuristic classification of the description
	// (e.g. "support_inbox", "research"); "" when generic.
	Intent string `json:"intent,omitempty"`
	// Assumptions records every non-obvious decision the generator made so
	// a reviewer can audit the draft without diffing it by hand.
	Assumptions []string `json:"assumptions,omitempty"`
}

// PolicyDraft is the generated tool-policy proposal. Effect is always
// "allow" with an explicit tool_allowlist (the default-deny posture the
// policies service enforces for unlisted tools), because generated configs
// must never widen permissions beyond what the description asked for.
type PolicyDraft struct {
	// Effect is the policy effect; always "allow" for generated drafts.
	Effect string `json:"effect"`
	// ToolAllowlist lists the exact tool names allowed (mirrors
	// policies.Policy.Conditions.ToolAllowlist).
	ToolAllowlist []string `json:"tool_allowlist,omitempty"`
	// Rationale documents the posture in one line.
	Rationale string `json:"rationale,omitempty"`
}

// RuntimeDraft is the generated runtime parameter set: the ReAct step budget
// plus the runtime.RuntimeConfig document (conversation history + token
// budget) the runtime already parses (issue #107). Embedding the runtime
// types lets validation reuse runtime.ParseRuntimeConfig verbatim.
type RuntimeDraft struct {
	// MaxSteps is the ReAct loop step budget (runtime max steps; 0 means the
	// runtime default of 10 applies).
	MaxSteps int `json:"max_steps,omitempty"`
	// ConversationHistory sizes the conversation history window.
	ConversationHistory runtime.ConversationHistoryConfig `json:"conversation_history,omitempty"`
	// TokenBudget caps cumulative run token usage.
	TokenBudget runtime.TokenBudgetConfig `json:"token_budget,omitempty"`
}

// RuntimeDocument renders the runtime_config JSON document (without the react
// knob) that callers can hand to the runtime seams; it round-trips through
// runtime.ParseRuntimeConfig.
func (d RuntimeDraft) RuntimeDocument() string {
	cfg := runtime.RuntimeConfig{
		ConversationHistory: d.ConversationHistory,
		TokenBudget:         d.TokenBudget,
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// WorkflowDSLDraft is the reviewable workflow definition a generator
// produces. It embeds the real workflows.DSL so the validator can reuse
// workflows.ValidateDSL verbatim — only VALID node graphs are ever returned.
type WorkflowDSLDraft struct {
	// Name is a short human-readable workflow name.
	Name string `json:"name"`
	// Description restates the caller's intent.
	Description string `json:"description,omitempty"`
	// DSL is the generated node graph (validated before return).
	DSL workflows.DSL `json:"dsl"`
	// Assumptions records generation decisions for review.
	Assumptions []string `json:"assumptions,omitempty"`
}

// AgentConfigGenerator turns natural-language descriptions into agent config
// drafts. llm may be nil, in which case the deterministic heuristics are used
// exclusively; when set, the model output is used only where it parses into a
// valid draft and the heuristics fill every gap.
type AgentConfigGenerator struct {
	llm       ConfigLLM
	tools     ToolCatalog
	callbacks []ProgressCallback
}

// ToolCatalog is the read-only slice of the tools registry configgen needs:
// the catalog entries a draft's tool bindings must reference. Satisfied by
// *tools.Registry (List).
type ToolCatalog interface {
	List() []tools.ToolInfo
}

// NewAgentConfigGenerator builds a generator. catalog is the tools registry
// the draft tool names are cross-checked against (nil disables the
// cross-check); llm may be nil for pure-heuristic generation.
func NewAgentConfigGenerator(catalog ToolCatalog, llm ConfigLLM) *AgentConfigGenerator {
	return &AgentConfigGenerator{llm: llm, tools: catalog}
}

// OnProgress registers a progress callback (UI seam). Callbacks fire once per
// generation step in order.
func (g *AgentConfigGenerator) OnProgress(cb ProgressCallback) {
	if cb != nil {
		g.callbacks = append(g.callbacks, cb)
	}
}

// Generate runs the 5-step generation pipeline:
//
//	1 structure   — classify the description, derive name/model/instructions
//	2 tools       — batched keyword→catalog tool mapping
//	3 policy      — default-deny tool allowlist draft
//	4 runtime     — react max steps + conversation history + token budget
//	5 validate    — ValidateAgentConfig; drafts that fail validation are
//	                repaired with safe defaults, never returned invalid
//
// The returned draft is always validated (never invalid) and fully
// deterministic for a given description when the LLM seam is nil.
func (g *AgentConfigGenerator) Generate(ctx context.Context, description string) (AgentConfigDraft, error) {
	desc := strings.TrimSpace(description)
	if desc == "" {
		return AgentConfigDraft{}, ErrEmptyDescription
	}

	intent := classifyIntent(desc)
	draft := AgentConfigDraft{
		Description: desc,
		Intent:      intent,
	}

	// Model content, when a seam is wired: parsed first so the heuristic
	// steps can treat missing fields as "keep the deterministic value".
	if modelDraft, ok := g.parseModelDraft(ctx, desc); ok {
		draft.mergeModel(modelDraft)
	}

	// Step 1: structure.
	draft.applyStructure(desc, intent)
	g.emit(ctx, StepStructure, draft)

	// Step 2: tools (batched mapping over the documented keyword table).
	draft.Tools = g.selectTools(desc, intent, draft.Tools)
	g.emit(ctx, StepTools, draft)

	// Step 3: policy/permissions.
	draft.Policy = buildPolicyDraft(draft.Tools)
	g.emit(ctx, StepPolicy, draft)

	// Step 4: runtime parameters.
	draft.Runtime = buildRuntimeDraft(desc, intent, draft.Runtime)
	g.emit(ctx, StepRuntime, draft)

	// Step 5: validate + assemble. Never return an invalid draft: the
	// validator doubles as a repair pass for model-authored fields.
	draft = ValidateAndRepairAgentDraft(draft)
	g.emit(ctx, StepValidate, draft)
	return draft, nil
}

// emit fans one step's partial snapshot out to every registered callback.
func (g *AgentConfigGenerator) emit(ctx context.Context, step int, draft AgentConfigDraft) {
	for _, cb := range g.callbacks {
		cb(step, GenerationSteps, draft)
	}
}

// parseModelDraft asks the LLM seam (when wired) for a structured draft and
// reports whether it produced usable content. Parse failures degrade to
// (false, nil) — the deterministic heuristics take over — never to an error,
// per the ConfigLLM contract.
func (g *AgentConfigGenerator) parseModelDraft(ctx context.Context, description string) (AgentConfigDraft, bool) {
	if g == nil || g.llm == nil {
		return AgentConfigDraft{}, false
	}
	prompt := strings.Join([]string{
		"Translate the following agent description into AgentOS configuration JSON.",
		`Shape: {"name": string, "model": string, "system_instructions": string,`,
		` "tools": [{"tool": string, "reason": string}]}.`,
		"Only use these catalog tool names: calculator, http_request, database_query,",
		"search, code_execute, webhook, json_transform, file_storage.",
		"Respond with JSON only, no prose.",
		"Description: " + description,
	}, "\n")
	raw := g.llm.Generate(ctx, prompt)
	if strings.TrimSpace(raw) == "" {
		return AgentConfigDraft{}, false
	}
	var draft AgentConfigDraft
	if err := json.Unmarshal([]byte(raw), &draft); err != nil {
		return AgentConfigDraft{}, false
	}
	return draft, true
}

// mergeModel copies the LLM-authored fields over the draft. The heuristic
// steps only fill fields the model left empty, so model content wins where it
// exists — but everything is re-validated in step 5.
func (d *AgentConfigDraft) mergeModel(model AgentConfigDraft) {
	d.Name = strings.TrimSpace(model.Name)
	d.Model = strings.TrimSpace(model.Model)
	d.SystemInstructions = strings.TrimSpace(model.SystemInstructions)
	if len(model.Tools) > 0 {
		d.Tools = model.Tools
	}
}

// applyStructure fills the structural fields (name/model/system
// instructions) that are still empty after the (optional) model pass.
func (d *AgentConfigDraft) applyStructure(description, intent string) {
	if d.Name == "" {
		d.Name = deriveName(description)
	}
	if d.Model == "" {
		d.Model = selectModel(description)
	}
	if d.SystemInstructions == "" {
		d.SystemInstructions = buildSystemInstructions(description, intent)
	}
}

// deriveName builds a short deterministic agent name from the description's
// leading words ("Support inbox triage agent for…" → "Support Inbox Triage").
func deriveName(description string) string {
	words := strings.Fields(strings.TrimSpace(description))
	title := make([]string, 0, 3)
	for _, w := range words {
		cleaned := strings.Trim(w, ".,;:!?()[]{}\"'")
		if cleaned == "" {
			continue
		}
		lower := strings.ToLower(cleaned)
		if isStopWord(lower) && len(title) > 0 {
			break
		}
		if len(lower) > 2 { // skip "a", "an", "to", …
			title = append(title, capitalize(lower))
		}
		if len(title) == 3 {
			break
		}
	}
	if len(title) == 0 {
		return "Generated Agent"
	}
	return strings.Join(title, " ")
}

// isStopWord reports whether w is a filler word that should end the derived
// name (deterministic name heuristic).
func isStopWord(w string) bool {
	switch w {
	case "that", "which", "who", "for", "with", "and", "or", "the", "then",
		"when", "where", "to", "from", "by", "on", "in", "of":
		return true
	}
	return false
}

// capitalize upper-cases the first rune of s (name heuristic helper).
func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// selectModel maps description complexity hints onto a model id.
// Deterministic mapping (documented):
//
//	vision/image/screenshot mentioned       → gpt-4o (vision-capable)
//	complex/multi-step/orchestrat/analy…    → gpt-4o (strong reasoning)
//	fast/simple/cheap/light mentioned       → gpt-4o-mini
//	default                                 → gpt-4o-mini (platform seed default)
func selectModel(description string) string {
	d := strings.ToLower(description)
	switch {
	case containsAny(d, "vision", "image", "screenshot", "photo", "picture"):
		return "gpt-4o"
	case containsAny(d, "complex", "multi-step", "multistep", "orchestrat", "analyz", "analys"):
		return "gpt-4o"
	case containsAny(d, "fast", "simple", "cheap", "light"):
		return "gpt-4o-mini"
	default:
		return "gpt-4o-mini"
	}
}

// buildSystemInstructions generates the system prompt skeleton for the intent.
func buildSystemInstructions(description, intent string) string {
	var b strings.Builder
	b.WriteString("You are ")
	b.WriteString(deriveName(description))
	b.WriteString(".\n\nGoal:\n")
	b.WriteString(description)
	b.WriteString("\n\nOperating rules:\n")
	b.WriteString("- Stay within the tools you are given; never fabricate tool results.\n")
	b.WriteString("- Be concise and state assumptions explicitly.\n")
	switch intent {
	case intentSupportInbox:
		b.WriteString("- Categorize every inbound item before acting on it.\n")
		b.WriteString("- Draft replies for review; never send without approval.\n")
	case intentResearch:
		b.WriteString("- Prefer searching available knowledge before answering.\n")
		b.WriteString("- Cite which source each fact came from.\n")
	case intentOps:
		b.WriteString("- Check current state before mutating anything.\n")
		b.WriteString("- Surface errors instead of retrying indefinitely.\n")
	case intentData:
		b.WriteString("- Validate structured input before transforming it.\n")
		b.WriteString("- Prefer read-only queries; escalate writes for approval.\n")
	default:
		b.WriteString("- Ask for clarification when the request is ambiguous.\n")
	}
	return b.String()
}

// Intent labels produced by classifyIntent.
const (
	intentSupportInbox = "support_inbox"
	intentResearch     = "research"
	intentOps          = "ops"
	intentData         = "data"
	intentAutomation   = "automation"
)

// classifyIntent buckets a description into one of the documented intents.
// Deterministic keyword matching; ties resolve by switch order.
func classifyIntent(description string) string {
	d := strings.ToLower(description)
	switch {
	case containsAny(d, "support", "inbox", "ticket", "triage", "categorize", "customer", "reply", "draft"):
		return intentSupportInbox
	case containsAny(d, "research", "search", "knowledge", "literature", "competitive", "scan"):
		return intentResearch
	case containsAny(d, "deploy", "monitor", "incident", "oncall", "on-call", "ops", "infrastructure", "alert"):
		return intentOps
	case containsAny(d, "database", "sql", "report", "analytics", "metric", "export", "transform"):
		return intentData
	case containsAny(d, "automate", "workflow", "schedule", "cron", "pipeline"):
		return intentAutomation
	default:
		return ""
	}
}

// heuristicTools is the DOCUMENTED keyword→catalog-tool mapping table
// (issue #129). The heuristic generator consults it batched: every keyword
// hit adds its tool once with the row's rationale. Tool names are exactly the
// tools.DefaultRegistry catalog names (internal/tools/catalog_builtin.go).
var heuristicTools = []struct {
	keywords []string
	tool     string
	reason   string
}{
	{[]string{"inbox", "email", "ticket", "support", "categorize", "classify", "triage", "parse", "extract", "json", "transform"}, tools.JSONTransformToolName, "structure and categorize inbound items"},
	{[]string{"search", "knowledge", "docs", "documentation", "research", "lookup", "find"}, tools.SearchToolName, "search available knowledge before answering"},
	{[]string{"api", "http", "rest", "endpoint", "fetch", "url", "web", "scrape"}, tools.HTTPToolName, "call external HTTP APIs"},
	{[]string{"database", "sql", "query", "records", "postgres", "data store"}, tools.DatabaseToolName, "query structured records"},
	{[]string{"calculate", "math", "arithmetic", "pricing", "invoice", "billing", "cost", "total"}, tools.NewCalculatorTool().Name(), "compute numeric results"},
	{[]string{"code", "script", "python", "javascript", "execute", "run"}, tools.CodeExecToolName, "execute sandboxed code"},
	{[]string{"notify", "webhook", "callback", "trigger", "send"}, tools.WebhookToolName, "send outbound webhook notifications"},
	{[]string{"file", "document", "storage", "upload", "attachment", "report"}, tools.FileStorageToolName, "store and retrieve files"},
}

// containsAny reports whether s contains any of the keywords.
func containsAny(s string, keywords ...string) bool {
	for _, k := range keywords {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// selectTools resolves the tool set: the LLM-authored bindings (cross-checked
// against the catalog) win; keyword hits from heuristicTools fill the rest.
// Order is deterministic (catalog table order), deduplicated, and every tool
// name is checked against the tools registry when a catalog is wired —
// unknown names are dropped (the validator re-checks too).
func (g *AgentConfigGenerator) selectTools(description, intent string, modelTools []ToolBinding) []ToolBinding {
	catalogNames := g.catalogNames()
	known := func(name string) bool {
		if catalogNames == nil {
			return true // no catalog wired: accept as-is (validator re-checks)
		}
		return catalogNames[name]
	}

	seen := map[string]bool{}
	out := make([]ToolBinding, 0, len(modelTools)+2)
	add := func(binding ToolBinding) {
		name := strings.TrimSpace(binding.Tool)
		if name == "" || seen[name] || !known(name) {
			return
		}
		seen[name] = true
		out = append(out, ToolBinding{Tool: name, Reason: strings.TrimSpace(binding.Reason)})
	}

	for _, t := range modelTools {
		add(t)
	}
	d := strings.ToLower(description)
	for _, row := range heuristicTools {
		if containsAny(d, row.keywords...) {
			add(ToolBinding{Tool: row.tool, Reason: row.reason})
		}
	}
	_ = intent // intent alone never selects tools; only keywords do
	return out
}

// catalogNames snapshots the catalog into a name set (nil-safe).
func (g *AgentConfigGenerator) catalogNames() map[string]bool {
	if g == nil || g.tools == nil {
		return nil
	}
	infos := g.tools.List()
	names := make(map[string]bool, len(infos))
	for _, info := range infos {
		names[info.Name] = true
	}
	return names
}

// buildPolicyDraft derives the default-deny-elsewhere tool policy from the
// selected tools (step 3). Deterministic: effect is always allow, the
// allowlist is exactly the selected tools sorted by name.
func buildPolicyDraft(toolBindings []ToolBinding) PolicyDraft {
	allow := make([]string, 0, len(toolBindings))
	for _, t := range toolBindings {
		allow = append(allow, t.Tool)
	}
	sort.Strings(allow)
	if len(allow) == 0 {
		return PolicyDraft{
			Effect:    "allow",
			Rationale: "no tools selected: agent runs without tool calls (default-deny elsewhere)",
		}
	}
	return PolicyDraft{
		Effect:        "allow",
		ToolAllowlist: allow,
		Rationale:     "default-deny posture: only the tools selected in step 2 are callable",
	}
}

// buildRuntimeDraft derives the runtime knobs (step 4). Deterministic
// mapping (documented):
//
//	max_steps            simple → 5, complex/long → 20, default → 10
//	                     (runtime.DefaultMaxSteps is 10)
//	conversation_history sliding_window @ 8000 tokens (runtime defaults);
//	                     summarize_older when the description mentions
//	                     long-running / conversation / multi-turn
//	token_budget         unlimited by default with the 0.8 alert threshold;
//	                     cost/budget/limit mentions set a 100k total cap
//
// Model-authored values survive when parseable (validated in step 5).
func buildRuntimeDraft(description, intent string, model RuntimeDraft) RuntimeDraft {
	d := strings.ToLower(description)
	draft := model

	if draft.MaxSteps == 0 {
		switch {
		case containsAny(d, "simple", "fast", "quick", "single-step"):
			draft.MaxSteps = 5
		case containsAny(d, "complex", "long", "multi-step", "multistep", "orchestrat", "pipeline", "deep"):
			draft.MaxSteps = 20
		default:
			draft.MaxSteps = runtime.DefaultMaxSteps
		}
	}
	if draft.ConversationHistory.Strategy == "" {
		if containsAny(d, "long-running", "long running", "conversation", "summarize", "multi-turn", "chat") {
			draft.ConversationHistory.Strategy = runtime.StrategySummarizeOlder
		} else {
			draft.ConversationHistory.Strategy = runtime.StrategySlidingWindow
		}
	}
	if draft.ConversationHistory.MaxTokens == 0 {
		draft.ConversationHistory.MaxTokens = runtime.DefaultHistoryMaxTokens
	}
	if draft.TokenBudget.MaxTotalTokens == 0 && !draft.hasAnyBudgetCap() {
		if containsAny(d, "budget", "cost", "expensive", "limit", "cap") {
			draft.TokenBudget.MaxTotalTokens = 100000
		}
	}
	if draft.TokenBudget.AlertThreshold <= 0 {
		draft.TokenBudget.AlertThreshold = runtime.DefaultAlertThreshold
	}
	return draft
}

// hasAnyBudgetCap reports whether any token-budget cap is set.
func (d RuntimeDraft) hasAnyBudgetCap() bool {
	return d.TokenBudget.MaxTotalTokens != 0 ||
		d.TokenBudget.MaxPromptTokens != 0 ||
		d.TokenBudget.MaxCompletionTokens != 0
}

// WorkflowConfigGenerator turns natural-language descriptions into workflow
// DSL drafts. Only the existing DSL node vocabulary is emitted (agent, tool,
// condition, approval, webhook, delay, end) and every edge is a forward
// on_success edge, so workflows.ValidateDSL accepts the graph.
type WorkflowConfigGenerator struct {
	llm       ConfigLLM
	callbacks []ProgressCallback
}

// NewWorkflowConfigGenerator builds a workflow draft generator. llm may be
// nil for pure-heuristic generation.
func NewWorkflowConfigGenerator(llm ConfigLLM) *WorkflowConfigGenerator {
	return &WorkflowConfigGenerator{llm: llm}
}

// OnProgress registers a progress callback (UI seam).
func (g *WorkflowConfigGenerator) OnProgress(cb ProgressCallback) {
	if cb != nil {
		g.callbacks = append(g.callbacks, cb)
	}
}

// Generate runs the same 5-step pipeline for workflows: structure (the node
// chain skeleton), tools (delivery-node selection), policy (approval-gate
// insertion), runtime (pacing/delivery parameters) and validate
// (workflows.ValidateDSL). The generated graphs follow the documented
// trigger → research → approval → send shape: an agent node does the work, a
// tool (or webhook) node delivers the result and an approval node gates the
// send when the description mentions approval/review/sign-off.
func (g *WorkflowConfigGenerator) Generate(ctx context.Context, description string) (WorkflowDSLDraft, error) {
	desc := strings.TrimSpace(description)
	if desc == "" {
		return WorkflowDSLDraft{}, ErrEmptyDescription
	}

	draft := WorkflowDSLDraft{
		Name:        deriveName(description) + " Workflow",
		Description: desc,
	}
	g.emit(ctx, StepStructure, draft)

	lower := strings.ToLower(desc)
	needsApproval := containsAny(lower,
		"approval", "approve", "review", "sign-off", "signoff", "gate", "before send", "publish")
	needsDelay := containsAny(lower, "delay", "wait", "later", "scheduled", "pause")
	needsWebhook := containsAny(lower, "webhook", "callback", "notify", "endpoint", "url")

	draft.DSL = buildWorkflowGraph(needsApproval, needsDelay, needsWebhook)
	g.emit(ctx, StepTools, draft)
	g.emit(ctx, StepPolicy, draft)
	g.emit(ctx, StepRuntime, draft)

	draft = ValidateAndRepairWorkflowDraft(draft)
	g.emit(ctx, StepValidate, draft)
	return draft, nil
}

// emit fans one step's partial snapshot out to every registered callback.
func (g *WorkflowConfigGenerator) emit(ctx context.Context, step int, draft WorkflowDSLDraft) {
	for _, cb := range g.callbacks {
		cb(step, GenerationSteps, draft)
	}
}

// buildWorkflowGraph assembles the deterministic node chain. Every edge is
// forward-only with on_success conditions, keeping graphs acyclic and
// trivially valid. Node ids are stable so reviewers can diff drafts.
//
//	workflow_start (agent: does the work)
//	  → [workflow_pace]  (optional pacing delay, when requested)
//	  → [workflow_gate]  (human approval, when requested)
//	  → workflow_send    (tool or webhook: delivers the result)
//	  → workflow_end
//
// agent_id / tool_id / url carry {{placeholders}} on purpose: the draft is
// NOT applied automatically, and reviewers bind the real agent/tool/URL at
// creation time through the normal endpoints.
func buildWorkflowGraph(needsApproval, needsDelay, needsWebhook bool) workflows.DSL {
	nodes := []workflows.Node{
		{
			ID:   "workflow_start",
			Type: workflows.NodeAgent,
			Name: "Do the work",
			Config: map[string]any{
				// agent_id is a DRAFT placeholder: the caller binds a real
				// agent id at creation time (reviewers must fill it in).
				"agent_id": "{{agent_to_bind}}",
				"inputs":   map[string]any{"prompt": "{{input}}"},
			},
		},
	}
	edges := make([]workflows.Edge, 0, 4)
	tail := "workflow_start"

	if needsDelay {
		nodes = append(nodes, workflows.Node{
			ID:     "workflow_pace",
			Type:   workflows.NodeDelay,
			Name:   "Pacing delay",
			Config: map[string]any{"seconds": 30},
		})
		edges = append(edges, workflows.Edge{From: tail, To: "workflow_pace", Condition: workflows.EdgeOnSuccess})
		tail = "workflow_pace"
	}
	if needsApproval {
		nodes = append(nodes, workflows.Node{
			ID:   "workflow_gate",
			Type: workflows.NodeApproval,
			Name: "Human approval gate",
			Config: map[string]any{
				"approvers":       []string{"OWNER", "ADMIN"},
				"timeout_seconds": 86400,
			},
		})
		edges = append(edges, workflows.Edge{From: tail, To: "workflow_gate", Condition: workflows.EdgeOnSuccess})
		tail = "workflow_gate"
	}
	if needsWebhook {
		nodes = append(nodes, workflows.Node{
			ID:   "workflow_send",
			Type: workflows.NodeWebhook,
			Name: "Deliver result",
			Config: map[string]any{
				// url is a DRAFT placeholder: reviewers fill the real URL.
				"url": "{{delivery_webhook_url}}",
			},
		})
	} else {
		nodes = append(nodes, workflows.Node{
			ID:   "workflow_send",
			Type: workflows.NodeTool,
			Name: "Deliver result",
			Config: map[string]any{
				// tool_id is a DRAFT placeholder: reviewers bind a real tool.
				"tool_id": "{{tool_to_bind}}",
			},
		})
	}
	edges = append(edges, workflows.Edge{From: tail, To: "workflow_send", Condition: workflows.EdgeOnSuccess})

	nodes = append(nodes, workflows.Node{
		ID:   "workflow_end",
		Type: workflows.NodeEnd,
		Name: "Done",
	})
	edges = append(edges, workflows.Edge{From: "workflow_send", To: "workflow_end", Condition: workflows.EdgeOnSuccess})

	return workflows.DSL{Nodes: nodes, Edges: edges}
}
