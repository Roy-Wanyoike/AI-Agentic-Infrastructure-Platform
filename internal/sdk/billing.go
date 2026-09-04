package sdk

// Billing resource (issue #54 CLI+SDK parity): the typed mirror of the
// billing HTTP surface in cmd/api/billing.go.
//
// Endpoints (all under /v1/billing, tenant taken from the credentials):
//
//	GET  /billing/plans               -> ListPlans
//	GET  /billing/subscription        -> GetSubscription (subscription + quota)
//	POST /billing/subscriptions       -> Subscribe (OWNER only)
//	POST /billing/subscription/cancel -> CancelSubscription (OWNER/ADMIN)
//	GET  /billing/invoices            -> ListInvoices (lines omitted)
//	GET  /billing/invoices/{id}       -> GetInvoice (with lines)
//
// The handler responses are snake_case map literals — every struct here is
// the exact typed projection of those shapes (billPlanView/billSubscriptionView/
// billQuotaView/billInvoiceView). Errors use the shared {"error":{...}}
// envelope and surface as *APIError (PLAN_NOT_FOUND, NO_SUBSCRIPTION, ...).

import (
	"context"
	"time"
)

// Plan is one entry of the global (non-tenant) plan catalog. IncludedQuota
// is the monthly included run budget; 0 means UNLIMITED (the documented
// sentinel).
type Plan struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	PriceCents    int64          `json:"price_cents"`
	Currency      string         `json:"currency"`
	IncludedQuota int64          `json:"included_quota"`
	Metadata      map[string]any `json:"metadata"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
}

// PlanList is the wrapped shape of GET /v1/billing/plans.
type PlanList struct {
	Plans []Plan `json:"plans"`
}

// Subscription is the per-tenant billing state (periods are half-open).
type Subscription struct {
	ID                string     `json:"id"`
	PlanID            string     `json:"plan_id"`
	Status            string     `json:"status"` // trial|active|past_due|canceled
	PeriodStart       time.Time  `json:"period_start"`
	PeriodEnd         time.Time  `json:"period_end"`
	CancelAtPeriodEnd bool       `json:"cancel_at_period_end"`
	CanceledAt        *time.Time `json:"canceled_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

// Quota is the monthly run-budget view the subscription endpoint reports
// alongside the subscription (GET /v1/billing/subscription → "quota").
type Quota struct {
	SubscriptionID string    `json:"subscription_id"`
	Status         string    `json:"status"`
	IncludedRuns   int64     `json:"included_runs"` // 0 = unlimited
	Unlimited      bool      `json:"unlimited"`
	ConsumedRuns   int64     `json:"consumed_runs"`
	RemainingRuns  int64     `json:"remaining_runs"`
	Exceeded       bool      `json:"exceeded"`
	PeriodStart    time.Time `json:"period_start"`
	PeriodEnd      time.Time `json:"period_end"`
}

// BillingStatus is the GET /v1/billing/subscription response: the org's
// current subscription plus its quota snapshot.
type BillingStatus struct {
	Subscription Subscription `json:"subscription"`
	Quota        Quota        `json:"quota"`
}

// InvoiceLine prices one metered source inside an invoice; Refs carries the
// pricing provenance ({"model":…} for run lines, {"included_quota",…} for
// overage lines).
type InvoiceLine struct {
	ID          string         `json:"id"`
	Source      string         `json:"source"` // run|eval|overage
	Description string         `json:"description"`
	Quantity    int64          `json:"quantity"`
	AmountCents int64          `json:"amount_cents"`
	Refs        map[string]any `json:"refs"`
	CreatedAt   time.Time      `json:"created_at"`
}

// Invoice is one billing document. Lines are only present on the detail
// response (GetInvoice); the listing omits them.
type Invoice struct {
	ID             string        `json:"id"`
	SubscriptionID string        `json:"subscription_id"`
	PeriodStart    time.Time     `json:"period_start"`
	PeriodEnd      time.Time     `json:"period_end"`
	SubtotalCents  int64         `json:"subtotal_cents"`
	Currency       string        `json:"currency"`
	Status         string        `json:"status"` // open|paid|void
	Lines          []InvoiceLine `json:"lines,omitempty"`
	CreatedAt      time.Time     `json:"created_at"`
	UpdatedAt      time.Time     `json:"updated_at"`
}

// InvoiceList is the wrapped shape of GET /v1/billing/invoices (newest
// first, lines omitted).
type InvoiceList struct {
	Invoices []Invoice `json:"invoices"`
}

// SubscribeRequest is the POST /v1/billing/subscriptions body.
type SubscribeRequest struct {
	PlanID string `json:"plan_id"`
}

// CancelSubscriptionRequest is the POST /v1/billing/subscription/cancel
// body. Immediate=false (the zero value, empty body included) schedules the
// cancel for the end of the running period.
type CancelSubscriptionRequest struct {
	Immediate bool `json:"immediate"`
}

// ListPlans returns the global plan catalog (GET /v1/billing/plans).
func (c *Client) ListPlans(ctx context.Context) (*PlanList, error) {
	var out PlanList
	if err := c.do(ctx, httpMethodGet, "/billing/plans", nil, nil, &out); err != nil {
		return nil, err
	}
	if out.Plans == nil {
		out.Plans = []Plan{}
	}
	return &out, nil
}

// GetSubscription returns the caller's current subscription plus its quota
// snapshot (GET /v1/billing/subscription). Canceled history stays readable
// (the read path falls back to the latest row). A NO_SUBSCRIPTION 404
// surfaces as *APIError.
func (c *Client) GetSubscription(ctx context.Context) (*BillingStatus, error) {
	var out BillingStatus
	if err := c.do(ctx, httpMethodGet, "/billing/subscription", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Subscribe commits the caller's org to a plan (POST /v1/billing/subscriptions,
// 201, trial state). OWNER only server-side; 409 SUBSCRIPTION_EXISTS on
// double-subscribe surfaces as *APIError.
func (c *Client) Subscribe(ctx context.Context, req SubscribeRequest) (*Subscription, error) {
	var out struct {
		Subscription Subscription `json:"subscription"`
	}
	if err := c.do(ctx, httpMethodPost, "/billing/subscriptions", nil, req, &out); err != nil {
		return nil, err
	}
	return &out.Subscription, nil
}

// CancelSubscription cancels the caller's subscription NOW (immediate) or at
// the end of the running period (POST /v1/billing/subscription/cancel).
func (c *Client) CancelSubscription(ctx context.Context, req CancelSubscriptionRequest) (*Subscription, error) {
	var out struct {
		Subscription Subscription `json:"subscription"`
	}
	if err := c.do(ctx, httpMethodPost, "/billing/subscription/cancel", nil, req, &out); err != nil {
		return nil, err
	}
	return &out.Subscription, nil
}

// ListInvoices returns the org's invoices, newest first, lines omitted
// (GET /v1/billing/invoices).
func (c *Client) ListInvoices(ctx context.Context) (*InvoiceList, error) {
	var out InvoiceList
	if err := c.do(ctx, httpMethodGet, "/billing/invoices", nil, nil, &out); err != nil {
		return nil, err
	}
	if out.Invoices == nil {
		out.Invoices = []Invoice{}
	}
	return &out, nil
}

// GetInvoice returns one tenant-scoped invoice with its lines
// (GET /v1/billing/invoices/{id}).
func (c *Client) GetInvoice(ctx context.Context, id string) (*Invoice, error) {
	var out struct {
		Invoice Invoice `json:"invoice"`
	}
	if err := c.do(ctx, httpMethodGet, "/billing/invoices/"+urlPathEscape(id), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out.Invoice, nil
}
