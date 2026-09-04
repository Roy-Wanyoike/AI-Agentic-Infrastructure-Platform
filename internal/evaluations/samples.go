package evaluations

// Eval-run samples + the completion observer seam for the eval-gated canary
// promotion engine (issue #51). The evaluations package stays decoupled from
// internal/deployments: it only (a) exposes compact completed-run aggregates
// (ListRunSamples — consumed by the wiring's EvalSampleSource adapter) and
// (b) fires a completion observer after every completed eval run (wired to
// deployments.Service.OnEvalRunCompleted). Both additions are read-only with
// respect to eval state: a sampling failure or a missing observer never
// changes any run outcome.

import (
	"context"
	"sort"
	"time"
)

// sampleLimitCap bounds ListRunSamples' limit so one caller cannot ask the
// store to stream the whole eval history.
const sampleLimitCap = 500

// RunSample is one completed eval run reduced to the aggregates the canary
// promotion policy consumes: case counts for the case-weighted pass rate,
// per-case latencies for the p95, and the run's total cost. CreatedAt is the
// run's creation time — the canary window anchor (a run belongs to the
// window it was started in).
type RunSample struct {
	RunID       string
	CreatedAt   time.Time
	Cases       int
	Passed      int
	LatenciesMS []float64
	CostCents   float64
}

// SetCompletionObserver wires a callback invoked after every COMPLETED eval
// run (RunDataset only ever exits runs as completed; per-case failures are
// results, not run failures). Exactly-once per run, fired asynchronously on
// a detached context (WithoutCancel: the request context dies with the HTTP
// handler, the observer must not) — observers must be cheap and must never
// mutate the run. A nil observer disables the seam (default). Call before
// serving or between runs (guarded by the service mutex).
func (s *Service) SetCompletionObserver(fn func(ctx context.Context, run *EvalRun)) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completionObserver = fn
}

// notifyCompletion invokes the completion observer exactly once per completed
// run, off the caller's stack (the eval-run request path must not block on
// observer work) and detached from the request context.
func (s *Service) notifyCompletion(ctx context.Context, run *EvalRun) {
	if s == nil || run == nil {
		return
	}
	s.mu.Lock()
	observer := s.completionObserver
	s.mu.Unlock()
	if observer == nil {
		return
	}
	go observer(context.WithoutCancel(ctx), run)
}

// ListRunSamples returns the tenant's most recent completed eval runs for one
// agent (newest first, capped by limit) as compact aggregates. Tenant guard:
// every query is scoped by organization_id; foreign or unknown agents
// surface as an empty slice (never another tenant's runs).
func (s *Service) ListRunSamples(ctx context.Context, orgID, agentID string, limit int) ([]RunSample, error) {
	if limit <= 0 {
		return []RunSample{}, nil
	}
	if limit > sampleLimitCap {
		limit = sampleLimitCap
	}
	if s.store != nil {
		runs, err := s.store.ListCompletedRuns(ctx, orgID, agentID, limit)
		if err != nil {
			return nil, err
		}
		samples := make([]RunSample, 0, len(runs))
		for _, run := range runs {
			results, err := s.store.ListResults(ctx, orgID, run.ID)
			if err != nil {
				return nil, err
			}
			samples = append(samples, runSampleFromResults(run, results))
		}
		return samples, nil
	}
	s.mu.Lock()
	matches := make([]*EvalRun, 0)
	for _, run := range s.runs {
		if run.OrganizationID == orgID && run.AgentID == agentID && run.Status == StatusCompleted {
			matches = append(matches, run)
		}
	}
	s.mu.Unlock()
	sort.Slice(matches, func(i, j int) bool {
		if !matches[i].CreatedAt.Equal(matches[j].CreatedAt) {
			return matches[i].CreatedAt.After(matches[j].CreatedAt)
		}
		return matches[i].ID > matches[j].ID
	})
	if len(matches) > limit {
		matches = matches[:limit]
	}
	samples := make([]RunSample, 0, len(matches))
	for _, run := range matches {
		samples = append(samples, runSampleFromResults(run, run.Results))
	}
	return samples, nil
}

// runSampleFromResults reduces one run + its results into a RunSample.
func runSampleFromResults(run *EvalRun, results []Result) RunSample {
	sample := RunSample{RunID: run.ID, CreatedAt: run.CreatedAt}
	for _, r := range results {
		sample.Cases++
		if r.Passed {
			sample.Passed++
		}
		sample.LatenciesMS = append(sample.LatenciesMS, r.LatencyMS)
		sample.CostCents += r.CostCents
	}
	return sample
}
