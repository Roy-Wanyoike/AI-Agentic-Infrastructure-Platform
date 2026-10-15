package tools

// webhooktool.go — the outbound webhook tool (issue #111; file name avoids
// the plural internal/webhooks package). It delivers POST/GET requests to an
// OPERATOR-ALLOW-LISTED URL, optionally signed with HMAC-SHA256.
//
// Security model:
//
//   - Allow-list, default deny. Targets match an entry when scheme, host and
//     port are equal and the path is the entry path or a descendant of it
//     (an entry path of "/" or "" allows every path on that origin). An
//     empty allow-list refuses every delivery — wiring one is mandatory.
//   - SSRF protection mirrors the http_request tool: http/https only, the
//     host is resolved up-front and every resolved IP is checked, and a
//     dial-level Control hook re-validates the concrete peer IP (covering
//     redirects and DNS rebinding). Loopback/private/link-local (including
//     the cloud metadata endpoint 169.254.169.254), CGNAT, ULA and
//     multicast destinations are refused.
//   - HMAC-SHA256 signature via the secrets seam: the narrow SecretResolver
//     interface is satisfied STRUCTURALLY by the platform secrets service
//     (Resolve(ctx, orgID, name) — internal/secrets imported read-only), so
//     the signing key lives in the encrypted secret store and is fetched per
//     delivery. The signature covers "{timestamp}.{body}" and is sent as
//     X-AgentOS-Signature: sha256=<hex> next to X-AgentOS-Timestamp (unix
//     seconds). Receivers MUST reject timestamps outside the replay window
//     (WebhookReplayWindow, 5 minutes) — the check is on the verifier side,
//     which is where the window belongs.
//   - Response cap (default 32 KiB, truncated), per-call timeout (default
//     10s), fire-and-forget mode that skips the response body but stays
//     synchronous (no goroutines, full context propagation).
//   - Tenant scoping: the organization comes from the run scope context
//     (policies.WithRunScope) and scopes both the signing secret and the
//     audit surface; tool input can never choose a tenant.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"agentos/internal/policies"
)

// WebhookToolName is the registry name of the webhook tool.
const WebhookToolName = "webhook"

// Webhook defaults (issue #111).
const (
	// DefaultWebhookMaxResponseBody caps how much of the receiver's response
	// is captured.
	DefaultWebhookMaxResponseBody = 32 << 10
	// DefaultWebhookTimeout bounds one delivery.
	DefaultWebhookTimeout = 10 * time.Second
	// MaxWebhookTimeout is the hard timeout ceiling.
	MaxWebhookTimeout = 60 * time.Second
	// WebhookReplayWindow is the freshness window receivers should enforce on
	// X-AgentOS-Timestamp before verifying the signature.
	WebhookReplayWindow = 5 * time.Minute
	// WebhookSignatureHeader and WebhookTimestampHeader are the wire names.
	WebhookSignatureHeader = "X-AgentOS-Signature"
	WebhookTimestampHeader = "X-AgentOS-Timestamp"
	// WebhookSignaturePrefix is the algorithm tag of the signature header.
	WebhookSignaturePrefix = "sha256="
)

// Webhook tool errors.
var (
	// ErrWebhookNotAllowed is returned when the target is not on the
	// configured allow-list.
	ErrWebhookNotAllowed = errors.New("tools: webhook target is not on the allow-list")
)

// SecretResolver resolves a secret NAME into its plaintext value at delivery
// time. It is satisfied structurally by the platform secrets service
// (*secrets.Service), mirroring the connectors.SecretResolver pattern.
type WebhookSecretResolver interface {
	Resolve(ctx context.Context, orgID, name string) (string, error)
}

// WebhookTarget is one allow-list entry.
type WebhookTarget struct {
	scheme string
	host   string // host or host:port (lower-cased)
	path   string // "" or "/" allow every path on the origin
}

// WebhookTool delivers allow-listed, optionally signed webhooks.
type WebhookTool struct {
	allowed         []WebhookTarget
	signer          WebhookSecretResolver
	signingKeyRef   string
	client          *http.Client
	allowPrivate    bool
	resolver        netResolver
	maxResponseBody int
	gate            *ToolPolicyGate
}

// netResolver is the subset of net.Resolver the SSRF pre-check needs
// (narrowed so tests can inject deterministic resolution).
type netResolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// NewWebhookTool returns the tool over the parsed allow-list. signer and
// signingKeyRef may be empty: deliveries are then sent unsigned (signed=false
// in the output) — production wiring SHOULD configure both.
func NewWebhookTool(allowedURLs []string, signer WebhookSecretResolver, signingKeyRef string) (*WebhookTool, error) {
	targets := make([]WebhookTarget, 0, len(allowedURLs))
	for _, raw := range allowedURLs {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("tools: invalid webhook allow-list entry %q", raw)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return nil, fmt.Errorf("tools: webhook allow-list entry %q must be http(s)", raw)
		}
		targets = append(targets, WebhookTarget{
			scheme: u.Scheme,
			host:   strings.ToLower(u.Host),
			path:   strings.TrimSuffix(u.Path, "/"),
		})
	}
	tool := &WebhookTool{
		allowed:         targets,
		signer:          signer,
		signingKeyRef:   signingKeyRef,
		maxResponseBody: DefaultWebhookMaxResponseBody,
	}
	tool.rebuildClient()
	return tool, nil
}

// WithAllowPrivate disables SSRF protection (tests / trusted environments).
// The transport is rebuilt so the dial-level Control hook honors the current
// flag instead of one captured at construction time.
func (t *WebhookTool) WithAllowPrivate(allow bool) *WebhookTool {
	t.allowPrivate = allow
	t.rebuildClient()
	return t
}

// rebuildClient constructs the delivery client: proxy disabled on purpose
// (same rationale as http_request) with the dial-level SSRF Control hook
// reflecting the current allowPrivate flag.
func (t *WebhookTool) rebuildClient() {
	dialer := &net.Dialer{
		Timeout: 5 * time.Second,
		Control: ssrfDialControl(t.allowPrivate),
	}
	t.client = &http.Client{
		Transport: &http.Transport{
			Proxy:       nil,
			DialContext: dialer.DialContext,
		},
	}
}

// WithResolver injects a deterministic DNS resolver (tests).
func (t *WebhookTool) WithResolver(r netResolver) *WebhookTool {
	t.resolver = r
	return t
}

// WithHTTPClient overrides the delivery client (tests).
func (t *WebhookTool) WithHTTPClient(c *http.Client) *WebhookTool {
	t.client = c
	return t
}

// WithPolicyGate attaches the per-tool policy gate (see policy.go).
func (t *WebhookTool) WithPolicyGate(gate *ToolPolicyGate) *WebhookTool {
	t.gate = gate
	return t
}

// Name implements Tool.
func (t *WebhookTool) Name() string { return WebhookToolName }

// DescribeTool publishes catalog metadata (issue #111).
func (t *WebhookTool) DescribeTool() ToolInfo {
	return ToolInfo{
		Name: t.Name(),
		Description: "Delivers a webhook to an OPERATOR-ALLOW-LISTED URL " +
			"(fire-and-forget or request-response), signed with HMAC-SHA256 over " +
			"{timestamp}.{body} via the secrets seam (X-AgentOS-Signature / " +
			"X-AgentOS-Timestamp). SSRF protection refuses private, loopback and " +
			"link-local destinations (including 169.254.169.254).",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url": map[string]any{
					"type":        "string",
					"description": "Absolute http(s) URL on the configured allow-list.",
				},
				"method": map[string]any{
					"type":        "string",
					"enum":        []string{"GET", "POST"},
					"description": "Delivery method (defaults to POST).",
				},
				"body": map[string]any{
					"type":        "string",
					"description": "POST body (signed together with the timestamp).",
				},
				"headers": map[string]any{
					"type":                 "object",
					"additionalProperties": map[string]any{"type": "string"},
					"description":          "Optional extra request headers.",
				},
				"fire_and_forget": map[string]any{
					"type":        "boolean",
					"description": "true returns {delivered,status,signed} without the receiver's body.",
				},
				"timeout_ms": map[string]any{
					"type":        "integer",
					"description": "Delivery deadline in milliseconds (defaults to 10000, capped at 60000).",
				},
				"estimated_cost_cents": map[string]any{
					"type":        "integer",
					"description": "Optional estimated cost surfaced to the policy engine (max_cost_cents condition).",
				},
			},
			"required": []string{"url"},
		},
	}
}

// Execute implements Tool using a background context.
func (t *WebhookTool) Execute(input map[string]any) (map[string]any, error) {
	return t.ExecuteContext(context.Background(), input)
}

// ExecuteContext implements ContextAware.
func (t *WebhookTool) ExecuteContext(ctx context.Context, input map[string]any) (map[string]any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if t == nil || t.client == nil {
		return nil, errors.New("tools: webhook tool is not initialized")
	}
	if err := t.gate.Allow(ctx, t.Name(), toolEstimatedCostCents(input["estimated_cost_cents"])); err != nil {
		return nil, err
	}
	orgID := policies.RunScopeFromContext(ctx).OrganizationID
	if orgID == "" {
		return nil, errors.New("tools: organization scope is required (run context)")
	}

	rawURL, err := stringArg(input["url"])
	if err != nil {
		return nil, fmt.Errorf("%w: url: %v", ErrInvalidToolInput, err)
	}
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("%w: cannot parse url %q: %v", ErrInvalidToolInput, rawURL, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("%w: url scheme %q is not allowed (http/https only)", ErrInvalidToolInput, parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("%w: url host is required", ErrInvalidToolInput)
	}
	if !t.allowListed(parsed) {
		return nil, fmt.Errorf("%w: %s", ErrWebhookNotAllowed, parsed.Host)
	}
	if err := t.checkDestination(ctx, parsed); err != nil {
		return nil, err
	}

	method := http.MethodPost
	if v, err := stringArg(input["method"]); err == nil {
		method = strings.ToUpper(strings.TrimSpace(v))
		if method != http.MethodGet && method != http.MethodPost {
			return nil, fmt.Errorf("%w: webhook method must be GET or POST, got %s", ErrInvalidToolInput, method)
		}
	}
	body := ""
	if v, err := stringArg(input["body"]); err == nil && method == http.MethodPost {
		body = v
	}

	// Sign before dispatch: key resolution failures must abort the delivery
	// rather than send an unsigned request when signing was configured.
	signature, ts, err := t.signRequest(ctx, orgID, body)
	if err != nil {
		return nil, err
	}

	fireAndForget := false
	switch v := input["fire_and_forget"].(type) {
	case bool:
		fireAndForget = v
	}

	timeout := timeoutArg(input["timeout_ms"], DefaultWebhookTimeout, MaxWebhookTimeout)
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var req *http.Request
	if method == http.MethodGet {
		req, err = http.NewRequestWithContext(callCtx, http.MethodGet, parsed.String(), nil)
	} else {
		req, err = http.NewRequestWithContext(callCtx, http.MethodPost, parsed.String(), strings.NewReader(body))
	}
	if err != nil {
		return nil, fmt.Errorf("%w: cannot build request: %v", ErrInvalidToolInput, err)
	}
	for key, value := range headerArgs(input["headers"]) {
		req.Header.Set(key, value)
	}
	if method == http.MethodPost && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if signature != "" {
		req.Header.Set(WebhookSignatureHeader, signature)
		req.Header.Set(WebhookTimestampHeader, ts)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		if callCtx.Err() != nil {
			return nil, fmt.Errorf("webhook delivery to %s timed out after %s", parsed.Host, timeout)
		}
		return nil, fmt.Errorf("webhook delivery to %s failed: %v", parsed.Host, err)
	}
	defer resp.Body.Close()

	if fireAndForget {
		// Drain a bounded amount so the connection is reusable, but never
		// surface the receiver's payload in fire-and-forget mode.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, int64(t.maxResponseBody)))
		return map[string]any{
			"delivered": true,
			"status":    resp.StatusCode,
			"signed":    signature != "",
		}, nil
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, int64(t.maxResponseBody)+1))
	if err != nil {
		return nil, fmt.Errorf("reading webhook response failed: %v", err)
	}
	responseBody := string(raw)
	if len(raw) > t.maxResponseBody {
		responseBody = string(raw[:t.maxResponseBody]) + httpBodyTruncationMarker
	}
	return map[string]any{
		"delivered": resp.StatusCode >= 200 && resp.StatusCode <= 299,
		"status":    resp.StatusCode,
		"body":      responseBody,
		"signed":    signature != "",
	}, nil
}

// allowListed reports whether u matches an allow-list entry (scheme + host +
// port equal, path equal or a descendant of the entry path).
func (t *WebhookTool) allowListed(u *url.URL) bool {
	if len(t.allowed) == 0 {
		return false // default deny: an empty allow-list refuses everything
	}
	host := strings.ToLower(u.Host)
	path := strings.TrimSuffix(u.Path, "/")
	for _, target := range t.allowed {
		if target.scheme != u.Scheme || target.host != host {
			continue
		}
		if target.path == "" || path == target.path || strings.HasPrefix(path, target.path+"/") {
			return true
		}
	}
	return false
}

// signRequest resolves the HMAC key through the secrets seam and computes
// "sha256=" + hex(HMAC-SHA256(key, "{timestamp}.{body}")). Unsigned mode
// (no signer/key ref configured) returns ("", "", nil).
func (t *WebhookTool) signRequest(ctx context.Context, orgID, body string) (string, string, error) {
	if t.signer == nil || strings.TrimSpace(t.signingKeyRef) == "" {
		return "", "", nil
	}
	key, err := t.signer.Resolve(ctx, orgID, t.signingKeyRef)
	if err != nil {
		return "", "", fmt.Errorf("tools: resolving webhook signing secret %q: %v", t.signingKeyRef, err)
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte(ts + "." + body))
	return WebhookSignaturePrefix + hex.EncodeToString(mac.Sum(nil)), ts, nil
}

// checkDestination performs the SSRF pre-check (resolution + forbidden IP
// rules); the dial-level Control hook on the transport is the second gate.
func (t *WebhookTool) checkDestination(ctx context.Context, u *url.URL) error {
	if t.allowPrivate {
		return nil
	}
	host := u.Hostname()
	lower := strings.ToLower(host)
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") ||
		strings.HasSuffix(lower, ".local") || strings.HasSuffix(lower, ".internal") {
		return fmt.Errorf("%w: %q resolves to a forbidden local name", ErrBlockedHost, host)
	}
	if ip := net.ParseIP(host); ip != nil {
		if isForbiddenIP(ip) {
			return fmt.Errorf("%w: %s is a forbidden address", ErrBlockedHost, host)
		}
		return nil
	}
	resolver := t.resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	ips, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("%w: cannot resolve host %q: %v", ErrBlockedHost, host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("%w: host %q resolved to no addresses", ErrBlockedHost, host)
	}
	for _, addr := range ips {
		if isForbiddenIP(addr.IP) {
			return fmt.Errorf("%w: host %q resolves to forbidden address %s", ErrBlockedHost, host, addr.IP)
		}
	}
	return nil
}
