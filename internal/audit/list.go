package audit

// list.go adds the keyset-paginated listing used by GET /v1/audit-events
// (issue #18). The trail stays append-only; this file only adds read-side
// pagination with the same (created_at, id) keyset semantics in both modes:
//   - Postgres store: the keyset predicate runs in SQL (see store.go);
//   - in-memory service (or a store that only implements Store): the page is
//     computed over the org-filtered slice with an identical ordering.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	// DefaultListLimit bounds a page when the caller does not ask for one.
	DefaultListLimit = 50
	// MaxListLimit caps a single page so one request cannot scan a whole
	// tenant history (defense in depth; callers may always ask for less).
	MaxListLimit = 200
)

// ErrInvalidCursor is returned when a paging cursor cannot be decoded.
var ErrInvalidCursor = errors.New("audit: invalid cursor")

// PagedStore is implemented by stores that can serve keyset-paginated
// listings natively. The Store contract only guarantees the unpaginated
// listing; services fall back to in-memory paging for those.
type PagedStore interface {
	// ListEntriesPaged returns one page (newest first) plus the cursor for
	// the next page ("" when the trail is exhausted).
	ListEntriesPaged(ctx context.Context, orgID string, limit int, cursor string) ([]*Entry, string, error)
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

// ListEntriesPaged returns one page of the tenant's audit trail, newest
// first. An empty cursor starts a fresh listing; the returned nextCursor is
// "" when the trail is exhausted. limit is clamped via NormalizeLimit, so the
// service — not the HTTP layer — is the point of truth for page bounds.
func (s *Service) ListEntriesPaged(ctx context.Context, orgID string, limit int, cursor string) ([]*Entry, string, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, "", ErrOrgRequired
	}
	limit = NormalizeLimit(limit)

	// Snapshot the store under the lock so delegation and fallback observe
	// the same backend even if SetStore races a listing (SetStore is a
	// boot-time operation, but the listing must not read a torn state).
	s.mu.Lock()
	paged, isPaged := s.store.(PagedStore)
	store := s.store
	s.mu.Unlock()

	if isPaged {
		return paged.ListEntriesPaged(ctx, orgID, limit, cursor)
	}

	// In-memory mode, or a store that predates PagedStore: fetch the whole
	// tenant trail and page it with the same keyset semantics as Postgres.
	var items []*Entry
	if store != nil {
		all, err := store.ListEntries(ctx, orgID)
		if err != nil {
			return nil, "", err
		}
		items = all
	} else {
		s.mu.Lock()
		for _, entry := range s.items {
			if entry.OrganizationID == orgID {
				items = append(items, entry)
			}
		}
		s.mu.Unlock()
	}
	if items == nil {
		items = make([]*Entry, 0)
	}
	return pageEntries(items, limit, cursor)
}

// pageEntries applies the shared keyset-pagination semantics to an already
// org-filtered slice: order by (created_at DESC, id DESC), skip everything at
// or after the cursor position, and return up to limit rows plus the next
// cursor ("" when the slice is exhausted).
func pageEntries(items []*Entry, limit int, cursor string) ([]*Entry, string, error) {
	sorted := make([]*Entry, len(items))
	copy(sorted, items)
	sort.SliceStable(sorted, func(i, j int) bool {
		if !sorted[i].CreatedAt.Equal(sorted[j].CreatedAt) {
			return sorted[i].CreatedAt.After(sorted[j].CreatedAt)
		}
		return sorted[i].ID > sorted[j].ID
	})

	start := 0
	if strings.TrimSpace(cursor) != "" {
		cursorTime, cursorID, err := decodeCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		start = len(sorted)
		for i, entry := range sorted {
			if entryBefore(entry, cursorTime, cursorID) {
				start = i
				break
			}
		}
	}

	next := ""
	end := len(sorted)
	if start+limit < len(sorted) {
		end = start + limit
		next = encodeCursor(sorted[end-1])
	}
	if start >= len(sorted) {
		return make([]*Entry, 0), "", nil
	}
	page := sorted[start:end]
	out := make([]*Entry, len(page))
	copy(out, page)
	return out, next, nil
}

// entryBefore reports whether entry sorts strictly before the cursor key
// (created_at DESC, id DESC): smaller timestamps first, then smaller ids.
func entryBefore(entry *Entry, cursorTime time.Time, cursorID string) bool {
	if entry.CreatedAt.Equal(cursorTime) {
		return entry.ID < cursorID
	}
	return entry.CreatedAt.Before(cursorTime)
}

// encodeCursor turns the last entry of a page into the opaque next-page
// cursor: base64url("RFC3339Nano(created_at)|id").
func encodeCursor(entry *Entry) string {
	if entry == nil {
		return ""
	}
	raw := entry.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + entry.ID
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
