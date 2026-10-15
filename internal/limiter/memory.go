package limiter

// memory.go — the in-memory sliding-window fallback (port of the
// internal/httpx fallback semantics, upgraded to weighted multi-window).
//
// Semantics (mirroring the Redis script exactly):
//   - per key+window entry log of {at, weight}; entries at or before
//     now-window are trimmed lazily on access;
//   - a check is two-phase: EVERY window is summed first; the check denies
//     (consuming NOTHING) when any window would exceed its cap, with
//     RetryAfter derived from the oldest entry of the tightest window (the
//     httpx fallback could not report the oldest-hit age because it reused
//     observability.RateLimiter; this port can, so both backends report the
//     same shape);
//   - allowed checks append a weighted entry to every window and report the
//     post-check Remaining of every window (cap - prior usage - weight).
//
// Windows are keyed by NAME (rpm/rpd/tpm), not duration: RPM and TPM share
// the one-minute window and must land in distinct maps.
//
// Concurrency: one mutex per Limiter instance's fallback map. The fallback
// is per-process BY DESIGN (see the package doc): it keeps enforcement
// approximate, never absent.

import (
	"sync"
	"time"
)

type memEntry struct {
	at     time.Time
	weight int
}

type memoryWindows struct {
	// mu guards entries; keyed by the rendered window key (the same string
	// the Redis path uses, minus the hash-tag braces, so diagnostics match).
	mu      sync.Mutex
	entries map[string][]memEntry
}

func newMemoryWindows() *memoryWindows {
	return &memoryWindows{entries: make(map[string][]memEntry)}
}

func memKey(key LimitKey, name string) string {
	return key.OrgID + "|" + string(key.Scope) + "|" + key.Subject + "|" + name
}

// trim drops entries at or before cutoff for the given slice.
func trimEntries(entries []memEntry, cutoff time.Time) []memEntry {
	out := entries[:0]
	for _, e := range entries {
		if e.at.After(cutoff) {
			out = append(out, e)
		}
	}
	return out
}

// sumWeights totals the live weights of a trimmed slice.
func sumWeights(entries []memEntry) int {
	total := 0
	for _, e := range entries {
		total += e.weight
	}
	return total
}

// allowBatch evaluates the check over the in-memory windows of ALL (key,
// window) pairs in one locked pass. It ALWAYS returns a Decision: a denial
// (NOTHING consumed, on any pair) or the allowed verdict with the post-check
// Remaining of every window (cap - prior usage - weight on allow; cap -
// prior usage on denial).
func (m *memoryWindows) allowBatch(now time.Time, pairs []keyedSpec) *Decision {
	m.mu.Lock()
	defer m.mu.Unlock()

	totals := make([]int64, len(pairs))
	for i, ps := range pairs {
		k := memKey(ps.key, ps.spec.name)
		entries := trimEntries(m.entries[k], now.Add(-ps.spec.window))
		m.entries[k] = entries
		totals[i] = int64(sumWeights(entries))
	}
	ps, used, denied := firstDenying(pairs, totals)
	decision := Decision{Allowed: !denied, Remaining: make(map[string]int, len(pairs))}
	for i, p := range pairs {
		remaining := p.spec.cap - int(totals[i])
		if !denied {
			remaining -= p.spec.weight // the check just consumed its cost
		}
		if remaining < 0 {
			remaining = 0
		}
		decision.Remaining[remainingKey(p.key.Scope, p.spec.name)] = remaining
	}
	if !denied {
		for _, p := range pairs {
			k := memKey(p.key, p.spec.name)
			m.entries[k] = append(m.entries[k], memEntry{at: now, weight: p.spec.weight})
		}
		return &decision
	}
	decision.Scope = ps.key.Scope
	decision.Reason = denyReason(ps.key, ps.spec, used)
	decision.RetryAfter = m.retryAfter(now, pairs, totals)
	return &decision
}

// retryAfter derives the batch retry hint: the minimum time until a denying
// window's oldest live entry ages out (the window duration when the denying
// window is empty). Entries were already trimmed by allowBatch.
func (m *memoryWindows) retryAfter(now time.Time, pairs []keyedSpec, totals []int64) time.Duration {
	retryAfter := time.Duration(0)
	for i, ps := range pairs {
		if i >= len(totals) || totals[i]+int64(ps.spec.weight) <= int64(ps.spec.cap) {
			continue
		}
		retry := ps.spec.window
		if entries := m.entries[memKey(ps.key, ps.spec.name)]; len(entries) > 0 {
			retry = entries[0].at.Add(ps.spec.window).Sub(now)
			if retry < time.Millisecond {
				retry = time.Millisecond
			}
		}
		if retryAfter == 0 || retry < retryAfter {
			retryAfter = retry
		}
	}
	return retryAfter
}

// add charges weighted entries without an allow check (Report path).
func (m *memoryWindows) add(now time.Time, key LimitKey, specs []windowSpec) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, spec := range specs {
		k := memKey(key, spec.name)
		entries := trimEntries(m.entries[k], now.Add(-spec.window))
		m.entries[k] = append(entries, memEntry{at: now, weight: spec.weight})
	}
}

// count reports the live entry count of one key/window (tests, diagnostics).
func (m *memoryWindows) count(now time.Time, key LimitKey, window time.Duration) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := memKey(key, windowName(window))
	entries := trimEntries(m.entries[k], now.Add(-window))
	m.entries[k] = entries
	return len(entries)
}
