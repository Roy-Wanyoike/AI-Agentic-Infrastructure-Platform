package main

// Issue #55 (production hardening): the CORS knob.
//
// Pre-#55 the API answered every request with Access-Control-Allow-Origin: *
// regardless of deployment — appropriate for zero-infrastructure development,
// wrong for anything reachable from the internet. AGENTOS_CORS_ORIGINS turns
// the wildcard off:
//
//      unset/empty  -> wildcard dev mode, byte-identical to the pre-#55 behavior
//                      (documented in corsMiddleware: local development must keep
//                      working with zero configuration)
//      comma-separated
//      allowlist    -> Access-Control-Allow-Origin echoes ONLY origins on the
//                      list; Vary: Origin is always set (responses now differ per
//                      Origin, so shared caches must key on it); and because the
//                      echo is a specific origin — never "*" — granting
//                      Access-Control-Allow-Credentials is safe.
//
// Matching is exact string equality against the browser's Origin header
// (scheme + host[:port], no trailing slash — entries like
// "https://app.example.com"). No syntax validation is attempted and no origin
// is ever transformed: whatever the operator lists is what gets echoed, so a
// typo fails closed (the browser blocks the request) rather than opening the
// wrong origin. An empty allowlist entry (",," or whitespace) is dropped, so
// a sloppy value can only shrink the allowlist.
//
// The parsing/matching logic lives in these pure functions so the matrix is
// unit-testable without HTTP plumbing (see cors_test.go).

import (
	"net/http"
	"os"
	"strings"
)

// CORSOriginsEnvVar names the comma-separated CORS allowlist knob.
const CORSOriginsEnvVar = "AGENTOS_CORS_ORIGINS"

// corsAllowMethods and corsAllowHeaders are the preflight grants shared by
// both modes (identical to the pre-#55 wildcard values: the API's own clients
// send Authorization / X-API-Key / Idempotency-Key and JSON bodies).
const (
	corsAllowMethods = "GET,POST,PUT,DELETE,OPTIONS"
	corsAllowHeaders = "Content-Type,Authorization,X-API-Key,api_key,Idempotency-Key"
)

// ParseCORSOrigins splits a raw AGENTOS_CORS_ORIGINS value into the allowlist.
// A blank value (or one containing only commas/whitespace) returns nil, which
// callers treat as "no allowlist configured" -> wildcard dev mode. Entries are
// trimmed; empty entries are dropped.
func ParseCORSOrigins(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	allowed := make([]string, 0, len(parts))
	for _, part := range parts {
		if entry := strings.TrimSpace(part); entry != "" {
			allowed = append(allowed, entry)
		}
	}
	if len(allowed) == 0 {
		return nil
	}
	return allowed
}

// CORSOriginsFromEnv parses CORSOriginsEnvVar from the environment.
func CORSOriginsFromEnv() []string {
	return ParseCORSOrigins(os.Getenv(CORSOriginsEnvVar))
}

// CORSOriginAllowed reports whether origin is on the allowlist. Exact string
// match: origins are compared verbatim (see the file-level matching note).
func CORSOriginAllowed(allowed []string, origin string) bool {
	if origin == "" {
		return false
	}
	for _, entry := range allowed {
		if entry == origin {
			return true
		}
	}
	return false
}

// ApplyCORSHeaders writes the allowlist-mode CORS headers onto h for one
// request. Vary: Origin is set unconditionally (cache correctness whether or
// not this particular origin is allowed); the access-control grants appear
// only for allowlisted origins, so a non-matching origin gets NO
// Access-Control-Allow-Origin header and the browser refuses the response.
func ApplyCORSHeaders(h http.Header, allowed []string, origin string) {
	// Responses are origin-dependent once the wildcard is off; every cached
	// response must be keyed by the request's Origin (RFC 7239 semantics for
	// CORS as implemented by browsers/CDNs).
	h.Add("Vary", "Origin")
	if !CORSOriginAllowed(allowed, origin) {
		return
	}
	h.Set("Access-Control-Allow-Origin", origin)
	// Credentials-safe by construction: the echoed origin is a specific
	// allowlist entry, never "*", so this grant cannot combine with a
	// wildcard value.
	h.Set("Access-Control-Allow-Credentials", "true")
	h.Set("Access-Control-Allow-Methods", corsAllowMethods)
	h.Set("Access-Control-Allow-Headers", corsAllowHeaders)
}
