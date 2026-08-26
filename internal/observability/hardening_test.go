package observability

import (
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
