package deployments

// Canary deployments with deterministic traffic splitting (issue #13).
//
// Design
// ------
// A deployment row owns BOTH serving versions of one agent+environment:
//
//      Version        - the STABLE version (what most traffic serves)
//      CanaryVersion  - the CANARY version (0 = no canary configured)
//      CanaryWeight   - percentage of traffic routed to the canary (0-100)
//
// The traffic split is resolved from the environment's single HEALTHY
// deployment row (the row the rollback/promote lifecycle already treats as
// "what serves traffic"); canary fields on non-healthy rows are staged config
// that never serves. Transitions:
//
//      ConfigureCanary  attach/replace the canary version on a healthy row
//      SetCanaryWeight  move the split point (0-100, rejected outside the range)
//      PromoteCanary    the canary becomes stable: Version = CanaryVersion and the
//                       canary config is cleared (weight -> 0). The row STAYS
//                       healthy - no lifecycle transition, no new ledger row - so
//                       the one-healthy-per-agent+environment invariant holds and
//                       history remains append-only.
//      AbortCanary      clear the canary config, keep the stable version serving
//                       100% (the split simply disappears; no new row needed
//                       because the stable version never changed).
//      ResolveVersion   deterministic per-agent selection (below)
//
// Selection strategy (documented per the issue)
// ---------------------------------------------
// ResolveVersionCtx computes bucket = FNV-1a32("<orgID>/<agentID>") % 100 and
// serves the canary version when bucket < CanaryWeight, else the stable
// version. FNV-1a is seedless and stable across processes and Go releases, so
// the SAME agent always lands on the SAME side for a given weight (sticky
// routing): weight 0 -> always stable, weight 100 -> always canary, and across
// a fleet of agents roughly CanaryWeight% of them resolve to the canary. No
// per-request randomness is involved.

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"time"

	"agentos/internal/agents"
)

var (
	// ErrInvalidCanaryWeight is returned when a canary weight falls outside
	// the 0-100 domain. Invalid input is REJECTED, not silently clamped.
	ErrInvalidCanaryWeight = errors.New("canary_weight must be an integer between 0 and 100")
	// ErrInvalidCanaryVersion is returned when a canary version is not a
	// usable canary target: non-positive, equal to the deployment's stable
	// version, or nonexistent for the agent (a canary may target ANY
	// existing version of the same agent - draft or archived included,
	// because only one version per agent can be published at a time and a
	// canary by definition runs NEXT to the published stable version).
	ErrInvalidCanaryVersion = errors.New("canary version must be an existing version of the same agent and differ from the stable version")
	// ErrNoCanary is returned by canary operations that require a canary
	// config which the deployment does not carry (set weight before
	// attaching a version, promote/abort without a canary).
	ErrNoCanary = errors.New("deployment has no canary configured")
	// ErrNoServingDeployment is returned by ResolveVersionCtx when no
	// healthy deployment serves the agent+environment (nothing to resolve).
	ErrNoServingDeployment = errors.New("no healthy deployment serves this agent+environment")
)

// VersionExistenceChecker validates that a config version exists within one
// tenant+agent regardless of publication status. internal/agents
// .VersionsService satisfies it with GetVersionCtx; the deployments Service
// picks it up automatically when the injected VersionChecker also implements
// it (nil disables the existence check - dev/test mode only, same contract as
// VersionChecker).
type VersionExistenceChecker interface {
	GetVersionCtx(ctx context.Context, orgID, agentID string, version int) (*agents.ConfigVersion, error)
}

// HasCanary reports whether the deployment carries a canary config.
func (d *Deployment) HasCanary() bool {
	return d != nil && d.CanaryVersion > 0
}

// canaryBucket maps (orgID, agentID) to a stable bucket in [0, 100). FNV-1a
// (seedless, deterministic across processes/Go versions) over "<orgID>/<agentID>".
func canaryBucket(orgID, agentID string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(orgID))
	_, _ = h.Write([]byte("/"))
	_, _ = h.Write([]byte(agentID))
	return int(h.Sum32() % 100)
}

// resolvesToCanary applies the deterministic strategy: the agent's fixed
// bucket compared against the weight threshold.
func resolvesToCanary(orgID, agentID string, weight int) bool {
	return canaryBucket(orgID, agentID) < weight
}

// CreateCanaryDeploymentCtx creates a deployment with an optional canary
// config (canaryVersion 0 = no canary; call CreateDeploymentCtx instead for
// that). Validation: weight 0-100 (rejected outside), canary version must
// differ from the stable version and must be an EXISTING version of the SAME
// agent (any status - see VersionExistenceChecker). The row starts in status
// requested; the split only applies once the row becomes the environment's
// healthy deployment.
func (s *Service) CreateCanaryDeploymentCtx(ctx context.Context, orgID, agentID string, version, canaryVersion, canaryWeight int, environment, createdBy string) (*Deployment, error) {
	if canaryVersion <= 0 {
		return nil, ErrInvalidCanaryVersion
	}
	return s.createDeployment(ctx, orgID, agentID, version, canaryVersion, canaryWeight, environment, createdBy)
}

// canaryVersionExists validates the canary target: within the tenant+agent it
// must be an existing version (any status). Cross-agent or unknown versions
// are rejected because GetVersionCtx scopes the lookup by organization_id AND
// agent_id.
func (s *Service) canaryVersionExists(ctx context.Context, orgID, agentID string, canaryVersion int) error {
	if s.finder == nil {
		return nil
	}
	if _, err := s.finder.GetVersionCtx(ctx, orgID, agentID, canaryVersion); err != nil {
		return ErrInvalidCanaryVersion
	}
	return nil
}

// SetCanaryVersionCtx attaches (or replaces) the canary version on a healthy
// deployment. The weight is left unchanged (0 when attaching to a canary-less
// row: ramp starts at 0% and SetCanaryWeight moves it). Rejected when the
// deployment is not healthy, the version equals the stable version, or the
// version is not a published version of the same agent.
func (s *Service) SetCanaryVersionCtx(ctx context.Context, orgID, depID string, canaryVersion int) (*Deployment, error) {
	if canaryVersion <= 0 {
		return nil, ErrInvalidCanaryVersion
	}
	deployment, err := s.GetDeploymentCtx(ctx, orgID, depID)
	if err != nil {
		return nil, err
	}
	if deployment.Status != StatusHealthy {
		return nil, fmt.Errorf("%w: canary operations require a healthy deployment", ErrInvalidTransition)
	}
	if canaryVersion == deployment.Version {
		return nil, ErrInvalidCanaryVersion
	}
	if err := s.canaryVersionExists(ctx, orgID, deployment.AgentID, canaryVersion); err != nil {
		return nil, err
	}
	s.mu.Lock()
	deployment.CanaryVersion = canaryVersion
	deployment.UpdatedAt = time.Now().UTC()
	// Issue #51: (re)attaching a canary opens a FRESH evaluation window —
	// the policy (when configured) restarts its evidence collection and any
	// prior decision is void. The policy itself is the operator's standing
	// rule for this row and survives attach/replace.
	if deployment.Promotion != nil {
		deployment.Promotion.WindowStart = deployment.UpdatedAt
		deployment.Promotion.Decision = nil
	}
	err = s.persistUpdate(ctx, orgID, deployment)
	s.mu.Unlock()
	return deployment, err
}

// SetCanaryWeightCtx moves the split point of an existing canary (0-100).
// Weights outside the range are rejected (ErrInvalidCanaryWeight), not
// clamped, so invalid inputs are never silently accepted.
func (s *Service) SetCanaryWeightCtx(ctx context.Context, orgID, depID string, weight int) (*Deployment, error) {
	if weight < 0 || weight > 100 {
		return nil, ErrInvalidCanaryWeight
	}
	deployment, err := s.GetDeploymentCtx(ctx, orgID, depID)
	if err != nil {
		return nil, err
	}
	if !deployment.HasCanary() {
		return nil, ErrNoCanary
	}
	if deployment.Status != StatusHealthy {
		return nil, fmt.Errorf("%w: canary operations require a healthy deployment", ErrInvalidTransition)
	}
	s.mu.Lock()
	deployment.CanaryWeight = weight
	deployment.UpdatedAt = time.Now().UTC()
	err = s.persistUpdate(ctx, orgID, deployment)
	s.mu.Unlock()
	return deployment, err
}

// PromoteCanaryCtx makes the canary the stable version: Version is swapped to
// the canary version and the canary config is cleared (weight -> 0). The row
// stays healthy; no lifecycle transition occurs. Rejected when the deployment
// has no canary (ErrNoCanary) or is not healthy (ErrInvalidTransition).
func (s *Service) PromoteCanaryCtx(ctx context.Context, orgID, depID string) (*Deployment, error) {
	deployment, err := s.GetDeploymentCtx(ctx, orgID, depID)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applyCanaryPromoteLocked(ctx, orgID, deployment)
}

// applyCanaryPromoteLocked is the transition body of PromoteCanaryCtx for a
// deployment already fetched. The caller MUST hold s.mu (in-memory rows are
// shared pointers; the canary promotion engine applies its auto-decisions
// through the same locked path so engine and manual actions serialize).
func (s *Service) applyCanaryPromoteLocked(ctx context.Context, orgID string, deployment *Deployment) (*Deployment, error) {
	if !deployment.HasCanary() {
		return nil, ErrNoCanary
	}
	if deployment.Status != StatusHealthy {
		return nil, fmt.Errorf("%w: canary operations require a healthy deployment", ErrInvalidTransition)
	}
	deployment.Version = deployment.CanaryVersion
	deployment.CanaryVersion = 0
	deployment.CanaryWeight = 0
	deployment.UpdatedAt = time.Now().UTC()
	return deployment, s.persistUpdate(ctx, orgID, deployment)
}

// AbortCanaryCtx clears the canary config and keeps the stable version serving
// 100% of traffic. Rejected when the deployment has no canary (ErrNoCanary) or
// is not healthy (ErrInvalidTransition).
func (s *Service) AbortCanaryCtx(ctx context.Context, orgID, depID string) (*Deployment, error) {
	deployment, err := s.GetDeploymentCtx(ctx, orgID, depID)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applyCanaryAbortLocked(ctx, orgID, deployment)
}

// applyCanaryAbortLocked is the transition body of AbortCanaryCtx for a
// deployment already fetched. The caller MUST hold s.mu (same contract as
// applyCanaryPromoteLocked).
func (s *Service) applyCanaryAbortLocked(ctx context.Context, orgID string, deployment *Deployment) (*Deployment, error) {
	if !deployment.HasCanary() {
		return nil, ErrNoCanary
	}
	if deployment.Status != StatusHealthy {
		return nil, fmt.Errorf("%w: canary operations require a healthy deployment", ErrInvalidTransition)
	}
	deployment.CanaryVersion = 0
	deployment.CanaryWeight = 0
	deployment.UpdatedAt = time.Now().UTC()
	return deployment, s.persistUpdate(ctx, orgID, deployment)
}

// ResolveVersionCtx returns the version that serves traffic for one
// agent+environment, honoring the canary split of the current HEALTHY
// deployment using the deterministic per-agent strategy documented in the
// package canary.go header (bucket = FNV-1a32(orgID/agentID) % 100; canary
// when bucket < weight). With no canary configured (or weight 0) the stable
// version is returned; weight 100 always returns the canary. The same inputs
// always resolve to the same side.
func (s *Service) ResolveVersionCtx(ctx context.Context, orgID, agentID, environment string) (int, error) {
	if s == nil || orgID == "" || agentID == "" {
		return 0, ErrNoServingDeployment
	}
	if !validEnvironment(environment) {
		return 0, ErrInvalidEnvironment
	}
	healthy, err := s.healthyDeployment(ctx, orgID, agentID, environment)
	if err != nil {
		return 0, err
	}
	if healthy == nil {
		return 0, ErrNoServingDeployment
	}
	if healthy.HasCanary() && resolvesToCanary(orgID, agentID, healthy.CanaryWeight) {
		return healthy.CanaryVersion, nil
	}
	return healthy.Version, nil
}
