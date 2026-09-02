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

type Service struct {
	mu    sync.Mutex
	items []*Entry
	store Store
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

// LogCtx records an audit entry for one tenant (organization_id scope).
func (s *Service) LogCtx(ctx context.Context, actor, action, organizationID, resource string, metadata map[string]any) (*Entry, error) {
	entry := &Entry{
		ID:             uuid.NewString(),
		Actor:          actor,
		Action:         action,
		OrganizationID: organizationID,
		Resource:       resource,
		Metadata:       metadata,
		CreatedAt:      time.Now().UTC(),
	}
	if s.store != nil {
		if err := s.store.InsertEntry(ctx, entry); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = append(s.items, entry)
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
