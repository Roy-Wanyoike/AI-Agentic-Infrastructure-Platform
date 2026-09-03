package workflows

import (
	"context"
	"time"

	"github.com/google/uuid"

	"agentos/internal/approvals"
	"agentos/internal/queue"
	"agentos/internal/runs"
)

// Engine carries the services ExecuteWorkflow needs to expand a workflow DAG
// into agent runs. All three services have in-memory modes, so the engine
// works in zero-infrastructure deployments too.
type Engine struct {
	Runs      *runs.Service
	Queue     *queue.Queue
	Approvals *approvals.Service
}

// ExecutionResult is the POST /workflows/{id}/execute response body.
type ExecutionResult struct {
	WorkflowRunID string   `json:"workflow_run_id"`
	RunIDs        []string `json:"run_ids"`
	Status        string   `json:"status"`
}

// ExecuteWorkflow expands the workflow DAG into agent runs:
//   - one run per agent/tool node, created in topological order (edges are
//     honored; the current implementation is a static expansion per the
//     contract's "sequential for now" note)
//   - a parallel node fans out: all of its downstream executable nodes are
//     created like any other node
//   - an approval node creates a pending approval record, pauses the run of
//     the preceding node and flips the workflow run to waiting_approval
//
// Every created run is enqueued on the queue service so workers pick them up.
func (s *Service) ExecuteWorkflow(ctx context.Context, orgID, workflowID, input, actor string) (*ExecutionResult, error) {
	s.mu.Lock()
	engine := s.engine
	s.mu.Unlock()
	if engine.Runs == nil || engine.Queue == nil {
		return nil, ErrEngineNotWired
	}
	wf, err := s.GetWorkflow(ctx, orgID, workflowID)
	if err != nil {
		return nil, err
	}
	if verrs := ValidateDSL(wf.DSL); len(verrs) > 0 {
		return nil, &ValidationErrors{Errors: verrs}
	}
	order, err := TopoOrder(wf.DSL)
	if err != nil {
		return nil, &ValidationErrors{Errors: []ValidationError{{Code: "cycle_detected", Message: err.Error()}}}
	}
	nodes := nodeMap(wf.DSL)

	now := time.Now().UTC()
	wr := &WorkflowRun{
		ID:             uuid.NewString(),
		WorkflowID:     wf.ID,
		OrganizationID: orgID,
		Input:          input,
		Status:         RunStatusPending,
		CreatedBy:      actor,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	// Durability bookkeeping: the run is born with a fresh heartbeat (the
	// recovery pass measures staleness against it) and, when the
	// WithDefaultRunDeadline option is set, a wall-clock budget the
	// watchdog enforces (status timeout + WORKFLOW_RUN_TIMEOUT).
	heartbeat := now
	wr.HeartbeatAt = &heartbeat
	if deadline := s.defaultDeadlineValue(); deadline > 0 {
		d := now.Add(deadline)
		wr.DeadlineAt = &d
	}
	if err := s.createWorkflowRun(ctx, orgID, wr); err != nil {
		return nil, err
	}

	result := &ExecutionResult{WorkflowRunID: wr.ID, RunIDs: make([]string, 0), Status: RunStatusPending}
	fallbackAgentID := firstAgentNodeID(wf.DSL)
	lastRunID := ""
	approvalsCreated := false

	for _, nodeID := range order {
		node := nodes[nodeID]
		started := time.Now().UTC()
		switch node.Type {
		case NodeAgent, NodeTool:
			nodeRun := &NodeRun{ID: uuid.NewString(), WorkflowRunID: wr.ID, NodeID: node.ID, Status: RunStatusPending, StartedAt: &started, CreatedAt: started}
			agentID := configString(node.Config, "agent_id")
			if agentID == "" && node.Type == NodeTool {
				// runs.agent_id carries an FK to agents(id): without an explicit
				// agent on the tool node, the first agent node carries the run.
				agentID = fallbackAgentID
			}
			runInput := resolveTemplate(configString(node.Config, "input"), input)
			if agentID == "" {
				// No agent available to carry the run: enqueue a bare tool task
				// and leave the node run unlinked (run_id stays empty).
				s.recordNodeRun(ctx, orgID, nodeRun)
				enqueue(engine.Queue, taskTypeFor(node), orgID, node.Config, wr.ID, node.ID, "", runInput)
				continue
			}
			run, err := engine.Runs.CreateRunCtx(ctx, orgID, agentID, runInput)
			if err != nil {
				nodeRun.Status = RunStatusFailed
				nodeRun.Error = err.Error()
				failed := time.Now().UTC()
				nodeRun.FinishedAt = &failed
				s.recordNodeRun(ctx, orgID, nodeRun)
				_ = s.updateWorkflowRunStatus(ctx, orgID, wr.ID, RunStatusFailed)
				return nil, err
			}
			nodeRun.RunID = run.ID
			s.recordNodeRun(ctx, orgID, nodeRun)
			enqueue(engine.Queue, taskTypeFor(node), orgID, node.Config, wr.ID, node.ID, run.ID, runInput)
			result.RunIDs = append(result.RunIDs, run.ID)
			lastRunID = run.ID
		case NodeApproval:
			nodeRun := &NodeRun{ID: uuid.NewString(), WorkflowRunID: wr.ID, NodeID: node.ID, Status: RunStatusWaitingApproval, StartedAt: &started, CreatedAt: started}
			if engine.Approvals != nil {
				action := configString(node.Config, "action")
				if action == "" {
					action = "workflow.continue"
				}
				reason := configString(node.Config, "reason")
				if reason == "" {
					reason = "approval gate: " + nodeDisplayName(node)
				}
				if _, err := engine.Approvals.Request(ctx, orgID, approvals.RequestInput{
					RunID:         lastRunID,
					WorkflowRunID: wr.ID,
					Resource:      wf.ID,
					Action:        action,
					Reason:        reason,
					Risk:          configString(node.Config, "risk"),
					Requester:     actor,
				}); err != nil {
					nodeRun.Error = err.Error()
				} else {
					approvalsCreated = true
				}
			}
			if lastRunID != "" {
				if _, err := engine.Runs.PauseRun(ctx, orgID, lastRunID); err == nil {
					nodeRun.RunID = lastRunID
				}
			}
			s.recordNodeRun(ctx, orgID, nodeRun)
		default:
			// Structural nodes (condition/parallel/delay/webhook/end) record a
			// completed node run without an agent run of their own.
			finished := time.Now().UTC()
			nodeRun := &NodeRun{ID: uuid.NewString(), WorkflowRunID: wr.ID, NodeID: node.ID, Status: RunStatusCompleted, StartedAt: &started, FinishedAt: &finished, CreatedAt: started}
			s.recordNodeRun(ctx, orgID, nodeRun)
		}
	}

	if approvalsCreated {
		_ = s.updateWorkflowRunStatus(ctx, orgID, wr.ID, RunStatusWaitingApproval)
	}
	return result, nil
}

func taskTypeFor(node Node) string {
	if node.Type == NodeTool {
		return "tool.run"
	}
	return "agent.run"
}

func nodeDisplayName(node Node) string {
	if node.Name != "" {
		return node.Name
	}
	return node.ID
}

// enqueue pushes one worker task carrying the tenant, node and run context.
func enqueue(q *queue.Queue, taskType, orgID string, config map[string]any, workflowRunID, nodeID, runID, input string) *queue.Task {
	payload := map[string]any{
		"organization_id": orgID,
		"workflow_run_id": workflowRunID,
		"node_id":         nodeID,
	}
	if runID != "" {
		payload["run_id"] = runID
	}
	if input != "" {
		payload["input"] = input
	}
	for _, key := range []string{"agent_id", "tool_id", "url"} {
		if v := configString(config, key); v != "" {
			payload[key] = v
		}
	}
	return q.Enqueue(taskType, payload)
}

// createWorkflowRun persists the workflow run through the store (when wired)
// and the in-memory cache.
func (s *Service) createWorkflowRun(ctx context.Context, orgID string, wr *WorkflowRun) error {
	if s.store != nil {
		if err := s.store.CreateWorkflowRun(ctx, orgID, wr); err != nil {
			return err
		}
	}
	s.mu.Lock()
	s.workflowRuns[wr.ID] = wr
	s.mu.Unlock()
	return nil
}

// recordNodeRun persists one node run through the store (when wired) and the
// in-memory cache. The tenant scope is stamped here so cache lookups
// (latestNodeRun, parentOfNodeRun, ...) see the same org guard as the store.
func (s *Service) recordNodeRun(ctx context.Context, orgID string, nr *NodeRun) {
	if nr.OrganizationID == "" {
		nr.OrganizationID = orgID
	}
	if s.store != nil {
		if err := s.store.CreateNodeRun(ctx, orgID, nr); err != nil {
			return
		}
	}
	s.mu.Lock()
	s.nodeRuns[nr.WorkflowRunID] = append(s.nodeRuns[nr.WorkflowRunID], nr)
	s.nodeRunIndex[nr.ID] = nr
	s.mu.Unlock()
}

// updateWorkflowRunStatus is the internal unexported variant used by the
// executor; the exported UpdateWorkflowRunStatus wraps the same logic.
func (s *Service) updateWorkflowRunStatus(ctx context.Context, orgID, id, status string) error {
	return s.UpdateWorkflowRunStatus(ctx, orgID, id, status)
}
