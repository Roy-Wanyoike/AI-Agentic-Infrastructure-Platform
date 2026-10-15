package qualitygates

// service.go — the tenant-scoped application service (issue #152).
//
// Owns the policy lifecycle (create / versioned update / delete / list /
// get), exception approval (time-bounded, audited, approver forced to the
// authenticated actor) and the evaluate orchestration (policy + explicit
// evidence refs -> Decision via the pure Decide).
//
// Storage: PolicyStore + EvidenceStore are injected; the in-memory stores
// are the default in BOTH API modes this round — the Postgres policy store
// is a documented follow-up (pre-assign the migration with the
// orchestrator; no migration ships in this round).
//
// Time: the service reads its injectable clock (SetClock), defaulting to
// time.Now — tests pin time for determinism.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// PolicyStore persists versioned gate policies per tenant. Implementations
// MUST scope every operation by organization_id (foreign IDs resolve to
// ErrPolicyNotFound — tenants can never probe each other's policies).
type PolicyStore interface {
	// PutPolicy inserts version 1 of a policy (existing ids are rejected).
	PutPolicy(ctx context.Context, policy *GatePolicy) error
	// AppendPolicyVersion appends a NEW version of an existing policy
	// (the store owns version numbering: version = last + 1).
	AppendPolicyVersion(ctx context.Context, policy *GatePolicy) (*GatePolicy, error)
	// GetPolicyVersion returns one exact version (version <= 0 = latest).
	GetPolicyVersion(ctx context.Context, orgID, id string, version int) (*GatePolicy, error)
	// ListPolicies returns the LATEST version of every policy, name-sorted.
	ListPolicies(ctx context.Context, orgID string) ([]*GatePolicy, error)
	// DeletePolicy removes every version of the policy.
	DeletePolicy(ctx context.Context, orgID, id string) error
}

// AuditSink is the best-effort audit seam (wired to internal/audit via an
// adapter in cmd/api). Failures never break the caller's operation — the
// adapter swallows them (house style, mirrors audit publishers elsewhere).
type AuditSink func(ctx context.Context, actor, action, orgID, resource string, metadata map[string]any)

// Audit actions emitted by the service.
const (
	AuditPolicyCreated       = "qualitygates.policy_created"
	AuditPolicyUpdated       = "qualitygates.policy_updated"
	AuditPolicyDeleted       = "qualitygates.policy_deleted"
	AuditExceptionApproved   = "qualitygates.exception_approved"
	AuditDecisionRecommended = "qualitygates.decision_recommended"
)

// Service is the tenant-scoped quality-gate application service.
type Service struct {
	mu       sync.Mutex // serializes policy mutations (version integrity)
	policies PolicyStore
	evidence EvidenceStore
	audit    AuditSink
	now      func() time.Time
}

// NewService returns a service over the given stores (either may be nil —
// calls then fail with the corresponding NotFound/invalid errors; the HTTP
// layer always injects both).
func NewService(policies PolicyStore, evidence EvidenceStore) *Service {
	return &Service{policies: policies, evidence: evidence, now: time.Now}
}

// SetAuditSink wires the best-effort audit seam (call before serving).
func (s *Service) SetAuditSink(sink AuditSink) {
	if s == nil {
		return
	}
	s.audit = sink
}

// SetClock replaces the service clock (tests pin time; production keeps
// time.Now).
func (s *Service) SetClock(now func() time.Time) {
	if s == nil || now == nil {
		return
	}
	s.now = now
}

// auditLocked emits one best-effort audit row (never fails the caller).
func (s *Service) auditLocked(ctx context.Context, actor, action, orgID, resource string, metadata map[string]any) {
	if s.audit == nil {
		return
	}
	s.audit(ctx, actor, action, orgID, resource, metadata)
}

// CreatePolicy validates and stores version 1 of a new policy. The caller-
// supplied ID/org/timestamps are overwritten: the service owns identity
// (uuid), tenancy (from ctx claims at the HTTP layer) and stamps.
func (s *Service) CreatePolicy(ctx context.Context, orgID, actor string, in GatePolicy) (*GatePolicy, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgRequired
	}
	in.ID = uuid.NewString()
	in.OrgID = orgID
	in.Version = 1
	in.CreatedBy = actor
	now := s.now().UTC()
	in.CreatedAt = now
	in.UpdatedAt = now
	if err := in.Validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.policies.PutPolicy(ctx, &in); err != nil {
		return nil, err
	}
	created := in
	s.auditLocked(ctx, actor, AuditPolicyCreated, orgID, in.ID, map[string]any{
		"name":               in.Name,
		"version":            in.Version,
		"evaluation_version": in.EvaluationVersion(),
		"required_suites":    suiteKinds(&in),
	})
	return &created, nil
}

// UpdatePolicy validates the input and appends a NEW immutable version of
// the policy (history is retained; decisions keep citing the version they
// ran against). CreatedAt/CreatedBy/version-history fields are carried
// from the stored policy.
func (s *Service) UpdatePolicy(ctx context.Context, orgID, id, actor string, in GatePolicy) (*GatePolicy, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgRequired
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.policies.GetPolicyVersion(ctx, orgID, id, 0)
	if err != nil {
		return nil, err
	}
	in.ID = current.ID
	in.OrgID = orgID
	in.CreatedAt = current.CreatedAt
	in.CreatedBy = current.CreatedBy
	now := s.now().UTC()
	in.UpdatedAt = now
	if err := in.Validate(); err != nil {
		return nil, err
	}
	updated, err := s.policies.AppendPolicyVersion(ctx, &in)
	if err != nil {
		return nil, err
	}
	s.auditLocked(ctx, actor, AuditPolicyUpdated, orgID, current.ID, map[string]any{
		"name":               in.Name,
		"version":            updated.Version,
		"evaluation_version": updated.EvaluationVersion(),
		"required_suites":    suiteKinds(updated),
	})
	return updated, nil
}

// DeletePolicy removes every version of the policy (audited).
func (s *Service) DeletePolicy(ctx context.Context, orgID, id, actor string) error {
	if strings.TrimSpace(orgID) == "" {
		return ErrOrgRequired
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.policies.GetPolicyVersion(ctx, orgID, id, 0)
	if err != nil {
		return err
	}
	if err := s.policies.DeletePolicy(ctx, orgID, id); err != nil {
		return err
	}
	s.auditLocked(ctx, actor, AuditPolicyDeleted, orgID, id, map[string]any{
		"name":    current.Name,
		"version": current.Version,
	})
	return nil
}

// ListPolicies returns the latest version of every tenant policy.
func (s *Service) ListPolicies(ctx context.Context, orgID string) ([]*GatePolicy, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgRequired
	}
	return s.policies.ListPolicies(ctx, orgID)
}

// GetPolicy returns one policy version (version <= 0 = latest).
func (s *Service) GetPolicy(ctx context.Context, orgID, id string, version int) (*GatePolicy, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgRequired
	}
	return s.policies.GetPolicyVersion(ctx, orgID, id, version)
}

// ApproveException approves a time-bounded waiver and appends it to the
// policy as a NEW version (policies are immutable versions — an exception
// is a policy mutation, so approval is versioned and auditable like any
// other change).
//
// Security contract: ApprovedBy is ALWAYS the authenticated actor
// (attribution cannot be forged via the request body); the expiry must be
// strictly in the future at approval time; security-critical suites are
// structurally unwaivable (the store validation refuses
// security_adversarial suites and Decide ignores exceptions for
// FailClosed requirements even against hand-built policies).
func (s *Service) ApproveException(ctx context.Context, orgID, id, actor string, ex Exception) (*GatePolicy, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgRequired
	}
	if strings.TrimSpace(actor) == "" {
		return nil, fmt.Errorf("%w: approval requires an authenticated actor", ErrInvalidException)
	}
	ex.Suite = SuiteKind(strings.TrimSpace(string(ex.Suite)))
	ex.ApprovedBy = actor
	now := s.now().UTC()
	ex.ApprovedAt = now
	if err := validateExceptionShape(ex); err != nil {
		return nil, err
	}
	if !ex.ExpiresAt.After(now) {
		return nil, fmt.Errorf("%w: expires_at must be in the future", ErrInvalidException)
	}
	if ex.Suite == SuiteSecurityAdversarial {
		return nil, fmt.Errorf("%w: security_adversarial suites cannot be waived (fail-closed, structural)", ErrInvalidException)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.policies.GetPolicyVersion(ctx, orgID, id, 0)
	if err != nil {
		return nil, err
	}
	next := *current
	next.Version = current.Version + 1
	next.UpdatedAt = now
	next.Exceptions = append(append([]Exception(nil), current.Exceptions...), ex)
	if err := next.Validate(); err != nil {
		return nil, err
	}
	updated, err := s.policies.AppendPolicyVersion(ctx, &next)
	if err != nil {
		return nil, err
	}
	s.auditLocked(ctx, actor, AuditExceptionApproved, orgID, id, map[string]any{
		"exception_suite":    string(ex.Suite),
		"exception_reason":   ex.Reason,
		"exception_expires":  ex.ExpiresAt.UTC().Format(time.RFC3339),
		"version":            updated.Version,
		"evaluation_version": updated.EvaluationVersion(),
	})
	return updated, nil
}

// Evaluate resolves the policy and the explicit evidence refs (tenant-
// scoped) and returns the deterministic Decision at the service clock's
// current instant. Unknown or foreign refs are ErrEvidenceNotFound (the
// caller explicitly named them — unlike the Decide-level "missing required
// evidence -> blocked", which covers REQUIRED suites nobody supplied).
// Every evaluation is audited with its recommendation + pinned policy
// versions (the audit-grade release-assurance trail).
func (s *Service) Evaluate(ctx context.Context, orgID, policyID string, evidenceRefs []string) (*Decision, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgRequired
	}
	policy, err := s.policies.GetPolicyVersion(ctx, orgID, policyID, 0)
	if err != nil {
		return nil, err
	}
	if !policy.Active {
		return nil, ErrInactivePolicy
	}
	records := make([]EvidenceRecord, 0, len(evidenceRefs))
	for _, ref := range evidenceRefs {
		rec, err := s.evidence.GetEvidence(ctx, orgID, ref)
		if err != nil {
			return nil, err
		}
		records = append(records, *rec)
	}
	decision := Decide(policy, records, s.now())
	s.auditLocked(ctx, "", AuditDecisionRecommended, orgID, policyID, map[string]any{
		"recommendation":            string(decision.Recommendation),
		"policy_version":            decision.PolicyVersion,
		"policy_evaluation_version": decision.PolicyEvaluationVersion,
	})
	return decision, nil
}

// suiteKinds returns the sorted kind labels of a policy's requirements
// (stable audit metadata).
func suiteKinds(p *GatePolicy) []string {
	kinds := make([]string, 0, len(p.RequiredSuites))
	for _, r := range p.RequiredSuites {
		kinds = append(kinds, string(r.Kind))
	}
	sort.Strings(kinds)
	return kinds
}

// InMemoryPolicyStore is the default PolicyStore (both API modes this
// round). It keeps the FULL version history per policy id; reads return
// copies (stored policies are immutable by contract). Concurrency-safe.
type InMemoryPolicyStore struct {
	mu    sync.RWMutex
	items map[string]map[string][]GatePolicy // orgID -> id -> versions (index = version-1)
	nower func() time.Time
}

// NewInMemoryPolicyStore returns an empty store.
func NewInMemoryPolicyStore() *InMemoryPolicyStore {
	return &InMemoryPolicyStore{items: make(map[string]map[string][]GatePolicy)}
}

// PutPolicy implements PolicyStore (insert version 1 only).
func (s *InMemoryPolicyStore) PutPolicy(ctx context.Context, policy *GatePolicy) error {
	if policy == nil || policy.Version != 1 {
		return fmt.Errorf("%w: PutPolicy stores version 1 only", ErrInvalidPolicy)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.items == nil {
		s.items = make(map[string]map[string][]GatePolicy)
	}
	org := s.items[policy.OrgID]
	if org == nil {
		org = make(map[string][]GatePolicy)
		s.items[policy.OrgID] = org
	}
	if _, exists := org[policy.ID]; exists {
		return fmt.Errorf("%w: policy %s already exists (append versions with AppendPolicyVersion)", ErrInvalidPolicy, policy.ID)
	}
	org[policy.ID] = []GatePolicy{*policy}
	return nil
}

// AppendPolicyVersion implements PolicyStore (the store owns numbering).
func (s *InMemoryPolicyStore) AppendPolicyVersion(ctx context.Context, policy *GatePolicy) (*GatePolicy, error) {
	if policy == nil {
		return nil, fmt.Errorf("%w: nil policy", ErrInvalidPolicy)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	history := s.items[policy.OrgID][policy.ID]
	if len(history) == 0 {
		return nil, ErrPolicyNotFound
	}
	next := *policy
	next.Version = history[len(history)-1].Version + 1
	history = append(history, next)
	s.items[policy.OrgID][policy.ID] = history
	stored := history[len(history)-1]
	return &stored, nil
}

// GetPolicyVersion implements PolicyStore (version <= 0 = latest).
func (s *InMemoryPolicyStore) GetPolicyVersion(ctx context.Context, orgID, id string, version int) (*GatePolicy, error) {
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(id) == "" {
		return nil, ErrPolicyNotFound
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	history := s.items[orgID][id]
	if len(history) == 0 {
		return nil, ErrPolicyNotFound
	}
	if version <= 0 {
		version = history[len(history)-1].Version
	}
	if version < 1 || version > len(history) {
		return nil, ErrPolicyNotFound
	}
	out := history[version-1]
	return &out, nil
}

// ListPolicies implements PolicyStore (latest versions, name-sorted).
func (s *InMemoryPolicyStore) ListPolicies(ctx context.Context, orgID string) ([]*GatePolicy, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgRequired
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*GatePolicy, 0, len(s.items[orgID]))
	for _, history := range s.items[orgID] {
		if len(history) == 0 {
			continue
		}
		latest := history[len(history)-1]
		out = append(out, &latest)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// DeletePolicy implements PolicyStore (every version).
func (s *InMemoryPolicyStore) DeletePolicy(ctx context.Context, orgID, id string) error {
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(id) == "" {
		return ErrPolicyNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	history, ok := s.items[orgID][id]
	if !ok || len(history) == 0 {
		return ErrPolicyNotFound
	}
	delete(s.items[orgID], id)
	return nil
}
