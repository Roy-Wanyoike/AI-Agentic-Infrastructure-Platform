package observability

import (
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// ---------------------------------------------------------------------------
// OTel HTTP server middleware (issue #80).
//
// TracingMiddleware opens one SERVER span per request carrying
// http.route / http.request.method / http.response.status_code / url.path /
// user_agent.original, marks 5xx responses as span errors, and renames the
// span to "<method> <route>" once the route is known. It mirrors
// MetricsMiddleware's route resolution (context route label, then
// http.Request.Pattern populated by ServeMux, then "unmatched") so traces and
// metrics aggregate under the same route identity.
//
// Zero-overhead-off: while tracing is disabled the constructor returns the
// next handler UNCHANGED, so the serving chain is byte-identical to the
// pre-tracing build. Callers (cmd/api/middleware_tracing.go) additionally
// fast-path on the env flag, making the disabled decision before any span
// machinery is touched.
// ---------------------------------------------------------------------------

// TracingMiddleware wraps next with an OTel server span. It must be
// constructed AFTER SetupTracing (or InstallTestTracing) so the tracer comes
// from the installed provider; constructed while disabled it returns next
// as-is — the documented fast path (`if !enabled { next }`).
func TracingMiddleware(next http.Handler) http.Handler {
	if next == nil {
		return nil
	}
	if !TracingEnabled() {
		return next
	}
	tracer := otel.GetTracerProvider().Tracer(TracerName)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, span := tracer.Start(r.Context(), r.Method+" "+AttrUnmatchedName,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String(AttrHTTPMethod, r.Method),
				attribute.String(AttrURLPath, r.URL.Path),
				attribute.String(AttrUserAgent, r.UserAgent()),
			))
		defer span.End()

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		inner := r.WithContext(ctx)
		next.ServeHTTP(rec, inner)

		// Route resolution mirrors MetricsMiddleware: context label first,
		// then the pattern ServeMux stamped on the dispatched request (inner
		// is the shallow copy this middleware handed down, which the mux
		// mutates in place), then the "unmatched" fallback.
		route := RouteNameFromContext(r.Context())
		if route == "" {
			route = inner.Pattern
		}
		if route == "" {
			route = AttrUnmatchedName
		}
		span.SetName(r.Method + " " + route)
		span.SetAttributes(
			attribute.String(AttrRoute, route),
			attribute.Int(AttrHTTPStatus, rec.status),
		)
		if rec.status >= http.StatusInternalServerError {
			span.SetStatus(codes.Error, http.StatusText(rec.status))
		}
	})
}
