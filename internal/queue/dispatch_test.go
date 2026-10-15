package queue

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// dispatch_test.go covers the task-dispatch layer (issue #116 req 2/3): the
// wire envelopes, AGENTOS_MESSAGING mode resolution, the memory/nats/hybrid
// dispatch behavior (including the hybrid never-lose-a-run fallback), the
// consumer-side executor wiring, and the workflow node.completed trigger
// seam. Broker flows run against the in-memory simulator with the injected
// FakeClock; real-broker verification lives in jetstream_nats_test.go.

func TestRunEnvelopeRoundTrip(t *testing.T) {
	env := RunEnvelope{
		TaskID:         "agent.run-2026-01-01T00:00:00Z",
		TaskType:       "agent.run",
		RunID:          "run-1",
		OrganizationID: "org-1",
		TraceID:        "trace-1",
		Payload:        map[string]any{"agent_id": "agent-1", "input": "hi"},
		CreatedAt:      time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := decodeRunEnvelope(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.TaskID != env.TaskID || got.TaskType != env.TaskType || got.RunID != env.RunID ||
		got.OrganizationID != env.OrganizationID || got.TraceID != env.TraceID || !got.CreatedAt.Equal(env.CreatedAt) {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if got.Payload["agent_id"] != "agent-1" {
		t.Fatalf("payload lost: %+v", got.Payload)
	}
	if _, err := decodeRunEnvelope([]byte("not json")); err == nil {
		t.Fatal("garbage envelope accepted")
	}
	if _, err := decodeRunEnvelope([]byte(`{"run_id":"r"}`)); err == nil {
		t.Fatal("envelope without task_type accepted")
	}
}

func TestNodeCompletedEnvelopeRoundTrip(t *testing.T) {
	env := NodeCompletedEnvelope{
		OrganizationID: "org-1",
		WorkflowRunID:  "wfr-1",
		NodeID:         "node-a",
		RunID:          "run-1",
		Status:         "completed",
		Output:         "ok",
		CompletedAt:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := decodeNodeCompletedEnvelope(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.NodeID != "node-a" || got.WorkflowRunID != "wfr-1" || got.Status != "completed" {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if _, err := decodeNodeCompletedEnvelope([]byte(`{"workflow_run_id":"w"}`)); err == nil {
		t.Fatal("envelope without node_id accepted")
	}
}

func TestMessagingModeFromEnv(t *testing.T) {
	t.Setenv(EnvMessaging, "")
	t.Setenv(EnvMessagingNATSURL, "")
	mode, url, err := MessagingModeFromEnv()
	if err != nil || mode != MessagingMemory || url != "" {
		t.Fatalf("default = (%v, %q, %v), want memory + empty url", mode, url, err)
	}

	t.Setenv(EnvMessaging, "  Hybrid ")
	t.Setenv(EnvMessagingNATSURL, "nats://broker:4222")
	mode, url, err = MessagingModeFromEnv()
	if err != nil || mode != MessagingHybrid || url != "nats://broker:4222" {
		t.Fatalf("hybrid = (%v, %q, %v)", mode, url, err)
	}

	t.Setenv(EnvMessaging, "kafka")
	if _, _, err := MessagingModeFromEnv(); err == nil || !strings.Contains(err.Error(), EnvMessaging) {
		t.Fatalf("invalid mode error = %v, want a %s validation error", err, EnvMessaging)
	}
}

func TestDispatcherMemoryModeDispatchesThroughQueue(t *testing.T) {
	q := NewQueue()
	d, err := NewDispatcherFromEnv(q) // env unset in tests -> memory
	if err != nil {
		t.Fatalf("NewDispatcherFromEnv: %v", err)
	}
	defer d.Close()
	if d.Mode() != MessagingMemory || d.EffectiveMode() != MessagingMemory || d.Consumer() != nil {
		t.Fatalf("memory dispatcher = mode %v effective %v consumer %v", d.Mode(), d.EffectiveMode(), d.Consumer())
	}
	task, err := d.Dispatch("agent.run", map[string]any{"organization_id": "org-1", "run_id": "run-1", "input": "hi"})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if task == nil || task.Type != "agent.run" || task.OrganizationID != "org-1" {
		t.Fatalf("dispatched task = %+v", task)
	}
	if q.Length() != 1 || q.Dequeue().ID != task.ID {
		t.Fatal("memory dispatch did not land in the queue")
	}
	// Admin surfaces honestly refuse in memory mode (the queue Inspector owns
	// task introspection there).
	if _, err := d.InspectDLQ(10); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("InspectDLQ in memory mode = %v, want ErrNotSupported", err)
	}
	if _, err := d.RequeueDLQ(1); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("RequeueDLQ in memory mode = %v, want ErrNotSupported", err)
	}
	if _, err := d.ReplayFrom(SubjectRunsCreate, time.Time{}, func(uint64, time.Time, []byte) error { return nil }); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("ReplayFrom in memory mode = %v, want ErrNotSupported", err)
	}
	if err := d.PublishNodeCompleted(NodeCompletedEnvelope{WorkflowRunID: "w", NodeID: "n", Status: "completed"}); err != nil {
		t.Fatalf("PublishNodeCompleted in memory mode = %v, want a documented no-op nil", err)
	}
}

func TestNewDispatcherValidation(t *testing.T) {
	if _, err := NewDispatcher(MessagingNATS, nil, NewQueue()); err == nil {
		t.Fatal("nats dispatcher without a seam accepted")
	}
	if _, err := NewDispatcher(MessagingMemory, NewSimJetStream(nil), NewQueue()); err == nil {
		t.Fatal("memory dispatcher with a seam accepted")
	}
	if _, err := NewDispatcher(MessagingMemory, nil, nil); err == nil {
		t.Fatal("memory dispatcher without a queue accepted")
	}
	if _, err := NewDispatcher("kafka", nil, NewQueue()); err == nil {
		t.Fatal("unknown mode accepted")
	}
}

func TestDispatcherNATSModeEndToEndWithSimulator(t *testing.T) {
	sim, _ := newTestSim(t)
	q := NewQueue()
	d, err := NewDispatcher(MessagingNATS, sim, q, WithDispatcherClock(NewFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))))
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	defer d.Close()
	if d.EffectiveMode() != MessagingNATS || d.Consumer() == nil {
		t.Fatalf("nats dispatcher = effective %v consumer %v", d.EffectiveMode(), d.Consumer())
	}

	var mu sync.Mutex
	var executed []*Task
	d.OnRun(func(_ context.Context, task *Task) error {
		mu.Lock()
		executed = append(executed, task)
		mu.Unlock()
		return nil
	})
	var triggers []*NodeCompletedEnvelope
	d.OnNodeCompleted(func(_ context.Context, env *NodeCompletedEnvelope) error {
		mu.Lock()
		triggers = append(triggers, env)
		mu.Unlock()
		return nil
	})
	c := d.Consumer()
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = c.Stop(time.Second) }()

	task, err := d.Dispatch("agent.run", map[string]any{
		"organization_id": "org-9", "run_id": "run-9", "agent_id": "agent-9", "input": "hello",
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	waitFor(t, "run task executed", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(executed) == 1
	})
	mu.Lock()
	exec := executed[0]
	mu.Unlock()
	if exec.ID != task.ID || exec.Type != "agent.run" || exec.OrganizationID != "org-9" {
		t.Fatalf("executed task = %+v", exec)
	}
	if exec.Payload["agent_id"] != "agent-9" || exec.Payload["run_id"] != "run-9" || exec.Payload["task_id"] != task.ID {
		t.Fatalf("envelope projection payload = %+v", exec.Payload)
	}
	// The memory queue stays untouched in nats mode.
	if q.Length() != 0 {
		t.Fatalf("memory queue length = %d, want 0", q.Length())
	}

	if err := d.PublishNodeCompleted(NodeCompletedEnvelope{
		OrganizationID: "org-9", WorkflowRunID: "wfr-1", NodeID: "node-a", Status: "completed",
	}); err != nil {
		t.Fatalf("PublishNodeCompleted: %v", err)
	}
	waitFor(t, "node-completed trigger", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(triggers) == 1
	})
	mu.Lock()
	trig := triggers[0]
	mu.Unlock()
	if trig.WorkflowRunID != "wfr-1" || trig.NodeID != "node-a" {
		t.Fatalf("trigger envelope = %+v", trig)
	}
}

func TestDispatcherWithoutExecutorAcksAndNeverDeadLetters(t *testing.T) {
	sim, _ := newTestSim(t)
	d, err := NewDispatcher(MessagingNATS, sim, NewQueue())
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	defer d.Close()
	c := d.Consumer()
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = c.Stop(time.Second) }()
	if _, err := sim.Publish(SubjectRunsCreate, []byte(`{"task_type":"agent.run","task_id":"t"}`)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if _, err := sim.Publish(SubjectWorkflowNodeCompleted, []byte(`{"workflow_run_id":"w","node_id":"n","status":"completed"}`)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	waitFor(t, "messages consumed", func() bool { return c.Stats().Processed == 2 })
	waitFor(t, "no dead letters", func() bool { return sim.RetainedCount(SubjectDLQ) == 0 })
}

func TestDispatcherMalformedEnvelopesDeadLetter(t *testing.T) {
	// RealClock sim: the retry cap (default 4) is exhausted through real-time
	// 1ms nak redeliveries — no injected clock to advance here.
	sim := NewSimJetStream(nil)
	t.Cleanup(func() { _ = sim.Close() })
	d, err := NewDispatcher(MessagingNATS, sim, NewQueue(), WithDispatcherNakDelay(time.Millisecond))
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	defer d.Close()
	c := d.Consumer()
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = c.Stop(time.Second) }()
	if _, err := sim.Publish(SubjectRunsCreate, []byte(`{`)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	waitFor(t, "malformed envelope dead-lettered", func() bool { return sim.RetainedCount(SubjectDLQ) == 1 })
	if c.Stats().DeadLettered != 1 {
		t.Fatalf("dead-letter counter = %d, want 1", c.Stats().DeadLettered)
	}
}

func TestDispatcherReplayAndDLQAdmin(t *testing.T) {
	sim, clock := newTestSim(t)
	q := NewQueue()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	d, err := NewDispatcher(MessagingNATS, sim, q, WithDispatcherClock(clock))
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	defer d.Close()

	// Replay (no consumer involved — the retained log only). Publish at two
	// injected instants, then replay the later window.
	if _, err := d.Dispatch("agent.run", map[string]any{"run_id": "r1"}); err != nil {
		t.Fatalf("dispatch 1: %v", err)
	}
	clock.Advance(time.Second)
	if _, err := d.Dispatch("agent.run", map[string]any{"run_id": "r2"}); err != nil {
		t.Fatalf("dispatch 2: %v", err)
	}
	n, err := d.ReplayFrom(SubjectRunsCreate, base.Add(time.Second), func(seq uint64, _ time.Time, data []byte) error {
		env, derr := decodeRunEnvelope(data)
		if derr != nil || env.Payload["run_id"] != "r2" {
			t.Errorf("replayed entry %d = %v %v", seq, env, derr)
		}
		return nil
	})
	if err != nil || n != 1 {
		t.Fatalf("ReplayFrom = (%d, %v), want (1, nil)", n, err)
	}
	if _, err := d.ReplayFrom(SubjectDLQ, base, func(uint64, time.Time, []byte) error { return nil }); err == nil {
		t.Fatal("DLQ replay accepted")
	}

	// DLQ inspect + requeue through the framework consumer wired to the
	// dispatcher's own handler (exactly the worker topology, one subject).
	// The executor only fails the dead-1 task: the two replay-dispatched
	// envelopes are consumed + acked by the same consumer without polluting
	// the DLQ count.
	var mu sync.Mutex
	fail := true
	d.OnRun(func(_ context.Context, task *Task) error {
		mu.Lock()
		defer mu.Unlock()
		if fail && task.ID == "dead-1" {
			return errors.New("nope")
		}
		return nil
	})
	consumer := NewConsumer(sim, "admin-dlq", WithClock(clock), WithMaxDeliver(1), WithNakDelay(time.Millisecond))
	if err := consumer.Subscribe(SubjectRunsCreate, d.handleRunCreate); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := consumer.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = consumer.Stop(time.Second) }()

	if _, err := sim.Publish(SubjectRunsCreate, []byte(`{"task_type":"agent.run","task_id":"dead-1","run_id":"run-dead"}`)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	waitFor(t, "dead letter", func() bool { return sim.RetainedCount(SubjectDLQ) == 1 })
	entries, err := d.InspectDLQ(10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("InspectDLQ = %v, %d", err, len(entries))
	}
	if entries[0].RunID != "run-dead" || entries[0].TaskID != "dead-1" {
		t.Fatalf("DLQ entry = %+v", entries[0])
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	if _, err := d.RequeueDLQ(entries[0].Sequence); err != nil {
		t.Fatalf("RequeueDLQ: %v", err)
	}
	waitFor(t, "requeued message processed", func() bool {
		return consumer.Stats().Processed == 3 // r1 + r2 replayed + the requeued dead-1
	})
	waitFor(t, "dlq drained", func() bool { return sim.RetainedCount(SubjectDLQ) == 0 })
}

// flakyPublishJS fails Publish calls after the first success (hybrid
// publish-time fallback test).
type flakyPublishJS struct {
	JetStream
	mu       sync.Mutex
	okFirst  int
	failRest bool
}

func (f *flakyPublishJS) Publish(subject string, data []byte) (uint64, error) {
	f.mu.Lock()
	if f.okFirst > 0 {
		f.okFirst--
		f.mu.Unlock()
		return f.JetStream.Publish(subject, data)
	}
	if f.failRest {
		f.mu.Unlock()
		return 0, errors.New("simulated broker outage")
	}
	f.mu.Unlock()
	return f.JetStream.Publish(subject, data)
}

func TestDispatcherHybridPublishFailureFallsBackToMemory(t *testing.T) {
	sim, _ := newTestSim(t)
	q := NewQueue()
	counter := newTestCounter()
	flaky := &flakyPublishJS{JetStream: sim, okFirst: 1, failRest: true}
	d, err := NewDispatcher(MessagingHybrid, flaky, q, WithDispatcherMetrics(counter))
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	defer d.Close()
	if d.EffectiveMode() != MessagingHybrid || d.Degraded() {
		t.Fatalf("hybrid state = effective %v degraded %v", d.EffectiveMode(), d.Degraded())
	}

	// First dispatch rides the broker.
	if _, err := d.Dispatch("agent.run", map[string]any{"run_id": "r1"}); err != nil {
		t.Fatalf("first dispatch: %v", err)
	}
	// Second dispatch hits the outage: the run MUST land in the memory queue.
	task, err := d.Dispatch("agent.run", map[string]any{"organization_id": "org-1", "run_id": "r2"})
	if err != nil {
		t.Fatalf("fallback dispatch returned an error (a run was lost): %v", err)
	}
	if task == nil || q.Length() != 1 || q.Peek().ID != task.ID {
		t.Fatalf("hybrid fallback did not enqueue to memory: task=%v q=%d", task, q.Length())
	}
	if got := counter.get(MetricMessagingPublishFailed); got != 1 {
		t.Fatalf("publish-failed metric = %d, want 1", got)
	}
	if got := counter.get(MetricMessagingFallback); got != 1 {
		t.Fatalf("fallback metric = %d, want 1", got)
	}
}

func TestDispatcherNATSModeSurfacesPublishErrors(t *testing.T) {
	flaky := &flakyPublishJS{JetStream: NewSimJetStream(nil), failRest: true}
	d, err := NewDispatcher(MessagingNATS, flaky, NewQueue())
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	defer d.Close()
	if _, err := d.Dispatch("agent.run", nil); err == nil {
		t.Fatal("nats mode swallowed a publish error (callers must see broker outages)")
	}
}

func TestDispatcherHybridStartupFallbackToMemory(t *testing.T) {
	// An unreachable broker address: hybrid degrades to memory, nats fails fast.
	t.Setenv(EnvMessaging, string(MessagingHybrid))
	t.Setenv(EnvMessagingNATSURL, "nats://127.0.0.1:1")
	q := NewQueue()
	counter := newTestCounter()
	d, err := NewDispatcherFromEnv(q, WithDispatcherMetrics(counter))
	if err != nil {
		t.Fatalf("hybrid startup fallback errored: %v", err)
	}
	defer d.Close()
	if !d.Degraded() || d.EffectiveMode() != MessagingMemory || d.Mode() != MessagingHybrid {
		t.Fatalf("hybrid fallback state = degraded %v effective %v mode %v", d.Degraded(), d.EffectiveMode(), d.Mode())
	}
	if d.Consumer() != nil {
		t.Fatal("degraded hybrid still carries a consumer")
	}
	if got := counter.get(MetricMessagingFallback); got != 1 {
		t.Fatalf("fallback metric = %d, want 1", got)
	}
	task, err := d.Dispatch("agent.run", map[string]any{"run_id": "r"})
	if err != nil || q.Length() != 1 || task.ID != q.Peek().ID {
		t.Fatalf("degraded dispatch = (%v, %v) q=%d", task, err, q.Length())
	}

	t.Setenv(EnvMessaging, string(MessagingNATS))
	t.Setenv(EnvMessagingNATSURL, "nats://127.0.0.1:1")
	if _, err := NewDispatcherFromEnv(q); !errors.Is(err, ErrBrokerUnavailable) {
		t.Fatalf("nats startup = %v, want ErrBrokerUnavailable", err)
	}
}

func TestDispatcherDispatchValidation(t *testing.T) {
	d, err := NewDispatcher(MessagingMemory, nil, NewQueue())
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	if _, err := d.Dispatch("", nil); err == nil {
		t.Fatal("empty task type accepted")
	}
}
