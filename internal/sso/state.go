package sso

import (
	"sync"
	"time"
)

// stateEntry is the server-side binding created at BeginLogin and consumed at
// CompleteLogin. It carries everything the callback needs so a forged or
// tampered callback URL cannot influence tenant resolution (the state is the
// key; the org comes from the stored entry, never from the request).
type stateEntry struct {
	OrgID       string
	Nonce       string
	RedirectURI string
	CreatedAt   time.Time
}

// stateStore is a single-use TTL cache: Put inserts an entry, Take removes
// and validates it (expiry checked against the injected clock). The 10-minute
// default TTL bounds the window in which a login can complete. In-memory
// storage is acceptable per the issue; entries are process-local so a
// multi-replica deployment must stick sessions or share the store later.
type stateStore struct {
	mu      sync.Mutex
	entries map[string]stateEntry
	ttl     time.Duration
	now     func() time.Time
}

func newStateStore(ttl time.Duration, now func() time.Time) *stateStore {
	if now == nil {
		now = time.Now
	}
	return &stateStore{
		entries: make(map[string]stateEntry),
		ttl:     ttl,
		now:     now,
	}
}

// Put stores a state binding and lazily evicts expired entries.
func (st *stateStore) Put(state string, entry stateEntry) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.sweepLocked()
	st.entries[state] = entry
}

// Take atomically removes and returns the binding for state. It returns
// false when the state is unknown (replay), expired, or bound to a stale
// entry — all three collapse into ErrStateInvalid upstream so an attacker
// cannot distinguish them.
func (st *stateStore) Take(state string, now time.Time) (stateEntry, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	entry, ok := st.entries[state]
	if !ok {
		return stateEntry{}, false
	}
	delete(st.entries, state)
	if st.ttl > 0 && now.Sub(entry.CreatedAt) > st.ttl {
		return stateEntry{}, false
	}
	return entry, true
}

// sweepLocked drops expired entries; callers hold st.mu.
func (st *stateStore) sweepLocked() {
	if st.ttl <= 0 {
		return
	}
	now := st.now()
	for state, entry := range st.entries {
		if now.Sub(entry.CreatedAt) > st.ttl {
			delete(st.entries, state)
		}
	}
}
