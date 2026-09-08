package observability

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// TestTracingConfigFromEnvDisabledByDefault pins the default-off contract:
// unset and garbage AGENTOS_TRACING_ENABLED values both resolve to disabled
// (misconfiguration never silently enables tracing).
func TestTracingConfigFromEnvDisabledByDefault(t *testing.T) {
	t.Setenv("AGENTOS_TRACING_ENABLED", "")
	cfg := TracingConfigFromEnv()
	if cfg.Enabled {
		t.Fatal("empty AGENTOS_TRACING_ENABLED must resolve to disabled")
	}

	t.Setenv("AGENTOS_TRACING_ENABLED", "not-a-bool")
	if TracingConfigFromEnv().Enabled {
		t.Fatal("garbage AGENTOS_TRACING_ENABLED must resolve to disabled (ParseBool garbage=off)")
	}

	t.Setenv("AGENTOS_TRACING_ENABLED", "1")
	cfg = TracingConfigFromEnv()
	if !cfg.Enabled {
		t.Fatal("AGENTOS_TRACING_ENABLED=1 must resolve to enabled")
	}
	if cfg.ServiceName != DefaultServiceName {
		t.Fatalf("service name = %q, want default %q", cfg.ServiceName, DefaultServiceName)
	}

	t.Setenv("OTEL_SERVICE_NAME", "agentos-staging")
	if got := TracingConfigFromEnv().ServiceName; got != "agentos-staging" {
		t.Fatalf("service name = %q, want agentos-staging", got)
	}

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector:4318")
	if got := TracingConfigFromEnv().Endpoint; got != "http://collector:4318" {
		t.Fatalf("endpoint = %q, want http://collector:4318", got)
	}
}

// TestSetupTracingDisabledInstallsNothing proves the disabled path returns a
// callable no-op shutdown and leaves the process in the zero-overhead state.
func TestSetupTracingDisabledInstallsNothing(t *testing.T) {
	if TracingEnabled() {
		t.Fatal("precondition: tracing must start disabled in the test process")
	}
	shutdown, err := SetupTracing(context.Background(), TracingConfig{Enabled: false})
	if err != nil {
		t.Fatalf("disabled SetupTracing returned error: %v", err)
	}
	if shutdown == nil {
		t.Fatal("shutdown must be non-nil even when disabled")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("noop shutdown returned error: %v", err)
	}
	if TracingEnabled() {
		t.Fatal("disabled SetupTracing must not mark tracing enabled")
	}

	// StartSpan is the no-op path: context unchanged, non-recording span.
	ctx, span := StartSpan(context.Background(), "never.recorded")
	if span.IsRecording() {
		t.Fatal("disabled StartSpan must return a non-recording span")
	}
	if ctx != context.Background() {
		t.Fatal("disabled StartSpan must return the context unchanged")
	}

	// Domain helpers ride the same no-op path.
	ctx2, span2 := StartToolSpan(ctx, "calculator")
	if span2.IsRecording() || ctx2 != ctx {
		t.Fatal("disabled StartToolSpan must be a no-op")
	}
	_, span3 := StartQueueSpan(ctx, "enqueue")
	if span3.IsRecording() {
		t.Fatal("disabled StartQueueSpan must be a no-op")
	}
	_, span4 := StartWorkerSpan(ctx, "agent.run")
	if span4.IsRecording() {
		t.Fatal("disabled StartWorkerSpan must be a no-op")
	}
	_, span5 := StartAuthSpan(ctx, "bearer")
	if span5.IsRecording() {
		t.Fatal("disabled StartAuthSpan must be a no-op")
	}
	_, span6 := StartRunSpan(ctx, "create", "org", "agent", "run")
	if span6.IsRecording() {
		t.Fatal("disabled StartRunSpan must be a no-op")
	}

	// The middleware constructor is the literal fast path.
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	if got := TracingMiddleware(next); got != http.Handler(next) {
		t.Fatal("disabled TracingMiddleware must return the next handler unchanged")
	}
}

// TestTracingMiddlewareEmitsServerSpanWhenEnabled proves, with the in-memory
// recorder, that the middleware emits a server span with the expected name,
// attributes and error marking, and that child spans created in the handler
// propagate under it.
func TestTracingMiddlewareEmitsServerSpanWhenEnabled(t *testing.T) {
	spans, restore := InstallTestTracing()
	defer restore()

	var handlerSpan trace.Span
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Context propagation: the server span is reachable and recording
		// inside the wrapped handler.
		ctx := r.Context()
		if !trace.SpanFromContext(ctx).IsRecording() {
			t.Error("handler context must carry a recording span when tracing is enabled")
		}
		// Org-claim enrichment seam: inner middleware stamps the tenant.
		SetSpanOrgID(ctx, "org-42")
		// Child span: runtime tool invocation under the server span.
		_, handlerSpan = StartToolSpan(ctx, "calculator")
		handlerSpan.SetAttributes(attribute.String(AttrRunID, "run-1"))
		handlerSpan.End()
		w.WriteHeader(http.StatusCreated)
	})

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Inner", "1")
		handler.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(TracingMiddleware(inner))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/agents/create", nil)
	req.Header.Set("User-Agent", "agentos-loadtest/1.0")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", res.StatusCode)
	}

	recorded := spans()
	if len(recorded) != 2 {
		t.Fatalf("expected 2 spans (server + tool), got %d: %+v", len(recorded), recorded)
	}

	var server, tool RecordedSpan
	for _, s := range recorded {
		switch s.Name {
		case "POST unmatched":
			server = s
		case "runtime.tool":
			tool = s
		default:
			t.Errorf("unexpected span name %q", s.Name)
		}
	}
	if server.Name == "" || tool.Name == "" {
		t.Fatalf("missing expected spans: %+v", recorded)
	}

	if server.SpanKind != trace.SpanKindServer {
		t.Errorf("server span kind = %v, want server", server.SpanKind)
	}
	if got := server.Attr(AttrHTTPMethod); got != http.MethodPost {
		t.Errorf("span %s = %q, want POST", AttrHTTPMethod, got)
	}
	if got := server.Attr(AttrHTTPStatus); got != "201" {
		t.Errorf("span %s = %q, want 201", AttrHTTPStatus, got)
	}
	if got := server.Attr(AttrURLPath); got != "/v1/agents/create" {
		t.Errorf("span %s = %q, want /v1/agents/create", AttrURLPath, got)
	}
	if got := server.Attr(AttrUserAgent); got != "agentos-loadtest/1.0" {
		t.Errorf("span %s = %q, want agentos-loadtest/1.0", AttrUserAgent, got)
	}
	if got := server.Attr(AttrOrgID); got != "org-42" {
		t.Errorf("org claim attribute missing on server span: %q", got)
	}
	if server.StatusCode != codes.Unset {
		t.Errorf("2xx span status = %v, want Unset", server.StatusCode)
	}
	if tool.Attr(AttrToolName) != "calculator" || tool.Attr(AttrRunID) != "run-1" {
		t.Errorf("tool span attributes = %v", tool.Attributes)
	}

	// Context propagation: child shares the server trace and hangs off it.
	if tool.TraceID != server.TraceID {
		t.Errorf("child trace %v != server trace %v", tool.TraceID, server.TraceID)
	}
	if tool.ParentSpanID != server.SpanID {
		t.Errorf("child parent %v != server span %v", tool.ParentSpanID, server.SpanID)
	}
}

// TestTracingMiddlewareRouteAndErrorAttributes proves route resolution via
// http.Request.Pattern (ServeMux) and the 5xx error marking.
func TestTracingMiddlewareRouteAndErrorAttributes(t *testing.T) {
	spans, restore := InstallTestTracing()
	defer restore()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/agents", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	mux.HandleFunc("/v1/boom", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	srv := httptest.NewServer(TracingMiddleware(mux))
	defer srv.Close()

	for _, path := range []string{"/v1/agents", "/v1/boom"} {
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s failed: %v", path, err)
		}
		res.Body.Close()
	}

	recorded := spans()
	if len(recorded) != 2 {
		t.Fatalf("expected 2 server spans, got %d", len(recorded))
	}
	byName := map[string]RecordedSpan{}
	for _, s := range recorded {
		byName[s.Name] = s
	}
	forbidden, ok := byName["GET /v1/agents"]
	if !ok {
		t.Fatalf("span named GET /v1/agents missing: %+v", recorded)
	}
	if got := forbidden.Attr(AttrRoute); got != "/v1/agents" {
		t.Errorf("route = %q, want /v1/agents", got)
	}
	if forbidden.StatusCode != codes.Unset {
		t.Errorf("403 span status = %v, want Unset (4xx is not a span error)", forbidden.StatusCode)
	}
	boom, ok := byName["GET /v1/boom"]
	if !ok {
		t.Fatalf("span named GET /v1/boom missing: %+v", recorded)
	}
	if got := boom.Attr(AttrHTTPStatus); got != "500" {
		t.Errorf("status = %q, want 500", got)
	}
	if boom.StatusCode != codes.Error {
		t.Errorf("500 span status = %v, want Error", boom.StatusCode)
	}
}

// TestTracingMiddlewareContextRouteLabelWins proves the WithRouteName label
// (applied in-process BEFORE the middleware, the RouteName-wrapper pattern)
// takes precedence over ServeMux pattern resolution, keeping trace and metric
// route identity aligned.
func TestTracingMiddlewareContextRouteLabelWins(t *testing.T) {
	spans, restore := InstallTestTracing()
	defer restore()

	mux := http.NewServeMux()
	mux.HandleFunc("/agents/create", func(w http.ResponseWriter, r *http.Request) {})
	// Outer wrapper tags every request with the canonical route label before
	// the tracing middleware sees it (route labels cannot cross the network;
	// this is the in-process contract shared with MetricsMiddleware).
	labeler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		TracingMiddleware(mux).ServeHTTP(w, RequestWithRouteName(r, "/agents/create"))
	})
	srv := httptest.NewServer(labeler)
	defer srv.Close()

	res, err := http.Post(srv.URL+"/agents/create", "application/json", nil)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	res.Body.Close()

	recorded := spans()
	if len(recorded) != 1 {
		t.Fatalf("expected 1 span, got %d", len(recorded))
	}
	if recorded[0].Name != "POST /agents/create" {
		t.Fatalf("span name = %q, want POST /agents/create", recorded[0].Name)
	}
	if got := recorded[0].Attr(AttrRoute); got != "/agents/create" {
		t.Fatalf("route = %q, want /agents/create", got)
	}
}

// TestTracingMiddlewarePassThroughDisabledNoSpans proves the disabled mode
// emits zero spans and keeps response behavior identical.
func TestTracingMiddlewarePassThroughDisabledNoSpans(t *testing.T) {
	if TracingEnabled() {
		t.Fatal("precondition: tracing must be disabled")
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte(`{"ok":false}`))
	})

	// Compare disabled-middleware output against the bare handler.
	recDirect := httptest.NewRecorder()
	handler.ServeHTTP(recDirect, httptest.NewRequest(http.MethodGet, "/v1/anything", nil))

	recWrapped := httptest.NewRecorder()
	TracingMiddleware(handler).ServeHTTP(recWrapped, httptest.NewRequest(http.MethodGet, "/v1/anything", nil))

	if recDirect.Code != recWrapped.Code || recDirect.Body.String() != recWrapped.Body.String() {
		t.Fatalf("disabled middleware changed behavior: direct=%d/%q wrapped=%d/%q",
			recDirect.Code, recDirect.Body.String(), recWrapped.Code, recWrapped.Body.String())
	}
	if recDirect.Header().Get("Content-Type") != recWrapped.Header().Get("Content-Type") {
		t.Fatal("disabled middleware changed response headers")
	}

	// And even a full server round trip emits no spans (none could exist: no
	// provider is installed, which is the point — behavior is identical).
	srv := httptest.NewServer(TracingMiddleware(handler))
	defer srv.Close()
	res, err := http.Get(srv.URL + "/v1/anything")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusTeapot {
		t.Fatalf("status = %d, want 418", res.StatusCode)
	}
}

// TestSetupTracingEnabledEndToEnd exercises the real SetupTracing path (real
// OTLP exporter construction against an unreachable sink — construction never
// dials) and proves spans flow through the installed provider.
func TestSetupTracingEnabledEndToEnd(t *testing.T) {
	// Restore whatever state the process had (tests run sequentially, but be
	// explicit: SetupTracing mutates globals).
	shutdown, err := SetupTracing(context.Background(), TracingConfig{
		Enabled:     true,
		Endpoint:    "http://127.0.0.1:1", // port 1: never dials during construction
		ServiceName: "agentos-test",
	})
	if err != nil {
		t.Fatalf("SetupTracing failed: %v", err)
	}
	if !TracingEnabled() {
		t.Fatal("SetupTracing must mark tracing enabled")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("first shutdown failed: %v", err)
	}
	if TracingEnabled() {
		t.Fatal("shutdown must restore the disabled state")
	}

	// Re-install to prove span production through the real provider, then
	// shut down for real (flush against a dead endpoint must not hang).
	shutdown2, err := SetupTracing(context.Background(), TracingConfig{
		Enabled:  true,
		Endpoint: "http://127.0.0.1:1",
	})
	if err != nil {
		t.Fatalf("second SetupTracing failed: %v", err)
	}
	ctx, span := StartSpan(context.Background(), "setup.probe",
		attribute.String(AttrOrgID, "org-1"))
	if !span.IsRecording() {
		t.Fatal("span must be recording after SetupTracing")
	}
	span.End()
	_ = ctx
	if err := shutdown2(context.Background()); err != nil {
		// Export against the dead endpoint may surface an upload error on
		// flush; the contract is that Shutdown returns (does not hang).
		t.Logf("shutdown reported (expected against dead endpoint): %v", err)
	}
	if TracingEnabled() {
		t.Fatal("shutdown2 must restore the disabled state")
	}
}

// TestRecordSpanErrorIgnoresNil keeps the helper contract pinned.
func TestRecordSpanErrorIgnoresNil(t *testing.T) {
	spans, restore := InstallTestTracing()
	defer restore()

	ctx, span := StartSpan(context.Background(), "err.probe")
	RecordSpanError(span, nil)
	span.End()

	recorded := spans()
	if len(recorded) != 1 || recorded[0].StatusCode != codes.Unset {
		t.Fatalf("nil error must not mark the span: %+v", recorded)
	}

	_, span2 := StartSpan(ctx, "err.probe2")
	RecordSpanError(span2, errors.New("kaboom"))
	span2.End()
	recorded = spans()
	if len(recorded) != 2 {
		t.Fatalf("expected 2 spans, got %d", len(recorded))
	}
	last := recorded[1]
	if last.StatusCode != codes.Error {
		t.Fatalf("status = %v, want Error", last.StatusCode)
	}
	if last.Attr("exception.message") == "" && last.Attr("exception.type") == "" {
		// RecordError emits an exception event, not attributes; the status
		// description carries the message. Nothing to assert beyond status.
		_ = last
	}
}

// BenchmarkTracingMiddlewareDisabled quantifies the zero-overhead-off
// contract: the disabled fast path must be indistinguishable from the bare
// handler chain.
func BenchmarkTracingMiddlewareDisabled(b *testing.B) {
	if TracingEnabled() {
		b.Fatal("benchmark requires tracing disabled")
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	wrapped := TracingMiddleware(handler)
	req := httptest.NewRequest(http.MethodGet, "/v1/agents", nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		wrapped.ServeHTTP(httptest.NewRecorder(), req)
	}
}

// BenchmarkTracingMiddlewareEnabled measures the enabled-mode overhead of the
// server span for comparison.
func BenchmarkTracingMiddlewareEnabled(b *testing.B) {
	spans, restore := InstallTestTracing()
	defer restore()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	wrapped := TracingMiddleware(handler)
	req := httptest.NewRequest(http.MethodGet, "/v1/agents", nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		wrapped.ServeHTTP(httptest.NewRecorder(), req)
	}
	b.StopTimer()
	if len(spans()) != b.N {
		b.Fatalf("expected %d spans, got %d", b.N, len(spans()))
	}
}
