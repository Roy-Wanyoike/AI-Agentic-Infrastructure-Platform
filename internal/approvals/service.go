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
}

func NewService() *Service {
	return &Service{approvals: make(map[string]*Approval)}
}

// NewServiceWithStore returns a service whose source of truth is a durable
// store; the in-memory map remains a write-through cache.
func NewServiceWithStore(store Store) *Service {
	s := NewService()
	s.store = store
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
	}
	if s.store != nil {
		if err := s.store.CreateApproval(ctx, orgID, approval); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	s.approvals[approval.ID] = approval
	s.mu.Unlock()
	return approval, nil
}

// List returns the tenant's approvals, optionally filtered by status.
func (s *Service) List(ctx context.Context, orgID, status string) ([]*Approval, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, errors.New("organization id is required")
	}
	if s.store != nil {
		return s.store.ListApprovals(ctx, orgID, strings.TrimSpace(status))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Approval, 0)
	status = strings.TrimSpace(status)
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

// Get resolves one approval strictly within one tenant.
func (s *Service) Get(ctx context.Context, orgID, id string) (*Approval, error) {
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(id) == "" {
		return nil, ErrApprovalNotFound
	}
	if s.store != nil {
		return s.store.GetApproval(ctx, orgID, id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.approvals[id]
	if !ok || a.OrganizationID != orgID {
		return nil, ErrApprovalNotFound
	}
	return a, nil
}

// Decide applies an approval decision. The approver identity comes from the
// authenticated caller. When the decision is approved and a run is linked,
// the paused run is resumed through the RunController (best effort: a failed
// resume does not roll back the decision; the run stays pausable/resumable).
func (s *Service) Decide(ctx context.Context, orgID, id, decision, reason, approver string) (*Approval, error) {
	decision = strings.ToLower(strings.TrimSpace(decision))
	if decision != StatusApproved && decision != StatusRejected {
		return nil, ErrInvalidDecision
	}
	approval, err := s.Get(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if approval.Status != StatusPending {
		s.mu.Unlock()
		return nil, ErrAlreadyDecided
	}
	now := time.Now().UTC()
	approval.Status = decision
	approval.Approver = strings.TrimSpace(approver)
	approval.DecisionReason = reason
	approval.DecidedAt = &now
	var (
		storeErr error
		runID    = approval.RunID
	)
	if s.store != nil {
		storeErr = s.store.UpdateApproval(ctx, orgID, id, decision, approval.Approver, reason, now)
	}
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
