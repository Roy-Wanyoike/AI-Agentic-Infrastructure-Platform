package observability

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// ---------------------------------------------------------------------------
// In-memory tracer provider for tests (issue #80).
//
// Production binaries must use SetupTracing; InstallTestTracing exists so
// binary-level tests (cmd/api/middleware_tracing_test.go) can assert the exact
// spans the middleware and span helpers emit without running an OTLP
// collector. It is synchronous: every ended span is snapshot-immediately into
// the sink, so assertions need no flushing or sleeps.
// ---------------------------------------------------------------------------

// RecordedSpan is an immutable snapshot of one ended span, exposing exactly
// what tests assert on: identity, parentage, kind, status and attributes.
type RecordedSpan struct {
	Name         string
	TraceID      trace.TraceID
	SpanID       trace.SpanID
	ParentSpanID trace.SpanID
	SpanKind     trace.SpanKind
	StatusCode   codes.Code
	Attributes   map[string]string
}

// Attr returns the attribute value for key ("", when absent).
func (s RecordedSpan) Attr(key string) string { return s.Attributes[key] }

// testSpanRecorder implements sdktrace.SpanProcessor by snapshotting ended
// spans into an in-memory sink.
type testSpanRecorder struct {
	mu    sync.Mutex
	spans []RecordedSpan
}

func (r *testSpanRecorder) OnStart(context.Context, sdktrace.ReadWriteSpan) {}

func (r *testSpanRecorder) OnEnd(s sdktrace.ReadOnlySpan) {
	if s == nil {
		return
	}
	attrs := make(map[string]string, len(s.Attributes()))
	for _, kv := range s.Attributes() {
		attrs[string(kv.Key)] = kv.Value.Emit()
	}
	snap := RecordedSpan{
		Name:         s.Name(),
		TraceID:      s.SpanContext().TraceID(),
		SpanID:       s.SpanContext().SpanID(),
		ParentSpanID: s.Parent().SpanID(),
		SpanKind:     s.SpanKind(),
		StatusCode:   s.Status().Code,
		Attributes:   attrs,
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.spans = append(r.spans, snap)
}

func (r *testSpanRecorder) Shutdown(context.Context) error   { return nil }
func (r *testSpanRecorder) ForceFlush(context.Context) error { return nil }

// InstallTestTracing swaps the global no-op tracer provider for a synchronous
// in-memory one and marks tracing enabled. The returned restore function puts
// the process back into the disabled state (previous provider and
// propagator); tests must defer it. Never call this from production code.
func InstallTestTracing() (recorded func() []RecordedSpan, restore func()) {
	rec := &testSpanRecorder{}
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))

	prevState := tracingState.Load()
	prevProvider := otel.GetTracerProvider()
	prevPropagator := otel.GetTextMapPropagator()

	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(defaultPropagator())
	tracingState.Store(&tracingRuntime{tracer: provider.Tracer(TracerName)})

	return func() []RecordedSpan {
			rec.mu.Lock()
			defer rec.mu.Unlock()
			return append([]RecordedSpan(nil), rec.spans...)
		}, func() {
			tracingState.Store(prevState)
			otel.SetTracerProvider(prevProvider)
			otel.SetTextMapPropagator(prevPropagator)
			_ = provider.Shutdown(context.Background())
		}
}

// defaultPropagator mirrors the composite installed by SetupTracing so tests
// exercise the same propagation semantics as production.
func defaultPropagator() propagation.TextMapPropagator {
	return propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	)
}
