package synth

// github_test.go — issue #151 tests for the least-privilege GitHub client.
// Everything runs against net/http/httptest fakes; NOTHING here talks to the
// real GitHub. Tests are deterministic: sleeps are recorded, never executed,
// and the clock is pinned.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

// fakeSecrets is a SecretSource that records lookups and returns ref-keyed
// values (the GitHub token, the webhook secret, …). It asserts the client
// fetches by (org, ref) and never embeds values anywhere in configuration.
type fakeSecrets struct {
	t       *testing.T
	org     string
	values  map[string]string // ref → value
	calls   int
	lastOrg string
	lastRef string
}

func (f *fakeSecrets) Resolve(_ context.Context, orgID, ref string) (string, error) {
	f.calls++
	f.lastOrg, f.lastRef = orgID, ref
	v, ok := f.values[ref]
	if !ok {
		f.t.Fatalf("SecretSource: unexpected lookup ref=%q (known: %v)", ref, f.values)
	}
	return v, nil
}

// newTestClient wires a GitHubClient to an httptest server with deterministic
// clock/sleep injection and returns the recorded sleeps.
func newTestClient(t *testing.T, baseURL, org string) (*GitHubClient, *fakeSecrets, *[]time.Duration) {
	t.Helper()
	secrets := &fakeSecrets{t: t, org: org, values: map[string]string{"gh-token": "tok-abc123"}}
	c, err := NewGitHubClient(GitHubClientConfig{
		OrgID:        org,
		TokenRef:     "gh-token",
		AllowedRepos: []string{"octo/demo", "Octo/Other"},
		BaseURL:      baseURL,
	}, secrets)
	if err != nil {
		t.Fatalf("NewGitHubClient: %v", err)
	}
	sleeps := &[]time.Duration{}
	c.now = func() time.Time { return time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC) }
	c.sleep = func(d time.Duration) { *sleeps = append(*sleeps, d) }
	return c, secrets, sleeps
}

// ---------------------------------------------------------------------------
// Construction + allowlist
// ---------------------------------------------------------------------------

func TestNewGitHubClientValidation(t *testing.T) {
	secrets := &fakeSecrets{t: t, org: "o", values: map[string]string{"r": "t"}}
	cases := []struct {
		name string
		cfg  GitHubClientConfig
	}{
		{"missing org", GitHubClientConfig{TokenRef: "r", AllowedRepos: []string{"a/b"}}},
		{"missing token ref", GitHubClientConfig{OrgID: "o", AllowedRepos: []string{"a/b"}}},
		{"missing allowlist", GitHubClientConfig{OrgID: "o", TokenRef: "r"}},
	}
	for _, tc := range cases {
		if _, err := NewGitHubClient(tc.cfg, secrets); err == nil {
			t.Errorf("%s: expected construction error", tc.name)
		}
	}
	if _, err := NewGitHubClient(GitHubClientConfig{OrgID: "o", TokenRef: "r", AllowedRepos: []string{"a/b"}}, nil); err == nil {
		t.Error("missing SecretSource: expected construction error")
	}
}

func TestGitHubClientAllowlistEnforcedBeforeNetwork(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(200)
	}))
	defer srv.Close()
	c, _, _ := newTestClient(t, srv.URL, "org1")

	_, err := c.CreateIssue(context.Background(), "octo/forbidden", CreateIssueRequest{Title: "x"})
	if err == nil {
		t.Fatal("expected ErrScopeViolation for non-allow-listed repo")
	}
	if !strings.Contains(err.Error(), "allowlist") {
		t.Errorf("error should mention the allowlist, got: %v", err)
	}
	_, err = c.CreateIssue(context.Background(), "octo", CreateIssueRequest{Title: "x"})
	if err == nil {
		t.Fatal("expected error for malformed repo shape")
	}
	if hits != 0 {
		t.Fatalf("network must not be touched for rejected repos, got %d hits", hits)
	}
	// Case-insensitive allowlist match is accepted (still verified below via happy path).
	if !c.RepoAllowed("OCTO/demo") || c.RepoAllowed("octo/secret") {
		t.Fatal("RepoAllowed matching is wrong")
	}
}

// ---------------------------------------------------------------------------
// Happy path + least-privilege headers
// ---------------------------------------------------------------------------

func TestGitHubClientCreateIssueLeastPrivilegeHeaders(t *testing.T) {
	var mu sync.Mutex
	var sawAuth, sawAccept, sawVersion, sawUA, sawCT string
	var sawPath, sawMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		sawAuth = r.Header.Get("Authorization")
		sawAccept = r.Header.Get("Accept")
		sawVersion = r.Header.Get("X-GitHub-Api-Version")
		sawUA = r.Header.Get("User-Agent")
		sawCT = r.Header.Get("Content-Type")
		sawPath, sawMethod = r.URL.Path, r.Method
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		_, _ = io.WriteString(w, `{"number":7,"title":"t","body":"b","state":"open","html_url":"https://git.test/octo/demo/issues/7","labels":[{"name":"synth"}]}`)
	}))
	defer srv.Close()
	c, secrets, _ := newTestClient(t, srv.URL, "org1")

	issue, err := c.CreateIssue(context.Background(), "octo/demo", CreateIssueRequest{Title: "t", Body: "b", Labels: []string{"synth"}})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if issue.Number != 7 || issue.State != "open" || issue.HTMLURL == "" {
		t.Fatalf("decoded issue wrong: %+v", issue)
	}
	if got := issue.LabelNames(); len(got) != 1 || got[0] != "synth" {
		t.Fatalf("labels: %v", got)
	}
	if sawAuth != "Bearer tok-abc123" {
		t.Errorf("Authorization = %q", sawAuth)
	}
	if sawAccept != "application/vnd.github+json" || sawVersion != apiVersion {
		t.Errorf("Accept=%q ApiVersion=%q", sawAccept, sawVersion)
	}
	if sawUA != "agentos-synth" || sawCT != "application/json" {
		t.Errorf("UA=%q CT=%q", sawUA, sawCT)
	}
	if sawMethod != http.MethodPost || sawPath != "/repos/octo/demo/issues" {
		t.Errorf("request = %s %s", sawMethod, sawPath)
	}
	if secrets.calls != 1 {
		t.Errorf("token must be resolved once per call, got %d", secrets.calls)
	}
}

func TestGitHubClientTokenNeverLeaksIntoErrors(t *testing.T) {
	// The server echoes the credential back inside a 500 body; the client
	// must scrub it from the surfaced snippet.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = io.WriteString(w, fmt.Sprintf(`{"message":"boom for %s"}`, r.Header.Get("Authorization")))
	}))
	defer srv.Close()
	c, _, _ := newTestClient(t, srv.URL, "org1")

	_, err := c.GetIssue(context.Background(), "octo/demo", 1)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "tok-abc123") {
		t.Fatalf("token leaked into error: %v", err)
	}
	ge, ok := err.(*GitHubError)
	if !ok || ge.Kind != GitHubErrServer || !strings.Contains(ge.Snippet, "[REDACTED]") {
		t.Fatalf("expected server-kind error with scrubbed snippet, got %#v", err)
	}
}

// ---------------------------------------------------------------------------
// Rate limits
// ---------------------------------------------------------------------------

func TestGitHubClientRetriesOn429HonoringRetryAfter(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "3")
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `{"message":"rate limit"}`)
			return
		}
		w.WriteHeader(201)
		_, _ = io.WriteString(w, `{"number":1,"title":"ok","state":"open"}`)
	}))
	defer srv.Close()
	c, _, sleeps := newTestClient(t, srv.URL, "org1")

	if _, err := c.CreateIssue(context.Background(), "octo/demo", CreateIssueRequest{Title: "ok"}); err != nil {
		t.Fatalf("expected retry to succeed: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 attempts, got %d", calls)
	}
	if len(*sleeps) != 1 || (*sleeps)[0] != 3*time.Second {
		t.Fatalf("Retry-After must be honored via sleep, recorded %v", *sleeps)
	}
}

func TestGitHubClient403SecondaryRateLimitClassified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(403)
	}))
	defer srv.Close()
	c, _, _ := newTestClient(t, srv.URL, "org1")

	_, err := c.SearchIssues(context.Background(), "octo/demo", "fingerprint:abc", 5)
	if err == nil {
		t.Fatal("expected error")
	}
	ge := err.(*GitHubError)
	if ge.Kind != GitHubErrRateLimited || ge.Status != 403 {
		t.Fatalf("kind=%s status=%d", ge.Kind, ge.Status)
	}
}

func TestGitHubClientExhaustedBudgetPausesNextCall(t *testing.T) {
	base := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", base.Add(10*time.Second).Unix()))
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"number":1,"title":"ok","state":"open"}`)
	}))
	defer srv.Close()
	c, _, sleeps := newTestClient(t, srv.URL, "org1")

	if _, err := c.GetIssue(context.Background(), "octo/demo", 1); err != nil {
		t.Fatalf("first call: %v", err)
	}
	// Remaining=0 on a 200 pauses the NEXT send until the (capped) reset.
	if _, err := c.GetIssue(context.Background(), "octo/demo", 1); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if len(*sleeps) != 1 || (*sleeps)[0] != c.maxBackoff {
		t.Fatalf("expected one capped pause sleep, recorded %v", *sleeps)
	}
}

// ---------------------------------------------------------------------------
// Error taxonomy
// ---------------------------------------------------------------------------

func TestGitHubClientErrorTaxonomy(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		extraHdr    map[string]string
		wantKind    GitHubErrorKind
		wantRevoked bool
	}{
		{"401 auth", 401, nil, GitHubErrAuth, false},
		{"403 forbidden is revoked", 403, map[string]string{"X-RateLimit-Remaining": "42"}, GitHubErrAuth, true},
		{"404 hides revocation", 404, nil, GitHubErrNotFound, true},
		{"422 validation", 422, nil, GitHubErrValidation, false},
		{"400 validation", 400, nil, GitHubErrValidation, false},
		{"500 server", 500, nil, GitHubErrServer, false},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			for k, v := range tc.extraHdr {
				w.Header().Set(k, v)
			}
			w.WriteHeader(tc.status)
		}))
		c, _, _ := newTestClient(t, srv.URL, "org1")
		_, err := c.GetIssue(context.Background(), "octo/demo", 9)
		srv.Close()
		if err == nil {
			t.Fatalf("%s: expected error", tc.name)
		}
		ge, ok := err.(*GitHubError)
		if !ok {
			t.Fatalf("%s: not a *GitHubError: %v", tc.name, err)
		}
		if ge.Kind != tc.wantKind {
			t.Errorf("%s: kind=%s want %s", tc.name, ge.Kind, tc.wantKind)
		}
		if got := errors.Is(err, ErrGitHubAccessRevoked); got != tc.wantRevoked {
			t.Errorf("%s: ErrGitHubAccessRevoked=%v want %v", tc.name, got, tc.wantRevoked)
		}
	}
}

// ---------------------------------------------------------------------------
// Search + update
// ---------------------------------------------------------------------------

func TestGitHubClientSearchIssuesScopesToRepo(t *testing.T) {
	var rawQuery, perPage string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawQuery = r.URL.Query().Get("q")
		perPage = r.URL.Query().Get("per_page")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"total_count":1,"items":[{"number":3,"title":"found","body":"x","state":"open","html_url":"u"}]}`)
	}))
	defer srv.Close()
	c, _, _ := newTestClient(t, srv.URL, "org1")

	items, err := c.SearchIssues(context.Background(), "octo/demo", `"agentos-synth" "fp-1" in:body`, 0)
	if err != nil {
		t.Fatalf("SearchIssues: %v", err)
	}
	if len(items) != 1 || items[0].Number != 3 {
		t.Fatalf("items: %+v", items)
	}
	if !strings.Contains(rawQuery, "repo:octo/demo") || !strings.Contains(rawQuery, "is:issue") ||
		!strings.Contains(rawQuery, `"agentos-synth"`) {
		t.Fatalf("query not repo-scoped: %q", rawQuery)
	}
	if perPage != "10" {
		t.Fatalf("default per_page = %q", perPage)
	}
}

func TestGitHubClientUpdateIssuePatch(t *testing.T) {
	var body UpdateIssueRequest
	var method, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"number":5,"title":"nt","body":"nb","state":"open"}`)
	}))
	defer srv.Close()
	c, _, _ := newTestClient(t, srv.URL, "org1")

	title := "nt"
	got, err := c.UpdateIssue(context.Background(), "octo/demo", 5, UpdateIssueRequest{Title: &title})
	if err != nil {
		t.Fatalf("UpdateIssue: %v", err)
	}
	if got.Title != "nt" || method != http.MethodPatch || path != "/repos/octo/demo/issues/5" {
		t.Fatalf("got=%+v method=%s path=%s", got, method, path)
	}
	if body.Title == nil || *body.Title != "nt" || body.State != nil {
		t.Fatalf("patch body wrong: %+v", body)
	}
}

// ---------------------------------------------------------------------------
// Webhook signatures
// ---------------------------------------------------------------------------

func TestVerifyGitHubSignature(t *testing.T) {
	secret := "whsec-xyz"
	payload := []byte(`{"action":"closed"}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if err := VerifyGitHubSignature(secret, payload, sig); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	if err := VerifyGitHubSignature(secret, []byte(`{"action":"opened"}`), sig); err == nil {
		t.Fatal("tampered payload accepted")
	}
	if err := VerifyGitHubSignature(secret, payload, "sha1=deadbeef"); err == nil {
		t.Fatal("sha1 scheme accepted")
	}
	if err := VerifyGitHubSignature(secret, payload, "sha256=zzzz"); err == nil {
		t.Fatal("non-hex signature accepted")
	}
	if err := VerifyGitHubSignature("", payload, sig); err == nil {
		t.Fatal("empty secret accepted")
	}
}

func TestGitHubClientVerifyWebhookSignatureRequiresConfig(t *testing.T) {
	c, _, _ := newTestClient(t, "http://unused.test", "org1")
	if err := c.VerifyWebhookSignature(context.Background(), []byte("p"), "sha256=00"); err == nil {
		t.Fatal("expected validation error when WebhookSecretRef is unset")
	}
}

func TestGitHubClientVerifyWebhookSignatureResolvesSecret(t *testing.T) {
	payload := []byte(`{"action":"reopened"}`)
	mac := hmac.New(sha256.New, []byte("whsec-1"))
	mac.Write(payload)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	whSecrets := &fakeSecrets{t: t, org: "org1", values: map[string]string{"wh-secret": "whsec-1"}}
	c, err := NewGitHubClient(GitHubClientConfig{
		OrgID:            "org1",
		TokenRef:         "unused",
		AllowedRepos:     []string{"octo/demo"},
		WebhookSecretRef: "wh-secret",
		BaseURL:          "http://unused.test",
	}, whSecrets)
	if err != nil {
		t.Fatalf("NewGitHubClient: %v", err)
	}

	if err := c.VerifyWebhookSignature(context.Background(), payload, sig); err != nil {
		t.Fatalf("valid webhook signature rejected: %v", err)
	}
	if whSecrets.calls != 1 {
		t.Fatalf("webhook secret resolved %d times, want 1", whSecrets.calls)
	}
}
