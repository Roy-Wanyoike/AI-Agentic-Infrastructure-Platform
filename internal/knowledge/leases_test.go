package knowledge

// leases_test.go — graph lifecycle coordination tests (issue #135).
// Concurrency scenarios are deterministic: they synchronize on the manager's
// observable LeaseState and on channels, never on sleeps-as-prediction (the
// only bounded waits assert a NEGATIVE — that an admission cannot happen
// while the state forbids it — or poll "eventually" with a hard cap).

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"
)

// waitFor polls pred until it holds or the cap expires (deterministic
// "eventually" with a hard bound, never a sleep-as-prediction).
func waitFor(t *testing.T, pred func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if pred() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition never became true: %s", msg)
}

// mustRecv receives one value or fails after the cap (catches lost
// wakeups deterministically).
func mustRecv[T any](t *testing.T, ch <-chan T, msg string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for: %s", msg)
		var zero T
		return zero
	}
}

// mustNotRecv asserts nothing arrives within the window. For lease
// admission this is an INVARIANT (the state forbids admission), not a
// timing guess.
func mustNotRecv[T any](t *testing.T, ch <-chan T, msg string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatalf("unexpected event: %s", msg)
	case <-time.After(60 * time.Millisecond):
	}
}

func TestGraphKeyIsOrgScoped(t *testing.T) {
	a := GraphKey("org-1", "kb-1")
	if a != GraphKey("org-1", "kb-1") {
		t.Fatal("GraphKey must be deterministic for the same (org, kb)")
	}
	if a == GraphKey("org-1", "kb-2") || a == GraphKey("org-2", "kb-1") || GraphKey("org-2", "kb-1") == GraphKey("org-2", "kb-2") {
		t.Fatalf("org-scoped keys collide: %q", a)
	}
	if GraphKey("org-a", "kb-x") == GraphKey("org-a/kb-x", "") {
		t.Fatal("labeled key must not collapse across org/kb boundaries")
	}
}

func TestConcurrentReadersBlockWriterUntilRelease(t *testing.T) {
	m := NewGraphLifecycleManager()
	key := GraphKey("org-1", "kb-1")
	ctx := context.Background()

	r1, err := m.AcquireRead(ctx, key)
	if err != nil {
		t.Fatalf("AcquireRead(1): %v", err)
	}
	defer r1.Release()
	r2, err := m.AcquireRead(ctx, key)
	if err != nil {
		t.Fatalf("AcquireRead(2): %v", err)
	}
	defer r2.Release()
	if st, _ := m.State(key); st.ActiveReaders != 2 || st.WriteLocked {
		t.Fatalf("state after two readers: %+v", st)
	}

	// Writer queues behind the readers.
	type writeResult struct {
		lease *WriteLease
		err   error
	}
	wCh := make(chan writeResult, 1)
	go func() {
		lease, err := m.AcquireWrite(ctx, key, time.Now().Add(2*time.Second))
		wCh <- writeResult{lease, err}
	}()
	waitFor(t, func() bool {
		st, _ := m.State(key)
		return st.Queue == 1
	}, "writer to queue behind the readers")
	if st, _ := m.State(key); st.WriteLocked || st.ActiveReaders != 2 {
		t.Fatalf("writer must not be admitted while readers hold: %+v", st)
	}

	// Readers release -> the writer is admitted.
	r1.Release()
	r2.Release()
	res := mustRecv(t, wCh, "writer admission after readers released")
	if res.err != nil {
		t.Fatalf("AcquireWrite: %v", res.err)
	}
	if st, _ := m.State(key); !st.WriteLocked || st.ActiveReaders != 0 {
		t.Fatalf("state while writer holds: %+v", st)
	}

	// While the write is held, new readers cannot be admitted (invariant,
	// not timing).
	rCh := make(chan *ReadLease, 1)
	go func() {
		lease, err := m.AcquireRead(ctx, key)
		if err != nil {
			rCh <- nil
			return
		}
		rCh <- lease
	}()
	mustNotRecv(t, rCh, "reader admitted while write lease held")
	if st, _ := m.State(key); !st.WriteLocked {
		t.Fatalf("write lock lost unexpectedly: %+v", st)
	}

	// Writer releases -> the queued reader proceeds.
	res.lease.Release()
	reader3 := mustRecv(t, rCh, "reader admission after write released")
	if reader3 == nil {
		t.Fatal("reader admission failed after write released")
	}
	reader3.Release()
	waitFor(t, func() bool {
		st, _ := m.State(key)
		return st.ActiveReaders == 0 && !st.WriteLocked
	}, "graph to drain after all releases")
}

func TestWriterPriorityBlocksNewReaders(t *testing.T) {
	m := NewGraphLifecycleManager()
	key := GraphKey("org-1", "kb-1")
	ctx := context.Background()

	r1, err := m.AcquireRead(ctx, key)
	if err != nil {
		t.Fatalf("AcquireRead: %v", err)
	}
	defer r1.Release()

	type writeResult struct {
		lease *WriteLease
		err   error
	}
	wCh := make(chan writeResult, 1)
	go func() {
		lease, err := m.AcquireWrite(ctx, key, time.Now().Add(2*time.Second))
		wCh <- writeResult{lease, err}
	}()
	waitFor(t, func() bool {
		st, _ := m.State(key)
		return st.Queue == 1
	}, "writer to queue")

	// A reader arriving while a writer WAITS must queue behind it
	// (writer-priority: no writer starvation).
	rCh := make(chan *ReadLease, 1)
	go func() {
		lease, err := m.AcquireRead(ctx, key)
		if err != nil {
			rCh <- nil
			return
		}
		rCh <- lease
	}()
	mustNotRecv(t, rCh, "reader admitted ahead of a queued writer")

	// Reader + writer release; the writer goes first, the reader follows.
	r1.Release()
	res := mustRecv(t, wCh, "writer admission")
	if res.err != nil {
		t.Fatalf("AcquireWrite: %v", res.err)
	}
	mustNotRecv(t, rCh, "reader admitted while write lease still held")
	res.lease.Release()
	reader2 := mustRecv(t, rCh, "queued reader admission after write")
	if reader2 == nil {
		t.Fatal("queued reader admission failed")
	}
	reader2.Release()
}

func TestAcquireWriteDeadlineTimeout(t *testing.T) {
	m := NewGraphLifecycleManager()
	key := GraphKey("org-1", "kb-1")
	ctx := context.Background()

	r1, err := m.AcquireRead(ctx, key)
	if err != nil {
		t.Fatalf("AcquireRead: %v", err)
	}
	defer r1.Release()

	// Explicit deadline in the past-ish future while a reader holds.
	_, err = m.AcquireWrite(ctx, key, time.Now().Add(40*time.Millisecond))
	if !errors.Is(err, ErrWriteTimeout) {
		t.Fatalf("AcquireWrite deadline err = %v, want ErrWriteTimeout", err)
	}
	// The queue must be rolled back: the graph is left clean for readers.
	if st, _ := m.State(key); st.Queue != 0 {
		t.Fatalf("phantom waiter after timeout: %+v", st)
	}
	r2, err := m.AcquireRead(ctx, key)
	if err != nil {
		t.Fatalf("reader admission after writer timeout: %v", err)
	}
	r2.Release()

	// Zero deadline -> the manager's configured write timeout (a reader
	// must hold the graph first, otherwise the fast path grants instantly).
	small := NewGraphLifecycleManager().WithWriteTimeout(20 * time.Millisecond)
	if small.defaultWriteTimeout != 20*time.Millisecond {
		t.Fatalf("WithWriteTimeout not applied: %v", small.defaultWriteTimeout)
	}
	hold, err := small.AcquireRead(ctx, key)
	if err != nil {
		t.Fatalf("AcquireRead on small manager: %v", err)
	}
	defer hold.Release()
	if _, err := small.AcquireWrite(ctx, key, time.Time{}); !errors.Is(err, ErrWriteTimeout) {
		t.Fatalf("zero-deadline err = %v, want ErrWriteTimeout", err)
	}
	// Non-positive overrides keep the default.
	if NewGraphLifecycleManager().WithWriteTimeout(-1).defaultWriteTimeout != DefaultWriteTimeout {
		t.Fatal("negative override must keep the default")
	}
}

func TestAcquireWriteFastPathWithExpiredDeadline(t *testing.T) {
	m := NewGraphLifecycleManager()
	key := GraphKey("org-1", "kb-1")
	// Nothing holds the graph: the write is granted instantly even with an
	// already-passed deadline (the fast path precedes the wait).
	lease, err := m.AcquireWrite(context.Background(), key, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("instant AcquireWrite with expired deadline: %v", err)
	}
	if st, _ := m.State(key); !st.WriteLocked {
		t.Fatalf("state after instant write: %+v", st)
	}
	lease.Release()
}

func TestLeaseReleaseIdempotent(t *testing.T) {
	m := NewGraphLifecycleManager()
	key := GraphKey("org-1", "kb-1")
	ctx := context.Background()

	read, err := m.AcquireRead(ctx, key)
	if err != nil {
		t.Fatalf("AcquireRead: %v", err)
	}
	read.Release()
	read.Release() // double release: no-op, no panic
	// Concurrent releases: exactly one takes effect.
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			read.Release()
		}()
	}
	wg.Wait()
	if st, _ := m.State(key); st.ActiveReaders != 0 {
		t.Fatalf("readers leaked after idempotent releases: %+v", st)
	}

	write, err := m.AcquireWrite(ctx, key, time.Time{})
	if err != nil {
		t.Fatalf("AcquireWrite: %v", err)
	}
	write.Release()
	write.Release()
	if st, _ := m.State(key); st.WriteLocked {
		t.Fatalf("write lock stuck after idempotent releases: %+v", st)
	}
	// A released write lease can be taken again.
	write2, err := m.AcquireWrite(ctx, key, time.Time{})
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	write2.Release()
}

func TestReleaseIsPanicSafe(t *testing.T) {
	m := NewGraphLifecycleManager()
	key := GraphKey("org-1", "kb-1")

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected the inner panic to propagate")
			}
		}()
		lease, err := m.AcquireRead(context.Background(), key)
		if err != nil {
			t.Fatalf("AcquireRead: %v", err)
		}
		defer lease.Release() // must run during panic unwinding
		panic("boom during search")
	}()
	if st, _ := m.State(key); st.ActiveReaders != 0 {
		t.Fatalf("lease leaked across panic: %+v", st)
	}

	// The reset panic propagates through AdminResetWithLock (recovered
	// here) — and the deferred release still unwound the write lease.
	func() {
		defer func() {
			if r := recover(); r == nil || r != "boom during reset" {
				t.Fatalf("expected the reset panic to propagate, got %v", r)
			}
		}()
		_ = m.AdminResetWithLock(context.Background(), key, time.Time{}, func(context.Context) error {
			panic("boom during reset")
		})
	}()
	if st, _ := m.State(key); st.WriteLocked || st.ActiveReaders != 0 {
		t.Fatalf("write lease leaked across panic: %+v", st)
	}
}

func TestSearchWithLeaseHoldsForDuration(t *testing.T) {
	m := NewGraphLifecycleManager()
	key := GraphKey("org-1", "kb-1")
	ctx := context.Background()

	// The lease is visible to the search itself and released afterwards.
	results, err := SearchWithLease(ctx, m, key, func(context.Context) ([]SearchResult, error) {
		st, ok := m.State(key)
		if !ok || st.ActiveReaders != 1 {
			t.Fatalf("search must run under exactly one read lease: %+v ok=%v", st, ok)
		}
		return []SearchResult{{DocumentID: "doc-1"}}, nil
	})
	if err != nil {
		t.Fatalf("SearchWithLease: %v", err)
	}
	if len(results) != 1 || results[0].DocumentID != "doc-1" {
		t.Fatalf("results not passed through: %v", results)
	}
	if st, _ := m.State(key); st.ActiveReaders != 0 {
		t.Fatalf("lease not released after search: %+v", st)
	}

	// Search errors propagate; the lease is still released.
	if _, err := SearchWithLease(ctx, m, key, func(context.Context) ([]SearchResult, error) {
		return nil, errors.New("search failed")
	}); err == nil || err.Error() != "search failed" {
		t.Fatalf("search error not propagated: %v", err)
	}
	if st, _ := m.State(key); st.ActiveReaders != 0 {
		t.Fatalf("lease not released after failed search: %+v", st)
	}

	// A write waits for the in-flight search and starts afterwards.
	type writeResult struct {
		lease *WriteLease
		err   error
	}
	wCh := make(chan writeResult, 1)
	searchGate := make(chan struct{})
	searchRunning := make(chan struct{})
	go func() {
		_, _ = SearchWithLease(ctx, m, key, func(context.Context) ([]SearchResult, error) {
			close(searchRunning) // the read lease is held from here
			<-searchGate         // hold it until the writer has queued
			return nil, nil
		})
	}()
	<-searchRunning
	go func() {
		lease, err := m.AcquireWrite(ctx, key, time.Now().Add(2*time.Second))
		wCh <- writeResult{lease, err}
	}()
	waitFor(t, func() bool {
		st, _ := m.State(key)
		return st.Queue == 1
	}, "writer to queue behind the in-flight search")
	if st, _ := m.State(key); st.WriteLocked {
		t.Fatal("writer admitted while the search held its read lease")
	}
	close(searchGate) // search releases -> writer admitted
	res := mustRecv(t, wCh, "writer admission after the search released")
	if res.err != nil {
		t.Fatalf("writer admitted during search: %v", res.err)
	}
	if res.lease != nil {
		res.lease.Release()
	}
}

func TestAdminResetWithLockIsExclusive(t *testing.T) {
	m := NewGraphLifecycleManager()
	key := GraphKey("org-1", "kb-1")
	ctx := context.Background()

	err := m.AdminResetWithLock(ctx, key, time.Time{}, func(context.Context) error {
		st, ok := m.State(key)
		if !ok || !st.WriteLocked || st.ActiveReaders != 0 {
			t.Fatalf("reset must run under the exclusive write lease: %+v", st)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("AdminResetWithLock: %v", err)
	}
	if st, _ := m.State(key); st.WriteLocked {
		t.Fatalf("write lease not released after reset: %+v", st)
	}

	// Reset failures surface verbatim.
	if err := m.AdminResetWithLock(ctx, key, time.Time{}, func(context.Context) error {
		return errors.New("reset failed")
	}); err == nil || err.Error() != "reset failed" {
		t.Fatalf("reset error not propagated: %v", err)
	}

	// While the reset holds the write lease, a search blocks (and proceeds
	// after the reset completed).
	blockReset := make(chan struct{})
	resetDone := make(chan error, 1)
	go func() {
		resetDone <- m.AdminResetWithLock(ctx, key, time.Now().Add(2*time.Second), func(context.Context) error {
			<-blockReset
			return nil
		})
	}()
	waitFor(t, func() bool {
		st, _ := m.State(key)
		return st.WriteLocked
	}, "reset to hold the write lease")
	searchDone := make(chan error, 1)
	go func() {
		_, err := SearchWithLease(ctx, m, key, func(context.Context) ([]SearchResult, error) {
			return nil, nil
		})
		searchDone <- err
	}()
	mustNotRecv(t, searchDone, "search admitted during reset")
	close(blockReset)
	if err := mustRecv(t, resetDone, "reset completion"); err != nil {
		t.Fatalf("reset failed: %v", err)
	}
	if err := mustRecv(t, searchDone, "search admission after reset"); err != nil {
		t.Fatalf("search after reset failed: %v", err)
	}
}

func TestAcquireHonorsContextCancellation(t *testing.T) {
	m := NewGraphLifecycleManager()
	key := GraphKey("org-1", "kb-1")
	ctx := context.Background()

	write, err := m.AcquireWrite(ctx, key, time.Time{})
	if err != nil {
		t.Fatalf("AcquireWrite: %v", err)
	}
	defer write.Release()

	// A cancelled reader cannot be admitted while the write is held.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := m.AcquireRead(cancelled, key); !errors.Is(err, context.Canceled) {
		t.Fatalf("AcquireRead(cancelled) = %v, want context.Canceled", err)
	}

	// A reader holding the graph: a cancelled writer must not be admitted.
	write.Release()
	read, err := m.AcquireRead(ctx, key)
	if err != nil {
		t.Fatalf("AcquireRead: %v", err)
	}
	defer read.Release()
	if _, err := m.AcquireWrite(cancelled, key, time.Now().Add(time.Second)); !errors.Is(err, context.Canceled) {
		t.Fatalf("AcquireWrite(cancelled) = %v, want context.Canceled", err)
	}
	if st, _ := m.State(key); st.Queue != 0 {
		t.Fatalf("phantom waiter after cancellation: %+v", st)
	}
}

func TestLifecycleManagerValidation(t *testing.T) {
	m := NewGraphLifecycleManager()
	ctx := context.Background()
	if _, err := m.AcquireRead(ctx, ""); !errors.Is(err, ErrGraphKeyRequired) {
		t.Fatalf("empty key AcquireRead = %v, want ErrGraphKeyRequired", err)
	}
	if _, err := m.AcquireWrite(ctx, "", time.Time{}); !errors.Is(err, ErrGraphKeyRequired) {
		t.Fatalf("empty key AcquireWrite = %v, want ErrGraphKeyRequired", err)
	}
	var nilManager *GraphLifecycleManager
	if _, err := nilManager.AcquireRead(ctx, GraphKey("o", "k")); !errors.Is(err, ErrNilLifecycleManager) {
		t.Fatalf("nil manager AcquireRead = %v, want ErrNilLifecycleManager", err)
	}
	if _, ok := nilManager.State(GraphKey("o", "k")); ok {
		t.Fatal("nil manager State must report unknown")
	}
	if _, err := NewGraphLifecycleManager().AcquireRead(ctx, GraphKey("o", "k")); err != nil {
		t.Fatalf("unknown key lazily creates state: %v", err)
	}
}

func TestLeaseStatesSnapshot(t *testing.T) {
	m := NewGraphLifecycleManager()
	ctx := context.Background()
	keyB := GraphKey("org-2", "kb-1")
	keyA := GraphKey("org-1", "kb-1")

	read, err := m.AcquireRead(ctx, keyB)
	if err != nil {
		t.Fatalf("AcquireRead: %v", err)
	}
	defer read.Release()
	write, err := m.AcquireWrite(ctx, keyA, time.Time{})
	if err != nil {
		t.Fatalf("AcquireWrite: %v", err)
	}
	defer write.Release()

	states := m.States()
	if len(states) != 2 {
		t.Fatalf("states = %d, want 2", len(states))
	}
	// Deterministic order: sorted by graph key.
	if states[0].GraphKey != keyA || states[1].GraphKey != keyB {
		t.Fatalf("states not sorted: %+v", states)
	}
	if !states[0].WriteLocked || states[0].ActiveReaders != 0 {
		t.Fatalf("org-1 state wrong: %+v", states[0])
	}
	if states[1].ActiveReaders != 1 || states[1].WriteLocked {
		t.Fatalf("org-2 state wrong: %+v", states[1])
	}
	// Unknown key: ok=false.
	if _, ok := m.State(GraphKey("org-9", "kb-9")); ok {
		t.Fatal("unknown key must report ok=false")
	}
}

func TestGraphLifecycleManagerNoGoroutineLeak(t *testing.T) {
	before := runtime.NumGoroutine()
	m := NewGraphLifecycleManager().WithWriteTimeout(10 * time.Millisecond)
	ctx := context.Background()
	keys := []string{GraphKey("org-1", "kb-1"), GraphKey("org-1", "kb-2")}

	for round := 0; round < 20; round++ {
		for _, key := range keys {
			read, err := m.AcquireRead(ctx, key)
			if err != nil {
				t.Fatalf("AcquireRead: %v", err)
			}
			// Writers time out against the held reader (typed error path).
			if _, err := m.AcquireWrite(ctx, key, time.Now().Add(time.Millisecond)); !errors.Is(err, ErrWriteTimeout) {
				t.Fatalf("expected ErrWriteTimeout, got %v", err)
			}
			read.Release()
			// Successful write cycle.
			write, err := m.AcquireWrite(ctx, key, time.Time{})
			if err != nil {
				t.Fatalf("AcquireWrite: %v", err)
			}
			write.Release()
		}
	}
	// Timers from the expired deadlines need one tick to unwind; settle.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("goroutine leak: before=%d after=%d", before, runtime.NumGoroutine())
}
