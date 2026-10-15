package synth

// worker_api.go — issue #148 (track 10-p): the API test worker.
//
// Executes journeys of KindAPI: `api_call` steps through a POOLED http.Client
// with an SSRF guard (resolve + block private/loopback/link-local unless the
// host is explicitly allowlisted in Environment.AllowedDomains), redacted
// request/response metadata in StepEvent.RedactedMeta (credential-bearing
// headers are stripped by default), bounded response-body evidence capture,
// and clock-injected latency measurement for latency_below_ms.
//
// Assertions evaluated worker-side (deterministic, never model-judged):
// api_status, json_schema (supported subset: type/enum/properties/required/
// items), url_pattern, latency_below_ms. Browser-only assertion kinds are
// recorded as StepSkipped — skipped is a DISTINCT outcome, never a pass.
//
// The same RunStateMachine, budgets, timeouts and kill-switch semantics as
// BrowserWorker apply (see worker.go).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// maxAPIBodyCapture bounds the response-body evidence artifact.
const maxAPIBodyCapture = 8192

// maxResponseBodyRead bounds how much of a response body is drained before
// closing (evidence + schema validation share the bound).
const maxResponseBodyRead = maxAPIBodyCapture + 1

// allowlistCtxKey carries the per-run domain allowlist into the pooled
// transport's dial guard (the client is worker-owned; the allowlist is
// environment-owned — the request context is the bridge).
type allowlistCtxKey struct{}

func withScopeAllowlist(ctx context.Context, allowed []string) context.Context {
	return context.WithValue(ctx, allowlistCtxKey{}, allowed)
}

func allowlistFromContext(ctx context.Context) []string {
	if allowed, ok := ctx.Value(allowlistCtxKey{}).([]string); ok {
		return allowed
	}
	return nil
}

// hostAllowlisted reports whether host matches the allowlist exactly or via a
// "*.suffix" wildcard. Unlike domainAllowed, an EMPTY allowlist authorizes
// nothing — the SSRF guard must never treat "no fence configured" as
// "private addresses authorized".
func hostAllowlisted(host string, allowed []string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if i := strings.LastIndex(host, ":"); i >= 0 && !strings.Contains(host, "]") {
		host = host[:i]
	}
	host = strings.Trim(host, "[]")
	if len(allowed) == 0 {
		return false
	}
	for _, a := range allowed {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "*" || a == host {
			return true
		}
		if strings.HasPrefix(a, "*.") {
			suffix := strings.TrimPrefix(a, "*")
			if strings.HasSuffix(host, suffix) && len(host) > len(suffix) {
				return true
			}
		}
	}
	return false
}

// forbiddenIP reports whether ip is a destination the SSRF guard refuses:
// unspecified, loopback, private (RFC1918 + IPv6 ULA), link-local unicast/
// multicast and multicast.
func forbiddenIP(ip net.IP) bool {
	return ip == nil || ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast()
}

// vetHost resolves host and refuses private destinations unless the host is
// explicitly allowlisted. Used both as the pre-flight scope check (clean
// "blocked" classification before any dial) and inside the dial guard.
func vetHost(ctx context.Context, host string, allowed []string) error {
	if hostAllowlisted(host, allowed) {
		return nil // explicitly authorized; may legitimately be internal
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnresolvableTarget, err)
	}
	if len(ips) == 0 {
		return ErrUnresolvableTarget
	}
	for _, ip := range ips {
		if forbiddenIP(ip.IP) {
			return ErrPrivateTarget
		}
	}
	return nil
}

// guardedDialContext returns a DialContext for the pooled transport: every
// dial re-resolves and re-checks the destination (no DNS-rebind window — the
// connection is pinned to the vetted IP), reading the per-run allowlist from
// the request context.
func guardedDialContext() func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("synth: invalid dial address %q: %w", addr, err)
		}
		if err := vetHost(ctx, host, allowlistFromContext(ctx)); err != nil {
			return nil, err
		}
		if hostAllowlisted(host, allowlistFromContext(ctx)) {
			return d.DialContext(ctx, network, addr)
		}
		// Pin to the vetted IP: the check and the dial observe the same
		// address even under adversarial DNS.
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil || len(ips) == 0 {
			return nil, ErrUnresolvableTarget
		}
		return d.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
	}
}

// apiRequestSpec is the optional JSON payload of an api_call step's Value:
// {"method":"POST","headers":{...},"body":"..."}. Target carries the URL.
type apiRequestSpec struct {
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

// APIWorker executes journeys of KindAPI. The pooled http.Client is built
// once per worker with the SSRF dial guard; tests may inject Client (and
// Clock) for fully deterministic runs. Bounds mirror BrowserWorker.
type APIWorker struct {
	// Client overrides the pooled client (test seam; production leaves nil).
	Client *http.Client
	// Evidence receives bounded response-body artifacts.
	Evidence EvidenceStore
	// Clock is the injectable time source.
	Clock Clock
	// StepTimeout bounds one step (DefaultStepTimeout when zero).
	StepTimeout time.Duration
	// RunWallClock bounds a whole run (DefaultRunWallClock when zero).
	RunWallClock time.Duration
	// MaxInFlight caps concurrent runs per worker instance.
	MaxInFlight int

	semOnce     sync.Once
	sem         chan struct{}
	clientOnce  sync.Once
	builtClient *http.Client
}

// NewAPIWorker wires an API worker with the given evidence store (nil →
// NoopEvidenceStore).
func NewAPIWorker(ev EvidenceStore) *APIWorker {
	return &APIWorker{Evidence: ev}
}

// Kind implements Worker.
func (w *APIWorker) Kind() RunKind { return KindAPI }

// Healthy implements Worker: the API worker needs no external runtime.
func (w *APIWorker) Healthy() bool { return w != nil }

// InFlightCap implements InFlightCap.
func (w *APIWorker) InFlightCap() int { return w.inFlightLimit() }

func (w *APIWorker) inFlightLimit() int {
	if w.MaxInFlight > 0 {
		return w.MaxInFlight
	}
	return DefaultWorkerInFlight
}

func (w *APIWorker) semaphore() chan struct{} {
	w.semOnce.Do(func() { w.sem = make(chan struct{}, w.inFlightLimit()) })
	return w.sem
}

func (w *APIWorker) clock() Clock {
	if w.Clock != nil {
		return w.Clock
	}
	return SystemClock
}

func (w *APIWorker) evidence() EvidenceStore {
	if w.Evidence != nil {
		return w.Evidence
	}
	return NoopEvidenceStore{}
}

// httpClient returns the injected client or builds the POOLED guarded one
// once per worker (connection reuse across steps; the SSRF dial guard reads
// the per-run allowlist from the request context).
func (w *APIWorker) httpClient() *http.Client {
	if w.Client != nil {
		return w.Client
	}
	w.clientOnce.Do(func() {
		w.builtClient = &http.Client{
			Transport: &http.Transport{
				DialContext:           guardedDialContext(),
				MaxIdleConns:          64,
				MaxIdleConnsPerHost:   8,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: time.Second,
			},
		}
	})
	return w.builtClient
}

// apiObservation is the running snapshot API assertions evaluate against
// (refreshed by every api_call step).
type apiObservation struct {
	url       string // sanitized (no query/userinfo)
	status    int
	latencyMS int64
	body      []byte
}

// apiExec carries the mutable per-run execution state (mirrors browserExec).
type apiExec struct {
	w         *APIWorker
	run       *Run
	sm        RunStateMachine
	clock     Clock
	store     EvidenceStore
	limiter   *rateLimiter
	budget    int
	consumed  int
	kill      <-chan struct{}
	allowlist []string
	obs       apiObservation
	assert    []Assertion
	assertIx  map[AssertionKind][]int
	claims    map[int]bool
	// lastMeta/lastRef carry the enrichment of the most recent api_call out
	// of doAPICall (the event is appended by execStep for uniform ordering).
	lastMeta map[string]string
	lastRef  string
}

// Execute implements Worker (see the Worker contract in worker.go for the
// (run, error) protocol and the cancellation semantics).
func (w *APIWorker) Execute(ctx context.Context, j *Journey, env *Environment, p *Persona, opts RunOptions) (*Run, error) {
	if j == nil {
		return nil, fmt.Errorf("synth: journey is required: %w", ErrInvalid)
	}
	if env == nil {
		return nil, fmt.Errorf("synth: environment is required: %w", ErrInvalid)
	}
	sem := w.semaphore()
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	default:
		return nil, ErrWorkerBusy
	}

	clock := w.clock()
	sm := RunStateMachine{}
	run := &Run{
		ID:            uuid.NewString(),
		OrgID:         j.OrgID,
		ProjectID:     j.ProjectID,
		JourneyID:     j.ID,
		JourneyVer:    j.Version,
		EnvironmentID: env.ID,
		Kind:          KindAPI,
		Status:        RunPending,
		AppVersion:    opts.AppVersion,
		CreatedAt:     clock.Now(),
	}
	run.UpdatedAt = run.CreatedAt
	if p != nil {
		run.PersonaID, run.PersonaVer = p.ID, p.Version
	}
	if err := sm.Transition(run, RunQueued, clock.Now()); err != nil {
		return run, err
	}
	if err := sm.Transition(run, RunRunning, clock.Now()); err != nil {
		return run, err
	}

	runCtx, cancel := haltContext(ctx, opts.KillSwitch)
	defer cancel()

	ex := &apiExec{
		w:         w,
		run:       run,
		sm:        sm,
		clock:     clock,
		store:     w.evidence(),
		limiter:   newRateLimiter(env.RateCapPerMin, DefaultRateWindow, clock),
		budget:    effectiveBudget(opts, env),
		kill:      opts.KillSwitch,
		allowlist: env.AllowedDomains,
		assert:    j.Assertions,
		assertIx:  groupAssertions(j.Assertions),
		claims:    make(map[int]bool),
	}

	status, errMsg := ex.runSteps(runCtx, j, env)
	// Best-effort cleanup (parity with BrowserWorker): skipped entirely on
	// a halt — a killed run must not touch the network again.
	if _, _, halted := haltStatus(ctx, opts.KillSwitch); !halted {
		ex.cleanup(runCtx, j, env)
	}
	if err := sm.Transition(run, status, clock.Now()); err != nil {
		return run, err
	}
	if errMsg != "" {
		run.Error = errMsg
	}
	return run, nil
}

// runSteps executes the main loop (guards identical to the browser worker).
func (ex *apiExec) runSteps(ctx context.Context, j *Journey, env *Environment) (RunStatus, string) {
	wall := ex.w.RunWallClock
	if wall <= 0 {
		wall = DefaultRunWallClock
	}
	deadline := ex.clock.Now().Add(wall)

	if st, msg, halted := haltStatus(ctx, ex.kill); halted {
		ex.event(-1, EventError, string(st), msg, nil, "")
		return st, msg
	}

	for i, step := range j.Steps {
		if st, msg, halted := haltStatus(ctx, ex.kill); halted {
			ex.event(i, EventError, string(st), msg, nil, "")
			return st, msg
		}
		if ex.consumed >= ex.budget {
			msg := ErrBudgetExceeded.Error() + fmt.Sprintf(" (step budget %d exhausted at step %d)", ex.budget, i)
			return ex.guardBlocked(j, i, msg, "budget")
		}
		if now := ex.clock.Now(); now.After(deadline) {
			msg := fmt.Sprintf("synth: run wall-clock cap (%s) exceeded", wall)
			return ex.guardBlocked(j, i, msg, "wall_clock")
		}
		if !ex.limiter.allow() {
			msg := fmt.Sprintf("synth: environment rate cap (%d/min) reached at step %d", env.RateCapPerMin, i)
			return ex.guardBlocked(j, i, msg, "rate_capped")
		}
		if step.WriteOp && env.Mode == ModeProductionReadOnly {
			msg := ErrReadOnlyMode.Error() + fmt.Sprintf(" (step %d: %s %s)", i, step.Action, step.Target)
			return ex.guardBlocked(j, i, msg, "write_denied_readonly")
		}

		outcome, detail := ex.execStep(ctx, i, step)
		switch outcome {
		case StepPassed, StepSkipped, StepBlocked:
		default: // StepFailed
			if st, msg, halted := haltStatus(ctx, ex.kill); halted {
				ex.event(i, EventError, string(st), msg, nil, "")
				return st, msg
			}
			ex.skipRest(j, i+1, detail)
			return RunFailed, truncate(detail, maxDetailChars)
		}
		ex.consumed++
	}
	return ex.epilogue(ctx, j)
}

// epilogue evaluates journey assertions not consumed by inline assert steps.
func (ex *apiExec) epilogue(ctx context.Context, j *Journey) (RunStatus, string) {
	for ai, a := range j.Assertions {
		if ex.claims[ai] {
			continue
		}
		status, detail := ex.evalAssertion(ctx, a)
		ex.event(-1, EventAssertion, status, detail, nil, "")
		if status == StepFailed {
			return RunFailed, "assertion " + string(a.Kind) + " failed: " + detail
		}
	}
	return RunCompleted, ""
}

func (ex *apiExec) guardBlocked(j *Journey, i int, reason, guard string) (RunStatus, string) {
	ex.event(i, EventAction, StepBlocked, reason, map[string]string{"guard": guard}, "")
	ex.skipRest(j, i+1, reason)
	return RunFailed, truncate(reason, maxDetailChars)
}

func (ex *apiExec) skipRest(j *Journey, from int, reason string) {
	for i := from; i < len(j.Steps); i++ {
		ex.event(i, EventAction, StepSkipped, "skipped: "+reason, nil, "")
	}
}

// cleanup executes journey.Cleanup best-effort (parity with the browser
// worker): cleanup events are recorded with negative StepIndex (-(i+1)) but
// never change the run outcome.
func (ex *apiExec) cleanup(ctx context.Context, j *Journey, env *Environment) {
	for i, step := range j.Cleanup {
		if _, _, halted := haltStatus(ctx, ex.kill); halted {
			return
		}
		if step.WriteOp && env.Mode == ModeProductionReadOnly {
			ex.event(-(i + 1), EventAction, StepBlocked, "cleanup blocked: "+ErrReadOnlyMode.Error(), nil, "")
			continue
		}
		ex.execStep(ctx, -(i + 1), step) // outcome intentionally ignored
	}
}

func (ex *apiExec) event(stepIndex int, typ, status, message string, meta map[string]string, evidenceRef string) {
	ex.run.StepEvents = append(ex.run.StepEvents, StepEvent{
		Index:        len(ex.run.StepEvents),
		StepIndex:    stepIndex,
		Type:         typ,
		Status:       status,
		Message:      truncate(message, maxDetailChars),
		RedactedMeta: meta,
		EvidenceRef:  evidenceRef,
		At:           ex.clock.Now(),
	})
}

// execStep performs one journey step (StepIndex negative = cleanup slot).
func (ex *apiExec) execStep(ctx context.Context, stepIndex int, step JourneyStep) (string, string) {
	stepTimeout := ex.w.StepTimeout
	if stepTimeout <= 0 {
		stepTimeout = DefaultStepTimeout
	}
	if step.TimeoutMS > 0 {
		stepTimeout = time.Duration(step.TimeoutMS) * time.Millisecond
	}
	sctx, cancel := context.WithTimeout(withScopeAllowlist(ctx, ex.allowlist), stepTimeout)
	defer cancel()

	var status, detail string
	switch step.Action {
	case "api_call":
		status, detail = ex.doAPICall(sctx, stepIndex, step)
		ex.event(stepIndex, EventNetwork, status, detail, ex.lastMeta, ex.lastRef)
	case "assert":
		status, detail = ex.execInlineAssert(ctx, stepIndex, step)
		ex.event(stepIndex, EventAssertion, status, detail, nil, "")
	case "navigate", "click", "fill", "select", "wait":
		status, detail = StepSkipped, fmt.Sprintf("action %q requires the browser worker (api driver has no browser surface)", step.Action)
		ex.event(stepIndex, EventAction, status, detail, nil, "")
	case "extract":
		status, detail = StepSkipped, "extract is not part of the api worker surface yet (assertions cover response checks)"
		ex.event(stepIndex, EventAction, status, detail, nil, "")
	default:
		status, detail = StepSkipped, fmt.Sprintf("unsupported api action %q", step.Action)
		ex.event(stepIndex, EventAction, status, detail, nil, "")
	}

	if status == StepFailed {
		if st, msg, halted := haltStatus(ctx, ex.kill); halted {
			ex.event(stepIndex, EventError, string(st), msg, nil, "")
			return string(st), msg
		}
		if errors.Is(sctx.Err(), context.DeadlineExceeded) && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			detail = fmt.Sprintf("step timeout after %s: %s", stepTimeout, detail)
			ex.event(stepIndex, EventError, StepFailed, detail, nil, "")
			return StepFailed, detail
		}
	}
	return status, detail
}

func (ex *apiExec) execInlineAssert(ctx context.Context, stepIndex int, step JourneyStep) (string, string) {
	kind := AssertionKind(strings.TrimSpace(step.Target))
	for _, ai := range ex.assertIx[kind] {
		if !ex.claims[ai] {
			ex.claims[ai] = true
			return ex.evalAssertion(ctx, ex.assert[ai])
		}
	}
	return StepFailed, fmt.Sprintf("assert step %d references kind %q but no unconsumed journey assertion of that kind exists", stepIndex, kind)
}

// doAPICall performs the HTTP request for one api_call step: scope/SSRF
// pre-flight, pooled client execution, latency measurement, redacted
// metadata and bounded body evidence. The returned status is StepPassed,
// StepFailed or StepBlocked; details land in ex.lastMeta/ex.lastRef.
func (ex *apiExec) doAPICall(ctx context.Context, stepIndex int, step JourneyStep) (string, string) {
	raw := strings.TrimSpace(step.Target)
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		ex.lastMeta = map[string]string{"url": raw}
		return StepFailed, fmt.Sprintf("api_call target %q is not a valid http(s) URL", raw)
	}
	// Pre-flight scope + SSRF check (clean "blocked" classification before
	// any dial; the dial guard re-checks against rebinding).
	if err := vetHost(ctx, u.Hostname(), ex.allowlist); err != nil {
		ex.lastMeta = map[string]string{"url": sanitizeURL(u)}
		return StepBlocked, ErrScopeViolation.Error() + " (" + err.Error() + ")"
	}

	spec := parseAPISpec(step.Value)
	method := strings.ToUpper(strings.TrimSpace(spec.Method))
	if method == "" {
		method = http.MethodGet
	}
	var bodyReader io.Reader
	if spec.Body != "" {
		bodyReader = strings.NewReader(spec.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bodyReader)
	if err != nil {
		ex.lastMeta = map[string]string{"url": sanitizeURL(u), "method": method}
		return StepFailed, "api_call request build failed: " + err.Error()
	}
	for k, v := range spec.Headers {
		req.Header.Set(k, v)
	}
	if spec.Body != "" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}

	reqHeaders := RedactHeader(req.Header)
	start := ex.clock.Now()
	resp, err := ex.w.httpClient().Do(req)
	latency := ex.clock.Now().Sub(start)
	if err != nil {
		ex.lastMeta = map[string]string{"url": sanitizeURL(u), "method": method, "error": truncate(err.Error(), 256)}
		if st, msg, halted := haltStatus(context.WithoutCancel(ctx), ex.kill); halted {
			_ = st
			return StepFailed, msg // re-classified by execStep as a halt
		}
		return StepFailed, "request failed: " + err.Error()
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodyRead))
	if readErr != nil && len(body) == 0 {
		ex.lastMeta = map[string]string{"url": sanitizeURL(u), "method": method}
		return StepFailed, "response read failed: " + readErr.Error()
	}
	if len(body) > maxAPIBodyCapture {
		body = body[:maxAPIBodyCapture]
	}

	ex.obs = apiObservation{url: sanitizeURL(u), status: resp.StatusCode, latencyMS: latency.Milliseconds(), body: body}
	meta := map[string]string{
		"method":     method,
		"url":        ex.obs.url,
		"status":     strconv.Itoa(resp.StatusCode),
		"latency_ms": strconv.FormatInt(ex.obs.latencyMS, 10),
	}
	for k, v := range reqHeaders {
		meta["req_"+strings.ToLower(k)] = v
	}
	for k, v := range RedactHeader(resp.Header) {
		meta["resp_"+strings.ToLower(k)] = v
	}
	ex.lastMeta = meta

	ref, _ := ex.store.Put(ctx, "api_response", body, map[string]string{
		"run_id": ex.run.ID, "status": strconv.Itoa(resp.StatusCode), "url": ex.obs.url,
	})
	ex.run.EvidenceRefs = append(ex.run.EvidenceRefs, ref)
	ex.lastRef = ref

	return StepPassed, fmt.Sprintf("%s %s -> %d (%dms)", method, ex.obs.url, resp.StatusCode, ex.obs.latencyMS)
}

// parseAPISpec decodes the optional Value JSON of an api_call step. An
// empty value yields an empty spec (plain GET); a malformed value decodes
// to whatever prefix JSON accepts (authoring defects surface as request
// failures downstream, never as silent method changes).
func parseAPISpec(value string) apiRequestSpec {
	var spec apiRequestSpec
	if strings.TrimSpace(value) == "" {
		return spec
	}
	_ = json.Unmarshal([]byte(value), &spec)
	return spec
}

// evalAssertion evaluates one deterministic assertion against HTTP
// observations. Browser-only kinds are StepSkipped (distinct, never pass).
func (ex *apiExec) evalAssertion(ctx context.Context, a Assertion) (string, string) {
	obs := ex.obs
	switch a.Kind {
	case AssertAPIStatus:
		if obs.status == 0 {
			return StepFailed, "api_status: no observed response"
		}
		if matchStatusClass(a.Expected, obs.status) {
			return StepPassed, fmt.Sprintf("status %d matches %s", obs.status, a.Expected)
		}
		return StepFailed, fmt.Sprintf("status %d does not match %s", obs.status, a.Expected)
	case AssertJSONSchema:
		var schema map[string]any
		if err := json.Unmarshal([]byte(a.Expected), &schema); err != nil {
			return StepFailed, "json_schema: invalid schema document: " + err.Error()
		}
		var doc any
		if err := json.Unmarshal(obs.body, &doc); err != nil {
			return StepFailed, "json_schema: response body is not valid JSON: " + err.Error()
		}
		if err := validateJSONSchema(schema, doc); err != nil {
			return StepFailed, "json_schema: " + err.Error()
		}
		return StepPassed, "response body satisfies schema"
	case AssertURLPattern:
		ok, err := matchURLPattern(a.Expected, obs.url)
		if err != nil {
			return StepFailed, "url_pattern: " + err.Error()
		}
		if ok {
			return StepPassed, "url matches pattern " + a.Expected
		}
		return StepFailed, fmt.Sprintf("url %q does not match pattern %q", obs.url, a.Expected)
	case AssertLatencyBelow:
		bound, err := parseNonNegative(a.Expected)
		if err != nil {
			return StepFailed, "latency_below_ms: invalid bound " + a.Expected
		}
		if obs.latencyMS < bound {
			return StepPassed, fmt.Sprintf("latency %dms below %dms", obs.latencyMS, bound)
		}
		return StepFailed, fmt.Sprintf("latency %dms exceeds %dms", obs.latencyMS, bound)
	case AssertElementVisible, AssertTextContains, AssertNoConsoleError, AssertA11y:
		return StepSkipped, fmt.Sprintf("%s requires the browser worker", string(a.Kind))
	default:
		return StepSkipped, fmt.Sprintf("unknown assertion kind %q", a.Kind)
	}
}

// validateJSONSchema checks doc against the schema subset supported by the
// api worker: type, enum, properties (recursive), required, items. Unknown
// keywords are ignored (forward-compatible per the JSON Schema spec). The
// returned error names the first violation path.
func validateJSONSchema(schema map[string]any, doc any) error {
	if schema == nil {
		return fmt.Errorf("empty schema")
	}
	if t, ok := schema["type"]; ok {
		switch tv := t.(type) {
		case string:
			if !jsonTypeMatches(tv, doc) {
				return fmt.Errorf("expected type %s, got %s", tv, jsonTypeName(doc))
			}
		case []any:
			matched := false
			for _, candidate := range tv {
				if name, ok := candidate.(string); ok && jsonTypeMatches(name, doc) {
					matched = true
					break
				}
			}
			if !matched {
				return fmt.Errorf("type not in allowed set %v (got %s)", tv, jsonTypeName(doc))
			}
		}
	}
	if allowed, ok := schema["enum"].([]any); ok {
		found := false
		for _, candidate := range allowed {
			if jsonDeepEqual(candidate, doc) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("value not in enum")
		}
	}
	switch node := doc.(type) {
	case map[string]any:
		if props, ok := schema["properties"].(map[string]any); ok {
			for key, sub := range props {
				value, present := node[key]
				if !present {
					continue
				}
				subSchema, ok := sub.(map[string]any)
				if !ok {
					continue
				}
				if err := validateJSONSchema(subSchema, value); err != nil {
					return fmt.Errorf("property %q: %w", key, err)
				}
			}
		}
		if required, ok := schema["required"].([]any); ok {
			for _, r := range required {
				name, ok := r.(string)
				if !ok {
					continue
				}
				if _, present := node[name]; !present {
					return fmt.Errorf("missing required property %q", name)
				}
			}
		}
	case []any:
		if items, ok := schema["items"].(map[string]any); ok {
			for i, item := range node {
				if err := validateJSONSchema(items, item); err != nil {
					return fmt.Errorf("item %d: %w", i, err)
				}
			}
		}
	}
	return nil
}

func jsonTypeName(doc any) string {
	switch doc.(type) {
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	case nil:
		return "null"
	default:
		return fmt.Sprintf("%T", doc)
	}
}

func jsonTypeMatches(want string, doc any) bool {
	switch want {
	case "object":
		_, ok := doc.(map[string]any)
		return ok
	case "array":
		_, ok := doc.([]any)
		return ok
	case "string":
		_, ok := doc.(string)
		return ok
	case "number":
		_, ok := doc.(float64)
		return ok
	case "integer":
		f, ok := doc.(float64)
		return ok && f == math.Trunc(f)
	case "boolean":
		_, ok := doc.(bool)
		return ok
	case "null":
		return doc == nil
	default:
		return false
	}
}

func jsonDeepEqual(a, b any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return bytes.Equal(ab, bb)
}
