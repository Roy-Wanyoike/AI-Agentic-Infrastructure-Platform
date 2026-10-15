package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// True parallel branch execution (issue #110).
//
// RunParallelBranches is the concurrency core: it executes a set of branches
// as real goroutines under a semaphore limiter and aggregates their results
// with one of four documented strategies. The dagExecutor (execute.go) builds
// branches from the fan-out children of a parallel node and calls this core;
// the core itself is executor-independent and fully covered by
// parallel_test.go with barrier-synchronized fakes (no sleeps).
//
// Aggregation strategies (AggregationStrategy):
//
//      all      — every branch must succeed; any failure fails the aggregate.
//                 Output: object {branch_id: output} of the successful outputs.
//      any      — first SUCCESSFUL completion wins; the remaining branches are
//                 cancelled. Fails only when every branch fails.
//      race     — first completion wins regardless of status; the remaining
//                 branches are cancelled. A failing first completion fails the
//                 aggregate (race semantics, documented).
//      majority — plurality vote over the successful outputs: the most frequently
//                 occurring serialized output wins; equal counts are broken by
//                 the lexicographically smallest serialized output. Failures do
//                 not vote; the aggregate fails only when no branch succeeded.
//
// Concurrency limits (ParallelLimits) compose three caps, and the effective
// limit is the minimum of the ones that are set:
//
//      Global      — AGENTOS_MAX_WORKFLOW_PARALLELISM (default 8), the
//                    deployment-wide ceiling.
//      PerWorkflow — the parallel node's DSL config.max_concurrency override.
//      OrgQuota    — the per-organization quota hook (OrgParallelQuota); the
//                    default hook is unlimited (0).
//
// Modes: fail_fast (default) cancels the remaining branches on the decision
// event (first failure for all, first success for any); collect_all
// (FailFast=false) lets every branch run to completion before aggregating.
// race/any always cancel the losers once decided, independent of the mode.
// ---------------------------------------------------------------------------

// AggregationStrategy selects how parallel branch results combine.
type AggregationStrategy string

// Aggregation strategies (see the package-level parallel execution docs).
const (
	// ParallelAggAll requires every branch to succeed.
	ParallelAggAll AggregationStrategy = "all"
	// ParallelAggAny takes the first successful branch and cancels the rest.
	ParallelAggAny AggregationStrategy = "any"
	// ParallelAggRace takes the first completion (success or failure).
	ParallelAggRace AggregationStrategy = "race"
	// ParallelAggMajority takes the most common successful output (plurality,
	// ties broken by the lexicographically smallest serialized output).
	ParallelAggMajority AggregationStrategy = "majority"
)

// Parallel execution modes surfaced in reports and node-run metadata.
const (
	// ParallelModeFailFast cancels remaining branches on the decision event.
	ParallelModeFailFast = "fail_fast"
	// ParallelModeCollectAll waits for every branch before aggregating.
	ParallelModeCollectAll = "collect_all"
)

// Parallel branch outcome statuses (mirrored onto node-run statuses).
const (
	ParallelBranchCompleted = "completed"
	ParallelBranchFailed    = "failed"
	ParallelBranchCancelled = "cancelled"
	ParallelBranchTimeout   = "timeout"
)

// EnvMaxWorkflowParallelism is the deployment-wide parallelism ceiling env
// var (issue #110).
const EnvMaxWorkflowParallelism = "AGENTOS_MAX_WORKFLOW_PARALLELISM"

// DefaultWorkflowParallelism is the global ceiling applied when
// AGENTOS_MAX_WORKFLOW_PARALLELISM is unset or invalid.
const DefaultWorkflowParallelism = 8

// DefaultMaxWorkflowParallelism resolves the global parallelism ceiling from
// AGENTOS_MAX_WORKFLOW_PARALLELISM (default DefaultWorkflowParallelism). It
// re-reads the environment on every call so deployments can hot-adjust and
// tests can use t.Setenv.
func DefaultMaxWorkflowParallelism() int {
	raw := strings.TrimSpace(os.Getenv(EnvMaxWorkflowParallelism))
	if raw == "" {
		return DefaultWorkflowParallelism
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return DefaultWorkflowParallelism
	}
	return n
}

// OrgParallelQuota is the per-organization parallelism quota hook. Wiring a
// quota lets a deployment cap concurrent workflow branches per tenant; the
// default (no hook wired) is unlimited.
type OrgParallelQuota interface {
	// MaxWorkflowParallelism returns the org's cap on concurrently executing
	// workflow branches. Values <= 0 mean unlimited.
	MaxWorkflowParallelism(ctx context.Context, orgID string) int
}

// ParallelLimits composes the three parallelism caps. Zero (or negative)
// component values mean "unset" for PerWorkflow/OrgQuota; Global <= 0 falls
// back to DefaultMaxWorkflowParallelism at resolution time.
type ParallelLimits struct {
	// Global is the deployment-wide ceiling (AGENTOS_MAX_WORKFLOW_PARALLELISM).
	Global int
	// PerWorkflow is the DSL config.max_concurrency override of the parallel node.
	PerWorkflow int
	// OrgQuota is the resolved per-organization cap (0 = unlimited).
	OrgQuota int
}

// Effective returns the minimum set cap; at least one cap is always set
// (Global defaults to DefaultMaxWorkflowParallelism), so the result is >= 1.
func (l ParallelLimits) Effective() int {
	eff := 0
	for _, v := range []int{l.Global, l.PerWorkflow, l.OrgQuota} {
		if v > 0 && (eff == 0 || v < eff) {
			eff = v
		}
	}
	if eff < 1 {
		eff = DefaultMaxWorkflowParallelism()
	}
	return eff
}

// ParallelBranch is one unit of parallel work. ID is the deterministic key
// used in reports and metadata (the executor uses the branch child node id).
type ParallelBranch struct {
	ID string
	// Execute runs the branch body. Implementations must respect ctx
	// cancellation: cancellation is how losers of any/race decisions and
	// fail_fast/timeout aborts are enforced.
	Execute func(ctx context.Context) (json.RawMessage, error)
}

// ParallelPolicy configures one RunParallelBranches invocation.
type ParallelPolicy struct {
	// Strategy aggregates the branch results (default ParallelAggAll).
	Strategy AggregationStrategy
	// FailFast selects fail_fast (true, default) vs collect_all (false).
	FailFast bool
	// BranchTimeout bounds every individual branch; 0 disables the cap.
	BranchTimeout time.Duration
}

// ParallelBranchOutcome is one branch's terminal record. Outcomes are
// reported in branch declaration order (deterministic).
type ParallelBranchOutcome struct {
	BranchID string
	Status   string // ParallelBranch* constant
	Output   json.RawMessage
	Error    string
	// Duration is the branch's wall time from dispatch to terminal
	// outcome; the concurrency-slot wait is included.
	Duration time.Duration
	// Wait is the subset of Duration spent waiting for a concurrency
	// slot (Duration - Wait is pure execution time).
	Wait time.Duration
}

// ExecutionShare returns the fraction of Duration spent executing rather
// than waiting for a concurrency slot (0 when Duration is 0).
func (o ParallelBranchOutcome) ExecutionShare() float64 {
	if o.Duration <= 0 {
		return 0
	}
	exec := o.Duration - o.Wait
	if exec < 0 {
		exec = 0
	}
	return exec.Seconds() / o.Duration.Seconds()
}

// ParallelReport is the aggregate result of one parallel execution.
type ParallelReport struct {
	Strategy AggregationStrategy
	Mode     string // ParallelModeFailFast | ParallelModeCollectAll
	// Outcomes lists every branch in declaration order (deterministic).
	Outcomes []ParallelBranchOutcome
	// Status is the aggregate status: completed | failed.
	Status string
	// Output is the aggregated output document (nil on failure).
	Output json.RawMessage
	// WinningBranch is the branch whose output won (any/race/majority; empty
	// for all, which aggregates every successful output).
	WinningBranch string
	// Error carries the joined failure reasons on aggregate failure.
	Error string
	// CompletionOrder lists branch indexes in completion order.
	CompletionOrder []int
	// MaxInFlight is the highest concurrently executing branch count.
	MaxInFlight int64
	// Limits echoes the resolved limits (Global defaulted) + the effective cap.
	Limits      ParallelLimits
	Effective   int
	Succeeded   int
	Failed      int
	Cancelled   int
	TimedOut    int
	BranchCount int
}

// ParallelLimiter is the counting semaphore enforcing the effective
// concurrency cap. It tracks the in-flight count and its high-water mark for
// execution metadata.
type ParallelLimiter struct {
	sem         chan struct{}
	inFlight    atomic.Int64
	maxInFlight atomic.Int64
}

// NewParallelLimiter returns a limiter with the given slot count (minimum 1).
func NewParallelLimiter(limit int) *ParallelLimiter {
	if limit < 1 {
		limit = 1
	}
	return &ParallelLimiter{sem: make(chan struct{}, limit)}
}

// InFlight returns the currently executing branch count.
func (l *ParallelLimiter) InFlight() int64 { return l.inFlight.Load() }

// MaxInFlight returns the high-water mark of the in-flight count.
func (l *ParallelLimiter) MaxInFlight() int64 { return l.maxInFlight.Load() }

// Acquire takes one slot, returning the release func, the time spent waiting
// and an error when ctx ended first. The release func is always non-nil and
// safe to call unconditionally (it is a no-op after a failed Acquire).
func (l *ParallelLimiter) Acquire(ctx context.Context) (func(), time.Duration, error) {
	start := time.Now()
	select {
	case l.sem <- struct{}{}:
	case <-ctx.Done():
		return func() {}, time.Since(start), ctx.Err()
	}
	// Double-check the context after winning the slot: select picks
	// randomly among ready cases, so an already-cancelled ctx must not
	// let a branch execute (issue #110 cancellation determinism).
	select {
	case <-ctx.Done():
		<-l.sem
		return func() {}, time.Since(start), ctx.Err()
	default:
	}
	wait := time.Since(start)
	cur := l.inFlight.Add(1)
	for {
		m := l.maxInFlight.Load()
		if cur <= m || l.maxInFlight.CompareAndSwap(m, cur) {
			break
		}
	}
	released := false
	return func() {
		if released {
			return
		}
		released = true
		l.inFlight.Add(-1)
		<-l.sem
	}, wait, nil
}

// RunParallelBranches executes branches concurrently under limits/policy and
// aggregates their results. It blocks until every branch has terminated (the
// losers observe cancellation through their own Execute implementations —
// the StepExecutor contract requires ctx compliance) and returns the report.
// A nil strategy falls back to ParallelAggAll.
func RunParallelBranches(ctx context.Context, branches []ParallelBranch, policy ParallelPolicy, limits ParallelLimits) ParallelReport {
	strategy := policy.Strategy
	switch strategy {
	case ParallelAggAll, ParallelAggAny, ParallelAggRace, ParallelAggMajority:
	default:
		strategy = ParallelAggAll
	}
	mode := ParallelModeFailFast
	if !policy.FailFast {
		mode = ParallelModeCollectAll
	}
	if limits.Global < 1 {
		limits.Global = DefaultMaxWorkflowParallelism()
	}
	eff := limits.Effective()
	report := ParallelReport{
		Strategy: strategy, Mode: mode,
		Status: RunStatusCompleted, Limits: limits, Effective: eff,
		BranchCount: len(branches),
		Outcomes:    make([]ParallelBranchOutcome, len(branches)),
	}
	if len(branches) == 0 {
		report.Output = json.RawMessage(`{}`)
		return report
	}

	limiter := NewParallelLimiter(eff)
	branchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	completions := make(chan int, len(branches))
	var wg sync.WaitGroup
	for i := range branches {
		wg.Add(1)
		go func(idx int, br ParallelBranch) {
			defer wg.Done()
			out := ParallelBranchOutcome{BranchID: br.ID}
			start := time.Now()
			release, wait, aerr := limiter.Acquire(branchCtx)
			defer release()
			out.Wait = wait
			out.Duration = time.Since(start)
			if aerr != nil {
				out.Status = classifyBranchError(aerr)
				out.Error = fmt.Sprintf("branch %s: cancelled before execution: %v", br.ID, aerr)
				report.Outcomes[idx] = out
				completions <- idx
				return
			}
			execCtx := branchCtx
			if policy.BranchTimeout > 0 {
				tctx, tcancel := context.WithTimeout(branchCtx, policy.BranchTimeout)
				defer tcancel()
				execCtx = tctx
			}
			output, err := br.Execute(execCtx)
			out.Duration = time.Since(start)
			out.Output = output
			out.Status = classifyBranchError(err)
			if err != nil {
				out.Error = err.Error()
			}
			report.Outcomes[idx] = out
			completions <- idx
		}(i, branches[i])
	}

	// Decision loop: consume completions as they happen so any/race/all+
	// fail_fast can cancel the losers without waiting for the stragglers.
	decided := false
	var winner int // valid once decided && strategy != all
	remaining := len(branches)
	report.CompletionOrder = make([]int, 0, len(branches))
	for remaining > 0 {
		var idx int
		select {
		case <-ctx.Done():
			// Outer cancellation: abort everything, then keep draining until
			// every branch observed the cancellation (Execute honors ctx).
			cancel()
			continue
		case idx = <-completions:
		}
		remaining--
		report.CompletionOrder = append(report.CompletionOrder, idx)
		if decided {
			continue
		}
		out := report.Outcomes[idx]
		switch strategy {
		case ParallelAggAll:
			if out.Status != ParallelBranchCompleted && policy.FailFast {
				cancel() // first failure fails the aggregate; abort the rest
				decided = true
			}
		case ParallelAggAny:
			if out.Status == ParallelBranchCompleted {
				winner, decided = idx, true
				cancel() // first success wins; cancel the rest
			}
		case ParallelAggRace:
			winner, decided = idx, true
			cancel() // first completion wins
		case ParallelAggMajority:
			// Plurality vote needs every vote: no early decision.
		}
	}
	wg.Wait()
	report.MaxInFlight = limiter.MaxInFlight()

	for _, out := range report.Outcomes {
		switch out.Status {
		case ParallelBranchCompleted:
			report.Succeeded++
		case ParallelBranchFailed:
			report.Failed++
		case ParallelBranchCancelled:
			report.Cancelled++
		case ParallelBranchTimeout:
			report.TimedOut++
		}
	}

	switch strategy {
	case ParallelAggAll:
		aggregateAll(&report)
	case ParallelAggAny:
		aggregateAny(&report, winner)
	case ParallelAggRace:
		aggregateRace(&report, winner)
	case ParallelAggMajority:
		aggregateMajority(&report)
	}
	return report
}

// aggregateAll implements the "all" strategy: any non-completed branch fails
// the aggregate; success emits every output keyed by branch id.
func aggregateAll(report *ParallelReport) {
	var failures []string
	merged := make(map[string]json.RawMessage, len(report.Outcomes))
	for _, out := range report.Outcomes {
		if out.Status == ParallelBranchCompleted {
			merged[out.BranchID] = out.Output
			continue
		}
		failures = append(failures, branchFailureText(out))
	}
	if len(failures) > 0 {
		report.Status = RunStatusFailed
		report.Error = strings.Join(failures, "; ")
		return
	}
	raw, err := json.Marshal(merged)
	if err != nil {
		report.Status = RunStatusFailed
		report.Error = fmt.Sprintf("all strategy: aggregate output marshal failed: %v", err)
		return
	}
	report.Output = raw
}

// aggregateAny implements "any": the first successful completion wins.
func aggregateAny(report *ParallelReport, winner int) {
	if report.Outcomes[winner].Status == ParallelBranchCompleted {
		report.WinningBranch = report.Outcomes[winner].BranchID
		report.Output = report.Outcomes[winner].Output
		return
	}
	// No success arrived before the decision window closed: fail with every
	// branch's reason.
	var failures []string
	for _, out := range report.Outcomes {
		failures = append(failures, branchFailureText(out))
	}
	report.Status = RunStatusFailed
	report.Error = strings.Join(failures, "; ")
}

// aggregateRace implements "race": the first completion wins whatever its
// status (a failing first completion fails the aggregate).
func aggregateRace(report *ParallelReport, winner int) {
	out := report.Outcomes[winner]
	report.WinningBranch = out.BranchID
	if out.Status == ParallelBranchCompleted {
		report.Output = out.Output
		return
	}
	report.Status = RunStatusFailed
	report.Error = branchFailureText(out)
}

// aggregateMajority implements "majority": plurality vote over the successful
// outputs; ties break to the lexicographically smallest serialized output.
// Failures do not vote; zero successes fail the aggregate.
func aggregateMajority(report *ParallelReport) {
	// Votes are order-independent; the winning branch, however, is picked
	// deterministically in declaration order (completion order is racy).
	counts := make(map[string]int)
	for _, out := range report.Outcomes {
		if out.Status != ParallelBranchCompleted {
			continue
		}
		counts[string(out.Output)]++
	}
	if len(counts) == 0 {
		var failures []string
		for _, out := range report.Outcomes {
			failures = append(failures, branchFailureText(out))
		}
		report.Status = RunStatusFailed
		report.Error = strings.Join(failures, "; ")
		return
	}
	bestKey := ""
	bestCount := 0
	for key, count := range counts {
		switch {
		case count > bestCount:
			bestKey, bestCount = key, count
		case count == bestCount && key < bestKey:
			// Tie: lexicographically smallest serialized output wins.
			bestKey = key
		}
	}
	for _, out := range report.Outcomes {
		if out.Status == ParallelBranchCompleted && string(out.Output) == bestKey {
			report.WinningBranch = out.BranchID
			report.Output = out.Output
			return
		}
	}
}

// branchFailureText renders one non-completed outcome for aggregate errors.
func branchFailureText(out ParallelBranchOutcome) string {
	if out.Error != "" {
		return fmt.Sprintf("branch %s: %s", out.BranchID, out.Error)
	}
	return fmt.Sprintf("branch %s: %s", out.BranchID, out.Status)
}

// classifyBranchError maps a branch error onto the ParallelBranch* statuses.
func classifyBranchError(err error) string {
	switch {
	case err == nil:
		return ParallelBranchCompleted
	case errors.Is(err, context.DeadlineExceeded):
		return ParallelBranchTimeout
	case errors.Is(err, context.Canceled):
		return ParallelBranchCancelled
	default:
		return ParallelBranchFailed
	}
}

// ---------------------------------------------------------------------------
// Parallel node config + DAG integration
// ---------------------------------------------------------------------------

// ParallelNodeConfig is the parsed config of a parallel node (issue #110).
type ParallelNodeConfig struct {
	Strategy       AggregationStrategy
	MaxConcurrency int // per-workflow DSL override (config.max_concurrency)
	FailFast       bool
	BranchTimeout  time.Duration
}

// ParallelNodeConfigFromNode parses a parallel node's config with defaults:
// strategy "all", fail_fast mode, no per-branch timeout, no DSL concurrency
// override (the deployment global AGENTOS_MAX_WORKFLOW_PARALLELISM still
// applies). Unknown strategies fall back to "all".
func ParallelNodeConfigFromNode(n Node) ParallelNodeConfig {
	cfg := ParallelNodeConfig{Strategy: ParallelAggAll, FailFast: true}
	if s, ok := n.Config["strategy"].(string); ok {
		switch AggregationStrategy(strings.ToLower(strings.TrimSpace(s))) {
		case ParallelAggAll, ParallelAggAny, ParallelAggRace, ParallelAggMajority:
			cfg.Strategy = AggregationStrategy(strings.ToLower(strings.TrimSpace(s)))
		}
	}
	if v := configFloat(n.Config, "max_concurrency"); v >= 1 {
		cfg.MaxConcurrency = int(v)
	}
	if ff, ok := n.Config["fail_fast"].(bool); ok {
		cfg.FailFast = ff
	}
	if v := configFloat(n.Config, "branch_timeout_seconds"); v > 0 {
		cfg.BranchTimeout = time.Duration(v * float64(time.Second))
	}
	return cfg
}

// parallelBranchGroup identifies the fan-out wave of a parallel node: the
// direct edge successors that
//
//   - are executable step nodes (agent/tool/delay/webhook/end — control-flow
//     and approval children keep their sequential scheduling), are not owned
//     by a loop/retry owner and are not referenced branch/case targets of a
//     condition/switch router,
//   - have no incoming edges other than from the parallel node itself (join
//     nodes with other predecessors activate through normal scheduling), and
//   - are not reachable from another wave member (interdependent successors
//     execute through normal edge activation once their in-wave predecessor
//     completes).
//
// The wave is returned in topological order (deterministic result ordering).
func (x *dagExecutor) parallelBranchGroup(parallelID string) []string {
	candidates := make([]string, 0, 4)
	seen := make(map[string]bool)
	for _, e := range x.edgesFrom[parallelID] {
		if e.To == parallelID || seen[e.To] {
			continue
		}
		seen[e.To] = true
		candidates = append(candidates, e.To)
	}
	wave := make([]string, 0, len(candidates))
	for _, c := range candidates {
		node, ok := x.nodes[c]
		if !ok || x.owned[c] != "" || x.referenced[c] || !isLoopOwnableNodeType(node.Type) {
			continue
		}
		sole := true
		for _, e := range x.dsl.Edges {
			if e.To == c && e.From != parallelID {
				if _, exists := x.state[e.From]; exists {
					sole = false
					break
				}
			}
		}
		if sole {
			wave = append(wave, c)
		}
	}
	// Drop candidates reachable from another wave member (interdependent).
	candidateSet := make(map[string]bool, len(wave))
	for _, c := range wave {
		candidateSet[c] = true
	}
	dependent := make(map[string]bool)
	for _, c := range wave {
		visited := make(map[string]bool)
		var dfs func(from string)
		dfs = func(from string) {
			for _, e := range x.edgesFrom[from] {
				if !candidateSet[e.To] || visited[e.To] {
					continue
				}
				visited[e.To] = true
				dependent[e.To] = true
				dfs(e.To)
			}
		}
		dfs(c)
	}
	final := make([]string, 0, len(wave))
	for _, c := range wave {
		if !dependent[c] {
			final = append(final, c)
		}
	}
	sort.Slice(final, func(i, j int) bool { return x.index[final[i]] < x.index[final[j]] })
	return final
}

// executeParallelNode runs the parallel node's fan-out wave concurrently
// (issue #110), records per-branch node runs with duration/wait/utilization
// metadata, aggregates per the node's strategy and routes the aggregate.
// Aggregate failure fails the workflow run (fail-fast contract).
func (x *dagExecutor) executeParallelNode(node Node, started time.Time) error {
	cfg := ParallelNodeConfigFromNode(node)
	group := x.parallelBranchGroup(node.ID)

	limits := ParallelLimits{Global: DefaultMaxWorkflowParallelism(), PerWorkflow: cfg.MaxConcurrency}
	if x.engine.OrgQuota != nil {
		limits.OrgQuota = x.engine.OrgQuota.MaxWorkflowParallelism(x.ctx, x.orgID)
	}

	// Take the wave children over from the ready queue before any
	// propagation: they execute under this node, never via the scheduler.
	for _, id := range group {
		if st := x.state[id]; st != nil {
			st.scheduled = true
		}
	}

	branches := make([]ParallelBranch, 0, len(group))
	// recorded[i] marks branches whose dispatch already created its own
	// node-run row (the default run-expansion path records a pending row for
	// agent/tool nodes and a failed row when run creation fails); the
	// recording pass below must not duplicate those rows. Each goroutine
	// writes only its own slot, and the pass reads after all branches
	// finished — no further synchronization needed.
	recorded := make([]bool, len(group))
	for i, childID := range group {
		child := x.nodes[childID]
		idx := i
		branches = append(branches, ParallelBranch{
			ID: childID,
			Execute: func(ctx context.Context) (json.RawMessage, error) {
				switch child.Type {
				case NodeAgent, NodeTool:
					if x.exec == nil {
						defer func() { recorded[idx] = true }()
					}
					return x.dispatchStepCtx(ctx, child)
				default:
					// Passive structural branch (delay/webhook/end): no
					// dispatch, the terminal branch row carries the output.
					return json.RawMessage(`{}`), nil
				}
			},
		})
	}
	policy := ParallelPolicy{Strategy: cfg.Strategy, FailFast: cfg.FailFast, BranchTimeout: cfg.BranchTimeout}
	report := RunParallelBranches(x.ctx, branches, policy, limits)

	// Record the branch node runs in deterministic (topo) order and route
	// their outgoing edges: completed branches propagate, cancelled/timed-out
	// branches prune (branch not taken), failed branches leave the decision
	// to the aggregate below. Branches whose run-expansion dispatch already
	// created its own row (recorded[i]) do not get a duplicate terminal row —
	// the pending/failed row stands until the worker finalizes it.
	now := time.Now().UTC()
	for i, out := range report.Outcomes {
		childID := group[i]
		finished := now
		metadata := map[string]any{
			"parallel_node": node.ID,
			"status":        out.Status,
			"duration_ms":   out.Duration.Milliseconds(),
			"wait_ms":       out.Wait.Milliseconds(),
			"utilization":   out.ExecutionShare(),
		}
		switch out.Status {
		case ParallelBranchCompleted:
			if !recorded[i] {
				x.svc.recordNodeRun(x.ctx, x.orgID, &NodeRun{
					ID: uuid.NewString(), WorkflowRunID: x.workflowRunID, NodeID: childID,
					Status: RunStatusCompleted, StartedAt: &started, FinishedAt: &finished, CreatedAt: started,
					Output: out.Output, Metadata: metadata,
				})
			}
			x.outputs[childID] = out.Output
			x.activateOwnedOutgoing(childID)
		case ParallelBranchCancelled, ParallelBranchTimeout:
			x.svc.recordNodeRun(x.ctx, x.orgID, &NodeRun{
				ID: uuid.NewString(), WorkflowRunID: x.workflowRunID, NodeID: childID,
				Status: runStatusForBranch(out.Status), ErrorCode: branchErrorCode(out.Status),
				StartedAt: &started, FinishedAt: &finished, CreatedAt: started,
				Metadata: metadata,
			})
			x.abandonParallelBranch(childID)
		default: // failed
			if !recorded[i] {
				x.svc.recordNodeRun(x.ctx, x.orgID, &NodeRun{
					ID: uuid.NewString(), WorkflowRunID: x.workflowRunID, NodeID: childID,
					Status: RunStatusFailed, Error: out.Error, StartedAt: &started, FinishedAt: &finished, CreatedAt: started,
					Metadata: metadata,
				})
			}
		}
	}

	if report.Status != RunStatusCompleted {
		return x.failNode(node, started, fmt.Errorf("parallel node %s: %s strategy failed: %s", node.ID, cfg.Strategy, report.Error))
	}

	branchOrder := make([]string, 0, len(group))
	durations := make(map[string]any, len(group))
	for i, out := range report.Outcomes {
		branchOrder = append(branchOrder, group[i])
		durations[group[i]] = out.Duration.Milliseconds()
	}
	raw := report.Output
	x.outputs[node.ID] = raw
	aggregate := map[string]any{
		"strategy":         string(cfg.Strategy),
		"mode":             report.Mode,
		"branches":         report.BranchCount,
		"succeeded":        report.Succeeded,
		"failed":           report.Failed,
		"cancelled":        report.Cancelled,
		"timed_out":        report.TimedOut,
		"winner":           report.WinningBranch,
		"branch_order":     branchOrder,
		"completion_order": report.CompletionOrder,
		"durations_ms":     durations,
		"max_in_flight":    report.MaxInFlight,
		"limits": map[string]any{
			"global": report.Limits.Global, "per_workflow": report.Limits.PerWorkflow,
			"org_quota": report.Limits.OrgQuota, "effective": report.Effective,
		},
	}
	finished := time.Now().UTC()
	x.svc.recordNodeRun(x.ctx, x.orgID, &NodeRun{
		ID: uuid.NewString(), WorkflowRunID: x.workflowRunID, NodeID: node.ID,
		Status: RunStatusCompleted, StartedAt: &started, FinishedAt: &finished, CreatedAt: started,
		Output: raw, Metadata: map[string]any{"parallel": aggregate},
	})
	// Fire the parallel node's outgoing edges: join/dependent successors
	// activate through normal scheduling; wave children are already done.
	x.propagate(node.ID)
	return nil
}

// abandonParallelBranch marks a cancelled/timed-out wave branch as skipped
// state and prunes its outgoing edges (a branch that never produced output
// does not activate its successors).
func (x *dagExecutor) abandonParallelBranch(childID string) {
	st := x.state[childID]
	if st == nil {
		return
	}
	st.skipped = true
	for _, e := range x.edgesFrom[childID] {
		x.prune(e.To, "predecessor_skipped")
	}
}

// runStatusForBranch maps a ParallelBranch* status onto a node-run status.
func runStatusForBranch(status string) string {
	switch status {
	case ParallelBranchCancelled:
		return RunStatusCancelled
	case ParallelBranchTimeout:
		return RunStatusTimeout
	case ParallelBranchFailed:
		return RunStatusFailed
	default:
		return RunStatusCompleted
	}
}

// branchErrorCode returns the stable error code for non-completed branches.
func branchErrorCode(status string) string {
	if status == ParallelBranchTimeout {
		return "PARALLEL_BRANCH_TIMEOUT"
	}
	return "PARALLEL_BRANCH_CANCELLED"
}
