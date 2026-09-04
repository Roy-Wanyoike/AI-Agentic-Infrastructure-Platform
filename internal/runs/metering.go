package runs

import (
        "context"
        "database/sql"
        "errors"
        "time"
)

// ---------------------------------------------------------------------------
// Step-count metering (issue #57).
//
// The usage-meter surface (GET /v1/usage/meters, internal/billing) needs one
// durable number the cost aggregates do not carry: how many TOOL steps were
// executed for a tenant inside a window. Tool executions ARE recorded today —
// runtime.StepTypeTool writes run_steps rows with step_type='tool' — but no
// aggregate existed, so this file adds one. It is strictly additive: the
// Store interface is untouched (the pgStore gains an extra method picked up
// through an optional interface type-assertion) and the in-memory service
// computes the same number from its step cache, mirroring the dual-mode
// pattern of AggregateCostsCtx in cost.go.
//
// Sandbox seconds are deliberately NOT metered anywhere in this platform yet
// (no durable record of sandbox execution duration exists), so no meter can
// be derived here — see the documented note in internal/billing/meters.go.
// ---------------------------------------------------------------------------

// StepTypeTool mirrors runtime.StepTypeTool ("tool"); StepTypeModel mirrors
// runtime.StepTypeModel ("model"). Declared here so the metering/billing side
// can filter by step type without importing runtime (runs must not depend on
// the execution runtime).
const (
        StepTypeTool  = "tool"
        StepTypeModel = "model"
)

// Tenant guard: the count joins run_steps to runs so the organization_id
// filter is authoritative, and aggregates over the half-open
// [from, to) created_at window exactly like the cost aggregates in cost.go.
const sqlCountStepsByType = `SELECT COUNT(*) FROM run_steps rs JOIN runs r ON r.id = rs.run_id WHERE r.organization_id = $1 AND rs.step_type = $2 AND rs.created_at >= $3 AND rs.created_at < $4`

// stepCountStore is the OPTIONAL Store capability behind
// Service.AggregateStepCountsCtx. Declared unexported so the runs.Store
// interface (and every existing mock/fake implementing it) stays unchanged;
// stores that implement the method are picked up by type assertion.
type stepCountStore interface {
        AggregateStepCounts(ctx context.Context, orgID, stepType string, from, to time.Time) (int64, error)
}

// AggregateStepCounts counts the run_steps rows of one step type for one
// tenant over the half-open [from, to) created_at window (issue #57 metering).
func (s *pgStore) AggregateStepCounts(ctx context.Context, orgID, stepType string, from, to time.Time) (int64, error) {
        if err := s.guard(); err != nil {
                return 0, err
        }
        var count int64
        // Tenant guard: WHERE r.organization_id = $1 (see sqlCountStepsByType).
        if err := s.db.QueryRowContext(ctx, sqlCountStepsByType, orgID, stepType, from, to).Scan(&count); err != nil {
                if errors.Is(err, sql.ErrNoRows) {
                        return 0, nil
                }
                return 0, err
        }
        return count, nil
}

// AggregateStepCountsCtx counts the recorded steps of the given step type
// (e.g. StepTypeTool) for one tenant over the half-open [from, to) window.
// Store mode delegates to the optional store capability; without a store the
// in-memory step cache is aggregated (zero-infrastructure parity with the
// in-memory cost aggregation).
func (s *Service) AggregateStepCountsCtx(ctx context.Context, orgID, stepType string, from, to time.Time) (int64, error) {
        if s == nil {
                return 0, errors.New("runs: service is nil")
        }
        if err := validateMeterWindow(orgID, stepType, from, to); err != nil {
                return 0, err
        }
        s.mu.Lock()
        if s.store != nil {
                sc, ok := s.store.(stepCountStore)
                // Release the service mutex: the store path never touches the maps.
                s.mu.Unlock()
                if !ok {
                        // A store is wired but cannot aggregate step counts: report
                        // honestly instead of answering 0 (a fabricated zero would
                        // understate metered usage, the one number billing must never
                        // fake).
                        return 0, errors.New("runs: store does not support step-count aggregation")
                }
                return sc.AggregateStepCounts(ctx, orgID, stepType, from, to)
        }
        defer s.mu.Unlock()
        var count int64
        for runID, steps := range s.steps {
                run, ok := s.runs[runID]
                if !ok || run.OrganizationID != orgID {
                        continue
                }
                for _, step := range steps {
                        if step.StepType != stepType {
                                continue
                        }
                        if step.CreatedAt.Before(from) || !step.CreatedAt.Before(to) {
                                continue
                        }
                        count++
                }
        }
        return count, nil
}

// validateMeterWindow mirrors the input rules of AggregateCostsCtx: a
// tenant id and step type are required and the window must be a non-empty
// half-open interval.
func validateMeterWindow(orgID, stepType string, from, to time.Time) error {
        if orgID == "" {
                return errors.New("organization id is required")
        }
        if stepType == "" {
                return errors.New("step type is required")
        }
        if from.IsZero() || to.IsZero() {
                return errors.New("from and to are required")
        }
        if !to.After(from) {
                return errors.New("to must be after from")
        }
        return nil
}
