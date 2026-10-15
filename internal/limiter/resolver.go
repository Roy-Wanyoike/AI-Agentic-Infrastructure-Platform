package limiter

// resolver.go — limit RESOLUTION (issue #124 requirement 2): which Limit
// applies to one (org, scope, subject) check.
//
// Resolution order (documented in the issue):
//
//      1. subject-specific row   (org, scope, subject)   — when present AND
//         enabled; when present AND disabled it SUPPRESSES the org default for
//         that subject (the explicit per-subject opt-out);
//      2. org default row        (org, scope, "")        — when present and
//         enabled;
//      3. unlimited              (no row)                — the platform default.
//
// Resolution runs against a Store (migration 036 rate_limits table; the
// policies package has no rate-limit condition type and is not modified).
// A short TTL cache bounds the per-check lookup cost on the enforcement
// hot path; writes through the Enforcer (UpsertConfig/DeleteConfig)
// invalidate the org's cached entries immediately, so the API surface and
// enforcement converge within the TTL even when the table is edited out of
// band.

import (
	"context"
	"errors"
	"sync"
	"time"
)

// DefaultResolverTTL bounds how long a resolved Limit may be reused.
const DefaultResolverTTL = 30 * time.Second

// Resolver resolves the effective Limit for (org, scope, subject) checks.
// It is safe for concurrent use.
type Resolver struct {
	store Store
	ttl   time.Duration

	mu      sync.RWMutex
	cache   map[string]cacheEntry
	nowFunc func() time.Time
}

type cacheEntry struct {
	limit     Limit
	found     bool // false = resolved to unlimited (negative cache)
	expiresAt time.Time
}

// ResolverOption configures a Resolver.
type ResolverOption func(*Resolver)

// WithResolverTTL overrides the cache TTL (0 disables caching).
func WithResolverTTL(ttl time.Duration) ResolverOption {
	return func(r *Resolver) { r.ttl = ttl }
}

// WithResolverClock overrides the cache clock (deterministic tests).
func WithResolverClock(now func() time.Time) ResolverOption {
	return func(r *Resolver) {
		if now != nil {
			r.nowFunc = now
		}
	}
}

// NewResolver builds a Resolver over the store.
func NewResolver(store Store, opts ...ResolverOption) *Resolver {
	r := &Resolver{
		store:   store,
		ttl:     DefaultResolverTTL,
		cache:   make(map[string]cacheEntry),
		nowFunc: time.Now,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Resolve returns the effective Limit and whether any limit applies (false =
// unlimited). A missing row (Store.ErrConfigNotFound) is the unlimited case,
// not an error; other store failures surface to the caller, which applies
// the fail mode (see Limiter.Allow).
func (r *Resolver) Resolve(ctx context.Context, orgID string, scope Scope, subject string) (Limit, bool, error) {
	ck := cacheKey(orgID, scope, subject)
	if r.ttl > 0 {
		r.mu.RLock()
		if entry, ok := r.cache[ck]; ok && r.nowFunc().Before(entry.expiresAt) {
			r.mu.RUnlock()
			return entry.limit, entry.found, nil
		}
		r.mu.RUnlock()
	}

	// 1. subject-specific row (when the check addresses a subject).
	if subject != "" {
		cfg, err := r.store.Get(ctx, orgID, scope, subject)
		if err != nil && !errors.Is(err, ErrConfigNotFound) {
			return Limit{}, false, err
		}
		if cfg != nil {
			return r.store1(ck, *cfg)
		}
	}
	// 2. org default row.
	def, err := r.store.Get(ctx, orgID, scope, "")
	if err != nil && !errors.Is(err, ErrConfigNotFound) {
		return Limit{}, false, err
	}
	if def != nil {
		return r.store1(ck, *def)
	}
	// 3. unlimited.
	r.remember(ck, Limit{}, false)
	return Limit{}, false, nil
}

// store1 applies the enabled flag of a resolved row and caches the outcome.
// A disabled row means "no limits" (explicit opt-out), not "not found".
func (r *Resolver) store1(ck string, cfg Config) (Limit, bool, error) {
	if !cfg.Enabled {
		r.remember(ck, Limit{}, false)
		return Limit{}, false, nil
	}
	limit := Limit{Scope: cfg.Scope, RPM: cfg.RPM, RPD: cfg.RPD, TPM: cfg.TPM}
	r.remember(ck, limit, limit.Active())
	return limit, limit.Active(), nil
}

func (r *Resolver) remember(ck string, limit Limit, found bool) {
	if r.ttl <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache[ck] = cacheEntry{limit: limit, found: found, expiresAt: r.nowFunc().Add(r.ttl)}
}

// Invalidate drops every cached resolution of one organization (called by
// the Enforcer after UpsertConfig/DeleteConfig so management writes are
// visible to enforcement immediately).
func (r *Resolver) Invalidate(orgID string) {
	if r == nil {
		return
	}
	prefix := orgID + "\x00"
	r.mu.Lock()
	defer r.mu.Unlock()
	for ck := range r.cache {
		if len(ck) >= len(prefix) && ck[:len(prefix)] == prefix {
			delete(r.cache, ck)
		}
	}
}

func cacheKey(orgID string, scope Scope, subject string) string {
	return orgID + "\x00" + string(scope) + "\x00" + subject
}
