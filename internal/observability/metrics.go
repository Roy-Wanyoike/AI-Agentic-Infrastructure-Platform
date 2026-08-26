package observability

import (
	"net/http"
	"sync"
	"time"
)

type Metrics struct {
	mu      sync.Mutex
	counts  map[string]int64
	latency map[string]float64
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
	return &Metrics{counts: make(map[string]int64), latency: make(map[string]float64)}
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
	for k, v := range m.counts { counts[k] = v }
	for k, v := range m.latency { latency[k] = v }
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
