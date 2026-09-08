package main

// Showback handler tests (wave-8 issue C): auth, the usage.read RBAC grant,
// the report envelope, window/group_by validation and tenant scoping from
// claims. All in-memory (the source is a stub — the runs adapter mapping is
// pinned by internal/billing/showback_test.go).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agentos/internal/apikeys"
	"agentos/internal/auth"
	"agentos/internal/billing"
)

// stubShowbackSource records the org it was called for so tests can pin the
// tenant resolution (claims ONLY, never a client-supplied org). The source
// contract is org-scoped: ShowbackForPeriod returns usage for the REQUESTED
// org only, so the stub filters by ownerOrg — mirroring how the real
// meters-backed source scopes its SQL by organization_id.
type stubShowbackSource struct {
	usage    []billing.ShowbackUsage
	err      error
	lastOrg  string
	ownerOrg string
}

func (s *stubShowbackSource) ShowbackForPeriod(_ context.Context, orgID string, _, _ time.Time) ([]billing.ShowbackUsage, error) {
	if s.err != nil {
		return nil, s.err
	}
	s.lastOrg = orgID
	if orgID != s.ownerOrg {
		return nil, nil // another tenant: nothing to report
	}
	return s.usage, nil
}

type showbackHandlerEnv struct {
	mux         *http.ServeMux
	src         *stubShowbackSource
	ownerToken  string
	viewerToken string
	otherToken  string
	otherOrg    string
}

func newShowbackHandlerEnv(t *testing.T, usage []billing.ShowbackUsage) *showbackHandlerEnv {
	t.Helper()
	authSvc := auth.NewService("test-secret")
	apiKeysSvc := apikeys.NewService()

	_, owner, err := authSvc.Register("Acme", "owner@acme.test", "secret123")
	if err != nil {
		t.Fatalf("Register(owner) returned error: %v", err)
	}
	ownerToken, err := authSvc.GenerateToken(owner)
	if err != nil {
		t.Fatalf("GenerateToken(owner) returned error: %v", err)
	}
	viewerToken, err := authSvc.GenerateToken(&auth.User{ID: "user-viewer", Organization: owner.Organization, Email: "viewer@acme.test", Role: "VIEWER"})
	if err != nil {
		t.Fatalf("GenerateToken(viewer) returned error: %v", err)
	}
	_, foreign, err := authSvc.Register("OtherCo", "owner@other.test", "secret123")
	if err != nil {
		t.Fatalf("Register(foreign) returned error: %v", err)
	}
	otherToken, err := authSvc.GenerateToken(foreign)
	if err != nil {
		t.Fatalf("GenerateToken(foreign) returned error: %v", err)
	}

	src := &stubShowbackSource{usage: usage, ownerOrg: owner.Organization}
	mux := http.NewServeMux()
	registerShowbackRoutes(mux, src, authSvc, apiKeysSvc)

	return &showbackHandlerEnv{
		mux: mux, src: src, ownerToken: ownerToken, viewerToken: viewerToken,
		otherToken: otherToken, otherOrg: foreign.Organization,
	}
}

func (e *showbackHandlerEnv) do(t *testing.T, path, token string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, strings.NewReader(""))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	e.mux.ServeHTTP(rr, req)
	out := map[string]any{}
	if strings.Contains(rr.Header().Get("Content-Type"), "application/json") {
		_ = json.Unmarshal(rr.Body.Bytes(), &out)
	}
	return rr, out
}

func TestShowbackRequiresAuth(t *testing.T) {
	env := newShowbackHandlerEnv(t, nil)
	if rr, _ := env.do(t, "/showback", ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("showback without token = %d, want 401", rr.Code)
	}
}

func TestShowbackRBACFollowsUsageRead(t *testing.T) {
	// usage.read grants OWNER/ADMIN/MEMBER/VIEWER — showback is read-only
	// cost reporting in the usage-costs family, so even VIEWER can read.
	env := newShowbackHandlerEnv(t, nil)
	for _, token := range []string{env.ownerToken, env.viewerToken} {
		if rr, _ := env.do(t, "/showback", token); rr.Code != http.StatusOK {
			t.Fatalf("showback as %q = %d, want 200", token, rr.Code)
		}
	}
}

func TestShowbackReportEnvelopeAndAggregation(t *testing.T) {
	env := newShowbackHandlerEnv(t, []billing.ShowbackUsage{
		{AgentID: "agent-b", Runs: 7, CostCents: 42.5},
		{AgentID: "agent-a", Runs: 3, CostCents: 10},
	})

	rr, body := env.do(t, "/showback?from=2026-03-01&to=2026-03-08&group_by=agent", env.ownerToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("showback = %d body=%s", rr.Code, rr.Body.String())
	}
	if body["group_by"] != "agent" {
		t.Fatalf("group_by echo mismatch: %v", body)
	}
	rows, _ := body["rows"].([]any)
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %v", body)
	}
	first, _ := rows[0].(map[string]any)
	// Deterministic order (sorted by scope_ref) + snake_case wire names.
	if first["scope_ref"] != "agent-a" || first["runs"] != float64(3) || first["cost_cents"] != float64(10) {
		t.Fatalf("unexpected first row: %v", first)
	}
	if _, ok := first["tool_calls"]; !ok {
		t.Fatal("rows must always carry tool_calls (0 documented, never omitted)")
	}
	if notes, _ := body["notes"].([]any); len(notes) != 0 {
		t.Fatalf("agent grouping needs no notes: %v", body["notes"])
	}

	// Tenant comes from claims ONLY: the foreign owner queries the same path
	// and gets its OWN org's (empty) report, never Acme's rows.
	rr, body = env.do(t, "/showback", env.otherToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("foreign showback = %d", rr.Code)
	}
	if rows, _ := body["rows"].([]any); len(rows) != 0 {
		t.Fatalf("foreign report must be empty, got %v", rows)
	}
	if env.src.lastOrg != env.otherOrg {
		t.Fatalf("source was called for %q, want the caller's org %q", env.src.lastOrg, env.otherOrg)
	}
}

func TestShowbackTeamFallbackNote(t *testing.T) {
	env := newShowbackHandlerEnv(t, []billing.ShowbackUsage{{AgentID: "agent-a", Runs: 3, CostCents: 10}})
	rr, body := env.do(t, "/showback?group_by=team", env.ownerToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("showback(team) = %d", rr.Code)
	}
	if body["group_by"] != "team" {
		t.Fatalf("group_by echo mismatch: %v", body)
	}
	notes, _ := body["notes"].([]any)
	if len(notes) == 0 {
		t.Fatal("team fallback must document the limitation in notes")
	}
}

func TestShowbackValidationErrors(t *testing.T) {
	env := newShowbackHandlerEnv(t, nil)
	cases := []struct {
		query    string
		wantCode int
		wantErr  string
	}{
		{"?group_by=model", http.StatusBadRequest, "INVALID_GROUP_BY"},
		{"?from=bananas", http.StatusBadRequest, "INVALID_TIME_RANGE"},
		{"?to=bananas", http.StatusBadRequest, "INVALID_TIME_RANGE"},
		{"?from=2026-03-08&to=2026-03-01", http.StatusBadRequest, "INVALID_TIME_RANGE"},
		// 93 days exceeds the 92-day cap.
		{"?from=2026-01-01&to=2026-04-04", http.StatusBadRequest, "INVALID_TIME_RANGE"},
	}
	for _, tc := range cases {
		rr, body := env.do(t, "/showback"+tc.query, env.ownerToken)
		if rr.Code != tc.wantCode {
			t.Fatalf("%s = %d body=%s", tc.query, rr.Code, rr.Body.String())
		}
		if got, _ := body["error"].(map[string]any)["code"]; got != tc.wantErr {
			t.Fatalf("%s error.code = %v, want %s", tc.query, got, tc.wantErr)
		}
	}
	// 92 days passes.
	if rr, _ := env.do(t, "/showback?from=2026-01-01&to=2026-04-03", env.ownerToken); rr.Code != http.StatusOK {
		t.Fatalf("92-day window = %d, want 200", rr.Code)
	}
	// Defaults apply when from/to are omitted.
	if rr, body := env.do(t, "/showback", env.ownerToken); rr.Code != http.StatusOK || body["group_by"] != "agent" {
		t.Fatalf("default window/group_by = %d %v", rr.Code, body["group_by"])
	}
	// Non-GET is 405.
	req := httptest.NewRequest(http.MethodPost, "/showback", strings.NewReader(""))
	req.Header.Set("Authorization", "Bearer "+env.ownerToken)
	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /showback = %d, want 405", rr.Code)
	}
}
