// Package httpx provides production-grade HTTP middleware for the AgentOS
// API: request correlation (X-Request-ID), a structured JSON error model,
// CORS, panic recovery, request timeouts, and a middleware chain helper.
//
// All middleware is plain net/http (no external dependencies) and composable:
//
//	handler := httpx.Chain(mux,
//	    httpx.RequestID,                        // outermost
//	    httpx.Recovery(logr),
//	    httpx.Timeout(30*time.Second),
//	    httpx.CORS(httpx.DefaultCORSOptions()), // innermost middleware
//	)
//
// The canonical wiring for cmd/api/main.go is documented in
// docs/api-contract.md and in the Task 1-c worklog entry.
package httpx

import "net/http"

// Chain applies middlewares in order so that middlewares[0] is the outermost
// wrapper around base. A nil base is treated as a 404 handler.
func Chain(base http.Handler, middlewares ...func(http.Handler) http.Handler) http.Handler {
	if base == nil {
		base = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		})
	}
	for i := len(middlewares) - 1; i >= 0; i-- {
		mw := middlewares[i]
		if mw == nil {
			continue
		}
		base = mw(base)
	}
	return base
}
