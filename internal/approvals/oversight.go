package approvals

// oversight.go — enterprise human-in-the-loop oversight v2 (issue #137):
// delegation chains, escalation SLAs, dual control (two-person rule) and the
// byte-deterministic evidence export, layered ADDITIVELY on the wave-2
// approvals service (existing exported signatures and statuses untouched).
//
// Design constraints carried over from the house conventions:
//   - DUAL MODE: in-memory maps remain the default; when the Service is built
//     with NewServiceWithStore and the Store also implements OversightStore
//     (the Postgres store does), oversight state is persisted through it.
//   - LAZY EVALUATION: expired delegations return the approval to the queue
//     and SLA breaches are stamped on List/Get access and by the explicit
//     EscalateDue sweep — there are NO background timers or goroutines.
//   - LOCKING: the service mutex guards the in-memory maps and the event
//     cache. Lazy evaluation NEVER runs while the caller holds the lock; the
//     lock is taken only around the mutation + event append, with an idem-
//     potence re-check inside, so concurrent readers cannot double-record.
//   - AUDIT: every oversight state change writes a best-effort audit row
//     through the Auditor seam (audit.Service satisfies it) using the same
//     LogCtx convention as the HTTP handlers.
//   - EVIDENCE: ExportEvidenceJSONL renders the org's immutable oversight
//     event log as NDJSON with a fixed field order (struct declaration order)
//     and RFC3339Nano UTC timestamps, mirroring internal/audit/export.go.
//     Two exports over unchanged data are byte-identical; export NEVER
//     mutates state (no lazy evaluation inside the export path).

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"agentos/internal/audit"
)

// StatusPartiallyApproved is the dual-control (two-person rule) marker: one
// distinct approver has decided, completion requires a second, different
// approver. It is a decidable (non-terminal) status.
const StatusPartiallyApproved = "partially_approved"

// EscalationReasonSLABreach is the machine-readable reason stamped on
// approvals escalated for missing the pending SLA.
const EscalationReasonSLABreach = "sla_breach"

// Oversight event types (the evidence export vocabulary). The set is closed
// and pinned by the migration 029 CHECK constraint on approval_events.type.
const (
	EventRequested         = "requested"
	EventDelegated         = "delegated"
	EventDelegationExpired = "delegation_expired"
	EventEscalated         = "escalated"
	EventFirstDecision     = "first_decision"
	EventDecided           = "decided"
)

// Audit actions emitted through the Auditor seam (issue #137 list). The
// evidence-export row (approval.evidence_exported) is written by the HTTP
// layer, which owns the request context.
const (
	AuditDelegated         = "approval.delegated"
	AuditDelegationExpired = "approval.delegation_expired"
	AuditEscalated         = "approval.escalated"
	AuditFirstDecision     = "approval.first_decision"
	AuditCompletedDual     = "approval.completed_dual"
	AuditEvidenceExported  = "approval.evidence_exported"
)

var (
	// ErrDualSelfApproval: dual control blocked the decision because the
	// approver is not distinct (same as the first approver, or the original
	// requester — maker must differ from checker).
	ErrDualSelfApproval = errors.New("dual control requires an approver distinct from the requester and the first approver")
	// ErrInvalidDelegation: delegation input is unusable (an approval that is
	// not pending surfaces as ErrAlreadyDecided instead).
	ErrInvalidDelegation = errors.New("delegation requires a to_user")
	// ErrInvalidEscalationPolicy: target status / SLA minutes are out of range.
	ErrInvalidEscalationPolicy = errors.New("escalation policy requires sla_minutes >= 0 and a target status of pending or partially_approved")
	// ErrInvalidEvidenceRange: evidence export window is inverted.
	ErrInvalidEvidenceRange = errors.New("evidence export: from must not be after to")
	// ErrOrgRequired mirrors audit.ErrOrgRequired for the oversight paths.
	ErrOrgRequired = errors.New("organization id is required")
)

// dualSelfApprovalError is the runtime error Decide returns when the
// two-person rule blocks a decision. It satisfies BOTH sentinels via
// errors.Is:
//   - ErrDualSelfApproval — the precise issue #137 identity (machine-readable
//     "dual_control_self_approval" mapping);
//   - ErrAlreadyDecided  — the wave-2 conflict sentinel, so the EXISTING
//     decision error mappers keep returning 409 Conflict for the block
//     instead of falling through to 500 (additive, backward-compatible:
//     no existing call site changes).
type dualSelfApprovalError struct{}

func (e *dualSelfApprovalError) Error() string { return ErrDualSelfApproval.Error() }

func (e *dualSelfApprovalError) Is(target error) bool {
	return target == ErrDualSelfApproval || target == ErrAlreadyDecided
}

// errDualSelfApproval returns the shared dual-control block error.
func errDualSelfApproval() error { return &dualSelfApprovalError{} }

// Delegation is one entry of an approval's delegation chain (from -> to with
// an optional expiry; the chain is append-only history).
type Delegation struct {
	ID             string     `json:"id"`
	OrganizationID string     `json:"organization_id"`
	ApprovalID     string     `json:"approval_id"`
	FromUser       string     `json:"from_user"`
	ToUser         string     `json:"to_user"`
	Note           string     `json:"note,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	ExpiresAt      *time.Time `json:"expires_at,omitempty"`
}

// EscalationPolicy is the per-org escalation SLA. target_status is the
// pending-state marker the SLA clock applies to (pending or
// partially_approved — the statuses a human still has to act on);
// sla_minutes == 0 disables escalation.
type EscalationPolicy struct {
	OrganizationID string    `json:"organization_id"`
	TargetStatus   string    `json:"target_status"`
	SLAMinutes     int       `json:"sla_minutes"`
	UpdatedBy      string    `json:"updated_by,omitempty"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// Event is one immutable oversight event. Detail is a free-form map (rendered
// with sorted keys by encoding/json, part of the determinism contract).
type Event struct {
	ID             string
	OrganizationID string
	ApprovalID     string
	Type           string
	Actor          string
	Detail         map[string]any
	CreatedAt      time.Time
}

// Auditor is the narrow best-effort audit seam (mirrors internal/mcp
// registry.go). *audit.Service satisfies it.
type Auditor interface {
	LogCtx(ctx context.Context, actor, action, organizationID, resource string, metadata map[string]any) (*audit.Entry, error)
}

// OversightStore is the ADDITIVE store contract for durable oversight state.
// It is intentionally a separate interface so existing Store implementations
// keep compiling; *pgStore implements both. Every statement is guarded by
// organization_id.
type OversightStore interface {
	// CreateDelegation appends one delegation-chain row.
	CreateDelegation(ctx context.Context, orgID string, d *Delegation) error
	// ListDelegations returns the chain of one approval within one tenant.
	ListDelegations(ctx context.Context, orgID, approvalID string) ([]*Delegation, error)
	// UpsertEscalationPolicy writes the org's single policy row.
	UpsertEscalationPolicy(ctx context.Context, orgID string, p *EscalationPolicy) error
	// GetEscalationPolicy resolves the org policy (nil, nil when unset).
	GetEscalationPolicy(ctx context.Context, orgID string) (*EscalationPolicy, error)
	// UpdateApprovalOversight persists the oversight columns of one approval.
	UpdateApprovalOversight(ctx context.Context, orgID, id string, a *Approval) error
	// InsertApprovalEvent appends one immutable oversight event row.
	InsertApprovalEvent(ctx context.Context, orgID string, e *Event) error
	// ListApprovalEvents returns the org's events inside [from, to] in
	// canonical order (created_at ASC, id ASC).
	ListApprovalEvents(ctx context.Context, orgID string, from, to time.Time) ([]*Event, error)
}

// SetAuditor wires the best-effort audit seam. Nil-safe (a nil auditor or nil
// receiver disables audit rows).
func (s *Service) SetAuditor(a Auditor) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auditor = a
}

// audit writes one best-effort oversight audit row (never fails the caller).
// Call WITHOUT the service lock held (LogCtx may hit the durable store).
func (s *Service) audit(ctx context.Context, actor, action, orgID, approvalID string, metadata map[string]any) {
	if s == nil {
		return
	}
	s.mu.Lock()
	auditor := s.auditor
	s.mu.Unlock()
	if auditor == nil {
		return
	}
	_, _ = auditor.LogCtx(ctx, actor, action, orgID, "approvals/"+approvalID, metadata)
}

// ---------------------------------------------------------------------------
// Delegation
// ---------------------------------------------------------------------------

// Delegate hands a PENDING approval to another org member: it stamps the
// delegation on the approval, appends the chain entry, records the oversight
// event and writes the best-effort audit row. Non-pending approvals are
// refused with ErrAlreadyDecided. expiresAt is optional (nil = open-ended
// delegation). Expired delegations lazily return the approval to the original
// queue on the next read (see lazyOne) — no timers.
func (s *Service) Delegate(ctx context.Context, orgID, approvalID, toUser, note string, expiresAt *time.Time, actor string) (*Approval, *Delegation, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, nil, ErrOrgRequired
	}
	toUser = strings.TrimSpace(toUser)
	if toUser == "" {
		return nil, nil, ErrInvalidDelegation
	}
	// Get runs the lazy pass first: an expired delegation must not block a
	// re-delegation (the approval is already back in the original queue).
	approval, err := s.Get(ctx, orgID, approvalID)
	if err != nil {
		return nil, nil, err
	}
	now := time.Now().UTC()
	delegation := &Delegation{
		ID:             uuid.NewString(),
		OrganizationID: orgID,
		ApprovalID:     approval.ID,
		FromUser:       strings.TrimSpace(actor),
		ToUser:         toUser,
		Note:           note,
		CreatedAt:      now,
		ExpiresAt:      expiresAt,
	}
	s.mu.Lock()
	if approval.Status != StatusPending {
		s.mu.Unlock()
		return nil, nil, ErrAlreadyDecided
	}
	approval.DelegatedTo = toUser
	approval.DelegatedBy = delegation.FromUser
	approval.DelegationNote = note
	approval.DelegationExpiresAt = expiresAt
	s.delegations[approval.ID] = append(s.delegations[approval.ID], delegation)
	s.appendEventLocked(ctx, orgID, approval.ID, EventDelegated, delegation.FromUser, map[string]any{
		"to":         toUser,
		"from":       delegation.FromUser,
		"note":       note,
		"expires_at": RFC3339OrEmpty(expiresAt),
	})
	oversight := s.oversight
	s.mu.Unlock()
	if oversight != nil {
		// The chain row is the source of truth for delegation history; the
		// columns carry the live assignment. Failure fails the operation.
		if err := oversight.CreateDelegation(ctx, orgID, delegation); err != nil {
			return nil, nil, err
		}
		if err := oversight.UpdateApprovalOversight(ctx, orgID, approval.ID, approval); err != nil {
			return nil, nil, err
		}
	}
	s.audit(ctx, delegation.FromUser, AuditDelegated, orgID, approval.ID, map[string]any{
		"to_user":    toUser,
		"note":       note,
		"expires_at": RFC3339OrEmpty(expiresAt),
	})
	return approval, delegation, nil
}

// Delegations returns the delegation chain of one approval within one tenant
// (oldest first).
func (s *Service) Delegations(ctx context.Context, orgID, approvalID string) ([]*Delegation, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgRequired
	}
	if s.oversight != nil {
		return s.oversight.ListDelegations(ctx, orgID, approvalID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Delegation, 0)
	for _, d := range s.delegations[approvalID] {
		if d.OrganizationID == orgID {
			out = append(out, d)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Escalation SLA
// ---------------------------------------------------------------------------

// SetEscalationPolicy writes the org's escalation policy. targetStatus
// defaults to pending; only non-terminal markers (pending,
// partially_approved) are accepted. slaMinutes < 0 is invalid; 0 disables the
// sweep. actor is recorded on the policy row.
func (s *Service) SetEscalationPolicy(ctx context.Context, orgID, targetStatus string, slaMinutes int, actor string) (*EscalationPolicy, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgRequired
	}
	targetStatus = strings.ToLower(strings.TrimSpace(targetStatus))
	if targetStatus == "" {
		targetStatus = StatusPending
	}
	if targetStatus != StatusPending && targetStatus != StatusPartiallyApproved {
		return nil, ErrInvalidEscalationPolicy
	}
	if slaMinutes < 0 {
		return nil, ErrInvalidEscalationPolicy
	}
	policy := &EscalationPolicy{
		OrganizationID: orgID,
		TargetStatus:   targetStatus,
		SLAMinutes:     slaMinutes,
		UpdatedBy:      strings.TrimSpace(actor),
		UpdatedAt:      time.Now().UTC(),
	}
	if s.oversight != nil {
		if err := s.oversight.UpsertEscalationPolicy(ctx, orgID, policy); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	s.policies[orgID] = policy
	s.mu.Unlock()
	return policy, nil
}

// GetEscalationPolicy resolves the org policy; an org without one gets the
// disabled default (sla_minutes = 0, target pending).
func (s *Service) GetEscalationPolicy(ctx context.Context, orgID string) (*EscalationPolicy, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgRequired
	}
	if policy := s.currentPolicy(ctx, orgID); policy != nil {
		return policy, nil
	}
	return &EscalationPolicy{OrganizationID: orgID, TargetStatus: StatusPending, SLAMinutes: 0}, nil
}

// currentPolicy resolves the live policy (store first, memory cache second);
// nil when the org has none. Call WITHOUT the service lock held.
func (s *Service) currentPolicy(ctx context.Context, orgID string) *EscalationPolicy {
	if s == nil {
		return nil
	}
	if s.oversight != nil {
		policy, err := s.oversight.GetEscalationPolicy(ctx, orgID)
		if err == nil && policy != nil {
			return policy
		}
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.policies[orgID]
}

// EscalateDue is the explicit SLA sweep: every approval still sitting in the
// policy's target status whose age reached the SLA (boundary inclusive:
// exactly at SLA = escalated) is stamped escalated with reason sla_breach.
// It returns the approvals escalated by THIS call. No background goroutine —
// the same evaluation also runs lazily on List/Get.
func (s *Service) EscalateDue(ctx context.Context, orgID, actor string) ([]*Approval, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgRequired
	}
	policy := s.currentPolicy(ctx, orgID)
	if policy == nil || policy.SLAMinutes <= 0 {
		return []*Approval{}, nil
	}
	now := time.Now().UTC()
	rows, err := s.listRaw(ctx, orgID, policy.TargetStatus)
	if err != nil {
		return nil, err
	}
	actor = strings.TrimSpace(actor)
	escalated := make([]*Approval, 0)
	for _, a := range rows {
		if a.Status != policy.TargetStatus || a.EscalatedAt != nil {
			continue
		}
		deadline := a.CreatedAt.Add(time.Duration(policy.SLAMinutes) * time.Minute)
		if now.Before(deadline) { // boundary inclusive: exactly at SLA = escalated
			continue
		}
		if !s.markEscalated(ctx, orgID, a, deadline, now, actor, policy.SLAMinutes) {
			continue
		}
		escalated = append(escalated, a)
	}
	return escalated, nil
}

// markEscalated stamps the SLA breach on one approval (idempotent via the
// under-lock re-check), persists it (store mode) and records event + audit.
// It reports whether THIS call performed the marking.
func (s *Service) markEscalated(ctx context.Context, orgID string, a *Approval, deadline, now time.Time, actor string, slaMinutes int) bool {
	s.mu.Lock()
	if a.EscalatedAt != nil {
		s.mu.Unlock()
		return false
	}
	a.EscalatedAt = &now
	a.EscalationReason = EscalationReasonSLABreach
	a.EscalationDeadline = &deadline
	s.appendEventLocked(ctx, orgID, a.ID, EventEscalated, actor, map[string]any{
		"reason":       EscalationReasonSLABreach,
		"deadline":     deadline.UTC().Format(time.RFC3339Nano),
		"escalated_at": now.UTC().Format(time.RFC3339Nano),
		"sla_minutes":  slaMinutes,
	})
	oversight := s.oversight
	s.mu.Unlock()
	if oversight != nil {
		// best-effort for the lazy path; the sweep surfaces store errors via
		// its own check below (the sweep re-persists and reports failure).
		_ = oversight.UpdateApprovalOversight(ctx, orgID, a.ID, a)
	}
	s.audit(ctx, actor, AuditEscalated, orgID, a.ID, map[string]any{
		"reason":   EscalationReasonSLABreach,
		"deadline": deadline.UTC().Format(time.RFC3339Nano),
	})
	return true
}

// ---------------------------------------------------------------------------
// Lazy evaluation (delegation expiry + SLA breach on access)
// ---------------------------------------------------------------------------

// lazyOversight applies the lazy oversight rules to a batch. Call WITHOUT the
// service lock held.
func (s *Service) lazyOversight(ctx context.Context, orgID string, list []*Approval) {
	if len(list) == 0 {
		return
	}
	policy := s.currentPolicy(ctx, orgID)
	now := time.Now().UTC()
	for _, a := range list {
		s.lazyOne(ctx, orgID, a, policy, now)
	}
}

// lazyOne evaluates the two lazy rules for one approval:
//  1. an expired delegation returns the approval to the original queue
//     (assignment columns cleared; the chain history is untouched);
//  2. an approval still in the policy's target status past its SLA (boundary
//     inclusive) is stamped escalated with reason sla_breach.
//
// Both rules are idempotent (the under-lock re-check records each
// application exactly once) and each application writes one event + one
// best-effort audit row. Call WITHOUT the service lock held.
func (s *Service) lazyOne(ctx context.Context, orgID string, a *Approval, policy *EscalationPolicy, now time.Time) {
	if a == nil {
		return
	}
	// 1. delegation expiry (pending only — decided approvals carry history,
	// not a live assignment).
	if a.Status == StatusPending && a.DelegatedTo != "" && a.DelegationExpiresAt != nil && now.After(*a.DelegationExpiresAt) {
		expiredTo := a.DelegatedTo
		var expiresAt time.Time
		s.mu.Lock()
		if a.DelegatedTo != "" { // re-check under the lock (idempotence)
			expiresAt = *a.DelegationExpiresAt
			a.DelegatedTo = ""
			a.DelegatedBy = ""
			a.DelegationNote = ""
			a.DelegationExpiresAt = nil
			s.appendEventLocked(ctx, orgID, a.ID, EventDelegationExpired, "system", map[string]any{
				"to":         expiredTo,
				"expires_at": expiresAt.UTC().Format(time.RFC3339Nano),
			})
		}
		oversight := s.oversight
		s.mu.Unlock()
		if oversight != nil {
			// best-effort: lazy evaluation must never fail a read.
			_ = oversight.UpdateApprovalOversight(ctx, orgID, a.ID, a)
		}
		s.audit(ctx, "system", AuditDelegationExpired, orgID, a.ID, map[string]any{
			"to_user": expiredTo,
		})
	}
	// 2. SLA breach.
	if policy != nil && policy.SLAMinutes > 0 && a.Status == policy.TargetStatus && a.EscalatedAt == nil {
		deadline := a.CreatedAt.Add(time.Duration(policy.SLAMinutes) * time.Minute)
		if !now.Before(deadline) { // boundary inclusive: exactly at SLA = escalated
			s.markEscalated(ctx, orgID, a, deadline, now, "system", policy.SLAMinutes)
		}
	}
}

// ---------------------------------------------------------------------------
// Oversight event log + evidence export
// ---------------------------------------------------------------------------

// appendEvent records one oversight event: durable row when the oversight
// store is present, plus the in-memory write-through cache. Call WITHOUT the
// lock held.
func (s *Service) appendEvent(ctx context.Context, orgID, approvalID, eventType, actor string, detail map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appendEventLocked(ctx, orgID, approvalID, eventType, actor, detail)
}

// appendEventLocked is the lock-held variant (caller owns s.mu).
func (s *Service) appendEventLocked(ctx context.Context, orgID, approvalID, eventType, actor string, detail map[string]any) {
	e := &Event{
		ID:             uuid.NewString(),
		OrganizationID: orgID,
		ApprovalID:     approvalID,
		Type:           eventType,
		Actor:          actor,
		Detail:         detail,
		CreatedAt:      time.Now().UTC(),
	}
	if s.oversight != nil {
		// best-effort: the in-memory cache always records the event; a failed
		// durable insert must not fail the approval operation.
		_ = s.oversight.InsertApprovalEvent(ctx, orgID, e)
	}
	s.events = append(s.events, e)
}

// Events returns the org's oversight events, optionally narrowed to one
// approval, in canonical order (created_at ASC, id ASC).
func (s *Service) Events(ctx context.Context, orgID, approvalID string) ([]*Event, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgRequired
	}
	all, err := s.eventsInRange(ctx, orgID, time.Time{}, time.Time{})
	if err != nil {
		return nil, err
	}
	out := make([]*Event, 0)
	for _, e := range all {
		if approvalID == "" || e.ApprovalID == approvalID {
			out = append(out, e)
		}
	}
	return out, nil
}

// eventsInRange snapshots the event source (store or memory cache) inside the
// [from, to] window (inclusive; zero bounds open) in canonical order.
func (s *Service) eventsInRange(ctx context.Context, orgID string, from, to time.Time) ([]*Event, error) {
	if s.oversight != nil {
		return s.oversight.ListApprovalEvents(ctx, orgID, from, to)
	}
	s.mu.Lock()
	snapshot := make([]*Event, len(s.events))
	copy(snapshot, s.events)
	s.mu.Unlock()
	out := make([]*Event, 0)
	for _, e := range snapshot {
		if e.OrganizationID != orgID {
			continue
		}
		if !from.IsZero() && e.CreatedAt.Before(from) {
			continue
		}
		if !to.IsZero() && e.CreatedAt.After(to) {
			continue
		}
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// evidenceEventLine is the per-event NDJSON line. Field order follows
// declaration order (encoding/json contract) — part of the determinism
// contract below. Detail renders with sorted map keys.
type evidenceEventLine struct {
	Type           string         `json:"type"`
	ID             string         `json:"id"`
	OrganizationID string         `json:"organization_id"`
	ApprovalID     string         `json:"approval_id"`
	Actor          string         `json:"actor,omitempty"`
	CreatedAt      string         `json:"created_at"`
	Detail         map[string]any `json:"detail,omitempty"`
}

// evidenceHeadLine is the trailing NDJSON line: the snapshot anchor.
type evidenceHeadLine struct {
	Type  string `json:"type"`
	Count int    `json:"count"`
}

// ExportEvidenceJSONL renders the org's oversight snapshot for the window
// [from, to] (inclusive; zero bounds open) as newline-delimited JSON:
//
//	{"type":"requested","id":...,"organization_id":...,"approval_id":...,
//	 "actor":...,"created_at":...,"detail":{...}}    (one line per event)
//	{"type":"evidence_head","count":N}              (final line)
//
// DETERMINISM CONTRACT (mirrors internal/audit/export.go): for the same
// events the bytes are IDENTICAL across calls and processes — lines are
// ordered by (created_at ASC, id ASC), rendered through fixed structs
// (declaration order) with RFC3339Nano UTC timestamps and sorted detail map
// keys. The export is a pure read: it NEVER runs the lazy evaluation and
// NEVER mutates approval state, so hashing the export (sha256sum) and parking
// the digest in WORM storage detects any later disagreement.
func (s *Service) ExportEvidenceJSONL(ctx context.Context, orgID string, from, to time.Time) ([]byte, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgRequired
	}
	if !from.IsZero() && !to.IsZero() && from.After(to) {
		return nil, ErrInvalidEvidenceRange
	}
	events, err := s.eventsInRange(ctx, orgID, from, to)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, 256*(len(events)+1))
	for _, e := range events {
		line, err := json.Marshal(evidenceEventLine{
			Type:           e.Type,
			ID:             e.ID,
			OrganizationID: e.OrganizationID,
			ApprovalID:     e.ApprovalID,
			Actor:          e.Actor,
			CreatedAt:      e.CreatedAt.UTC().Format(time.RFC3339Nano),
			Detail:         e.Detail,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, line...)
		out = append(out, '\n')
	}
	head, err := json.Marshal(evidenceHeadLine{Type: "evidence_head", Count: len(events)})
	if err != nil {
		return nil, err
	}
	out = append(out, head...)
	out = append(out, '\n')
	return out, nil
}

// RFC3339OrEmpty formats an optional timestamp for detail payloads (empty
// string for nil — stable, JSON-friendly).
func RFC3339OrEmpty(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}
