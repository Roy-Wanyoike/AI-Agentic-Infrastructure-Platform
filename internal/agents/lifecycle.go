package agents

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"agentos/internal/audit"
)

// Issue #106: the agent lifecycle state machine.
//
//	draft → building → active → paused → archived
//	                     ↓         ↑
//	                     disabled ─┘
//
// The canonical persisted values are UPPERCASE, matching the historical
// agents.status column (migration 001 seeds 'DRAFT' and Service.CreateAgentCtx
// has always written "DRAFT"). Parsing is case-insensitive and whitespace
// tolerant so legacy rows, API payloads ("active") and Go callers
// (StateActive) all normalize onto the same states. Unknown or blank values
// never parse — callers must decide explicitly how to treat them (the
// lifecycle methods reject them; the run-creation guard treats an unparseable
// status as not-active, fail-closed).
//
// archived is TERMINAL: no transition leaves it; an archived agent is a
// read-only artifact (its runs and versions remain queryable).
const (
	StateDraft    AgentState = "DRAFT"
	StateBuilding AgentState = "BUILDING"
	StateActive   AgentState = "ACTIVE"
	StatePaused   AgentState = "PAUSED"
	StateDisabled AgentState = "DISABLED"
	StateArchived AgentState = "ARCHIVED"
)

// AgentState is one node of the agent lifecycle state machine. The zero
// value is not a valid state; use ParseAgentState (API boundary) or the
// State* constants (Go callers).
type AgentState string

// agentTransitions is the authoritative transition table (issue #106):
//
//	draft    → {building, archived}
//	building → {active, draft, disabled}
//	active   → {paused, disabled, archived}
//	paused   → {active, disabled, archived}
//	disabled → {building, draft, archived}
//	archived → {} (terminal)
//
// Every state must appear exactly once as a key; TestAgentTransitionsTable
// pins the table against the issue contract so a typo cannot silently
// widen (or close) a legal move.
var agentTransitions = map[AgentState][]AgentState{
	StateDraft:    {StateBuilding, StateArchived},
	StateBuilding: {StateActive, StateDraft, StateDisabled},
	StateActive:   {StatePaused, StateDisabled, StateArchived},
	StatePaused:   {StateActive, StateDisabled, StateArchived},
	StateDisabled: {StateBuilding, StateDraft, StateArchived},
	StateArchived: {},
}

// AllAgentStates returns every lifecycle state in table order
// (draft, building, active, paused, disabled, archived). A defensive copy:
// callers cannot mutate the transition table through the returned slice.
func AllAgentStates() []AgentState {
	return []AgentState{
		StateDraft, StateBuilding, StateActive, StatePaused, StateDisabled, StateArchived,
	}
}

// ParseAgentState normalizes a wire/storage value onto an AgentState.
// Matching is case-insensitive and trims surrounding whitespace, so the
// historical "DRAFT" rows, lowercase API payloads ("active") and future
// drift all land on the canonical state. The boolean is false for blank or
// unknown values — callers decide fail-open vs fail-closed.
func ParseAgentState(raw string) (AgentState, bool) {
	normalized := AgentState(strings.ToUpper(strings.TrimSpace(raw)))
	switch normalized {
	case StateDraft, StateBuilding, StateActive, StatePaused, StateDisabled, StateArchived:
		return normalized, true
	default:
		return "", false
	}
}

// CanTransition reports whether from → to is a legal move of the state
// machine. Unknown states (including "") can never transition.
func CanTransition(from, to AgentState) bool {
	targets, ok := agentTransitions[from]
	if !ok {
		return false
	}
	for _, target := range targets {
		if target == to {
			return true
		}
	}
	return false
}

// AllowedTransitions returns the legal target states from one state, in the
// deterministic table order (the issue #106 contract order, e.g.
// draft -> building, archived). Unknown states yield an empty (non-nil)
// slice. A defensive copy: mutating the result cannot corrupt the transition
// table.
func AllowedTransitions(from AgentState) []AgentState {
	return append([]AgentState(nil), agentTransitions[from]...)
}

// ErrInvalidTransition is the sentinel wrapped by every *TransitionError so
// callers can classify rejections with errors.Is without type assertions.
var ErrInvalidTransition = errors.New("invalid agent state transition")

// ErrUnknownAgentState is returned when a transition targets a value that is
// not one of the six lifecycle states (blank payloads, typos, future drift).
var ErrUnknownAgentState = errors.New("unknown agent state")

// TransitionError is the typed rejection for an illegal current → target
// move. It carries BOTH endpoints so API handlers can render precise
// messages ("cannot transition from ACTIVE to BUILDING") and tests can pin
// the offending pair. It always wraps ErrInvalidTransition.
type TransitionError struct {
	// Current is the state the agent was actually in when the move was
	// attempted (parsed case-insensitively; canonical form).
	Current AgentState
	// Target is the requested state (canonical form).
	Target AgentState
}

// Error implements error with the current → target pair embedded.
func (e *TransitionError) Error() string {
	return fmt.Sprintf("invalid agent state transition: %s → %s (allowed: %s)",
		e.Current, e.Target, strings.Join(stateNames(AllowedTransitions(e.Current)), ", "))
}

// Unwrap exposes the ErrInvalidTransition sentinel for errors.Is.
func (e *TransitionError) Unwrap() error { return ErrInvalidTransition }

func stateNames(states []AgentState) []string {
	names := make([]string, 0, len(states))
	for _, state := range states {
		names = append(names, string(state))
	}
	return names
}

// validateTransition resolves the agent's current status onto a lifecycle
// state and checks the move against the table. Shared by both transition
// entry points.
func validateTransition(currentStatus string, target AgentState) (AgentState, error) {
	if _, ok := ParseAgentState(string(target)); !ok {
		return "", fmt.Errorf("%w: %q", ErrUnknownAgentState, string(target))
	}
	current, ok := ParseAgentState(currentStatus)
	if !ok {
		// A status outside the state machine (hand-edited row, future
		// drift) is never silently migrated: the caller sees the raw value
		// and must reconcile it. Fail-closed.
		return "", fmt.Errorf("agent status %q is not a lifecycle state; reconcile it before transitioning", currentStatus)
	}
	if !CanTransition(current, target) {
		return current, &TransitionError{Current: current, Target: target}
	}
	return current, nil
}

// AgentAuditor is the narrow audit seam for lifecycle events. It is
// satisfied structurally by *audit.Service (its LogCtx signature matches),
// mirroring the mcp.Registry Auditor convention, so the wiring layer passes
// the shared service directly. Audit failures NEVER fail a transition.
type AgentAuditor interface {
	LogCtx(ctx context.Context, actor, action, organizationID, resource string, metadata map[string]any) (*audit.Entry, error)
}

// SetAuditor wires (or replaces) the lifecycle audit seam. Nil-safe; passing
// nil disables the audit emission. The seam is optional: transitions stay
// fully functional (and tested) without it.
func (s *Service) SetAuditor(auditor AgentAuditor) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auditor = auditor
}

// TransitionState moves one agent of one tenant to the target lifecycle
// state, rejecting illegal moves with *TransitionError (wrapping
// ErrInvalidTransition).
//
// Semantics:
//   - Tenant guard: the lookup is organization-scoped; foreign/unknown
//     agents surface as ErrAgentNotFound with no existence leak.
//   - The move is validated against the agentTransitions table BEFORE any
//     write; a rejected move leaves the agent (and its UpdatedAt) untouched.
//   - Concurrency: the whole read-validate-persist sequence runs under the
//     service mutex, so two racing transitions are serialized — exactly one
//     of them sees the pre-move state and the loser receives
//     *TransitionError instead of a corrupted status.
//   - Persistence: the new status is written through the Store when one is
//     configured (Postgres mode) and mirrored into the in-memory cache
//     otherwise. The status column already exists (migration 001,
//     DEFAULT 'DRAFT'); 032 adds the (organization_id, status) index.
//   - Audit: an agent.state_changed entry is recorded through the auditor
//     seam when one is wired (SetAuditor), best-effort.
func (s *Service) TransitionState(ctx context.Context, orgID, agentID string, target AgentState) error {
	_, err := s.TransitionStateWithAgent(ctx, orgID, agentID, target)
	return err
}

// TransitionStateWithAgent is the variant of TransitionState that returns
// the updated Agent (the same representation GetAgentCtx resolves).
func (s *Service) TransitionStateWithAgent(ctx context.Context, orgID, agentID string, target AgentState) (*Agent, error) {
	if s == nil {
		return nil, ErrAgentNotFound
	}
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(agentID) == "" {
		return nil, ErrAgentNotFound
	}
	// Validate the target up front so an unknown state never masks a
	// not-found with a confusing order of errors.
	if _, ok := ParseAgentState(string(target)); !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownAgentState, string(target))
	}

	// Linearizable transition: hold the service mutex across the whole
	// read-validate-persist sequence. (Go mutexes are not reentrant, so the
	// persist path writes the store directly instead of calling
	// UpdateAgentCtx, which would self-deadlock.)
	s.mu.Lock()
	defer s.mu.Unlock()

	var agent *Agent
	if s.store != nil {
		var err error
		// Tenant guard: WHERE id = $1 AND organization_id = $2.
		agent, err = s.store.GetAgent(ctx, orgID, agentID)
		if err != nil {
			return nil, ErrAgentNotFound
		}
	} else {
		cached, ok := s.agents[agentID]
		if !ok || cached.OrganizationID != orgID {
			return nil, ErrAgentNotFound
		}
		// Struct copy: in-memory mode shares cache pointers, and mutating
		// before validation would leak a partial transition past a
		// rejection.
		snapshot := *cached
		agent = &snapshot
	}

	from, err := validateTransition(agent.Status, target)
	if err != nil {
		return nil, err
	}

	agent.Status = string(target)
	agent.UpdatedAt = time.Now().UTC()

	if s.store != nil {
		if err := s.store.UpdateAgent(ctx, orgID, agent); err != nil {
			return nil, err
		}
	} else {
		cached := s.agents[agentID]
		*cached = *agent
	}

	s.recordStateChange(ctx, agent.OrganizationID, agent.ID, from, target)
	return agent, nil
}

// recordStateChange emits the agent.state_changed audit entry through the
// optional auditor seam (best-effort: audit failures never fail a
// transition that has already been persisted). from/target are canonical
// state values.
func (s *Service) recordStateChange(ctx context.Context, orgID, agentID string, from, target AgentState) {
	if s.auditor == nil {
		return
	}
	_, _ = s.auditor.LogCtx(ctx, "", "agent.state_changed", orgID,
		"agents/"+agentID, map[string]any{
			"from": string(from),
			"to":   string(target),
		})
}
