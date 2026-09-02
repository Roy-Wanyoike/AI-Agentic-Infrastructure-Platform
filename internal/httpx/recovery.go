package httpx

import (
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
)

// Recovery returns middleware that converts panics from wrapped handlers into
// a structured 500 JSON error (internal_error, with the request correlation
// ID) and logs the panic with the provided structured logger — typically the
// *slog.Logger produced by internal/logger.New. A nil logger falls back to
// slog.Default().
//
// http.ErrAbortHandler is deliberately re-panicked: it is net/http's own
// convention for aborting a response and must not be reported as a bug.
//
// If the panic happens after the handler already started writing the response
// (e.g. mid-SSE-stream), the middleware logs and stops without attempting a
// second WriteHeader.
func Recovery(logger *slog.Logger) func(http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec := &trackedResponseWriter{ResponseWriter: w}
			defer func() {
				if p := recover(); p != nil {
					if p == http.ErrAbortHandler {
						panic(p)
					}
					logger.ErrorContext(r.Context(), "panic recovered in http handler",
						"panic", fmt.Sprint(p),
						"stack", string(debug.Stack()),
						"method", r.Method,
						"path", r.URL.Path,
						"remote_addr", r.RemoteAddr,
						"request_id", RequestIDFromContext(r.Context()),
					)
					if !rec.wroteHeader {
						WriteError(w, r, http.StatusInternalServerError,
							ErrorCodeInternal, "internal server error")
					}
				}
			}()
			next.ServeHTTP(rec, r)
		})
	}
}

// trackedResponseWriter records whether the wrapped handler has committed a
// response (explicit WriteHeader or an implicit one via Write) so Recovery
// knows whether an error response is still possible. Flush is forwarded so
// SSE handlers keep working behind Recovery.
type trackedResponseWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (t *trackedResponseWriter) WriteHeader(status int) {
	t.wroteHeader = true
	t.ResponseWriter.WriteHeader(status)
}

func (t *trackedResponseWriter) Write(b []byte) (int, error) {
	t.wroteHeader = true
	return t.ResponseWriter.Write(b)
}

func (t *trackedResponseWriter) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
