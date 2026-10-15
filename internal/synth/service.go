package synth

// service.go — issue #147 (flagship #118): tenant-scoped synthetic assurance
// registry — projects, targets (with ownership verification), environments
// (hard safety posture) and personas.
//
// Dual-mode house style (internal/policies + internal/tenant precedent):
//   - NewService()            -> zero-infrastructure in-memory maps.
//   - NewServiceWithStore()   -> Postgres store (migration 034 tables) is the
//     source of truth; the in-memory map acts as a write-through cache.
//
// Invariants enforced HERE (the safety contract pinned by types.go):
//   - Every record is org-scoped. The org id always comes from the caller
//     (the HTTP layer derives it from the auth claims, never from the wire);
//     cross-tenant lookups are indistinguishable from missing records
//     (ErrNotFound — no existence leak across tenants).
//   - Targets execute only when VERIFIED (ownership challenge/confirm flow).
//   - Environment mode is constrained by kind: production environments
//     default to production_read_only and can never be pre_production;
//     production requires a non-empty domain allowlist; rate caps are
//     honored only in pre_production mode (types.go semantics).
//   - AssertExecutionAllowed is the ScopeGuard seam the worker tracks
//     (#148/#155) call before every navigation/action: unverified target
//     denies ALL, production_read_only denies writes, requests outside the
//     allowlisted domains are scope violations.
//
// Personas never impersonate real customer accounts (types.go): the BuiltIn
// key must be one of the documented built-in personas or "" for custom, and
// this layer stores credential references only — never secrets.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// builtInPersonas is the documented set of built-in persona keys (types.go,
// issue #149 consumes them). A persona is either one of these or a custom
// one (BuiltIn == "").
var builtInPersonas = map[string]bool{
	"visitor":            true,
	"new_user":           true,
	"returning_user":     true,
	"standard_customer":  true,
	"administrator":      true,
	"restricted_user":    true,
	"mobile_user":        true,
	"accessibility_user": true,
	"slow_network_user":  true,
}

// Store is the durable persistence seam for the registry. Every method
// takes the organization id so the source of truth stays tenant-guarded:
// a lookup scoped to a foreign org returns synth.ErrNotFound exactly like
// an unknown id. store.go implements it over the migration 034 tables.
type Store interface {
	CreateProject(ctx context.Context, p *Project) error
	GetProject(ctx context.Context, orgID, id string) (*Project, error)
	ListProjects(ctx context.Context, orgID string) ([]*Project, error)
	UpdateProject(ctx context.Context, p *Project) error
	DeleteProject(ctx context.Context, orgID, id string) error

	CreateTarget(ctx context.Context, t *Target) error
	GetTarget(ctx context.Context, orgID, id string) (*Target, error)
	ListTargets(ctx context.Context, orgID, projectID string) ([]*Target, error)
	UpdateTarget(ctx context.Context, t *Target) error
	DeleteTarget(ctx context.Context, orgID, id string) error

	CreateEnvironment(ctx context.Context, e *Environment) error
	GetEnvironment(ctx context.Context, orgID, id string) (*Environment, error)
	ListEnvironments(ctx context.Context, orgID, targetID string) ([]*Environment, error)
	UpdateEnvironment(ctx context.Context, e *Environment) error
	DeleteEnvironment(ctx context.Context, orgID, id string) error

	CreatePersona(ctx context.Context, p *Persona) error
	GetPersona(ctx context.Context, orgID, id string) (*Persona, error)
	ListPersonas(ctx context.Context, orgID string) ([]*Persona, error)
	UpdatePersona(ctx context.Context, p *Persona) error
	DeletePersona(ctx context.Context, orgID, id string) error
}

// Service is the dual-mode registry: in-memory maps + mutex with an
// optional durable Store that becomes the source of truth (write-through
// cache in that mode).
type Service struct {
	mu           sync.RWMutex
	projects     map[string]*Project
	targets      map[string]*Target
	environments map[string]*Environment
	personas     map[string]*Persona

	// rateMu guards the per-process rate-cap window (pre_production only).
	rateMu   sync.Mutex
	rateSeen map[string][]time.Time

	store Store
}

// NewService returns the zero-infrastructure in-memory registry.
func NewService() *Service {
	return &Service{
		projects:     make(map[string]*Project),
		targets:      make(map[string]*Target),
		environments: make(map[string]*Environment),
		personas:     make(map[string]*Persona),
		rateSeen:     make(map[string][]time.Time),
	}
}

// NewServiceWithStore returns a registry backed by a durable store; the
// store is the source of truth and the in-memory map acts as a
// write-through cache.
func NewServiceWithStore(store Store) *Service {
	s := NewService()
	s.store = store
	return s
}

// ---------------------------------------------------------------------------
// projects
// ---------------------------------------------------------------------------

// CreateProject creates a tenant-scoped project container. The ID and
// timestamps are assigned here.
func (s *Service) CreateProject(ctx context.Context, orgID, name string) (*Project, error) {
	if s == nil {
		return nil, fmt.Errorf("synth: service is nil")
	}
	if strings.TrimSpace(orgID) == "" {
		return nil, fmt.Errorf("%w: organization id is required", ErrInvalid)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("%w: project name is required", ErrInvalid)
	}
	now := time.Now().UTC()
	p := &Project{ID: uuid.NewString(), OrgID: orgID, Name: name, CreatedAt: now, UpdatedAt: now}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store != nil {
		if err := s.store.CreateProject(ctx, p); err != nil {
			return nil, err
		}
	}
	s.projects[p.ID] = cloneProject(p)
	return cloneProject(p), nil
}

// GetProject resolves one project guarded by organization_id. A foreign or
// unknown id is ErrNotFound either way (no existence leak).
func (s *Service) GetProject(ctx context.Context, orgID, id string) (*Project, error) {
	if s == nil {
		return nil, fmt.Errorf("synth: service is nil")
	}
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(id) == "" {
		return nil, ErrNotFound
	}
	s.mu.RLock()
	if s.store == nil {
		p, ok := s.projects[id]
		s.mu.RUnlock()
		if !ok || p.OrgID != orgID {
			return nil, ErrNotFound
		}
		return cloneProject(p), nil
	}
	s.mu.RUnlock()
	p, err := s.store.GetProject(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.projects[p.ID] = cloneProject(p)
	s.mu.Unlock()
	return cloneProject(p), nil
}

// ListProjects returns the tenant's projects ordered by creation time.
func (s *Service) ListProjects(ctx context.Context, orgID string) ([]*Project, error) {
	if s == nil {
		return nil, fmt.Errorf("synth: service is nil")
	}
	if strings.TrimSpace(orgID) == "" {
		return nil, fmt.Errorf("%w: organization id is required", ErrInvalid)
	}
	s.mu.RLock()
	if s.store == nil {
		out := make([]*Project, 0)
		for _, p := range s.projects {
			if p.OrgID == orgID {
				out = append(out, cloneProject(p))
			}
		}
		s.mu.RUnlock()
		sortProjects(out)
		return out, nil
	}
	s.mu.RUnlock()
	list, err := s.store.ListProjects(ctx, orgID)
	if err != nil {
		return nil, err
	}
	if list == nil {
		list = []*Project{}
	}
	sortProjects(list)
	return list, nil
}

// UpdateProject renames a tenant-owned project.
func (s *Service) UpdateProject(ctx context.Context, orgID, id, name string) (*Project, error) {
	if s == nil {
		return nil, fmt.Errorf("synth: service is nil")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("%w: project name is required", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.getLocked(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	p.Name = name
	p.UpdatedAt = time.Now().UTC()
	if s.store != nil {
		if err := s.store.UpdateProject(ctx, p); err != nil {
			return nil, err
		}
	}
	s.projects[p.ID] = cloneProject(p)
	return cloneProject(p), nil
}

// DeleteProject removes a tenant-owned project (cascades to its
// targets/environments via the store's FK semantics; in-memory mode deletes
// the dependent records explicitly so the maps stay consistent).
func (s *Service) DeleteProject(ctx context.Context, orgID, id string) error {
	if s == nil {
		return fmt.Errorf("synth: service is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.getLocked(ctx, orgID, id); err != nil {
		return err
	}
	if s.store != nil {
		if err := s.store.DeleteProject(ctx, orgID, id); err != nil {
			return err
		}
	}
	delete(s.projects, id)
	for id, t := range s.targets {
		if t.ProjectID == id && t.OrgID == orgID {
			delete(s.targets, id)
		}
	}
	for id, e := range s.environments {
		if _, ok := s.targets[e.TargetID]; !ok && e.OrgID == orgID {
			delete(s.environments, id)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// targets + ownership verification
// ---------------------------------------------------------------------------

// CreateTarget registers an application under test. The target starts
// UNVERIFIED with a challenge token issued; tests may only execute against
// verified targets (AssertExecutionAllowed denies all on unverified).
func (s *Service) CreateTarget(ctx context.Context, orgID, projectID, name, baseURL string) (*Target, error) {
	if s == nil {
		return nil, fmt.Errorf("synth: service is nil")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("%w: target name is required", ErrInvalid)
	}
	baseURL = strings.TrimSpace(baseURL)
	if err := validateBaseURL(baseURL); err != nil {
		return nil, err
	}
	// The project anchor must exist INSIDE the caller's org — the guard
	// makes cross-tenant anchoring indistinguishable from a typo (404).
	if _, err := s.GetProject(ctx, orgID, projectID); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	t := &Target{
		ID: uuid.NewString(), OrgID: orgID, ProjectID: projectID,
		Name: name, BaseURL: baseURL, Verified: false,
		VerifyToken: newChallengeToken(),
		CreatedAt:   now, UpdatedAt: now,
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store != nil {
		if err := s.store.CreateTarget(ctx, t); err != nil {
			return nil, err
		}
	}
	s.targets[t.ID] = cloneTarget(t)
	return cloneTarget(t), nil
}

// GetTarget resolves one target guarded by organization_id.
func (s *Service) GetTarget(ctx context.Context, orgID, id string) (*Target, error) {
	if s == nil {
		return nil, fmt.Errorf("synth: service is nil")
	}
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(id) == "" {
		return nil, ErrNotFound
	}
	s.mu.RLock()
	if s.store == nil {
		t, ok := s.targets[id]
		s.mu.RUnlock()
		if !ok || t.OrgID != orgID {
			return nil, ErrNotFound
		}
		return cloneTarget(t), nil
	}
	s.mu.RUnlock()
	t, err := s.store.GetTarget(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.targets[t.ID] = cloneTarget(t)
	s.mu.Unlock()
	return cloneTarget(t), nil
}

// ListTargets returns the tenant's targets, optionally narrowed to one
// project (projectID "" lists across the org).
func (s *Service) ListTargets(ctx context.Context, orgID, projectID string) ([]*Target, error) {
	if s == nil {
		return nil, fmt.Errorf("synth: service is nil")
	}
	if strings.TrimSpace(orgID) == "" {
		return nil, fmt.Errorf("%w: organization id is required", ErrInvalid)
	}
	s.mu.RLock()
	if s.store == nil {
		out := make([]*Target, 0)
		for _, t := range s.targets {
			if t.OrgID == orgID && (projectID == "" || t.ProjectID == projectID) {
				out = append(out, cloneTarget(t))
			}
		}
		s.mu.RUnlock()
		sortTargets(out)
		return out, nil
	}
	s.mu.RUnlock()
	list, err := s.store.ListTargets(ctx, orgID, projectID)
	if err != nil {
		return nil, err
	}
	if list == nil {
		list = []*Target{}
	}
	sortTargets(list)
	return list, nil
}

// UpdateTarget renames a tenant-owned target (verification state is never
// mutated here — only the challenge/confirm flow owns it).
func (s *Service) UpdateTarget(ctx context.Context, orgID, id, name string) (*Target, error) {
	if s == nil {
		return nil, fmt.Errorf("synth: service is nil")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("%w: target name is required", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.targetLocked(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	t.Name = name
	t.UpdatedAt = time.Now().UTC()
	if s.store != nil {
		if err := s.store.UpdateTarget(ctx, t); err != nil {
			return nil, err
		}
	}
	s.targets[t.ID] = cloneTarget(t)
	return cloneTarget(t), nil
}

// DeleteTarget removes a tenant-owned target (dependent environments are
// removed with it; store mode cascades via FK).
func (s *Service) DeleteTarget(ctx context.Context, orgID, id string) error {
	if s == nil {
		return fmt.Errorf("synth: service is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.targetLocked(ctx, orgID, id); err != nil {
		return err
	}
	if s.store != nil {
		if err := s.store.DeleteTarget(ctx, orgID, id); err != nil {
			return err
		}
	}
	delete(s.targets, id)
	for eid, e := range s.environments {
		if e.TargetID == id && e.OrgID == orgID {
			delete(s.environments, eid)
		}
	}
	return nil
}

// StartTargetVerification (re)issues the ownership challenge token for an
// UNVERIFIED target. The token is returned to the caller ONCE and must be
// surfaced at the target's BaseURL (e.g. /.well-known/agentos-synth-challenge)
// before ConfirmTargetVerification succeeds. On an already-verified target
// this is a documented no-op: verification is never silently revoked.
func (s *Service) StartTargetVerification(ctx context.Context, orgID, id string) (*Target, error) {
	if s == nil {
		return nil, fmt.Errorf("synth: service is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.targetLocked(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	if t.Verified {
		return cloneTarget(t), nil // no-op: never silently un-verify
	}
	t.VerifyToken = newChallengeToken()
	t.UpdatedAt = time.Now().UTC()
	if s.store != nil {
		if err := s.store.UpdateTarget(ctx, t); err != nil {
			return nil, err
		}
	}
	s.targets[t.ID] = cloneTarget(t)
	return cloneTarget(t), nil
}

// ConfirmTargetVerification completes the ownership challenge: when token
// matches the outstanding challenge the target flips to Verified=true. The
// challenge token is a ONE-TIME secret — it is cleared on success so it can
// never be replayed.
func (s *Service) ConfirmTargetVerification(ctx context.Context, orgID, id, token string) (*Target, error) {
	if s == nil {
		return nil, fmt.Errorf("synth: service is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.targetLocked(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	if t.Verified {
		return cloneTarget(t), nil // idempotent confirm
	}
	if token == "" || token != t.VerifyToken || t.VerifyToken == "" {
		return nil, fmt.Errorf("%w: challenge token does not match", ErrTargetUnverified)
	}
	now := time.Now().UTC()
	t.Verified = true
	t.VerifiedAt = &now
	t.VerifyToken = "" // one-time challenge secret consumed
	t.UpdatedAt = now
	if s.store != nil {
		if err := s.store.UpdateTarget(ctx, t); err != nil {
			return nil, err
		}
	}
	s.targets[t.ID] = cloneTarget(t)
	return cloneTarget(t), nil
}

// ---------------------------------------------------------------------------
// environments (safety posture)
// ---------------------------------------------------------------------------

// EnvSpec is the create/update input for an environment. It mirrors the
// wire shape so the HTTP layer can decode straight into it.
type EnvSpec struct {
	Kind           EnvironmentKind `json:"kind"`
	Mode           EnvironmentMode `json:"mode"` // "" -> kind-derived default
	AllowedDomains []string        `json:"allowed_domains"`
	RateCapPerMin  int             `json:"rate_cap_per_min"`
	BudgetPerRun   int             `json:"budget_per_run"`
	QuietWindows   []QuietWindow   `json:"quiet_windows"`
	TestWindows    []QuietWindow   `json:"test_windows"`
}

// validateEnvSpec enforces the types.go mode semantics:
//
//	production kind      -> mode defaults to production_read_only;
//	                        pre_production is NEVER valid on production;
//	                        a non-empty AllowedDomains allowlist is REQUIRED.
//	local/dev/staging    -> mode defaults to pre_production; any valid mode
//	                        is otherwise allowed.
//	rate caps            -> honored only in pre_production mode; a cap on any
//	                        other mode is a validation error (never silently
//	                        dropped — a dropped cap looks like a working one).
//	windows              -> weekday 0..6, HH:MM times, loadable IANA location.
func validateEnvSpec(spec *EnvSpec) error {
	switch spec.Kind {
	case EnvLocal, EnvDev, EnvStaging, EnvProduction:
	default:
		return fmt.Errorf("%w: kind %q (want local, dev, staging or production)", ErrInvalid, string(spec.Kind))
	}
	switch spec.Mode {
	case "":
		if spec.Kind == EnvProduction {
			spec.Mode = ModeProductionReadOnly // production defaults to read-only
		} else {
			spec.Mode = ModePreProduction
		}
	case ModePreProduction, ModeProductionReadOnly, ModeReleaseComparison:
	default:
		return fmt.Errorf("%w: mode %q (want pre_production, production_read_only or release_comparison)", ErrInvalid, string(spec.Mode))
	}
	if spec.Kind == EnvProduction && spec.Mode == ModePreProduction {
		return fmt.Errorf("%w: pre_production mode is not allowed on production environments (production defaults to production_read_only)", ErrInvalid)
	}
	if spec.Mode == ModePreProduction && spec.Kind == EnvProduction {
		return fmt.Errorf("%w: pre_production mode requires a non-production kind", ErrInvalid)
	}
	// Normalize the domain allowlist: trimmed, lowercased, no empties.
	domains := make([]string, 0, len(spec.AllowedDomains))
	for _, d := range spec.AllowedDomains {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "" {
			continue
		}
		if strings.ContainsAny(d, "/ \t") || strings.Contains(d, "://") {
			return fmt.Errorf("%w: allowed domain %q must be a bare host (no scheme, path or whitespace)", ErrInvalid, d)
		}
		domains = append(domains, d)
	}
	spec.AllowedDomains = domains
	if spec.Kind == EnvProduction && len(spec.AllowedDomains) == 0 {
		return fmt.Errorf("%w: production environments require a non-empty allowed_domains allowlist", ErrInvalid)
	}
	if spec.RateCapPerMin < 0 {
		return fmt.Errorf("%w: rate_cap_per_min must be >= 0", ErrInvalid)
	}
	if spec.RateCapPerMin > 0 && spec.Mode != ModePreProduction {
		return fmt.Errorf("%w: rate caps are only honored in pre_production mode", ErrInvalid)
	}
	if spec.BudgetPerRun < 0 {
		return fmt.Errorf("%w: budget_per_run must be >= 0", ErrInvalid)
	}
	for _, w := range append(append([]QuietWindow{}, spec.QuietWindows...), spec.TestWindows...) {
		if err := validateQuietWindow(w); err != nil {
			return err
		}
	}
	return nil
}

// validateQuietWindow checks one timezone-aware interval policy (types.go:
// weekdays 0=Sunday..6=Saturday, "HH:MM" times, IANA Location).
func validateQuietWindow(w QuietWindow) error {
	for _, d := range w.Weekdays {
		if d < 0 || d > 6 {
			return fmt.Errorf("%w: quiet window weekday %d out of range (0=Sunday..6=Saturday)", ErrInvalid, d)
		}
	}
	if !validHHMM(w.Start) || !validHHMM(w.End) {
		return fmt.Errorf("%w: quiet window times must be HH:MM", ErrInvalid)
	}
	if _, err := time.LoadLocation(w.Location); err != nil {
		return fmt.Errorf("%w: quiet window location %q is not a loadable IANA name", ErrInvalid, w.Location)
	}
	return nil
}

// validHHMM reports whether s is a strict "HH:MM" 24-hour clock time.
func validHHMM(s string) bool {
	if len(s) != 5 || s[2] != ':' {
		return false
	}
	h, m := s[0:2], s[3:5]
	for _, part := range [2]string{h, m} {
		for i := 0; i < len(part); i++ {
			if part[i] < '0' || part[i] > '9' {
				return false
			}
		}
	}
	return h <= "23" && m <= "59"
}

// CreateEnvironment binds a runnable context with hard safety rails to a
// target of the caller's org.
func (s *Service) CreateEnvironment(ctx context.Context, orgID, targetID string, spec EnvSpec) (*Environment, error) {
	if s == nil {
		return nil, fmt.Errorf("synth: service is nil")
	}
	if err := validateEnvSpec(&spec); err != nil {
		return nil, err
	}
	// The target anchor must exist INSIDE the caller's org.
	t, err := s.GetTarget(ctx, orgID, targetID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	e := &Environment{
		ID: uuid.NewString(), OrgID: orgID, TargetID: t.ID,
		Kind: spec.Kind, Mode: spec.Mode,
		AllowedDomains: spec.AllowedDomains, RateCapPerMin: spec.RateCapPerMin,
		BudgetPerRun: spec.BudgetPerRun,
		QuietWindows: spec.QuietWindows, TestWindows: spec.TestWindows,
		CreatedAt: now, UpdatedAt: now,
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store != nil {
		if err := s.store.CreateEnvironment(ctx, e); err != nil {
			return nil, err
		}
	}
	s.environments[e.ID] = cloneEnvironment(e)
	return cloneEnvironment(e), nil
}

// GetEnvironment resolves one environment guarded by organization_id.
func (s *Service) GetEnvironment(ctx context.Context, orgID, id string) (*Environment, error) {
	if s == nil {
		return nil, fmt.Errorf("synth: service is nil")
	}
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(id) == "" {
		return nil, ErrNotFound
	}
	s.mu.RLock()
	if s.store == nil {
		e, ok := s.environments[id]
		s.mu.RUnlock()
		if !ok || e.OrgID != orgID {
			return nil, ErrNotFound
		}
		return cloneEnvironment(e), nil
	}
	s.mu.RUnlock()
	e, err := s.store.GetEnvironment(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.environments[e.ID] = cloneEnvironment(e)
	s.mu.Unlock()
	return cloneEnvironment(e), nil
}

// ListEnvironments returns the tenant's environments, optionally narrowed
// to one target (targetID "" lists across the org).
func (s *Service) ListEnvironments(ctx context.Context, orgID, targetID string) ([]*Environment, error) {
	if s == nil {
		return nil, fmt.Errorf("synth: service is nil")
	}
	if strings.TrimSpace(orgID) == "" {
		return nil, fmt.Errorf("%w: organization id is required", ErrInvalid)
	}
	s.mu.RLock()
	if s.store == nil {
		out := make([]*Environment, 0)
		for _, e := range s.environments {
			if e.OrgID == orgID && (targetID == "" || e.TargetID == targetID) {
				out = append(out, cloneEnvironment(e))
			}
		}
		s.mu.RUnlock()
		sortEnvironments(out)
		return out, nil
	}
	s.mu.RUnlock()
	list, err := s.store.ListEnvironments(ctx, orgID, targetID)
	if err != nil {
		return nil, err
	}
	if list == nil {
		list = []*Environment{}
	}
	sortEnvironments(list)
	return list, nil
}

// UpdateEnvironment replaces the safety posture of a tenant-owned
// environment. The full spec is re-validated, so a production environment
// can never escalate to pre_production and can never drop its allowlist.
func (s *Service) UpdateEnvironment(ctx context.Context, orgID, id string, spec EnvSpec) (*Environment, error) {
	if s == nil {
		return nil, fmt.Errorf("synth: service is nil")
	}
	if err := validateEnvSpec(&spec); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.environmentLocked(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	// Identity fields (org/target) are immutable; the posture is replaced.
	e.Kind = spec.Kind
	e.Mode = spec.Mode
	e.AllowedDomains = spec.AllowedDomains
	e.RateCapPerMin = spec.RateCapPerMin
	e.BudgetPerRun = spec.BudgetPerRun
	e.QuietWindows = spec.QuietWindows
	e.TestWindows = spec.TestWindows
	e.UpdatedAt = time.Now().UTC()
	if s.store != nil {
		if err := s.store.UpdateEnvironment(ctx, e); err != nil {
			return nil, err
		}
	}
	s.environments[e.ID] = cloneEnvironment(e)
	return cloneEnvironment(e), nil
}

// DeleteEnvironment removes a tenant-owned environment.
func (s *Service) DeleteEnvironment(ctx context.Context, orgID, id string) error {
	if s == nil {
		return fmt.Errorf("synth: service is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.environmentLocked(ctx, orgID, id); err != nil {
		return err
	}
	if s.store != nil {
		if err := s.store.DeleteEnvironment(ctx, orgID, id); err != nil {
			return err
		}
	}
	delete(s.environments, id)
	return nil
}

// ---------------------------------------------------------------------------
// personas
// ---------------------------------------------------------------------------

// PersonaSpec is the create/update input for a persona.
type PersonaSpec struct {
	Name        string            `json:"name"`
	BuiltIn     string            `json:"built_in"`
	RoleHints   []string          `json:"role_hints"`
	Constraints map[string]string `json:"constraints"`
}

// validatePersonaSpec enforces the types.go persona contract: a persona is
// either a documented built-in key or custom (BuiltIn == ""); constraints
// are the ONLY place declarative traits live (credentials stay out —
// personas map to synthetic credentials by reference only).
func validatePersonaSpec(spec *PersonaSpec) error {
	spec.Name = strings.TrimSpace(spec.Name)
	spec.BuiltIn = strings.TrimSpace(spec.BuiltIn)
	if spec.Name == "" {
		return fmt.Errorf("%w: persona name is required", ErrInvalid)
	}
	if spec.BuiltIn != "" && !builtInPersonas[spec.BuiltIn] {
		return fmt.Errorf("%w: built_in %q is not a documented built-in persona", ErrInvalid, spec.BuiltIn)
	}
	return nil
}

// CreatePersona registers a synthetic identity profile for the org.
func (s *Service) CreatePersona(ctx context.Context, orgID string, spec PersonaSpec) (*Persona, error) {
	if s == nil {
		return nil, fmt.Errorf("synth: service is nil")
	}
	if strings.TrimSpace(orgID) == "" {
		return nil, fmt.Errorf("%w: organization id is required", ErrInvalid)
	}
	if err := validatePersonaSpec(&spec); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	p := &Persona{
		ID: uuid.NewString(), OrgID: orgID,
		Name: spec.Name, BuiltIn: spec.BuiltIn,
		RoleHints: spec.RoleHints, Constraints: spec.Constraints,
		Version: 1, CreatedAt: now, UpdatedAt: now,
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store != nil {
		if err := s.store.CreatePersona(ctx, p); err != nil {
			return nil, err
		}
	}
	s.personas[p.ID] = clonePersona(p)
	return clonePersona(p), nil
}

// GetPersona resolves one persona guarded by organization_id.
func (s *Service) GetPersona(ctx context.Context, orgID, id string) (*Persona, error) {
	if s == nil {
		return nil, fmt.Errorf("synth: service is nil")
	}
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(id) == "" {
		return nil, ErrNotFound
	}
	s.mu.RLock()
	if s.store == nil {
		p, ok := s.personas[id]
		s.mu.RUnlock()
		if !ok || p.OrgID != orgID {
			return nil, ErrNotFound
		}
		return clonePersona(p), nil
	}
	s.mu.RUnlock()
	p, err := s.store.GetPersona(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.personas[p.ID] = clonePersona(p)
	s.mu.Unlock()
	return clonePersona(p), nil
}

// ListPersonas returns the tenant's personas ordered by creation time.
func (s *Service) ListPersonas(ctx context.Context, orgID string) ([]*Persona, error) {
	if s == nil {
		return nil, fmt.Errorf("synth: service is nil")
	}
	if strings.TrimSpace(orgID) == "" {
		return nil, fmt.Errorf("%w: organization id is required", ErrInvalid)
	}
	s.mu.RLock()
	if s.store == nil {
		out := make([]*Persona, 0)
		for _, p := range s.personas {
			if p.OrgID == orgID {
				out = append(out, clonePersona(p))
			}
		}
		s.mu.RUnlock()
		sortPersonas(out)
		return out, nil
	}
	s.mu.RUnlock()
	list, err := s.store.ListPersonas(ctx, orgID)
	if err != nil {
		return nil, err
	}
	if list == nil {
		list = []*Persona{}
	}
	sortPersonas(list)
	return list, nil
}

// UpdatePersona replaces the mutable fields of a tenant-owned persona and
// BUMPS the version (journeys pin persona versions — #149).
func (s *Service) UpdatePersona(ctx context.Context, orgID, id string, spec PersonaSpec) (*Persona, error) {
	if s == nil {
		return nil, fmt.Errorf("synth: service is nil")
	}
	if err := validatePersonaSpec(&spec); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.personaLocked(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	p.Name = spec.Name
	p.BuiltIn = spec.BuiltIn
	p.RoleHints = spec.RoleHints
	p.Constraints = spec.Constraints
	p.Version++
	p.UpdatedAt = time.Now().UTC()
	if s.store != nil {
		if err := s.store.UpdatePersona(ctx, p); err != nil {
			return nil, err
		}
	}
	s.personas[p.ID] = clonePersona(p)
	return clonePersona(p), nil
}

// DeletePersona removes a tenant-owned persona.
func (s *Service) DeletePersona(ctx context.Context, orgID, id string) error {
	if s == nil {
		return fmt.Errorf("synth: service is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.personaLocked(ctx, orgID, id); err != nil {
		return err
	}
	if s.store != nil {
		if err := s.store.DeletePersona(ctx, orgID, id); err != nil {
			return err
		}
	}
	delete(s.personas, id)
	return nil
}

// ---------------------------------------------------------------------------
// ScopeGuard seam (the worker tracks #148/#155 call this)
// ---------------------------------------------------------------------------

// ScopeDecision reasons (machine-readable; the ScopeDecision.Reason
// vocabulary from types.go, extended with the registry's own verdicts).
const (
	ReasonAllowlistedDomain  = "allowlisted_domain"
	ReasonNoAllowlist        = "no_allowlist_configured"
	ReasonWriteDeniedRO      = "write_denied_readonly"
	ReasonDomainNotAllowed   = "domain_not_allowed"
	ReasonRateCapped         = "rate_capped"
	ReasonTargetUnverified   = "target_unverified"
	ReasonReleaseComparison  = "release_comparison_write"
	ReasonPreProductionWrite = "pre_production_write"
)

// AssertExecutionAllowed is the ScopeGuard: the server-side scope/policy
// verdict the worker tracks must consult before ANY navigation or action.
// Untrusted page content can never expand authorized scope — only this
// function can.
//
// Verdict ladder (first failure wins):
//
//  1. env must exist in the caller's org            -> ErrNotFound
//  2. env's target must be VERIFIED                 -> ErrTargetUnverified
//     (denies ALL actions, reads and writes alike)
//  3. request host must be allowlisted              -> ErrScopeViolation
//     (production REQUIRES an allowlist at creation; non-production envs
//     without an allowlist permit same-org testing and report
//     ReasonNoAllowlist — documented, fail-explicit, never fail-open for
//     production)
//  4. writeOp on production_read_only               -> ErrReadOnlyMode
//  5. pre_production rate cap exceeded              -> ErrScopeViolation
//     (rate caps are honored only in pre_production per types.go; the
//     counter is per-process — distributed enforcement is the worker
//     plane's job)
//
// release_comparison allows writes (through isolated synthetic accounts —
// the persona-isolation obligation lands with #149 and is documented here).
// On allow the returned decision carries Allowed=true and the reason
// describing WHY the action is allowed; on deny the error is one of the
// typed errors from types.go wrapped with the decision.
func (s *Service) AssertExecutionAllowed(ctx context.Context, orgID, envID, requestURL string, writeOp bool) (ScopeDecision, error) {
	decision := ScopeDecision{Allowed: false, At: time.Now().UTC()}
	e, err := s.GetEnvironment(ctx, orgID, envID)
	if err != nil {
		return decision, err
	}
	t, err := s.GetTarget(ctx, orgID, e.TargetID)
	if err != nil {
		return decision, err
	}
	if !t.Verified {
		decision.Reason = ReasonTargetUnverified
		return decision, fmt.Errorf("%w: target %s failed the ownership challenge", ErrTargetUnverified, t.ID)
	}
	host := hostOf(requestURL)
	if len(e.AllowedDomains) > 0 {
		if !domainAllowed(host, e.AllowedDomains) {
			decision.Reason = ReasonDomainNotAllowed
			return decision, fmt.Errorf("%w: host %q is outside the environment's allowed domains", ErrScopeViolation, host)
		}
		decision.Reason = ReasonAllowlistedDomain
	} else {
		decision.Reason = ReasonNoAllowlist
	}
	if writeOp {
		switch e.Mode {
		case ModeProductionReadOnly:
			decision.Reason = ReasonWriteDeniedRO
			return decision, fmt.Errorf("%w: environment %s runs in production_read_only mode", ErrReadOnlyMode, e.ID)
		case ModeReleaseComparison:
			// Writes only through isolated synthetic accounts (#149 seam);
			// the registry records the posture, the worker proves isolation.
			decision.Reason = ReasonReleaseComparison
		default:
			decision.Reason = ReasonPreProductionWrite
		}
	}
	if e.Mode == ModePreProduction && e.RateCapPerMin > 0 && s.rateCapped(orgID, envID, e.RateCapPerMin) {
		decision.Reason = ReasonRateCapped
		return decision, fmt.Errorf("%w: pre_production rate cap of %d/min exceeded", ErrScopeViolation, e.RateCapPerMin)
	}
	decision.Allowed = true
	return decision, nil
}

// rateCapped records one observation and reports whether the per-minute
// cap for (org, env) is exceeded. Per-process, sliding one-minute window.
func (s *Service) rateCapped(orgID, envID string, cap int) bool {
	key := orgID + ":" + envID
	now := time.Now().UTC()
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	fresh := s.rateSeen[key][:0]
	for _, ts := range s.rateSeen[key] {
		if now.Sub(ts) < time.Minute {
			fresh = append(fresh, ts)
		}
	}
	if len(fresh) >= cap {
		s.rateSeen[key] = fresh
		return true
	}
	s.rateSeen[key] = append(fresh, now)
	return false
}

// ---------------------------------------------------------------------------
// internal helpers
// ---------------------------------------------------------------------------

// getLocked resolves a project under a HELD s.mu (store mode reads through
// to the store). Callers must hold s.mu.
func (s *Service) getLocked(ctx context.Context, orgID, id string) (*Project, error) {
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(id) == "" {
		return nil, ErrNotFound
	}
	if s.store != nil {
		p, err := s.store.GetProject(ctx, orgID, id)
		if err != nil {
			return nil, err
		}
		s.projects[p.ID] = cloneProject(p)
		return cloneProject(p), nil
	}
	p, ok := s.projects[id]
	if !ok || p.OrgID != orgID {
		return nil, ErrNotFound
	}
	return cloneProject(p), nil
}

// targetLocked resolves a target under a HELD s.mu. Callers must hold s.mu.
func (s *Service) targetLocked(ctx context.Context, orgID, id string) (*Target, error) {
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(id) == "" {
		return nil, ErrNotFound
	}
	if s.store != nil {
		t, err := s.store.GetTarget(ctx, orgID, id)
		if err != nil {
			return nil, err
		}
		s.targets[t.ID] = cloneTarget(t)
		return cloneTarget(t), nil
	}
	t, ok := s.targets[id]
	if !ok || t.OrgID != orgID {
		return nil, ErrNotFound
	}
	return cloneTarget(t), nil
}

// environmentLocked resolves an environment under a HELD s.mu. Callers must
// hold s.mu.
func (s *Service) environmentLocked(ctx context.Context, orgID, id string) (*Environment, error) {
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(id) == "" {
		return nil, ErrNotFound
	}
	if s.store != nil {
		e, err := s.store.GetEnvironment(ctx, orgID, id)
		if err != nil {
			return nil, err
		}
		s.environments[e.ID] = cloneEnvironment(e)
		return cloneEnvironment(e), nil
	}
	e, ok := s.environments[id]
	if !ok || e.OrgID != orgID {
		return nil, ErrNotFound
	}
	return cloneEnvironment(e), nil
}

// personaLocked resolves a persona under a HELD s.mu. Callers must hold s.mu.
func (s *Service) personaLocked(ctx context.Context, orgID, id string) (*Persona, error) {
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(id) == "" {
		return nil, ErrNotFound
	}
	if s.store != nil {
		p, err := s.store.GetPersona(ctx, orgID, id)
		if err != nil {
			return nil, err
		}
		s.personas[p.ID] = clonePersona(p)
		return clonePersona(p), nil
	}
	p, ok := s.personas[id]
	if !ok || p.OrgID != orgID {
		return nil, ErrNotFound
	}
	return clonePersona(p), nil
}

// validateBaseURL requires an http(s) URL with a host — targets are web
// applications under test, not opaque strings.
func validateBaseURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("%w: target base_url is required", ErrInvalid)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("%w: target base_url %q must be an absolute http(s) URL", ErrInvalid, raw)
	}
	return nil
}

// newChallengeToken mints 32 random bytes as hex — the ownership challenge
// secret (one-time, never exported via the API after issue).
func newChallengeToken() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failure is unrecoverable for a security token.
		panic("synth: challenge token entropy unavailable: " + err.Error())
	}
	return hex.EncodeToString(buf)
}

// hostOf extracts the lowercase host (port, userinfo, path, query and
// fragment stripped) from a URL-ish request string; "" when nothing
// host-like remains. Best-effort, mirroring the tenant.HostOf convention.
func hostOf(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	s := raw
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#/"); i >= 0 {
		s = s[:i]
	}
	if at := strings.LastIndex(s, "@"); at >= 0 {
		s = s[at+1:]
	}
	// Bracketed IPv6 literals: take the inner address verbatim.
	if strings.HasPrefix(s, "[") {
		if end := strings.Index(s, "]"); end >= 0 {
			return strings.ToLower(s[1:end])
		}
	}
	if h, _, ok := strings.Cut(s, ":"); ok {
		s = h // strip :port
	}
	return strings.ToLower(strings.Trim(s, "[]"))
}

// domainAllowed reports whether host matches one of the allowlisted
// domains. Matching is suffix-based on LABEL boundaries: "api.example.com"
// matches allowlist entry "example.com" but "evilexample.com" does not.
func domainAllowed(host string, domains []string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return false
	}
	for _, d := range domains {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "" {
			continue
		}
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// --- shallow copies (never hand callers our internal pointers) -------------

func cloneProject(p *Project) *Project { c := *p; return &c }
func cloneTarget(t *Target) *Target    { c := *t; return &c }
func cloneEnvironment(e *Environment) *Environment {
	c := *e
	c.AllowedDomains = append([]string(nil), e.AllowedDomains...)
	c.QuietWindows = append([]QuietWindow(nil), e.QuietWindows...)
	c.TestWindows = append([]QuietWindow(nil), e.TestWindows...)
	return &c
}
func clonePersona(p *Persona) *Persona {
	c := *p
	c.RoleHints = append([]string(nil), p.RoleHints...)
	c.Constraints = copyConstraints(p.Constraints)
	return &c
}

func copyConstraints(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// --- deterministic list ordering (creation time, then id) ------------------

// sortByCreation orders one resource list by creation time then id —
// stable, tenant-friendly ordering shared by every List method.
type sortByCreation[T interface{ createdAtKey() (time.Time, string) }] []T

func (l sortByCreation[T]) Len() int { return len(l) }
func (l sortByCreation[T]) Less(i, j int) bool {
	ti, idi := l[i].createdAtKey()
	tj, idj := l[j].createdAtKey()
	if !ti.Equal(tj) {
		return ti.Before(tj)
	}
	return idi < idj
}
func (l sortByCreation[T]) Swap(i, j int) { l[i], l[j] = l[j], l[i] }

func (p *Project) createdAtKey() (time.Time, string)     { return p.CreatedAt, p.ID }
func (t *Target) createdAtKey() (time.Time, string)      { return t.CreatedAt, t.ID }
func (e *Environment) createdAtKey() (time.Time, string) { return e.CreatedAt, e.ID }
func (p *Persona) createdAtKey() (time.Time, string)     { return p.CreatedAt, p.ID }

func sortProjects(list []*Project) { sort.Sort(sortByCreation[*Project](list)) }
func sortTargets(list []*Target)   { sort.Sort(sortByCreation[*Target](list)) }
func sortEnvironments(list []*Environment) {
	sort.Sort(sortByCreation[*Environment](list))
}
func sortPersonas(list []*Persona) { sort.Sort(sortByCreation[*Persona](list)) }
