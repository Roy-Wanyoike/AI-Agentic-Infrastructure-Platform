package knowledge

// leases.go — graph lifecycle coordination (issue #135): read leases +
// exclusive write locks per knowledge-graph lifecycle key.
//
// Write operations on a knowledge graph (reset, re-ingest, schema change)
// must be EXCLUSIVE: they run only when no reader holds a lease, and while
// they run no new reader may enter. Read operations (search, run-scoped
// retrieval) hold a READ LEASE for their duration so a writer cannot tear
// the graph out from under them.
//
// Scope: ONE graph = one (organization, knowledge base) pair. Callers build
// the org-scoped key with GraphKey — two tenants can never collide on (or
// reach into) each other's lock state. Single-resource semantics: there is
// no lock ordering to reason about; the ONLY deadlock hazard is a write
// that waits forever for readers, which the configurable write deadline
// (typed ErrWriteTimeout) eliminates.
//
// Semantics (all documented on the methods):
//   - writer-priority: readers arriving while a writer is WAITING block
//     behind it (no writer starvation); readers already holding leases are
//     never interrupted.
//   - leases are NOT re-entrant: a second AcquireRead by the same caller
//     returns an independent lease (release both). Nesting AcquireRead
//     inside a read section while a writer is queued will block on the
//     writer-priority rule — hold ONE lease for the whole read duration
//     instead (SearchWithLease).
//   - Release is idempotent and panic-safe: call it via defer; double and
//     concurrent releases are no-ops, and a panic between acquire and
//     release still releases (defer runs during unwinding).
//
// Integration seams: SearchWithLease wraps the read path (acquire/release
// around one search), AdminResetWithLock wraps write operations. The
// runtime/workflow "hold-for-duration" wiring (a run holding its read lease
// across tool calls) is deliberate follow-up work in internal/runtime —
// this file only ships the coordination primitive and its search/reset
// seams; internal/runtime is NOT modified here.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Lease errors surfaced to callers for status mapping.
var (
	// ErrWriteTimeout is returned by AcquireWrite when the write could not
	// start before the deadline (readers had not released in time).
	ErrWriteTimeout = errors.New("knowledge: write lease deadline exceeded waiting for active readers")
	// ErrGraphKeyRequired is returned when the lifecycle key is empty (keys
	// are org-scoped; an empty key cannot identify a tenant's graph).
	ErrGraphKeyRequired = errors.New("knowledge: graph lifecycle key is required")
	// ErrNilLifecycleManager is returned by lease operations on a nil
	// manager (fail closed instead of panicking).
	ErrNilLifecycleManager = errors.New("knowledge: graph lifecycle manager is nil")
)

// DefaultWriteTimeout is the write deadline used when AcquireWrite receives
// a zero deadline and no manager-level override was configured.
const DefaultWriteTimeout = 30 * time.Second

// GraphKey returns the org-scoped lifecycle key of one knowledge base
// graph: "org/<orgID>/kb/<kbID>". EVERY lease call must go through this
// helper: the org/kb labels keep tenant keys disjoint, so one organization
// can never block or observe another's graph state. The platform's UUID
// org/kb identifiers never contain the "/" separator.
func GraphKey(orgID, kbID string) string {
	return "org/" + orgID + "/kb/" + kbID
}

// LeaseState is a point-in-time snapshot of one graph's lifecycle state
// (observability surface; the api/fragments/retrieval-tools.yaml LeaseState
// schema mirrors it).
type LeaseState struct {
	// GraphKey is the org-scoped lifecycle key (GraphKey format).
	GraphKey string `json:"graph_key"`
	// ActiveReaders is the number of read leases currently held.
	ActiveReaders int `json:"active_readers"`
	// WriteLocked reports whether the exclusive write lease is held.
	WriteLocked bool `json:"write_locked"`
	// Queue is the number of writers waiting for readers to release.
	Queue int `json:"queue"`
}

// graphLock is the per-graph lock state, guarded by its own mutex. The
// changed channel is a broadcast: every state transition closes it (and
// installs a fresh one), waking all waiters to re-check the predicate.
type graphLock struct {
	mu      sync.Mutex
	readers int           // active read leases
	writing bool          // exclusive write lease held
	waiting int           // writers queued for the readers to release
	changed chan struct{} // closed + replaced on every transition
}

// broadcast wakes every waiter (callers must hold g.mu).
func (g *graphLock) broadcast() {
	close(g.changed)
	g.changed = make(chan struct{})
}

// GraphLifecycleManager coordinates exclusive write operations against
// concurrent read operations per org-scoped graph key. Zero-value safe to
// construct only via NewGraphLifecycleManager. The manager spawns no
// background goroutines (mutex + broadcast channels only), so it cannot
// leak by itself.
type GraphLifecycleManager struct {
	// defaultWriteTimeout applies when AcquireWrite's deadline is zero.
	defaultWriteTimeout time.Duration

	mu     sync.Mutex
	graphs map[string]*graphLock
}

// NewGraphLifecycleManager returns a manager with the default write timeout
// (DefaultWriteTimeout); override it with WithWriteTimeout.
func NewGraphLifecycleManager() *GraphLifecycleManager {
	return &GraphLifecycleManager{
		defaultWriteTimeout: DefaultWriteTimeout,
		graphs:              make(map[string]*graphLock),
	}
}

// WithWriteTimeout overrides the deadline applied when AcquireWrite is
// called with a zero deadline. Non-positive values keep the default.
func (m *GraphLifecycleManager) WithWriteTimeout(d time.Duration) *GraphLifecycleManager {
	if d > 0 {
		m.defaultWriteTimeout = d
	}
	return m
}

// graphFor returns (creating if needed) the lock state of one key.
func (m *GraphLifecycleManager) graphFor(key string) *graphLock {
	m.mu.Lock()
	defer m.mu.Unlock()
	g, ok := m.graphs[key]
	if !ok {
		g = &graphLock{changed: make(chan struct{})}
		m.graphs[key] = g
	}
	return g
}

// writeDelay resolves the effective wait budget for a write: the manager
// default for a zero deadline, otherwise time until the deadline (an
// already-passed deadline yields a non-positive delay, so the timeout fires
// immediately — after the instant-acquire fast path is still tried).
func (m *GraphLifecycleManager) writeDelay(deadline time.Time) time.Duration {
	if deadline.IsZero() {
		return m.defaultWriteTimeout
	}
	return time.Until(deadline)
}

// ReadLease is one held read permission on a graph. Hold it for the whole
// read (search/run) duration; release exactly once via Release (idempotent,
// panic-safe — the canonical pattern is `defer lease.Release()`).
type ReadLease struct {
	manager  *GraphLifecycleManager
	key      string
	released atomic.Bool
}

// Release relinquishes the read lease. Idempotent and safe under
// concurrent Release calls (exactly the first call takes effect) and
// panic-safe (a deferred Release still runs during panic unwinding).
func (l *ReadLease) Release() {
	if l == nil || l.manager == nil || !l.released.CompareAndSwap(false, true) {
		return // already released (or never acquired)
	}
	g := l.manager.graphFor(l.key)
	g.mu.Lock()
	if g.readers > 0 {
		g.readers--
	}
	g.broadcast()
	g.mu.Unlock()
}

// WriteLease is the exclusive write permission on a graph (reset,
// re-ingest, schema change). No reader can acquire while it is held, and it
// is only granted once all readers released.
type WriteLease struct {
	manager  *GraphLifecycleManager
	key      string
	released atomic.Bool
}

// Release relinquishes the write lease. Idempotent and panic-safe (same
// contract as ReadLease.Release).
func (l *WriteLease) Release() {
	if l == nil || l.manager == nil || !l.released.CompareAndSwap(false, true) {
		return
	}
	g := l.manager.graphFor(l.key)
	g.mu.Lock()
	g.writing = false
	g.broadcast()
	g.mu.Unlock()
}

// AcquireRead takes a read lease on the graph. It blocks while a write is
// held OR a writer is waiting (writer-priority: queued writers are never
// starved), and honors ctx cancellation. NOT re-entrant — see the package
// comment; hold one lease for the whole read duration (SearchWithLease).
func (m *GraphLifecycleManager) AcquireRead(ctx context.Context, graphKey string) (*ReadLease, error) {
	if m == nil {
		return nil, ErrNilLifecycleManager
	}
	if graphKey == "" {
		return nil, ErrGraphKeyRequired
	}
	g := m.graphFor(graphKey)
	g.mu.Lock()
	for g.writing || g.waiting > 0 {
		ch := g.changed
		g.mu.Unlock()
		select {
		case <-ch: // a transition happened; re-check the predicate
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		g.mu.Lock()
	}
	g.readers++
	g.mu.Unlock()
	return &ReadLease{manager: m, key: graphKey}, nil
}

// AcquireWrite takes the exclusive write lease on the graph, blocking until
// every active reader has released. Writer-priority admission means readers
// arriving while this call waits queue behind it. The wait is bounded by
// the deadline (a zero deadline uses the manager's configured write
// timeout) and by ctx cancellation; a deadline exceeded while readers are
// still active returns the typed ErrWriteTimeout — queue state is rolled
// back so the graph is never left with a phantom waiter. An instantly
// available write lock is granted even with an already-passed deadline (the
// fast path precedes the wait).
func (m *GraphLifecycleManager) AcquireWrite(ctx context.Context, graphKey string, deadline time.Time) (*WriteLease, error) {
	if m == nil {
		return nil, ErrNilLifecycleManager
	}
	if graphKey == "" {
		return nil, ErrGraphKeyRequired
	}
	g := m.graphFor(graphKey)

	// Fast path: nobody home — take the lock immediately.
	g.mu.Lock()
	if !g.writing && g.readers == 0 {
		g.writing = true
		g.broadcast()
		g.mu.Unlock()
		return &WriteLease{manager: m, key: graphKey}, nil
	}
	// Queue as a waiting writer (readers arriving afterwards block on the
	// writer-priority rule).
	g.waiting++
	ch := g.changed
	g.mu.Unlock()

	timer := time.NewTimer(m.writeDelay(deadline))
	defer timer.Stop()

	for {
		select {
		case <-ch: // a transition happened; re-check the predicate
			g.mu.Lock()
			if !g.writing && g.readers == 0 {
				g.writing = true
				g.waiting--
				g.broadcast()
				g.mu.Unlock()
				return &WriteLease{manager: m, key: graphKey}, nil
			}
			ch = g.changed
			g.mu.Unlock()
		case <-timer.C:
			g.mu.Lock()
			g.waiting--
			g.broadcast() // waiters count changed; wake queued readers
			g.mu.Unlock()
			return nil, fmt.Errorf("%w: graph %s", ErrWriteTimeout, graphKey)
		case <-ctx.Done():
			g.mu.Lock()
			g.waiting--
			g.broadcast()
			g.mu.Unlock()
			return nil, ctx.Err()
		}
	}
}

// State snapshots one graph's lifecycle state (ok=false for unknown keys).
func (m *GraphLifecycleManager) State(graphKey string) (LeaseState, bool) {
	if m == nil {
		return LeaseState{}, false
	}
	m.mu.Lock()
	g, ok := m.graphs[graphKey]
	m.mu.Unlock()
	if !ok {
		return LeaseState{}, false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return LeaseState{
		GraphKey:      graphKey,
		ActiveReaders: g.readers,
		WriteLocked:   g.writing,
		Queue:         g.waiting,
	}, true
}

// States snapshots every tracked graph's state, sorted by graph key
// (deterministic for callers rendering or diffing the snapshot).
func (m *GraphLifecycleManager) States() []LeaseState {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	pairs := make([]struct {
		key string
		g   *graphLock
	}, 0, len(m.graphs))
	for key, g := range m.graphs {
		pairs = append(pairs, struct {
			key string
			g   *graphLock
		}{key, g})
	}
	m.mu.Unlock()
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].key < pairs[j].key })
	out := make([]LeaseState, 0, len(pairs))
	for _, p := range pairs {
		p.g.mu.Lock()
		out = append(out, LeaseState{
			GraphKey:      p.key,
			ActiveReaders: p.g.readers,
			WriteLocked:   p.g.writing,
			Queue:         p.g.waiting,
		})
		p.g.mu.Unlock()
	}
	return out
}

// ---------------------------------------------------------------------------
// Integration seams
// ---------------------------------------------------------------------------

// WithReadLease runs fn while holding a read lease on the graph: acquire ->
// fn(ctx) -> release, with the release deferred so a panic inside fn still
// releases the lease before propagating. This is the hold-for-duration
// pattern reads use; nested AcquireRead calls are NOT re-entrant and must
// not be made inside fn while writers may be waiting.
func (m *GraphLifecycleManager) WithReadLease(ctx context.Context, graphKey string, fn func(context.Context) error) error {
	lease, err := m.AcquireRead(ctx, graphKey)
	if err != nil {
		return err
	}
	defer lease.Release() // panic-safe: releases during unwinding too
	return fn(ctx)
}

// SearchWithLease is the read-path seam (issue #135): it holds a read lease
// for the WHOLE search duration — acquire before the first candidate read,
// release after the results are out of the graph — so a concurrent reset /
// re-ingest / schema change can never run under an in-flight search. The
// generic result type keeps it wrapper-free for every search shape
// ([]SearchResult, a page of chunks, …). (A package-level function because
// Go methods cannot carry type parameters.)
func SearchWithLease[R any](ctx context.Context, m *GraphLifecycleManager, graphKey string, search func(context.Context) (R, error)) (R, error) {
	var (
		zero R
		out  R
	)
	err := m.WithReadLease(ctx, graphKey, func(c context.Context) error {
		var innerErr error
		out, innerErr = search(c)
		return innerErr
	})
	if err != nil {
		return zero, err
	}
	return out, nil
}

// AdminResetWithLock is the write-path seam (issue #135): it holds the
// exclusive write lease for the duration of one administrative write
// (reset, re-ingest, schema change) — the write starts only after every
// reader released, and no reader can enter until it completes. The deadline
// bounds the wait for readers (typed ErrWriteTimeout on expiry); zero
// selects the manager's configured write timeout.
func (m *GraphLifecycleManager) AdminResetWithLock(ctx context.Context, graphKey string, deadline time.Time, reset func(context.Context) error) error {
	lease, err := m.AcquireWrite(ctx, graphKey, deadline)
	if err != nil {
		return err
	}
	defer lease.Release() // panic-safe: releases during unwinding too
	return reset(ctx)
}
