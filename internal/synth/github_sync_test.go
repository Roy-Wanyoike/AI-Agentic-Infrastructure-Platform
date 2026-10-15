package synth

// github_sync_test.go — issue #151 tests for the idempotent finding syncer.
// GitHub is faked with net/http/httptest; the tests pin the idempotency
// contract (fingerprint key), the redaction guarantee, the exact-match
// search guard and the webhook linkback loop. All deterministic.

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
// Fakes
// ---------------------------------------------------------------------------

// fakeSyncGitHub is a minimal in-memory GitHub: issues create/get/patch plus
// repo-scoped search, with per-endpoint hit counters the tests assert on.
type fakeSyncGitHub struct {
	t *testing.T

	mu         sync.Mutex
	nextNumber int
	issues     map[int]*GitHubIssue
	searchTxt  []GitHubIssue // items returned by /search/issues

	posts, gets, patches, searches int
	lastPost                       CreateIssueRequest
	lastPatch                      UpdateIssueRequest
	lastSearchQuery                string
}

func newFakeSyncGitHub(t *testing.T) *fakeSyncGitHub {
	return &fakeSyncGitHub{t: t, nextNumber: 100, issues: map[int]*GitHubIssue{}}
}

func (f *fakeSyncGitHub) serve() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/repos/octo/demo/issues":
			f.posts++
			var req CreateIssueRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.lastPost = req
			f.nextNumber++
			issue := &GitHubIssue{Number: f.nextNumber, Title: req.Title, Body: req.Body,
				State: "open", HTMLURL: fmt.Sprintf("https://git.test/octo/demo/issues/%d", f.nextNumber)}
			for _, l := range req.Labels {
				issue.Labels = append(issue.Labels, ghLabel{Name: l})
			}
			f.issues[issue.Number] = issue
			f.writeJSON(w, issue)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/octo/demo/issues/"):
			f.gets++
			var n int
			_, _ = fmt.Sscanf(r.URL.Path, "/repos/octo/demo/issues/%d", &n)
			issue, ok := f.issues[n]
			if !ok {
				w.WriteHeader(404)
				_, _ = io.WriteString(w, `{"message":"Not Found"}`)
				return
			}
			f.writeJSON(w, issue)
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/repos/octo/demo/issues/"):
			f.patches++
			var n int
			_, _ = fmt.Sscanf(r.URL.Path, "/repos/octo/demo/issues/%d", &n)
			issue, ok := f.issues[n]
			if !ok {
				w.WriteHeader(404)
				_, _ = io.WriteString(w, `{"message":"Not Found"}`)
				return
			}
			var req UpdateIssueRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.lastPatch = req
			if req.Title != nil {
				issue.Title = *req.Title
			}
			if req.Body != nil {
				issue.Body = *req.Body
			}
			if req.Labels != nil {
				issue.Labels = nil
				for _, l := range req.Labels {
					issue.Labels = append(issue.Labels, ghLabel{Name: l})
				}
			}
			f.writeJSON(w, issue)
		case r.Method == http.MethodGet && r.URL.Path == "/search/issues":
			f.searches++
			f.lastSearchQuery = r.URL.Query().Get("q")
			f.writeJSON(w, issueSearchResult{TotalCount: len(f.searchTxt), Items: f.searchTxt})
		default:
			w.WriteHeader(404)
			_, _ = io.WriteString(w, `{"message":"unexpected route"}`)
		}
	}))
}

func (f *fakeSyncGitHub) writeJSON(w http.ResponseWriter, v any) {
	_ = json.NewEncoder(w).Encode(v)
}

// newSyncTest builds a syncer + fake GitHub + store with the webhook secret
// configured (so webhook tests share the fixture).
func newSyncTest(t *testing.T) (*FindingSyncer, *fakeSyncGitHub, *InMemoryGitHubRefStore) {
	t.Helper()
	fake := newFakeSyncGitHub(t)
	srv := fake.serve()
	t.Cleanup(srv.Close)
	c, secrets, _ := newTestClient(t, srv.URL, "org1")
	c.cfg.WebhookSecretRef = "wh-secret"
	secrets.values["wh-secret"] = "whsec-1"
	store := NewInMemoryGitHubRefStore()
	s, err := NewFindingSyncer(c, store, "org1", FindingSyncerConfig{
		Repo:        "octo/demo",
		Labels:      []string{"triage"},
		IssuePrefix: "[synth] ",
	})
	if err != nil {
		t.Fatalf("NewFindingSyncer: %v", err)
	}
	return s, fake, store
}

// testFinding is a confirmed finding whose description carries a secret that
// MUST be scrubbed before it reaches the fake GitHub.
func testFinding() *Finding {
	yes := true
	return &Finding{
		ID:           "F-1",
		OrgID:        "org1",
		RunID:        "R-9",
		JourneyID:    "J-3",
		AssertionIx:  2,
		Status:       FindingConfirmed,
		Severity:     SevHigh,
		Title:        "Checkout button inert after login",
		Description:  "POST /cart returns 500. Customer sent: Authorization: Bearer live-super-secret and api_key: sk-live-1",
		Fingerprint:  "fp-abc123",
		ReproSteps:   "1. navigate https://staging.shop.dev/login\n2. fill #email qa@example.com\n3. click button[type=submit]",
		EvidenceRefs: []string{"evidence://runs/R-9/trace.zip", "evidence://runs/R-9/console.log"},
		ReproRunID:   "R-10",
		Reproducible: &yes,
		Confidence:   0.92,
	}
}

// ---------------------------------------------------------------------------
// Scrubbing
// ---------------------------------------------------------------------------

func TestScrubSecrets(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"authorization header", "Authorization: Bearer abc.def", "Authorization: [REDACTED]"},
		{"json api_key", `"api_key": "sk-123"`, `"api_key": [REDACTED]`},
		{"dashed api-key", "api-key=v123", "api-key=[REDACTED]"},
		{"underscore api_key", "api_key=sk", "api_key=[REDACTED]"},
		{"token in url (over-redacts to end of line — by design)", "https://x.test/cb?token=tok1&next=/a", "https://x.test/cb?token=[REDACTED]"},
		{"password colon", "password: hunter2", "password: [REDACTED]"},
		{"cookie header (whole value)", "Cookie: session=abc; Path=/", "Cookie: [REDACTED]"},
		{"secret equals", "secret=zzz", "secret=[REDACTED]"},
		{"case insensitive", "TOKEN: xyz", "TOKEN: [REDACTED]"},
		{"plain prose untouched", "the token count was 5 and access was granted", "the token count was 5 and access was granted"},
		{"no denylisted key", "total=42 status=open", "total=42 status=open"},
	}
	for _, tc := range cases {
		if got := ScrubSecrets(tc.in); got != tc.want {
			t.Errorf("%s: ScrubSecrets(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Create / idempotent re-sync
// ---------------------------------------------------------------------------

func TestSyncCreateThenIdempotentReSync(t *testing.T) {
	s, fake, store := newSyncTest(t)
	ctx := context.Background()
	f := testFinding()

	// First sync: create.
	res, err := s.SyncFinding(ctx, f)
	if err != nil {
		t.Fatalf("create sync: %v", err)
	}
	if !res.Created || res.Updated || res.Unchanged {
		t.Fatalf("first sync flags wrong: %+v", res)
	}
	if fake.posts != 1 || fake.patches != 0 || fake.searches != 1 {
		// searches==1: the store was empty, so the GitHub search fallback ran
		// before the create — that is the documented resolution order.
		t.Fatalf("call counts posts=%d patches=%d searches=%d", fake.posts, fake.patches, fake.searches)
	}
	ref := res.Ref
	if ref.Repo != "octo/demo" || ref.IssueNumber == 0 || ref.Fingerprint != f.Fingerprint ||
		ref.IdempotencyKey != "synth-finding:fp-abc123" || ref.FindingID != "F-1" || ref.OrgID != "org1" {
		t.Fatalf("ref wrong: %+v", ref)
	}

	// Body contract: scrubbed, sections, fingerprint footer, machine marker.
	body := fake.lastPost.Body
	if !strings.Contains(body, "## Severity rationale") ||
		!strings.Contains(body, "## Reproducible steps") ||
		!strings.Contains(body, "## Evidence") ||
		!strings.Contains(body, "## Environment / build context") {
		t.Fatalf("missing sections:\n%s", body)
	}
	if strings.Contains(body, "live-super-secret") || strings.Contains(body, "sk-live-1") {
		t.Fatalf("secret leaked into issue body:\n%s", body)
	}
	if !strings.Contains(body, "Authorization: [REDACTED]") {
		t.Fatalf("scrub marker missing:\n%s", body)
	}
	if !strings.Contains(body, "fingerprint: `fp-abc123`") ||
		!strings.Contains(body, "idempotency-key: `synth-finding:fp-abc123`") ||
		!strings.Contains(body, "- finding: `F-1`") {
		t.Fatalf("fingerprint footer missing:\n%s", body)
	}
	if _, _, _, ok := parseSyncMarker(body); !ok {
		t.Fatalf("machine marker missing:\n%s", body)
	}
	title := fake.lastPost.Title
	if title != "[synth] Checkout button inert after login" {
		t.Fatalf("title = %q", title)
	}
	if !hasLabel(fake.lastPost.Labels, "severity:high") || !hasLabel(fake.lastPost.Labels, "triage") ||
		!hasLabel(fake.lastPost.Labels, "synth-finding") {
		t.Fatalf("labels = %v", fake.lastPost.Labels)
	}

	// Identical re-sync: UPDATES path, but content unchanged → no write.
	res2, err := s.SyncFinding(ctx, f)
	if err != nil {
		t.Fatalf("re-sync: %v", err)
	}
	if !res2.Unchanged || res2.Created || res2.Updated {
		t.Fatalf("second sync flags wrong: %+v", res2)
	}
	if fake.patches != 0 || fake.posts != 1 || fake.searches != 1 {
		t.Fatalf("unchanged re-sync must not write or search: posts=%d patches=%d searches=%d",
			fake.posts, fake.patches, fake.searches)
	}
	if res2.Ref.IssueNumber != ref.IssueNumber {
		t.Fatalf("issue number drifted: %d → %d", ref.IssueNumber, res2.Ref.IssueNumber)
	}

	// Changed severity → content hash differs → exactly one PATCH.
	f.Severity = SevCritical
	res3, err := s.SyncFinding(ctx, f)
	if err != nil {
		t.Fatalf("changed re-sync: %v", err)
	}
	if !res3.Updated || fake.patches != 1 {
		t.Fatalf("changed re-sync: flags=%+v patches=%d", res3, fake.patches)
	}
	if !hasLabel(fake.lastPatch.Labels, "severity:critical") {
		t.Fatalf("patch labels = %v", fake.lastPatch.Labels)
	}

	// Ref store reflects the link under both keys.
	got, _ := store.GetByFingerprint(ctx, "org1", "fp-abc123")
	if got == nil || got.IssueNumber != ref.IssueNumber {
		t.Fatalf("fingerprint lookup: %+v", got)
	}
	got, _ = store.GetByFinding(ctx, "org1", "F-1")
	if got == nil || got.Fingerprint != "fp-abc123" {
		t.Fatalf("finding lookup: %+v", got)
	}
}

func hasLabel(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Stored-ref-first + search fallback with exact-match guard
// ---------------------------------------------------------------------------

func TestSyncUsesStoredRefBeforeSearch(t *testing.T) {
	s, fake, store := newSyncTest(t)
	ctx := context.Background()

	// Seed the store directly (as if synced by a previous process).
	if err := store.Put(ctx, GitHubRef{
		OrgID: "org1", FindingID: "F-1", Fingerprint: "fp-abc123",
		Repo: "octo/demo", IssueNumber: 77, IdempotencyKey: "synth-finding:fp-abc123",
		SyncedAt: time.Date(2025, 5, 1, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	fake.issues[77] = &GitHubIssue{Number: 77, State: "open",
		Title: "[synth] Checkout button inert after login"}

	res, err := s.SyncFinding(ctx, testFinding())
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if fake.searches != 0 {
		t.Fatalf("stored ref must short-circuit the search fallback, searches=%d", fake.searches)
	}
	if res.Ref.IssueNumber != 77 || !res.Updated {
		t.Fatalf("expected adopt+update of stored issue, got %+v (fake body empty → patch)", res)
	}
	if fake.patches != 1 {
		t.Fatalf("expected one patch to current content, patches=%d", fake.patches)
	}
}

func TestSyncSearchFallbackAdoptsExactMatch(t *testing.T) {
	s, fake, _ := newSyncTest(t)
	ctx := context.Background()
	fake.searchTxt = []GitHubIssue{{
		Number: 42, State: "open", HTMLURL: "https://git.test/octo/demo/issues/42",
		Title: "renamed by a human",
		Body:  "junk text\n<!-- agentos-synth v1 finding=F-1 fingerprint=fp-abc123 hash=00000000 -->",
	}}
	fake.issues[42] = &GitHubIssue{Number: 42, State: "open", HTMLURL: "https://git.test/octo/demo/issues/42",
		Title: "renamed by a human",
		Body:  "junk text\n<!-- agentos-synth v1 finding=F-1 fingerprint=fp-abc123 hash=00000000 -->"}

	res, err := s.SyncFinding(ctx, testFinding())
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if res.Created || !res.Updated {
		t.Fatalf("expected adoption via update, got %+v", res)
	}
	if fake.posts != 0 || fake.patches != 1 {
		t.Fatalf("posts=%d patches=%d", fake.posts, fake.patches)
	}
	if res.Ref.IssueNumber != 42 || res.Ref.IdempotencyKey != "synth-finding:fp-abc123" {
		t.Fatalf("adopted ref wrong: %+v", res.Ref)
	}
	if !strings.Contains(fake.lastSearchQuery, "repo:octo/demo") {
		t.Fatalf("search query not repo-scoped: %q", fake.lastSearchQuery)
	}
}

func TestSyncSearchFallbackRejectsSubstringMention(t *testing.T) {
	s, fake, _ := newSyncTest(t)
	ctx := context.Background()
	// Body MENTIONS the fingerprint but carries no machine marker — the
	// exact-match guard must reject it and fall through to create.
	fake.searchTxt = []GitHubIssue{{
		Number: 9, Title: "related chatter",
		Body: "someone wrote fingerprint=fp-abc123 in a comment about fp-abc123-extra\n" +
			"<!-- agentos-synth v1 finding=F-9 fingerprint=fp-other hash=00000000 -->",
	}}

	res, err := s.SyncFinding(ctx, testFinding())
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if !res.Created || fake.posts != 1 {
		t.Fatalf("expected a fresh create, got %+v posts=%d", res, fake.posts)
	}
}

func TestSyncSearchFallbackRefusesAmbiguityAndConflict(t *testing.T) {
	// Two issues carry the EXACT marker for the same fingerprint → refuse.
	s, fake, _ := newSyncTest(t)
	ctx := context.Background()
	mk := func(id string) GitHubIssue {
		return GitHubIssue{Number: 1, Title: "x",
			Body: "<!-- agentos-synth v1 finding=" + id + " fingerprint=fp-abc123 hash=00000000 -->"}
	}
	fake.searchTxt = []GitHubIssue{mk("F-1"), mk("F-1")}
	if _, err := s.SyncFinding(ctx, testFinding()); err == nil {
		t.Fatal("expected ambiguous-search error")
	} else if !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("wrong error: %v", err)
	}
	if fake.posts != 0 {
		t.Fatalf("must not create on ambiguity, posts=%d", fake.posts)
	}

	// Same fingerprint claimed by a DIFFERENT finding id → conflict, no write.
	s2, fake2, _ := newSyncTest(t)
	fake2.searchTxt = []GitHubIssue{mk("F-99")}
	if _, err := s2.SyncFinding(ctx, testFinding()); err == nil {
		t.Fatal("expected conflicting-fingerprint error")
	} else if !strings.Contains(err.Error(), "different finding") {
		t.Fatalf("wrong error: %v", err)
	}
	if fake2.posts != 0 {
		t.Fatalf("must not create on conflict, posts=%d", fake2.posts)
	}
}

// ---------------------------------------------------------------------------
// Validation + revocation propagation
// ---------------------------------------------------------------------------

func TestSyncValidatesFindingAndConstruction(t *testing.T) {
	s, fake, _ := newSyncTest(t)
	ctx := context.Background()

	cases := []struct {
		name string
		mut  func(*Finding)
	}{
		{"nil finding", func(f *Finding) { _ = f }},
		{"missing fingerprint", func(f *Finding) { f.Fingerprint = "" }},
		{"missing title", func(f *Finding) { f.Title = "" }},
		{"wrong org", func(f *Finding) { f.OrgID = "org2" }},
	}
	for _, tc := range cases {
		var f *Finding
		if tc.name != "nil finding" {
			f = testFinding()
			tc.mut(f)
		}
		if _, err := s.SyncFinding(ctx, f); err == nil {
			t.Errorf("%s: expected ErrInvalid", tc.name)
		}
	}
	if fake.posts != 0 {
		t.Fatalf("rejected findings must not hit the network, posts=%d", fake.posts)
	}

	// Org mismatch between syncer and client refuses at construction.
	c, _, _ := newTestClient(t, "http://unused.test", "orgX")
	if _, err := NewFindingSyncer(c, NewInMemoryGitHubRefStore(), "org1", FindingSyncerConfig{Repo: "octo/demo"}); err == nil {
		t.Error("expected org-mismatch construction error")
	}
	// Disallowed repo refuses at construction.
	c2, _, _ := newTestClient(t, "http://unused.test", "org1")
	if _, err := NewFindingSyncer(c2, NewInMemoryGitHubRefStore(), "org1", FindingSyncerConfig{Repo: "octo/secret"}); err == nil {
		t.Error("expected allowlist construction error")
	}
}

func TestSyncSurfacesRevokedAccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(404)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	}))
	defer srv.Close()
	c, _, _ := newTestClient(t, srv.URL, "org1")
	store := NewInMemoryGitHubRefStore()
	s, err := NewFindingSyncer(c, store, "org1", FindingSyncerConfig{Repo: "octo/demo"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.Put(ctx, GitHubRef{OrgID: "org1", FindingID: "F-1", Fingerprint: "fp-abc123",
		Repo: "octo/demo", IssueNumber: 5, IdempotencyKey: "synth-finding:fp-abc123"}); err != nil {
		t.Fatal(err)
	}
	_, err = s.SyncFinding(ctx, testFinding())
	if !errors.Is(err, ErrGitHubAccessRevoked) {
		t.Fatalf("revoked access must surface as ErrGitHubAccessRevoked, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Webhook linkback
// ---------------------------------------------------------------------------

func TestSyncFromWebhook(t *testing.T) {
	s, _, _ := newSyncTest(t)
	markerBody := "<!-- agentos-synth v1 finding=F-1 fingerprint=fp-abc123 hash=00000000 -->"

	payload := func(action, repo, body string) []byte {
		b, _ := json.Marshal(map[string]any{
			"action":     action,
			"issue":      map[string]any{"number": 7, "state": mapState(action), "html_url": "https://git.test/octo/demo/issues/7", "body": body},
			"repository": map[string]any{"full_name": repo},
		})
		return b
	}
	sign := func(secret string, p []byte) string {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(p)
		return "sha256=" + hex.EncodeToString(mac.Sum(nil))
	}

	// Valid closed event on OUR issue → caller-wired status update.
	p := payload("closed", "octo/demo", markerBody)
	upd, err := s.SyncFromWebhook(p, sign("whsec-1", p))
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	if !upd.Managed || upd.FindingID != "F-1" || upd.Fingerprint != "fp-abc123" ||
		upd.Action != "closed" || upd.IssueState != "closed" || upd.Repo != "octo/demo" ||
		upd.IssueNumber != 7 || upd.OrgID != "org1" {
		t.Fatalf("update wrong: %+v", upd)
	}

	// Reopened event flows through the same struct.
	p2 := payload("reopened", "octo/demo", markerBody)
	upd2, err := s.SyncFromWebhook(p2, sign("whsec-1", p2))
	if err != nil {
		t.Fatalf("reopened: %v", err)
	}
	if upd2.Action != "reopened" || upd2.IssueState != "open" || !upd2.Managed {
		t.Fatalf("reopened update wrong: %+v", upd2)
	}

	// Bad signature → typed auth error, nothing parsed.
	if _, err := s.SyncFromWebhook(p, "sha256="+strings.Repeat("0", 64)); err == nil {
		t.Fatal("invalid signature accepted")
	}

	// Foreign repo (even with a VALID signature) → allowlist refusal.
	p3 := payload("closed", "octo/other", markerBody)
	if _, err := s.SyncFromWebhook(p3, sign("whsec-1", p3)); err == nil {
		t.Fatal("foreign repo accepted")
	} else if !strings.Contains(err.Error(), "allowlist") && !strings.Contains(err.Error(), "scope") && !strings.Contains(err.Error(), "synced repo") {
		t.Fatalf("wrong error: %v", err)
	}

	// Unmanaged issue (no marker) → no error, Managed=false.
	p4 := payload("closed", "octo/demo", "just a human issue")
	upd4, err := s.SyncFromWebhook(p4, sign("whsec-1", p4))
	if err != nil {
		t.Fatalf("unmanaged issue: %v", err)
	}
	if upd4.Managed || upd4.FindingID != "" || upd4.Fingerprint != "" {
		t.Fatalf("unmanaged update wrong: %+v", upd4)
	}
}

func mapState(action string) string {
	if action == "closed" {
		return "closed"
	}
	return "open"
}

// ---------------------------------------------------------------------------
// In-memory ref store
// ---------------------------------------------------------------------------

func TestInMemoryGitHubRefStore(t *testing.T) {
	store := NewInMemoryGitHubRefStore()
	ctx := context.Background()

	if err := store.Put(ctx, GitHubRef{OrgID: "org1", FindingID: "F-1", Fingerprint: "fp-a",
		Repo: "octo/demo", IssueNumber: 1}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := store.Put(ctx, GitHubRef{OrgID: "org1"}); err == nil {
		t.Fatal("expected validation error for ref without finding/fingerprint")
	}

	ref, err := store.GetByFingerprint(ctx, "org1", "fp-a")
	if err != nil || ref == nil || ref.FindingID != "F-1" {
		t.Fatalf("fingerprint get: %+v err=%v", ref, err)
	}
	ref, err = store.GetByFinding(ctx, "org1", "F-1")
	if err != nil || ref == nil || ref.Fingerprint != "fp-a" {
		t.Fatalf("finding get: %+v err=%v", ref, err)
	}
	// Misses are (nil, nil) — a missing link is normal pre-creation state.
	if ref, _ := store.GetByFingerprint(ctx, "org1", "nope"); ref != nil {
		t.Fatalf("expected nil miss, got %+v", ref)
	}
	if ref, _ := store.GetByFinding(ctx, "org2", "F-1"); ref != nil {
		t.Fatal("cross-tenant leak: org2 must not see org1's ref")
	}
	// Upsert overwrites both keys.
	if err := store.Put(ctx, GitHubRef{OrgID: "org1", FindingID: "F-1", Fingerprint: "fp-a",
		Repo: "octo/demo", IssueNumber: 2}); err != nil {
		t.Fatal(err)
	}
	if ref, _ := store.GetByFinding(ctx, "org1", "F-1"); ref.IssueNumber != 2 {
		t.Fatalf("upsert failed: %+v", ref)
	}
	// Returned values are copies — mutating them must not corrupt the store.
	ref, _ = store.GetByFinding(ctx, "org1", "F-1")
	ref.IssueNumber = 999
	ref2, _ := store.GetByFinding(ctx, "org1", "F-1")
	if ref2.IssueNumber != 2 {
		t.Fatalf("store leaked a mutable pointer: %+v", ref2)
	}
}
