package synth

// worker_api_test.go — issue #148: deterministic tests for the API test
// worker. Every HTTP exchange runs against an httptest server bound to
// 127.0.0.1 — exactly the class of destination the SSRF guard polices — so
// the environment allowlist decides who may talk to it. Time is injected
// through tickClock: doAPICall brackets each request with exactly two Now()
// calls, so every api_call observes exactly one tick of latency, making
// latency_below_ms assertions fully deterministic. No external network, no
// sleeps; the in-flight test synchronizes via channels.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Test doubles.
// ---------------------------------------------------------------------------

// tickClock is a deterministic Clock that advances a fixed step on every
// Now() call.
type tickClock struct {
	mu   sync.Mutex
	t    time.Time
	step time.Duration
}

func newTickClock(step time.Duration) *tickClock {
	return &tickClock{t: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), step: step}
}

func (c *tickClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(c.step)
	return c.t
}

// apiEnv builds an Environment whose allowlist contains exactly the given
// hosts (no arguments = empty fence; private destinations stay blocked).
func apiEnv(allowed ...string) *Environment {
	return &Environment{
		ID:             "env-api-1",
		OrgID:          "org-1",
		Kind:           EnvStaging,
		Mode:           ModePreProduction,
		AllowedDomains: allowed,
	}
}

// apiJourney builds a minimal journey with the given steps and assertions.
func apiJourney(steps []JourneyStep, assertions []Assertion) *Journey {
	return &Journey{
		ID:         "j-api-1",
		OrgID:      "org-1",
		ProjectID:  "proj-1",
		Name:       "api contract journey",
		Status:     JourneyReviewed,
		Version:    3,
		Steps:      steps,
		Assertions: assertions,
	}
}

func apiStep(action, target, value string) JourneyStep {
	return JourneyStep{Action: action, Target: target, Value: value}
}

// eventsOf filters run.StepEvents by event type.
func eventsOf(run *Run, typ string) []StepEvent {
	var out []StepEvent
	for _, ev := range run.StepEvents {
		if ev.Type == typ {
			out = append(out, ev)
		}
	}
	return out
}

// eventsAtStep filters run.StepEvents by journey step index (negative for
// cleanup slots).
func eventsAtStep(run *Run, ix int) []StepEvent {
	var out []StepEvent
	for _, ev := range run.StepEvents {
		if ev.StepIndex == ix {
			out = append(out, ev)
		}
	}
	return out
}

// apiServer is a tiny deterministic httptest fixture: /echo reflects the
// received method/headers back as JSON (proving what traveled the wire) and
// sets a credential-bearing response header; /user returns a fixed JSON
// document; /cleanup is a counted write endpoint.
type apiServer struct {
	srv      *httptest.Server
	mu       sync.Mutex
	cleanups int
}

func newAPIServer(t *testing.T) *apiServer {
	t.Helper()
	a := &apiServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "session=server-secret; Path=/")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"method":        r.Method,
			"authorization": r.Header.Get("Authorization"),
			"cookie":        r.Header.Get("Cookie"),
		})
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"user":{"name":"ada","roles":["admin"],"n":42}}`))
	})
	mux.HandleFunc("/cleanup", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.cleanups++
		a.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	a.srv = httptest.NewServer(mux)
	t.Cleanup(a.srv.Close)
	return a
}

func (a *apiServer) cleanupHits() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cleanups
}

// ---------------------------------------------------------------------------
// SSRF guard.
// ---------------------------------------------------------------------------

func TestAPIWorkerSSRFBlocksPrivateTargets(t *testing.T) {
	a := newAPIServer(t)
	store := NewInMemoryEvidenceStore()
	w := NewAPIWorker(store)
	w.Clock = newTickClock(2 * time.Millisecond)

	// Empty allowlist: the fence authorizes nothing — loopback and
	// link-local destinations are refused before any dial.
	env := apiEnv()
	j := apiJourney([]JourneyStep{
		apiStep("api_call", a.srv.URL+"/user", ""),                          // 127.0.0.1 (loopback)
		apiStep("api_call", "http://169.254.169.254/latest/meta-data/", ""), // link-local (cloud metadata)
	}, nil)

	run, err := w.Execute(context.Background(), j, env, nil, RunOptions{})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	// A blocked STEP is non-fatal (only blocked GUARDS are); with no failing
	// assertion the run completes — the verdicts live on the events.
	if run.Status != RunCompleted {
		t.Fatalf("run status = %q, want %q: %s", run.Status, RunCompleted, run.Error)
	}

	net := eventsOf(run, EventNetwork)
	if len(net) != 2 {
		t.Fatalf("got %d network events, want 2", len(net))
	}
	for i, ev := range net {
		if ev.Status != StepBlocked {
			t.Errorf("network event %d status = %q, want %q", i, ev.Status, StepBlocked)
		}
		if !strings.Contains(ev.Message, ErrScopeViolation.Error()) || !strings.Contains(ev.Message, ErrPrivateTarget.Error()) {
			t.Errorf("network event %d message = %q, want scope violation with private-target refusal", i, ev.Message)
		}
		// The sanitized URL keeps scheme://host/path but never the query.
		if u := ev.RedactedMeta["url"]; strings.Contains(u, "?") {
			t.Errorf("metadata url %q carries a query string", u)
		}
	}
	// Blocked before any dial: the server must have seen zero requests and
	// no response-body evidence may exist.
	if hits := a.cleanupHits(); hits != 0 {
		t.Errorf("server received %d requests on blocked targets, want 0", hits)
	}
	if len(run.EvidenceRefs) != 0 || store.Len() != 0 {
		t.Errorf("blocked run stored evidence (refs=%d, store=%d), want none", len(run.EvidenceRefs), store.Len())
	}
}

func TestAPIWorkerAllowlistHonored(t *testing.T) {
	a := newAPIServer(t)
	w := NewAPIWorker(nil)
	w.Clock = newTickClock(2 * time.Millisecond)

	// Exactly 127.0.0.1 is allowlisted (the httptest host): the loopback
	// call is explicitly authorized, but "localhost" — same IP, different
	// host — stays outside the fence (matching is by host, not by IP).
	env := apiEnv("127.0.0.1")
	localhostURL := strings.Replace(a.srv.URL, "127.0.0.1", "localhost", 1)
	j := apiJourney([]JourneyStep{
		apiStep("api_call", a.srv.URL+"/user", ""),
		apiStep("api_call", localhostURL+"/user", ""),
	}, nil)

	run, err := w.Execute(context.Background(), j, env, nil, RunOptions{})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	net := eventsOf(run, EventNetwork)
	if len(net) != 2 {
		t.Fatalf("got %d network events, want 2", len(net))
	}
	if net[0].Status != StepPassed {
		t.Errorf("allowlisted loopback call status = %q (msg %q), want %q", net[0].Status, net[0].Message, StepPassed)
	}
	if net[1].Status != StepBlocked {
		t.Errorf("non-allowlisted localhost call status = %q, want %q", net[1].Status, StepBlocked)
	}
	if !strings.Contains(net[1].Message, ErrScopeViolation.Error()) {
		t.Errorf("localhost call message = %q, want scope violation", net[1].Message)
	}
}

// ---------------------------------------------------------------------------
// Redaction.
// ---------------------------------------------------------------------------

func TestAPIWorkerRedactsCredentialMetadata(t *testing.T) {
	a := newAPIServer(t)
	store := NewInMemoryEvidenceStore()
	w := NewAPIWorker(store)
	w.Clock = newTickClock(2 * time.Millisecond)

	spec := `{"method":"POST","body":"{\"k\":1}","headers":{` +
		`"Authorization":"Bearer wire-secret",` +
		`"Cookie":"session=wire-secret",` +
		`"X-API-Key":"plain-key"}}`
	target := a.srv.URL + "/echo?token=query-secret"
	j := apiJourney([]JourneyStep{apiStep("api_call", target, spec)}, nil)

	run, err := w.Execute(context.Background(), j, apiEnv("127.0.0.1"), nil, RunOptions{})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if run.Status != RunCompleted {
		t.Fatalf("run status = %q, want %q: %s", run.Status, RunCompleted, run.Error)
	}

	net := eventsOf(run, EventNetwork)
	if len(net) != 1 {
		t.Fatalf("got %d network events, want 1", len(net))
	}
	meta := net[0].RedactedMeta
	if meta == nil {
		t.Fatal("network event carries no metadata")
	}
	// Credential-bearing headers are redacted in both directions…
	for _, key := range []string{"req_authorization", "req_cookie", "resp_set-cookie"} {
		if got := meta[key]; got != RedactedPlaceholder {
			t.Errorf("meta[%q] = %q, want %q", key, got, RedactedPlaceholder)
		}
	}
	// …non-credential headers pass through…
	if got := meta["req_x-api-key"]; got != "plain-key" {
		t.Errorf("meta[req_x-api-key] = %q, want plain pass-through", got)
	}
	// …the URL is sanitized (no query)…
	if got := meta["url"]; got != a.srv.URL+"/echo" {
		t.Errorf("meta[url] = %q, want %q (query dropped)", got, a.srv.URL+"/echo")
	}
	if got := meta["method"]; got != "POST" {
		t.Errorf("meta[method] = %q, want POST", got)
	}
	if got := meta["status"]; got != "200" {
		t.Errorf("meta[status] = %q, want 200", got)
	}
	// …and latency is the single injected clock tick (deterministic).
	if got := meta["latency_ms"]; got != "2" {
		t.Errorf("meta[latency_ms] = %q, want the one injected tick %q", got, "2")
	}
	// No secret material survives anywhere on the run record.
	blob, err := json.Marshal(run)
	if err != nil {
		t.Fatalf("marshal run: %v", err)
	}
	for _, secret := range []string{"wire-secret", "server-secret", "query-secret"} {
		if strings.Contains(string(blob), secret) {
			t.Errorf("run record leaks %q", secret)
		}
	}
	// The wire itself is untouched (redaction is metadata-only): the echo
	// proves the real Authorization header was still sent, and the response
	// body landed in the evidence store under the event's ref.
	ref := net[0].EvidenceRef
	if ref == "" {
		t.Fatal("network event carries no evidence ref")
	}
	body, ok, err := store.Get(context.Background(), ref)
	if err != nil || !ok {
		t.Fatalf("evidence ref %q missing (err=%v)", ref, err)
	}
	var echo map[string]string
	if err := json.Unmarshal(body, &echo); err != nil {
		t.Fatalf("evidence payload is not the response body: %v", err)
	}
	if echo["authorization"] != "Bearer wire-secret" {
		t.Errorf("server saw authorization %q, want the real wire header", echo["authorization"])
	}
	if got := run.EvidenceRefs; len(got) != 1 || got[0] != ref {
		t.Errorf("run.EvidenceRefs = %v, want [%s]", got, ref)
	}
}

// ---------------------------------------------------------------------------
// Deterministic assertions.
// ---------------------------------------------------------------------------

func TestAPIWorkerAssertionsPass(t *testing.T) {
	a := newAPIServer(t)
	w := NewAPIWorker(nil)
	w.Clock = newTickClock(2 * time.Millisecond)

	urlPattern := "^" + regexp.QuoteMeta(a.srv.URL+"/user") + "$"
	j := apiJourney(
		[]JourneyStep{
			apiStep("api_call", a.srv.URL+"/user", ""),
			apiStep("assert", string(AssertAPIStatus), ""),
			apiStep("assert", string(AssertLatencyBelow), ""),
		},
		[]Assertion{
			{Kind: AssertAPIStatus, Expected: "2xx"},
			{Kind: AssertLatencyBelow, Expected: "1000"},
			{Kind: AssertURLPattern, Expected: urlPattern},
		},
	)

	run, err := w.Execute(context.Background(), j, apiEnv("127.0.0.1"), nil, RunOptions{})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if run.Status != RunCompleted {
		t.Fatalf("run status = %q, want %q: %s", run.Status, RunCompleted, run.Error)
	}
	asserts := eventsOf(run, EventAssertion)
	if len(asserts) != 3 {
		t.Fatalf("got %d assertion events, want 3 (two inline + one epilogue)", len(asserts))
	}
	for i, ev := range asserts {
		if ev.Status != StepPassed {
			t.Errorf("assertion event %d status = %q (msg %q), want %q", i, ev.Status, ev.Message, StepPassed)
		}
	}
	// The two inline asserts consumed their journey assertions: the epilogue
	// must have evaluated only the unconsumed url_pattern.
	if !strings.Contains(asserts[2].Message, "url matches pattern") {
		t.Errorf("epilogue event message = %q, want the url_pattern assertion", asserts[2].Message)
	}
}

func TestAPIWorkerJSONSchemaAssertion(t *testing.T) {
	a := newAPIServer(t)
	w := NewAPIWorker(nil)
	w.Clock = newTickClock(2 * time.Millisecond)

	schema := `{"type":"object",` +
		`"required":["user"],` +
		`"properties":{"user":{"type":"object",` +
		`"required":["name","roles"],` +
		`"properties":{"name":{"type":"string"},` +
		`"n":{"type":"integer"},` +
		`"roles":{"type":"array","items":{"enum":["admin","viewer"]}}}}}}`

	t.Run("pass", func(t *testing.T) {
		j := apiJourney(
			[]JourneyStep{apiStep("api_call", a.srv.URL+"/user", "")},
			[]Assertion{{Kind: AssertJSONSchema, Expected: schema}},
		)
		run, err := w.Execute(context.Background(), j, apiEnv("127.0.0.1"), nil, RunOptions{})
		if err != nil {
			t.Fatalf("Execute returned error: %v", err)
		}
		if run.Status != RunCompleted {
			t.Fatalf("run status = %q, want %q: %s", run.Status, RunCompleted, run.Error)
		}
		asserts := eventsOf(run, EventAssertion)
		if len(asserts) != 1 || asserts[0].Status != StepPassed {
			t.Fatalf("assertion events = %+v, want a single passed json_schema", asserts)
		}
	})

	t.Run("fail_missing_required", func(t *testing.T) {
		broken := strings.Replace(schema, `"required":["user"]`, `"required":["user","session"]`, 1)
		j := apiJourney(
			[]JourneyStep{apiStep("api_call", a.srv.URL+"/user", "")},
			[]Assertion{{Kind: AssertJSONSchema, Expected: broken}},
		)
		run, err := w.Execute(context.Background(), j, apiEnv("127.0.0.1"), nil, RunOptions{})
		if err != nil {
			t.Fatalf("Execute returned error: %v", err)
		}
		if run.Status != RunFailed {
			t.Fatalf("run status = %q, want %q", run.Status, RunFailed)
		}
		if !strings.Contains(run.Error, "missing required property") {
			t.Errorf("run.Error = %q, want the missing-required violation", run.Error)
		}
	})

	t.Run("fail_enum_violation", func(t *testing.T) {
		j := apiJourney(
			[]JourneyStep{apiStep("api_call", a.srv.URL+"/user", "")},
			[]Assertion{{Kind: AssertJSONSchema, Expected: strings.Replace(schema, `["admin","viewer"]`, `["viewer"]`, 1)}},
		)
		run, err := w.Execute(context.Background(), j, apiEnv("127.0.0.1"), nil, RunOptions{})
		if err != nil {
			t.Fatalf("Execute returned error: %v", err)
		}
		if run.Status != RunFailed || !strings.Contains(run.Error, "not in enum") {
			t.Fatalf("run = (%q, %q), want an enum violation", run.Status, run.Error)
		}
	})
}

// Assertion failures fail the run fast: remaining steps are skipped and the
// failure detail names the violated expectation.
func TestAPIWorkerAssertionFailuresFailTheRun(t *testing.T) {
	a := newAPIServer(t)
	w := NewAPIWorker(nil)
	w.Clock = newTickClock(2 * time.Millisecond)

	cases := []struct {
		name      string
		kind      AssertionKind
		expected  string
		wantInErr string
	}{
		{"status_mismatch", AssertAPIStatus, "404", "status 200 does not match 404"},
		{"latency_bound_zero", AssertLatencyBelow, "0", "latency 2ms exceeds 0ms"},
		{"url_pattern_mismatch", AssertURLPattern, `^https://`, "does not match pattern"},
		{"invalid_schema_document", AssertJSONSchema, "{not json", "invalid schema document"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			j := apiJourney(
				[]JourneyStep{
					apiStep("api_call", a.srv.URL+"/user", ""),
					apiStep("assert", string(tc.kind), ""),
					apiStep("api_call", a.srv.URL+"/user", ""), // must be skipped after the failure
				},
				[]Assertion{{Kind: tc.kind, Expected: tc.expected}},
			)
			run, err := w.Execute(context.Background(), j, apiEnv("127.0.0.1"), nil, RunOptions{})
			if err != nil {
				t.Fatalf("Execute returned error: %v", err)
			}
			if run.Status != RunFailed {
				t.Fatalf("run status = %q, want %q", run.Status, RunFailed)
			}
			if !strings.Contains(run.Error, tc.wantInErr) {
				t.Errorf("run.Error = %q, want it to contain %q", run.Error, tc.wantInErr)
			}
			// api_call steps emit network events, assert steps assertion
			// events: the only ACTION event is the skipped trailing step.
			skips := eventsOf(run, EventAction)
			if len(skips) != 1 || skips[0].Status != StepSkipped {
				t.Errorf("action events = %+v, want one skipped event (fail-fast)", skips)
			}
		})
	}
}

// Skipped is a DISTINCT outcome: browser-only and unknown assertion kinds
// handed to the api worker are recorded as skipped and neither pass nor fail
// the run.
func TestAPIWorkerBrowserOnlyAssertionsSkipped(t *testing.T) {
	a := newAPIServer(t)
	w := NewAPIWorker(nil)
	w.Clock = newTickClock(2 * time.Millisecond)

	j := apiJourney(
		[]JourneyStep{apiStep("api_call", a.srv.URL+"/user", "")},
		[]Assertion{
			{Kind: AssertElementVisible, Expected: "#save"},
			{Kind: AssertionKind("totally_unknown"), Expected: "x"},
		},
	)
	run, err := w.Execute(context.Background(), j, apiEnv("127.0.0.1"), nil, RunOptions{})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if run.Status != RunCompleted {
		t.Fatalf("run status = %q, want %q (skipped is not a failure)", run.Status, RunCompleted)
	}
	asserts := eventsOf(run, EventAssertion)
	if len(asserts) != 2 {
		t.Fatalf("got %d assertion events, want 2", len(asserts))
	}
	for i, ev := range asserts {
		if ev.Status != StepSkipped {
			t.Errorf("assertion event %d status = %q, want %q (never a pass)", i, ev.Status, StepSkipped)
		}
	}
	if !strings.Contains(asserts[0].Message, "requires the browser worker") {
		t.Errorf("element_visible message = %q, want browser-worker routing", asserts[0].Message)
	}
}

// ---------------------------------------------------------------------------
// Lifecycle: cleanup, malformed targets, in-flight cap.
// ---------------------------------------------------------------------------

func TestAPIWorkerCleanupBestEffort(t *testing.T) {
	a := newAPIServer(t)
	w := NewAPIWorker(nil)
	w.Clock = newTickClock(2 * time.Millisecond)

	t.Run("runs_after_steps", func(t *testing.T) {
		j := apiJourney([]JourneyStep{apiStep("api_call", a.srv.URL+"/user", "")}, nil)
		j.Cleanup = []JourneyStep{apiStep("api_call", a.srv.URL+"/cleanup", "")}

		run, err := w.Execute(context.Background(), j, apiEnv("127.0.0.1"), nil, RunOptions{})
		if err != nil {
			t.Fatalf("Execute returned error: %v", err)
		}
		if run.Status != RunCompleted {
			t.Fatalf("run status = %q, want %q: %s", run.Status, RunCompleted, run.Error)
		}
		if hits := a.cleanupHits(); hits != 1 {
			t.Errorf("server saw %d cleanup hits, want 1", hits)
		}
		cleanup := eventsAtStep(run, -1)
		if len(cleanup) == 0 || cleanup[0].Status != StepPassed {
			t.Errorf("cleanup events = %+v, want a passed event at the negative step index", cleanup)
		}
	})

	t.Run("write_denied_in_readonly", func(t *testing.T) {
		j := apiJourney([]JourneyStep{apiStep("api_call", a.srv.URL+"/user", "")}, nil)
		cleanup := apiStep("api_call", a.srv.URL+"/cleanup", "")
		cleanup.WriteOp = true // state-changing cleanup: denied in read-only mode
		j.Cleanup = []JourneyStep{cleanup}

		env := apiEnv("127.0.0.1")
		env.Mode = ModeProductionReadOnly
		before := a.cleanupHits()
		run, err := w.Execute(context.Background(), j, env, nil, RunOptions{})
		if err != nil {
			t.Fatalf("Execute returned error: %v", err)
		}
		if run.Status != RunCompleted {
			t.Fatalf("run status = %q, want %q (cleanup never changes the outcome)", run.Status, RunCompleted)
		}
		if hits := a.cleanupHits() - before; hits != 0 {
			t.Errorf("server saw %d cleanup hits in read-only mode, want 0", hits)
		}
		var blocked bool
		for _, ev := range eventsAtStep(run, -1) {
			if ev.Status == StepBlocked && strings.Contains(ev.Message, ErrReadOnlyMode.Error()) {
				blocked = true
			}
		}
		if !blocked {
			t.Error("read-only cleanup produced no blocked event")
		}
	})
}

func TestAPIWorkerMalformedTargetFails(t *testing.T) {
	w := NewAPIWorker(nil)
	w.Clock = newTickClock(2 * time.Millisecond)

	j := apiJourney([]JourneyStep{apiStep("api_call", "not-a-url", "")}, nil)
	run, err := w.Execute(context.Background(), j, apiEnv(), nil, RunOptions{})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if run.Status != RunFailed {
		t.Fatalf("run status = %q, want %q", run.Status, RunFailed)
	}
	net := eventsOf(run, EventNetwork)
	if len(net) != 1 || net[0].Status != StepFailed {
		t.Fatalf("network events = %+v, want one failed event", net)
	}
	if !strings.Contains(net[0].Message, "not a valid http(s) URL") {
		t.Errorf("event message = %q, want the URL validation failure", net[0].Message)
	}
}

func TestAPIWorkerInFlightCap(t *testing.T) {
	var once sync.Once
	reached := make(chan struct{})
	gate := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(reached) })
		<-gate // hold the first run in flight until the test releases it
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	w := NewAPIWorker(nil)
	w.Clock = newTickClock(2 * time.Millisecond)
	w.MaxInFlight = 1
	env := apiEnv("127.0.0.1")

	j := apiJourney([]JourneyStep{apiStep("api_call", srv.URL+"/hold", "")}, nil)
	done := make(chan *Run, 1)
	go func() {
		run, err := w.Execute(context.Background(), j, env, nil, RunOptions{})
		if err != nil {
			t.Errorf("first Execute returned error: %v", err)
			done <- nil
			return
		}
		done <- run
	}()

	// Deterministic handshake: wait until the first run is actually inside
	// the request handler (its in-flight slot is held), then assert the cap.
	select {
	case <-reached:
	case <-time.After(2 * time.Second):
		t.Fatal("first run never reached the server")
	}
	if _, err := w.Execute(context.Background(), j, env, nil, RunOptions{}); err != ErrWorkerBusy {
		t.Fatalf("second Execute error = %v, want %v", err, ErrWorkerBusy)
	}

	close(gate)
	select {
	case run := <-done:
		if run == nil || run.Status != RunCompleted {
			t.Fatalf("first run = %+v, want completed", run)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first run never completed after the gate opened")
	}
}

// Compile-time contract checks.
var (
	_ Worker      = (*APIWorker)(nil)
	_ InFlightCap = (*APIWorker)(nil)
)
