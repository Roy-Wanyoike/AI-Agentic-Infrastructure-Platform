package observability

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// TestPayloadTracePropagation table-drives the queue-task trace context
// seam: with tracing enabled the W3C traceparent rides the payload and the
// consumer rejoins the producer's trace; with tracing disabled the payload is
// byte-identical (no keys added) and extraction is the identity function.
func TestPayloadTracePropagation(t *testing.T) {
	t.Run("disabled_leaves_payload_untouched", func(t *testing.T) {
		if TracingEnabled() {
			t.Fatal("precondition: tracing must be disabled")
		}
		payload := map[string]any{"run_id": "run-1"}
		InjectTraceToPayload(context.Background(), payload)
		if len(payload) != 1 {
			t.Fatalf("disabled inject must not add keys, got %v", payload)
		}
		if got := ExtractTraceFromPayload(context.Background(), payload); got != context.Background() {
			t.Fatal("disabled extract must return the context unchanged")
		}
		if h := InjectTraceHeaders(context.Background()); len(h) != 0 {
			t.Fatalf("disabled header inject must be empty, got %v", h)
		}
		if tid := TraceIDFromContext(context.Background()); tid != "" {
			t.Fatalf("disabled trace id must be empty, got %q", tid)
		}
	})

	t.Run("enabled_roundtrip_joins_producer_trace", func(t *testing.T) {
		spans, restore := InstallTestTracing()
		defer restore()

		// Producer: a parent span whose context is injected into the payload.
		pctx, parent := StartSpan(context.Background(), "test.producer",
			attribute.String(AttrRunID, "run-1"))
		payload := map[string]any{"run_id": "run-1"}
		InjectTraceToPayload(pctx, payload)
		parent.End()

		tp, ok := payload[TraceParentPayloadKey].(string)
		if !ok || !strings.HasPrefix(tp, "00-") {
			t.Fatalf("payload must carry a W3C traceparent, got %v", payload[TraceParentPayloadKey])
		}

		// Consumer: a fresh context rejoins the producer's trace.
		cctx := ExtractTraceFromPayload(context.Background(), payload)
		childCtx, child := StartSpan(cctx, "test.consumer")
		child.End()

		recorded := spans()
		if len(recorded) != 2 {
			t.Fatalf("expected 2 spans, got %d: %+v", len(recorded), recorded)
		}
		if recorded[1].TraceID != recorded[0].TraceID {
			t.Fatalf("consumer trace %v must equal producer trace %v", recorded[1].TraceID, recorded[0].TraceID)
		}
		if recorded[1].ParentSpanID != recorded[0].SpanID {
			t.Fatalf("consumer parent %v must be producer span %v", recorded[1].ParentSpanID, recorded[0].SpanID)
		}
		if trace.SpanFromContext(childCtx).SpanContext().TraceID() != recorded[0].TraceID {
			t.Fatal("extracted context must carry the producer trace id")
		}
	})

	t.Run("enabled_extract_without_traceparent_is_identity", func(t *testing.T) {
		_, restore := InstallTestTracing()
		defer restore()
		payload := map[string]any{"run_id": "run-2"}
		if got := ExtractTraceFromPayload(context.Background(), payload); got != context.Background() {
			t.Fatal("a payload without traceparent must not fabricate a parent")
		}
	})
}

// TestHeaderPropagationAndTraceID pins the http.Header carrier: enabled mode
// emits a parseable traceparent header carrying the current trace id, and
// TraceIDFromContext round-trips through a fresh context extracted from it.
func TestHeaderPropagationAndTraceID(t *testing.T) {
	spans, restore := InstallTestTracing()
	defer restore()

	ctx, span := StartSpan(context.Background(), "test.headers")
	headers := InjectTraceHeaders(ctx)
	if got := headers.Get("Traceparent"); !strings.HasPrefix(got, "00-") {
		t.Fatalf("traceparent header missing/malformed: %q", got)
	}
	wantTrace := span.SpanContext().TraceID().String()
	if !strings.Contains(headers.Get("Traceparent"), wantTrace) {
		t.Fatalf("traceparent %q must carry trace id %s", headers.Get("Traceparent"), wantTrace)
	}
	if got := TraceIDFromContext(ctx); got != wantTrace {
		t.Fatalf("TraceIDFromContext = %q, want %q", got, wantTrace)
	}
	span.End()

	// A consumer that extracts from the headers lands on the same trace.
	extracted := otel.GetTextMapPropagator().Extract(context.Background(), propagation.HeaderCarrier(headers))
	if tid := TraceIDFromContext(extracted); tid != wantTrace {
		t.Fatalf("extracted trace id = %q, want %q", tid, wantTrace)
	}
	if len(spans()) != 1 {
		t.Fatalf("expected exactly 1 span, got %d", len(spans()))
	}
}

// TestRunTraceRegistry covers record/lookup plus the FIFO eviction bound.
func TestRunTraceRegistry(t *testing.T) {
	RecordRunTraceID("run-a", trace.TraceID{0x1})
	wantA := trace.TraceID{0x1}.String()
	if got, ok := LookupRunTraceID("run-a"); !ok || got != wantA {
		t.Fatalf("LookupRunTraceID(run-a) = %q,%v", got, ok)
	}
	// Invalid trace ids are ignored (disabled-mode call sites stay no-ops).
	RecordRunTraceID("run-bad", trace.TraceID{})
	RecordRunTraceID("", trace.TraceID{0x2})
	if _, ok := LookupRunTraceID("run-bad"); ok {
		t.Fatal("invalid trace id must not be recorded")
	}
	if _, ok := LookupRunTraceID(""); ok {
		t.Fatal("empty run id must not be recorded")
	}
	if _, ok := LookupRunTraceID("run-missing"); ok {
		t.Fatal("unknown run id must miss")
	}

	// Eviction: flooding past the cap drops the oldest entries.
	for i := 0; i < RunTraceRegistryCap+10; i++ {
		id := trace.TraceID{byte(i / 256), byte(i % 256)}
		RecordRunTraceID("run-flood-"+strings.Repeat("x", 0)+itoa(i), id)
	}
	if _, ok := LookupRunTraceID("run-a"); ok {
		t.Fatal("oldest entry must be evicted once the cap is exceeded")
	}
	last := RunTraceRegistryCap + 9
	if _, ok := LookupRunTraceID("run-flood-" + itoa(last)); !ok {
		t.Fatal("newest entry must remain recorded")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// TestExecutionSpanHierarchy proves the run → agent → step → model/tool span
// tree, the required agentos.* attributes on every execution span, and the
// model-call cost/usage attribution.
func TestExecutionSpanHierarchy(t *testing.T) {
	spans, restore := InstallTestTracing()
	defer restore()

	runCtx, runSpan := StartRunExecuteSpan(context.Background(), "org-1", "agent-1", "run-1", "gpt-4o-mini", "wfrun-1", "node-1", 2)
	agentCtx, agentSpan := StartAgentSpan(runCtx, "org-1", "agent-1", "run-1", "gpt-4o-mini")
	stepCtx, stepSpan := StartStepSpan(agentCtx, "org-1", "agent-1", "run-1", 3)
	_, modelSpan := StartModelSpan(stepCtx, "org-1", "agent-1", "run-1", "gpt-4o-mini")
	SetModelSpanUsage(modelSpan, 120, 45, 165, 3700)
	modelSpan.End()
	_, toolSpan := StartToolInvocationSpan(stepCtx, "org-1", "agent-1", "run-1", "calculator")
	toolSpan.End()
	stepSpan.End()
	agentSpan.End()
	runSpan.End()

	recorded := spans()
	if len(recorded) != 5 {
		t.Fatalf("expected 5 spans, got %d: %+v", len(recorded), recorded)
	}
	byName := map[string]RecordedSpan{}
	for _, s := range recorded {
		byName[s.Name] = s
	}
	run, ok := byName[SpanRunExecute]
	if !ok {
		t.Fatalf("run span missing: %+v", recorded)
	}
	agent, ok := byName[SpanAgentExecute]
	if !ok {
		t.Fatalf("agent span missing: %+v", recorded)
	}
	step, ok := byName[SpanStepExecute]
	if !ok {
		t.Fatalf("step span missing: %+v", recorded)
	}
	model, ok := byName[SpanModelCall]
	if !ok {
		t.Fatalf("model span missing: %+v", recorded)
	}
	tool, ok := byName[SpanToolCall]
	if !ok {
		t.Fatalf("tool span missing: %+v", recorded)
	}

	// Parent chain: tool+model → step → agent → run.
	if tool.ParentSpanID != step.SpanID || model.ParentSpanID != step.SpanID {
		t.Fatalf("model/tool spans must be children of the step span")
	}
	if step.ParentSpanID != agent.SpanID || agent.ParentSpanID != run.SpanID {
		t.Fatalf("step/agent spans must be children of agent/run spans")
	}
	for _, s := range []RecordedSpan{run, agent, step, model, tool} {
		if s.Attr(AttrOrgID) != "org-1" || s.Attr(AttrAgentID) != "agent-1" || s.Attr(AttrRunID) != "run-1" {
			t.Errorf("span %s missing identity attrs: %v", s.Name, s.Attributes)
		}
	}
	if got := run.Attr(AttrModelID); got != "gpt-4o-mini" {
		t.Errorf("run span %s = %q", AttrModelID, got)
	}
	if got := run.Attr(AttrWorkflowID); got != "wfrun-1" {
		t.Errorf("run span %s = %q", AttrWorkflowID, got)
	}
	if got := run.Attr(AttrWorkflowNodeID); got != "node-1" {
		t.Errorf("run span %s = %q", AttrWorkflowNodeID, got)
	}
	if got := run.Attr(AttrAttempt); got != "2" {
		t.Errorf("run span %s = %q", AttrAttempt, got)
	}
	if got := step.Attr(AttrStepIndex); got != "3" {
		t.Errorf("step span %s = %q", AttrStepIndex, got)
	}
	if got := tool.Attr(AttrToolName); got != "calculator" {
		t.Errorf("tool span %s = %q", AttrToolName, got)
	}
	if got := model.Attr(AttrModelID); got != "gpt-4o-mini" {
		t.Errorf("model span %s = %q", AttrModelID, got)
	}
	if got := model.Attr(AttrTokensPrompt); got != "120" {
		t.Errorf("model span %s = %q", AttrTokensPrompt, got)
	}
	if got := model.Attr(AttrTokensCompletion); got != "45" {
		t.Errorf("model span %s = %q", AttrTokensCompletion, got)
	}
	if got := model.Attr(AttrTokensTotal); got != "165" {
		t.Errorf("model span %s = %q", AttrTokensTotal, got)
	}
	if got := model.Attr(AttrCostMillicents); got != "3700" {
		t.Errorf("model span %s = %q", AttrCostMillicents, got)
	}

	// Trace exposure: the run's trace id is registered for GET /runs/{id}/trace.
	tid, ok := LookupRunTraceID("run-1")
	if !ok || tid != run.TraceID.String() {
		t.Fatalf("registry trace id = %q,%v want %q", tid, ok, run.TraceID.String())
	}
}

// TestExecutionSpansDisabledAreNoOps pins the zero-overhead-off contract for
// every #114 helper: contexts unchanged, non-recording spans, empty registry.
func TestExecutionSpansDisabledAreNoOps(t *testing.T) {
	if TracingEnabled() {
		t.Fatal("precondition: tracing must be disabled")
	}
	base := context.Background()

	ctx, runSpan := StartRunExecuteSpan(base, "o", "a", "r", "m", "w", "n", 1)
	if ctx != base || runSpan.IsRecording() {
		t.Fatal("disabled StartRunExecuteSpan must be a no-op")
	}
	if _, ok := LookupRunTraceID("r"); ok {
		t.Fatal("disabled run span must not register a trace id")
	}
	ctx2, agentSpan := StartAgentSpan(ctx, "o", "a", "r", "m")
	if ctx2 != base || agentSpan.IsRecording() {
		t.Fatal("disabled StartAgentSpan must be a no-op")
	}
	ctx3, stepSpan := StartStepSpan(ctx2, "o", "a", "r", 1)
	if ctx3 != base || stepSpan.IsRecording() {
		t.Fatal("disabled StartStepSpan must be a no-op")
	}
	ctx4, modelSpan := StartModelSpan(ctx3, "o", "a", "r", "m")
	if ctx4 != base || modelSpan.IsRecording() {
		t.Fatal("disabled StartModelSpan must be a no-op")
	}
	SetModelSpanUsage(modelSpan, 1, 2, 3, 4) // must not panic
	ctx5, toolSpan := StartToolInvocationSpan(ctx4, "o", "a", "r", "calculator")
	if ctx5 != base || toolSpan.IsRecording() {
		t.Fatal("disabled StartToolInvocationSpan must be a no-op")
	}
	ctx6, enqSpan := StartRunEnqueueSpan(ctx5, "o", "a", "r")
	if ctx6 != base || enqSpan.IsRecording() {
		t.Fatal("disabled StartRunEnqueueSpan must be a no-op")
	}
	_, wf := StartSpan(ContextWithWorkflow(base, "wfr", "nd"), "probe")
	if wf.IsRecording() {
		t.Fatal("no span expected while disabled")
	}
	if got := AttemptFromContext(ContextWithAttempt(base, 4)); got != 4 {
		t.Fatalf("attempt context round-trip = %d", got)
	}
	if got := WorkflowFromContext(ContextWithWorkflow(base, "wfr", "nd")); got.WorkflowRunID != "wfr" || got.NodeID != "nd" {
		t.Fatalf("workflow context round-trip = %+v", got)
	}
}

// TestTracingRoundTripperClientSpan proves the provider HTTP instrumentation:
// enabled mode opens a CLIENT span child of the active span, injects the W3C
// traceparent into the outgoing headers, records the response status and
// marks 5xx as span errors; disabled mode is a transparent passthrough with
// no headers and no spans.
func TestTracingRoundTripperClientSpan(t *testing.T) {
	type captured struct {
		traceparent string
	}
	cases := []struct {
		name        string
		enabled     bool
		serverCode  int
		wantHeaders bool
		wantSpans   int
		wantErrSpan bool
	}{
		{name: "enabled_2xx", enabled: true, serverCode: 200, wantHeaders: true, wantSpans: 2, wantErrSpan: false},
		{name: "enabled_5xx_marks_error", enabled: true, serverCode: 503, wantHeaders: true, wantSpans: 2, wantErrSpan: true},
		{name: "disabled_passthrough", enabled: false, serverCode: 200, wantHeaders: false, wantSpans: 0, wantErrSpan: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var restore func()
			var recorded func() []RecordedSpan
			if tc.enabled {
				recorded, restore = InstallTestTracing()
			} else {
				recorded = func() []RecordedSpan { return nil }
				restore = func() {}
			}
			defer restore()

			capCh := make(chan captured, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				capCh <- captured{traceparent: r.Header.Get("Traceparent")}
				w.WriteHeader(tc.serverCode)
			}))
			defer srv.Close()

			parentCtx, parent := StartSpan(context.Background(), "model.call")
			client := &http.Client{Transport: NewTracingRoundTripper(http.DefaultTransport)}
			req, _ := http.NewRequestWithContext(parentCtx, http.MethodPost, srv.URL+"/chat/completions", nil)
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("round trip failed: %v", err)
			}
			resp.Body.Close()
			parent.End()

			cap := <-capCh
			if tc.wantHeaders {
				if cap.traceparent == "" {
					t.Fatal("expected traceparent header on the outbound request")
				}
			} else if cap.traceparent != "" {
				t.Fatalf("disabled mode must not inject traceparent, got %q", cap.traceparent)
			}

			got := recorded()
			if tc.enabled && len(got) != tc.wantSpans {
				t.Fatalf("expected %d spans, got %d: %+v", tc.wantSpans, len(got), got)
			}
			if !tc.enabled {
				return
			}
			var clientSpan RecordedSpan
			for _, s := range got {
				if strings.HasPrefix(s.Name, SpanHTTPClient+" ") {
					clientSpan = s
				}
			}
			if clientSpan.Name == "" {
				t.Fatalf("client span missing: %+v", got)
			}
			if clientSpan.SpanKind != trace.SpanKindClient {
				t.Errorf("span kind = %v, want client", clientSpan.SpanKind)
			}
			if clientSpan.ParentSpanID != parent.SpanContext().SpanID() {
				t.Errorf("client span must be a child of the active span")
			}
			if got2 := clientSpan.Attr(AttrHTTPStatus); got2 != itoa(tc.serverCode) {
				t.Errorf("span %s = %q, want %d", AttrHTTPStatus, got2, tc.serverCode)
			}
			if got2 := clientSpan.Attr("server.address"); got2 == "" {
				t.Errorf("span missing server.address: %v", clientSpan.Attributes)
			}
			isErr := clientSpan.StatusCode == codes.Error
			if isErr != tc.wantErrSpan {
				t.Errorf("span status code = %v, wantErrSpan=%v", clientSpan.StatusCode, tc.wantErrSpan)
			}
		})
	}
}
