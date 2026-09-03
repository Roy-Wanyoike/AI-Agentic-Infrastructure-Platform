package workflows

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Durable-execution knobs and machine error codes (wave-3 track 3-c).
// ---------------------------------------------------------------------------

// DefaultStaleAfter is the recovery staleness threshold: a non-terminal
// workflow run whose heartbeat is older than this is considered orphaned by
// the recovery pass. Overridable with AGENTOS_WORKFLOW_STALE_AFTER (see
// StaleAfterFromEnv) or the WithStaleAfter service option.
const DefaultStaleAfter = 5 * time.Minute

// StaleAfterEnvVar is the env knob for the staleness threshold. It is read by
// StaleAfterFromEnv (the wiring helper) — internal/config is owned by another
// track, so the workflows service carries its own option instead.
const StaleAfterEnvVar = "AGENTOS_WORKFLOW_STALE_AFTER"

// Machine error codes recorded on workflow_run / workflow_node_run rows.
const (
	// ErrorCodeNodeOrphaned is stamped on node runs that were pending/running
	// when their workflow run was claimed by the recovery pass (worker crash
	// or lost worker).
	ErrorCodeNodeOrphaned = "NODE_ORPHANED"
	// ErrorCodeWorkflowRunTimeout is stamped by the watchdog on runs (and
	// their in-flight node runs) that exceeded deadline_at.
	ErrorCodeWorkflowRunTimeout = "WORKFLOW_RUN_TIMEOUT"
	// ErrorCodeNodeFailed is the fallback workflow-run error code when the
	// run is finalized as failed because a node failed without an error code.
	ErrorCodeNodeFailed = "NODE_FAILED"
)

// RecoveryBatchLimit caps how many candidate runs a single recovery pass
// fetches (per query) so one pass cannot run away on a huge backlog.
const RecoveryBatchLimit = 100

var (
	// ErrNodeRunTerminal is returned by BeginNodeRun when the latest attempt
	// of the node already reached a terminal state: replayed tasks must skip
	// the node instead of re-executing it.
	ErrNodeRunTerminal = errors.New("workflow node run already terminal")
	// ErrNodeRunInFlight is returned by BeginNodeRun when the node run is
	// currently claimed by another worker with a fresh heartbeat.
	ErrNodeRunInFlight = errors.New("workflow node run already in flight")
	// ErrNodeRunNotFound is returned when a node-run checkpoint row does not
	// exist (or belongs to another tenant).
	ErrNodeRunNotFound = errors.New("workflow node run not found")
	// ErrInvalidNodeStatus is returned by FinishNodeRun for a non-terminal
	// target status.
	ErrInvalidNodeStatus = errors.New("node run finish status must be terminal")
)

// Option configures a durability knob on the Service.
type Option func(*Service)

// WithStaleAfter overrides the recovery staleness threshold
// (AGENTOS_WORKFLOW_STALE_AFTER, default 5m).
func WithStaleAfter(d time.Duration) Option {
	return func(s *Service) {
		if d > 0 {
			s.staleAfter = d
		}
	}
}

// WithDefaultRunDeadline stamps every newly executed workflow run with
// deadline_at = now + d so the watchdog (recovery pass) times it out when the
// budget is exhausted. Zero (default) disables the watchdog for new runs.
func WithDefaultRunDeadline(d time.Duration) Option {
	return func(s *Service) {
		s.defaultDeadline = d
	}
}

// StaleAfterFromEnv resolves the staleness threshold from
// AGENTOS_WORKFLOW_STALE_AFTER (Go duration, e.g. "90s", "10m"); invalid or
// missing values fall back to DefaultStaleAfter (5m). Wiring helper for
// main.go/cmd/worker; internal/config is owned by another track.
func StaleAfterFromEnv() time.Duration {
	v := strings.TrimSpace(os.Getenv(StaleAfterEnvVar))
	if v == "" {
		return DefaultStaleAfter
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	return DefaultStaleAfter
}

// isTerminalRunStatus reports whether a workflow-run status is terminal.
func isTerminalRunStatus(status string) bool {
	switch status {
	case RunStatusCompleted, RunStatusFailed, RunStatusCancelled, RunStatusTimeout:
		return true
	}
	return false
}

// isTerminalNodeStatus reports whether a node-run status is terminal.
func isTerminalNodeStatus(status string) bool {
	switch status {
	case RunStatusCompleted, RunStatusFailed, RunStatusCancelled, RunStatusTimeout:
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Idempotent checkpointing.
//
// Node transitions persist synchronously through the checkpoint API below.
// Every node execution attempt is one row keyed by
// (workflow_run_id, node_id, attempt):
//
//   - BeginNodeRun claims the node (pending placeholder -> running, or a new
//     attempt row when the previous attempt is terminal) and stamps
//     locked_at/heartbeat_at; it refuses terminal rows (ErrNodeRunTerminal)
//     so a replayed task never re-executes finished work.
//   - HeartbeatNodeRun extends the in-flight lease.
//   - FinishNodeRun persists the terminal transition idempotently.
//
// The store-backed path is atomic (guarded UPDATE / ON CONFLICT INSERT); the
// in-memory path implements the same state machine over the maps.
// ---------------------------------------------------------------------------

// BeginNodeRun claims one node of a workflow run for execution and returns
// its checkpoint row. Idempotency contract:
//   - latest attempt terminal                  -> ErrNodeRunTerminal (skip the task)
//   - latest attempt terminal failed with
//     NODE_ORPHANED (recovery artifact)        -> fresh attempt row (attempt+1)
//   - latest attempt running, fresh lock       -> ErrNodeRunInFlight (another worker)
//   - latest attempt pending/stale running     -> claimed in place (attempt+1)
//   - no checkpoint row yet                    -> attempt 1 row created
//
// runID links the agent run carrying this node's execution; when empty the
// previous attempt's run id is inherited. Every claim bumps attempt, so a
// node's timeline reads as one row per execution attempt, keyed by
// (workflow_run_id, node_id, attempt).
func (s *Service) BeginNodeRun(ctx context.Context, orgID, workflowRunID, nodeID, runID string) (*NodeRun, error) {
	if s == nil || strings.TrimSpace(orgID) == "" || strings.TrimSpace(workflowRunID) == "" || strings.TrimSpace(nodeID) == "" {
		return nil, ErrWorkflowRunNotFound
	}
	latest, err := s.latestNodeRun(ctx, orgID, workflowRunID, nodeID)
	if err != nil {
		return nil, err
	}
	orphaned := latest != nil && latest.Status == RunStatusFailed && latest.ErrorCode == ErrorCodeNodeOrphaned
	if latest != nil && isTerminalNodeStatus(latest.Status) && !orphaned {
		return nil, ErrNodeRunTerminal
	}
	if latest != nil && latest.Status == RunStatusWaitingApproval {
		return nil, ErrNodeRunInFlight
	}
	staleAfter := s.staleAfterValue()
	if latest != nil && latest.Status == RunStatusRunning && nodeHeartbeatFresh(latest, staleAfter) {
		return nil, ErrNodeRunInFlight
	}

	now := time.Now().UTC()
	effectiveRunID := strings.TrimSpace(runID)
	if effectiveRunID == "" && latest != nil {
		effectiveRunID = latest.RunID
	}

	if latest == nil || orphaned {
		// Fresh attempt row (first execution, or a restart after the recovery
		// pass orphaned the previous attempt). The unique
		// (workflow_run_id, node_id, attempt) arbiter makes the insert
		// idempotent: losing a race surfaces the canonical row instead.
		attempt := 1
		if orphaned {
			attempt = latest.Attempt + 1
		}
		nr := &NodeRun{
			ID:            uuid.NewString(),
			WorkflowRunID: workflowRunID,
			NodeID:        nodeID,
			RunID:         effectiveRunID,
			Status:        RunStatusRunning,
			Attempt:       attempt,
			StartedAt:     &now,
			LockedAt:      &now,
			HeartbeatAt:   &now,
			CreatedAt:     now,
		}
		created, err := s.insertNodeRun(ctx, orgID, nr)
		if err != nil {
			return nil, err
		}
		if !created {
			// Lost a race against an identical checkpoint insert; surface the
			// canonical row so the caller can proceed safely.
			if existing, lerr := s.latestNodeRun(ctx, orgID, workflowRunID, nodeID); lerr == nil && existing != nil {
				_ = s.touchRunHeartbeat(ctx, orgID, workflowRunID, now)
				return existing, nil
			}
		}
		_ = s.touchRunHeartbeat(ctx, orgID, workflowRunID, now)
		return nr, nil
	}

	// Claim the existing pending/stale row in place (attempt bumps).
	claimed, err := s.claimNodeRunRow(ctx, orgID, latest.ID, effectiveRunID, now)
	if err != nil {
		return nil, err
	}
	if !claimed {
		return nil, ErrNodeRunInFlight
	}
	nr := *latest
	nr.Status = RunStatusRunning
	nr.RunID = effectiveRunID
	nr.LockedAt = &now
	nr.HeartbeatAt = &now
	if nr.StartedAt == nil {
		nr.StartedAt = &now
	}
	if s.store != nil {
		// The store claim bumped the attempt in SQL; latest is the pre-claim
		// read, so the returned row reflects the post-claim attempt. The
		// in-memory claim already bumped the cached row the copy came from.
		nr.Attempt = latest.Attempt + 1
	}
	_ = s.touchRunHeartbeat(ctx, orgID, workflowRunID, now)
	return &nr, nil
}

// HeartbeatNodeRun refreshes the liveness stamps of one running node run and
// its parent workflow run so the recovery pass does not orphan in-flight work.
func (s *Service) HeartbeatNodeRun(ctx context.Context, orgID, nodeRunID string) error {
	if s == nil || strings.TrimSpace(orgID) == "" || strings.TrimSpace(nodeRunID) == "" {
		return ErrNodeRunNotFound
	}
	now := time.Now().UTC()
	if s.store != nil {
		if err := s.store.TouchNodeRun(ctx, orgID, nodeRunID, now); err != nil {
			return err
		}
	}
	parent := s.touchNodeRunCache(orgID, nodeRunID, now)
	if parent != "" {
		_ = s.touchRunHeartbeat(ctx, orgID, parent, now)
	}
	return nil
}

// FinishNodeRun checkpoints a terminal transition (succeeded/failed/...)
// synchronously. It is idempotent: finishing an already-terminal node run is
// a no-op and never re-executes work. When the finish converges the whole
// workflow run (every node terminal), the run is finalized immediately.
func (s *Service) FinishNodeRun(ctx context.Context, orgID, nodeRunID, status, errorCode string) error {
	if s == nil || strings.TrimSpace(orgID) == "" || strings.TrimSpace(nodeRunID) == "" {
		return ErrNodeRunNotFound
	}
	if !isTerminalNodeStatus(status) {
		return ErrInvalidNodeStatus
	}
	now := time.Now().UTC()
	marked, err := s.markNodeRunRow(ctx, orgID, nodeRunID, status, errorCode, now)
	if err != nil {
		return err
	}
	if !marked {
		return nil // already terminal: idempotent no-op
	}
	if s.store != nil {
		s.mirrorNodeRunStatus(orgID, nodeRunID, status, errorCode, now)
	}
	if parent := s.parentOfNodeRun(orgID, nodeRunID); parent != "" {
		s.fastFinalize(ctx, orgID, parent)
	}
	return nil
}

// TouchWorkflowRunHeartbeat refreshes the liveness stamp of one workflow run
// (used by recovery claims and available to operator wiring).
func (s *Service) TouchWorkflowRunHeartbeat(ctx context.Context, orgID, workflowRunID string) error {
	if s == nil || strings.TrimSpace(orgID) == "" || strings.TrimSpace(workflowRunID) == "" {
		return ErrWorkflowRunNotFound
	}
	return s.touchRunHeartbeat(ctx, orgID, workflowRunID, time.Now().UTC())
}

// SetWorkflowRunDeadline pins deadline_at on a non-terminal workflow run; the
// watchdog times the run out (status timeout) once the instant passes.
func (s *Service) SetWorkflowRunDeadline(ctx context.Context, orgID, workflowRunID string, deadline time.Time) error {
	if s == nil || strings.TrimSpace(orgID) == "" || strings.TrimSpace(workflowRunID) == "" {
		return ErrWorkflowRunNotFound
	}
	if s.store != nil {
		if err := s.store.SetWorkflowRunDeadline(ctx, orgID, workflowRunID, deadline); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	wr, ok := s.workflowRuns[workflowRunID]
	if !ok || wr.OrganizationID != orgID {
		if s.store == nil {
			return ErrWorkflowRunNotFound
		}
		return nil
	}
	d := deadline
	wr.DeadlineAt = &d
	return nil
}

// fastFinalize converges a workflow run whose every node is terminal: failed
// when any node genuinely failed, completed otherwise. Best-effort (errors
// ignored) so callers never fail a task because of bookkeeping.
func (s *Service) fastFinalize(ctx context.Context, orgID, workflowRunID string) {
	s.finalizeIfConverged(ctx, orgID, workflowRunID, time.Now().UTC())
}

// finalizeIfConverged inspects the node timeline of one run:
//
//   - a genuinely failed node (any latest attempt failed with an error code
//     other than NODE_ORPHANED) fails the whole run immediately — fail-fast,
//     mirroring the executor's and the recovery pass's semantics;
//   - otherwise, once every node's latest attempt is terminal, the run
//     completes.
//
// NODE_ORPHANED failures never converge a run: they are recovery artifacts
// whose nodes are pending re-execution on a fresh attempt.
func (s *Service) finalizeIfConverged(ctx context.Context, orgID, workflowRunID string, now time.Time) {
	_, nodes, err := s.GetWorkflowRun(ctx, orgID, workflowRunID)
	if err != nil || len(nodes) == 0 {
		return
	}
	wf, err := s.GetWorkflow(ctx, orgID, s.workflowIDOf(ctx, orgID, workflowRunID))
	if err != nil {
		return
	}
	latest := latestNodeRunByNode(nodes)

	// Pass 1: fail-fast on a genuine node failure (siblings may still be
	// pending/in-flight — the run is dead anyway).
	anyFailed := false
	failureCode := ""
	for _, n := range wf.DSL.Nodes {
		nr := latest[n.ID]
		if nr == nil || nr.Status != RunStatusFailed || nr.ErrorCode == ErrorCodeNodeOrphaned {
			continue
		}
		anyFailed = true
		if failureCode == "" {
			failureCode = nr.ErrorCode
		}
	}
	if anyFailed {
		if failureCode == "" {
			failureCode = ErrorCodeNodeFailed
		}
		_, _ = s.finalizeRun(ctx, orgID, workflowRunID, RunStatusFailed, failureCode, now)
		return
	}

	// Pass 2: converge only when every node reached a terminal state and
	// none of them is an orphan artifact awaiting a fresh attempt.
	for _, n := range wf.DSL.Nodes {
		nr := latest[n.ID]
		if nr == nil || !isTerminalNodeStatus(nr.Status) {
			return // not converged (missing checkpoint or in-flight)
		}
		if nr.Status == RunStatusFailed && nr.ErrorCode == ErrorCodeNodeOrphaned {
			return // orphan artifact: a fresh attempt is pending re-execution
		}
	}
	_, _ = s.finalizeRun(ctx, orgID, workflowRunID, RunStatusCompleted, "", now)
}

// workflowIDOf resolves the parent workflow of a run (cache first, store
// second); "" when unresolvable.
func (s *Service) workflowIDOf(ctx context.Context, orgID, workflowRunID string) string {
	s.mu.Lock()
	wr, ok := s.workflowRuns[workflowRunID]
	s.mu.Unlock()
	if ok && wr.OrganizationID == orgID {
		return wr.WorkflowID
	}
	if s.store != nil {
		run, err := s.store.GetWorkflowRun(ctx, orgID, workflowRunID)
		if err == nil {
			return run.WorkflowID
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Checkpoint primitives shared by the executor, recovery pass and API paths.
// ---------------------------------------------------------------------------

// latestNodeRun returns the highest-attempt checkpoint row of one node.
func (s *Service) latestNodeRun(ctx context.Context, orgID, workflowRunID, nodeID string) (*NodeRun, error) {
	if s.store != nil {
		return s.store.LatestNodeRun(ctx, orgID, workflowRunID, nodeID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var latest *NodeRun
	for _, nr := range s.nodeRuns[workflowRunID] {
		if nr.NodeID != nodeID || nr.OrganizationID != orgID {
			continue
		}
		if latest == nil || nr.Attempt > latest.Attempt {
			latest = nr
		}
	}
	return latest, nil
}

// insertNodeRun persists one checkpoint row idempotently (store mode: ON
// CONFLICT DO NOTHING keyed by workflow_run_id+node_id+attempt) and maintains
// the in-memory write-through cache.
func (s *Service) insertNodeRun(ctx context.Context, orgID string, nr *NodeRun) (bool, error) {
	if nr.OrganizationID == "" {
		nr.OrganizationID = orgID
	}
	if s.store != nil {
		created, err := s.store.InsertNodeRun(ctx, orgID, nr)
		if err != nil {
			return false, err
		}
		if !created {
			return false, nil
		}
	} else {
		s.mu.Lock()
		for _, existing := range s.nodeRuns[nr.WorkflowRunID] {
			if existing.NodeID == nr.NodeID && existing.Attempt == nr.Attempt && existing.OrganizationID == orgID {
				s.mu.Unlock()
				return false, nil
			}
		}
		s.nodeRuns[nr.WorkflowRunID] = append(s.nodeRuns[nr.WorkflowRunID], nr)
		s.nodeRunIndex[nr.ID] = nr
		s.mu.Unlock()
		return true, nil
	}
	// Write-through cache (store mode keeps cache reads consistent too).
	s.mu.Lock()
	s.nodeRuns[nr.WorkflowRunID] = append(s.nodeRuns[nr.WorkflowRunID], nr)
	s.nodeRunIndex[nr.ID] = nr
	s.mu.Unlock()
	return true, nil
}

// claimNodeRunRow atomically moves one non-terminal checkpoint row to running
// and bumps its attempt (one claim == one execution attempt).
func (s *Service) claimNodeRunRow(ctx context.Context, orgID, nodeRunID, runID string, at time.Time) (bool, error) {
	if s.store != nil {
		return s.store.ClaimNodeRun(ctx, orgID, nodeRunID, runID, at)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	nr, ok := s.nodeRunIndex[nodeRunID]
	if !ok || nr.OrganizationID != orgID {
		return false, nil
	}
	if isTerminalNodeStatus(nr.Status) || nr.Status == RunStatusWaitingApproval {
		return false, nil
	}
	nr.Status = RunStatusRunning
	nr.Attempt++
	nr.LockedAt = &at
	nr.HeartbeatAt = &at
	if nr.StartedAt == nil {
		nr.StartedAt = &at
	}
	if runID != "" {
		nr.RunID = runID
	}
	return true, nil
}

// markNodeRunRow persists a guarded node-run transition; marked is false when
// the row was already terminal (idempotent no-op).
func (s *Service) markNodeRunRow(ctx context.Context, orgID, nodeRunID, status, errorCode string, at time.Time) (bool, error) {
	if s.store != nil {
		return s.store.MarkNodeRunStatus(ctx, orgID, nodeRunID, status, errorCode, at)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	nr, ok := s.nodeRunIndex[nodeRunID]
	if !ok || nr.OrganizationID != orgID || isTerminalNodeStatus(nr.Status) {
		return false, nil
	}
	nr.Status = status
	nr.ErrorCode = errorCode
	nr.FinishedAt = &at
	return true, nil
}

// mirrorNodeRunStatus applies a transition to the write-through cache row
// after the store accepted it (store mode only).
func (s *Service) mirrorNodeRunStatus(orgID, nodeRunID, status, errorCode string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	nr, ok := s.nodeRunIndex[nodeRunID]
	if !ok || nr.OrganizationID != orgID || isTerminalNodeStatus(nr.Status) {
		return
	}
	nr.Status = status
	nr.ErrorCode = errorCode
	nr.FinishedAt = &at
}

// touchNodeRunCache refreshes the in-memory lease of a running node run and
// returns its parent workflow run id ("" when unknown or not running).
func (s *Service) touchNodeRunCache(orgID, nodeRunID string, at time.Time) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	nr, ok := s.nodeRunIndex[nodeRunID]
	if !ok || nr.OrganizationID != orgID || nr.Status != RunStatusRunning {
		return ""
	}
	if s.store == nil {
		nr.HeartbeatAt = &at
	}
	return nr.WorkflowRunID
}

// parentOfNodeRun resolves the workflow run id of a checkpoint row.
func (s *Service) parentOfNodeRun(orgID, nodeRunID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	nr, ok := s.nodeRunIndex[nodeRunID]
	if !ok || nr.OrganizationID != orgID {
		return ""
	}
	return nr.WorkflowRunID
}

// touchRunHeartbeat best-effort refreshes a workflow run's heartbeat.
func (s *Service) touchRunHeartbeat(ctx context.Context, orgID, workflowRunID string, at time.Time) error {
	if s.store != nil {
		return s.store.TouchWorkflowRunHeartbeat(ctx, orgID, workflowRunID, at)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	wr, ok := s.workflowRuns[workflowRunID]
	if !ok || wr.OrganizationID != orgID || isTerminalRunStatus(wr.Status) {
		return ErrWorkflowRunNotFound
	}
	wr.HeartbeatAt = &at
	wr.UpdatedAt = at
	return nil
}

// finalizeRun transitions a non-terminal run to completed/failed.
func (s *Service) finalizeRun(ctx context.Context, orgID, workflowRunID, status, errorCode string, at time.Time) (bool, error) {
	if s.store != nil {
		return s.store.FinalizeWorkflowRun(ctx, orgID, workflowRunID, status, errorCode, at)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	wr, ok := s.workflowRuns[workflowRunID]
	if !ok || wr.OrganizationID != orgID {
		return false, nil
	}
	if isTerminalRunStatus(wr.Status) {
		return false, nil
	}
	wr.Status = status
	wr.ErrorCode = errorCode
	wr.FinishedAt = &at
	wr.UpdatedAt = at
	return true, nil
}

// staleAfterValue reads the current staleness knob.
func (s *Service) staleAfterValue() time.Duration {
	if s == nil {
		return DefaultStaleAfter
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.staleAfter > 0 {
		return s.staleAfter
	}
	return DefaultStaleAfter
}

// defaultDeadlineValue reads the default wall-clock budget stamped onto newly
// executed workflow runs (0 disables the watchdog for new runs).
func (s *Service) defaultDeadlineValue() time.Duration {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.defaultDeadline
}

// nodeHeartbeatFresh reports whether a running node run holds a fresh lease.
func nodeHeartbeatFresh(nr *NodeRun, staleAfter time.Duration) bool {
	if nr == nil {
		return false
	}
	ref := nr.HeartbeatAt
	if ref == nil {
		ref = nr.LockedAt
	}
	if ref == nil {
		return false
	}
	return time.Since(*ref) < staleAfter
}

// latestNodeRunByNode indexes a node timeline by node id, keeping the
// highest-attempt row per node.
func latestNodeRunByNode(nodes []*NodeRun) map[string]*NodeRun {
	out := make(map[string]*NodeRun, len(nodes))
	for _, nr := range nodes {
		if nr == nil {
			continue
		}
		if cur, ok := out[nr.NodeID]; !ok || nr.Attempt > cur.Attempt {
			out[nr.NodeID] = nr
		}
	}
	return out
}
