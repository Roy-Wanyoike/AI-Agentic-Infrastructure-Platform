package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Tenant guards mirror migration 016: subscription/invoice reads always carry
// organization_id; plan reads are global (the catalog is shared, non-tenant
// data) while plan DELETE is refused while any subscription references it.
const (
	sqlInsertPlan = `INSERT INTO plans (id, name, price_cents, currency, included_quota, metadata, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8)`
	sqlListPlans  = `SELECT id, name, price_cents, currency, included_quota, COALESCE(metadata::text, ''), created_at, updated_at FROM plans ORDER BY created_at ASC, id ASC`
	sqlSelectPlan = `SELECT id, name, price_cents, currency, included_quota, COALESCE(metadata::text, ''), created_at, updated_at FROM plans WHERE id = $1`
	sqlUpdatePlan = `UPDATE plans SET name = $2, price_cents = $3, currency = $4, included_quota = $5, metadata = $6::jsonb, updated_at = $7 WHERE id = $1`
	// Delete is guarded: a plan any subscription points at is history and
	// cannot be removed (RowsAffected == 0 -> ErrPlanInUse in the caller).
	sqlDeletePlan = `DELETE FROM plans WHERE id = $1 AND NOT EXISTS (SELECT 1 FROM subscriptions s WHERE s.plan_id = plans.id)`

	sqlInsertSubscription = `INSERT INTO subscriptions (id, organization_id, plan_id, status, period_start, period_end, cancel_at_period_end, canceled_at, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`
	// Tenant guard: newest first so the service can pick the live row (at most
	// one by uq_subscriptions_one_live) or the latest history row.
	sqlSelectSubscriptionsByOrg = `SELECT id, organization_id, plan_id, status, period_start, period_end, cancel_at_period_end, canceled_at, created_at, updated_at FROM subscriptions WHERE organization_id = $1 ORDER BY created_at DESC, id DESC`
	sqlUpdateSubscription       = `UPDATE subscriptions SET plan_id = $2, status = $3, period_start = $4, period_end = $5, cancel_at_period_end = $6, canceled_at = $7, updated_at = $8 WHERE id = $1`

	// Rollover transition table — the exact mirror of the in-memory
	// memoryStore.RolloverDueSubscriptions. Predicates are disjoint and every
	// statement is idempotent per instant: each row moves at most one step
	// (the trial->past_due dunning conversion SHIFTS the period window into
	// the future, so the transitioned row no longer matches period_end <= now
	// and cannot cascade into the past_due->canceled statement below). All
	// four run in one transaction.
	sqlRolloverExpireTrials  = `UPDATE subscriptions SET status = 'past_due', period_start = period_end, period_end = period_end + make_interval(days => $2), updated_at = $1 WHERE status = 'trial' AND period_end <= $1`
	sqlRolloverCancelFlagged = `UPDATE subscriptions SET status = 'canceled', canceled_at = $1, cancel_at_period_end = FALSE, updated_at = $1 WHERE status = 'active' AND cancel_at_period_end AND period_end <= $1`
	sqlRolloverExpirePastDue = `UPDATE subscriptions SET status = 'canceled', canceled_at = $1, updated_at = $1 WHERE status = 'past_due' AND period_end <= $1`
	// Renewal: the window shifts to [old_end, old_end + periodDays) from the
	// same DefaultPeriodDays constant the in-memory path uses.
	sqlRolloverRenewActive = `UPDATE subscriptions SET period_start = period_end, period_end = period_end + make_interval(days => $2), cancel_at_period_end = FALSE, updated_at = $1 WHERE status = 'active' AND NOT cancel_at_period_end AND period_end <= $1`

	sqlInsertInvoice       = `INSERT INTO invoices (id, organization_id, subscription_id, period_start, period_end, subtotal_cents, currency, status, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`
	sqlInsertInvoiceLine   = `INSERT INTO invoice_lines (id, invoice_id, organization_id, source, description, quantity, amount_cents, refs, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9)`
	sqlSelectInvoicesByOrg = `SELECT id, organization_id, subscription_id, period_start, period_end, subtotal_cents, currency, status, created_at, updated_at FROM invoices WHERE organization_id = $1 ORDER BY created_at DESC, id DESC`
	// Tenant guard: single-invoice reads are scoped to one organization_id.
	sqlSelectInvoice = `SELECT id, organization_id, subscription_id, period_start, period_end, subtotal_cents, currency, status, created_at, updated_at FROM invoices WHERE id = $1 AND organization_id = $2`
	// Idempotency lookup backing uq_invoices_org_period (voided invoices never
	// block regeneration).
	sqlSelectInvoiceByPeriod = `SELECT id, organization_id, subscription_id, period_start, period_end, subtotal_cents, currency, status, created_at, updated_at FROM invoices WHERE organization_id = $1 AND period_start = $2 AND period_end = $3 AND status <> 'void' ORDER BY created_at DESC, id DESC LIMIT 1`
	sqlSelectInvoiceLines    = `SELECT id, invoice_id, organization_id, source, description, quantity, amount_cents, COALESCE(refs::text, ''), created_at FROM invoice_lines WHERE invoice_id = $1 ORDER BY created_at ASC, id ASC`
	// Tenant guard: status flips are scoped to one organization_id.
	sqlUpdateInvoiceStatus = `UPDATE invoices SET status = $2, updated_at = $3 WHERE id = $1 AND organization_id = $4`
)

// pgStore is the Postgres-backed Store implementation (migration 016).
type pgStore struct {
	db *sql.DB
}

// NewPostgresStore returns a Store backed by *sql.DB (lib/pq driver).
func NewPostgresStore(db *sql.DB) Store {
	return &pgStore{db: db}
}

func (s *pgStore) guard() error {
	if s == nil || s.db == nil {
		return errors.New("billing: database is nil")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Plans
// ---------------------------------------------------------------------------

func (s *pgStore) CreatePlan(ctx context.Context, plan *Plan) error {
	if err := s.guard(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, sqlInsertPlan,
		plan.ID, plan.Name, plan.PriceCents, plan.Currency, plan.IncludedQuota,
		metadataParam(plan.Metadata), plan.CreatedAt, plan.UpdatedAt)
	return err
}

func (s *pgStore) ListPlans(ctx context.Context) ([]*Plan, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, sqlListPlans)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*Plan, 0)
	for rows.Next() {
		plan, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, plan)
	}
	return out, rows.Err()
}

func (s *pgStore) GetPlan(ctx context.Context, id string) (*Plan, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	return scanPlan(s.db.QueryRowContext(ctx, sqlSelectPlan, id))
}

func (s *pgStore) UpdatePlan(ctx context.Context, plan *Plan) error {
	if err := s.guard(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlUpdatePlan,
		plan.ID, plan.Name, plan.PriceCents, plan.Currency, plan.IncludedQuota,
		metadataParam(plan.Metadata), plan.UpdatedAt)
	if err != nil {
		return err
	}
	return planAffected(res)
}

func (s *pgStore) DeletePlan(ctx context.Context, id string) error {
	if err := s.guard(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlDeletePlan, id)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		// Either unknown id or still referenced; the service pre-reads the
		// plan so this maps to the in-use refusal.
		return ErrPlanInUse
	}
	return nil
}

// scanner abstracts *sql.Rows and *sql.Row for the scan helpers.
type scanner interface {
	Scan(dest ...any) error
}

func scanPlan(sc scanner) (*Plan, error) {
	var p Plan
	var metadataRaw string
	if err := sc.Scan(&p.ID, &p.Name, &p.PriceCents, &p.Currency, &p.IncludedQuota,
		&metadataRaw, &p.CreatedAt, &p.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrPlanNotFound
		}
		return nil, err
	}
	p.Metadata = metadataFromParam(metadataRaw)
	return &p, nil
}

// ---------------------------------------------------------------------------
// Subscriptions
// ---------------------------------------------------------------------------

func (s *pgStore) CreateSubscription(ctx context.Context, sub *Subscription) error {
	if err := s.guard(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, sqlInsertSubscription,
		sub.ID, sub.OrganizationID, sub.PlanID, sub.Status,
		sub.PeriodStart, sub.PeriodEnd, sub.CancelAtPeriodEnd, sub.CanceledAt,
		sub.CreatedAt, sub.UpdatedAt)
	return err
}

func (s *pgStore) ListSubscriptionsByOrg(ctx context.Context, orgID string) ([]*Subscription, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, sqlSelectSubscriptionsByOrg, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*Subscription, 0)
	for rows.Next() {
		sub, err := scanSubscription(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

func (s *pgStore) UpdateSubscription(ctx context.Context, sub *Subscription) error {
	if err := s.guard(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlUpdateSubscription,
		sub.ID, sub.PlanID, sub.Status, sub.PeriodStart, sub.PeriodEnd,
		sub.CancelAtPeriodEnd, sub.CanceledAt, sub.UpdatedAt)
	if err != nil {
		return err
	}
	return subscriptionAffected(res)
}

// RolloverDueSubscriptions applies the four due-period transitions in one
// transaction; see sqlRollover* for the predicate table.
func (s *pgStore) RolloverDueSubscriptions(ctx context.Context, now time.Time, periodDays int) (int64, error) {
	if err := s.guard(); err != nil {
		return 0, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var total int64
	for _, stmt := range []struct {
		query string
		args  []any
	}{
		{sqlRolloverExpireTrials, []any{now, periodDays}},
		{sqlRolloverCancelFlagged, []any{now}},
		{sqlRolloverExpirePastDue, []any{now}},
		{sqlRolloverRenewActive, []any{now, periodDays}},
	} {
		res, err := tx.ExecContext(ctx, stmt.query, stmt.args...)
		if err != nil {
			return 0, err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return 0, err
		}
		total += affected
	}
	return total, tx.Commit()
}

func scanSubscription(sc scanner) (*Subscription, error) {
	var sub Subscription
	var canceledAt sql.NullTime
	if err := sc.Scan(&sub.ID, &sub.OrganizationID, &sub.PlanID, &sub.Status,
		&sub.PeriodStart, &sub.PeriodEnd, &sub.CancelAtPeriodEnd, &canceledAt,
		&sub.CreatedAt, &sub.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNoSubscription
		}
		return nil, err
	}
	if canceledAt.Valid {
		t := canceledAt.Time
		sub.CanceledAt = &t
	}
	return &sub, nil
}

// ---------------------------------------------------------------------------
// Invoices
// ---------------------------------------------------------------------------

// CreateInvoice persists the document and every line in one transaction
// (all-or-nothing: an invoice never exists without its lines).
func (s *pgStore) CreateInvoice(ctx context.Context, inv *Invoice) error {
	if err := s.guard(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, sqlInsertInvoice,
		inv.ID, inv.OrganizationID, inv.SubscriptionID, inv.PeriodStart,
		inv.PeriodEnd, inv.SubtotalCents, inv.Currency, inv.Status,
		inv.CreatedAt, inv.UpdatedAt); err != nil {
		return err
	}
	for _, line := range inv.Lines {
		if _, err := tx.ExecContext(ctx, sqlInsertInvoiceLine,
			line.ID, line.InvoiceID, line.OrganizationID, line.Source,
			line.Description, line.Quantity, line.AmountCents,
			metadataParam(line.Refs), line.CreatedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *pgStore) ListInvoicesByOrg(ctx context.Context, orgID string) ([]*Invoice, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, sqlSelectInvoicesByOrg, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*Invoice, 0)
	for rows.Next() {
		inv, err := scanInvoice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

func (s *pgStore) GetInvoice(ctx context.Context, orgID, id string) (*Invoice, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	// Tenant guard: WHERE id = $1 AND organization_id = $2.
	return scanInvoice(s.db.QueryRowContext(ctx, sqlSelectInvoice, id, orgID))
}

func (s *pgStore) FindInvoiceByPeriod(ctx context.Context, orgID string, periodStart, periodEnd time.Time) (*Invoice, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	return scanInvoice(s.db.QueryRowContext(ctx, sqlSelectInvoiceByPeriod, orgID, periodStart, periodEnd))
}

func (s *pgStore) GetInvoiceLines(ctx context.Context, invoiceID string) ([]*InvoiceLine, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, sqlSelectInvoiceLines, invoiceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*InvoiceLine, 0)
	for rows.Next() {
		var line InvoiceLine
		var refsRaw string
		if err := rows.Scan(&line.ID, &line.InvoiceID, &line.OrganizationID,
			&line.Source, &line.Description, &line.Quantity, &line.AmountCents,
			&refsRaw, &line.CreatedAt); err != nil {
			return nil, err
		}
		line.Refs = metadataFromParam(refsRaw)
		out = append(out, &line)
	}
	return out, rows.Err()
}

func (s *pgStore) UpdateInvoiceStatus(ctx context.Context, orgID, id, status string, updatedAt time.Time) error {
	if err := s.guard(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlUpdateInvoiceStatus, id, status, updatedAt, orgID)
	if err != nil {
		return err
	}
	return invoiceAffected(res)
}

func scanInvoice(sc scanner) (*Invoice, error) {
	var inv Invoice
	if err := sc.Scan(&inv.ID, &inv.OrganizationID, &inv.SubscriptionID,
		&inv.PeriodStart, &inv.PeriodEnd, &inv.SubtotalCents, &inv.Currency,
		&inv.Status, &inv.CreatedAt, &inv.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrInvoiceNotFound
		}
		return nil, err
	}
	return &inv, nil
}

// ---------------------------------------------------------------------------
// shared param/scan helpers
// ---------------------------------------------------------------------------

// metadataParam marshals the map for a JSONB column; nil stays NULL.
func metadataParam(meta map[string]any) any {
	if len(meta) == 0 {
		return nil
	}
	b, err := json.Marshal(meta)
	if err != nil {
		return nil
	}
	return string(b)
}

func metadataFromParam(raw string) map[string]any {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		return nil
	}
	return meta
}

func planAffected(res sql.Result) error {
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrPlanNotFound
	}
	return nil
}

func subscriptionAffected(res sql.Result) error {
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrNoSubscription
	}
	return nil
}

func invoiceAffected(res sql.Result) error {
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrInvoiceNotFound
	}
	return nil
}
