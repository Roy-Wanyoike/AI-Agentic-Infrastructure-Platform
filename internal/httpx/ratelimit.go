package httpx

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"agentos/internal/observability"
)

// Task 2-c (governance): distributed rate limiting middleware.
//
//   - Redis path: sliding-window limiter keyed `ratelimit:{scope}:{id}` where
//     scope identifies the route family (auth | api | execute | ...) and id is
//     the caller identity (API key / bearer token hash / client IP).
//   - Fallback path: when Redis is nil (zero-infrastructure mode) or errors,
//     the per-key in-memory observability.RateLimiter is used.
//   - Configuration: AGENTOS_RATE_LIMIT_RPM (requests per minute, default 120,
//     see RateLimitFromEnv); the constructor accepts limit/window explicitly.
//   - When the limit is reached the middleware responds 429 with a Retry-After
//     header (seconds) and the structured JSON error model.

// ErrorCodeRateLimited is the machine-readable code emitted on 429 responses.
const ErrorCodeRateLimited = "rate_limited"

// HeaderRateLimitScope / HeaderRateLimitIdentity let inner handlers pin the
// limiter scope and caller identity for a request (context helpers below are
// preferred). They are exported for tests and route wrappers.
const (
	RateLimitScopeDefault = "api"
	RetryAfterHeader      = "Retry-After"
	XRateLimitLimit       = "X-RateLimit-Limit"
	XRateLimitRemaining   = "X-RateLimit-Remaining"
)

type rateLimitScopeKey struct{}
type rateLimitIdentityKey struct{}

// WithRateLimitScope returns a context carrying the limiter scope (e.g.
// "auth", "api", "execute"). To give a whole route a distinct bucket, inject
// the context BEFORE the rate limit middleware runs (scope wrapper outermost):
//
//	scoped := func(next http.Handler) http.Handler {
//	    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
//	        next.ServeHTTP(w, r.WithContext(WithRateLimitScope(r.Context(), "execute")))
//	    })
//	}
//	handler := scoped(rateLimitMiddleware(inner))
func WithRateLimitScope(ctx context.Context, scope string) context.Context {
	if strings.TrimSpace(scope) == "" {
		return ctx
	}
	return context.WithValue(ctx, rateLimitScopeKey{}, strings.TrimSpace(scope))
}

// RateLimitScopeFromContext returns the request scope previously attached with
// WithRateLimitScope, or "" when absent.
func RateLimitScopeFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	scope, _ := ctx.Value(rateLimitScopeKey{}).(string)
	return scope
}

// WithRateLimitIdentity pins the caller identity used to build the limiter
// key. Authentication middleware can use this to rate limit by organization
// instead of raw credentials.
func WithRateLimitIdentity(ctx context.Context, id string) context.Context {
	if strings.TrimSpace(id) == "" {
		return ctx
	}
	return context.WithValue(ctx, rateLimitIdentityKey{}, strings.TrimSpace(id))
}

// rateLimitIdentityFromContext returns the pinned identity, or "" when absent.
func rateLimitIdentityFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(rateLimitIdentityKey{}).(string)
	return id
}

// RateLimitFromEnv resolves the rate limit configuration from the
// AGENTOS_RATE_LIMIT_RPM environment variable (requests per minute). The
// default is 120 rpm over a one-minute window. Invalid values fall back to the
// default. internal/config keeps its settings unexported, so the env contract
// for this middleware lives here.
func RateLimitFromEnv() (limit int, window time.Duration) {
	limit = 120
	if raw := strings.TrimSpace(os.Getenv("AGENTOS_RATE_LIMIT_RPM")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	return limit, time.Minute
}

// RedisClientFromEnv returns a Redis client for the rate limiter from
// REDIS_ADDR (host:port) or REDIS_URL (redis:// URL), or nil when neither is
// set (zero-infrastructure mode). The middleware treats nil as "use the
// in-memory fallback".
func RedisClientFromEnv() *redis.Client {
	if addr := strings.TrimSpace(os.Getenv("REDIS_ADDR")); addr != "" {
		return redis.NewClient(&redis.Options{Addr: addr})
	}
	if rawURL := strings.TrimSpace(os.Getenv("REDIS_URL")); rawURL != "" {
		opts, err := redis.ParseURL(rawURL)
		if err == nil {
			return redis.NewClient(opts)
		}
		slog.Warn("invalid REDIS_URL; rate limiter will use the in-memory fallback", "error", err.Error())
	}
	return nil
}

// rateLimitScript is an atomic sliding-window rate limiter. It trims entries
// older than the window, rejects when the bucket is full (returning the
// remaining backoff in milliseconds) and otherwise records the request.
//
// KEYS[1] = ratelimit:{scope}:{id}
// ARGV[1] = now (ms since epoch)
// ARGV[2] = window (ms)
// ARGV[3] = limit (max requests per window)
// ARGV[4] = unique member for this request
//
// Returns {allowed(0|1), retry_ms, count}.
var rateLimitScript = redis.NewScript(`
local key = KEYS[1]
local now = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
local limit = tonumber(ARGV[3])
local member = ARGV[4]
redis.call('ZREMRANGEBYSCORE', key, 0, now - window)
local count = redis.call('ZCARD', key)
if count >= limit then
    local retry = window
    local oldest = redis.call('ZRANGE', key, 0, 0, 'WITHSCORES')
    if oldest[2] then
        retry = tonumber(oldest[2]) + window - now
        if retry < 1 then retry = 1 end
    end
    redis.call('PEXPIRE', key, window)
    return {0, retry, count}
end
redis.call('ZADD', key, now, member)
redis.call('PEXPIRE', key, window)
return {1, 0, redis.call('ZCARD', key)}
`)

// NewRateLimitMiddleware returns middleware enforcing `limit` requests per
// `window` per caller. rdb is the optional Redis client; when it is nil (or a
// Redis call fails) the middleware falls back to the in-memory
// observability.RateLimiter. When both backends are unavailable requests fail
// open (allowed) so the API stays reachable.
func NewRateLimitMiddleware(rdb *redis.Client, fallback *observability.RateLimiter, limit int, window time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if limit <= 0 || window <= 0 {
				// Disabled or misconfigured: fail open.
				next.ServeHTTP(w, r)
				return
			}
			scope := RateLimitScopeFromContext(r.Context())
			if scope == "" {
				scope = RateLimitScopeDefault
			}
			identity := rateLimitIdentityFromContext(r.Context())
			if identity == "" {
				identity = rateLimitCallerIdentity(r)
			}
			key := fmt.Sprintf("ratelimit:%s:%s", scope, identity)

			allowed, retryAfter, remaining := consumeRateLimit(r.Context(), rdb, fallback, key, limit, window)

			w.Header().Set(XRateLimitLimit, strconv.Itoa(limit))
			if remaining >= 0 {
				w.Header().Set(XRateLimitRemaining, strconv.Itoa(remaining))
			}
			if !allowed {
				seconds := int(math.Ceil(float64(retryAfter) / float64(time.Millisecond)))
				if seconds < 1 {
					seconds = 1
				}
				w.Header().Set(RetryAfterHeader, strconv.Itoa(seconds))
				WriteError(w, r, http.StatusTooManyRequests, ErrorCodeRateLimited,
					fmt.Sprintf("rate limit exceeded: max %d requests per %s; retry in %d seconds", limit, window, seconds))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// consumeRateLimit tries Redis first and falls back to the in-memory limiter.
// It returns (allowed, retryAfterMs, remaining); remaining is -1 when unknown.
func consumeRateLimit(ctx context.Context, rdb *redis.Client, fallback *observability.RateLimiter, key string, limit int, window time.Duration) (bool, int64, int) {
	if rdb != nil {
		nowMs := time.Now().UnixMilli()
		member := rateLimitMember()
		res, err := rateLimitScript.Run(ctx, rdb, []string{key},
			nowMs, window.Milliseconds(), limit, member).Result()
		if err == nil {
			if vals, ok := res.([]interface{}); ok && len(vals) >= 3 {
				allowed, _ := vals[0].(int64)
				retryMs, _ := vals[1].(int64)
				count, _ := vals[2].(int64)
				remaining := limit - int(count)
				if remaining < 0 {
					remaining = 0
				}
				if allowed == 1 {
					return true, 0, remaining
				}
				return false, retryMs, remaining
			}
		} else {
			slog.Warn("rate limiter falling back to in-memory", "error", err.Error())
		}
	}
	if fallback != nil {
		// The in-memory limiter owns its own window; retry-after is the full
		// window because it cannot report the age of the oldest hit.
		if fallback.Allow(key) {
			return true, 0, -1
		}
		return false, window.Milliseconds(), 0
	}
	// No backends available: fail open.
	return true, 0, -1
}

// rateLimitMember builds a unique sorted-set member for one request.
func rateLimitMember() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return fmt.Sprintf("%d:%s", time.Now().UnixNano(), hex.EncodeToString(buf))
}

// rateLimitCallerIdentity derives the caller identity from request headers:
// the API key or bearer token (hashed, never logged raw) or, failing those,
// the client IP. Identity headers keep the limiter usable in front of the
// auth middleware (e.g. on /auth/login).
func rateLimitCallerIdentity(r *http.Request) string {
	if id := rateLimitIdentityFromContext(r.Context()); id != "" {
		return id
	}
	if key := strings.TrimSpace(r.Header.Get("X-API-Key")); key != "" {
		return "key:" + shortHash(key)
	}
	if authz := strings.TrimSpace(r.Header.Get("Authorization")); authz != "" {
		return "token:" + shortHash(authz)
	}
	return "ip:" + clientIP(r)
}

// clientIP extracts the remote host without port, honouring the first entry
// of X-Forwarded-For when present.
func clientIP(r *http.Request) string {
	if fwd := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); fwd != "" {
		if first := strings.Split(fwd, ",")[0]; strings.TrimSpace(first) != "" {
			return strings.TrimSpace(first)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	out := hex.EncodeToString(sum[:])
	if len(out) > 16 {
		out = out[:16]
	}
	return out
}
