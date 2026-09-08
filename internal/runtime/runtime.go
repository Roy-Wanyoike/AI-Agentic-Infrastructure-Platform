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
	"time"

	"agentos/internal/agents"
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
	systemPrompt := buildSystemPrompt(agent, r.toolRegistry)
	messages := []models.Message{
		{Role: "user", Content: input},
	}

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
			return "", err // caller cancelled the run
		}
		run.Steps = stepIndex

		callStart := time.Now()
		resp, err := r.provider.Complete(ctx, models.CompletionRequest{
			System:   systemPrompt,
			Messages: messages,
			Model:    agent.Model,
		})
		modelStep := Step{
			Index:      stepIndex,
			Type:       StepTypeModel,
			Name:       r.provider.Name(),
			Input:      truncateForRecord(lastUserContent(messages), 512),
			DurationMS: time.Since(callStart).Milliseconds(),
		}
		if resp != nil {
			modelStep.TokenUsage = resp.Usage
			run.Tokens.PromptTokens += resp.Usage.PromptTokens
			run.Tokens.CompletionTokens += resp.Usage.CompletionTokens
			run.Tokens.TotalTokens += resp.Usage.TotalTokens
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
			return "", fmt.Errorf("runtime: model call failed: %w", err)
		}

		text := strings.TrimSpace(resp.Text)
		modelStep.Status = StepSucceeded
		modelStep.Output = truncateForRecord(text, 512)
		r.record(ctx, run.ID, modelStep)

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

		messages = append(messages,
			models.Message{Role: "assistant", Content: text},
			models.Message{Role: "tool", Name: toolName, Content: observation},
		)
	}
	return "", fmt.Errorf("%w: budget was %d steps", ErrMaxStepsExceeded, r.maxSteps)
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
