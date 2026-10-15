package queue

// dispatch.go is the task-dispatch + coordination layer over the consumer
// framework (issue #116 req 2/3). It closes the loop between the producers
// (run creation, workflow node completion) and the Consumer (consumer.go):
//
//      AGENTOS_MESSAGING=memory (default) -> tasks flow through the platform
//      *Queue exactly as before (zero-infra dev unchanged; the Dispatcher is a
//      no-op wrapper around Queue.Enqueue).
//
//      AGENTOS_MESSAGING=nats -> run creation Dispatch()es a RunEnvelope onto
//      agentos.run.create through the JetStream seam; workers consume it with
//      the Consumer framework and execute via the existing task path.
//      Constructor fails fast when the broker is unreachable (an explicit nats
//      request that silently degraded would split the task flow, same policy as
//      queue.NewFromConfig for redis).
//
//      AGENTOS_MESSAGING=hybrid -> same as nats, but a broker failure (at
//      startup or at publish time) falls back to the memory queue with a log
//      line and a metric: a run is never lost to a broker outage. The memory
//      queue stays drained by the worker's existing loop, so fallback tasks are
//      picked up unchanged.
//
// Workflow coordination (req 3): the worker publishes one
// NodeCompletedEnvelope onto agentos.workflow.node.completed after each
// finished node (fan-out: any number of listeners). The trigger action is a
// caller-supplied callback (OnNodeCompleted) — the framework deliberately
// knows nothing about DSL scheduling (this package cannot import
// internal/workflows: import cycle, and "what runs next" must keep a single
// point of truth in the workflow service). cmd/worker wires the trigger to
// the workflow service's existing recovery/re-enqueue seam; see
// cmd/worker/messaging.go for the documented choice.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
)

// Environment variables consumed by MessagingModeFromEnv. EnvMessagingNATSURL
// is intentionally the same variable the domain-event publisher
// (internal/events EnvNATSURL) uses: one broker endpoint for the process.
const (
	EnvMessaging        = "AGENTOS_MESSAGING"
	EnvMessagingNATSURL = "AGENTOS_NATS_URL"
)

// MessagingMode selects the task-dispatch plane (AGENTOS_MESSAGING).
type MessagingMode string

const (
	// MessagingMemory is the default: the in-process/redis *Queue keeps
	// serving task dispatch; no broker connection is made.
	MessagingMemory MessagingMode = "memory"
	// MessagingNATS dispatches tasks as JetStream envelopes; broker-down is
	// an error (fail fast at startup, publish errors surface to callers).
	MessagingNATS MessagingMode = "nats"
	// MessagingHybrid is nats with a memory safety net: broker failures fall
	// back to the memory queue so runs are never lost.
	MessagingHybrid MessagingMode = "hybrid"
)

// MessagingModeFromEnv resolves AGENTOS_MESSAGING (default memory) and
// AGENTOS_NATS_URL. An unknown mode is an error — fail fast at startup beats
// silently running a different topology than the operator asked for (the
// queue.NewFromConfig convention).
func MessagingModeFromEnv() (MessagingMode, string, error) {
	mode := MessagingMode(strings.ToLower(strings.TrimSpace(os.Getenv(EnvMessaging))))
	switch mode {
	case "", MessagingMemory:
		return MessagingMemory, os.Getenv(EnvMessagingNATSURL), nil
	case MessagingNATS, MessagingHybrid:
		return mode, os.Getenv(EnvMessagingNATSURL), nil
	default:
		return "", "", fmt.Errorf("queue: invalid %s mode %q (supported: %q, %q, %q)",
			EnvMessaging, mode, MessagingMemory, MessagingNATS, MessagingHybrid)
	}
}

// ---------------------------------------------------------------------------
// Wire envelopes
// ---------------------------------------------------------------------------

// RunEnvelope is the wire shape published to SubjectRunsCreate when a run
// task is dispatched through the messaging plane (issue #116 req 2). The
// top-level identity fields (run_id, organization_id, trace) make routing,
// DLQ inspection and correlation possible without decoding the payload; the
// payload map carries the exact keys the existing execution path consumes
// (agent_id, input, workflow_run_id, node_id, environment, ...).
type RunEnvelope struct {
	// TaskID is the logical task identity (same format as Queue.Enqueue:
	// "<task_type>-<rfc3339nano>"); it rides the DLQ envelope for requeue
	// correlation.
	TaskID string `json:"task_id"`
	// TaskType is the queue task type ("agent.run", "tool.run", ...).
	TaskType string `json:"task_type"`
	// RunID is the agent/tool run the task executes.
	RunID string `json:"run_id"`
	// OrganizationID is the tenant scope (mirrors Task.OrganizationID).
	OrganizationID string `json:"organization_id"`
	// TraceID is the distributed-trace correlation id when the caller has
	// one (best effort; empty otherwise).
	TraceID string `json:"trace_id,omitempty"`
	// Payload is the original enqueue payload, republished verbatim.
	Payload map[string]any `json:"payload,omitempty"`
	// CreatedAt is the dispatch instant (broker messages also carry the
	// broker timestamp; this one travels with the envelope).
	CreatedAt time.Time `json:"created_at"`
}

// decodeRunEnvelope parses one retained/delivered agentos.runs.create
// message. It is shared by the delivery pipeline (DLQ identity extraction in
// consumer.go) and the Dispatcher's own consumer handler.
func decodeRunEnvelope(data []byte) (RunEnvelope, error) {
	var env RunEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return RunEnvelope{}, fmt.Errorf("queue: run envelope: %v", err)
	}
	if env.TaskType == "" {
		return RunEnvelope{}, fmt.Errorf("queue: run envelope: task_type is required")
	}
	return env, nil
}

// newRunEnvelope builds the envelope for one dispatched task. TaskID uses the
// Queue.Enqueue format so callers can correlate across both dispatch planes.
func newRunEnvelope(taskType string, payload map[string]any, now time.Time) RunEnvelope {
	return RunEnvelope{
		TaskID:         taskType + "-" + now.Format(time.RFC3339Nano),
		TaskType:       taskType,
		RunID:          stringField(payload, "run_id"),
		OrganizationID: stringField(payload, "organization_id"),
		TraceID:        stringField(payload, "trace_id"),
		Payload:        payload,
		CreatedAt:      now,
	}
}

// stringField extracts a string key from a payload map ("" when absent or
// non-string) — the same convention organizationFromPayload uses.
func stringField(payload map[string]any, key string) string {
	if payload == nil {
		return ""
	}
	v, _ := payload[key].(string)
	return v
}

// NodeCompletedEnvelope is the wire shape published to
// SubjectWorkflowNodeCompleted after one workflow node reaches a terminal
// state (issue #116 req 3, fan-out). Consumers decide what "completed"
// triggers; the framework only guarantees at-least-once delivery of the fact.
type NodeCompletedEnvelope struct {
	OrganizationID string    `json:"organization_id"`
	WorkflowRunID  string    `json:"workflow_run_id"`
	NodeID         string    `json:"node_id"`
	RunID          string    `json:"run_id,omitempty"`
	Status         string    `json:"status"`
	Output         string    `json:"output,omitempty"`
	Error          string    `json:"error,omitempty"`
	CompletedAt    time.Time `json:"completed_at"`
}

// decodeNodeCompletedEnvelope parses one fan-out notification.
func decodeNodeCompletedEnvelope(data []byte) (NodeCompletedEnvelope, error) {
	var env NodeCompletedEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return NodeCompletedEnvelope{}, fmt.Errorf("queue: node-completed envelope: %v", err)
	}
	if env.WorkflowRunID == "" || env.NodeID == "" {
		return NodeCompletedEnvelope{}, fmt.Errorf("queue: node-completed envelope: workflow_run_id and node_id are required")
	}
	return env, nil
}

// ---------------------------------------------------------------------------
// Dispatcher
// ---------------------------------------------------------------------------

// Dispatcher is the mode-aware task-dispatch + coordination surface. Producers
// call Dispatch (runs) / PublishNodeCompleted (workflow fan-out); the worker
// registers callbacks via OnRun / OnNodeCompleted and runs the Consumer
// returned by Consumer(). Construct with NewDispatcherFromEnv (binaries) or
// NewDispatcher (wiring that owns the JetStream seam, e.g. tests with the
// simulator).
type Dispatcher struct {
	mode   MessagingMode
	js     JetStream // nil => memory-effective (no broker)
	memory *Queue    // always non-nil: memory target + hybrid fallback target

	runExecutor func(context.Context, *Task) error
	nodeTrigger func(context.Context, *NodeCompletedEnvelope) error

	consumerOptions []ConsumerOption // forwarded to the Consumer at construction
	consumer        *Consumer        // non-nil iff js != nil

	log     *slog.Logger
	metrics Counter
	clock   Clock

	// fallback records that a hybrid startup degraded to memory (EffectiveMode
	// reports it; operators alert on MetricMessagingFallback).
	fallback bool
}

// DispatcherOption configures a Dispatcher.
type DispatcherOption func(*Dispatcher)

// WithDispatcherLogger injects the logger (default slog.Default()).
func WithDispatcherLogger(l *slog.Logger) DispatcherOption {
	return func(d *Dispatcher) {
		if l != nil {
			d.log = l
		}
	}
}

// WithDispatcherMetrics attaches the metrics sink (the Consumer's counter is
// wired to the same sink).
func WithDispatcherMetrics(m Counter) DispatcherOption {
	return func(d *Dispatcher) {
		if m != nil {
			d.metrics = m
		}
	}
}

// WithDispatcherClock injects the Clock (tests use NewFakeClock; the same
// clock flows into the Consumer and the simulator).
func WithDispatcherClock(c Clock) DispatcherOption {
	return func(d *Dispatcher) {
		if c != nil {
			d.clock = c
		}
	}
}

// WithDispatcherConcurrency sets the Consumer's in-flight cap (see
// WithConcurrency).
func WithDispatcherConcurrency(n int) DispatcherOption {
	return func(d *Dispatcher) {
		if n >= 1 {
			d.consumerOptions = append(d.consumerOptions, WithConcurrency(n))
		}
	}
}

// WithDispatcherNakDelay sets the Consumer's redelivery delay (see
// WithNakDelay).
func WithDispatcherNakDelay(d time.Duration) DispatcherOption {
	return func(dd *Dispatcher) {
		if d > 0 {
			dd.consumerOptions = append(dd.consumerOptions, WithNakDelay(d))
		}
	}
}

// NewDispatcher builds a Dispatcher in the given mode. js must be non-nil for
// nats/hybrid (pass the simulator in tests; the real adapter in production
// wiring) and nil for memory. q is the memory target: required for
// memory/hybrid, optional (but recommended — the worker drains it) for nats.
// Callbacks are registered with OnRun / OnNodeCompleted BEFORE the Consumer
// is started.
func NewDispatcher(mode MessagingMode, js JetStream, q *Queue, opts ...DispatcherOption) (*Dispatcher, error) {
	d := &Dispatcher{
		mode:   mode,
		js:     js,
		memory: q,
		log:    slog.Default(),
		clock:  RealClock(),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(d)
		}
	}
	switch mode {
	case MessagingMemory:
		if q == nil {
			return nil, fmt.Errorf("queue: memory dispatcher requires a queue")
		}
		if js != nil {
			return nil, fmt.Errorf("queue: memory dispatcher must not carry a broker seam")
		}
	case MessagingNATS:
		if js == nil {
			return nil, fmt.Errorf("queue: nats dispatcher requires a JetStream seam")
		}
	case MessagingHybrid:
		// js == nil is the degraded startup state (broker unreachable at
		// construction): the dispatcher runs memory-effective with the
		// fallback flag set. The memory queue is mandatory either way — it is
		// the safety net that keeps runs flowing when the broker is down.
		if q == nil {
			return nil, fmt.Errorf("queue: hybrid dispatcher requires a memory queue as its fallback target")
		}
	default:
		return nil, fmt.Errorf("queue: invalid messaging mode %q", mode)
	}
	if d.js != nil {
		copts := append([]ConsumerOption(nil), d.consumerOptions...)
		copts = append(copts,
			WithClock(d.clock),
			WithLogger(d.log),
			WithCounter(d.metrics),
		)
		c := NewConsumer(d.js, "agentos-dispatcher", copts...)
		if err := c.Subscribe(SubjectRunsCreate, d.handleRunCreate); err != nil {
			return nil, err
		}
		if err := c.Subscribe(SubjectWorkflowNodeCompleted, d.handleNodeCompleted); err != nil {
			return nil, err
		}
		d.consumer = c
	}
	return d, nil
}

// NewDispatcherFromEnv resolves AGENTOS_MESSAGING/AGENTOS_NATS_URL and builds
// the Dispatcher:
//
//   - memory: no broker connection, Dispatch delegates to q.Enqueue.
//   - nats: connects (fail fast on broker-down — a silently degraded nats
//     deployment would split the task flow between processes).
//   - hybrid: connects; broker-down at startup degrades to memory with a log
//     line + MetricMessagingFallback and EffectiveMode()=="memory" — runs keep
//     flowing through the queue the worker already drains.
func NewDispatcherFromEnv(q *Queue, opts ...DispatcherOption) (*Dispatcher, error) {
	mode, url, err := MessagingModeFromEnv()
	if err != nil {
		return nil, err
	}
	switch mode {
	case MessagingMemory:
		return NewDispatcher(MessagingMemory, nil, q, opts...)
	case MessagingNATS:
		js, jerr := NewNATSJetStream(url)
		if jerr != nil {
			return nil, jerr
		}
		return NewDispatcher(MessagingNATS, js, q, opts...)
	default: // hybrid
		js, jerr := NewNATSJetStream(url)
		if jerr != nil {
			d, berr := NewDispatcher(MessagingHybrid, nil, q, opts...)
			if berr != nil {
				return nil, berr
			}
			d.fallback = true
			d.incr(MetricMessagingFallback)
			d.log.Warn("queue: hybrid messaging degraded to memory (broker unreachable); runs keep flowing through the memory queue",
				"mode", string(MessagingHybrid), "error", jerr.Error())
			return d, nil
		}
		return NewDispatcher(MessagingHybrid, js, q, opts...)
	}
}

// Mode reports the requested messaging mode.
func (d *Dispatcher) Mode() MessagingMode { return d.mode }

// EffectiveMode reports the mode actually in effect: hybrid degrades to
// memory when the broker is unreachable at startup (the fallback flag stays
// set so operators can distinguish the degradation).
func (d *Dispatcher) EffectiveMode() MessagingMode {
	if d.js == nil {
		return MessagingMemory
	}
	return d.mode
}

// Degraded reports whether a hybrid startup fell back to memory.
func (d *Dispatcher) Degraded() bool { return d.fallback }

// Consumer returns the framework Consumer consuming the messaging-plane
// subjects, or nil in memory-effective mode (nothing to start).
func (d *Dispatcher) Consumer() *Consumer { return d.consumer }

// OnRun registers the executor the consumer hands decoded run tasks to
// (the worker passes its existing task path). Without an executor the
// consumer still consumes + acks run-dispatch messages (an API-only process
// subscribes to keep cursors moving) — logged, never dead-lettered.
func (d *Dispatcher) OnRun(executor func(context.Context, *Task) error) {
	d.runExecutor = executor
}

// OnNodeCompleted registers the workflow-coordination trigger (req 3). It is
// invoked for every decoded agentos.workflow.node.completed notification; a
// returned error naks the notification (retry-cap -> DLQ, so a broken trigger
// is observable instead of silently dropping facts).
func (d *Dispatcher) OnNodeCompleted(trigger func(context.Context, *NodeCompletedEnvelope) error) {
	d.nodeTrigger = trigger
}

// Dispatch sends one run task through the active plane and mirrors the
// Queue.Enqueue contract:
//
//   - memory-effective: q.Enqueue (the historical path, unchanged).
//   - nats/hybrid: publish a RunEnvelope to agentos.runs.create. The returned
//     *Task is the envelope's detached projection (Status queued, the
//     envelope TaskID) — in this plane the broker log, not a process-local
//     slice, is the work list; introspection for messaging-dispatched tasks
//     rides the DLQ/Log admin surface and the run timeline.
//   - hybrid publish/marshal failure: the run falls back to the memory queue
//     (log + MetricMessagingFallback) and is returned like a memory enqueue —
//     a broker outage never loses a run.
//
// A nil error with a nil task cannot happen in memory mode; in nats mode a
// publish failure returns (nil, err) for the caller to surface.
func (d *Dispatcher) Dispatch(taskType string, payload map[string]any) (*Task, error) {
	if taskType == "" {
		return nil, fmt.Errorf("queue: task type is required")
	}
	if d.js == nil {
		if d.memory == nil {
			return nil, fmt.Errorf("queue: no dispatch target (memory queue is nil)")
		}
		return d.memory.Enqueue(taskType, payload), nil
	}
	env := newRunEnvelope(taskType, payload, d.clock.Now())
	b, err := json.Marshal(env)
	if err != nil {
		return d.dispatchFailed(taskType, payload, fmt.Errorf("marshal run envelope: %v", err))
	}
	if _, perr := d.js.Publish(SubjectRunsCreate, b); perr != nil {
		return d.dispatchFailed(taskType, payload, perr)
	}
	d.incr(MetricMessagingPublished)
	now := d.clock.Now()
	return &Task{
		ID:             env.TaskID,
		Type:           env.TaskType,
		Payload:        payload,
		Status:         StatusQueued,
		CreatedAt:      now,
		UpdatedAt:      now,
		OrganizationID: env.OrganizationID,
	}, nil
}

// dispatchFailed applies the hybrid fallback policy to one failed publish:
// hybrid never loses the run (memory queue + metrics + log); nats surfaces
// the error.
func (d *Dispatcher) dispatchFailed(taskType string, payload map[string]any, cause error) (*Task, error) {
	d.incr(MetricMessagingPublishFailed)
	if d.mode != MessagingHybrid || d.memory == nil {
		return nil, fmt.Errorf("queue: dispatch %s: %v", taskType, cause)
	}
	d.incr(MetricMessagingFallback)
	d.log.Warn("queue: run-dispatch publish failed; hybrid fallback to memory queue (run preserved)",
		"task_type", taskType, "error", cause.Error())
	return d.memory.Enqueue(taskType, payload), nil
}

// PublishNodeCompleted fans out one workflow-node-completed notification.
// Memory-effective mode is a documented no-op (single-process deployments
// coordinate through the DAG executor directly; there is no fan-out plane to
// notify). Hybrid publish failures are logged and returned — notifications
// are derived facts (the recovery pass is the durability net), so unlike run
// dispatch they are NOT mirrored into the memory queue.
func (d *Dispatcher) PublishNodeCompleted(env NodeCompletedEnvelope) error {
	if d.js == nil {
		return nil
	}
	b, err := json.Marshal(env)
	if err != nil {
		err = fmt.Errorf("queue: marshal node-completed envelope: %v", err)
		d.log.Warn("queue: node-completed fan-out failed", "error", err.Error())
		return err
	}
	if _, perr := d.js.Publish(SubjectWorkflowNodeCompleted, b); perr != nil {
		d.incr(MetricMessagingPublishFailed)
		d.log.Warn("queue: node-completed fan-out failed", "workflow_run_id", env.WorkflowRunID,
			"node_id", env.NodeID, "error", perr.Error())
		return perr
	}
	d.incr(MetricMessagingPublished)
	return nil
}

// incr bumps the metrics sink when one is attached.
func (d *Dispatcher) incr(name string) {
	if d.metrics != nil {
		d.metrics.Inc(name)
	}
}

// handleRunCreate is the internal agentos.runs.create consumer: decode ->
// project the *Task the existing execution path consumes -> OnRun. Decode
// failures are dead-lettered by the framework (a malformed envelope is never
// retried into a crash loop); an absent executor logs and acks.
func (d *Dispatcher) handleRunCreate(ctx context.Context, msg Message) error {
	env, err := decodeRunEnvelope(msg.Data())
	if err != nil {
		return err
	}
	if d.runExecutor == nil {
		d.log.Info("queue: run-dispatch consumed without an executor (ack; API-only consumer)",
			"task_id", env.TaskID, "run_id", env.RunID)
		return nil
	}
	return d.runExecutor(ctx, env.asTask())
}

// asTask projects the envelope onto the *Task shape the existing worker path
// consumes. Envelope identity fields are authoritative; payload keys the
// execution path reads (run_id, organization_id, trace_id) are stamped from
// them so the two can never disagree.
func (e RunEnvelope) asTask() *Task {
	payload := make(map[string]any, len(e.Payload)+4)
	for k, v := range e.Payload {
		payload[k] = v
	}
	if e.RunID != "" {
		payload["run_id"] = e.RunID
	}
	if e.OrganizationID != "" {
		payload["organization_id"] = e.OrganizationID
	}
	if e.TraceID != "" {
		payload["trace_id"] = e.TraceID
	}
	payload["task_id"] = e.TaskID
	return &Task{
		ID:             e.TaskID,
		Type:           e.TaskType,
		Payload:        payload,
		Status:         StatusQueued,
		CreatedAt:      e.CreatedAt,
		UpdatedAt:      e.CreatedAt,
		OrganizationID: e.OrganizationID,
	}
}

// handleNodeCompleted is the internal agentos.workflow.node.completed
// consumer: decode -> OnNodeCompleted. Without a trigger the notification is
// logged + acked (fan-out consumers that do not coordinate are legitimate).
func (d *Dispatcher) handleNodeCompleted(ctx context.Context, msg Message) error {
	env, err := decodeNodeCompletedEnvelope(msg.Data())
	if err != nil {
		return err
	}
	if d.nodeTrigger == nil {
		d.log.Debug("queue: node-completed consumed without a trigger (ack)",
			"workflow_run_id", env.WorkflowRunID, "node_id", env.NodeID, "status", env.Status)
		return nil
	}
	return d.nodeTrigger(ctx, &env)
}

// ---------------------------------------------------------------------------
// Admin surface (issue #116 req 4): replay + DLQ operations on the
// Dispatcher, so an API/admin process can administer the plane without
// running a consumer. HTTP exposure rides the existing queue-ops seam
// (cmd/api/queue_ops.go) in the api wiring follow-up; the fragment
// api/fragments/nats-core.yaml documents the contract.
// ---------------------------------------------------------------------------

// InspectDLQ lists retained dead letters oldest-first (bounded by limit>0).
// Memory-effective mode has no retained log: ErrNotSupported.
func (d *Dispatcher) InspectDLQ(limit int) ([]DLQEntry, error) {
	if d.js == nil {
		return nil, fmt.Errorf("%w: memory mode has no messaging DLQ (task dead letters ride the queue Inspector)", ErrNotSupported)
	}
	return inspectDLQ(d.js, SubjectDLQ, limit)
}

// RequeueDLQ republishes the dead letter seq back onto its original subject
// and consumes the DLQ entry.
func (d *Dispatcher) RequeueDLQ(seq uint64) (*DLQEntry, error) {
	if d.js == nil {
		return nil, fmt.Errorf("%w: memory mode has no messaging DLQ (task requeue rides the queue Inspector)", ErrNotSupported)
	}
	return requeueDLQ(d.js, SubjectDLQ, seq)
}

// ReplayFrom re-invokes fn for every retained message of subject published at
// or after from (oldest-first). Only replayable subjects are accepted
// (IsReplayableSubject) — the admin surface can never inject traffic outside
// the messaging plane or loop the DLQ back into itself.
func (d *Dispatcher) ReplayFrom(subject string, from time.Time, fn func(seq uint64, ts time.Time, data []byte) error) (int, error) {
	if !IsReplayableSubject(subject) {
		return 0, fmt.Errorf("queue: subject %q is not replayable (replayable: %v)", subject, ReplayingSubjects())
	}
	if d.js == nil {
		return 0, fmt.Errorf("%w: memory mode has no retained log", ErrNotSupported)
	}
	return replayFrom(d.js, subject, from, fn)
}

// Close releases the broker connection (nats/hybrid). The memory queue's
// Close is owned by its constructor site (unchanged).
func (d *Dispatcher) Close() error {
	if d == nil || d.js == nil {
		return nil
	}
	return d.js.Close()
}
