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
			if out, ok := r.runCalculator(expr); ok {
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
			r.record(ctx, run.ID, toolStep)
			// Feed the failure back to the model so it can adapt; the loop
			// bound protects against infinite failure retries.
			observation = fmt.Sprintf("tool error: %v", toolErr)
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
// context-aware path when the tool supports it.
func (r *Runner) executeTool(ctx context.Context, name string, args map[string]any) (string, error) {
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

// record pushes a step to the recorder, ignoring recorder failures
// (observability must never break execution).
func (r *Runner) record(ctx context.Context, runID string, step Step) {
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
