// Package limiter implements issue #124: granular, per-scope rate limiting
// beyond the global HTTP rate limiter in internal/httpx (which stays
// untouched — this package only reuses its sliding-window ideas).
//
// Hierarchy (enforced at three seams):
//
//	Organization → Agent (RPM/RPD) → Tool (RPM) → Model (RPM/TPM)
//
//	- run creation (cmd/api)      -> org + agent scopes, 429 on denial
//	- tool invocation (runtime)   -> tool scope, denial recorded on the step,
//	                                 the run continues
//	- model calls (runtime)       -> model scope (RPM) + TPM accounting, a
//	                                 denial fails the run before the provider
//	                                 call is issued
//
// ENGINE. *Limiter is the sliding-window counter engine. Limits RESOLVE from
// rate-limit configuration (migration 036 rate_limits table, see store.go /
// resolver.go; the policies package has no rate-limit condition type and is
// intentionally not modified) and CHECK atomically across every active
// window: one Lua script trims, counts and adds against all of a key's
// windows in a single atomic Redis script, so a denial never partially
// consumes another window and the remaining budget of every window is
// reported on every decision (Decision.Remaining). Window keys are
//
//	ratelimit:{org}:{scope}:{id}:{window}
//
// (the {org} hash tag keeps every key of one check in the same Redis Cluster
// slot). The in-memory fallback ports the internal/httpx fallback semantics:
// when Redis is nil (zero-infrastructure mode) or errors, per-process
// in-memory sliding windows keep enforcement approximate instead of absent.
//
// FAILURE MODE. Consistent with the global limiter the default is fail-open:
// AGENTOS_LIMITER_FAIL_MODE=open allows traffic when the limiter cannot
// evaluate (Redis down with no fallback, or a failing configuration store).
// AGENTOS_LIMITER_FAIL_MODE=closed denies instead (reason
// limiter_unavailable) for operators who prefer availability loss over
// over-admission. The tradeoff, explicitly:
//
//	open   (default) — an infra outage degrades to the in-memory approximation
//	                   or to allowing; the platform never becomes unreachable
//	                   because its limiter is, but limits can be exceeded.
//	closed           — infra outages deny traffic; limits are never exceeded,
//	                   but a limiter outage IS an outage.
//
// RPD is a ROLLING 24h window (not UTC midnight): deterministic, avoids the
// midnight thundering herd, and matches the sliding-window engine without a
// second counter shape. TPM counts tokens against the model scope; because a
// provider response's true token count is only known AFTER the call, TPM is
// enforced pre-call against the tokens already recorded in the window and the
// actual usage is charged afterwards via Report (see enforcer.go) — the
// standard one-call-granularity token-limit semantics.
//
// NAMING. The issue asks for BOTH a `Limiter` struct (this file, the engine)
// and "hooks behind a Limiter interface": the hook seams are consumer-side
// interfaces (internal/runtime.GranularLimiter, cmd/api.limiterGate) that
// *Enforcer satisfies, with a no-op default so unwired modes (tests,
// zero-config development) are unaffected.
package limiter

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Scope identifies one enforcement tier of the issue #124 hierarchy.
type Scope string

// The four scopes. ScopeOrg has no subject (the org default IS the subject);
// the other scopes resolve subject-specific rows over the org default.
const (
	ScopeOrg   Scope = "org"
	ScopeAgent Scope = "agent"
	ScopeTool  Scope = "tool"
	ScopeModel Scope = "model"
)

// ParseScope parses a wire/config scope value.
func ParseScope(raw string) (Scope, bool) {
	scope := Scope(strings.ToLower(strings.TrimSpace(raw)))
	switch scope {
	case ScopeOrg, ScopeAgent, ScopeTool, ScopeModel:
		return scope, true
	default:
		return "", false
	}
}

// Valid reports whether the scope is one of the four known scopes.
func (s Scope) Valid() bool {
	_, ok := ParseScope(string(s))
	return ok
}

// Windows used by the engine. RPM and TPM share the one-minute window (as
// separate keys); RPD is the documented rolling 24h window.
const (
	MinuteWindow = time.Minute
	DayWindow    = 24 * time.Hour
)

// windowName values name the per-window counters inside Decision.Remaining
// ("agent:rpm", "model:tpm", ...).
const (
	windowRPM = "rpm"
	windowRPD = "rpd"
	windowTPM = "tpm"
)

// Machine-readable reasons carried on Decision. ReasonRateLimited matches the
// HTTP error code and the runtime step code so one vocabulary spans run
// creation, steps and metrics.
const (
	ReasonRateLimited        = "rate_limited"
	ReasonLimiterUnavailable = "limiter_unavailable"
)

// FailMode selects the behavior when the limiter cannot evaluate (see the
// package doc for the tradeoff).
type FailMode string

// Fail mode values and the environment variable that selects them.
const (
	FailModeOpen   FailMode = "open"
	FailModeClosed FailMode = "closed"

	// FailModeEnvVar selects the failure behavior; invalid values fall back
	// to the default (open) with no error.
	FailModeEnvVar = "AGENTOS_LIMITER_FAIL_MODE"
)

// FailModeFromEnv resolves AGENTOS_LIMITER_FAIL_MODE (open|closed, default
// open).
func FailModeFromEnv() FailMode {
	switch FailMode(strings.ToLower(strings.TrimSpace(os.Getenv(FailModeEnvVar)))) {
	case FailModeClosed:
		return FailModeClosed
	default:
		return FailModeOpen
	}
}

// Limit is one configured limit set. Zero values mean "unlimited" for that
// dimension (the store validation rejects negatives); a Limit with no
// positive field is inactive and never denies.
type Limit struct {
	Scope Scope `json:"scope"`
	// RPM: requests per minute window.
	RPM int `json:"rpm"`
	// RPD: requests per rolling 24h window.
	RPD int `json:"rpd"`
	// TPM: tokens per minute window (model scope).
	TPM int `json:"tpm"`
}

// Active reports whether any dimension is configured.
func (l Limit) Active() bool {
	return l.RPM > 0 || l.RPD > 0 || l.TPM > 0
}

// LimitKey identifies the counter namespace for one check.
type LimitKey struct {
	OrgID string
	Scope Scope
	// Subject is the agent id / tool id / model name; "" addresses the org
	// default of the scope (and the org scope itself, which has no subject).
	Subject string
}

// Tokens is the cost one check charges against the windows.
type Tokens struct {
	// Requests counts against RPM (minute window) and RPD (rolling 24h).
	Requests int
	// Tokens counts against TPM (minute window, model scope).
	Tokens int
}

// Decision is the outcome of one Allow check. Remaining carries the budget
// left in every checked window, keyed "<scope>:<window>" (e.g. "agent:rpm");
// windows that are unlimited (or not checked) are absent.
type Decision struct {
	Allowed bool
	// Reason explains a denial ("agent RPM limit exceeded: 3 used of 3 in
	// the rolling window") or the fail-mode behavior on infrastructure
	// errors.
	Reason string
	// RetryAfter is when the FIRST denying window frees up (the minimum over
	// the denied windows); zero when allowed.
	RetryAfter time.Duration
	// Remaining maps "<scope>:<window>" to the units still available after
	// the check (clamped at 0).
	Remaining map[string]int
	// Scope is the scope whose window denied ("agent" for an agent RPM
	// denial); empty when allowed.
	Scope Scope
}

// remainingKey builds the Decision.Remaining key for one scope/window pair.
func remainingKey(scope Scope, window string) string {
	return string(scope) + ":" + window
}

// redisKey renders the Redis key for one scope/window pair:
// ratelimit:{org}:{scope}:{id}:{window}. The {org} braces are a Redis
// Cluster hash tag: all windows of one check share a slot, which keeps the
// multi-window Lua script cluster-safe. name is the window NAME (rpm/rpd/
// tpm), NOT the duration: RPM and TPM share the one-minute window and must
// land in distinct keys.
func (k LimitKey) redisKey(name string) string {
	return fmt.Sprintf("ratelimit:{%s}:%s:%s:%s", k.OrgID, k.Scope, k.Subject, name)
}

func windowName(window time.Duration) string {
	if window == DayWindow {
		return windowRPD
	}
	return windowRPM
}

// windowSpec is one active window of a check.
type windowSpec struct {
	name   string
	window time.Duration
	cap    int
	// weight is the cost charged to this window by the current check.
	weight int
}

// windowsFor builds the active window list for a limit + cost pair.
func windowsFor(limit Limit, cost Tokens) []windowSpec {
	specs := make([]windowSpec, 0, 3)
	if limit.RPM > 0 {
		specs = append(specs, windowSpec{name: windowRPM, window: MinuteWindow, cap: limit.RPM, weight: cost.Requests})
	}
	if limit.RPD > 0 {
		specs = append(specs, windowSpec{name: windowRPD, window: DayWindow, cap: limit.RPD, weight: cost.Requests})
	}
	if limit.TPM > 0 {
		specs = append(specs, windowSpec{name: windowTPM, window: MinuteWindow, cap: limit.TPM, weight: cost.Tokens})
	}
	return specs
}

// Limiter is the granular sliding-window counter engine (issue #124). It
// resolves the Limit for a key through the optional Resolver, then checks
// every active window atomically: Redis via one multi-key Lua script when a
// client is configured, per-process in-memory sliding windows otherwise.
// The zero-configuration constructor (nil redis client, nil resolver) never
// denies.
type Limiter struct {
	rdb      *redis.Client
	resolver *Resolver
	fallback *memoryWindows
	failMode FailMode
	now      func() time.Time
	member   func() string
}

// Option configures a Limiter.
type Option func(*Limiter)

// WithFailMode overrides AGENTOS_LIMITER_FAIL_MODE for this engine (tests,
// embedded runs).
func WithFailMode(mode FailMode) Option {
	return func(l *Limiter) {
		if mode == FailModeOpen || mode == FailModeClosed {
			l.failMode = mode
		}
	}
}

// WithClock overrides the wall clock (deterministic tests).
func WithClock(now func() time.Time) Option {
	return func(l *Limiter) {
		if now != nil {
			l.now = now
		}
	}
}

// NewLimiter builds the engine. rdb is the optional Redis client (nil = the
// in-memory fallback owns every check, zero-infrastructure mode); resolver
// resolves the configured Limit per key (nil = unlimited: every check
// allows). The failure mode defaults to AGENTOS_LIMITER_FAIL_MODE.
func NewLimiter(rdb *redis.Client, resolver *Resolver, opts ...Option) *Limiter {
	l := &Limiter{
		rdb:      rdb,
		resolver: resolver,
		fallback: newMemoryWindows(),
		failMode: FailModeFromEnv(),
		now:      time.Now,
		member:   newWindowMember,
	}
	for _, opt := range opts {
		opt(l)
	}
	return l
}

// Allow checks the request against every active window of the key's
// configured Limit. Decision matrix:
//
//   - no configured limit (or resolver absent) -> allowed, Remaining empty
//   - every window has budget                  -> allowed, all consumed
//     atomically (no partial consumption on denial)
//   - any window exceeded                      -> denied with the scope, the
//     minimum RetryAfter over the denied windows and the Remaining of every
//     window (a denial consumes NOTHING: Remaining reports the units still
//     in the window, cap minus prior usage)
//   - Redis error, fail-mode open              -> in-memory fallback verdict
//   - Redis error, fail-mode closed            -> denied limiter_unavailable
//   - resolver/store error                     -> fail-mode verdict (see the
//     package doc: open allows, closed denies)
//
// The returned error is non-nil only for resolver failures and is already
// reflected in the Decision (callers that only look at Decision need no
// error handling).
func (l *Limiter) Allow(ctx context.Context, key LimitKey, cost Tokens) (Decision, error) {
	return l.AllowBatch(ctx, []LimitKey{key}, cost)
}

// keyedSpec pairs one active window with the key it belongs to (the batch
// check evaluates (key, window) pairs across keys in hierarchy order).
type keyedSpec struct {
	key  LimitKey
	spec windowSpec
}

// AllowBatch checks SEVERAL keys in ONE atomic pass — the run-creation gate
// (org scope + agent scope) needs it: a denied request must consume NOTHING
// from ANY scope, which sequential per-key checks cannot guarantee (the org
// window would consume even when the agent window denies). Decision matrix
// per Allow; across keys additionally:
//
//   - every key has budget -> all keys consumed atomically
//   - any key exceeded     -> denied consuming NOTHING on any key; Decision.
//     Scope names the first denying key in input order (hierarchy order)
//     and Remaining reports every checked window of every key
//
// Redis: all (key, window) pairs go through the ONE multi-key Lua script (a
// cluster-safe hash tag requires the caller to group keys per org, which the
// run gate does by construction). Memory: one locked two-phase pass.
func (l *Limiter) AllowBatch(ctx context.Context, keys []LimitKey, cost Tokens) (Decision, error) {
	if l == nil {
		return Decision{Allowed: true, Reason: "limiter disabled"}, nil
	}
	pairs := make([]keyedSpec, 0, len(keys)*3)
	for _, key := range keys {
		limit, err := l.resolve(ctx, key)
		if err != nil {
			return l.unavailableDecision(), err
		}
		for _, spec := range windowsFor(limit, cost) {
			pairs = append(pairs, keyedSpec{key: key, spec: spec})
		}
	}
	if len(pairs) == 0 {
		// Nothing configured: nothing to check, nothing to report.
		return Decision{Allowed: true, Reason: "no limit configured"}, nil
	}
	if l.rdb != nil {
		decision, rerr := l.allowBatchRedis(ctx, pairs)
		if rerr == nil {
			return decision, nil
		}
		slog.Warn("granular limiter: redis check failed",
			"error", rerr.Error(), "keys", len(keys), "org", keys[0].OrgID)
		if l.failMode == FailModeClosed {
			// Fail-closed operators prioritize exactness: a per-process
			// fallback would under-count across replicas and admit traffic
			// the configured limit forbids.
			return l.unavailableDecision(), nil
		}
		// Fail-open (default): port of the httpx semantics — fall through to
		// the in-memory fallback so enforcement stays approximate, not absent.
	}
	return *l.fallback.allowBatch(l.now(), pairs), nil
}

// Report charges cost to the key's windows WITHOUT an allow check. It is the
// post-call TPM accounting seam: the model seam checks RPM/TPM before a
// provider call (Requests: 1, zero tokens) and reports the response's true
// token usage here afterwards. Windows without a positive cost weight are
// left untouched; Report never fails the caller's flow (errors are logged).
func (l *Limiter) Report(ctx context.Context, key LimitKey, cost Tokens) error {
	if l == nil {
		return nil
	}
	if cost.Tokens <= 0 && cost.Requests <= 0 {
		return nil
	}
	limit, err := l.resolve(ctx, key)
	if err != nil {
		return err
	}
	if !limit.Active() {
		return nil
	}
	var specs []windowSpec
	if limit.TPM > 0 && cost.Tokens > 0 {
		specs = append(specs, windowSpec{name: windowTPM, window: MinuteWindow, cap: limit.TPM, weight: cost.Tokens})
	}
	if limit.RPM > 0 && cost.Requests > 0 {
		specs = append(specs, windowSpec{name: windowRPM, window: MinuteWindow, cap: limit.RPM, weight: cost.Requests})
	}
	if limit.RPD > 0 && cost.Requests > 0 {
		specs = append(specs, windowSpec{name: windowRPD, window: DayWindow, cap: limit.RPD, weight: cost.Requests})
	}
	if len(specs) == 0 {
		return nil
	}
	if l.rdb != nil {
		if rerr := l.reportRedis(ctx, key, specs); rerr == nil {
			return nil
		} else {
			slog.Warn("granular limiter: redis report failed",
				"error", rerr.Error(), "scope", string(key.Scope), "org", key.OrgID)
			if l.failMode == FailModeClosed {
				return rerr
			}
		}
		// Fail-open: charge the in-memory fallback so the next pre-call
		// check on this process sees the usage.
	}
	l.fallback.add(l.now(), key, specs)
	return nil
}

// resolve fetches the effective Limit for the key. A nil resolver means
// unlimited. Resolution failures are infrastructure errors and follow the
// fail mode (see Allow).
func (l *Limiter) resolve(ctx context.Context, key LimitKey) (Limit, error) {
	if l.resolver == nil {
		return Limit{}, nil
	}
	limit, found, err := l.resolver.Resolve(ctx, key.OrgID, key.Scope, key.Subject)
	if err != nil {
		return Limit{}, err
	}
	if !found {
		return Limit{}, nil
	}
	return limit, nil
}

// unavailableDecision builds the fail-mode verdict for infrastructure
// errors. Fail-open allows (the platform must not become unreachable because
// its limiter is); fail-closed denies with limiter_unavailable and a
// conservative 1s retry hint so immediate caller retries cannot hot-loop a
// broken backend.
func (l *Limiter) unavailableDecision() Decision {
	if l.failMode == FailModeClosed {
		return Decision{
			Allowed:    false,
			Reason:     ReasonLimiterUnavailable + ": rate limiter unavailable (fail closed)",
			RetryAfter: time.Second,
			Remaining:  map[string]int{},
		}
	}
	return Decision{Allowed: true, Reason: ReasonLimiterUnavailable + ": fail open"}
}

// granularScript is the atomic multi-window sliding-window check (the httpx
// single-window pattern generalized to N windows in ONE script so a check is
// atomic ACROSS windows and never partially consumes on denial).
//
//	KEYS[i]        = ratelimit:{org}:{scope}:{id}:{window}
//	ARGV[1]        = now (ms since epoch)
//	ARGV[2]        = unique member base for this check
//	ARGV[3+3(i-1)] = window i duration (ms)
//	ARGV[4+3(i-1)] = window i limit
//	ARGV[5+3(i-1)] = window i weight (cost)
//
// Phase 1 trims (ZREMRANGEBYSCORE) and sums (ZRANGE + weight parse) every
// window; any exceeded window denies with the MINIMUM retry. Phase 2 adds
// the weighted member (ZADD) and refreshes TTL (PEXPIRE) on every window.
// Members are "<weight>:<uniq>:<i>" so the weighted sum is computable.
//
// Returns {allowed(0|1), retry_ms, denied_index, total_1, total_2, ...} where
// total_i is window i's used total BEFORE any add (absent when i > #KEYS).
var granularScript = redis.NewScript(`
local now = tonumber(ARGV[1])
local member = ARGV[2]
local denied = false
local first_retry = nil
local totals = {}
for i = 1, #KEYS do
    local window = tonumber(ARGV[3 + (i - 1) * 3])
    local limit = tonumber(ARGV[4 + (i - 1) * 3])
    local weight = tonumber(ARGV[5 + (i - 1) * 3])
    redis.call('ZREMRANGEBYSCORE', KEYS[i], 0, now - window)
    local total = 0
    local entries = redis.call('ZRANGE', KEYS[i], 0, -1)
    for _, e in ipairs(entries) do
        local w = tonumber(string.match(e, '^(%d+):'))
        total = total + (w or 1)
    end
    totals[i] = total
    if total + weight > limit then
        denied = true
        local retry = window
        if entries[1] then
            retry = tonumber(redis.call('ZSCORE', KEYS[i], entries[1])) + window - now
            if retry < 1 then retry = 1 end
        end
        if first_retry == nil or retry < first_retry then
            first_retry = retry
        end
    end
end
if denied then
    -- Refresh TTLs (PEXPIRE) so idle denied keys cannot linger forever.
    for i = 1, #KEYS do
        redis.call('PEXPIRE', KEYS[i], tonumber(ARGV[3 + (i - 1) * 3]))
    end
    local out = {0, first_retry or 1}
    for i = 1, #KEYS do out[2 + i] = totals[i] end
    return out
end
for i = 1, #KEYS do
    local window = tonumber(ARGV[3 + (i - 1) * 3])
    local weight = tonumber(ARGV[5 + (i - 1) * 3])
    redis.call('ZADD', KEYS[i], now, tostring(weight) .. ':' .. member .. ':' .. tostring(i))
    redis.call('PEXPIRE', KEYS[i], window)
end
local out = {1, 0}
for i = 1, #KEYS do out[2 + i] = totals[i] end
return out
`)

// allowBatchRedis evaluates the whole batch atomically in Redis (one script
// run over every (key, window) pair).
func (l *Limiter) allowBatchRedis(ctx context.Context, pairs []keyedSpec) (Decision, error) {
	now := l.now()
	keys := make([]string, 0, len(pairs))
	args := make([]interface{}, 0, 2+3*len(pairs))
	args = append(args, now.UnixMilli(), l.member())
	for _, ps := range pairs {
		keys = append(keys, ps.key.redisKey(ps.spec.name))
		args = append(args, ps.spec.window.Milliseconds(), ps.spec.cap, ps.spec.weight)
	}
	res, err := granularScript.Run(ctx, l.rdb, keys, args...).Result()
	if err != nil {
		return Decision{}, err
	}
	vals, ok := res.([]interface{})
	if !ok || len(vals) < 2 {
		return Decision{}, fmt.Errorf("limiter: unexpected script response %T", res)
	}
	allowed, _ := vals[0].(int64)
	retryMs, _ := vals[1].(int64)
	totals := make([]int64, 0, len(pairs))
	for _, v := range vals[2:] {
		n, _ := v.(int64)
		totals = append(totals, n)
	}

	decision := Decision{Allowed: allowed == 1, Remaining: make(map[string]int, len(pairs))}
	for i, ps := range pairs {
		used := int64(0)
		if i < len(totals) {
			used = totals[i]
		}
		remaining := ps.spec.cap - int(used)
		if allowed == 1 {
			remaining -= ps.spec.weight // the check just consumed its cost
		}
		if remaining < 0 {
			remaining = 0
		}
		decision.Remaining[remainingKey(ps.key.Scope, ps.spec.name)] = remaining
	}
	if decision.Allowed {
		return decision, nil
	}
	ps, total, ok := firstDenying(pairs, totals)
	if ok {
		decision.Scope = ps.key.Scope
		decision.Reason = denyReason(ps.key, ps.spec, total)
	}
	if retryMs > 0 {
		decision.RetryAfter = time.Duration(retryMs) * time.Millisecond
	} else if len(pairs) > 0 {
		decision.RetryAfter = pairs[0].spec.window
	}
	return decision, nil
}

// reportRedis charges weighted members without an allow check (post-call TPM
// accounting).
func (l *Limiter) reportRedis(ctx context.Context, key LimitKey, specs []windowSpec) error {
	now := l.now().UnixMilli()
	member := l.member()
	pipe := l.rdb.Pipeline()
	for i, spec := range specs {
		k := key.redisKey(spec.name)
		cutoff := strconv.FormatInt(now-spec.window.Milliseconds(), 10)
		pipe.ZRemRangeByScore(ctx, k, "0", cutoff)
		pipe.ZAdd(ctx, k, redis.Z{Score: float64(now), Member: fmt.Sprintf("%d:%s:%d", spec.weight, member, i)})
		pipe.PExpire(ctx, k, spec.window)
	}
	_, err := pipe.Exec(ctx)
	return err
}

// firstDenying returns the FIRST (key, window) pair whose window would
// exceed its cap — hierarchy order, so an org denial names the org scope.
func firstDenying(pairs []keyedSpec, totals []int64) (keyedSpec, int64, bool) {
	for i, ps := range pairs {
		if i >= len(totals) {
			break
		}
		if totals[i]+int64(ps.spec.weight) > int64(ps.spec.cap) {
			return ps, totals[i], true
		}
	}
	if len(pairs) > 0 {
		return pairs[0], 0, false
	}
	return keyedSpec{}, 0, false
}

// denyReason renders the human-facing denial reason for one denying window:
// the usage OBSERVED in the window (a denial consumes nothing, so the usage
// never includes this check's cost).
func denyReason(key LimitKey, spec windowSpec, used int64) string {
	return fmt.Sprintf("%s %s limit exceeded: %d used of %d in the rolling window",
		string(key.Scope), strings.ToUpper(spec.name), used, spec.cap)
}

// newWindowMember builds a unique sorted-set member base for one check (the
// httpx pattern: nanotime + random hex, with a deterministic fallback when
// the random source fails).
func newWindowMember() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return fmt.Sprintf("%d:%s", time.Now().UnixNano(), hex.EncodeToString(buf))
}

// MemoryStats exposes the fallback's live entry count for one key/window
// (tests and diagnostics; not part of the enforcement contract).
func (l *Limiter) MemoryStats(key LimitKey, window time.Duration) int {
	if l == nil {
		return 0
	}
	return l.fallback.count(l.now(), key, window)
}

// CeilSeconds converts a retry duration to the Retry-After header value
// (>= 1s, matching the httpx convention).
func CeilSeconds(d time.Duration) int {
	if d <= 0 {
		return 1
	}
	seconds := int(math.Ceil(d.Seconds()))
	if seconds < 1 {
		return 1
	}
	return seconds
}
