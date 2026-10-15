// Package runtime implements the agent execution engine: a bounded
// model-in-the-loop that drives an agent through model calls and tool
// executions until it produces a final answer or hits a safety limit.
//
// Safeguards enforced on every run:
//   - max steps (model iterations)
//   - max total runtime (wall clock, via context deadline)
//   - per-tool-call timeout
//   - caller context cancellation is honored between and during steps
//   - loop detection (identical tool call repeated too many times)
//   - governance policy checks at the tool seam (issue #75): a tool denied
//     by policy is never invoked; the denial is visible on the step
//   - retries are left to the provider/router layer; the loop itself never
//     silently retries non-transient failures
//
// The loop is deterministic and fully offline when no Provider is configured
// (legacy behavior: math expressions route to the calculator tool, everything
// else gets a canned completion), which keeps local development and tests
// free of external dependencies.
package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"agentos/internal/agents"
	"agentos/internal/apikeys"
	"agentos/internal/audit"
	"agentos/internal/events"
	"agentos/internal/models"
	"agentos/internal/observability"
	"agentos/internal/policies"
	"agentos/internal/tools"
)

type RunStatus string

const (
	StatusQueued    RunStatus = "QUEUED"
	StatusRunning   RunStatus = "RUNNING"
	StatusCompleted RunStatus = "COMPLETED"
	StatusFailed    RunStatus = "FAILED"
)

// Default safety limits.
const (
	DefaultMaxSteps     = 10
	DefaultMaxRuntime   = 60 * time.Second
	DefaultToolTimeout  = 10 * time.Second
	MaxLoopRepeats      = 3 // identical consecutive tool calls allowed before abort
	defaultRunIDPrefix  = "run-"
	legacyFallbackSteps = 1
)

// StepType values recorded through the StepRecorder.
const (
	StepTypeModel = "model"
	StepTypeTool  = "tool"
	// StepTypeBudget marks a token-budget annotation step (issue #107): a
	// non-execution record that persists a BudgetSnapshot (warn or
	// exceeded) on the run timeline so budget decisions are auditable.
	StepTypeBudget = "budget"
	// StepTypeCancelled marks the final partial-results record of a
	// gracefully cancelled run (issue #107, see shutdown.go).
	StepTypeCancelled = "cancelled"
)

// Step statuses recorded through the StepRecorder.
const (
	StepSucceeded = "succeeded"
	StepFailed    = "failed"
)

// PolicyDecision carries the governance verdict that shaped a step (issue
// #75). It mirrors the evaluated policies.Decision so step records (and the
// run_steps output_meta documents built from them) can show WHY a tool was
// denied without importing the policy engine's types into the timeline.
type PolicyDecision struct {
	PolicyID string `json:"policy_id,omitempty"`
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

// Tool policy outcome codes recorded on denied/blocked steps. They match the
// error codes the create-run enforcement answers with so operators grep one
// vocabulary across run creation, steps and audit.
const (
	ToolPolicyDeniedCode      = "policy_denied"
	ToolPolicyUnavailableCode = "policy_unavailable"
)

// ToolPolicyError marks a tool invocation that was blocked BEFORE execution
// by the governance seam (issue #75). Code distinguishes an explicit policy
// denial (policy_denied) from a failing decision source that failed closed
// (policy_unavailable).
type ToolPolicyError struct {
	Tool     string
	Code     string
	Decision *policies.Decision
	Err      error // underlying evaluation failure (unavailable only)
}

// Error implements error.
func (e *ToolPolicyError) Error() string {
	if e.Code == ToolPolicyUnavailableCode {
		return fmt.Sprintf("%s: policy evaluation for tool %q failed: %v", e.Code, e.Tool, e.Err)
	}
	reason := ""
	if e.Decision != nil {
		reason = e.Decision.Reason
	}
	return fmt.Sprintf("%s: tool %q denied by policy: %s", e.Code, e.Tool, reason)
}

// Unwrap exposes the underlying evaluation failure.
func (e *ToolPolicyError) Unwrap() error { return e.Err }

// Step is one recorded unit of execution: either a model call or a tool call.
// The coordinator persists these as run_steps rows for the execution timeline.
type Step struct {
	Index      int
	Type       string // "model" | "tool"
	Name       string // provider name or tool name
	Status     string // "succeeded" | "failed"
	Input      string
	Output     string
	Error      string
	DurationMS int64
	TokenUsage models.Usage
	// Policy is set when a governance decision shaped this step (issue
	// #75): tool denials and fail-closed evaluation failures carry the
	// policy id, decision and reason for the dashboard/audit trail.
	Policy *PolicyDecision
}

// StepRecorder receives every step as it completes. Implementations must be
// safe for concurrent use; a failing recorder never aborts the run (it is
// observability, not control flow).
type StepRecorder interface {
	RecordStep(ctx context.Context, runID string, step Step) error
}

// StepRecorderFunc adapts a plain function to StepRecorder.
type StepRecorderFunc func(ctx context.Context, runID string, step Step) error

// RecordStep implements StepRecorder.
func (f StepRecorderFunc) RecordStep(ctx context.Context, runID string, step Step) error {
	if f == nil {
		return nil
	}
	return f(ctx, runID, step)
}

// Typed loop errors. Use errors.Is to classify.
var (
	// ErrMaxStepsExceeded means the agent burned its step budget without a
	// final answer. Treat as a run failure with a specific cause.
	ErrMaxStepsExceeded = errors.New("runtime: max steps exceeded")
	// ErrMaxRuntimeExceeded means the wall-clock budget ran out.
	ErrMaxRuntimeExceeded = errors.New("runtime: max runtime exceeded")
	// ErrLoopDetected means the model repeated the same tool call too many
	// times, indicating a degenerate conversation.
	ErrLoopDetected = errors.New("runtime: tool call loop detected")
	// ErrModelRequired is returned when a provider is required but absent.
	ErrModelRequired = errors.New("runtime: model provider is required")
)

// Run is the outcome of a single execution.
type Run struct {
	ID        string
	AgentID   string
	Input     string
	Output    string
	Status    RunStatus
	Error     string
	Steps     int
	Tokens    models.Usage
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Runner executes agents. The zero-value-compatible constructor
// NewRunner(agentService, toolRegistry) keeps the offline deterministic
// behavior; options add provider-driven execution.
type Runner struct {
	agentService *agents.Service
	toolRegistry *tools.Registry

	provider    models.Provider
	recorder    StepRecorder
	maxSteps    int
	maxRuntime  time.Duration
	toolTimeout time.Duration
	// metrics is optional (nil-safe DI, issue #12): when wired it feeds
	// the agentos_tools_total counter for every recorded tool step. See
	// WithMetrics / SetMetrics.
	metrics *observability.Metrics
	// policy is optional (nil-safe DI, issue #75): when wired, every tool
	// invocation is evaluated against the organization's governance
	// policies BEFORE execution and a denied tool is never invoked. The
	// run scope (tenant + environment) is read from the caller-stamped
	// context (policies.WithRunScope). See WithPolicyEnforcer /
	// SetPolicyEnforcer.
	policy *policies.Enforcer
	// limiter is optional (nil-safe DI, issue #124): when wired, tool
	// invocations and provider model calls are checked against the
	// organization's granular rate limits (org/agent/tool/model scopes;
	// see limiter_hooks.go). The run scope is read from the same
	// caller-stamped context. See WithGranularLimiter / SetGranularLimiter.
	limiter GranularLimiter
	// agentKeyAudit is optional (nil-safe DI, wave-8 agent identity v1):
	// when wired, a tool invocation denied by an agent key's allow-list
	// writes a best-effort audit row (action agentkey.tool_denied). See
	// WithAgentKeyAudit / SetAgentKeyAudit.
	agentKeyAudit *audit.Service
	// eventPub is optional (nil-safe DI, issue #107): when wired, run
	// lifecycle events (run.cancelled, run.token_budget_exceeded) are
	// published best-effort through the platform event bus. See
	// WithEventPublisher.
	eventPub events.Publisher
	// runtimeCfg + agentCfgSrc + summarizer implement the issue #107 per-agent
	// runtime configuration: runner-wide defaults (env-configurable in the
	// worker) optionally overridden per agent through a raw runtime_config
	// JSON source, plus an optional model-backed history summarizer
	// (mechanical default). See WithRuntimeConfig / WithAgentConfigSource /
	// WithHistorySummarizer.
	runtimeCfg  RuntimeConfig
	agentCfgSrc func(agentID string) json.RawMessage
	summarizer  Summarizer
	// Shutdown gate (issue #107): stopping refuses new runs
	// (ErrShuttingDown); inflight counts executing runs; drained is the
	// one-shot channel Shutdown waits on. Guarded by gateMu.
	gateMu   sync.Mutex
	stopping bool
	inflight int
	drained  chan struct{}
}

// Option configures a Runner.
type Option func(*Runner)

// WithProvider sets the model provider used for execution. When nil (the
// default), the runner stays in deterministic offline mode.
func WithProvider(p models.Provider) Option {
	return func(r *Runner) { r.provider = p }
}

// WithStepRecorder attaches a step recorder (observability sink).
func WithStepRecorder(rec StepRecorder) Option {
	return func(r *Runner) { r.recorder = rec }
}

// WithMetrics attaches the process-wide Metrics registry used to maintain the
// agentos_tools_total counter (incremented for every recorded tool step).
// Passing nil disables the counter (nil-safe DI, issue #12).
func WithMetrics(m *observability.Metrics) Option {
	return func(r *Runner) { r.metrics = m }
}

// WithPolicyEnforcer attaches the governance enforcement seam (issue #75):
// tool invocations are evaluated before execution and denied tools are never
// invoked. Passing nil keeps the legacy always-allow behavior (nil-safe DI).
func WithPolicyEnforcer(e *policies.Enforcer) Option {
	return func(r *Runner) { r.policy = e }
}

// WithAgentKeyAudit attaches the audit sink for agent-key tool denials
// (wave-8, agent identity v1): when an agent key requests a tool outside its
// allow-list, a best-effort agentkey.tool_denied row is written with the key
// id, agent id and tool name. Passing nil disables the row (nil-safe DI).
func WithAgentKeyAudit(svc *audit.Service) Option {
	return func(r *Runner) { r.agentKeyAudit = svc }
}

// WithLimits overrides max model steps and max total runtime. Values <= 0
// keep the defaults.
func WithLimits(maxSteps int, maxRuntime time.Duration) Option {
	return func(r *Runner) {
		if maxSteps > 0 {
			r.maxSteps = maxSteps
		}
		if maxRuntime > 0 {
			r.maxRuntime = maxRuntime
		}
	}
}

// WithToolTimeout overrides the per-tool-call timeout. Values <= 0 keep the
// default.
func WithToolTimeout(d time.Duration) Option {
	return func(r *Runner) {
		if d > 0 {
			r.toolTimeout = d
		}
	}
}

// WithEventPublisher attaches the platform event seam (issue #107): run
// lifecycle events (run.cancelled, run.token_budget_exceeded) are published
// best-effort — a nil publisher, a context without a tenant scope or a
// publish failure never affects execution. Passing nil keeps the legacy
// event-free behavior (nil-safe DI).
func WithEventPublisher(pub events.Publisher) Option {
	return func(r *Runner) { r.eventPub = pub }
}

// WithRuntimeConfig sets the runner-wide runtime configuration (issue #107):
// the conversation-history and token-budget defaults every run starts from.
// Per-agent overrides (WithAgentConfigSource) layer on top; unset fields fall
// back to the safe defaults (see RuntimeConfig.WithDefaults).
func WithRuntimeConfig(cfg RuntimeConfig) Option {
	return func(r *Runner) { r.runtimeCfg = cfg }
}

// WithAgentConfigSource attaches the per-agent runtime_config document
// source (issue #107): for every run the runner asks the source
// for the agent's raw runtime_config JSON and overlays a VALID override on
// the runner-wide defaults. A nil source, an absent document or an invalid
// document falls back to the runner-wide configuration — bad config must
// never break a run. The source is consulted once per run (before the loop
// starts) and must be safe for concurrent use.
func WithAgentConfigSource(src func(agentID string) json.RawMessage) Option {
	return func(r *Runner) { r.agentCfgSrc = src }
}

// WithHistorySummarizer injects the Summarizer behind the summarize_older
// truncation strategy (issue #107). Nil keeps the deterministic
// MechanicalSummarizer default; a failing summarizer degrades to the
// mechanical summary (see ConversationHistory.Prompt).
func WithHistorySummarizer(s Summarizer) Option {
	return func(r *Runner) { r.summarizer = s }
}

// NewRunner builds a Runner with default limits and offline behavior. The
// signature is unchanged from earlier versions so existing callers compile.
func NewRunner(agentService *agents.Service, toolRegistry *tools.Registry) *Runner {
	return &Runner{
		agentService: agentService,
		toolRegistry: toolRegistry,
		maxSteps:     DefaultMaxSteps,
		maxRuntime:   DefaultMaxRuntime,
		toolTimeout:  DefaultToolTimeout,
	}
}

// SetMetrics attaches (or clears, via nil) the metrics registry after
// construction. Nil-safe on the receiver. Like the Option constructors, call
// this before runs are in flight (the Runner is not lock-protected).
func (r *Runner) SetMetrics(m *observability.Metrics) {
	if r == nil {
		return
	}
	r.metrics = m
}

// SetPolicyEnforcer attaches (or clears, via nil) the governance enforcement
// seam after construction. Nil-safe on the receiver. Like the Option
// constructors, call this before runs are in flight (the Runner is not
// lock-protected).
func (r *Runner) SetPolicyEnforcer(e *policies.Enforcer) {
	if r == nil {
		return
	}
	r.policy = e
}

// SetAgentKeyAudit attaches (or clears, via nil) the agent-key denial audit
// sink after construction. Nil-safe on the receiver; wire before runs are in
// flight (the Runner is not lock-protected).
func (r *Runner) SetAgentKeyAudit(svc *audit.Service) {
	if r == nil {
		return
	}
	r.agentKeyAudit = svc
}

// resolveRuntimeConfig computes the effective runtime configuration for one
// run (issue #107): the runner-wide defaults (WithRuntimeConfig / the
// worker's RuntimeConfigFromEnv) overlaid with the agent's runtime_config
// document read through WithAgentConfigSource, then the safe defaults are
// applied to still-unset fields. A nil source, an absent document or an
// invalid document keeps the base configuration untouched (bad config must
// never break a run).
func (r *Runner) resolveRuntimeConfig(agentID string) RuntimeConfig {
	cfg := r.runtimeCfg
	if r.agentCfgSrc != nil {
		if raw := r.agentCfgSrc(agentID); len(strings.TrimSpace(string(raw))) > 0 {
			if override, err := ParseRuntimeConfig(raw); err == nil {
				cfg = mergeRuntimeConfig(cfg, override)
			}
		}
	}
	return cfg.WithDefaults()
}

// NewRunnerWithOptions builds a Runner with options applied.
func NewRunnerWithOptions(agentService *agents.Service, toolRegistry *tools.Registry, opts ...Option) *Runner {
	r := NewRunner(agentService, toolRegistry)
	for _, opt := range opts {
		if opt != nil {
			opt(r)
		}
	}
	return r
}

// extractMathExpression detects arithmetic input for the offline fallback.
func extractMathExpression(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return ""
	}
	lower := strings.ToLower(trimmed)
	for _, token := range []string{"what is ", "what's ", "calculate ", "compute ", "evaluate ", "solve "} {
		lower = strings.ReplaceAll(lower, token, "")
	}
	lower = strings.Trim(lower, "? .!:")
	if lower == "" {
		return ""
	}
	for _, ch := range []string{"+", "-", "*", "/", "%"} {
		if strings.Contains(lower, ch) {
			return strings.TrimSpace(lower)
		}
	}
	return ""
}

// runCalculator executes the calculator tool and formats its result.
func (r *Runner) runCalculator(expression string) (string, bool) {
	if r.toolRegistry == nil {
		return "", false
	}
	tool, ok := r.toolRegistry.Get("calculator")
	if !ok {
		return "", false
	}
	result, err := tool.Execute(map[string]any{"expression": expression})
	if err != nil {
		return "", false
	}
	switch v := result["result"].(type) {
	case int64:
		return strconv.FormatInt(v, 10), true
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), true
	case int:
		return strconv.Itoa(v), true
	case string:
		return v, true
	default:
		return "0", true
	}
}

// Run executes an agent with a freshly generated run ID.
func (r *Runner) Run(ctx context.Context, agentID, input string) (*Run, error) {
	return r.RunWithID(ctx, newRunID(), agentID, input)
}

// RunWithID executes an agent under the caller-supplied run ID (typically the
// persisted run identifier) and returns the final run state.
func (r *Runner) RunWithID(ctx context.Context, runID, agentID, input string) (*Run, error) {
	if r == nil || r.agentService == nil {
		return nil, errors.New("runtime: agent service is required")
	}
	if strings.TrimSpace(agentID) == "" {
		return nil, errors.New("runtime: agent id is required")
	}
	if strings.TrimSpace(runID) == "" {
		runID = newRunID()
	}

	// Issue #107 shutdown gate: a runner that stopped accepting new runs
	// (Stop / Shutdown) refuses here with ErrShuttingDown; every other
	// path releases the in-flight slot on the way out (defer).
	if err := r.begin(); err != nil {
		return nil, err
	}
	defer r.finish()

	agent, ok := r.agentService.Get(agentID)
	if !ok {
		return nil, errors.New("runtime: agent not found")
	}

	// Bound the whole run by the wall-clock budget unless the caller already
	// set a tighter deadline.
	deadline := time.Now().Add(r.maxRuntime)
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

	output, err := r.execute(ctx, run, agent, input)
	run.UpdatedAt = time.Now().UTC()
	if err != nil {
		run.Status = StatusFailed
		run.Error = err.Error()
		// Cancellation is recorded as failure with its cause; the caller can
		// inspect ctx to distinguish user cancellation from timeouts.
		return run, err
	}
	run.Output = output
	run.Status = StatusCompleted
	return run, nil
}

// execute drives the loop and returns the final output text.
func (r *Runner) execute(ctx context.Context, run *Run, agent *agents.Agent, input string) (string, error) {
	// Offline mode: no provider configured -> legacy deterministic behavior.
	if r.provider == nil {
		if expr := extractMathExpression(input); expr != "" {
			// Issue #75: the offline calculator path goes through the
			// same governance seam as the provider loop — a denied
			// tool is never invoked and the denial is visible on the
			// recorded step; the run then completes through the
			// offline-fallback answer below (skip semantics).
			if decision, code, blocked := r.checkToolPolicy(ctx, "calculator"); blocked {
				r.record(ctx, run.ID, Step{
					Index: 1, Type: StepTypeTool, Name: "calculator",
					Status: StepFailed, Input: expr,
					Output: policyObservation(&decision),
					Error:  policyStepError(code, "calculator", decision),
					Policy: policyStepField(decision),
				})
			} else if err := r.authorizeAgentKeyTool(ctx, "calculator"); err != nil {
				// Wave-8 agent identity: the same least-privilege
				// gate as the provider loop, with skip semantics
				// (the run completes through the offline-fallback
				// answer below; the denial is visible on the step).
				r.record(ctx, run.ID, Step{
					Index: 1, Type: StepTypeTool, Name: "calculator",
					Status: StepFailed, Input: expr,
					Error: err.Error(),
				})
			} else if decision := r.checkToolRateLimit(ctx, "calculator"); decision != nil {
				// Issue #124: the offline calculator path goes
				// through the same granular tool-scope rate
				// limit as the provider loop (skip semantics,
				// like the policy seam above): the denial is
				// visible on the recorded step and the run
				// completes through the offline-fallback
				// answer below.
				r.record(ctx, run.ID, Step{
					Index: 1, Type: StepTypeTool, Name: "calculator",
					Status: StepFailed, Input: expr,
					Output: "tool rate limited: " + decision.Reason,
					Error:  (&ToolRateLimitError{Tool: "calculator", Decision: *decision}).Error(),
				})
			} else if out, ok := r.runCalculator(expr); ok {
				r.record(ctx, run.ID, Step{
					Index: 1, Type: StepTypeTool, Name: "calculator",
					Status: StepSucceeded, Input: expr, Output: out,
					DurationMS: 0,
				})
				run.Steps = legacyFallbackSteps
				return out, nil
			}
		}
		r.record(ctx, run.ID, Step{
			Index: 1, Type: StepTypeModel, Name: "offline-fallback",
			Status: StepSucceeded, Input: input,
			Output: "Completed " + agent.Name + " in response to: " + input,
		})
		run.Steps = legacyFallbackSteps
		return "Completed " + agent.Name + " in response to: " + input, nil
	}

	// Provider mode: bounded model/tool loop.
	//
	// Issue #107: the loop is now configuration-driven — the per-agent
	// runtime config sizes the conversation history sent to the provider
	// (ConversationHistory) and enforces the cumulative token budget
	// (TokenBudget); caller cancellation unwinds gracefully through
	// unwindCancelled (shutdown.go). Config resolution happens once, before
	// the loop starts.
	cfg := r.resolveRuntimeConfig(agent.ID)
	budget := NewTokenBudget(cfg.TokenBudget)
	history := NewConversationHistory(cfg.ConversationHistory.MaxTokens, cfg.ConversationHistory.Strategy)
	history.Summarizer = r.summarizer
	systemPrompt := buildSystemPrompt(agent, r.toolRegistry)
	history.Append(models.Message{Role: "user", Content: input})

	type lastCall struct {
		tool string
		args string
	}
	var last lastCall
	var lastRepeat int

	for stepIndex := 1; stepIndex <= r.maxSteps; stepIndex++ {
		if err := ctx.Err(); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return "", fmt.Errorf("%w: %v", ErrMaxRuntimeExceeded, err)
			}
			// Issue #107: graceful cancellation between steps — no
			// step is in flight, so unwind with the partial state.
			return "", r.unwindCancelled(ctx, run, err)
		}
		run.Steps = stepIndex

		// Issue #124: granular model-scope rate limit BEFORE the
		// provider call (model RPM; TPM is charged post-call via
		// reportModelTokens below). A denial records the failed model
		// step and fails the run with the machine-readable
		// rate_limited code — a throttled model must not consume
		// provider quota. Hook logic lives in limiter_hooks.go.
		modelSubject := agent.Model
		if strings.TrimSpace(modelSubject) == "" {
			modelSubject = r.provider.Name()
		}
		if decision := r.checkModelRateLimit(ctx, modelSubject); decision != nil {
			merr := &ModelRateLimitError{Model: modelSubject, Decision: *decision}
			r.record(ctx, run.ID, Step{
				Index: stepIndex, Type: StepTypeModel, Name: modelSubject,
				Status: StepFailed, Input: truncateForRecord(input, 512),
				Error: merr.Error(),
			})
			return "", merr
		}

		promptView := history.Prompt(ctx)
		callStart := time.Now()
		resp, err := r.provider.Complete(ctx, models.CompletionRequest{
			System:   systemPrompt,
			Messages: promptView,
			Model:    agent.Model,
		})
		modelStep := Step{
			Index:      stepIndex,
			Type:       StepTypeModel,
			Name:       r.provider.Name(),
			Input:      truncateForRecord(lastUserContent(promptView), 512),
			DurationMS: time.Since(callStart).Milliseconds(),
		}
		// Issue #107: account the provider-reported usage into the
		// cumulative budget as soon as a response (with usage) exists;
		// the classification outcome is acted on in the success path
		// below (a failed call returns before any budget decision).
		var bstate BudgetState
		if resp != nil {
			modelStep.TokenUsage = resp.Usage
			run.Tokens.PromptTokens += resp.Usage.PromptTokens
			run.Tokens.CompletionTokens += resp.Usage.CompletionTokens
			run.Tokens.TotalTokens += resp.Usage.TotalTokens
			bstate = budget.Record(resp.Usage)
			// Issue #124: charge the TRUE token usage of the
			// completed call into the model scope's TPM window
			// (post-call accounting; the pre-call check enforced
			// the window against previously reported usage).
			// Best-effort; never breaks the run.
			r.reportModelTokens(ctx, modelSubject, resp.Usage)
		}
		if err != nil {
			modelStep.Status = StepFailed
			modelStep.Error = err.Error()
			r.record(ctx, run.ID, modelStep)
			// Never auto-retry here: transient handling belongs to the
			// provider/router layer. Surface typed causes.
			if errors.Is(err, context.DeadlineExceeded) {
				return "", fmt.Errorf("%w: %v", ErrMaxRuntimeExceeded, err)
			}
			// Issue #107: a caller cancellation fails the CURRENT step
			// (recorded above) and then unwinds gracefully — partial
			// results marker + run.cancelled event + typed outcome.
			if isCancellation(err) {
				return "", r.unwindCancelled(ctx, run, err)
			}
			return "", fmt.Errorf("runtime: model call failed: %w", err)
		}

		text := strings.TrimSpace(resp.Text)
		modelStep.Status = StepSucceeded
		modelStep.Output = truncateForRecord(text, 512)
		r.record(ctx, run.ID, modelStep)

		// Issue #107: token budget outcomes. The warn annotation lands
		// once when the alert threshold is first crossed; a violation
		// records the failed annotation, publishes the event and
		// hard-stops the loop with the typed error (run status mapping
		// unchanged — FAILED through the existing failure path).
		if bstate.Alert != nil {
			r.record(ctx, run.ID, budgetAnnotationStep(stepIndex, *bstate.Alert, StepSucceeded, ""))
		}
		if bstate.Exceeded != nil {
			r.record(ctx, run.ID, budgetAnnotationStep(stepIndex, bstate.Snapshot, StepFailed, bstate.Exceeded.Error()))
			r.emitRunEvent(ctx, events.EventRunTokenBudgetExceeded, run.ID, budgetEventPayload(run, bstate.Exceeded))
			return "", bstate.Exceeded
		}

		toolName, toolArgs, isTool := parseToolCall(text)
		if !isTool {
			return text, nil // final answer
		}

		// Loop detection: identical consecutive tool+arguments.
		argsKey := canonicalJSON(toolArgs)
		if argsKey == last.args && toolName == last.tool {
			lastRepeat++
		} else {
			lastRepeat = 1
			last = lastCall{tool: toolName, args: argsKey}
		}
		if lastRepeat >= MaxLoopRepeats {
			return "", fmt.Errorf("%w: tool %q called identically %d times", ErrLoopDetected, toolName, lastRepeat)
		}

		observation, toolErr := r.executeTool(ctx, toolName, toolArgs)
		toolStep := Step{
			Index:      stepIndex,
			Type:       StepTypeTool,
			Name:       toolName,
			Input:      truncateForRecord(argsKey, 512),
			Output:     truncateForRecord(observation, 512),
			DurationMS: 0,
		}
		if toolErr != nil {
			toolStep.Status = StepFailed
			toolStep.Error = toolErr.Error()
			toolStep.DurationMS = 0
			// Issue #75: policy blocks surface the verdict on the
			// step (policy id + decision + reason) and the
			// observation fed back to the model says the tool was
			// withheld by governance, not that it failed.
			var policyErr *ToolPolicyError
			if errors.As(toolErr, &policyErr) {
				toolStep.Output = truncateForRecord(policyObservation(policyErr.Decision), 512)
				if policyErr.Decision != nil {
					toolStep.Policy = policyStepField(*policyErr.Decision)
				}
			}
			r.record(ctx, run.ID, toolStep)
			// Feed the failure back to the model so it can adapt; the loop
			// bound protects against infinite failure retries.
			observation = fmt.Sprintf("tool error: %v", toolErr)
			if errors.As(toolErr, &policyErr) {
				observation = policyObservation(policyErr.Decision)
			}
		} else {
			toolStep.Status = StepSucceeded
			r.record(ctx, run.ID, toolStep)
		}

		// Issue #107: a cancellation that landed during the tool call —
		// the current step completed above, so unwind gracefully now
		// instead of feeding the observation back into a dead run.
		if cerr := ctx.Err(); cerr != nil && isCancellation(cerr) {
			return "", r.unwindCancelled(ctx, run, cerr)
		}

		history.Append(
			models.Message{Role: "assistant", Content: text},
			models.Message{Role: "tool", Name: toolName, Content: observation},
		)
	}
	return "", fmt.Errorf("%w: budget was %d steps", ErrMaxStepsExceeded, r.maxSteps)
}

// budgetAnnotationStep builds the dedicated "budget" annotation step that
// persists a BudgetSnapshot on the run timeline (issue #107): warn
// annotations carry a succeeded status, violations a failed status with the
// typed error text. The snapshot JSON rides in the step Output so budget
// data is persisted with the run trace.
func budgetAnnotationStep(index int, snap BudgetSnapshot, status, errMsg string) Step {
	encoded, _ := json.Marshal(snap)
	return Step{
		Index:  index,
		Type:   StepTypeBudget,
		Name:   "token_budget",
		Status: status,
		Output: string(encoded),
		Error:  errMsg,
	}
}

// budgetEventPayload renders the run.token_budget_exceeded event payload
// (issue #107): enough to alert and attribute without leaking prompts.
func budgetEventPayload(run *Run, violation *TokenBudgetExceededError) map[string]any {
	return map[string]any{
		"run_id":    run.ID,
		"agent_id":  run.AgentID,
		"steps":     run.Steps,
		"dimension": violation.Dimension,
		"used":      violation.Used,
		"limit":     violation.Limit,
	}
}

// executeTool runs one tool call with the configured timeout, preferring the
// context-aware path when the tool supports it. Issue #75: the governance
// policy is evaluated BEFORE the tool is resolved or executed — a denied (or
// require_approval-gated) tool, or a failing decision source (fail closed),
// returns a ToolPolicyError and the tool is never invoked.
func (r *Runner) executeTool(ctx context.Context, name string, args map[string]any) (string, error) {
	if decision, code, blocked := r.checkToolPolicy(ctx, name); blocked {
		return "", &ToolPolicyError{Tool: name, Code: code, Decision: &decision}
	}
	// Wave-8 agent identity: an agent-key caller may only invoke tools in
	// its key's allow-list (nil allow-list = all tools). The seam is
	// nil-safe: every human/JWT/user-key caller reads nil claims here and
	// passes unchanged.
	if err := r.authorizeAgentKeyTool(ctx, name); err != nil {
		return "", err
	}
	// Issue #124: granular tool-scope rate limit AFTER the policy and
	// agent-key checks, BEFORE the tool resolves/executes — a denial is a
	// typed ToolRateLimitError (code rate_limited): the tool is never
	// invoked, the step records the denial and the run CONTINUES (the
	// observation fed back to the model states the rate limit). Hook logic
	// lives in limiter_hooks.go.
	if decision := r.checkToolRateLimit(ctx, name); decision != nil {
		return "", &ToolRateLimitError{Tool: name, Decision: *decision}
	}
	if r.toolRegistry == nil {
		return "", fmt.Errorf("tool %q is not registered", name)
	}
	tool, ok := r.toolRegistry.Get(name)
	if !ok {
		return "", fmt.Errorf("tool %q is not registered", name)
	}

	callCtx, cancel := context.WithTimeout(ctx, r.toolTimeout)
	defer cancel()

	if aware, ok := tool.(tools.ContextAware); ok {
		result, err := aware.ExecuteContext(callCtx, args)
		if err != nil {
			return "", err
		}
		return formatToolResult(result), nil
	}
	result, err := tool.Execute(args)
	if err != nil {
		return "", err
	}
	return formatToolResult(result), nil
}

// authorizeAgentKeyTool consults the agent-key least-privilege seam (wave-8,
// agent identity v1): apikeys.AuthorizeAgentKeyTool decides from the claims
// stamped on the execution context (nil claims = not an agent-key caller =
// allowed). A denial additionally writes the best-effort agentkey.tool_denied
// audit row when an audit sink is wired (observability must never break
// execution); the returned error carries the machine-readable
// agent_key_tool_not_allowed code and surfaces on the recorded tool step.
func (r *Runner) authorizeAgentKeyTool(ctx context.Context, name string) error {
	claims := apikeys.AgentKeyClaimsFromContext(ctx)
	err := apikeys.AuthorizeAgentKeyTool(claims, name)
	if err == nil || r.agentKeyAudit == nil || claims == nil {
		return err
	}
	_, _ = r.agentKeyAudit.LogCtx(ctx, claims.AgentID, apikeys.AgentKeyToolDeniedAction, claims.OrganizationID,
		"agent-keys/"+claims.KeyID, map[string]any{
			"key_id":   claims.KeyID,
			"agent_id": claims.AgentID,
			"tool":     name,
		})
	return err
}

// checkToolPolicy consults the optional governance seam (nil enforcer =
// disabled). It reports blocked=true (with the ToolPolicyError code) when
// the invocation must NOT execute: an explicit deny, a require_approval
// decision (mid-run approval pauses are not supported, so an
// approval-gated tool fails closed with the reason), or a failing decision
// source (ToolPolicyUnavailableCode). The returned Decision carries the
// reason for the step record and the model observation.
func (r *Runner) checkToolPolicy(ctx context.Context, name string) (policies.Decision, string, bool) {
	if !r.policy.Enabled() {
		return policies.Decision{}, "", false
	}
	scope := policies.RunScopeFromContext(ctx)
	decision, err := r.policy.AuthorizeToolCall(ctx, scope.OrganizationID, name, scope.Environment)
	if err != nil {
		// Fail closed: a governance outage must degrade to "no tools
		// execute", never to "policies bypassed".
		return policies.Decision{Decision: policies.EffectDeny, Reason: err.Error()}, ToolPolicyUnavailableCode, true
	}
	if decision.Allowed() && !decision.RequireApproval {
		return decision, "", false
	}
	return decision, ToolPolicyDeniedCode, true
}

// policyStepError renders the step error string for a blocked invocation.
func policyStepError(code, tool string, decision policies.Decision) string {
	return (&ToolPolicyError{Tool: tool, Code: code, Decision: &decision}).Error()
}

// policyStepField renders a decision for the Step.Policy record.
func policyStepField(decision policies.Decision) *PolicyDecision {
	return &PolicyDecision{
		PolicyID: decision.MatchedPolicyID,
		Decision: decision.Decision,
		Reason:   decision.Reason,
	}
}

// policyObservation is the observation fed back to the model when a tool is
// withheld by governance. It states the denial explicitly so the model can
// produce its answer without the tool.
func policyObservation(decision *policies.Decision) string {
	reason := ""
	if decision != nil {
		reason = decision.Reason
	}
	if reason == "" {
		return "tool denied by policy"
	}
	return "tool denied by policy: " + reason
}

// record pushes a step to the recorder and maintains the agentos_tools_total
// counter for tool steps, ignoring recorder failures (observability must
// never break execution).
func (r *Runner) record(ctx context.Context, runID string, step Step) {
	// Issue #12: every recorded tool step is a tool execution - count it
	// regardless of the step outcome and even when no recorder is wired
	// (the offline calculator path records through the same choke point).
	if step.Type == StepTypeTool && r.metrics != nil {
		r.metrics.IncTools()
	}
	if r.recorder == nil {
		return
	}
	_ = r.recorder.RecordStep(ctx, runID, step)
}

// toolCall is the JSON shape models use to invoke a tool.
type toolCall struct {
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments"`
}

// parseToolCall inspects a model response: it is a tool call when the trimmed
// text (markdown fences stripped) parses to an object with a non-empty
// "tool" field. Anything else is a final answer.
func parseToolCall(text string) (string, map[string]any, bool) {
	trimmed := strings.TrimSpace(text)
	trimmed = strings.TrimPrefix(trimmed, "```json")
	trimmed = strings.TrimPrefix(trimmed, "```")
	trimmed = strings.TrimSuffix(trimmed, "```")
	trimmed = strings.TrimSpace(trimmed)
	if trimmed == "" || trimmed[0] != '{' {
		return "", nil, false
	}
	var call toolCall
	if err := json.Unmarshal([]byte(trimmed), &call); err != nil {
		return "", nil, false
	}
	if strings.TrimSpace(call.Tool) == "" {
		return "", nil, false
	}
	if call.Arguments == nil {
		call.Arguments = map[string]any{}
	}
	return strings.TrimSpace(call.Tool), call.Arguments, true
}

// canonicalJSON produces a stable key for loop detection.
func canonicalJSON(args map[string]any) string {
	b, err := json.Marshal(args)
	if err != nil {
		return fmt.Sprintf("%v", args)
	}
	return string(b)
}

// buildSystemPrompt composes the agent instructions plus the tool contract
// the model must follow to invoke tools.
func buildSystemPrompt(agent *agents.Agent, registry *tools.Registry) string {
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
	b.WriteString(`
To use a tool, reply with ONLY a JSON object:
{"tool": "<tool name>", "arguments": { ... }}
When you have the final answer (or need no tool), reply with plain text only.
`)
	return b.String()
}

// formatToolResult renders a tool result map as a compact observation string.
func formatToolResult(result map[string]any) string {
	if result == nil {
		return ""
	}
	b, err := json.Marshal(result)
	if err != nil {
		return fmt.Sprintf("%v", result)
	}
	return string(b)
}

// lastUserContent returns the most recent user message (for step records).
func lastUserContent(messages []models.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return messages[i].Content
		}
	}
	if len(messages) > 0 {
		return messages[0].Content
	}
	return ""
}

// truncateForRecord caps strings stored in step records.
func truncateForRecord(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "...[truncated]"
}

// newRunID generates a random run identifier.
func newRunID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%s%d", defaultRunIDPrefix, time.Now().UnixNano())
	}
	return defaultRunIDPrefix + hex.EncodeToString(buf)
}
