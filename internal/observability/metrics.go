package observability

import "sync"

type Metrics struct {
	mu      sync.Mutex
	counts  map[string]int64
	latency map[string]float64
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
