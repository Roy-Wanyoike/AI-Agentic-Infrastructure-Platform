package main

// audit_chain_test.go — HTTP contract tests for the tamper-evidence
// endpoints (wave 8 issue A): RBAC matrix (audit.read = OWNER/ADMIN, same
// grant as the audit list endpoint), tenant scope strictly from the auth
// claims (cross-tenant probes -> 404, no existence leak), the export
// contract (required format param, ndjson content type, X-Audit-Head-Hash
// consistency with the body's chain_head line), the error envelope shape,
// tamper surfacing over HTTP through a corruptible store, self-audit rows,
// and the /v1 + /api/v1 dual mount.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentos/internal/apikeys"
	"agentos/internal/audit"
	"agentos/internal/auth"
)

// chainHandlerEnv is the audit-chain variant of auditHandlerEnv: bearer
// tokens for every role relevant to audit.read plus a foreign-org token.
type chainHandlerEnv struct {
	mux         *http.ServeMux
	svc         *audit.Service
	orgID       string
	ownerToken  string
	adminToken  string
	memberToken string
	viewerToken string
	otherToken  string
	otherOrgID  string
}

func newChainHandlerEnv(t *testing.T) *chainHandlerEnv {
	t.Helper()
	return newChainHandlerEnvWithStore(t, nil)
}

// newChainHandlerEnvWithStore wires the handlers over an optional Store
// (nil = in-memory). A corruptible store enables HTTP-level tamper tests.
func newChainHandlerEnvWithStore(t *testing.T, store audit.Store) *chainHandlerEnv {
	t.Helper()
	authSvc := auth.NewService("test-secret")
	apiKeysSvc := apikeys.NewService()

	_, owner, err := authSvc.Register("Acme", "owner@acme.test", "secret123")
	if err != nil {
		t.Fatalf("Register(owner) returned error: %v", err)
	}
	token := func(id, email, role string) string {
		t.Helper()
		generated, err := authSvc.GenerateToken(&auth.User{
			ID: id, Organization: owner.Organization, Email: email, Role: role,
		})
		if err != nil {
			t.Fatalf("GenerateToken(%s) returned error: %v", role, err)
		}
		return generated
	}
	_, foreign, err := authSvc.Register("OtherCo", "owner@other.test", "secret123")
	if err != nil {
		t.Fatalf("Register(foreign) returned error: %v", err)
	}
	otherToken, err := authSvc.GenerateToken(foreign)
	if err != nil {
		t.Fatalf("GenerateToken(foreign) returned error: %v", err)
	}

	var svc *audit.Service
	if store != nil {
		svc = audit.NewServiceWithStore(store)
	} else {
		svc = audit.NewService()
	}
	mux := http.NewServeMux()
	registerAuditChainRoutes(mux, svc, authSvc, apiKeysSvc)

	return &chainHandlerEnv{
		mux:         mux,
		svc:         svc,
		orgID:       owner.Organization,
		ownerToken:  token("user-owner", "owner@acme.test", "OWNER"),
		adminToken:  token("user-admin", "admin@acme.test", "ADMIN"),
		memberToken: token("user-member", "member@acme.test", "MEMBER"),
		viewerToken: token("user-viewer", "viewer@acme.test", "VIEWER"),
		otherToken:  otherToken,
		otherOrgID:  foreign.Organization,
	}
}

func (e *chainHandlerEnv) do(t *testing.T, path, token string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, strings.NewReader(""))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	e.mux.ServeHTTP(rr, req)
	var decoded map[string]any
	if rr.Body.Len() > 0 {
		_ = json.Unmarshal(rr.Body.Bytes(), &decoded)
	}
	return rr, decoded
}

// seedChainLogs writes three entries through the real write path so the
// trail is fully stamped.
func seedChainLogs(t *testing.T, env *chainHandlerEnv) {
	t.Helper()
	for i := 0; i < 3; i++ {
		if _, err := env.svc.LogCtx(t.Context(), "alice", "agent.created", env.orgID, "agents/a-1", map[string]any{"i": i}); err != nil {
			t.Fatalf("LogCtx %d: %v", i, err)
		}
	}
}

// chainCorruptibleStore is a minimal audit.Store whose rows can be edited
// after insertion — the injected corruption hook simulating a database-level
// edit of an append-only row (mirrors internal/audit's corruptibleStore; it
// lives here because that one is unexported and in another package).
type chainCorruptibleStore struct {
	entries []*audit.Entry
}

func (s *chainCorruptibleStore) InsertEntry(_ context.Context, entry *audit.Entry) error {
	s.entries = append(s.entries, entry)
	return nil
}

func (s *chainCorruptibleStore) ListEntries(_ context.Context, orgID string) ([]*audit.Entry, error) {
	out := make([]*audit.Entry, 0)
	for _, entry := range s.entries {
		if entry.OrganizationID == orgID {
			out = append(out, entry)
		}
	}
	return out, nil
}

func TestAuditChainVerifyRBACMatrix(t *testing.T) {
	env := newChainHandlerEnv(t)
	seedChainLogs(t, env)

	if rr, _ := env.do(t, "/audit/verify", ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated verify must be 401, got %d", rr.Code)
	}
	for _, tc := range []struct {
		name, token string
		want        int
	}{
		{"OWNER", env.ownerToken, http.StatusOK},
		{"ADMIN", env.adminToken, http.StatusOK},
		{"MEMBER", env.memberToken, http.StatusForbidden},
		{"VIEWER", env.viewerToken, http.StatusForbidden},
	} {
		rr, _ := env.do(t, "/audit/verify", tc.token)
		if rr.Code != tc.want {
			t.Fatalf("%s verify: got %d want %d", tc.name, rr.Code, tc.want)
		}
	}

	// OWNER body: the healthy chain verdict.
	rr, decoded := env.do(t, "/audit/verify", env.ownerToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("verify should be 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if decoded["valid"] != true {
		t.Fatalf("healthy chain must verify: %v", decoded)
	}
	if length, _ := decoded["length"].(float64); int(length) != 3 {
		t.Fatalf("length = %v, want 3", decoded["length"])
	}
	head, _, err := env.svc.ChainHead(t.Context(), env.orgID)
	if err != nil || decoded["head_hash"] != head {
		t.Fatalf("head_hash must match ChainHead: %v vs %q (%v)", decoded["head_hash"], head, err)
	}
	if _, present := decoded["broken_at_index"]; present {
		t.Fatalf("healthy chain must not render broken_at_index: %v", decoded)
	}
}

func TestAuditChainVerifySurfacesTampering(t *testing.T) {
	store := &chainCorruptibleStore{}
	env := newChainHandlerEnvWithStore(t, store)
	seedChainLogs(t, env)

	baseline, decoded := env.do(t, "/audit/verify", env.ownerToken)
	var healthy struct {
		Valid    bool   `json:"valid"`
		Length   int    `json:"length"`
		HeadHash string `json:"head_hash"`
	}
	if err := json.Unmarshal(baseline.Body.Bytes(), &healthy); err != nil || !healthy.Valid {
		t.Fatalf("pre-tamper verify must be valid: %s (%v)", baseline.Body.String(), err)
	}
	_ = decoded

	// Simulate a database-level edit of the second row.
	store.entries[1].Action = "agent.tampered"
	rr, tampered := env.do(t, "/audit/verify", env.ownerToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("a broken chain is still a valid ANSWER: got %d", rr.Code)
	}
	if tampered["valid"] != false {
		t.Fatalf("tampered chain must report valid=false: %v", tampered)
	}
	if index, _ := tampered["broken_at_index"].(float64); int(index) != 1 {
		t.Fatalf("broken_at_index = %v, want 1", tampered["broken_at_index"])
	}
	if tampered["expected_head_hash"] != healthy.HeadHash {
		t.Fatalf("expected_head_hash must be the claimed (pre-tamper) head: %v vs %q", tampered["expected_head_hash"], healthy.HeadHash)
	}
	if tampered["head_hash"] == healthy.HeadHash {
		t.Fatal("recomputed head must move after tampering")
	}

	// Read-only posture: the verify GET must NOT append an audit row — a
	// read that mutates the trail it verdicts would recursively grow the
	// chain and make the reported length depend on how often it was checked.
	entries, err := env.svc.ListCtx(t.Context(), env.orgID)
	if err != nil {
		t.Fatalf("ListCtx: %v", err)
	}
	for _, entry := range entries {
		if entry.Action == "audit.verify" {
			t.Fatalf("verify must not self-audit: found audit.verify row for actor %q", entry.Actor)
		}
	}
}

func TestAuditChainExportContract(t *testing.T) {
	env := newChainHandlerEnv(t)
	seedChainLogs(t, env)

	rr, _ := env.do(t, "/audit/export?format=jsonl", env.ownerToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("export should be 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); got != "application/x-ndjson" {
		t.Fatalf("Content-Type = %q, want application/x-ndjson", got)
	}
	body := rr.Body.String()
	if !strings.HasSuffix(body, "\n") {
		t.Fatal("export body must be newline-terminated")
	}
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected 3 entry lines + chain_head, got %d", len(lines))
	}
	// The header must agree with the body's own chain_head line.
	var headLine struct {
		Type         string `json:"type"`
		HeadHash     string `json:"head_hash"`
		Length       int    `json:"length"`
		ChainVersion string `json:"chain_version"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &headLine); err != nil {
		t.Fatalf("final line unparsable: %v", err)
	}
	if headLine.Type != "chain_head" || headLine.ChainVersion != "audit-chain/v1" || headLine.Length != 3 {
		t.Fatalf("chain_head line wrong: %+v", headLine)
	}
	if got := rr.Header().Get("X-Audit-Head-Hash"); got != headLine.HeadHash || got == "" {
		t.Fatalf("X-Audit-Head-Hash = %q, want the body's head %q", got, headLine.HeadHash)
	}
	// Entry lines carry the caller-scoped tenant, never a foreign org.
	for _, line := range lines[:3] {
		if !strings.Contains(line, `"organization_id":"`+env.orgID+`"`) {
			t.Fatalf("entry line must be tenant-scoped: %s", line)
		}
	}

	// Pure-snapshot contract: a second export over unchanged data is
	// byte-identical — no audit.export self-audit row is appended, because
	// a GET must not mutate the trail it evidences (byte-level determinism
	// for unchanged data is pinned at the service level in
	// internal/audit/export_test.go).
	rr2, _ := env.do(t, "/audit/export?format=jsonl", env.ownerToken)
	if rr2.Code != http.StatusOK {
		t.Fatalf("second export should be 200, got %d", rr2.Code)
	}
	if rr2.Body.String() != rr.Body.String() {
		t.Fatalf("exports over unchanged data must be byte-identical (pure snapshot):\n--- first:\n%s\n--- second:\n%s", rr.Body.String(), rr2.Body.String())
	}
	header2 := rr2.Header().Get("X-Audit-Head-Hash")
	if header2 == "" || header2 != rr.Header().Get("X-Audit-Head-Hash") {
		t.Fatalf("head-hash anchor must be stable across identical exports: %q vs %q", header2, rr.Header().Get("X-Audit-Head-Hash"))
	}
}

func TestAuditChainExportBadFormat(t *testing.T) {
	env := newChainHandlerEnv(t)
	for _, path := range []string{"/audit/export", "/audit/export?format=csv", "/audit/export?format="} {
		rr, decoded := env.do(t, path, env.ownerToken)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("%s must be 400, got %d", path, rr.Code)
		}
		if errCodeAud(decoded) != "INVALID_REQUEST" {
			t.Fatalf("%s must use the INVALID_REQUEST envelope: %v", path, decoded)
		}
		if _, ok := decoded["error"].(map[string]any)["message"].(string); !ok {
			t.Fatalf("error envelope must carry a message: %v", decoded)
		}
	}
}

func TestAuditChainCrossTenant404NoLeak(t *testing.T) {
	env := newChainHandlerEnv(t)
	seedChainLogs(t, env) // rows belong to env.orgID only

	// A foreign organization_id parameter is a cross-tenant probe: 404,
	// same code as an unknown trail, no existence leak.
	for _, path := range []string{
		"/audit/verify?organization_id=" + env.otherOrgID,
		"/audit/export?format=jsonl&organization_id=" + env.otherOrgID,
	} {
		rr, decoded := env.do(t, path, env.ownerToken)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("%s must be 404, got %d", path, rr.Code)
		}
		if errCodeAud(decoded) != "AUDIT_NOT_FOUND" {
			t.Fatalf("%s must use the AUDIT_NOT_FOUND envelope: %v", path, decoded)
		}
	}
	// The foreign owner sees only their own (empty) trail — never Acme's rows.
	rr, decoded := env.do(t, "/audit/verify", env.otherToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("foreign verify should be 200, got %d", rr.Code)
	}
	if length, _ := decoded["length"].(float64); int(length) != 0 {
		t.Fatalf("cross-tenant verify must not see Acme rows: %v", decoded)
	}
	rr, _ = env.do(t, "/audit/export?format=jsonl", env.otherToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("foreign export should be 200, got %d", rr.Code)
	}
	if strings.Contains(rr.Body.String(), env.orgID) {
		t.Fatal("cross-tenant export leaked the foreign tenant id")
	}
	// Naming your OWN org explicitly is fine (it matches the claims).
	rr, _ = env.do(t, "/audit/verify?organization_id="+env.orgID, env.ownerToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("self-scoped verify should be 200, got %d", rr.Code)
	}
}

func TestAuditChainUnavailableService(t *testing.T) {
	authSvc := auth.NewService("test-secret")
	apiKeysSvc := apikeys.NewService()
	_, owner, err := authSvc.Register("Acme", "owner@acme.test", "secret123")
	if err != nil {
		t.Fatalf("Register(owner) returned error: %v", err)
	}
	token, err := authSvc.GenerateToken(owner)
	if err != nil {
		t.Fatalf("GenerateToken returned error: %v", err)
	}
	mux := http.NewServeMux()
	registerAuditChainRoutes(mux, nil, authSvc, apiKeysSvc)
	for _, path := range []string{"/audit/verify", "/audit/export?format=jsonl"} {
		req := httptest.NewRequest(http.MethodGet, path, strings.NewReader(""))
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("nil audit service must be 503 for %s, got %d", path, rr.Code)
		}
	}
}

// TestAuditChainDualMount replicates the main.go StripPrefix wiring and pins
// that the endpoints are served under BOTH /v1 and /api/v1.
func TestAuditChainDualMount(t *testing.T) {
	env := newChainHandlerEnv(t)
	outer := http.NewServeMux()
	outer.Handle("/v1/", http.StripPrefix("/v1", env.mux))
	outer.Handle("/api/v1/", http.StripPrefix("/api/v1", env.mux))
	for _, path := range []string{"/v1/audit/verify", "/api/v1/audit/verify", "/v1/audit/export?format=jsonl", "/api/v1/audit/export?format=jsonl"} {
		req := httptest.NewRequest(http.MethodGet, path, strings.NewReader(""))
		req.Header.Set("Authorization", "Bearer "+env.ownerToken)
		rr := httptest.NewRecorder()
		outer.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s should be 200 through the dual mount, got %d: %s", path, rr.Code, rr.Body.String())
		}
	}
}
