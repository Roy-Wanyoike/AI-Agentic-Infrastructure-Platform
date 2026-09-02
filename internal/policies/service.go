package policies

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ErrPolicyNotFound is returned when a policy id does not exist within the
// caller's organization.
var ErrPolicyNotFound = errors.New("policies: policy not found")

// Store persists policy records. Every method takes the organization_id so
// the durable source of truth stays tenant-scoped.
type Store interface {
	// CreatePolicy inserts the policy row with its organization_id scope.
	CreatePolicy(ctx context.Context, policy *Policy) error
	// GetPolicy resolves one policy guarded by organization_id.
	GetPolicy(ctx context.Context, orgID, id string) (*Policy, error)
	// UpdatePolicy replaces the mutable fields of one tenant-owned policy.
	UpdatePolicy(ctx context.Context, policy *Policy) error
	// DeletePolicy removes one tenant-owned policy. It returns
	// ErrPolicyNotFound when the tenant guard or the id lookup rejects.
	DeletePolicy(ctx context.Context, orgID, id string) error
	// ListPolicies returns every policy of exactly one tenant.
	ListPolicies(ctx context.Context, orgID string) ([]*Policy, error)
}

// Service is the dual-mode policy service: in-memory maps + mutex with an
// optional durable Store (Postgres) that becomes the source of truth.
type Service struct {
	mu       sync.RWMutex
	policies map[string]*Policy
	store    Store
}

// NewService returns the zero-infrastructure in-memory service.
func NewService() *Service {
	return &Service{policies: make(map[string]*Policy)}
}

// NewServiceWithStore returns a service backed by a durable store; the store
// is the source of truth and the in-memory map acts as a write-through cache.
func NewServiceWithStore(store Store) *Service {
	s := NewService()
	s.store = store
	return s
}

// SetStore attaches a durable store after construction.
func (s *Service) SetStore(store Store) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store = store
}

// CreatePolicyCtx validates and persists a new policy for one tenant. The
// record's ID and timestamps are assigned here.
func (s *Service) CreatePolicyCtx(ctx context.Context, orgID string, policy *Policy) (*Policy, error) {
	if s == nil {
		return nil, errors.New("policies: service is nil")
	}
	if strings.TrimSpace(orgID) == "" {
		return nil, errors.New("policies: organization id is required")
	}
	if policy == nil {
		return nil, ErrInvalidPolicy
	}
	policy = policy.Normalize()
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	policy.ID = uuid.NewString()
	policy.OrganizationID = orgID
	now := time.Now().UTC()
	policy.CreatedAt = now
	policy.UpdatedAt = now

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store != nil {
		if err := s.store.CreatePolicy(ctx, policy); err != nil {
			return nil, err
		}
	}
	s.policies[policy.ID] = clonePolicy(policy)
	return clonePolicy(policy), nil
}

// ListPoliciesCtx returns the tenant's policies ordered by priority desc,
// then creation time ascending (the same order the evaluator applies).
func (s *Service) ListPoliciesCtx(ctx context.Context, orgID string) ([]*Policy, error) {
	if s == nil {
		return nil, errors.New("policies: service is nil")
	}
	if strings.TrimSpace(orgID) == "" {
		return nil, errors.New("policies: organization id is required")
	}
	if s.store != nil {
		list, err := s.store.ListPolicies(ctx, orgID)
		if err != nil {
			return nil, err
		}
		sortPolicies(list)
		return list, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := make([]*Policy, 0)
	for _, p := range s.policies {
		if p.OrganizationID == orgID {
			list = append(list, clonePolicy(p))
		}
	}
	sortPolicies(list)
	return list, nil
}

// GetPolicyCtx resolves one tenant-owned policy.
func (s *Service) GetPolicyCtx(ctx context.Context, orgID, id string) (*Policy, error) {
	if s == nil {
		return nil, errors.New("policies: service is nil")
	}
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(id) == "" {
		return nil, ErrPolicyNotFound
	}
	if s.store != nil {
		return s.store.GetPolicy(ctx, orgID, id)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.policies[id]
	if !ok || p.OrganizationID != orgID {
		return nil, ErrPolicyNotFound
	}
	return clonePolicy(p), nil
}

// UpdatePolicyCtx performs the full (PUT) update of a tenant-owned policy.
// Mutable fields are replaced wholesale; id/organization_id/created_at are
// preserved and updated_at is bumped.
func (s *Service) UpdatePolicyCtx(ctx context.Context, orgID, id string, update *Policy) (*Policy, error) {
	if s == nil {
		return nil, errors.New("policies: service is nil")
	}
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(id) == "" {
		return nil, ErrPolicyNotFound
	}
	if update == nil {
		return nil, ErrInvalidPolicy
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var existing *Policy
	if s.store != nil {
		stored, err := s.store.GetPolicy(ctx, orgID, id)
		if err != nil {
			return nil, err
		}
		existing = stored
	} else {
		cached, ok := s.policies[id]
		if !ok || cached.OrganizationID != orgID {
			return nil, ErrPolicyNotFound
		}
		existing = cached
	}

	updated := &Policy{
		ID:             existing.ID,
		OrganizationID: existing.OrganizationID,
		Name:           update.Name,
		Effect:         update.Effect,
		ResourceType:   update.ResourceType,
		Actions:        update.Actions,
		Conditions:     update.Conditions,
		Priority:       update.Priority,
		Enabled:        update.Enabled,
		CreatedAt:      existing.CreatedAt,
		UpdatedAt:      time.Now().UTC(),
	}
	updated.Normalize()
	if err := updated.Validate(); err != nil {
		return nil, err
	}

	if s.store != nil {
		if err := s.store.UpdatePolicy(ctx, updated); err != nil {
			return nil, err
		}
	}
	s.policies[id] = clonePolicy(updated)
	return clonePolicy(updated), nil
}

// DeletePolicyCtx removes one tenant-owned policy.
func (s *Service) DeletePolicyCtx(ctx context.Context, orgID, id string) error {
	if s == nil {
		return errors.New("policies: service is nil")
	}
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(id) == "" {
		return ErrPolicyNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store != nil {
		if err := s.store.DeletePolicy(ctx, orgID, id); err != nil {
			return err
		}
	} else {
		p, ok := s.policies[id]
		if !ok || p.OrganizationID != orgID {
			return ErrPolicyNotFound
		}
	}
	delete(s.policies, id)
	return nil
}

// EvaluateCtx lists the tenant's enabled policy candidates and runs the
// engine over the request.
func (s *Service) EvaluateCtx(ctx context.Context, orgID string, req EvaluateRequest) (Decision, error) {
	candidates, err := s.ListPoliciesCtx(ctx, orgID)
	if err != nil {
		return Decision{}, err
	}
	return Evaluate(candidates, req), nil
}

// sortPolicies orders by priority desc, then created_at asc, then id — the
// deterministic order the evaluator resolves matches in.
func sortPolicies(list []*Policy) {
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].Priority != list[j].Priority {
			return list[i].Priority > list[j].Priority
		}
		if !list[i].CreatedAt.Equal(list[j].CreatedAt) {
			return list[i].CreatedAt.Before(list[j].CreatedAt)
		}
		return list[i].ID < list[j].ID
	})
}

// clonePolicy returns a deep copy so callers cannot mutate cached state.
func clonePolicy(p *Policy) *Policy {
	if p == nil {
		return nil
	}
	out := *p
	out.Actions = append([]string(nil), p.Actions...)
	out.Conditions.ToolAllowlist = append([]string(nil), p.Conditions.ToolAllowlist...)
	out.Conditions.Environments = append([]string(nil), p.Conditions.Environments...)
	if p.Conditions.MaxCostCents != nil {
		cost := *p.Conditions.MaxCostCents
		out.Conditions.MaxCostCents = &cost
	}
	return &out
}
