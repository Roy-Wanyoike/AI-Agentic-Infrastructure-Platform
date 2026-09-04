package events

// list.go adds the keyset-paginated read side used by GET /v1/events
// (issue #56). The trail stays append-only; this file only adds read-side
// pagination with the same (created_at, id) keyset semantics in every mode,
// mirroring how AppendEvent splits memory vs store:
//
//   - Postgres store (pgStore): the keyset predicate runs in SQL;
//   - MemoryStore: the zero-infrastructure persistence half — an unbounded,
//     mutex-guarded slice implementing the same Store contract as pgStore
//     (AppendEvent) plus the native paged listing;
//   - MemoryPublisher: its ring buffer is served read-only through the same
//     paging helper (best-effort diagnostics view; the ring evicts old rows);
//   - AuditPublisher: read-through decorator, so a caller holding only the
//     publisher (the cmd/api wiring) can reach the store behind it.
//
// internal/audit/list.go (issue #18) established this exact shape; the
// ordering, cursor encoding and exhaustion rules are identical so both feeds
// behave the same on the dashboard.

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultListLimit bounds a page when the caller does not ask for one.
	DefaultListLimit = 50
	// MaxListLimit caps a single page so one request cannot scan a whole
	// tenant history (defense in depth; callers may always ask for less).
	MaxListLimit = 200
)

var (
	// ErrInvalidCursor is returned when a paging cursor cannot be decoded.
	ErrInvalidCursor = errors.New("events: invalid cursor")
	// ErrOrgRequired is returned when a listing is attempted without a
	// tenant scope (defense in depth: the HTTP layer derives the org from
	// the auth claims and never accepts a blank one).
	ErrOrgRequired = errors.New("events: organization id is required")
	// ErrListUnsupported is returned by a read-through decorator whose
	// backing store cannot serve listings (Store only guarantees AppendEvent).
	ErrListUnsupported = errors.New("events: store does not support listing")
)

// EventFilter carries the optional read-side filters of the events listing.
// The zero value lists every event of the tenant. EntityType/EntityID map to
// the resource_type/resource_id columns (the domain object an event is
// about), Type to the pinned contract event type, and Since bounds the page
// from below (created_at >= Since, RFC3339 at the HTTP layer).
type EventFilter struct {
	EntityType string
	EntityID   string
	Type       string
	Since      time.Time
}

// PagedStore is implemented by stores that can serve keyset-paginated
// listings natively. The Store contract only guarantees AppendEvent; callers
// must type-assert before listing.
type PagedStore interface {
	// ListEventsPaged returns one page (newest first) plus the cursor for
	// the next page ("" when the trail is exhausted).
	ListEventsPaged(ctx context.Context, orgID string, filter EventFilter, limit int, cursor string) ([]Event, string, error)
}

// NormalizeLimit clamps a requested page size into [1, MaxListLimit]; values
// <= 0 fall back to DefaultListLimit.
func NormalizeLimit(limit int) int {
	if limit <= 0 {
		return DefaultListLimit
	}
	if limit > MaxListLimit {
		return MaxListLimit
	}
	return limit
}

// ---------------------------------------------------------------------------
// Shared in-memory paging (identical semantics in every non-SQL mode)
// ---------------------------------------------------------------------------

// pageEvents applies the shared keyset-pagination semantics to an already
// org-filtered slice: apply the filters, order by (timestamp DESC, id DESC),
// skip everything at or after the cursor position, and return up to limit
// rows plus the next cursor ("" when the slice is exhausted).
func pageEvents(items []Event, filter EventFilter, limit int, cursor string) ([]Event, string, error) {
	matches := make([]Event, 0, len(items))
	for _, event := range items {
		if !eventMatches(&event, &filter) {
			continue
		}
		matches = append(matches, event)
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if !matches[i].Timestamp.Equal(matches[j].Timestamp) {
			return matches[i].Timestamp.After(matches[j].Timestamp)
		}
		return matches[i].ID > matches[j].ID
	})

	start := 0
	if strings.TrimSpace(cursor) != "" {
		cursorTime, cursorID, err := decodeCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		start = len(matches)
		for i := range matches {
			if eventBefore(&matches[i], cursorTime, cursorID) {
				start = i
				break
			}
		}
	}

	next := ""
	end := len(matches)
	if start+limit < len(matches) {
		end = start + limit
		next = encodeCursor(&matches[end-1])
	}
	if start >= len(matches) {
		return make([]Event, 0), "", nil
	}
	page := matches[start:end]
	out := make([]Event, len(page))
	copy(out, page)
	return out, next, nil
}

// eventMatches reports whether one event satisfies the read filters (every
// unset filter matches everything).
func eventMatches(event *Event, filter *EventFilter) bool {
	if filter == nil {
		return true
	}
	if filter.Type != "" && event.Type != filter.Type {
		return false
	}
	if filter.EntityType != "" && event.Resource.Type != filter.EntityType {
		return false
	}
	if filter.EntityID != "" && event.Resource.ID != filter.EntityID {
		return false
	}
	if !filter.Since.IsZero() && event.Timestamp.Before(filter.Since) {
		return false
	}
	return true
}

// eventBefore reports whether event sorts strictly before the cursor key
// (timestamp DESC, id DESC): smaller timestamps first, then smaller ids.
func eventBefore(event *Event, cursorTime time.Time, cursorID string) bool {
	if event.Timestamp.Equal(cursorTime) {
		return event.ID < cursorID
	}
	return event.Timestamp.Before(cursorTime)
}

// encodeCursor turns the last event of a page into the opaque next-page
// cursor: base64url("RFC3339Nano(timestamp)|id").
func encodeCursor(event *Event) string {
	if event == nil {
		return ""
	}
	raw := event.Timestamp.UTC().Format(time.RFC3339Nano) + "|" + event.ID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeCursor reverses encodeCursor. Any malformed payload (bad base64,
// missing separator, unparsable timestamp) is ErrInvalidCursor.
func decodeCursor(cursor string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(cursor))
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%w: not base64url", ErrInvalidCursor)
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 || parts[1] == "" {
		return time.Time{}, "", fmt.Errorf("%w: missing id segment", ErrInvalidCursor)
	}
	t, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%w: unparsable timestamp", ErrInvalidCursor)
	}
	return t.UTC(), parts[1], nil
}

// ---------------------------------------------------------------------------
// MemoryStore — the zero-infrastructure half of the Store split
// ---------------------------------------------------------------------------

// MemoryStore is an unbounded in-memory Store: the zero-infrastructure
// counterpart of pgStore.AppendEvent. It validates and normalizes exactly
// like the Postgres store (same ensureDefaults/validate path) so events are
// contract-complete in both modes, and it serves keyset-paginated listings
// natively (PagedStore) so GET /v1/events works without infrastructure.
// Appends never evict (unlike the MemoryPublisher ring), which keeps cursor
// walks stable under concurrent inserts.
type MemoryStore struct {
	mu     sync.Mutex
	events []Event
}

// NewMemoryStore returns an empty in-memory event store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{events: make([]Event, 0)}
}

// AppendEvent implements the Store contract with pgStore semantics: validate
// + defaults + append (never update or delete).
func (s *MemoryStore) AppendEvent(_ context.Context, event *Event) error {
	if s == nil {
		return errors.New("events: memory store is nil")
	}
	if event == nil {
		return ErrInvalidEvent
	}
	stored := *event
	ensureDefaults(&stored)
	if err := validate(&stored); err != nil {
		return err
	}
	if stored.Payload != nil {
		payload := make(map[string]any, len(stored.Payload))
		for k, v := range stored.Payload {
			payload[k] = v
		}
		stored.Payload = payload
	}
	s.mu.Lock()
	s.events = append(s.events, stored)
	s.mu.Unlock()
	return nil
}

// ListEventsPaged implements PagedStore: one keyset page of the tenant's
// events, newest first, plus the next-page cursor ("" = exhausted).
func (s *MemoryStore) ListEventsPaged(_ context.Context, orgID string, filter EventFilter, limit int, cursor string) ([]Event, string, error) {
	if s == nil {
		return nil, "", errors.New("events: memory store is nil")
	}
	if strings.TrimSpace(orgID) == "" {
		return nil, "", ErrOrgRequired
	}
	limit = NormalizeLimit(limit)
	s.mu.Lock()
	orgEvents := make([]Event, 0, len(s.events))
	for _, event := range s.events {
		// Tenant guard: only the caller's organization_id is visible.
		if event.TenantID == orgID {
			orgEvents = append(orgEvents, event)
		}
	}
	s.mu.Unlock()
	return pageEvents(orgEvents, filter, limit, cursor)
}

// ---------------------------------------------------------------------------
// MemoryPublisher ring-buffer read side (best-effort diagnostics view)
// ---------------------------------------------------------------------------

// ListEventsPaged serves keyset pages from the publisher's ring buffer. The
// ring evicts the oldest rows once full, so walks that outlive an eviction
// may legitimately miss evicted rows — the keyset predicate itself stays
// duplicate-free. Only the requested tenant's rows are ever returned.
func (m *MemoryPublisher) ListEventsPaged(ctx context.Context, orgID string, filter EventFilter, limit int, cursor string) ([]Event, string, error) {
	if m == nil {
		return nil, "", errors.New("events: memory publisher is nil")
	}
	if strings.TrimSpace(orgID) == "" {
		return nil, "", ErrOrgRequired
	}
	limit = NormalizeLimit(limit)
	snapshot := m.Snapshot()
	orgEvents := make([]Event, 0, len(snapshot))
	for _, event := range snapshot {
		if event.TenantID == orgID {
			orgEvents = append(orgEvents, event)
		}
	}
	return pageEvents(orgEvents, filter, limit, cursor)
}

// ---------------------------------------------------------------------------
// AuditPublisher read-through
// ---------------------------------------------------------------------------

// ListEventsPaged forwards the listing to the backing store when it supports
// keyset pagination. A pass-through decorator (nil store) or a store that
// only implements Store answers ErrListUnsupported: the events contract has
// no unpaginated read to fall back on (append-only by design).
func (a *AuditPublisher) ListEventsPaged(ctx context.Context, orgID string, filter EventFilter, limit int, cursor string) ([]Event, string, error) {
	if a == nil {
		return nil, "", ErrListUnsupported
	}
	paged, ok := a.store.(PagedStore)
	if !ok {
		return nil, "", ErrListUnsupported
	}
	return paged.ListEventsPaged(ctx, orgID, filter, limit, cursor)
}

// ---------------------------------------------------------------------------
// Postgres keyset pagination (same table pgStore.AppendEvent writes)
// ---------------------------------------------------------------------------

// scanEventColumns is the projection shared by every listing query. The
// nullable text columns are COALESCEd to ” and the JSONB payload to its
// text form so a single scan shape covers every row.
const scanEventColumns = `id, organization_id, type, COALESCE(project_id, ''), COALESCE(resource_type, ''), COALESCE(resource_id, ''), COALESCE(execution_id, ''), COALESCE(trace_id, ''), COALESCE(payload::text, ''), created_at`

// ListEventsPaged implements PagedStore (issue #56): one keyset page of the
// tenant's events, newest first, plus the next-page cursor ("" = exhausted).
// The limit is clamped again here so direct callers get the same bounds as
// NormalizeLimit callers, and the fetch takes limit+1 rows so the next cursor
// is only emitted when a follow-up page actually exists. Every predicate is
// AND-composed onto the organization_id tenant guard, so filters and cursor
// stay stable across pages.
func (s *pgStore) ListEventsPaged(ctx context.Context, orgID string, filter EventFilter, limit int, cursor string) ([]Event, string, error) {
	if s == nil || s.db == nil {
		return nil, "", errors.New("events: database is nil")
	}
	if strings.TrimSpace(orgID) == "" {
		return nil, "", ErrOrgRequired
	}
	limit = NormalizeLimit(limit)

	// Tenant guard first: every additional predicate composes onto it.
	where := "organization_id = $1"
	args := []any{orgID}
	if filter.Type != "" {
		where += fmt.Sprintf(" AND type = $%d", len(args)+1)
		args = append(args, filter.Type)
	}
	if filter.EntityType != "" {
		where += fmt.Sprintf(" AND resource_type = $%d", len(args)+1)
		args = append(args, filter.EntityType)
	}
	if filter.EntityID != "" {
		where += fmt.Sprintf(" AND resource_id = $%d", len(args)+1)
		args = append(args, filter.EntityID)
	}
	if !filter.Since.IsZero() {
		where += fmt.Sprintf(" AND created_at >= $%d", len(args)+1)
		args = append(args, filter.Since)
	}
	if strings.TrimSpace(cursor) != "" {
		before, beforeID, err := decodeCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		// Keyset predicate: the row-value comparison (a Postgres feature)
		// keeps (created_at DESC, id DESC) walks duplicate- and skip-free.
		where += fmt.Sprintf(" AND (created_at, id) < ($%d, $%d)", len(args)+1, len(args)+2)
		args = append(args, before, beforeID)
	}
	args = append(args, limit+1)
	query := "SELECT " + scanEventColumns + " FROM events WHERE " + where +
		" ORDER BY created_at DESC, id DESC LIMIT $" + strconv.Itoa(len(args))

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out, err := scanEventRows(rows)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		next = encodeCursor(&out[len(out)-1])
	}
	return out, next, nil
}

// scanEventRows drains an events listing result set (matching
// scanEventColumns order).
func scanEventRows(rows *sql.Rows) ([]Event, error) {
	out := make([]Event, 0)
	for rows.Next() {
		var (
			event      Event
			projectID  string
			entityType string
			entityID   string
			execID     string
			traceID    string
			payload    string
		)
		if err := rows.Scan(&event.ID, &event.TenantID, &event.Type, &projectID, &entityType, &entityID, &execID, &traceID, &payload, &event.Timestamp); err != nil {
			return nil, err
		}
		event.ProjectID = projectID
		event.Resource = Resource{Type: entityType, ID: entityID}
		event.ExecutionID = execID
		event.TraceID = traceID
		if payload != "" {
			decoded := map[string]any{}
			if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
				return nil, err
			}
			event.Payload = decoded
		}
		out = append(out, event)
	}
	return out, rows.Err()
}
