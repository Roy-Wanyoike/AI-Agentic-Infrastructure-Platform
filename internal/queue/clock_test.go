package queue

import (
	"context"
	"testing"
	"time"
)

// clock_test.go covers the injected-clock contract the messaging layer relies
// on (issue #116): deterministic FakeClock advancement, timer ordering,
// Stop-before-deadline, and ctx-aware Sleep. No real-time sleeps drive
// correctness anywhere in this file.

func TestFakeClockAdvanceFiresDueTimersInDeadlineOrder(t *testing.T) {
	clock := NewFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	fired := make(chan string, 8)
	// Three timers with out-of-order deadlines; Advance must fire them in
	// deadline order (the FakeClock contract the retry/backoff tests lean on).
	t300 := clock.NewTimer(300 * time.Millisecond)
	t100 := clock.NewTimer(100 * time.Millisecond)
	t200 := clock.NewTimer(200 * time.Millisecond)
	go func() {
		<-t100.C()
		fired <- "100"
		<-t200.C()
		fired <- "200"
		<-t300.C()
		fired <- "300"
	}()

	if got := clock.PendingTimers(); got != 3 {
		t.Fatalf("PendingTimers before advance = %d, want 3", got)
	}
	clock.Advance(300 * time.Millisecond)

	select {
	case got := <-fired:
		if got != "100" {
			t.Fatalf("first firing = %q, want 100", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timers did not fire after Advance")
	}
	for _, want := range []string{"200", "300"} {
		select {
		case got := <-fired:
			if got != want {
				t.Fatalf("firing order = %q, want %q", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timer %q did not fire", want)
		}
	}
	if got := clock.PendingTimers(); got != 0 {
		t.Fatalf("PendingTimers after advance = %d, want 0", got)
	}
}

func TestFakeClockAdvanceOnlyFiresTimersWithinStep(t *testing.T) {
	clock := NewFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	far := clock.NewTimer(10 * time.Second)
	near := clock.NewTimer(time.Second)

	clock.Advance(500 * time.Millisecond)
	select {
	case <-near.C():
		t.Fatal("timer fired before its deadline")
	default:
	}
	if got := clock.PendingTimers(); got != 2 {
		t.Fatalf("PendingTimers = %d, want 2 (nothing due yet)", got)
	}

	clock.Advance(500 * time.Millisecond) // now = 1s: only `near` is due
	select {
	case <-near.C():
	default:
		t.Fatal("due timer did not fire")
	}
	select {
	case <-far.C():
		t.Fatal("future timer fired early")
	default:
	}
	if got := clock.PendingTimers(); got != 1 {
		t.Fatalf("PendingTimers = %d, want 1", got)
	}
}

func TestFakeClockTimerStopPreventsFiring(t *testing.T) {
	clock := NewFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	tmr := clock.NewTimer(time.Second)

	if !tmr.Stop() {
		t.Fatal("Stop on a pending timer reported false")
	}
	clock.Advance(2 * time.Second)
	select {
	case <-tmr.C():
		t.Fatal("stopped timer fired")
	default:
	}
	if tmr.Stop() {
		t.Fatal("Stop on an already-stopped timer reported true")
	}
	if got := clock.PendingTimers(); got != 0 {
		t.Fatalf("PendingTimers = %d, want 0 (stopped timer removed)", got)
	}
}

func TestFakeClockNowOnlyMovesViaAdvance(t *testing.T) {
	start := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	clock := NewFakeClock(start)
	if got := clock.Now(); !got.Equal(start) {
		t.Fatalf("Now = %v, want %v", got, start)
	}
	clock.Advance(90 * time.Second)
	if got, want := clock.Now(), start.Add(90*time.Second); !got.Equal(want) {
		t.Fatalf("Now after Advance = %v, want %v", got, want)
	}
}

func TestSleepReturnsOnTimerFire(t *testing.T) {
	clock := NewFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	done := make(chan error, 1)
	go func() { done <- Sleep(context.Background(), clock, time.Second) }()
	// Determinism: Advance only after the Sleep goroutine registered its
	// timer — advancing first would miss the firing entirely.
	deadline := time.Now().Add(2 * time.Second)
	for clock.PendingTimers() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("sleep timer was never armed")
		}
		time.Sleep(time.Millisecond)
	}
	clock.Advance(time.Second)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Sleep returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Sleep did not return after the clock advanced")
	}
}

func TestSleepReturnsOnContextCancel(t *testing.T) {
	clock := NewFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Sleep(ctx, clock, time.Hour) }()
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("Sleep error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Sleep did not return after cancellation")
	}
	clock.Advance(2 * time.Hour) // the abandoned timer must not leak a firing
}

func TestRealClockImplementsClock(t *testing.T) {
	var c Clock = RealClock()
	if c.Now().IsZero() {
		t.Fatal("RealClock.Now returned the zero instant")
	}
	tmr := c.NewTimer(time.Millisecond)
	select {
	case <-tmr.C():
	case <-time.After(2 * time.Second):
		t.Fatal("RealClock timer did not fire")
	}
}
