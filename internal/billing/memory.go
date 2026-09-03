package billing

import (
	"context"
	"sync"
	"time"
)

// memoryStore is the zero-infrastructure Store: plain maps under one mutex,
// mirroring the Postgres semantics that the schema enforces there:
//   - one LIVE subscription per organization (uq_subscriptions_one_live)
//   - one non-void invoice per (organization, period_start, period_end)
//     (uq_invoices_org_period)
//   - plan deletion refused while subscriptions reference the plan
type memoryStore struct {
	mu    sync.Mutex
	plans []*Plan         // creation order
	subs  []*Subscription // append order (creation order)
	invs  []*Invoice      // append order (creation order); Lines attached
}

func newMemoryStore() Store { return &memoryStore{} }

func (m *memoryStore) CreatePlan(_ context.Context, plan *Plan) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.plans {
		if p.Name == plan.Name {
			return ErrPlanExists
		}
	}
	cp := *plan
	m.plans = append(m.plans, &cp)
	return nil
}

func (m *memoryStore) ListPlans(_ context.Context) ([]*Plan, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Plan, 0, len(m.plans))
	for _, p := range m.plans {
		cp := *p
		out = append(out, &cp)
	}
	return out, nil
}

func (m *memoryStore) GetPlan(_ context.Context, id string) (*Plan, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.plans {
		if p.ID == id {
			cp := *p
			return &cp, nil
		}
	}
	return nil, ErrPlanNotFound
}

func (m *memoryStore) UpdatePlan(_ context.Context, plan *Plan) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.plans {
		if p.ID == plan.ID {
			continue
		}
		if p.Name == plan.Name {
			return ErrPlanExists
		}
	}
	for i, p := range m.plans {
		if p.ID == plan.ID {
			cp := *plan
			m.plans[i] = &cp
			return nil
		}
	}
	return ErrPlanNotFound
}

func (m *memoryStore) DeletePlan(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, sub := range m.subs {
		if sub.PlanID == id {
			return ErrPlanInUse
		}
	}
	for i, p := range m.plans {
		if p.ID == id {
			m.plans = append(m.plans[:i], m.plans[i+1:]...)
			return nil
		}
	}
	return ErrPlanNotFound
}

func (m *memoryStore) CreateSubscription(_ context.Context, sub *Subscription) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.subs {
		if s.OrganizationID == sub.OrganizationID && isLiveStatus(s.Status) && isLiveStatus(sub.Status) {
			return ErrSubscriptionExists
		}
	}
	cp := *sub
	m.subs = append(m.subs, &cp)
	return nil
}

// ListSubscriptionsByOrg returns the org's subscriptions newest first.
func (m *memoryStore) ListSubscriptionsByOrg(_ context.Context, orgID string) ([]*Subscription, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Subscription, 0)
	for i := len(m.subs) - 1; i >= 0; i-- {
		if m.subs[i].OrganizationID == orgID {
			cp := *m.subs[i]
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (m *memoryStore) UpdateSubscription(_ context.Context, sub *Subscription) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, s := range m.subs {
		if s.ID == sub.ID {
			cp := *sub
			m.subs[i] = &cp
			return nil
		}
	}
	return ErrNoSubscription
}

// RolloverDueSubscriptions applies the same transition table as the Postgres
// implementation (see pgStore.RolloverDueSubscriptions): trial -> past_due
// (window shifts so the dunning state opens a fresh grace period), active+
// cancel_at_period_end -> canceled, past_due -> canceled (grace exhausted),
// active -> renewed [old_end, old_end+periodDays). Every row moves at most one
// step per call and the predicates are disjoint, so the batch is idempotent
// per instant: after the transition no row matches period_end <= now anymore.
func (m *memoryStore) RolloverDueSubscriptions(_ context.Context, now time.Time, periodDays int) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var changed int64
	for _, sub := range m.subs {
		if sub.Status == StatusCanceled || sub.PeriodEnd.After(now) {
			continue
		}
		switch {
		case sub.Status == StatusTrial:
			// Trial expired without a payment method: past_due opens the
			// dunning window [old_end, old_end + periodDays) so the batch
			// stays idempotent per instant and dunning gets a full cycle
			// (mirrors the Postgres statement exactly).
			sub.Status = StatusPastDue
			sub.PeriodStart = sub.PeriodEnd
			sub.PeriodEnd = sub.PeriodEnd.Add(time.Duration(periodDays) * 24 * time.Hour)
		case sub.Status == StatusActive && sub.CancelAtPeriodEnd:
			sub.Status = StatusCanceled
			sub.CancelAtPeriodEnd = false
			sub.CanceledAt = &now
		case sub.Status == StatusPastDue:
			sub.Status = StatusCanceled
			sub.CanceledAt = &now
		case sub.Status == StatusActive:
			// Renew: shift the window, reset the cancel flag (fresh decision
			// per period — mirrors Stripe's renewal behavior).
			sub.PeriodStart = sub.PeriodEnd
			sub.PeriodEnd = sub.PeriodEnd.Add(time.Duration(periodDays) * 24 * time.Hour)
			sub.CancelAtPeriodEnd = false
		}
		sub.UpdatedAt = now
		changed++
	}
	return changed, nil
}

func (m *memoryStore) CreateInvoice(_ context.Context, inv *Invoice) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.invs {
		if e.OrganizationID == inv.OrganizationID &&
			e.Status != InvoiceVoid &&
			e.PeriodStart.Equal(inv.PeriodStart) &&
			e.PeriodEnd.Equal(inv.PeriodEnd) {
			return ErrInvoiceNotFound // slot taken; the service replays first
		}
	}
	cp := *inv
	cp.Lines = make([]*InvoiceLine, 0, len(inv.Lines))
	for _, line := range inv.Lines {
		lc := *line
		cp.Lines = append(cp.Lines, &lc)
	}
	m.invs = append(m.invs, &cp)
	return nil
}

// ListInvoicesByOrg returns the org's invoices newest first (lines omitted).
func (m *memoryStore) ListInvoicesByOrg(_ context.Context, orgID string) ([]*Invoice, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Invoice, 0)
	for i := len(m.invs) - 1; i >= 0; i-- {
		if m.invs[i].OrganizationID == orgID {
			cp := *m.invs[i]
			cp.Lines = nil
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (m *memoryStore) GetInvoice(_ context.Context, orgID, id string) (*Invoice, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Tenant guard: WHERE id = $1 AND organization_id = $2.
	for i := len(m.invs) - 1; i >= 0; i-- {
		if m.invs[i].ID == id && m.invs[i].OrganizationID == orgID {
			cp := *m.invs[i]
			cp.Lines = nil
			return &cp, nil
		}
	}
	return nil, ErrInvoiceNotFound
}

func (m *memoryStore) FindInvoiceByPeriod(_ context.Context, orgID string, periodStart, periodEnd time.Time) (*Invoice, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.invs) - 1; i >= 0; i-- {
		inv := m.invs[i]
		if inv.OrganizationID == orgID && inv.Status != InvoiceVoid &&
			inv.PeriodStart.Equal(periodStart) && inv.PeriodEnd.Equal(periodEnd) {
			cp := *inv
			cp.Lines = nil
			return &cp, nil
		}
	}
	return nil, ErrInvoiceNotFound
}

func (m *memoryStore) GetInvoiceLines(_ context.Context, invoiceID string) ([]*InvoiceLine, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, inv := range m.invs {
		if inv.ID != invoiceID {
			continue
		}
		out := make([]*InvoiceLine, 0, len(inv.Lines))
		for _, line := range inv.Lines {
			lc := *line
			out = append(out, &lc)
		}
		return out, nil
	}
	return nil, ErrInvoiceNotFound
}

func (m *memoryStore) UpdateInvoiceStatus(_ context.Context, orgID, id, status string, updatedAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, inv := range m.invs {
		if inv.ID == id && inv.OrganizationID == orgID {
			inv.Status = status
			inv.UpdatedAt = updatedAt
			return nil
		}
	}
	return ErrInvoiceNotFound
}
