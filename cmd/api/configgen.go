package main

// configgen.go — issue #129 HTTP wiring: LLM-driven auto-configuration.
// POST /config/generate translates a natural-language description into an
// AgentOS configuration DRAFT (agent config or workflow DSL).
//
// Endpoint (registered by registerConfigGenRoutes; served under BOTH /v1 and
// /api/v1 like every versioned surface):
//
//	POST /config/generate   {type: "agent"|"workflow", description, context}
//	                        → draft + validation result + step progress
//	                        (configgen.use — OWNER/ADMIN/MEMBER)
//
// Design:
//   - DRAFTS ONLY: the handler never creates or mutates a resource. The
//     caller reviews the draft and creates the real objects through the
//     normal endpoints (POST /agents/create, POST /workflows). This is the
//     issue #129 safety contract, enforced by construction: the generators
//     in internal/configgen have no store seam at all.
//   - Dual-mode LLM seam: when a model provider is configured (the same
//     models.Provider the worker runtime and evaluation runner use) the
//     generators consult it through the narrow configgen.ConfigLLM adapter;
//     otherwise — and whenever the model returns nothing usable — the
//     deterministic heuristic generator answers, so the endpoint works with
//     zero infrastructure and never fails because of a model outage.
//   - Step-by-step generation: the handler wires a ProgressCallback that
//     records each completed step (1..5) into the response so UIs can render
//     partial progress; the final draft is returned only after step 5
//     (validate) succeeds.
//   - RBAC via internal/auth/permissions_configgen.go (init() append):
//     configgen.use for OWNER/ADMIN/MEMBER; VIEWER is excluded because
//     drafts shape future resource creation.
//   - Error envelope {"error":{"code","message"}}: 400 INVALID_REQUEST
//     (malformed/empty JSON body), 422 VALIDATION_ERROR (blank description,
//     unknown type), 401/403 via the auth middleware chain.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"agentos/internal/apikeys"
	"agentos/internal/auth"
	"agentos/internal/configgen"
	"agentos/internal/models"
	"agentos/internal/tools"
)

// configGenService bundles the draft generators behind one HTTP surface.
// The catalog and LLM seam are request-independent; the generators
// themselves are instantiated per request so the progress callback never
// leaks state across callers.
type configGenService struct {
	catalog configgen.ToolCatalog
	llm     configgen.ConfigLLM
}

// newConfigGenService builds the draft generators. Dual-mode LLM seam: when
// haveProvider is true the provider is wrapped into the narrow
// configgen.ConfigLLM adapter (model-backed generation with deterministic
// fallback); otherwise the nil seam keeps generation fully heuristic. Both
// modes return VALID drafts only.
func newConfigGenService(provider models.Provider, haveProvider bool) *configGenService {
	var llm configgen.ConfigLLM
	if haveProvider && provider != nil {
		llm = &configgen.ProviderLLM{Provider: provider}
	}
	return &configGenService{
		catalog: tools.DefaultRegistry(),
		llm:     llm,
	}
}

// generateRequest is the POST /config/generate body.
type generateRequest struct {
	// Type selects the generator: "agent" or "workflow".
	Type string `json:"type"`
	// Description is the natural-language intent to translate.
	Description string `json:"description"`
	// Context carries optional free-form caller context (team conventions,
	// existing resource names…) that is echoed into the draft assumptions.
	// Never interpreted by the server.
	Context map[string]any `json:"context,omitempty"`
}

// generateStep is one entry of the response's progress array.
type generateStep struct {
	Step  int `json:"step"`
	Total int `json:"total"`
}

// generateResponse is the POST /config/generate body. Exactly one of
// agent_draft / workflow_draft is populated, matching the request type.
type generateResponse struct {
	Type     string         `json:"type"`
	Agent    any            `json:"agent_draft,omitempty"`
	Workflow any            `json:"workflow_draft,omitempty"`
	Progress []generateStep `json:"progress"`
	// Valid reports whether the returned draft passed the validators
	// (ValidateAgentConfig / ValidateWorkflowDSL). Generated drafts are
	// always valid; the field exists so clients can assert the contract.
	Valid  bool     `json:"valid"`
	Issues []string `json:"issues,omitempty"`
}

// registerConfigGenRoutes mounts the auto-configuration surface.
func registerConfigGenRoutes(apiMux *http.ServeMux, svc *configGenService, authSvc *auth.Service, apiKeysSvc *apikeys.Service) {
	if apiMux == nil || svc == nil {
		return
	}
	// auth wrap pattern from cmd/api/main.go / synth.go:
	// RequireAuthOrAPIKey outer, RequirePermission inner.
	wrap := func(h http.HandlerFunc) http.Handler {
		return auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(
			auth.RequirePermission(authSvc, auth.PermissionConfigGenUse)(h))
	}
	apiMux.Handle("POST /config/generate", wrap(configGenerateHandler(svc)))
}

// configGenerateHandler translates a description into a configuration draft.
func configGenerateHandler(svc *configGenService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req generateRequest
		if !decodeConfigGenBody(w, r, &req) {
			return
		}
		req.Type = strings.ToLower(strings.TrimSpace(req.Type))
		if req.Type == "" {
			writeErrorConfigGen(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "type is required (agent or workflow)")
			return
		}
		if strings.TrimSpace(req.Description) == "" {
			writeErrorConfigGen(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "description is required")
			return
		}

		ctx := r.Context()
		switch req.Type {
		case "agent":
			gen := configgen.NewAgentConfigGenerator(svc.catalog, svc.llm)
			progress := newProgressRecorder(gen.OnProgress)
			draft, err := gen.Generate(ctx, req.Description)
			if err != nil {
				writeErrorConfigGen(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", err.Error())
				return
			}
			issues := configgen.ValidateAgentConfig(draft, svc.catalog)
			writeJSONConfigGen(w, http.StatusOK, generateResponse{
				Type:     "agent",
				Agent:    withContextAssumptions(draft, req.Context),
				Progress: progress.steps,
				Valid:    len(issues) == 0,
				Issues:   issues,
			})
		case "workflow":
			gen := configgen.NewWorkflowConfigGenerator(svc.llm)
			progress := newProgressRecorder(gen.OnProgress)
			draft, err := gen.Generate(ctx, req.Description)
			if err != nil {
				writeErrorConfigGen(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", err.Error())
				return
			}
			errs := configgen.ValidateWorkflowDSL(draft)
			issues := make([]string, 0, len(errs))
			for _, e := range errs {
				issues = append(issues, e.Message)
			}
			writeJSONConfigGen(w, http.StatusOK, generateResponse{
				Type:     "workflow",
				Workflow: withWorkflowContextAssumptions(draft, req.Context),
				Progress: progress.steps,
				Valid:    len(errs) == 0,
				Issues:   issues,
			})
		default:
			writeErrorConfigGen(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR",
				"unknown type "+req.Type+" (want agent or workflow)")
		}
	}
}

// progressRecorder adapts configgen.ProgressCallback into the response's
// progress array. The generators call it synchronously, so no locking is
// needed within one request.
type progressRecorder struct {
	steps []generateStep
}

// newProgressRecorder wires a recorder onto a generator's OnProgress
// registration function and returns the completed recorder.
func newProgressRecorder(register func(configgen.ProgressCallback)) *progressRecorder {
	rec := &progressRecorder{steps: make([]generateStep, 0, configgen.GenerationSteps)}
	register(func(step, total int, partial any) {
		rec.steps = append(rec.steps, generateStep{Step: step, Total: total})
	})
	return rec
}

// withContextAssumptions echoes the caller context into the draft's
// assumptions so reviewers see what context shaped the draft.
func withContextAssumptions(draft configgen.AgentConfigDraft, context map[string]any) configgen.AgentConfigDraft {
	if len(context) > 0 {
		raw, _ := json.Marshal(context)
		draft.Assumptions = append(draft.Assumptions,
			"caller context supplied to the generator: "+string(raw))
	}
	return draft
}

// withWorkflowContextAssumptions is the workflow twin of
// withContextAssumptions.
func withWorkflowContextAssumptions(draft configgen.WorkflowDSLDraft, context map[string]any) configgen.WorkflowDSLDraft {
	if len(context) > 0 {
		raw, _ := json.Marshal(context)
		draft.Assumptions = append(draft.Assumptions,
			"caller context supplied to the generator: "+string(raw))
	}
	return draft
}

// --- house envelope helpers (same shape as synth.go/residency.go) -----------

func writeJSONConfigGen(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeErrorConfigGen(w http.ResponseWriter, status int, code, message string) {
	writeJSONConfigGen(w, status, map[string]any{"error": map[string]any{"code": code, "message": message}})
}

// decodeConfigGenBody decodes the JSON request body bounded to 1 MiB. Returns
// false when the response was already written.
func decodeConfigGenBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.Body == nil {
		return true
	}
	defer r.Body.Close() //nolint:errcheck // read path; nothing actionable
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErrorConfigGen(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
		return false
	}
	if len(raw) == 0 {
		writeErrorConfigGen(w, http.StatusBadRequest, "INVALID_REQUEST", "request body is required")
		return false
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		writeErrorConfigGen(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid JSON body")
		return false
	}
	return true
}
