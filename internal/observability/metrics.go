package observability

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"
)

type Metrics struct {
	mu         sync.Mutex
	counts     map[string]int64
	latency    map[string]float64
	histograms map[string]*Histogram // bucketed observations (Task 2-h)
}

type RateLimiter struct {
	mu       sync.Mutex
	limit    int
	window   time.Duration
	requests map[string][]time.Time
}

type Quota struct {
	mu       sync.Mutex
	limit    int
	window   time.Duration
	requests map[string][]time.Time
}

func NewMetrics() *Metrics {
	return &Metrics{
		counts:     make(map[string]int64),
		latency:    make(map[string]float64),
		histograms: make(map[string]*Histogram),
	}
}

func (m *Metrics) Inc(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counts[name]++
}

func (m *Metrics) Observe(name string, value float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.latency[name] = value
}

func (m *Metrics) Snapshot() (map[string]int64, map[string]float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	counts := make(map[string]int64, len(m.counts))
	latency := make(map[string]float64, len(m.latency))
	for k, v := range m.counts {
		counts[k] = v
	}
	for k, v := range m.latency {
		latency[k] = v
	}
	return counts, latency
}

func NewRateLimiter(limit int, window time.Duration) *RateLimiter {
	if limit <= 0 {
		limit = 1
	}
	if window <= 0 {
		window = time.Minute
	}
	return &RateLimiter{limit: limit, window: window, requests: make(map[string][]time.Time)}
}

func (r *RateLimiter) Allow(key string) bool {
	if r == nil {
		return false
	}
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()

	entries := r.requests[key]
	cutoff := now.Add(-r.window)
	filtered := entries[:0]
	for _, when := range entries {
		if when.After(cutoff) {
			filtered = append(filtered, when)
		}
	}
	r.requests[key] = filtered
	if len(filtered) >= r.limit {
		return false
	}
	r.requests[key] = append(r.requests[key], now)
	return true
}

func NewQuota(limit int, window time.Duration) *Quota {
	if limit <= 0 {
		limit = 1
	}
	if window <= 0 {
		window = time.Minute
	}
	return &Quota{limit: limit, window: window, requests: make(map[string][]time.Time)}
}

func (q *Quota) Allow(key string) bool {
	if q == nil {
		return false
	}
	now := time.Now()
	q.mu.Lock()
	defer q.mu.Unlock()

	entries := q.requests[key]
	cutoff := now.Add(-q.window)
	filtered := entries[:0]
	for _, when := range entries {
		if when.After(cutoff) {
			filtered = append(filtered, when)
		}
	}
	q.requests[key] = filtered
	if len(filtered) >= q.limit {
		return false
	}
	q.requests[key] = append(q.requests[key], now)
	return true
}

func (q *Quota) Used(key string) int {
	if q == nil {
		return 0
	}
	now := time.Now()
	q.mu.Lock()
	defer q.mu.Unlock()

	entries := q.requests[key]
	cutoff := now.Add(-q.window)
	filtered := entries[:0]
	for _, when := range entries {
		if when.After(cutoff) {
			filtered = append(filtered, when)
		}
	}
	q.requests[key] = filtered
	return len(filtered)
}

func SecurityHeaders(next http.Handler) http.Handler {
	if next == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Frame-Options", "DENY")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Referrer-Policy", "no-referrer")
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'")
			w.WriteHeader(http.StatusOK)
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'")
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// HTTP metrics middleware (Task 1-c, additive extension).
//
// The middleware reuses the existing Metrics primitives (Inc / Observe) and
// records request count + latest duration labelled by route pattern, HTTP
// method and response status. Metric keys surface through the existing
// Snapshot() and therefore through GET /v1/metrics as, e.g.:
//
//      http_requests_total{route="/v1/agents",method="GET",status="200"}
//      http_request_duration_seconds{route="/v1/agents",method="GET",status="200"}
// ---------------------------------------------------------------------------

// HTTP metric name prefixes used by MetricsMiddleware.
const (
	HTTPRequestsMetric  = "http_requests_total"
	HTTPDurationMetric  = "http_request_duration_seconds"
	unmatchedRouteLabel = "unmatched"
)

// ContextRouteName is the context key handlers (or the RouteName wrapper) use
// to declare the route pattern for a request. ServeMux patterns are not
// introspectable from outer middleware, so the route label must be injected
// explicitly; requests without one are recorded as "unmatched".
type ContextRouteName struct{}

// WithRouteName returns a context carrying the route pattern label.
func WithRouteName(ctx context.Context, route string) context.Context {
	if route == "" {
		return ctx
	}
	return context.WithValue(ctx, ContextRouteName{}, route)
}

// RouteNameFromContext returns the route pattern label previously attached
// with WithRouteName, or "" when absent.
func RouteNameFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	route, _ := ctx.Value(ContextRouteName{}).(string)
	return route
}

// RequestWithRouteName is a convenience helper producing a shallow request
// copy whose context carries the route pattern label.
func RequestWithRouteName(r *http.Request, route string) *http.Request {
	return r.WithContext(WithRouteName(r.Context(), route))
}

// RouteName returns middleware that tags every request passing through it
// with a static route pattern label, e.g. RouteName("/v1/agents")(handler).
func RouteName(route string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, RequestWithRouteName(r, route))
	})
}

// httpMetricKey builds the labelled metric key recorded in the snapshot.
func httpMetricKey(metric, route, method string, status int) string {
	if route == "" {
		route = unmatchedRouteLabel
	}
	return fmt.Sprintf("%s{route=%q,method=%q,status=%q}", metric, route, method, fmt.Sprintf("%03d", status))
}

// HTTPCountKey exposes the count-key format (useful for tests and dashboards).
func HTTPCountKey(route, method string, status int) string {
	return httpMetricKey(HTTPRequestsMetric, route, method, status)
}

// HTTPDurationKey exposes the duration-key format.
func HTTPDurationKey(route, method string, status int) string {
	return httpMetricKey(HTTPDurationMetric, route, method, status)
}

// IncHTTP increments the request counter for the labelled route/method/status.
func (m *Metrics) IncHTTP(route, method string, status int) {
	m.Inc(httpMetricKey(HTTPRequestsMetric, route, method, status))
}

// ObserveHTTP records the latest request duration (seconds) for the labelled
// route/method/status (legacy last-value gauge, surfaced through Snapshot()
// and /metrics?format=json) AND a bucketed observation of the
// agentos_request_duration_seconds family for percentile estimation and
// Prometheus exposition (Task 2-h).
func (m *Metrics) ObserveHTTP(route, method string, status int, seconds float64) {
	m.Observe(httpMetricKey(HTTPDurationMetric, route, method, status), seconds)
	m.ObserveHistogram(httpMetricKey(MetricRequestDuration, route, method, status), seconds)
}

// MetricsMiddleware records request count and duration for every request by
// route pattern, HTTP method and response status.
//
// Route resolution order:
//  1. the route label from the request context (set via WithRouteName /
//     RequestWithRouteName / RouteName by handlers or inner middleware);
//  2. http.Request.Pattern (populated when this middleware runs inside a
//     ServeMux handler chain on Go 1.23+);
//  3. the fallback label "unmatched".
func MetricsMiddleware(m *Metrics, next http.Handler) http.Handler {
	if m == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		route := RouteNameFromContext(r.Context())
		if route == "" {
			route = r.Pattern
		}
		if route == "" {
			route = unmatchedRouteLabel
		}
		m.IncHTTP(route, r.Method, rec.status)
		m.ObserveHTTP(route, r.Method, rec.status, time.Since(start).Seconds())
	})
}

// statusRecorder captures the response status code (defaulting to 200 when a
// handler writes a body without an explicit WriteHeader) while forwarding
// Flush so SSE handlers keep working behind the middleware.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.wroteHeader = true
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
