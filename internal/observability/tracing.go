package observability

import (
	"context"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// ---------------------------------------------------------------------------
// Distributed tracing (issue #80, additive extension).
//
// The package stays metrics-first: tracing is strictly opt-in behind
// AGENTOS_TRACING_ENABLED and, while disabled, costs one atomic load per
// helper call and installs nothing — the global OpenTelemetry default is a
// no-op provider, so no span is ever recorded and no exporter is created.
//
// SetupTracing is the single entry point binaries call once at startup:
//
//      shutdown, err := observability.SetupTracing(ctx, observability.TracingConfigFromEnv())
//      if err != nil { log.Warn("tracing disabled", "error", err) }
//      defer func() { _ = shutdown(context.Background()) }()
//
// When enabled it installs an SDK tracer provider exporting spans over OTLP/HTTP
// to OTEL_EXPORTER_OTLP_ENDPOINT (the OTel default localhost:4318 applies when
// the variable is unset) and the W3C TraceContext + Baggage propagators.
// ---------------------------------------------------------------------------

// Environment variables read by TracingConfigFromEnv. AGENTOS_TRACING_ENABLED
// follows the project's ParseBool convention ("1"/"t"/"true"/... enable,
// unset/empty/garbage stays off — mirroring internal/billing/enforcement.go).
// OTEL_EXPORTER_OTLP_ENDPOINT and OTEL_SERVICE_NAME are the standard OpenTelemetry
// variable names.
const (
	TracingEnabledEnvVar  = "AGENTOS_TRACING_ENABLED"
	OTELEndpointEnvVar    = "OTEL_EXPORTER_OTLP_ENDPOINT"
	OTelServiceNameEnvVar = "OTEL_SERVICE_NAME"
	// DefaultServiceName is applied when OTEL_SERVICE_NAME is unset.
	DefaultServiceName = "agentos"
)

// TracerName is the instrumentation scope every AgentOS span is created with,
// so backends can filter spans by library.
const TracerName = "agentos"

// Attribute keys used on AgentOS spans. HTTP keys follow OpenTelemetry
// semantic conventions (v1.26+ HTTP server spans); the agentos.* namespace
// carries domain attributes (tenant, run, tool, task), matching the
// agentos_* Prometheus metric prefix convention.
const (
	AttrRoute         = "http.route"
	AttrHTTPMethod    = "http.request.method"
	AttrHTTPStatus    = "http.response.status_code"
	AttrURLPath       = "url.path"
	AttrUserAgent     = "user_agent.original"
	AttrOrgID         = "agentos.org.id"
	AttrAuthSource    = "agentos.auth.source"
	AttrAgentID       = "agentos.agent.id"
	AttrRunID         = "agentos.run.id"
	AttrToolName      = "agentos.tool.name"
	AttrTaskType      = "agentos.task.type"
	AttrTaskID        = "agentos.task.id"
	AttrUnmatchedName = "unmatched" // route fallback, same label as MetricsMiddleware
)

// TracingConfig is the resolved tracing configuration.
type TracingConfig struct {
	// Enabled is AGENTOS_TRACING_ENABLED (default off, garbage-off).
	Enabled bool
	// Endpoint is OTEL_EXPORTER_OTLP_ENDPOINT. Empty keeps the exporter's
	// OTel-spec default (http://localhost:4318). A full URL (http:// or
	// https:// scheme) selects plaintext vs TLS transport.
	Endpoint string
	// ServiceName is OTEL_SERVICE_NAME (DefaultServiceName when unset),
	// recorded on the resource as service.name.
	ServiceName string
}

// TracingConfigFromEnv resolves the tracing configuration from the process
// environment. Misconfiguration never silently enables tracing: garbage
// boolean values and empty service names fall back to the safe defaults.
func TracingConfigFromEnv() TracingConfig {
	cfg := TracingConfig{Endpoint: strings.TrimSpace(os.Getenv(OTELEndpointEnvVar))}
	if raw, ok := os.LookupEnv(TracingEnabledEnvVar); ok && strings.TrimSpace(raw) != "" {
		on, err := strconv.ParseBool(strings.TrimSpace(raw))
		if err == nil {
			cfg.Enabled = on
		}
	}
	if name := strings.TrimSpace(os.Getenv(OTelServiceNameEnvVar)); name != "" {
		cfg.ServiceName = name
	} else {
		cfg.ServiceName = DefaultServiceName
	}
	return cfg
}

// tracingRuntime carries the installed tracer; a nil atomic pointer means
// tracing is disabled and every helper short-circuits to the no-op span.
type tracingRuntime struct {
	tracer trace.Tracer
}

var tracingState atomic.Pointer[tracingRuntime]

// TracingEnabled reports whether SetupTracing (or InstallTestTracing) has
// installed a live tracer provider in this process.
func TracingEnabled() bool {
	return tracingState.Load() != nil
}

// StartSpan opens a span named name on the current tracer, attaching attrs.
// When tracing is disabled it returns the context unchanged plus the no-op
// span (one atomic load, no allocation), so call sites never need a flag.
func StartSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	rt := tracingState.Load()
	if rt == nil {
		return ctx, trace.SpanFromContext(ctx) // no-op span: recording() == false
	}
	if len(attrs) == 0 {
		return rt.tracer.Start(ctx, name)
	}
	return rt.tracer.Start(ctx, name, trace.WithAttributes(attrs...))
}

// StartAuthSpan opens the auth-validation child span (issue #80: "auth
// validation child span"). source names the credential channel, e.g.
// "bearer", "api_key", "api_key_query".
func StartAuthSpan(ctx context.Context, source string) (context.Context, trace.Span) {
	return StartSpan(ctx, "auth.validate", attribute.String(AttrAuthSource, source))
}

// StartRunSpan opens a runs-service span. op is the operation suffix
// ("create", "get", "list", ...); empty identifiers are omitted from the
// span attributes.
func StartRunSpan(ctx context.Context, op, orgID, agentID, runID string) (context.Context, trace.Span) {
	attrs := make([]attribute.KeyValue, 0, 3)
	if orgID != "" {
		attrs = append(attrs, attribute.String(AttrOrgID, orgID))
	}
	if agentID != "" {
		attrs = append(attrs, attribute.String(AttrAgentID, agentID))
	}
	if runID != "" {
		attrs = append(attrs, attribute.String(AttrRunID, runID))
	}
	return StartSpan(ctx, "runs."+op, attrs...)
}

// StartToolSpan opens a runtime tool-invocation span.
func StartToolSpan(ctx context.Context, tool string) (context.Context, trace.Span) {
	return StartSpan(ctx, "runtime.tool", attribute.String(AttrToolName, tool))
}

// StartQueueSpan opens a queue span; op is "enqueue" or "dequeue".
func StartQueueSpan(ctx context.Context, op string) (context.Context, trace.Span) {
	return StartSpan(ctx, "queue."+op)
}

// StartWorkerSpan opens a worker-execution span for the given task type.
func StartWorkerSpan(ctx context.Context, taskType string) (context.Context, trace.Span) {
	return StartSpan(ctx, "worker.execute", attribute.String(AttrTaskType, taskType))
}

// RecordSpanError marks span as failed: the error becomes an exception event
// and the span status moves to Error (no-op when err is nil).
func RecordSpanError(span trace.Span, err error) {
	if err == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

// SetSpanOrgID stamps the tenant identifier onto whatever span is active in
// ctx (typically the HTTP server span reached from inside the auth
// middleware). It is a no-op when tracing is disabled or orgID is empty, so
// the auth seam (see docs/load-testing.md and the worklog wiring notes) can
// call it unconditionally.
func SetSpanOrgID(ctx context.Context, orgID string) {
	if orgID == "" {
		return
	}
	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		span.SetAttributes(attribute.String(AttrOrgID, orgID))
	}
}

// SetupTracing installs the global tracer provider when cfg.Enabled. It
// returns a shutdown function that flushes pending spans and restores the
// disabled (no-op) state; the returned function is always non-nil and safe to
// call multiple times. When disabled it returns the no-op shutdown immediately
// and installs NOTHING — the zero-overhead contract for the default mode.
func SetupTracing(ctx context.Context, cfg TracingConfig) (func(context.Context) error, error) {
	if !cfg.Enabled {
		return func(context.Context) error { return nil }, nil
	}

	var opts []otlptracehttp.Option
	if cfg.Endpoint != "" {
		opts = append(opts, otlptracehttp.WithEndpointURL(cfg.Endpoint))
	}
	exporter, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return func(context.Context) error { return nil }, err
	}

	res, err := sdkresource.Merge(sdkresource.Default(), sdkresource.NewSchemaless(
		attribute.String("service.name", cfg.ServiceName),
		attribute.String("service.version", serviceVersion()),
	))
	if err != nil {
		_ = exporter.Shutdown(ctx)
		return func(context.Context) error { return nil }, err
	}

	// BatchSpanProcessor with SDK defaults (5s batch timeout, 2048 queue,
	// 512 batch size, 30s export timeout) — sane for a request-path platform.
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	tracingState.Store(&tracingRuntime{tracer: provider.Tracer(TracerName)})

	var once sync.Once
	return func(ctx context.Context) error {
		var shutdownErr error
		once.Do(func() {
			tracingState.Store(nil)
			otel.SetTracerProvider(trace.NewNoopTracerProvider())
			shutdownErr = provider.Shutdown(ctx)
		})
		return shutdownErr
	}, nil
}

// serviceVersion resolves the resource service.version from build info: the
// module version when it is a real tag, otherwise the short VCS revision,
// otherwise "dev".
func serviceVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" && len(setting.Value) >= 12 {
			return setting.Value[:12]
		}
	}
	return "dev"
}
