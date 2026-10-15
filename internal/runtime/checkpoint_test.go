package runtime

// checkpoint_test.go — issue #115: the durable-checkpoint seam. Pins the
// end-of-step save cadence, the resume-from-checkpoint behavior (history and
// token accounting restored, the user input NOT re-appended, the step bound
// still covering the cumulative count) and the corrupt-snapshot degradation
// to a fresh start.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"agentos/internal/agents"
	"agentos/internal/models"
	"agentos/internal/tools"
)

// seededCheckpointer returns one pre-baked checkpoint document for a single
// run (and nothing for any other run).
type seededCheckpointer struct {
	runID string
	state json.RawMessage
}

func (s *seededCheckpointer) SaveCheckpoint(context.Context, string, CheckpointState) error {
	return nil
}

func (s *seededCheckpointer) LatestCheckpoint(_ context.Context, runID string) (*Checkpoint, error) {
	if runID != s.runID {
		return nil, errNoCheckpoint
	}
	return &Checkpoint{RunID: runID, State: s.state}, nil
}

// memCheckpointer is a Checkpointer double: a mutex-guarded map plus the
// corruption hooks the degradation tests need.
type memCheckpointer struct {
	mu     sync.Mutex
	saved  map[string][]CheckpointState
	latest map[string]CheckpointState
	// corrupt makes the next LatestCheckpoint return a broken document.
	corrupt bool
	// saveErr fails every SaveCheckpoint (checkpoint failures never break a
	// run — the durability contract).
	saveErr error
}

func newMemCheckpointer() *memCheckpointer {
	return &memCheckpointer{saved: map[string][]CheckpointState{}, latest: map[string]CheckpointState{}}
}

func (m *memCheckpointer) SaveCheckpoint(_ context.Context, runID string, state CheckpointState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.saveErr != nil {
		return m.saveErr
	}
	cp := CheckpointState{StepIndex: state.StepIndex, Usage: state.Usage}
	cp.Messages = append(cp.Messages, state.Messages...)
	m.saved[runID] = append(m.saved[runID], cp)
	m.latest[runID] = cp
	return nil
}

func (m *memCheckpointer) LatestCheckpoint(_ context.Context, runID string) (*Checkpoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, ok := m.latest[runID]
	if !ok {
		return nil, errNoCheckpoint
	}
	if m.corrupt {
		return &Checkpoint{RunID: runID, StepIndex: state.StepIndex, State: json.RawMessage("{not json")}, nil
	}
	blob, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	return &Checkpoint{RunID: runID, StepIndex: state.StepIndex, State: blob}, nil
}

func userMessageCount(reqs []models.CompletionRequest) int {
	n := 0
	for _, req := range reqs {
		for _, msg := range req.Messages {
			if msg.Role == "user" {
				n++
			}
		}
	}
	return n
}

func TestCheckpointSavedAfterEachStep(t *testing.T) {
	agentsvc := agents.NewService()
	agent := newTestAgent(t, agentsvc)
	registry := tools.NewRegistry()
	registry.Register(tools.NewCalculatorTool())

	cp := newMemCheckpointer()
	provider := &scriptedProvider{name: "scripted", resps: []*models.CompletionResponse{
		{Text: `{"tool": "calculator", "arguments": {"expression": "6 * 7"}}`},
		{Text: "the result is 42", Model: "test-model", Usage: models.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}},
	}}
	runner := NewRunnerWithOptions(agentsvc, registry,
		WithProvider(provider), WithCheckpointer(cp))

	run, err := runner.RunWithID(context.Background(), "run-cp-1", agent.ID, "compute 6*7")
	if err != nil {
		t.Fatalf("RunWithID: %v", err)
	}
	if run.Steps != 2 {
		t.Fatalf("steps = %d, want 2", run.Steps)
	}
	cp.mu.Lock()
	saves := len(cp.saved["run-cp-1"])
	last := CheckpointState{}
	if saves > 0 {
		last = cp.saved["run-cp-1"][saves-1]
	}
	cp.mu.Unlock()
	if saves != 2 {
		t.Fatalf("checkpoint saves = %d, want one per completed step (2)", saves)
	}
	if last.StepIndex != 2 {
		t.Fatalf("last checkpoint step = %d, want 2", last.StepIndex)
	}
	if len(last.Messages) < 3 {
		t.Fatalf("last checkpoint must carry the conversation so far (user+assistant+tool), got %d messages", len(last.Messages))
	}
	if last.Usage.TotalTokens != 15 {
		t.Fatalf("last checkpoint usage = %+v, want the cumulative 15 tokens", last.Usage)
	}
}

func TestCheckpointSaveFailureNeverBreaksRun(t *testing.T) {
	agentsvc := agents.NewService()
	agent := newTestAgent(t, agentsvc)
	cp := newMemCheckpointer()
	cp.saveErr = errors.New("checkpoint store down")
	provider := &scriptedProvider{name: "scripted", resps: []*models.CompletionResponse{
		{Text: "final answer", Model: "test-model"},
	}}
	runner := NewRunnerWithOptions(agentsvc, tools.NewRegistry(),
		WithProvider(provider), WithCheckpointer(cp))

	run, err := runner.RunWithID(context.Background(), "run-cp-2", agent.ID, "hello")
	if err != nil || run.Status != StatusCompleted {
		t.Fatalf("a failing checkpointer must never break the run: run=%+v err=%v", run, err)
	}
}

func TestResumeFromCheckpoint(t *testing.T) {
	agentsvc := agents.NewService()
	agent := newTestAgent(t, agentsvc)

	// Pre-load the checkpoint of an interrupted run: one completed step with
	// the full conversation and the token accounting that already happened.
	checkpointed := CheckpointState{
		StepIndex: 1,
		Messages: []models.Message{
			{Role: "user", Content: "resume me"},
			{Role: "assistant", Content: "step one reply"},
		},
		Usage: models.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
	}
	blob, err := json.Marshal(checkpointed)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	cp := &seededCheckpointer{runID: "run-resume-1", state: blob}

	provider := &scriptedProvider{name: "scripted", resps: []*models.CompletionResponse{
		{Text: "resumed answer", Model: "test-model", Usage: models.Usage{PromptTokens: 20, CompletionTokens: 10, TotalTokens: 30}},
	}}
	runner := NewRunnerWithOptions(agentsvc, tools.NewRegistry(),
		WithProvider(provider), WithCheckpointer(cp))

	run, err := runner.RunWithID(context.Background(), "run-resume-1", agent.ID, "resume me")
	if err != nil || run.Status != StatusCompleted {
		t.Fatalf("resume run failed: run=%+v err=%v", run, err)
	}
	// The loop continued from step 2 (resumeStart+1): exactly ONE new step.
	if run.Steps != 2 {
		t.Fatalf("steps = %d, want 2 (1 checkpointed + 1 resumed)", run.Steps)
	}
	// Token accounting re-seeded from the checkpoint: cumulative 150+30.
	if run.Tokens.TotalTokens != 180 {
		t.Fatalf("total tokens = %d, want 180 (checkpoint reseed + resumed step)", run.Tokens.TotalTokens)
	}
	// The checkpointed history was restored and the user input was NOT
	// appended again: exactly one user message in every provider request.
	if n := userMessageCount(provider.requests); n != 1 {
		t.Fatalf("user messages across requests = %d, want exactly 1 (no duplicate input)", n)
	}
	if len(provider.requests) == 0 || !strings.Contains(provider.requests[0].Messages[1].Content, "step one reply") {
		t.Fatalf("provider must receive the restored history, got %+v", provider.requests)
	}
}

func TestResumeWithoutCheckpointStartsFresh(t *testing.T) {
	agentsvc := agents.NewService()
	agent := newTestAgent(t, agentsvc)
	cp := newMemCheckpointer() // wired but empty: fresh start
	provider := &scriptedProvider{name: "scripted", resps: []*models.CompletionResponse{
		{Text: "fresh answer", Model: "test-model"},
	}}
	runner := NewRunnerWithOptions(agentsvc, tools.NewRegistry(),
		WithProvider(provider), WithCheckpointer(cp))

	run, err := runner.RunWithID(context.Background(), "run-fresh", agent.ID, "hello")
	if err != nil || run.Status != StatusCompleted || run.Steps != 1 {
		t.Fatalf("no-checkpoint run must behave as before: run=%+v err=%v", run, err)
	}
	if n := userMessageCount(provider.requests); n != 1 {
		t.Fatalf("user messages = %d, want 1", n)
	}
}

func TestCorruptCheckpointDegradesToFreshStart(t *testing.T) {
	agentsvc := agents.NewService()
	agent := newTestAgent(t, agentsvc)
	cp := newMemCheckpointer()
	cp.saved["run-corrupt"] = []CheckpointState{{StepIndex: 3}}
	cp.latest["run-corrupt"] = CheckpointState{StepIndex: 3}
	cp.corrupt = true

	provider := &scriptedProvider{name: "scripted", resps: []*models.CompletionResponse{
		{Text: "clean start", Model: "test-model"},
	}}
	runner := NewRunnerWithOptions(agentsvc, tools.NewRegistry(),
		WithProvider(provider), WithCheckpointer(cp))

	run, err := runner.RunWithID(context.Background(), "run-corrupt", agent.ID, "hello")
	if err != nil || run.Status != StatusCompleted {
		t.Fatalf("corrupt snapshot must degrade to a fresh start, not fail: run=%+v err=%v", run, err)
	}
	if run.Steps != 1 {
		t.Fatalf("fresh start steps = %d, want 1", run.Steps)
	}
	// The user input IS appended on a fresh start.
	if n := userMessageCount(provider.requests); n != 1 {
		t.Fatalf("user messages = %d, want 1 (input appended on fresh start)", n)
	}
}

func TestEmptyCheckpointSnapshotIgnored(t *testing.T) {
	// A snapshot with step_index 0 or no messages is not usable: resume
	// degrades to a fresh start (resumeFromCheckpoint contract).
	agentsvc := agents.NewService()
	agent := newTestAgent(t, agentsvc)
	empty := CheckpointState{StepIndex: 0, Messages: nil}
	blob, _ := json.Marshal(empty)
	cp := &seededCheckpointer{runID: "run-empty", state: blob}
	provider := &scriptedProvider{name: "scripted", resps: []*models.CompletionResponse{
		{Text: "ok", Model: "test-model"},
	}}
	runner := NewRunnerWithOptions(agentsvc, tools.NewRegistry(),
		WithProvider(provider), WithCheckpointer(cp))
	run, err := runner.RunWithID(context.Background(), "run-empty", agent.ID, "hello")
	if err != nil || run.Steps != 1 {
		t.Fatalf("empty snapshot must start fresh: run=%+v err=%v", run, err)
	}
}

func TestNilCheckpointerKeepsLegacyBehavior(t *testing.T) {
	agentsvc := agents.NewService()
	agent := newTestAgent(t, agentsvc)
	runner := NewRunnerWithOptions(agentsvc, tools.NewRegistry(), WithProvider(&scriptedProvider{name: "s", resps: []*models.CompletionResponse{
		{Text: "done", Model: "test-model"},
	}}))
	runner.SetCheckpointer(nil) // nil-safe no-op
	run, err := runner.RunWithID(context.Background(), "run-plain", agent.ID, "hello")
	if err != nil || run.Status != StatusCompleted {
		t.Fatalf("no checkpointer: legacy behavior required, run=%+v err=%v", run, err)
	}
}
