package orchestration

// delegation.go — issue #108: the agent-to-agent delegation protocol.
//
// A delegator agent A hands a subtask to agent B by creating a CHILD RUN
// (sub-run) for B through the RunFactory seam and executing it through the
// Executor seam (the same execution shape the worker uses: a run id + agent
// id + input driven through the runtime). The result flows back to A through
// the AgentCommBus as a MessageTypeResponse message; the full delegation
// chain is retained in the per-org in-memory ledger (source of truth for
// traceability) and mirrored into run step metadata by the RunFactory
// adapter (the runs.Run model has no parent_run_id column — see the
// cmd/api/orchestration.go adapter).
//
// Safety rails:
//   - delegation depth limit (default 3) to stop recursion bombs;
//   - optional approval gate: when the delegator's config requires approval,
//     the delegation is filed through the ApprovalRouter seam (the
//     internal/approvals service) and the child run is NEVER executed until
//     a human approves (the delegation record parks in awaiting_approval);
//   - per-delegation execution timeout (context deadline);
//   - child token usage is attributed to the parent run via the
//     RecordParentDelegationStep seam.
//
// All seams are narrow interfaces defined HERE (not in internal/runs — which
// this package does not import) and adapted at the wiring layer (cmd/api):
// in-memory and Postgres runs modes both work because the adapters wrap the
// dual-mode runs.Service.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// DefaultDelegationDepth is the maximum delegation chain length (levels of
// child runs) enforced by NewDelegationService when no explicit limit is set.
const DefaultDelegationDepth = 3

// DefaultDelegationTimeout bounds one child-run execution when the caller
// context carries no tighter deadline.
const DefaultDelegationTimeout = 60 * time.Second

// DefaultLedgerRecordsPerOrg bounds the in-memory delegation ledger per
// organization (oldest records are evicted first).
const DefaultLedgerRecordsPerOrg = 1000

// Delegation lifecycle statuses (DelegationRecord.Status).
const (
	DelegationStatusPending          = "pending"           // created, execution queued (async mode)
	DelegationStatusAwaitingApproval = "awaiting_approval" // parked at the approval gate
	DelegationStatusCompleted        = "completed"
	DelegationStatusFailed           = "failed"
)

// delegationApprovalRisk is the risk level filed on delegation approval
// requests (medium: an agent-to-agent delegation is reversible by rejecting,
// but it consumes runs and tokens).
const delegationApprovalRisk = "medium"

// ActionOrchestrationDelegate is the approval action string used when routing
// a delegation through the approval service.
const ActionOrchestrationDelegate = "orchestration.delegate"

// Typed delegation errors. Use errors.Is to classify.
var (
	// ErrInvalidRequest is returned for malformed delegation requests
	// (missing fields, self-delegation, negative depth).
	ErrInvalidRequest = errors.New("orchestration: invalid delegation request")
	// ErrDepthExceeded is returned when the delegation would create a child
	// run beyond the configured depth limit.
	ErrDepthExceeded = errors.New("orchestration: delegation depth limit exceeded")
	// ErrApprovalUnavailable is returned when a delegation requires approval
	// but no ApprovalRouter is wired (fail closed, mirroring the run
	// creation require_approval contract).
	ErrApprovalUnavailable = errors.New("orchestration: delegation requires approval but no approval service is wired")
	// ErrDelegationNotFound is returned when the ledger has no record for
	// the (org, delegation id) pair.
	ErrDelegationNotFound = errors.New("orchestration: delegation not found")
)

// Usage is the token usage of one executed sub-run (kept structurally
// identical to the runtime's usage model without importing it).
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ExecuteRequest is one child-run execution order. The child run row already
// exists (created via RunFactory) when the Executor is invoked; RunID is its
// id and the Executor owns the QUEUED -> terminal status transitions.
type ExecuteRequest struct {
	OrgID       string
	RunID       string // pre-created child run id (the runtime executes under it)
	AgentID     string // the delegated-to agent
	Input       string // the delegated task text
	ParentRunID string // delegating run ("" at the top of a chain)
	Depth       int    // child depth (1 for a top-level delegation)
}

// ExecutionResult is the outcome of one child-run execution.
type ExecutionResult struct {
	RunID  string
	Status string // terminal runs status: "COMPLETED" or "FAILED"
	Output string
	Error  string // non-empty when Status == "FAILED"
	Tokens Usage
	Steps  int
}

// Executor is the runtime execution seam (issue #108). The adapter lives in
// the wiring layer (cmd/api) and drives child runs through the shared
// runtime Runner — the same seam shape the worker uses (RunWithID with
// runs-service status transitions).
//
// Contract: on a returned error the child run's terminal state is NOT
// guaranteed (the orchestrator finalizes it FAILED via RunFactory); on a
// returned result the executor has already driven the child run to the
// terminal status carried in ExecutionResult.Status.
type Executor interface {
	ExecuteRun(ctx context.Context, req ExecuteRequest) (*ExecutionResult, error)
}

// RunFactory is the child-run persistence seam (the narrow interface over
// the runs service; implemented by an adapter around *runs.Service in
// cmd/api, so it works in both in-memory and Postgres modes).
type RunFactory interface {
	// CreateSubRun creates the child run row for the delegated agent and
	// records the delegation linkage (parent run id, depth, delegator) in
	// step metadata. Returns the child run id.
	CreateSubRun(ctx context.Context, orgID, agentID, input string, meta map[string]any) (string, error)
	// FinishSubRun drives the child run to a terminal status. Only the
	// orchestrator's error path calls it (the Executor owns terminal
	// transitions on result paths).
	FinishSubRun(ctx context.Context, orgID, runID, status, output string) error
	// RecordParentDelegationStep appends a delegation step (with child token
	// usage metadata) to the PARENT run's timeline so child usage is
	// attributed to the delegating run.
	RecordParentDelegationStep(ctx context.Context, orgID, parentRunID string, meta map[string]any) error
}

// ApprovalInput carries the fields of a delegation approval request (shaped
// after the internal/approvals RequestInput; adapted at the wiring layer).
type ApprovalInput struct {
	RunID     string // the (pre-created, parked) child run
	Resource  string
	Action    string
	Reason    string
	Risk      string
	Requester string
}

// ApprovalRouter is the human-in-the-loop seam (internal/approvals). When a
// delegation requires approval the service files a request through it and
// parks the delegation at awaiting_approval — the child run is never
// executed by the orchestrator.
type ApprovalRouter interface {
	Request(ctx context.Context, orgID string, in ApprovalInput) (approvalID string, err error)
}

// DelegationRecord is one delegation link in the traceability ledger.
type DelegationRecord struct {
	ID          string
	OrgID       string
	FromAgentID string
	ToAgentID   string
	Task        string
	ParentRunID string
	ChildRunID  string
	Depth       int // child depth (1 for a top-level delegation)
	Status      string
	ApprovalID  string // set when routed through the approval gate
	Result      string
	Error       string
	Tokens      Usage
	// ResponseDelivered reports whether the response message reached the
	// delegator's inbox channel. The ledger remains the source of truth even
	// when delivery failed (saturated inbox).
	ResponseDelivered bool
	CreatedAt         time.Time
	CompletedAt       time.Time
}

// clone returns a shallow copy (all fields are values) for safe snapshots.
func (r *DelegationRecord) clone() *DelegationRecord {
	cpy := *r
	return &cpy
}

// DelegationRequest is one delegation order.
type DelegationRequest struct {
	OrgID           string
	FromAgentID     string // the delegator ("" = supervisor-initiated: no response routing)
	ToAgentID       string // the delegated-to agent
	Task            string // the subtask text (becomes the child run input)
	ParentRunID     string // delegating run ("" at the top of a chain)
	Depth           int    // depth of the CALLING chain (0 = top-level)
	RequireApproval bool   // from the delegator agent's delegation config
	Requester       string // actor id for approval requests (may be empty)
}

// DelegationOptions configures a DelegationService. Zero durations/limits
// select the documented defaults.
type DelegationOptions struct {
	Bus        AgentCommBus
	Runs       RunFactory
	Executor   Executor
	Approvals  ApprovalRouter
	MaxDepth   int           // default DefaultDelegationDepth
	Timeout    time.Duration // per-delegation execution timeout; default DefaultDelegationTimeout
	MaxRecords int           // per-org ledger bound; default DefaultLedgerRecordsPerOrg
	Now        func() time.Time
	NewID      func() string
}

// DelegationService implements the delegation protocol on top of the bus +
// runs/executor/approval seams. Safe for concurrent use.
type DelegationService struct {
	mu     sync.Mutex
	ledger map[string][]*DelegationRecord // orgID -> records, oldest first
	opts   DelegationOptions
}

// NewDelegationService builds the protocol service, filling option defaults.
func NewDelegationService(opts DelegationOptions) *DelegationService {
	if opts.MaxDepth <= 0 {
		opts.MaxDepth = DefaultDelegationDepth
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultDelegationTimeout
	}
	if opts.MaxRecords <= 0 {
		opts.MaxRecords = DefaultLedgerRecordsPerOrg
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	if opts.NewID == nil {
		opts.NewID = func() string { return uuid.NewString() }
	}
	return &DelegationService{
		ledger: make(map[string][]*DelegationRecord),
		opts:   opts,
	}
}

// SetExecutor attaches (or replaces) the runtime execution seam. Nil-safe on
// the receiver. Used by the wiring layer when the executor is only available
// after the full service graph exists.
func (s *DelegationService) SetExecutor(ex Executor) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opts.Executor = ex
}

// validate checks a delegation request and returns the child depth.
// FromAgentID may be empty: supervisor-initiated tasks have no delegator
// agent (no response routing — the ledger stays the source of truth).
func (s *DelegationService) validate(req DelegationRequest) (int, error) {
	if strings.TrimSpace(req.OrgID) == "" || strings.TrimSpace(req.ToAgentID) == "" ||
		strings.TrimSpace(req.Task) == "" {
		return 0, fmt.Errorf("%w: org_id, to_agent_id and task are required", ErrInvalidRequest)
	}
	if req.FromAgentID == req.ToAgentID {
		return 0, fmt.Errorf("%w: self-delegation is not allowed", ErrInvalidRequest)
	}
	if req.Depth < 0 {
		return 0, fmt.Errorf("%w: depth must not be negative", ErrInvalidRequest)
	}
	childDepth := req.Depth + 1
	if childDepth > s.opts.MaxDepth {
		return 0, fmt.Errorf("%w: child depth %d exceeds limit %d", ErrDepthExceeded, childDepth, s.opts.MaxDepth)
	}
	return childDepth, nil
}

// appendLocked inserts the record into the org ledger under the ledger bound
// (oldest evicted first). Caller holds s.mu.
func (s *DelegationService) appendLocked(rec *DelegationRecord) {
	org := s.ledger[rec.OrgID]
	if len(org) >= s.opts.MaxRecords {
		// Drop enough of the oldest entries to stay within the bound.
		excess := len(org) - s.opts.MaxRecords + 1
		org = org[excess:]
	}
	s.ledger[rec.OrgID] = append(org, rec)
}

// lookupLocked finds a record by id within one org. Caller holds s.mu.
func (s *DelegationService) lookupLocked(orgID, id string) *DelegationRecord {
	for _, rec := range s.ledger[orgID] {
		if rec.ID == id {
			return rec
		}
	}
	return nil
}

// Prepare performs the synchronous pre-execution half of the protocol:
// validation, ledger insertion, delegator inbox registration, the approval
// gate (when required) and child-run creation. It returns the record in
// status pending (execution still owed — call Execute) or awaiting_approval
// (terminal for the orchestrator; a human decision resumes it).
func (s *DelegationService) Prepare(ctx context.Context, req DelegationRequest) (*DelegationRecord, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	if s.opts.Bus == nil || s.opts.Runs == nil {
		return nil, errors.New("orchestration: delegation service requires bus and runs seams")
	}
	childDepth, err := s.validate(req)
	if err != nil {
		return nil, err
	}
	rec := &DelegationRecord{
		ID:          s.opts.NewID(),
		OrgID:       req.OrgID,
		FromAgentID: req.FromAgentID,
		ToAgentID:   req.ToAgentID,
		Task:        req.Task,
		ParentRunID: req.ParentRunID,
		Depth:       childDepth,
		Status:      DelegationStatusPending,
		CreatedAt:   s.opts.Now(),
	}
	s.mu.Lock()
	s.appendLocked(rec)
	s.mu.Unlock()

	// Register the delegator's inbox BEFORE executing so the response has a
	// delivery target (idempotent when the inbox already exists).
	// Supervisor-initiated delegations (empty delegator) have no response
	// target — the ledger stays the source of truth for their outcome.
	if req.FromAgentID != "" {
		if _, err := s.opts.Bus.Receive(ctx, req.OrgID, req.FromAgentID); err != nil {
			s.fail(ctx, rec, err.Error())
			return rec, fmt.Errorf("orchestration: delegator inbox unavailable: %w", err)
		}
	}

	// Approval gate FIRST (fail closed before any child run exists).
	if req.RequireApproval {
		if s.opts.Approvals == nil {
			err := ErrApprovalUnavailable
			s.fail(ctx, rec, err.Error())
			return rec, err
		}
		approvalID, err := s.opts.Approvals.Request(ctx, req.OrgID, ApprovalInput{
			Resource:  "orchestration/delegations/" + rec.ID,
			Action:    ActionOrchestrationDelegate,
			Reason:    fmt.Sprintf("delegation %q from agent %s to agent %s", req.Task, req.FromAgentID, req.ToAgentID),
			Risk:      delegationApprovalRisk,
			Requester: req.Requester,
		})
		if err != nil {
			s.fail(ctx, rec, err.Error())
			return rec, fmt.Errorf("orchestration: approval routing failed: %w", err)
		}
		// Link the parked child run so the human decision gates a real run
		// row (the approval resource references the delegation too).
		childID, rerr := s.opts.Runs.CreateSubRun(ctx, req.OrgID, req.ToAgentID, req.Task, map[string]any{
			"role":          "child",
			"parent_run_id": req.ParentRunID,
			"delegation_id": rec.ID,
			"from_agent_id": req.FromAgentID,
			"depth":         childDepth,
			"status":        DelegationStatusAwaitingApproval,
		})
		if rerr != nil {
			s.fail(ctx, rec, rerr.Error())
			return rec, fmt.Errorf("orchestration: child run creation failed: %w", rerr)
		}
		s.mu.Lock()
		rec.ChildRunID = childID
		rec.ApprovalID = approvalID
		rec.Status = DelegationStatusAwaitingApproval
		s.mu.Unlock()
		return rec, nil
	}

	// Normal path: create the child run (linkage recorded in step metadata —
	// runs.Run has no parent column).
	childID, err := s.opts.Runs.CreateSubRun(ctx, req.OrgID, req.ToAgentID, req.Task, map[string]any{
		"role":          "child",
		"parent_run_id": req.ParentRunID,
		"delegation_id": rec.ID,
		"from_agent_id": req.FromAgentID,
		"depth":         childDepth,
	})
	if err != nil {
		s.fail(ctx, rec, err.Error())
		return rec, fmt.Errorf("orchestration: child run creation failed: %w", err)
	}
	s.mu.Lock()
	rec.ChildRunID = childID
	s.mu.Unlock()

	// Notify the delegated agent's inbox (best-effort: a pure-executor agent
	// may have no inbox registered; the sub-run executes regardless). A
	// supervisor-initiated delegation has no sender identity to notify
	// with, so the notification is skipped entirely.
	if req.FromAgentID != "" {
		notifyPayload, _ := json.Marshal(map[string]any{
			"delegation_id": rec.ID,
			"child_run_id":  childID,
			"parent_run_id": req.ParentRunID,
			"task":          req.Task,
			"depth":         childDepth,
		})
		_ = s.opts.Bus.Send(ctx, AgentMessage{
			OrgID:       req.OrgID,
			FromAgentID: req.FromAgentID,
			ToAgentID:   req.ToAgentID,
			RunID:       childID,
			MessageType: MessageTypeDelegate,
			Payload:     notifyPayload,
		})
	}
	return rec, nil
}

// Execute performs the execution half of the protocol for a record prepared
// by Prepare (status pending): run the child run through the Executor seam
// under the delegation timeout, attribute child usage to the parent run,
// finalize the ledger record and route the response message back to the
// delegator's inbox. The ledger is the source of truth: a failed response
// delivery (saturated inbox) is recorded on the record, not raised.
func (s *DelegationService) Execute(ctx context.Context, rec *DelegationRecord) (*DelegationRecord, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	if rec == nil {
		return nil, fmt.Errorf("%w: delegation record is required", ErrInvalidRequest)
	}
	if rec.Status != DelegationStatusPending {
		return nil, fmt.Errorf("%w: delegation %s is in status %s, expected pending", ErrInvalidRequest, rec.ID, rec.Status)
	}
	if s.opts.Executor == nil {
		return nil, errors.New("orchestration: delegation service requires an executor")
	}
	execCtx, cancel := context.WithTimeout(ctx, s.opts.Timeout)
	defer cancel()

	result, execErr := s.opts.Executor.ExecuteRun(execCtx, ExecuteRequest{
		OrgID:       rec.OrgID,
		RunID:       rec.ChildRunID,
		AgentID:     rec.ToAgentID,
		Input:       rec.Task,
		ParentRunID: rec.ParentRunID,
		Depth:       rec.Depth,
	})
	if execErr != nil {
		// The executor contract leaves the terminal state open on errors —
		// finalize the child run FAILED (best-effort) and record it.
		if s.opts.Runs != nil && rec.ChildRunID != "" {
			_ = s.opts.Runs.FinishSubRun(ctx, rec.OrgID, rec.ChildRunID, "FAILED", execErr.Error())
		}
		s.fail(ctx, rec, execErr.Error())
		s.recordParentStep(ctx, rec, "failed", Usage{}, execErr.Error())
		s.respond(ctx, rec, rec.Error, Usage{})
		return rec, execErr
	}

	completed := result != nil && result.Status == "COMPLETED"
	s.mu.Lock()
	if completed {
		rec.Status = DelegationStatusCompleted
		rec.Result = result.Output
		rec.Tokens = result.Tokens
	} else {
		rec.Status = DelegationStatusFailed
		if result != nil {
			rec.Error = result.Error
			rec.Result = result.Output
			rec.Tokens = result.Tokens
		} else {
			rec.Error = "executor returned no result"
		}
	}
	rec.CompletedAt = s.opts.Now()
	s.mu.Unlock()

	if completed {
		s.recordParentStep(ctx, rec, "completed", result.Tokens, "")
	} else {
		s.recordParentStep(ctx, rec, "failed", usageOf(result), errOf(result))
	}
	s.respond(ctx, rec, rec.Error, rec.Tokens)
	return rec, nil
}

func usageOf(r *ExecutionResult) Usage {
	if r == nil {
		return Usage{}
	}
	return r.Tokens
}

func errOf(r *ExecutionResult) string {
	if r == nil {
		return "executor returned no result"
	}
	return r.Error
}

// fail marks a record failed (best-effort across the whole failure surface —
// the ledger never lies about a terminal state).
func (s *DelegationService) fail(ctx context.Context, rec *DelegationRecord, cause string) {
	s.mu.Lock()
	rec.Status = DelegationStatusFailed
	rec.Error = cause
	rec.CompletedAt = s.opts.Now()
	s.mu.Unlock()
}

// recordParentStep attributes child usage to the parent run timeline
// (best-effort: a missing parent — top-level delegation without a run — or
// a failing step write never fails the delegation itself).
func (s *DelegationService) recordParentStep(ctx context.Context, rec *DelegationRecord, status string, tokens Usage, execErr string) {
	if rec.ParentRunID == "" || s.opts.Runs == nil {
		return
	}
	meta := map[string]any{
		"role":          "parent",
		"delegation_id": rec.ID,
		"child_run_id":  rec.ChildRunID,
		"to_agent_id":   rec.ToAgentID,
		"depth":         rec.Depth,
		"status":        status,
		"tokens": map[string]any{
			"prompt_tokens":     tokens.PromptTokens,
			"completion_tokens": tokens.CompletionTokens,
			"total_tokens":      tokens.TotalTokens,
		},
	}
	if execErr != "" {
		meta["error"] = execErr
	}
	_ = s.opts.Runs.RecordParentDelegationStep(ctx, rec.OrgID, rec.ParentRunID, meta)
}

// respond routes the result back to the delegator through the bus
// (MessageTypeResponse). Supervisor-initiated delegations (empty delegator)
// have no response target. Delivery failures are recorded on the record; the
// ledger remains the source of truth.
func (s *DelegationService) respond(ctx context.Context, rec *DelegationRecord, execErr string, tokens Usage) {
	if s.opts.Bus == nil || rec.FromAgentID == "" {
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"delegation_id": rec.ID,
		"child_run_id":  rec.ChildRunID,
		"parent_run_id": rec.ParentRunID,
		"status":        rec.Status,
		"output":        rec.Result,
		"error":         execErr,
		"tokens": map[string]any{
			"prompt_tokens":     tokens.PromptTokens,
			"completion_tokens": tokens.CompletionTokens,
			"total_tokens":      tokens.TotalTokens,
		},
	})
	err := s.opts.Bus.Send(ctx, AgentMessage{
		OrgID:       rec.OrgID,
		FromAgentID: rec.ToAgentID,
		ToAgentID:   rec.FromAgentID,
		RunID:       rec.ChildRunID,
		MessageType: MessageTypeResponse,
		Payload:     payload,
	})
	s.mu.Lock()
	rec.ResponseDelivered = err == nil
	s.mu.Unlock()
}

// Delegate runs the full protocol synchronously: Prepare then Execute
// (awaiting_approval records are terminal — no execution). Prefer Prepare +
// Execute for async callers (the HTTP layer does exactly that).
func (s *DelegationService) Delegate(ctx context.Context, req DelegationRequest) (*DelegationRecord, error) {
	rec, err := s.Prepare(ctx, req)
	if err != nil {
		return rec, err
	}
	if rec.Status != DelegationStatusPending {
		return rec, nil
	}
	return s.Execute(ctx, rec)
}

// ListDelegations returns the org's delegation ledger (chronological order —
// chain order for traceability). When runID is non-empty only records whose
// parent OR child run matches are returned (the delegation chain visible
// from one run). Records are snapshots; org scoping is strict.
func (s *DelegationService) ListDelegations(orgID, runID string) []*DelegationRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*DelegationRecord, 0, len(s.ledger[orgID]))
	for _, rec := range s.ledger[orgID] {
		if runID != "" && rec.ParentRunID != runID && rec.ChildRunID != runID {
			continue
		}
		out = append(out, rec.clone())
	}
	return out
}

// GetDelegation resolves one record within the org (strict tenant guard).
func (s *DelegationService) GetDelegation(orgID, id string) (*DelegationRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.lookupLocked(orgID, id)
	if rec == nil {
		return nil, ErrDelegationNotFound
	}
	return rec.clone(), nil
}
