package billing

// ---------------------------------------------------------------------------
// Per-scope budget gates (wave-8 issue C).
//
// A Budget is a tenant-configured spend ceiling over ONE scope: either the
// whole organization ("organization") or a single agent ("agent"). Budgets
// are monthly (calendar month, UTC) and evaluate consumption from the SAME
// durable ledger the usage report reads — runs.cost_cents / run counts per
// agent (CostGroupByAgent aggregation). Two knobs per budget:
//
//      LimitRuns      — projected run count ceiling (0 = unlimited)
//      LimitCostCents — projected cost ceiling in USD cents (0 = unlimited)
//
// MONEY UNIT (deliberate): the task brief said "micros", but it also says to
// CHECK how meters/margin represent cost and mirror it exactly — the entire
// billing stack (Plan.PriceCents, Invoice.AmountCents, runs.CostBucket.
// CostCents, MarginReport.MarginCents, migration 016 *_cents columns) is
// USD CENTS. Budgets use int64 cents so a budget evaluation always
// reconciles 1:1 with the usage-cost report and invoices of the same
// window; introducing a second money unit here would invite exactly the
// billing/meter drift this package documents against everywhere else.
//
// Enforcement posture: budgets are part of billing enforcement. They reuse
// EnforcementEnvVar (AGENTOS_BILLING_ENFORCEMENT, EnforcementFromEnv) — the
// SAME flag as quota enforcement — so operators cannot end up with quota
// enforcement on and budgets silently off (or vice versa). Budgets are an
// ADDITIVE check: the API evaluates them AFTER the quota gate and the worker
// AFTER the quota gate, so existing single-gate behavior is unchanged when
// no budget rows exist.
//
// Period semantics (documented choice): subscriptions reset on a rolling
// 30-day [period_start, period_end) window (DefaultPeriodDays) — meter data
// has no period of its own (MetersForPeriod takes an arbitrary window), so
// budgets use the CALENDAR MONTH [first day 00:00 UTC, first day of next
// month 00:00 UTC). Finance expects month-to-date showback; a rolling
// per-org window would make budgets of different orgs incomparable.
//
// Consumption lookup is INJECTED via ConsumptionSource (the service stays
// pure); the shipped adapter is RunsConsumptionSource over the per-agent
// cost aggregation. Source failures are PROPAGATED, never swallowed (same
// rule as quotaFor/margin: a faked 0 consumption would fake availability).
// ---------------------------------------------------------------------------

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"agentos/internal/runs"
)

// ReasonBudgetExceeded is the machine-readable denial reason for a budget
// gate refusal (parallel to ReasonQuotaExceeded): the API 402 error code,
// the worker's failed-run output slot and the audit metadata "reason" field.
const ReasonBudgetExceeded = "budget_exceeded"

// Budget scopes and the only supported period.
const (
	BudgetScopeOrganization = "organization"
	BudgetScopeAgent        = "agent"
	BudgetPeriodMonthly     = "monthly"
)

// Service errors — mapped onto the HTTP error envelope by cmd/api/budgets.go.
var (
	ErrBudgetNotFound = errors.New("billing: budget not found")
	// ErrBudgetConflict is the 409-style "at most one ACTIVE budget per
	// (organization, scope, ref)" violation: a second Create for an
	// already-budgeted scope (or an Update that activates a budget while a
	// sibling is active) is refused; update the existing budget instead.
	ErrBudgetConflict = errors.New("billing: an active budget already exists for this scope")
	ErrInvalidBudget  = errors.New("billing: invalid budget")
)

// Budget is one tenant-configured spend ceiling. Limits use the 0 = unlimited
// sentinel (mirroring Plan.IncludedQuota).
type Budget struct {
	ID             string
	OrganizationID string
	Scope          string // BudgetScopeOrganization | BudgetScopeAgent
	RefID          string // agent id for BudgetScopeAgent; empty for org scope
	Period         string // BudgetPeriodMonthly
	LimitRuns      int64  // 0 = unlimited
	LimitCostCents int64  // 0 = unlimited (USD cents — see the money-unit note above)
	IsActive       bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// BudgetInput is the validated create/update payload.
type BudgetInput struct {
	Scope          string
	RefID          string
	Period         string // empty -> BudgetPeriodMonthly
	LimitRuns      int64
	LimitCostCents int64
	IsActive       bool
}

// BudgetDecision is the outcome of EvaluateBudget (also the dry-run wire
// shape behind GET /budgets/evaluate).
type BudgetDecision struct {
	Allowed        bool
	DeniedBudgetID string // budget that refused the projection (empty when allowed)
	DeniedScope    string // scope of the refusing budget
	Reason         string // machine-readable: ReasonBudgetExceeded when denied
	Message        string // deterministic operator-facing message
}

// ConsumptionRow is one per-agent slice of metered consumption over a window.
// ToolCalls is carried for source parity with the usage meters; the shipped
// adapter leaves it 0 (no per-agent tool-call aggregate exists today — the
// documented-omission convention of meters.go).
type ConsumptionRow struct {
	AgentID   string
	Runs      int64
	ToolCalls int64
	CostCents float64
}

// ConsumptionSource is the budget-facing view of the metering layer. The
// service stays pure: callers inject any ledger; tests inject fakes.
type ConsumptionSource interface {
	ConsumptionForPeriod(ctx context.Context, orgID string, from, to time.Time) ([]ConsumptionRow, error)
}

// RunsCostAggregator is declared in usage.go (satisfied by
// *runs.Service.AggregateCostsCtx).

// RunsConsumptionSource adapts the runs service per-agent cost aggregation
// into budget consumption rows. Production wiring:
//
//	billing.NewRunsConsumptionSource(runsSvc)
//
// NIL-DEPENDENCY CONTRACT (mirrors RunsUsageSource): a nil adapter or nil
// aggregator contributes zero consumption instead of failing the read.
type RunsConsumptionSource struct {
	aggregator RunsCostAggregator
}

// NewRunsConsumptionSource wires the budget service to the runs cost ledger
// (group_by=agent: one row per agent with run count and token cost).
func NewRunsConsumptionSource(aggregator RunsCostAggregator) *RunsConsumptionSource {
	return &RunsConsumptionSource{aggregator: aggregator}
}

// ConsumptionForPeriod implements ConsumptionSource.
func (r *RunsConsumptionSource) ConsumptionForPeriod(ctx context.Context, orgID string, from, to time.Time) ([]ConsumptionRow, error) {
	if r == nil || r.aggregator == nil {
		return nil, nil
	}
	buckets, _, err := r.aggregator.AggregateCostsCtx(ctx, orgID, from, to, runs.CostGroupByAgent)
	if err != nil {
		return nil, err
	}
	out := make([]ConsumptionRow, 0, len(buckets))
	for _, bucket := range buckets {
		out = append(out, ConsumptionRow{
			AgentID:   bucket.AgentID,
			Runs:      bucket.Runs,
			CostCents: bucket.CostCents,
		})
	}
	return out, nil
}

// BudgetStore persists budget rows. Every tenant-scoped method filters by
// organization_id so a tenant can only ever observe its own budgets.
type BudgetStore interface {
	CreateBudget(ctx context.Context, budget *Budget) error
	// ListBudgets returns the org's budgets (creation order).
	ListBudgets(ctx context.Context, orgID string) ([]*Budget, error)
	// GetBudget resolves one budget within one tenant (organization_id guard).
	GetBudget(ctx context.Context, orgID, id string) (*Budget, error)
	UpdateBudget(ctx context.Context, budget *Budget) error
	DeleteBudget(ctx context.Context, orgID, id string) error
}

// BudgetService evaluates and manages per-scope budgets. Dual-mode like the
// rest of the package: NewBudgetService (in-memory) or
// NewBudgetServiceWithStore (Postgres store + consumption source).
type BudgetService struct {
	mu          sync.Mutex
	store       BudgetStore
	consumption ConsumptionSource
	nowFn       func() time.Time
}

// NewBudgetService returns the zero-infrastructure budget service. Without a
// ConsumptionSource every evaluation sees zero consumption (the same
// provenance as invoice generation without a UsageSource); wire one via
// NewBudgetServiceWithStore or SetConsumptionSource for ledger-backed math.
func NewBudgetService() *BudgetService {
	return newBudgetService(newMemoryBudgetStore(), nil)
}

// NewBudgetServiceWithStore returns a durable budget service. consumption may
// be nil (zero consumption; deterministic tests); production wiring passes
// billing.NewRunsConsumptionSource(runsSvc).
func NewBudgetServiceWithStore(store BudgetStore, consumption ConsumptionSource) *BudgetService {
	return newBudgetService(store, consumption)
}

func newBudgetService(store BudgetStore, consumption ConsumptionSource) *BudgetService {
	return &BudgetService{
		store:       store,
		consumption: consumption,
		nowFn:       func() time.Time { return time.Now().UTC() },
	}
}

// SetConsumptionSource swaps the metering source (nil disables ledger reads).
func (s *BudgetService) SetConsumptionSource(source ConsumptionSource) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.consumption = source
}

// SetClock overrides the wall clock (tests inject deterministic time).
func (s *BudgetService) SetClock(now func() time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if now != nil {
		s.nowFn = now
	}
}

func (s *BudgetService) now() time.Time {
	return s.nowFn().UTC()
}

// monthWindow returns the UTC calendar-month window containing now:
// [first day 00:00 UTC, first day of next month 00:00 UTC).
func monthWindow(now time.Time) (time.Time, time.Time) {
	now = now.UTC()
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	return from, from.AddDate(0, 1, 0)
}

func validateBudgetInput(in BudgetInput) (BudgetInput, error) {
	switch in.Scope {
	case BudgetScopeOrganization:
		in.RefID = "" // org budgets never carry a ref
	case BudgetScopeAgent:
		if strings.TrimSpace(in.RefID) == "" {
			return in, fmt.Errorf("%w: scope %q requires a non-empty ref_id (the agent id)", ErrInvalidBudget, BudgetScopeAgent)
		}
	default:
		return in, fmt.Errorf("%w: scope must be %q or %q", ErrInvalidBudget, BudgetScopeOrganization, BudgetScopeAgent)
	}
	if in.Period == "" {
		in.Period = BudgetPeriodMonthly
	}
	if in.Period != BudgetPeriodMonthly {
		return in, fmt.Errorf("%w: period must be %q", ErrInvalidBudget, BudgetPeriodMonthly)
	}
	if in.LimitRuns < 0 {
		return in, fmt.Errorf("%w: limit_runs must be >= 0", ErrInvalidBudget)
	}
	if in.LimitCostCents < 0 {
		return in, fmt.Errorf("%w: limit_cost_cents must be >= 0", ErrInvalidBudget)
	}
	if in.LimitRuns == 0 && in.LimitCostCents == 0 {
		return in, fmt.Errorf("%w: at least one of limit_runs or limit_cost_cents must be set (0 = unlimited)", ErrInvalidBudget)
	}
	return in, nil
}

// CreateBudgetCtx validates the input, enforces the one-active-budget-per-
// (scope, ref) invariant (ErrBudgetConflict) and persists the row.
func (s *BudgetService) CreateBudgetCtx(ctx context.Context, orgID string, in BudgetInput) (*Budget, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrBudgetNotFound
	}
	in, err := validateBudgetInput(in)
	if err != nil {
		return nil, err
	}
	if in.IsActive {
		if err := s.ensureNoActiveSibling(ctx, orgID, in.Scope, in.RefID, ""); err != nil {
			return nil, err
		}
	}
	now := s.now()
	budget := &Budget{
		ID:             uuid.NewString(),
		OrganizationID: orgID,
		Scope:          in.Scope,
		RefID:          in.RefID,
		Period:         in.Period,
		LimitRuns:      in.LimitRuns,
		LimitCostCents: in.LimitCostCents,
		IsActive:       in.IsActive,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := s.store.CreateBudget(ctx, budget); err != nil {
		return nil, err
	}
	return budget, nil
}

// ensureNoActiveSibling refuses a second ACTIVE budget for the same
// (organization, scope, ref). self is the budget id allowed to stay active
// (UpdateBudget passing itself); "" on create.
func (s *BudgetService) ensureNoActiveSibling(ctx context.Context, orgID, scope, refID, self string) error {
	existing, err := s.store.ListBudgets(ctx, orgID)
	if err != nil {
		return err
	}
	for _, b := range existing {
		if b.IsActive && b.Scope == scope && b.RefID == refID && b.ID != self {
			return fmt.Errorf("%w: budget %s (%s scope%s) is active", ErrBudgetConflict, b.ID, scope, refLabel(refID))
		}
	}
	return nil
}

func refLabel(refID string) string {
	if refID == "" {
		return ""
	}
	return ", ref " + refID
}

// ListBudgetsCtx returns the org's budgets, newest last (creation order).
// activeOnly filters to IsActive rows (the ?active=true wire filter).
func (s *BudgetService) ListBudgetsCtx(ctx context.Context, orgID string, activeOnly bool) ([]*Budget, error) {
	budgets, err := s.store.ListBudgets(ctx, orgID)
	if err != nil {
		return nil, err
	}
	if !activeOnly {
		return budgets, nil
	}
	out := make([]*Budget, 0, len(budgets))
	for _, b := range budgets {
		if b.IsActive {
			out = append(out, b)
		}
	}
	return out, nil
}

// GetBudgetCtx resolves one tenant-scoped budget.
func (s *BudgetService) GetBudgetCtx(ctx context.Context, orgID, id string) (*Budget, error) {
	return s.store.GetBudget(ctx, orgID, id)
}

// UpdateBudgetCtx rewrites the mutable fields of one tenant-scoped budget
// (full PUT semantics). Activating a budget for a scope that already has an
// active sibling is refused (ErrBudgetConflict); re-saving an already-active
// budget is fine.
func (s *BudgetService) UpdateBudgetCtx(ctx context.Context, orgID, id string, in BudgetInput) (*Budget, error) {
	budget, err := s.store.GetBudget(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	in, err = validateBudgetInput(in)
	if err != nil {
		return nil, err
	}
	if in.IsActive {
		if err := s.ensureNoActiveSibling(ctx, orgID, in.Scope, in.RefID, id); err != nil {
			return nil, err
		}
	}
	budget.Scope = in.Scope
	budget.RefID = in.RefID
	budget.Period = in.Period
	budget.LimitRuns = in.LimitRuns
	budget.LimitCostCents = in.LimitCostCents
	budget.IsActive = in.IsActive
	budget.UpdatedAt = s.now()
	if err := s.store.UpdateBudget(ctx, budget); err != nil {
		return nil, err
	}
	return budget, nil
}

// DeleteBudgetCtx removes one tenant-scoped budget.
func (s *BudgetService) DeleteBudgetCtx(ctx context.Context, orgID, id string) error {
	if _, err := s.store.GetBudget(ctx, orgID, id); err != nil {
		return err
	}
	return s.store.DeleteBudget(ctx, orgID, id)
}

// EvaluateBudget is the single budget decision point shared by the API's
// run-creation gate, the worker's pre-execution gate and the dry-run
// evaluate endpoint. It projects the requested work (projectedRuns runs,
// projectedCostCents cost) on top of the current calendar month's metered
// consumption and refuses when any ACTIVE budget covering the scope would be
// exceeded.
//
// Decision order is deterministic: organization-scope budgets are evaluated
// before agent-scope budgets; within one scope, earliest created_at then id
// (the policies-engine tiebreak convention). The FIRST violation wins and is
// reported in BudgetDecision.DeniedBudgetID/DeniedScope.
//
// An empty agentID only ever matches organization-scope budgets (the dry-run
// dashboard use case). Limit == 0 means unlimited and never denies.
func (s *BudgetService) EvaluateBudget(ctx context.Context, orgID, agentID string, projectedRuns int64, projectedCostCents int64) (*BudgetDecision, error) {
	budgets, err := s.store.ListBudgets(ctx, orgID)
	if err != nil {
		return nil, err
	}
	if len(budgets) == 0 {
		return &BudgetDecision{Allowed: true}, nil
	}

	// Deterministic candidate order: organization first, then agent; within a
	// scope by created_at then id (creation order already satisfies this for
	// the memory store; sort defensively so Postgres ordering can never flip
	// the decision).
	candidates := orderBudgetsForEvaluation(budgets)
	if len(candidates) == 0 {
		return &BudgetDecision{Allowed: true}, nil
	}

	from, to := monthWindow(s.now())
	rows, err := s.consumptionForWindow(ctx, orgID, from, to)
	if err != nil {
		return nil, err
	}

	var orgRuns int64
	var orgCost float64
	var agentRuns int64
	var agentCost float64
	for _, row := range rows {
		orgRuns += row.Runs
		orgCost += row.CostCents
		if agentID != "" && row.AgentID == agentID {
			agentRuns += row.Runs
			agentCost += row.CostCents
		}
	}

	for _, budget := range candidates {
		var projectedRunsTotal, projectedCostTotal int64
		switch budget.Scope {
		case BudgetScopeOrganization:
			projectedRunsTotal = orgRuns + projectedRuns
			projectedCostTotal = roundCents(orgCost) + projectedCostCents
		case BudgetScopeAgent:
			if agentID == "" || budget.RefID != agentID {
				continue
			}
			projectedRunsTotal = agentRuns + projectedRuns
			projectedCostTotal = roundCents(agentCost) + projectedCostCents
		}
		if budget.LimitRuns > 0 && projectedRunsTotal > budget.LimitRuns {
			return deniedDecision(budget, budgetRunMessage(budget, budget.LimitRuns, projectedRunsTotal)), nil
		}
		if budget.LimitCostCents > 0 && projectedCostTotal > budget.LimitCostCents {
			return deniedDecision(budget, budgetCostMessage(budget, budget.LimitCostCents, projectedCostTotal)), nil
		}
	}
	return &BudgetDecision{Allowed: true}, nil
}

// consumptionForWindow reads the metered consumption over [from, to). A nil
// source contributes zero (zero-infrastructure mode, documented above);
// source ERRORS are propagated (never fake availability).
func (s *BudgetService) consumptionForWindow(ctx context.Context, orgID string, from, to time.Time) ([]ConsumptionRow, error) {
	s.mu.Lock()
	source := s.consumption
	s.mu.Unlock()
	if source == nil {
		return nil, nil
	}
	return source.ConsumptionForPeriod(ctx, orgID, from, to)
}

// orderBudgetsForEvaluation pins the deterministic candidate order:
// organization-scope budgets before agent-scope budgets, then created_at
// ascending, then id ascending.
func orderBudgetsForEvaluation(budgets []*Budget) []*Budget {
	out := make([]*Budget, 0, len(budgets))
	for _, b := range budgets {
		if b.IsActive {
			out = append(out, b)
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			a, b := out[j-1], out[j]
			if budgetOrderKey(a) <= budgetOrderKey(b) {
				break
			}
			out[j-1], out[j] = b, a
		}
	}
	return out
}

func budgetOrderKey(b *Budget) string {
	scopeRank := "0"
	if b.Scope == BudgetScopeAgent {
		scopeRank = "1"
	}
	return scopeRank + "|" + b.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + b.ID
}

func roundCents(v float64) int64 {
	return int64(math.Round(v))
}

func deniedDecision(budget *Budget, message string) *BudgetDecision {
	return &BudgetDecision{
		Allowed:        false,
		DeniedBudgetID: budget.ID,
		DeniedScope:    budget.Scope,
		Reason:         ReasonBudgetExceeded,
		Message:        message,
	}
}

// BudgetExceededMessage renders the decision's operator-facing message
// (deterministic, mirroring QuotaExceededMessage; the API 402 body and the
// worker audit metadata carry it verbatim).
func BudgetExceededMessage(d *BudgetDecision) string {
	return d.Message
}

func budgetRunMessage(budget *Budget, limit, projected int64) string {
	return fmt.Sprintf(
		"monthly run budget exceeded: budget %s (%s scope) allows %d runs, projected %d runs for the current calendar month; raise the budget limit or wait for the month to reset",
		budget.ID, budget.Scope, limit, projected,
	)
}

func budgetCostMessage(budget *Budget, limit, projected int64) string {
	return fmt.Sprintf(
		"monthly cost budget exceeded: budget %s (%s scope) allows %d cost cents, projected %d cost cents for the current calendar month; raise the budget limit or wait for the month to reset",
		budget.ID, budget.Scope, limit, projected,
	)
}

// ---------------------------------------------------------------------------
// In-memory BudgetStore (zero-infrastructure mode)
// ---------------------------------------------------------------------------

type memoryBudgetStore struct {
	mu      sync.Mutex
	budgets []*Budget // creation order
}

func newMemoryBudgetStore() BudgetStore { return &memoryBudgetStore{} }

func (m *memoryBudgetStore) CreateBudget(_ context.Context, budget *Budget) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, b := range m.budgets {
		if b.IsActive && budget.IsActive && b.OrganizationID == budget.OrganizationID &&
			b.Scope == budget.Scope && b.RefID == budget.RefID {
			return ErrBudgetConflict
		}
	}
	cp := *budget
	m.budgets = append(m.budgets, &cp)
	return nil
}

func (m *memoryBudgetStore) ListBudgets(_ context.Context, orgID string) ([]*Budget, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Budget, 0)
	for _, b := range m.budgets {
		if b.OrganizationID == orgID {
			cp := *b
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (m *memoryBudgetStore) GetBudget(_ context.Context, orgID, id string) (*Budget, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, b := range m.budgets {
		if b.ID == id && b.OrganizationID == orgID {
			cp := *b
			return &cp, nil
		}
	}
	return nil, ErrBudgetNotFound
}

func (m *memoryBudgetStore) UpdateBudget(_ context.Context, budget *Budget) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, b := range m.budgets {
		if b.ID == budget.ID && b.OrganizationID == budget.OrganizationID {
			if budget.IsActive {
				for _, other := range m.budgets {
					if other.ID != budget.ID && other.IsActive &&
						other.OrganizationID == budget.OrganizationID &&
						other.Scope == budget.Scope && other.RefID == budget.RefID {
						return ErrBudgetConflict
					}
				}
			}
			cp := *budget
			m.budgets[i] = &cp
			return nil
		}
	}
	return ErrBudgetNotFound
}

func (m *memoryBudgetStore) DeleteBudget(_ context.Context, orgID, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, b := range m.budgets {
		if b.ID == id && b.OrganizationID == orgID {
			m.budgets = append(m.budgets[:i], m.budgets[i+1:]...)
			return nil
		}
	}
	return ErrBudgetNotFound
}

// ---------------------------------------------------------------------------
// Postgres BudgetStore (migration 026) — mirrors the pgStore pattern of
// store.go verbatim: SQL constants with tenant guards, RowsAffected checks
// mapped to service errors, COALESCE-free plain columns.
// ---------------------------------------------------------------------------

const (
	sqlInsertBudget = `INSERT INTO budgets (id, organization_id, scope, ref_id, period, limit_runs, limit_cost_cents, is_active, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`
	// Tenant guard: every read filters on organization_id.
	sqlSelectBudgetsByOrg = `SELECT id, organization_id, scope, ref_id, period, limit_runs, limit_cost_cents, is_active, created_at, updated_at FROM budgets WHERE organization_id = $1 ORDER BY created_at ASC, id ASC`
	sqlSelectBudget       = `SELECT id, organization_id, scope, ref_id, period, limit_runs, limit_cost_cents, is_active, created_at, updated_at FROM budgets WHERE id = $1 AND organization_id = $2`
	sqlUpdateBudget       = `UPDATE budgets SET scope = $2, ref_id = $3, period = $4, limit_runs = $5, limit_cost_cents = $6, is_active = $7, updated_at = $8 WHERE id = $1 AND organization_id = $9`
	sqlDeleteBudget       = `DELETE FROM budgets WHERE id = $1 AND organization_id = $2`
)

// BudgetPostgresStore is the durable BudgetStore (migration 026). The
// uq_budgets_one_active partial unique index is the durable backstop for the
// service-level ErrBudgetConflict check.
type BudgetPostgresStore struct {
	db *sql.DB
}

// NewBudgetPostgresStore returns a BudgetStore backed by *sql.DB (lib/pq).
func NewBudgetPostgresStore(db *sql.DB) BudgetStore {
	return &BudgetPostgresStore{db: db}
}

func (s *BudgetPostgresStore) guard() error {
	if s == nil || s.db == nil {
		return errors.New("billing: database is nil")
	}
	return nil
}

func (s *BudgetPostgresStore) CreateBudget(ctx context.Context, budget *Budget) error {
	if err := s.guard(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, sqlInsertBudget,
		budget.ID, budget.OrganizationID, budget.Scope, budget.RefID, budget.Period,
		budget.LimitRuns, budget.LimitCostCents, budget.IsActive, budget.CreatedAt, budget.UpdatedAt)
	return err
}

func (s *BudgetPostgresStore) ListBudgets(ctx context.Context, orgID string) ([]*Budget, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, sqlSelectBudgetsByOrg, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*Budget, 0)
	for rows.Next() {
		budget, err := scanBudget(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, budget)
	}
	return out, rows.Err()
}

func (s *BudgetPostgresStore) GetBudget(ctx context.Context, orgID, id string) (*Budget, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	// Tenant guard: WHERE id = $1 AND organization_id = $2.
	return scanBudget(s.db.QueryRowContext(ctx, sqlSelectBudget, id, orgID))
}

func (s *BudgetPostgresStore) UpdateBudget(ctx context.Context, budget *Budget) error {
	if err := s.guard(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlUpdateBudget,
		budget.ID, budget.Scope, budget.RefID, budget.Period, budget.LimitRuns,
		budget.LimitCostCents, budget.IsActive, budget.UpdatedAt, budget.OrganizationID)
	if err != nil {
		return err
	}
	return budgetAffected(res)
}

func (s *BudgetPostgresStore) DeleteBudget(ctx context.Context, orgID, id string) error {
	if err := s.guard(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlDeleteBudget, id, orgID)
	if err != nil {
		return err
	}
	return budgetAffected(res)
}

func scanBudget(sc scanner) (*Budget, error) {
	var b Budget
	if err := sc.Scan(&b.ID, &b.OrganizationID, &b.Scope, &b.RefID, &b.Period,
		&b.LimitRuns, &b.LimitCostCents, &b.IsActive, &b.CreatedAt, &b.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrBudgetNotFound
		}
		return nil, err
	}
	return &b, nil
}

func budgetAffected(res sql.Result) error {
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrBudgetNotFound
	}
	return nil
}
