package billing

// Issue #24 service tests: plan CRUD + validation, the subscription lifecycle
// state machine (trial -> active -> past_due -> canceled, rollover included),
// monthly run-budget quota boundaries, and invoice math priced from seeded
// usage rows. Everything runs on the in-memory store with an injected clock —
// no infrastructure.

import (
	"context"
	"errors"
	"testing"
	"time"
)

// base is the deterministic test instant (UTC).
var base = time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

// clock returns a SetClock callback over a mutable instant.
func clock(at *time.Time) func() time.Time {
	return func() time.Time { return *at }
}

type stubUsage struct {
	rows []UsageRow
	err  error
}

func (s *stubUsage) UsageForPeriod(_ context.Context, _ string, _, _ time.Time) ([]UsageRow, error) {
	return s.rows, s.err
}

func newTestService(at *time.Time) *Service {
	s := NewService()
	s.SetClock(clock(at))
	return s
}

func mustPlan(t *testing.T, s *Service, name string, priceCents, quota int64, meta map[string]any) *Plan {
	t.Helper()
	plan, err := s.CreatePlanCtx(context.Background(), PlanInput{
		Name: name, PriceCents: priceCents, IncludedQuota: quota, Metadata: meta,
	})
	if err != nil {
		t.Fatalf("CreatePlanCtx(%s) returned error: %v", name, err)
	}
	return plan
}

func mustSubscribe(t *testing.T, s *Service, orgID, planID string) *Subscription {
	t.Helper()
	sub, err := s.SubscribeCtx(context.Background(), orgID, planID)
	if err != nil {
		t.Fatalf("SubscribeCtx returned error: %v", err)
	}
	return sub
}

// ---------------------------------------------------------------------------
// Plan catalog
// ---------------------------------------------------------------------------

func TestPlanCRUD(t *testing.T) {
	ctx := context.Background()
	s := newTestService(&base)

	plan := mustPlan(t, s, "starter", 1900, 1000, map[string]any{"tier": "starter"})
	if plan.ID == "" || plan.Currency != DefaultCurrency {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	if plan.CreatedAt.IsZero() || plan.UpdatedAt.IsZero() {
		t.Fatal("plan timestamps should be assigned")
	}

	// Duplicate name is rejected.
	if _, err := s.CreatePlanCtx(ctx, PlanInput{Name: "starter"}); !errors.Is(err, ErrPlanExists) {
		t.Fatalf("duplicate name: expected ErrPlanExists, got %v", err)
	}

	// List returns the catalog in creation order.
	mustPlan(t, s, "pro", 9900, 0, nil)
	plans, err := s.ListPlansCtx(ctx)
	if err != nil {
		t.Fatalf("ListPlansCtx returned error: %v", err)
	}
	if len(plans) != 2 || plans[0].Name != "starter" || plans[1].Name != "pro" {
		t.Fatalf("unexpected catalog: %+v", plans)
	}

	// Update renames and rewrites fields; renames keep the unique invariant.
	updated, err := s.UpdatePlanCtx(ctx, plan.ID, PlanInput{Name: "starter-v2", PriceCents: 2900, Currency: "EUR", IncludedQuota: 2000})
	if err != nil {
		t.Fatalf("UpdatePlanCtx returned error: %v", err)
	}
	if updated.Name != "starter-v2" || updated.PriceCents != 2900 || updated.Currency != "eur" || updated.IncludedQuota != 2000 {
		t.Fatalf("unexpected updated plan: %+v", updated)
	}
	if !updated.UpdatedAt.After(plan.UpdatedAt) && !updated.UpdatedAt.Equal(plan.UpdatedAt) {
		t.Fatal("updated_at should advance")
	}
	if _, err := s.UpdatePlanCtx(ctx, plans[1].ID, PlanInput{Name: "starter-v2"}); !errors.Is(err, ErrPlanExists) {
		t.Fatalf("rename collision: expected ErrPlanExists, got %v", err)
	}

	// A plan referenced by a subscription cannot be deleted.
	sub := mustSubscribe(t, s, "org-1", updated.ID)
	if err := s.DeletePlanCtx(ctx, updated.ID); !errors.Is(err, ErrPlanInUse) {
		t.Fatalf("delete in use: expected ErrPlanInUse, got %v", err)
	}

	// Unreferenced plans delete; unknown plans report not-found.
	if err := s.DeletePlanCtx(ctx, plans[1].ID); err != nil {
		t.Fatalf("delete unreferenced returned error: %v", err)
	}
	if err := s.DeletePlanCtx(ctx, plans[1].ID); !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("delete unknown: expected ErrPlanNotFound, got %v", err)
	}
	if _, err := s.GetPlanCtx(ctx, plans[1].ID); !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("get unknown: expected ErrPlanNotFound, got %v", err)
	}

	// Canceled subscriptions still reference their plan (history must be
	// auditable), so deletion stays refused even after a cancel.
	if _, err := s.CancelSubscriptionCtx(ctx, "org-1", true); err != nil {
		t.Fatalf("cancel returned error: %v", err)
	}
	if err := s.DeletePlanCtx(ctx, updated.ID); !errors.Is(err, ErrPlanInUse) {
		t.Fatalf("delete after cancel: expected ErrPlanInUse, got %v", err)
	}
	_ = sub
}

func TestPlanValidation(t *testing.T) {
	s := newTestService(&base)
	cases := []struct {
		name string
		in   PlanInput
		want error
	}{
		{"empty name", PlanInput{Name: "   "}, ErrInvalidPlan},
		{"negative price", PlanInput{Name: "x", PriceCents: -1}, ErrInvalidPlan},
		{"negative quota", PlanInput{Name: "x", IncludedQuota: -5}, ErrInvalidPlan},
		{"bad currency", PlanInput{Name: "x", Currency: "dollars"}, ErrInvalidPlan},
		{"short currency", PlanInput{Name: "x", Currency: "us"}, ErrInvalidPlan},
	}
	for _, tc := range cases {
		if _, err := s.CreatePlanCtx(context.Background(), tc.in); !errors.Is(err, tc.want) {
			t.Fatalf("%s: expected %v, got %v", tc.name, tc.want, err)
		}
	}
	// Blank currency defaults; 0 price and 0 quota (unlimited) are valid.
	plan, err := s.CreatePlanCtx(context.Background(), PlanInput{Name: "free"})
	if err != nil {
		t.Fatalf("valid plan rejected: %v", err)
	}
	if plan.Currency != "usd" || plan.PriceCents != 0 || plan.IncludedQuota != 0 {
		t.Fatalf("unexpected defaults: %+v", plan)
	}
}

// ---------------------------------------------------------------------------
// Subscription lifecycle
// ---------------------------------------------------------------------------

func TestSubscribeStartsTrial(t *testing.T) {
	s := newTestService(&base)
	plan := mustPlan(t, s, "starter", 1900, 1000, nil)

	sub := mustSubscribe(t, s, "org-1", plan.ID)
	if sub.Status != StatusTrial {
		t.Fatalf("expected trial, got %s", sub.Status)
	}
	if !sub.PeriodStart.Equal(base) || !sub.PeriodEnd.Equal(base.Add(DefaultTrialDays*24*time.Hour)) {
		t.Fatalf("unexpected trial window: [%v, %v)", sub.PeriodStart, sub.PeriodEnd)
	}
	if sub.CancelAtPeriodEnd || sub.CanceledAt != nil {
		t.Fatalf("fresh subscription should not be flagged: %+v", sub)
	}

	// One live subscription per org.
	if _, err := s.SubscribeCtx(context.Background(), "org-1", plan.ID); !errors.Is(err, ErrSubscriptionExists) {
		t.Fatalf("second subscribe: expected ErrSubscriptionExists, got %v", err)
	}
	// Guards.
	if _, err := s.SubscribeCtx(context.Background(), "", plan.ID); !errors.Is(err, ErrNoSubscription) {
		t.Fatalf("empty org: expected ErrNoSubscription, got %v", err)
	}
	if _, err := s.SubscribeCtx(context.Background(), "org-1", ""); !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("empty plan: expected ErrPlanNotFound, got %v", err)
	}
	if _, err := s.SubscribeCtx(context.Background(), "org-1", "plan-nope"); !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("unknown plan: expected ErrPlanNotFound, got %v", err)
	}
}

func TestActivateFromTrialStartsFreshBillingPeriod(t *testing.T) {
	s := newTestService(&base)
	plan := mustPlan(t, s, "starter", 1900, 1000, nil)
	sub := mustSubscribe(t, s, "org-1", plan.ID)

	activated, err := s.ActivateSubscriptionCtx(context.Background(), "org-1")
	if err != nil {
		t.Fatalf("activate returned error: %v", err)
	}
	if activated.Status != StatusActive {
		t.Fatalf("expected active, got %s", activated.Status)
	}
	// Trial days are NOT carried into the paid period: [now, now+30d).
	if !activated.PeriodStart.Equal(base) || !activated.PeriodEnd.Equal(base.Add(DefaultPeriodDays*24*time.Hour)) {
		t.Fatalf("unexpected paid period: [%v, %v)", activated.PeriodStart, activated.PeriodEnd)
	}
	if activated.PeriodEnd.Sub(activated.PeriodStart) != DefaultPeriodDays*24*time.Hour {
		t.Fatal("paid period must be the monthly window")
	}
	_ = sub
}

func TestActivateInvalidTransitions(t *testing.T) {
	s := newTestService(&base)
	plan := mustPlan(t, s, "starter", 1900, 1000, nil)
	mustSubscribe(t, s, "org-1", plan.ID)

	// First activate: trial -> active (valid, starts the paid period).
	if _, err := s.ActivateSubscriptionCtx(context.Background(), "org-1"); err != nil {
		t.Fatalf("first activate returned error: %v", err)
	}
	// active -> active is a no-op error (the period is already running).
	if _, err := s.ActivateSubscriptionCtx(context.Background(), "org-1"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("second activate: expected ErrInvalidTransition, got %v", err)
	}

	// Canceling removes the live row: activation (and every other transition)
	// then reports no subscription.
	if _, err := s.CancelSubscriptionCtx(context.Background(), "org-1", true); err != nil {
		t.Fatalf("cancel returned error: %v", err)
	}
	if _, err := s.ActivateSubscriptionCtx(context.Background(), "org-1"); !errors.Is(err, ErrNoSubscription) {
		t.Fatalf("activate after cancel: expected ErrNoSubscription, got %v", err)
	}
}

func TestMarkPastDueKeepsPeriod(t *testing.T) {
	s := newTestService(&base)
	plan := mustPlan(t, s, "starter", 1900, 1000, nil)
	mustSubscribe(t, s, "org-1", plan.ID)
	activated, err := s.ActivateSubscriptionCtx(context.Background(), "org-1")
	if err != nil {
		t.Fatalf("activate returned error: %v", err)
	}

	pastDue, err := s.MarkSubscriptionPastDueCtx(context.Background(), "org-1")
	if err != nil {
		t.Fatalf("mark past due returned error: %v", err)
	}
	if pastDue.Status != StatusPastDue {
		t.Fatalf("expected past_due, got %s", pastDue.Status)
	}
	// The period keeps running so metered usage stays attributable.
	if !pastDue.PeriodStart.Equal(activated.PeriodStart) || !pastDue.PeriodEnd.Equal(activated.PeriodEnd) {
		t.Fatal("mark past_due must not move the period")
	}

	// past_due -> past_due is invalid; trial -> past_due is invalid.
	if _, err := s.MarkSubscriptionPastDueCtx(context.Background(), "org-1"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("double past_due: expected ErrInvalidTransition, got %v", err)
	}
	mustSubscribe(t, s, "org-2", plan.ID)
	if _, err := s.MarkSubscriptionPastDueCtx(context.Background(), "org-2"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("trial past_due: expected ErrInvalidTransition, got %v", err)
	}
}

func TestCancelImmediateVsPeriodEnd(t *testing.T) {
	s := newTestService(&base)
	plan := mustPlan(t, s, "starter", 1900, 1000, nil)
	mustSubscribe(t, s, "org-1", plan.ID)
	mustSubscribe(t, s, "org-2", plan.ID)
	if _, err := s.ActivateSubscriptionCtx(context.Background(), "org-2"); err != nil {
		t.Fatalf("activate org-2 returned error: %v", err)
	}

	// Immediate: canceled NOW, no flag.
	nowSub, err := s.CancelSubscriptionCtx(context.Background(), "org-1", true)
	if err != nil {
		t.Fatalf("immediate cancel returned error: %v", err)
	}
	if nowSub.Status != StatusCanceled || nowSub.CanceledAt == nil || !nowSub.CanceledAt.Equal(base) {
		t.Fatalf("unexpected immediate cancel result: %+v", nowSub)
	}
	if nowSub.CancelAtPeriodEnd {
		t.Fatal("immediate cancel must not set cancel_at_period_end")
	}

	// Deferred: still active, flag set, completed by the rollover.
	defSub, err := s.CancelSubscriptionCtx(context.Background(), "org-2", false)
	if err != nil {
		t.Fatalf("deferred cancel returned error: %v", err)
	}
	if defSub.Status != StatusActive || !defSub.CancelAtPeriodEnd || defSub.CanceledAt != nil {
		t.Fatalf("unexpected deferred cancel result: %+v", defSub)
	}
	// Canceling twice just keeps the flag (idempotent scheduling).
	if _, err := s.CancelSubscriptionCtx(context.Background(), "org-2", false); err != nil {
		t.Fatalf("second deferred cancel returned error: %v", err)
	}

	// A canceled org has no live subscription left.
	if _, err := s.CancelSubscriptionCtx(context.Background(), "org-1", false); !errors.Is(err, ErrNoSubscription) {
		t.Fatalf("cancel after cancel: expected ErrNoSubscription, got %v", err)
	}
}

func TestGetCurrentSubscriptionFallsBackToHistory(t *testing.T) {
	s := newTestService(&base)
	plan := mustPlan(t, s, "starter", 1900, 1000, nil)

	// Never subscribed -> ErrNoSubscription.
	if _, err := s.GetCurrentSubscriptionCtx(context.Background(), "org-x"); !errors.Is(err, ErrNoSubscription) {
		t.Fatalf("expected ErrNoSubscription, got %v", err)
	}
	sub := mustSubscribe(t, s, "org-1", plan.ID)
	if _, err := s.CancelSubscriptionCtx(context.Background(), "org-1", true); err != nil {
		t.Fatalf("cancel returned error: %v", err)
	}
	// Canceled history stays readable over the read path.
	got, err := s.GetCurrentSubscriptionCtx(context.Background(), "org-1")
	if err != nil {
		t.Fatalf("GetCurrentSubscriptionCtx returned error: %v", err)
	}
	if got.ID != sub.ID || got.Status != StatusCanceled {
		t.Fatalf("expected canceled history row, got %+v", got)
	}
}

func TestResubscribeAfterCancelOpensNewLifecycle(t *testing.T) {
	s := newTestService(&base)
	plan := mustPlan(t, s, "starter", 1900, 1000, nil)
	first := mustSubscribe(t, s, "org-1", plan.ID)
	if _, err := s.CancelSubscriptionCtx(context.Background(), "org-1", true); err != nil {
		t.Fatalf("cancel returned error: %v", err)
	}

	second := mustSubscribe(t, s, "org-1", plan.ID)
	if second.ID == first.ID {
		t.Fatal("re-subscribe must open a NEW lifecycle row")
	}
	if second.Status != StatusTrial {
		t.Fatalf("expected fresh trial, got %s", second.Status)
	}
	if second.CreatedAt.Before(first.CreatedAt) {
		t.Fatal("new row should be newer")
	}
}

func TestChangePlanSwapsImmediatelyAndKeepsPeriod(t *testing.T) {
	s := newTestService(&base)
	starter := mustPlan(t, s, "starter", 1900, 1000, nil)
	pro := mustPlan(t, s, "pro", 9900, 0, nil)
	sub := mustSubscribe(t, s, "org-1", starter.ID)
	activated, err := s.ActivateSubscriptionCtx(context.Background(), "org-1")
	if err != nil {
		t.Fatalf("activate returned error: %v", err)
	}

	changed, err := s.ChangePlanCtx(context.Background(), "org-1", pro.ID)
	if err != nil {
		t.Fatalf("change plan returned error: %v", err)
	}
	if changed.PlanID != pro.ID {
		t.Fatalf("expected plan swap to %s, got %s", pro.ID, changed.PlanID)
	}
	// The running period is kept (no proration in this milestone).
	if !changed.PeriodStart.Equal(activated.PeriodStart) || !changed.PeriodEnd.Equal(activated.PeriodEnd) {
		t.Fatal("change plan must keep the running period")
	}
	_ = sub

	if _, err := s.ChangePlanCtx(context.Background(), "org-1", "plan-nope"); !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("unknown plan: expected ErrPlanNotFound, got %v", err)
	}
	if _, err := s.ChangePlanCtx(context.Background(), "org-nope", pro.ID); !errors.Is(err, ErrNoSubscription) {
		t.Fatalf("no subscription: expected ErrNoSubscription, got %v", err)
	}
}

func TestRolloverTransitions(t *testing.T) {
	t.Run("trial expires to past_due", func(t *testing.T) {
		at := base
		s := newTestService(&at)
		plan := mustPlan(t, s, "starter", 1900, 1000, nil)
		mustSubscribe(t, s, "org-1", plan.ID)
		changed, err := s.RolloverDueSubscriptionsCtx(context.Background())
		if err != nil || changed != 0 {
			t.Fatalf("not-due subscription rolled: changed=%d err=%v", changed, err)
		}
		at = base.Add(DefaultTrialDays * 24 * time.Hour) // period_end reached
		changed, err = s.RolloverDueSubscriptionsCtx(context.Background())
		if err != nil || changed != 1 {
			t.Fatalf("expected 1 rollover, got %d err=%v", changed, err)
		}
		sub, err := s.GetCurrentSubscriptionCtx(context.Background(), "org-1")
		if err != nil || sub.Status != StatusPastDue {
			t.Fatalf("expected past_due, got %s err=%v", sub.Status, err)
		}
		// The dunning window is a full fresh period starting at the old end.
		trialEnd := base.Add(DefaultTrialDays * 24 * time.Hour)
		if !sub.PeriodStart.Equal(trialEnd) || !sub.PeriodEnd.Equal(trialEnd.Add(DefaultPeriodDays*24*time.Hour)) {
			t.Fatalf("unexpected dunning window: [%v, %v)", sub.PeriodStart, sub.PeriodEnd)
		}
		// Idempotent per instant (one step per row per call — the freshly
		// past_due row must NOT cascade into canceled at the same instant).
		if changed, err := s.RolloverDueSubscriptionsCtx(context.Background()); err != nil || changed != 0 {
			t.Fatalf("second rollover changed %d rows err=%v", changed, err)
		}
	})

	t.Run("past_due expires to canceled", func(t *testing.T) {
		at := base
		s := newTestService(&at)
		plan := mustPlan(t, s, "starter", 1900, 1000, nil)
		mustSubscribe(t, s, "org-1", plan.ID)
		trialEnd := base.Add(DefaultTrialDays * 24 * time.Hour)
		at = trialEnd
		if _, err := s.RolloverDueSubscriptionsCtx(context.Background()); err != nil {
			t.Fatalf("rollover returned error: %v", err)
		}
		// The dunning window opened at the trial end; exhausting it cancels.
		at = trialEnd.Add(DefaultPeriodDays * 24 * time.Hour)
		changed, err := s.RolloverDueSubscriptionsCtx(context.Background())
		if err != nil || changed != 1 {
			t.Fatalf("expected 1 rollover, got %d err=%v", changed, err)
		}
		sub, _ := s.GetCurrentSubscriptionCtx(context.Background(), "org-1")
		if sub.Status != StatusCanceled || sub.CanceledAt == nil || !sub.CanceledAt.Equal(at) {
			t.Fatalf("expected canceled with canceled_at=now, got %+v", sub)
		}
	})

	t.Run("active with cancel flag cancels at period end", func(t *testing.T) {
		at := base
		s := newTestService(&at)
		plan := mustPlan(t, s, "starter", 1900, 1000, nil)
		mustSubscribe(t, s, "org-1", plan.ID)
		if _, err := s.ActivateSubscriptionCtx(context.Background(), "org-1"); err != nil {
			t.Fatalf("activate returned error: %v", err)
		}
		if _, err := s.CancelSubscriptionCtx(context.Background(), "org-1", false); err != nil {
			t.Fatalf("deferred cancel returned error: %v", err)
		}
		at = base.Add(DefaultPeriodDays * 24 * time.Hour)
		changed, err := s.RolloverDueSubscriptionsCtx(context.Background())
		if err != nil || changed != 1 {
			t.Fatalf("expected 1 rollover, got %d err=%v", changed, err)
		}
		sub, _ := s.GetCurrentSubscriptionCtx(context.Background(), "org-1")
		if sub.Status != StatusCanceled || sub.CancelAtPeriodEnd {
			t.Fatalf("expected completed cancel (flag cleared), got %+v", sub)
		}
	})

	t.Run("active renews the monthly window", func(t *testing.T) {
		at := base
		s := newTestService(&at)
		plan := mustPlan(t, s, "starter", 1900, 1000, nil)
		mustSubscribe(t, s, "org-1", plan.ID)
		activated, err := s.ActivateSubscriptionCtx(context.Background(), "org-1")
		if err != nil {
			t.Fatalf("activate returned error: %v", err)
		}
		// Meter some quota in period 1; the renewal must reset the counter.
		if err := s.RecordQuotaConsumptionCtx(context.Background(), "org-1", 5); err != nil {
			t.Fatalf("record consumption returned error: %v", err)
		}
		at = base.Add(DefaultPeriodDays * 24 * time.Hour)
		changed, err := s.RolloverDueSubscriptionsCtx(context.Background())
		if err != nil || changed != 1 {
			t.Fatalf("expected 1 rollover, got %d err=%v", changed, err)
		}
		sub, _ := s.GetCurrentSubscriptionCtx(context.Background(), "org-1")
		if sub.Status != StatusActive || sub.CancelAtPeriodEnd {
			t.Fatalf("expected renewed active sub, got %+v", sub)
		}
		if !sub.PeriodStart.Equal(activated.PeriodEnd) || !sub.PeriodEnd.Equal(activated.PeriodEnd.Add(DefaultPeriodDays*24*time.Hour)) {
			t.Fatalf("unexpected renewed window: [%v, %v)", sub.PeriodStart, sub.PeriodEnd)
		}
		quota, err := s.CheckQuotaCtx(context.Background(), "org-1")
		if err != nil {
			t.Fatalf("CheckQuotaCtx returned error: %v", err)
		}
		if quota.ConsumedRuns != 0 {
			t.Fatalf("quota counter should reset on renewal, got %d", quota.ConsumedRuns)
		}
	})
}

// ---------------------------------------------------------------------------
// Quota
// ---------------------------------------------------------------------------

func TestQuotaBoundaries(t *testing.T) {
	ctx := context.Background()
	s := newTestService(&base)
	plan := mustPlan(t, s, "metered", 1900, 10, nil)
	mustSubscribe(t, s, "org-1", plan.ID)

	quota, err := s.CheckQuotaCtx(ctx, "org-1")
	if err != nil {
		t.Fatalf("CheckQuotaCtx returned error: %v", err)
	}
	if quota.IncludedRuns != 10 || quota.ConsumedRuns != 0 || quota.RemainingRuns != 10 || quota.Exceeded {
		t.Fatalf("fresh quota wrong: %+v", quota)
	}

	// Exactly at the boundary: consumed == included is still within budget.
	if err := s.RecordQuotaConsumptionCtx(ctx, "org-1", 10); err != nil {
		t.Fatalf("record returned error: %v", err)
	}
	quota, _ = s.CheckQuotaCtx(ctx, "org-1")
	if quota.Exceeded || quota.RemainingRuns != 0 {
		t.Fatalf("boundary consumed==included: %+v", quota)
	}

	// One past the boundary exceeds it.
	if err := s.RecordQuotaConsumptionCtx(ctx, "org-1", 1); err != nil {
		t.Fatalf("record returned error: %v", err)
	}
	quota, _ = s.CheckQuotaCtx(ctx, "org-1")
	if !quota.Exceeded || quota.RemainingRuns != 0 || quota.ConsumedRuns != 11 {
		t.Fatalf("over-boundary quota wrong: %+v", quota)
	}
	if quota.PeriodStart.IsZero() || quota.PeriodEnd.IsZero() {
		t.Fatal("quota should carry the subscription window")
	}
}

func TestQuotaUnlimitedPlan(t *testing.T) {
	ctx := context.Background()
	s := newTestService(&base)
	plan := mustPlan(t, s, "unlimited", 9900, 0, nil) // 0 = unlimited sentinel
	mustSubscribe(t, s, "org-1", plan.ID)

	if err := s.RecordQuotaConsumptionCtx(ctx, "org-1", 9999); err != nil {
		t.Fatalf("record returned error: %v", err)
	}
	quota, err := s.CheckQuotaCtx(ctx, "org-1")
	if err != nil {
		t.Fatalf("CheckQuotaCtx returned error: %v", err)
	}
	if !quota.Unlimited || quota.Exceeded || quota.RemainingRuns != 0 {
		t.Fatalf("unlimited plan must never exceed: %+v", quota)
	}
}

func TestQuotaFromUsageSource(t *testing.T) {
	ctx := context.Background()
	s := newTestService(&base)
	plan := mustPlan(t, s, "metered", 1900, 10, nil)
	mustSubscribe(t, s, "org-1", plan.ID)

	usage := &stubUsage{rows: []UsageRow{
		{Source: LineSourceRun, Model: "m1", Runs: 4, CostCents: 1.5},
		{Source: LineSourceRun, Model: "m2", Runs: 7, CostCents: 0.25},
	}}
	s.SetUsageSource(usage)

	quota, err := s.CheckQuotaCtx(ctx, "org-1")
	if err != nil {
		t.Fatalf("CheckQuotaCtx returned error: %v", err)
	}
	if quota.ConsumedRuns != 11 || !quota.Exceeded {
		t.Fatalf("aggregate quota wrong: %+v", quota)
	}

	// UsageSource failures propagate (never silently report quota available).
	usage.err = errors.New("aggregate down")
	if _, err := s.CheckQuotaCtx(ctx, "org-1"); err == nil {
		t.Fatal("expected the usage source error to propagate")
	}
}

func TestRecordQuotaConsumptionRequiresSubscription(t *testing.T) {
	s := newTestService(&base)
	if err := s.RecordQuotaConsumptionCtx(context.Background(), "org-ghost", 1); !errors.Is(err, ErrNoSubscription) {
		t.Fatalf("expected ErrNoSubscription, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Invoices
// ---------------------------------------------------------------------------

// seededInvoiceService returns a service with a quota-10 plan carrying a
// 2-cent overage rate and a subscription for org-1.
func seededInvoiceService(t *testing.T, at *time.Time, rows []UsageRow) (*Service, *Plan) {
	t.Helper()
	s := newTestService(at)
	plan := mustPlan(t, s, "metered", 1900, 10, map[string]any{"overage_run_rate_cents": 2})
	mustSubscribe(t, s, "org-1", plan.ID)
	s.SetUsageSource(&stubUsage{rows: rows})
	return s, plan
}

func TestInvoiceMathAgainstSeededUsage(t *testing.T) {
	ctx := context.Background()
	s, plan := seededInvoiceService(t, &base, []UsageRow{
		{Source: LineSourceRun, Model: "model-a", Runs: 8, CostCents: 1.5},  // -> 2c (half away from zero)
		{Source: LineSourceRun, Model: "model-b", Runs: 5, CostCents: 0.49}, // -> 0c (rounds down)
		{Source: LineSourceEval, Model: "eval-suite", Runs: 2, CostCents: 3.75},
		{Source: LineSourceRun, Model: "model-c", Runs: 0, CostCents: 0}, // empty bucket: no line
	})

	from := base
	to := base.Add(DefaultPeriodDays * 24 * time.Hour)
	inv, created, err := s.GenerateInvoiceCtx(ctx, "org-1", from, to)
	if err != nil {
		t.Fatalf("GenerateInvoiceCtx returned error: %v", err)
	}
	if !created {
		t.Fatal("first generation should create")
	}
	if inv.Status != InvoiceOpen || inv.Currency != plan.Currency || inv.SubscriptionID == "" {
		t.Fatalf("unexpected invoice header: %+v", inv)
	}
	if len(inv.Lines) != 4 { // 3 usage lines + 1 overage
		t.Fatalf("expected 4 lines, got %d: %+v", len(inv.Lines), inv.Lines)
	}

	var overage *InvoiceLine
	var runCents int64
	runRuns := int64(0)
	for _, line := range inv.Lines {
		switch line.Source {
		case LineSourceOverage:
			overage = line
		case LineSourceRun:
			runCents += line.AmountCents
			runRuns += line.Quantity
		}
	}
	if runRuns != 13 {
		t.Fatalf("expected 13 metered runs, got %d", runRuns)
	}
	if runCents != 2 { // 1.5 -> 2, 0.49 -> 0
		t.Fatalf("expected run lines to total 2c, got %d", runCents)
	}
	if overage == nil {
		t.Fatal("expected an overage line (13 runs > 10 included)")
	}
	if overage.Quantity != 3 || overage.AmountCents != 6 {
		t.Fatalf("overage line wrong: %+v", overage)
	}
	if overage.Refs["included_quota"] != int64(10) || overage.Refs["overage_run_rate_cents"] != int64(2) || overage.Refs["metered_runs"] != int64(13) {
		t.Fatalf("overage refs missing provenance: %+v", overage.Refs)
	}
	// Subtotal is the exact sum of the line amounts (2 + 0 + 4 + 6).
	if inv.SubtotalCents != 12 {
		t.Fatalf("expected subtotal 12c, got %d", inv.SubtotalCents)
	}
	// Model provenance is recorded on usage lines.
	for _, line := range inv.Lines {
		if line.Source == LineSourceRun && line.Refs["model"] == "" {
			t.Fatalf("run line missing model ref: %+v", line)
		}
	}
}

func TestInvoiceOverageRequiresRateAndQuota(t *testing.T) {
	ctx := context.Background()
	// Unlimited plan (0) never overages; positive quota without a rate never overages.
	for _, tc := range []struct {
		quota int64
		meta  map[string]any
	}{
		{0, map[string]any{"overage_run_rate_cents": 2}}, // unlimited sentinel
		{10, nil}, // no rate configured
		{10, map[string]any{"overage_run_rate_cents": 0}}, // zero rate
	} {
		s := newTestService(&base)
		plan := mustPlan(t, s, "p", 100, tc.quota, tc.meta)
		mustSubscribe(t, s, "org-1", plan.ID)
		s.SetUsageSource(&stubUsage{rows: []UsageRow{
			{Source: LineSourceRun, Model: "m", Runs: 50, CostCents: 1},
		}})
		inv, _, err := s.GenerateInvoiceCtx(ctx, "org-1", base, base.Add(24*time.Hour))
		if err != nil {
			t.Fatalf("GenerateInvoiceCtx returned error: %v", err)
		}
		for _, line := range inv.Lines {
			if line.Source == LineSourceOverage {
				t.Fatalf("quota=%d meta=%v: unexpected overage line", tc.quota, tc.meta)
			}
		}
	}
}

func TestInvoiceIdempotencyAndVoidRegeneration(t *testing.T) {
	ctx := context.Background()
	s, _ := seededInvoiceService(t, &base, []UsageRow{
		{Source: LineSourceRun, Model: "m", Runs: 3, CostCents: 1},
	})
	from := base
	to := base.Add(24 * time.Hour)

	first, created, err := s.GenerateInvoiceCtx(ctx, "org-1", from, to)
	if err != nil || !created {
		t.Fatalf("first generate: created=%v err=%v", created, err)
	}
	replay, created, err := s.GenerateInvoiceCtx(ctx, "org-1", from, to)
	if err != nil {
		t.Fatalf("replay returned error: %v", err)
	}
	if created || replay.ID != first.ID || len(replay.Lines) != len(first.Lines) {
		t.Fatalf("replay must return the existing invoice unchanged: created=%v", created)
	}

	// A different window is a different invoice.
	other, created, err := s.GenerateInvoiceCtx(ctx, "org-1", to, to.Add(24*time.Hour))
	if err != nil || !created || other.ID == first.ID {
		t.Fatalf("distinct window should create a distinct invoice: created=%v", created)
	}

	// Voiding frees the (org, period) slot.
	if _, err := s.SettleInvoiceCtx(ctx, "org-1", first.ID, InvoiceVoid); err != nil {
		t.Fatalf("void returned error: %v", err)
	}
	regen, created, err := s.GenerateInvoiceCtx(ctx, "org-1", from, to)
	if err != nil || !created {
		t.Fatalf("regeneration after void: created=%v err=%v", created, err)
	}
	if regen.ID == first.ID {
		t.Fatal("regenerated invoice must be a new document")
	}
}

func TestInvoiceWindowValidation(t *testing.T) {
	ctx := context.Background()
	s, _ := seededInvoiceService(t, &base, nil)
	cases := []struct {
		name     string
		from, to time.Time
	}{
		{"inverted", base.Add(time.Hour), base},
		{"empty", base, base},
		{"zero from", time.Time{}, base},
		{"zero to", base, time.Time{}},
		{"oversized", base, base.Add((maxInvoiceWindowDays + 2) * 24 * time.Hour)},
	}
	for _, tc := range cases {
		if _, _, err := s.GenerateInvoiceCtx(ctx, "org-1", tc.from, tc.to); !errors.Is(err, ErrInvalidPeriod) {
			t.Fatalf("%s: expected ErrInvalidPeriod, got %v", tc.name, err)
		}
	}
}

func TestInvoiceRejectsUnknownUsageRowSource(t *testing.T) {
	ctx := context.Background()
	s, _ := seededInvoiceService(t, &base, []UsageRow{
		{Source: "widgets", Runs: 1, CostCents: 1},
	})
	if _, _, err := s.GenerateInvoiceCtx(ctx, "org-1", base, base.Add(24*time.Hour)); !errors.Is(err, ErrInvalidUsageRow) {
		t.Fatalf("expected ErrInvalidUsageRow, got %v", err)
	}
}

func TestInvoiceWithoutUsageSourceIsEmptyButValid(t *testing.T) {
	ctx := context.Background()
	s := newTestService(&base)
	plan := mustPlan(t, s, "metered", 1900, 10, nil)
	mustSubscribe(t, s, "org-1", plan.ID)

	inv, created, err := s.GenerateInvoiceCtx(ctx, "org-1", base, base.Add(24*time.Hour))
	if err != nil || !created {
		t.Fatalf("generate: created=%v err=%v", created, err)
	}
	if len(inv.Lines) != 0 || inv.SubtotalCents != 0 {
		t.Fatalf("zero-infrastructure invoice should be empty, got %+v", inv)
	}
}

func TestListAndGetInvoices(t *testing.T) {
	ctx := context.Background()
	s, _ := seededInvoiceService(t, &base, []UsageRow{
		{Source: LineSourceRun, Model: "m", Runs: 3, CostCents: 1},
	})
	w1from, w1to := base, base.Add(24*time.Hour)
	w2from, w2to := w1to, w1to.Add(24*time.Hour)
	inv1, _, err := s.GenerateInvoiceCtx(ctx, "org-1", w1from, w1to)
	if err != nil {
		t.Fatalf("generate 1: %v", err)
	}
	inv2, _, err := s.GenerateInvoiceCtx(ctx, "org-1", w2from, w2to)
	if err != nil {
		t.Fatalf("generate 2: %v", err)
	}

	// List is newest first and omits lines.
	list, err := s.ListInvoicesCtx(ctx, "org-1")
	if err != nil {
		t.Fatalf("ListInvoicesCtx returned error: %v", err)
	}
	if len(list) != 2 || list[0].ID != inv2.ID || list[1].ID != inv1.ID {
		t.Fatalf("unexpected list order: %+v", list)
	}
	if list[0].Lines != nil {
		t.Fatal("list entries should omit lines")
	}

	// Get returns the full document with lines.
	got, err := s.GetInvoiceCtx(ctx, "org-1", inv1.ID)
	if err != nil {
		t.Fatalf("GetInvoiceCtx returned error: %v", err)
	}
	if got.ID != inv1.ID || len(got.Lines) == 0 {
		t.Fatalf("expected invoice with lines, got %+v", got)
	}

	// Tenant isolation: another org can neither list nor fetch org-1's data.
	mustPlan(t, s, "other-plan", 1, 1, nil)
	if list, err := s.ListInvoicesCtx(ctx, "org-2"); err != nil || len(list) != 0 {
		t.Fatalf("foreign list should be empty: %v %v", list, err)
	}
	if _, err := s.GetInvoiceCtx(ctx, "org-2", inv1.ID); !errors.Is(err, ErrInvoiceNotFound) {
		t.Fatalf("foreign get: expected ErrInvoiceNotFound, got %v", err)
	}
	if _, err := s.GetInvoiceCtx(ctx, "org-1", "inv-nope"); !errors.Is(err, ErrInvoiceNotFound) {
		t.Fatalf("unknown get: expected ErrInvoiceNotFound, got %v", err)
	}
}

func TestSettleInvoice(t *testing.T) {
	ctx := context.Background()
	s, _ := seededInvoiceService(t, &base, []UsageRow{
		{Source: LineSourceRun, Model: "m", Runs: 3, CostCents: 1},
	})
	from, to := base, base.Add(24*time.Hour)
	inv, _, err := s.GenerateInvoiceCtx(ctx, "org-1", from, to)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	// Invalid target status.
	if _, err := s.SettleInvoiceCtx(ctx, "org-1", inv.ID, "draft"); !errors.Is(err, ErrInvalidInvoiceState) {
		t.Fatalf("invalid status: expected ErrInvalidInvoiceState, got %v", err)
	}
	// Foreign org cannot settle (tenant guard).
	if _, err := s.SettleInvoiceCtx(ctx, "org-2", inv.ID, InvoicePaid); !errors.Is(err, ErrInvoiceNotFound) {
		t.Fatalf("foreign settle: expected ErrInvoiceNotFound, got %v", err)
	}

	paid, err := s.SettleInvoiceCtx(ctx, "org-1", inv.ID, InvoicePaid)
	if err != nil {
		t.Fatalf("pay returned error: %v", err)
	}
	if paid.Status != InvoicePaid {
		t.Fatalf("expected paid, got %s", paid.Status)
	}
	// Settled invoices are immutable.
	if _, err := s.SettleInvoiceCtx(ctx, "org-1", inv.ID, InvoiceVoid); !errors.Is(err, ErrInvalidInvoiceState) {
		t.Fatalf("settle settled: expected ErrInvalidInvoiceState, got %v", err)
	}
	if _, err := s.SettleInvoiceCtx(ctx, "org-1", "inv-nope", InvoicePaid); !errors.Is(err, ErrInvoiceNotFound) {
		t.Fatalf("unknown settle: expected ErrInvoiceNotFound, got %v", err)
	}
}
