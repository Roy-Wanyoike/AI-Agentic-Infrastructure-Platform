package workflows

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Workflow lifecycle statuses (lowercase values pinned by the API contract).
const (
	StatusDraft     = "draft"
	StatusPublished = "published"
	StatusArchived  = "archived"
)

// Workflow run statuses. pending|running|waiting_approval are control-plane
// states; completed|failed|cancelled are terminal. timeout is the terminal
// state assigned by the recovery watchdog when deadline_at passes
// (wave-3 track 3-c).
const (
	RunStatusPending         = "pending"
	RunStatusRunning         = "running"
	RunStatusWaitingApproval = "waiting_approval"
	RunStatusCompleted       = "completed"
	RunStatusFailed          = "failed"
	RunStatusCancelled       = "cancelled"
	RunStatusTimeout         = "timeout"
)

var (
	ErrWorkflowNotFound     = errors.New("workflow not found")
	ErrWorkflowRunNotFound  = errors.New("workflow run not found")
	ErrEngineNotWired       = errors.New("workflow execution engine is not wired")
	ErrWorkflowNameRequired = errors.New("workflow name is required")
)

// Workflow is a versioned, tenant-scoped workflow definition. Status starts
// at draft; Publish snapshots the current DSL into an immutable Version.
type Workflow struct {
	ID             string    `json:"id"`
	OrganizationID string    `json:"organization_id"`
	Name           string    `json:"name"`
	Description    string    `json:"description,omitempty"`
	Status         string    `json:"status"`
	CurrentVersion int       `json:"current_version"`
	DSL            DSL       `json:"dsl"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// Version is one immutable published snapshot of a workflow DSL.
type Version struct {
	ID             string    `json:"id"`
	WorkflowID     string    `json:"workflow_id"`
	OrganizationID string    `json:"organization_id,omitempty"`
	Version        int       `json:"version"`
	Status         string    `json:"status"`
	DSL            DSL       `json:"dsl_snapshot"`
	PublishedBy    string    `json:"published_by,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// WorkflowRun is one execution of a workflow (expansion of its DAG). The
// durability fields (attempt/locked_at/heartbeat_at/finished_at/deadline_at/
// error_code) are maintained by the durable-execution layer (durable.go,
// recovery.go) and the columns were added by migration 013.
type WorkflowRun struct {
	ID             string     `json:"id"`
	WorkflowID     string     `json:"workflow_id"`
	OrganizationID string     `json:"organization_id"`
	Input          string     `json:"input,omitempty"`
	Status         string     `json:"status"`
	CreatedBy      string     `json:"created_by,omitempty"`
	Attempt        int        `json:"attempt"`
	LockedAt       *time.Time `json:"locked_at,omitempty"`
	HeartbeatAt    *time.Time `json:"heartbeat_at,omitempty"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	DeadlineAt     *time.Time `json:"deadline_at,omitempty"`
	ErrorCode      string     `json:"error_code,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// NodeRun records one checkpointed execution attempt of a DAG node. Rows are
// keyed idempotently by (workflow_run_id, node_id, attempt): a replayed task
// never re-executes a node whose latest attempt is already terminal.
// OrganizationID is the tenant scope of the row; it is persisted in the
// organization_id column (via the store's orgID argument) but excluded from
// the JSON wire shape for backwards compatibility.
type NodeRun struct {
	ID             string     `json:"id"`
	WorkflowRunID  string     `json:"workflow_run_id"`
	NodeID         string     `json:"node_id"`
	RunID          string     `json:"run_id,omitempty"`
	Status         string     `json:"status"`
	Error          string     `json:"error,omitempty"`
	Attempt        int        `json:"attempt"`
	LockedAt       *time.Time `json:"locked_at,omitempty"`
	HeartbeatAt    *time.Time `json:"heartbeat_at,omitempty"`
	ErrorCode      string     `json:"error_code,omitempty"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	OrganizationID string     `json:"-"`
}

// Service is the dual-mode workflow service: in-memory maps by default,
// Postgres-backed when constructed with NewServiceWithStore.
type Service struct {
	mu              sync.Mutex
	workflows       map[string]*Workflow
	versions        map[string][]*Version
	workflowRuns    map[string]*WorkflowRun
	nodeRuns        map[string][]*NodeRun
	nodeRunIndex    map[string]*NodeRun
	store           Store
	engine          Engine
	staleAfter      time.Duration
	defaultDeadline time.Duration
}

func NewService() *Service {
	return &Service{
		workflows:    make(map[string]*Workflow),
		versions:     make(map[string][]*Version),
		workflowRuns: make(map[string]*WorkflowRun),
		nodeRuns:     make(map[string][]*NodeRun),
		nodeRunIndex: make(map[string]*NodeRun),
		staleAfter:   DefaultStaleAfter,
	}
}

// NewServiceWithStore returns a service whose source of truth is a durable
// store; the in-memory maps remain a write-through cache.
func NewServiceWithStore(store Store) *Service {
	s := NewService()
	s.store = store
	return s
}

// NewServiceWithOptions builds a service with durability knobs applied
// (WithoutStore: purely in-memory). See WithStaleAfter / WithDefaultRunDeadline.
func NewServiceWithOptions(store Store, opts ...Option) *Service {
	s := NewServiceWithStore(store)
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	return s
}

// SetEngine wires the execution engine (runs + queue + approvals services).
// Required before ExecuteWorkflow can expand a DAG.
func (s *Service) SetEngine(e Engine) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.engine = e
}

// CreateWorkflow validates the DSL and persists a new draft workflow for one
// tenant. Invalid DSLs are rejected with *ValidationErrors (rendered as 422).
func (s *Service) CreateWorkflow(ctx context.Context, orgID, name, description string, dsl DSL) (*Workflow, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, errors.New("organization id is required")
	}
	if strings.TrimSpace(name) == "" {
		return nil, ErrWorkflowNameRequired
	}
	if verrs := ValidateDSL(dsl); len(verrs) > 0 {
		return nil, &ValidationErrors{Errors: verrs}
	}
	now := time.Now().UTC()
	wf := &Workflow{
		ID:             uuid.NewString(),
		OrganizationID: orgID,
		Name:           name,
		Description:    description,
		Status:         StatusDraft,
		CurrentVersion: 0,
		DSL:            dsl,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if s.store != nil {
		if err := s.store.CreateWorkflow(ctx, orgID, wf); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	s.workflows[wf.ID] = wf
	s.mu.Unlock()
	return wf, nil
}

// ListWorkflows returns the tenant's workflows (organization_id guard).
func (s *Service) ListWorkflows(ctx context.Context, orgID string) ([]*Workflow, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, errors.New("organization id is required")
	}
	if s.store != nil {
		return s.store.ListWorkflows(ctx, orgID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Workflow, 0)
	for _, wf := range s.workflows {
		if wf.OrganizationID == orgID {
			out = append(out, wf)
		}
	}
	return out, nil
}

// GetWorkflow resolves one workflow strictly within one tenant; foreign
// workflows surface as ErrWorkflowNotFound (HTTP 404).
func (s *Service) GetWorkflow(ctx context.Context, orgID, id string) (*Workflow, error) {
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(id) == "" {
		return nil, ErrWorkflowNotFound
	}
	if s.store != nil {
		return s.store.GetWorkflow(ctx, orgID, id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	wf, ok := s.workflows[id]
	if !ok || wf.OrganizationID != orgID {
		return nil, ErrWorkflowNotFound
	}
	return wf, nil
}

// GetVersions returns the immutable published versions of one workflow.
func (s *Service) GetVersions(ctx context.Context, orgID, workflowID string) ([]*Version, error) {
	if _, err := s.GetWorkflow(ctx, orgID, workflowID); err != nil {
		return nil, err
	}
	if s.store != nil {
		return s.store.ListVersions(ctx, orgID, workflowID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.versions[workflowID]
	out := make([]*Version, len(list))
	copy(out, list)
	return out, nil
}

// ValidateWorkflow validates the workflow's current DSL (POST /validate).
func (s *Service) ValidateWorkflow(ctx context.Context, orgID, id string) ([]ValidationError, error) {
	wf, err := s.GetWorkflow(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	return ValidateDSL(wf.DSL), nil
}

// Publish snapshots the workflow's current DSL as the next immutable version
// and marks the workflow published. The DSL must be valid to publish.
func (s *Service) Publish(ctx context.Context, orgID, id, publishedBy string) (*Workflow, *Version, error) {
	wf, err := s.GetWorkflow(ctx, orgID, id)
	if err != nil {
		return nil, nil, err
	}
	if verrs := ValidateDSL(wf.DSL); len(verrs) > 0 {
		return nil, nil, &ValidationErrors{Errors: verrs}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	version := wf.CurrentVersion + 1
	now := time.Now().UTC()
	v := &Version{
		ID:          uuid.NewString(),
		WorkflowID:  wf.ID,
		Version:     version,
		Status:      StatusPublished,
		DSL:         wf.DSL,
		PublishedBy: publishedBy,
		CreatedAt:   now,
	}
	if s.store != nil {
		if err := s.store.CreateVersion(ctx, orgID, v); err != nil {
			return nil, nil, err
		}
		if err := s.store.UpdateWorkflowStatus(ctx, orgID, wf.ID, StatusPublished, version, now); err != nil {
			return nil, nil, err
		}
	}
	wf.Status = StatusPublished
	wf.CurrentVersion = version
	wf.UpdatedAt = now
	s.versions[wf.ID] = append(s.versions[wf.ID], v)
	return wf, v, nil
}

// GetWorkflowRun resolves one workflow run with its node runs.
func (s *Service) GetWorkflowRun(ctx context.Context, orgID, id string) (*WorkflowRun, []*NodeRun, error) {
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(id) == "" {
		return nil, nil, ErrWorkflowRunNotFound
	}
	if s.store != nil {
		wr, err := s.store.GetWorkflowRun(ctx, orgID, id)
		if err != nil {
			return nil, nil, err
		}
		nodes, err := s.store.ListNodeRuns(ctx, orgID, id)
		if err != nil {
			return nil, nil, err
		}
		return wr, nodes, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	wr, ok := s.workflowRuns[id]
	if !ok || wr.OrganizationID != orgID {
		return nil, nil, ErrWorkflowRunNotFound
	}
	nodes := make([]*NodeRun, len(s.nodeRuns[id]))
	copy(nodes, s.nodeRuns[id])
	return wr, nodes, nil
}

// UpdateWorkflowRunStatus transitions a workflow run status within one tenant
// (used by the approvals decide flow to resume/cancel the parent run).
func (s *Service) UpdateWorkflowRunStatus(ctx context.Context, orgID, id, status string) error {
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(id) == "" {
		return ErrWorkflowRunNotFound
	}
	if s.store != nil {
		if err := s.store.UpdateWorkflowRunStatus(ctx, orgID, id, status, time.Now().UTC()); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	wr, ok := s.workflowRuns[id]
	if !ok {
		if s.store == nil {
			return ErrWorkflowRunNotFound
		}
		return nil
	}
	if wr.OrganizationID != orgID {
		return ErrWorkflowRunNotFound
	}
	wr.Status = status
	wr.UpdatedAt = time.Now().UTC()
	return nil
}

// ---------------------------------------------------------------------------
// Legacy pre-DSL API (kept for backwards compatibility with the original
// in-memory demo service and its tests). New code must use CreateWorkflow /
// ExecuteWorkflow with a DSL.
// ---------------------------------------------------------------------------

// StepType is the legacy step type of the pre-DSL API.
type StepType string

const (
	StepAgent     StepType = "agent"
	StepTool      StepType = "tool"
	StepDelay     StepType = "delay"
	StepCondition StepType = "condition"
	StepEnd       StepType = "end"
)

// Step is one legacy sequential step; converted into DSL nodes by Create.
type Step struct {
	ID     string
	Type   StepType
	Name   string
	Config map[string]any
}

// Create builds a workflow from a linear legacy step list (no validation:
// legacy steps do not carry per-type config). See CreateWorkflow for the
// validated DSL path.
func (s *Service) Create(name string, steps []Step) (*Workflow, error) {
	if strings.TrimSpace(name) == "" {
		return nil, ErrWorkflowNameRequired
	}
	if len(steps) == 0 {
		return nil, errors.New("workflow requires at least one step")
	}
	dsl := legacyStepsToDSL(steps)
	now := time.Now().UTC()
	wf := &Workflow{
		ID:             fmt.Sprintf("wf-%d", len(s.workflows)+1),
		OrganizationID: "",
		Name:           name,
		Status:         StatusDraft,
		DSL:            dsl,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workflows[wf.ID] = wf
	return wf, nil
}

// Get is the legacy in-memory lookup.
func (s *Service) Get(id string) (*Workflow, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	wf, ok := s.workflows[id]
	return wf, ok
}

// Execute is the legacy trace builder; see ExecuteWorkflow for the real DAG
// expansion into agent runs.
func (s *Service) Execute(id string) (string, error) {
	return s.ExecuteWithApproval(id, "approved")
}

// ExecuteWithApproval is the legacy approval-gated trace builder.
func (s *Service) ExecuteWithApproval(id, decision string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	wf, ok := s.workflows[id]
	if !ok {
		return "", errors.New("workflow not found")
	}
	if strings.TrimSpace(decision) == "" {
		return "", errors.New("approval decision is required")
	}
	if !strings.EqualFold(decision, "approved") && !strings.EqualFold(decision, "rejected") {
		return "", errors.New("decision must be approved or rejected")
	}
	if strings.EqualFold(decision, "rejected") {
		wf.Status = "REJECTED"
		wf.UpdatedAt = time.Now().UTC()
		return "approval:rejected", nil
	}
	trace := make([]string, 0, len(wf.DSL.Nodes))
	for _, node := range wf.DSL.Nodes {
		if node.Type == NodeCondition {
			if condition, ok := node.Config["value"].(bool); ok && condition {
				trace = append(trace, fmt.Sprintf("%s:%s:pass", node.Type, node.Name))
				continue
			}
		}
		trace = append(trace, fmt.Sprintf("%s:%s", node.Type, node.Name))
	}
	wf.Status = RunStatusCompleted
	wf.UpdatedAt = time.Now().UTC()
	return strings.Join(trace, " -> "), nil
}

// legacyStepsToDSL converts the linear legacy step list into an equivalent
// DSL (sequential edges). Empty step ids get positional fallbacks.
func legacyStepsToDSL(steps []Step) DSL {
	dsl := DSL{Nodes: make([]Node, 0, len(steps)), Edges: make([]Edge, 0, len(steps))}
	ids := make([]string, 0, len(steps))
	for i, step := range steps {
		id := step.ID
		if id == "" {
			id = fmt.Sprintf("s%d", i+1)
		}
		ids = append(ids, id)
		dsl.Nodes = append(dsl.Nodes, Node{ID: id, Type: NodeType(step.Type), Name: step.Name, Config: step.Config})
		if i > 0 {
			dsl.Edges = append(dsl.Edges, Edge{From: ids[i-1], To: id, Condition: EdgeAlways})
		}
	}
	return dsl
}
