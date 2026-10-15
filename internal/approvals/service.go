package approvals

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"agentos/internal/runs"
)

// Approval statuses (pinned by the wave-2 API contract).
const (
	StatusPending   = "pending"
	StatusApproved  = "approved"
	StatusRejected  = "rejected"
	StatusCancelled = "cancelled"
)

// Risk levels accepted on Request; anything else defaults to medium.
const (
	RiskLow    = "low"
	RiskMedium = "medium"
	RiskHigh   = "high"
)

var (
	ErrApprovalNotFound = errors.New("approval not found")
	ErrInvalidDecision  = errors.New("decision must be approved or rejected")
	ErrAlreadyDecided   = errors.New("approval has already been decided")
)

// Approval is a human-in-the-loop gate record. Approving a pending approval
// resumes the linked paused run (RunID) through the RunController.
type Approval struct {
	ID             string     `json:"id"`
	OrganizationID string     `json:"organization_id"`
	RunID          string     `json:"run_id,omitempty"`
	WorkflowRunID  string     `json:"workflow_run_id,omitempty"`
	Resource       string     `json:"resource,omitempty"`
	Action         string     `json:"action,omitempty"`
	Reason         string     `json:"reason,omitempty"`
	Risk           string     `json:"risk,omitempty"`
	Status         string     `json:"status"`
	Requester      string     `json:"requester,omitempty"`
	Approver       string     `json:"approver,omitempty"`
	DecisionReason string     `json:"decision_reason,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	DecidedAt      *time.Time `json:"decided_at,omitempty"`

	// Issue #137 oversight fields (ADDITIVE; all omitempty so the wave-2
	// JSON contract is unchanged for approvals that do not use them).
	RequireDual         bool       `json:"require_dual,omitempty"`
	FirstApprover       string     `json:"first_approver,omitempty"`
	FirstDecision       string     `json:"first_decision,omitempty"`
	FirstDecidedAt      *time.Time `json:"first_decided_at,omitempty"`
	DelegatedTo         string     `json:"delegated_to,omitempty"`
	DelegatedBy         string     `json:"delegated_by,omitempty"`
	DelegationNote      string     `json:"delegation_note,omitempty"`
	DelegationExpiresAt *time.Time `json:"delegation_expires_at,omitempty"`
	EscalatedAt         *time.Time `json:"escalated_at,omitempty"`
	EscalationReason    string     `json:"escalation_reason,omitempty"`
	EscalationDeadline  *time.Time `json:"escalation_deadline,omitempty"`
}

// RequestInput carries the fields of a new approval request.
type RequestInput struct {
	RunID         string
	WorkflowRunID string
	Resource      string
	Action        string
	Reason        string
	Risk          string
	Requester     string
	// RequireDual turns on the two-person rule (issue #137): the first
	// decision parks the approval in partially_approved and completion
	// requires a DISTINCT second approver. ADDITIVE field.
	RequireDual bool
}

// RunController is the subset of the runs service the approvals service needs
// to resume a paused linked run when an approval is approved. *runs.Service
// satisfies it.
type RunController interface {
	ResumeRun(ctx context.Context, orgID, runID string) (*runs.Run, error)
}

// Service is the dual-mode approvals service: in-memory maps by default,
// Postgres-backed when constructed with NewServiceWithStore.
type Service struct {
	mu        sync.Mutex
	approvals map[string]*Approval
	store     Store
	runs      RunController

	// Issue #137 oversight state (ADDITIVE). oversight is the optional
	// durable oversight store (detected when the Store passed to
	// NewServiceWithStore also implements OversightStore); auditor is the
	// optional best-effort audit seam; the remaining fields are the
	// in-memory write-through caches for zero-infrastructure mode.
	oversight   OversightStore
	auditor     Auditor
	delegations map[string][]*Delegation
	events      []*Event
	policies    map[string]*EscalationPolicy
}

func NewService() *Service {
	return &Service{
		approvals:   make(map[string]*Approval),
		delegations: make(map[string][]*Delegation), // issue #137 oversight caches
		policies:    make(map[string]*EscalationPolicy),
	}
}

// NewServiceWithStore returns a service whose source of truth is a durable
// store; the in-memory map remains a write-through cache. When the store also
// implements OversightStore (the Postgres store does) the oversight features
// (delegations, escalation policies, events) persist through it too.
func NewServiceWithStore(store Store) *Service {
	s := NewService()
	s.store = store
	if o, ok := store.(OversightStore); ok {
		s.oversight = o
	}
	return s
}

// SetRunController wires the runs service used to resume paused linked runs.
func (s *Service) SetRunController(rc RunController) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs = rc
}

// Request records a new pending approval for one tenant (organization_id is
// taken from the authenticated caller by the handler, never from the client).
func (s *Service) Request(ctx context.Context, orgID string, in RequestInput) (*Approval, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, errors.New("organization id is required")
	}
	risk := strings.ToLower(strings.TrimSpace(in.Risk))
	switch risk {
	case RiskLow, RiskHigh:
	case "":
		risk = RiskMedium
	default:
		risk = RiskMedium
	}
	now := time.Now().UTC()
	approval := &Approval{
		ID:             uuid.NewString(),
		OrganizationID: orgID,
		RunID:          strings.TrimSpace(in.RunID),
		WorkflowRunID:  strings.TrimSpace(in.WorkflowRunID),
		Resource:       in.Resource,
		Action:         in.Action,
		Reason:         in.Reason,
		Risk:           risk,
		Status:         StatusPending,
		Requester:      in.Requester,
		CreatedAt:      now,
		RequireDual:    in.RequireDual, // issue #137: two-person rule
	}
	if s.store != nil {
		if err := s.store.CreateApproval(ctx, orgID, approval); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	s.approvals[approval.ID] = approval
	s.appendEventLocked(ctx, orgID, approval.ID, EventRequested, in.Requester, map[string]any{
		"risk":         risk,
		"resource":     in.Resource,
		"action":       in.Action,
		"require_dual": in.RequireDual,
	})
	s.mu.Unlock()
	return approval, nil
}

// List returns the tenant's approvals, optionally filtered by status. Issue
// #137: the read applies the LAZY oversight evaluation (expired delegations
// return to the queue; SLA breaches are stamped) before returning.
func (s *Service) List(ctx context.Context, orgID, status string) ([]*Approval, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, errors.New("organization id is required")
	}
	rows, err := s.listRaw(ctx, orgID, strings.TrimSpace(status))
	if err != nil {
		return nil, err
	}
	s.lazyOversight(ctx, orgID, rows)
	return rows, nil
}

// listRaw fetches the tenant's approvals without the lazy oversight pass
// (store mode hits the store; memory mode filters the map under the lock).
func (s *Service) listRaw(ctx context.Context, orgID, status string) ([]*Approval, error) {
	if s.store != nil {
		return s.store.ListApprovals(ctx, orgID, status)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Approval, 0)
	for _, a := range s.approvals {
		if a.OrganizationID != orgID {
			continue
		}
		if status != "" && a.Status != status {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

// Get resolves one approval strictly within one tenant. Issue #137: the read
// applies the LAZY oversight evaluation (expired delegation returns to the
// queue; SLA breach is stamped) before returning.
func (s *Service) Get(ctx context.Context, orgID, id string) (*Approval, error) {
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(id) == "" {
		return nil, ErrApprovalNotFound
	}
	var (
		a   *Approval
		err error
	)
	if s.store != nil {
		a, err = s.store.GetApproval(ctx, orgID, id)
	} else {
		s.mu.Lock()
		var ok bool
		a, ok = s.approvals[id]
		s.mu.Unlock()
		if !ok || a.OrganizationID != orgID {
			return nil, ErrApprovalNotFound
		}
	}
	if err != nil {
		return nil, err
	}
	s.lazyOne(ctx, orgID, a, s.currentPolicy(ctx, orgID), time.Now().UTC())
	return a, nil
}

// Decide applies an approval decision. The approver identity comes from the
// authenticated caller. When the decision is approved and a run is linked,
// the paused run is resumed through the RunController (best effort: a failed
// resume does not roll back the decision; the run stays pausable/resumable).
//
// Issue #137 (additive, transparent to existing callers): when the approval
// carries RequireDual, the FIRST decision parks it in partially_approved
// (recording first_approver) and only a DISTINCT second approver completes it
// — self-approval (the requester or the first approver deciding again) is
// blocked with ErrDualSelfApproval. Works with approve AND reject: the second
// decision sets the final outcome. The existing decide endpoint enforces this
// through this same code path.
func (s *Service) Decide(ctx context.Context, orgID, id, decision, reason, approver string) (*Approval, error) {
	decision = strings.ToLower(strings.TrimSpace(decision))
	if decision != StatusApproved && decision != StatusRejected {
		return nil, ErrInvalidDecision
	}
	approval, err := s.Get(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	approver = strings.TrimSpace(approver)
	now := time.Now().UTC()
	if approval.RequireDual {
		return s.decideDual(ctx, orgID, approval, decision, reason, approver, now)
	}
	s.mu.Lock()
	if approval.Status != StatusPending {
		s.mu.Unlock()
		return nil, ErrAlreadyDecided
	}
	approval.Status = decision
	approval.Approver = approver
	approval.DecisionReason = reason
	approval.DecidedAt = &now
	var (
		storeErr error
		runID    = approval.RunID
	)
	if s.store != nil {
		storeErr = s.store.UpdateApproval(ctx, orgID, id, decision, approval.Approver, reason, now)
	}
	s.appendEventLocked(ctx, orgID, id, EventDecided, approver, map[string]any{
		"decision": decision,
		"reason":   reason,
		"dual":     false,
	})
	runController := s.runs
	s.mu.Unlock()
	if storeErr != nil {
		return nil, storeErr
	}
	// Approved approvals resume the paused linked run (API contract).
	if decision == StatusApproved && runID != "" && runController != nil {
		_, _ = runController.ResumeRun(ctx, orgID, runID)
	}
	return approval, nil
}

// decideDual implements the two-person rule (issue #137). The first decision
// (either approve or reject) records first_approver/first_decision and parks
// the approval in partially_approved; a DISTINCT second approver completes it
// with their own decision. Maker-checker: the original requester can never be
// an approver on a dual-controlled approval, and nobody decides twice.
func (s *Service) decideDual(ctx context.Context, orgID string, approval *Approval, decision, reason, approver string, now time.Time) (*Approval, error) {
	s.mu.Lock()
	if approval.Status == StatusPartiallyApproved {
		if approver == approval.FirstApprover || approver == approval.Requester {
			s.mu.Unlock()
			return nil, errDualSelfApproval() // issue #137: two-person rule block
		}
		// Completion: the second decision sets the final outcome.
		firstApprover, firstDecision := approval.FirstApprover, approval.FirstDecision
		approval.Status = decision
		approval.Approver = approver
		approval.DecisionReason = reason
		approval.DecidedAt = &now
		var (
			storeErr error
			runID    = approval.RunID
		)
		if s.store != nil {
			// UpdateApproval leaves the oversight columns (first_*)
			// untouched: the pair stays visible on the final record.
			storeErr = s.store.UpdateApproval(ctx, orgID, approval.ID, decision, approver, reason, now)
		}
		s.appendEventLocked(ctx, orgID, approval.ID, EventDecided, approver, map[string]any{
			"decision":       decision,
			"reason":         reason,
			"dual":           true,
			"first_approver": firstApprover,
			"first_decision": firstDecision,
		})
		runController := s.runs
		s.mu.Unlock()
		if storeErr != nil {
			return nil, storeErr
		}
		s.audit(ctx, approver, AuditCompletedDual, orgID, approval.ID, map[string]any{
			"decision":       decision,
			"first_approver": firstApprover,
			"first_decision": firstDecision,
			"reason":         reason,
		})
		// Only the COMPLETED approval resumes the paused linked run.
		if decision == StatusApproved && runID != "" && runController != nil {
			_, _ = runController.ResumeRun(ctx, orgID, runID)
		}
		return approval, nil
	}
	if approval.Status != StatusPending {
		s.mu.Unlock()
		return nil, ErrAlreadyDecided
	}
	// Maker-checker: the requester can never approve their own request.
	if approver == approval.Requester {
		s.mu.Unlock()
		return nil, errDualSelfApproval() // issue #137: two-person rule block
	}
	// First decision: park in partially_approved, record the pair seed.
	approval.FirstApprover = approver
	approval.FirstDecision = decision
	approval.FirstDecidedAt = &now
	approval.Status = StatusPartiallyApproved
	var storeErr error
	if s.store != nil {
		storeErr = s.store.UpdateApproval(ctx, orgID, approval.ID, StatusPartiallyApproved, approver, reason, now)
		if storeErr == nil && s.oversight != nil {
			storeErr = s.oversight.UpdateApprovalOversight(ctx, orgID, approval.ID, approval)
		}
	}
	s.appendEventLocked(ctx, orgID, approval.ID, EventFirstDecision, approver, map[string]any{
		"decision": decision,
		"reason":   reason,
	})
	s.mu.Unlock()
	if storeErr != nil {
		return nil, storeErr
	}
	s.audit(ctx, approver, AuditFirstDecision, orgID, approval.ID, map[string]any{
		"decision": decision,
		"reason":   reason,
	})
	return approval, nil
}

// Cancel marks a pending approval as cancelled (used when the linked run or
// workflow run is cancelled out-of-band).
func (s *Service) Cancel(ctx context.Context, orgID, id string) (*Approval, error) {
	approval, err := s.Get(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if approval.Status != StatusPending {
		return nil, ErrAlreadyDecided
	}
	now := time.Now().UTC()
	approval.Status = StatusCancelled
	approval.DecidedAt = &now
	if s.store != nil {
		if err := s.store.UpdateApproval(ctx, orgID, id, StatusCancelled, "", "cancelled", now); err != nil {
			return nil, err
		}
	}
	return approval, nil
}
