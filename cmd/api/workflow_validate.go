package main

// workflow_validate.go — issue #109 HTTP wiring: POST /workflows/validate,
// the pre-save validation endpoint for workflow definitions.
//
// The endpoint accepts an ad-hoc DSL document (the same shape POST
// /workflows/create carries under "dsl") and runs the FULL ValidateDSL rule
// set against it — structural rules, per-type config requirements, the
// issue #109 control-flow wiring rules (condition/switch branch targets, loop
// bodies, retry targets) and control-flow expression syntax — WITHOUT
// creating or modifying any workflow. Users get typed 422 validation errors
// (code, message, node_id) before they ever attempt a save or publish.
//
// Design (mirrors cmd/api/workflows.go):
//   - Tenant scope: validation is stateless, so there is nothing to scope;
//     the org id still comes exclusively from the authenticated claims and is
//     required (the service rejects empty tenants for uniform behavior).
//   - RBAC reuses the EXISTING workflows.read grant: validation mutates
//     nothing, so MEMBER/VIEWER authors can pre-validate definitions the same
//     way they can read workflows.
//   - Body shape: either {"dsl": {...nodes, edges...}} or a bare DSL object
//     ({"nodes": [...], "edges": [...]}). The wrapped form is the canonical
//     one; the bare form keeps curl usage one level of nesting lighter.
//   - Responses mirror the existing workflow validation contract exactly:
//     200 {"valid": true} or 422 {"errors": [{"code","message","node_id"}]}.

import (
	"encoding/json"
	"io"
	"net/http"

	"agentos/internal/apikeys"
	"agentos/internal/auth"
	"agentos/internal/workflows"
)

// registerWorkflowValidateRoutes mounts the issue #109 DSL validation
// endpoint on apiMux (served under both /v1 and /api/v1 like every versioned
// surface).
func registerWorkflowValidateRoutes(apiMux *http.ServeMux, wfSvc *workflows.Service, authSvc *auth.Service, apiKeysSvc *apikeys.Service) {
	if apiMux == nil || wfSvc == nil {
		return
	}
	wrap := func(perm auth.Permission, h http.HandlerFunc) http.Handler {
		return auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(auth.RequirePermission(authSvc, perm)(h))
	}
	apiMux.Handle("POST /workflows/validate", wrap(auth.PermissionWorkflowsRead, validateWorkflowDSLHandler(wfSvc)))
}

// validateWorkflowDSLHandler validates an ad-hoc DSL document against the
// full ValidateDSL rule set (stateless; nothing is persisted). Two body
// shapes are accepted: the canonical wrapped form {"dsl": {"nodes": [...],
// "edges": [...]}} and the bare DSL document {"nodes": [...], "edges":
// [...]}. A body that parses as JSON but carries neither form fails
// validation with the standard empty-graph error, keeping one error contract
// for malformed-but-parseable payloads.
func validateWorkflowDSLHandler(wfSvc *workflows.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWorkflowValidateBodyBytes))
		if err != nil {
			writeErrorWF(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
			return
		}
		dsl, err := decodeWorkflowValidateBody(raw)
		if err != nil {
			writeErrorWF(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body: expected {\"dsl\": {\"nodes\": [...], \"edges\": [...]}} or a bare DSL object")
			return
		}
		orgID, _, ok := wfClaims(w, r)
		if !ok {
			return
		}
		verrs := wfSvc.ValidateWorkflowDSL(r.Context(), orgID, dsl)
		if len(verrs) > 0 {
			writeJSONWF(w, http.StatusUnprocessableEntity, map[string]any{"errors": verrs})
			return
		}
		writeJSONWF(w, http.StatusOK, map[string]any{"valid": true})
	}
}

// maxWorkflowValidateBodyBytes bounds the validation request body: workflow
// definitions are structural documents, and 1 MiB covers graphs orders of
// magnitude larger than anything the executor can schedule.
const maxWorkflowValidateBodyBytes = 1 << 20

// decodeWorkflowValidateBody parses the two accepted request shapes into a
// DSL document (see validateWorkflowDSLHandler).
func decodeWorkflowValidateBody(raw []byte) (workflows.DSL, error) {
	var wrapped struct {
		DSL *workflows.DSL `json:"dsl"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil && wrapped.DSL != nil {
		return *wrapped.DSL, nil
	}
	var dsl workflows.DSL
	if err := json.Unmarshal(raw, &dsl); err != nil {
		return workflows.DSL{}, err
	}
	return dsl, nil
}
