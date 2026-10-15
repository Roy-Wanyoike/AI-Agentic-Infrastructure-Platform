package queue

// clock.go provides the time abstraction for the NATS messaging layer
// (issue #116). Redelivery scheduling (nak with delay) and the consumer
// reconnect backoff must be deterministic under test, so every timer in the
// messaging layer flows through this Clock. The default is real time; tests
// inject FakeClock and drive it with Advance.

import (
	"context"
	"sync"
	"time"
)

// Clock abstracts the two time operations the messaging layer needs: reading
// the current instant and waiting for a duration. Implementations must be
// safe for concurrent use.
type Clock interface {
	Now() time.Time
	// NewTimer returns a Timer that fires once after d. The timer is not
	// cancelled automatically: callers own it (see Timer.Stop).
	NewTimer(d time.Duration) Timer
}

// Timer is the Clock-scoped equivalent of *time.Timer.
type Timer interface {
	// C yields the firing instant. The channel has capacity one and receives
	// exactly one value (never closed).
	C() <-chan time.Time
	// Stop prevents a pending firing; it reports whether the timer was
	// stopped before firing (mirrors *time.Timer.Stop semantics).
	Stop() bool
}

// realClock is the production Clock.
type realClock struct{}

// RealClock returns the wall-clock Clock used by default.
func RealClock() Clock { return realClock{} }

func (realClock) Now() time.Time { return time.Now().UTC() }

func (realClock) NewTimer(d time.Duration) Timer {
	return &realTimer{t: time.NewTimer(d)}
}

type realTimer struct{ t *time.Timer }

func (t *realTimer) C() <-chan time.Time { return t.t.C }
func (t *realTimer) Stop() bool          { return t.t.Stop() }

// Sleep waits for d (or until ctx is cancelled) on the given Clock. It is the
// messaging-layer replacement for time.Sleep so tests can advance FakeClock
// instead of sleeping in real time.
func Sleep(ctx context.Context, c Clock, d time.Duration) error {
	if c == nil {
		c = RealClock()
	}
	timer := c.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ---------------------------------------------------------------------------
// FakeClock — deterministic, advanceable Clock for tests.
// ---------------------------------------------------------------------------

// FakeClock is a Clock whose current instant only moves via Advance. Timers
// created through NewTimer fire exactly when an Advance crosses their
// deadline, so redelivery and backoff tests are fully deterministic (no
// real-time sleeps drive correctness; real time only bounds failure via the
// test's own deadlines).
type FakeClock struct {
	mu     sync.Mutex
	now    time.Time
	nextID uint64
	timers map[uint64]*fakeTimer
}

// NewFakeClock returns a FakeClock starting at start.
func NewFakeClock(start time.Time) *FakeClock {
	return &FakeClock{
		now:    start.UTC(),
		timers: make(map[uint64]*fakeTimer),
	}
}

// Now implements Clock.
func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// NewTimer implements Clock. The returned timer fires when a later Advance
// moves the clock to or past its deadline.
func (c *FakeClock) NewTimer(d time.Duration) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	t := &fakeTimer{
		id:       c.nextID,
		deadline: c.now.Add(d),
		ch:       make(chan time.Time, 1),
		clock:    c,
	}
	c.timers[t.id] = t
	return t
}

// Advance moves the clock forward by d and fires every timer whose deadline
// is now due, in deadline order. Timers that were stopped before their
// deadline never fire. Firing is synchronous with respect to Advance: when
// Advance returns, every due timer channel holds a value.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	now := c.now.Add(d)
	// Collect due timers first (firing below must not observe the map while
	// it mutates through Stop).
	type due struct {
		ch       chan time.Time
		deadline time.Time
	}
	dueTimers := make([]due, 0, len(c.timers))
	for _, t := range c.timers {
		if !t.deadline.After(now) {
			dueTimers = append(dueTimers, due{ch: t.ch, deadline: t.deadline})
		}
	}
	for id, t := range c.timers {
		if !t.deadline.After(now) {
			delete(c.timers, id)
		}
	}
	c.now = now
	c.mu.Unlock()

	// Fire in deadline order for determinism.
	for i := 0; i < len(dueTimers); i++ {
		for j := i + 1; j < len(dueTimers); j++ {
			if dueTimers[j].deadline.Before(dueTimers[i].deadline) {
				dueTimers[i], dueTimers[j] = dueTimers[j], dueTimers[i]
			}
		}
		dueTimers[i].ch <- now // buffered(1): never blocks
	}
}

// PendingTimers reports how many timers have not fired yet (test helper for
// asserting that a backoff/redelivery timer is armed).
func (c *FakeClock) PendingTimers() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

type fakeTimer struct {
	id       uint64
	deadline time.Time
	ch       chan time.Time
	clock    *FakeClock
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }

func (t *fakeTimer) Stop() bool {
	c := t.clock
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.timers[t.id]; !ok {
		return false // already fired (or never armed)
	}
	delete(c.timers, t.id)
	return true
}
