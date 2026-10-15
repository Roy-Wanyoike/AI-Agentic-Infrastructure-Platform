package observability

import (
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// ---------------------------------------------------------------------------
// Instrumented provider HTTP transport (issue #114).
//
// The OpenAI-compatible provider (internal/models) issues its HTTP through an
// injectable *http.Client. NewTracingRoundTripper wraps that transport so every
// provider round trip becomes a CLIENT span CHILD of the model-call span that
// is active on the request context — the leaf of the run → agent → step →
// model → HTTP hierarchy. The W3C traceparent is injected into the outgoing
// request headers, so an OTel-instrumented provider endpoint can continue the
// same trace.
//
// Disabled contract: TracingEnabled() is checked FIRST and the call delegates
// to the wrapped transport untouched — no span, no header, no request clone;
// the fast path is the wrapped transport itself.
// ---------------------------------------------------------------------------

// TracingRoundTripper wraps a base http.RoundTripper with OTel client-span
// instrumentation and W3C trace header propagation. Build it with
// NewTracingRoundTripper; the zero value is not usable.
type TracingRoundTripper struct {
	base http.RoundTripper
}

// NewTracingRoundTripper wraps base (http.DefaultTransport when nil) with the
// execution-tracing HTTP instrumentation. The returned transport is always
// non-nil and safe for concurrent use by multiple http.Clients.
func NewTracingRoundTripper(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &TracingRoundTripper{base: base}
}

// Base returns the wrapped transport (diagnostics/testing seam).
func (t *TracingRoundTripper) Base() http.RoundTripper {
	if t == nil || t.base == nil {
		return http.DefaultTransport
	}
	return t.base
}

// RoundTrip implements http.RoundTripper. When tracing is disabled it
// delegates without touching the request (byte-identical fast path). When
// enabled it opens a client span ("HTTP <method>") as a child of the span in
// the request context, injects the W3C trace context into the outgoing
// headers, and records the response status (5xx → span error per OTel HTTP
// conventions) or the transport error.
func (t *TracingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if t == nil || t.base == nil {
		return http.DefaultTransport.RoundTrip(req)
	}
	if !TracingEnabled() {
		return t.base.RoundTrip(req)
	}

	attrs := []attribute.KeyValue{
		attribute.String(AttrHTTPMethod, req.Method),
	}
	if req.URL != nil {
		if host := req.URL.Host; host != "" {
			attrs = append(attrs, attribute.String("server.address", host))
		}
		if req.URL.Scheme != "" {
			attrs = append(attrs, attribute.String("url.scheme", req.URL.Scheme))
		}
		attrs = append(attrs, attribute.String(AttrURLPath, req.URL.Path))
	}
	spanCtx, span := startSpanKind(req.Context(), SpanHTTPClient+" "+req.Method, trace.SpanKindClient, attrs...)

	// RoundTrip must not mutate the caller's request: clone for header
	// injection. Clone shares the body reference (unread); the provider
	// builds a fresh bytes.Reader per attempt, so no rewind is needed.
	outbound := req.Clone(spanCtx)
	otel.GetTextMapPropagator().Inject(spanCtx, propagation.HeaderCarrier(outbound.Header))

	resp, err := t.base.RoundTrip(outbound)
	if err != nil {
		RecordSpanError(span, err)
		span.End()
		return nil, err
	}
	span.SetAttributes(attribute.Int(AttrHTTPStatus, resp.StatusCode))
	if resp.StatusCode >= http.StatusInternalServerError {
		span.SetStatus(codes.Error, http.StatusText(resp.StatusCode))
	}
	span.End()
	return resp, nil
}
