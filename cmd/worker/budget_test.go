package main

// Tests for the worker-side budget enforcement gate (wave-8 issue C). The
// gate is store-agnostic (it only consumes billing.BudgetService), so the
// memory-mode suite below exercises every decision branch; the Postgres-mode
// EvaluateBudget SQL is pinned by internal/billing (budgets_test.go + the
// sqlmock-backed create-run gate test in cmd/api), and cmd/worker/main.go
// wires the gate to the same durable aggregates only when a DSN exists.

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"agentos/internal/audit"
	"agentos/internal/billing"
	"agentos/internal/runs"
)

// stubWorkerConsumption feeds deterministic budget consumption (the seam the
// production RunsConsumptionSource fills with per-agent cost aggregates).
type stubWorkerConsumption struct {
	rows []billing.ConsumptionRow
	err  error
}

func (s *stubWorkerConsumption) ConsumptionForPeriod(_ context.Context, _ string, _, _ time.Time) ([]billing.ConsumptionRow, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.rows, nil
}

func newWorkerBudgetGate(t *testing.T, consumption billing.ConsumptionSource, budgets ...billing.BudgetInput) *budgetEnforcer {
	t.Helper()
	svc := billing.NewBudgetService()
	if consumption != nil {
		svc.SetConsumptionSource(consumption)
	}
	for _, in := range budgets {
		if _, err := svc.CreateBudgetCtx(context.Background(), "org-1", in); err != nil {
			t.Fatalf("seed budget failed: %v", err)
		}
	}
	return &budgetEnforcer{
		budgets: svc,
		runs:    runs.NewService(),
		audit:   audit.NewService(),
		log:     slog.Default(),
	}
}

func TestBudgetEnforcerAllowMatrix(t *testing.T) {
	t.Run("nil enforcer allows (gate not wired)", func(t *testing.T) {
		var gate *budgetEnforcer
		if !gate.allow(context.Background(), "org-1", "run-1", "agent-1") {
			t.Fatal("nil gate must allow")
		}
	})
	t.Run("nil budget service allows", func(t *testing.T) {
		gate := &budgetEnforcer{}
		if !gate.allow(context.Background(), "org-1", "run-1", "agent-1") {
			t.Fatal("unwired gate must allow")
		}
	})
	t.Run("no budgets allows", func(t *testing.T) {
		gate := newWorkerBudgetGate(t, &stubWorkerConsumption{})
		if !gate.allow(context.Background(), "org-1", "run-1", "agent-1") {
			t.Fatal("org without budgets must execute")
		}
	})
	t.Run("consumption failure degrades to allow (already accepted work)", func(t *testing.T) {
		gate := newWorkerBudgetGate(t, &stubWorkerConsumption{err: errors.New("ledger unavailable")},
			billing.BudgetInput{Scope: billing.BudgetScopeOrganization, LimitRuns: 1, IsActive: true})
		if !gate.allow(context.Background(), "org-1", "run-1", "agent-1") {
			t.Fatal("metering failure must not destroy accepted work")
		}
	})
	t.Run("over-budget run denied with reason budget_exceeded", func(t *testing.T) {
		gate := newWorkerBudgetGate(t,
			&stubWorkerConsumption{rows: []billing.ConsumptionRow{{AgentID: "agent-1", Runs: 10}}},
			billing.BudgetInput{Scope: billing.BudgetScopeOrganization, LimitRuns: 10, IsActive: true})
		run, err := gate.runs.Create("org-1", "agent-1", "denied")
		if err != nil {
			t.Fatalf("seed run failed: %v", err)
		}

		if gate.allow(context.Background(), "org-1", run.ID, "agent-1") {
			t.Fatal("over-budget run must be denied")
		}
		// Run marked FAILED with the machine reason in the output slot.
		denied, _ := gate.runs.Get(run.ID)
		if denied.Status != runs.StatusFailed || denied.Output != billing.ReasonBudgetExceeded {
			t.Fatalf("run not failed with budget reason: %+v", denied)
		}
		// Denial audited with the machine reason + budget identity.
		entries := gate.audit.List()
		if len(entries) != 1 {
			t.Fatalf("audit rows = %d, want 1", len(entries))
		}
		if entries[0].Action != "run.budget_denied" || entries[0].OrganizationID != "org-1" ||
			entries[0].Metadata["reason"] != billing.ReasonBudgetExceeded {
			t.Fatalf("unexpected audit entry: %+v", entries[0])
		}
	})
	t.Run("within-budget run executes", func(t *testing.T) {
		gate := newWorkerBudgetGate(t,
			&stubWorkerConsumption{rows: []billing.ConsumptionRow{{AgentID: "agent-1", Runs: 2}}},
			billing.BudgetInput{Scope: billing.BudgetScopeAgent, RefID: "agent-1", LimitRuns: 10, IsActive: true})
		if !gate.allow(context.Background(), "org-1", "run-1", "agent-1") {
			t.Fatal("within-budget run must execute")
		}
		if len(gate.audit.List()) != 0 {
			t.Fatal("allowed runs must not be audited as denials")
		}
	})
	t.Run("agent budget does not hit other agents", func(t *testing.T) {
		gate := newWorkerBudgetGate(t,
			&stubWorkerConsumption{rows: []billing.ConsumptionRow{{AgentID: "agent-1", Runs: 99}}},
			billing.BudgetInput{Scope: billing.BudgetScopeAgent, RefID: "agent-1", LimitRuns: 1, IsActive: true})
		if !gate.allow(context.Background(), "org-1", "run-1", "agent-other") {
			t.Fatal("agent-scope budget must not deny other agents")
		}
	})
}
