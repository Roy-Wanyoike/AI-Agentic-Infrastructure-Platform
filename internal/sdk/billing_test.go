package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

// TestBillingResource drives the full billing surface against one fake API,
// asserting routes, methods, request bodies and response decoding for every
// endpoint of cmd/api/billing.go.
func TestBillingResource(t *testing.T) {
	var reqBody map[string]any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/billing/subscription":
			_, _ = w.Write([]byte(`{"subscription":{"id":"sub-1","plan_id":"starter","status":"active",` +
				`"period_start":"2025-06-01T00:00:00Z","period_end":"2025-07-01T00:00:00Z",` +
				`"cancel_at_period_end":false,"created_at":"2025-06-01T00:00:00Z","updated_at":"2025-06-02T00:00:00Z"},` +
				`"quota":{"subscription_id":"sub-1","status":"active","included_runs":1000,"unlimited":false,` +
				`"consumed_runs":120,"remaining_runs":880,"exceeded":false,` +
				`"period_start":"2025-06-01T00:00:00Z","period_end":"2025-07-01T00:00:00Z"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/billing/plans":
			_, _ = w.Write([]byte(`{"plans":[{"id":"starter","name":"Starter","price_cents":2900,"currency":"usd",` +
				`"included_quota":1000,"metadata":{"seats":"5"},"created_at":"2025-01-01T00:00:00Z","updated_at":"2025-01-01T00:00:00Z"},` +
				`{"id":"scale","name":"Scale","price_cents":9900,"currency":"usd","included_quota":0,` +
				`"metadata":null,"created_at":"2025-01-01T00:00:00Z","updated_at":"2025-01-01T00:00:00Z"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/billing/invoices":
			_, _ = w.Write([]byte(`{"invoices":[{"id":"inv-1","subscription_id":"sub-1",` +
				`"period_start":"2025-06-01T00:00:00Z","period_end":"2025-07-01T00:00:00Z",` +
				`"subtotal_cents":1250,"currency":"usd","status":"paid",` +
				`"created_at":"2025-07-01T00:00:00Z","updated_at":"2025-07-01T00:00:00Z"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/billing/invoices/inv-1":
			_, _ = w.Write([]byte(`{"invoice":{"id":"inv-1","subscription_id":"sub-1",` +
				`"period_start":"2025-06-01T00:00:00Z","period_end":"2025-07-01T00:00:00Z",` +
				`"subtotal_cents":1250,"currency":"usd","status":"paid",` +
				`"lines":[{"id":"ln-1","source":"run","description":"runs","quantity":100,"amount_cents":1000,` +
				`"refs":{"model":"gpt-4o-mini"},"created_at":"2025-07-01T00:00:00Z"}],` +
				`"created_at":"2025-07-01T00:00:00Z","updated_at":"2025-07-01T00:00:00Z"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/billing/subscriptions":
			_ = json.NewDecoder(r.Body).Decode(&reqBody)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"subscription":{"id":"sub-2","plan_id":"starter","status":"trial",` +
				`"period_start":"2025-07-01T00:00:00Z","period_end":"2025-08-01T00:00:00Z",` +
				`"cancel_at_period_end":false,"created_at":"2025-07-01T00:00:00Z","updated_at":"2025-07-01T00:00:00Z"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/billing/subscription/cancel":
			_ = json.NewDecoder(r.Body).Decode(&reqBody)
			_, _ = w.Write([]byte(`{"subscription":{"id":"sub-1","plan_id":"starter","status":"canceled",` +
				`"period_start":"2025-06-01T00:00:00Z","period_end":"2025-07-01T00:00:00Z",` +
				`"cancel_at_period_end":true,"created_at":"2025-06-01T00:00:00Z","updated_at":"2025-06-30T00:00:00Z"}}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ctx := context.Background()

	status, err := c.GetSubscription(ctx)
	if err != nil {
		t.Fatalf("GetSubscription: %v", err)
	}
	if status.Subscription.ID != "sub-1" || status.Subscription.PlanID != "starter" {
		t.Errorf("subscription = %+v", status.Subscription)
	}
	if status.Quota.IncludedRuns != 1000 || status.Quota.ConsumedRuns != 120 ||
		status.Quota.RemainingRuns != 880 || status.Quota.Exceeded {
		t.Errorf("quota = %+v", status.Quota)
	}

	plans, err := c.ListPlans(ctx)
	if err != nil {
		t.Fatalf("ListPlans: %v", err)
	}
	if len(plans.Plans) != 2 || plans.Plans[0].PriceCents != 2900 {
		t.Errorf("plans = %+v", plans.Plans)
	}
	if plans.Plans[1].IncludedQuota != 0 || plans.Plans[1].Metadata != nil {
		t.Errorf("scale plan = %+v", plans.Plans[1])
	}

	invoices, err := c.ListInvoices(ctx)
	if err != nil {
		t.Fatalf("ListInvoices: %v", err)
	}
	if len(invoices.Invoices) != 1 || invoices.Invoices[0].Lines != nil {
		t.Errorf("invoice list should omit lines: %+v", invoices.Invoices)
	}

	inv, err := c.GetInvoice(ctx, "inv-1")
	if err != nil {
		t.Fatalf("GetInvoice: %v", err)
	}
	if inv.SubtotalCents != 1250 || len(inv.Lines) != 1 || inv.Lines[0].Source != "run" {
		t.Errorf("invoice detail = %+v", inv)
	}

	sub, err := c.Subscribe(ctx, SubscribeRequest{PlanID: "starter"})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if sub.ID != "sub-2" || sub.Status != "trial" {
		t.Errorf("subscribed = %+v", sub)
	}
	if reqBody["plan_id"] != "starter" {
		t.Errorf("subscribe body = %v", reqBody)
	}

	canceled, err := c.CancelSubscription(ctx, CancelSubscriptionRequest{Immediate: true})
	if err != nil {
		t.Fatalf("CancelSubscription: %v", err)
	}
	if canceled.Status != "canceled" || !canceled.CancelAtPeriodEnd {
		t.Errorf("canceled = %+v", canceled)
	}
	if reqBody["immediate"] != true {
		t.Errorf("cancel body = %v", reqBody)
	}
}

func TestBillingNoSubscriptionError(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"NO_SUBSCRIPTION","message":"no subscription for organization"}}`))
	})
	_, err := c.GetSubscription(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 404 || apiErr.Code != "NO_SUBSCRIPTION" {
		t.Fatalf("want 404 NO_SUBSCRIPTION *APIError, got %v", err)
	}
}
