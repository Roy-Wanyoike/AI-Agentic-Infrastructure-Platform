package a2a

// delegate_test.go — the outbound delegation contract: allow path (envelope
// on the wire, result fields, audit metadata without payloads), the gate
// denial path (no wire call, 403-style typed error, audited refusal), the
// nil-gate "allowed_no_gate" contract, URL-fetched cards, transport
// failures and validation.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingTarget is the remote A2A endpoint under test: it records the
// requests it receives and answers 200 with a tiny JSON ack.
type recordingTarget struct {
	srv *httptest.Server

	mu     sync.Mutex
	bodies []string
	paths  []string
	status int // response status to answer (default 200)
}

func newRecordingTarget() *recordingTarget {
	t := &recordingTarget{status: http.StatusOK}
	t.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		t.mu.Lock()
		t.bodies = append(t.bodies, string(raw))
		t.paths = append(t.paths, r.URL.Path)
		status := t.status
		t.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	return t
}

func (t *recordingTarget) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.bodies)
}

func (t *recordingTarget) lastBody() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.bodies) == 0 {
		return ""
	}
	return t.bodies[len(t.bodies)-1]
}

func (t *recordingTarget) close() { t.srv.Close() }

func delegateService(t *testing.T, endpoint string) (*Service, *stubAuditor, *AgentCard) {
	t.Helper()
	svc := NewService()
	svc.SetAgentChecker(newStubAgentChecker("org-1/agent-1"))
	auditor := &stubAuditor{}
	svc.SetAuditor(auditor)
	input := validInput("agent-1")
	input.EndpointURL = endpoint
	card, err := svc.CreateCard(context.Background(), "org-1", input)
	if err != nil {
		t.Fatalf("create card: %v", err)
	}
	return svc, auditor, card
}

func TestDelegateToAgentCardAllowPath(t *testing.T) {
	target := newRecordingTarget()
	defer target.close()
	svc, auditor, card := delegateService(t, target.srv.URL)

	message := "delegate-secret-payload-7"
	result, err := svc.DelegateToAgentCard(context.Background(), "org-1", DelegationRequest{
		CardID:  card.ID,
		Message: message,
		Actor:   "user-1",
	})
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	if result.Status != StatusSubmitted || result.HTTPStatus != http.StatusOK {
		t.Fatalf("result wrong: %+v", result)
	}
	if result.LatencyMS < 0 {
		t.Fatalf("latency must never be negative: %d", result.LatencyMS)
	}
	if result.Verdict != VerdictAllowedNoGate {
		t.Fatalf("nil gate must record %q, got %q", VerdictAllowedNoGate, result.Verdict)
	}
	if result.CardID != card.ID || result.Endpoint != target.srv.URL {
		t.Fatalf("result refs wrong: %+v", result)
	}

	// The wire carried the A2A-style envelope.
	if target.count() != 1 {
		t.Fatalf("expected exactly one POST, got %d", target.count())
	}
	var env MessageEnvelope
	if err := json.Unmarshal([]byte(target.lastBody()), &env); err != nil {
		t.Fatalf("envelope unmarshal: %v (body=%s)", err, target.lastBody())
	}
	if env.Role != "user" || env.Kind != "message" {
		t.Fatalf("envelope role/kind wrong: %+v", env)
	}
	if len(env.Parts) != 1 || env.Parts[0].Type != "text" || env.Parts[0].Text != message {
		t.Fatalf("envelope parts wrong: %+v", env.Parts)
	}

	// Audit: a2a.delegated with metadata ONLY (no message payload anywhere).
	actions := auditor.actions()
	if actions["a2a.delegated"] != 1 {
		t.Fatalf("audit actions wrong: %v", actions)
	}
	if auditor.contains(message) || auditor.contains("delegate-secret-payload") {
		t.Fatal("the audit trail must never carry the message payload")
	}
}

func TestDelegateGateDeniesBeforeTheWire(t *testing.T) {
	target := newRecordingTarget()
	defer target.close()
	svc, auditor, card := delegateService(t, target.srv.URL)
	gate := &stubGate{verdict: DelegationVerdict{Allowed: false, Reason: "card not on the delegation allowlist"}}
	svc.SetDelegationGate(gate)

	_, err := svc.DelegateToAgentCard(context.Background(), "org-1", DelegationRequest{
		CardID:  card.ID,
		Message: "should never fly",
		Actor:   "user-1",
	})
	var denied *DelegationDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("denial must be *DelegationDeniedError, got %v", err)
	}
	if !errors.Is(err, ErrDelegationDenied) {
		t.Fatalf("denial must wrap ErrDelegationDenied, got %v", err)
	}
	if denied.Verdict.Reason != "card not on the delegation allowlist" {
		t.Fatalf("verdict reason wrong: %+v", denied.Verdict)
	}
	if denied.Verdict.Decision != VerdictDenied {
		t.Fatalf("denied verdict decision = %q", denied.Verdict.Decision)
	}
	// NOTHING hit the wire.
	if target.count() != 0 {
		t.Fatalf("a denied delegation must not POST, got %d calls", target.count())
	}
	// The refusal is audited; the success event is not.
	actions := auditor.actions()
	if actions["a2a.delegation_denied"] != 1 || actions["a2a.delegated"] != 0 {
		t.Fatalf("audit actions wrong: %v", actions)
	}
	if auditor.contains("should never fly") {
		t.Fatal("denial audit must not carry the message payload")
	}
}

func TestDelegateGateAllowsWithRecordedVerdict(t *testing.T) {
	target := newRecordingTarget()
	defer target.close()
	svc, auditor, card := delegateService(t, target.srv.URL)
	svc.SetDelegationGate(&stubGate{verdict: DelegationVerdict{Allowed: true, Decision: "allowed_by_policy", Reason: "on allowlist"}})

	result, err := svc.DelegateToAgentCard(context.Background(), "org-1", DelegationRequest{
		CardID: card.ID, Message: "go", Actor: "user-1",
	})
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	// An allowing gate keeps its own decision string; the default fills in
	// as "allowed".
	if result.Verdict == "" || result.Verdict == VerdictDenied {
		t.Fatalf("allowing gate verdict must be preserved, got %q", result.Verdict)
	}
	if auditor.actions()["a2a.delegated"] != 1 {
		t.Fatalf("audit actions wrong: %v", auditor.actions())
	}
}

func TestDelegateByCardURL(t *testing.T) {
	// One httptest server plays BOTH sides: GET /card serves the canonical
	// Agent Card JSON; POST /run accepts the delegation.
	var mu sync.Mutex
	postBodies := []string{}
	cardJSON := `{
	  "name": "Remote Agent",
	  "description": "Lives somewhere else",
	  "version": "2.0.0",
	  "provider": {"organization": "RemoteCo", "url": "https://remote.example"},
	  "url": "PLACEHOLDER",
	  "capabilities": {"streaming": true, "pushNotifications": false, "stateTransitionHistory": false},
	  "skills": [{"id": "echo", "name": "Echo", "description": "Echoes", "tags": ["demo"]}],
	  "authentication": {"schemes": ["oauth2"]},
	  "preferredTransport": "JSONRPC"
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/card":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(strings.ReplaceAll(cardJSON, "PLACEHOLDER", "http://"+r.Host+"/run")))
		case r.Method == http.MethodPost && r.URL.Path == "/run":
			raw, _ := io.ReadAll(r.Body)
			mu.Lock()
			postBodies = append(postBodies, string(raw))
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	svc, auditor := NewService(), &stubAuditor{}
	svc.SetAuditor(auditor)
	result, err := svc.DelegateToAgentCard(context.Background(), "org-1", DelegationRequest{
		CardURL: srv.URL + "/card",
		Message: "hello remote",
		Actor:   "user-1",
	})
	if err != nil {
		t.Fatalf("delegate by URL: %v", err)
	}
	if result.Status != StatusSubmitted || result.CardID != "" {
		t.Fatalf("unregistered card result wrong: %+v", result)
	}
	if result.Endpoint != srv.URL+"/run" {
		t.Fatalf("the POST must target the card's url field, got %q", result.Endpoint)
	}
	mu.Lock()
	if len(postBodies) != 1 || !strings.Contains(postBodies[0], `"text":"hello remote"`) {
		t.Fatalf("remote POST bodies wrong: %v", postBodies)
	}
	mu.Unlock()
	// The audit row references the remote delegation without an internal id.
	actions := auditor.actions()
	if actions["a2a.delegated"] != 1 {
		t.Fatalf("audit actions wrong: %v", actions)
	}
}

func TestDelegateTransportFailure(t *testing.T) {
	svc, _, card := delegateService(t, "http://127.0.0.1:1/run") // nothing listens
	_, err := svc.DelegateToAgentCard(context.Background(), "org-1", DelegationRequest{
		CardID: card.ID, Message: "hi", Actor: "user-1",
	})
	if !errors.Is(err, ErrDelegationUnreachable) {
		t.Fatalf("expected ErrDelegationUnreachable, got %v", err)
	}
}

func TestDelegateRemoteFailureIsThePayload(t *testing.T) {
	target := newRecordingTarget()
	target.mu.Lock()
	target.status = http.StatusInternalServerError
	target.mu.Unlock()
	defer target.close()
	svc, auditor, card := delegateService(t, target.srv.URL)

	result, err := svc.DelegateToAgentCard(context.Background(), "org-1", DelegationRequest{
		CardID: card.ID, Message: "hi", Actor: "user-1",
	})
	if err != nil {
		t.Fatalf("a non-2xx remote answer is the outcome, not an error: %v", err)
	}
	if result.Status != StatusFailed || result.HTTPStatus != http.StatusInternalServerError {
		t.Fatalf("failed-delegation result wrong: %+v", result)
	}
	if auditor.actions()["a2a.delegated"] != 1 {
		t.Fatalf("the failed wire outcome is still audited: %v", auditor.actions())
	}
}

func TestDelegateValidation(t *testing.T) {
	target := newRecordingTarget()
	defer target.close()
	svc, _, card := delegateService(t, target.srv.URL)
	ctx := context.Background()

	tests := []struct {
		name    string
		orgID   string
		req     DelegationRequest
		wantErr error
	}{
		{"missing org", "", DelegationRequest{CardID: card.ID, Message: "m"}, ErrOrgRequired},
		{"missing message", "org-1", DelegationRequest{CardID: card.ID, Message: "  "}, ErrMessageRequired},
		{"no target", "org-1", DelegationRequest{Message: "m"}, ErrCardTargetRequired},
		{"unknown card", "org-1", DelegationRequest{CardID: "nope", Message: "m"}, ErrNotFound},
		{"cross-org card", "org-2", DelegationRequest{CardID: card.ID, Message: "m"}, ErrNotFound},
		{"unfetchable card_url", "org-1", DelegationRequest{CardURL: "http://127.0.0.1:1/card", Message: "m"}, ErrCardFetchFailed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.DelegateToAgentCard(ctx, tc.orgID, tc.req); !errors.Is(err, tc.wantErr) {
				t.Fatalf("expected %v, got %v", tc.wantErr, err)
			}
		})
	}

	// A registered card without an endpoint is refused at delegation time
	// (the registry itself accepts it).
	endpointless := NewService()
	endpointless.SetAgentChecker(newStubAgentChecker("org-1/agent-1"))
	input := validInput("agent-1")
	input.EndpointURL = ""
	noURL, err := endpointless.CreateCard(ctx, "org-1", input)
	if err != nil {
		t.Fatalf("create endpointless card: %v", err)
	}
	if _, err := endpointless.DelegateToAgentCard(ctx, "org-1", DelegationRequest{CardID: noURL.ID, Message: "m"}); !errors.Is(err, ErrEndpointRequired) {
		t.Fatalf("expected ErrEndpointRequired, got %v", err)
	}

	// A fetched payload that is not a usable card -> ErrCardInvalid.
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"name":"only-a-name"}`))
	}))
	defer badSrv.Close()
	if _, err := svc.DelegateToAgentCard(ctx, "org-1", DelegationRequest{CardURL: badSrv.URL, Message: "m"}); !errors.Is(err, ErrCardInvalid) {
		t.Fatalf("expected ErrCardInvalid, got %v", err)
	}
}

func TestMessageEnvelopeShape(t *testing.T) {
	env := newMessageEnvelope("ping")
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"role", "parts", "kind"} {
		if _, ok := generic[key]; !ok {
			t.Errorf("envelope must carry %q: %s", key, raw)
		}
	}
	parts := generic["parts"].([]any)
	first := parts[0].(map[string]any)
	if first["type"] != "text" || first["text"] != "ping" {
		t.Errorf("part wrong: %v", first)
	}
}

func TestDelegationTimeoutIsConfigurable(t *testing.T) {
	svc := NewService()
	if svc.timeout != DefaultDelegationTimeout {
		t.Fatalf("default timeout = %v", svc.timeout)
	}
	svc.SetDelegationTimeout(50 * time.Millisecond)
	if svc.timeout != 50*time.Millisecond {
		t.Fatalf("configured timeout = %v", svc.timeout)
	}
	// <= 0 restores the default.
	svc.SetDelegationTimeout(0)
	if svc.timeout != DefaultDelegationTimeout {
		t.Fatalf("zero timeout must restore the default, got %v", svc.timeout)
	}
}
