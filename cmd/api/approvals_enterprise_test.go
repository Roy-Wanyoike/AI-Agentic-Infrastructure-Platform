package main

// approvals_enterprise_test.go — HTTP coverage for the wave-9 issue #137
// enterprise oversight surface (cmd/api/approvals_enterprise.go), exercised
// through the REAL middleware chain (RequireAuthOrAPIKey + RequirePermission)
// exactly like main.go wires it: delegation lifecycle + membership guard +
// org isolation, escalation policy CRUD + explicit SLA sweep, evidence export
// (determinism, window, role gate, org isolation) and the dual-control
// two-person rule enforced transparently on the EXISTING decide endpoint.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agentos/internal/apikeys"
	"agentos/internal/approvals"
	"agentos/internal/audit"
	"agentos/internal/auth"
	"agentos/internal/organizations"
	"agentos/internal/queue"
	"agentos/internal/runs"
	"agentos/internal/workflows"
)

func apEntSetup(t *testing.T) (*http.ServeMux, *auth.Service, *approvals.Service, *organizations.Service, *audit.Service, *auth.MemoryStore) {
	t.Helper()
	identities := auth.NewMemoryStore()
	authSvc := auth.NewServiceWithStore("ap-ent-test-secret", identities)
	apiKeysSvc := apikeys.NewService()
	apSvc := approvals.NewService()
	orgsSvc := organizations.NewService()
	auditSvc := audit.NewService()

	mux := http.NewServeMux()
	registerApprovalEnterpriseRoutes(mux, apSvc, orgsSvc, authSvc, apiKeysSvc, auditSvc)
	return mux, authSvc, apSvc, orgsSvc, auditSvc, identities
}

// apEntSetupWithWorkflows mirrors main.go: the enterprise oversight routes
// are mounted NEXT TO the existing wave-2 approval endpoints (decide), so
// dual control can be tested end-to-end over HTTP.
func apEntSetupWithWorkflows(t *testing.T) (*http.ServeMux, *auth.Service, *approvals.Service, *organizations.Service, *audit.Service, *auth.MemoryStore) {
	t.Helper()
	identities := auth.NewMemoryStore()
	authSvc := auth.NewServiceWithStore("ap-ent-test-secret", identities)
	apiKeysSvc := apikeys.NewService()
	apSvc := approvals.NewService()
	orgsSvc := organizations.NewService()
	auditSvc := audit.NewService()

	mux := http.NewServeMux()
	registerWorkflowsRoutes(mux, workflows.NewService(), apSvc, runs.NewService(), queue.NewQueue(), authSvc, apiKeysSvc)
	registerApprovalEnterpriseRoutes(mux, apSvc, orgsSvc, authSvc, apiKeysSvc, auditSvc)
	return mux, authSvc, apSvc, orgsSvc, auditSvc, identities
}

// apEntOwner provisions the tenant coherently across BOTH services (the
// newOrgHandlerEnv precedent): the org exists in the organizations service
// AND the auth identity store, the owner identity is seeded with the same
// org id, and a membership row backs the directory. The returned token is
// bound to the shared org id, so claims.OrganizationID resolves everywhere.
func apEntOwner(t *testing.T, authSvc *auth.Service, orgsSvc *organizations.Service, identities *auth.MemoryStore, email string) (orgID, userID, token string) {
	t.Helper()
	ctx := context.Background()
	orgName := "Acme-" + strings.ReplaceAll(strings.ReplaceAll(email, "@", "-at-"), ".", "-") // unique per tenant in the test
	org, err := orgsSvc.CreateCtx(ctx, orgName)
	if err != nil {
		t.Fatalf("CreateCtx (org) returned error: %v", err)
	}
	if err := identities.CreateOrganization(ctx, &auth.Organization{ID: org.ID, Name: orgName}); err != nil {
		t.Fatalf("CreateOrganization (identity store): %v", err)
	}
	userID = "user-" + strings.ReplaceAll(strings.ReplaceAll(email, "@", "_at_"), ".", "_")
	if err := identities.CreateUser(ctx, &auth.User{
		ID: userID, Organization: org.ID, Email: email, Role: "OWNER", Active: true,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateUser (%s): %v", email, err)
	}
	if err := orgsSvc.AddMemberCtx(ctx, org.ID, userID, "OWNER"); err != nil {
		t.Fatalf("AddMemberCtx (owner): %v", err)
	}
	token, err = authSvc.GenerateToken(&auth.User{ID: userID, Organization: org.ID, Email: email, Role: "OWNER", Active: true})
	if err != nil {
		t.Fatalf("GenerateToken returned error: %v", err)
	}
	return org.ID, userID, token
}

// apEntGhostToken mints an OWNER token for a user that is not registered with
// the auth service (the RBAC middleware falls back to the token role claims —
// the wfViewerToken precedent).
func apEntGhostToken(t *testing.T, authSvc *auth.Service, orgID, userID string) string {
	t.Helper()
	token, err := authSvc.GenerateToken(&auth.User{
		ID:           userID,
		Organization: orgID,
		Email:        userID + "@ghost.acme.test",
		Role:         "OWNER",
	})
	if err != nil {
		t.Fatalf("GenerateToken (ghost) returned error: %v", err)
	}
	return token
}

func apEntToken(t *testing.T, authSvc *auth.Service, orgID, userID, role string) string {
	t.Helper()
	token, err := authSvc.GenerateToken(&auth.User{
		ID:           userID,
		Organization: orgID,
		Email:        userID + "@ghost.acme.test",
		Role:         role,
	})
	if err != nil {
		t.Fatalf("GenerateToken (%s) returned error: %v", role, err)
	}
	return token
}

func apEntAuditActions(t *testing.T, auditSvc *audit.Service, orgID string) []string {
	t.Helper()
	entries, _, err := auditSvc.ListEntriesPaged(context.Background(), orgID, 200, "")
	if err != nil {
		t.Fatalf("ListEntriesPaged: %v", err)
	}
	actions := make([]string, 0, len(entries))
	for _, e := range entries {
		actions = append(actions, e.Action)
	}
	return actions
}

func apEntCount(actions []string, action string) int {
	n := 0
	for _, a := range actions {
		if a == action {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// Delegation
// ---------------------------------------------------------------------------

func TestApprovalEnterpriseDelegateLifecycleHTTP(t *testing.T) {
	mux, authSvc, apSvc, orgsSvc, auditSvc, identities := apEntSetup(t)
	ctx := context.Background()
	orgID, ownerID, token := apEntOwner(t, authSvc, orgsSvc, identities, "owner-delegate@acme.test")

	a, err := apSvc.Request(ctx, orgID, approvals.RequestInput{Action: "deploy", Requester: "maker-1"})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	// The delegatee must be an org member.
	if err := orgsSvc.AddMemberCtx(ctx, orgID, "member-2", "MEMBER"); err != nil {
		t.Fatalf("AddMemberCtx: %v", err)
	}
	expires := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)

	rr := wfDo(t, mux, http.MethodPost, "/approvals/"+a.ID+"/delegate",
		token, `{"to_user":"member-2","note":"please handle","expires_at":"`+expires+`"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("delegate: expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := wfDecode(t, rr)
	approval, _ := body["approval"].(map[string]any)
	delegation, _ := body["delegation"].(map[string]any)
	if approval["delegated_to"] != "member-2" || approval["delegated_by"] != ownerID {
		t.Fatalf("approval assignment malformed: %v", approval)
	}
	if delegation["to_user"] != "member-2" || delegation["from_user"] != ownerID || delegation["note"] != "please handle" {
		t.Fatalf("delegation chain entry malformed: %v", delegation)
	}
	// The chain is queryable through the service (org-scoped).
	chain, err := apSvc.Delegations(ctx, orgID, a.ID)
	if err != nil || len(chain) != 1 || chain[0].ToUser != "member-2" {
		t.Fatalf("delegation chain: len=%d err=%v", len(chain), err)
	}
	if n := apEntCount(apEntAuditActions(t, auditSvc, orgID), approvals.AuditDelegated); n != 1 {
		t.Fatalf("approval.delegated audit rows = %d, want 1", n)
	}
}

func TestApprovalEnterpriseDelegateRequiresOrgMember(t *testing.T) {
	mux, authSvc, apSvc, orgsSvc, _, identities := apEntSetup(t)
	ctx := context.Background()
	orgID, _, token := apEntOwner(t, authSvc, orgsSvc, identities, "owner-member@acme.test")
	a, _ := apSvc.Request(ctx, orgID, approvals.RequestInput{Action: "deploy", Requester: "maker-1"})

	rr := wfDo(t, mux, http.MethodPost, "/approvals/"+a.ID+"/delegate", token, `{"to_user":"stranger-9"}`)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("non-member delegatee: expected 422, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "DELEGATEE_NOT_MEMBER") {
		t.Fatalf("expected DELEGATEE_NOT_MEMBER code, got %s", rr.Body.String())
	}
}

func TestApprovalEnterpriseDelegateValidation(t *testing.T) {
	mux, authSvc, apSvc, orgsSvc, _, identities := apEntSetup(t)
	ctx := context.Background()
	orgID, _, token := apEntOwner(t, authSvc, orgsSvc, identities, "owner-validate@acme.test")
	a, _ := apSvc.Request(ctx, orgID, approvals.RequestInput{Action: "deploy", Requester: "maker-1"})
	_ = orgsSvc.AddMemberCtx(ctx, orgID, "member-2", "MEMBER")

	// Missing to_user -> 422 validation error.
	if rr := wfDo(t, mux, http.MethodPost, "/approvals/"+a.ID+"/delegate", token, `{"note":"x"}`); rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("empty to_user: expected 422, got %d body=%s", rr.Code, rr.Body.String())
	}
	// Malformed expires_at -> 400 invalid request.
	if rr := wfDo(t, mux, http.MethodPost, "/approvals/"+a.ID+"/delegate", token, `{"to_user":"member-2","expires_at":"not-a-time"}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("bad expires_at: expected 400, got %d body=%s", rr.Code, rr.Body.String())
	}
	// Malformed JSON body -> 400.
	if rr := wfDo(t, mux, http.MethodPost, "/approvals/"+a.ID+"/delegate", token, `{nope`); rr.Code != http.StatusBadRequest {
		t.Fatalf("bad body: expected 400, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestApprovalEnterpriseDelegateCrossTenant404(t *testing.T) {
	mux, authSvc, apSvc, orgsSvc, auditSvc, identities := apEntSetup(t)
	ctx := context.Background()
	orgID, _, _ := apEntOwner(t, authSvc, orgsSvc, identities, "owner-ct@acme.test")
	a, _ := apSvc.Request(ctx, orgID, approvals.RequestInput{Action: "deploy", Requester: "maker-1"})

	// A second, unrelated tenant probes the approval id. The probe is a
	// WELL-FORMED request for that tenant (the delegatee is its own member),
	// so the only failure left is the tenant guard -> 404, no existence leak.
	foreignOrgID, _, foreignToken := apEntOwner(t, authSvc, orgsSvc, identities, "owner-other@corp.test")
	if err := orgsSvc.AddMemberCtx(ctx, foreignOrgID, "member-2", "MEMBER"); err != nil {
		t.Fatalf("AddMemberCtx (foreign): %v", err)
	}
	rr := wfDo(t, mux, http.MethodPost, "/approvals/"+a.ID+"/delegate", foreignToken, `{"to_user":"member-2"}`)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant delegate: expected 404, got %d body=%s", rr.Code, rr.Body.String())
	}
	// No existence leak: the foreign org's audit trail records nothing.
	if n := apEntCount(apEntAuditActions(t, auditSvc, foreignOrgID), approvals.AuditDelegated); n != 0 {
		t.Fatalf("foreign audit rows = %d, want 0", n)
	}
}

func TestApprovalEnterpriseDelegateAuthAndRBAC(t *testing.T) {
	mux, authSvc, apSvc, orgsSvc, _, identities := apEntSetup(t)
	ctx := context.Background()
	orgID, _, token := apEntOwner(t, authSvc, orgsSvc, identities, "owner-rbac@acme.test")
	a, _ := apSvc.Request(ctx, orgID, approvals.RequestInput{Action: "deploy", Requester: "maker-1"})
	_ = orgsSvc.AddMemberCtx(ctx, orgID, "member-2", "MEMBER")

	// No credentials -> 401.
	if rr := wfDo(t, mux, http.MethodPost, "/approvals/"+a.ID+"/delegate", "", `{"to_user":"member-2"}`); rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated delegate: expected 401, got %d", rr.Code)
	}
	// VIEWER lacks approvals.decide -> 403 (middleware).
	viewer := apEntToken(t, authSvc, orgID, "viewer-1", "VIEWER")
	if rr := wfDo(t, mux, http.MethodPost, "/approvals/"+a.ID+"/delegate", viewer, `{"to_user":"member-2"}`); rr.Code != http.StatusForbidden {
		t.Fatalf("viewer delegate: expected 403, got %d body=%s", rr.Code, rr.Body.String())
	}
	// Decided approvals refuse delegation (409) even for the owner.
	if _, err := apSvc.Decide(ctx, orgID, a.ID, approvals.StatusApproved, "ok", "maker-1"); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if rr := wfDo(t, mux, http.MethodPost, "/approvals/"+a.ID+"/delegate", token, `{"to_user":"member-2"}`); rr.Code != http.StatusConflict {
		t.Fatalf("decided approval delegate: expected 409, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Escalation policy + sweep
// ---------------------------------------------------------------------------

func TestApprovalEnterpriseEscalationPolicyHTTP(t *testing.T) {
	mux, authSvc, apSvc, orgsSvc, _, identities := apEntSetup(t)
	ctx := context.Background()
	orgID, userID, token := apEntOwner(t, authSvc, orgsSvc, identities, "owner-policy@acme.test")

	// Default policy: disabled (sla_minutes = 0, target pending).
	rr := wfDo(t, mux, http.MethodGet, "/approvals/escalation-policy", token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("get default policy: expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	policy, _ := wfDecode(t, rr)["policy"].(map[string]any)
	if policy["sla_minutes"].(float64) != 0 || policy["target_status"] != approvals.StatusPending {
		t.Fatalf("default policy malformed: %v", policy)
	}

	// PUT stores the SLA and records the actor from the claims.
	rr = wfDo(t, mux, http.MethodPut, "/approvals/escalation-policy", token, `{"target_status":"pending","sla_minutes":45}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("put policy: expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	policy, _ = wfDecode(t, rr)["policy"].(map[string]any)
	if policy["sla_minutes"].(float64) != 45 || policy["updated_by"] != userID {
		t.Fatalf("stored policy malformed: %v", policy)
	}
	live, _ := apSvc.GetEscalationPolicy(ctx, orgID)
	if live.SLAMinutes != 45 {
		t.Fatalf("policy not persisted by the PUT: %+v", live)
	}

	// Invalid policy payloads -> 422 VALIDATION_ERROR.
	for _, body := range []string{
		`{"target_status":"approved","sla_minutes":30}`,
		`{"sla_minutes":-1}`,
	} {
		if rr := wfDo(t, mux, http.MethodPut, "/approvals/escalation-policy", token, body); rr.Code != http.StatusUnprocessableEntity {
			t.Fatalf("invalid policy %s: expected 422, got %d body=%s", body, rr.Code, rr.Body.String())
		}
	}

	// MEMBER lacks approvals.decide -> 403 on read AND write.
	member := apEntToken(t, authSvc, orgID, "member-1", "MEMBER")
	if rr := wfDo(t, mux, http.MethodGet, "/approvals/escalation-policy", member, ""); rr.Code != http.StatusForbidden {
		t.Fatalf("member policy read: expected 403, got %d", rr.Code)
	}
	if rr := wfDo(t, mux, http.MethodPut, "/approvals/escalation-policy", member, `{"sla_minutes":5}`); rr.Code != http.StatusForbidden {
		t.Fatalf("member policy write: expected 403, got %d", rr.Code)
	}
}

func TestApprovalEnterpriseEscalateDueSweepHTTP(t *testing.T) {
	mux, authSvc, apSvc, orgsSvc, _, identities := apEntSetup(t)
	ctx := context.Background()
	orgID, _, token := apEntOwner(t, authSvc, orgsSvc, identities, "owner-sweep@acme.test")

	if _, err := apSvc.SetEscalationPolicy(ctx, orgID, approvals.StatusPending, 30, "seed"); err != nil {
		t.Fatalf("SetEscalationPolicy: %v", err)
	}
	breached, _ := apSvc.Request(ctx, orgID, approvals.RequestInput{Action: "deploy", Requester: "maker-1"})
	breached.CreatedAt = time.Now().UTC().Add(-time.Hour) // past the 30m SLA
	fresh, _ := apSvc.Request(ctx, orgID, approvals.RequestInput{Action: "deploy", Requester: "maker-1"})

	rr := wfDo(t, mux, http.MethodPost, "/approvals/escalate-due", token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("escalate-due: expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	payload := wfDecode(t, rr)
	if payload["count"].(float64) != 1 {
		t.Fatalf("sweep count = %v, want 1", payload["count"])
	}
	escalated, _ := payload["escalated"].([]any)
	if len(escalated) != 1 {
		t.Fatalf("escalated list = %d, want 1", len(escalated))
	}
	first, _ := escalated[0].(map[string]any)
	if first["id"] != breached.ID || first["escalation_reason"] != approvals.EscalationReasonSLABreach {
		t.Fatalf("wrong approval escalated: %v", first)
	}

	// Idempotent: the second sweep finds nothing new.
	rr = wfDo(t, mux, http.MethodPost, "/approvals/escalate-due", token, "")
	if rr.Code != http.StatusOK || wfDecode(t, rr)["count"].(float64) != 0 {
		t.Fatalf("second sweep must be a no-op, got %s", rr.Body.String())
	}
	// The fresh approval is untouched.
	got, _ := apSvc.Get(ctx, orgID, fresh.ID)
	if got.EscalatedAt != nil {
		t.Fatalf("fresh approval must not be escalated: %#v", got)
	}
}

// ---------------------------------------------------------------------------
// Evidence export
// ---------------------------------------------------------------------------

func TestApprovalEnterpriseEvidenceExportHTTP(t *testing.T) {
	mux, authSvc, apSvc, orgsSvc, auditSvc, identities := apEntSetup(t)
	ctx := context.Background()
	orgID, _, token := apEntOwner(t, authSvc, orgsSvc, identities, "owner-evidence@acme.test")

	// Seed an oversight trail: plain request+decide, a delegation, an SLA hit.
	a, _ := apSvc.Request(ctx, orgID, approvals.RequestInput{Action: "deploy", Requester: "maker-1"})
	if _, err := apSvc.Decide(ctx, orgID, a.ID, approvals.StatusApproved, "ok", "maker-1"); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if _, err := apSvc.SetEscalationPolicy(ctx, orgID, approvals.StatusPending, 30, "admin"); err != nil {
		t.Fatalf("SetEscalationPolicy: %v", err)
	}
	b, _ := apSvc.Request(ctx, orgID, approvals.RequestInput{Action: "rollback", Requester: "maker-2"})
	b.CreatedAt = time.Now().UTC().Add(-2 * time.Hour)
	if _, _, err := apSvc.Delegate(ctx, orgID, b.ID, "user-9", "handoff", nil, "maker-2"); err != nil {
		t.Fatalf("Delegate: %v", err)
	}

	fetch := func(query string) *httptest.ResponseRecorder {
		return wfDo(t, mux, http.MethodGet, "/approvals/evidence"+query, token, "")
	}
	rr := fetch("")
	if rr.Code != http.StatusOK {
		t.Fatalf("evidence: expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("content type = %q, want application/x-ndjson", ct)
	}
	first := rr.Body.Bytes()
	// Byte-determinism: a second export over unchanged data is identical.
	second := fetch("").Body.Bytes()
	if !bytes.Equal(first, second) {
		t.Fatal("two exports over unchanged data must be byte-identical")
	}
	// Trailing evidence_head line agrees with X-Evidence-Count.
	lines := bytes.Split(bytes.TrimRight(first, "\n"), []byte("\n"))
	var head struct {
		Type  string `json:"type"`
		Count int    `json:"count"`
	}
	if err := json.Unmarshal(lines[len(lines)-1], &head); err != nil || head.Type != "evidence_head" {
		t.Fatalf("head line malformed: %q (%v)", lines[len(lines)-1], err)
	}
	if got := rr.Header().Get("X-Evidence-Count"); got != "5" || head.Count != 5 {
		t.Fatalf("evidence count mismatch: header=%s head=%+v (want 5)", got, head)
	}
	// Event vocabulary: requested, decided, delegated and escalated all present.
	for _, want := range []string{"\"type\":\"requested\"", "\"type\":\"decided\"", "\"type\":\"delegated\"", "\"type\":\"escalated\""} {
		if !strings.Contains(string(first), want) {
			t.Fatalf("event %s missing from the export:\n%s", want, first)
		}
	}

	// Window filtering: a `to` bound before any activity yields only the head.
	early := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339)
	rr = fetch("?from=" + early + "&to=" + early)
	if rr.Code != http.StatusOK {
		t.Fatalf("windowed evidence: expected 200, got %d", rr.Code)
	}
	if rr.Header().Get("X-Evidence-Count") != "0" || !strings.Contains(rr.Body.String(), `"type":"evidence_head","count":0`) {
		t.Fatalf("empty window must carry count 0, got %s / %s", rr.Header().Get("X-Evidence-Count"), rr.Body.String())
	}

	// Bad inputs: inverted window -> 422; unknown format -> 400; bad RFC3339 -> 400.
	now := time.Now().UTC().Format(time.RFC3339)
	if rr := fetch("?from=" + now + "&to=" + early); rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("inverted window: expected 422, got %d body=%s", rr.Code, rr.Body.String())
	}
	if rr := fetch("?format=csv"); rr.Code != http.StatusBadRequest {
		t.Fatalf("bad format: expected 400, got %d", rr.Code)
	}
	if rr := fetch("?from=yesterday"); rr.Code != http.StatusBadRequest {
		t.Fatalf("bad from: expected 400, got %d", rr.Code)
	}
	// The export is a pure read but the ACCESS is audited.
	if n := apEntCount(apEntAuditActions(t, auditSvc, orgID), approvals.AuditEvidenceExported); n < 1 {
		t.Fatal("approval.evidence_exported audit row missing")
	}
}

func TestApprovalEnterpriseEvidenceRoleGate(t *testing.T) {
	mux, authSvc, apSvc, orgsSvc, _, identities := apEntSetup(t)
	ctx := context.Background()
	orgID, _, ownerToken := apEntOwner(t, authSvc, orgsSvc, identities, "owner-evrole@acme.test")
	a, _ := apSvc.Request(ctx, orgID, approvals.RequestInput{Action: "deploy", Requester: "maker-1"})
	_ = a

	// VIEWER passes the approvals.read middleware but fails the OWNER/ADMIN
	// in-handler gate.
	viewer := apEntToken(t, authSvc, orgID, "viewer-1", "VIEWER")
	if rr := wfDo(t, mux, http.MethodGet, "/approvals/evidence", viewer, ""); rr.Code != http.StatusForbidden {
		t.Fatalf("viewer evidence: expected 403, got %d body=%s", rr.Code, rr.Body.String())
	}
	// MEMBER likewise (approvals.read-level permission, but not OWNER/ADMIN).
	member := apEntToken(t, authSvc, orgID, "member-1", "MEMBER")
	if rr := wfDo(t, mux, http.MethodGet, "/approvals/evidence", member, ""); rr.Code != http.StatusForbidden {
		t.Fatalf("member evidence: expected 403, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(wfDo(t, mux, http.MethodGet, "/approvals/evidence", member, "").Body.String(), "OWNER or ADMIN") {
		t.Fatal("role-gate error message must name OWNER or ADMIN")
	}
	// No credentials -> 401 at the middleware.
	if rr := wfDo(t, mux, http.MethodGet, "/approvals/evidence", "", ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated evidence: expected 401, got %d", rr.Code)
	}
	// OWNER succeeds.
	if rr := wfDo(t, mux, http.MethodGet, "/approvals/evidence", ownerToken, ""); rr.Code != http.StatusOK {
		t.Fatalf("owner evidence: expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestApprovalEnterpriseEvidenceOrgIsolation(t *testing.T) {
	mux, authSvc, apSvc, orgsSvc, _, identities := apEntSetup(t)
	ctx := context.Background()
	orgID, _, _ := apEntOwner(t, authSvc, orgsSvc, identities, "owner-eviso@acme.test")
	a, _ := apSvc.Request(ctx, orgID, approvals.RequestInput{Action: "deploy", Requester: "maker-1"})
	if _, err := apSvc.Decide(ctx, orgID, a.ID, approvals.StatusApproved, "ok", "maker-1"); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	// A second tenant's export must be empty — never the other org's events.
	_, _, foreignToken := apEntOwner(t, authSvc, orgsSvc, identities, "owner-eviso@corp.test")
	rr := wfDo(t, mux, http.MethodGet, "/approvals/evidence", foreignToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("foreign evidence: expected 200, got %d", rr.Code)
	}
	if rr.Header().Get("X-Evidence-Count") != "0" {
		t.Fatalf("foreign export leaked events: %s", rr.Body.String())
	}
	if bytes.Contains(rr.Body.Bytes(), []byte(a.ID)) {
		t.Fatal("foreign export contains the other org's approval id")
	}
}

// ---------------------------------------------------------------------------
// Dual control over the EXISTING decide endpoint (transparent enforcement)
// ---------------------------------------------------------------------------

func TestApprovalEnterpriseDualControlOverHTTP(t *testing.T) {
	mux, authSvc, apSvc, orgsSvc, auditSvc, identities := apEntSetupWithWorkflows(t)
	ctx := context.Background()
	orgID, ownerID, ownerToken := apEntOwner(t, authSvc, orgsSvc, identities, "owner-dual@acme.test")
	secondToken := apEntGhostToken(t, authSvc, orgID, "user-second")

	a, err := apSvc.Request(ctx, orgID, approvals.RequestInput{Action: "deploy", Requester: "maker-1", RequireDual: true})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}

	// First decision parks the approval in partially_approved.
	rr := wfDo(t, mux, http.MethodPost, "/approvals/"+a.ID+"/decide", ownerToken, `{"decision":"approved","reason":"looks good"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("first decision: expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	first, _ := wfDecode(t, rr)["approval"].(map[string]any)
	if first["status"] != approvals.StatusPartiallyApproved || first["first_approver"] != ownerID {
		t.Fatalf("first decision malformed: %v", first)
	}

	// Same approver again -> 409 Conflict. The service returns the dual-control
	// block error, which satisfies BOTH the issue #137 sentinel (machine-readable
	// dual_control_self_approval mapping) and the wave-2 ErrAlreadyDecided
	// sentinel the EXISTING decide handler maps — the message carries the
	// two-person-rule identity over HTTP (see integrator note: the
	// workflows.go mapper may additionally surface the literal code once the
	// file-ownership restriction lifts).
	rr = wfDo(t, mux, http.MethodPost, "/approvals/"+a.ID+"/decide", ownerToken, `{"decision":"approved","reason":"again"}`)
	if rr.Code != http.StatusConflict {
		t.Fatalf("self approval: expected 409, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "dual control requires an approver distinct") {
		t.Fatalf("expected dual-control block message, got %s", rr.Body.String())
	}

	// The maker cannot complete their own dual-controlled request either.
	makerToken := apEntGhostToken(t, authSvc, orgID, "maker-1")
	rr = wfDo(t, mux, http.MethodPost, "/approvals/"+a.ID+"/decide", makerToken, `{"decision":"approved","reason":"self"}`)
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "dual control requires an approver distinct") {
		t.Fatalf("requester completion must be blocked, got %d body=%s", rr.Code, rr.Body.String())
	}

	// A DISTINCT second approver completes it (reject works symmetrically
	// below; here approve) — the pair is visible on the final record.
	rr = wfDo(t, mux, http.MethodPost, "/approvals/"+a.ID+"/decide", secondToken, `{"decision":"approved","reason":"second pair of eyes"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("second decision: expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	done, _ := wfDecode(t, rr)["approval"].(map[string]any)
	if done["status"] != approvals.StatusApproved || done["approver"] != "user-second" || done["first_approver"] != ownerID {
		t.Fatalf("completed dual approval malformed: %v", done)
	}
	actions := apEntAuditActions(t, auditSvc, orgID)
	if n := apEntCount(actions, approvals.AuditFirstDecision); n != 1 {
		t.Fatalf("approval.first_decision audit rows = %d, want 1", n)
	}
	if n := apEntCount(actions, approvals.AuditCompletedDual); n != 1 {
		t.Fatalf("approval.completed_dual audit rows = %d, want 1", n)
	}
}

func TestApprovalEnterpriseDualControlRejectOverHTTP(t *testing.T) {
	mux, authSvc, apSvc, orgsSvc, _, identities := apEntSetupWithWorkflows(t)
	ctx := context.Background()
	orgID, _, ownerToken := apEntOwner(t, authSvc, orgsSvc, identities, "owner-dualrej@acme.test")
	secondToken := apEntGhostToken(t, authSvc, orgID, "user-rejector")

	a, _ := apSvc.Request(ctx, orgID, approvals.RequestInput{Action: "deploy", Requester: "maker-1", RequireDual: true})

	// First decision REJECTS; the second approver CONFIRMS the rejection.
	if rr := wfDo(t, mux, http.MethodPost, "/approvals/"+a.ID+"/decide", ownerToken, `{"decision":"rejected","reason":"unsafe"}`); rr.Code != http.StatusOK {
		t.Fatalf("first rejection: expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	rr := wfDo(t, mux, http.MethodPost, "/approvals/"+a.ID+"/decide", secondToken, `{"decision":"rejected","reason":"confirmed"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("second rejection: expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	done, _ := wfDecode(t, rr)["approval"].(map[string]any)
	if done["status"] != approvals.StatusRejected {
		t.Fatalf("dual rejection must complete as rejected: %v", done)
	}

	// A non-dual approval keeps its single-decision behavior (no regression).
	plain, _ := apSvc.Request(ctx, orgID, approvals.RequestInput{Action: "ping", Requester: "maker-1"})
	rr = wfDo(t, mux, http.MethodPost, "/approvals/"+plain.ID+"/decide", ownerToken, `{"decision":"approved","reason":"solo"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("plain decide: expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	if st, _ := wfDecode(t, rr)["approval"].(map[string]any); st["status"] != approvals.StatusApproved {
		t.Fatalf("plain approval must complete on the first decision: %v", st)
	}
}
