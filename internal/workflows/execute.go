package workflows

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"agentos/internal/approvals"
	"agentos/internal/queue"
	"agentos/internal/runs"
)

// Engine carries the services ExecuteWorkflow needs to expand a workflow DAG
// into agent runs. All three services have in-memory modes, so the engine
// works in zero-infrastructure deployments too.
//
// StepExecutor (issue #109) optionally replaces the default run-expansion
// dispatch: when set, executable nodes execute synchronously through it and
// their JSON outputs feed the condition/switch/loop expressions.
type Engine struct {
	Runs         *runs.Service
	Queue        *queue.Queue
	Approvals    *approvals.Service
	StepExecutor StepExecutor
	// OrgQuota (issue #110) optionally caps concurrent workflow branches per
	// organization; nil means unlimited (the global
	// AGENTOS_MAX_WORKFLOW_PARALLELISM ceiling still applies).
	OrgQuota OrgParallelQuota
}

// ExecutionResult is the POST /workflows/{id}/execute response body.
type ExecutionResult struct {
	WorkflowRunID string   `json:"workflow_run_id"`
	RunIDs        []string `json:"run_ids"`
	Status        string   `json:"status"`
}

// StepRequest carries everything a StepExecutor needs to execute one node's
// business step. Input is the resolved node input ({{input}} template against
// the workflow input; inside loop bodies {{item}} is also substituted); Item /
// LoopIndex are set when the node executes inside a loop body.
type StepRequest struct {
	OrganizationID string
	WorkflowID     string
	WorkflowRunID  string
	NodeID         string
	NodeType       string
	NodeName       string
	Config         map[string]any
	Input          string
	Item           any
	LoopIndex      int
}

// StepExecutor executes the business step of one executable node (agent/tool
// work by default) and returns its JSON output document. The output becomes
// the node's recorded output, addressable from condition expressions as
// $.nodes.<id>.output.<path>. Implementations must respect ctx cancellation.
type StepExecutor interface {
	ExecuteStep(ctx context.Context, req StepRequest) (json.RawMessage, error)
}

// Clock abstracts time for the injectable-timing control-flow nodes (retry
// backoff, wait_until polling). Tests substitute a manual clock so no test
// ever sleeps for real.
type Clock interface {
	// Now returns the current clock time (UTC).
	Now() time.Time
	// Sleep waits for d or until ctx is cancelled; it returns ctx.Err() when
	// cancelled and nil otherwise.
	Sleep(ctx context.Context, d time.Duration) error
}

// realClock is the production Clock backed by the time package.
type realClock struct{}

// Now returns time.Now().UTC().
func (realClock) Now() time.Time { return time.Now().UTC() }

// Sleep blocks for d (or until ctx is done) using time.NewTimer.
func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// ExecuteWorkflow expands the workflow DAG into agent runs through the
// deterministic DAG scheduler:
//
//   - executable nodes (agent/tool) dispatch through the wired StepExecutor —
//     by default the run-expansion executor (one agent/tool run per node,
//     enqueued on the queue service exactly like the pre-#109 executor)
//   - condition/switch nodes evaluate their expression against the recorded
//     node outputs and route to the matching branch target; every node that
//     is no longer reachable is recorded with status "skipped"
//   - loop nodes iterate their body subgraph per item (capped by
//     max_iterations) with per-iteration checkpoints in the node-run metadata
//   - retry nodes re-execute their target with geometric backoff (injectable
//     clock), wait_until nodes poll their expression until truthy or timeout
//   - a parallel node fans out (sequential fan-out until issue #110 lands)
//   - an approval node creates a pending approval record, pauses the run of
//     the preceding node and flips the workflow run to waiting_approval
//
// Every created run is enqueued on the queue service so workers pick them up.
func (s *Service) ExecuteWorkflow(ctx context.Context, orgID, workflowID, input, actor string) (*ExecutionResult, error) {
	s.mu.Lock()
	engine := s.engine
	s.mu.Unlock()
	if engine.StepExecutor == nil && (engine.Runs == nil || engine.Queue == nil) {
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

	x := s.newDagExecutor(ctx, engine, wf, wr, input, actor, order, nodes)
	if err := x.run(); err != nil {
		return nil, err
	}
	return x.result, nil
}

// ---------------------------------------------------------------------------
// DAG scheduler (issue #109 control flow + dynamic skips)
// ---------------------------------------------------------------------------

// loopFrame is one active loop iteration; it feeds $.item / $.loop.index and
// the {{item}} input template while the body executes.
type loopFrame struct {
	nodeID string
	item   any
	index  int
}

// nodeExecState tracks the runtime scheduling state of one DAG node. It is
// deliberately separate from the persisted NodeRun status: a dispatched node
// run may stay pending until a worker finishes it while the scheduler already
// moved on to the node's successors.
type nodeExecState struct {
	activeIn  int  // pending incoming activations (dynamic reachable set)
	scheduled bool // enqueued on the ready queue (or already skipped)
	done      bool // executed (by the scheduler or its loop/retry owner)
	skipped   bool // permanently skipped (branch not taken / unreachable)
}

// dagExecutor is the single-run scheduler. It walks the static topological
// order with a dynamically computed reachable set: condition/switch routing
// prunes untaken branches, and the resulting skip cascades through dependent
// nodes so the recorded node timeline always reflects what actually ran.
type dagExecutor struct {
	ctx           context.Context
	svc           *Service
	engine        Engine
	exec          StepExecutor
	clock         Clock
	orgID         string
	workflowID    string
	workflowRunID string
	actor         string
	input         string

	dsl       DSL
	nodes     map[string]Node
	order     []string
	index     map[string]int
	edgesFrom map[string][]Edge

	state     map[string]*nodeExecState
	owned     map[string]string // node id -> owner node id (loop body / retry target)
	ownerDone map[string]bool
	outputs   map[string]json.RawMessage

	// refs maps a control-flow node id to the branch/case/default targets it
	// may route to (config-driven, edge-less routing); referenced is the
	// reverse set: such nodes activate only through their referrer's route
	// decision, never as scheduler roots.
	refs       map[string][]string
	referenced map[string]bool

	// mu guards the run-expansion result fields (result.RunIDs/lastRunID)
	// against concurrent branch dispatch from parallel waves (issue #110).
	mu sync.Mutex

	ready     []string // pending node ids, kept sorted by topo index
	loopStack []loopFrame

	result           *ExecutionResult
	lastRunID        string
	approvalsCreated bool
}

// newDagExecutor builds the scheduler for one workflow run.
func (s *Service) newDagExecutor(ctx context.Context, engine Engine, wf *Workflow, wr *WorkflowRun, input, actor string, order []string, nodes map[string]Node) *dagExecutor {
	clock := s.clock
	if clock == nil {
		clock = realClock{}
	}
	x := &dagExecutor{
		ctx:           ctx,
		svc:           s,
		engine:        engine,
		exec:          engine.StepExecutor,
		clock:         clock,
		orgID:         wr.OrganizationID,
		workflowID:    wf.ID,
		workflowRunID: wr.ID,
		actor:         actor,
		input:         input,
		dsl:           wf.DSL,
		nodes:         nodes,
		order:         order,
		index:         make(map[string]int, len(order)),
		edgesFrom:     make(map[string][]Edge),
		state:         make(map[string]*nodeExecState, len(order)),
		owned:         make(map[string]string),
		ownerDone:     make(map[string]bool),
		outputs:       make(map[string]json.RawMessage, len(order)),
		refs:          make(map[string][]string),
		referenced:    make(map[string]bool),
		result:        &ExecutionResult{WorkflowRunID: wr.ID, RunIDs: make([]string, 0), Status: RunStatusPending},
	}
	for i, id := range order {
		x.index[id] = i
		x.state[id] = &nodeExecState{activeIn: 0}
	}
	// Ownership: loop bodies and retry targets execute under their owner, so
	// the scheduler never schedules them and edges INTO them never activate.
	for _, node := range wf.DSL.Nodes {
		switch node.Type {
		case NodeLoop:
			for _, bodyID := range LoopConfigFromNode(node).Body {
				if _, ok := x.state[bodyID]; ok {
					x.owned[bodyID] = node.ID
				}
			}
		case NodeRetry:
			if target, _ := RetryConfigFromNode(node); target != "" {
				if _, ok := x.state[target]; ok {
					x.owned[target] = node.ID
				}
			}
		case NodeCondition:
			bt := BranchTargetsFromConfig(node.Config)
			x.addRefs(node.ID, bt.True, bt.False)
		case NodeSwitch:
			cases, def := SwitchTargetsFromConfig(node.Config)
			targets := make([]string, 0, len(cases)+1)
			for _, target := range cases {
				targets = append(targets, target)
			}
			targets = append(targets, def)
			x.addRefs(node.ID, targets...)
		}
	}
	for _, e := range wf.DSL.Edges {
		if _, ok := x.state[e.From]; !ok {
			continue
		}
		if _, ok := x.state[e.To]; !ok {
			continue
		}
		if _, isLoopBack := x.owned[e.To]; isLoopBack {
			continue // owned nodes are driven by their owner, not by edges
		}
		// Loop-back edges (issue #109) are the documented cycle
		// exemption: an edge from a loop's own body back into the loop
		// node is structural (iteration is driven by the loop node, not
		// by the edge) and must not activate the loop a second time —
		// otherwise the loop's activeIn never reaches 0 and the whole
		// subgraph deadlocks into the unreachable sweep.
		if x.nodes[e.To].Type == NodeLoop && x.owned[e.From] == e.To {
			continue
		}
		x.edgesFrom[e.From] = append(x.edgesFrom[e.From], e)
		x.state[e.To].activeIn++
	}
	return x
}

// addRefs registers config-driven routing targets (branches/cases/default)
// for one control-flow node. Empty targets are ignored; every target becomes
// "referenced", i.e. barred from root scheduling (it can only activate
// through its referrer's route decision).
func (x *dagExecutor) addRefs(condID string, targets ...string) {
	for _, t := range targets {
		if t == "" {
			continue
		}
		if _, ok := x.state[t]; !ok {
			continue
		}
		x.refs[condID] = append(x.refs[condID], t)
		x.referenced[t] = true
	}
}

// run drains the ready queue and finalizes the node timeline: anything never
// activated is recorded as skipped (dynamic unreachable set).
func (x *dagExecutor) run() error {
	// Roots: nodes without pending incoming activations. Owned nodes execute
	// under their owner; referenced nodes (branch/case/default targets) only
	// activate through their referrer's routing decision.
	for _, id := range x.order {
		if x.owned[id] != "" || x.referenced[id] {
			continue
		}
		if st := x.state[id]; st != nil && st.activeIn == 0 {
			x.schedule(id)
		}
	}
	for len(x.ready) > 0 {
		id := x.popReady()
		if err := x.executeNode(id); err != nil {
			return err
		}
	}
	// Final sweep: nodes never activated (only reachable through pruned
	// branches, or whose referrer never executed) are skipped, never
	// completed. The sweep walks topological order so each skip cascades
	// before its successors are examined.
	for _, id := range x.order {
		st := x.state[id]
		if st == nil || st.done || st.skipped || st.scheduled {
			continue
		}
		if owner := x.owned[id]; owner != "" && !x.ownerDone[owner] {
			x.skipNode(id, "owner_not_executed")
			continue
		}
		if x.owned[id] == "" {
			reason := "unreachable"
			if x.referenced[id] {
				reason = "branch_not_taken"
			}
			x.skipNode(id, reason)
		}
	}
	if x.approvalsCreated {
		_ = x.svc.updateWorkflowRunStatus(x.ctx, x.orgID, x.workflowRunID, RunStatusWaitingApproval)
	}
	return nil
}

// schedule enqueues a node whose activations are all resolved.
func (x *dagExecutor) schedule(id string) {
	st := x.state[id]
	if st == nil || st.scheduled || st.skipped || st.done {
		return
	}
	st.scheduled = true
	pos := sort.Search(len(x.ready), func(i int) bool { return x.index[x.ready[i]] > x.index[id] })
	x.ready = append(x.ready, "")
	copy(x.ready[pos+1:], x.ready[pos:])
	x.ready[pos] = id
}

// popReady returns the ready node with the smallest topo index (deterministic).
func (x *dagExecutor) popReady() string {
	id := x.ready[0]
	x.ready = x.ready[1:]
	return id
}

// consume processes one activation edge parent->child: the child loses one
// pending activation and becomes ready at zero.
func (x *dagExecutor) consume(child string) {
	if x.owned[child] != "" {
		return // owned nodes are driven by their owner, never scheduled directly
	}
	st := x.state[child]
	if st == nil || st.skipped || st.done || st.scheduled {
		return
	}
	st.activeIn--
	if st.activeIn <= 0 {
		x.schedule(child)
	}
}

// prune removes one activation edge whose branch was not taken. When the child
// loses its last pending activation it is skipped and the skip cascades.
func (x *dagExecutor) prune(child, reason string) {
	st := x.state[child]
	if st == nil || st.skipped || st.done || st.scheduled {
		return
	}
	if _, owned := x.owned[child]; owned {
		return // owners decide over their own nodes
	}
	st.activeIn--
	if st.activeIn <= 0 {
		x.skipNode(child, reason)
	}
}

// skipNode records a skipped node run and cascades the skip to its successors
// (edges) and to the config-driven routing targets it owns decisions for
// (refs) — a skipped condition/switch can never route anymore.
func (x *dagExecutor) skipNode(id, reason string) {
	st := x.state[id]
	if st == nil || st.skipped || st.done {
		return
	}
	st.skipped = true
	st.scheduled = true
	now := time.Now().UTC()
	x.svc.recordNodeRun(x.ctx, x.orgID, &NodeRun{
		ID:            uuid.NewString(),
		WorkflowRunID: x.workflowRunID,
		NodeID:        id,
		Status:        RunStatusSkipped,
		StartedAt:     &now,
		FinishedAt:    &now,
		CreatedAt:     now,
		Metadata:      map[string]any{"skip_reason": reason},
	})
	for _, e := range x.edgesFrom[id] {
		x.prune(e.To, "predecessor_skipped")
	}
	for _, ref := range x.refs[id] {
		x.prune(ref, "referrer_skipped")
	}
}

// propagate activates the outgoing edges of a completed node.
func (x *dagExecutor) propagate(id string) {
	st := x.state[id]
	if st != nil {
		st.done = true
	}
	if owner, ok := x.owned[id]; ok {
		x.ownerDone[owner] = true
	}
	for _, e := range x.edgesFrom[id] {
		x.consume(e.To)
	}
}

// activateOwnedOutgoing fires the outgoing edges of a node owned by an
// owner node (loop body / retry target) exactly once, after the owner
// finished driving it.
func (x *dagExecutor) activateOwnedOutgoing(id string) {
	st := x.state[id]
	if st == nil || st.done {
		return
	}
	st.done = true
	for _, e := range x.edgesFrom[id] {
		x.consume(e.To)
	}
}

// evalEnv builds the expression environment from the recorded outputs and the
// active loop frame.
func (x *dagExecutor) evalEnv() EvalEnv {
	env := NewEvalEnv()
	for id, raw := range x.outputs {
		env.Outputs[id] = raw
	}
	env.Input = x.input
	env.Context["organization_id"] = x.orgID
	env.Context["workflow_id"] = x.workflowID
	env.Context["workflow_run_id"] = x.workflowRunID
	if len(x.loopStack) > 0 {
		top := x.loopStack[len(x.loopStack)-1]
		env.Item = top.item
		env.LoopIndex = top.index
	}
	return env
}

// executeNode dispatches one node by DSL type. A returned error fails the
// whole workflow run (fail-fast, matching the pre-#109 executor contract).
func (x *dagExecutor) executeNode(id string) error {
	node, ok := x.nodes[id]
	if !ok {
		return fmt.Errorf("workflows: node %q missing from DSL", id)
	}
	started := time.Now().UTC()
	switch node.Type {
	case NodeAgent, NodeTool:
		return x.executeWorkNode(node, started)
	case NodeApproval:
		return x.executeApprovalNode(node, started)
	case NodeCondition:
		return x.executeConditionNode(node, started)
	case NodeSwitch:
		return x.executeSwitchNode(node, started)
	case NodeLoop:
		return x.executeLoopNode(node, started)
	case NodeRetry:
		return x.executeRetryNode(node, started)
	case NodeWaitUntil:
		return x.executeWaitUntilNode(node, started)
	case NodeParallel:
		return x.executeParallelNode(node, started)
	default:
		// Structural nodes (delay/webhook/end) record a completed
		// node run without an agent run of their own (pre-#109 contract).
		finished := time.Now().UTC()
		x.svc.recordNodeRun(x.ctx, x.orgID, &NodeRun{
			ID: uuid.NewString(), WorkflowRunID: x.workflowRunID, NodeID: node.ID,
			Status: RunStatusCompleted, StartedAt: &started, FinishedAt: &finished, CreatedAt: started,
		})
		x.propagate(node.ID)
		return nil
	}
}

// executeWorkNode dispatches an agent/tool node through the step executor
// (default: run expansion + enqueue) and records its output.
func (x *dagExecutor) executeWorkNode(node Node, started time.Time) error {
	output, err := x.dispatchStep(node)
	if err != nil {
		return x.failNode(node, started, err)
	}
	if x.exec != nil {
		// A synchronous executor completed the work here and now.
		finished := time.Now().UTC()
		x.svc.recordNodeRun(x.ctx, x.orgID, &NodeRun{
			ID: uuid.NewString(), WorkflowRunID: x.workflowRunID, NodeID: node.ID,
			Status: RunStatusCompleted, StartedAt: &started, FinishedAt: &finished, CreatedAt: started,
			Output: output,
		})
	}
	x.outputs[node.ID] = output
	x.propagate(node.ID)
	return nil
}

// dispatchStep executes one node's business step on the executor's own
// context (sequential paths: loop bodies, retry targets).
func (x *dagExecutor) dispatchStep(node Node) (json.RawMessage, error) {
	return x.dispatchStepCtx(x.ctx, node)
}

// dispatchStepCtx executes one node's business step on an explicit context —
// parallel branches (issue #110) run on their per-branch cancellation/timeout
// context so losers observe any/race/fail_fast decisions. With a custom
// StepExecutor it runs synchronously and returns the executor's JSON output;
// without one it falls back to the historical run-expansion dispatch (create
// agent/tool run + enqueue + return the run reference as the node output).
func (x *dagExecutor) dispatchStepCtx(ctx context.Context, node Node) (json.RawMessage, error) {
	runInput := x.resolveStepInput(node)
	if x.exec != nil {
		out, err := x.exec.ExecuteStep(ctx, StepRequest{
			OrganizationID: x.orgID,
			WorkflowID:     x.workflowID,
			WorkflowRunID:  x.workflowRunID,
			NodeID:         node.ID,
			NodeType:       string(node.Type),
			NodeName:       nodeDisplayName(node),
			Config:         node.Config,
			Input:          runInput,
			Item:           x.currentItem(),
			LoopIndex:      x.currentLoopIndex(),
		})
		if err != nil {
			return nil, err
		}
		return out, nil
	}
	return x.dispatchRunExpansion(ctx, node, runInput)
}

// dispatchRunExpansion is the default StepExecutor behavior: it creates the
// agent/tool run, records the pending node run, enqueues the worker task and
// reports {"run_id": ...} as the node output.
func (x *dagExecutor) dispatchRunExpansion(ctx context.Context, node Node, runInput string) (json.RawMessage, error) {
	started := time.Now().UTC()
	engine := x.engine
	if engine.Runs == nil || engine.Queue == nil {
		return nil, ErrEngineNotWired
	}
	nodeRun := &NodeRun{ID: uuid.NewString(), WorkflowRunID: x.workflowRunID, NodeID: node.ID, Status: RunStatusPending, StartedAt: &started, CreatedAt: started}
	agentID := configString(node.Config, "agent_id")
	if agentID == "" && node.Type == NodeTool {
		// runs.agent_id carries an FK to agents(id): without an explicit
		// agent on the tool node, the first agent node carries the run.
		agentID = firstAgentNodeID(x.dsl)
	}
	if agentID == "" {
		// No agent available to carry the run: enqueue a bare tool task
		// and leave the node run unlinked (run_id stays empty).
		x.svc.recordNodeRun(x.ctx, x.orgID, nodeRun)
		enqueue(engine.Queue, taskTypeFor(node), x.orgID, node.Config, x.workflowRunID, node.ID, "", runInput)
		return json.RawMessage(`{}`), nil
	}
	run, err := engine.Runs.CreateRunCtx(ctx, x.orgID, agentID, runInput)
	if err != nil {
		nodeRun.Status = RunStatusFailed
		nodeRun.Error = err.Error()
		failed := time.Now().UTC()
		nodeRun.FinishedAt = &failed
		x.svc.recordNodeRun(x.ctx, x.orgID, nodeRun)
		return nil, err
	}
	nodeRun.RunID = run.ID
	x.svc.recordNodeRun(x.ctx, x.orgID, nodeRun)
	enqueue(engine.Queue, taskTypeFor(node), x.orgID, node.Config, x.workflowRunID, node.ID, run.ID, runInput)
	x.mu.Lock()
	x.result.RunIDs = append(x.result.RunIDs, run.ID)
	x.lastRunID = run.ID
	x.mu.Unlock()
	out, _ := json.Marshal(map[string]any{"run_id": run.ID})
	return out, nil
}

// executeApprovalNode mirrors the pre-#109 approval gate: create the pending
// approval, pause the preceding run and record the gate node run.
func (x *dagExecutor) executeApprovalNode(node Node, started time.Time) error {
	nodeRun := &NodeRun{ID: uuid.NewString(), WorkflowRunID: x.workflowRunID, NodeID: node.ID, Status: RunStatusWaitingApproval, StartedAt: &started, CreatedAt: started}
	if x.engine.Approvals != nil {
		action := configString(node.Config, "action")
		if action == "" {
			action = "workflow.continue"
		}
		reason := configString(node.Config, "reason")
		if reason == "" {
			reason = "approval gate: " + nodeDisplayName(node)
		}
		if _, err := x.engine.Approvals.Request(x.ctx, x.orgID, approvals.RequestInput{
			RunID:         x.lastRunID,
			WorkflowRunID: x.workflowRunID,
			Resource:      x.workflowID,
			Action:        action,
			Reason:        reason,
			Risk:          configString(node.Config, "risk"),
			Requester:     x.actor,
		}); err != nil {
			nodeRun.Error = err.Error()
		} else {
			x.approvalsCreated = true
		}
	}
	if x.lastRunID != "" {
		if _, err := x.engine.Runs.PauseRun(x.ctx, x.orgID, x.lastRunID); err == nil {
			nodeRun.RunID = x.lastRunID
		}
	}
	x.svc.recordNodeRun(x.ctx, x.orgID, nodeRun)
	x.propagate(node.ID)
	return nil
}

// executeConditionNode evaluates the if expression, records the decision and
// routes to the configured branch target (config.branches.true/false). Every
// outgoing edge that was not taken is pruned so the skip cascades to the
// untaken branch's subgraph ("skipped" node runs). Nodes without a branches
// config keep the pre-#109 semantics: a plain structural gate that completes
// and propagates every outgoing edge.
func (x *dagExecutor) executeConditionNode(node Node, started time.Time) error {
	expr := configString(node.Config, "expression")
	value, err := EvaluateExpression(expr, x.evalEnv())
	if err != nil {
		return x.failNode(node, started, err)
	}
	raw, merr := json.Marshal(value)
	if merr != nil {
		return x.failNode(node, started, merr)
	}
	x.outputs[node.ID] = raw

	decision := Truthy(value)
	bt := BranchTargetsFromConfig(node.Config)
	_, hasBranches := node.Config["branches"]
	if !hasBranches {
		// Legacy pass-through: no branch config means the condition node
		// is a plain gate; every outgoing edge continues.
		finished := time.Now().UTC()
		x.svc.recordNodeRun(x.ctx, x.orgID, &NodeRun{
			ID: uuid.NewString(), WorkflowRunID: x.workflowRunID, NodeID: node.ID,
			Status: RunStatusCompleted, StartedAt: &started, FinishedAt: &finished, CreatedAt: started,
			Output: raw, Metadata: map[string]any{"condition": decision},
		})
		x.propagate(node.ID)
		return nil
	}
	taken := bt.False
	if decision {
		taken = bt.True
	}
	metadata := map[string]any{"condition": decision}
	if bt.True != "" || bt.False != "" {
		metadata["branch_taken"] = taken
	}
	finished := time.Now().UTC()
	x.svc.recordNodeRun(x.ctx, x.orgID, &NodeRun{
		ID: uuid.NewString(), WorkflowRunID: x.workflowRunID, NodeID: node.ID,
		Status: RunStatusCompleted, StartedAt: &started, FinishedAt: &finished, CreatedAt: started,
		Output: raw, Metadata: metadata,
	})
	x.route(node.ID, taken, decision)
	return nil
}

// executeSwitchNode evaluates the switch expression, matches the canonical
// case value and routes to the matching target (config.default when nothing
// matches). Untaken case edges are pruned.
func (x *dagExecutor) executeSwitchNode(node Node, started time.Time) error {
	expr := configString(node.Config, "expression")
	value, err := EvaluateExpression(expr, x.evalEnv())
	if err != nil {
		return x.failNode(node, started, err)
	}
	raw, merr := json.Marshal(value)
	if merr != nil {
		return x.failNode(node, started, merr)
	}
	x.outputs[node.ID] = raw

	cases, defaultTarget := SwitchTargetsFromConfig(node.Config)
	matched := ""
	taken := defaultTarget
	// Case keys are matched in sorted order so two keys that canonicalize to
	// the same value (e.g. "3" and "3.0") resolve deterministically.
	caseKeys := make([]string, 0, len(cases))
	for caseKey := range cases {
		caseKeys = append(caseKeys, caseKey)
	}
	sort.Strings(caseKeys)
	for _, caseKey := range caseKeys {
		if switchValueMatches(value, caseKey) {
			matched = caseKey
			taken = cases[caseKey]
			break
		}
	}
	metadata := map[string]any{"switch_value": SwitchMatchValue(value)}
	if matched != "" {
		metadata["matched_case"] = matched
	} else {
		metadata["matched_case"] = nil
	}
	metadata["branch_taken"] = taken
	finished := time.Now().UTC()
	x.svc.recordNodeRun(x.ctx, x.orgID, &NodeRun{
		ID: uuid.NewString(), WorkflowRunID: x.workflowRunID, NodeID: node.ID,
		Status: RunStatusCompleted, StartedAt: &started, FinishedAt: &finished, CreatedAt: started,
		Output: raw, Metadata: metadata,
	})
	x.route(node.ID, taken, Truthy(value))
	return nil
}

// route applies a condition/switch decision. Both activation channels are
// kept symmetric — drawn edges (edge to the taken target consumed, other
// outgoing edges pruned) and config-driven references (branch/case/default
// targets: taken target activated, other references pruned). An empty taken
// target prunes everything: the workflow continues through other paths only.
func (x *dagExecutor) route(condID, taken string, decision bool) {
	reason := "condition_branch_not_taken"
	if !decision {
		reason = "condition_false"
	} else {
		reason = "condition_true"
	}
	activatedTaken := false
	for _, e := range x.edgesFrom[condID] {
		if taken != "" && e.To == taken {
			x.consume(e.To)
			activatedTaken = true
			continue
		}
		x.prune(e.To, reason)
	}
	for _, ref := range x.refs[condID] {
		if ref == taken {
			x.consume(ref)
			activatedTaken = true
			continue
		}
		x.prune(ref, reason)
	}
	if taken != "" && !activatedTaken {
		// Routing target neither drawn nor referenced: activate directly so a
		// routing decision always reaches its target.
		x.consume(taken)
	}
}

// loopBodyOrder returns the body node ids in an order that respects edges
// among body nodes (Kahn with declaration-order tie-breaking).
func (x *dagExecutor) loopBodyOrder(body []string) []string {
	inBody := make(map[string]bool, len(body))
	for _, id := range body {
		if _, ok := x.state[id]; ok {
			inBody[id] = true
		}
	}
	inDeg := make(map[string]int, len(body))
	for _, id := range body {
		inDeg[id] = 0
	}
	for _, e := range x.dsl.Edges {
		if inBody[e.From] && inBody[e.To] {
			inDeg[e.To]++
		}
	}
	ordered := make([]string, 0, len(body))
	remaining := make([]string, len(body))
	copy(remaining, body)
	for len(ordered) < len(body) {
		progress := false
		for _, id := range remaining {
			if inDeg[id] < 0 {
				continue
			}
			if inDeg[id] == 0 {
				ordered = append(ordered, id)
				inDeg[id] = -1
				progress = true
				for _, e := range x.dsl.Edges {
					if e.From == id && inBody[e.To] && inDeg[e.To] > 0 {
						inDeg[e.To]--
					}
				}
			}
		}
		if !progress {
			// Body-internal cycles cannot pass ValidateDSL; fall back to the
			// declaration order rather than deadlocking.
			for _, id := range remaining {
				if inDeg[id] >= 0 {
					ordered = append(ordered, id)
					inDeg[id] = -1
				}
			}
			break
		}
	}
	return ordered
}

// loopMetadataRecordsCap bounds how many per-iteration records are stored in
// the loop node-run metadata (the full iteration count is always recorded).
const loopMetadataRecordsCap = 1000

// executeLoopNode evaluates the loop expression (an array), executes the body
// subgraph once per item (capped by max_iterations) and checkpoints every
// iteration into the loop node-run metadata.
func (x *dagExecutor) executeLoopNode(node Node, started time.Time) error {
	cfg := LoopConfigFromNode(node)
	value, err := EvaluateExpression(cfg.Expression, x.evalEnv())
	if err != nil {
		return x.failNode(node, started, err)
	}
	items, ok := value.([]any)
	if !ok {
		return x.failNode(node, started, fmt.Errorf("loop node %s: expression must evaluate to an array, got %s", node.ID, typeName(value)))
	}
	maxIter := cfg.MaxIterations
	if maxIter < 1 {
		maxIter = DefaultLoopMaxIterations
	}
	truncated := len(items) > maxIter
	if truncated {
		items = items[:maxIter]
	}
	bodyOrder := x.loopBodyOrder(cfg.Body)

	results := make([]any, 0, len(items))
	records := make([]any, 0, len(items))
	for i, item := range items {
		x.loopStack = append(x.loopStack, loopFrame{nodeID: node.ID, item: item, index: i})
		bodyOutputs := make(map[string]any, len(bodyOrder))
		status := RunStatusCompleted
		var iterErr error
		for _, bodyID := range bodyOrder {
			if iterErr != nil {
				// Fail-fast: after a failed body node the remaining
				// body nodes of this iteration do not execute.
				break
			}
			bodyNode := x.nodes[bodyID]
			switch bodyNode.Type {
			case NodeAgent, NodeTool:
				out, derr := x.dispatchStep(bodyNode)
				if derr != nil {
					status = RunStatusFailed
					iterErr = fmt.Errorf("loop node %s iteration %d: body node %s failed: %w", node.ID, i, bodyID, derr)
					continue
				}
				x.outputs[bodyID] = out
				bodyOutputs[bodyID] = json.RawMessage(out)
			default:
				// Passive structural body nodes complete per item.
				bodyOutputs[bodyID] = nil
			}
		}
		frame := x.loopStack[len(x.loopStack)-1]
		x.loopStack = x.loopStack[:len(x.loopStack)-1]

		record := map[string]any{
			"index":   frame.index,
			"item":    frame.item,
			"status":  status,
			"outputs": bodyOutputs,
		}
		if iterErr != nil {
			record["error"] = iterErr.Error()
		}
		if len(records) < loopMetadataRecordsCap {
			records = append(records, record)
		}
		results = append(results, record["outputs"])
		if iterErr != nil {
			return x.failNode(node, started, iterErr)
		}
	}

	output := map[string]any{"iterations": len(items), "truncated": truncated, "results": results}
	raw, merr := json.Marshal(output)
	if merr != nil {
		return x.failNode(node, started, merr)
	}
	x.outputs[node.ID] = raw
	finished := time.Now().UTC()
	x.svc.recordNodeRun(x.ctx, x.orgID, &NodeRun{
		ID: uuid.NewString(), WorkflowRunID: x.workflowRunID, NodeID: node.ID,
		Status: RunStatusCompleted, StartedAt: &started, FinishedAt: &finished, CreatedAt: started,
		Output: raw,
		Metadata: map[string]any{
			"loop": map[string]any{
				"iterations":     len(items),
				"max_iterations": maxIter,
				"truncated":      truncated,
				"body":           cfg.Body,
				"records":        records,
				"records_full":   len(items) <= loopMetadataRecordsCap,
			},
		},
	})
	// Body nodes are done (per-item); fire their outgoing edges once so
	// post-loop processing continues, then propagate the loop node itself.
	for _, bodyID := range bodyOrder {
		x.activateOwnedOutgoing(bodyID)
	}
	x.propagate(node.ID)
	return nil
}

// executeRetryNode re-executes its target node (config.target) on failure up
// to config.attempts with geometric backoff (injectable clock). Per-attempt
// failures are checkpointed as failed node-run rows.
func (x *dagExecutor) executeRetryNode(node Node, started time.Time) error {
	targetID, policy := RetryConfigFromNode(node)
	target, ok := x.nodes[targetID]
	if !ok {
		return x.failNode(node, started, fmt.Errorf("retry node %s: unknown target %q", node.ID, targetID))
	}
	attempts := policy.Attempts
	if attempts < 1 {
		attempts = DefaultRetryAttempts
	}
	var lastOut json.RawMessage
	var attemptErrors []string
	used := 0
	for attempt := 1; attempt <= attempts; attempt++ {
		used = attempt
		attemptStart := time.Now().UTC()
		out, derr := x.dispatchStep(target)
		if derr == nil {
			lastOut = out
			break
		}
		attemptErrors = append(attemptErrors, derr.Error())
		failed := time.Now().UTC()
		// Checkpoint the failed attempt (attempt index is 0-based like every
		// other node run row).
		x.svc.recordNodeRun(x.ctx, x.orgID, &NodeRun{
			ID: uuid.NewString(), WorkflowRunID: x.workflowRunID, NodeID: target.ID,
			Status: RunStatusFailed, Error: derr.Error(), Attempt: attempt - 1,
			StartedAt: &attemptStart, FinishedAt: &failed, CreatedAt: attemptStart,
			Metadata: map[string]any{"retry_node": node.ID, "retry_attempt": attempt},
		})
		if attempt < attempts {
			delay := policy.DelayFor(attempt)
			if serr := x.clock.Sleep(x.ctx, delay); serr != nil {
				return x.failNode(node, started, fmt.Errorf("retry node %s cancelled during backoff: %w", node.ID, serr))
			}
		}
	}
	if len(lastOut) == 0 {
		return x.failNode(node, started, fmt.Errorf("retry node %s: target %s failed after %d attempt(s): %s", node.ID, targetID, used, strings.Join(attemptErrors, "; ")))
	}

	now := time.Now().UTC()
	if x.exec != nil {
		// Synchronous executor: the target finished here and now, so this
		// terminal checkpoint carries the retry summary. With the default
		// run-expansion dispatch the target's pending row stands until the
		// worker finalizes the queued run.
		x.svc.recordNodeRun(x.ctx, x.orgID, &NodeRun{
			ID: uuid.NewString(), WorkflowRunID: x.workflowRunID, NodeID: target.ID,
			Status: RunStatusCompleted, Attempt: used - 1,
			StartedAt: &now, FinishedAt: &now, CreatedAt: now,
			Output:   lastOut,
			Metadata: map[string]any{"retry_node": node.ID, "retry_attempts": used, "retry_errors": attemptErrors},
		})
	}
	x.outputs[targetID] = lastOut
	finished := time.Now().UTC()
	x.svc.recordNodeRun(x.ctx, x.orgID, &NodeRun{
		ID: uuid.NewString(), WorkflowRunID: x.workflowRunID, NodeID: node.ID,
		Status: RunStatusCompleted, StartedAt: &started, FinishedAt: &finished, CreatedAt: started,
		Output: lastOut,
		Metadata: map[string]any{"target": targetID, "attempts": used, "backoff_policy": map[string]any{
			"base_delay_ms": policy.BaseDelay.Milliseconds(), "multiplier": policy.Multiplier, "max_delay_ms": policy.MaxDelay.Milliseconds(),
		}},
	})
	x.activateOwnedOutgoing(targetID)
	x.propagate(node.ID)
	return nil
}

// executeWaitUntilNode polls its expression until truthy or until the timeout
// expires (injectable clock; the poll counter is exposed to the expression as
// $.context.polls so predicates can simulate readiness).
func (x *dagExecutor) executeWaitUntilNode(node Node, started time.Time) error {
	cfg := WaitUntilConfigFromNode(node)
	deadline := x.clock.Now().Add(cfg.Timeout)
	polls := 0
	for {
		polls++
		env := x.evalEnv()
		env.Context["polls"] = float64(polls)
		value, err := EvaluateExpression(cfg.Expression, env)
		if err != nil {
			return x.failNode(node, started, err)
		}
		if Truthy(value) {
			raw, merr := json.Marshal(value)
			if merr != nil {
				return x.failNode(node, started, merr)
			}
			x.outputs[node.ID] = raw
			finished := time.Now().UTC()
			x.svc.recordNodeRun(x.ctx, x.orgID, &NodeRun{
				ID: uuid.NewString(), WorkflowRunID: x.workflowRunID, NodeID: node.ID,
				Status: RunStatusCompleted, StartedAt: &started, FinishedAt: &finished, CreatedAt: started,
				Output:   raw,
				Metadata: map[string]any{"polls": polls, "wait_ms": x.clock.Now().Sub(started).Milliseconds(), "timeout_seconds": cfg.Timeout.Seconds(), "poll_interval_ms": cfg.PollInterval.Milliseconds()},
			})
			x.propagate(node.ID)
			return nil
		}
		if !x.clock.Now().Before(deadline) {
			finished := time.Now().UTC()
			x.svc.recordNodeRun(x.ctx, x.orgID, &NodeRun{
				ID: uuid.NewString(), WorkflowRunID: x.workflowRunID, NodeID: node.ID,
				Status: RunStatusFailed, ErrorCode: ErrorCodeWaitUntilTimeout,
				StartedAt: &started, FinishedAt: &finished, CreatedAt: started,
				Metadata: map[string]any{"polls": polls, "timeout_seconds": cfg.Timeout.Seconds(), "poll_interval_ms": cfg.PollInterval.Milliseconds()},
			})
			err := fmt.Errorf("wait_until node %s: condition not met within %s (%d polls)", node.ID, cfg.Timeout, polls)
			_ = x.svc.updateWorkflowRunStatus(x.ctx, x.orgID, x.workflowRunID, RunStatusFailed)
			return err
		}
		if serr := x.clock.Sleep(x.ctx, cfg.PollInterval); serr != nil {
			return x.failNode(node, started, fmt.Errorf("wait_until node %s cancelled while polling: %w", node.ID, serr))
		}
	}
}

// currentItem returns the active loop item (nil outside loop bodies).
func (x *dagExecutor) currentItem() any {
	if len(x.loopStack) == 0 {
		return nil
	}
	return x.loopStack[len(x.loopStack)-1].item
}

// currentLoopIndex returns the active loop iteration index (-1 outside loops).
func (x *dagExecutor) currentLoopIndex() int {
	if len(x.loopStack) == 0 {
		return -1
	}
	return x.loopStack[len(x.loopStack)-1].index
}

// resolveStepInput resolves the node input template against the workflow
// input and, inside loop bodies, the {{item}} placeholder.
func (x *dagExecutor) resolveStepInput(node Node) string {
	runInput := resolveTemplate(configString(node.Config, "input"), x.input)
	if len(x.loopStack) == 0 || !strings.Contains(runInput, "{{item}}") {
		return runInput
	}
	item := x.currentItem()
	itemText := ""
	if s, ok := item.(string); ok {
		itemText = s
	} else if item != nil {
		if b, err := json.Marshal(item); err == nil {
			itemText = string(b)
		}
	}
	return strings.ReplaceAll(runInput, "{{item}}", itemText)
}

// failNode records a failed node run, fails the workflow run and returns the
// error so the executor aborts (fail-fast contract).
func (x *dagExecutor) failNode(node Node, started time.Time, cause error) error {
	finished := time.Now().UTC()
	code := ""
	var ee *ExprError
	if asExprError(cause, &ee) {
		code = "EXPRESSION_ERROR"
	}
	x.svc.recordNodeRun(x.ctx, x.orgID, &NodeRun{
		ID: uuid.NewString(), WorkflowRunID: x.workflowRunID, NodeID: node.ID,
		Status: RunStatusFailed, Error: cause.Error(), ErrorCode: code,
		StartedAt: &started, FinishedAt: &finished, CreatedAt: started,
	})
	_ = x.svc.updateWorkflowRunStatus(x.ctx, x.orgID, x.workflowRunID, RunStatusFailed)
	return cause
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
