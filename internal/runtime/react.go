// react.go — issue #134: the ReACT (Reason + Act) agent pattern on top of the
// existing bounded runtime loop.
//
// DESIGN CHOICE (option (a) from the issue): a ReACTRunner WRAPS the existing
// *Runner and drives its own loop — runtime.go is unchanged (zero lines
// touched). This is possible because every seam the ReACT loop needs is
// already exported or package-visible on Runner, so the wrapper REUSES the
// production machinery instead of duplicating it:
//
//   - model calls: rr.provider (WithProvider), the model-scope rate limit
//     (checkModelRateLimit) and post-call TPM accounting (reportModelTokens);
//   - tool execution: Runner.executeTool — the EXISTING tool/policy seam
//     (governance policy #75, agent-key allow-lists, granular rate limits
//     #124, per-call timeout) — so a denied tool is never invoked in ReACT
//     runs either;
//   - step records: Runner.record (StepRecorder + agentos_tools_total
//     metrics);
//   - token budgets: budget.go (NewTokenBudget, BudgetState, the typed
//     ErrTokenBudgetExceeded outcome, budget annotation steps and the
//     run.token_budget_exceeded event) — reused verbatim, not duplicated;
//   - conversation history: history.go (ConversationHistory strategies,
//     Summarizer) — the ReACT loop appends turns and prompts through the
//     same budgeted Prompt view;
//   - observability: trace_hooks.go run/step/model/tool spans;
//   - resilience: checkpoint.go end-of-step snapshots and resume, the
//     shutdown gate (begin/finish, ErrShuttingDown), the wall-clock budget
//     and graceful cancellation (unwindCancelled).
//
// What ReACT adds on top of one model call per step:
//
//  1. MULTIPLE TOOL CALLS PER STEP: the model may reply with
//     {"tools": [{"tool": ..., "arguments": ...}, ...]} (a bare single-call
//     object {"tool": ..., "arguments": ...} — the base runtime's shape —
//     and a top-level JSON array of calls are also accepted). Up to
//     ReactConfig.MaxToolCallsPerStep (default 3) requests are executed per
//     step, in order; observations are fed back as tool messages before the
//     next step.
//
//  2. REFLECTION ROUNDS: when the model produces a draft (non-tool) answer,
//     a quality assessment (QualityAssessment: Completeness / Accuracy /
//     Coherence in [0,1] plus ShouldReflect) decides whether to continue
//     with additional tool calls and a refined answer. Up to
//     MaxReflectionRounds (default 2) rounds; a negative value disables
//     reflection. The assessor is, in order of precedence: the injected
//     Assessor seam (WithAssessor — owns its own resource accounting), the
//     AssessmentLLM seam (WithAssessmentProvider — the runner itself
//     performs a budgeted, recorded model call that must reply with a JSON
//     QualityAssessment; any failure degrades to the heuristic), and finally
//     the deterministic HeuristicAssessor (documented below).
//     StopOnSatisfactory halts reflection as soon as all three scores are
//     >= QualityThreshold, regardless of ShouldReflect.
//
//  3. LONG-OUTPUT CHUNKING: for long-form tasks (documented heuristic
//     trigger: estimated output length > LongOutputThresholdTokens —
//     see estimateLongOutputTokens) the answer is produced step by step:
//     a section-planning model call (JSON array of section titles, capped at
//     MaxSections) followed by a bounded generate-with-tool-support sub-loop
//     per section, assembled into a single document. Chunked mode is itself
//     the refinement granularity, so the reflection pass does not run on top
//     of it (documented trade-off: bounded cost).
//
// Every model call — main loop, planning, sections, LLM assessment — shares
// ONE step budget (Runner.maxSteps) and ONE token budget, so the ReACT
// pattern stays a bounded, auditable loop like the base runtime.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"agentos/internal/agents"
	"agentos/internal/events"
	"agentos/internal/models"
	"agentos/internal/tools"
)

// StepTypeReflection marks the quality-assessment annotation step recorded on
// the run timeline whenever a draft answer is assessed (issue #134): the
// structured QualityAssessment JSON rides in the step Output so reflection
// decisions are auditable alongside model/tool steps. The run_steps store
// keeps step_type as free-form TEXT, so this additive type needs no schema
// change.
const StepTypeReflection = "reflection"

// ReACT defaults (issue #134). All are safe, bounded values; per-agent
// runtime_config `react` overrides them (see ReactConfig).
const (
	// DefaultMaxToolCallsPerStep caps the tool requests honored from ONE
	// model reply (the default from the issue).
	DefaultMaxToolCallsPerStep = 3
	// DefaultMaxReflectionRounds is the default number of assess-and-refine
	// rounds after the first draft answer.
	DefaultMaxReflectionRounds = 2
	// DefaultQualityThreshold is the default all-dimensions bar behind
	// StopOnSatisfactory.
	DefaultQualityThreshold = 0.8
	// DefaultLongOutputThresholdTokens is the estimated-output-length trigger
	// above which long-output chunked generation kicks in.
	DefaultLongOutputThresholdTokens = 1200
	// DefaultMaxSections caps the section plan of chunked generation.
	DefaultMaxSections = 8
	// DefaultReflectionPrompt is the default instruction appended to the
	// reflection feedback message (the documented default from the issue).
	DefaultReflectionPrompt = "Review your draft answer against the quality assessment below. " +
		"Identify what is missing or wrong, call tools if needed to fill the gaps, " +
		"then reply with a refined final answer as plain text."
)

// Heuristic assessor knobs (all deterministic; see HeuristicAssessor).
const (
	// heuristicMinDraftTokens: below this estimated length a draft is
	// considered thin (penalizes completeness/accuracy).
	heuristicMinDraftTokens = 32
	// heuristicSubstantiveTokens: at/above this estimated length a draft
	// counts as fully elaborated (bonus in completeness/coherence).
	heuristicSubstantiveTokens = 96
	// heuristicSatisfactoryBar: the heuristic reports ShouldReflect when the
	// lowest dimension is below this bar.
	heuristicSatisfactoryBar = 0.9
	// keywordLongOutputEstimateTokens is the estimated output size implied by
	// long-form keywords in the input (report/essay/...).
	keywordLongOutputEstimateTokens = 1600
	// sectionMaxModelCalls bounds each section's generate-act-refine sub-loop
	// of chunked generation (documented): one drafting call plus up to two
	// tool-assisted rewrites.
	sectionMaxModelCalls = 3
)

// failureMarkers are the draft substrings the heuristic treats as unresolved
// execution failures (accuracy penalty). Deliberately short and aligned with
// the runtime's own failure vocabulary.
var failureMarkers = []string{"tool error", "denied by policy", "rate limited", "not registered"}

// ReactConfig is the `react` half of the per-agent runtime_config document
// (issue #134). It is parsed from the SAME runtime_config JSON that carries
// conversation_history/token_budget — but independently, in THIS file, so
// budget.go/RuntimeConfig stay untouched (additive parsing; that document's
// decoder ignores unknown keys by design).
type ReactConfig struct {
	// MaxToolCallsPerStep caps tool requests honored per model reply.
	// 0 applies DefaultMaxToolCallsPerStep; negative values are rejected by
	// ParseReactConfig.
	MaxToolCallsPerStep int `json:"max_tool_calls_per_step,omitempty"`
	// MaxReflectionRounds bounds the assess-and-refine loop after the first
	// draft (0 applies DefaultMaxReflectionRounds; a NEGATIVE value is the
	// documented explicit disable, mirroring history.MaxTokens semantics).
	MaxReflectionRounds int `json:"max_reflection_rounds,omitempty"`
	// ReflectionPrompt overrides the instruction appended to the reflection
	// feedback message (empty applies DefaultReflectionPrompt).
	ReflectionPrompt string `json:"reflection_prompt,omitempty"`
	// Temperature overrides the sampling temperature of every ReACT model
	// call (0 = provider/agent default; values must be within (0,2] or
	// ParseReactConfig rejects the document).
	Temperature float64 `json:"temperature,omitempty"`
	// StopOnSatisfactory halts reflection as soon as all quality dimensions
	// are >= QualityThreshold, even when the assessor says ShouldReflect.
	// Known merge limitation (same class as the base runtime's merge): the
	// boolean has no "unset" JSON representation, so only an override of
	// `true` changes it — configure "off" runner-wide.
	StopOnSatisfactory bool `json:"stop_on_satisfactory,omitempty"`
	// QualityThreshold is the all-dimensions bar behind StopOnSatisfactory
	// (0 applies DefaultQualityThreshold; must be within (0,1]).
	QualityThreshold float64 `json:"quality_threshold,omitempty"`
	// LongOutputThresholdTokens is the estimated-output-length trigger for
	// chunked long-output generation (0 applies
	// DefaultLongOutputThresholdTokens; negative disables chunking).
	LongOutputThresholdTokens int `json:"long_output_threshold_tokens,omitempty"`
	// MaxSections caps the section plan of chunked generation (0 applies
	// DefaultMaxSections).
	MaxSections int `json:"max_sections,omitempty"`
}

// WithDefaults fills unset ReactConfig fields with the issue #134 safe
// defaults. A negative MaxReflectionRounds (explicit reflection disable) and
// a negative LongOutputThresholdTokens (explicit chunking disable) are
// preserved.
func (c ReactConfig) WithDefaults() ReactConfig {
	if c.MaxToolCallsPerStep <= 0 {
		c.MaxToolCallsPerStep = DefaultMaxToolCallsPerStep
	}
	if c.MaxReflectionRounds == 0 {
		c.MaxReflectionRounds = DefaultMaxReflectionRounds
	}
	if c.ReflectionPrompt == "" {
		c.ReflectionPrompt = DefaultReflectionPrompt
	}
	if c.QualityThreshold <= 0 {
		c.QualityThreshold = DefaultQualityThreshold
	}
	if c.LongOutputThresholdTokens == 0 {
		c.LongOutputThresholdTokens = DefaultLongOutputThresholdTokens
	}
	if c.MaxSections <= 0 {
		c.MaxSections = DefaultMaxSections
	}
	return c
}

// reactConfigDoc is the wrapper shape ParseReactConfig decodes: the `react`
// object inside the per-agent runtime_config document.
type reactConfigDoc struct {
	React ReactConfig `json:"react"`
}

// ParseReactConfig decodes the `react` object from a per-agent runtime_config
// JSON document ({"react": {...}}). An empty or blank document parses to the
// zero config (defaults apply via WithDefaults). It returns an error for
// malformed JSON or invalid values: a negative max_tool_calls_per_step, a
// quality_threshold outside (0,1], a temperature outside (0,2] or a negative
// max_sections. Callers that must never break a run on bad config (the
// ReACTRunner) treat the error as "keep the base react configuration".
// Unset fields stay zero: merge before applying WithDefaults.
func ParseReactConfig(raw []byte) (ReactConfig, error) {
	var doc reactConfigDoc
	if len(strings.TrimSpace(string(raw))) == 0 {
		return ReactConfig{}, nil
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ReactConfig{}, fmt.Errorf("runtime: invalid runtime_config react section: %w", err)
	}
	cfg := doc.React
	if cfg.MaxToolCallsPerStep < 0 {
		return ReactConfig{}, fmt.Errorf("runtime: invalid runtime_config: react.max_tool_calls_per_step must be >= 0 (0 = default %d)", DefaultMaxToolCallsPerStep)
	}
	if cfg.QualityThreshold < 0 || cfg.QualityThreshold > 1 {
		return ReactConfig{}, fmt.Errorf("runtime: invalid runtime_config: react.quality_threshold must be within (0, 1]")
	}
	if cfg.Temperature < 0 || cfg.Temperature > 2 {
		return ReactConfig{}, fmt.Errorf("runtime: invalid runtime_config: react.temperature must be within (0, 2] (0 = provider default)")
	}
	if cfg.MaxSections < 0 {
		return ReactConfig{}, fmt.Errorf("runtime: invalid runtime_config: react.max_sections must be >= 0 (0 = default %d)", DefaultMaxSections)
	}
	return cfg, nil
}

// mergeReactConfig overlays the per-agent react override on the base config:
// only fields the override SETS (non-zero) win; everything else keeps the
// base value. Parsed-but-unset fields must therefore stay zero in
// ParseReactConfig (they do).
func mergeReactConfig(base, override ReactConfig) ReactConfig {
	if override.MaxToolCallsPerStep != 0 {
		base.MaxToolCallsPerStep = override.MaxToolCallsPerStep
	}
	if override.MaxReflectionRounds != 0 {
		base.MaxReflectionRounds = override.MaxReflectionRounds
	}
	if override.ReflectionPrompt != "" {
		base.ReflectionPrompt = override.ReflectionPrompt
	}
	if override.Temperature != 0 {
		base.Temperature = override.Temperature
	}
	if override.StopOnSatisfactory {
		base.StopOnSatisfactory = true
	}
	if override.QualityThreshold != 0 {
		base.QualityThreshold = override.QualityThreshold
	}
	if override.LongOutputThresholdTokens != 0 {
		base.LongOutputThresholdTokens = override.LongOutputThresholdTokens
	}
	if override.MaxSections != 0 {
		base.MaxSections = override.MaxSections
	}
	return base
}

// --- quality self-assessment (issue #134) -----------------------------------

// QualityAssessment is the structured verdict on a draft answer. The three
// scores are in [0,1]; ShouldReflect asks for another refine round (the
// runner still bounds rounds by ReactConfig.MaxReflectionRounds and honors
// StopOnSatisfactory).
type QualityAssessment struct {
	Completeness  float64 `json:"completeness"`
	Accuracy      float64 `json:"accuracy"`
	Coherence     float64 `json:"coherence"`
	ShouldReflect bool    `json:"should_reflect"`
	// Notes is the free-text rationale (assessor-specific; "heuristic" for
	// the default assessor).
	Notes string `json:"notes,omitempty"`
}

// minDimension returns the lowest of the three scores (the StopOnSatisfactory
// criterion requires ALL dimensions to clear the threshold).
func (a QualityAssessment) minDimension() float64 {
	return min(a.Completeness, min(a.Accuracy, a.Coherence))
}

// satisfied reports whether every dimension clears threshold.
func (a QualityAssessment) satisfied(threshold float64) bool {
	return a.Completeness >= threshold && a.Accuracy >= threshold && a.Coherence >= threshold
}

// clamp01 pins a score to [0,1] (NaN-safe: any comparison with NaN is false,
// so NaN falls through to the 0 branch).
func clamp01(v float64) float64 {
	if !(v > 0) {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// AssessRequest carries everything an assessor needs to evaluate one draft.
type AssessRequest struct {
	// Question is the original user input the draft answers.
	Question string
	// DraftAnswer is the candidate final answer.
	DraftAnswer string
	// Observations are the tool observations gathered so far in the run
	// (may be empty for single-shot answers).
	Observations []string
	// RoundsRemaining is how many reflection rounds the runner would still
	// allow (advisory context for the assessor).
	RoundsRemaining int
}

// Assessor evaluates draft answers for the reflection loop (issue #134
// seam). Implementations must be safe for concurrent use only if the runner
// is shared across goroutines; the ReACT loop itself is single-goroutine per
// run. NOTE: an injected assessor OWNS its own resource accounting — only
// the built-in AssessmentLLM path flows through the runner's token budget
// and step records.
type Assessor interface {
	Assess(ctx context.Context, req AssessRequest) (QualityAssessment, error)
}

// AssessorFunc adapts a plain function to Assessor.
type AssessorFunc func(ctx context.Context, req AssessRequest) (QualityAssessment, error)

// Assess implements Assessor.
func (f AssessorFunc) Assess(ctx context.Context, req AssessRequest) (QualityAssessment, error) {
	return f(ctx, req)
}

// HeuristicAssessor is the deterministic default Assessor (issue #134): no
// model call, fully offline, a pure function of (draft, observations).
// Documented heuristics, each dimension clamped to [0,1]:
//
//   - Completeness — coverage-of-tool-outputs: base 0.5; when observations
//     exist, +0.25 if any salient token (a number or a word of 6+ chars)
//     from any observation appears in the draft, -0.25 otherwise; with no
//     observations, +0.25 when the draft reaches heuristicMinDraftTokens.
//     A draft of at least heuristicSubstantiveTokens earns a further +0.25.
//   - Accuracy — unresolved-failure markers: base 0.5; -0.25 when the draft
//     itself carries a failure marker ("tool error", "denied by policy",
//     "rate limited", "not registered") instead of a resolved answer;
//     +0.25 when the draft reaches heuristicMinDraftTokens.
//   - Coherence — structure: base 0.5; +0.25 for 2+ sentence-ish segments;
//     +0.25 when the draft reaches heuristicSubstantiveTokens or ends with
//     terminal punctuation.
//   - ShouldReflect — true when the lowest dimension is below
//     heuristicSatisfactoryBar (0.9): a draft that is not clearly complete
//     earns another round, subject to the runner's rounds bound.
type HeuristicAssessor struct{}

// Assess implements Assessor with the documented deterministic heuristics.
func (HeuristicAssessor) Assess(_ context.Context, req AssessRequest) (QualityAssessment, error) {
	draft := strings.TrimSpace(req.DraftAnswer)
	if draft == "" {
		return QualityAssessment{ShouldReflect: true, Notes: "empty draft"}, nil
	}
	tokens := EstimateTokens(draft)

	completeness := 0.5
	if len(req.Observations) > 0 {
		if observationCovered(draft, req.Observations) {
			completeness += 0.25
		} else {
			completeness -= 0.25
		}
	} else if tokens >= heuristicMinDraftTokens {
		completeness += 0.25
	}
	if tokens >= heuristicSubstantiveTokens {
		completeness += 0.25
	}

	accuracy := 0.5
	if draftHasFailureMarkers(draft) {
		accuracy -= 0.25
	}
	if tokens >= heuristicMinDraftTokens {
		accuracy += 0.25
	}

	coherence := 0.5
	if countSentences(draft) >= 2 {
		coherence += 0.25
	}
	if tokens >= heuristicSubstantiveTokens || strings.ContainsAny(draft[len(draft)-1:], ".!?") {
		coherence += 0.25
	}

	a := QualityAssessment{
		Completeness: clamp01(completeness),
		Accuracy:     clamp01(accuracy),
		Coherence:    clamp01(coherence),
		Notes:        "heuristic",
	}
	a.ShouldReflect = a.minDimension() < heuristicSatisfactoryBar
	return a, nil
}

// salientToken matches the observation tokens most likely to be quoted
// verbatim in a faithful answer: numbers (integer or decimal) and words of
// 6+ characters.
var salientToken = regexp.MustCompile(`[0-9]+(?:\.[0-9]+)?|[A-Za-z_][A-Za-z0-9_-]{5,}`)

// observationCovered reports whether ANY salient token of any observation
// appears in the draft (deterministic scan order; first hit wins).
func observationCovered(draft string, observations []string) bool {
	for _, obs := range observations {
		for _, tok := range salientToken.FindAllString(obs, -1) {
			if tok != "" && strings.Contains(draft, tok) {
				return true
			}
		}
	}
	return false
}

// draftHasFailureMarkers reports whether the draft contains any unresolved
// failure marker (see failureMarkers).
func draftHasFailureMarkers(draft string) bool {
	lower := strings.ToLower(draft)
	for _, m := range failureMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// countSentences counts non-empty sentence-ish segments (. ! ? or newline
// separated).
func countSentences(draft string) int {
	n := 0
	for _, part := range strings.FieldsFunc(draft, func(r rune) bool {
		return r == '.' || r == '!' || r == '?' || r == '\n'
	}) {
		if strings.TrimSpace(part) != "" {
			n++
		}
	}
	return n
}

// --- the ReACT runner --------------------------------------------------------

// ReACTRunner executes agents with the ReACT pattern (issue #134): multiple
// tool calls per reasoning step, configurable reflection rounds with quality
// self-assessment, and chunked long-output generation. It WRAPS an existing
// *Runner (option (a) of the issue) and reuses its provider, tool/policy
// seam, budget, history, checkpoints, traces, metrics and shutdown gate —
// runtime.go is not modified. Construct with NewReACTRunner.
type ReACTRunner struct {
	*Runner

	// reactCfg is the runner-wide default react configuration; per-agent
	// runtime_config `react` documents overlay it (resolveReactConfig).
	reactCfg ReactConfig
	// assessor is the optional custom assessment seam (highest precedence).
	assessor Assessor
	// assessmentProvider + assessmentModel implement the AssessmentLLM seam:
	// the runner performs the assessment model call itself (budgeted,
	// recorded) and parses the structured QualityAssessment; failures degrade
	// to the heuristic assessor.
	assessmentProvider models.Provider
	assessmentModel    string
}

// ReACTOption configures a ReACTRunner.
type ReACTOption func(*ReACTRunner)

// WithReactConfig sets the runner-wide default react configuration
// (issue #134). Per-agent runtime_config `react` documents (read through the
// wrapped Runner's WithAgentConfigSource) overlay it.
func WithReactConfig(cfg ReactConfig) ReACTOption {
	return func(rr *ReACTRunner) { rr.reactCfg = cfg }
}

// WithAssessor injects a custom assessment seam for the reflection loop
// (highest precedence over the AssessmentLLM seam and the heuristic). The
// injected assessor owns its own resource accounting.
func WithAssessor(a Assessor) ReACTOption {
	return func(rr *ReACTRunner) { rr.assessor = a }
}

// WithAssessmentProvider wires the AssessmentLLM seam: reflection quality is
// evaluated by a model call through the given provider (recorded and
// token-budgeted like every other model call). The response must parse as a
// JSON QualityAssessment; ANY failure (transport, malformed JSON, invalid
// scores) degrades to the deterministic heuristic — assessment must never
// break a run. An empty model id falls back to the agent's model.
func WithAssessmentProvider(p models.Provider, model string) ReACTOption {
	return func(rr *ReACTRunner) {
		if p != nil {
			rr.assessmentProvider = p
			rr.assessmentModel = model
		}
	}
}

// NewReACTRunner builds a ReACTRunner on top of an already-configured base
// Runner (provider, recorder, policy, limiter, metrics, checkpoints, event
// publisher, runtime config — everything the base loop uses). The wrapped
// runner must be non-nil. With a nil provider on the base runner the ReACT
// runner delegates to the base offline behavior (documented: ReACT is a
// provider-mode pattern).
func NewReACTRunner(base *Runner, opts ...ReACTOption) *ReACTRunner {
	rr := &ReACTRunner{Runner: base}
	for _, opt := range opts {
		if opt != nil {
			opt(rr)
		}
	}
	return rr
}

// resolveReactConfig computes the effective react configuration for one run:
// the runner-wide default (WithReactConfig) overlaid with the agent's
// runtime_config `react` object read through the base Runner's agent config
// source, then WithDefaults. A nil source, an absent document or an invalid
// document keeps the base configuration (bad config must never break a run —
// the same doctrine as resolveRuntimeConfig).
func (rr *ReACTRunner) resolveReactConfig(agentID string) ReactConfig {
	cfg := rr.reactCfg
	if src := rr.agentCfgSrc; src != nil {
		if raw := src(agentID); len(strings.TrimSpace(string(raw))) > 0 {
			if override, err := ParseReactConfig(raw); err == nil {
				cfg = mergeReactConfig(cfg, override)
			}
		}
	}
	return cfg.WithDefaults()
}

// Run executes an agent with a freshly generated run ID (mirrors Runner.Run).
func (rr *ReACTRunner) Run(ctx context.Context, agentID, input string) (*Run, error) {
	return rr.RunWithID(ctx, newRunID(), agentID, input)
}

// RunWithID executes an agent under the caller-supplied run ID with the ReACT
// loop. Offline mode (no provider on the wrapped Runner) delegates to the
// base Runner so legacy deterministic behavior is preserved unchanged.
func (rr *ReACTRunner) RunWithID(ctx context.Context, runID, agentID, input string) (*Run, error) {
	if rr == nil || rr.Runner == nil {
		return nil, errors.New("runtime: react runner requires a base runner")
	}
	if rr.provider == nil {
		// Documented delegation: ReACT is a provider-mode pattern; the
		// offline deterministic path stays exactly the base runtime's.
		return rr.Runner.RunWithID(ctx, runID, agentID, input)
	}
	if rr.agentService == nil {
		return nil, errors.New("runtime: agent service is required")
	}
	if strings.TrimSpace(agentID) == "" {
		return nil, errors.New("runtime: agent id is required")
	}
	if strings.TrimSpace(runID) == "" {
		runID = newRunID()
	}

	// Issue #107 shutdown gate: ReACT runs participate in Stop/Shutdown
	// draining exactly like base runs (inflight slot released on the way
	// out).
	if err := rr.begin(); err != nil {
		return nil, err
	}
	defer rr.finish()

	agent, ok := rr.agentService.Get(agentID)
	if !ok {
		return nil, errors.New("runtime: agent not found")
	}

	// Wall-clock budget: the caller's deadline wins when tighter (parity
	// with Runner.RunWithID).
	deadline := time.Now().Add(rr.maxRuntime)
	if existing, ok := ctx.Deadline(); ok && existing.Before(deadline) {
		deadline = existing
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	started := time.Now().UTC()
	run := &Run{
		ID:        runID,
		AgentID:   agentID,
		Input:     input,
		Status:    StatusRunning,
		CreatedAt: started,
		UpdatedAt: started,
	}

	// Issue #114 trace hierarchy root (run + agent spans); nil when tracing
	// is disabled.
	runCtx, tr := rr.startRunTrace(ctx, run, agent)
	ctx = runCtx
	defer tr.end(run)

	output, err := rr.executeReact(ctx, run, agent, input)
	run.UpdatedAt = time.Now().UTC()
	if err != nil {
		run.Status = StatusFailed
		run.Error = err.Error()
		return run, err
	}
	run.Output = output
	run.Status = StatusCompleted
	return run, nil
}

// reactState is the shared per-run state of the ReACT loop: everything the
// main loop, the chunked long-output path and the reflection path touch.
type reactState struct {
	runner  *ReACTRunner
	run     *Run
	agent   *agents.Agent
	react   ReactConfig          // resolved react config
	budget  *TokenBudget         // may be nil (unlimited; nil-safe Record)
	history *ConversationHistory // history.go strategies apply unchanged
	system  string
	orgID   string
	step    int // last used step index (1-based; 0 = none yet)
}

// nextStep reserves the next step index; ok is false when the step budget
// (Runner.maxSteps) is exhausted. Every model call of the ReACT pattern —
// main loop, section planning, sections, LLM assessment — consumes exactly
// one step, so the whole pattern stays bounded by maxSteps.
func (s *reactState) nextStep() (int, bool) {
	if s.step >= s.runner.maxSteps {
		return 0, false
	}
	s.step++
	s.run.Steps = s.step
	return s.step, true
}

// executeReact drives the ReACT loop for one run. The error taxonomy matches
// the base runtime exactly (ErrMaxStepsExceeded, ErrMaxRuntimeExceeded,
// ErrLoopDetected, ErrTokenBudgetExceeded, graceful cancellation), so callers
// and the runs API need no new classifications.
func (rr *ReACTRunner) executeReact(ctx context.Context, run *Run, agent *agents.Agent, input string) (string, error) {
	// Issue #107 configuration: history + token budget resolve exactly as in
	// the base loop (reused, not duplicated).
	cfg := rr.resolveRuntimeConfig(agent.ID)
	reactCfg := rr.resolveReactConfig(agent.ID)
	budget := NewTokenBudget(cfg.TokenBudget)
	history := NewConversationHistory(cfg.ConversationHistory.MaxTokens, cfg.ConversationHistory.Strategy)
	history.Summarizer = rr.summarizer

	st := &reactState{
		runner:  rr,
		run:     run,
		agent:   agent,
		react:   reactCfg,
		budget:  budget,
		history: history,
		system:  buildReactSystemPrompt(agent, rr.toolRegistry, reactCfg),
		orgID:   runScopeOrgID(ctx),
	}

	// Issue #115 checkpoint resume (mirrors the base loop): continue after
	// the snapshotted step with the restored history/usage; a fresh run
	// starts from the user input.
	resumeStart := 0
	if state, ok := rr.resumeFromCheckpoint(ctx, run.ID); ok {
		resumeStart = state.StepIndex
		run.Steps = resumeStart
		if run.Tokens.TotalTokens == 0 && run.Tokens.PromptTokens == 0 && run.Tokens.CompletionTokens == 0 {
			run.Tokens = state.Usage
		}
		if budget != nil {
			budget.Record(state.Usage)
		}
		history.Messages = append(history.Messages, state.Messages...)
	} else {
		history.Append(models.Message{Role: "user", Content: input})
	}
	st.step = resumeStart

	// Long-output chunked generation (documented heuristic trigger). When it
	// triggers it REPLACES the single-draft flow for this run.
	if reactCfg.LongOutputThresholdTokens > 0 && estimateLongOutputTokens(input) > reactCfg.LongOutputThresholdTokens {
		out, err := rr.runChunked(ctx, st, input)
		if err == nil {
			// Final answer checkpoint (base-loop parity: the end-of-run
			// snapshot carries the whole conversation).
			history.Append(models.Message{Role: "assistant", Content: out})
			rr.saveCheckpoint(ctx, run.ID, CheckpointState{StepIndex: st.step, Messages: history.Messages, Usage: run.Tokens})
		}
		return out, err
	}

	var loop loopGuard
	var obsLog []string // every observation of the run (assessor coverage)
	roundsUsed := 0

	for {
		if _, ok := st.nextStep(); !ok {
			return "", fmt.Errorf("%w: budget was %d steps", ErrMaxStepsExceeded, rr.maxSteps)
		}
		if err := ctx.Err(); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return "", fmt.Errorf("%w: %v", ErrMaxRuntimeExceeded, err)
			}
			return "", rr.unwindCancelled(ctx, run, err)
		}

		stepCtx, stepSpan := rr.startStepTrace(ctx, st.orgID, agent.ID, run.ID, st.step)
		resp, err := rr.completeModel(ctx, st)
		if err != nil {
			stepSpan.end(err)
			return "", err
		}
		text := strings.TrimSpace(resp.Text)

		calls := parseReactToolCalls(text, reactCfg.MaxToolCallsPerStep)
		if len(calls) == 0 {
			// Draft answer → reflection gate.
			done, assessment, assessed := rr.reflectOrFinish(ctx, st, input, text, roundsUsed, obsLog)
			if assessed {
				// Auditable annotation step: the structured assessment rides
				// on the timeline (parity with the budget annotation steps of
				// budget.go). Recorded for reflect AND satisfactory outcomes;
				// suppressed entirely when reflection is disabled/exhausted.
				rr.record(ctx, run.ID, reflectionStep(st.step, assessment))
			}
			if done {
				// Final answer checkpoint (base-loop parity).
				history.Append(models.Message{Role: "assistant", Content: text})
				rr.saveCheckpoint(ctx, run.ID, CheckpointState{StepIndex: st.step, Messages: history.Messages, Usage: run.Tokens})
				stepSpan.end(nil)
				return text, nil
			}
			roundsUsed++
			// Reflecting: the feedback message was appended; next iteration.
			rr.saveCheckpoint(ctx, run.ID, CheckpointState{StepIndex: st.step, Messages: history.Messages, Usage: run.Tokens})
			stepSpan.end(nil)
			continue
		}

		// Act: execute every (capped) tool request through the EXISTING
		// tool/policy seam, recording one tool step per call.
		obs, obsMsgs, loopErr := rr.actToolCalls(ctx, stepCtx, st, calls, &loop)
		if loopErr != nil {
			stepSpan.end(loopErr)
			return "", loopErr
		}
		obsLog = append(obsLog, obs...)

		// A cancellation that landed during tool execution unwinds gracefully
		// instead of feeding observations into a dead run (base-loop parity).
		if cerr := ctx.Err(); cerr != nil && isCancellation(cerr) {
			stepSpan.end(cerr)
			return "", rr.unwindCancelled(ctx, run, cerr)
		}

		history.Append(append([]models.Message{{Role: "assistant", Content: text}}, obsMsgs...)...)
		rr.saveCheckpoint(ctx, run.ID, CheckpointState{StepIndex: st.step, Messages: history.Messages, Usage: run.Tokens})
		stepSpan.end(nil)
	}
}

// loopGuard tracks the flattened consecutive-identical (tool, arguments)
// sequence across a run: the generalization of the base loop's single-call
// repeat rule to multiple calls per step.
type loopGuard struct {
	tool    string
	args    string
	repeats int
}

// observe updates the guard with one call and reports whether the repeat
// limit (MaxLoopRepeats, base-runtime constant) is hit.
func (g *loopGuard) observe(tool, argsKey string) bool {
	if argsKey == g.args && tool == g.tool {
		g.repeats++
	} else {
		g.repeats = 1
		g.tool, g.args = tool, argsKey
	}
	return g.repeats >= MaxLoopRepeats
}

// reflectionStep builds the "reflection" annotation step that persists the
// structured QualityAssessment on the run timeline (parity with the budget
// annotation steps of budget.go).
func reflectionStep(index int, a QualityAssessment) Step {
	encoded, _ := json.Marshal(a)
	return Step{
		Index:  index,
		Type:   StepTypeReflection,
		Name:   "quality_assessment",
		Status: StepSucceeded,
		Output: string(encoded),
	}
}

// reflectOrFinish runs the reflection gate on a draft answer. It returns
// (done=true) when the draft is final, or (done=false) after appending the
// reflection feedback message to the history (the caller then continues the
// loop, increments its rounds counter and checkpoints). assessed reports
// whether an actual assessment was produced (false when reflection is
// disabled or the rounds budget is exhausted — no annotation step is
// recorded then). The assessment always degrades to the heuristic on seam
// failure; it never fails the run.
func (rr *ReACTRunner) reflectOrFinish(ctx context.Context, st *reactState, input, draft string, roundsUsed int, obsLog []string) (done bool, a QualityAssessment, assessed bool) {
	cfg := st.react
	if cfg.MaxReflectionRounds < 0 || roundsUsed >= cfg.MaxReflectionRounds {
		return true, QualityAssessment{}, false
	}

	a = rr.assessDraft(ctx, st, AssessRequest{
		Question:        input,
		DraftAnswer:     draft,
		Observations:    obsLog,
		RoundsRemaining: cfg.MaxReflectionRounds - roundsUsed,
	})

	// StopOnSatisfactory: the operator's bar overrides the assessor's
	// opinion — all dimensions >= threshold halts reflection.
	if cfg.StopOnSatisfactory && a.satisfied(cfg.QualityThreshold) {
		return true, a, true
	}
	if !a.ShouldReflect {
		return true, a, true
	}

	// Feed the assessment back and let the loop continue: the model may call
	// tools to fill the gaps before refining.
	st.history.Append(
		models.Message{Role: "assistant", Content: draft},
		models.Message{Role: "user", Content: reflectionFeedbackMessage(cfg, a)},
	)
	return false, a, true
}

// assessDraft evaluates a draft through the configured seam: injected
// Assessor > AssessmentLLM provider > HeuristicAssessor. It NEVER fails the
// run: every seam failure degrades to the deterministic heuristic.
func (rr *ReACTRunner) assessDraft(ctx context.Context, st *reactState, req AssessRequest) QualityAssessment {
	if rr.assessor != nil {
		if a, err := rr.assessor.Assess(ctx, req); err == nil {
			return normalizeAssessment(a)
		}
		return fallbackAssessment(req, "assessor failed")
	}
	if rr.assessmentProvider != nil {
		if a, ok := rr.assessViaLLM(ctx, st, req); ok {
			return a
		}
		return fallbackAssessment(req, "llm assessment failed")
	}
	a, err := HeuristicAssessor{}.Assess(ctx, req)
	if err != nil { // unreachable for the heuristic; kept for seam symmetry
		return fallbackAssessment(req, "assessment failed")
	}
	return a
}

// assessmentPrompt is the system prompt of the AssessmentLLM seam. It demands
// a strict JSON QualityAssessment so parsing stays deterministic.
const assessmentPrompt = "You are a strict answer quality assessor. Evaluate the draft answer " +
	"against the question and the tool observations. Reply with ONLY a JSON object: " +
	`{"completeness": <0-1>, "accuracy": <0-1>, "coherence": <0-1>, "should_reflect": <bool>, "notes": "<short rationale>"}.`

// assessViaLLM performs the budgeted, recorded assessment model call
// (AssessmentLLM seam). ok is false on ANY failure — the caller degrades to
// the heuristic (assessment must never break a run).
func (rr *ReACTRunner) assessViaLLM(ctx context.Context, st *reactState, req AssessRequest) (QualityAssessment, bool) {
	if _, ok := st.nextStep(); !ok {
		return QualityAssessment{}, false
	}
	model := strings.TrimSpace(rr.assessmentModel)
	if model == "" {
		model = st.agent.Model
	}
	spanCtx, span := rr.startModelTrace(ctx, st.orgID, st.agent.ID, st.run.ID, model)
	resp, err := rr.assessmentProvider.Complete(spanCtx, models.CompletionRequest{
		System: assessmentPrompt,
		Messages: []models.Message{
			{Role: "user", Content: "Question: " + req.Question + "\n\nTool observations:\n" +
				strings.Join(req.Observations, "\n") + "\n\nDraft answer:\n" + req.DraftAnswer},
		},
		Model:       model,
		Temperature: st.react.Temperature,
	})
	rr.endModelTrace(span, model, resp, err)
	if err != nil || resp == nil {
		return QualityAssessment{}, false
	}

	// Account usage exactly like the main loop (a real provider call).
	st.run.Tokens.PromptTokens += resp.Usage.PromptTokens
	st.run.Tokens.CompletionTokens += resp.Usage.CompletionTokens
	st.run.Tokens.TotalTokens += resp.Usage.TotalTokens
	st.budget.Record(resp.Usage)
	rr.record(ctx, st.run.ID, Step{
		Index:      st.step,
		Type:       StepTypeModel,
		Name:       rr.assessmentProvider.Name(),
		Status:     StepSucceeded,
		Input:      truncateForRecord(assessmentPrompt, 512),
		Output:     truncateForRecord(resp.Text, 512),
		TokenUsage: resp.Usage,
	})

	a, ok := parseAssessment(resp.Text)
	if !ok {
		return QualityAssessment{}, false
	}
	return a, true
}

// parseAssessment decodes a fenced-or-bare JSON QualityAssessment.
func parseAssessment(text string) (QualityAssessment, bool) {
	trimmed := strings.TrimSpace(text)
	trimmed = strings.TrimPrefix(trimmed, "```json")
	trimmed = strings.TrimPrefix(trimmed, "```")
	trimmed = strings.TrimSuffix(trimmed, "```")
	trimmed = strings.TrimSpace(trimmed)
	var a QualityAssessment
	if err := json.Unmarshal([]byte(trimmed), &a); err != nil {
		return QualityAssessment{}, false
	}
	return normalizeAssessment(a), true
}

// normalizeAssessment pins an assessment to a sane, deterministic shape:
// scores clamped to [0,1].
func normalizeAssessment(a QualityAssessment) QualityAssessment {
	a.Completeness = clamp01(a.Completeness)
	a.Accuracy = clamp01(a.Accuracy)
	a.Coherence = clamp01(a.Coherence)
	return a
}

// fallbackAssessment builds the degraded verdict when a seam fails: the
// heuristic scores the draft so the loop still reflects sensibly (bounded by
// rounds) instead of trusting a broken seam.
func fallbackAssessment(req AssessRequest, why string) QualityAssessment {
	a, err := HeuristicAssessor{}.Assess(context.Background(), req)
	if err != nil { // unreachable; defensive
		return QualityAssessment{ShouldReflect: true, Notes: why}
	}
	a.Notes = why
	return a
}

// reflectionFeedbackMessage composes the user-turn feedback that opens a new
// reflection round: the operator/documented prompt plus the structured
// assessment.
func reflectionFeedbackMessage(cfg ReactConfig, a QualityAssessment) string {
	var b strings.Builder
	b.WriteString(cfg.ReflectionPrompt)
	b.WriteString("\n\nQuality assessment of your draft (JSON):\n")
	if encoded, err := json.Marshal(a); err == nil {
		b.Write(encoded)
	}
	b.WriteString("\nRefine your answer now.")
	return b.String()
}

// completeModel performs ONE main-loop provider call with every base-runtime
// seam: pre-call model-scope rate limit (#124), model-call span (#114),
// provider-reported usage accounting into run.Tokens and the token budget
// (#107), post-call TPM accounting, budget alert/exceeded annotations and the
// run.token_budget_exceeded event, and the typed error mapping. The model
// step is recorded here on success AND failure, mirroring runtime.go.
func (rr *ReACTRunner) completeModel(ctx context.Context, st *reactState) (*models.CompletionResponse, error) {
	run, agent := st.run, st.agent

	modelSubject := agent.Model
	if strings.TrimSpace(modelSubject) == "" {
		modelSubject = rr.provider.Name()
	}
	if decision := rr.checkModelRateLimit(ctx, modelSubject); decision != nil {
		merr := &ModelRateLimitError{Model: modelSubject, Decision: *decision}
		rr.record(ctx, run.ID, Step{
			Index: st.step, Type: StepTypeModel, Name: modelSubject,
			Status: StepFailed, Error: merr.Error(),
		})
		return nil, merr
	}

	callStart := time.Now()
	spanCtx, span := rr.startModelTrace(ctx, st.orgID, agent.ID, run.ID, agent.Model)
	resp, err := rr.provider.Complete(spanCtx, models.CompletionRequest{
		System:      st.system,
		Messages:    st.history.Prompt(ctx),
		Model:       agent.Model,
		Temperature: st.react.Temperature,
	})
	rr.endModelTrace(span, agent.Model, resp, err)

	modelStep := Step{
		Index:      st.step,
		Type:       StepTypeModel,
		Name:       rr.provider.Name(),
		Input:      truncateForRecord(lastUserContent(st.history.Messages), 512),
		DurationMS: time.Since(callStart).Milliseconds(),
	}
	var bstate BudgetState
	if resp != nil {
		modelStep.TokenUsage = resp.Usage
		run.Tokens.PromptTokens += resp.Usage.PromptTokens
		run.Tokens.CompletionTokens += resp.Usage.CompletionTokens
		run.Tokens.TotalTokens += resp.Usage.TotalTokens
		bstate = st.budget.Record(resp.Usage)
		rr.reportModelTokens(ctx, modelSubject, resp.Usage)
	}
	if err != nil {
		modelStep.Status = StepFailed
		modelStep.Error = err.Error()
		rr.record(ctx, run.ID, modelStep)
		// Never auto-retry here (base-loop parity): typed causes surface.
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("%w: %v", ErrMaxRuntimeExceeded, err)
		}
		if isCancellation(err) {
			return nil, rr.unwindCancelled(ctx, run, err)
		}
		return nil, fmt.Errorf("runtime: model call failed: %w", err)
	}

	modelStep.Status = StepSucceeded
	modelStep.Output = truncateForRecord(strings.TrimSpace(resp.Text), 512)
	rr.record(ctx, run.ID, modelStep)

	// Budget outcomes: warn annotation once at the alert crossing; a
	// violation hard-stops the ReACT loop through the SAME typed error and
	// event as the base loop.
	if bstate.Alert != nil {
		rr.record(ctx, run.ID, budgetAnnotationStep(st.step, *bstate.Alert, StepSucceeded, ""))
	}
	if bstate.Exceeded != nil {
		rr.record(ctx, run.ID, budgetAnnotationStep(st.step, bstate.Snapshot, StepFailed, bstate.Exceeded.Error()))
		rr.emitRunEvent(ctx, events.EventRunTokenBudgetExceeded, run.ID, budgetEventPayload(run, bstate.Exceeded))
		return nil, bstate.Exceeded
	}
	return resp, nil
}

// actToolCalls executes a batch of parsed tool calls through the EXISTING
// tool/policy seam (Runner.executeTool: governance #75, agent-key allow-lists
// wave-8, granular rate limits #124, per-call timeout), recording one tool
// step per call with the same policy-verdict visibility as the base loop.
// It returns the observation strings and the tool messages to append to the
// history. The only error is the typed loop-detection abort.
func (rr *ReACTRunner) actToolCalls(ctx context.Context, stepCtx context.Context, st *reactState, calls []toolCall, loop *loopGuard) (obs []string, msgs []models.Message, err error) {
	for _, call := range calls {
		argsKey := canonicalJSON(call.Arguments)
		if loop.observe(call.Tool, argsKey) {
			return obs, msgs, fmt.Errorf("%w: tool %q called identically %d times", ErrLoopDetected, call.Tool, loop.repeats)
		}

		toolCtx, toolSpan := rr.startToolTrace(stepCtx, st.orgID, st.agent.ID, st.run.ID, call.Tool)
		observation, toolErr := rr.executeTool(toolCtx, call.Tool, call.Arguments)
		rr.endToolTrace(toolSpan, toolErr)

		toolStep := Step{
			Index:  st.step,
			Type:   StepTypeTool,
			Name:   call.Tool,
			Input:  truncateForRecord(argsKey, 512),
			Output: truncateForRecord(observation, 512),
		}
		if toolErr != nil {
			toolStep.Status = StepFailed
			toolStep.Error = toolErr.Error()
			// Issue #75 parity: policy blocks state the governance verdict on
			// the step and in the observation fed back to the model.
			var policyErr *ToolPolicyError
			if errors.As(toolErr, &policyErr) {
				toolStep.Output = truncateForRecord(policyObservation(policyErr.Decision), 512)
				if policyErr.Decision != nil {
					toolStep.Policy = policyStepField(*policyErr.Decision)
				}
			}
			observation = fmt.Sprintf("tool error: %v", toolErr)
			if errors.As(toolErr, &policyErr) {
				observation = policyObservation(policyErr.Decision)
			}
		} else {
			toolStep.Status = StepSucceeded
		}
		rr.record(ctx, st.run.ID, toolStep)

		obs = append(obs, observation)
		msgs = append(msgs, models.Message{Role: "tool", Name: call.Tool, Content: observation})
	}
	return obs, msgs, nil
}

// --- multi-tool parsing (issue #134) -----------------------------------------

// toolCallList is the multi-call reply shape: {"tools": [ ... ]}.
type toolCallList struct {
	Tools []toolCall `json:"tools"`
}

// parseReactToolCalls parses every tool request from one model reply, capped
// at max (the FIRST max requests win — deterministic and documented).
// Accepted shapes, in precedence order:
//  1. {"tools": [{"tool": "...", "arguments": {...}}, ...]}   (ReACT multi)
//  2. {"tool": "...", "arguments": {...}}                      (legacy single,
//     the base runtime's shape — full backward compatibility)
//  3. [{"tool": "...", "arguments": {...}}, ...]               (JSON array)
//
// Anything else (plain text) is a draft/final answer: zero calls. Entries
// with an empty tool name are skipped.
func parseReactToolCalls(text string, max int) []toolCall {
	trimmed := strings.TrimSpace(text)
	trimmed = strings.TrimPrefix(trimmed, "```json")
	trimmed = strings.TrimPrefix(trimmed, "```")
	trimmed = strings.TrimSuffix(trimmed, "```")
	trimmed = strings.TrimSpace(trimmed)
	if trimmed == "" {
		return nil
	}

	var calls []toolCall
	switch {
	case strings.HasPrefix(trimmed, "{"):
		var list toolCallList
		if err := json.Unmarshal([]byte(trimmed), &list); err == nil && len(list.Tools) > 0 {
			calls = list.Tools
		} else if name, args, ok := parseToolCall(trimmed); ok {
			calls = []toolCall{{Tool: name, Arguments: args}}
		}
	case strings.HasPrefix(trimmed, "["):
		if err := json.Unmarshal([]byte(trimmed), &calls); err != nil {
			calls = nil
		}
	default:
		return nil
	}

	out := make([]toolCall, 0, len(calls))
	for _, c := range calls {
		if strings.TrimSpace(c.Tool) == "" {
			continue
		}
		if c.Arguments == nil {
			c.Arguments = map[string]any{}
		}
		out = append(out, c)
		if max > 0 && len(out) >= max {
			break
		}
	}
	return out
}

// buildReactSystemPrompt composes the agent instructions and the ReACT tool
// contract: multi-call replies are documented next to the legacy single-call
// shape so a model can use either.
func buildReactSystemPrompt(agent *agents.Agent, registry *tools.Registry, cfg ReactConfig) string {
	var b strings.Builder
	b.WriteString("You are ")
	b.WriteString(agent.Name)
	b.WriteString(". ")
	if strings.TrimSpace(agent.Instructions) != "" {
		b.WriteString(strings.TrimSpace(agent.Instructions))
		b.WriteString("\n")
	}
	if registry != nil && len(registry.Names()) > 0 {
		b.WriteString("\nAvailable tools: ")
		b.WriteString(strings.Join(registry.Names(), ", "))
		b.WriteString(".\n")
	}
	b.WriteString(fmt.Sprintf(`
ReACT loop: reason step by step, then act. To call tools, reply with ONLY a JSON object:
{"tools": [{"tool": "<name>", "arguments": { ... }}, ...]}
You may request up to %d tool calls per reply. A single call may also use the short form
{"tool": "<name>", "arguments": { ... }}. Tool observations arrive before your next turn.
When you have the final answer, reply with plain text only.
`, cfg.MaxToolCallsPerStep))
	return b.String()
}

// --- long-output chunked generation (issue #134) -----------------------------

// long-output trigger patterns (see estimateLongOutputTokens).
var (
	longOutputWords = regexp.MustCompile(`(\d[\d,]*)\s*(?:-\s*)?words?\b`)
	longOutputPages = regexp.MustCompile(`(\d[\d,]*)\s+pages?\b`)
	longFormKeyword = regexp.MustCompile(`(?i)\b(report|essay|whitepaper|white paper|guide|comprehensive|in[- ]depth|detailed|long[- ]form|chapter|thesis|dissertation)\b`)
)

// estimateLongOutputTokens is the DOCUMENTED heuristic trigger for chunked
// long-output generation: an ESTIMATE of the requested output size, derived
// from the input alone (deterministic, no model call):
//  1. an explicit word count ("N words") dominates: ~1.3 tokens/word;
//  2. else an explicit page count ("N pages", ~500 words/page);
//  3. else a long-form keyword (report/essay/comprehensive/...) implies
//     keywordLongOutputEstimateTokens;
//  4. else 0 (no long-output signal).
//
// The runner chunks when the estimate exceeds
// ReactConfig.LongOutputThresholdTokens (default 1200).
func estimateLongOutputTokens(input string) int {
	parse := func(re *regexp.Regexp) (int, bool) {
		m := re.FindStringSubmatch(input)
		if len(m) < 2 {
			return 0, false
		}
		n, err := strconv.Atoi(strings.ReplaceAll(m[1], ",", ""))
		if err != nil || n <= 0 {
			return 0, false
		}
		return n, true
	}
	if words, ok := parse(longOutputWords); ok {
		return words * 13 / 10
	}
	if pages, ok := parse(longOutputPages); ok {
		return pages * 500 * 13 / 10
	}
	if longFormKeyword.MatchString(input) {
		return keywordLongOutputEstimateTokens
	}
	return 0
}

// sectionPlanPrompt asks the model for a JSON array of section titles.
const sectionPlanPrompt = "Plan the document you will write. Reply with ONLY a JSON array of " +
	"section title strings, in order, at most %d sections. No prose."

// runChunked produces long-form answers step by step: plan sections →
// generate each section with tool support → assemble. It replaces the
// single-draft flow (and its reflection pass — the section loop IS the
// incremental refinement, documented trade-off). Every model call shares the
// run's step and token budgets.
func (rr *ReACTRunner) runChunked(ctx context.Context, st *reactState, input string) (string, error) {
	sections, err := rr.planSections(ctx, st, input)
	if err != nil {
		return "", err
	}

	var doc strings.Builder
	for i, title := range sections {
		text, err := rr.writeSection(ctx, st, input, title, i+1, len(sections))
		if err != nil {
			return "", err
		}
		doc.WriteString("# " + title + "\n\n" + text + "\n\n")
		st.history.Append(
			models.Message{Role: "assistant", Content: "Section written: " + title},
			models.Message{Role: "user", Content: "Section accepted. Continue with the next section when asked."},
		)
	}
	return strings.TrimSpace(doc.String()), nil
}

// planSections runs the planning model call and parses the titles. Fallbacks
// are deterministic: fenced/line-separated titles are accepted; a plan that
// cannot be parsed degrades to a single section (the task itself) so chunked
// generation always makes progress.
func (rr *ReACTRunner) planSections(ctx context.Context, st *reactState, input string) ([]string, error) {
	if _, ok := st.nextStep(); !ok {
		return nil, fmt.Errorf("%w: budget was %d steps", ErrMaxStepsExceeded, rr.maxSteps)
	}
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("%w: %v", ErrMaxRuntimeExceeded, err)
		}
		return nil, rr.unwindCancelled(ctx, st.run, err)
	}

	// Planning is a plain model step (no tool execution): the step span is
	// opened and closed here; the model-call span opens in completeModel.
	_, stepSpan := rr.startStepTrace(ctx, st.orgID, st.agent.ID, st.run.ID, st.step)
	st.history.Append(models.Message{
		Role:    "user",
		Content: fmt.Sprintf(sectionPlanPrompt, st.react.MaxSections) + "\n\nTask: " + input,
	})
	resp, err := rr.completeModel(ctx, st)
	if err != nil {
		stepSpan.end(err)
		return nil, err
	}
	stepSpan.end(nil)

	titles := parseSectionPlan(resp.Text, st.react.MaxSections)
	st.history.Append(models.Message{Role: "assistant", Content: strings.Join(titles, " | ")})
	return titles, nil
}

// parseSectionPlan extracts section titles from a planner reply: a JSON
// array of strings, else non-empty lines (fences and list markers stripped),
// else the whole text as one section. Capped at max.
func parseSectionPlan(text string, max int) []string {
	trimmed := strings.TrimSpace(text)
	trimmed = strings.TrimPrefix(trimmed, "```json")
	trimmed = strings.TrimPrefix(trimmed, "```")
	trimmed = strings.TrimSuffix(trimmed, "```")
	trimmed = strings.TrimSpace(trimmed)

	var titles []string
	if strings.HasPrefix(trimmed, "[") {
		if err := json.Unmarshal([]byte(trimmed), &titles); err != nil {
			titles = nil
		}
	}
	if len(titles) == 0 {
		for _, line := range strings.Split(trimmed, "\n") {
			line = strings.TrimSpace(strings.TrimLeft(line, "-*#0123456789. "))
			if line != "" {
				titles = append(titles, line)
			}
		}
	}
	if len(titles) == 0 {
		titles = []string{strings.TrimSpace(text)}
	}
	if len(titles) > max {
		titles = titles[:max]
	}
	return titles
}

// writeSection generates one section with tool support: up to
// sectionMaxModelCalls model calls; tool requests inside a section reply are
// executed through the existing seam and fed back until the model replies
// with the section text (plain, non-tool). Bounded by the run's step budget.
func (rr *ReACTRunner) writeSection(ctx context.Context, st *reactState, input, title string, index, total int) (string, error) {
	prompt := fmt.Sprintf("Now write section %d of %d: %q. Task context: %s\n"+
		"Call tools if the section needs data, otherwise reply with the section text as plain prose.",
		index, total, title, input)
	for attempt := 0; attempt < sectionMaxModelCalls; attempt++ {
		if _, ok := st.nextStep(); !ok {
			return "", fmt.Errorf("%w: budget was %d steps", ErrMaxStepsExceeded, rr.maxSteps)
		}
		if err := ctx.Err(); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return "", fmt.Errorf("%w: %v", ErrMaxRuntimeExceeded, err)
			}
			return "", rr.unwindCancelled(ctx, st.run, err)
		}
		st.history.Append(models.Message{Role: "user", Content: prompt})

		stepCtx, stepSpan := rr.startStepTrace(ctx, st.orgID, st.agent.ID, st.run.ID, st.step)
		resp, err := rr.completeModel(ctx, st)
		if err != nil {
			stepSpan.end(err)
			return "", err
		}
		text := strings.TrimSpace(resp.Text)

		calls := parseReactToolCalls(text, st.react.MaxToolCallsPerStep)
		if len(calls) == 0 {
			rr.saveCheckpoint(ctx, st.run.ID, CheckpointState{StepIndex: st.step, Messages: st.history.Messages, Usage: st.run.Tokens})
			stepSpan.end(nil)
			return text, nil
		}

		// Act inside the section, then ask for the section text again.
		var loop loopGuard
		_, obsMsgs, loopErr := rr.actToolCalls(ctx, stepCtx, st, calls, &loop)
		if loopErr != nil {
			stepSpan.end(loopErr)
			return "", loopErr
		}
		st.history.Append(append([]models.Message{{Role: "assistant", Content: text}}, obsMsgs...)...)
		rr.saveCheckpoint(ctx, st.run.ID, CheckpointState{StepIndex: st.step, Messages: st.history.Messages, Usage: st.run.Tokens})
		stepSpan.end(nil)
		prompt = "Continue: reply with the section text as plain prose (no tool JSON)."
	}
	// The section sub-loop exhausted its rewrite budget with the model still
	// requesting tools: fail the run with the typed loop error (a degenerate
	// section conversation must not loop forever).
	return "", fmt.Errorf("%w: section %q kept requesting tools", ErrLoopDetected, title)
}
