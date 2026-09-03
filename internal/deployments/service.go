package deployments

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Environments (wave2 contract: development|staging|production).
const (
	EnvironmentDevelopment = "development"
	EnvironmentStaging     = "staging"
	EnvironmentProduction  = "production"
)

// Deployment lifecycle statuses (wave2 contract:
// requested|validated|deploying|healthy|failed). A deployment advances one
// step per explicit promote; failed and healthy are terminal. The environment
// "points at" the row with status healthy (at most one per agent+environment;
// a superseded healthy row is demoted to failed and stamped with
// superseded_at + health.superseded_by so history stays queryable).
const (
	StatusRequested = "requested"
	StatusValidated = "validated"
	StatusDeploying = "deploying"
	StatusHealthy   = "healthy"
	StatusFailed    = "failed"
)

var (
	// ErrDeploymentNotFound is returned when a deployment does not exist for
	// the caller's tenant (foreign-tenant rows surface as not found).
	ErrDeploymentNotFound = errors.New("deployment not found")
	// ErrInvalidTransition is returned when promote/fail is called on a
	// deployment whose status cannot advance.
	ErrInvalidTransition = errors.New("invalid deployment lifecycle transition")
	// ErrInvalidEnvironment is returned for unknown environment names.
	ErrInvalidEnvironment = errors.New("environment must be one of development|staging|production")
	// ErrVersionNotDeployable is returned when the target agent version does
	// not exist or is not published.
	ErrVersionNotDeployable = errors.New("agent version must exist and be published to create a deployment")
	// ErrNoPreviousHealthy is returned by rollback when the agent+environment
	// has no previous healthy deployment to re-point to.
	ErrNoPreviousHealthy = errors.New("no previous healthy deployment to roll back to")
)

// Health is the deployment health payload (wave2 contract:
// {"error_rate":0.0,"last_check_at"} plus internal superseded/error markers).
type Health struct {
	ErrorRate    float64    `json:"error_rate"`
	LastCheckAt  *time.Time `json:"last_check_at"`
	SupersededBy string     `json:"superseded_by,omitempty"`
	Error        string     `json:"error,omitempty"`
}

// Deployment is one lifecycle row for (agent, version, environment). Rows are
// an append-only ledger: rollback creates a NEW healthy row for the previous
// version rather than mutating history.
type Deployment struct {
	ID             string
	OrganizationID string
	AgentID        string
	Version        int
	Environment    string
	Status         string
	Health         *Health
	CreatedBy      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	SupersededAt   *time.Time // set when a healthy row was replaced (demoted)
}

// VersionChecker validates that an agent version is deployable (exists and is
// published). internal/agents.VersionsService implements it via
// ResolvePublishedVersion.
type VersionChecker interface {
	ResolvePublishedVersion(ctx context.Context, orgID, agentID string, version int) error
}

// Store abstracts durable deployment storage. Implementations MUST scope every
// query by organization_id (tenant guard).
type Store interface {
	// CreateDeployment inserts one deployment row within one tenant.
	CreateDeployment(ctx context.Context, orgID string, deployment *Deployment) error
	// GetDeployment fetches one deployment within one tenant.
	GetDeployment(ctx context.Context, orgID, id string) (*Deployment, error)
	// ListDeployments returns the tenant's deployments, optionally filtered by
	// agent (empty agentID = all agents).
	ListDeployments(ctx context.Context, orgID, agentID string) ([]*Deployment, error)
	// UpdateDeployment persists mutable lifecycle fields only (status, health,
	// updated_at, superseded_at).
	UpdateDeployment(ctx context.Context, orgID string, deployment *Deployment) error
	// GetHealthyDeployment returns the current healthy deployment for one
	// agent+environment (nil when none).
	GetHealthyDeployment(ctx context.Context, orgID, agentID, environment string) (*Deployment, error)
	// GetPreviousHealthyDeployment returns the most recently superseded
	// healthy deployment for one agent+environment, excluding excludeID.
	GetPreviousHealthyDeployment(ctx context.Context, orgID, agentID, environment, excludeID string) (*Deployment, error)
}

// Service is the dual-mode deployments service: in-memory maps by default,
// Postgres-backed when constructed with a Store.
type Service struct {
	mu       sync.Mutex
	store    Store
	resolver VersionChecker
	// items caches deployments in in-memory mode, keyed by agentID.
	items map[string][]*Deployment
}

// NewService returns the in-memory deployments service. The resolver validates
// that deployment targets reference published agent versions (nil skips the
// check - dev/test mode only).
func NewService(resolver VersionChecker) *Service {
	return &Service{
		resolver: resolver,
		items:    make(map[string][]*Deployment),
	}
}

// NewServiceWithStore returns a service whose source of truth is a durable
// store (Postgres).
func NewServiceWithStore(store Store, resolver VersionChecker) *Service {
	s := NewService(resolver)
	s.store = store
	return s
}

// CreateDeploymentCtx validates the environment and the target agent version
// (exists + published via the VersionChecker) and creates a deployment in
// status requested.
func (s *Service) CreateDeploymentCtx(ctx context.Context, orgID, agentID string, version int, environment, createdBy string) (*Deployment, error) {
	if s == nil {
		return nil, errors.New("deployments service is nil")
	}
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(agentID) == "" {
		return nil, ErrDeploymentNotFound
	}
	if version <= 0 {
		return nil, fmt.Errorf("%w: version must be a positive integer", ErrVersionNotDeployable)
	}
	if !validEnvironment(environment) {
		return nil, ErrInvalidEnvironment
	}
	if s.resolver != nil {
		if err := s.resolver.ResolvePublishedVersion(ctx, orgID, agentID, version); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrVersionNotDeployable, err)
		}
	}
	now := time.Now().UTC()
	deployment := &Deployment{
		ID:             uuid.NewString(),
		OrganizationID: orgID,
		AgentID:        agentID,
		Version:        version,
		Environment:    environment,
		Status:         StatusRequested,
		Health:         &Health{ErrorRate: 0},
		CreatedBy:      createdBy,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if s.store != nil {
		if err := s.store.CreateDeployment(ctx, orgID, deployment); err != nil {
			return nil, err
		}
		return deployment, nil
	}
	s.mu.Lock()
	s.items[agentID] = append(s.items[agentID], deployment)
	s.mu.Unlock()
	return deployment, nil
}

// GetDeploymentCtx fetches one deployment within the caller's tenant.
func (s *Service) GetDeploymentCtx(ctx context.Context, orgID, id string) (*Deployment, error) {
	if s == nil || strings.TrimSpace(orgID) == "" || strings.TrimSpace(id) == "" {
		return nil, ErrDeploymentNotFound
	}
	if s.store != nil {
		return s.store.GetDeployment(ctx, orgID, id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, deployments := range s.items {
		for _, deployment := range deployments {
			if deployment.ID == id && deployment.OrganizationID == orgID {
				return deployment, nil
			}
		}
	}
	return nil, ErrDeploymentNotFound
}

// ListDeploymentsCtx returns the tenant's deployments, optionally filtered by
// agent_id (empty = all agents of the tenant).
func (s *Service) ListDeploymentsCtx(ctx context.Context, orgID, agentID string) ([]*Deployment, error) {
	if s == nil || strings.TrimSpace(orgID) == "" {
		return []*Deployment{}, ErrDeploymentNotFound
	}
	if s.store != nil {
		return s.store.ListDeployments(ctx, orgID, agentID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Deployment, 0)
	for _, deployments := range s.items {
		for _, deployment := range deployments {
			if deployment.OrganizationID != orgID {
				continue
			}
			if agentID != "" && deployment.AgentID != agentID {
				continue
			}
			out = append(out, deployment)
		}
	}
	return out, nil
}

// PromoteDeploymentCtx advances the lifecycle one step:
// requested -> validated -> deploying -> healthy. healthy and failed are
// terminal. Reaching healthy demotes the previous healthy deployment of the
// same agent+environment (one healthy deployment per agent+environment).
func (s *Service) PromoteDeploymentCtx(ctx context.Context, orgID, id string) (*Deployment, error) {
	deployment, err := s.GetDeploymentCtx(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	next, ok := nextStatus(deployment.Status)
	if !ok {
		return nil, fmt.Errorf("%w: %s cannot advance", ErrInvalidTransition, deployment.Status)
	}
	now := time.Now().UTC()
	if next == StatusHealthy {
		// Demote the current healthy deployment first so the partial unique
		// index (one healthy per agent+environment) stays satisfied.
		if err := s.demoteHealthy(ctx, orgID, deployment); err != nil {
			return nil, err
		}
	}
	deployment.Status = next
	deployment.UpdatedAt = now
	if next == StatusHealthy && deployment.Health != nil {
		check := now
		deployment.Health.LastCheckAt = &check
	}
	return deployment, s.persistUpdate(ctx, orgID, deployment)
}

// FailDeploymentCtx marks a non-terminal deployment as failed (worker path;
// the lifecycle keeps requested|validated|deploying -> failed).
func (s *Service) FailDeploymentCtx(ctx context.Context, orgID, id, reason string) (*Deployment, error) {
	deployment, err := s.GetDeploymentCtx(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	switch deployment.Status {
	case StatusRequested, StatusValidated, StatusDeploying:
	default:
		return nil, fmt.Errorf("%w: %s cannot fail", ErrInvalidTransition, deployment.Status)
	}
	now := time.Now().UTC()
	deployment.Status = StatusFailed
	deployment.UpdatedAt = now
	if deployment.Health == nil {
		deployment.Health = &Health{}
	}
	deployment.Health.Error = reason
	deployment.Health.LastCheckAt = &now
	return deployment, s.persistUpdate(ctx, orgID, deployment)
}

// RollbackDeploymentCtx re-points the environment to the previous healthy
// deployment's version. It creates a NEW healthy deployment row for that
// version (immediate re-point; the version was proven healthy before) and
// demotes the currently healthy row. When the environment already serves the
// previous healthy version, the existing row is returned unchanged
// (idempotent). Returns the current deployment for the previous version and
// that version number.
func (s *Service) RollbackDeploymentCtx(ctx context.Context, orgID, id, createdBy string) (*Deployment, int, error) {
	current, err := s.GetDeploymentCtx(ctx, orgID, id)
	if err != nil {
		return nil, 0, err
	}
	healthy, err := s.healthyDeployment(ctx, orgID, current.AgentID, current.Environment)
	if err != nil {
		return nil, 0, err
	}
	// The previous healthy deployment relative to the row being rolled back:
	// when the environment still serves a healthy row other than `current`
	// (e.g. current failed mid-flight), that row IS the previous healthy;
	// otherwise fall back to the most recently superseded healthy row.
	var previous *Deployment
	if healthy != nil && healthy.ID != current.ID {
		previous = healthy
	} else {
		previous, err = s.previousHealthy(ctx, orgID, current.AgentID, current.Environment, current.ID)
		if err != nil {
			return nil, 0, err
		}
	}
	now := time.Now().UTC()
	// Idempotent: the environment already serves the previous version.
	if healthy != nil && healthy.Version == previous.Version {
		return healthy, previous.Version, nil
	}
	rollback := &Deployment{
		ID:             uuid.NewString(),
		OrganizationID: orgID,
		AgentID:        current.AgentID,
		Version:        previous.Version,
		Environment:    current.Environment,
		Status:         StatusHealthy,
		Health:         &Health{ErrorRate: 0, LastCheckAt: &now},
		CreatedBy:      createdBy,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	// Demote the currently healthy deployment BEFORE the new healthy row lands
	// (one healthy deployment per agent+environment).
	if err := s.demoteHealthy(ctx, orgID, rollback); err != nil {
		return nil, 0, err
	}
	if s.store != nil {
		if err := s.store.CreateDeployment(ctx, orgID, rollback); err != nil {
			return nil, 0, err
		}
		return rollback, previous.Version, nil
	}
	s.mu.Lock()
	s.items[current.AgentID] = append(s.items[current.AgentID], rollback)
	s.mu.Unlock()
	return rollback, previous.Version, nil
}

// demoteHealthy stamps the current healthy deployment (same agent+environment)
// as superseded: status failed + superseded_at + health.superseded_by.
func (s *Service) demoteHealthy(ctx context.Context, orgID string, incoming *Deployment) error {
	current, err := s.healthyDeployment(ctx, orgID, incoming.AgentID, incoming.Environment)
	if err != nil {
		return err
	}
	if current == nil || current.ID == incoming.ID {
		return nil
	}
	now := time.Now().UTC()
	current.Status = StatusFailed
	current.SupersededAt = &now
	current.UpdatedAt = now
	if current.Health == nil {
		current.Health = &Health{}
	}
	current.Health.SupersededBy = incoming.ID
	return s.persistUpdate(ctx, orgID, current)
}

// healthyDeployment returns the current healthy deployment for one
// agent+environment (nil when none exists).
func (s *Service) healthyDeployment(ctx context.Context, orgID, agentID, environment string) (*Deployment, error) {
	if s.store != nil {
		return s.store.GetHealthyDeployment(ctx, orgID, agentID, environment)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var healthy *Deployment
	for _, deployment := range s.items[agentID] {
		if deployment.OrganizationID == orgID && deployment.Environment == environment && deployment.Status == StatusHealthy {
			// Defensive: in-memory mode should only ever hold one.
			if healthy == nil || deployment.UpdatedAt.After(healthy.UpdatedAt) {
				healthy = deployment
			}
		}
	}
	return healthy, nil
}

// previousHealthy returns the most recently superseded healthy deployment
// (status failed + superseded_at set) excluding excludeID.
func (s *Service) previousHealthy(ctx context.Context, orgID, agentID, environment, excludeID string) (*Deployment, error) {
	if s.store != nil {
		previous, err := s.store.GetPreviousHealthyDeployment(ctx, orgID, agentID, environment, excludeID)
		if err != nil {
			return nil, err
		}
		if previous == nil {
			return nil, ErrNoPreviousHealthy
		}
		return previous, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var previous *Deployment
	for _, deployment := range s.items[agentID] {
		if deployment.OrganizationID != orgID || deployment.Environment != environment || deployment.ID == excludeID {
			continue
		}
		if deployment.SupersededAt == nil {
			continue
		}
		if previous == nil || deployment.SupersededAt.After(*previous.SupersededAt) {
			previous = deployment
		}
	}
	if previous == nil {
		return nil, ErrNoPreviousHealthy
	}
	return previous, nil
}

// persistUpdate writes lifecycle fields through the store (in-memory mode
// mutates the shared pointer, so persistence is already done).
func (s *Service) persistUpdate(ctx context.Context, orgID string, deployment *Deployment) error {
	if s.store != nil {
		return s.store.UpdateDeployment(ctx, orgID, deployment)
	}
	return nil
}

// nextStatus maps a status to its promote successor.
func nextStatus(status string) (string, bool) {
	switch status {
	case StatusRequested:
		return StatusValidated, true
	case StatusValidated:
		return StatusDeploying, true
	case StatusDeploying:
		return StatusHealthy, true
	default:
		return "", false
	}
}

func validEnvironment(environment string) bool {
	switch environment {
	case EnvironmentDevelopment, EnvironmentStaging, EnvironmentProduction:
		return true
	default:
		return false
	}
}
