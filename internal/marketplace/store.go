package marketplace

// store.go — Postgres-backed Store (lib/pq driver; the scheduler/secrets
// store pattern). Every statement is tenant/status guarded:
//
//   - BrowseListings only ever reads status='published' rows — the global
//     catalog never leaks drafts or unlisted rows through the browse path;
//   - UnlistListing filters publisher_org_id, so a foreign org cannot unlist
//     (or even confirm the existence of) another org's listing: 0 rows
//     affected maps to ErrNotFound;
//   - GetListingBySlug is status-agnostic BY DESIGN (publisher-private point
//     lookups for drafts) — visibility is enforced by the service, which
//     passes the caller's org id.
//
// The keyset browse runs entirely in SQL: ILIKE substring match on
// name/description (LIKE metacharacters escaped so both modes match
// literally), ANY-overlap tag filter over the GIN-indexed tags column, and a
// (created_at, id) keyset predicate with a limit+1 overfetch so an exact-fit
// page emits no next cursor.

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/lib/pq"
)

const (
	// listingColumns is the full-row projection; version_snapshot is coerced
	// to text (NULL cannot occur — the column is NOT NULL — but COALESCE
	// keeps the scan total).
	listingColumns = `id, publisher_org_id, publisher_user_id, source_agent_id,
                COALESCE(version_snapshot::text, '{}'), name, slug, description,
                tags, status, download_count, created_at, updated_at`

	sqlInsertListing = `INSERT INTO marketplace_listings
                (id, publisher_org_id, publisher_user_id, source_agent_id, version_snapshot,
                 name, slug, description, tags, status, download_count, created_at, updated_at)
                VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`

	sqlSelectListingBySlug = `SELECT ` + listingColumns + ` FROM marketplace_listings
                WHERE slug = $1`

	// $1 ILIKE pattern over both text columns (pattern is pre-escaped by
	// escapeLike, so an empty query degrades to '%%' = match all);
	// $2 tags ANY-overlap (empty array matches nothing via cardinality);
	// $3/$4 (created_at, id) keyset cursor (NULL timestamp = first page);
	// $5 = limit+1 overfetch (the service trims and derives the cursor).
	sqlBrowseListings = `SELECT ` + listingColumns + ` FROM marketplace_listings
                WHERE status = 'published'
                  AND (name ILIKE $1 OR description ILIKE $1)
                  AND (COALESCE(cardinality($2::text[]), 0) = 0 OR tags && $2::text[])
                  AND ($3::timestamptz IS NULL OR (created_at, id) < ($3::timestamptz, $4))
                ORDER BY created_at DESC, id DESC
                LIMIT $5`

	sqlIncrementDownloadCount = `UPDATE marketplace_listings
                SET download_count = download_count + 1
                WHERE id = $1
                RETURNING download_count`

	// Publisher-org guard: only the owning org can unlist; 0 rows affected
	// (unknown slug OR foreign org) -> ErrNotFound (no existence leak).
	sqlUnlistListing = `UPDATE marketplace_listings
                SET status = 'unlisted', updated_at = $3
                WHERE slug = $1 AND publisher_org_id = $2`
)

// Store abstracts durable listing storage. Implementations MUST keep the
// browse path published-only and the unlist path publisher-org-guarded.
type Store interface {
	// CreateListing inserts one listing row (slug conflicts surface as
	// ErrDuplicateSlug).
	CreateListing(ctx context.Context, listing *Listing) error
	// GetListingBySlug fetches one listing by its globally-unique slug,
	// regardless of status (service enforces visibility).
	GetListingBySlug(ctx context.Context, slug string) (*Listing, error)
	// BrowseListings returns one published-only page plus the next-page
	// cursor ("" when exhausted). limit is already normalized; the store
	// overfetches by one row to detect an exact-fit page.
	BrowseListings(ctx context.Context, opts BrowseOptions) ([]*Listing, string, error)
	// IncrementDownloadCount bumps the install counter atomically and
	// returns the new value.
	IncrementDownloadCount(ctx context.Context, listingID string) (int, error)
	// UnlistListing flips status to unlisted for the PUBLISHER org only;
	// unknown or foreign slugs surface as ErrNotFound.
	UnlistListing(ctx context.Context, publisherOrgID, slug string) error
}

// pgStore is the Postgres Store implementation.
type pgStore struct {
	db *sql.DB
}

// NewPostgresStore returns a Store backed by *sql.DB.
func NewPostgresStore(db *sql.DB) Store {
	return &pgStore{db: db}
}

func (s *pgStore) guard() error {
	if s == nil || s.db == nil {
		return errors.New("marketplace: database is nil")
	}
	return nil
}

// mapConstraintErr translates Postgres unique violations (the global
// UNIQUE(slug) constraint, SQLSTATE 23505) into ErrDuplicateSlug.
func mapConstraintErr(err error) error {
	var pqErr *pq.Error
	if errors.As(err, &pqErr) && pqErr.Code == "23505" {
		return ErrDuplicateSlug
	}
	return err
}

func (s *pgStore) CreateListing(ctx context.Context, listing *Listing) error {
	if err := s.guard(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, sqlInsertListing,
		listing.ID, listing.PublisherOrgID, listing.PublisherUserID,
		listing.SourceAgentID, listing.VersionSnapshot,
		listing.Name, listing.Slug, listing.Description,
		pq.StringArray(listing.Tags), listing.Status, listing.DownloadCount,
		listing.CreatedAt, listing.UpdatedAt)
	return mapConstraintErr(err)
}

func (s *pgStore) GetListingBySlug(ctx context.Context, slug string) (*Listing, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	return scanListing(s.db.QueryRowContext(ctx, sqlSelectListingBySlug, slug))
}

func (s *pgStore) BrowseListings(ctx context.Context, opts BrowseOptions) ([]*Listing, string, error) {
	if err := s.guard(); err != nil {
		return nil, "", err
	}
	pattern := likePattern(opts.Query)
	var (
		tags       pq.StringArray
		cursorTime any // nil => first page ($3::timestamptz IS NULL short-circuits)
		cursorID   string
	)
	if len(opts.Tags) > 0 {
		tags = pq.StringArray(opts.Tags)
	}
	if opts.Cursor != "" {
		t, id, err := decodeCursor(opts.Cursor)
		if err != nil {
			return nil, "", err
		}
		cursorTime = t
		cursorID = id
	}
	// Overfetch by one to distinguish an exact-fit page (no cursor) from a
	// truncated one — identical semantics to the in-memory pager.
	rows, err := s.db.QueryContext(ctx, sqlBrowseListings,
		pattern, tags, cursorTime, cursorID, opts.Limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	out := make([]*Listing, 0, opts.Limit+1)
	for rows.Next() {
		listing, err := scanListing(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, listing)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > opts.Limit {
		next = encodeCursor(out[opts.Limit-1])
		out = out[:opts.Limit]
	}
	return out, next, nil
}

func (s *pgStore) IncrementDownloadCount(ctx context.Context, listingID string) (int, error) {
	if err := s.guard(); err != nil {
		return 0, err
	}
	var count int
	err := s.db.QueryRowContext(ctx, sqlIncrementDownloadCount, listingID).Scan(&count)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	return count, nil
}

func (s *pgStore) UnlistListing(ctx context.Context, publisherOrgID, slug string) error {
	if err := s.guard(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlUnlistListing, slug, publisherOrgID, time.Now().UTC())
	if err != nil {
		return err
	}
	if affected, err := res.RowsAffected(); err == nil && affected == 0 {
		return ErrNotFound
	}
	return nil
}

// scanner abstracts *sql.Row / *sql.Rows for the listing projection.
type scanner interface {
	Scan(dest ...any) error
}

func scanListing(sc scanner) (*Listing, error) {
	var (
		listing   Listing
		tags      pq.StringArray
		createdAt time.Time
		updatedAt time.Time
	)
	err := sc.Scan(&listing.ID, &listing.PublisherOrgID, &listing.PublisherUserID,
		&listing.SourceAgentID, &listing.VersionSnapshot,
		&listing.Name, &listing.Slug, &listing.Description,
		&tags, &listing.Status, &listing.DownloadCount,
		&createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	listing.Tags = []string(tags)
	listing.CreatedAt = createdAt
	listing.UpdatedAt = updatedAt
	return &listing, nil
}

// likePattern builds the ILIKE substring pattern for the text query. LIKE
// metacharacters are escaped so both store modes interpret the query
// literally (the in-memory browse uses strings.Contains).
func likePattern(query string) string {
	escaped := strings.NewReplacer(
		`\`, `\\`,
		`%`, `\%`,
		`_`, `\_`,
	).Replace(query)
	return "%" + escaped + "%"
}
