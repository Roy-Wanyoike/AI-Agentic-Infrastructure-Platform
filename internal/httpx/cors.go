package httpx

import (
	"net/http"
	"strconv"
	"strings"
)

// CORSOptions configures the CORS middleware. The zero value is usable and
// falls back to the permissive development defaults returned by
// DefaultCORSOptions (allow every origin, common methods, and the headers
// this API actually consumes).
type CORSOptions struct {
	// AllowedOrigins is the origin allow-list. The wildcard "*" allows every
	// origin. Exact matches are case-insensitive (origins are compared
	// normalized to lowercase). Default: ["*"].
	AllowedOrigins []string
	// AllowedMethods is the method list advertised on preflight responses
	// (Access-Control-Allow-Methods). Default: GET, POST, PATCH, DELETE,
	// PUT, OPTIONS.
	AllowedMethods []string
	// AllowedHeaders is the request-header allow-list advertised on
	// preflight responses (Access-Control-Allow-Headers). Default includes
	// the headers this API consumes: Content-Type, Authorization, X-API-Key,
	// api_key (legacy query-param auth companion), X-Request-ID, Accept.
	AllowedHeaders []string
	// ExposedHeaders is set as Access-Control-Expose-Headers (optional).
	ExposedHeaders []string
	// AllowCredentials enables Access-Control-Allow-Credentials. Never
	// combined with the "*" origin wildcard: when credentials are allowed
	// the middleware echoes the exact request origin instead.
	AllowCredentials bool
	// MaxAgeSeconds is advertised as Access-Control-Max-Age on preflight
	// responses. Default: 86400 (24h).
	MaxAgeSeconds int
}

// DefaultCORSOptions returns the permissive development configuration
// matching what cmd/api/main.go hand-rolled previously (plus PATCH/DELETE
// which the API contract documents).
func DefaultCORSOptions() CORSOptions {
	return CORSOptions{
		AllowedOrigins: []string{"*"},
		AllowedMethods: []string{
			http.MethodGet, http.MethodPost, http.MethodPatch,
			http.MethodDelete, http.MethodPut, http.MethodOptions,
		},
		AllowedHeaders: []string{
			"Content-Type", "Authorization", "X-API-Key", "api_key",
			"X-Request-ID", "Accept",
		},
		MaxAgeSeconds: 86400,
	}
}

func (o CORSOptions) withDefaults() CORSOptions {
	def := DefaultCORSOptions()
	if len(o.AllowedOrigins) == 0 {
		o.AllowedOrigins = def.AllowedOrigins
	}
	if len(o.AllowedMethods) == 0 {
		o.AllowedMethods = def.AllowedMethods
	}
	if len(o.AllowedHeaders) == 0 {
		o.AllowedHeaders = def.AllowedHeaders
	}
	if o.MaxAgeSeconds <= 0 {
		o.MaxAgeSeconds = def.MaxAgeSeconds
	}
	return o
}

// originAllowed reports whether the request origin is allowed. allowAll is
// true when the configured list contains the "*" wildcard.
func originAllowed(origin string, allowed []string) (string, bool) {
	allowAll := false
	for _, candidate := range allowed {
		if candidate == "*" {
			allowAll = true
			continue
		}
		if strings.EqualFold(candidate, origin) {
			return candidate, true
		}
	}
	if allowAll {
		return "*", true
	}
	return "", false
}

func allowHeaderValue(list []string) string {
	return strings.Join(list, ",")
}

// CORS returns composable CORS middleware.
//
// Behavior:
//   - Preflight requests (OPTIONS carrying Access-Control-Request-Method) are
//     answered directly: 204 No Content plus the computed CORS headers.
//   - Every other request gains Access-Control-Allow-Origin (and credentials
//     / expose-headers when configured) and is passed to the wrapped handler.
//   - When the origin is not allowed, no Access-Control-Allow-Origin header is
//     emitted; the request still flows through so the API's auth layer can
//     answer it uniformly.
//
// Preflight responses carry Vary: Origin, Access-Control-Request-Method and
// Access-Control-Request-Headers so caches key correctly.
func CORS(opts CORSOptions) func(http.Handler) http.Handler {
	cfg := opts.withDefaults()
	allowAll := false
	for _, o := range cfg.AllowedOrigins {
		if o == "*" {
			allowAll = true
		}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			isPreflight := r.Method == http.MethodOptions &&
				r.Header.Get("Access-Control-Request-Method") != ""

			applyHeaders := func() {
				if origin == "" {
					return
				}
				matched, ok := originAllowed(origin, cfg.AllowedOrigins)
				if !ok {
					// Origin not permitted: emit no ACAO header.
					return
				}
				switch {
				case allowAll && !cfg.AllowCredentials:
					w.Header().Set("Access-Control-Allow-Origin", "*")
				case allowAll && cfg.AllowCredentials:
					// Cannot use "*" with credentials; echo the origin.
					w.Header().Set("Access-Control-Allow-Origin", origin)
					w.Header().Add("Vary", "Origin")
				default:
					w.Header().Set("Access-Control-Allow-Origin", matched)
					w.Header().Add("Vary", "Origin")
				}
				if cfg.AllowCredentials {
					w.Header().Set("Access-Control-Allow-Credentials", "true")
				}
				if len(cfg.ExposedHeaders) > 0 {
					w.Header().Set("Access-Control-Expose-Headers", allowHeaderValue(cfg.ExposedHeaders))
				}
			}

			if isPreflight {
				applyHeaders()
				w.Header().Set("Access-Control-Allow-Methods", allowHeaderValue(cfg.AllowedMethods))
				requestedHeaders := r.Header.Get("Access-Control-Request-Headers")
				if len(cfg.AllowedHeaders) == 1 && cfg.AllowedHeaders[0] == "*" {
					// Permit whatever was requested.
					if requestedHeaders != "" {
						w.Header().Set("Access-Control-Allow-Headers", requestedHeaders)
					}
				} else {
					w.Header().Set("Access-Control-Allow-Headers", allowHeaderValue(cfg.AllowedHeaders))
				}
				w.Header().Set("Access-Control-Max-Age", strconv.Itoa(cfg.MaxAgeSeconds))
				w.Header().Add("Vary", "Origin")
				w.Header().Add("Vary", "Access-Control-Request-Method")
				w.Header().Add("Vary", "Access-Control-Request-Headers")
				w.WriteHeader(http.StatusNoContent)
				return
			}

			applyHeaders()
			next.ServeHTTP(w, r)
		})
	}
}
