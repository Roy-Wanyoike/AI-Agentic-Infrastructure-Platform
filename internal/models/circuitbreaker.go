package models

// circuitbreaker.go — issue #112: per-provider circuit breaker.
//
// A CircuitBreaker guards one model provider (or, in the router, one
// candidate) so a persistently failing backend is skipped instead of
// absorbing every request's timeout budget. The classic three-state machine:
//
//	closed     — normal operation; every call is allowed. Consecutive failures
//	             are counted; reaching FailureThreshold opens the breaker.
//	open       — every call is rejected (Allow()==false) until the cooldown
//	             elapses; rejected calls are counted but never attempted.
//	half-open  — entered when the cooldown elapses; a single probe call is
//	           allowed. HalfOpenSuccesses consecutive probe successes close
//	           the breaker; a probe failure re-opens it (cooldown restarts).
//
// The clock is injectable (Clock interface) so state-machine timing is fully
// deterministic in tests — no sleeps. All methods are safe for concurrent
// use; Snapshot exposes metrics-style counters for observability.

import (
	"sync"
	"time"
)

// BreakerState is the state of a CircuitBreaker.
type BreakerState string

const (
	// BreakerClosed passes all calls and counts consecutive failures.
	BreakerClosed BreakerState = "closed"
	// BreakerOpen rejects all calls until the cooldown elapses.
	BreakerOpen BreakerState = "open"
	// BreakerHalfOpen admits a single probe call after the cooldown.
	BreakerHalfOpen BreakerState = "half-open"
)

// Circuit breaker defaults (issue #112): a provider is skipped after 5
// consecutive failures, probed again after a 30s cooldown, and re-closed
// after 2 successful probes.
const (
	DefaultFailureThreshold   = 5
	DefaultCooldown           = 30 * time.Second
	DefaultHalfOpenSuccesses  = 2
	defaultBreakerMaxFailures = 1 << 20 // sanity bound on the failure counter
)

// Clock abstracts time for the breaker so transitions are testable without
// real sleeps.
type Clock interface {
	Now() time.Time
}

// realClock is the production Clock.
type realClock struct{}

// Now implements Clock using the wall clock.
func (realClock) Now() time.Time { return time.Now() }

// CircuitBreakerConfig configures one breaker. The zero Clock means the wall
// clock; zero thresholds mean the documented defaults above.
type CircuitBreakerConfig struct {
	// FailureThreshold is the number of consecutive failures (while closed)
	// that opens the breaker. Default DefaultFailureThreshold.
	FailureThreshold int
	// Cooldown is how long the breaker stays open before admitting a
	// half-open probe. Default DefaultCooldown.
	Cooldown time.Duration
	// HalfOpenSuccesses is the number of consecutive successful probes
	// required to close the breaker from half-open. Default
	// DefaultHalfOpenSuccesses.
	HalfOpenSuccesses int
	// Clock supplies time; nil uses the wall clock.
	Clock Clock
}

// DefaultCircuitBreakerConfig returns the documented defaults with the wall
// clock.
func DefaultCircuitBreakerConfig() CircuitBreakerConfig {
	return CircuitBreakerConfig{
		FailureThreshold:  DefaultFailureThreshold,
		Cooldown:          DefaultCooldown,
		HalfOpenSuccesses: DefaultHalfOpenSuccesses,
		Clock:             realClock{},
	}
}

// BreakerSnapshot is a point-in-time, metrics-style view of one breaker.
type BreakerSnapshot struct {
	State               BreakerState
	ConsecutiveFailures int
	HalfOpenSuccesses   int
	Successes           uint64 // successful RecordSuccess calls (total)
	Failures            uint64 // recorded failures (total)
	Rejections          uint64 // calls denied because the breaker was open
	OpenCount           uint64 // closed->open transitions (total)
	Transitions         uint64 // all state transitions (total)
	OpenedAt            time.Time
	LastTransition      time.Time
}

// CircuitBreaker is the per-provider state machine described in the file
// header. Construct with NewCircuitBreaker; the zero value is not usable.
type CircuitBreaker struct {
	mu  sync.Mutex
	cfg CircuitBreakerConfig

	state               BreakerState
	consecutiveFailures int
	halfOpenSuccesses   int
	probeInFlight       bool
	openedAt            time.Time
	lastTransition      time.Time

	successes   uint64
	failures    uint64
	rejections  uint64
	openCount   uint64
	transitions uint64
}

// NewCircuitBreaker builds a breaker; zero config fields take the documented
// defaults. The breaker starts closed.
func NewCircuitBreaker(cfg CircuitBreakerConfig) *CircuitBreaker {
	if cfg.FailureThreshold <= 0 {
		cfg.FailureThreshold = DefaultFailureThreshold
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = DefaultCooldown
	}
	if cfg.HalfOpenSuccesses <= 0 {
		cfg.HalfOpenSuccesses = DefaultHalfOpenSuccesses
	}
	if cfg.Clock == nil {
		cfg.Clock = realClock{}
	}
	now := cfg.Clock.Now()
	return &CircuitBreaker{
		cfg:            cfg,
		state:          BreakerClosed,
		lastTransition: now,
	}
}

// Allow reports whether a call may proceed right now. While closed every
// call is allowed; while open every call is denied (counted as a rejection)
// until the cooldown elapses, at which point the breaker moves to half-open
// and admits one probe; while half-open only one probe may be in flight.
func (b *CircuitBreaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.cfg.Clock.Now()
	switch b.state {
	case BreakerClosed:
		return true
	case BreakerOpen:
		if now.Sub(b.openedAt) >= b.cfg.Cooldown {
			b.transitionLocked(BreakerHalfOpen, now)
			b.halfOpenSuccesses = 0
			b.probeInFlight = true
			return true
		}
		b.rejections++
		return false
	case BreakerHalfOpen:
		if b.probeInFlight {
			b.rejections++
			return false
		}
		b.probeInFlight = true
		return true
	default:
		// Unreachable; a corrupted state fails open rather than wedging
		// the provider forever.
		return true
	}
}

// RecordSuccess records one successful call. While closed it resets the
// consecutive-failure count; while half-open it counts toward closing.
func (b *CircuitBreaker) RecordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.cfg.Clock.Now()
	b.successes++
	switch b.state {
	case BreakerClosed:
		b.consecutiveFailures = 0
	case BreakerHalfOpen:
		b.halfOpenSuccesses++
		b.probeInFlight = false
		if b.halfOpenSuccesses >= b.cfg.HalfOpenSuccesses {
			b.transitionLocked(BreakerClosed, now)
			b.consecutiveFailures = 0
			b.halfOpenSuccesses = 0
		}
	}
}

// RecordFailure records one failed call. While closed it counts toward the
// open threshold; while half-open it immediately re-opens the breaker (the
// cooldown restarts). Failures recorded while open keep the breaker open
// without extending its cooldown.
func (b *CircuitBreaker) RecordFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.cfg.Clock.Now()
	b.failures++
	if b.consecutiveFailures < defaultBreakerMaxFailures {
		b.consecutiveFailures++
	}
	switch b.state {
	case BreakerClosed:
		if b.consecutiveFailures >= b.cfg.FailureThreshold {
			b.openedAt = now
			b.transitionLocked(BreakerOpen, now)
		}
	case BreakerHalfOpen:
		b.openedAt = now
		b.halfOpenSuccesses = 0
		b.probeInFlight = false
		b.transitionLocked(BreakerOpen, now)
	}
}

// State returns the current state.
func (b *CircuitBreaker) State() BreakerState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// Snapshot returns a consistent metrics-style view of the breaker.
func (b *CircuitBreaker) Snapshot() BreakerSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	return BreakerSnapshot{
		State:               b.state,
		ConsecutiveFailures: b.consecutiveFailures,
		HalfOpenSuccesses:   b.halfOpenSuccesses,
		Successes:           b.successes,
		Failures:            b.failures,
		Rejections:          b.rejections,
		OpenCount:           b.openCount,
		Transitions:         b.transitions,
		OpenedAt:            b.openedAt,
		LastTransition:      b.lastTransition,
	}
}

// transitionLocked moves state and bookkeeping forward; the caller holds mu.
func (b *CircuitBreaker) transitionLocked(next BreakerState, now time.Time) {
	if b.state == next {
		return
	}
	if next == BreakerOpen {
		b.openCount++
	}
	b.state = next
	b.transitions++
	b.lastTransition = now
}
