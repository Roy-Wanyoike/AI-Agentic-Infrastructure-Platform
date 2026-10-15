package queue

// consumer.go is the NATS consumer framework (issue #116 req 1): subscription
// management, bounded-concurrency delivery, ack/nak with a retry cap that
// dead-letters to agentos.dlq, a reconnect supervision loop with exponential
// backoff, and the admin surfaces (replay-from-timestamp + DLQ
// inspect/requeue). It programs exclusively against the narrow JetStream seam
// (jetstream.go), so the identical framework runs against the embedded
// simulator (tests, memory mode) and the real broker (jetstream_nats.go).
//
// Delivery contract (mirrors the pull/ProcessNext engine in queue.go):
//
//      handler nil        -> Ack            (the task completed)
//      handler error      -> NakWithDelay   (redelivery; attempts tracked
//                                           server-side via Message.Deliveries)
//      error on attempt N == MaxDeliver -> publish a DLQEnvelope to the dead-
//      letter subject and Ack the original (terminal; same shape as the engine's
//      Attempts>=4 -> dead_letter rule)
//
// Panics in handlers are recovered and treated like errors (the broker
// goroutine must never die). Handlers receive a drain context: it stays alive
// while Stop(grace) waits for in-flight work and is cancelled only after the
// grace window elapses, so a draining handler is never aborted mid-run.
//
// Reconnect resilience: a supervision loop re-establishes any subscription
// whose Subscription.Valid() reports false, with per-subscription exponential
// backoff. Initial subscribe failures are also retried there — Start never
// fails because the broker is down (config/stream errors DO fail Start). For
// the real broker, durable consumers keep their server-side cursor across
// restarts, so a re-subscribe resumes where the previous attempt stopped.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"sync"
	"sync/atomic"
	"time"
)

// Metrics sink names surfaced through the optional Counter (structurally
// satisfied by *observability.Metrics without importing it).
const (
	MetricMessagingProcessed     = "messaging_processed_total"
	MetricMessagingRedelivered   = "messaging_redelivered_total"
	MetricMessagingDeadLettered  = "messaging_dead_lettered_total"
	MetricMessagingPublished     = "messaging_published_total"
	MetricMessagingFallback      = "messaging_fallback_total"
	MetricMessagingPublishFailed = "messaging_publish_failed_total"
)

// Counter is the minimal metrics sink the messaging layer bumps. It is an
// interface rather than a concrete type so callers can hand in the process
// registry (any type with Inc(string)) without this package depending on it.
type Counter interface {
	Inc(name string)
}

// Handler processes one delivered message. Returning nil acknowledges the
// message; a non-nil error schedules redelivery (see the package comment for
// the retry-cap contract).
type Handler func(ctx context.Context, msg Message) error

// DLQEnvelope is the wire shape published to SubjectDLQ when a message
// exhausts its retry cap.
type DLQEnvelope struct {
	// Subject is the original subject the message was published to; requeue
	// republishes the payload back onto it.
	Subject string `json:"subject"`
	// TaskID identifies the logical task when the dead-lettered message is a
	// run-dispatch envelope (best-effort extraction, empty otherwise).
	TaskID string `json:"task_id,omitempty"`
	// Error is the last handler error.
	Error string `json:"error"`
	// Attempts is the delivery count at dead-letter time.
	Attempts uint64 `json:"attempts"`
	// DeadLetteredAt is the terminal-failure instant.
	DeadLetteredAt time.Time `json:"dead_lettered_at"`
	// Payload is the original message bytes, base64-encoded in JSON (requeue
	// republishes them verbatim). Bytes (not json.RawMessage) on purpose: the
	// DLQ's most important customer is a MALFORMED payload, which cannot ride
	// as raw JSON — a RawMessage marshal failure would drop the dead letter
	// exactly when it matters. Base64 always marshals and round-trips any
	// bytes losslessly.
	Payload []byte `json:"payload_base64,omitempty"`
}

// DLQEntry is the inspected view of one retained dead letter (the admin
// surface; Payload carries the original envelope bytes so requeue can
// republish them verbatim).
type DLQEntry struct {
	Sequence       uint64    `json:"sequence"`
	Subject        string    `json:"subject"`
	TaskID         string    `json:"task_id,omitempty"`
	RunID          string    `json:"run_id,omitempty"`
	OrganizationID string    `json:"organization_id,omitempty"`
	Error          string    `json:"error"`
	Attempts       uint64    `json:"attempts"`
	DeadLetteredAt time.Time `json:"dead_lettered_at"`
	// Payload is the original message bytes, base64-encoded in JSON (see
	// DLQEnvelope.Payload for why it is not raw JSON).
	Payload []byte `json:"payload_base64,omitempty"`
}

// Consumer errors.
var (
	// ErrAlreadyStarted is returned by Subscribe after Start.
	ErrAlreadyStarted = errors.New("queue: consumer already started")
	// ErrStopped is returned by Start after Stop.
	ErrStopped = errors.New("queue: consumer is stopped")
	// ErrGraceExceeded is returned by Stop when in-flight handlers were
	// still running after the grace window (they are then cancelled).
	ErrGraceExceeded = errors.New("queue: consumer stop grace exceeded")
	// ErrNotSupported reports a seam the active broker implementation does
	// not provide (e.g. Log on a minimal implementation).
	ErrNotSupported = errors.New("queue: operation not supported by the broker implementation")
)

// Consumer consumes one or more subjects through the JetStream seam with
// bounded concurrency, retry-cap dead-lettering and reconnect resilience.
// Construct with NewConsumer, register subjects with Subscribe, run with
// Start, shut down with Stop.
type Consumer struct {
	name string
	js   JetStream

	concurrency int
	maxDeliver  uint64
	nakDelay    time.Duration
	dlqSubject  string

	superviseInterval time.Duration
	reconnectBase     time.Duration
	reconnectMax      time.Duration

	clock   Clock
	log     *slog.Logger
	metrics Counter

	mu      sync.Mutex
	specs   []*consumerSpec
	started bool
	stopped bool

	intakeCtx    context.Context
	intakeCancel context.CancelFunc
	drainCtx     context.Context
	drainCancel  context.CancelFunc
	wg           sync.WaitGroup
	sem          chan struct{}

	superviseDone chan struct{}

	processed    atomic.Int64
	redeliveries atomic.Int64
	deadLettered atomic.Int64
}

// consumerSpec is one registered subject subscription plus its reconnect
// backoff state.
type consumerSpec struct {
	subject string
	handler Handler
	sub     Subscription

	backoffMu   sync.Mutex
	backoff     time.Duration
	nextAttempt time.Time
}

// ConsumerOption configures a Consumer.
type ConsumerOption func(*Consumer)

// WithConcurrency caps the in-flight handlers (semaphore). Values < 1 fall
// back to defaultConsumerConcurrency.
func WithConcurrency(n int) ConsumerOption {
	return func(c *Consumer) {
		if n >= 1 {
			c.concurrency = n
		}
	}
}

// WithMaxDeliver sets the retry cap: the delivery whose count reaches the cap
// and fails is dead-lettered. Mirrors the engine's Attempts>=4 rule.
func WithMaxDeliver(n uint64) ConsumerOption {
	return func(c *Consumer) {
		if n >= 1 {
			c.maxDeliver = n
		}
	}
}

// WithNakDelay sets the redelivery delay used for retryable failures.
func WithNakDelay(d time.Duration) ConsumerOption {
	return func(c *Consumer) {
		if d > 0 {
			c.nakDelay = d
		}
	}
}

// WithDLQSubject overrides the dead-letter subject (default SubjectDLQ).
func WithDLQSubject(subject string) ConsumerOption {
	return func(c *Consumer) {
		if subject != "" {
			c.dlqSubject = subject
		}
	}
}

// WithClock injects the Clock (tests use NewFakeClock for determinism).
func WithClock(clock Clock) ConsumerOption {
	return func(c *Consumer) {
		if clock != nil {
			c.clock = clock
		}
	}
}

// WithLogger injects the logger (default slog.Default()).
func WithLogger(l *slog.Logger) ConsumerOption {
	return func(c *Consumer) {
		if l != nil {
			c.log = l
		}
	}
}

// WithCounter attaches the metrics sink.
func WithCounter(m Counter) ConsumerOption {
	return func(c *Consumer) {
		c.metrics = m
	}
}

// WithSupervision tunes the reconnect supervision: how often subscriptions
// are health-checked and the exponential backoff bounds for re-subscribing.
func WithSupervision(interval, backoffBase, backoffMax time.Duration) ConsumerOption {
	return func(c *Consumer) {
		if interval > 0 {
			c.superviseInterval = interval
		}
		if backoffBase > 0 {
			c.reconnectBase = backoffBase
		}
		if backoffMax > 0 {
			c.reconnectMax = backoffMax
		}
	}
}

// Framework defaults (documented on each option).
const (
	defaultConsumerConcurrency = 4
	defaultConsumerMaxDeliver  = 4 // same cap as the pull/ProcessNext engine
	defaultNakDelay            = time.Second
	defaultSuperviseInterval   = time.Second
	defaultReconnectBase       = 250 * time.Millisecond
	defaultReconnectMax        = 10 * time.Second
)

// NewConsumer returns a Consumer over js.
func NewConsumer(js JetStream, name string, opts ...ConsumerOption) *Consumer {
	c := &Consumer{
		name:              name,
		js:                js,
		concurrency:       defaultConsumerConcurrency,
		maxDeliver:        defaultConsumerMaxDeliver,
		nakDelay:          defaultNakDelay,
		dlqSubject:        SubjectDLQ,
		superviseInterval: defaultSuperviseInterval,
		reconnectBase:     defaultReconnectBase,
		reconnectMax:      defaultReconnectMax,
		clock:             RealClock(),
		log:               slog.Default(),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	c.sem = make(chan struct{}, c.concurrency)
	return c
}

// Subscribe registers subject with handler. The subscription is established
// by Start (or re-established by its supervision loop); registering after
// Start is ErrAlreadyStarted.
func (c *Consumer) Subscribe(subject string, handler Handler) error {
	if subject == "" || handler == nil {
		return fmt.Errorf("queue: subject and handler are required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return ErrStopped
	}
	if c.started {
		return ErrAlreadyStarted
	}
	c.specs = append(c.specs, &consumerSpec{subject: subject, handler: handler})
	return nil
}

// Start ensures the retention stream, establishes every subscription and
// launches the delivery + supervision loops. Broker-down at Start time is NOT
// an error: the supervision loop keeps retrying with backoff until it comes
// up (connection resilience). Stream/configuration errors are.
func (c *Consumer) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return ErrStopped
	}
	if c.started {
		c.mu.Unlock()
		return ErrAlreadyStarted
	}
	if len(c.specs) == 0 {
		c.mu.Unlock()
		return fmt.Errorf("queue: consumer %s has no subscriptions", c.name)
	}
	c.started = true
	specs := append([]*consumerSpec(nil), c.specs...)
	c.mu.Unlock()

	if err := c.js.EnsureStream(MessagingStreamName, MessagingStreamSubjects); err != nil {
		c.mu.Lock()
		c.started = false
		c.mu.Unlock()
		return fmt.Errorf("queue: ensure stream: %v", err)
	}

	c.intakeCtx, c.intakeCancel = context.WithCancel(ctx)
	c.drainCtx, c.drainCancel = context.WithCancel(context.Background())
	c.superviseDone = make(chan struct{})

	for _, s := range specs {
		c.trySubscribe(s) // failures are retried by the supervision loop
	}
	go c.supervise(specs)
	c.log.Info("queue: consumer started",
		"name", c.name, "subscriptions", len(specs),
		"concurrency", c.concurrency, "max_deliver", c.maxDeliver)
	return nil
}

// Stop shuts the consumer down: intake stops immediately, in-flight handlers
// are waited on for up to grace, then their context is cancelled. Returns
// ErrGraceExceeded when handlers outlived the grace window (they were
// cancelled; unacked messages redeliver on the next consumer start).
func (c *Consumer) Stop(grace time.Duration) error {
	c.mu.Lock()
	if !c.started || c.stopped {
		c.mu.Unlock()
		return nil
	}
	c.stopped = true
	c.mu.Unlock()

	c.intakeCancel()

	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(done)
	}()
	timer := c.clock.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C():
		c.drainCancel()
		c.log.Warn("queue: consumer stop grace exceeded; in-flight handlers cancelled", "name", c.name)
		<-done
		return ErrGraceExceeded
	}
	c.drainCancel()
	c.log.Info("queue: consumer stopped", "name", c.name,
		"processed", c.processed.Load(), "dead_lettered", c.deadLettered.Load())
	return nil
}

// Stats is the ops/test snapshot of one consumer.
type ConsumerStats struct {
	Subscriptions int
	Processing    int
	Processed     int64
	Redeliveries  int64
	DeadLettered  int64
}

// Stats reports the current counters (test + diagnostics helper).
func (c *Consumer) Stats() ConsumerStats {
	c.mu.Lock()
	specs := len(c.specs)
	c.mu.Unlock()
	return ConsumerStats{
		Subscriptions: specs,
		Processing:    len(c.sem),
		Processed:     c.processed.Load(),
		Redeliveries:  c.redeliveries.Load(),
		DeadLettered:  c.deadLettered.Load(),
	}
}

// ---------------------------------------------------------------------------
// Admin surfaces (issue #116 req 4). The same helpers back the Dispatcher
// methods so the API process can administer the DLQ/replay without running a
// consumer.
// ---------------------------------------------------------------------------

// Replay re-invokes fn for every retained message of subject published at or
// after from (oldest-first). It never acks/republishes anything by itself —
// the caller decides what a replay does (the HTTP admin endpoint republishes
// onto the original subject). Returns the number of replayed messages.
func (c *Consumer) Replay(subject string, from time.Time, fn func(seq uint64, ts time.Time, data []byte) error) (int, error) {
	return replayFrom(c.js, subject, from, fn)
}

// InspectDLQ lists retained dead letters oldest-first (bounded by limit>0).
func (c *Consumer) InspectDLQ(limit int) ([]DLQEntry, error) {
	return inspectDLQ(c.js, c.dlqSubject, limit)
}

// RequeueDLQ republishes the dead letter seq back onto its original subject
// and consumes the DLQ entry (JetStream message delete). The republished
// message is a fresh delivery: consumers treat it like any new message.
func (c *Consumer) RequeueDLQ(seq uint64) (*DLQEntry, error) {
	return requeueDLQ(c.js, c.dlqSubject, seq)
}

// replayFrom is the shared replay implementation (Consumer + Dispatcher).
// Only messaging-plane subjects are replayable: the admin surface can never
// inject traffic outside the plane or loop the DLQ back into itself.
func replayFrom(js JetStream, subject string, from time.Time, fn func(seq uint64, ts time.Time, data []byte) error) (int, error) {
	if !IsReplayableSubject(subject) {
		return 0, fmt.Errorf("queue: subject %q is not replayable (replayable: %v)", subject, ReplayingSubjects())
	}
	lg, ok := js.(Log)
	if !ok {
		return 0, fmt.Errorf("%w: no retained-log surface", ErrNotSupported)
	}
	count := 0
	var visitErr error
	err := lg.Range(subject, func(seq uint64, ts time.Time, data []byte) bool {
		if ts.Before(from) {
			return true
		}
		if ferr := fn(seq, ts, data); ferr != nil {
			visitErr = ferr
			return false
		}
		count++
		return true
	})
	if visitErr != nil {
		return count, visitErr // the visitor's error wins over the range's
	}
	if err != nil && errors.Is(err, ErrNotRetained) {
		return 0, nil // nothing retained: an empty replay succeeds with 0
	}
	return count, err
}

// inspectDLQ is the shared DLQ listing implementation.
func inspectDLQ(js JetStream, dlqSubject string, limit int) ([]DLQEntry, error) {
	lg, ok := js.(Log)
	if !ok {
		return nil, fmt.Errorf("%w: no retained-log surface", ErrNotSupported)
	}
	entries := make([]DLQEntry, 0, 16)
	err := lg.Range(dlqSubject, func(seq uint64, ts time.Time, data []byte) bool {
		entry, perr := decodeDLQEnvelope(seq, data)
		if perr == nil {
			entries = append(entries, *entry)
		}
		return limit <= 0 || len(entries) < limit
	})
	if err != nil && errors.Is(err, ErrNotRetained) {
		return []DLQEntry{}, nil // empty DLQ
	}
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// requeueDLQ is the shared DLQ requeue implementation.
func requeueDLQ(js JetStream, dlqSubject string, seq uint64) (*DLQEntry, error) {
	lg, ok := js.(Log)
	if !ok {
		return nil, fmt.Errorf("%w: no retained-log surface", ErrNotSupported)
	}
	var found *DLQEntry
	err := lg.Range(dlqSubject, func(lseq uint64, _ time.Time, data []byte) bool {
		if lseq != seq {
			return true
		}
		if entry, derr := decodeDLQEnvelope(seq, data); derr == nil {
			found = entry
		}
		return false
	})
	if err != nil && !errors.Is(err, ErrNotRetained) {
		return nil, err
	}
	if found == nil {
		return nil, ErrEntryNotFound
	}
	if _, err := js.Publish(found.Subject, found.Payload); err != nil {
		return nil, fmt.Errorf("queue: requeue publish to %s: %v", found.Subject, err)
	}
	if err := lg.Delete(dlqSubject, seq); err != nil {
		// The republish already happened; the stale DLQ entry is retried on
		// the next requeue call (idempotence note: republishing twice is
		// better than losing the dead letter).
		return found, fmt.Errorf("queue: consume dlq entry: %v", err)
	}
	return found, nil
}

// decodeDLQEnvelope parses one retained DLQ message.
func decodeDLQEnvelope(seq uint64, data []byte) (*DLQEntry, error) {
	var env DLQEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("queue: dlq envelope %d: %v", seq, err)
	}
	entry := &DLQEntry{
		Sequence:       seq,
		Subject:        env.Subject,
		TaskID:         env.TaskID,
		Error:          env.Error,
		Attempts:       env.Attempts,
		DeadLetteredAt: env.DeadLetteredAt,
		Payload:        env.Payload,
	}
	// Best-effort run identity for run-dispatch payloads: the inspect view
	// surfaces it so operators can correlate with GET /queue/tasks/{id} and
	// the runs timeline without decoding the payload by hand.
	var run RunEnvelope
	if err := json.Unmarshal(env.Payload, &run); err == nil && run.TaskType != "" {
		entry.RunID = run.RunID
		entry.OrganizationID = run.OrganizationID
		if entry.TaskID == "" {
			entry.TaskID = run.TaskID
		}
	}
	return entry, nil
}

// ---------------------------------------------------------------------------
// Delivery pipeline
// ---------------------------------------------------------------------------

// trySubscribe establishes (or re-establishes) one subscription, honoring the
// per-spec reconnect backoff. Called from Start and the supervision loop.
func (c *Consumer) trySubscribe(s *consumerSpec) {
	s.backoffMu.Lock()
	defer s.backoffMu.Unlock()
	now := c.clock.Now()
	if s.nextAttempt.After(now) {
		return // still backing off
	}
	if s.sub != nil && s.sub.Valid() {
		s.backoff = 0
		return
	}
	sub, err := c.js.Subscribe(s.subject, durableName(c.name, s.subject), c.jsHandler(s))
	if err != nil {
		if s.backoff == 0 {
			s.backoff = c.reconnectBase
		} else {
			s.backoff *= 2
			if s.backoff > c.reconnectMax {
				s.backoff = c.reconnectMax
			}
		}
		s.nextAttempt = now.Add(s.backoff)
		c.log.Warn("queue: consumer subscribe failed; will retry",
			"name", c.name, "subject", s.subject, "backoff", s.backoff.String(), "error", err.Error())
		return
	}
	s.sub = sub
	s.backoff = 0
	s.nextAttempt = time.Time{}
	c.log.Info("queue: consumer subscribed", "name", c.name, "subject", s.subject)
}

// supervise is the connection-resilience loop: every superviseInterval it
// checks every subscription's validity and re-establishes dead ones with
// exponential backoff (per subscription). It exits when intake is cancelled
// (Stop).
func (c *Consumer) supervise(specs []*consumerSpec) {
	defer close(c.superviseDone)
	for {
		timer := c.clock.NewTimer(c.superviseInterval)
		select {
		case <-c.intakeCtx.Done():
			timer.Stop()
			return
		case <-timer.C():
		}
		for _, s := range specs {
			c.trySubscribe(s)
		}
	}
}

// jsHandler is the broker-facing intake: it bounds concurrency with the
// semaphore (backpressure toward the broker delivery loop) and hands the
// message to a worker goroutine.
func (c *Consumer) jsHandler(s *consumerSpec) func(Message) {
	return func(msg Message) {
		// Register in-flight BEFORE checking shutdown, under the same mutex
		// Stop uses to flip `stopped`, so Wait can never race an Add.
		c.mu.Lock()
		if c.stopped {
			c.mu.Unlock()
			return // leave unacked: redelivered by the next consumer run
		}
		c.wg.Add(1)
		c.mu.Unlock()

		select {
		case <-c.intakeCtx.Done():
			c.wg.Done()
			return
		case c.sem <- struct{}{}:
		}
		go c.process(s, msg)
	}
}

// process runs one handler with the retry-cap/dead-letter contract.
func (c *Consumer) process(s *consumerSpec, msg Message) {
	defer c.wg.Done()
	defer func() { <-c.sem }()
	defer func() {
		if r := recover(); r != nil {
			c.handleFailure(s, msg, fmt.Errorf("queue: handler panic: %v", r))
		}
	}()

	err := s.handler(c.drainCtx, msg)
	if err == nil {
		c.processed.Add(1)
		c.incr(MetricMessagingProcessed)
		if aerr := msg.Ack(); aerr != nil {
			// Ack failure redelivers the message later (server-side); the
			// handler already succeeded, so a replayed execution must be
			// idempotent — the same at-least-once contract the pull model
			// documents (BeginNodeRun refuses terminal replays).
			c.log.Warn("queue: ack failed (message will redeliver)",
				"subject", msg.Subject(), "seq", msg.Sequence(), "error", aerr.Error())
		}
		return
	}
	c.handleFailure(s, msg, err)
}

// handleFailure implements nak-with-retry-cap → dead-letter.
func (c *Consumer) handleFailure(s *consumerSpec, msg Message, err error) {
	attempts := msg.Deliveries()
	if attempts == 0 {
		attempts = 1 // unknown server metadata: count this delivery
	}
	if attempts < c.maxDeliver {
		c.redeliveries.Add(1)
		c.incr(MetricMessagingRedelivered)
		c.log.Warn("queue: handler failed; nak for redelivery",
			"subject", msg.Subject(), "seq", msg.Sequence(),
			"attempt", attempts, "max_deliver", c.maxDeliver, "error", err.Error())
		_ = msg.NakWithDelay(c.nakDelay)
		return
	}

	// Retry cap exhausted: dead-letter (terminal) exactly like the engine's
	// Attempts>=4 -> dead_letter rule. The DLQ publish failing leaves the
	// message unacked (naked) so the next redelivery retries the DLQ write.
	c.deadLettered.Add(1)
	c.incr(MetricMessagingDeadLettered)
	c.log.Error("queue: message dead-lettered",
		"subject", msg.Subject(), "seq", msg.Sequence(), "attempts", attempts,
		"dlq", c.dlqSubject, "error", err.Error())
	env := DLQEnvelope{
		Subject:        msg.Subject(),
		Error:          err.Error(),
		Attempts:       attempts,
		DeadLetteredAt: c.clock.Now(),
		Payload:        append([]byte(nil), msg.Data()...),
	}
	if run, rerr := decodeRunEnvelope(msg.Data()); rerr == nil {
		env.TaskID = run.TaskID
	}
	b, merr := json.Marshal(env)
	if merr != nil {
		c.log.Error("queue: dlq envelope marshal failed", "error", merr.Error())
		_ = msg.Nak()
		return
	}
	if _, perr := c.js.Publish(c.dlqSubject, b); perr != nil {
		c.log.Error("queue: dlq publish failed; message stays unacked for retry",
			"error", perr.Error())
		_ = msg.Nak()
		return
	}
	_ = msg.Ack() // terminal: safely parked in the DLQ
}

func (c *Consumer) incr(name string) {
	if c.metrics != nil {
		c.metrics.Inc(name)
	}
}

// durableName derives the broker-side durable consumer name from the
// consumer's logical name and subject (NATS-safe: letters, digits, dashes).
func durableName(consumer, subject string) string {
	return "agentos-" + sanitizeDurable(consumer) + "-" + sanitizeDurable(path.Base("/"+subject))
}

func sanitizeDurable(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9', ch == '-', ch == '_':
			out = append(out, ch)
		default:
			out = append(out, '-')
		}
	}
	if len(out) == 0 {
		return "sub"
	}
	return string(out)
}
