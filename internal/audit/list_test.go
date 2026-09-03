package audit

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// seedEntry writes one entry directly into the in-memory service (bypassing
// LogCtx so tests control the timestamp and id exactly).
func seedEntry(t *testing.T, s *Service, orgID string, at time.Time, id, action string) *Entry {
	t.Helper()
	entry := &Entry{
		ID:             id,
		Actor:          "user-" + id,
		Action:         action,
		OrganizationID: orgID,
		Resource:       "resources/" + id,
		Metadata:       map[string]any{"seed": id},
		CreatedAt:      at,
	}
	s.mu.Lock()
	s.items = append(s.items, entry)
	s.mu.Unlock()
	return entry
}

func TestNormalizeLimit(t *testing.T) {
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

func TestListEntriesPagedInMemoryWalksAllPages(t *testing.T) {
	service := NewService()
	org := "org-1"
	base := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	// Seed 7 entries for org-1 (plus 3 for a foreign org) out of chronological
	// order; the listing must sort them newest first regardless.
	offsets := []int{3, 0, 5, 1, 6, 2, 4}
	for i, off := range offsets {
		seedEntry(t, service, org, base.Add(time.Duration(off)*time.Minute), fmt.Sprintf("e%d", i), "agent.created")
	}
	for i := 0; i < 3; i++ {
		seedEntry(t, service, "org-other", base.Add(time.Duration(i)*time.Minute), fmt.Sprintf("f%d", i), "run.failed")
	}
	// e0 sits at +3m, e1 at +0m, e2 at +5m, e3 at +1m, e4 at +6m, e5 at +2m,
	// e6 at +4m -> newest-first order is:
	wantOrder := []string{"e4", "e2", "e6", "e0", "e5", "e3", "e1"}

	var gotIDs []string
	cursor := ""
	pages := 0
	for {
		entries, next, err := service.ListEntriesPaged(context.Background(), org, 3, cursor)
		if err != nil {
			t.Fatalf("ListEntriesPaged(page %d): %v", pages, err)
		}
		if len(entries) > 3 {
			t.Fatalf("page %d exceeded the limit: %d entries", pages, len(entries))
		}
		for _, e := range entries {
			if e.OrganizationID != org {
				t.Fatalf("cross-tenant leak on page %d: %s (%s)", pages, e.ID, e.OrganizationID)
			}
			gotIDs = append(gotIDs, e.ID)
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
		t.Fatalf("7 entries at limit 3 should take 3 pages, got %d", pages)
	}
	if len(gotIDs) != 7 {
		t.Fatalf("expected 7 entries across pages, got %d: %v", len(gotIDs), gotIDs)
	}
	seen := make(map[string]bool, len(gotIDs))
	for i, id := range gotIDs {
		if seen[id] {
			t.Errorf("entry %s repeated across pages", id)
		}
		seen[id] = true
		if id != wantOrder[i] {
			t.Errorf("position %d: got %s want %s (newest-first violated)", i, id, wantOrder[i])
		}
	}
}

func TestListEntriesPagedInMemorySameTimestampIDTiebreak(t *testing.T) {
	service := NewService()
	same := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	seedEntry(t, service, "org-1", same, "id-b", "agent.created")
	seedEntry(t, service, "org-1", same, "id-a", "agent.created")
	seedEntry(t, service, "org-1", same, "id-c", "agent.created")

	entries, next, err := service.ListEntriesPaged(context.Background(), "org-1", 2, "")
	if err != nil {
		t.Fatalf("ListEntriesPaged: %v", err)
	}
	if next == "" {
		t.Fatal("a follow-up page should exist")
	}
	// Same timestamp: the id tiebreak (DESC) must order id-c, id-b, id-a.
	if entries[0].ID != "id-c" || entries[1].ID != "id-b" {
		t.Fatalf("tiebreak order wrong: %s, %s", entries[0].ID, entries[1].ID)
	}
	entries, next, err = service.ListEntriesPaged(context.Background(), "org-1", 2, next)
	if err != nil {
		t.Fatalf("ListEntriesPaged(page 2): %v", err)
	}
	if len(entries) != 1 || entries[0].ID != "id-a" || next != "" {
		t.Fatalf("final page wrong: %v (next=%q)", entries, next)
	}
}

func TestListEntriesPagedExactMultipleNoTrailingEmptyPage(t *testing.T) {
	service := NewService()
	base := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 4; i++ {
		seedEntry(t, service, "org-1", base.Add(time.Duration(i)*time.Second), fmt.Sprintf("e%d", i), "agent.created")
	}
	// limit=4 == total: page 1 must report exhaustion, not emit a cursor that
	// would produce one empty page.
	entries, next, err := service.ListEntriesPaged(context.Background(), "org-1", 4, "")
	if err != nil {
		t.Fatalf("ListEntriesPaged: %v", err)
	}
	if len(entries) != 4 || next != "" {
		t.Fatalf("exact-fit page should carry everything and no cursor: %d entries, next=%q", len(entries), next)
	}
}

func TestListEntriesPagedInvalidCursor(t *testing.T) {
	service := NewService()
	seedEntry(t, service, "org-1", time.Now(), "e1", "agent.created")
	for _, bad := range []string{"not-base64!!", "eyJhIjoxfQ", ""} {
		if bad == "" {
			continue // empty cursor = first page, valid by contract
		}
		if _, _, err := service.ListEntriesPaged(context.Background(), "org-1", 10, bad); !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("cursor %q should be ErrInvalidCursor, got %v", bad, err)
		}
	}
}

func TestListEntriesPagedEmptyOrgAndRequiredValidation(t *testing.T) {
	service := NewService()
	if _, _, err := service.ListEntriesPaged(context.Background(), "  ", 10, ""); !errors.Is(err, ErrOrgRequired) {
		t.Fatalf("blank org should be ErrOrgRequired, got %v", err)
	}
	entries, next, err := service.ListEntriesPaged(context.Background(), "org-empty", 10, "")
	if err != nil || len(entries) != 0 || next != "" {
		t.Fatalf("unknown org should be an empty page, got %v next=%q err=%v", entries, next, err)
	}
}

// plainStore implements only the historical Store contract; the service must
// still paginate its results in memory (fallback path).
type plainStore struct {
	entries []*Entry
}

func (s *plainStore) InsertEntry(_ context.Context, entry *Entry) error {
	s.entries = append(s.entries, entry)
	return nil
}

func (s *plainStore) ListEntries(_ context.Context, orgID string) ([]*Entry, error) {
	out := make([]*Entry, 0)
	for _, e := range s.entries {
		if e.OrganizationID == orgID {
			out = append(out, e)
		}
	}
	return out, nil
}

// pagedStoreSpy implements PagedStore and records the arguments it receives.
type pagedStoreSpy struct {
	gotOrg    string
	gotLimit  int
	gotCursor string
	entries   []*Entry
	next      string
}

func (s *pagedStoreSpy) InsertEntry(_ context.Context, _ *Entry) error { return nil }

func (s *pagedStoreSpy) ListEntries(_ context.Context, _ string) ([]*Entry, error) {
	return s.entries, nil
}

func (s *pagedStoreSpy) ListEntriesPaged(_ context.Context, orgID string, limit int, cursor string) ([]*Entry, string, error) {
	s.gotOrg, s.gotLimit, s.gotCursor = orgID, limit, cursor
	return s.entries, s.next, nil
}

func TestListEntriesPagedFallsBackForPlainStore(t *testing.T) {
	store := &plainStore{}
	service := NewServiceWithStore(store)
	base := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		store.entries = append(store.entries, &Entry{
			ID: fmt.Sprintf("e%d", i), Action: "agent.created", OrganizationID: "org-1",
			CreatedAt: base.Add(time.Duration(i) * time.Second),
		})
	}
	store.entries = append(store.entries, &Entry{ID: "foreign", OrganizationID: "org-2", CreatedAt: base})

	entries, next, err := service.ListEntriesPaged(context.Background(), "org-1", 2, "")
	if err != nil {
		t.Fatalf("ListEntriesPaged: %v", err)
	}
	if len(entries) != 2 || next == "" {
		t.Fatalf("fallback pagination broken: %d entries, next=%q", len(entries), next)
	}
	if entries[0].ID != "e4" {
		t.Fatalf("fallback should honor newest-first ordering, got %s", entries[0].ID)
	}
	total := len(entries)
	for next != "" {
		entries, next, err = service.ListEntriesPaged(context.Background(), "org-1", 2, next)
		if err != nil {
			t.Fatalf("ListEntriesPaged(continuation): %v", err)
		}
		total += len(entries)
	}
	if total != 5 {
		t.Fatalf("fallback should page all 5 tenant entries, got %d", total)
	}
}

func TestListEntriesPagedDelegatesToPagedStore(t *testing.T) {
	spy := &pagedStoreSpy{
		entries: []*Entry{{ID: "s1", Action: "agent.created", OrganizationID: "org-1", CreatedAt: time.Now()}},
		next:    "cursor-from-store",
	}
	service := NewServiceWithStore(spy)

	entries, next, err := service.ListEntriesPaged(context.Background(), "org-1", 100000, "abc")
	if err != nil {
		t.Fatalf("ListEntriesPaged: %v", err)
	}
	if len(entries) != 1 || next != "cursor-from-store" {
		t.Fatalf("delegation should return the store's page verbatim: %v next=%q", entries, next)
	}
	if spy.gotOrg != "org-1" {
		t.Errorf("store should receive the tenant id, got %q", spy.gotOrg)
	}
	if spy.gotLimit != MaxListLimit {
		t.Errorf("service should normalize the limit before delegating, got %d", spy.gotLimit)
	}
	if spy.gotCursor != "abc" {
		t.Errorf("store should receive the raw cursor, got %q", spy.gotCursor)
	}
}

func TestCursorRoundTrip(t *testing.T) {
	at := time.Date(2025, 6, 1, 12, 30, 0, 123456789, time.UTC)
	entry := &Entry{ID: "run-42|special", CreatedAt: at}
	decoded, id, err := decodeCursor(encodeCursor(entry))
	if err != nil {
		t.Fatalf("decodeCursor: %v", err)
	}
	if !decoded.Equal(at) || id != entry.ID {
		t.Fatalf("round trip mismatch: %v / %q", decoded, id)
	}
}
