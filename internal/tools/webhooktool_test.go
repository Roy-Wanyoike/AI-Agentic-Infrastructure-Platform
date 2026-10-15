package tools

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"agentos/internal/policies"
)

// webhookCtx carries the org scope signing/tenant rules need.
func webhookCtx() context.Context {
	return policies.WithRunScope(context.Background(), policies.RunScope{
		OrganizationID: "org-1",
		Environment:    "production",
	})
}

// staticResolver resolves every secret name to a fixed key (test signer).
type staticResolver struct{ key string }

func (s staticResolver) Resolve(_ context.Context, orgID, name string) (string, error) {
	if orgID == "" || name == "" {
		return "", errors.New("resolver requires org and name")
	}
	return s.key, nil
}

// fakeResolver resolves nothing (failure path).
type failingResolver struct{ called bool }

func (f *failingResolver) Resolve(_ context.Context, _, _ string) (string, error) {
	f.called = true
	return "", errors.New("secrets unavailable")
}

// fakeNetResolver answers every hostname with the given IPs.
type fakeNetResolver struct{ ips []net.IP }

func (f fakeNetResolver) LookupIPAddr(_ context.Context, _ string) ([]net.IPAddr, error) {
	out := make([]net.IPAddr, 0, len(f.ips))
	for _, ip := range f.ips {
		out = append(out, net.IPAddr{IP: ip})
	}
	return out, nil
}

func newSignedWebhookTool(t *testing.T, allow []string) *WebhookTool {
	t.Helper()
	tool, err := NewWebhookTool(allow, staticResolver{key: "whsec_test_123"}, "webhook-signing-key")
	if err != nil {
		t.Fatalf("NewWebhookTool failed: %v", err)
	}
	return tool.WithAllowPrivate(true) // httptest binds loopback
}

func computeTestSignature(key string, ts string, body string) string {
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte(ts + "." + body))
	return WebhookSignaturePrefix + hex.EncodeToString(mac.Sum(nil))
}

func TestWebhookToolSignedPostDelivery(t *testing.T) {
	var (
		gotMethod, gotBody, gotSig, gotTS, gotContentType string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		gotSig = r.Header.Get(WebhookSignatureHeader)
		gotTS = r.Header.Get(WebhookTimestampHeader)
		gotContentType = r.Header.Get("Content-Type")
		_, _ = w.Write([]byte(`{"accepted":true}`))
	}))
	defer server.Close()

	tool := newSignedWebhookTool(t, []string{server.URL + "/hooks/"})
	result, err := tool.ExecuteContext(webhookCtx(), map[string]any{
		"url":  server.URL + "/hooks/agent-events",
		"body": `{"event":"run.finished"}`,
	})
	if err != nil {
		t.Fatalf("ExecuteContext returned error: %v", err)
	}
	if gotMethod != http.MethodPost || gotBody != `{"event":"run.finished"}` {
		t.Fatalf("unexpected delivery method=%q body=%q", gotMethod, gotBody)
	}
	if gotContentType != "application/json" {
		t.Fatalf("expected default content type, got %q", gotContentType)
	}

	// Signature must verify against the resolved key over "ts.body".
	ts, err := strconv.ParseInt(gotTS, 10, 64)
	if err != nil || time.Since(time.Unix(ts, 0)) > time.Minute {
		t.Fatalf("timestamp header should be a fresh unix time, got %q", gotTS)
	}
	want := computeTestSignature("whsec_test_123", gotTS, `{"event":"run.finished"}`)
	if gotSig != want {
		t.Fatalf("signature mismatch: got %q want %q", gotSig, want)
	}
	if !strings.HasPrefix(gotSig, WebhookSignaturePrefix) {
		t.Fatalf("signature header missing algorithm prefix: %q", gotSig)
	}

	if result["status"] != http.StatusOK {
		t.Fatalf("unexpected status %#v", result["status"])
	}
	if body, _ := result["body"].(string); !strings.Contains(body, "accepted") {
		t.Fatalf("unexpected response body %#v", result["body"])
	}
	if result["signed"] != true || result["delivered"] != true {
		t.Fatalf("expected delivered+signed, got %#v", result)
	}
}

func TestWebhookToolSignatureCoversBodyAndTimestamp(t *testing.T) {
	// Tampered body must produce a different signature (the verifier-side
	// replay/tamper contract: signature = HMAC(key, "ts.body")).
	sig1 := computeTestSignature("k", "1700000000", "body-a")
	sig2 := computeTestSignature("k", "1700000000", "body-b")
	if sig1 == sig2 {
		t.Fatal("signature must depend on the body")
	}
	sig3 := computeTestSignature("k", "1700000001", "body-a")
	if sig1 == sig3 {
		t.Fatal("signature must depend on the timestamp")
	}
	// WebhookReplayWindow documents the verifier-side freshness bound.
	if WebhookReplayWindow != 5*time.Minute {
		t.Fatalf("unexpected replay window %s", WebhookReplayWindow)
	}
}

func TestWebhookToolFireAndForget(t *testing.T) {
	var called int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called++
		_, _ = w.Write([]byte(strings.Repeat("x", 4096)))
	}))
	defer server.Close()

	tool := newSignedWebhookTool(t, []string{server.URL})
	result, err := tool.ExecuteContext(webhookCtx(), map[string]any{
		"url":             server.URL,
		"body":            `{"n":1}`,
		"fire_and_forget": true,
	})
	if err != nil {
		t.Fatalf("ExecuteContext returned error: %v", err)
	}
	if called != 1 {
		t.Fatalf("expected exactly one delivery, got %d", called)
	}
	if result["delivered"] != true || result["status"] != http.StatusOK {
		t.Fatalf("unexpected result %#v", result)
	}
	if _, hasBody := result["body"]; hasBody {
		t.Fatalf("fire-and-forget must not surface the response body: %#v", result)
	}
}

func TestWebhookToolAllowListEnforcement(t *testing.T) {
	tool, err := NewWebhookTool([]string{
		"https://hooks.example.com/org1",
		"https://other.example.com",
	}, nil, "")
	if err != nil {
		t.Fatalf("NewWebhookTool failed: %v", err)
	}
	tool.WithAllowPrivate(true).WithHTTPClient(&http.Client{Timeout: time.Second})

	cases := []struct {
		name string
		url  string
		ok   bool
	}{
		{"exact allowed origin", "https://other.example.com/anything", true},
		{"path prefix at boundary", "https://hooks.example.com/org1/child/deeper", true},
		{"exact path", "https://hooks.example.com/org1", true},
		{"prefix without boundary", "https://hooks.example.com/org1x/evil", false},
		{"other path on allowed host", "https://hooks.example.com/other", false},
		{"scheme mismatch", "http://other.example.com/", false},
		{"host mismatch", "https://evil.example.com/", false},
		{"port counts as different host", "https://other.example.com:8443/", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tool.ExecuteContext(webhookCtx(), map[string]any{"url": tc.url, "body": "{}"})
			if tc.ok && err != nil {
				// Network failures are acceptable for allowed targets (we
				// only assert the allow-list gate opened); allow-list errors are not.
				if errors.Is(err, ErrWebhookNotAllowed) || strings.Contains(err.Error(), "allow-list") {
					t.Fatalf("allowed target rejected: %v", err)
				}
				return
			}
			if !tc.ok {
				if err == nil {
					t.Fatalf("expected allow-list rejection for %s", tc.url)
				}
				if !errors.Is(err, ErrWebhookNotAllowed) {
					t.Fatalf("expected ErrWebhookNotAllowed, got %v", err)
				}
			}
		})
	}
}

func TestWebhookToolDefaultDenyWhenAllowListEmpty(t *testing.T) {
	tool, err := NewWebhookTool(nil, nil, "")
	if err != nil {
		t.Fatalf("NewWebhookTool failed: %v", err)
	}
	tool.WithAllowPrivate(true)
	if _, err := tool.ExecuteContext(webhookCtx(), map[string]any{"url": "https://anything.example.com/"}); !errors.Is(err, ErrWebhookNotAllowed) {
		t.Fatalf("empty allow-list must deny, got %v", err)
	}
}

func TestWebhookToolSSRFBlocked(t *testing.T) {
	// Real httptest server on loopback — blocked despite being allow-listed.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("you should not see this"))
	}))
	defer server.Close()

	tool, err := NewWebhookTool([]string{
		server.URL, "http://127.0.0.1/", "http://10.1.2.3/", "http://169.254.169.254/",
		"http://[::1]/", "http://localhost/", "http://db.internal/", "http://foo.local/",
	}, nil, "")
	if err != nil {
		t.Fatalf("NewWebhookTool failed: %v", err)
	}
	for _, target := range []string{
		server.URL,
		"http://127.0.0.1/x",
		"http://10.1.2.3/y",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]/z",
	} {
		if _, err := tool.ExecuteContext(webhookCtx(), map[string]any{"url": target}); !errors.Is(err, ErrBlockedHost) {
			t.Fatalf("expected ErrBlockedHost for %s, got %v", target, err)
		}
	}

	// Local names are refused without resolution.
	for _, host := range []string{"http://localhost/", "http://db.internal/", "http://foo.local/"} {
		if _, err := tool.ExecuteContext(webhookCtx(), map[string]any{"url": host}); !errors.Is(err, ErrBlockedHost) {
			t.Fatalf("expected ErrBlockedHost for %s, got %v", host, err)
		}
	}

	// A hostname resolving to a private IP is refused (deterministic via the
	// injected resolver — covers DNS rebinding of public-looking names).
	tool2, err := NewWebhookTool([]string{"https://rebind.example.com/"}, nil, "")
	if err != nil {
		t.Fatalf("NewWebhookTool failed: %v", err)
	}
	tool2.WithResolver(fakeNetResolver{ips: []net.IP{net.ParseIP("10.0.0.9")}})
	if _, err := tool2.ExecuteContext(webhookCtx(), map[string]any{"url": "https://rebind.example.com/"}); !errors.Is(err, ErrBlockedHost) {
		t.Fatalf("expected ErrBlockedHost for private resolution, got %v", err)
	}
	// A public resolution passes the pre-check (no dial happens — the client
	// would fail; we assert the error is NOT a block).
	tool3, err := NewWebhookTool([]string{"https://public.example.com/"}, nil, "")
	if err != nil {
		t.Fatalf("NewWebhookTool failed: %v", err)
	}
	tool3.WithHTTPClient(&http.Client{Timeout: 500 * time.Millisecond}).
		WithResolver(fakeNetResolver{ips: []net.IP{net.ParseIP("93.184.216.34")}})
	if _, err := tool3.ExecuteContext(webhookCtx(), map[string]any{"url": "https://public.example.com/"}); err != nil && errors.Is(err, ErrBlockedHost) {
		t.Fatalf("public resolution must pass the SSRF pre-check, got %v", err)
	}
}

func TestWebhookToolSigningFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer server.Close()

	// Signer configured but resolution fails -> delivery aborts unsigned.
	failing := &failingResolver{}
	tool, err := NewWebhookTool([]string{server.URL}, failing, "wh-key")
	if err != nil {
		t.Fatalf("NewWebhookTool failed: %v", err)
	}
	tool.WithAllowPrivate(true)
	if _, err := tool.ExecuteContext(webhookCtx(), map[string]any{"url": server.URL, "body": "{}"}); err == nil {
		t.Fatal("expected signing failure to abort the delivery")
	}
	if !failing.called {
		t.Fatal("resolver should have been consulted")
	}

	// No signer configured -> unsigned delivery still works.
	unsigned, err := NewWebhookTool([]string{server.URL}, nil, "")
	if err != nil {
		t.Fatalf("NewWebhookTool failed: %v", err)
	}
	unsigned.WithAllowPrivate(true)
	result, err := unsigned.ExecuteContext(webhookCtx(), map[string]any{"url": server.URL, "body": "{}"})
	if err != nil {
		t.Fatalf("unsigned delivery failed: %v", err)
	}
	if result["signed"] != false {
		t.Fatalf("expected signed=false, got %#v", result["signed"])
	}
}

func TestWebhookToolScopeAndMethodRules(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer server.Close()
	tool := newSignedWebhookTool(t, []string{server.URL})

	// No org scope -> refused (secret resolution and audit need a tenant).
	if _, err := tool.ExecuteContext(context.Background(), map[string]any{"url": server.URL}); err == nil {
		t.Fatal("expected organization-scope refusal")
	}
	// Missing url.
	if _, err := tool.ExecuteContext(webhookCtx(), map[string]any{}); err == nil {
		t.Fatal("expected missing-url rejection")
	}
	// Disallowed method.
	if _, err := tool.ExecuteContext(webhookCtx(), map[string]any{"url": server.URL, "method": "DELETE"}); err == nil {
		t.Fatal("expected method rejection")
	}
	// GET works and carries no body.
	var gotMethod string
	getServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
	}))
	defer getServer.Close()
	getTool := newSignedWebhookTool(t, []string{getServer.URL})
	if _, err := getTool.ExecuteContext(webhookCtx(), map[string]any{"url": getServer.URL, "method": "GET", "body": "ignored"}); err != nil {
		t.Fatalf("GET delivery failed: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Fatalf("expected GET, got %s", gotMethod)
	}
}

func TestWebhookToolResponseSizeCap(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("y", DefaultWebhookMaxResponseBody*2)))
	}))
	defer server.Close()

	tool := newSignedWebhookTool(t, []string{server.URL})
	result, err := tool.ExecuteContext(webhookCtx(), map[string]any{"url": server.URL})
	if err != nil {
		t.Fatalf("ExecuteContext returned error: %v", err)
	}
	body, _ := result["body"].(string)
	if len(body) != DefaultWebhookMaxResponseBody+len(httpBodyTruncationMarker) {
		t.Fatalf("expected capped body, got %d bytes", len(body))
	}
	if !strings.HasSuffix(body, httpBodyTruncationMarker) {
		t.Fatal("expected truncation marker")
	}
}

func TestWebhookToolBadAllowListEntries(t *testing.T) {
	if _, err := NewWebhookTool([]string{"ftp://x.example/"}, nil, ""); err == nil {
		t.Fatal("non-http scheme entry must be rejected at construction")
	}
	if _, err := NewWebhookTool([]string{"not a url"}, nil, ""); err == nil {
		t.Fatal("unparseable entry must be rejected at construction")
	}
	if _, err := NewWebhookTool([]string{""}, nil, ""); err != nil {
		t.Fatalf("blank entries should be skipped, got %v", err)
	}
}

func TestWebhookToolPolicyGateDeniesBeforeNetwork(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("denied webhook must never be delivered")
	}))
	defer server.Close()

	gate := NewToolPolicyGate(policies.EvaluatorFunc(func(_ context.Context, _ string, _ policies.EvaluateRequest) (policies.Decision, error) {
		return policies.Decision{Decision: policies.EffectDeny, Reason: "webhooks paused"}, nil
	}))
	tool := newSignedWebhookTool(t, []string{server.URL}).WithPolicyGate(gate)
	_, err := tool.ExecuteContext(webhookCtx(), map[string]any{"url": server.URL})
	if !errors.Is(err, ToolPolicyDenied) {
		t.Fatalf("expected ToolPolicyDenied, got %v", err)
	}
}
