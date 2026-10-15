package runtime

// shutdown_test.go — issue #107 tests: graceful cancellation (current step
// completes, partial results recorded, run.cancelled event published, typed
// outcome), the runner shutdown gate (Stop / Shutdown / grace timeout) and
// the token-budget/event wiring inside the provider loop.
//
// Determinism: every blocking point is a channel the test controls (gated
// providers) or a context deadline; waits poll bounded state (InFlight) or
// read channels. There are no sleeps whose duration races production code —
// the only fixed waits guard NEGATIVE assertions (a correct Shutdown must
// still be blocked) and can never flake in the failure direction.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"agentos/internal/agents"
	"agentos/internal/events"
	"agentos/internal/models"
	"agentos/internal/policies"
	"agentos/internal/tools"
)

// --- deterministic fakes -----------------------------------------------------

// gatedProvider answers each Complete call from a per-call script: gates[i]
// (when non-nil) blocks call i until the test closes it or the run context is
// cancelled — modeling a real provider call under cancellation; resps[i]
// (when non-nil) is the scripted answer; anything unscripted answers with a
// final "done".
type gatedProvider struct {
	name string

	mu       sync.Mutex
	gates    []chan struct{}
	resps    []*models.CompletionResponse
	usages   []models.Usage
	calls    int
	requests []models.CompletionRequest
}

func (p *gatedProvider) Name() string { return p.name }

func (p *gatedProvider) Complete(ctx context.Context, req models.CompletionRequest) (*models.CompletionResponse, error) {
	p.mu.Lock()
	i := p.calls
	p.calls++
	var gate chan struct{}
	if i < len(p.gates) {
		gate = p.gates[i]
	}
	p.requests = append(p.requests, req)
	p.mu.Unlock()

	if gate != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-gate:
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if i < len(p.resps) && p.resps[i] != nil {
		resp := *p.resps[i]
		if i < len(p.usages) {
			resp.Usage = p.usages[i]
		}
		return &resp, nil
	}
	return &models.CompletionResponse{Text: "done", Model: "gated"}, nil
}

func (p *gatedProvider) gate(i int) chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.gates[i]
}

// capturingPublisher records every published event (contract-valid fakes in
// these tests never publish invalid types; the platform publishers would
// reject those).
type capturingPublisher struct {
	mu     sync.Mutex
	events []events.Event
}

func (c *capturingPublisher) Publish(_ context.Context, e events.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
	return nil
}

func (c *capturingPublisher) snapshot() []events.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]events.Event(nil), c.events...)
}

// waitForInFlight polls the runner gate until it holds want runs (bounded;
// the tiny sleep is a poll interval, not a timing assumption).
func waitForInFlight(t *testing.T, r *Runner, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if r.InFlight() == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("runner never reached in-flight=%d (now %d)", want, r.InFlight())
}

// newShutdownTestRunner builds a provider-mode runner with a recorder,
// publisher and runtime config; the agent is created in org-1.
func newShutdownTestRunner(t *testing.T, provider models.Provider, pub events.Publisher, cfg RuntimeConfig, rec StepRecorder) (*Runner, string) {
	t.Helper()
	agentService := agents.NewService()
	agent := newTestAgent(t, agentService)
	registry := tools.NewRegistry()
	registry.Register(tools.NewCalculatorTool())
	runner := NewRunnerWithOptions(agentService, registry,
		WithProvider(provider),
		WithStepRecorder(rec),
		WithEventPublisher(pub),
		WithRuntimeConfig(cfg),
	)
	return runner, agent.ID
}

// --- graceful cancellation ---------------------------------------------------

func TestGracefulCancellationMidModelStepCompletesStepAndRecordsPartial(t *testing.T) {
	// The provider call is in flight when the caller cancels: the provider
	// returns the cancellation, the CURRENT step is recorded as failed, the
	// cancellation marker summarizing the partial state is recorded and the
	// loop unwinds with the typed outcome.
	provider := &gatedProvider{name: "gated", gates: []chan struct{}{make(chan struct{})}}
	rec := &recordingRecorder{}
	runner, agentID := newShutdownTestRunner(t, provider, nil, RuntimeConfig{}, rec)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		waitForInFlight(t, runner, 1)
		cancel() // cancel while the model call is gated
	}()
	run, err := runner.RunWithID(ctx, "run-cancel-mid", agentID, "hello")
	cancel()

	if err == nil {
		t.Fatal("expected the cancellation to surface")
	}
	if !errors.Is(err, ErrRunCancelled) {
		t.Fatalf("expected ErrRunCancelled, got %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("the caller's cause must stay classifiable (context.Canceled), got %v", err)
	}
	if run == nil || run.Status != StatusFailed {
		t.Fatalf("expected failed run, got %+v", run)
	}

	steps := rec.all()
	if len(steps) != 2 {
		t.Fatalf("expected [failed model step, cancellation marker], got %+v", steps)
	}
	if steps[0].Type != StepTypeModel || steps[0].Status != StepFailed ||
		!strings.Contains(steps[0].Error, context.Canceled.Error()) {
		t.Fatalf("current step must complete (failed, with the cause): %+v", steps[0])
	}
	marker := steps[1]
	if marker.Type != StepTypeCancelled || marker.Status != StepFailed {
		t.Fatalf("expected a failed cancellation marker, got %+v", marker)
	}
	if !strings.Contains(marker.Input, "run cancelled at step 1") {
		t.Fatalf("marker must carry the partial state: %q", marker.Input)
	}
	if !strings.Contains(marker.Error, ErrRunCancelled.Error()) {
		t.Fatalf("marker must carry the typed cause: %q", marker.Error)
	}
}

func TestGracefulCancellationAfterToolStepRecordsPartialResults(t *testing.T) {
	// Step 1 completes (model tool call + successful tool execution), then the
	// caller cancels while the NEXT model call is in flight: the partial
	// results (2 recorded steps) are visible on the marker.
	release := make(chan struct{})
	provider := &gatedProvider{
		name:  "gated",
		gates: []chan struct{}{nil, release},
		resps: []*models.CompletionResponse{
			{Text: `{"tool": "calculator", "arguments": {"expression": "2 + 2"}}`},
		},
	}
	rec := &recordingRecorder{}
	runner, agentID := newShutdownTestRunner(t, provider, nil, RuntimeConfig{}, rec)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		// Cancel only after the first loop iteration fully completed (model +
		// tool recorded) — the recorder observes the tool step.
		waitForInFlight(t, runner, 1)
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if len(rec.all()) >= 2 {
				cancel()
				return
			}
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	run, err := runner.RunWithID(ctx, "run-cancel-partial", agentID, "compute 2+2")
	cancel()

	if !errors.Is(err, ErrRunCancelled) {
		t.Fatalf("expected ErrRunCancelled, got %v", err)
	}
	if run.Steps != 2 {
		t.Fatalf("expected 2 completed steps, got %d", run.Steps)
	}
	steps := rec.all()
	if len(steps) != 4 {
		t.Fatalf("expected [model, tool, failed model, marker], got %+v", steps)
	}
	if steps[0].Type != StepTypeModel || steps[0].Status != StepSucceeded ||
		steps[1].Type != StepTypeTool || steps[1].Status != StepSucceeded {
		t.Fatalf("the completed steps must be intact: %+v", steps[:2])
	}
	if !strings.Contains(steps[3].Input, "run cancelled at step 2") {
		t.Fatalf("marker must record the partial results: %q", steps[3].Input)
	}
}

func TestGracefulCancellationBetweenSteps(t *testing.T) {
	// The recorder cancels exactly when the tool step lands, so the loop's
	// top-of-iteration check unwinds BEFORE the next model call starts.
	release := make(chan struct{})
	defer close(release) // unblock the never-started second call (cleanup)
	provider := &gatedProvider{
		name:  "gated",
		gates: []chan struct{}{nil, release},
		resps: []*models.CompletionResponse{
			{Text: `{"tool": "calculator", "arguments": {"expression": "3 + 3"}}`},
		},
	}
	rec := &recordingRecorder{}
	runner, agentID := newShutdownTestRunner(t, provider, nil, RuntimeConfig{}, rec)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	recorder := StepRecorderFunc(func(c context.Context, runID string, step Step) error {
		if err := rec.RecordStep(c, runID, step); err != nil {
			return err
		}
		if step.Type == StepTypeTool {
			cancel() // cancel exactly between steps
		}
		return nil
	})
	runner.recorder = recorder

	run, err := runner.RunWithID(ctx, "run-cancel-between", agentID, "compute 3+3")
	if !errors.Is(err, ErrRunCancelled) {
		t.Fatalf("expected ErrRunCancelled, got %v", err)
	}
	steps := rec.all()
	if len(steps) != 3 {
		t.Fatalf("expected [model, tool, marker] with no extra model call, got %+v", steps)
	}
	if steps[2].Type != StepTypeCancelled {
		t.Fatalf("expected the cancellation marker last, got %+v", steps[2])
	}
	if !strings.Contains(steps[2].Input, "run cancelled at step 1") {
		t.Fatalf("marker must reflect run.Steps at unwind: %q", steps[2].Input)
	}
	if run.Status != StatusFailed {
		t.Fatalf("cancellation maps to the existing FAILED status, got %s", run.Status)
	}
}

func TestCancellationPublishesRunCancelledEventOnlyWithTenantScope(t *testing.T) {
	pub := &capturingPublisher{}
	provider := &gatedProvider{name: "gated", gates: []chan struct{}{make(chan struct{})}}
	rec := &recordingRecorder{}
	runner, agentID := newShutdownTestRunner(t, provider, pub, RuntimeConfig{}, rec)

	// Tenant scope stamped (the worker stamps the run scope on every task):
	// the run.cancelled event reaches the bus.
	ctx := policies.WithRunScope(context.Background(), policies.RunScope{OrganizationID: "org-1"})
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		waitForInFlight(t, runner, 1)
		cancel()
	}()
	_, err := runner.RunWithID(ctx, "run-cancel-event", agentID, "hello")
	cancel()
	if !errors.Is(err, ErrRunCancelled) {
		t.Fatalf("expected ErrRunCancelled, got %v", err)
	}
	published := pub.snapshot()
	if len(published) != 1 {
		t.Fatalf("expected exactly one event, got %+v", published)
	}
	if published[0].Type != events.EventRunCancelled ||
		published[0].TenantID != "org-1" ||
		published[0].Resource != (events.Resource{Type: "run", ID: "run-cancel-event"}) {
		t.Fatalf("run.cancelled event shape wrong: %+v", published[0])
	}

	// No tenant scope (legacy callers): the event seam stays silent (the
	// events contract requires a tenant) and the run still cancels.
	pub2 := &capturingPublisher{}
	provider2 := &gatedProvider{name: "gated", gates: []chan struct{}{make(chan struct{})}}
	runner2, agentID2 := newShutdownTestRunner(t, provider2, pub2, RuntimeConfig{}, &recordingRecorder{})
	ctx2, cancel2 := context.WithCancel(context.Background())
	go func() {
		waitForInFlight(t, runner2, 1)
		cancel2()
	}()
	_, err = runner2.RunWithID(ctx2, "run-cancel-noscope", agentID2, "hello")
	cancel2()
	if !errors.Is(err, ErrRunCancelled) {
		t.Fatalf("expected ErrRunCancelled, got %v", err)
	}
	if got := pub2.snapshot(); len(got) != 0 {
		t.Fatalf("no scope -> no event, got %+v", got)
	}
}

// --- token budget end-to-end -------------------------------------------------

func TestTokenBudgetExceededHardStopsRunAndPublishesEvent(t *testing.T) {
	pub := &capturingPublisher{}
	provider := &gatedProvider{
		name:  "gated",
		resps: []*models.CompletionResponse{{Text: "big answer"}},
		usages: []models.Usage{
			{PromptTokens: 30, CompletionTokens: 20, TotalTokens: 50},
		},
	}
	rec := &recordingRecorder{}
	cfg := RuntimeConfig{TokenBudget: TokenBudgetConfig{MaxTotalTokens: 10}}
	runner, agentID := newShutdownTestRunner(t, provider, pub, cfg, rec)

	ctx := policies.WithRunScope(context.Background(), policies.RunScope{OrganizationID: "org-1"})
	run, err := runner.RunWithID(ctx, "run-budget", agentID, "hello")
	if err == nil {
		t.Fatal("expected the budget violation to fail the run")
	}
	if !errors.Is(err, ErrTokenBudgetExceeded) {
		t.Fatalf("expected ErrTokenBudgetExceeded, got %v", err)
	}
	if !strings.HasPrefix(err.Error(), "token_budget_exceeded:") {
		t.Fatalf("machine-readable outcome marker missing: %q", err.Error())
	}
	if run.Status != StatusFailed || run.Tokens.TotalTokens != 50 {
		t.Fatalf("run must fail through the existing path with usage recorded: %+v", run)
	}

	steps := rec.all()
	if len(steps) != 2 || steps[0].Type != StepTypeModel || steps[0].Status != StepSucceeded {
		t.Fatalf("expected [model, budget annotation], got %+v", steps)
	}
	bstep := steps[1]
	if bstep.Type != StepTypeBudget || bstep.Status != StepFailed {
		t.Fatalf("expected a failed budget annotation, got %+v", bstep)
	}
	var snap BudgetSnapshot
	if jerr := json.Unmarshal([]byte(bstep.Output), &snap); jerr != nil {
		t.Fatalf("annotation must carry the snapshot JSON: %v (%q)", jerr, bstep.Output)
	}
	if snap.Phase != "exceeded" || snap.Limit != budgetDimensionTotal || snap.TotalTokens != 50 {
		t.Fatalf("snapshot wrong: %+v", snap)
	}
	if !strings.Contains(bstep.Error, "token_budget_exceeded") {
		t.Fatalf("annotation error must carry the outcome: %q", bstep.Error)
	}

	published := pub.snapshot()
	if len(published) != 1 || published[0].Type != events.EventRunTokenBudgetExceeded {
		t.Fatalf("expected one run.token_budget_exceeded event, got %+v", published)
	}
	if published[0].TenantID != "org-1" || published[0].Resource.ID != "run-budget" {
		t.Fatalf("event tenant/resource wrong: %+v", published[0])
	}
	if published[0].Payload["dimension"] != budgetDimensionTotal || published[0].Payload["limit"] != 10 {
		t.Fatalf("event payload wrong: %+v", published[0].Payload)
	}
}

func TestTokenBudgetWarnAnnotationRecordedOnce(t *testing.T) {
	pub := &capturingPublisher{}
	provider := &gatedProvider{
		name:  "gated",
		resps: []*models.CompletionResponse{{Text: "one"}, {Text: "two"}},
		usages: []models.Usage{
			{TotalTokens: 85}, // crosses 0.8 * 100
			{TotalTokens: 5},  // no second warn
		},
	}
	rec := &recordingRecorder{}
	cfg := RuntimeConfig{TokenBudget: TokenBudgetConfig{MaxTotalTokens: 100}}
	runner, agentID := newShutdownTestRunner(t, provider, pub, cfg, rec)

	run, err := runner.Run(context.Background(), agentID, "hello")
	if err != nil {
		t.Fatalf("run under the hard limit must complete: %v", err)
	}
	if run.Status != StatusCompleted {
		t.Fatalf("expected completed, got %s (%s)", run.Status, run.Error)
	}
	var budgetSteps []Step
	for _, s := range rec.all() {
		if s.Type == StepTypeBudget {
			budgetSteps = append(budgetSteps, s)
		}
	}
	if len(budgetSteps) != 1 {
		t.Fatalf("warn must be recorded exactly once, got %+v", rec.all())
	}
	if budgetSteps[0].Status != StepSucceeded {
		t.Fatalf("warn annotation is not a failure, got %+v", budgetSteps[0])
	}
	var snap BudgetSnapshot
	if jerr := json.Unmarshal([]byte(budgetSteps[0].Output), &snap); jerr != nil {
		t.Fatalf("warn snapshot must be JSON: %v", jerr)
	}
	if snap.Phase != "warn" || snap.TotalTokens != 85 || snap.Limit != budgetDimensionTotal {
		t.Fatalf("warn snapshot wrong: %+v", snap)
	}
	if got := pub.snapshot(); len(got) != 0 {
		t.Fatalf("a warn must not publish events, got %+v", got)
	}
}

func TestNoBudgetConfiguredKeepsLegacyStepStream(t *testing.T) {
	// Defaults are unlimited: no budget annotations ever appear and the loop
	// behavior is byte-identical to the pre-#107 stream.
	provider := &gatedProvider{
		name:  "gated",
		resps: []*models.CompletionResponse{{Text: "answer"}},
		usages: []models.Usage{
			{PromptTokens: 100000, CompletionTokens: 100000, TotalTokens: 200000},
		},
	}
	rec := &recordingRecorder{}
	runner, agentID := newShutdownTestRunner(t, provider, nil, RuntimeConfig{}, rec)

	if _, err := runner.Run(context.Background(), agentID, "hello"); err != nil {
		t.Fatalf("run returned error: %v", err)
	}
	steps := rec.all()
	if len(steps) != 1 || steps[0].Type != StepTypeModel {
		t.Fatalf("expected a single model step, got %+v", steps)
	}
}

func TestPerAgentConfigSourceOverridesAndInvalidFallback(t *testing.T) {
	provider := &gatedProvider{
		name:  "gated",
		resps: []*models.CompletionResponse{{Text: "answer"}},
		usages: []models.Usage{
			{TotalTokens: 50},
		},
	}
	cases := []struct {
		name        string
		raw         json.RawMessage
		wantErr     bool
		errContains string
	}{
		{
			name:        "valid override enforces the agent budget",
			raw:         json.RawMessage(`{"token_budget":{"max_total_tokens":10}}`),
			wantErr:     true,
			errContains: "token_budget_exceeded",
		},
		{
			name:    "invalid override falls back to the runner defaults",
			raw:     json.RawMessage(`{"token_budget": {`), // malformed
			wantErr: false,
		},
		{
			name:    "blank override falls back to the runner defaults",
			raw:     json.RawMessage(`   `),
			wantErr: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &recordingRecorder{}
			agentService := agents.NewService()
			agent := newTestAgent(t, agentService)
			runner := NewRunnerWithOptions(agentService, tools.NewRegistry(),
				WithProvider(provider),
				WithStepRecorder(rec),
				WithAgentConfigSource(func(agentID string) json.RawMessage { return c.raw }),
			)
			run, err := runner.Run(context.Background(), agent.ID, "hello")
			if c.wantErr {
				if err == nil || !strings.Contains(err.Error(), c.errContains) {
					t.Fatalf("expected %s, got %v", c.errContains, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("run should complete on the fallback defaults: %v", err)
			}
			if run.Status != StatusCompleted {
				t.Fatalf("expected completed, got %s", run.Status)
			}
			for _, s := range rec.all() {
				if s.Type == StepTypeBudget {
					t.Fatalf("no budget annotations expected on the fallback path: %+v", s)
				}
			}
		})
	}
}

// --- conversation history end-to-end ----------------------------------------

func TestRuntimeLoopTruncatesHistoryPerConfig(t *testing.T) {
	// After the first iteration the full history is [user(6 tok), assistant
	// tool-call(~18 tok), tool observation(8 tok)]. With max_tokens 20 only
	// the newest message fits the sliding window; disabled (-1) sends all.
	cases := []struct {
		name       string
		maxTokens  int
		wantSecond int // messages in the second request
		wantRole   string
	}{
		{"tight budget keeps only the newest turn", 20, 1, "tool"},
		{"negative disables history management", -1, 3, "tool"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			provider := &gatedProvider{
				name: "gated",
				resps: []*models.CompletionResponse{
					{Text: `{"tool": "calculator", "arguments": {"expression": "2 + 2"}}`},
					{Text: "the result is 4"},
				},
			}
			cfg := RuntimeConfig{ConversationHistory: ConversationHistoryConfig{
				Strategy:  StrategySlidingWindow,
				MaxTokens: c.maxTokens,
			}}
			agentService := agents.NewService()
			agent := newTestAgent(t, agentService)
			registry := tools.NewRegistry()
			registry.Register(tools.NewCalculatorTool())
			runner := NewRunnerWithOptions(agentService, registry,
				WithProvider(provider), WithRuntimeConfig(cfg))

			run, err := runner.Run(context.Background(), agent.ID, "compute 2+2")
			if err != nil {
				t.Fatalf("run returned error: %v", err)
			}
			if run.Status != StatusCompleted || run.Output != "the result is 4" {
				t.Fatalf("run wrong: %+v", run)
			}
			provider.mu.Lock()
			requests := append([]models.CompletionRequest(nil), provider.requests...)
			provider.mu.Unlock()
			if len(requests) != 2 {
				t.Fatalf("expected 2 provider calls, got %d", len(requests))
			}
			got := requests[1].Messages
			if len(got) != c.wantSecond {
				t.Fatalf("second request messages = %d, want %d: %+v", len(got), c.wantSecond, got)
			}
			if got[len(got)-1].Role != c.wantRole {
				t.Fatalf("newest message must be a tool turn, got %+v", got[len(got)-1])
			}
			if c.wantSecond == 1 && got[0].Content == "" {
				t.Fatal("the truncated window must keep the tool observation content")
			}
		})
	}
}

// --- shutdown gate ------------------------------------------------------------

func TestStopRefusesNewRuns(t *testing.T) {
	agentService := agents.NewService()
	agent := newTestAgent(t, agentService)
	runner := NewRunner(agentService, tools.NewRegistry())

	if got := runner.InFlight(); got != 0 {
		t.Fatalf("idle runner in-flight = %d, want 0", got)
	}
	runner.Stop()
	run, err := runner.RunWithID(context.Background(), "run-stopped", agent.ID, "x")
	if !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("expected ErrShuttingDown, got %v", err)
	}
	if run != nil {
		t.Fatalf("a refused run returns no run state, got %+v", run)
	}
	if got := runner.InFlight(); got != 0 {
		t.Fatalf("refused runs must not count as in-flight, got %d", got)
	}
	// Stop is idempotent and the refusal is sticky.
	runner.Stop()
	if _, err := runner.RunWithID(context.Background(), "run-stopped-2", agent.ID, "x"); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("shutdown is irreversible, got %v", err)
	}
}

func TestShutdownIdleRunnerReturnsImmediately(t *testing.T) {
	runner := NewRunner(agents.NewService(), tools.NewRegistry())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := runner.Shutdown(ctx); err != nil {
		t.Fatalf("idle Shutdown must drain immediately, got %v", err)
	}
	// And the runner is now stopped.
	agentService := agents.NewService()
	agent := newTestAgent(t, agentService)
	if _, err := runner.RunWithID(ctx, "run-after", agent.ID, "x"); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("expected ErrShuttingDown after Shutdown, got %v", err)
	}
}

func TestShutdownNilSafe(t *testing.T) {
	var runner *Runner
	runner.Stop() // must not panic
	if err := runner.Shutdown(context.Background()); err != nil {
		t.Fatalf("nil Shutdown must be a no-op, got %v", err)
	}
	if got := runner.InFlight(); got != 0 {
		t.Fatalf("nil InFlight must be 0, got %d", got)
	}
}

func TestShutdownWaitsForInflightRun(t *testing.T) {
	provider := &gatedProvider{name: "gated", gates: []chan struct{}{make(chan struct{})}}
	agentService := agents.NewService()
	agent := newTestAgent(t, agentService)
	runner := NewRunnerWithOptions(agentService, tools.NewRegistry(), WithProvider(provider))

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		if _, err := runner.Run(context.Background(), agent.ID, "x"); err != nil {
			t.Errorf("run returned error: %v", err)
		}
	}()
	waitForInFlight(t, runner, 1)

	shutdownErr := make(chan error, 1)
	go func() { shutdownErr <- runner.Shutdown(context.Background()) }()

	// NEGATIVE guard: with the run gated, a correct Shutdown cannot return.
	select {
	case err := <-shutdownErr:
		t.Fatalf("Shutdown returned while a run was in flight: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(provider.gates[0]) // let the run finish
	if err := <-shutdownErr; err != nil {
		t.Fatalf("Shutdown must return nil once drained, got %v", err)
	}
	<-runDone
	if got := runner.InFlight(); got != 0 {
		t.Fatalf("drained runner in-flight = %d, want 0", got)
	}
}

func TestShutdownTimesOutWithInflightRun(t *testing.T) {
	provider := &gatedProvider{name: "gated", gates: []chan struct{}{make(chan struct{})}}
	agentService := agents.NewService()
	agent := newTestAgent(t, agentService)
	runner := NewRunnerWithOptions(agentService, tools.NewRegistry(), WithProvider(provider))

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_, _ = runner.Run(context.Background(), agent.ID, "x")
	}()
	waitForInFlight(t, runner, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := runner.Shutdown(ctx)
	if !errors.Is(err, ErrShutdownTimeout) {
		t.Fatalf("expected ErrShutdownTimeout, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("grace period not honored: %v", elapsed)
	}
	if !strings.Contains(err.Error(), "1 run(s) still in flight") {
		t.Fatalf("timeout error should report the in-flight count: %q", err.Error())
	}

	// Drain: the stop state is sticky, a retry with a fresh context succeeds
	// once the run finishes.
	close(provider.gates[0])
	<-runDone
	if err := runner.Shutdown(context.Background()); err != nil {
		t.Fatalf("retry after drain must succeed, got %v", err)
	}
}

func TestShutdownConcurrentCallersAreSafe(t *testing.T) {
	provider := &gatedProvider{name: "gated", gates: []chan struct{}{make(chan struct{})}}
	agentService := agents.NewService()
	agent := newTestAgent(t, agentService)
	runner := NewRunnerWithOptions(agentService, tools.NewRegistry(), WithProvider(provider))

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_, _ = runner.Run(context.Background(), agent.ID, "x")
	}()
	waitForInFlight(t, runner, 1)

	const waiters = 4
	var wg sync.WaitGroup
	errs := make([]error, waiters)
	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			errs[i] = runner.Shutdown(ctx)
		}(i)
	}
	close(provider.gates[0])
	wg.Wait()
	<-runDone
	for i, err := range errs {
		if err != nil {
			t.Fatalf("waiter %d failed: %v", i, err)
		}
	}
}

// --- env parsing ---------------------------------------------------------------

func TestShutdownGraceFromEnv(t *testing.T) {
	cases := []struct {
		name string
		val  string
		want time.Duration
	}{
		{"unset keeps the default", "", DefaultShutdownGrace},
		{"explicit seconds honored", "5", 5 * time.Second},
		{"explicit zero means no grace", "0", 0},
		{"negative falls back to the default", "-3", DefaultShutdownGrace},
		{"invalid falls back to the default", "soon", DefaultShutdownGrace},
		{"whitespace tolerated", " 7 ", 7 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(EnvShutdownGrace, c.val) // "" behaves like unset
			if got := ShutdownGraceFromEnv(); got != c.want {
				t.Fatalf("ShutdownGraceFromEnv() = %v, want %v", got, c.want)
			}
		})
	}
	if DefaultShutdownGrace != 10*time.Second {
		t.Fatalf("issue #107 default grace is 10s, got %v", DefaultShutdownGrace)
	}
}
