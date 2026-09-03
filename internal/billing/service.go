// Package billing implements real billing & subscriptions (issue #24): a
// global plan catalog, per-organization subscription lifecycle
// (trial -> active -> past_due -> canceled), monthly run-budget quota checks,
// and usage invoicing priced from EXISTING cost aggregates.
//
// Dual-mode persistence: Service always delegates to a Store — either the
// in-memory store (zero-infrastructure mode, NewService) or the Postgres store
// (migration 016, NewServiceWithStore + NewPostgresStore). The Postgres store
// is sqlmock-tested; the in-memory store mirrors its semantics (one live
// subscription per org, idempotent invoice regeneration per period).
//
// Cost provenance (deliberate, documented): billing NEVER stores or recomputes
// token costs. Invoices are priced from injected UsageSource rows; the shipped
// adapter (RunsUsageSource) feeds the same runs.cost_cents aggregation that
// powers GET /v1/usage/costs, so an invoice always reconciles with the usage
// report of the same window.
package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Lifecycle/status constants.
const (
	// Subscription statuses (mirrors ck_subscriptions_status in migration 016).
	StatusTrial    = "trial"
	StatusActive   = "active"
	StatusPastDue  = "past_due"
	StatusCanceled = "canceled"

	// Invoice statuses (mirrors ck_invoices_status).
	InvoiceOpen = "open"
	InvoicePaid = "paid"
	InvoiceVoid = "void"

	// Invoice line sources (mirrors ck_invoice_lines_source). "eval" is a
	// schema-supported seam: no eval-side org/window cost aggregate exists in
	// internal/evaluations today, so shipped generators emit run/overage lines
	// and custom UsageSource implementations may emit eval rows.
	LineSourceRun     = "run"
	LineSourceEval    = "eval"
	LineSourceOverage = "overage"

	// DefaultCurrency is used when a plan omits its currency.
	DefaultCurrency = "usd"
	// DefaultTrialDays is the trial window granted at subscribe time.
	DefaultTrialDays = 14
	// DefaultPeriodDays is the monthly billing period length. Both the SQL
	// rollover (make_interval(days => ...)) and the in-memory rollover use
	// this single constant so the two backends can never drift.
	DefaultPeriodDays = 30
	// maxInvoiceWindowDays caps one invoice window (defense in depth, mirrors
	// the usage-costs report cap).
	maxInvoiceWindowDays = 366
)

// Plan is one entry of the global (non-tenant) plan catalog.
//
// IncludedQuota semantics: the included monthly RUN budget for subscribers.
// 0 means UNLIMITED (documented sentinel — a paid plan with no metered
// ceiling); overage lines are only ever generated for plans with
// IncludedQuota > 0.
type Plan struct {
	ID            string
	Name          string
	PriceCents    int64
	Currency      string // ISO-4217-ish 3-letter code, lowercase ("usd")
	IncludedQuota int64  // monthly included run budget; 0 = unlimited
	Metadata      map[string]any
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Subscription is the per-tenant billing state. Periods are half-open
// [PeriodStart, PeriodEnd).
type Subscription struct {
	ID                string
	OrganizationID    string
	PlanID            string
	Status            string // trial|active|past_due|canceled
	PeriodStart       time.Time
	PeriodEnd         time.Time
	CancelAtPeriodEnd bool
	CanceledAt        *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// Invoice is one billing document for a half-open period. SubtotalCents is
// always the exact sum of its line amounts.
type Invoice struct {
	ID             string
	OrganizationID string
	SubscriptionID string
	PeriodStart    time.Time
	PeriodEnd      time.Time
	SubtotalCents  int64
	Currency       string
	Status         string // open|paid|void
	Lines          []*InvoiceLine
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// InvoiceLine prices one metered source inside an invoice. Refs carries the
// pricing provenance ({"model": ...} for run lines, {"included_quota",
// "overage_run_rate_cents", "metered_runs"} for overage lines) so an invoice
// can be audited against the usage-cost report.
type InvoiceLine struct {
	ID             string
	InvoiceID      string
	OrganizationID string
	Source         string // run|eval|overage
	Description    string
	Quantity       int64
	AmountCents    int64
	Refs           map[string]any
	CreatedAt      time.Time
}

// QuotaStatus is the monthly run-budget view returned by CheckQuotaCtx.
type QuotaStatus struct {
	OrganizationID string
	SubscriptionID string
	Status         string // subscription status
	IncludedRuns   int64  // plan.IncludedQuota (0 = unlimited)
	Unlimited      bool   // true when the plan has IncludedQuota == 0
	ConsumedRuns   int64  // metered runs inside the live period
	RemainingRuns  int64  // max(0, included - consumed); 0 when unlimited
	Exceeded       bool   // consumed > included (never true when unlimited)
	PeriodStart    time.Time
	PeriodEnd      time.Time
}

// Service errors — mapped onto the HTTP error envelope by cmd/api/billing.go.
var (
	ErrPlanNotFound         = errors.New("billing: plan not found")
	ErrPlanExists           = errors.New("billing: plan name already exists")
	ErrPlanInUse            = errors.New("billing: plan is referenced by subscriptions and cannot be deleted")
	ErrInvalidPlan          = errors.New("billing: invalid plan")
	ErrNoSubscription       = errors.New("billing: organization has no subscription")
	ErrSubscriptionExists   = errors.New("billing: organization already has a live subscription")
	ErrSubscriptionCanceled = errors.New("billing: subscription is canceled")
	ErrInvalidTransition    = errors.New("billing: invalid subscription transition")
	ErrInvoiceNotFound      = errors.New("billing: invoice not found")
	ErrInvalidInvoiceState  = errors.New("billing: invalid invoice state")
	ErrInvalidPeriod        = errors.New("billing: invalid period")
	ErrInvalidUsageRow      = errors.New("billing: invalid usage row source")
)

// subscription statuses that keep the uq_subscriptions_one_live slot taken.
func isLiveStatus(status string) bool {
	return status == StatusTrial || status == StatusActive || status == StatusPastDue
}

// currencyRe pins the plan currency to a 3-letter code (stored lowercase).
var currencyRe = regexp.MustCompile(`^[a-zA-Z]{3}$`)

// Store persists billing rows. Every tenant-scoped method filters by
// organization_id so a tenant can only ever observe its own data.
type Store interface {
	// Plans (global catalog).
	CreatePlan(ctx context.Context, plan *Plan) error
	ListPlans(ctx context.Context) ([]*Plan, error)
	GetPlan(ctx context.Context, id string) (*Plan, error)
	UpdatePlan(ctx context.Context, plan *Plan) error
	// DeletePlan removes an unreferenced plan; implementations must refuse
	// (no-op + zero rows affected contract via ErrPlanInUse in the service)
	// when any subscription still points at the plan.
	DeletePlan(ctx context.Context, id string) error

	// Subscriptions.
	CreateSubscription(ctx context.Context, sub *Subscription) error
	// ListSubscriptionsByOrg returns the org's subscriptions, newest first
	// (the live row — at most one by uq_subscriptions_one_live — is picked by
	// the service).
	ListSubscriptionsByOrg(ctx context.Context, orgID string) ([]*Subscription, error)
	UpdateSubscription(ctx context.Context, sub *Subscription) error
	// RolloverDueSubscriptions applies the period transitions of every due
	// live subscription (trial->past_due with a shifted dunning window,
	// active+cancel_at_period_end -> canceled, past_due -> canceled,
	// active -> renewed period) and returns the number of affected rows.
	RolloverDueSubscriptions(ctx context.Context, now time.Time, periodDays int) (int64, error)

	// Invoices (CreateInvoice persists the document AND its lines atomically).
	CreateInvoice(ctx context.Context, inv *Invoice) error
	ListInvoicesByOrg(ctx context.Context, orgID string) ([]*Invoice, error)
	GetInvoice(ctx context.Context, orgID, id string) (*Invoice, error)
	// FindInvoiceByPeriod returns the non-void invoice covering the exact
	// half-open period, or ErrInvoiceNotFound.
	FindInvoiceByPeriod(ctx context.Context, orgID string, periodStart, periodEnd time.Time) (*Invoice, error)
	GetInvoiceLines(ctx context.Context, invoiceID string) ([]*InvoiceLine, error)
	UpdateInvoiceStatus(ctx context.Context, orgID, id, status string, updatedAt time.Time) error
}

// Service is the billing facade. Both constructors share the same logic; only
// the Store differs (in-memory vs Postgres). The quota counters below are the
// documented PERSISTENCE SEAM for quota tracking: they are process-local by
// design. When a UsageSource is wired (the production wiring passes
// RunsUsageSource), CheckQuotaCtx re-counts metered runs from the durable
// cost aggregates instead, and these counters become the zero-infrastructure
// fallback only.
type Service struct {
	mu       sync.Mutex
	store    Store
	usage    UsageSource
	nowFn    func() time.Time
	consumed map[string]*quotaUse // org -> counter (persistence seam, see above)
}

// quotaUse is the process-local consumed-runs counter bound to one period.
// When `now` leaves its window the counter resets (period-aware).
type quotaUse struct {
	periodStart time.Time
	periodEnd   time.Time
	consumed    int64
}

func newService(store Store) *Service {
	return &Service{
		store:    store,
		nowFn:    func() time.Time { return time.Now().UTC() },
		consumed: make(map[string]*quotaUse),
	}
}

// NewService returns the zero-infrastructure (in-memory) billing service.
func NewService() *Service { return newService(newMemoryStore()) }

// NewServiceWithStore returns a Postgres-backed billing service. usage may be
// nil (quota then relies on the process-local counters fed by
// RecordQuotaConsumptionCtx); production wiring passes
// NewRunsUsageSource(runsSvc) so quota + invoices read the durable aggregates.
func NewServiceWithStore(store Store, usage UsageSource) *Service {
	s := newService(store)
	s.usage = usage
	return s
}

// SetUsageSource swaps the metering source (nil disables aggregate reads).
func (s *Service) SetUsageSource(usage UsageSource) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usage = usage
}

// SetClock overrides the wall clock (tests inject deterministic time).
func (s *Service) SetClock(now func() time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if now != nil {
		s.nowFn = now
	}
}

func (s *Service) now() time.Time {
	return s.nowFn().UTC()
}

// ---------------------------------------------------------------------------
// Plan catalog (admin-level CRUD; the HTTP surface only exposes reads —
// see cmd/api/billing.go for the RBAC scoping).
// ---------------------------------------------------------------------------

// PlanInput is the validated create/update payload for a plan.
type PlanInput struct {
	Name          string
	PriceCents    int64
	Currency      string
	IncludedQuota int64
	Metadata      map[string]any
}

func normalizeCurrency(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultCurrency, nil
	}
	if !currencyRe.MatchString(raw) {
		return "", fmt.Errorf("%w: currency must be a 3-letter code", ErrInvalidPlan)
	}
	return strings.ToLower(raw), nil
}

func validatePlanInput(in PlanInput) (string, error) {
	if strings.TrimSpace(in.Name) == "" {
		return "", fmt.Errorf("%w: name is required", ErrInvalidPlan)
	}
	if len(strings.TrimSpace(in.Name)) > 100 {
		return "", fmt.Errorf("%w: name is too long (max 100 chars)", ErrInvalidPlan)
	}
	if in.PriceCents < 0 {
		return "", fmt.Errorf("%w: price_cents must be >= 0", ErrInvalidPlan)
	}
	if in.IncludedQuota < 0 {
		return "", fmt.Errorf("%w: included_quota must be >= 0", ErrInvalidPlan)
	}
	return normalizeCurrency(in.Currency)
}

// CreatePlanCtx adds a plan to the global catalog. IDs and timestamps are
// assigned here; the plan name is unique.
func (s *Service) CreatePlanCtx(ctx context.Context, in PlanInput) (*Plan, error) {
	currency, err := validatePlanInput(in)
	if err != nil {
		return nil, err
	}
	now := s.now()
	plan := &Plan{
		ID:            uuid.NewString(),
		Name:          strings.TrimSpace(in.Name),
		PriceCents:    in.PriceCents,
		Currency:      currency,
		IncludedQuota: in.IncludedQuota,
		Metadata:      in.Metadata,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := s.store.CreatePlan(ctx, plan); err != nil {
		return nil, err
	}
	return plan, nil
}

// ListPlansCtx returns the full catalog (creation order).
func (s *Service) ListPlansCtx(ctx context.Context) ([]*Plan, error) {
	return s.store.ListPlans(ctx)
}

// GetPlanCtx resolves one plan by id.
func (s *Service) GetPlanCtx(ctx context.Context, id string) (*Plan, error) {
	return s.store.GetPlan(ctx, id)
}

// UpdatePlanCtx rewrites the mutable plan fields. Renames keep the unique
// name invariant (ErrPlanExists on collision).
func (s *Service) UpdatePlanCtx(ctx context.Context, id string, in PlanInput) (*Plan, error) {
	currency, err := validatePlanInput(in)
	if err != nil {
		return nil, err
	}
	plan, err := s.store.GetPlan(ctx, id)
	if err != nil {
		return nil, err
	}
	plan.Name = strings.TrimSpace(in.Name)
	plan.PriceCents = in.PriceCents
	plan.Currency = currency
	plan.IncludedQuota = in.IncludedQuota
	plan.Metadata = in.Metadata
	plan.UpdatedAt = s.now()
	if err := s.store.UpdatePlan(ctx, plan); err != nil {
		return nil, err
	}
	return plan, nil
}

// DeletePlanCtx removes a plan that no subscription references.
func (s *Service) DeletePlanCtx(ctx context.Context, id string) error {
	if _, err := s.store.GetPlan(ctx, id); err != nil {
		return err
	}
	return s.store.DeletePlan(ctx, id)
}

// ---------------------------------------------------------------------------
// Subscription lifecycle
// ---------------------------------------------------------------------------
//
// Period semantics (documented choices):
//
//   - Every period is the half-open interval [period_start, period_end); all
//     windows (quota re-count, invoice math) use `created_at/period >= start
//     AND < end` so adjacent windows never overlap or gap.
//   - Periods are monthly: DefaultPeriodDays (30d) everywhere, from the single
//     DefaultPeriodDays constant, so the Postgres rollover (make_interval) and
//     the in-memory rollover agree to the second.
//   - Subscribe starts a TRIAL of DefaultTrialDays (14d): [now, now+14d).
//   - ActivateCtx (payment confirmed) starts a FRESH monthly billing period
//     [now, now+30d) — trial days are not carried into the paid period. Allowed
//     from trial AND past_due (dunning recovery re-baselines the cycle).
//   - MarkPastDueCtx flips active -> past_due in place (payment failure; the
//     period keeps running so already-metered usage stays attributable).
//   - Rollover (RolloverDueSubscriptionsCtx / store-side bulk UPDATE) applies
//     to every live subscription whose period_end <= now:
//       trial            -> past_due   (trial expired without payment method;
//                                       the window SHIFTS to
//                                       [old_end, old_end + 30d) so past_due
//                                       opens a real dunning/grace period —
//                                       dunning gets a full cycle and the
//                                       rollover stays idempotent per instant,
//                                       i.e. one step per row per call)
//       active+cancel    -> canceled   (cancel_at_period_end honored; flag
//                                       cleared — the cancel completed)
//       past_due         -> canceled   (dunning window exhausted)
//       active (no flag) -> renewed:   period shifts to
//                                       [old_end, old_end + 30d) and the
//                                       cancel_at_period_end flag resets so
//                                       every period carries a fresh decision.
//   - CancelCtx supports two modes: immediate (status=canceled, canceled_at=
//     now) and at-period-end (cancel_at_period_end=true; the rollover
//     completes it). Re-subscribing after a cancel opens a NEW lifecycle row —
//     canceled rows are history, never revived.
//   - ChangePlanCtx swaps the plan immediately and keeps the running period
//     (no proration in this milestone: usage is metered against cost
//     aggregates, and the NEXT invoice simply prices with the new plan's
//     currency/quota knobs).

// SubscribeCtx creates the org's subscription in trial state (14-day window).
// Fails while a live subscription exists (one per org, migration 016 partial
// unique index) and re-subscribes cleanly after a cancel.
func (s *Service) SubscribeCtx(ctx context.Context, orgID, planID string) (*Subscription, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrNoSubscription
	}
	if strings.TrimSpace(planID) == "" {
		return nil, ErrPlanNotFound
	}
	if _, err := s.store.GetPlan(ctx, planID); err != nil {
		return nil, err
	}
	subs, err := s.store.ListSubscriptionsByOrg(ctx, orgID)
	if err != nil {
		return nil, err
	}
	for _, sub := range subs {
		if isLiveStatus(sub.Status) {
			return nil, ErrSubscriptionExists
		}
	}
	now := s.now()
	sub := &Subscription{
		ID:             uuid.NewString(),
		OrganizationID: orgID,
		PlanID:         planID,
		Status:         StatusTrial,
		PeriodStart:    now,
		PeriodEnd:      now.Add(DefaultTrialDays * 24 * time.Hour),
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := s.store.CreateSubscription(ctx, sub); err != nil {
		return nil, err
	}
	s.resetQuotaCounter(orgID)
	return sub, nil
}

// GetCurrentSubscriptionCtx returns the org's live subscription, or — when no
// live row exists — the most recent one in any status (canceled history is
// still readable over GET /v1/billing/subscription). ErrNoSubscription when
// the org never subscribed.
func (s *Service) GetCurrentSubscriptionCtx(ctx context.Context, orgID string) (*Subscription, error) {
	subs, err := s.store.ListSubscriptionsByOrg(ctx, orgID)
	if err != nil {
		return nil, err
	}
	var latest *Subscription
	for _, sub := range subs {
		if isLiveStatus(sub.Status) {
			return sub, nil
		}
		if latest == nil {
			latest = sub // newest-first order: first row wins the fallback
		}
	}
	if latest == nil {
		return nil, ErrNoSubscription
	}
	return latest, nil
}

// getLiveSubscriptionCtx resolves the live subscription or ErrNoSubscription.
func (s *Service) getLiveSubscriptionCtx(ctx context.Context, orgID string) (*Subscription, error) {
	subs, err := s.store.ListSubscriptionsByOrg(ctx, orgID)
	if err != nil {
		return nil, err
	}
	for _, sub := range subs {
		if isLiveStatus(sub.Status) {
			return sub, nil
		}
	}
	return nil, ErrNoSubscription
}

// ActivateSubscriptionCtx moves a trial/past_due subscription to active and
// starts a fresh monthly period [now, now+30d). From active it is a no-op
// error (ErrInvalidTransition); canceled subscriptions stay canceled.
func (s *Service) ActivateSubscriptionCtx(ctx context.Context, orgID string) (*Subscription, error) {
	return s.transitionLive(ctx, orgID, func(sub *Subscription, now time.Time) error {
		switch sub.Status {
		case StatusTrial, StatusPastDue:
			sub.Status = StatusActive
			sub.PeriodStart = now
			sub.PeriodEnd = now.Add(DefaultPeriodDays * 24 * time.Hour)
			return nil
		default:
			return fmt.Errorf("%w: cannot activate from %s", ErrInvalidTransition, sub.Status)
		}
	})
}

// MarkSubscriptionPastDueCtx flips active -> past_due (payment failure).
// The period is untouched so metered usage stays attributable to it.
func (s *Service) MarkSubscriptionPastDueCtx(ctx context.Context, orgID string) (*Subscription, error) {
	return s.transitionLive(ctx, orgID, func(sub *Subscription, _ time.Time) error {
		if sub.Status != StatusActive {
			return fmt.Errorf("%w: cannot mark %s past_due", ErrInvalidTransition, sub.Status)
		}
		sub.Status = StatusPastDue
		return nil
	})
}

// ChangePlanCtx swaps the subscription's plan immediately, keeping the running
// period (no proration — see the period-semantics comment above). The next
// invoice prices with the new plan (currency + quota knobs).
func (s *Service) ChangePlanCtx(ctx context.Context, orgID, planID string) (*Subscription, error) {
	if strings.TrimSpace(planID) == "" {
		return nil, ErrPlanNotFound
	}
	if _, err := s.store.GetPlan(ctx, planID); err != nil {
		return nil, err
	}
	return s.transitionLive(ctx, orgID, func(sub *Subscription, now time.Time) error {
		if sub.Status == StatusCanceled {
			return ErrSubscriptionCanceled
		}
		sub.PlanID = planID
		sub.UpdatedAt = now
		return nil
	})
}

// CancelSubscriptionCtx cancels the org's live subscription. immediate=true
// cancels NOW; immediate=false schedules the cancel for the end of the
// running period (cancel_at_period_end=true, completed by the rollover).
func (s *Service) CancelSubscriptionCtx(ctx context.Context, orgID string, immediate bool) (*Subscription, error) {
	return s.transitionLive(ctx, orgID, func(sub *Subscription, now time.Time) error {
		if sub.Status == StatusCanceled {
			return ErrSubscriptionCanceled
		}
		if immediate {
			sub.Status = StatusCanceled
			sub.CancelAtPeriodEnd = false
			sub.CanceledAt = &now
			return nil
		}
		sub.CancelAtPeriodEnd = true
		return nil
	})
}

// transitionLive loads the live subscription, applies the mutation and
// persists it. `mut` mutates the loaded subscription in place.
func (s *Service) transitionLive(ctx context.Context, orgID string, mut func(sub *Subscription, now time.Time) error) (*Subscription, error) {
	sub, err := s.getLiveSubscriptionCtx(ctx, orgID)
	if err != nil {
		return nil, err
	}
	now := s.now()
	if err := mut(sub, now); err != nil {
		return nil, err
	}
	sub.UpdatedAt = now
	if err := s.store.UpdateSubscription(ctx, sub); err != nil {
		return nil, err
	}
	return sub, nil
}

// RolloverDueSubscriptionsCtx advances every due live subscription to its
// next lifecycle state (see the period-semantics comment on the Service type).
// It is idempotent per instant: already-rolled rows no longer match
// period_end <= now. Returns the number of affected subscriptions.
func (s *Service) RolloverDueSubscriptionsCtx(ctx context.Context) (int64, error) {
	return s.store.RolloverDueSubscriptions(ctx, s.now(), DefaultPeriodDays)
}

// ---------------------------------------------------------------------------
// Quota: monthly run budget from the plan
// ---------------------------------------------------------------------------
//
// Quota model (documented):
//
//   - The budget counts RUNS (not tokens) inside the LIVE period window
//     [period_start, period_end) of the subscription.
//   - plan.included_quota == 0 means UNLIMITED: never exceeded, no overage.
//   - Durable path: when a UsageSource is wired (production: RunsUsageSource
//     over the runs.cost_cents aggregates), consumed runs are re-counted from
//     the aggregates of the live window — the same numbers the usage-cost
//     report shows. No quota state is persisted anywhere.
//   - Persistence seam: without a UsageSource, the Service keeps process-local
//     consumed counters (fed by RecordQuotaConsumptionCtx). They are per
//     process and reset when the period window rolls over; callers that need
//     exact durable accounting must wire the UsageSource. This keeps the
//     zero-infrastructure mode fully functional without faking persistence.

// RecordQuotaConsumptionCtx feeds the process-local counter (persistence seam
// above). It records without rejecting: CheckQuotaCtx is the single decision
// point so callers can enforce their own policy (reject, degrade, warn).
func (s *Service) RecordQuotaConsumptionCtx(ctx context.Context, orgID string, runs int64) error {
	sub, err := s.getLiveSubscriptionCtx(ctx, orgID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	use := s.consumed[orgID]
	if use == nil || !use.periodStart.Equal(sub.PeriodStart) || !use.periodEnd.Equal(sub.PeriodEnd) {
		use = &quotaUse{periodStart: sub.PeriodStart, periodEnd: sub.PeriodEnd}
		s.consumed[orgID] = use
	}
	use.consumed += runs
	return nil
}

func (s *Service) resetQuotaCounter(orgID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.consumed, orgID)
}

// CheckQuotaCtx resolves the monthly run-budget state for the org's CURRENT
// subscription (any status; canceled subscriptions report their last period).
func (s *Service) CheckQuotaCtx(ctx context.Context, orgID string) (*QuotaStatus, error) {
	sub, err := s.GetCurrentSubscriptionCtx(ctx, orgID)
	if err != nil {
		return nil, err
	}
	return s.quotaFor(ctx, sub)
}

// quotaFor computes the QuotaStatus for one subscription. UsageSource failures
// are PROPAGATED (not swallowed): a silent fallback to consumed=0 would report
// quota available that is already spent — the one error billing must never
// fake. Callers decide how to degrade.
func (s *Service) quotaFor(ctx context.Context, sub *Subscription) (*QuotaStatus, error) {
	status := &QuotaStatus{
		OrganizationID: sub.OrganizationID,
		SubscriptionID: sub.ID,
		Status:         sub.Status,
		PeriodStart:    sub.PeriodStart,
		PeriodEnd:      sub.PeriodEnd,
	}
	plan, err := s.store.GetPlan(ctx, sub.PlanID)
	if err != nil {
		// The plan vanished behind the FK guard — treat as unlimited rather
		// than failing the read (documented defensive choice).
		status.Unlimited = true
		return status, nil
	}
	status.IncludedRuns = plan.IncludedQuota
	status.Unlimited = plan.IncludedQuota == 0

	var consumed int64
	if s.usage != nil {
		rows, err := s.usage.UsageForPeriod(ctx, sub.OrganizationID, sub.PeriodStart, sub.PeriodEnd)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			consumed += row.Runs
		}
	} else {
		s.mu.Lock()
		use := s.consumed[sub.OrganizationID]
		if use != nil && use.periodStart.Equal(sub.PeriodStart) && use.periodEnd.Equal(sub.PeriodEnd) {
			consumed = use.consumed
		}
		s.mu.Unlock()
	}
	status.ConsumedRuns = consumed
	if !status.Unlimited {
		if remaining := plan.IncludedQuota - consumed; remaining > 0 {
			status.RemainingRuns = remaining
		}
		// Boundary: consumed == included is still within the budget
		// (remaining 0); only consumed > included exceeds it.
		status.Exceeded = consumed > plan.IncludedQuota
	}
	return status, nil
}

// ---------------------------------------------------------------------------
// Invoices
// ---------------------------------------------------------------------------
//
// Invoice math (documented):
//
//   - The invoice covers the exact half-open window [from, to) passed by the
//     caller (a subscription period, or any audited true-up window).
//   - run lines: one line per model bucket of the UsageSource aggregate —
//     quantity = metered runs of the bucket, amount_cents = round(cost_cents)
//     (half away from zero). refs records the model.
//   - eval lines: emitted only when a UsageSource reports source="eval" rows
//     (schema-supported seam — see LineSourceEval).
//   - overage line: when the plan has included_quota > 0 AND the plan metadata
//     carries {"overage_run_rate_cents": N} with N > 0, metered run-source
//     runs beyond the included quota are priced quantity = runs - included,
//     amount = quantity * N; refs records the inputs.
//   - subtotal_cents = exact sum of line amounts. The recurring plan price
//     (price_cents) is catalog data and intentionally NOT an invoice line:
//     the line-source enum is run/eval/overage (no "plan" source in this
//     milestone; folding the subscription fee in is a future extension).
//   - Regeneration is idempotent per (org, from, to): the non-void invoice
//     covering the exact window is returned unchanged (created=false), backed
//     by uq_invoices_org_period in Postgres.

// GenerateInvoiceCtx builds (or replays) the invoice for the org over
// [from, to). The bool result is false when an existing non-void invoice for
// the exact window was returned unchanged.
func (s *Service) GenerateInvoiceCtx(ctx context.Context, orgID string, from, to time.Time) (*Invoice, bool, error) {
	if from.IsZero() || to.IsZero() || !from.Before(to) {
		return nil, false, fmt.Errorf("%w: from must be before to", ErrInvalidPeriod)
	}
	if to.Sub(from).Hours()/24 > maxInvoiceWindowDays {
		return nil, false, fmt.Errorf("%w: window exceeds the maximum of %d days", ErrInvalidPeriod, maxInvoiceWindowDays)
	}
	sub, err := s.GetCurrentSubscriptionCtx(ctx, orgID)
	if err != nil {
		return nil, false, err
	}
	plan, err := s.store.GetPlan(ctx, sub.PlanID)
	if err != nil {
		return nil, false, err
	}

	if existing, err := s.store.FindInvoiceByPeriod(ctx, orgID, from, to); err == nil && existing != nil {
		lines, err := s.store.GetInvoiceLines(ctx, existing.ID)
		if err != nil {
			return nil, false, err
		}
		existing.Lines = lines
		return existing, false, nil
	}

	var rows []UsageRow
	if s.usage != nil {
		rows, err = s.usage.UsageForPeriod(ctx, orgID, from, to)
		if err != nil {
			return nil, false, err
		}
	}

	now := s.now()
	inv := &Invoice{
		ID:             uuid.NewString(),
		OrganizationID: orgID,
		SubscriptionID: sub.ID,
		PeriodStart:    from,
		PeriodEnd:      to,
		Currency:       plan.Currency,
		Status:         InvoiceOpen,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	var runRuns int64
	for _, row := range rows {
		if row.Source == "" {
			row.Source = LineSourceRun
		}
		if row.Source != LineSourceRun && row.Source != LineSourceEval {
			return nil, false, fmt.Errorf("%w: %q", ErrInvalidUsageRow, row.Source)
		}
		if row.Source == LineSourceRun {
			runRuns += row.Runs
		}
		amount := int64(math.Round(row.CostCents))
		if row.Runs == 0 && amount == 0 {
			continue // an empty bucket is noise, not a billable line
		}
		line := &InvoiceLine{
			ID:             uuid.NewString(),
			InvoiceID:      inv.ID,
			OrganizationID: orgID,
			Source:         row.Source,
			Quantity:       row.Runs,
			AmountCents:    amount,
			CreatedAt:      now,
		}
		if row.Model != "" {
			line.Refs = map[string]any{"model": row.Model}
			line.Description = fmt.Sprintf("%s usage — model %s", row.Source, row.Model)
		} else {
			line.Description = fmt.Sprintf("%s usage", row.Source)
		}
		inv.Lines = append(inv.Lines, line)
	}

	// Overage: run-source runs beyond the included quota (included_quota > 0
	// only; 0 is the unlimited sentinel and never overages).
	if rate := overageRateCents(plan.Metadata); plan.IncludedQuota > 0 && runRuns > plan.IncludedQuota && rate > 0 {
		overage := runRuns - plan.IncludedQuota
		inv.Lines = append(inv.Lines, &InvoiceLine{
			ID:             uuid.NewString(),
			InvoiceID:      inv.ID,
			OrganizationID: orgID,
			Source:         LineSourceOverage,
			Description:    "overage — runs beyond the included quota",
			Quantity:       overage,
			AmountCents:    overage * rate,
			Refs: map[string]any{
				"included_quota":         plan.IncludedQuota,
				"overage_run_rate_cents": rate,
				"metered_runs":           runRuns,
			},
			CreatedAt: now,
		})
	}

	for _, line := range inv.Lines {
		inv.SubtotalCents += line.AmountCents
	}

	if err := s.store.CreateInvoice(ctx, inv); err != nil {
		return nil, false, err
	}
	return inv, true, nil
}

// overageRateCents reads the optional plan metadata knob
// {"overage_run_rate_cents": N}. JSON decoding yields float64; integer and
// json.Number shapes are accepted for programmatic callers. Anything else
// (or N <= 0) means "no overage pricing".
func overageRateCents(meta map[string]any) int64 {
	if len(meta) == 0 {
		return 0
	}
	// JSON decoding yields float64 for numbers; int/int64/json.Number shapes
	// are accepted for programmatic callers building metadata in Go.
	switch v := meta["overage_run_rate_cents"].(type) {
	case float64:
		if v > 0 {
			return int64(math.Round(v))
		}
	case int:
		if v > 0 {
			return int64(v)
		}
	case int64:
		if v > 0 {
			return v
		}
	case json.Number:
		if n, err := v.Int64(); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// ListInvoicesCtx returns the org's invoices, newest first (lines omitted —
// use GetInvoiceCtx for the full document).
func (s *Service) ListInvoicesCtx(ctx context.Context, orgID string) ([]*Invoice, error) {
	return s.store.ListInvoicesByOrg(ctx, orgID)
}

// GetInvoiceCtx returns one tenant-scoped invoice with its lines.
func (s *Service) GetInvoiceCtx(ctx context.Context, orgID, id string) (*Invoice, error) {
	inv, err := s.store.GetInvoice(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	lines, err := s.store.GetInvoiceLines(ctx, inv.ID)
	if err != nil {
		return nil, err
	}
	inv.Lines = lines
	return inv, nil
}

// SettleInvoiceCtx moves an open invoice to paid or void (terminal states;
// settled invoices are immutable). Voided invoices free their (org, period)
// idempotency slot (see uq_invoices_org_period).
func (s *Service) SettleInvoiceCtx(ctx context.Context, orgID, id, status string) (*Invoice, error) {
	if status != InvoicePaid && status != InvoiceVoid {
		return nil, fmt.Errorf("%w: status must be paid or void", ErrInvalidInvoiceState)
	}
	inv, err := s.store.GetInvoice(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	if inv.Status != InvoiceOpen {
		return nil, fmt.Errorf("%w: invoice is already %s", ErrInvalidInvoiceState, inv.Status)
	}
	if err := s.store.UpdateInvoiceStatus(ctx, orgID, id, status, s.now()); err != nil {
		return nil, err
	}
	inv.Status = status
	return inv, nil
}
