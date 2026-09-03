package main

// Canary deployment handler tests (issue #13): auth (401), RBAC (VIEWER/MEMBER
// lack deployments.deploy), the full canary flow through the registered
// middleware chain (attach -> ramp -> promote/abort), validation errors, and
// the canary fields in deployment responses. Reuses the versionsHandlerEnv
// from versions_test.go (same package; identical wiring to main.go routes()).

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
)

// seedPublishedVersions creates and publishes n config versions on the env's
// agent and returns their version numbers (starting at the legacy v1 + 1).
// NOTE: publishing a version ARCHIVES the previously published one, so after
// seeding only the LAST returned number is still published; the earlier ones
// remain existing-but-archived (still valid CANARY targets, never stable
// deployment targets).
func seedPublishedVersions(t *testing.T, env *versionsHandlerEnv, n int) []int {
	t.Helper()
	numbers := make([]int, 0, n)
	for range n {
		version, err := env.versionsSvc.CreateVersionCtx(context.Background(), env.orgID, env.agentID, "user-1")
		if err != nil {
			t.Fatalf("seed version failed: %v", err)
		}
		if _, err := env.versionsSvc.PublishVersionCtx(context.Background(), env.orgID, env.agentID, version.Version, "user-1"); err != nil {
			t.Fatalf("seed publish failed: %v", err)
		}
		numbers = append(numbers, version.Version)
	}
	return numbers
}

// healthyDeployment creates a deployment for a published version and promotes
// it to healthy through the HTTP API (exercises the real chain), returning the
// deployment id.
func healthyDeployment(t *testing.T, env *versionsHandlerEnv, version int, bodyExtra string) string {
	t.Helper()
	body := `{"agent_id":"` + env.agentID + `","version":` + strconv.Itoa(version) + `,"environment":"production"` + bodyExtra + `}`
	rr, resp := env.do(t, http.MethodPost, "/deployments/create", env.ownerToken, body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create deployment: expected %d, got %d body=%s", http.StatusCreated, rr.Code, rr.Body.String())
	}
	dep, _ := resp["deployment"].(map[string]any)
	id, _ := dep["id"].(string)
	for range 3 {
		if rr, resp = env.do(t, http.MethodPost, "/deployments/"+id+"/promote", env.ownerToken, ""); rr.Code != http.StatusOK {
			t.Fatalf("promote: expected %d, got %d body=%s", http.StatusOK, rr.Code, resp)
		}
	}
	return id
}

func TestCanaryEndpointsRequireAuth(t *testing.T) {
	env := newVersionsHandlerEnv(t)
	paths := []struct{ method, path string }{
		{http.MethodPost, "/deployments/dep-1/canary"},
		{http.MethodPost, "/deployments/dep-1/canary/promote"},
		{http.MethodPost, "/deployments/dep-1/canary/abort"},
	}
	for _, p := range paths {
		rr, _ := env.do(t, p.method, p.path, "", "")
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without credentials: expected %d, got %d body=%s", p.method, p.path, http.StatusUnauthorized, rr.Code, rr.Body.String())
		}
	}
}

func TestCanaryRBACOwnerOnly(t *testing.T) {
	env := newVersionsHandlerEnv(t)
	versions := seedPublishedVersions(t, env, 2)
	// Only the LAST seeded version is still published (see seedPublishedVersions).
	id := healthyDeployment(t, env, versions[1], ``)

	// VIEWER and MEMBER lack deployments.deploy: every traffic-changing
	// canary operation is 403 (OWNER/ADMIN only, like promote/rollback).
	for _, token := range []string{env.viewerToken, env.memberToken} {
		for _, p := range []struct{ method, path, body string }{
			{http.MethodPost, "/deployments/" + id + "/canary", `{"canary_weight":50}`},
			{http.MethodPost, "/deployments/" + id + "/canary/promote", ""},
			{http.MethodPost, "/deployments/" + id + "/canary/abort", ""},
		} {
			rr, _ := env.do(t, p.method, p.path, token, p.body)
			if rr.Code != http.StatusForbidden {
				t.Fatalf("%s %s without deployments.deploy: expected %d, got %d body=%s", p.method, p.path, http.StatusForbidden, rr.Code, rr.Body.String())
			}
		}
	}
	// OWNER passes the middleware chain (the handler then validates state).
	rr, _ := env.do(t, http.MethodPost, "/deployments/"+id+"/canary", env.ownerToken, `{"canary_weight":50}`)
	// No canary attached yet -> 409 INVALID_STATE (not 403).
	if rr.Code != http.StatusConflict {
		t.Fatalf("owner set weight without canary: expected %d, got %d body=%s", http.StatusConflict, rr.Code, rr.Body.String())
	}
}

func TestCanaryHandlerFlow(t *testing.T) {
	env := newVersionsHandlerEnv(t)
	versions := seedPublishedVersions(t, env, 2)
	// Stable = the LAST seeded version (still published); the canary is the
	// earlier archived version (canaries may target any existing version of
	// the same agent - see internal/deployments/canary.go).
	stable, canary := versions[1], versions[0]

	// Create a deployment WITH a staged canary config (v<canary> @10%).
	rr, resp := env.do(t, http.MethodPost, "/deployments/create", env.ownerToken,
		`{"agent_id":"`+env.agentID+`","version":`+strconv.Itoa(stable)+`,"environment":"production","canary_version":`+strconv.Itoa(canary)+`,"canary_weight":10}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create with canary: expected %d, got %d body=%s", http.StatusCreated, rr.Code, rr.Body.String())
	}
	dep, _ := resp["deployment"].(map[string]any)
	id, _ := dep["id"].(string)
	if dep["canary_version"].(float64) != float64(canary) || dep["canary_weight"].(float64) != 10 {
		t.Fatalf("create should stage the canary config, got %v", dep)
	}
	for range 3 {
		if rr, resp = env.do(t, http.MethodPost, "/deployments/"+id+"/promote", env.ownerToken, ""); rr.Code != http.StatusOK {
			t.Fatalf("promote: got %d body=%s", rr.Code, resp)
		}
	}

	// Ramp the split point to 50% (canary_version omitted -> kept).
	rr, resp = env.do(t, http.MethodPost, "/deployments/"+id+"/canary", env.ownerToken, `{"canary_weight":50}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("set weight: expected %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	dep, _ = resp["deployment"].(map[string]any)
	if dep["canary_version"].(float64) != float64(canary) || dep["canary_weight"].(float64) != 50 {
		t.Fatalf("weight update must keep the canary version, got %v", dep)
	}

	// GET single and list both carry the canary fields.
	rr, resp = env.do(t, http.MethodGet, "/deployments/"+id, env.ownerToken, "")
	dep, _ = resp["deployment"].(map[string]any)
	if _, ok := dep["canary_version"]; !ok {
		t.Fatalf("deployment response must include canary_version, got %v", dep)
	}
	if _, ok := dep["canary_weight"]; !ok {
		t.Fatalf("deployment response must include canary_weight, got %v", dep)
	}
	rr, resp = env.do(t, http.MethodGet, "/deployments?agent_id="+env.agentID, env.ownerToken, "")
	raw, _ := json.Marshal(resp["deployments"])
	var views []map[string]any
	if err := json.Unmarshal(raw, &views); err != nil || len(views) != 1 {
		t.Fatalf("expected 1 deployment, got %s (%v)", raw, err)
	}
	if views[0]["canary_version"].(float64) != float64(canary) || views[0]["canary_weight"].(float64) != 50 {
		t.Fatalf("list should carry canary fields, got %v", views[0])
	}

	// Promote the canary: stable swaps to the canary version, config cleared.
	rr, resp = env.do(t, http.MethodPost, "/deployments/"+id+"/canary/promote", env.ownerToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("canary promote: expected %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	dep, _ = resp["deployment"].(map[string]any)
	if dep["version"].(float64) != float64(canary) || dep["canary_version"].(float64) != 0 || dep["canary_weight"].(float64) != 0 || dep["status"] != "healthy" {
		t.Fatalf("promote should swap stable=canary and clear config, got %v", dep)
	}

	// Promote/abort without a canary -> 409 INVALID_STATE.
	rr, resp = env.do(t, http.MethodPost, "/deployments/"+id+"/canary/promote", env.ownerToken, "")
	if rr.Code != http.StatusConflict || errCode(t, resp) != "INVALID_STATE" {
		t.Fatalf("promote without canary: expected 409 INVALID_STATE, got %d %v", rr.Code, resp)
	}

	// Abort clears the canary and keeps the stable version.
	rr, _ = env.do(t, http.MethodPost, "/deployments/"+id+"/canary", env.ownerToken, `{"canary_version":`+strconv.Itoa(stable)+`,"canary_weight":25}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("attach canary v%d: got %d", stable, rr.Code)
	}
	rr, resp = env.do(t, http.MethodPost, "/deployments/"+id+"/canary/abort", env.ownerToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("abort: expected %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	dep, _ = resp["deployment"].(map[string]any)
	if dep["version"].(float64) != float64(canary) || dep["canary_version"].(float64) != 0 || dep["canary_weight"].(float64) != 0 {
		t.Fatalf("abort must clear canary and keep stable, got %v", dep)
	}
}

func TestCanaryHandlerValidation(t *testing.T) {
	env := newVersionsHandlerEnv(t)
	versions := seedPublishedVersions(t, env, 2)
	// Deploy the LAST seeded version (the only one still published).
	id := healthyDeployment(t, env, versions[1], ``)

	cases := []struct {
		name, path, body, code string
		status                 int
	}{
		// NOTE: order matters - "weight without canary" must run before
		// any case attaches a canary version to this deployment.
		{"empty canary body", "/deployments/" + id + "/canary", `{}`, "VALIDATION_ERROR", http.StatusUnprocessableEntity},
		{"malformed canary body", "/deployments/" + id + "/canary", `{"canary_weight":`, "INVALID_REQUEST", http.StatusBadRequest},
		{"weight without canary", "/deployments/" + id + "/canary", `{"canary_weight":25}`, "INVALID_STATE", http.StatusConflict},
		// The weight range is validated before the canary-presence check,
		// so these never mutate the deployment.
		{"weight below 0", "/deployments/" + id + "/canary", `{"canary_weight":-1}`, "VALIDATION_ERROR", http.StatusUnprocessableEntity},
		{"weight above 100", "/deployments/" + id + "/canary", `{"canary_weight":101}`, "VALIDATION_ERROR", http.StatusUnprocessableEntity},
		// A canary version that does not exist under this agent (unknown
		// or cross-agent) is a validation error, like canary == stable.
		{"unknown canary version", "/deployments/" + id + "/canary", `{"canary_version":9}`, "VALIDATION_ERROR", http.StatusUnprocessableEntity},
		{"canary equals stable", "/deployments/" + id + "/canary", `{"canary_version":` + strconv.Itoa(versions[1]) + `}`, "VALIDATION_ERROR", http.StatusUnprocessableEntity},
		{"weight without version at create", "/deployments/create", `{"agent_id":"` + env.agentID + `","version":` + strconv.Itoa(versions[1]) + `,"environment":"production","canary_weight":10}`, "VALIDATION_ERROR", http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		rr, body := env.do(t, http.MethodPost, tc.path, env.ownerToken, tc.body)
		if rr.Code != tc.status || errCode(t, body) != tc.code {
			t.Fatalf("%s: expected %d %s, got %d %v", tc.name, tc.status, tc.code, rr.Code, body)
		}
	}

	// Unknown deployment id -> 404 on every canary route.
	for _, path := range []string{
		"/deployments/dep-missing/canary",
		"/deployments/dep-missing/canary/promote",
		"/deployments/dep-missing/canary/abort",
	} {
		body := ""
		if path == "/deployments/dep-missing/canary" {
			body = `{"canary_weight":10}`
		}
		rr, resp := env.do(t, http.MethodPost, path, env.ownerToken, body)
		if rr.Code != http.StatusNotFound || errCode(t, resp) != "DEPLOYMENT_NOT_FOUND" {
			t.Fatalf("%s: expected 404 DEPLOYMENT_NOT_FOUND, got %d %v", path, rr.Code, resp)
		}
	}

	// Cross-tenant access surfaces as 404 (tenant guard).
	rr, resp := env.do(t, http.MethodPost, "/deployments/"+id+"/canary", env.otherToken, `{"canary_weight":10}`)
	if rr.Code != http.StatusNotFound || errCode(t, resp) != "DEPLOYMENT_NOT_FOUND" {
		t.Fatalf("cross-tenant canary set: expected 404 DEPLOYMENT_NOT_FOUND, got %d %v", rr.Code, resp)
	}

	// Canary ops are rejected on non-healthy rows (staged config only).
	rr, resp = env.do(t, http.MethodPost, "/deployments/create", env.ownerToken,
		`{"agent_id":"`+env.agentID+`","version":`+strconv.Itoa(versions[1])+`,"environment":"staging","canary_version":`+strconv.Itoa(versions[0])+`,"canary_weight":5}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create staged canary: expected %d, got %d body=%s", http.StatusCreated, rr.Code, rr.Body.String())
	}
	staged, _ := resp["deployment"].(map[string]any)
	stagedID, _ := staged["id"].(string)
	for _, path := range []string{
		"/deployments/" + stagedID + "/canary",
		"/deployments/" + stagedID + "/canary/promote",
		"/deployments/" + stagedID + "/canary/abort",
	} {
		rr, resp = env.do(t, http.MethodPost, path, env.ownerToken, `{"canary_weight":5}`)
		if rr.Code != http.StatusConflict || errCode(t, resp) != "INVALID_STATE" {
			t.Fatalf("%s on requested row: expected 409 INVALID_STATE, got %d %v", path, rr.Code, resp)
		}
	}
}
