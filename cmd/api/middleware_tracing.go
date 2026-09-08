package main

import (
	"net/http"

	"agentos/internal/observability"
)

// ---------------------------------------------------------------------------
// OTel tracing middleware glue (issue #80).
//
// tracingMiddleware is the API-side seam for distributed tracing: it reads the
// AGENTOS_TRACING_ENABLED flag once at routes() construction time (the same
// pattern corsMiddleware uses for AGENTOS_CORS_ORIGINS) and, while tracing is
// disabled — the default — returns the next handler UNCHANGED, so the serving
// chain is byte-identical to the pre-tracing build and the per-request cost is
// zero. When enabled it delegates to observability.TracingMiddleware, which
// additionally guards on the installed provider, so an env flip without a
// successful SetupTracing still stays a safe pass-through.
//
// Wiring (cmd/api/main.go routes(), tracing outermost so rate-limit
// rejections and metrics are covered by the server span):
//
//	return tracingMiddleware(observability.MetricsMiddleware(a.metricsSvc, rateLimit(corsMiddleware(mux))))
//
// with SetupTracing called once in main() before the server is built
// (see the worklog wiring notes for the exact lines).
// ---------------------------------------------------------------------------

func tracingMiddleware(next http.Handler) http.Handler {
	if !observability.TracingConfigFromEnv().Enabled {
		return next // fast path: one env read at construction, zero per-request cost
	}
	return observability.TracingMiddleware(next)
}
