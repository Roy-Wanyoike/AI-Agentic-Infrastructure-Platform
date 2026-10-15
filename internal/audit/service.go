package audit

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type Entry struct {
	ID             string
	Actor          string
	Action         string
	OrganizationID string
	Resource       string
	Metadata       map[string]any
	CreatedAt      time.Time
}

// Store persists audit entries. Listing is guarded by organization_id so a
// tenant can only ever read its own audit trail.
type Store interface {
	// InsertEntry appends one audit row with its organization_id scope.
	InsertEntry(ctx context.Context, entry *Entry) error
	// ListEntries returns the audit trail of exactly one tenant.
	ListEntries(ctx context.Context, orgID string) ([]*Entry, error)
}

// Entries are immutable by contract: they are stamped with their chain hash
// at write time (see chain.go) and no API mutates or deletes a row afterwards.
type Service struct {
	mu    sync.Mutex
	items []*Entry
	store Store
	// heads caches the per-org chain position (head hash + trail length);
	// LogCtx extends it on insert under mu, ChainHead fills it on a miss.
	heads map[string]chainHeadState
	// lastStamp is the logical clock floor: LogCtx never stamps two rows
	// with the same (or backwards) CreatedAt, so the canonical chain
	// order (created_at ASC, id ASC) always matches insertion order even
	// on coarse clocks.
	lastStamp time.Time
}

func NewService() *Service { return &Service{items: make([]*Entry, 0)} }

// NewServiceWithStore returns a service that also persists every entry to the
// durable store; the in-memory slice remains for legacy List() callers.
func NewServiceWithStore(store Store) *Service {
	s := NewService()
	s.store = store
	return s
}

func (s *Service) SetStore(store Store) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store = store
	// A store swap changes the listing source: cached per-org chain heads
	// were computed against the previous backend and must be recomputed.
	s.heads = nil
}

// Log is the legacy context-free entry point; see LogCtx. It keeps the
// historical signature and swallows persistence errors (best-effort audit).
func (s *Service) Log(actor, action, organizationID, resource string) *Entry {
	entry, err := s.LogCtx(context.Background(), actor, action, organizationID, resource, nil)
	if err != nil {
		return nil
	}
	return entry
}

// LogCtx records an audit entry for one tenant (organization_id scope) and
// stamps it into the tamper-evident chain (chain.go): under the service mutex
// the previous per-org head is resolved (cache, or one replay through the
// Store contract on a cold cache), the entry's CreatedAt is forced strictly
// increasing (logical clock), the caller's metadata map is copied (the caller
// keeps ownership of it) and the service-owned chain stamp is added under
// ChainMetadataKey. Persistence, the in-memory append and the head-cache
// update all happen under the same lock so the cached head can never drift
// from the appended rows. A failed insert leaves the cache untouched.
func (s *Service) LogCtx(ctx context.Context, actor, action, organizationID, resource string, metadata map[string]any) (*Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	createdAt := time.Now().UTC()
	if !createdAt.After(s.lastStamp) {
		createdAt = s.lastStamp.Add(time.Nanosecond)
	}
	s.lastStamp = createdAt

	prev := ChainGenesisPrevHash
	length := 0
	stamp := true
	var hash string
	if head, ok := s.heads[organizationID]; ok {
		prev, length = head.hash, head.length
	} else {
		entries, err := s.orgEntriesLocked(ctx, organizationID)
		if err != nil {
			// Cold-cache replay failure (store unavailable to LIST) must
			// not lose the audit row: audit persistence is best-effort by
			// contract, so the entry is written UNSTAMPED instead of
			// failing the caller. The next successful replay includes this
			// row in the positional hash progression, and the next stamped
			// entry chains from it — tamper evidence self-heals, and
			// VerifyEntries treats unstamped rows as chain participants
			// without a verifiable link.
			stamp = false
		} else {
			prev = replayChain(canonicalChainOrder(entries))
			length = len(entries)
		}
	}

	stamped := make(map[string]any, len(metadata)+1)
	for k, v := range metadata {
		stamped[k] = v
	}
	entry := &Entry{
		ID:             uuid.NewString(),
		Actor:          actor,
		Action:         action,
		OrganizationID: organizationID,
		Resource:       resource,
		Metadata:       stamped,
		CreatedAt:      createdAt,
	}
	if stamp {
		// The stamp is added after fingerprinting: the fingerprint excludes
		// the reserved key, so the hash attests the caller-visible content.
		hash = entryFingerprint(prev, entry)
		stamped[ChainMetadataKey] = map[string]any{
			"version": ChainVersionV1,
			"prev":    prev,
			"hash":    hash,
		}
	}

	if s.store != nil {
		if err := s.store.InsertEntry(ctx, entry); err != nil {
			return nil, err
		}
	}
	s.items = append(s.items, entry)
	if stamp {
		if s.heads == nil {
			s.heads = make(map[string]chainHeadState)
		}
		s.heads[organizationID] = chainHeadState{hash: hash, length: length + 1}
	}
	return entry, nil
}

// List is the legacy context-free entry point (in-memory view); see ListCtx.
func (s *Service) List() []*Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Entry, len(s.items))
	copy(out, s.items)
	return out
}

// ListCtx returns the persisted audit trail of exactly one tenant
// (organization_id guard).
func (s *Service) ListCtx(ctx context.Context, orgID string) ([]*Entry, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgRequired
	}
	if s.store != nil {
		return s.store.ListEntries(ctx, orgID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Entry, 0)
	for _, entry := range s.items {
		if entry.OrganizationID == orgID {
			out = append(out, entry)
		}
	}
	return out, nil
}
