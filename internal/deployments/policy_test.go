package deployments

// Eval-gated canary promotion tests (issue #51): promote-on-success and
// rollback-on-failure with exact reason strings, the MinCanaryRuns evidence
// gate, the flag-off no-op contract, p95/cost gates (0 = disabled), the
// evaluation window, idempotence (decision recorded once, incl. concurrent
// triggers), manual-action precedence, sticky-split invariance, and the
// status read model.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeSampleSource serves a fixed sample list and counts engine consultations.
type fakeSampleSource struct {
	mu      sync.Mutex
	samples []EvalSample
	err     error
	calls   int
}

func (f *fakeSampleSource) CanaryEvalSamples(_ context.Context, _, _ string, _ int) ([]EvalSample, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.samples, nil
}

func (f *fakeSampleSource) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// recordingAuditer captures automatic decisions (with a mutex: the engine
// fires from observer goroutines in the cmd/api loop tests and concurrently
// in the idempotence test).
type recordingAuditer struct {
	mu        sync.Mutex
	orgIDs    []string
	depIDs    []string
	decisions []*CanaryDecision
}

func (a *recordingAuditer) AuditCanaryDecision(_ context.Context, orgID string, dep *Deployment, decision *CanaryDecision) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.orgIDs = append(a.orgIDs, orgID)
	a.depIDs = append(a.depIDs, dep.ID)
	a.decisions = append(a.decisions, decision)
}

func (a *recordingAuditer) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.decisions)
}

func (a *recordingAuditer) last() *CanaryDecision {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.decisions) == 0 {
		return nil
	}
	return a.decisions[len(a.decisions)-1]
}

// policyFixture builds a service with a healthy canary deployment (canary
// version canaryVersion at split weight), the given policy attached, and the
// injected sample source + audit recorder.
func policyFixture(t *testing.T, orgID, agentID string, weight int, policy *AgentPromotionPolicy) (*Service, *Deployment, *fakeSampleSource, *recordingAuditer) {
	t.Helper()
	svc := canaryFixture(&fakeResolver{
		allowed:  map[string]bool{agentID + "/2": true, agentID + "/3": true},
		existing: map[string]bool{agentID + "/2": true, agentID + "/3": true},
	})
	dep := healthyWithCanary(t, svc, orgID, agentID, 2, 3, weight, EnvironmentProduction)
	if policy != nil {
		updated, err := svc.SetCanaryPromotionPolicyCtx(context.Background(), orgID, dep.ID, policy)
		if err != nil {
			t.Fatalf("SetCanaryPromotionPolicyCtx returned error: %v", err)
		}
		dep = updated
	}
	source := &fakeSampleSource{}
	svc.SetCanarySampleSource(source)
	auditer := &recordingAuditer{}
	svc.SetCanaryDecisionAuditer(auditer)
	return svc, dep, source, auditer
}

// passingSamples builds n completed eval runs at the given case pass rate
// (each run: cases cases, passed passed) with one latency per case.
func passingSamples(n, cases, passed int, latencyMS float64) []EvalSample {
	samples := make([]EvalSample, 0, n)
	for i := range n {
		latencies := make([]float64, cases)
		for j := range latencies {
			latencies[j] = latencyMS
		}
		samples = append(samples, EvalSample{
			RunID:       "run-" + string(rune('a'+i)),
			CreatedAt:   time.Now().UTC(),
			Cases:       cases,
			Passed:      passed,
			LatenciesMS: latencies,
			CostCents:   5,
		})
	}
	return samples
}

func TestCanaryAutoPromoteOnSuccess(t *testing.T) {
	t.Setenv(AutoPromoteEnvVar, "true")
	svc, dep, source, auditer := policyFixture(t, "org-1", "agent-a", 10, &AgentPromotionPolicy{MinPassRate: 0.80, MinCanaryRuns: 2})
	source.samples = passingSamples(2, 10, 9, 25) // case-weighted pass rate 0.90

	svc.OnEvalRunCompleted(context.Background(), "org-1", "agent-a")

	fresh, err := svc.GetDeploymentCtx(context.Background(), "org-1", dep.ID)
	if err != nil {
		t.Fatalf("GetDeploymentCtx returned error: %v", err)
	}
	// PROMOTE = canary->100%: the canary became the stable version and the
	// canary config cleared (same semantics as the manual promote endpoint).
	if fresh.Version != 3 || fresh.CanaryVersion != 0 || fresh.CanaryWeight != 0 {
		t.Fatalf("auto-promote should swap canary to stable, got %+v", fresh)
	}
	if fresh.Status != StatusHealthy {
		t.Fatalf("auto-promote must keep the row healthy, got %q", fresh.Status)
	}
	decision := fresh.Promotion.Decision
	if decision == nil || decision.Action != CanaryDecisionPromote {
		t.Fatalf("expected a recorded promote decision, got %+v", decision)
	}
	// Reason assertion (exact string shape from the issue).
	if decision.Reason != "pass_rate 0.90 >= 0.80 → promote" {
		t.Fatalf("unexpected promote reason: %q", decision.Reason)
	}
	if decision.RunsCounted != 2 || decision.PassRate != 0.90 {
		t.Fatalf("decision should carry the sample stats, got %+v", decision)
	}
	if decision.Policy.MinPassRate != 0.80 || decision.Policy.MinCanaryRuns != 2 {
		t.Fatalf("decision should snapshot the policy, got %+v", decision.Policy)
	}
	if auditer.count() != 1 {
		t.Fatalf("exactly one audit event expected, got %d", auditer.count())
	}
	// The split now serves the promoted version for this agent.
	got, err := svc.ResolveVersionCtx(context.Background(), "org-1", "agent-a", EnvironmentProduction)
	if err != nil || got != 3 {
		t.Fatalf("after promote the stable v3 serves, got %d/%v", got, err)
	}
}

func TestCanaryAutoRollbackOnFailure(t *testing.T) {
	t.Setenv(AutoPromoteEnvVar, "true")
	svc, dep, source, auditer := policyFixture(t, "org-1", "agent-a", 50, &AgentPromotionPolicy{MinPassRate: 0.80, MinCanaryRuns: 2})
	// 31/50 + 31/50 = 62/100 = 0.62 — the exact example reason from the issue.
	source.samples = []EvalSample{
		{RunID: "run-1", CreatedAt: time.Now().UTC(), Cases: 50, Passed: 31,
			LatenciesMS: []float64{10, 20}, CostCents: 3},
		{RunID: "run-2", CreatedAt: time.Now().UTC(), Cases: 50, Passed: 31,
			LatenciesMS: []float64{10}, CostCents: 4},
	}

	svc.OnEvalRunCompleted(context.Background(), "org-1", "agent-a")

	fresh, err := svc.GetDeploymentCtx(context.Background(), "org-1", dep.ID)
	if err != nil {
		t.Fatalf("GetDeploymentCtx returned error: %v", err)
	}
	// ROLLBACK = revert to baseline: the canary config is cleared and the
	// stable version keeps serving 100% (abort semantics; row stays healthy).
	if fresh.Version != 2 || fresh.CanaryVersion != 0 || fresh.CanaryWeight != 0 {
		t.Fatalf("auto-rollback should clear the canary and keep stable v2, got %+v", fresh)
	}
	if fresh.Status != StatusHealthy {
		t.Fatalf("rollback keeps the row healthy, got %q", fresh.Status)
	}
	decision := fresh.Promotion.Decision
	if decision == nil || decision.Action != CanaryDecisionRollback {
		t.Fatalf("expected a recorded rollback decision, got %+v", decision)
	}
	if decision.Reason != "pass_rate 0.62 < 0.80 → rollback" {
		t.Fatalf("unexpected rollback reason: %q", decision.Reason)
	}
	if auditer.count() != 1 || auditer.last().Action != CanaryDecisionRollback {
		t.Fatalf("expected one rollback audit event, got %+v", auditer.last())
	}
	if got, _ := svc.ResolveVersionCtx(context.Background(), "org-1", "agent-a", EnvironmentProduction); got != 2 {
		t.Fatalf("after rollback the baseline v2 serves, got v%d", got)
	}
}

func TestCanaryPolicyLatencyAndCostGates(t *testing.T) {
	t.Setenv(AutoPromoteEnvVar, "true")
	orgID, agentID := "org-1", "agent-a"

	// Latency gate: pass rate fine, p95 over the boundary -> rollback.
	svc, dep, source, _ := policyFixture(t, orgID, agentID, 10, &AgentPromotionPolicy{MinPassRate: 0.5, MinCanaryRuns: 1, MaxP95LatencyMs: 100})
	source.samples = passingSamples(1, 3, 3, 0)
	source.samples[0].LatenciesMS = []float64{50, 200, 300} // nearest-rank p95 = 300
	svc.OnEvalRunCompleted(context.Background(), orgID, agentID)
	fresh, _ := svc.GetDeploymentCtx(context.Background(), orgID, dep.ID)
	if fresh.Promotion.Decision == nil || fresh.Promotion.Decision.Reason != "p95_latency_ms 300 > 100 → rollback" {
		t.Fatalf("expected p95 rollback reason, got %+v", fresh.Promotion.Decision)
	}

	// Cost gate: pass rate + p95 fine, avg cost per run over the boundary.
	svc2, dep2, source2, _ := policyFixture(t, orgID, agentID, 10, &AgentPromotionPolicy{MinPassRate: 0.5, MinCanaryRuns: 1, MaxCostPerRunCents: 100})
	source2.samples = []EvalSample{
		{RunID: "run-1", CreatedAt: time.Now().UTC(), Cases: 2, Passed: 2, LatenciesMS: []float64{10}, CostCents: 50},
		{RunID: "run-2", CreatedAt: time.Now().UTC(), Cases: 2, Passed: 2, LatenciesMS: []float64{10}, CostCents: 260},
	} // avg 155 > 100
	svc2.OnEvalRunCompleted(context.Background(), orgID, agentID)
	fresh2, _ := svc2.GetDeploymentCtx(context.Background(), orgID, dep2.ID)
	if fresh2.Promotion.Decision == nil || fresh2.Promotion.Decision.Reason != "avg_cost_per_run_cents 155 > 100 → rollback" {
		t.Fatalf("expected cost rollback reason, got %+v", fresh2.Promotion.Decision)
	}

	// 0 = disabled: the same violating samples promote when the gates are off.
	svc3, dep3, source3, _ := policyFixture(t, orgID, agentID, 10, &AgentPromotionPolicy{MinPassRate: 0.5, MinCanaryRuns: 1})
	source3.samples = []EvalSample{
		{RunID: "run-1", CreatedAt: time.Now().UTC(), Cases: 2, Passed: 2, LatenciesMS: []float64{5000}, CostCents: 999},
	}
	svc3.OnEvalRunCompleted(context.Background(), orgID, agentID)
	fresh3, _ := svc3.GetDeploymentCtx(context.Background(), orgID, dep3.ID)
	if fresh3.Promotion.Decision == nil || fresh3.Promotion.Decision.Action != CanaryDecisionPromote {
		t.Fatalf("disabled gates must not block promotion, got %+v", fresh3.Promotion.Decision)
	}
}

func TestCanaryMinCanaryRunsGate(t *testing.T) {
	t.Setenv(AutoPromoteEnvVar, "true")
	svc, dep, source, auditer := policyFixture(t, "org-1", "agent-a", 25, &AgentPromotionPolicy{MinPassRate: 0.8, MinCanaryRuns: 3})
	source.samples = passingSamples(2, 10, 10, 10) // perfect but not enough runs

	svc.OnEvalRunCompleted(context.Background(), "org-1", "agent-a")

	fresh, _ := svc.GetDeploymentCtx(context.Background(), "org-1", dep.ID)
	if fresh.CanaryVersion != 3 || fresh.CanaryWeight != 25 {
		t.Fatalf("below MinCanaryRuns the canary stays attached untouched, got %+v", fresh)
	}
	if fresh.Promotion.Decision != nil {
		t.Fatalf("no decision may be recorded below MinCanaryRuns, got %+v", fresh.Promotion.Decision)
	}
	if auditer.count() != 0 {
		t.Fatalf("no audit below MinCanaryRuns, got %d", auditer.count())
	}

	// The third completed run crosses the evidence gate -> decision.
	source.samples = passingSamples(3, 10, 10, 10)
	svc.OnEvalRunCompleted(context.Background(), "org-1", "agent-a")
	fresh, _ = svc.GetDeploymentCtx(context.Background(), "org-1", dep.ID)
	if fresh.Promotion.Decision == nil || fresh.Promotion.Decision.Action != CanaryDecisionPromote {
		t.Fatalf("third run should trigger the promote decision, got %+v", fresh.Promotion.Decision)
	}
}

func TestCanaryAutoPromoteFlagOff(t *testing.T) {
	orgID, agentID := "org-1", "agent-a"
	for _, tc := range []struct {
		name, value string
		set         bool
	}{
		{name: "unset", set: false},
		{name: "explicit off", value: "0", set: true},
		{name: "garbage off", value: "yes-please", set: true},
		{name: "blank off", value: " ", set: true},
	} {
		if tc.set {
			t.Setenv(AutoPromoteEnvVar, tc.value)
		}
		svc, dep, source, auditer := policyFixture(t, orgID, agentID, 40, &AgentPromotionPolicy{MinPassRate: 0.8, MinCanaryRuns: 1})
		source.samples = passingSamples(1, 10, 0, 5) // would roll back if the engine ran

		svc.OnEvalRunCompleted(context.Background(), orgID, agentID)

		fresh, _ := svc.GetDeploymentCtx(context.Background(), orgID, dep.ID)
		if fresh.CanaryVersion != 3 || fresh.CanaryWeight != 40 {
			t.Fatalf("%s: flag off must leave the canary untouched, got %+v", tc.name, fresh)
		}
		if fresh.Promotion.Decision != nil || auditer.count() != 0 || source.callCount() != 0 {
			t.Fatalf("%s: flag off must be a no-op (decision=%v audits=%d samples=%d)", tc.name, fresh.Promotion.Decision, auditer.count(), source.callCount())
		}
	}
}

func TestCanaryDecisionIdempotent(t *testing.T) {
	t.Setenv(AutoPromoteEnvVar, "true")
	svc, dep, source, auditer := policyFixture(t, "org-1", "agent-a", 10, &AgentPromotionPolicy{MinPassRate: 0.8, MinCanaryRuns: 1})
	source.samples = passingSamples(1, 10, 9, 10)

	// Concurrent triggers: exactly ONE decision may be recorded.
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			svc.OnEvalRunCompleted(context.Background(), "org-1", "agent-a")
		}()
	}
	wg.Wait()

	fresh, _ := svc.GetDeploymentCtx(context.Background(), "org-1", dep.ID)
	if fresh.Promotion.Decision == nil {
		t.Fatal("expected a recorded decision")
	}
	decidedAt := fresh.Promotion.Decision.DecidedAt
	if auditer.count() != 1 {
		t.Fatalf("decision must be audited exactly once, got %d", auditer.count())
	}

	// Re-triggering after the decision: no re-decide, no re-audit (the canary
	// is gone post-promote, and the recorded decision is immutable).
	for range 3 {
		svc.OnEvalRunCompleted(context.Background(), "org-1", "agent-a")
	}
	fresh, _ = svc.GetDeploymentCtx(context.Background(), "org-1", dep.ID)
	if !fresh.Promotion.Decision.DecidedAt.Equal(decidedAt) {
		t.Fatal("the recorded decision must never be rewritten (idempotence)")
	}
	if auditer.count() != 1 {
		t.Fatalf("re-triggers must not audit again, got %d", auditer.count())
	}
	if calls := source.callCount(); calls < 1 || calls > 8 {
		t.Fatalf("sample source consulted at most once per trigger (1-8), got %d", calls)
	}
}

func TestCanaryManualActionRespected(t *testing.T) {
	t.Setenv(AutoPromoteEnvVar, "true")
	orgID, agentID := "org-1", "agent-a"

	// Manual abort in flight: the engine stands down (no decision, no audit).
	svc, dep, source, auditer := policyFixture(t, orgID, agentID, 30, &AgentPromotionPolicy{MinPassRate: 0.8, MinCanaryRuns: 1})
	source.samples = passingSamples(1, 10, 0, 5) // would roll back
	if _, err := svc.AbortCanaryCtx(context.Background(), orgID, dep.ID); err != nil {
		t.Fatalf("manual abort returned error: %v", err)
	}
	svc.OnEvalRunCompleted(context.Background(), orgID, agentID)
	fresh, _ := svc.GetDeploymentCtx(context.Background(), orgID, dep.ID)
	if fresh.Promotion.Decision != nil || auditer.count() != 0 {
		t.Fatalf("engine must respect the manual abort, got decision=%+v audits=%d", fresh.Promotion.Decision, auditer.count())
	}

	// Manual promote in flight: same contract.
	svc2, dep2, source2, auditer2 := policyFixture(t, orgID, agentID, 30, &AgentPromotionPolicy{MinPassRate: 0.8, MinCanaryRuns: 1})
	source2.samples = passingSamples(1, 10, 10, 5) // would promote
	if _, err := svc2.PromoteCanaryCtx(context.Background(), orgID, dep2.ID); err != nil {
		t.Fatalf("manual promote returned error: %v", err)
	}
	svc2.OnEvalRunCompleted(context.Background(), orgID, agentID)
	fresh2, _ := svc2.GetDeploymentCtx(context.Background(), orgID, dep2.ID)
	if fresh2.Promotion.Decision != nil || auditer2.count() != 0 {
		t.Fatalf("engine must respect the manual promote, got decision=%+v audits=%d", fresh2.Promotion.Decision, auditer2.count())
	}
}

func TestCanaryStickySplitUnaffectedByPolicy(t *testing.T) {
	t.Setenv(AutoPromoteEnvVar, "true")
	orgID, agentID := "org-1", "agent-a"
	ctx := context.Background()

	// A policy attached but still under its evidence gate must not perturb the
	// deterministic split (weight 100 -> always canary, weight 0 -> always
	// stable, same side on every resolution).
	svc, dep, source, _ := policyFixture(t, orgID, agentID, 100, &AgentPromotionPolicy{MinPassRate: 0.8, MinCanaryRuns: 99})
	source.samples = passingSamples(1, 10, 10, 5)
	for range 5 {
		if got, err := svc.ResolveVersionCtx(ctx, orgID, agentID, EnvironmentProduction); err != nil || got != 3 {
			t.Fatalf("weight 100 must keep resolving the canary v3, got %d/%v", got, err)
		}
	}
	if _, err := svc.SetCanaryWeightCtx(ctx, orgID, dep.ID, 0); err != nil {
		t.Fatalf("SetCanaryWeightCtx(0) returned error: %v", err)
	}
	for range 5 {
		if got, err := svc.ResolveVersionCtx(ctx, orgID, agentID, EnvironmentProduction); err != nil || got != 2 {
			t.Fatalf("weight 0 must keep resolving the stable v2, got %d/%v", got, err)
		}
	}
	svc.OnEvalRunCompleted(ctx, orgID, agentID) // under the gate: no transition
	if got, _ := svc.ResolveVersionCtx(ctx, orgID, agentID, EnvironmentProduction); got != 2 {
		t.Fatalf("a pending policy must not move the split, got v%d", got)
	}
	// Engine decisions move the split only through the SAME promote/abort
	// primitives as manual actions — never through the selection function.
	if canaryBucket(orgID, agentID) != canaryBucket(orgID, agentID) {
		t.Fatal("bucket must be pure")
	}
}

func TestCanaryPromotionWindowExcludesStaleRuns(t *testing.T) {
	t.Setenv(AutoPromoteEnvVar, "true")
	svc, dep, source, _ := policyFixture(t, "org-1", "agent-a", 10, &AgentPromotionPolicy{MinPassRate: 0.8, MinCanaryRuns: 2})
	windowStart := dep.Promotion.WindowStart
	source.samples = []EvalSample{
		// One run from BEFORE the window (previous canary evaluation) and one
		// from inside: only the in-window run counts -> gate stays shut.
		{RunID: "run-old", CreatedAt: windowStart.Add(-time.Hour), Cases: 10, Passed: 10, LatenciesMS: []float64{1}},
		{RunID: "run-new", CreatedAt: windowStart.Add(time.Minute), Cases: 10, Passed: 10, LatenciesMS: []float64{1}},
	}
	svc.OnEvalRunCompleted(context.Background(), "org-1", "agent-a")
	fresh, _ := svc.GetDeploymentCtx(context.Background(), "org-1", dep.ID)
	if fresh.Promotion.Decision != nil {
		t.Fatalf("stale runs must not satisfy the evidence gate, got %+v", fresh.Promotion.Decision)
	}
	// A second in-window run completes the evidence -> decision.
	source.samples = append(source.samples, EvalSample{RunID: "run-new-2", CreatedAt: windowStart.Add(2 * time.Minute), Cases: 10, Passed: 10, LatenciesMS: []float64{1}})
	svc.OnEvalRunCompleted(context.Background(), "org-1", "agent-a")
	fresh, _ = svc.GetDeploymentCtx(context.Background(), "org-1", dep.ID)
	if fresh.Promotion.Decision == nil || fresh.Promotion.Decision.RunsCounted != 2 {
		t.Fatalf("two in-window runs should decide with runs_counted=2, got %+v", fresh.Promotion.Decision)
	}
}

func TestCanaryPolicyLifecycleTransitions(t *testing.T) {
	t.Setenv(AutoPromoteEnvVar, "true")
	orgID, agentID := "org-1", "agent-a"
	ctx := context.Background()

	// Validation: out-of-domain values are rejected, not clamped.
	svc, dep, _, _ := policyFixture(t, orgID, agentID, 10, nil)
	for _, policy := range []*AgentPromotionPolicy{
		{MinPassRate: 1.5, MinCanaryRuns: 1},
		{MinPassRate: -0.1, MinCanaryRuns: 1},
		{MinPassRate: 0.8, MinCanaryRuns: -1},
		{MinPassRate: 0.8, MinCanaryRuns: 1, MaxP95LatencyMs: -5},
		{MinPassRate: 0.8, MinCanaryRuns: 1, MaxCostPerRunCents: -5},
	} {
		if _, err := svc.SetCanaryPromotionPolicyCtx(ctx, orgID, dep.ID, policy); !errors.Is(err, ErrInvalidCanaryPolicy) {
			t.Fatalf("invalid policy %+v should be ErrInvalidCanaryPolicy, got %v", policy, err)
		}
	}
	// A policy without a canary is a state conflict.
	plain := deployAndPromote(t, svc, orgID, agentID, 2, EnvironmentStaging)
	if _, err := svc.SetCanaryPromotionPolicyCtx(ctx, orgID, plain.ID, &AgentPromotionPolicy{MinPassRate: 0.8, MinCanaryRuns: 1}); !errors.Is(err, ErrNoCanary) {
		t.Fatalf("policy without canary should be ErrNoCanary, got %v", err)
	}

	// Attach on the healthy canary row: window opens at attach.
	policy := &AgentPromotionPolicy{MinPassRate: 0.8, MinCanaryRuns: 1}
	attached, err := svc.SetCanaryPromotionPolicyCtx(ctx, orgID, dep.ID, policy)
	if err != nil {
		t.Fatalf("attach policy returned error: %v", err)
	}
	if attached.Promotion == nil || attached.Promotion.WindowStart.IsZero() || attached.Promotion.Decision != nil {
		t.Fatalf("attach should open a fresh undecided window, got %+v", attached.Promotion)
	}

	// Deciding, then re-attaching the canary: fresh window, decision void.
	source := &fakeSampleSource{samples: passingSamples(1, 10, 0, 1)}
	svc.SetCanarySampleSource(source)
	svc.OnEvalRunCompleted(ctx, orgID, agentID)
	decided, _ := svc.GetDeploymentCtx(ctx, orgID, dep.ID)
	if decided.Promotion.Decision == nil {
		t.Fatal("fixture should have decided")
	}
	oldWindow := decided.Promotion.WindowStart // time COPY: the struct mutates in place
	reattached, err := svc.SetCanaryVersionCtx(ctx, orgID, dep.ID, 3)
	if err != nil {
		t.Fatalf("re-attach returned error: %v", err)
	}
	if reattached.Promotion.Decision != nil || !reattached.Promotion.WindowStart.After(oldWindow) {
		t.Fatalf("re-attach must open a fresh window and void the decision, got %+v", reattached.Promotion)
	}

	// Demote: the promotion state belongs to the ended window and is cleared.
	newer := deployAndPromote(t, svc, orgID, agentID, 3, EnvironmentProduction)
	demoted, _ := svc.GetDeploymentCtx(ctx, orgID, dep.ID)
	if demoted.Status != StatusFailed || demoted.Promotion != nil {
		t.Fatalf("demoted row must lose the promotion state, got %+v", demoted)
	}
	if newer.Status != StatusHealthy {
		t.Fatalf("newer row should be healthy, got %q", newer.Status)
	}
}

func TestCanaryStatusCtx(t *testing.T) {
	t.Setenv(AutoPromoteEnvVar, "true")
	orgID, agentID := "org-1", "agent-a"
	ctx := context.Background()

	svc, dep, source, _ := policyFixture(t, orgID, agentID, 35, &AgentPromotionPolicy{MinPassRate: 0.8, MinCanaryRuns: 3, MaxP95LatencyMs: 500})
	source.samples = passingSamples(2, 10, 8, 40) // 0.8 pass rate, p95 40ms

	status, err := svc.CanaryStatusCtx(ctx, orgID, agentID, EnvironmentProduction)
	if err != nil {
		t.Fatalf("CanaryStatusCtx returned error: %v", err)
	}
	if !status.CanaryActive || status.CanaryVersion != 3 || status.CanaryWeight != 35 || status.StableVersion != 2 {
		t.Fatalf("unexpected split view: %+v", status)
	}
	if status.DeploymentID != dep.ID {
		t.Fatalf("status should reference the healthy row, got %q", status.DeploymentID)
	}
	if status.Policy == nil || status.Policy.MinPassRate != 0.8 || status.Decision != nil {
		t.Fatalf("unexpected policy/decision view: %+v/%+v", status.Policy, status.Decision)
	}
	if status.Stats == nil || status.Stats.RunsCounted != 2 || status.Stats.PassRate != 0.8 || status.Stats.P95LatencyMS != 40 || status.Stats.AvgCostCents != 5 {
		t.Fatalf("unexpected sample stats: %+v", status.Stats)
	}

	// Environment scoping: unknown env is a validation error, an env without a
	// healthy deployment is not-found, and a foreign tenant sees nothing.
	if _, err := svc.CanaryStatusCtx(ctx, orgID, agentID, "chaos"); !errors.Is(err, ErrInvalidEnvironment) {
		t.Fatalf("unknown env should be ErrInvalidEnvironment, got %v", err)
	}
	if _, err := svc.CanaryStatusCtx(ctx, orgID, agentID, EnvironmentStaging); !errors.Is(err, ErrNoServingDeployment) {
		t.Fatalf("missing env should be ErrNoServingDeployment, got %v", err)
	}
	if _, err := svc.CanaryStatusCtx(ctx, "org-other", agentID, EnvironmentProduction); !errors.Is(err, ErrNoServingDeployment) {
		t.Fatalf("foreign tenant should be ErrNoServingDeployment, got %v", err)
	}

	// After a decision the status keeps surfacing it (last decision + reason).
	source.samples = passingSamples(3, 10, 0, 5) // 0.0 < 0.8 -> rollback
	svc.SetCanaryDecisionAuditer(nil)            // silent
	svc.OnEvalRunCompleted(ctx, orgID, agentID)
	status, err = svc.CanaryStatusCtx(ctx, orgID, agentID, EnvironmentProduction)
	if err != nil {
		t.Fatalf("CanaryStatusCtx returned error: %v", err)
	}
	if status.Decision == nil || status.Decision.Action != CanaryDecisionRollback || status.Decision.Reason != "pass_rate 0.00 < 0.80 → rollback" {
		t.Fatalf("status should surface the last decision, got %+v", status.Decision)
	}
	if status.CanaryActive || status.CanaryVersion != 0 {
		t.Fatalf("post-rollback the canary is gone, got %+v", status)
	}
}

func TestCanaryEngineInertWithoutCollaborators(t *testing.T) {
	t.Setenv(AutoPromoteEnvVar, "true")
	// No sample source wired: the engine must be a no-op (and never panic).
	svc, dep, _, _ := policyFixture(t, "org-1", "agent-a", 10, &AgentPromotionPolicy{MinPassRate: 0.8, MinCanaryRuns: 1})
	svc.SetCanarySampleSource(nil)
	svc.OnEvalRunCompleted(context.Background(), "org-1", "agent-a")
	fresh, _ := svc.GetDeploymentCtx(context.Background(), "org-1", dep.ID)
	if fresh.Promotion.Decision != nil {
		t.Fatalf("no source -> no decision, got %+v", fresh.Promotion.Decision)
	}
	// A disabled policy (MinCanaryRuns < 1) never decides even with samples.
	svc2, dep2, source2, _ := policyFixture(t, "org-1", "agent-a", 10, &AgentPromotionPolicy{MinPassRate: 0.8, MinCanaryRuns: 0})
	source2.samples = passingSamples(5, 10, 0, 5)
	svc2.OnEvalRunCompleted(context.Background(), "org-1", "agent-a")
	fresh2, _ := svc2.GetDeploymentCtx(context.Background(), "org-1", dep2.ID)
	if fresh2.Promotion.Decision != nil || fresh2.CanaryVersion != 3 {
		t.Fatalf("disabled policy must never decide, got %+v", fresh2.Promotion)
	}
}
