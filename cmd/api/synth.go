package main

// synth.go — issue #147 (flagship #118) HTTP wiring: the tenant-scoped
// synthetic assurance registry (projects, targets, environments, personas)
// on top of internal/synth/service.go.
//
// Endpoints (registered on apiMux by registerSynthRoutes; served under BOTH
// /v1 and /api/v1 like every versioned surface):
//
//	POST   /synth/projects                     create project            (synth.write)
//	GET    /synth/projects                     list projects             (synth.read)
//	GET    /synth/projects/{id}                get project               (synth.read)
//	PUT    /synth/projects/{id}                rename project            (synth.write)
//	DELETE /synth/projects/{id}                delete project            (synth.write)
//	POST   /synth/targets                      create target (unverified)(synth.write)
//	GET    /synth/targets[?project_id=]        list targets              (synth.read)
//	GET    /synth/targets/{id}                 get target                (synth.read)
//	PUT    /synth/targets/{id}                 rename target             (synth.write)
//	DELETE /synth/targets/{id}                 delete target             (synth.write)
//	POST   /synth/targets/{id}/verify          challenge issue OR confirm(synth.write)
//	POST   /synth/targets/{id}/environments    create environment        (synth.write; production kind: OWNER/ADMIN)
//	GET    /synth/targets/{id}/environments    list environments         (synth.read)
//	GET    /synth/environments/{id}            get environment           (synth.read)
//	PUT    /synth/environments/{id}            replace safety posture    (synth.write; production kind: OWNER/ADMIN)
//	DELETE /synth/environments/{id}            delete environment        (synth.write)
//	POST   /synth/personas                     create persona            (synth.write)
//	GET    /synth/personas                     list personas             (synth.read)
//	GET    /synth/personas/{id}                get persona               (synth.read)
//	PUT    /synth/personas/{id}                update persona (bumps ver)(synth.write)
//	DELETE /synth/personas/{id}                delete persona            (synth.write)
//
// Design (mirrors cmd/api/residency.go):
//   - RBAC via NEW grants internal/auth/permissions_synth.go (init() append):
//     synth.read for every role (dashboard surface), synth.write for
//     OWNER/ADMIN/MEMBER. ONE extra gate lives HERE in the handler, not in
//     the service: PRODUCTION TEST ENABLEMENT (creating/updating an
//     environment with kind=production) requires OWNER or ADMIN, because it
//     touches the production_read_only default. A MEMBER with synth.write
//     can still build local/dev/staging scope freely.
//   - Tenant scope comes EXCLUSIVELY from the auth claims: the org id is
//     never accepted from the wire. Cross-tenant ids surface as 404 one
//     layer down (org-guarded Store, no existence leak).
//   - The target ownership challenge token is rendered EXACTLY ONCE — in
//     the 200 response of POST /synth/targets/{id}/verify when it ISSUES a
//     challenge. Every other rendering (list/get/confirm/delete) is an
//     explicit field allowlist that never carries verify_token.
//   - Error envelope {"error":{"code","message"}}: 404 NOT_FOUND
//     (synth.ErrNotFound — cross-org is 404 by design), 400 VALIDATION_ERROR
//     (synth.ErrInvalid — mode semantics, allowlists, windows, names), 403
//     VERIFICATION_FAILED (wrong challenge token) / FORBIDDEN (production
//     enablement without OWNER/ADMIN), 500 INTERNAL_ERROR otherwise.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"agentos/internal/apikeys"
	"agentos/internal/auth"
	"agentos/internal/synth"
)

// registerSynthRoutes mounts the synthetic assurance registry surface.
func registerSynthRoutes(apiMux *http.ServeMux, svc *synth.Service, authSvc *auth.Service, apiKeysSvc *apikeys.Service) {
	if apiMux == nil || svc == nil {
		return
	}
	// auth wrap pattern from cmd/api/main.go / residency.go:
	// RequireAuthOrAPIKey outer, RequirePermission inner.
	wrap := func(perm auth.Permission, h http.Handler) http.Handler {
		return auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(auth.RequirePermission(authSvc, perm)(h))
	}
	write := func(h http.HandlerFunc) http.Handler { return wrap(auth.PermissionSynthWrite, h) }
	read := func(h http.HandlerFunc) http.Handler { return wrap(auth.PermissionSynthRead, h) }

	// projects
	apiMux.Handle("POST /synth/projects", write(synthProjectCreateHandler(svc)))
	apiMux.Handle("GET /synth/projects", read(synthProjectListHandler(svc)))
	apiMux.Handle("GET /synth/projects/{id}", read(synthProjectGetHandler(svc)))
	apiMux.Handle("PUT /synth/projects/{id}", write(synthProjectUpdateHandler(svc)))
	apiMux.Handle("DELETE /synth/projects/{id}", write(synthProjectDeleteHandler(svc)))

	// targets (+ ownership verification + environment binding)
	apiMux.Handle("POST /synth/targets", write(synthTargetCreateHandler(svc)))
	apiMux.Handle("GET /synth/targets", read(synthTargetListHandler(svc)))
	apiMux.Handle("GET /synth/targets/{id}", read(synthTargetGetHandler(svc)))
	apiMux.Handle("PUT /synth/targets/{id}", write(synthTargetUpdateHandler(svc)))
	apiMux.Handle("DELETE /synth/targets/{id}", write(synthTargetDeleteHandler(svc)))
	apiMux.Handle("POST /synth/targets/{id}/verify", write(synthTargetVerifyHandler(svc)))
	apiMux.Handle("POST /synth/targets/{id}/environments", write(synthEnvironmentCreateHandler(svc)))
	apiMux.Handle("GET /synth/targets/{id}/environments", read(synthEnvironmentListForTargetHandler(svc)))

	// environments
	apiMux.Handle("GET /synth/environments/{id}", read(synthEnvironmentGetHandler(svc)))
	apiMux.Handle("PUT /synth/environments/{id}", write(synthEnvironmentUpdateHandler(svc)))
	apiMux.Handle("DELETE /synth/environments/{id}", write(synthEnvironmentDeleteHandler(svc)))

	// personas
	apiMux.Handle("POST /synth/personas", write(synthPersonaCreateHandler(svc)))
	apiMux.Handle("GET /synth/personas", read(synthPersonaListHandler(svc)))
	apiMux.Handle("GET /synth/personas/{id}", read(synthPersonaGetHandler(svc)))
	apiMux.Handle("PUT /synth/personas/{id}", write(synthPersonaUpdateHandler(svc)))
	apiMux.Handle("DELETE /synth/personas/{id}", write(synthPersonaDeleteHandler(svc)))
}

// --- house envelope helpers (distinct names; same shape as residency.go) ----

func writeJSONSynth(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeErrorSynth(w http.ResponseWriter, status int, code, message string) {
	writeJSONSynth(w, status, map[string]any{"error": map[string]any{"code": code, "message": message}})
}

// decodeSynthBody decodes the JSON request body bounded to 1 MiB. Returns
// false when the response was already written.
func decodeSynthBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.Body == nil {
		return true
	}
	defer r.Body.Close() //nolint:errcheck // read path; nothing actionable
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErrorSynth(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid request body")
		return false
	}
	if len(raw) == 0 {
		return true
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		writeErrorSynth(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid JSON body")
		return false
	}
	return true
}

// synthClaims extracts the caller claims or writes the 401/404 shape,
// returning ok=false when the response was already written.
func synthClaims(w http.ResponseWriter, r *http.Request) (auth.UserClaims, bool) {
	claims, err := auth.ExtractClaims(r.Context())
	if err != nil {
		writeErrorSynth(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
		return auth.UserClaims{}, false
	}
	if claims.OrganizationID == "" {
		writeErrorSynth(w, http.StatusNotFound, "NOT_FOUND", "organization not found")
		return auth.UserClaims{}, false
	}
	return claims, true
}

// mapSynthError renders the service error vocabulary onto the house
// envelope. Cross-org/unknown ids are the same 404 (no existence leak).
func mapSynthError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, synth.ErrNotFound):
		writeErrorSynth(w, http.StatusNotFound, "NOT_FOUND", "resource not found")
	case errors.Is(err, synth.ErrInvalid):
		writeErrorSynth(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
	case errors.Is(err, synth.ErrTargetUnverified):
		// A wrong/missing challenge token is a failed ownership proof.
		writeErrorSynth(w, http.StatusForbidden, "VERIFICATION_FAILED", err.Error())
	case errors.Is(err, synth.ErrScopeViolation), errors.Is(err, synth.ErrReadOnlyMode):
		writeErrorSynth(w, http.StatusForbidden, "SCOPE_VIOLATION", err.Error())
	default:
		writeErrorSynth(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
	}
}

// synthProductionGate enforces the handler-level RBAC rule: enabling tests
// against PRODUCTION (creating/updating a production-kind environment)
// requires OWNER or ADMIN — it trades away the production_read_only default
// and is therefore an account-administration decision, not an everyday
// MEMBER write. Returns ok=false when the response was already written.
func synthProductionGate(w http.ResponseWriter, claims auth.UserClaims, kind synth.EnvironmentKind) bool {
	if kind != synth.EnvProduction {
		return true
	}
	if claims.Role == "OWNER" || claims.Role == "ADMIN" {
		return true
	}
	writeErrorSynth(w, http.StatusForbidden, "FORBIDDEN",
		"production test enablement requires the OWNER or ADMIN role")
	return false
}

// --- rendering (explicit field allowlists; NEVER verify_token) ---------------

func synthTime(t time.Time) string {
	if t.IsZero() {
		return time.Time{}.UTC().Format(time.RFC3339)
	}
	return t.UTC().Format(time.RFC3339)
}

func synthNullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

func synthProjectJSON(p *synth.Project) map[string]any {
	return map[string]any{
		"id": p.ID, "org_id": p.OrgID, "name": p.Name,
		"created_at": synthTime(p.CreatedAt), "updated_at": synthTime(p.UpdatedAt),
	}
}

func synthTargetJSON(t *synth.Target) map[string]any {
	return map[string]any{
		"id": t.ID, "org_id": t.OrgID, "project_id": t.ProjectID,
		"name": t.Name, "base_url": t.BaseURL,
		"verified": t.Verified, "verified_at": synthNullableTime(t.VerifiedAt),
		// verify_token is deliberately absent: challenge secrets are
		// rendered exactly once by the verify handler.
		"created_at": synthTime(t.CreatedAt), "updated_at": synthTime(t.UpdatedAt),
	}
}

func synthEnvironmentJSON(e *synth.Environment) map[string]any {
	domains := e.AllowedDomains
	if domains == nil {
		domains = []string{}
	}
	quiet := e.QuietWindows
	if quiet == nil {
		quiet = []synth.QuietWindow{}
	}
	test := e.TestWindows
	if test == nil {
		test = []synth.QuietWindow{}
	}
	return map[string]any{
		"id": e.ID, "org_id": e.OrgID, "target_id": e.TargetID,
		"kind": string(e.Kind), "mode": string(e.Mode),
		"allowed_domains": domains, "rate_cap_per_min": e.RateCapPerMin,
		"budget_per_run": e.BudgetPerRun,
		"quiet_windows":  quiet, "test_windows": test,
		"created_at": synthTime(e.CreatedAt), "updated_at": synthTime(e.UpdatedAt),
	}
}

func synthPersonaJSON(p *synth.Persona) map[string]any {
	hints := p.RoleHints
	if hints == nil {
		hints = []string{}
	}
	constraints := p.Constraints
	if constraints == nil {
		constraints = map[string]string{}
	}
	return map[string]any{
		"id": p.ID, "org_id": p.OrgID, "name": p.Name, "built_in": p.BuiltIn,
		"role_hints": hints, "constraints": constraints, "version": p.Version,
		"created_at": synthTime(p.CreatedAt), "updated_at": synthTime(p.UpdatedAt),
	}
}

// --- project handlers --------------------------------------------------------

func synthProjectCreateHandler(svc *synth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := synthClaims(w, r)
		if !ok {
			return
		}
		var body struct {
			Name string `json:"name"`
		}
		if !decodeSynthBody(w, r, &body) {
			return
		}
		p, err := svc.CreateProject(r.Context(), claims.OrganizationID, body.Name)
		if err != nil {
			mapSynthError(w, err)
			return
		}
		writeJSONSynth(w, http.StatusCreated, map[string]any{"project": synthProjectJSON(p)})
	}
}

func synthProjectListHandler(svc *synth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := synthClaims(w, r)
		if !ok {
			return
		}
		list, err := svc.ListProjects(r.Context(), claims.OrganizationID)
		if err != nil {
			mapSynthError(w, err)
			return
		}
		out := make([]map[string]any, 0, len(list))
		for _, p := range list {
			out = append(out, synthProjectJSON(p))
		}
		writeJSONSynth(w, http.StatusOK, map[string]any{"projects": out})
	}
}

func synthProjectGetHandler(svc *synth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := synthClaims(w, r)
		if !ok {
			return
		}
		p, err := svc.GetProject(r.Context(), claims.OrganizationID, r.PathValue("id"))
		if err != nil {
			mapSynthError(w, err)
			return
		}
		writeJSONSynth(w, http.StatusOK, map[string]any{"project": synthProjectJSON(p)})
	}
}

func synthProjectUpdateHandler(svc *synth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := synthClaims(w, r)
		if !ok {
			return
		}
		var body struct {
			Name string `json:"name"`
		}
		if !decodeSynthBody(w, r, &body) {
			return
		}
		p, err := svc.UpdateProject(r.Context(), claims.OrganizationID, r.PathValue("id"), body.Name)
		if err != nil {
			mapSynthError(w, err)
			return
		}
		writeJSONSynth(w, http.StatusOK, map[string]any{"project": synthProjectJSON(p)})
	}
}

func synthProjectDeleteHandler(svc *synth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := synthClaims(w, r)
		if !ok {
			return
		}
		if err := svc.DeleteProject(r.Context(), claims.OrganizationID, r.PathValue("id")); err != nil {
			mapSynthError(w, err)
			return
		}
		writeJSONSynth(w, http.StatusOK, map[string]any{"deleted": true})
	}
}

// --- target handlers ----------------------------------------------------------

func synthTargetCreateHandler(svc *synth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := synthClaims(w, r)
		if !ok {
			return
		}
		var body struct {
			ProjectID string `json:"project_id"`
			Name      string `json:"name"`
			BaseURL   string `json:"base_url"`
		}
		if !decodeSynthBody(w, r, &body) {
			return
		}
		t, err := svc.CreateTarget(r.Context(), claims.OrganizationID, body.ProjectID, body.Name, body.BaseURL)
		if err != nil {
			mapSynthError(w, err)
			return
		}
		writeJSONSynth(w, http.StatusCreated, map[string]any{"target": synthTargetJSON(t)})
	}
}

func synthTargetListHandler(svc *synth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := synthClaims(w, r)
		if !ok {
			return
		}
		list, err := svc.ListTargets(r.Context(), claims.OrganizationID, r.URL.Query().Get("project_id"))
		if err != nil {
			mapSynthError(w, err)
			return
		}
		out := make([]map[string]any, 0, len(list))
		for _, t := range list {
			out = append(out, synthTargetJSON(t))
		}
		writeJSONSynth(w, http.StatusOK, map[string]any{"targets": out})
	}
}

func synthTargetGetHandler(svc *synth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := synthClaims(w, r)
		if !ok {
			return
		}
		t, err := svc.GetTarget(r.Context(), claims.OrganizationID, r.PathValue("id"))
		if err != nil {
			mapSynthError(w, err)
			return
		}
		writeJSONSynth(w, http.StatusOK, map[string]any{"target": synthTargetJSON(t)})
	}
}

func synthTargetUpdateHandler(svc *synth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := synthClaims(w, r)
		if !ok {
			return
		}
		var body struct {
			Name string `json:"name"`
		}
		if !decodeSynthBody(w, r, &body) {
			return
		}
		t, err := svc.UpdateTarget(r.Context(), claims.OrganizationID, r.PathValue("id"), body.Name)
		if err != nil {
			mapSynthError(w, err)
			return
		}
		writeJSONSynth(w, http.StatusOK, map[string]any{"target": synthTargetJSON(t)})
	}
}

func synthTargetDeleteHandler(svc *synth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := synthClaims(w, r)
		if !ok {
			return
		}
		if err := svc.DeleteTarget(r.Context(), claims.OrganizationID, r.PathValue("id")); err != nil {
			mapSynthError(w, err)
			return
		}
		writeJSONSynth(w, http.StatusOK, map[string]any{"deleted": true})
	}
}

// synthTargetVerifyHandler is the challenge/confirm endpoint for target
// ownership verification (types.go: tests execute only against VERIFIED
// targets).
//
//	{} or {"action":"issue"}   -> (re)issues the ownership challenge and
//	                              returns the token EXACTLY ONCE
//	                              (status "challenge_issued").
//	{"token":"<challenge>"}    -> confirms ownership; on match the target
//	                              flips verified=true and the one-time
//	                              token is consumed. A mismatch is
//	                              403 VERIFICATION_FAILED.
//
// Confirming an already-verified target is an idempotent 200; re-issuing on
// a verified target is a documented no-op that never silently un-verifies.
func synthTargetVerifyHandler(svc *synth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := synthClaims(w, r)
		if !ok {
			return
		}
		var body struct {
			Action string `json:"action"`
			Token  string `json:"token"`
		}
		if !decodeSynthBody(w, r, &body) {
			return
		}
		id := r.PathValue("id")
		// A non-empty token means CONFIRM; otherwise ISSUE a challenge.
		if strings.TrimSpace(body.Token) != "" {
			t, err := svc.ConfirmTargetVerification(r.Context(), claims.OrganizationID, id, body.Token)
			if err != nil {
				mapSynthError(w, err)
				return
			}
			writeJSONSynth(w, http.StatusOK, map[string]any{
				"status": "verified", "target": synthTargetJSON(t),
			})
			return
		}
		t, err := svc.StartTargetVerification(r.Context(), claims.OrganizationID, id)
		if err != nil {
			mapSynthError(w, err)
			return
		}
		if t.Verified {
			writeJSONSynth(w, http.StatusOK, map[string]any{
				"status": "verified", "target": synthTargetJSON(t),
			})
			return
		}
		if strings.TrimSpace(body.Action) != "" && body.Action != "issue" {
			writeErrorSynth(w, http.StatusBadRequest, "VALIDATION_ERROR",
				"unknown verify action (want \"issue\" or a non-empty token)")
			return
		}
		// The ONLY wire rendering of the challenge token, ever.
		writeJSONSynth(w, http.StatusOK, map[string]any{
			"status": "challenge_issued",
			"target": synthTargetJSON(t),
			"verification": map[string]any{
				"token":        t.VerifyToken,
				"instructions": "serve this token at any path on the target base_url (e.g. /.well-known/agentos-synth-challenge) and confirm via POST with {\"token\": ...}",
			},
		})
	}
}

// --- environment handlers ------------------------------------------------------

func synthEnvironmentCreateHandler(svc *synth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := synthClaims(w, r)
		if !ok {
			return
		}
		var spec synth.EnvSpec
		if !decodeSynthBody(w, r, &spec) {
			return
		}
		// Production test enablement: OWNER/ADMIN only (handler-level rule,
		// see synthProductionGate). Validated BEFORE the service call.
		if !synthProductionGate(w, claims, spec.Kind) {
			return
		}
		e, err := svc.CreateEnvironment(r.Context(), claims.OrganizationID, r.PathValue("id"), spec)
		if err != nil {
			mapSynthError(w, err)
			return
		}
		writeJSONSynth(w, http.StatusCreated, map[string]any{"environment": synthEnvironmentJSON(e)})
	}
}

func synthEnvironmentListForTargetHandler(svc *synth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := synthClaims(w, r)
		if !ok {
			return
		}
		list, err := svc.ListEnvironments(r.Context(), claims.OrganizationID, r.PathValue("id"))
		if err != nil {
			mapSynthError(w, err)
			return
		}
		writeJSONSynth(w, http.StatusOK, map[string]any{"environments": synthEnvironmentListJSON(list)})
	}
}

// synthEnvironmentListJSON renders a batch of environments.
func synthEnvironmentListJSON(list []*synth.Environment) []map[string]any {
	out := make([]map[string]any, 0, len(list))
	for _, e := range list {
		out = append(out, synthEnvironmentJSON(e))
	}
	return out
}

func synthEnvironmentGetHandler(svc *synth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := synthClaims(w, r)
		if !ok {
			return
		}
		e, err := svc.GetEnvironment(r.Context(), claims.OrganizationID, r.PathValue("id"))
		if err != nil {
			mapSynthError(w, err)
			return
		}
		writeJSONSynth(w, http.StatusOK, map[string]any{"environment": synthEnvironmentJSON(e)})
	}
}

func synthEnvironmentUpdateHandler(svc *synth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := synthClaims(w, r)
		if !ok {
			return
		}
		var spec synth.EnvSpec
		if !decodeSynthBody(w, r, &spec) {
			return
		}
		if !synthProductionGate(w, claims, spec.Kind) {
			return
		}
		e, err := svc.UpdateEnvironment(r.Context(), claims.OrganizationID, r.PathValue("id"), spec)
		if err != nil {
			mapSynthError(w, err)
			return
		}
		writeJSONSynth(w, http.StatusOK, map[string]any{"environment": synthEnvironmentJSON(e)})
	}
}

func synthEnvironmentDeleteHandler(svc *synth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := synthClaims(w, r)
		if !ok {
			return
		}
		if err := svc.DeleteEnvironment(r.Context(), claims.OrganizationID, r.PathValue("id")); err != nil {
			mapSynthError(w, err)
			return
		}
		writeJSONSynth(w, http.StatusOK, map[string]any{"deleted": true})
	}
}

// --- persona handlers -----------------------------------------------------------

func synthPersonaCreateHandler(svc *synth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := synthClaims(w, r)
		if !ok {
			return
		}
		var spec synth.PersonaSpec
		if !decodeSynthBody(w, r, &spec) {
			return
		}
		p, err := svc.CreatePersona(r.Context(), claims.OrganizationID, spec)
		if err != nil {
			mapSynthError(w, err)
			return
		}
		writeJSONSynth(w, http.StatusCreated, map[string]any{"persona": synthPersonaJSON(p)})
	}
}

func synthPersonaListHandler(svc *synth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := synthClaims(w, r)
		if !ok {
			return
		}
		list, err := svc.ListPersonas(r.Context(), claims.OrganizationID)
		if err != nil {
			mapSynthError(w, err)
			return
		}
		out := make([]map[string]any, 0, len(list))
		for _, p := range list {
			out = append(out, synthPersonaJSON(p))
		}
		writeJSONSynth(w, http.StatusOK, map[string]any{"personas": out})
	}
}

func synthPersonaGetHandler(svc *synth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := synthClaims(w, r)
		if !ok {
			return
		}
		p, err := svc.GetPersona(r.Context(), claims.OrganizationID, r.PathValue("id"))
		if err != nil {
			mapSynthError(w, err)
			return
		}
		writeJSONSynth(w, http.StatusOK, map[string]any{"persona": synthPersonaJSON(p)})
	}
}

func synthPersonaUpdateHandler(svc *synth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := synthClaims(w, r)
		if !ok {
			return
		}
		var spec synth.PersonaSpec
		if !decodeSynthBody(w, r, &spec) {
			return
		}
		p, err := svc.UpdatePersona(r.Context(), claims.OrganizationID, r.PathValue("id"), spec)
		if err != nil {
			mapSynthError(w, err)
			return
		}
		writeJSONSynth(w, http.StatusOK, map[string]any{"persona": synthPersonaJSON(p)})
	}
}

func synthPersonaDeleteHandler(svc *synth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := synthClaims(w, r)
		if !ok {
			return
		}
		if err := svc.DeletePersona(r.Context(), claims.OrganizationID, r.PathValue("id")); err != nil {
			mapSynthError(w, err)
			return
		}
		writeJSONSynth(w, http.StatusOK, map[string]any{"deleted": true})
	}
}
