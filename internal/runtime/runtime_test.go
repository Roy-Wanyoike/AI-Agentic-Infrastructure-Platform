package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"agentos/internal/agents"
	"agentos/internal/models"
	"agentos/internal/tools"
)

func newTestAgent(t *testing.T, agentService *agents.Service) *agents.Agent {
	t.Helper()
	agent, err := agentService.Create("org-1", "Math Agent", "Performs calculations", "Solve math problems", "test-model")
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	return agent
}

func TestRunAgentUsesToolWhenPromptRequiresIt(t *testing.T) {
	agentService := agents.NewService()
	agent := newTestAgent(t, agentService)
	toolRegistry := tools.NewRegistry()
	toolRegistry.Register(tools.NewCalculatorTool())
	runner := NewRunner(agentService, toolRegistry)

	run, err := runner.Run(context.Background(), agent.ID, "What is 2 + 2?")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if run.Status != StatusCompleted {
		t.Fatalf("expected completed status, got %s", run.Status)
	}
	if run.Output == "" {
		t.Fatal("run output should not be empty")
	}
}

func TestRunAgentUsesToolForGenericArithmeticInput(t *testing.T) {
	agentService := agents.NewService()
	agent := newTestAgent(t, agentService)
	toolRegistry := tools.NewRegistry()
	toolRegistry.Register(tools.NewCalculatorTool())
	runner := NewRunner(agentService, toolRegistry)

	run, err := runner.Run(context.Background(), agent.ID, "What is 3 * 4?")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if run.Output != "12" {
		t.Fatalf("expected arithmetic tool result 12, got %q", run.Output)
	}
}

func TestRunAgentRejectsUnknownAgent(t *testing.T) {
	runner := NewRunner(agents.NewService(), tools.NewRegistry())
	if _, err := runner.Run(context.Background(), "missing-agent", "hello"); err == nil {
		t.Fatal("Run should reject unknown agent IDs")
	}
}

// --- provider-driven loop tests ---

// scriptedProvider answers each call with a pre-set response.
type scriptedProvider struct {
	mu       sync.Mutex
	name     string
	resps    []*models.CompletionResponse
	errs     []error
	calls    int
	requests []models.CompletionRequest
}

func (p *scriptedProvider) Name() string { return p.name }

func (p *scriptedProvider) Complete(ctx context.Context, req models.CompletionRequest) (*models.CompletionResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	i := p.calls
	if i >= len(p.resps) && i >= len(p.errs) {
		// default: final answer with whatever is left
		return &models.CompletionResponse{Text: "done", Model: "scripted"}, nil
	}
	p.calls++
	if i < len(p.requests) || true {
		p.requests = append(p.requests, req)
	}
	if i < len(p.errs) && p.errs[i] != nil {
		return nil, p.errs[i]
	}
	return p.resps[i], nil
}

func TestRunWithProviderFinalAnswer(t *testing.T) {
	agentService := agents.NewService()
	agent := newTestAgent(t, agentService)
	provider := &scriptedProvider{name: "scripted", resps: []*models.CompletionResponse{
		{Text: "the answer is 42", Model: "test-model", Usage: models.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}},
	}}
	runner := NewRunnerWithOptions(agentService, tools.NewRegistry(), WithProvider(provider))

	run, err := runner.Run(context.Background(), agent.ID, "hello agent")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if run.Status != StatusCompleted {
		t.Fatalf("expected completed, got %s (error: %s)", run.Status, run.Error)
	}
	if run.Output != "the answer is 42" {
		t.Fatalf("unexpected output %q", run.Output)
	}
	if run.Tokens.TotalTokens != 15 {
		t.Fatalf("expected token usage recorded, got %+v", run.Tokens)
	}
	if run.Steps != 1 {
		t.Fatalf("expected 1 step, got %d", run.Steps)
	}
}

func TestRunWithProviderToolCallRoundTrip(t *testing.T) {
	agentService := agents.NewService()
	agent := newTestAgent(t, agentService)
	registry := tools.NewRegistry()
	registry.Register(tools.NewCalculatorTool())

	provider := &scriptedProvider{name: "scripted", resps: []*models.CompletionResponse{
		{Text: `{"tool": "calculator", "arguments": {"expression": "6 * 7"}}`},
		{Text: "the result is 42"},
	}}
	runner := NewRunnerWithOptions(agentService, registry, WithProvider(provider))

	run, err := runner.Run(context.Background(), agent.ID, "compute 6*7 for me")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if run.Status != StatusCompleted {
		t.Fatalf("expected completed, got %s (%s)", run.Status, run.Error)
	}
	if run.Output != "the result is 42" {
		t.Fatalf("unexpected output %q", run.Output)
	}
	if run.Steps != 2 {
		t.Fatalf("expected 2 steps (model+tool answered), got %d", run.Steps)
	}
}

func TestRunWithProviderMaxStepsAbort(t *testing.T) {
	agentService := agents.NewService()
	agent := newTestAgent(t, agentService)
	registry := tools.NewRegistry()
	registry.Register(tools.NewCalculatorTool())

	// Always demands the same tool call -> loop detector fires at 3 repeats,
	// before maxSteps=10.
	call := `{"tool": "calculator", "arguments": {"expression": "1 + 1"}}`
	provider := &scriptedProvider{name: "scripted", resps: []*models.CompletionResponse{
		{Text: call}, {Text: call}, {Text: call},
	}}
	runner := NewRunnerWithOptions(agentService, registry, WithProvider(provider))

	run, err := runner.Run(context.Background(), agent.ID, "loop please")
	if err == nil {
		t.Fatal("expected loop detection error")
	}
	if !errors.Is(err, ErrLoopDetected) {
		t.Fatalf("expected ErrLoopDetected, got %v", err)
	}
	if run.Status != StatusFailed {
		t.Fatalf("expected failed status, got %s", run.Status)
	}

	// Distinct arguments each time defeat loop detection -> max steps fires.
	provider2 := &scriptedProvider{name: "scripted", resps: make([]*models.CompletionResponse, 0, 12)}
	for i := 0; i < 12; i++ {
		b, _ := json.Marshal(map[string]any{"expression": "1 + " + string(rune('0'+i%10))})
		provider2.resps = append(provider2.resps, &models.CompletionResponse{Text: `{"tool": "calculator", "arguments": ` + string(b) + `}`})
	}
	runner2 := NewRunnerWithOptions(agentService, registry, WithProvider(provider2), WithLimits(5, time.Minute))
	_, err = runner2.Run(context.Background(), agent.ID, "churn")
	if !errors.Is(err, ErrMaxStepsExceeded) {
		t.Fatalf("expected ErrMaxStepsExceeded, got %v", err)
	}
}

// blockingProvider sleeps until its response delay elapses or ctx is done,
// faithfully modeling a real provider call under cancellation.
type blockingProvider struct {
	name     string
	delay    time.Duration
	respText string
}

func (p *blockingProvider) Name() string { return p.name }

func (p *blockingProvider) Complete(ctx context.Context, req models.CompletionRequest) (*models.CompletionResponse, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(p.delay):
		return &models.CompletionResponse{Text: p.respText, Model: "blocking"}, nil
	}
}

func TestRunWithProviderCancellationHonored(t *testing.T) {
	agentService := agents.NewService()
	agent := newTestAgent(t, agentService)
	provider := &blockingProvider{name: "blocking", delay: 2 * time.Second, respText: "too late"}
	runner := NewRunnerWithOptions(agentService, tools.NewRegistry(), WithProvider(provider))

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	run, err := runner.RunWithID(ctx, "run-cancel", agent.ID, "go")
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if run.Status != StatusFailed {
		t.Fatalf("expected failed status, got %s", run.Status)
	}
}

func TestRunWithProviderMaxRuntimeEnforced(t *testing.T) {
	agentService := agents.NewService()
	agent := newTestAgent(t, agentService)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"late"}}]}`))
	}))
	defer slow.Close()

	provider := models.NewOpenAIProvider("slow", "test-key", slow.URL, "test-model", slow.Client())
	runner := NewRunnerWithOptions(agentService, tools.NewRegistry(), WithProvider(provider), WithLimits(5, 30*time.Millisecond))

	_, err := runner.Run(context.Background(), agent.ID, "be quick")
	if !errors.Is(err, ErrMaxRuntimeExceeded) && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected runtime budget error, got %v", err)
	}
}

func TestStepRecorderReceivesModelAndToolSteps(t *testing.T) {
	agentService := agents.NewService()
	agent := newTestAgent(t, agentService)
	registry := tools.NewRegistry()
	registry.Register(tools.NewCalculatorTool())

	provider := &scriptedProvider{name: "scripted", resps: []*models.CompletionResponse{
		{Text: `{"tool": "calculator", "arguments": {"expression": "2 + 2"}}`, Usage: models.Usage{TotalTokens: 7}},
		{Text: "final: 4"},
	}}

	var mu sync.Mutex
	var steps []Step
	rec := StepRecorderFunc(func(ctx context.Context, runID string, step Step) error {
		mu.Lock()
		defer mu.Unlock()
		steps = append(steps, step)
		return nil
	})
	runner := NewRunnerWithOptions(agentService, registry, WithProvider(provider), WithStepRecorder(rec))

	run, err := runner.Run(context.Background(), agent.ID, "record me")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if run.Status != StatusCompleted {
		t.Fatalf("expected completed, got %s", run.Status)
	}
	mu.Lock()
	defer mu.Unlock()
	// model(tool call) -> tool -> model(final answer) = 3 recorded steps
	if len(steps) != 3 {
		t.Fatalf("expected 3 recorded steps, got %d", len(steps))
	}
	if steps[0].Type != StepTypeModel || steps[1].Type != StepTypeTool || steps[2].Type != StepTypeModel {
		t.Fatalf("unexpected step types: %s, %s, %s", steps[0].Type, steps[1].Type, steps[2].Type)
	}
	if steps[1].Name != "calculator" || steps[1].Status != StepSucceeded {
		t.Fatalf("unexpected tool step: %+v", steps[1])
	}
}

func TestRunGeneratesUniqueRunIDs(t *testing.T) {
	agentService := agents.NewService()
	agent := newTestAgent(t, agentService)
	runner := NewRunner(agentService, tools.NewRegistry())

	r1, _ := runner.Run(context.Background(), agent.ID, "a")
	r2, _ := runner.Run(context.Background(), agent.ID, "b")
	if r1.ID == r2.ID || r1.ID == "" || r1.ID == "run-1" {
		t.Fatalf("expected unique generated IDs, got %q and %q", r1.ID, r2.ID)
	}
}

func TestRunWithIDUsesSuppliedID(t *testing.T) {
	agentService := agents.NewService()
	agent := newTestAgent(t, agentService)
	runner := NewRunner(agentService, tools.NewRegistry())
	run, err := runner.RunWithID(context.Background(), "run-abc123", agent.ID, "x")
	if err != nil {
		t.Fatalf("RunWithID returned error: %v", err)
	}
	if run.ID != "run-abc123" {
		t.Fatalf("expected supplied run id, got %q", run.ID)
	}
}

func TestParseToolCallDefensive(t *testing.T) {
	if _, _, is := parseToolCall("plain final answer"); is {
		t.Fatal("plain text must not parse as tool call")
	}
	if _, _, is := parseToolCall(`{"no_tool": true}`); is {
		t.Fatal("object without tool field must not parse as tool call")
	}
	if _, _, is := parseToolCall("not json {"); is {
		t.Fatal("malformed json must not parse as tool call")
	}
	name, args, is := parseToolCall("```json\n{\"tool\":\"calculator\",\"arguments\":{\"expression\":\"1+1\"}}\n```")
	if !is || name != "calculator" || args["expression"] != "1+1" {
		t.Fatalf("fenced tool call should parse: %v %v %v", name, args, is)
	}
}

func TestToolFailureFeedsBackToModel(t *testing.T) {
	agentService := agents.NewService()
	agent := newTestAgent(t, agentService)
	registry := tools.NewRegistry() // empty: calculator NOT registered

	provider := &scriptedProvider{name: "scripted", resps: []*models.CompletionResponse{
		{Text: `{"tool": "calculator", "arguments": {"expression": "1 + 1"}}`},
		{Text: "recovered, answer is 2"},
	}}
	runner := NewRunnerWithOptions(agentService, registry, WithProvider(provider))

	run, err := runner.Run(context.Background(), agent.ID, "use missing tool")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if run.Status != StatusCompleted || run.Output != "recovered, answer is 2" {
		t.Fatalf("expected recovery after tool failure, got %s / %q", run.Status, run.Output)
	}
	// second request must contain the tool error observation
	last := provider.requests[len(provider.requests)-1]
	found := false
	for _, m := range last.Messages {
		if m.Role == "tool" && strings.Contains(m.Content, "tool error") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected tool error observation in follow-up messages: %+v", last.Messages)
	}
}
