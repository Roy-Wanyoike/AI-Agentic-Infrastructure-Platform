package main

// Issue #51 handler tests: the canary_policy field on the canary surface
// (attach validation + fresh window) and the additive GET
// /agents/{agentId}/canary/status read model (RBAC runs.read, environment
// scoping, decision + stats payload). The full eval -> decision loop is
// covered end-to-end through WireCanaryAutoPromotion with the real
// evaluations service.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"agentos/internal/audit"
	"agentos/internal/deployments"
	"agentos/internal/evaluations"
	"agentos/internal/runtime"
)

// statusOf fetches the canary status view for the env's agent via the HTTP
// route (default environment=production).
func statusOf(t *testing.T, env *versionsHandlerEnv, token string) map[string]any {
	t.Helper()
	rr, resp := env.do(t, http.MethodGet, "/agents/"+env.agentID+"/canary/status", token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("canary status: expected %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	status, _ := resp["canary_status"].(map[string]any)
	if status == nil {
		t.Fatalf("expected a canary_status object, got %v", resp)
	}
	return status
}

func TestCanaryPolicyAttachAndStatusHTTP(t *testing.T) {
	env := newVersionsHandlerEnv(t)
	versions := seedPublishedVersions(t, env, 2)
	stable, canary := versions[1], versions[0]

	// Validation first: an out-of-domain policy is rejected at create...
	rr, body := env.do(t, http.MethodPost, "/deployments/create", env.ownerToken,
		`{"agent_id":"`+env.agentID+`","version":`+strconv.Itoa(stable)+`,"environment":"production","canary_version":`+strconv.Itoa(canary)+`,"canary_policy":{"min_pass_rate":1.5,"min_canary_runs":1}}`)
	if rr.Code != http.StatusUnprocessableEntity || errCode(t, body) != "VALIDATION_ERROR" {
		t.Fatalf("invalid policy at create: expected 422 VALIDATION_ERROR, got %d %v", rr.Code, body)
	}
	// ...and a policy without a canary_version is a validation error too.
	rr, body = env.do(t, http.MethodPost, "/deployments/create", env.ownerToken,
		`{"agent_id":"`+env.agentID+`","version":`+strconv.Itoa(stable)+`,"environment":"production","canary_policy":{"min_pass_rate":0.8,"min_canary_runs":1}}`)
	if rr.Code != http.StatusUnprocessableEntity || errCode(t, body) != "VALIDATION_ERROR" {
		t.Fatalf("policy without canary at create: expected 422, got %d %v", rr.Code, body)
	}

	// Create the healthy deployment WITH a staged canary + policy in one body.
	rr, body = env.do(t, http.MethodPost, "/deployments/create", env.ownerToken,
		`{"agent_id":"`+env.agentID+`","version":`+strconv.Itoa(stable)+`,"environment":"production","canary_version":`+strconv.Itoa(canary)+`,"canary_weight":20,"canary_policy":{"min_pass_rate":0.8,"min_canary_runs":3,"max_p95_latency_ms":500}}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create with policy: expected %d, got %d body=%s", http.StatusCreated, rr.Code, rr.Body.String())
	}
	dep, _ := body["deployment"].(map[string]any)
	id, _ := dep["id"].(string)
	for range 3 {
		if rr, body = env.do(t, http.MethodPost, "/deployments/"+id+"/promote", env.ownerToken, ""); rr.Code != http.StatusOK {
			t.Fatalf("promote: got %d body=%s", rr.Code, body)
		}
	}

	// The status read model (runs.read: every role may read it).
	status := statusOf(t, env, env.ownerToken)
	if status["canary_active"] != true || status["canary_version"].(float64) != float64(canary) || status["canary_weight"].(float64) != 20 {
		t.Fatalf("unexpected split view: %v", status)
	}
	policy, _ := status["policy"].(map[string]any)
	if policy == nil || policy["min_pass_rate"].(float64) != 0.8 || policy["min_canary_runs"].(float64) != 3 || policy["max_p95_latency_ms"].(float64) != 500 {
		t.Fatalf("policy should surface on the status view, got %v", status)
	}
	if _, ok := status["decision"]; ok {
		t.Fatalf("no decision may exist before the engine runs, got %v", status)
	}
	if _, ok := status["window_start"]; !ok {
		t.Fatalf("attach should have opened the evaluation window, got %v", status)
	}
	if _, ok := status["stats"]; ok {
		t.Fatalf("no sample source wired -> no stats, got %v", status)
	}
	// VIEWER (runs.read) may read the status too.
	statusOf(t, env, env.viewerToken)
	// MEMBER may read it (runs.read grants MEMBER+).
	statusOf(t, env, env.memberToken)

	// Environment scoping: unknown env -> 422; env without a healthy row -> 404.
	rr, body = env.do(t, http.MethodGet, "/agents/"+env.agentID+"/canary/status?environment=chaos", env.ownerToken, "")
	if rr.Code != http.StatusUnprocessableEntity || errCode(t, body) != "VALIDATION_ERROR" {
		t.Fatalf("unknown env: expected 422, got %d %v", rr.Code, body)
	}
	rr, body = env.do(t, http.MethodGet, "/agents/"+env.agentID+"/canary/status?environment=staging", env.ownerToken, "")
	if rr.Code != http.StatusNotFound || errCode(t, body) != "NOT_FOUND" {
		t.Fatalf("missing env: expected 404 NOT_FOUND, got %d %v", rr.Code, body)
	}
	// Cross-tenant: the foreign OWNER sees nothing (404, no existence leak).
	rr, body = env.do(t, http.MethodGet, "/agents/"+env.agentID+"/canary/status", env.otherToken, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant status: expected 404, got %d %v", rr.Code, body)
	}

	// Without a canary the status still answers (canary_active=false, no policy).
	rr, body = env.do(t, http.MethodPost, "/deployments/"+id+"/canary/abort", env.ownerToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("abort: got %d body=%s", rr.Code, body)
	}
	status = statusOf(t, env, env.ownerToken)
	if status["canary_active"] != false {
		t.Fatalf("post-abort status should show no canary, got %v", status)
	}
	// The policy is the operator's STANDING rule for the row: it survives
	// abort so the next canary attach is eval-gated again (the engine only
	// ever acts while a canary is active).
	if _, ok := status["policy"]; !ok {
		t.Fatalf("post-abort the standing policy must remain configured, got %v", status)
	}
}

// TestCanaryAutoPromotionEndToEndHTTP runs the full loop: policy attach ->
// two completed eval runs -> engine promotes -> decision + reason on the
// status endpoint + one audit entry. Also covers the flag-off no-op.
func TestCanaryAutoPromotionEndToEndHTTP(t *testing.T) {
	t.Setenv(deployments.AutoPromoteEnvVar, "true")

	env := newVersionsHandlerEnv(t)
	versions := seedPublishedVersions(t, env, 2)
	stable, canary := versions[1], versions[0]
	id := healthyDeployment(t, env, stable, `,"canary_version":`+strconv.Itoa(canary)+`,"canary_weight":10`)

	// Wire the loop exactly like newApp would (reported wiring diff): eval
	// service + deployments service + in-memory audit.
	evalSvc := evaluations.NewService(evaluations.Deps{
		Agents: env.agentsSvc,
		Runner: &stubEvalRunner{fn: func(context.Context, string, string) (*runtime.Run, error) {
			return &runtime.Run{Status: runtime.StatusCompleted, Output: "ok"}, nil
		}},
		CaseTimeout: 2 * time.Second,
	})
	auditSvc := audit.NewService()
	WireCanaryAutoPromotion(evalSvc, env.depSvc, auditSvc, slog.Default())

	// Attach the promotion policy through the HTTP canary surface.
	if rr, body := env.do(t, http.MethodPost, "/deployments/"+id+"/canary", env.ownerToken,
		`{"canary_policy":{"min_pass_rate":0.5,"min_canary_runs":2}}`); rr.Code != http.StatusOK {
		t.Fatalf("attach policy: expected %d, got %d body=%s", http.StatusOK, rr.Code, body)
	}

	ds, err := evalSvc.CreateDataset(context.Background(), env.orgID, "canary gate", "", []evaluations.Case{
		{ID: "c1", Input: "q1", Expected: "ok", Scorer: evaluations.ScorerExact},
		{ID: "c2", Input: "q2", Expected: "ok", Scorer: evaluations.ScorerExact},
	})
	if err != nil {
		t.Fatalf("CreateDataset returned error: %v", err)
	}
	var status map[string]any
	// First run: evidence gate still shut. The canary is still ACTIVE, so
	// the status surfaces fresh sample stats (the operator's "why hasn't it
	// promoted yet" view).
	if _, err := evalSvc.RunDataset(context.Background(), env.orgID, ds.ID, env.agentID); err != nil {
		t.Fatalf("RunDataset(1) returned error: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		status = statusOf(t, env, env.ownerToken)
		if stats, ok := status["stats"]; ok && stats.(map[string]any)["runs_counted"].(float64) >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sample stats never surfaced for the active canary; status=%v", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, ok := status["decision"]; ok {
		t.Fatalf("one run must not satisfy min_canary_runs=2, got %v", status["decision"])
	}
	// Second run: the evidence gate opens -> promote.
	if _, err := evalSvc.RunDataset(context.Background(), env.orgID, ds.ID, env.agentID); err != nil {
		t.Fatalf("RunDataset(2) returned error: %v", err)
	}

	deadline = time.Now().Add(3 * time.Second)
	for {
		status = statusOf(t, env, env.ownerToken)
		if _, ok := status["decision"]; ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("auto-promotion never landed; status=%v", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	decision, _ := status["decision"].(map[string]any)
	if decision["action"] != deployments.CanaryDecisionPromote {
		t.Fatalf("expected auto promote, got %v", decision)
	}
	if decision["reason"] != "pass_rate 1.00 >= 0.50 → promote" {
		t.Fatalf("unexpected decision reason: %v", decision["reason"])
	}
	// PROMOTE = the canary became stable: stable_version swapped to the canary
	// version and the canary config cleared (serves 100%). With no active
	// canary the fresh-stats block is intentionally omitted (it is the
	// "why hasn't it promoted yet" view of an ACTIVE canary).
	if status["stable_version"].(float64) != float64(canary) || status["canary_active"] != false {
		t.Fatalf("post-promote the canary version serves 100%%: %v", status)
	}
	if _, ok := status["stats"]; ok {
		t.Fatalf("post-promote no active canary -> no stats, got %v", status)
	}

	// Exactly one audit entry for the automatic decision.
	entries := 0
	for _, e := range auditSvc.List() {
		if e.Action == "deployment.canary_auto_promote" {
			entries++
			if e.OrganizationID != env.orgID || e.Resource != id || e.Metadata["reason"] != "pass_rate 1.00 >= 0.50 → promote" {
				t.Fatalf("unexpected audit entry: %+v", e)
			}
		}
	}
	if entries != 1 {
		t.Fatalf("expected exactly one canary_auto_promote audit entry, got %d", entries)
	}
}

// TestCanaryAutoRollbackEndToEndHTTP: failing evals under an active policy
// roll the canary back (stable keeps serving) with a persisted reason.
func TestCanaryAutoRollbackEndToEndHTTP(t *testing.T) {
	t.Setenv(deployments.AutoPromoteEnvVar, "true")

	env := newVersionsHandlerEnv(t)
	versions := seedPublishedVersions(t, env, 2)
	stable, canary := versions[1], versions[0]
	id := healthyDeployment(t, env, stable, `,"canary_version":`+strconv.Itoa(canary)+`,"canary_weight":50`)

	evalSvc := evaluations.NewService(evaluations.Deps{
		Agents: env.agentsSvc,
		Runner: &stubEvalRunner{fn: func(context.Context, string, string) (*runtime.Run, error) {
			return &runtime.Run{Status: runtime.StatusCompleted, Output: "wrong"}, nil
		}},
		CaseTimeout: 2 * time.Second,
	})
	auditSvc := audit.NewService()
	WireCanaryAutoPromotion(evalSvc, env.depSvc, auditSvc, slog.Default())

	if _, err := evalSvc.CreateDataset(context.Background(), env.orgID, "canary gate", "", []evaluations.Case{
		{ID: "c1", Input: "q1", Expected: "ok", Scorer: evaluations.ScorerExact},
	}); err != nil {
		t.Fatalf("CreateDataset returned error: %v", err)
	}
	if rr, body := env.do(t, http.MethodPost, "/deployments/"+id+"/canary", env.ownerToken,
		`{"canary_policy":{"min_pass_rate":0.8,"min_canary_runs":1}}`); rr.Code != http.StatusOK {
		t.Fatalf("attach policy: expected %d, got %d body=%s", http.StatusOK, rr.Code, body)
	}

	// One failing eval run: pass rate 0.00 < 0.80 -> rollback.
	dsID := mustCanaryDataset(t, evalSvc, env.orgID)
	if _, err := evalSvc.RunDataset(context.Background(), env.orgID, dsID, env.agentID); err != nil {
		t.Fatalf("RunDataset returned error: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	var status map[string]any
	for {
		status = statusOf(t, env, env.ownerToken)
		if _, ok := status["decision"]; ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("auto-rollback never landed; status=%v", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	decision, _ := status["decision"].(map[string]any)
	if decision["action"] != deployments.CanaryDecisionRollback ||
		decision["reason"] != "pass_rate 0.00 < 0.80 → rollback" {
		t.Fatalf("unexpected rollback decision: %v", decision)
	}
	// ROLLBACK = revert to baseline: the stable version keeps serving and the
	// canary config is cleared.
	if status["stable_version"].(float64) != float64(stable) || status["canary_active"] != false {
		t.Fatalf("rollback keeps the stable version serving: %v", status)
	}
	if !strings.Contains(decision["reason"].(string), "rollback") {
		t.Fatalf("reason should name the action: %v", decision["reason"])
	}
	raw, _ := json.Marshal(auditSvc.List())
	if !strings.Contains(string(raw), "deployment.canary_auto_rollback") {
		t.Fatalf("rollback audit entry missing: %s", raw)
	}
}

// mustCanaryDataset creates a one-case failing dataset for the rollback test.
func mustCanaryDataset(t *testing.T, evalSvc *evaluations.Service, orgID string) string {
	t.Helper()
	ds, err := evalSvc.CreateDataset(context.Background(), orgID, "canary gate 2", "", []evaluations.Case{
		{ID: "c1", Input: "q1", Expected: "ok", Scorer: evaluations.ScorerExact},
	})
	if err != nil {
		t.Fatalf("CreateDataset returned error: %v", err)
	}
	return ds.ID
}

// TestCanaryAutoPromotionFlagOffEndToEnd: with the flag unset (the default)
// the wired engine is a full no-op — eval runs complete, the canary stays
// untouched, no decision, no audit entry.
func TestCanaryAutoPromotionFlagOffEndToEnd(t *testing.T) {
	env := newVersionsHandlerEnv(t)
	versions := seedPublishedVersions(t, env, 2)
	stable, canary := versions[1], versions[0]
	id := healthyDeployment(t, env, stable, `,"canary_version":`+strconv.Itoa(canary)+`,"canary_weight":40`)

	evalSvc := evaluations.NewService(evaluations.Deps{
		Agents: env.agentsSvc,
		Runner: &stubEvalRunner{fn: func(context.Context, string, string) (*runtime.Run, error) {
			return &runtime.Run{Status: runtime.StatusCompleted, Output: "terrible"}, nil
		}},
		CaseTimeout: 2 * time.Second,
	})
	auditSvc := audit.NewService()
	WireCanaryAutoPromotion(evalSvc, env.depSvc, auditSvc, slog.Default())

	if rr, body := env.do(t, http.MethodPost, "/deployments/"+id+"/canary", env.ownerToken,
		`{"canary_policy":{"min_pass_rate":0.8,"min_canary_runs":1}}`); rr.Code != http.StatusOK {
		t.Fatalf("attach policy: expected %d, got %d body=%s", http.StatusOK, rr.Code, body)
	}
	ds, err := evalSvc.CreateDataset(context.Background(), env.orgID, "canary gate", "", []evaluations.Case{
		{ID: "c1", Input: "q1", Expected: "ok", Scorer: evaluations.ScorerExact},
	})
	if err != nil {
		t.Fatalf("CreateDataset returned error: %v", err)
	}
	// A failing eval run completes; the engine must not act (flag off).
	if _, err := evalSvc.RunDataset(context.Background(), env.orgID, ds.ID, env.agentID); err != nil {
		t.Fatalf("RunDataset returned error: %v", err)
	}
	time.Sleep(100 * time.Millisecond) // any async trigger would have landed

	status := statusOf(t, env, env.ownerToken)
	if status["canary_active"] != true || status["canary_weight"].(float64) != 40 {
		t.Fatalf("flag off must leave the canary untouched, got %v", status)
	}
	if _, ok := status["decision"]; ok {
		t.Fatalf("flag off must not record a decision, got %v", status["decision"])
	}
	if len(auditSvc.List()) != 0 {
		t.Fatalf("flag off must not audit decisions, got %v", auditSvc.List())
	}
}
