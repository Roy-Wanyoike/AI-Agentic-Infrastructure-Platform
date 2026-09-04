package events

// Issue #56 read-side tests: keyset pagination semantics in both modes —
//   - MemoryStore (zero-infrastructure half): ordering, tiebreak, exhaustion,
//     tenant scoping, filters, and pagination stability under CONCURRENT
//     appends (the keyset walk must never repeat or reorder rows);
//   - MemoryPublisher ring + AuditPublisher read-through decorators;
//   - pgStore (sqlmock): the SQL keyset predicate, tenant guard binding,
//     filter composition, limit+1 exhaustion probe and row scanning.
// Mirrors internal/audit/list_test.go (issue #18) shape for shape.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

var listTestBase = time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)

// seedEvent appends one contract-valid event with an exact timestamp/id
// (bypassing time.Now so ordering assertions are deterministic).
func seedEvent(t *testing.T, s *MemoryStore, orgID string, at time.Time, id, eventType, entityType, entityID string) Event {
	t.Helper()
	event := Event{
		ID:        id,
		Type:      eventType,
		TenantID:  orgID,
		Timestamp: at,
		Resource:  Resource{Type: entityType, ID: entityID},
		Payload:   map[string]any{"seed": id},
	}
	if err := s.AppendEvent(context.Background(), &event); err != nil {
		t.Fatalf("AppendEvent(%s): %v", id, err)
	}
	return event
}

func TestNormalizeLimitEvents(t *testing.T) {
	cases := []struct {
		in   int
		want int
	}{
		{0, DefaultListLimit},
		{-5, DefaultListLimit},
		{1, 1},
		{10, 10},
		{MaxListLimit, MaxListLimit},
		{MaxListLimit + 1, MaxListLimit},
		{100000, MaxListLimit},
	}
	for _, tc := range cases {
		if got := NormalizeLimit(tc.in); got != tc.want {
			t.Errorf("NormalizeLimit(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestMemoryStoreListWalksAllPages(t *testing.T) {
	store := NewMemoryStore()
	org := "org-1"
	// 7 events for org-1 (plus 3 foreign) seeded out of chronological order;
	// offsets minutes: e0=+3 e1=+0 e2=+5 e3=+1 e4=+6 e5=+2 e6=+4.
	offsets := []int{3, 0, 5, 1, 6, 2, 4}
	for i, off := range offsets {
		seedEvent(t, store, org, listTestBase.Add(time.Duration(off)*time.Minute),
			fmt.Sprintf("e%d", i), EventRunStarted, "run", fmt.Sprintf("r-%d", i))
	}
	for i := 0; i < 3; i++ {
		seedEvent(t, store, "org-other", listTestBase.Add(time.Duration(i)*time.Minute),
			fmt.Sprintf("f%d", i), EventRunFailed, "run", fmt.Sprintf("x-%d", i))
	}
	wantOrder := []string{"e4", "e2", "e6", "e0", "e5", "e3", "e1"}

	var gotIDs []string
	cursor := ""
	pages := 0
	for {
		events, next, err := store.ListEventsPaged(context.Background(), org, EventFilter{}, 3, cursor)
		if err != nil {
			t.Fatalf("ListEventsPaged(page %d): %v", pages, err)
		}
		if len(events) > 3 {
			t.Fatalf("page %d exceeded the limit: %d events", pages, len(events))
		}
		for _, event := range events {
			if event.TenantID != org {
				t.Fatalf("cross-tenant leak on page %d: %s (%s)", pages, event.ID, event.TenantID)
			}
			gotIDs = append(gotIDs, event.ID)
		}
		pages++
		if next == "" {
			break
		}
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
		cursor = next
	}
	if pages != 3 {
		t.Fatalf("7 events at limit 3 should take 3 pages, got %d", pages)
	}
	if len(gotIDs) != 7 {
		t.Fatalf("expected 7 events across pages, got %d: %v", len(gotIDs), gotIDs)
	}
	seen := make(map[string]bool, len(gotIDs))
	for i, id := range gotIDs {
		if seen[id] {
			t.Errorf("event %s repeated across pages", id)
		}
		seen[id] = true
		if id != wantOrder[i] {
			t.Errorf("position %d: got %s want %s (newest-first violated)", i, id, wantOrder[i])
		}
	}
}

func TestMemoryStoreSameTimestampIDTiebreak(t *testing.T) {
	store := NewMemoryStore()
	same := listTestBase
	seedEvent(t, store, "org-1", same, "id-b", EventRunStarted, "run", "r")
	seedEvent(t, store, "org-1", same, "id-a", EventRunStarted, "run", "r")
	seedEvent(t, store, "org-1", same, "id-c", EventRunStarted, "run", "r")

	events, next, err := store.ListEventsPaged(context.Background(), "org-1", EventFilter{}, 2, "")
	if err != nil {
		t.Fatalf("ListEventsPaged: %v", err)
	}
	if next == "" {
		t.Fatal("a follow-up page should exist")
	}
	// Same timestamp: the id tiebreak (DESC) must order id-c, id-b, id-a.
	if events[0].ID != "id-c" || events[1].ID != "id-b" {
		t.Fatalf("tiebreak order wrong: %s, %s", events[0].ID, events[1].ID)
	}
	events, next, err = store.ListEventsPaged(context.Background(), "org-1", EventFilter{}, 2, next)
	if err != nil {
		t.Fatalf("ListEventsPaged(page 2): %v", err)
	}
	if len(events) != 1 || events[0].ID != "id-a" || next != "" {
		t.Fatalf("final page wrong: %v (next=%q)", events, next)
	}
}

func TestMemoryStoreExactMultipleNoTrailingEmptyPage(t *testing.T) {
	store := NewMemoryStore()
	for i := 0; i < 4; i++ {
		seedEvent(t, store, "org-1", listTestBase.Add(time.Duration(i)*time.Second),
			fmt.Sprintf("e%d", i), EventAgentCreated, "agent", "a")
	}
	events, next, err := store.ListEventsPaged(context.Background(), "org-1", EventFilter{}, 4, "")
	if err != nil {
		t.Fatalf("ListEventsPaged: %v", err)
	}
	if len(events) != 4 || next != "" {
		t.Fatalf("exact-fit page should carry everything and no cursor: %d events, next=%q", len(events), next)
	}
}

// TestMemoryStorePaginationStabilityUnderConcurrentInserts pins the keyset
// guarantee the HTTP contract relies on: while producers append concurrently,
// every cursor walk yields strictly descending (timestamp, id) keys and never
// repeats a row; once the writers quiesce, a fresh walk covers every event
// exactly once.
func TestMemoryStorePaginationStabilityUnderConcurrentInserts(t *testing.T) {
	store := NewMemoryStore()
	const (
		writers   = 8
		perWriter = 25
		total     = writers * perWriter
		pageLimit = 7
	)

	// Phase A: walk pages WHILE writers are appending. The walk may or may
	// not observe rows inserted after its cursor position (both are legal),
	// but it must never repeat a row, never go backwards, and never error.
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				event := Event{
					Type:     EventRunCompleted,
					TenantID: "org-1",
					Resource: Resource{Type: "run", ID: fmt.Sprintf("r-%d-%d", w, i)},
				}
				if err := store.AppendEvent(context.Background(), &event); err != nil {
					t.Errorf("concurrent AppendEvent: %v", err)
					return
				}
			}
		}(w)
	}
	writersDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(writersDone)
	}()

	walks := 0
loop:
	for {
		select {
		case <-writersDone:
			break loop
		default:
		}
		if err := walkAssertStable(t, store, pageLimit); err != nil {
			t.Fatalf("unstable walk during concurrent inserts: %v", err)
		}
		walks++
		if walks > 500 {
			t.Fatal("walk loop did not terminate")
		}
	}
	if walks < 1 {
		t.Fatal("phase A must complete at least one concurrent-insert walk")
	}

	// Phase B: writers are done — a fresh walk must cover exactly total
	// events, newest first, no duplicates.
	type keyed struct {
		ts time.Time
		id string
	}
	var got []keyed
	cursor := ""
	for {
		events, next, err := store.ListEventsPaged(context.Background(), "org-1", EventFilter{}, pageLimit, cursor)
		if err != nil {
			t.Fatalf("ListEventsPaged(quiesced): %v", err)
		}
		for _, event := range events {
			got = append(got, keyed{ts: event.Timestamp, id: event.ID})
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if len(got) != total {
		t.Fatalf("quiesced walk should cover %d events, got %d", total, len(got))
	}
	seen := make(map[string]bool, len(got))
	for i, k := range got {
		if seen[k.id] {
			t.Errorf("event %s repeated across quiesced pages", k.id)
		}
		seen[k.id] = true
		// The ordering contract is on the (timestamp, id) keyset — concurrent
		// inserts can legitimately produce ids that do not sort monotonically
		// across DIFFERENT timestamps.
		if i > 0 {
			prev := got[i-1]
			if k.ts.After(prev.ts) || (k.ts.Equal(prev.ts) && k.id > prev.id) {
				t.Errorf("order violated at %d: (%v, %s) after (%v, %s)", i, k.ts, k.id, prev.ts, prev.id)
			}
		}
	}
}

// walkAssertStable walks every page of org-1 and asserts the keyset
// invariants: page-local limit respected, strictly descending (timestamp, id)
// keys across page boundaries, zero duplicate ids.
func walkAssertStable(t *testing.T, store *MemoryStore, limit int) error {
	t.Helper()
	cursor := ""
	seen := make(map[string]bool)
	var lastTime time.Time
	var lastID string
	first := true
	pages := 0
	for {
		events, next, err := store.ListEventsPaged(context.Background(), "org-1", EventFilter{}, limit, cursor)
		if err != nil {
			return err
		}
		if len(events) > limit {
			return fmt.Errorf("page %d exceeded the limit: %d", pages, len(events))
		}
		for _, event := range events {
			if seen[event.ID] {
				return fmt.Errorf("duplicate event %s across pages", event.ID)
			}
			seen[event.ID] = true
			if !first {
				if event.Timestamp.After(lastTime) || (event.Timestamp.Equal(lastTime) && event.ID > lastID) {
					return fmt.Errorf("order violated: %s after (%v, %s)", event.ID, lastTime, lastID)
				}
			}
			first = false
			lastTime = event.Timestamp
			lastID = event.ID
		}
		pages++
		if next == "" {
			return nil
		}
		cursor = next
	}
}

// got0Safe avoids the unused-variable dance in the quiesced walk above.
func got0Safe(events []Event) []Event { return events }

func TestMemoryStoreTenantScoping(t *testing.T) {
	store := NewMemoryStore()
	seedEvent(t, store, "org-1", listTestBase, "mine-1", EventAgentCreated, "agent", "a-1")
	seedEvent(t, store, "org-2", listTestBase.Add(time.Minute), "theirs-1", EventRunFailed, "run", "r-1")

	// org-2's newer event must never surface in org-1's pages (and the other
	// way around).
	events, next, err := store.ListEventsPaged(context.Background(), "org-1", EventFilter{}, 10, "")
	if err != nil {
		t.Fatalf("ListEventsPaged(org-1): %v", err)
	}
	if next != "" || len(events) != 1 || events[0].ID != "mine-1" {
		t.Fatalf("org-1 must see exactly its own event: %v (next=%q)", events, next)
	}
	events, _, err = store.ListEventsPaged(context.Background(), "org-2", EventFilter{}, 10, "")
	if err != nil {
		t.Fatalf("ListEventsPaged(org-2): %v", err)
	}
	if len(events) != 1 || events[0].ID != "theirs-1" {
		t.Fatalf("org-2 must see exactly its own event: %v", events)
	}
	if _, _, err := store.ListEventsPaged(context.Background(), "  ", EventFilter{}, 10, ""); !errors.Is(err, ErrOrgRequired) {
		t.Fatalf("blank org should be ErrOrgRequired, got %v", err)
	}
}

func TestMemoryStoreFilters(t *testing.T) {
	store := NewMemoryStore()
	seedEvent(t, store, "org-1", listTestBase, "run-start", EventRunStarted, "run", "r-1")
	seedEvent(t, store, "org-1", listTestBase.Add(time.Minute), "run-fail", EventRunFailed, "run", "r-2")
	seedEvent(t, store, "org-1", listTestBase.Add(2*time.Minute), "agent-create", EventAgentCreated, "agent", "a-1")
	seedEvent(t, store, "org-1", listTestBase.Add(3*time.Minute), "run-fail-r1", EventRunFailed, "run", "r-1")

	at := func(id string) time.Time {
		t.Helper()
		events, _, err := store.ListEventsPaged(context.Background(), "org-1", EventFilter{}, MaxListLimit, "")
		if err != nil {
			t.Fatalf("seed listing: %v", err)
		}
		for _, event := range events {
			if event.ID == id {
				return event.Timestamp
			}
		}
		t.Fatalf("event %s not found", id)
		return time.Time{}
	}

	cases := []struct {
		name   string
		filter EventFilter
		want   []string
	}{
		{"type filter", EventFilter{Type: EventRunFailed}, []string{"run-fail-r1", "run-fail"}},
		{"entity_type filter", EventFilter{EntityType: "agent"}, []string{"agent-create"}},
		{"entity_id filter", EventFilter{EntityID: "r-1"}, []string{"run-fail-r1", "run-start"}},
		{"type + entity_id", EventFilter{Type: EventRunStarted, EntityID: "r-1"}, []string{"run-start"}},
		{"since excludes older", EventFilter{Since: at("run-fail")}, []string{"run-fail-r1", "agent-create", "run-fail"}},
		{"since is inclusive", EventFilter{Since: at("agent-create")}, []string{"run-fail-r1", "agent-create"}},
		{"combined", EventFilter{Type: EventRunFailed, EntityType: "run", EntityID: "r-2", Since: at("run-fail")}, []string{"run-fail"}},
		{"no match", EventFilter{EntityType: "deployment"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events, next, err := store.ListEventsPaged(context.Background(), "org-1", tc.filter, MaxListLimit, "")
			if err != nil {
				t.Fatalf("ListEventsPaged: %v", err)
			}
			if next != "" {
				t.Fatalf("unbounded page must not emit a cursor, got %q", next)
			}
			got := make([]string, 0, len(events))
			for _, event := range events {
				got = append(got, event.ID)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("filter %v: got %v want %v", tc.filter, got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("filter %v: position %d got %s want %s (full: %v)", tc.filter, i, got[i], tc.want[i], got)
				}
			}
		})
	}
}

func TestMemoryStoreInvalidCursorAndValidation(t *testing.T) {
	store := NewMemoryStore()
	seedEvent(t, store, "org-1", listTestBase, "e1", EventRunStarted, "run", "r")
	for _, bad := range []string{"not-base64!!", "eyJhIjoxfQ"} {
		if _, _, err := store.ListEventsPaged(context.Background(), "org-1", EventFilter{}, 10, bad); !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("cursor %q should be ErrInvalidCursor, got %v", bad, err)
		}
	}
	// AppendEvent mirrors pgStore validation.
	if err := store.AppendEvent(context.Background(), nil); !errors.Is(err, ErrInvalidEvent) {
		t.Errorf("nil event should be ErrInvalidEvent, got %v", err)
	}
	if err := store.AppendEvent(context.Background(), &Event{ID: "x", Type: "bogus.type", TenantID: "org-1"}); !errors.Is(err, ErrInvalidEvent) {
		t.Errorf("unknown type must be ErrInvalidEvent, got %v", err)
	}
	if err := store.AppendEvent(context.Background(), &Event{ID: "x", Type: EventRunStarted}); !errors.Is(err, ErrInvalidEvent) {
		t.Errorf("missing tenant must be ErrInvalidEvent, got %v", err)
	}
	// A provided timestamp is preserved (not overwritten) so cursor walks are
	// deterministic for seeded data.
	seeded := seedEvent(t, store, "org-1", listTestBase, "ts-kept", EventRunStarted, "run", "r")
	if !seeded.Timestamp.Equal(listTestBase) {
		t.Fatalf("provided timestamp must be preserved, got %v", seeded.Timestamp)
	}
}

func TestMemoryPublisherListEventsPaged(t *testing.T) {
	pub := NewMemoryPublisher()
	mustPublish(t, pub, EventRunStarted, "org-1", "p-1")
	mustPublish(t, pub, EventRunFailed, "org-2", "p-2")
	mustPublish(t, pub, EventRunCompleted, "org-1", "p-3")

	events, next, err := pub.ListEventsPaged(context.Background(), "org-1", EventFilter{}, 10, "")
	if err != nil {
		t.Fatalf("MemoryPublisher.ListEventsPaged: %v", err)
	}
	if next != "" || len(events) != 2 {
		t.Fatalf("org-1 should see exactly its 2 ring events, got %v (next=%q)", events, next)
	}
	if events[0].ID != "p-3" || events[1].ID != "p-1" {
		t.Fatalf("ring listing must be newest first: %v", events)
	}
	if _, _, err := pub.ListEventsPaged(context.Background(), "", EventFilter{}, 10, ""); !errors.Is(err, ErrOrgRequired) {
		t.Fatalf("blank org should be ErrOrgRequired, got %v", err)
	}
}

func mustPublish(t *testing.T, pub Publisher, eventType, orgID, id string) {
	t.Helper()
	event := NewEvent(eventType, orgID, "run", "r", nil)
	event.ID = id
	if err := pub.Publish(context.Background(), event); err != nil {
		t.Fatalf("Publish(%s): %v", id, err)
	}
}

func TestAuditPublisherListReadThrough(t *testing.T) {
	store := NewMemoryStore()
	seedEvent(t, store, "org-1", listTestBase, "via-pub", EventRunStarted, "run", "r")
	pub := NewAuditPublisher(store, nil)

	events, next, err := pub.ListEventsPaged(context.Background(), "org-1", EventFilter{}, 10, "")
	if err != nil {
		t.Fatalf("AuditPublisher.ListEventsPaged: %v", err)
	}
	if next != "" || len(events) != 1 || events[0].ID != "via-pub" {
		t.Fatalf("read-through must reach the store: %v (next=%q)", events, next)
	}

	// A pass-through decorator (nil store) cannot list: the Store contract
	// guarantees no read, so the honest answer is ErrListUnsupported.
	if _, _, err := NewAuditPublisher(nil, nil).ListEventsPaged(context.Background(), "org-1", EventFilter{}, 10, ""); !errors.Is(err, ErrListUnsupported) {
		t.Fatalf("nil store should be ErrListUnsupported, got %v", err)
	}
}

func TestEventCursorRoundTrip(t *testing.T) {
	at := listTestBase.Add(30 * time.Minute).Add(123456789 * time.Nanosecond)
	event := &Event{ID: "run-42|special", Timestamp: at}
	decoded, id, err := decodeCursor(encodeCursor(event))
	if err != nil {
		t.Fatalf("decodeCursor: %v", err)
	}
	if !decoded.Equal(at) || id != event.ID {
		t.Fatalf("round trip mismatch: %v / %q", decoded, id)
	}
}

// ---------------------------------------------------------------------------
// pgStore (sqlmock) — the SQL half of the split
// ---------------------------------------------------------------------------

func eventRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "organization_id", "type", "project_id", "resource_type",
		"resource_id", "execution_id", "trace_id", "payload", "created_at",
	})
}

func TestPgStoreListEventsPagedFirstPage(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	store := &pgStore{db: db}
	mock.ExpectQuery(`SELECT (.+) FROM events WHERE organization_id = \$1 ORDER BY created_at DESC, id DESC LIMIT \$2`).
		WithArgs("org-1", 3). // limit+1 exhaustion probe
		WillReturnRows(eventRows().
			AddRow("e2", "org-1", EventRunFailed, "proj-1", "run", "r-2", "exec-2", "trace-2", `{"reason":"timeout"}`, listTestBase.Add(2*time.Minute)).
			AddRow("e1", "org-1", EventRunStarted, "", "run", "r-1", "", "", "", listTestBase))

	events, next, err := store.ListEventsPaged(context.Background(), "org-1", EventFilter{}, 2, "")
	if err != nil {
		t.Fatalf("ListEventsPaged: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 rows, got %d: %v", len(events), events)
	}
	if events[0].ID != "e2" || events[0].TenantID != "org-1" || events[0].Type != EventRunFailed {
		t.Fatalf("row projection wrong: %+v", events[0])
	}
	if events[0].ProjectID != "proj-1" || events[0].Resource.Type != "run" || events[0].Resource.ID != "r-2" {
		t.Fatalf("resource projection wrong: %+v", events[0])
	}
	if events[0].Payload["reason"] != "timeout" {
		t.Fatalf("payload must decode as JSON: %+v", events[0].Payload)
	}
	// Nullable columns degrade to empty strings, NULL payload to nil.
	if events[1].ProjectID != "" || events[1].ExecutionID != "" || events[1].TraceID != "" || events[1].Payload != nil {
		t.Fatalf("NULL columns must scan as zero values: %+v", events[1])
	}
	// limit+1 rows were not returned -> trail exhausted, no cursor.
	if next != "" {
		t.Fatalf("exhausted trail must not emit a cursor, got %q", next)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestPgStoreListEventsPagedEmitsCursorOnFullPage(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	store := &pgStore{db: db}
	mock.ExpectQuery(`SELECT (.+) FROM events WHERE organization_id = \$1 ORDER BY created_at DESC, id DESC LIMIT \$2`).
		WithArgs("org-1", 2). // limit(1)+1
		WillReturnRows(eventRows().
			AddRow("e2", "org-1", EventRunFailed, "", "run", "r", "", "", "", listTestBase.Add(2*time.Minute)).
			AddRow("e1", "org-1", EventRunStarted, "", "run", "r", "", "", "", listTestBase).
			AddRow("e0", "org-1", EventRunStarted, "", "run", "r", "", "", "", listTestBase.Add(-time.Minute)))

	events, next, err := store.ListEventsPaged(context.Background(), "org-1", EventFilter{}, 1, "")
	if err != nil {
		t.Fatalf("ListEventsPaged: %v", err)
	}
	if len(events) != 1 || events[0].ID != "e2" {
		t.Fatalf("page must be trimmed to the limit: %v", events)
	}
	if next == "" {
		t.Fatal("a follow-up page exists, cursor must be emitted")
	}
	// The cursor encodes the page's last row: a continuation query keyed on
	// exactly that (created_at, id) pair must be accepted.
	mock.ExpectQuery(`FROM events WHERE organization_id = \$1 AND \(created_at, id\) < \(\$2, \$3\) ORDER BY created_at DESC, id DESC LIMIT \$4`).
		WithArgs("org-1", listTestBase.Add(2*time.Minute), "e2", 2).
		WillReturnRows(eventRows().AddRow("e1", "org-1", EventRunStarted, "", "run", "r", "", "", "", listTestBase))
	if _, _, err := store.ListEventsPaged(context.Background(), "org-1", EventFilter{}, 1, next); err != nil {
		t.Fatalf("emitted cursor must decode for the next page: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestPgStoreListEventsPagedCursorPredicate(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	store := &pgStore{db: db}
	cursorTime := listTestBase.Add(time.Minute)
	cursor := encodeCursor(&Event{ID: "e1", Timestamp: cursorTime})
	mock.ExpectQuery(`FROM events WHERE organization_id = \$1 AND \(created_at, id\) < \(\$2, \$3\) ORDER BY created_at DESC, id DESC LIMIT \$4`).
		WithArgs("org-1", cursorTime, "e1", 3).
		WillReturnRows(eventRows().AddRow("e0", "org-1", EventRunStarted, "", "run", "r", "", "", "", listTestBase))

	events, next, err := store.ListEventsPaged(context.Background(), "org-1", EventFilter{}, 2, cursor)
	if err != nil {
		t.Fatalf("ListEventsPaged(cursor): %v", err)
	}
	if len(events) != 1 || next != "" || events[0].ID != "e0" {
		t.Fatalf("continuation page wrong: %v (next=%q)", events, next)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestPgStoreListEventsPagedFilterBindingOrder(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	store := &pgStore{db: db}
	since := listTestBase
	mock.ExpectQuery(`FROM events WHERE organization_id = \$1 AND type = \$2 AND resource_type = \$3 AND resource_id = \$4 AND created_at >= \$5 ORDER BY created_at DESC, id DESC LIMIT \$6`).
		WithArgs("org-1", EventRunFailed, "run", "r-1", since, 3).
		WillReturnRows(eventRows())

	if _, _, err := store.ListEventsPaged(context.Background(), "org-1", EventFilter{
		Type:       EventRunFailed,
		EntityType: "run",
		EntityID:   "r-1",
		Since:      since,
	}, 2, ""); err != nil {
		t.Fatalf("ListEventsPaged(filtered): %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestPgStoreListEventsPagedTenantGuardAlwaysBound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	store := &pgStore{db: db}
	// Even a fully-filtered query keeps organization_id as its FIRST bound
	// parameter — the tenant guard is never optional.
	mock.ExpectQuery(`WHERE organization_id = \$1 AND type = \$2`).
		WithArgs("org-1", EventRunStarted, 3).
		WillReturnRows(eventRows())

	if _, _, err := store.ListEventsPaged(context.Background(), "org-1", EventFilter{Type: EventRunStarted}, 2, ""); err != nil {
		t.Fatalf("ListEventsPaged: %v", err)
	}

	// Guard rails: nil db and blank org never reach SQL.
	if _, _, err := (&pgStore{}).ListEventsPaged(context.Background(), "org-1", EventFilter{}, 2, ""); err == nil {
		t.Fatal("nil db must error")
	}
	if _, _, err := store.ListEventsPaged(context.Background(), "", EventFilter{}, 2, ""); !errors.Is(err, ErrOrgRequired) {
		t.Fatalf("blank org should be ErrOrgRequired, got %v", err)
	}
	if _, _, err := store.ListEventsPaged(context.Background(), "org-1", EventFilter{}, 2, "not-base64!!"); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("malformed cursor should be ErrInvalidCursor, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}
