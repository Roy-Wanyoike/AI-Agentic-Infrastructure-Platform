package httpx

import (
	"net/http"
	"time"
)

// Timeout wraps the handler chain with http.TimeoutHandler. When a request
// exceeds d the client receives 503 Service Unavailable with a plain-text
// body (http.TimeoutHandler's contract, outside the JSON error model).
//
// d <= 0 disables the middleware entirely (the wrapped handler is returned
// unchanged), which is also the recommended configuration for SSE routes:
// Server-Sent Events are long-lived by design, so a global short timeout
// would kill event streams. Prefer a generous duration (e.g. 30s) for JSON
// traffic and bypass/raise it for /runs/{id}/events.
func Timeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if d <= 0 {
			return next
		}
		return http.TimeoutHandler(next, d, "request timed out\n")
	}
}
