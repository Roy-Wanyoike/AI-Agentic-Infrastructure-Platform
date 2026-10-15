package orchestration

// supervisor.go — issue #108: the supervisor pattern.
//
// A supervisor receives a task, selects a worker agent, delegates through
// the delegation protocol (Delegator seam — *DelegationService satisfies it)
// and aggregates the result. Failure handling: on a failed attempt the next
// candidate worker is tried (configurable retry budget); when every attempt
// is exhausted a typed SupervisorError (errors.Is-compatible with
// ErrAllWorkersFailed) carries the per-attempt trace.
//
// Routing strategies:
//   - round_robin:       rotate the worker list across RunTask calls;
//   - least_loaded:      pick the worker with the fewest in-flight
//                        delegations (ties keep configuration order);
//   - capability_match:  restrict candidates to workers declaring the task's
//                        required capability (no requirement -> all workers
//                        in configuration order).
//
// Planner wiring: the existing planner (orchestration.go) produces Plan /
// Task values; SupervisorTasksFromPlan adapts them into supervisor tasks and
// RunPlan executes a plan's tasks sequentially through the supervisor.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// RoutingStrategy selects how the supervisor orders candidate workers.
type RoutingStrategy string

const (
	// RouteRoundRobin rotates through the worker list across tasks.
	RouteRoundRobin RoutingStrategy = "round_robin"
	// RouteLeastLoaded picks the worker with the fewest in-flight delegations.
	RouteLeastLoaded RoutingStrategy = "least_loaded"
	// RouteCapabilityMatch restricts candidates to workers declaring the
	// required capability.
	RouteCapabilityMatch RoutingStrategy = "capability_match"
)

// DefaultSupervisorConcurrency is the in-flight delegation cap used when
// SupervisorConfig.MaxConcurrent is not set (<= 0 means "use this default"
// at the facade level; a Supervisor built directly with 0 is unlimited).
const DefaultSupervisorConcurrency = 4

// DefaultSupervisorRetries is the retry budget used by the facade when
// SupervisorConfig.MaxRetries is not set.
const DefaultSupervisorRetries = 2

// WorkerAgent is one candidate worker in the supervisor's routing table.
type WorkerAgent struct {
	AgentID      string
	Capabilities []string
}

// SupervisorConfig configures a Supervisor. The worker list is deployment
// configuration for this process (the delegations themselves stay
// tenant-scoped through DelegationRequest.OrgID); a per-org directory can
// replace it later behind the same RunTask surface.
type SupervisorConfig struct {
	Workers       []WorkerAgent
	Strategy      RoutingStrategy // default round_robin when empty
	MaxConcurrent int             // <= 0: unlimited
	MaxRetries    int             // retries AFTER the initial attempt
}

// Typed supervisor errors. Use errors.Is to classify.
var (
	// ErrNoWorkers is returned when the supervisor has an empty worker list.
	ErrNoWorkers = errors.New("orchestration: supervisor has no workers")
	// ErrNoCapableWorker is returned by the capability_match strategy when
	// no worker declares the task's required capability.
	ErrNoCapableWorker = errors.New("orchestration: no worker matches the required capability")
	// ErrAllWorkersFailed is the typed cause of every SupervisorError.
	ErrAllWorkersFailed = errors.New("orchestration: all supervisor workers failed")
	// ErrInvalidStrategy is returned by NewSupervisor for unknown strategies.
	ErrInvalidStrategy = errors.New("orchestration: unknown routing strategy")
)

// SupervisorTask is one task handed to the supervisor.
type SupervisorTask struct {
	OrgID              string
	Task               string // the task text (becomes the delegated input)
	FromAgentID        string // response routing target ("" = no comm routing)
	ParentRunID        string
	RequiredCapability string
	Depth              int    // delegation depth of the calling chain
	TaskRef            string // caller-side task reference (planner task id)
}

// WorkerAttempt records one failed delegation attempt for traceability.
type WorkerAttempt struct {
	WorkerAgentID string
	DelegationID  string
	Error         string
}

// SupervisorResult is the aggregated outcome of one supervised task.
type SupervisorResult struct {
	TaskRef       string
	Task          string
	Status        string // delegation status: completed | awaiting_approval | ...
	WorkerAgentID string
	DelegationID  string
	ChildRunID    string
	Output        string
	Tokens        Usage
	// Attempts carries the FAILED attempts that preceded the successful one
	// (empty on first-try success).
	Attempts []WorkerAttempt
}

// SupervisorError is the typed all-workers-failed error. errors.Is(err,
// ErrAllWorkersFailed) matches; the Attempts slice carries the trace.
type SupervisorError struct {
	TaskRef  string
	Attempts []WorkerAttempt
}

// Error implements error. The message embeds the typed cause text; use
// errors.Is(err, ErrAllWorkersFailed) for classification (Is below).
func (e *SupervisorError) Error() string {
	parts := make([]string, 0, len(e.Attempts))
	for _, a := range e.Attempts {
		parts = append(parts, fmt.Sprintf("%s: %s", a.WorkerAgentID, a.Error))
	}
	return fmt.Sprintf("%s (task %s): %s", ErrAllWorkersFailed.Error(), e.TaskRef, strings.Join(parts, "; "))
}

// Is makes errors.Is(err, ErrAllWorkersFailed) work (Go error-wrapping
// contract for custom error types).
func (e *SupervisorError) Is(target error) bool { return target == ErrAllWorkersFailed }

// Delegator is the subset of the delegation protocol the supervisor uses
// (*DelegationService satisfies it).
type Delegator interface {
	Delegate(ctx context.Context, req DelegationRequest) (*DelegationRecord, error)
}

// Supervisor routes tasks to worker agents through the delegation protocol.
// Safe for concurrent use.
type Supervisor struct {
	mu        sync.Mutex
	cfg       SupervisorConfig
	delegator Delegator
	rr        int            // round-robin cursor
	inflight  map[string]int // worker id -> in-flight delegations
	sem       chan struct{}  // nil when unlimited
}

// NewSupervisor builds a supervisor. The strategy is validated eagerly
// (ErrInvalidStrategy); an empty worker list is tolerated at construction
// and surfaces as ErrNoWorkers at RunTask time.
func NewSupervisor(cfg SupervisorConfig, d Delegator) (*Supervisor, error) {
	if cfg.Strategy == "" {
		cfg.Strategy = RouteRoundRobin
	}
	switch cfg.Strategy {
	case RouteRoundRobin, RouteLeastLoaded, RouteCapabilityMatch:
	default:
		return nil, fmt.Errorf("%w: %q", ErrInvalidStrategy, cfg.Strategy)
	}
	if d == nil {
		return nil, errors.New("orchestration: supervisor requires a delegator")
	}
	s := &Supervisor{
		cfg:       cfg,
		delegator: d,
		inflight:  make(map[string]int),
	}
	if cfg.MaxConcurrent > 0 {
		s.sem = make(chan struct{}, cfg.MaxConcurrent)
	}
	return s, nil
}

// candidatesLocked returns the strategy-ordered candidate list for the task.
// Caller holds s.mu.
func (s *Supervisor) candidatesLocked(task SupervisorTask) ([]WorkerAgent, error) {
	workers := append([]WorkerAgent(nil), s.cfg.Workers...)
	switch s.cfg.Strategy {
	case RouteRoundRobin:
		if len(workers) == 0 {
			return nil, ErrNoWorkers
		}
		start := s.rr % len(workers)
		s.rr++
		rotated := make([]WorkerAgent, 0, len(workers))
		for i := 0; i < len(workers); i++ {
			rotated = append(rotated, workers[(start+i)%len(workers)])
		}
		return rotated, nil
	case RouteLeastLoaded:
		if len(workers) == 0 {
			return nil, ErrNoWorkers
		}
		// Stable insertion sort by in-flight count (ties keep config order).
		for i := 1; i < len(workers); i++ {
			for j := i; j > 0 && s.inflight[workers[j].AgentID] < s.inflight[workers[j-1].AgentID]; j-- {
				workers[j], workers[j-1] = workers[j-1], workers[j]
			}
		}
		return workers, nil
	case RouteCapabilityMatch:
		if strings.TrimSpace(task.RequiredCapability) == "" {
			// No requirement: every worker is a candidate in config order.
			return workers, nil
		}
		capable := make([]WorkerAgent, 0, len(workers))
		for _, w := range workers {
			for _, c := range w.Capabilities {
				if c == task.RequiredCapability {
					capable = append(capable, w)
					break
				}
			}
		}
		if len(capable) == 0 {
			return nil, fmt.Errorf("%w: %q", ErrNoCapableWorker, task.RequiredCapability)
		}
		return capable, nil
	default:
		// Unreachable: NewSupervisor validated the strategy.
		return nil, ErrInvalidStrategy
	}
}

// RunTask selects a worker, delegates through the protocol and aggregates
// the result. Failed attempts move to the next candidate until the retry
// budget (1 + MaxRetries, capped by the candidate list length) is exhausted,
// then the typed SupervisorError is returned.
func (s *Supervisor) RunTask(ctx context.Context, task SupervisorTask) (*SupervisorResult, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	if strings.TrimSpace(task.OrgID) == "" || strings.TrimSpace(task.Task) == "" {
		return nil, fmt.Errorf("%w: org_id and task are required", ErrInvalidRequest)
	}
	s.mu.Lock()
	candidates, err := s.candidatesLocked(task)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}

	// Concurrency cap (ctx-aware acquire; bounded wait, no busy loop).
	if s.sem != nil {
		select {
		case s.sem <- struct{}{}:
			defer func() { <-s.sem }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	maxAttempts := 1 + s.cfg.MaxRetries
	if maxAttempts > len(candidates) {
		maxAttempts = len(candidates)
	}
	result := &SupervisorResult{TaskRef: task.TaskRef, Task: task.Task}
	for i := 0; i < maxAttempts; i++ {
		candidate := candidates[i]
		s.mu.Lock()
		s.inflight[candidate.AgentID]++
		s.mu.Unlock()
		rec, err := s.delegator.Delegate(ctx, DelegationRequest{
			OrgID:       task.OrgID,
			FromAgentID: task.FromAgentID,
			ToAgentID:   candidate.AgentID,
			Task:        task.Task,
			ParentRunID: task.ParentRunID,
			Depth:       task.Depth,
		})
		s.mu.Lock()
		s.inflight[candidate.AgentID]--
		s.mu.Unlock()

		if err == nil && rec != nil && rec.Status == DelegationStatusAwaitingApproval {
			// Parked at the approval gate: not a failure, not a result —
			// surface it and stop (retrying another worker would double-file
			// the same human decision).
			result.Status = rec.Status
			result.DelegationID = rec.ID
			result.ChildRunID = rec.ChildRunID
			result.WorkerAgentID = candidate.AgentID
			return result, nil
		}
		if err != nil || rec == nil || rec.Status == DelegationStatusFailed {
			attempt := WorkerAttempt{WorkerAgentID: candidate.AgentID}
			if rec != nil {
				attempt.DelegationID = rec.ID
				if rec.Error != "" {
					attempt.Error = rec.Error
				}
			}
			if err != nil {
				attempt.Error = err.Error()
			}
			if attempt.Error == "" {
				attempt.Error = "delegation returned no result"
			}
			result.Attempts = append(result.Attempts, attempt)
			continue
		}
		result.Status = rec.Status
		result.WorkerAgentID = candidate.AgentID
		result.DelegationID = rec.ID
		result.ChildRunID = rec.ChildRunID
		result.Output = rec.Result
		result.Tokens = rec.Tokens
		return result, nil
	}
	return nil, &SupervisorError{TaskRef: task.TaskRef, Attempts: result.Attempts}
}

// SupervisorTasksFromPlan adapts the planner's task list into supervisor
// tasks (planner -> executor wiring; the planner itself is untouched).
// Org/response fields are left to the caller.
func SupervisorTasksFromPlan(plan *Plan) []SupervisorTask {
	if plan == nil {
		return nil
	}
	tasks := make([]SupervisorTask, 0, len(plan.Tasks))
	for _, t := range plan.Tasks {
		tasks = append(tasks, SupervisorTask{
			TaskRef: t.ID,
			Task:    t.Description,
		})
	}
	return tasks
}

// RunPlan executes a planner plan sequentially through the supervisor
// (org-scoped; fromAgentID receives the response messages when non-empty).
// Fail-fast: the first task that ends in SupervisorError aborts the plan and
// the error is returned with the results gathered so far dropped — the
// delegation ledger retains every attempt for traceability.
func (s *Supervisor) RunPlan(ctx context.Context, orgID string, plan *Plan, fromAgentID string) ([]SupervisorResult, error) {
	if plan == nil || len(plan.Tasks) == 0 {
		return nil, fmt.Errorf("%w: plan has no tasks", ErrInvalidRequest)
	}
	tasks := SupervisorTasksFromPlan(plan)
	results := make([]SupervisorResult, 0, len(tasks))
	for _, t := range tasks {
		t.OrgID = orgID
		t.FromAgentID = fromAgentID
		res, err := s.RunTask(ctx, t)
		if err != nil {
			return results, err
		}
		results = append(results, *res)
	}
	return results, nil
}
