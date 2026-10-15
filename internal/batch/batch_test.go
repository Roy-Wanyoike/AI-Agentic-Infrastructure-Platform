package batch

// batch_test.go — issue #123 service contract, exercised WITHOUT flaky sleeps:
// the concurrency pool is verified with deterministic start-barriers (units
// block until the test releases them, so "exactly max_concurrency running" is
// observed, never raced), plus partial-failure isolation, cancel semantics
// (in-flight ctx + not-yet-started items), capped per-org history, per-item
// error recording, atomic validation (no record on invalid submission), the
// additive batch.updated SSE stream, audit rows and org isolation.

import (
	"context"
	"errors"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentos/internal/audit"
	"agentos/internal/streaming"
)

// countingStore wraps MemoryStore to observe Put calls (atomic-validation
// assertions).
type countingStore struct {
	*MemoryStore
	mu   sync.Mutex
	puts int
}

func (c *countingStore) Put(b *Batch) error {
	c.mu.Lock()
	c.puts++
	c.mu.Unlock()
	return c.MemoryStore.Put(b)
}

func (c *countingStore) putCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.puts
}

// gate is a deterministic barrier: units signal `started` and block until
// `release` closes (or their ctx fires).
func gate(started chan<- int, release <-chan struct{}, run func()) func(ctx context.Context) (map[string]any, error) {
	return func(ctx context.Context) (map[string]any, error) {
		run()
		select {
		case <-release:
			return map[string]any{"ok": true}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// waitUntil spins (no sleeps, deterministic observation of barrier points
// the pool guarantees) until cond holds or the deadline passes.
func waitUntil(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timeout waiting for " + msg)
		}
		runtime.Gosched()
	}
}

func TestBatchPoolRespectsMaxConcurrency(t *testing.T) {
	const (
		poolSize = 2
		total    = 6
	)
	svc := NewServiceWithOptions(Options{DefaultConcurrency: poolSize})

	var (
		mu        sync.Mutex
		inFlight  int
		maxInFlt  int
		violation string
	)
	release := make(chan struct{})
	units := make([]Unit, total)
	for i := range units {
		units[i] = Unit{Ref: strconv.Itoa(i), Run: func(ctx context.Context) (map[string]any, error) {
			mu.Lock()
			inFlight++
			if inFlight > maxInFlt {
				maxInFlt = inFlight
			}
			if inFlight > poolSize && violation == "" {
				violation = "more than max_concurrency units ran concurrently"
			}
			cur := inFlight
			mu.Unlock()
			<-release
			mu.Lock()
			inFlight--
			mu.Unlock()
			return map[string]any{"idx": cur}, nil
		}}
	}

	b, err := svc.Submit(context.Background(), "org-1", TypeRuns, units, SubmitOptions{MaxConcurrency: poolSize})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if b.MaxConcurrency != poolSize || b.Total != total || b.Status != StatusRunning {
		t.Fatalf("submitted snapshot wrong: %+v", b)
	}

	// Deterministic barrier: the pool can never run more than 2 units, so
	// in-flight reaches EXACTLY 2 and stays there while the test holds the
	// release. If the pool were broken, in-flight would blow past 2 and the
	// equality below can never hold.
	waitUntil(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return inFlight == poolSize
	}, "pool to fill to max_concurrency")

	mu.Lock()
	if maxInFlt != poolSize || violation != "" {
		bad := violation
		mu.Unlock()
		t.Fatalf("pool violated max_concurrency: max=%d violation=%q", maxInFlt, bad)
	}
	mu.Unlock()

	close(release)
	final, err := svc.Wait(context.Background(), "org-1", b.ID)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if final.Status != StatusCompleted || !final.IsTerminal() {
		t.Fatalf("batch status = %q, want completed", final.Status)
	}
	if sum := final.Summary(); sum.Succeeded != total || sum.Failed != 0 || sum.Cancelled != 0 {
		t.Fatalf("summary wrong: %+v", sum)
	}
	for _, it := range final.Items {
		if it.Status != ItemSucceeded || it.Result == nil {
			t.Fatalf("item %q not settled as succeeded: %+v", it.Ref, it)
		}
	}
}

func TestBatchConcurrencyClamping(t *testing.T) {
	svc := NewServiceWithOptions(Options{})
	trivial := []Unit{{Ref: "a", Run: func(context.Context) (map[string]any, error) { return nil, nil }}}
	cases := []struct {
		requested int
		want      int
	}{
		{requested: 0, want: DefaultConcurrency},
		{requested: 1000, want: MaxConcurrencyCap},
		{requested: 1, want: 1},
		{requested: 5, want: 5},
	}
	for _, tc := range cases {
		b, err := svc.Submit(context.Background(), "org-1", TypeRuns, trivial, SubmitOptions{MaxConcurrency: tc.requested})
		if err != nil {
			t.Fatalf("submit(%d): %v", tc.requested, err)
		}
		if b.MaxConcurrency != tc.want {
			t.Fatalf("requested %d -> pool %d, want %d", tc.requested, b.MaxConcurrency, tc.want)
		}
		if _, err := svc.Wait(context.Background(), "org-1", b.ID); err != nil {
			t.Fatalf("wait: %v", err)
		}
	}
}

func TestBatchPartialFailureIsolation(t *testing.T) {
	svc := NewServiceWithOptions(Options{})
	failAt := map[int]string{1: "boom-1", 3: "boom-3"}
	units := make([]Unit, 4)
	for i := range units {
		i := i
		units[i] = Unit{Ref: "item-" + strconv.Itoa(i), Run: func(context.Context) (map[string]any, error) {
			if msg, bad := failAt[i]; bad {
				return nil, errors.New(msg)
			}
			return map[string]any{"run_id": "run-" + strconv.Itoa(i)}, nil
		}}
	}

	b, err := svc.Submit(context.Background(), "org-1", TypeRuns, units, SubmitOptions{})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	final, err := svc.Wait(context.Background(), "org-1", b.ID)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	// One failing item never aborts the batch: status is completed with the
	// per-item rollup.
	if final.Status != StatusCompleted {
		t.Fatalf("status = %q, want completed (isolation)", final.Status)
	}
	sum := final.Summary()
	if sum.Total != 4 || sum.Succeeded != 2 || sum.Failed != 2 || sum.Cancelled != 0 {
		t.Fatalf("summary wrong: %+v", sum)
	}
	for i, want := range failAt {
		it := final.Items[i]
		if it.Status != ItemFailed {
			t.Fatalf("item %d status = %q, want failed", i, it.Status)
		}
		if it.Error != want {
			t.Fatalf("item %d error = %q, want %q", i, it.Error, want)
		}
		if it.EndedAt == nil {
			t.Fatalf("item %d missing ended_at", i)
		}
	}
	for _, i := range []int{0, 2} {
		it := final.Items[i]
		if it.Status != ItemSucceeded || it.Error != "" {
			t.Fatalf("item %d must be a clean success: %+v", i, it)
		}
		if got := it.Result["run_id"]; got != "run-"+strconv.Itoa(i) {
			t.Fatalf("item %d result wrong: %v", i, it.Result)
		}
	}
}

func TestBatchPanicIsolation(t *testing.T) {
	svc := NewServiceWithOptions(Options{})
	units := []Unit{
		{Ref: "panics", Run: func(context.Context) (map[string]any, error) { panic("kaboom") }},
		{Ref: "fine", Run: func(context.Context) (map[string]any, error) { return map[string]any{"ok": 1}, nil }},
	}
	b, err := svc.Submit(context.Background(), "org-1", TypeAgentUpdate, units, SubmitOptions{})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	final, err := svc.Wait(context.Background(), "org-1", b.ID)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if final.Status != StatusCompleted || final.Summary().Succeeded != 1 || final.Summary().Failed != 1 {
		t.Fatalf("panic not isolated: %+v", final.Summary())
	}
	if it := final.Items[0]; it.Status != ItemFailed || !strings.Contains(it.Error, "kaboom") {
		t.Fatalf("panicking item wrong: %+v", it)
	}
}

func TestBatchCancelSemantics(t *testing.T) {
	svc := NewServiceWithOptions(Options{DefaultConcurrency: 1})

	// Whichever unit wins the single pool slot is nondeterministic; record its
	// index so the assertions track the actual in-flight item.
	var started atomic.Int32
	started.Store(-1)
	release := make(chan struct{})
	units := make([]Unit, 3)
	for i := range units {
		i := i
		units[i] = Unit{Ref: strconv.Itoa(i), Run: func(ctx context.Context) (map[string]any, error) {
			started.CompareAndSwap(-1, int32(i))
			select {
			case <-release:
				return nil, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}}
	}

	b, err := svc.Submit(context.Background(), "org-1", TypeRuns, units, SubmitOptions{MaxConcurrency: 1})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	// The first unit holds the only slot; the other two are pending.
	waitUntil(t, func() bool { return started.Load() >= 0 }, "first unit to start")
	inFlight := int(started.Load())
	snap, ok := svc.Get("org-1", b.ID)
	if !ok {
		t.Fatal("batch vanished")
	}
	if snap.Items[inFlight].Status != ItemRunning {
		t.Fatalf("item %d = %q, want running", inFlight, snap.Items[inFlight].Status)
	}
	for i := range units {
		if i == inFlight {
			continue
		}
		if snap.Items[i].Status != ItemPending {
			t.Fatalf("item %d = %q, want pending", i, snap.Items[i].Status)
		}
	}

	// Cancel: in-flight ctx fires, not-yet-started items never run.
	cancelled, err := svc.Cancel(context.Background(), "org-1", b.ID, "user-1")
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	for i := range units {
		if i == inFlight {
			continue
		}
		if cancelled.Items[i].Status != ItemCancelled {
			t.Fatalf("pending item %d not cancelled: %+v", i, cancelled.Items[i])
		}
	}

	final, err := svc.Wait(context.Background(), "org-1", b.ID)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if final.Status != StatusCancelled {
		t.Fatalf("status = %q, want cancelled", final.Status)
	}
	sum := final.Summary()
	if sum.Cancelled != 3 || sum.Succeeded != 0 || sum.Failed != 0 {
		t.Fatalf("summary wrong: %+v", sum)
	}
	if it := final.Items[inFlight]; it.Status != ItemCancelled || it.Error == "" {
		t.Fatalf("in-flight item not cancelled: %+v", it)
	}

	// Idempotent: cancelling a terminal batch returns the same snapshot.
	again, err := svc.Cancel(context.Background(), "org-1", b.ID, "user-1")
	if err != nil {
		t.Fatalf("second cancel: %v", err)
	}
	if again.Status != StatusCancelled || again.Summary().Cancelled != 3 {
		t.Fatalf("second cancel mutated the batch: %+v", again.Summary())
	}
}

func TestBatchAtomicValidationLeavesNothingBehind(t *testing.T) {
	store := &countingStore{MemoryStore: NewMemoryStore(0)}
	svc := NewServiceWithOptions(Options{Store: store, MaxItems: 2})
	unit := Unit{Ref: "a", Run: func(context.Context) (map[string]any, error) { return nil, nil }}

	if _, err := svc.Submit(context.Background(), "org-1", TypeRuns, nil, SubmitOptions{}); !errors.Is(err, ErrNoItems) {
		t.Fatalf("empty submission: want ErrNoItems, got %v", err)
	}
	tooMany := make([]Unit, 3)
	for i := range tooMany {
		tooMany[i] = unit
	}
	if _, err := svc.Submit(context.Background(), "org-1", TypeRuns, tooMany, SubmitOptions{}); !errors.Is(err, ErrTooManyItems) {
		t.Fatalf("oversized submission: want ErrTooManyItems, got %v", err)
	}
	if _, err := svc.Submit(context.Background(), "  ", TypeRuns, []Unit{unit}, SubmitOptions{}); !errors.Is(err, ErrOrgRequired) {
		t.Fatalf("missing org: want ErrOrgRequired, got %v", err)
	}
	if puts := store.putCount(); puts != 0 {
		t.Fatalf("invalid submissions persisted %d records; atomic validation broken", puts)
	}
}

func TestBatchCappedHistoryAndOrgIsolation(t *testing.T) {
	svc := NewServiceWithOptions(Options{HistoryPerOrg: 3})
	unit := Unit{Ref: "a", Run: func(context.Context) (map[string]any, error) { return nil, nil }}

	ids := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		b, err := svc.Submit(context.Background(), "org-a", TypeRuns, []Unit{unit}, SubmitOptions{})
		if err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
		ids = append(ids, b.ID)
		if _, err := svc.Wait(context.Background(), "org-a", b.ID); err != nil {
			t.Fatalf("wait %d: %v", i, err)
		}
	}
	// Oldest evicted first: only the last 3 survive.
	for i, id := range ids {
		_, ok := svc.Get("org-a", id)
		want := i >= 2
		if ok != want {
			t.Fatalf("batch %d retention = %v, want %v", i, ok, want)
		}
	}
	// Tenant guard: another organization can never observe org-a's batches.
	if _, ok := svc.Get("org-b", ids[4]); ok {
		t.Fatal("cross-org Get leaked a batch")
	}
	if _, err := svc.Wait(context.Background(), "org-b", ids[4]); !errors.Is(err, ErrBatchNotFound) {
		t.Fatalf("cross-org Wait: want ErrBatchNotFound, got %v", err)
	}
}

func TestBatchEmitsSSEProgressEvents(t *testing.T) {
	svc := NewServiceWithOptions(Options{})
	streamer := streaming.NewService()
	svc.SetStreamer(streamer)

	units := make([]Unit, 3)
	for i := range units {
		units[i] = Unit{Ref: "u" + strconv.Itoa(i), Run: func(context.Context) (map[string]any, error) {
			return nil, nil
		}}
	}
	b, err := svc.Submit(context.Background(), "org-1", TypeRuns, units, SubmitOptions{})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	// Subscribe BEFORE the events are needed; the subscriber buffer holds
	// every event because publication happens-before the finalize that closes
	// the wake channel Wait returns on.
	events := streamer.Subscribe(b.ID)
	if _, err := svc.Wait(context.Background(), "org-1", b.ID); err != nil {
		t.Fatalf("wait: %v", err)
	}

	var (
		sawUpdated int
		lastStates map[string]string
		lastDone   int
		lastTotal  int
	)
	// The payloads travel in-process (no JSON round-trip), so counters arrive
	// as int; asInt keeps the assertion robust to any future wire hop.
	asInt := func(v any) int {
		switch n := v.(type) {
		case int:
			return n
		case int64:
			return int(n)
		case float64:
			return int(n)
		}
		return -1
	}
drain:
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				break drain
			}
			if ev.Type != EventBatchType || ev.Name != EventBatchUpdated {
				t.Fatalf("unexpected event type/name: %q/%q", ev.Type, ev.Name)
			}
			if ev.Payload["batch_id"] != b.ID {
				t.Fatalf("event batch_id = %v, want %v", ev.Payload["batch_id"], b.ID)
			}
			lastDone = asInt(ev.Payload["completed"])
			lastTotal = asInt(ev.Payload["total"])
			// The payload travels in-process, so item_states keeps its
			// map[string]string type (no JSON round-trip).
			raw, ok := ev.Payload["item_states"].(map[string]string)
			if !ok {
				t.Fatalf("event item_states missing: %v", ev.Payload)
			}
			lastStates = make(map[string]string, len(raw))
			for ref, st := range raw {
				lastStates[ref] = st
			}
			sawUpdated++
		default:
			break drain
		}
	}
	if sawUpdated == 0 {
		t.Fatal("no batch.updated events observed")
	}
	if lastDone != 3 || lastTotal != 3 {
		t.Fatalf("final event counters = %v/%v, want 3/3", lastDone, lastTotal)
	}
	if len(lastStates) != 3 {
		t.Fatalf("item_states = %v, want 3 refs", lastStates)
	}
	for ref, st := range lastStates {
		if st != string(ItemSucceeded) {
			t.Fatalf("item %q final state = %q, want succeeded", ref, st)
		}
	}
	// The replayable history on the SSE seam carries the same additive type.
	if hist := streamer.History(b.ID); len(hist) != sawUpdated {
		t.Fatalf("stream history = %d events, want %d", len(hist), sawUpdated)
	}
}

func TestBatchNoEventsWithoutStreamer(t *testing.T) {
	svc := NewServiceWithOptions(Options{})
	unit := Unit{Ref: "a", Run: func(context.Context) (map[string]any, error) { return nil, nil }}
	b, err := svc.Submit(context.Background(), "org-1", TypeRuns, []Unit{unit}, SubmitOptions{})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := svc.Wait(context.Background(), "org-1", b.ID); err != nil {
		t.Fatalf("wait: %v", err)
	}
	// Unwired streamer must be a clean no-op (nothing to assert beyond "no
	// panic"); the record is still complete.
	final, _ := svc.Get("org-1", b.ID)
	if final.Status != StatusCompleted {
		t.Fatalf("status = %q", final.Status)
	}
}

func TestBatchAuditRows(t *testing.T) {
	svc := NewServiceWithOptions(Options{})
	auditSvc := audit.NewService()
	svc.SetAuditor(auditSvc)

	unit := Unit{Ref: "a", Run: func(context.Context) (map[string]any, error) { return nil, nil }}
	b, err := svc.Submit(context.Background(), "org-1", TypeRuns, []Unit{unit}, SubmitOptions{Actor: "user-9"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := svc.Wait(context.Background(), "org-1", b.ID); err != nil {
		t.Fatalf("wait: %v", err)
	}
	entries, err := auditSvc.ListCtx(context.Background(), "org-1")
	if err != nil {
		t.Fatalf("audit list: %v", err)
	}
	counts := map[string]*audit.Entry{}
	for _, e := range entries {
		counts[e.Action] = e
	}
	created, ok := counts["batch.created"]
	if !ok {
		t.Fatalf("missing batch.created row: %v", counts)
	}
	if created.Actor != "user-9" || created.Resource != "batches/"+b.ID {
		t.Fatalf("batch.created row wrong: %+v", created)
	}
	if created.Metadata["total"] != 1 {
		t.Fatalf("batch.created metadata wrong: %v", created.Metadata)
	}
	done, ok := counts["batch.completed"]
	if !ok {
		t.Fatalf("missing batch.completed row: %v", counts)
	}
	if done.Metadata["succeeded"] != 1 || done.Metadata["status"] != StatusCompleted {
		t.Fatalf("batch.completed metadata wrong: %v", done.Metadata)
	}
}

func TestBatchWaitUnknownAndNilSafety(t *testing.T) {
	svc := NewServiceWithOptions(Options{})
	if _, err := svc.Wait(context.Background(), "org-1", "missing"); !errors.Is(err, ErrBatchNotFound) {
		t.Fatalf("unknown wait: want ErrBatchNotFound, got %v", err)
	}
	if _, ok := svc.Get("", ""); ok {
		t.Fatal("empty-scope Get must miss")
	}
	// Nil-safe seams and service.
	var nilSvc *Service
	nilSvc.SetStreamer(nil)
	nilSvc.SetAuditor(nil)
	svc.SetStreamer(nil)
	svc.SetAuditor(nil)
	if _, err := svc.Cancel(context.Background(), "org-1", "missing", ""); !errors.Is(err, ErrBatchNotFound) {
		t.Fatalf("cancel unknown: want ErrBatchNotFound, got %v", err)
	}
}

func TestMaxItemsFromEnv(t *testing.T) {
	t.Setenv(EnvMaxItems, "")
	if got := MaxItemsFromEnv(); got != DefaultMaxItems {
		t.Fatalf("unset env = %d, want %d", got, DefaultMaxItems)
	}
	t.Setenv(EnvMaxItems, "7")
	if got := MaxItemsFromEnv(); got != 7 {
		t.Fatalf("env=7 -> %d, want 7", got)
	}
	for _, bad := range []string{"abc", "-3", "0"} {
		t.Setenv(EnvMaxItems, bad)
		if got := MaxItemsFromEnv(); got != DefaultMaxItems {
			t.Fatalf("env=%q -> %d, want default %d", bad, got, DefaultMaxItems)
		}
	}
}

func TestMemoryStoreRejectsNilRecord(t *testing.T) {
	store := NewMemoryStore(0)
	if err := store.Put(nil); err == nil {
		t.Fatal("nil record must be rejected")
	}
}
