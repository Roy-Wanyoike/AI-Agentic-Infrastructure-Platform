package usage

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type Record struct {
	ID             string
	OrganizationID string
	Resource       string
	Quantity       int
	Metadata       map[string]any
	CreatedAt      time.Time
}

var ErrOrgRequired = errors.New("organization id is required")

// Store persists usage records. Every read is guarded by organization_id so a
// tenant can only ever see its own metering data.
type Store interface {
	// InsertRecord appends one metering row with its organization_id scope.
	InsertRecord(ctx context.Context, record *Record) error
	// ListRecords returns the raw metering rows of exactly one tenant.
	ListRecords(ctx context.Context, orgID string) ([]*Record, error)
	// TotalsByResource aggregates quantities per resource for one tenant.
	TotalsByResource(ctx context.Context, orgID string) (map[string]int, error)
}

type Service struct {
	mu      sync.Mutex
	values  map[string]int
	records []*Record
	store   Store
}

func NewService() *Service { return &Service{values: make(map[string]int)} }

// NewServiceWithStore returns a service that persists metering rows to the
// durable store; in-memory counters remain for legacy Snapshot() callers.
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

// Record is the legacy in-memory counter (no tenant scope); see RecordUsageCtx
// for the durable, tenant-scoped path.
func (s *Service) Record(resource string, delta int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[resource] += delta
	s.records = append(s.records, &Record{Resource: resource, Quantity: delta})
}

// RecordUsageCtx persists one metering event for one tenant
// (organization_id scope) and keeps the legacy in-memory counter in sync.
func (s *Service) RecordUsageCtx(ctx context.Context, orgID, resource string, quantity int, metadata map[string]any) error {
	if strings.TrimSpace(orgID) == "" {
		return ErrOrgRequired
	}
	if strings.TrimSpace(resource) == "" {
		return errors.New("resource is required")
	}
	record := &Record{
		ID:             uuid.NewString(),
		OrganizationID: orgID,
		Resource:       resource,
		Quantity:       quantity,
		Metadata:       metadata,
		CreatedAt:      time.Now().UTC(),
	}
	if s.store != nil {
		if err := s.store.InsertRecord(ctx, record); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[resource] += quantity
	s.records = append(s.records, record)
	return nil
}

// Snapshot is the legacy in-memory counter view.
func (s *Service) Snapshot() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int, len(s.values))
	for k, v := range s.values {
		out[k] = v
	}
	return out
}

// ListRecordsCtx returns the metering rows of exactly one tenant
// (organization_id guard).
func (s *Service) ListRecordsCtx(ctx context.Context, orgID string) ([]*Record, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgRequired
	}
	if s.store != nil {
		return s.store.ListRecords(ctx, orgID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Record, 0)
	for _, record := range s.records {
		if record.OrganizationID == orgID {
			out = append(out, record)
		}
	}
	return out, nil
}

// UsageTotalsCtx aggregates metered quantities per resource for one tenant
// (organization_id guard).
func (s *Service) UsageTotalsCtx(ctx context.Context, orgID string) (map[string]int, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgRequired
	}
	if s.store != nil {
		return s.store.TotalsByResource(ctx, orgID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int)
	for _, record := range s.records {
		if record.OrganizationID == orgID {
			out[record.Resource] += record.Quantity
		}
	}
	return out, nil
}
