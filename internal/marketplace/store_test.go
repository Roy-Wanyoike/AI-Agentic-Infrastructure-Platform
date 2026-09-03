package marketplace

// Postgres store tests (sqlmock). The SQL statements are pinned so the
// status/publisher guards (browse is published-only, unlist is
// publisher-org-scoped), the global slug uniqueness mapping (23505 ->
// ErrDuplicateSlug) and the keyset browse (limit+1 overfetch, escaped ILIKE
// pattern, tag-overlap filter) cannot silently regress. Mirrors the
// store-test conventions of internal/billing and internal/secrets.

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"

	"agentos/internal/agents"
)

func newMockStore(t *testing.T) (sqlmock.Sqlmock, Store) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return mock, NewPostgresStore(db)
}

var tsA = time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

func sampleListing(id, slug, status string) *Listing {
	return &Listing{
		ID:              id,
		PublisherOrgID:  "org-a",
		PublisherUserID: "user-a",
		SourceAgentID:   "agent-a",
		VersionSnapshot: `{"name":"Support Bot","description":"d","instructions":"i","model":"m","status":"DRAFT"}`,
		Name:            "Support Bot",
		Slug:            slug,
		Description:     "Helps customers",
		Tags:            []string{"rag", "sql"},
		Status:          status,
		DownloadCount:   3,
		CreatedAt:       tsA,
		UpdatedAt:       tsA,
	}
}

func listingRows(listings ...*Listing) *sqlmock.Rows {
	rows := sqlmock.NewRows([]string{
		"id", "publisher_org_id", "publisher_user_id", "source_agent_id",
		"COALESCE(version_snapshot::text, '{}')", "name", "slug", "description",
		"tags", "status", "download_count", "created_at", "updated_at",
	})
	for _, l := range listings {
		rows.AddRow(l.ID, l.PublisherOrgID, l.PublisherUserID, l.SourceAgentID,
			l.VersionSnapshot, l.Name, l.Slug, l.Description,
			pq.StringArray(l.Tags), l.Status, l.DownloadCount, l.CreatedAt, l.UpdatedAt)
	}
	return rows
}

func TestPostgresStoreNilDBGuard(t *testing.T) {
	store := NewPostgresStore(nil)
	ctx := context.Background()
	pg, ok := store.(*pgStore)
	if !ok {
		t.Fatalf("NewPostgresStore must return a *pgStore")
	}
	if err := pg.guard(); err == nil {
		t.Fatal("nil db must fail the guard")
	}
	if err := store.CreateListing(ctx, sampleListing("l-1", "slug", StatusPublished)); err == nil {
		t.Fatal("CreateListing on nil db should fail")
	}
	if _, err := store.GetListingBySlug(ctx, "slug"); err == nil {
		t.Fatal("GetListingBySlug on nil db should fail")
	}
	if _, _, err := store.BrowseListings(ctx, BrowseOptions{}); err == nil {
		t.Fatal("BrowseListings on nil db should fail")
	}
	if _, err := store.IncrementDownloadCount(ctx, "l-1"); err == nil {
		t.Fatal("IncrementDownloadCount on nil db should fail")
	}
	if err := store.UnlistListing(ctx, "org-a", "slug"); err == nil {
		t.Fatal("UnlistListing on nil db should fail")
	}
}

func TestPostgresStoreCreateListing(t *testing.T) {
	mock, store := newMockStore(t)
	ctx := context.Background()
	listing := sampleListing("l-1", "support-bot", StatusPublished)

	mock.ExpectExec(regexp.QuoteMeta(sqlInsertListing)).
		WithArgs("l-1", "org-a", "user-a", "agent-a", listing.VersionSnapshot,
			"Support Bot", "support-bot", "Helps customers",
			pq.StringArray([]string{"rag", "sql"}), StatusPublished, 3,
			tsA, tsA).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := store.CreateListing(ctx, listing); err != nil {
		t.Fatalf("CreateListing returned error: %v", err)
	}

	// Global slug uniqueness: SQLSTATE 23505 maps to ErrDuplicateSlug.
	mock.ExpectExec(regexp.QuoteMeta(sqlInsertListing)).
		WillReturnError(&pq.Error{Code: "23505", Message: `duplicate key value violates unique constraint "marketplace_listings_slug_key"`})
	if err := store.CreateListing(ctx, listing); !errors.Is(err, ErrDuplicateSlug) {
		t.Fatalf("expected ErrDuplicateSlug, got %v", err)
	}

	// Other driver errors pass through unchanged.
	boom := errors.New("boom")
	mock.ExpectExec(regexp.QuoteMeta(sqlInsertListing)).WillReturnError(boom)
	if err := store.CreateListing(ctx, listing); !errors.Is(err, boom) {
		t.Fatalf("expected passthrough error, got %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPostgresStoreGetListingBySlug(t *testing.T) {
	mock, store := newMockStore(t)
	ctx := context.Background()

	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectListingBySlug)).
		WithArgs("support-bot").
		WillReturnRows(listingRows(sampleListing("l-1", "support-bot", StatusUnlisted)))
	got, err := store.GetListingBySlug(ctx, "support-bot")
	if err != nil {
		t.Fatalf("GetListingBySlug returned error: %v", err)
	}
	if got.ID != "l-1" || got.Status != StatusUnlisted || got.DownloadCount != 3 {
		t.Fatalf("unexpected listing: %+v", got)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "rag" || got.Tags[1] != "sql" {
		t.Fatalf("tags not scanned: %v", got.Tags)
	}
	if !got.CreatedAt.Equal(tsA) {
		t.Fatalf("created_at not scanned: %v", got.CreatedAt)
	}

	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectListingBySlug)).
		WithArgs("nope").
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	if _, err := store.GetListingBySlug(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPostgresStoreBrowseListings(t *testing.T) {
	mock, store := newMockStore(t)
	ctx := context.Background()

	// First page, no filters: pattern "%%", empty tag array (nil), NULL
	// cursor, limit+1 overfetch. Three rows for limit 2 -> truncated page
	// with a cursor anchored at the second row.
	older := sampleListing("l-old", "older", StatusPublished)
	older.CreatedAt = tsA.Add(-time.Minute)
	oldest := sampleListing("l-oldest", "oldest", StatusPublished)
	oldest.CreatedAt = tsA.Add(-2 * time.Minute)

	mock.ExpectQuery(regexp.QuoteMeta(sqlBrowseListings)).
		WithArgs("%%", nil, nil, "", 3).
		WillReturnRows(listingRows(sampleListing("l-1", "newest", StatusPublished), older, oldest))
	page, next, err := store.BrowseListings(ctx, BrowseOptions{Limit: 2})
	if err != nil {
		t.Fatalf("BrowseListings returned error: %v", err)
	}
	if len(page) != 2 || page[0].Slug != "newest" || page[1].Slug != "older" {
		t.Fatalf("unexpected page: %+v", page)
	}
	if next == "" {
		t.Fatal("truncated page must emit a cursor")
	}
	cursorTime, cursorID, err := decodeCursor(next)
	if err != nil {
		t.Fatalf("decodeCursor: %v", err)
	}
	if cursorID != "l-old" || !cursorTime.Equal(older.CreatedAt) {
		t.Fatalf("cursor must encode the last returned row, got %s", next)
	}

	// Exact-fit page: two rows for limit 2 -> no cursor.
	mock.ExpectQuery(regexp.QuoteMeta(sqlBrowseListings)).
		WithArgs("%%", nil, nil, "", 3).
		WillReturnRows(listingRows(sampleListing("l-1", "newest", StatusPublished), older))
	page, next, err = store.BrowseListings(ctx, BrowseOptions{Limit: 2})
	if err != nil {
		t.Fatalf("BrowseListings(2) returned error: %v", err)
	}
	if len(page) != 2 || next != "" {
		t.Fatalf("exact-fit page must emit no cursor, got %d rows next=%q", len(page), next)
	}

	// Filters + cursor: escaped pattern, tag array, cursor args.
	cursor := encodeCursor(older)
	mock.ExpectQuery(regexp.QuoteMeta(sqlBrowseListings)).
		WithArgs(`%50\% off\_\%,%`, pq.StringArray([]string{"rag", "sql"}), older.CreatedAt, "l-old", 3).
		WillReturnRows(listingRows(oldest))
	page, next, err = store.BrowseListings(ctx, BrowseOptions{
		Query:  `50% off_%,`,
		Tags:   []string{"rag", "sql"},
		Limit:  2,
		Cursor: cursor,
	})
	if err != nil {
		t.Fatalf("BrowseListings(filtered) returned error: %v", err)
	}
	if len(page) != 1 || next != "" {
		t.Fatalf("unexpected filtered page: %d rows next=%q", len(page), next)
	}

	// Invalid cursor surfaces as ErrInvalidCursor.
	if _, _, err := store.BrowseListings(ctx, BrowseOptions{Limit: 2, Cursor: "!!"}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("expected ErrInvalidCursor, got %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestLikePatternEscapesMetacharacters(t *testing.T) {
	cases := map[string]string{
		"":           "%%",
		"plain":      "%plain%",
		`50% off_%,`: `%50\% off\_\%,%`,
		`back\slash`: `%back\\slash%`,
		"unicode ✓":  "%unicode ✓%",
	}
	for query, want := range cases {
		if got := likePattern(query); got != want {
			t.Errorf("likePattern(%q) = %q, want %q", query, got, want)
		}
	}
}

func TestPostgresStoreIncrementDownloadCount(t *testing.T) {
	mock, store := newMockStore(t)
	ctx := context.Background()

	mock.ExpectQuery(regexp.QuoteMeta(sqlIncrementDownloadCount)).
		WithArgs("l-1").
		WillReturnRows(sqlmock.NewRows([]string{"download_count"}).AddRow(7))
	count, err := store.IncrementDownloadCount(ctx, "l-1")
	if err != nil {
		t.Fatalf("IncrementDownloadCount returned error: %v", err)
	}
	if count != 7 {
		t.Fatalf("expected new count 7, got %d", count)
	}

	mock.ExpectQuery(regexp.QuoteMeta(sqlIncrementDownloadCount)).
		WithArgs("ghost").
		WillReturnRows(sqlmock.NewRows([]string{"download_count"}))
	if _, err := store.IncrementDownloadCount(ctx, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPostgresStoreUnlistListing(t *testing.T) {
	mock, store := newMockStore(t)
	ctx := context.Background()

	mock.ExpectExec(regexp.QuoteMeta(sqlUnlistListing)).
		WithArgs("support-bot", "org-a", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.UnlistListing(ctx, "org-a", "support-bot"); err != nil {
		t.Fatalf("UnlistListing returned error: %v", err)
	}

	// Unknown slug OR foreign publisher org: 0 rows -> ErrNotFound (the
	// org-guarded UPDATE makes both indistinguishable: no existence leak).
	mock.ExpectExec(regexp.QuoteMeta(sqlUnlistListing)).
		WithArgs("support-bot", "org-a", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := store.UnlistListing(ctx, "org-a", "support-bot"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

// TestServiceOverPostgresStoreRoundTrip is the Postgres-mode publish ->
// browse -> get -> install -> unlist round trip: the marketplace service runs
// on a mocked pgStore while the agents domain stays a REAL in-memory
// *agents.Service, proving the dual-mode wiring (durable listings + live
// agents domain) end to end.
func TestServiceOverPostgresStoreRoundTrip(t *testing.T) {
	mock, store := newMockStore(t)
	agentsSvc := agents.NewService()
	ctx := context.Background()

	source, err := agentsSvc.CreateAgentCtx(ctx, "org-a", "Support Bot", "Helps customers", "Be polite", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("CreateAgentCtx: %v", err)
	}
	svc := NewServiceWithStore(store, agentsSvc, nil)

	// Publish: slug pre-check (no rows) + INSERT. The generated listing id
	// and timestamps are opaque; the CONFIG SNAPSHOT is pinned exactly —
	// the live-config document must be the agents.AgentSnapshot shape.
	expectedSnapshot := `{"name":"Support Bot","description":"Helps customers","instructions":"Be polite","model":"gpt-4o-mini","status":"DRAFT"}`
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectListingBySlug)).
		WithArgs("support-bot").
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectExec(regexp.QuoteMeta(sqlInsertListing)).
		WithArgs(sqlmock.AnyArg(), "org-a", "user-a", source.ID, expectedSnapshot,
			"Support Bot", "support-bot", "Helps customers",
			pq.StringArray([]string{"rag", "sql"}), StatusPublished, 0,
			sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	published, err := svc.Publish(ctx, "org-a", "user-a", PublishInput{
		AgentID: source.ID, Name: "Support Bot", Description: "Helps customers", Tags: []string{"rag", "sql"},
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// Duplicate slug: pre-check finds the existing row -> ErrDuplicateSlug
	// (no INSERT is attempted).
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectListingBySlug)).
		WithArgs("support-bot").
		WillReturnRows(listingRows(published))
	if _, err := svc.Publish(ctx, "org-a", "user-a", PublishInput{AgentID: source.ID, Name: "Support Bot"}); !errors.Is(err, ErrDuplicateSlug) {
		t.Fatalf("expected ErrDuplicateSlug, got %v", err)
	}

	// Browse: one published row (under the default limit -> no cursor).
	mock.ExpectQuery(regexp.QuoteMeta(sqlBrowseListings)).
		WithArgs("%%", nil, nil, "", DefaultBrowseLimit+1).
		WillReturnRows(listingRows(published))
	page, next, err := svc.Browse(ctx, BrowseOptions{})
	if err != nil {
		t.Fatalf("Browse: %v", err)
	}
	if len(page) != 1 || next != "" || page[0].Slug != "support-bot" {
		t.Fatalf("unexpected browse page: %+v next=%q", page, next)
	}

	// Install into org-b: point lookup + atomic counter bump; the new agent
	// lands in the REAL agents service under org-b.
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectListingBySlug)).
		WithArgs("support-bot").
		WillReturnRows(listingRows(published))
	mock.ExpectQuery(regexp.QuoteMeta(sqlIncrementDownloadCount)).
		WithArgs(published.ID).
		WillReturnRows(sqlmock.NewRows([]string{"download_count"}).AddRow(1))
	result, err := svc.Install(ctx, "org-b", "support-bot")
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if result.Agent.OrganizationID != "org-b" || result.Agent.Instructions != "Be polite" {
		t.Fatalf("unexpected install result: %+v", result.Agent)
	}
	if result.Listing.DownloadCount != 1 {
		t.Fatalf("download_count must reflect the store bump, got %d", result.Listing.DownloadCount)
	}

	// Unlist: publisher-org-guarded UPDATE; then a foreign unlist of the same
	// slug finds 0 rows (status flip is org-scoped) -> ErrNotFound.
	mock.ExpectExec(regexp.QuoteMeta(sqlUnlistListing)).
		WithArgs("support-bot", "org-a", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := svc.Unlist(ctx, "org-a", "support-bot"); err != nil {
		t.Fatalf("Unlist: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}
