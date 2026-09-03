package observability

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRateLimiterAllowsWithinLimit(t *testing.T) {
	limiter := NewRateLimiter(2, time.Minute)
	if !limiter.Allow("client-1") {
		t.Fatal("first request should be allowed")
	}
	if !limiter.Allow("client-1") {
		t.Fatal("second request should be allowed")
	}
	if limiter.Allow("client-1") {
		t.Fatal("third request should be rejected once limit is reached")
	}
}

func TestSecurityHeadersMiddlewareAddsSafetyHeaders(t *testing.T) {
	handler := SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, response.Code)
	}
	if got := response.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Fatalf("expected X-Frame-Options DENY, got %q", got)
	}
	if got := response.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("expected X-Content-Type-Options nosniff, got %q", got)
	}
	if got := response.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("expected Access-Control-Allow-Origin *, got %q", got)
	}
	if got := response.Header().Get("Content-Security-Policy"); got == "" {
		t.Fatal("expected content security policy header to be set")
	}
}

func TestQuotaAllowsWithinLimitAndRejectsAfterLimit(t *testing.T) {
	quota := NewQuota(2, time.Minute)
	if !quota.Allow("tenant-a") {
		t.Fatal("first quota check should be allowed")
	}
	if !quota.Allow("tenant-a") {
		t.Fatal("second quota check should be allowed")
	}
	if quota.Allow("tenant-a") {
		t.Fatal("third quota check should be rejected once the tenant limit is reached")
	}
	if got := quota.Used("tenant-a"); got != 2 {
		t.Fatalf("expected quota usage to be 2, got %d", got)
	}
}

// --- Task 1-c: HTTP metrics middleware ----------------------------------------

func TestRouteNameContextHelpers(t *testing.T) {
	if got := RouteNameFromContext(context.Background()); got != "" {
		t.Fatalf("expected empty route name, got %q", got)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/agents", nil)
	tagged := RequestWithRouteName(req, "/v1/agents")
	if got := RouteNameFromContext(tagged.Context()); got != "/v1/agents" {
		t.Fatalf("expected route name in context, got %q", got)
	}
	// Original request must be untouched.
	if got := RouteNameFromContext(req.Context()); got != "" {
		t.Fatalf("expected original request context untouched, got %q", got)
	}
	// Empty route name is a no-op.
	if RequestWithRouteName(req, "").Context() != req.Context() {
		t.Fatal("empty route name should not modify the context")
	}
	// RouteName wrapper tags requests passing through.
	var seen string
	handler := RouteName("/v1/runs", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RouteNameFromContext(r.Context())
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/runs", nil))
	if seen != "/v1/runs" {
		t.Fatalf("expected RouteName wrapper to tag request, got %q", seen)
	}
}

func TestMetricsMiddlewareRecordsCountAndDuration(t *testing.T) {
	metrics := NewMetrics()
	handler := MetricsMiddleware(metrics, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	handler = RouteName("/v1/agents/create", handler)

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/agents/create", nil))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/agents/create", nil))

	counts, latency := metrics.Snapshot()
	countKey := HTTPCountKey("/v1/agents/create", http.MethodPost, http.StatusCreated)
	if got := counts[countKey]; got != 2 {
		t.Fatalf("expected count 2 for %q, got %d (snapshot: %v)", countKey, got, counts)
	}
	durationKey := HTTPDurationKey("/v1/agents/create", http.MethodPost, http.StatusCreated)
	if _, ok := latency[durationKey]; !ok {
		t.Fatalf("expected duration observation for %q, snapshot: %v", durationKey, latency)
	}
	if got := latency[durationKey]; got < 0 {
		t.Fatalf("expected non-negative duration, got %v", got)
	}
}

func TestMetricsMiddlewareFallsBackToUnmatched(t *testing.T) {
	metrics := NewMetrics()
	handler := MetricsMiddleware(metrics, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/nowhere", nil))

	counts, _ := metrics.Snapshot()
	if got := counts[HTTPCountKey("unmatched", http.MethodGet, http.StatusTeapot)]; got != 1 {
		t.Fatalf("expected unmatched fallback count 1, snapshot: %v", counts)
	}
}

func TestMetricsMiddlewareDefaultsMissingStatusTo200(t *testing.T) {
	metrics := NewMetrics()
	handler := MetricsMiddleware(metrics, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Write body without explicit WriteHeader -> implicit 200.
		_, _ = w.Write([]byte("ok"))
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))

	counts, _ := metrics.Snapshot()
	if got := counts[HTTPCountKey("unmatched", http.MethodGet, http.StatusOK)]; got != 1 {
		t.Fatalf("expected implicit 200 recorded, snapshot: %v", counts)
	}
}

func TestMetricsMiddlewareNilMetricsPassesThrough(t *testing.T) {
	handler := MetricsMiddleware(nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("expected pass-through with nil metrics, got %d", rec.Code)
	}
}
