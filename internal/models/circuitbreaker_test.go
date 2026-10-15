package models

import (
	"testing"
	"time"
)

// fakeClock is a controllable Clock for deterministic breaker tests.
type fakeClock struct {
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func TestCircuitBreakerDefaults(t *testing.T) {
	cfg := DefaultCircuitBreakerConfig()
	if cfg.FailureThreshold != 5 {
		t.Fatalf("FailureThreshold = %d, want 5", cfg.FailureThreshold)
	}
	if cfg.Cooldown != 30*time.Second {
		t.Fatalf("Cooldown = %s, want 30s", cfg.Cooldown)
	}
	if cfg.HalfOpenSuccesses != 2 {
		t.Fatalf("HalfOpenSuccesses = %d, want 2", cfg.HalfOpenSuccesses)
	}
	// Zero fields fall back to the documented defaults.
	b := NewCircuitBreaker(CircuitBreakerConfig{})
	if b.cfg.FailureThreshold != DefaultFailureThreshold ||
		b.cfg.Cooldown != DefaultCooldown ||
		b.cfg.HalfOpenSuccesses != DefaultHalfOpenSuccesses {
		t.Fatalf("zero config must take defaults, got %+v", b.cfg)
	}
	if b.State() != BreakerClosed {
		t.Fatalf("new breaker state = %s, want closed", b.State())
	}
	if !b.Allow() {
		t.Fatal("closed breaker must allow calls")
	}
}

func TestCircuitBreakerOpensAfterConsecutiveFailures(t *testing.T) {
	clk := newFakeClock()
	b := NewCircuitBreaker(CircuitBreakerConfig{FailureThreshold: 3, Clock: clk})

	for i := 0; i < 2; i++ {
		if !b.Allow() {
			t.Fatalf("call %d should be allowed while closed", i+1)
		}
		b.RecordFailure()
	}
	if b.State() != BreakerClosed {
		t.Fatalf("2 failures below threshold 3 must stay closed, got %s", b.State())
	}
	b.RecordFailure() // third consecutive failure -> open
	if b.State() != BreakerOpen {
		t.Fatalf("3 consecutive failures must open the breaker, got %s", b.State())
	}
	if b.Allow() {
		t.Fatal("open breaker must reject calls")
	}
	snap := b.Snapshot()
	if snap.ConsecutiveFailures != 3 || snap.Failures != 3 || snap.OpenCount != 1 || snap.Transitions != 1 {
		t.Fatalf("unexpected snapshot after open: %+v", snap)
	}
	if snap.OpenedAt != clk.Now() {
		t.Fatalf("OpenedAt = %s, want the clock time %s", snap.OpenedAt, clk.Now())
	}
	// The rejection is counted.
	if s := b.Snapshot(); s.Rejections != 1 {
		t.Fatalf("Rejections = %d, want 1", s.Rejections)
	}
}

func TestCircuitBreakerSuccessResetsConsecutiveFailures(t *testing.T) {
	clk := newFakeClock()
	b := NewCircuitBreaker(CircuitBreakerConfig{FailureThreshold: 3, Clock: clk})

	b.RecordFailure()
	b.RecordFailure()
	if !b.Allow() {
		t.Fatal("closed breaker must allow")
	}
	b.RecordSuccess()
	if s := b.Snapshot(); s.ConsecutiveFailures != 0 {
		t.Fatalf("success must reset the consecutive count, got %d", s.ConsecutiveFailures)
	}
	b.RecordFailure()
	b.RecordFailure()
	if b.State() != BreakerClosed {
		t.Fatalf("interleaved success must prevent opening, got %s", b.State())
	}
	snap := b.Snapshot()
	if snap.Successes != 1 || snap.Failures != 4 {
		t.Fatalf("counters: successes=%d failures=%d, want 1/4", snap.Successes, snap.Failures)
	}
}

func TestCircuitBreakerCooldownToHalfOpenThenClose(t *testing.T) {
	clk := newFakeClock()
	b := NewCircuitBreaker(CircuitBreakerConfig{FailureThreshold: 2, Cooldown: 30 * time.Second, HalfOpenSuccesses: 2, Clock: clk})

	b.RecordFailure()
	b.RecordFailure()
	if b.State() != BreakerOpen {
		t.Fatalf("want open, got %s", b.State())
	}

	// Before the cooldown: still open, calls rejected.
	clk.Advance(29 * time.Second)
	if b.Allow() {
		t.Fatal("open breaker must reject before the cooldown elapses")
	}

	// Exactly at the cooldown: transition to half-open, one probe allowed.
	clk.Advance(1 * time.Second)
	if !b.Allow() {
		t.Fatal("after the cooldown the breaker must admit a half-open probe")
	}
	if b.State() != BreakerHalfOpen {
		t.Fatalf("want half-open, got %s", b.State())
	}
	// Second concurrent call during the probe is rejected.
	if b.Allow() {
		t.Fatal("half-open must admit only one probe at a time")
	}

	// First probe success: still half-open (needs 2).
	b.RecordSuccess()
	if b.State() != BreakerHalfOpen {
		t.Fatalf("one of two probe successes must stay half-open, got %s", b.State())
	}
	// Second probe.
	if !b.Allow() {
		t.Fatal("half-open must admit the next probe after the first completed")
	}
	b.RecordSuccess()
	if b.State() != BreakerClosed {
		t.Fatalf("two probe successes must close the breaker, got %s", b.State())
	}
	if !b.Allow() {
		t.Fatal("closed breaker must allow again")
	}
	snap := b.Snapshot()
	if snap.Transitions != 3 { // open -> half-open -> closed
		t.Fatalf("Transitions = %d, want 3", snap.Transitions)
	}
	if snap.ConsecutiveFailures != 0 || snap.HalfOpenSuccesses != 0 {
		t.Fatalf("counters must reset on close: %+v", snap)
	}
}

func TestCircuitBreakerProbeFailureReopens(t *testing.T) {
	clk := newFakeClock()
	b := NewCircuitBreaker(CircuitBreakerConfig{FailureThreshold: 2, Cooldown: 30 * time.Second, HalfOpenSuccesses: 2, Clock: clk})

	b.RecordFailure()
	b.RecordFailure()
	clk.Advance(30 * time.Second)
	if !b.Allow() {
		t.Fatal("cooldown elapsed: probe expected")
	}
	b.RecordFailure() // probe fails -> immediate re-open
	if b.State() != BreakerOpen {
		t.Fatalf("probe failure must re-open, got %s", b.State())
	}
	if b.Allow() {
		t.Fatal("re-opened breaker must reject")
	}
	// The cooldown restarted from the re-open moment.
	clk.Advance(15 * time.Second)
	if b.Allow() {
		t.Fatal("re-open restarts the cooldown")
	}
	clk.Advance(15 * time.Second)
	if !b.Allow() {
		t.Fatal("full new cooldown must admit a probe again")
	}
	if b.State() != BreakerHalfOpen {
		t.Fatalf("want half-open after the new cooldown, got %s", b.State())
	}
	if s := b.Snapshot(); s.OpenCount != 2 {
		t.Fatalf("OpenCount = %d, want 2 (initial + probe-failure re-open)", s.OpenCount)
	}
}

func TestCircuitBreakerFailureWhileOpenKeepsCooldown(t *testing.T) {
	clk := newFakeClock()
	b := NewCircuitBreaker(CircuitBreakerConfig{FailureThreshold: 1, Cooldown: time.Minute, Clock: clk})

	b.RecordFailure()
	openedAt := b.Snapshot().OpenedAt
	clk.Advance(10 * time.Second)
	b.RecordFailure() // failures while open must not extend the cooldown
	if s := b.Snapshot(); !s.OpenedAt.Equal(openedAt) {
		t.Fatalf("OpenedAt moved from %s to %s; failures while open must not restart the cooldown", openedAt, s.OpenedAt)
	}
	clk.Advance(50 * time.Second) // 60s since openedAt
	if !b.Allow() {
		t.Fatal("cooldown measured from the original open must admit a probe now")
	}
}
