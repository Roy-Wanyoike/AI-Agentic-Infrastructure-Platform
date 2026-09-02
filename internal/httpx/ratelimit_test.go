package httpx

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"agentos/internal/observability"
)

func newTestRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, rdb
}

func rateLimitRequest(handler http.Handler, apiKey string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/runs", strings.NewReader("{}"))
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestRateLimitMiddlewareRedisAllowsUpToLimitThen429(t *testing.T) {
	_, rdb := newTestRedis(t)
	limiter := NewRateLimitMiddleware(rdb, observability.NewRateLimiter(120, time.Minute), 3, time.Minute)
	handler := limiter(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))

	for i := 0; i < 3; i++ {
		rec := rateLimitRequest(handler, "ak_test")
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i+1, rec.Code)
		}
	}

	rec := rateLimitRequest(handler, "ak_test")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 once limit exceeded, got %d", rec.Code)
	}
	retryAfter := rec.Header().Get("Retry-After")
	seconds, err := strconv.Atoi(strings.TrimSpace(retryAfter))
	if err != nil {
		t.Fatalf("Retry-After must be seconds as integer, got %q: %v", retryAfter, err)
	}
	if seconds < 1 || seconds > 60 {
		t.Fatalf("Retry-After out of range: %d", seconds)
	}

	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("429 body must be structured JSON, got %q: %v", rec.Body.String(), err)
	}
	if body.Error.Code != ErrorCodeRateLimited {
		t.Fatalf("expected error code %q, got %q", ErrorCodeRateLimited, body.Error.Code)
	}
	if body.Error.Message == "" {
		t.Fatal("expected non-empty error message")
	}

	// Remaining-requests header tracks the budget.
	if remaining := rec.Header().Get(XRateLimitRemaining); remaining != "0" {
		t.Fatalf("expected X-RateLimit-Remaining 0 on 429, got %q", remaining)
	}
	if limitHeader := rec.Header().Get(XRateLimitLimit); limitHeader != "3" {
		t.Fatalf("expected X-RateLimit-Limit 3, got %q", limitHeader)
	}
}

func TestRateLimitMiddlewareRedisKeyFormat(t *testing.T) {
	mr, rdb := newTestRedis(t)
	limiter := NewRateLimitMiddleware(rdb, nil, 10, time.Minute)
	handler := limiter(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	_ = rateLimitRequest(handler, "ak_bucketkey")
	found := false
	for _, key := range mr.Keys() {
		if strings.HasPrefix(key, "ratelimit:api:") && strings.Contains(key, "key:") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a ratelimit:api:key:<hash> key in redis, got %v", mr.Keys())
	}

	// Distinct identities get distinct buckets.
	rec := rateLimitRequest(handler, "ak_other")
	if rec.Code != http.StatusOK {
		t.Fatalf("second identity should be independent, got %d", rec.Code)
	}

	// Requests without credentials fall back to the client IP bucket.
	req := httptest.NewRequest(http.MethodGet, "/v1/agents", nil)
	req.RemoteAddr = "10.0.0.9:5555"
	recIP := httptest.NewRecorder()
	handler.ServeHTTP(recIP, req)
	if recIP.Code != http.StatusOK {
		t.Fatalf("IP identity should be allowed, got %d", recIP.Code)
	}
}

func TestRateLimitMiddlewareScopeFromContext(t *testing.T) {
	_, rdb := newTestRedis(t)
	limiter := NewRateLimitMiddleware(rdb, nil, 1, time.Minute)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	base := limiter(inner)
	// The scope must be pinned OUTSIDE (before) the limiter middleware so it
	// is present on the request context when the limiter reads it: same
	// identity, different scope -> independent buckets.
	scopedHandler := withRateLimitScopeFixed("execute", limiter(inner))

	req := httptest.NewRequest(http.MethodPost, "/v1/runs", nil)
	req.Header.Set("X-API-Key", "ak_scope")

	rec := httptest.NewRecorder()
	base.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first request in default scope should pass, got %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	base.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request in default scope should be limited, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	scopedHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("execute scope bucket should be independent, got %d", rec.Code)
	}
}

// withRateLimitScopeFixed pins a scope for the wrapped handler (test helper
// mirroring what a route wrapper would do).
func withRateLimitScopeFixed(scope string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := WithRateLimitScope(r.Context(), scope)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func TestRateLimitMiddlewareFallsBackWhenRedisDown(t *testing.T) {
	// Redis client pointed at a closed port: every script call fails.
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 50 * time.Millisecond, MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	fallback := observability.NewRateLimiter(2, time.Minute)
	limiter := NewRateLimitMiddleware(rdb, fallback, 2, time.Minute)
	handler := limiter(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for i := 0; i < 2; i++ {
		if rec := rateLimitRequest(handler, "ak_fallback"); rec.Code != http.StatusOK {
			t.Fatalf("request %d through fallback should pass, got %d", i+1, rec.Code)
		}
	}
	rec := rateLimitRequest(handler, "ak_fallback")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("fallback limiter should enforce, got %d", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("fallback 429 must include Retry-After")
	}
}

func TestRateLimitMiddlewareNilRedisUsesFallback(t *testing.T) {
	fallback := observability.NewRateLimiter(1, time.Minute)
	limiter := NewRateLimitMiddleware(nil, fallback, 1, time.Minute)
	handler := limiter(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	if rec := rateLimitRequest(handler, "ak_mem"); rec.Code != http.StatusOK {
		t.Fatalf("first request should pass, got %d", rec.Code)
	}
	if rec := rateLimitRequest(handler, "ak_mem"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("in-memory fallback should enforce the limit, got %d", rec.Code)
	}
}

func TestRateLimitMiddlewareNoBackendFailsOpen(t *testing.T) {
	limiter := NewRateLimitMiddleware(nil, nil, 1, time.Minute)
	handler := limiter(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	for i := 0; i < 5; i++ {
		if rec := rateLimitRequest(handler, "ak_open"); rec.Code != http.StatusOK {
			t.Fatalf("fail-open mode should allow request %d, got %d", i+1, rec.Code)
		}
	}
}

func TestRateLimitMiddlewareDisabledConfigPassesThrough(t *testing.T) {
	_, rdb := newTestRedis(t)
	limiter := NewRateLimitMiddleware(rdb, nil, 0, time.Minute)
	called := false
	handler := limiter(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	_ = rateLimitRequest(handler, "ak_disabled")
	if !called {
		t.Fatal("limit<=0 must disable the middleware and call the handler")
	}
}

func TestRateLimitFromEnv(t *testing.T) {
	t.Setenv("AGENTOS_RATE_LIMIT_RPM", "240")
	limit, window := RateLimitFromEnv()
	if limit != 240 {
		t.Fatalf("expected 240 from env, got %d", limit)
	}
	if window != time.Minute {
		t.Fatalf("expected 1m window, got %s", window)
	}

	t.Setenv("AGENTOS_RATE_LIMIT_RPM", "not-a-number")
	limit, _ = RateLimitFromEnv()
	if limit != 120 {
		t.Fatalf("invalid env value should fall back to 120, got %d", limit)
	}

	t.Setenv("AGENTOS_RATE_LIMIT_RPM", "")
	limit, _ = RateLimitFromEnv()
	if limit != 120 {
		t.Fatalf("empty env value should fall back to 120, got %d", limit)
	}
}

func TestRateLimitRedisScriptRejectsAndRecovers(t *testing.T) {
	mr, rdb := newTestRedis(t)
	key := "ratelimit:execute:key:abc"
	res := rateLimitScript.Run(t.Context(), rdb, []string{key}, time.Now().UnixMilli(), time.Minute.Milliseconds(), 2, "m1")
	vals, err := res.Slice()
	if err != nil {
		t.Fatalf("script failed: %v", err)
	}
	allowed, _ := vals[0].(int64)
	if allowed != 1 {
		t.Fatalf("first call should be allowed, got %v", vals)
	}

	res = rateLimitScript.Run(t.Context(), rdb, []string{key}, time.Now().UnixMilli(), time.Minute.Milliseconds(), 2, "m2")
	vals, _ = res.Slice()
	allowed, _ = vals[0].(int64)
	if allowed != 1 {
		t.Fatalf("second call should be allowed, got %v", vals)
	}

	res = rateLimitScript.Run(t.Context(), rdb, []string{key}, time.Now().UnixMilli(), time.Minute.Milliseconds(), 2, "m3")
	vals, _ = res.Slice()
	allowed, _ = vals[0].(int64)
	retryMs, _ := vals[1].(int64)
	if allowed != 0 {
		t.Fatalf("third call should be denied, got %v", vals)
	}
	if retryMs <= 0 || retryMs > time.Minute.Milliseconds() {
		t.Fatalf("retry window out of range: %d", retryMs)
	}

	// After the window passes the bucket drains.
	mr.FastForward(2 * time.Minute)
	res = rateLimitScript.Run(t.Context(), rdb, []string{key}, time.Now().Add(2*time.Minute).UnixMilli(), time.Minute.Milliseconds(), 2, "m4")
	vals, _ = res.Slice()
	allowed, _ = vals[0].(int64)
	if allowed != 1 {
		t.Fatalf("after fast-forward the request should be allowed, got %v", vals)
	}
}
