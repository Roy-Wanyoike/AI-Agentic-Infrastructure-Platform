package limiter

// limiter_test.go — the sliding-window engine (issue #124): in-memory
// fallback semantics, the Redis path over miniredis (deterministic via the
// WithClock seam), atomicity across windows, weighted TPM accounting,
// Report semantics and the fail-mode matrix. House rule: deterministic —
// every timing comes from the injected clock, never from sleeps.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newTestClock returns a controllable clock starting at a fixed UTC instant.
func newTestClock() (*time.Time, func() time.Time, func(d time.Duration)) {
	now := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	return &now, func() time.Time { return now }, func(d time.Duration) { now = now.Add(d) }
}

func newMemoryLimiter(t *testing.T, store *MemoryStore, opts ...Option) (*Limiter, func(time.Duration)) {
	t.Helper()
	_, clock, advance := newTestClock()
	opts = append(opts, WithClock(clock))
	resolver := NewResolver(store, WithResolverClock(clock))
	return NewLimiter(nil, resolver, opts...), advance
}

func upsert(t *testing.T, store *MemoryStore, org string, cfg Config) {
	t.Helper()
	if err := store.Upsert(context.Background(), org, cfg); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
}

func TestMemoryUnlimitedWhenNoConfig(t *testing.T) {
	l, _ := newMemoryLimiter(t, NewMemoryStore())
	d, err := l.Allow(context.Background(), LimitKey{OrgID: "org-1", Scope: ScopeAgent, Subject: "agent-1"}, Tokens{Requests: 1})
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if !d.Allowed || len(d.Remaining) != 0 {
		t.Fatalf("unlimited key must allow with empty remaining: %+v", d)
	}
}

func TestMemoryRPMBasicSlide(t *testing.T) {
	store := NewMemoryStore()
	upsert(t, store, "org-1", Config{Scope: ScopeAgent, Subject: "a1", RPM: 2, Enabled: true})
	l, advance := newMemoryLimiter(t, store)
	ctx := context.Background()
	key := LimitKey{OrgID: "org-1", Scope: ScopeAgent, Subject: "a1"}

	for i := 0; i < 2; i++ {
		d, err := l.Allow(ctx, key, Tokens{Requests: 1})
		if err != nil || !d.Allowed {
			t.Fatalf("call %d: want allowed, got %+v err=%v", i, d, err)
		}
		if got := d.Remaining["agent:rpm"]; got != 1-i {
			t.Fatalf("call %d remaining = %d, want %d", i, got, 1-i)
		}
	}
	d, _ := l.Allow(ctx, key, Tokens{Requests: 1})
	if d.Allowed || d.Scope != ScopeAgent {
		t.Fatalf("third call must deny on the agent scope: %+v", d)
	}
	if d.Remaining["agent:rpm"] != 0 {
		t.Fatalf("denied remaining = %d, want 0", d.Remaining["agent:rpm"])
	}
	if d.RetryAfter <= 0 || d.RetryAfter > MinuteWindow {
		t.Fatalf("retry-after out of range: %v", d.RetryAfter)
	}
	// Sliding: after the window the budget returns.
	advance(MinuteWindow + time.Second)
	if d, _ := l.Allow(ctx, key, Tokens{Requests: 1}); !d.Allowed {
		t.Fatalf("post-window call must allow: %+v", d)
	}
}

func TestMemoryRPDRolling24h(t *testing.T) {
	store := NewMemoryStore()
	upsert(t, store, "org-1", Config{Scope: ScopeOrg, RPD: 3, Enabled: true})
	l, advance := newMemoryLimiter(t, store)
	ctx := context.Background()
	key := LimitKey{OrgID: "org-1", Scope: ScopeOrg}

	for i := 0; i < 3; i++ {
		if d, _ := l.Allow(ctx, key, Tokens{Requests: 1}); !d.Allowed {
			t.Fatalf("call %d denied: %+v", i, d)
		}
	}
	if d, _ := l.Allow(ctx, key, Tokens{Requests: 1}); d.Allowed {
		t.Fatal("4th daily call must deny")
	}
	// Rolling window: only 22h pass — still denied; at 24h+1s the first
	// entry ages out and one request is available again.
	advance(22 * time.Hour)
	if d, _ := l.Allow(ctx, key, Tokens{Requests: 1}); d.Allowed {
		t.Fatal("22h in, the rolling day must still deny")
	}
	advance(2*time.Hour + time.Second)
	d, _ := l.Allow(ctx, key, Tokens{Requests: 1})
	if !d.Allowed {
		t.Fatalf("past 24h the entries must age out: %+v", d)
	}
	// The refill call consumed one of the three daily slots (all three
	// original entries were stamped at the same instant, so the whole
	// window emptied at once).
	if got := d.Remaining["org:rpd"]; got != 2 {
		t.Fatalf("remaining after refill = %d, want 2", got)
	}
}

func TestDenialConsumesNothing(t *testing.T) {
	store := NewMemoryStore()
	upsert(t, store, "org-1", Config{Scope: ScopeAgent, Subject: "a1", RPM: 1, Enabled: true})
	l, advance := newMemoryLimiter(t, store)
	ctx := context.Background()
	key := LimitKey{OrgID: "org-1", Scope: ScopeAgent, Subject: "a1"}

	if d, _ := l.Allow(ctx, key, Tokens{Requests: 1}); !d.Allowed {
		t.Fatal("first call must allow")
	}
	for i := 0; i < 5; i++ {
		if d, _ := l.Allow(ctx, key, Tokens{Requests: 1}); d.Allowed {
			t.Fatalf("denied call %d must not be allowed", i)
		}
	}
	// A minute later exactly ONE slot exists: the denials consumed nothing.
	advance(MinuteWindow + time.Second)
	if d, _ := l.Allow(ctx, key, Tokens{Requests: 1}); !d.Allowed {
		t.Fatal("the single freed slot must be available")
	}
	if d, _ := l.Allow(ctx, key, Tokens{Requests: 1}); d.Allowed {
		t.Fatal("no phantom slots from denied calls")
	}
}

func TestMemoryTPMWeighted(t *testing.T) {
	store := NewMemoryStore()
	upsert(t, store, "org-1", Config{Scope: ScopeModel, Subject: "gpt-test", TPM: 100, Enabled: true})
	l, _ := newMemoryLimiter(t, store)
	ctx := context.Background()
	key := LimitKey{OrgID: "org-1", Scope: ScopeModel, Subject: "gpt-test"}

	// Pre-call check (RPM-style: 1 request, 0 tokens) then post-call report.
	if d, _ := l.Allow(ctx, key, Tokens{Requests: 1}); !d.Allowed {
		t.Fatal("call 1 must allow")
	}
	if err := l.Report(ctx, key, Tokens{Tokens: 60}); err != nil {
		t.Fatalf("Report: %v", err)
	}
	if d, _ := l.Allow(ctx, key, Tokens{Requests: 1}); !d.Allowed {
		t.Fatal("call 2 must allow (40 tokens left)")
	}
	if err := l.Report(ctx, key, Tokens{Tokens: 60}); err != nil {
		t.Fatalf("Report: %v", err)
	}
	// Window now holds 120 > 100: the next pre-call check denies on TPM.
	d, _ := l.Allow(ctx, key, Tokens{Requests: 1})
	if d.Allowed {
		t.Fatal("call 3 must deny: TPM exceeded")
	}
	if got := d.Remaining["model:tpm"]; got != 0 {
		t.Fatalf("tpm remaining = %d, want 0", got)
	}
	// Report with zero cost is a no-op, never an error.
	if err := l.Report(ctx, key, Tokens{}); err != nil {
		t.Fatalf("zero-cost Report: %v", err)
	}
}

func TestAllowModelZeroTokensDoesNotConsumeTPM(t *testing.T) {
	store := NewMemoryStore()
	upsert(t, store, "org-1", Config{Scope: ScopeModel, Subject: "m", TPM: 10, Enabled: true})
	l, _ := newMemoryLimiter(t, store)
	ctx := context.Background()
	key := LimitKey{OrgID: "org-1", Scope: ScopeModel, Subject: "m"}
	for i := 0; i < 5; i++ {
		if d, _ := l.Allow(ctx, key, Tokens{Requests: 1, Tokens: 0}); !d.Allowed {
			t.Fatalf("zero-token checks must never consume TPM (call %d denied: %+v)", i, d)
		}
	}
}

func TestFailModeOpenOnStoreError(t *testing.T) {
	store := &failingStore{}
	_, clock, _ := newTestClock()
	l := NewLimiter(nil, NewResolver(store, WithResolverClock(clock)), WithClock(clock))
	d, err := l.Allow(context.Background(), LimitKey{OrgID: "org-1", Scope: ScopeAgent, Subject: "a"}, Tokens{Requests: 1})
	if err == nil {
		t.Fatal("store failure must surface as an error")
	}
	if !d.Allowed || d.Scope != "" {
		t.Fatalf("fail-open must allow with no denying scope: %+v", d)
	}
}

func TestFailModeClosedOnStoreError(t *testing.T) {
	store := &failingStore{}
	_, clock, _ := newTestClock()
	l := NewLimiter(nil, NewResolver(store, WithResolverClock(clock)), WithClock(clock), WithFailMode(FailModeClosed))
	d, err := l.Allow(context.Background(), LimitKey{OrgID: "org-1", Scope: ScopeAgent, Subject: "a"}, Tokens{Requests: 1})
	if err == nil {
		t.Fatal("store failure must surface as an error")
	}
	if d.Allowed {
		t.Fatalf("fail-closed must deny: %+v", d)
	}
	if d.Reason == "" || !contains(d.Reason, ReasonLimiterUnavailable) {
		t.Fatalf("reason must carry limiter_unavailable: %q", d.Reason)
	}
	if d.RetryAfter <= 0 {
		t.Fatalf("fail-closed retry hint must be positive: %v", d.RetryAfter)
	}
}

func TestFailModeFromEnv(t *testing.T) {
	t.Setenv(FailModeEnvVar, "closed")
	if FailModeFromEnv() != FailModeClosed {
		t.Fatal("closed must parse")
	}
	t.Setenv(FailModeEnvVar, "OPEN")
	if FailModeFromEnv() != FailModeOpen {
		t.Fatal("open must parse")
	}
	t.Setenv(FailModeEnvVar, "garbage")
	if FailModeFromEnv() != FailModeOpen {
		t.Fatal("invalid values fall back to open")
	}
}

// --- Redis path (miniredis) -------------------------------------------------

func newRedisLimiter(t *testing.T, store *MemoryStore, opts ...Option) (*Limiter, func(time.Duration), *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	_, clock, advance := newTestClock()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	opts = append(opts, WithClock(clock))
	l := NewLimiter(rdb, NewResolver(store, WithResolverClock(clock)), opts...)
	return l, advance, mr
}

func TestRedisRPMBasicSlide(t *testing.T) {
	store := NewMemoryStore()
	upsert(t, store, "org-1", Config{Scope: ScopeAgent, Subject: "a1", RPM: 2, Enabled: true})
	l, advance, _ := newRedisLimiter(t, store)
	ctx := context.Background()
	key := LimitKey{OrgID: "org-1", Scope: ScopeAgent, Subject: "a1"}

	for i := 0; i < 2; i++ {
		d, err := l.Allow(ctx, key, Tokens{Requests: 1})
		if err != nil || !d.Allowed {
			t.Fatalf("call %d: want allowed, got %+v err=%v", i, d, err)
		}
		if got := d.Remaining["agent:rpm"]; got != 1-i {
			t.Fatalf("call %d remaining = %d, want %d", i, got, 1-i)
		}
	}
	d, _ := l.Allow(ctx, key, Tokens{Requests: 1})
	if d.Allowed || d.Scope != ScopeAgent || !contains(d.Reason, "RPM") {
		t.Fatalf("third call must deny naming the window: %+v", d)
	}
	advance(MinuteWindow + time.Second)
	if d, _ := l.Allow(ctx, key, Tokens{Requests: 1}); !d.Allowed {
		t.Fatalf("post-window call must allow: %+v", d)
	}
}

func TestRedisAtomicAcrossWindows(t *testing.T) {
	store := NewMemoryStore()
	// One row with an RPM and an RPD dimension (10/min, 2/rolling-day): the
	// 3rd request must deny on the RPD window while RPM still has budget —
	// and consume NOTHING from RPM.
	upsert(t, store, "org-1", Config{Scope: ScopeAgent, Subject: "a1", RPM: 10, RPD: 2, Enabled: true})
	l, _, _ := newRedisLimiter(t, store)
	ctx := context.Background()
	key := LimitKey{OrgID: "org-1", Scope: ScopeAgent, Subject: "a1"}

	for i := 0; i < 2; i++ {
		d, err := l.Allow(ctx, key, Tokens{Requests: 1})
		if err != nil || !d.Allowed {
			t.Fatalf("call %d: %+v err=%v", i, d, err)
		}
	}
	d, _ := l.Allow(ctx, key, Tokens{Requests: 1})
	if d.Allowed {
		t.Fatal("3rd call must deny on the RPD window")
	}
	if got := d.Remaining["agent:rpm"]; got != 8 {
		t.Fatalf("rpm remaining = %d, want 8 (denial consumed nothing): %+v", got, d)
	}
	if got := d.Remaining["agent:rpd"]; got != 0 {
		t.Fatalf("rpd remaining = %d, want 0", got)
	}
	// Both windows were checked in ONE atomic script: the key materialized
	// with exactly the two consumed requests.
	if got := l.MemoryStats(key, MinuteWindow); got != 0 {
		t.Fatalf("redis path must not touch the fallback: %d", got)
	}
}

func TestRedisTPMWeightedAndReport(t *testing.T) {
	store := NewMemoryStore()
	upsert(t, store, "org-1", Config{Scope: ScopeModel, Subject: "gpt-test", TPM: 100, Enabled: true})
	l, _, _ := newRedisLimiter(t, store)
	ctx := context.Background()
	key := LimitKey{OrgID: "org-1", Scope: ScopeModel, Subject: "gpt-test"}

	if d, _ := l.Allow(ctx, key, Tokens{Requests: 1}); !d.Allowed {
		t.Fatal("call 1 must allow")
	}
	if err := l.Report(ctx, key, Tokens{Tokens: 60}); err != nil {
		t.Fatalf("Report: %v", err)
	}
	// Remaining reflects the reported tokens (cap 100, used 60).
	d, _ := l.Allow(ctx, key, Tokens{Requests: 1})
	if !d.Allowed || d.Remaining["model:tpm"] != 40 {
		t.Fatalf("call 2 must allow with 40 token budget left: %+v", d)
	}
	if err := l.Report(ctx, key, Tokens{Tokens: 60}); err != nil {
		t.Fatalf("Report: %v", err)
	}
	if d, _ := l.Allow(ctx, key, Tokens{Requests: 1}); d.Allowed {
		t.Fatal("call 3 must deny: 120 tokens recorded against cap 100")
	}
}

func TestRedisFailOpenFallsBackToMemory(t *testing.T) {
	store := NewMemoryStore()
	upsert(t, store, "org-1", Config{Scope: ScopeAgent, Subject: "a1", RPM: 1, Enabled: true})
	l, _, mr := newRedisLimiter(t, store) // default fail mode: open
	ctx := context.Background()
	key := LimitKey{OrgID: "org-1", Scope: ScopeAgent, Subject: "a1"}

	if d, _ := l.Allow(ctx, key, Tokens{Requests: 1}); !d.Allowed {
		t.Fatal("pre-outage call must allow")
	}
	mr.Close() // infrastructure outage
	// Fail-open: the in-memory fallback keeps enforcing (approximate). The
	// fallback is per-process and starts EMPTY here (the pre-outage usage
	// lived in Redis), so the first post-outage call consumes the
	// process-local slot and the SECOND hits the configured limit.
	if d, _ := l.Allow(ctx, key, Tokens{Requests: 1}); !d.Allowed {
		t.Fatalf("fallback must allow the first post-outage call: %+v", d)
	}
	if d, _ := l.Allow(ctx, key, Tokens{Requests: 1}); d.Allowed {
		t.Fatal("fallback must still enforce the configured limit")
	}
}

func TestRedisFailClosedDeniesOnOutage(t *testing.T) {
	store := NewMemoryStore()
	upsert(t, store, "org-1", Config{Scope: ScopeAgent, Subject: "a1", RPM: 10, Enabled: true})
	l, _, mr := newRedisLimiter(t, store, WithFailMode(FailModeClosed))
	ctx := context.Background()
	key := LimitKey{OrgID: "org-1", Scope: ScopeAgent, Subject: "a1"}

	if d, _ := l.Allow(ctx, key, Tokens{Requests: 1}); !d.Allowed {
		t.Fatal("pre-outage call must allow")
	}
	mr.Close()
	d, _ := l.Allow(ctx, key, Tokens{Requests: 1})
	if d.Allowed || !contains(d.Reason, ReasonLimiterUnavailable) {
		t.Fatalf("fail-closed must deny with limiter_unavailable: %+v", d)
	}
}

func TestRedisIsolationAcrossSubjectsAndOrgs(t *testing.T) {
	store := NewMemoryStore()
	upsert(t, store, "org-1", Config{Scope: ScopeAgent, Subject: "a1", RPM: 1, Enabled: true})
	l, _, _ := newRedisLimiter(t, store)
	ctx := context.Background()

	if d, _ := l.Allow(ctx, LimitKey{OrgID: "org-1", Scope: ScopeAgent, Subject: "a1"}, Tokens{Requests: 1}); !d.Allowed {
		t.Fatal("a1 first call must allow")
	}
	// Different subject: own bucket.
	if d, _ := l.Allow(ctx, LimitKey{OrgID: "org-1", Scope: ScopeAgent, Subject: "a2"}, Tokens{Requests: 1}); !d.Allowed {
		t.Fatal("a2 must have its own bucket")
	}
	// Same subject id, different org: own bucket (tenant isolation).
	if d, _ := l.Allow(ctx, LimitKey{OrgID: "org-2", Scope: ScopeAgent, Subject: "a1"}, Tokens{Requests: 1}); !d.Allowed {
		t.Fatal("org-2/a1 must have its own bucket")
	}
	// a1 in org-1 is spent.
	if d, _ := l.Allow(ctx, LimitKey{OrgID: "org-1", Scope: ScopeAgent, Subject: "a1"}, Tokens{Requests: 1}); d.Allowed {
		t.Fatal("a1/org-1 must be spent")
	}
}

// --- helpers ----------------------------------------------------------------

type failingStore struct{ MemoryStore }

var errStoreDown = errors.New("store down")

func (f *failingStore) Get(context.Context, string, Scope, string) (*Config, error) {
	return nil, errStoreDown
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
