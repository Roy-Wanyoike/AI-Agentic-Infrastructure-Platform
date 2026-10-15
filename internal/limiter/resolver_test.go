package limiter

// resolver_test.go — the LIMIT RESOLUTION contract (issue #124 requirement
// 2): subject-specific > org default > unlimited, the disabled-row opt-out,
// the TTL cache and its invalidation.

import (
	"context"
	"testing"
	"time"
)

func TestResolveSubjectSpecificOverOrgDefault(t *testing.T) {
	store := NewMemoryStore()
	upsert(t, store, "org-1", Config{Scope: ScopeAgent, RPM: 10, Enabled: true})               // org default
	upsert(t, store, "org-1", Config{Scope: ScopeAgent, Subject: "a1", RPM: 2, Enabled: true}) // subject row
	r := NewResolver(store, WithResolverTTL(0))

	limit, found, err := r.Resolve(context.Background(), "org-1", ScopeAgent, "a1")
	if err != nil || !found {
		t.Fatalf("Resolve: %v found=%v", err, found)
	}
	if limit.RPM != 2 {
		t.Fatalf("subject row must win: %+v", limit)
	}

	// Another agent falls back to the org default.
	limit, found, _ = r.Resolve(context.Background(), "org-1", ScopeAgent, "a2")
	if !found || limit.RPM != 10 {
		t.Fatalf("org default must apply: %+v found=%v", limit, found)
	}
}

func TestResolveUnlimitedWhenNoRows(t *testing.T) {
	r := NewResolver(NewMemoryStore(), WithResolverTTL(0))
	limit, found, err := r.Resolve(context.Background(), "org-1", ScopeTool, "t1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if found || limit.Active() {
		t.Fatalf("no rows must resolve to unlimited: %+v found=%v", limit, found)
	}
}

func TestResolveDisabledSubjectSuppressesOrgDefault(t *testing.T) {
	store := NewMemoryStore()
	upsert(t, store, "org-1", Config{Scope: ScopeTool, RPM: 10, Enabled: true})
	// Explicit per-subject opt-out.
	upsert(t, store, "org-1", Config{Scope: ScopeTool, Subject: "t-open", Enabled: false})
	r := NewResolver(store, WithResolverTTL(0))

	limit, found, _ := r.Resolve(context.Background(), "org-1", ScopeTool, "t-open")
	if found || limit.Active() {
		t.Fatalf("disabled subject row must suppress the org default: %+v found=%v", limit, found)
	}
	limit, found, _ = r.Resolve(context.Background(), "org-1", ScopeTool, "t-closed")
	if !found || limit.RPM != 10 {
		t.Fatalf("other subjects keep the default: %+v found=%v", limit, found)
	}
}

func TestResolveDisabledOrgDefaultMeansUnlimited(t *testing.T) {
	store := NewMemoryStore()
	upsert(t, store, "org-1", Config{Scope: ScopeModel, Subject: "m1", TPM: 5, Enabled: true})
	upsert(t, store, "org-1", Config{Scope: ScopeModel, Enabled: false})
	r := NewResolver(store, WithResolverTTL(0))

	// Subject row still wins (enabled).
	if limit, found, _ := r.Resolve(context.Background(), "org-1", ScopeModel, "m1"); !found || limit.TPM != 5 {
		t.Fatalf("enabled subject row must win: %+v %v", limit, found)
	}
	// Disabled org default: every other model unlimited.
	if _, found, _ := r.Resolve(context.Background(), "org-1", ScopeModel, "m2"); found {
		t.Fatal("disabled org default must mean unlimited for other subjects")
	}
}

func TestResolverCacheAndInvalidation(t *testing.T) {
	store := NewMemoryStore()
	upsert(t, store, "org-1", Config{Scope: ScopeAgent, Subject: "a1", RPM: 1, Enabled: true})
	_, clock, advance := newTestClock()
	r := NewResolver(store, WithResolverTTL(time.Minute), WithResolverClock(clock))

	if limit, _, _ := r.Resolve(context.Background(), "org-1", ScopeAgent, "a1"); limit.RPM != 1 {
		t.Fatal("initial resolve")
	}
	// Out-of-band store edit: the cached value persists within the TTL.
	upsert(t, store, "org-1", Config{Scope: ScopeAgent, Subject: "a1", RPM: 99, Enabled: true})
	if limit, _, _ := r.Resolve(context.Background(), "org-1", ScopeAgent, "a1"); limit.RPM != 1 {
		t.Fatalf("cache must serve within TTL: %+v", limit)
	}
	// The Enforcer invalidates on writes; out-of-band edits converge after
	// the TTL.
	advance(time.Minute + time.Second)
	if limit, _, _ := r.Resolve(context.Background(), "org-1", ScopeAgent, "a1"); limit.RPM != 99 {
		t.Fatalf("TTL expiry must refresh: %+v", limit)
	}

	// Write-through invalidation: Upsert + Invalidate is visible immediately.
	upsert(t, store, "org-1", Config{Scope: ScopeAgent, Subject: "a1", RPM: 7, Enabled: true})
	r.Invalidate("org-1")
	if limit, _, _ := r.Resolve(context.Background(), "org-1", ScopeAgent, "a1"); limit.RPM != 7 {
		t.Fatalf("invalidation must expose the new value: %+v", limit)
	}
	// Invalidation is org-scoped: org-2 entries survive org-1 writes.
	r.Invalidate("org-1")
	if limit, found, _ := r.Resolve(context.Background(), "org-2", ScopeAgent, "z"); found {
		t.Fatalf("unrelated org must stay unlimited: %+v", limit)
	}
}

func TestResolverNegativeCache(t *testing.T) {
	store := NewMemoryStore()
	_, clock, _ := newTestClock()
	r := NewResolver(store, WithResolverTTL(time.Minute), WithResolverClock(clock))
	ctx := context.Background()

	// Unlimited resolution caches the negative result.
	if _, found, _ := r.Resolve(ctx, "org-1", ScopeAgent, "a1"); found {
		t.Fatal("no rows = unlimited")
	}
	upsert(t, store, "org-1", Config{Scope: ScopeAgent, Subject: "a1", RPM: 3, Enabled: true})
	// Still negative within the TTL (documented convergence bound).
	if _, found, _ := r.Resolve(ctx, "org-1", ScopeAgent, "a1"); found {
		t.Fatal("negative cache must hold within TTL")
	}
	r.Invalidate("org-1")
	if limit, found, _ := r.Resolve(ctx, "org-1", ScopeAgent, "a1"); !found || limit.RPM != 3 {
		t.Fatalf("invalidation must expose the new row: %+v %v", limit, found)
	}
}
