package main

// Tests for the worker-side quota enforcement gate (issue #47). The gate is
// store-agnostic (it only consumes billing.Service), so the memory-mode suite
// below exercises every decision branch; the Postgres-mode CheckQuotaCtx SQL
// is pinned separately by internal/billing (store_test.go + the sqlmock-backed
// create-run gate test in cmd/api), and cmd/worker/main.go wires the gate to
// the same durable aggregates only when a DSN is configured.

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"agentos/internal/audit"
	"agentos/internal/billing"
	"agentos/internal/queue"
	"agentos/internal/runs"
)

// stubUsageSource feeds deterministic quota consumption (the same seam the
// production RunsUsageSource fills with runs.cost_cents aggregates).
type stubUsageSource struct {
	rows []billing.UsageRow
	err  error
}

func (s stubUsageSource) UsageForPeriod(_ context.Context, _ string, _, _ time.Time) ([]billing.UsageRow, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.rows, nil
}

// newGateBilling builds an in-memory billing service with a plan of the given
// included quota, subscribes org-1 to it, and optionally wires a usage source.
func newGateBilling(t *testing.T, includedQuota int64, usage billing.UsageSource) *billing.Service {
	t.Helper()
	svc := billing.NewService()
	if usage != nil {
		svc.SetUsageSource(usage)
	}
	plan, err := svc.CreatePlanCtx(context.Background(), billing.PlanInput{
		Name: "starter", PriceCents: 1900, IncludedQuota: includedQuota,
	})
	if err != nil {
		t.Fatalf("CreatePlanCtx returned error: %v", err)
	}
	if _, err := svc.SubscribeCtx(context.Background(), "org-1", plan.ID); err != nil {
		t.Fatalf("SubscribeCtx returned error: %v", err)
	}
	return svc
}

// TestQuotaEnforcerAllowMatrix covers the worker gate decision matrix end to
// end: run marking (status + reason), audit rows, and the allow/deny result.
func TestQuotaEnforcerAllowMatrix(t *testing.T) {
	cases := []struct {
		name          string
		included      int64
		usage         billing.UsageSource
		consume       int64 // fed via RecordQuotaConsumptionCtx when > 0 (memory counters)
		wantAllow     bool
		wantStatus    runs.RunStatus
		wantOutput    string
		wantAuditRows int
	}{
		{
			name:          "capped org over quota denied",
			included:      10,
			consume:       11,
			wantAllow:     false,
			wantStatus:    runs.StatusFailed,
			wantOutput:    billing.ReasonQuotaExceeded,
			wantAuditRows: 1,
		},
		{
			name:       "capped org within quota executes",
			included:   10,
			consume:    3,
			wantAllow:  true,
			wantStatus: runs.StatusQueued,
		},
		{
			name:       "unlimited plan never denied",
			included:   0,
			consume:    9999,
			wantAllow:  true,
			wantStatus: runs.StatusQueued,
		},
		{
			name:       "org without subscription executes",
			included:   10,
			consume:    -1, // skip the org-1 subscription entirely below
			wantAllow:  true,
			wantStatus: runs.StatusQueued,
		},
		{
			name:     "usage-source exceeded denied (PG-mode parity)",
			included: 10,
			usage: stubUsageSource{rows: []billing.UsageRow{
				{Source: billing.LineSourceRun, Model: "gpt-4o-mini", Runs: 11},
			}},
			wantAllow:     false,
			wantStatus:    runs.StatusFailed,
			wantOutput:    billing.ReasonQuotaExceeded,
			wantAuditRows: 1,
		},
		{
			name:       "usage-source failure degrades to allow",
			included:   10,
			usage:      stubUsageSource{err: errors.New("aggregate down")},
			wantAllow:  true,
			wantStatus: runs.StatusQueued,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			bsvc := newGateBilling(t, tc.included, tc.usage)
			orgID := "org-1"
			if tc.consume == -1 {
				orgID = "org-never-subscribed"
			} else if tc.consume > 0 {
				if err := bsvc.RecordQuotaConsumptionCtx(ctx, orgID, tc.consume); err != nil {
					t.Fatalf("RecordQuotaConsumptionCtx returned error: %v", err)
				}
			}

			rs := runs.NewService()
			run, err := rs.Create(orgID, "agent-1", "input")
			if err != nil {
				t.Fatalf("Create run returned error: %v", err)
			}
			auditSvc := audit.NewService()
			gate := &quotaEnforcer{billing: bsvc, runs: rs, audit: auditSvc, log: slog.Default()}

			got := gate.allow(ctx, orgID, run.ID, "agent-1")
			if got != tc.wantAllow {
				t.Fatalf("allow() = %v, want %v", got, tc.wantAllow)
			}
			stored, ok := rs.Get(run.ID)
			if !ok {
				t.Fatal("run disappeared from the service")
			}
			if stored.Status != tc.wantStatus {
				t.Fatalf("run status = %q, want %q", stored.Status, tc.wantStatus)
			}
			if stored.Output != tc.wantOutput {
				t.Fatalf("run output (failure reason) = %q, want %q", stored.Output, tc.wantOutput)
			}
			entries := auditSvc.List()
			if len(entries) != tc.wantAuditRows {
				t.Fatalf("audit rows = %d, want %d", len(entries), tc.wantAuditRows)
			}
			if tc.wantAuditRows == 1 {
				e := entries[0]
				if e.Action != "run.quota_denied" || e.OrganizationID != orgID || e.Resource != "runs/"+run.ID {
					t.Fatalf("unexpected audit entry: %+v", e)
				}
				if e.Metadata["reason"] != billing.ReasonQuotaExceeded {
					t.Fatalf("audit metadata reason = %v, want %q", e.Metadata["reason"], billing.ReasonQuotaExceeded)
				}
				if e.Metadata["agent_id"] != "agent-1" {
					t.Fatalf("audit metadata agent_id = %v, want agent-1", e.Metadata["agent_id"])
				}
			}
		})
	}
}

// TestQuotaEnforcerNilSafe pins the "not wired -> allow" contract: a nil
// enforcer (flag off, or enforcement on without durable billing state) never
// blocks a run.
func TestQuotaEnforcerNilSafe(t *testing.T) {
	var gate *quotaEnforcer
	if !gate.allow(context.Background(), "org-1", "run-1", "agent-1") {
		t.Fatal("nil enforcer must allow execution")
	}
}

// TestQuotaDenialAcksTaskAndContinues asserts the queue-loop contract the
// worker relies on: a quota denial is terminal for the task — the handler's
// denial branch returns nil, the queue ACKs (no retry/requeue), and the next
// task is consumed normally.
func TestQuotaDenialAcksTaskAndContinues(t *testing.T) {
	ctx := context.Background()
	bsvc := newGateBilling(t, 10, nil)
	if err := bsvc.RecordQuotaConsumptionCtx(ctx, "org-1", 11); err != nil {
		t.Fatalf("RecordQuotaConsumptionCtx returned error: %v", err)
	}

	rs := runs.NewService()
	overQuotaRun, _ := rs.Create("org-1", "agent-1", "denied")
	fineRun, _ := rs.Create("org-2", "agent-1", "executes") // org-2 has no subscription

	gate := &quotaEnforcer{billing: bsvc, runs: rs, audit: audit.NewService(), log: slog.Default()}

	q := queue.NewQueue()
	q.Enqueue("agent.run", map[string]any{"run_id": overQuotaRun.ID, "organization_id": "org-1"})
	q.Enqueue("agent.run", map[string]any{"run_id": fineRun.ID, "organization_id": "org-2"})

	worker := queue.NewWorker(q, func(task *queue.Task) error {
		orgID, _ := task.Payload["organization_id"].(string)
		runID, _ := task.Payload["run_id"].(string)
		// Mirrors the cmd/worker processTask denial branch: allow -> execute
		// (noop here), deny -> return nil so the task is ACKed, never retried.
		if !gate.allow(ctx, orgID, runID, "agent-1") {
			return nil
		}
		return rs.UpdateStatus(runID, runs.StatusCompleted, "done")
	})

	if err := worker.ProcessNext(); err != nil {
		t.Fatalf("first ProcessNext returned error: %v", err)
	}
	if err := worker.ProcessNext(); err != nil {
		t.Fatalf("second ProcessNext returned error: %v", err)
	}
	if q.Length() != 0 {
		t.Fatalf("queue length = %d, want 0 (denied task must be ACKed, not requeued)", q.Length())
	}
	denied, _ := rs.Get(overQuotaRun.ID)
	if denied.Status != runs.StatusFailed || denied.Output != billing.ReasonQuotaExceeded {
		t.Fatalf("denied run = %+v, want FAILED/quota_exceeded", denied)
	}
	executed, _ := rs.Get(fineRun.ID)
	if executed.Status != runs.StatusCompleted {
		t.Fatalf("over-quota denial must not stop the loop; fine run = %+v, want COMPLETED", executed)
	}
}
