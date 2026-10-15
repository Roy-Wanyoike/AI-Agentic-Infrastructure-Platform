package observability

import (
	"context"
	"net/http"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// ---------------------------------------------------------------------------
// Execution trace hierarchy (issue #114, additive extension of issue #80).
//
// One distributed trace per run: run → agent → step → tool/model → provider
// HTTP. Every span below is created through the same disabled-aware seam as
// the rest of the package: while tracing is OFF (the default) every helper
// short-circuits on one atomic load, returns the caller's context unchanged
// plus a non-recording no-op span, and NOTHING is allocated, recorded or
// injected — call sites stay byte-identical to the pre-#114 fast path.
//
// The boundary spans and their parents:
//
//	run.enqueue   (API, create-run)   root of the run trace
//	run.execute   (worker, task pull) child of the extracted queue-task context
//	agent.execute (runtime loop)      child of run.execute
//	step.execute  (one loop iteration) child of agent.execute
//	model.call    (provider call)     child of step.execute
//	tool.call     (tool invocation)   child of step.execute
//	HTTP <method> (provider wire)     child of model.call (TracingRoundTripper)
//
// Trace context crosses process boundaries through the queue task payload
// (W3C traceparent/tracestate keys), webhook delivery headers, NATS message
// headers and the events envelope; see InjectTraceToPayload /
// ExtractTraceFromPayload / InjectTraceHeaders / TraceIDFromContext.
// ---------------------------------------------------------------------------

// Span names used by the execution hierarchy. Kept as constants so backends
// and tests can filter on stable identifiers.
const (
	// SpanRunEnqueue is the API-side root span opened when a run is created
	// and its agent.run task is enqueued.
	SpanRunEnqueue = "run.enqueue"
	// SpanRunExecute is the worker-side span opened when a run task is
	// dequeued and executed (joins the producer trace via the task payload).
	SpanRunExecute = "run.execute"
	// SpanAgentExecute wraps one agent's participation in a run.
	SpanAgentExecute = "agent.execute"
	// SpanStepExecute wraps one bounded-loop step (model call + optional tool).
	SpanStepExecute = "step.execute"
	// SpanModelCall wraps one model provider completion call.
	SpanModelCall = "model.call"
	// SpanToolCall wraps one tool invocation (governance checks included).
	SpanToolCall = "tool.call"
	// SpanHTTPClient prefixes the provider HTTP client spans opened by
	// TracingRoundTripper (full name: SpanHTTPClient + " " + method).
	SpanHTTPClient = "HTTP"
)

// Domain attribute keys for the execution hierarchy. The agentos.* namespace
// matches the existing metric/attribute conventions (AttrOrgID, AttrAgentID,
// AttrRunID, AttrToolName live in tracing.go).
const (
	// AttrWorkflowID identifies the workflow run a span belongs to, when the
	// run was started by the workflow engine.
	AttrWorkflowID = "agentos.workflow.id"
	// AttrWorkflowNodeID identifies the workflow node that produced the run.
	AttrWorkflowNodeID = "agentos.workflow.node.id"
	// AttrModelID is the model id requested for a model-call span.
	AttrModelID = "agentos.model.id"
	// AttrStepIndex is the 1-based bounded-loop step index of a step span.
	AttrStepIndex = "agentos.step.index"
	// AttrAttempt is the queue delivery attempt (1-based) a run span executes on.
	AttrAttempt = "agentos.attempt"
	// AttrRunStatus is the terminal run status stamped on the run span when it ends.
	AttrRunStatus = "agentos.run.status"
	// AttrCostMillicents is the priced cost of a model call in millicents
	// (1/1000 cent, USD), mirroring the runs ledger pricing (models.ComputeCostCents).
	AttrCostMillicents = "agentos.cost.millicents"
	// AttrTokensPrompt is the provider-reported prompt token count of a model call.
	AttrTokensPrompt = "agentos.usage.prompt_tokens"
	// AttrTokensCompletion is the provider-reported completion token count.
	AttrTokensCompletion = "agentos.usage.completion_tokens"
	// AttrTokensTotal is the provider-reported total token count.
	AttrTokensTotal = "agentos.usage.total_tokens"
)

// Payload keys carrying the W3C trace context inside queue task payloads.
// The queue envelope (internal/queue Task) is out of bounds for edits, so the
// propagation keys ride the producer-built payload map instead.
const (
	TraceParentPayloadKey = "traceparent"
	TraceStatePayloadKey  = "tracestate"
)

// payloadCarrier adapts a queue task payload map to the OTel TextMapCarrier
// interface. Only the two W3C keys are ever read or written.
type payloadCarrier map[string]any

// Get implements propagation.TextMapCarrier.
func (c payloadCarrier) Get(key string) string {
	v, _ := c[key].(string)
	return v
}

// Set implements propagation.TextMapCarrier.
func (c payloadCarrier) Set(key, value string) { c[key] = value }

// Keys implements propagation.TextMapCarrier.
func (c payloadCarrier) Keys() []string {
	return []string{TraceParentPayloadKey, TraceStatePayloadKey}
}

// InjectTraceToPayload stamps the W3C trace context of ctx into the queue
// task payload (traceparent/tracestate payload keys). No-op when tracing is
// disabled or payload is nil — a disabled call site leaves the payload
// byte-identical, so the queue wire format is unchanged by default.
func InjectTraceToPayload(ctx context.Context, payload map[string]any) {
	if !TracingEnabled() || payload == nil || ctx == nil {
		return
	}
	otel.GetTextMapPropagator().Inject(ctx, payloadCarrier(payload))
}

// ExtractTraceFromPayload rebuilds the producer's trace context from a queue
// task payload, so consumer-side spans join the producer's trace as children.
// Returns ctx unchanged when tracing is disabled or the payload carries no
// traceparent key (never fabricates a parent).
func ExtractTraceFromPayload(ctx context.Context, payload map[string]any) context.Context {
	if !TracingEnabled() || payload == nil || ctx == nil {
		return ctx
	}
	if _, ok := payload[TraceParentPayloadKey].(string); !ok {
		return ctx
	}
	return otel.GetTextMapPropagator().Extract(ctx, payloadCarrier(payload))
}

// InjectTraceHeaders returns the W3C trace context of ctx as HTTP headers
// (traceparent/tracestate) for outbound integrations — webhook deliveries and
// NATS message headers. The returned header is empty when tracing is
// disabled, making call sites free to apply it unconditionally.
func InjectTraceHeaders(ctx context.Context) http.Header {
	h := make(http.Header)
	if !TracingEnabled() || ctx == nil {
		return h
	}
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(h))
	return h
}

// TraceIDFromContext returns the hex trace id of the span in ctx ("" when
// tracing is disabled or the context carries no valid span context).
func TraceIDFromContext(ctx context.Context) string {
	if !TracingEnabled() || ctx == nil {
		return ""
	}
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	return sc.TraceID().String()
}

// startSpanKind opens a span with an explicit span kind on the installed
// tracer (the execution hierarchy uses Internal spans except the provider
// HTTP client spans). Disabled → context unchanged + no-op span.
func startSpanKind(ctx context.Context, name string, kind trace.SpanKind, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	rt := tracingState.Load()
	if rt == nil {
		return ctx, trace.SpanFromContext(ctx)
	}
	opts := []trace.SpanStartOption{trace.WithSpanKind(kind)}
	if len(attrs) > 0 {
		opts = append(opts, trace.WithAttributes(attrs...))
	}
	return rt.tracer.Start(ctx, name, opts...)
}

// execSpanAttrs is the identity block stamped on every execution span: tenant,
// run and agent (the issue #114 required attribute set that applies to all
// execution spans). Empty values are omitted.
func execSpanAttrs(orgID, agentID, runID string) []attribute.KeyValue {
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
	return attrs
}

// StartRunEnqueueSpan opens the API-side run root span ("run.enqueue") when a
// run is created and its task enqueued, and registers the run's trace id in
// the trace registry (LookupRunTraceID — the GET /runs/{id}/trace exposure).
// The span ends when the enqueue completes; the worker joins the same trace
// through the task payload's traceparent.
func StartRunEnqueueSpan(ctx context.Context, orgID, agentID, runID string) (context.Context, trace.Span) {
	ctx, span := startSpanKind(ctx, SpanRunEnqueue, trace.SpanKindInternal, execSpanAttrs(orgID, agentID, runID)...)
	if runID != "" {
		RecordRunTraceID(runID, span.SpanContext().TraceID())
	}
	return ctx, span
}

// StartRunExecuteSpan opens the worker-side run span ("run.execute", the
// execution root inside the worker process) and registers the run's trace id.
// model is the agent's configured model id; workflowID/nodeID are set when the
// run belongs to a workflow (both omitted otherwise); attempt is the queue
// delivery attempt (0 = unknown/first pull-model attempt).
func StartRunExecuteSpan(ctx context.Context, orgID, agentID, runID, model, workflowID, nodeID string, attempt int) (context.Context, trace.Span) {
	attrs := execSpanAttrs(orgID, agentID, runID)
	if model != "" {
		attrs = append(attrs, attribute.String(AttrModelID, model))
	}
	if workflowID != "" {
		attrs = append(attrs, attribute.String(AttrWorkflowID, workflowID))
	}
	if nodeID != "" {
		attrs = append(attrs, attribute.String(AttrWorkflowNodeID, nodeID))
	}
	if attempt > 0 {
		attrs = append(attrs, attribute.Int(AttrAttempt, attempt))
	}
	ctx, span := startSpanKind(ctx, SpanRunExecute, trace.SpanKindConsumer, attrs...)
	if runID != "" {
		RecordRunTraceID(runID, span.SpanContext().TraceID())
	}
	return ctx, span
}

// StartAgentSpan opens the agent span ("agent.execute") — the child of the
// run span that wraps one agent's bounded loop.
func StartAgentSpan(ctx context.Context, orgID, agentID, runID, model string) (context.Context, trace.Span) {
	attrs := execSpanAttrs(orgID, agentID, runID)
	if model != "" {
		attrs = append(attrs, attribute.String(AttrModelID, model))
	}
	return startSpanKind(ctx, SpanAgentExecute, trace.SpanKindInternal, attrs...)
}

// StartStepSpan opens the per-step span ("step.execute") for one bounded-loop
// iteration (child of the agent span). index is the 1-based step index.
func StartStepSpan(ctx context.Context, orgID, agentID, runID string, index int) (context.Context, trace.Span) {
	attrs := append(execSpanAttrs(orgID, agentID, runID), attribute.Int(AttrStepIndex, index))
	return startSpanKind(ctx, SpanStepExecute, trace.SpanKindInternal, attrs...)
}

// StartModelSpan opens the model-call span ("model.call", child of the step
// span) wrapping one provider completion call.
func StartModelSpan(ctx context.Context, orgID, agentID, runID, modelID string) (context.Context, trace.Span) {
	attrs := execSpanAttrs(orgID, agentID, runID)
	if modelID != "" {
		attrs = append(attrs, attribute.String(AttrModelID, modelID))
	}
	return startSpanKind(ctx, SpanModelCall, trace.SpanKindClient, attrs...)
}

// SetModelSpanUsage mirrors the provider-reported token usage and the priced
// cost (millicents) of one completion onto its model-call span — the
// trace-based cost attribution contract (issue #114): cost is computed by the
// same models.ComputeCostCents pricing the runs ledger uses, so spans and
// billing agree.
func SetModelSpanUsage(span trace.Span, promptTokens, completionTokens, totalTokens int, costMillicents int64) {
	if span == nil || !span.IsRecording() {
		return
	}
	span.SetAttributes(
		attribute.Int(AttrTokensPrompt, promptTokens),
		attribute.Int(AttrTokensCompletion, completionTokens),
		attribute.Int(AttrTokensTotal, totalTokens),
		attribute.Int64(AttrCostMillicents, costMillicents),
	)
}

// StartToolInvocationSpan opens the tool-invocation span ("tool.call", child
// of the step span) wrapping one tool call including its governance checks.
func StartToolInvocationSpan(ctx context.Context, orgID, agentID, runID, tool string) (context.Context, trace.Span) {
	attrs := execSpanAttrs(orgID, agentID, runID)
	if tool != "" {
		attrs = append(attrs, attribute.String(AttrToolName, tool))
	}
	return startSpanKind(ctx, SpanToolCall, trace.SpanKindInternal, attrs...)
}

// ---------------------------------------------------------------------------
// Run trace registry (issue #114: trace id exposure).
//
// The registry maps run id → trace id for runs whose root span was created in
// THIS process (API create-run / worker execution). It is what
// GET /v1/runs/{id}/trace serves from. It is intentionally in-memory and
// bounded: the trace id is authoritative per trace (API + worker share one
// trace through the task payload), so the process that started the run can
// always answer; entries evict FIFO once the cap is reached.
// ---------------------------------------------------------------------------

// RunTraceRegistryCap bounds the in-memory run → trace registry. Runs beyond
// the cap evict oldest-first (the endpoint then reports 404, never a wrong id).
const RunTraceRegistryCap = 8192

var (
	runTraceMu    sync.Mutex
	runTraceIDs   = make(map[string]string)
	runTraceOrder = make([]string, 0, RunTraceRegistryCap)
)

// RecordRunTraceID associates a run with the trace id of its root span.
// Invalid (zero) trace ids are ignored so disabled-mode call sites can call
// unconditionally. Safe for concurrent use.
func RecordRunTraceID(runID string, traceID trace.TraceID) {
	if runID == "" || !traceID.IsValid() {
		return
	}
	runTraceMu.Lock()
	defer runTraceMu.Unlock()
	if _, exists := runTraceIDs[runID]; !exists {
		runTraceOrder = append(runTraceOrder, runID)
		for len(runTraceOrder) > RunTraceRegistryCap {
			oldest := runTraceOrder[0]
			runTraceOrder = runTraceOrder[1:]
			delete(runTraceIDs, oldest)
		}
	}
	runTraceIDs[runID] = traceID.String()
}

// LookupRunTraceID returns the recorded trace id for a run ("" and false when
// this process never started the run's trace — e.g. tracing was disabled, the
// registry evicted the entry, or the run was created in another process).
func LookupRunTraceID(runID string) (string, bool) {
	runTraceMu.Lock()
	defer runTraceMu.Unlock()
	id, ok := runTraceIDs[runID]
	return id, ok
}

// --- queue-task attempt context ---------------------------------------------

// attemptContextKey carries the queue delivery attempt into the run span.
type attemptContextKey struct{}

// ContextWithAttempt stamps the queue delivery attempt (1-based, 0 = unknown)
// onto the context; StartRunExecuteSpan reads it into agentos.attempt. The
// worker calls it when a dequeued task is handed to the runner.
func ContextWithAttempt(ctx context.Context, attempt int) context.Context {
	return context.WithValue(ctx, attemptContextKey{}, attempt)
}

// AttemptFromContext returns the stamped delivery attempt (0 when unset).
func AttemptFromContext(ctx context.Context) int {
	v, _ := ctx.Value(attemptContextKey{}).(int)
	return v
}

// --- workflow context --------------------------------------------------------

type workflowContextKey struct{}

// WorkflowContext carries the workflow run/node identity of a task payload
// into the runtime so run spans carry agentos.workflow.id and
// agentos.workflow.node.id without the runtime importing the workflow engine.
type WorkflowContext struct {
	WorkflowRunID string
	NodeID        string
}

// ContextWithWorkflow stamps the workflow identity onto the context (no-op
// values are allowed; the span helpers omit empty attributes).
func ContextWithWorkflow(ctx context.Context, workflowRunID, nodeID string) context.Context {
	return context.WithValue(ctx, workflowContextKey{}, WorkflowContext{WorkflowRunID: workflowRunID, NodeID: nodeID})
}

// WorkflowFromContext returns the stamped workflow identity (zero value when
// the run is not workflow-driven).
func WorkflowFromContext(ctx context.Context) WorkflowContext {
	wf, _ := ctx.Value(workflowContextKey{}).(WorkflowContext)
	return wf
}
