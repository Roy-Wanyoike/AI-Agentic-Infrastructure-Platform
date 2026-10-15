package synth

// github.go — issue #151 (wave-10 track 10-s): least-privilege GitHub REST v3
// client for the synthetic-assurance track. net/http + encoding/json only (no
// SDK dependency — house rule for this track); tests never talk to the real
// GitHub (they fake it with net/http/httptest).
//
// Security posture (least privilege, enforced in this order):
//
//  1. Credential-by-reference: the token is resolved through the injected
//     SecretSource immediately before each API call and is never stored on
//     the client, cached, logged, or embedded in any error. The platform
//     secrets service (internal/secrets.Service) satisfies SecretSource
//     structurally (Resolve(ctx, orgID, name) (string, error)) — no adapter
//     code is required to wire it in.
//  2. Repository allowlist: GitHubClientConfig.AllowedRepos pins the exact
//     "owner/name" repositories the org may touch. Anything outside the
//     allowlist fails fast with ErrScopeViolation BEFORE any network I/O.
//  3. Narrow surface: only the issue-scoped REST calls the finding syncer
//     needs exist here (create/get/update/search issues). No admin, org, or
//     webhook-management endpoints are reachable through this client.
//  4. Rate-limit aware: Retry-After, X-RateLimit-Remaining and
//     X-RateLimit-Reset drive a bounded retry/backoff policy; an exhausted
//     budget pauses outbound calls until the documented reset. Production
//     callers with long wait windows should treat a rate_limited error as a
//     scheduling signal rather than relying on in-request blocking.
//  5. Revocation visibility: GitHub deliberately hides lost authorization
//     behind 404 (private resource) and non-rate-limit 403. Both surface as
//     errors satisfying errors.Is(err, ErrGitHubAccessRevoked) so callers
//     stop syncing and alert instead of hammering a revoked credential.
//
// Error taxonomy (GitHubError.Kind): rate_limited | auth | not_found |
// validation — plus transport (the request never left the process) and
// server (5xx without a Retry-After contract) for operational completeness.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Error taxonomy
// ---------------------------------------------------------------------------

// GitHubErrorKind classifies GitHub API failures. The #151 contract names
// rate_limited, auth, not_found and validation; transport and server cover
// failures that never produced a classifiable HTTP response.
type GitHubErrorKind string

const (
	GitHubErrRateLimited GitHubErrorKind = "rate_limited"
	GitHubErrAuth        GitHubErrorKind = "auth"
	GitHubErrNotFound    GitHubErrorKind = "not_found"
	GitHubErrValidation  GitHubErrorKind = "validation"
	GitHubErrTransport   GitHubErrorKind = "transport"
	GitHubErrServer      GitHubErrorKind = "server"
)

// ErrGitHubAccessRevoked is the typed sentinel for revoked or never-granted
// repository access. GitHub hides authorization gaps behind 404 and non-
// rate-limit 403, so the client conservatively marks BOTH as potentially
// revoked: errors.Is(err, ErrGitHubAccessRevoked) is the caller's stop signal.
var ErrGitHubAccessRevoked = errors.New("synth: github access revoked or forbidden")

// maxSnippetBytes bounds the sanitized response-body snippet carried by
// GitHubError. Error snippets never include request bodies or headers.
const maxSnippetBytes = 200

// maxBodyBytes bounds how much of any single response body is read.
const maxBodyBytes = 1 << 20

// GitHubError is the typed error returned for every classifiable GitHub API
// failure. It never embeds the credential (the resolved token is scrubbed
// from snippets), request bodies, or customer data from response bodies.
type GitHubError struct {
	Kind          GitHubErrorKind
	Status        int
	Method        string
	URL           string        // redacted: scheme+host+path only, never the query
	RetryAfter    time.Duration // >0 when the server asked to back off
	RateRemaining int           // -1 when the header was absent
	RateLimit     int           // -1 when the header was absent
	RateReset     time.Time     // zero when the header was absent
	Snippet       string        // sanitized response-body excerpt (token-scrubbed)
	Err           error

	revoked bool // true → errors.Is(err, ErrGitHubAccessRevoked)
}

func (e *GitHubError) Error() string {
	return fmt.Sprintf("synth: github %s %s: status=%d kind=%s: %v",
		e.Method, e.URL, e.Status, e.Kind, e.Err)
}

// Unwrap exposes the underlying cause.
func (e *GitHubError) Unwrap() error { return e.Err }

// Is reports match against the typed sentinels, chiefly
// ErrGitHubAccessRevoked for 404/non-rate-limit 403 responses.
func (e *GitHubError) Is(target error) bool {
	return target == ErrGitHubAccessRevoked && e.revoked
}

// ---------------------------------------------------------------------------
// Configuration, credential seam, construction
// ---------------------------------------------------------------------------

// SecretSource resolves a credential VALUE by reference at request time.
// Values are fetched fresh per call (rotation-safe), never logged and never
// retained by consumers. internal/secrets.Service satisfies this interface
// directly via structural typing.
type SecretSource interface {
	Resolve(ctx context.Context, orgID, ref string) (string, error)
}

// GitHubClientConfig configures one org-scoped GitHub REST v3 client.
type GitHubClientConfig struct {
	// OrgID scopes the client: it is the SecretSource lookup key and the
	// tenant recorded by the finding syncer. Required.
	OrgID string

	// TokenRef is the SecretSource REFERENCE (e.g. secret name) that holds
	// the GitHub token. The token itself must never appear in config, logs
	// or errors. Required.
	TokenRef string

	// AllowedRepos is the per-org repository allowlist as "owner/name"
	// entries. Requests against any other repo fail with ErrScopeViolation
	// before touching the network. At least one entry is required.
	AllowedRepos []string

	// BaseURL overrides https://api.github.com (tests point it at an
	// httptest server). Must be scheme + host, no path suffix required.
	BaseURL string

	// WebhookSecretRef optionally points at the SecretSource reference for
	// the webhook signing secret used by VerifyWebhookSignature and
	// FindingSyncer.SyncFromWebhook. Empty disables webhook verification.
	WebhookSecretRef string

	// HTTPClient overrides the default client (30s per-request timeout).
	HTTPClient *http.Client

	// MaxRetries bounds rate-limit retry attempts (default 2, clamped to
	// 0..5; attempts = MaxRetries+1).
	MaxRetries int

	// MaxBackoff caps any single backoff or rate-limit pause (default 5s).
	MaxBackoff time.Duration

	// UserAgent is sent with every request (default "agentos-synth").
	UserAgent string
}

// apiVersion is the pinned GitHub REST API version header value.
const apiVersion = "2022-11-28"

// GitHubClient is a least-privilege, org-scoped GitHub REST v3 client over
// net/http. It is safe for concurrent use.
type GitHubClient struct {
	cfg        GitHubClientConfig
	secrets    SecretSource
	http       *http.Client
	allowed    map[string]bool // lowercased "owner/name" → true
	userAgent  string
	maxRetries int
	maxBackoff time.Duration

	mu          sync.Mutex
	pausedUntil time.Time // rate budget exhausted: no sends before this

	// now/sleep are injectable for deterministic tests.
	now   func() time.Time
	sleep func(time.Duration)
}

// NewGitHubClient validates config and returns an org-scoped client. It
// fails fast on missing org/token/allowlist/secret-source rather than
// discovering the gap on first use.
func NewGitHubClient(cfg GitHubClientConfig, secrets SecretSource) (*GitHubClient, error) {
	if strings.TrimSpace(cfg.OrgID) == "" {
		return nil, fmt.Errorf("%w: github client requires OrgID", ErrInvalid)
	}
	if strings.TrimSpace(cfg.TokenRef) == "" {
		return nil, fmt.Errorf("%w: github client requires TokenRef (credential-by-reference)", ErrInvalid)
	}
	if len(cfg.AllowedRepos) == 0 {
		return nil, fmt.Errorf("%w: github client requires a non-empty AllowedRepos allowlist", ErrInvalid)
	}
	if secrets == nil {
		return nil, fmt.Errorf("%w: github client requires a SecretSource", ErrInvalid)
	}
	base := cfg.BaseURL
	if base == "" {
		base = "https://api.github.com"
	}
	if _, err := url.Parse(base); err != nil {
		return nil, fmt.Errorf("%w: invalid github BaseURL: %v", ErrInvalid, err)
	}

	allowed := make(map[string]bool, len(cfg.AllowedRepos))
	for _, r := range cfg.AllowedRepos {
		allowed[strings.ToLower(strings.TrimSpace(r))] = true
	}

	maxRetries := cfg.MaxRetries
	if maxRetries == 0 {
		maxRetries = 2
	}
	if maxRetries > 5 {
		maxRetries = 5
	}
	maxBackoff := cfg.MaxBackoff
	if maxBackoff <= 0 {
		maxBackoff = 5 * time.Second
	}
	userAgent := cfg.UserAgent
	if userAgent == "" {
		userAgent = "agentos-synth"
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}

	return &GitHubClient{
		cfg:        cfg,
		secrets:    secrets,
		http:       httpClient,
		allowed:    allowed,
		userAgent:  userAgent,
		maxRetries: maxRetries,
		maxBackoff: maxBackoff,
		now:        time.Now,
		sleep:      time.Sleep,
	}, nil
}

// RepoAllowed reports whether repo ("owner/name", case-insensitive) is on
// the org allowlist.
func (c *GitHubClient) RepoAllowed(repo string) bool {
	return c.allowed[strings.ToLower(strings.TrimSpace(repo))]
}

// repoShapePattern guards the "owner/name" form before it is ever placed on
// a URL path.
var repoShapePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)

// requireRepo enforces BOTH the "owner/name" shape and the org allowlist.
func (c *GitHubClient) requireRepo(repo string) error {
	trimmed := strings.TrimSpace(repo)
	if !repoShapePattern.MatchString(trimmed) {
		return fmt.Errorf("%w: github repo %q must have the \"owner/name\" shape", ErrInvalid, repo)
	}
	if !c.RepoAllowed(trimmed) {
		return fmt.Errorf("%w: repo %q is not on the github allowlist for org %q",
			ErrScopeViolation, trimmed, c.cfg.OrgID)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Rate-limit plumbing
// ---------------------------------------------------------------------------

// rateMeta carries the rate-limit state parsed from response headers.
type rateMeta struct {
	remaining  int // -1 when absent
	limit      int // -1 when absent
	reset      time.Time
	retryAfter time.Duration // 0 when absent
}

func readRateMeta(h http.Header) rateMeta {
	m := rateMeta{remaining: -1, limit: -1}
	if v := h.Get("X-RateLimit-Remaining"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			m.remaining = n
		}
	}
	if v := h.Get("X-RateLimit-Limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			m.limit = n
		}
	}
	if v := h.Get("X-RateLimit-Reset"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			m.reset = time.Unix(n, 0)
		}
	}
	if v := h.Get("Retry-After"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			m.retryAfter = time.Duration(n) * time.Second
		}
	}
	return m
}

// observeBudget records an exhausted rate budget: when the headers say zero
// requests remain, the client pauses outbound sends until the reset instant.
func (c *GitHubClient) observeBudget(m rateMeta) {
	if m.remaining != 0 || m.reset.IsZero() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if m.reset.After(c.now()) && m.reset.After(c.pausedUntil) {
		c.pausedUntil = m.reset
	}
}

// pausedFor returns the capped remaining pause (0 = send immediately).
func (c *GitHubClient) pausedFor() time.Duration {
	c.mu.Lock()
	pause := c.pausedUntil
	c.mu.Unlock()
	if pause.IsZero() {
		return 0
	}
	d := pause.Sub(c.now())
	if d <= 0 {
		return 0
	}
	return minDuration(d, c.maxBackoff)
}

// backoffFor chooses the sleep before a retry: Retry-After first, then the
// reset window, then a conservative default — always capped by MaxBackoff.
func (c *GitHubClient) backoffFor(m rateMeta) time.Duration {
	d := m.retryAfter
	if d <= 0 && !m.reset.IsZero() {
		if r := m.reset.Sub(c.now()); r > 0 {
			d = r
		}
	}
	if d <= 0 {
		d = time.Second
	}
	return minDuration(d, c.maxBackoff)
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// Core request path
// ---------------------------------------------------------------------------

// do executes one REST call with allowlist-free paths (repo checks happen at
// the typed call sites), rate-limit retries, and strict error classification.
func (c *GitHubClient) do(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	if d := c.pausedFor(); d > 0 {
		c.sleep(d)
	}

	// Credential-by-reference: resolved per call, never retained.
	token, err := c.secrets.Resolve(ctx, c.cfg.OrgID, c.cfg.TokenRef)
	if err != nil {
		// SecretSource errors carry the REFERENCE, never the value.
		return fmt.Errorf("synth: github token resolve failed (ref=%s org=%s): %w",
			c.cfg.TokenRef, c.cfg.OrgID, err)
	}

	full := strings.TrimSuffix(c.cfg.BaseURL, "/") + path
	redactedURL := full // queries (search terms) are deliberately dropped

	var payload []byte
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return &GitHubError{Kind: GitHubErrValidation, Method: method, URL: redactedURL,
				Err: fmt.Errorf("marshal request: %v", err)}
		}
	}

	attempts := c.maxRetries + 1
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, full, bytes.NewReader(payload))
		if err != nil {
			return &GitHubError{Kind: GitHubErrValidation, Method: method, URL: redactedURL,
				Err: fmt.Errorf("build request: %v", err)}
		}
		// Least-privilege headers: bearer token, pinned API version, JSON.
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", apiVersion)
		req.Header.Set("User-Agent", c.userAgent)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if query != nil && len(query) > 0 {
			// Encode manually so `full` stays the redaction-friendly URL.
			req.URL.RawQuery = query.Encode()
		}

		resp, err := c.http.Do(req)
		if err != nil {
			return &GitHubError{Kind: GitHubErrTransport, Method: method, URL: redactedURL,
				Err: fmt.Errorf("transport: %v", err)}
		}
		meta := readRateMeta(resp.Header)
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
		resp.Body.Close()
		c.observeBudget(meta)

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if out != nil && len(bodyBytes) > 0 {
				if err := json.Unmarshal(bodyBytes, out); err != nil {
					return &GitHubError{Kind: GitHubErrServer, Method: method, URL: redactedURL,
						Status: resp.StatusCode, Err: fmt.Errorf("decode response: %v", err)}
				}
			}
			return nil
		}

		// Rate-limit-aware retries: 429/503 always retry when budget allows;
		// 403 only when it signals the secondary rate limit.
		retryable := resp.StatusCode == 429 || resp.StatusCode == 503 ||
			(resp.StatusCode == 403 && (meta.retryAfter > 0 || meta.remaining == 0))
		if retryable && attempt+1 < attempts {
			c.sleep(c.backoffFor(meta))
			continue
		}
		return c.classify(method, redactedURL, resp.StatusCode, meta, bodyBytes, token)
	}
}

// classify maps a terminal HTTP failure onto the #151 error taxonomy.
func (c *GitHubClient) classify(method, redactedURL string, status int, meta rateMeta, body []byte, token string) *GitHubError {
	ge := &GitHubError{
		Method:        method,
		URL:           redactedURL,
		Status:        status,
		RetryAfter:    meta.retryAfter,
		RateRemaining: meta.remaining,
		RateLimit:     meta.limit,
		RateReset:     meta.reset,
		Snippet:       sanitizeSnippet(body, token),
	}
	switch {
	case status == 403 && (meta.retryAfter > 0 || meta.remaining == 0):
		ge.Kind, ge.Err = GitHubErrRateLimited, errors.New("secondary rate limit exceeded")
	case status == 429:
		ge.Kind, ge.Err = GitHubErrRateLimited, errors.New("primary rate limit exceeded")
	case status == 503:
		ge.Kind, ge.Err = GitHubErrServer, errors.New("service unavailable")
	case status == 401:
		ge.Kind, ge.Err = GitHubErrAuth, errors.New("bad credentials")
	case status == 403:
		// Non-rate-limit 403: GitHub's signal for forbidden/revoked access.
		ge.Kind, ge.Err, ge.revoked = GitHubErrAuth, errors.New("forbidden"), true
	case status == 404:
		// GitHub hides revoked private-repo access behind 404 — treat as
		// potentially revoked so callers stop and alert.
		ge.Kind, ge.Err, ge.revoked = GitHubErrNotFound, errors.New("not found or access revoked"), true
	case status >= 400 && status < 500:
		ge.Kind, ge.Err = GitHubErrValidation, fmt.Errorf("request rejected (status %d)", status)
	default:
		ge.Kind, ge.Err = GitHubErrServer, fmt.Errorf("unexpected status %d", status)
	}
	return ge
}

// sanitizeSnippet strips control characters, caps length, and removes any
// accidental echo of the credential before a body excerpt lands in an error.
func sanitizeSnippet(body []byte, token string) string {
	if len(body) == 0 {
		return ""
	}
	s := string(body)
	if token != "" {
		s = strings.ReplaceAll(s, token, "[REDACTED]")
	}
	var b strings.Builder
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' {
			b.WriteRune(' ')
			continue
		}
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	out := b.String()
	if len(out) > maxSnippetBytes {
		out = out[:maxSnippetBytes]
	}
	return out
}

// ---------------------------------------------------------------------------
// Typed REST surface (narrow: issues only)
// ---------------------------------------------------------------------------

// ghLabel mirrors the GitHub label object (name only — that is all the sync
// contract needs).
type ghLabel struct {
	Name string `json:"name"`
}

// GitHubIssue is the minimal issue projection consumed by the finding
// syncer; it is intentionally narrower than the full GitHub issue object.
type GitHubIssue struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	State     string    `json:"state"` // open|closed
	HTMLURL   string    `json:"html_url"`
	Labels    []ghLabel `json:"labels,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// LabelNames flattens label objects to plain names.
func (i *GitHubIssue) LabelNames() []string {
	names := make([]string, 0, len(i.Labels))
	for _, l := range i.Labels {
		names = append(names, l.Name)
	}
	return names
}

// CreateIssueRequest is the payload for POST /repos/{owner}/{repo}/issues.
type CreateIssueRequest struct {
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	Labels []string `json:"labels,omitempty"`
}

// UpdateIssueRequest is the payload for PATCH /repos/{owner}/{repo}/issues/{n}.
// Pointer fields are omitted when nil so partial patches never clear state.
type UpdateIssueRequest struct {
	Title  *string  `json:"title,omitempty"`
	Body   *string  `json:"body,omitempty"`
	State  *string  `json:"state,omitempty"` // "open" | "closed"
	Labels []string `json:"labels,omitempty"`
}

// issueSearchResult is the /search/issues envelope.
type issueSearchResult struct {
	TotalCount int           `json:"total_count"`
	Items      []GitHubIssue `json:"items"`
}

// CreateIssue creates one issue in an allow-listed repository.
func (c *GitHubClient) CreateIssue(ctx context.Context, repo string, req CreateIssueRequest) (*GitHubIssue, error) {
	if err := c.requireRepo(repo); err != nil {
		return nil, err
	}
	var out GitHubIssue
	if err := c.do(ctx, http.MethodPost, "/repos/"+repo+"/issues", nil, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetIssue fetches one issue from an allow-listed repository.
func (c *GitHubClient) GetIssue(ctx context.Context, repo string, number int) (*GitHubIssue, error) {
	if err := c.requireRepo(repo); err != nil {
		return nil, err
	}
	var out GitHubIssue
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/issues/%d", repo, number), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateIssue patches one issue in an allow-listed repository.
func (c *GitHubClient) UpdateIssue(ctx context.Context, repo string, number int, req UpdateIssueRequest) (*GitHubIssue, error) {
	if err := c.requireRepo(repo); err != nil {
		return nil, err
	}
	var out GitHubIssue
	if err := c.do(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/issues/%d", repo, number), nil, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SearchIssues runs a repo-scoped issue search. The repo and is:issue
// qualifiers are appended server-side here so callers cannot widen the
// search beyond the allow-listed repository. limit<=0 defaults to 10
// (capped at 50).
func (c *GitHubClient) SearchIssues(ctx context.Context, repo, query string, limit int) ([]GitHubIssue, error) {
	if err := c.requireRepo(repo); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}
	q := strings.TrimSpace(query) + fmt.Sprintf(" repo:%s is:issue", repo)
	v := url.Values{}
	v.Set("q", q)
	v.Set("per_page", strconv.Itoa(limit))
	var out issueSearchResult
	if err := c.do(ctx, http.MethodGet, "/search/issues", v, nil, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// ---------------------------------------------------------------------------
// Webhook signature verification
// ---------------------------------------------------------------------------

// signatureScheme is the only accepted X-Hub-Signature-256 scheme.
const signatureScheme = "sha256="

// VerifyGitHubSignature validates a GitHub webhook signature header against
// payload using HMAC-SHA256 with a CONSTANT-TIME compare (crypto/hmac.Equal).
// The header must be exactly "sha256=<lowercase-hex>"; any other scheme is
// rejected outright (no sha1 fallback — HMAC-SHA1 is not acceptable here).
func VerifyGitHubSignature(webhookSecret string, payload []byte, signatureHeader string) error {
	if webhookSecret == "" {
		return &GitHubError{Kind: GitHubErrValidation, Err: errors.New("webhook signing secret is empty")}
	}
	if !strings.HasPrefix(strings.ToLower(signatureHeader), signatureScheme) {
		return &GitHubError{Kind: GitHubErrValidation,
			Err: errors.New("signature header must use the sha256 scheme")}
	}
	rawHex := signatureHeader[len(signatureScheme):]
	got, err := hex.DecodeString(rawHex)
	if err != nil {
		return &GitHubError{Kind: GitHubErrValidation, Err: fmt.Errorf("signature is not valid hex: %v", err)}
	}
	mac := hmac.New(sha256.New, []byte(webhookSecret))
	mac.Write(payload)
	expected := mac.Sum(nil)
	if !hmac.Equal(expected, got) {
		return &GitHubError{Kind: GitHubErrAuth, Err: errors.New("webhook signature mismatch")}
	}
	return nil
}

// VerifyWebhookSignature resolves the configured webhook signing secret by
// reference and verifies the X-Hub-Signature-256 header for payload. It
// fails with a validation error when no WebhookSecretRef was configured.
func (c *GitHubClient) VerifyWebhookSignature(ctx context.Context, payload []byte, signatureHeader string) error {
	if c.cfg.WebhookSecretRef == "" {
		return &GitHubError{Kind: GitHubErrValidation,
			Err: errors.New("webhook signing secret not configured (WebhookSecretRef is empty)")}
	}
	secret, err := c.secrets.Resolve(ctx, c.cfg.OrgID, c.cfg.WebhookSecretRef)
	if err != nil {
		return fmt.Errorf("synth: github webhook secret resolve failed (ref=%s): %w", c.cfg.WebhookSecretRef, err)
	}
	return VerifyGitHubSignature(secret, payload, signatureHeader)
}
