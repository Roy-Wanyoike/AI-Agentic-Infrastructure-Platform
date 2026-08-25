package usage

import "sync"

type Record struct {
    Resource  string
    Quantity  int
}

type Service struct {
    mu     sync.Mutex
    values map[string]int
}

func NewService() *Service { return &Service{values: make(map[string]int)} }

func (s *Service) Record(resource string, delta int) {
    s.mu.Lock(); defer s.mu.Unlock()
    s.values[resource] += delta
}

func (s *Service) Snapshot() map[string]int {
    s.mu.Lock(); defer s.mu.Unlock()
    out := make(map[string]int, len(s.values))
    for k, v := range s.values { out[k] = v }
    return out
}
