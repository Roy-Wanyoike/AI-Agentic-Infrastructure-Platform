package queue

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// consumer_test.go exercises the consumer framework (issue #116 req 1/4) end
// to end against the in-memory simulator with the injected FakeClock:
// lifecycle + graceful stop, ack, nak-with-retry-cap -> dead-letter, bounded
// concurrency, reconnect supervision (initial failure + connection drop),
// replay-from-timestamp and DLQ inspect/requeue. No real-time sleeps drive
// correctness; the polling helper only bounds failure.

// testCounter is the Counter sink for metric assertions.
type testCounter struct {
	mu     sync.Mutex
	counts map[string]int64
}

func newTestCounter() *testCounter { return &testCounter{counts: map[string]int64{}} }

func (c *testCounter) Inc(name string) {
	c.mu.Lock()
	c.counts[name]++
	c.mu.Unlock()
}

func (c *testCounter) get(name string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[name]
}

// captureJS decorates a JetStream seam, optionally failing the first N
// Subscribe calls (initial-outage + reconnect-backoff tests) and capturing
// the live subscriptions (connection-drop tests).
type captureJS struct {
	JetStream
	mu        sync.Mutex
	failAfter int // fail the first N subscribes
	subs      []Subscription
}

func (c *captureJS) Subscribe(subject, durable string, handler func(Message)) (Subscription, error) {
	c.mu.Lock()
	if c.failAfter > 0 {
		c.failAfter--
		c.mu.Unlock()
		return nil, fmt.Errorf("simulated subscribe outage")
	}
	c.mu.Unlock()
	sub, err := c.JetStream.Subscribe(subject, durable, handler)
	if err == nil {
		c.mu.Lock()
		c.subs = append(c.subs, sub)
		c.mu.Unlock()
	}
	return sub, err
}

func (c *captureJS) liveSubs() []Subscription {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Subscription(nil), c.subs...)
}

func TestConsumerLifecycleSubscribeStartStop(t *testing.T) {
	sim, _ := newTestSim(t)
	clock := NewFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	c := NewConsumer(sim, "lifecycle", WithClock(clock), WithSupervision(time.Hour, time.Hour, time.Hour))

	if err := c.Subscribe(SubjectRunsCreate, func(context.Context, Message) error { return nil }); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := c.Subscribe("", nil); err == nil {
		t.Fatal("empty subscribe accepted")
	}
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := c.Subscribe(SubjectRunsCreate, func(context.Context, Message) error { return nil }); !errors.Is(err, ErrAlreadyStarted) {
		t.Fatalf("Subscribe after Start = %v, want ErrAlreadyStarted", err)
	}
	if err := c.Start(context.Background()); !errors.Is(err, ErrAlreadyStarted) {
		t.Fatalf("second Start = %v, want ErrAlreadyStarted", err)
	}
	if err := c.Stop(time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := c.Stop(time.Second); err != nil {
		t.Fatalf("idempotent Stop = %v, want nil", err)
	}
	if err := c.Start(context.Background()); !errors.Is(err, ErrStopped) {
		t.Fatalf("Start after Stop = %v, want ErrStopped", err)
	}
	if err := c.Subscribe(SubjectRunsCreate, func(context.Context, Message) error { return nil }); !errors.Is(err, ErrStopped) {
		t.Fatalf("Subscribe after Stop = %v, want ErrStopped", err)
	}
}

func TestConsumerStartFailsWithoutSubscriptionsOrStream(t *testing.T) {
	sim, _ := newTestSim(t)
	clock := NewFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	c := NewConsumer(sim, "empty", WithClock(clock))
	if err := c.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "no subscriptions") {
		t.Fatalf("Start without subscriptions = %v, want an error", err)
	}

	// A closed broker is a configuration error at Start (not a retryable
	// outage: the seam cannot even ensure the stream).
	closed := NewSimJetStream(clock)
	_ = closed.Close()
	c2 := NewConsumer(closed, "closed", WithClock(clock))
	_ = c2.Subscribe(SubjectRunsCreate, func(context.Context, Message) error { return nil })
	if err := c2.Start(context.Background()); err == nil {
		t.Fatal("Start against a closed broker succeeded, want a configuration error")
	}
}

func TestConsumerAckCompletesWithoutRedelivery(t *testing.T) {
	sim, clock := newTestSim(t)
	counter := newTestCounter()
	c := NewConsumer(sim, "ack", WithClock(clock), WithCounter(counter),
		WithSupervision(time.Hour, time.Hour, time.Hour))
	done := make(chan struct{}, 4)
	if err := c.Subscribe(SubjectRunsCreate, func(_ context.Context, m Message) error {
		done <- struct{}{}
		return nil
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = c.Stop(time.Second) }()

	if _, err := sim.Publish(SubjectRunsCreate, []byte(`{"task_type":"agent.run"}`)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler not invoked")
	}
	waitFor(t, "processed counter", func() bool { return c.Stats().Processed == 1 })
	if got := counter.get(MetricMessagingProcessed); got != 1 {
		t.Fatalf("processed metric = %d, want 1", got)
	}
	clock.Advance(time.Hour) // any redelivery timer would fire now
	waitFor(t, "no redelivery", func() bool { return c.Stats().Redeliveries == 0 && sim.RetainedCount(SubjectDLQ) == 0 })
	if c.Stats().Redeliveries != 0 || sim.RetainedCount(SubjectDLQ) != 0 {
		t.Fatal("acked message redelivered or dead-lettered")
	}
}

func TestConsumerRetryCapDeadLettersToDLQ(t *testing.T) {
	sim, clock := newTestSim(t)
	counter := newTestCounter()
	c := NewConsumer(sim, "dlq", WithClock(clock), WithMaxDeliver(2), WithNakDelay(time.Second),
		WithCounter(counter),
		WithSupervision(time.Hour, time.Hour, time.Hour))
	var calls int
	var mu sync.Mutex
	if err := c.Subscribe(SubjectRunsCreate, func(_ context.Context, m Message) error {
		mu.Lock()
		calls++
		mu.Unlock()
		return errors.New("boom")
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = c.Stop(time.Second) }()

	payload := []byte(`{"task_type":"agent.run","task_id":"task-1","run_id":"run-9","organization_id":"org-7"}`)
	if _, err := sim.Publish(SubjectRunsCreate, payload); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Attempt 1 fails -> nak(1s). The redelivery is armed on the injected
	// clock and fires exactly when Advance crosses it. Two timers are pending
	// once the nak is armed: the redelivery and the (1h) supervision tick.
	waitFor(t, "first attempt", func() bool { mu.Lock(); defer mu.Unlock(); return calls == 1 })
	waitFor(t, "redelivery armed", func() bool {
		return c.Stats().Redeliveries == 1 && clock.PendingTimers() == 2
	})
	if got := sim.RetainedCount(SubjectDLQ); got != 0 {
		t.Fatalf("DLQ populated before the retry cap: %d", got)
	}
	clock.Advance(time.Second)

	// Attempt 2 (deliveries == maxDeliver) fails -> dead-letter + ack.
	waitFor(t, "dead letter", func() bool { return sim.RetainedCount(SubjectDLQ) == 1 })
	waitFor(t, "dead-letter counter", func() bool { return c.Stats().DeadLettered == 1 })

	entries, err := c.InspectDLQ(10)
	if err != nil {
		t.Fatalf("InspectDLQ: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("DLQ entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.Subject != SubjectRunsCreate || e.Error != "boom" || e.Attempts != 2 {
		t.Fatalf("DLQ entry = %+v", e)
	}
	if e.TaskID != "task-1" || e.RunID != "run-9" || e.OrganizationID != "org-7" {
		t.Fatalf("DLQ identity fields not extracted: %+v", e)
	}
	if got := counter.get(MetricMessagingRedelivered); got != 1 {
		t.Fatalf("redelivered metric = %d, want 1", got)
	}
	if got := counter.get(MetricMessagingDeadLettered); got != 1 {
		t.Fatalf("dead-lettered metric = %d, want 1", got)
	}

	// The terminal message was acked: a further advance produces nothing new.
	clock.Advance(time.Hour)
	waitFor(t, "dlq stable", func() bool { return sim.RetainedCount(SubjectDLQ) == 1 })
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Fatalf("handler calls = %d, want 2 (no post-dead-letter retries)", calls)
	}
}

func TestConsumerDLQRequeueRunsAgain(t *testing.T) {
	sim, clock := newTestSim(t)
	c := NewConsumer(sim, "requeue", WithClock(clock), WithMaxDeliver(1),
		WithSupervision(time.Hour, time.Hour, time.Hour))
	var fail atomicBool
	if err := c.Subscribe(SubjectRunsCreate, func(_ context.Context, m Message) error {
		if fail.load() {
			return errors.New("transient")
		}
		return nil
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = c.Stop(time.Second) }()

	fail.store(true) // the first delivery must fail into the DLQ
	if _, err := sim.Publish(SubjectRunsCreate, []byte(`{"task_type":"agent.run","task_id":"task-r"}`)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	waitFor(t, "dead letter", func() bool { return sim.RetainedCount(SubjectDLQ) == 1 })

	// Fix the underlying fault, then requeue: the payload is republished onto
	// the original subject and the DLQ entry is consumed.
	fail.store(false)
	entries, err := c.InspectDLQ(0) // limit<=0 => unbounded
	if err != nil || len(entries) != 1 {
		t.Fatalf("InspectDLQ = %v, %d entries", err, len(entries))
	}
	entry, err := c.RequeueDLQ(entries[0].Sequence)
	if err != nil {
		t.Fatalf("RequeueDLQ: %v", err)
	}
	if entry.Subject != SubjectRunsCreate || entry.TaskID != "task-r" {
		t.Fatalf("requeued entry = %+v", entry)
	}
	waitFor(t, "requeued delivery processed", func() bool { return c.Stats().Processed == 1 })
	waitFor(t, "dlq drained", func() bool { return len(mustInspect(t, c)) == 0 })

	if _, err := c.RequeueDLQ(entries[0].Sequence); !errors.Is(err, ErrEntryNotFound) {
		t.Fatalf("second requeue = %v, want ErrEntryNotFound", err)
	}
}

func mustInspect(t *testing.T, c *Consumer) []DLQEntry {
	t.Helper()
	entries, err := c.InspectDLQ(0)
	if err != nil {
		t.Fatalf("InspectDLQ: %v", err)
	}
	return entries
}

// atomicBool is a tiny test flag (no imports beyond sync).
type atomicBool struct {
	mu sync.Mutex
	v  bool
}

func (b *atomicBool) load() bool   { b.mu.Lock(); defer b.mu.Unlock(); return b.v }
func (b *atomicBool) store(v bool) { b.mu.Lock(); b.v = v; b.mu.Unlock() }

func TestConsumerBoundedConcurrency(t *testing.T) {
	sim, _ := newTestSim(t)
	const cap = 2
	c := NewConsumer(sim, "cap", WithClock(NewFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))),
		WithConcurrency(cap), WithSupervision(time.Hour, time.Hour, time.Hour))

	var mu sync.Mutex
	active, maxActive := 0, 0
	release := make(chan struct{})
	if err := c.Subscribe(SubjectRunsCreate, func(_ context.Context, m Message) error {
		mu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		mu.Unlock()
		<-release
		mu.Lock()
		active--
		mu.Unlock()
		return nil
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = c.Stop(time.Second) }()

	const total = 6
	for i := 0; i < total; i++ {
		if _, err := sim.Publish(SubjectRunsCreate, []byte{byte(i)}); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	// The semaphore admits exactly `cap` handlers; the rest wait in the
	// broker mailbox.
	waitFor(t, "semaphore saturated", func() bool { return c.Stats().Processing == cap })
	mu.Lock()
	reached := maxActive
	mu.Unlock()
	if reached != cap {
		t.Fatalf("max in-flight = %d, want exactly %d (cap honored and parallelism proven)", reached, cap)
	}
	close(release)
	waitFor(t, "all processed", func() bool { return c.Stats().Processed == total })
	if got := c.Stats().Processing; got != 0 {
		t.Fatalf("in-flight after drain = %d, want 0", got)
	}
}

func TestConsumerStopGraceExceededCancelsDrainingHandlers(t *testing.T) {
	sim, clock := newTestSim(t)
	c := NewConsumer(sim, "grace", WithClock(clock), WithSupervision(time.Hour, time.Hour, time.Hour))
	handlerEntered := make(chan struct{}, 1)
	if err := c.Subscribe(SubjectRunsCreate, func(ctx context.Context, m Message) error {
		handlerEntered <- struct{}{}
		<-ctx.Done() // a draining handler is only cancelled after the grace window
		return nil
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := sim.Publish(SubjectRunsCreate, []byte("x")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	select {
	case <-handlerEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("handler not entered")
	}

	stopErr := make(chan error, 1)
	go func() { stopErr <- c.Stop(time.Hour) }()
	// Determinism: keep advancing until Stop returns. Stop arms its grace
	// timer some time after the call (the supervision tick may or may not be
	// pending yet), and only an Advance that lands after the arming can fire
	// it — polling Advance removes the interleaving from the equation.
	graceResult := func() (bool, error) {
		select {
		case err := <-stopErr:
			return true, err
		default:
			return false, nil
		}
	}
	stopDeadline := time.Now().Add(2 * time.Second)
	for {
		if done, err := graceResult(); done {
			if !errors.Is(err, ErrGraceExceeded) {
				t.Fatalf("Stop = %v, want ErrGraceExceeded", err)
			}
			break
		}
		if time.Now().After(stopDeadline) {
			t.Fatal("Stop did not return after the grace timer fired")
		}
		clock.Advance(time.Hour) // fires the grace timer once Stop has armed it
		time.Sleep(time.Millisecond)
	}
}

func TestConsumerGracefulStopWaitsForInFlight(t *testing.T) {
	sim, _ := newTestSim(t)
	clock := NewFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	c := NewConsumer(sim, "drain", WithClock(clock), WithSupervision(time.Hour, time.Hour, time.Hour))
	finished := make(chan struct{}, 1)
	if err := c.Subscribe(SubjectRunsCreate, func(ctx context.Context, m Message) error {
		time.Sleep(50 * time.Millisecond) // real-time work bounded by the test
		finished <- struct{}{}
		return nil
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := sim.Publish(SubjectRunsCreate, []byte("x")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	waitFor(t, "handler started", func() bool { return c.Stats().Processing == 1 })

	stopErr := make(chan error, 1)
	go func() { stopErr <- c.Stop(time.Second) }()
	// The handler completes inside the grace window: Stop waits (no
	// ErrGraceExceeded) and the handler ran to completion.
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not finish")
	}
	select {
	case err := <-stopErr:
		if err != nil {
			t.Fatalf("Stop = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return after the in-flight handler finished")
	}
	if c.Stats().Processed != 1 {
		t.Fatalf("processed = %d, want 1 (draining handler completed and acked)", c.Stats().Processed)
	}
}

func TestConsumerHandlerPanicIsContainedAndDeadLettered(t *testing.T) {
	sim, _ := newTestSim(t)
	clock := NewFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	c := NewConsumer(sim, "panic", WithClock(clock), WithMaxDeliver(1),
		WithSupervision(time.Hour, time.Hour, time.Hour))
	if err := c.Subscribe(SubjectRunsCreate, func(context.Context, Message) error {
		panic("handler exploded")
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = c.Stop(time.Second) }()
	if _, err := sim.Publish(SubjectRunsCreate, []byte(`{"task_type":"agent.run"}`)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	waitFor(t, "dead letter", func() bool { return sim.RetainedCount(SubjectDLQ) == 1 })
	entries, err := c.InspectDLQ(10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("InspectDLQ = %v, %d", err, len(entries))
	}
	if !strings.Contains(entries[0].Error, "handler exploded") {
		t.Fatalf("DLQ error = %q, want the panic value", entries[0].Error)
	}
}

func TestConsumerReconnectsAfterInitialSubscribeFailure(t *testing.T) {
	sim, clock := newTestSim(t)
	flaky := &captureJS{JetStream: sim, failAfter: 1}
	c := NewConsumer(flaky, "outage", WithClock(clock),
		WithSupervision(50*time.Millisecond, 100*time.Millisecond, time.Second))
	delivered := make(chan Message, 4)
	if err := c.Subscribe(SubjectRunsCreate, func(_ context.Context, m Message) error {
		delivered <- m
		return nil
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v (broker-down at Start must NOT fail)", err)
	}
	// t+0: initial subscribe failed; backoff armed at base=100ms. Each poll
	// advances exactly one supervision tick, so the retry lands on the tick
	// that crosses nextAttempt regardless of goroutine interleaving.
	waitFor(t, "subscribe retried after backoff", func() bool {
		clock.Advance(50 * time.Millisecond)
		return len(flaky.liveSubs()) == 1
	})
	if _, err := sim.Publish(SubjectRunsCreate, []byte(`{"task_type":"agent.run"}`)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	select {
	case <-delivered:
	case <-time.After(2 * time.Second):
		t.Fatal("message not delivered after reconnect")
	}
	_ = c.Stop(time.Second)
}

func TestConsumerReconnectsAfterConnectionDropAndRedelivers(t *testing.T) {
	sim, clock := newTestSim(t)
	wrapped := &captureJS{JetStream: sim}
	c := NewConsumer(wrapped, "drop", WithClock(clock), WithNakDelay(time.Hour),
		WithSupervision(50*time.Millisecond, 10*time.Millisecond, time.Second))
	var attempts int
	var mu sync.Mutex
	if err := c.Subscribe(SubjectRunsCreate, func(_ context.Context, m Message) error {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n == 1 {
			// Fail without acking; the redelivery delay is far in the future —
			// only the reconnect path can redeliver this message now.
			return errors.New("drop me")
		}
		return nil
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = c.Stop(time.Second) }()

	if _, err := sim.Publish(SubjectRunsCreate, []byte(`{"task_type":"agent.run"}`)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	waitFor(t, "first failed attempt", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return attempts == 1
	})

	// Simulate a connection drop on the live subscription: the supervision
	// loop must re-establish it, and the durable replays the unacked message.
	subs := wrapped.liveSubs()
	if len(subs) != 1 {
		t.Fatalf("live subscriptions = %d, want 1", len(subs))
	}
	sim.DisconnectSubscription(subs[0])
	clock.Advance(50 * time.Millisecond) // one supervision pass -> resubscribe

	waitFor(t, "redelivery after reconnect", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return attempts == 2
	})
	waitFor(t, "replayed message processed", func() bool { return c.Stats().Processed == 1 })
}

func TestConsumerReplayFromTimestamp(t *testing.T) {
	sim, clock := newTestSim(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := NewConsumer(sim, "replay", WithClock(clock)) // Replay needs no Start

	publishAt := func(offset time.Duration, body string) uint64 {
		t.Helper()
		clock.Advance(offset)
		seq, err := sim.Publish(SubjectRunsCreate, []byte(body))
		if err != nil {
			t.Fatalf("publish at %v: %v", offset, err)
		}
		return seq
	}
	publishAt(0, "m0")           // t0
	publishAt(time.Second, "m1") // t0+1s (== the replay origin: inclusive)
	publishAt(time.Second, "m2") // t0+2s

	var replayed []uint64
	n, err := c.Replay(SubjectRunsCreate, base.Add(time.Second), func(seq uint64, _ time.Time, data []byte) error {
		replayed = append(replayed, seq)
		if len(data) == 0 {
			t.Fatalf("replayed seq %d carries no data", seq)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if n != 2 || len(replayed) != 2 || replayed[0] != 2 || replayed[1] != 3 {
		t.Fatalf("replay from t0+1s = %d entries %v, want seqs [2 3] (origin inclusive)", n, replayed)
	}
	// Earliest-boundary replay returns everything; a failing visitor stops
	// the scan with the error.
	n, err = c.Replay(SubjectRunsCreate, base, func(seq uint64, _ time.Time, _ []byte) error {
		return fmt.Errorf("stop at %d", seq)
	})
	if err == nil || n != 0 {
		t.Fatalf("failing visitor = (%d, %v), want (0, error)", n, err)
	}
	if _, err := c.Replay(SubjectWorkflowNodeCompleted, base, func(uint64, time.Time, []byte) error { return nil }); err != nil {
		t.Fatalf("replay of an empty log = %v, want nil (0 replayed)", err)
	}
}

func TestConsumerReplayRejectsNonReplayableSubjects(t *testing.T) {
	sim, _ := newTestSim(t)
	c := NewConsumer(sim, "replay-guard")
	if _, err := sim.Publish(SubjectDLQ, []byte("{}")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if _, err := c.Replay(SubjectDLQ, time.Time{}, func(uint64, time.Time, []byte) error { return nil }); err == nil {
		t.Fatal("DLQ replay accepted (would loop dead letters)")
	}
	if _, err := c.Replay("agentos.foo", time.Time{}, func(uint64, time.Time, []byte) error { return nil }); err == nil {
		t.Fatal("foreign subject replay accepted")
	}
	if !IsReplayableSubject(SubjectRunsCreate) || !IsReplayableSubject(SubjectWorkflowNodeCompleted) {
		t.Fatal("messaging-plane subjects must be replayable")
	}
}
