package main

// workflow_validate_test.go — issue #109: POST /workflows/validate endpoint
// tests (auth gate, valid DSL, typed 422 errors incl. control-flow rules,
// both body shapes, malformed body).

import (
	"net/http"
	"strings"
	"testing"

	"agentos/internal/apikeys"
	"agentos/internal/auth"
	"agentos/internal/workflows"
)

// wfValidateSetup registers the issue #109 validation route exactly like the
// orchestrator will (single register line next to registerResidencyRoutes).
func wfValidateSetup(t *testing.T) (*http.ServeMux, *auth.Service) {
	t.Helper()
	authSvc := auth.NewService("wf-validate-secret")
	apiKeysSvc := apikeys.NewService()
	wfSvc := workflows.NewService()
	mux := http.NewServeMux()
	registerWorkflowValidateRoutes(mux, wfSvc, authSvc, apiKeysSvc)
	return mux, authSvc
}

func TestWorkflowValidateRequiresAuth(t *testing.T) {
	mux, _ := wfValidateSetup(t)
	rr := wfDo(t, mux, http.MethodPost, "/workflows/validate", "", `{"dsl":{"nodes":[],"edges":[]}}`)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated validate = %d, want 401", rr.Code)
	}
}

func TestWorkflowValidateAcceptsValidDSL(t *testing.T) {
	mux, authSvc := wfValidateSetup(t)
	_, _, token := wfRegisterOwner(t, authSvc, "validate-ok@acme.test")
	body := `{"dsl":` + wfTestDSL + `}`
	rr := wfDo(t, mux, http.MethodPost, "/workflows/validate", token, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("valid DSL = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	payload := wfDecode(t, rr)
	if payload["valid"] != true {
		t.Fatalf("response should carry valid=true, got %v", payload)
	}
}

func TestWorkflowValidateAcceptsBareDSLBody(t *testing.T) {
	mux, authSvc := wfValidateSetup(t)
	_, _, token := wfRegisterOwner(t, authSvc, "validate-bare@acme.test")
	rr := wfDo(t, mux, http.MethodPost, "/workflows/validate", token, wfTestDSL)
	if rr.Code != http.StatusOK {
		t.Fatalf("bare DSL body = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
}

func TestWorkflowValidateReturnsTypedErrors(t *testing.T) {
	mux, authSvc := wfValidateSetup(t)
	_, _, token := wfRegisterOwner(t, authSvc, "validate-bad@acme.test")
	// Dangling condition branch target + invalid expression + unknown edge.
	body := `{"dsl":{"nodes":[
                {"id":"c1","type":"condition","config":{"expression":"$.nodes.a.output","branches":{"true":"ghost"}}},
                {"id":"s1","type":"switch","config":{"expression":"len(x) > 0","cases":{"a":"missing-target"}}}
        ],"edges":[{"from":"c1","to":"nowhere","condition":"on_success"}]}}`
	rr := wfDo(t, mux, http.MethodPost, "/workflows/validate", token, body)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid DSL = %d, want 422 (body=%s)", rr.Code, rr.Body.String())
	}
	payload := wfDecode(t, rr)
	errs, ok := payload["errors"].([]any)
	if !ok || len(errs) == 0 {
		t.Fatalf("response should carry a non-empty errors list, got %v", payload)
	}
	joined := rr.Body.String()
	for _, want := range []string{"dangling_branch_target", "invalid_expression", "missing_node_ref"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("errors should mention %q, got %s", want, joined)
		}
	}
}

func TestWorkflowValidateMalformedBody(t *testing.T) {
	mux, authSvc := wfValidateSetup(t)
	_, _, token := wfRegisterOwner(t, authSvc, "validate-malformed@acme.test")
	rr := wfDo(t, mux, http.MethodPost, "/workflows/validate", token, `{not json`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("malformed body = %d, want 400", rr.Code)
	}
}
