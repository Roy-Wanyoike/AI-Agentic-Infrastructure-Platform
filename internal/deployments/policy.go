package deployments

// Eval-gated canary promotion with automatic rollback (issue #51) — the
// "CI/CD for agents" loop that wires the canary traffic split (issue #13) to
// the evaluation runner (issue #14):
//
//      attach a canary + an AgentPromotionPolicy on the healthy deployment row
//      evals run against the agent (evaluations.Service.RunDataset)
//      every completed eval run triggers OnEvalRunCompleted (cheap, non-blocking)
//      once >= MinCanaryRuns completed eval runs exist inside the canary window,
//      the policy decides: PROMOTE (canary becomes stable, canary->100%) or
//      ROLLBACK (revert to baseline: canary cleared, stable keeps serving)
//      the decision (action + reason + sample stats) is recorded ONCE on the row
//      and an audit event is emitted; manual promote/abort stay authoritative —
//      once either lands, the engine stands down (no flapping)
//
// The whole feature is gated behind AGENTOS_CANARY_AUTOPROMOTE (default OFF =
// byte-identical current behavior; see AutoPromotionFromEnv).

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// AutoPromoteEnvVar gates the automatic canary promotion/rollback engine
// (issue #51): AGENTOS_CANARY_AUTOPROMOTE. Default OFF — the flag is parsed
// with strconv.ParseBool so "1"/"t"/"T"/"true"/"TRUE"/"True" enable it and
// "0"/"f"/"F"/"false"/"FALSE"/"False" (or unset/empty) keep it off; any other
// value is treated as off (misconfiguration never silently enables automatic
// traffic-changing decisions, mirroring internal/billing/enforcement.go).
//
// The flag is read at decision time (not process start) so tests can flip it
// with t.Setenv and operators can rotate it on restart.
const AutoPromoteEnvVar = "AGENTOS_CANARY_AUTOPROMOTE"

// AutoPromotionFromEnv reports whether eval-gated canary autopromotion is
// enabled via AutoPromoteEnvVar (default OFF — see the comment above).
func AutoPromotionFromEnv() bool {
	raw, ok := os.LookupEnv(AutoPromoteEnvVar)
	if !ok || strings.TrimSpace(raw) == "" {
		return false
	}
	on, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	return on
}

// Decision actions recorded on the deployment row.
const (
	CanaryDecisionPromote  = "promote"
	CanaryDecisionRollback = "rollback"
)

// MaxCanarySampleRuns caps how many completed eval runs one decision may
// consider (newest first). A bound keeps the engine cheap no matter how long
// a canary window stays open.
const MaxCanarySampleRuns = 100

// AgentPromotionPolicy is the eval-gated promotion policy attached to one
// canary (persisted inside the deployment row's canary_promotion JSONB).
type AgentPromotionPolicy struct {
	// MinPassRate is the minimum case-weighted pass rate across the sampled
	// eval runs (0.0-1.0) required to promote. A sample below it rolls back.
	MinPassRate float64 `json:"min_pass_rate"`
	// MinCanaryRuns is the minimum number of completed eval runs required
	// before ANY decision is made (the evidence gate). Values < 1 disable the
	// policy entirely — the engine never decides on an empty sample.
	MinCanaryRuns int `json:"min_canary_runs"`
	// MaxP95LatencyMs is the maximum tolerated p95 case latency (ms) across
	// the sampled runs; 0 disables the latency gate.
	MaxP95LatencyMs int64 `json:"max_p95_latency_ms"`
	// MaxCostPerRunCents is the maximum tolerated average cost per eval run
	// (cents); 0 disables the cost gate.
	MaxCostPerRunCents int64 `json:"max_cost_per_run_cents"`
}

// ErrInvalidCanaryPolicy is returned when a policy carries out-of-domain
// values (invalid input is REJECTED, not clamped — same contract as
// ErrInvalidCanaryWeight).
var ErrInvalidCanaryPolicy = errors.New("canary_policy: min_pass_rate must be within [0,1] and every threshold must be non-negative (0 disables a gate)")

// Validate rejects out-of-domain policy values.
func (p *AgentPromotionPolicy) Validate() error {
	if p == nil {
		return nil
	}
	if p.MinPassRate < 0 || p.MinPassRate > 1 || p.MinCanaryRuns < 0 ||
		p.MaxP95LatencyMs < 0 || p.MaxCostPerRunCents < 0 {
		return ErrInvalidCanaryPolicy
	}
	return nil
}

// enabled reports whether the policy can ever produce a decision. A policy
// without at least one required run is inert (deciding on an empty sample
// would instantly promote/rollback on no evidence).
func (p *AgentPromotionPolicy) enabled() bool {
	return p != nil && p.MinCanaryRuns >= 1
}

// CanaryDecision is the immutable record of one automatic policy decision,
// persisted on the deployment row (idempotence: recorded exactly once per
// canary window) and surfaced by the status endpoint.
type CanaryDecision struct {
	Action       string               `json:"action"` // promote | rollback
	Reason       string               `json:"reason"` // e.g. "pass_rate 0.62 < 0.80 → rollback"
	DecidedAt    time.Time            `json:"decided_at"`
	RunsCounted  int                  `json:"runs_counted"`
	PassRate     float64              `json:"pass_rate"`
	P95LatencyMS int64                `json:"p95_latency_ms"`
	AvgCostCents float64              `json:"avg_cost_cents"`
	Policy       AgentPromotionPolicy `json:"policy"` // policy in effect at decision time
}

// CanaryPromotion is the persisted eval-gated promotion state of one canary
// (deployment.canary_promotion JSONB; nil on the Deployment = no policy
// configured). WindowStart is stamped when the canary is attached/replaced so
// eval runs from a PREVIOUS window never count toward this one; Decision is
// nil until the engine records its single decision for the window.
type CanaryPromotion struct {
	Policy      AgentPromotionPolicy `json:"policy"`
	WindowStart time.Time            `json:"window_start,omitempty"`
	Decision    *CanaryDecision      `json:"decision,omitempty"`
}

// EvalSample is one completed eval run's contribution to the canary sample
// (case-weighted aggregates: per-case latencies for the p95, total cost for
// the per-run average). The deployments package defines this narrow shape so
// it never imports internal/evaluations — wiring adapts via
// EvalSampleSourceFunc.
type EvalSample struct {
	RunID       string
	CreatedAt   time.Time
	Cases       int
	Passed      int
	LatenciesMS []float64
	CostCents   float64
}

// EvalSampleSource returns the tenant's most recent completed eval runs for
// one agent, newest first, up to limit. Implementations MUST scope the query
// by organization_id.
type EvalSampleSource interface {
	CanaryEvalSamples(ctx context.Context, orgID, agentID string, limit int) ([]EvalSample, error)
}

// EvalSampleSourceFunc adapts a plain function to EvalSampleSource.
type EvalSampleSourceFunc func(ctx context.Context, orgID, agentID string, limit int) ([]EvalSample, error)

// CanaryEvalSamples implements EvalSampleSource.
func (f EvalSampleSourceFunc) CanaryEvalSamples(ctx context.Context, orgID, agentID string, limit int) ([]EvalSample, error) {
	return f(ctx, orgID, agentID, limit)
}

// CanaryDecisionAuditer receives one audit event per automatic decision
// (wired to the audit service; best-effort — audit failures never block a
// decision).
type CanaryDecisionAuditer interface {
	AuditCanaryDecision(ctx context.Context, orgID string, deployment *Deployment, decision *CanaryDecision)
}

// CanaryDecisionAuditerFunc adapts a plain function to CanaryDecisionAuditer.
type CanaryDecisionAuditerFunc func(ctx context.Context, orgID string, deployment *Deployment, decision *CanaryDecision)

// AuditCanaryDecision implements CanaryDecisionAuditer.
func (f CanaryDecisionAuditerFunc) AuditCanaryDecision(ctx context.Context, orgID string, deployment *Deployment, decision *CanaryDecision) {
	f(ctx, orgID, deployment, decision)
}

// CanarySampleStats aggregates the in-window sample the policy decided on
// (surfaced by the status endpoint: runs counted, pass rate, p95, avg cost).
type CanarySampleStats struct {
	RunsCounted  int     `json:"runs_counted"`
	CasesCounted int     `json:"cases_counted"`
	PassedCases  int     `json:"passed_cases"`
	PassRate     float64 `json:"pass_rate"`
	P95LatencyMS int64   `json:"p95_latency_ms"`
	AvgCostCents float64 `json:"avg_cost_cents"`
}

// CanaryStatus is the read model served by GET /agents/{agentId}/canary/status.
type CanaryStatus struct {
	AgentID       string                `json:"agent_id"`
	Environment   string                `json:"environment"`
	DeploymentID  string                `json:"deployment_id"`
	StableVersion int                   `json:"stable_version"`
	CanaryVersion int                   `json:"canary_version"`
	CanaryWeight  int                   `json:"canary_weight"` // current split %
	CanaryActive  bool                  `json:"canary_active"`
	WindowStart   *time.Time            `json:"window_start,omitempty"`
	Policy        *AgentPromotionPolicy `json:"policy,omitempty"`
	Decision      *CanaryDecision       `json:"decision,omitempty"`
	Stats         *CanarySampleStats    `json:"stats,omitempty"`
}

// SetCanarySampleSource wires the eval sample source (nil disables sampling;
// call before serving, like runtime.WithMetrics wiring).
func (s *Service) SetCanarySampleSource(src EvalSampleSource) {
	if s == nil {
		return
	}
	s.samples = src
}

// SetCanaryDecisionAuditer wires the decision audit sink (nil disables the
// audit event; call before serving).
func (s *Service) SetCanaryDecisionAuditer(a CanaryDecisionAuditer) {
	if s == nil {
		return
	}
	s.auditer = a
}

// SetCanaryPromotionPolicyCtx attaches (nil clears) the eval-gated promotion
// policy on a deployment. Setting a policy opens a FRESH evaluation window
// (WindowStart = now, any prior decision cleared). Rejected when the
// deployment does not exist, the policy is invalid, or a non-nil policy
// targets a canary-less row (ErrNoCanary).
func (s *Service) SetCanaryPromotionPolicyCtx(ctx context.Context, orgID, depID string, policy *AgentPromotionPolicy) (*Deployment, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	deployment, err := s.GetDeploymentCtx(ctx, orgID, depID)
	if err != nil {
		return nil, err
	}
	if policy != nil && !deployment.HasCanary() {
		return nil, ErrNoCanary
	}
	now := time.Now().UTC()
	s.mu.Lock()
	if policy == nil {
		deployment.Promotion = nil
	} else {
		deployment.Promotion = &CanaryPromotion{Policy: *policy, WindowStart: now}
	}
	deployment.UpdatedAt = now
	err = s.persistUpdate(ctx, orgID, deployment)
	s.mu.Unlock()
	return deployment, err
}

// OnEvalRunCompleted is the seam the evaluations completion observer calls
// after every completed eval run (wired by WireCanaryAutoPromotion in
// cmd/api; already off the request path). Cheap and non-blocking by
// contract: with the flag off (the default) it returns immediately after one
// env read; it never returns an error and never fails the eval run that
// triggered it.
func (s *Service) OnEvalRunCompleted(ctx context.Context, orgID, agentID string) {
	if s == nil || orgID == "" || agentID == "" {
		return
	}
	// Gate 1: the feature flag (default OFF -> byte-identical legacy behavior).
	if !AutoPromotionFromEnv() {
		return
	}
	// Gate 2: a sample source must be wired (otherwise no evidence exists).
	if s.samples == nil {
		return
	}
	rows, err := s.ListDeploymentsCtx(ctx, orgID, agentID)
	if err != nil {
		return
	}
	for _, dep := range rows {
		s.decideCanary(ctx, orgID, dep)
	}
}

// canaryDecisionPending reports whether the deployment row is a decision
// candidate: healthy, canary active, policy attached + enabled, and no
// decision recorded yet (idempotence: one decision per canary window).
// Callers that share the row with concurrent writers must hold s.mu.
func canaryDecisionPending(d *Deployment) bool {
	return d != nil && d.Status == StatusHealthy && d.HasCanary() &&
		d.Promotion != nil && d.Promotion.Decision == nil && d.Promotion.Policy.enabled()
}

// decideCanary runs the decision pipeline for one candidate row: sample ->
// aggregate -> evaluate -> record once -> apply -> audit. All shared-state
// reads and writes happen under the service mutex; the (potentially slower)
// sample fetch and audit write happen outside it. The candidate row is the
// in-memory shared pointer in memory mode and a store copy in store mode —
// either way the record+apply below is serialized against manual
// promote/abort (which hold the same mutex for their mutations).
func (s *Service) decideCanary(ctx context.Context, orgID string, candidate *Deployment) {
	// Snapshot the gate inputs under the mutex.
	s.mu.Lock()
	if !canaryDecisionPending(candidate) {
		s.mu.Unlock()
		return
	}
	policy := candidate.Promotion.Policy
	windowStart := candidate.Promotion.WindowStart
	s.mu.Unlock()

	samples, err := s.samples.CanaryEvalSamples(ctx, orgID, candidate.AgentID, MaxCanarySampleRuns)
	if err != nil {
		// Sampling is best-effort: skip this trigger, the next completed eval
		// run re-fires the engine.
		return
	}
	samples = inWindow(samples, windowStart)
	stats := aggregateCanarySamples(samples)
	// Evidence gate: not enough completed eval runs yet -> no decision, no
	// record; the split keeps serving and the next run re-fires.
	if stats.RunsCounted < policy.MinCanaryRuns {
		return
	}
	decision := evaluateCanaryPolicy(policy, stats)

	// Record once, then apply — re-validated under the mutex so a manual
	// action (or a concurrent engine run) that landed during the sample fetch
	// wins and the engine stands down (no flapping).
	s.mu.Lock()
	if !canaryDecisionPending(candidate) || candidate.Promotion.WindowStart != windowStart {
		s.mu.Unlock()
		return
	}
	candidate.Promotion.Decision = decision
	candidate.UpdatedAt = time.Now().UTC()
	if err := s.persistUpdate(ctx, orgID, candidate); err != nil {
		candidate.Promotion.Decision = nil // not durable -> the window stays open
		s.mu.Unlock()
		return
	}
	var applyErr error
	if decision.Action == CanaryDecisionPromote {
		_, applyErr = s.applyCanaryPromoteLocked(ctx, orgID, candidate)
	} else {
		// Rollback reverts to the baseline: the canary config is cleared and
		// the stable version keeps serving 100% (abort semantics).
		_, applyErr = s.applyCanaryAbortLocked(ctx, orgID, candidate)
	}
	s.mu.Unlock()
	if applyErr != nil {
		// The manual action raced in after the record: the decision stays
		// recorded (never rewritten) but the transition was refused — no flap.
		return
	}
	if s.auditer != nil {
		s.auditer.AuditCanaryDecision(ctx, orgID, candidate, decision)
	}
}

// inWindow keeps only samples created at/after the canary window start (a
// zero WindowStart admits the whole history).
func inWindow(samples []EvalSample, windowStart time.Time) []EvalSample {
	if windowStart.IsZero() {
		return samples
	}
	out := make([]EvalSample, 0, len(samples))
	for _, sample := range samples {
		if !sample.CreatedAt.Before(windowStart) {
			out = append(out, sample)
		}
	}
	return out
}

// aggregateCanarySamples reduces the sample set: case-weighted pass rate,
// nearest-rank p95 over ALL case latencies, mean cost per run.
func aggregateCanarySamples(samples []EvalSample) *CanarySampleStats {
	stats := &CanarySampleStats{RunsCounted: len(samples)}
	var latencies []float64
	var totalCost float64
	for _, sample := range samples {
		stats.CasesCounted += sample.Cases
		stats.PassedCases += sample.Passed
		latencies = append(latencies, sample.LatenciesMS...)
		totalCost += sample.CostCents
	}
	if stats.CasesCounted > 0 {
		stats.PassRate = float64(stats.PassedCases) / float64(stats.CasesCounted)
	}
	stats.P95LatencyMS = percentile95(latencies)
	if len(samples) > 0 {
		stats.AvgCostCents = totalCost / float64(len(samples))
	}
	return stats
}

// percentile95 returns the nearest-rank 95th percentile of the latencies
// (0 for an empty slice).
func percentile95(latencies []float64) int64 {
	if len(latencies) == 0 {
		return 0
	}
	sorted := make([]float64, len(latencies))
	copy(sorted, latencies)
	sort.Float64s(sorted)
	idx := int(math.Ceil(0.95*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return int64(math.Round(sorted[idx]))
}

// evaluateCanaryPolicy applies the gates in a deterministic order (pass rate,
// then p95 latency, then cost) and returns the single decision. Every
// rollback reason follows the "<metric> <observed> <op> <threshold> →
// rollback" shape; promotion reads "pass_rate X >= Y → promote".
func evaluateCanaryPolicy(policy AgentPromotionPolicy, stats *CanarySampleStats) *CanaryDecision {
	action := CanaryDecisionPromote
	reason := fmt.Sprintf("pass_rate %.2f >= %.2f → promote", stats.PassRate, policy.MinPassRate)
	switch {
	case stats.PassRate < policy.MinPassRate:
		action = CanaryDecisionRollback
		reason = fmt.Sprintf("pass_rate %.2f < %.2f → rollback", stats.PassRate, policy.MinPassRate)
	case policy.MaxP95LatencyMs > 0 && stats.P95LatencyMS > policy.MaxP95LatencyMs:
		action = CanaryDecisionRollback
		reason = fmt.Sprintf("p95_latency_ms %d > %d → rollback", stats.P95LatencyMS, policy.MaxP95LatencyMs)
	case policy.MaxCostPerRunCents > 0 && int64(math.Round(stats.AvgCostCents)) > policy.MaxCostPerRunCents:
		action = CanaryDecisionRollback
		reason = fmt.Sprintf("avg_cost_per_run_cents %d > %d → rollback", int64(math.Round(stats.AvgCostCents)), policy.MaxCostPerRunCents)
	}
	return &CanaryDecision{
		Action:       action,
		Reason:       reason,
		DecidedAt:    time.Now().UTC(),
		RunsCounted:  stats.RunsCounted,
		PassRate:     stats.PassRate,
		P95LatencyMS: stats.P95LatencyMS,
		AvgCostCents: stats.AvgCostCents,
		Policy:       policy,
	}
}

// CanaryStatusCtx builds the status read model for one agent+environment:
// current split %, the policy in effect, the last decision (+reason), and —
// when a sample source is wired and the canary is active — fresh sample
// stats. ErrNoServingDeployment when no healthy row serves the pair;
// ErrInvalidEnvironment for unknown environments.
func (s *Service) CanaryStatusCtx(ctx context.Context, orgID, agentID, environment string) (*CanaryStatus, error) {
	if s == nil || orgID == "" || agentID == "" {
		return nil, ErrDeploymentNotFound
	}
	if !validEnvironment(environment) {
		return nil, ErrInvalidEnvironment
	}
	healthy, err := s.healthyDeployment(ctx, orgID, agentID, environment)
	if err != nil {
		return nil, err
	}
	if healthy == nil {
		return nil, ErrNoServingDeployment
	}
	status := &CanaryStatus{
		AgentID:       agentID,
		Environment:   environment,
		DeploymentID:  healthy.ID,
		StableVersion: healthy.Version,
		CanaryVersion: healthy.CanaryVersion,
		CanaryWeight:  healthy.CanaryWeight,
		CanaryActive:  healthy.HasCanary(),
	}
	if healthy.Promotion != nil {
		policy := healthy.Promotion.Policy
		status.Policy = &policy
		status.Decision = healthy.Promotion.Decision
		if !healthy.Promotion.WindowStart.IsZero() {
			windowStart := healthy.Promotion.WindowStart
			status.WindowStart = &windowStart
		}
	}
	// Fresh sample stats for an ACTIVE canary: the endpoint is the operator's
	// "why hasn't it promoted yet" view. Best-effort: a sampling failure
	// simply omits stats (never fails the read).
	if status.CanaryActive && s.samples != nil {
		if samples, err := s.samples.CanaryEvalSamples(ctx, orgID, agentID, MaxCanarySampleRuns); err == nil {
			stats := aggregateCanarySamples(inWindow(samples, healthy.Promotion.WindowStart))
			status.Stats = stats
		}
	}
	return status, nil
}
