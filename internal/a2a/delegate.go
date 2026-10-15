package a2a

// delegate.go — the OUTBOUND side of the A2A surface (issue #136):
// DelegateToAgentCard resolves a target Agent Card (a REGISTERED card of
// the caller's org, or a card fetched live from a caller-supplied URL) and
// POSTs the message envelope to the card's endpoint with a configurable
// timeout.
//
// GOVERNANCE: before any bytes hit the wire the wired DelegationGate is
// evaluated. A nil gate is an explicit contract: ALLOW with the recorded
// verdict "allowed_no_gate". A denying gate aborts the delegation BEFORE
// the HTTP call with ErrDelegationDenied (the HTTP layer renders the
// policy-style 403 delegation_denied verdict).
//
// AUDIT (the mcp.tool_call precedent): delegation outcomes are audited by
// this service through the Auditor seam — a2a.delegated on any completed
// wire outcome, a2a.delegation_denied on a gate refusal. Message payloads
// are NEVER logged; metadata carries card/endpoint/verdict/result only.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Delegation defaults and limits.
const (
	// DefaultDelegationTimeout bounds the single outbound POST.
	DefaultDelegationTimeout = 10 * time.Second
	// maxDelegateResponseBody caps how much of the remote response is read
	// (remote endpoints are untrusted; keeps memory bounded).
	maxDelegateResponseBody = 1 << 20
)

// Gate verdict strings (recorded in audit metadata and rendered to the
// caller).
const (
	VerdictAllowedNoGate = "allowed_no_gate"
	VerdictAllowed       = "allowed"
	VerdictDenied        = "denied"
)

// DelegationGate is the policy hook evaluated before every outbound
// delegation. Return Allowed=false to refuse.
type DelegationGate interface {
	EvaluateDelegation(ctx context.Context, orgID string, card *AgentCard, message string) DelegationVerdict
}

// DelegationGateFunc adapts a plain function to the DelegationGate seam
// (the connectors.SecretResolverFunc precedent).
type DelegationGateFunc func(ctx context.Context, orgID string, card *AgentCard, message string) DelegationVerdict

// EvaluateDelegation satisfies DelegationGate.
func (f DelegationGateFunc) EvaluateDelegation(ctx context.Context, orgID string, card *AgentCard, message string) DelegationVerdict {
	return f(ctx, orgID, card, message)
}

// DelegationVerdict is the gate outcome.
type DelegationVerdict struct {
	Allowed bool
	// Decision is the machine-readable verdict: VerdictAllowed,
	// VerdictAllowedNoGate or VerdictDenied.
	Decision string
	// Reason is the operator-facing policy explanation (never a payload).
	Reason string
}

// DelegationRequest selects the target card and carries the message.
type DelegationRequest struct {
	// CardID resolves a REGISTERED card (org-scoped; unknown -> ErrNotFound).
	CardID string
	// CardURL fetches the card live from a well-known URL (used when the
	// remote card is not registered here). Exactly one of CardID/CardURL
	// should be set; CardID wins when both are present.
	CardURL string
	Message string
	// Actor is the authenticated caller (audit actor only — never logged
	// with payload material).
	Actor string
}

// DelegationResult reports the completed (or refused) delegation outcome.
type DelegationResult struct {
	CardID     string
	Endpoint   string
	Status     string // StatusSubmitted (2xx) | StatusFailed (non-2xx)
	HTTPStatus int
	LatencyMS  int64
	Verdict    string
}

// DelegationDeniedError carries the gate verdict on the denial path so the
// HTTP layer can render the policy-style verdict without re-deriving it.
type DelegationDeniedError struct {
	Verdict DelegationVerdict
}

// Error implements the error interface.
func (e *DelegationDeniedError) Error() string {
	if e.Verdict.Reason != "" {
		return "delegation denied by policy: " + e.Verdict.Reason
	}
	return "delegation denied by policy"
}

// Unwrap makes errors.Is(err, ErrDelegationDenied) work.
func (e *DelegationDeniedError) Unwrap() error {
	return ErrDelegationDenied
}

// ErrDelegationDenied is the sentinel for a gate-refused delegation.
var ErrDelegationDenied = errors.New("delegation denied by policy")

// ErrDelegationUnreachable is the sentinel for a transport-level failure
// (endpoint down, timeout, TLS error).
var ErrDelegationUnreachable = errors.New("delegation endpoint unreachable")

// MessageEnvelope is the A2A-style payload POSTed to the target card
// endpoint: role + typed text parts (+ kind), the minimal shape a generic
// A2A server expects.
type MessageEnvelope struct {
	Role     string         `json:"role"`
	Parts    []MessagePart  `json:"parts"`
	Kind     string         `json:"kind"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// MessagePart is one typed text part of the envelope.
type MessagePart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// newMessageEnvelope builds the outbound envelope for one text message.
func newMessageEnvelope(message string) MessageEnvelope {
	return MessageEnvelope{
		Role: "user",
		Kind: "message",
		Parts: []MessagePart{
			{Type: "text", Text: message},
		},
		Metadata: map[string]any{"source": "agentos-a2a"},
	}
}

// defaultDelegationClient returns the stdlib client used for card fetches
// and delegation POSTs (tests replace it via SetHTTPClient).
func defaultDelegationClient() *http.Client {
	return &http.Client{Timeout: DefaultDelegationTimeout}
}

// SetHTTPClient replaces the client used by card fetches and delegation
// POSTs (tests inject clients against httptest servers). Nil-safe.
func (s *Service) SetHTTPClient(c *http.Client) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if c == nil {
		c = defaultDelegationClient()
	}
	s.httpClient = c
}

// SetDelegationTimeout bounds the delegation POST (tests shrink it).
// <= 0 restores the default. Nil-safe.
func (s *Service) SetDelegationTimeout(d time.Duration) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if d <= 0 {
		d = DefaultDelegationTimeout
	}
	s.timeout = d
}

// DelegateToAgentCard resolves the target card, evaluates the gate, and
// POSTs the message envelope to the card endpoint. Outcomes:
//
//   - gate denied        -> *DelegationDeniedError (wraps ErrDelegationDenied);
//     the refusal is audited (a2a.delegation_denied) and NO wire call is made.
//   - transport failure  -> error wrapping ErrDelegationUnreachable.
//   - remote answered    -> *DelegationResult with Status submitted (2xx) or
//     failed (non-2xx); the outcome is the payload (no error), audited as
//     a2a.delegated.
func (s *Service) DelegateToAgentCard(ctx context.Context, orgID string, req DelegationRequest) (*DelegationResult, error) {
	if orgID == "" {
		return nil, ErrOrgRequired
	}
	if strings.TrimSpace(req.Message) == "" {
		return nil, ErrMessageRequired
	}
	card, err := s.resolveDelegationCard(ctx, orgID, req)
	if err != nil {
		return nil, err
	}
	if card.EndpointURL == "" {
		return nil, ErrEndpointRequired
	}

	verdict := s.evaluateGate(ctx, orgID, card, req.Message)
	if !verdict.Allowed {
		s.auditDelegation(ctx, req.Actor, orgID, card, verdict, nil, 0, true)
		return nil, &DelegationDeniedError{Verdict: verdict}
	}

	result, err := s.postMessage(ctx, card, req.Message)
	if err != nil {
		return nil, err
	}
	result.Verdict = verdict.Decision
	result.CardID = card.ID
	s.auditDelegation(ctx, req.Actor, orgID, card, verdict, result, result.HTTPStatus, false)
	return result, nil
}

// resolveDelegationCard resolves a registered card (org-scoped) or fetches
// one live from a URL.
func (s *Service) resolveDelegationCard(ctx context.Context, orgID string, req DelegationRequest) (*AgentCard, error) {
	if req.CardID != "" {
		return s.GetCard(ctx, orgID, req.CardID)
	}
	if req.CardURL != "" {
		return fetchCard(ctx, s.httpClient, s.timeout, req.CardURL)
	}
	return nil, ErrCardTargetRequired
}

// evaluateGate applies the wired gate; a nil gate allows with the recorded
// "allowed_no_gate" verdict (issue #136 contract).
func (s *Service) evaluateGate(ctx context.Context, orgID string, card *AgentCard, message string) DelegationVerdict {
	s.mu.RLock()
	gate := s.gate
	s.mu.RUnlock()
	if gate == nil {
		return DelegationVerdict{Allowed: true, Decision: VerdictAllowedNoGate, Reason: "no delegation gate wired"}
	}
	verdict := gate.EvaluateDelegation(ctx, orgID, card, message)
	if !verdict.Allowed {
		verdict.Decision = VerdictDenied
		return verdict
	}
	if verdict.Decision == "" || verdict.Decision == VerdictAllowedNoGate {
		verdict.Decision = VerdictAllowed
	}
	return verdict
}

// postMessage executes the outbound POST and measures the latency.
func (s *Service) postMessage(ctx context.Context, card *AgentCard, message string) (*DelegationResult, error) {
	s.mu.RLock()
	client := s.httpClient
	timeout := s.timeout
	s.mu.RUnlock()
	if client == nil {
		client = defaultDelegationClient()
	}

	envelope := newMessageEnvelope(message)
	body, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDelegationUnreachable, err)
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	httpReq, err := http.NewRequestWithContext(callCtx, http.MethodPost, card.EndpointURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDelegationUnreachable, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(httpReq)
	latency := time.Since(start)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDelegationUnreachable, err)
	}
	defer resp.Body.Close()
	// Drain (capped) so the connection is reusable; the body content is
	// deliberately not interpreted in v1.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDelegateResponseBody))

	status := StatusSubmitted
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		status = StatusFailed
	}
	return &DelegationResult{
		Endpoint:   card.EndpointURL,
		Status:     status,
		HTTPStatus: resp.StatusCode,
		LatencyMS:  latency.Milliseconds(),
	}, nil
}

// auditDelegation writes the best-effort delegation audit row through the
// Auditor seam. Metadata carries card/endpoint/verdict/result ONLY — the
// message payload never enters the audit trail.
func (s *Service) auditDelegation(ctx context.Context, actor, orgID string, card *AgentCard, verdict DelegationVerdict, result *DelegationResult, httpStatus int, denied bool) {
	s.mu.RLock()
	auditor := s.auditor
	s.mu.RUnlock()
	if auditor == nil {
		return
	}
	action := "a2a.delegated"
	metadata := map[string]any{
		"card_id":     card.ID,
		"card_name":   card.Name,
		"endpoint":    card.EndpointURL,
		"verdict":     verdict.Decision,
		"http_status": httpStatus,
	}
	if denied {
		action = "a2a.delegation_denied"
		metadata["reason"] = verdict.Reason
	}
	if result != nil {
		metadata["status"] = result.Status
		metadata["latency_ms"] = result.LatencyMS
	}
	resource := "a2a_cards/" + card.ID
	if card.ID == "" {
		// URL-resolved (unregistered) card: the endpoint is the only stable
		// reference (same topology-not-secret rationale as MCP registration
		// audits).
		resource = "a2a_delegate"
	}
	_, _ = auditor.LogCtx(ctx, actor, action, orgID, resource, metadata)
}

// canonicalCard mirrors the wire shape ToJSON renders, so a card fetched
// from a URL parses back into the model.
type canonicalCard struct {
	Name               string         `json:"name"`
	Description        string         `json:"description"`
	Version            string         `json:"version"`
	Provider           *CardProvider  `json:"provider"`
	URL                string         `json:"url"`
	Capabilities       *Capabilities  `json:"capabilities"`
	Skills             []Skill        `json:"skills"`
	Authentication     *canonicalAuth `json:"authentication"`
	PreferredTransport string         `json:"preferredTransport"`
}

// canonicalAuth is the authentication block of a canonical card.
type canonicalAuth struct {
	Schemes []string `json:"schemes"`
}

// fetchCard GETs a canonical Agent Card from a well-known URL and parses it
// into the model. The fetch is bounded by the delegation timeout and the
// response body by maxDelegateResponseBody.
func fetchCard(ctx context.Context, client *http.Client, timeout time.Duration, cardURL string) (*AgentCard, error) {
	if client == nil {
		client = defaultDelegationClient()
	}
	fetchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, cardURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCardFetchFailed, err)
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCardFetchFailed, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("%w: http status %d", ErrCardFetchFailed, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxDelegateResponseBody))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCardFetchFailed, err)
	}
	var cc canonicalCard
	if err := json.Unmarshal(raw, &cc); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCardInvalid, err)
	}
	// Minimal validity: a card is only usable when it names itself, versions
	// itself and carries the endpoint messages go to.
	if cc.Name == "" || cc.Version == "" || cc.URL == "" {
		return nil, ErrCardInvalid
	}
	card := &AgentCard{
		Name:               cc.Name,
		Description:        cc.Description,
		Version:            cc.Version,
		EndpointURL:        cc.URL,
		PreferredTransport: cc.PreferredTransport,
		Skills:             normalizeSkills(cc.Skills),
	}
	if cc.Provider != nil {
		card.Provider = *cc.Provider
	}
	if cc.Capabilities != nil {
		card.Capabilities = *cc.Capabilities
	}
	if cc.Authentication != nil && len(cc.Authentication.Schemes) > 0 {
		card.AuthScheme = cc.Authentication.Schemes[0]
	}
	return card, nil
}
