package synth

// evidence_store.go — issue #148 (wave-10 track 10-p): the storage seam for
// redacted run artifacts (screenshots, traces, console captures, bounded API
// response bodies).
//
// Workers never embed raw artifacts in Run/StepEvent records — they store the
// payload here and keep only the returned opaque reference string on the
// record (#150 consumes those refs when building findings). Production
// deployments back this seam with object storage; the in-memory
// implementation below exists for tests and zero-infra mode.
//
// Redaction responsibility is split deliberately:
//   - the CALLER (worker) redacts secrets from metadata and bounds payloads
//     before calling Put (see RedactHeaders / sanitizeURL in worker.go);
//   - the STORE treats payloads as opaque bytes and adds tenant-scoped refs.
// The store therefore never has to understand secret formats.

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// EvidenceStore persists one redacted artifact and returns a stable opaque
// reference suitable for Run.EvidenceRefs / StepEvent.EvidenceRef. kind is a
// short classification ("screenshot", "trace", "console", "api_response");
// meta is caller-redacted, bounded metadata. Implementations must be safe for
// concurrent use.
type EvidenceStore interface {
	Put(ctx context.Context, kind string, payload []byte, meta map[string]string) (string, error)
	Get(ctx context.Context, ref string) ([]byte, bool, error)
}

// evidenceItem is one stored artifact.
type evidenceItem struct {
	Kind     string
	Payload  []byte
	Meta     map[string]string
	StoredAt time.Time
}

// InMemoryEvidenceStore is the zero-infrastructure EvidenceStore used by
// tests and single-node deployments. References are deterministic
// ("mem://evidence/<kind>-<seq>") and sequence numbers are monotonic under
// the store mutex.
type InMemoryEvidenceStore struct {
	mu    sync.Mutex
	seq   int
	items map[string]evidenceItem
}

// NewInMemoryEvidenceStore returns an empty in-memory evidence store.
func NewInMemoryEvidenceStore() *InMemoryEvidenceStore {
	return &InMemoryEvidenceStore{items: make(map[string]evidenceItem)}
}

// Put stores the payload and returns its opaque reference. A nil store is
// tolerated by workers via NoopEvidenceStore; calling Put on a zero-value
// InMemoryEvidenceStore works (the map is lazily created) but prefer
// NewInMemoryEvidenceStore.
func (s *InMemoryEvidenceStore) Put(_ context.Context, kind string, payload []byte, meta map[string]string) (string, error) {
	if kind == "" {
		kind = "artifact"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.items == nil {
		s.items = make(map[string]evidenceItem)
	}
	s.seq++
	ref := fmt.Sprintf("mem://evidence/%s-%06d", kind, s.seq)
	cp := make([]byte, len(payload))
	copy(cp, payload)
	m := make(map[string]string, len(meta))
	for k, v := range meta {
		m[k] = v
	}
	s.items[ref] = evidenceItem{Kind: kind, Payload: cp, Meta: m, StoredAt: time.Now().UTC()}
	return ref, nil
}

// Get returns the payload for a reference. The second result is false for
// unknown references (missing evidence is never an error at read time — the
// ref may point at externally-managed storage).
func (s *InMemoryEvidenceStore) Get(_ context.Context, ref string) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.items[ref]
	if !ok {
		return nil, false, nil
	}
	cp := make([]byte, len(item.Payload))
	copy(cp, item.Payload)
	return cp, true, nil
}

// Len reports how many artifacts are stored (test/monitoring convenience).
func (s *InMemoryEvidenceStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.items)
}

// NoopEvidenceStore discards artifacts and returns empty references. Workers
// fall back to it when no store is wired, so evidence capture is always
// best-effort and never breaks a run.
type NoopEvidenceStore struct{}

// Put discards the payload and returns an empty reference.
func (NoopEvidenceStore) Put(context.Context, string, []byte, map[string]string) (string, error) {
	return "", nil
}

// Get always reports the reference as unknown.
func (NoopEvidenceStore) Get(context.Context, string) ([]byte, bool, error) {
	return nil, false, nil
}
