package main

// a2a.go — issue #136 HTTP wiring: the A2A (Agent-to-Agent) protocol
// surface — the org-scoped Agent Card registry, the canonical discovery
// card, the inbound message endpoint and the governed outbound delegation.
//
// Endpoints (registered on apiMux by registerA2ARoutes; served under BOTH
// /v1 and /api/v1):
//
//      POST   /a2a/agents              publish a card          (OWNER/ADMIN)
//      GET    /a2a/agents              list cards              (MEMBER+)
//      GET    /a2a/agents/{id}         get one card row        (MEMBER+)
//      GET    /a2a/agents/{id}/card    canonical Agent Card    (any org member)
//      PATCH  /a2a/agents/{id}         update a card           (OWNER/ADMIN)
//      DELETE /a2a/agents/{id}         delete a card           (OWNER/ADMIN)
//      POST   /a2a/agents/{id}/message inbound A2A message     (runs.execute)
//      POST   /a2a/delegate            outbound delegation     (runs.execute)
//
// Design (mirrors cmd/api/mcp_registry.go exactly):
//   - RBAC reuses the EXISTING permission enums (no new grants): reads use
//     connectors.read (the MEMBER+ matrix the MCP registry established for
//     integration-topology listings), the discovery card uses agents.read
//     (every authenticated org role incl. VIEWER — it is the discovery
//     surface within the org), writes use agents.write (OWNER/ADMIN), and
//     the two invocation surfaces use runs.execute.
//   - Tenant scope comes exclusively from the auth claims; cross-tenant ids
//     surface as 404 without an existence leak.
//   - The registry row is rendered through an explicit field allowlist;
//     the discovery endpoint renders the CANONICAL A2A card (AgentCard.
//     ToJSON) consumable by a generic A2A client. auth_scheme is a SCHEME
//     NAME — no credential material exists on a card.
//   - Audit: card lifecycle + inbound messages are audited here
//     (a2a.card_published / a2a.card_updated / a2a.card_deleted /
//     a2a.message_received); delegation outcomes are audited by the a2a
//     service itself via the Auditor seam (a2a.delegated /
//     a2a.delegation_denied). MESSAGE PAYLOADS ARE NEVER LOGGED — only
//     ids, byte counts and result metadata.
//   - Inbound messages create runs through the runs + queue services'
//     EXPORTED API (CreateRunCtx + Enqueue("agent.run", ...)) — the same
//     two exported calls the interactive create-run handler makes, via the
//     internal/a2a RunSink seam so the service stays stubbable in tests.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"agentos/internal/a2a"
	"agentos/internal/apikeys"
	"agentos/internal/audit"
	"agentos/internal/auth"
	"agentos/internal/queue"
	"agentos/internal/runs"
)

// registerA2ARoutes mounts the A2A protocol endpoints.
func registerA2ARoutes(apiMux *http.ServeMux, svc *a2a.Service, runsSvc *runs.Service, workQueue *queue.Queue, authSvc *auth.Service, apiKeysSvc *apikeys.Service, auditSvc *audit.Service) {
	if apiMux == nil || svc == nil {
		return
	}
	// Wire the inbound-message execution seam over the runs + queue
	// services' exported API (the sink degrades explicitly — 503 — when
	// neither service is available).
	svc.SetRunSink(a2aRunSink{runsSvc: runsSvc, workQueue: workQueue})

	// auth wrap pattern from cmd/api/main.go / mcp_registry.go:
	// RequireAuthOrAPIKey outer, RequirePermission inner.
	wrap := func(perm auth.Permission, h http.Handler) http.Handler {
		return auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(auth.RequirePermission(authSvc, perm)(h))
	}

	apiMux.Handle("POST /a2a/agents", wrap(auth.PermissionAgentsWrite, http.HandlerFunc(createA2AAgentCardHandler(svc, auditSvc))))
	apiMux.Handle("GET /a2a/agents", wrap(auth.PermissionConnectorsRead, http.HandlerFunc(listA2AAgentCardsHandler(svc))))
	apiMux.Handle("GET /a2a/agents/{id}", wrap(auth.PermissionConnectorsRead, http.HandlerFunc(getA2AAgentCardHandler(svc))))
	apiMux.Handle("GET /a2a/agents/{id}/card", wrap(auth.PermissionAgentsRead, http.HandlerFunc(a2aDiscoveryCardHandler(svc))))
	apiMux.Handle("PATCH /a2a/agents/{id}", wrap(auth.PermissionAgentsWrite, http.HandlerFunc(patchA2AAgentCardHandler(svc, auditSvc))))
	apiMux.Handle("DELETE /a2a/agents/{id}", wrap(auth.PermissionAgentsWrite, http.HandlerFunc(deleteA2AAgentCardHandler(svc, auditSvc))))
	apiMux.Handle("POST /a2a/agents/{id}/message", wrap(auth.PermissionRunsExecute, http.HandlerFunc(a2aMessageHandler(svc, auditSvc))))
	apiMux.Handle("POST /a2a/delegate", wrap(auth.PermissionRunsExecute, http.HandlerFunc(a2aDelegateHandler(svc))))
}

// a2aRunSink adapts the runs + queue services to the internal/a2a RunSink
// seam: CreateRunCtx (exported) persists the QUEUED run, Enqueue (exported)
// hands "agent.run" to the worker — the exact two exported calls the
// interactive create-run handler makes. When only the queue is wired the
// message is recorded as a queued delegation and the task id stands in for
// the run id.
type a2aRunSink struct {
	runsSvc   *runs.Service
	workQueue *queue.Queue
}

// StartRun implements a2a.RunSink.
func (s a2aRunSink) StartRun(ctx context.Context, orgID, agentID, message string) (string, string, error) {
	if s.runsSvc == nil && s.workQueue == nil {
		return "", "", a2a.ErrRunSinkUnavailable
	}
	var runID string
	if s.runsSvc != nil {
		// Tenant guard: the run row is created with the caller's org scope.
		run, err := s.runsSvc.CreateRunCtx(ctx, orgID, agentID, message)
		if err != nil {
			return "", "", err
		}
		runID = run.ID
	}
	if s.workQueue == nil {
		// Degraded wiring: the run row exists but nothing can execute it.
		return runID, runID, nil
	}
	payload := map[string]any{"organization_id": orgID, "agent_id": agentID, "input": message}
	if runID != "" {
		payload["run_id"] = runID
	}
	task := s.workQueue.Enqueue("agent.run", payload)
	if task == nil {
		return "", "", errors.New("failed to enqueue run")
	}
	if runID == "" {
		runID = task.ID
	}
	return runID, task.ID, nil
}

// writeJSONA2A serializes v with the given status (distinct helper name to
// avoid clashing with helpers in other handler files).
func writeJSONA2A(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeA2AError emits the contract error envelope:
// {"error": {"code": "MACHINE_READABLE_CODE", "message": "..."}}.
func writeA2AError(w http.ResponseWriter, status int, code, message string) {
	writeJSONA2A(w, status, map[string]any{
		"error": map[string]any{"code": code, "message": message},
	})
}

// readA2AJSON decodes the request body into dst, writing a 400 envelope on
// malformed JSON. Returns false when the response is already written.
func readA2AJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeA2AError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
		return false
	}
	return true
}

// a2aCardID extracts the path value or renders the 404 (no existence leak
// for empty ids), returning "" when the response is already written.
func a2aCardID(w http.ResponseWriter, r *http.Request) string {
	id := r.PathValue("id")
	if id == "" {
		writeA2AError(w, http.StatusNotFound, "AGENT_CARD_NOT_FOUND", "agent card not found")
		return ""
	}
	return id
}

// mapA2AError converts a2a service errors into contract error responses.
// Validation failures are 422; unknown/foreign ids are 404 without an
// existence leak; a gate denial is the policy-style 403 delegation_denied;
// anything else is a generic 500.
func mapA2AError(w http.ResponseWriter, err error) {
	var denied *a2a.DelegationDeniedError
	switch {
	case errors.As(err, &denied):
		writeJSONA2A(w, http.StatusForbidden, map[string]any{
			"error":   map[string]any{"code": "delegation_denied", "message": err.Error()},
			"verdict": map[string]any{"decision": "delegation_denied", "reason": denied.Verdict.Reason},
		})
	case errors.Is(err, a2a.ErrNotFound):
		writeA2AError(w, http.StatusNotFound, "AGENT_CARD_NOT_FOUND", "agent card not found")
	case errors.Is(err, a2a.ErrOrgRequired),
		errors.Is(err, a2a.ErrNameRequired),
		errors.Is(err, a2a.ErrNameTooLong),
		errors.Is(err, a2a.ErrVersionRequired),
		errors.Is(err, a2a.ErrAgentRequired),
		errors.Is(err, a2a.ErrAgentUnknown),
		errors.Is(err, a2a.ErrEndpointInvalid),
		errors.Is(err, a2a.ErrEndpointRequired),
		errors.Is(err, a2a.ErrMessageRequired),
		errors.Is(err, a2a.ErrCardTargetRequired),
		errors.Is(err, a2a.ErrCardInvalid):
		writeA2AError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", err.Error())
	case errors.Is(err, a2a.ErrCardFetchFailed):
		writeA2AError(w, http.StatusUnprocessableEntity, "CARD_FETCH_FAILED", err.Error())
	case errors.Is(err, a2a.ErrRunSinkUnavailable):
		writeA2AError(w, http.StatusServiceUnavailable, "RUN_SINK_UNAVAILABLE", err.Error())
	case errors.Is(err, a2a.ErrDelegationUnreachable):
		writeA2AError(w, http.StatusBadGateway, "DELEGATION_UNREACHABLE", err.Error())
	default:
		writeA2AError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
	}
}

// a2aCardPayload mirrors the HTTP create body.
type a2aCardPayload struct {
	AgentID            string            `json:"agent_id"`
	Name               string            `json:"name"`
	Description        string            `json:"description"`
	Version            string            `json:"version"`
	Provider           *a2a.CardProvider `json:"provider"`
	EndpointURL        string            `json:"endpoint_url"`
	Capabilities       *a2a.Capabilities `json:"capabilities"`
	Skills             []a2a.Skill       `json:"skills"`
	AuthScheme         string            `json:"auth_scheme"`
	PreferredTransport string            `json:"preferred_transport"`
}

// registryCardJSON renders one card through an explicit field allowlist.
// auth_scheme is a SCHEME NAME; no credential material exists on a card.
func registryCardJSON(c *a2a.AgentCard) map[string]any {
	skills := make([]map[string]any, 0, len(c.Skills))
	for _, s := range c.Skills {
		tags := s.Tags
		if tags == nil {
			tags = []string{}
		}
		skills = append(skills, map[string]any{
			"id":          s.ID,
			"name":        s.Name,
			"description": s.Description,
			"tags":        tags,
		})
	}
	return map[string]any{
		"id":              c.ID,
		"organization_id": c.OrganizationID,
		"agent_id":        c.AgentID,
		"name":            c.Name,
		"description":     c.Description,
		"version":         c.Version,
		"provider": map[string]any{
			"organization": c.Provider.Organization,
			"url":          c.Provider.URL,
		},
		"endpoint_url": c.EndpointURL,
		"capabilities": map[string]any{
			"streaming":                c.Capabilities.Streaming,
			"push_notifications":       c.Capabilities.PushNotifications,
			"state_transition_history": c.Capabilities.StateTransitionHistory,
		},
		"skills":              skills,
		"auth_scheme":         c.AuthScheme,
		"preferred_transport": c.PreferredTransport,
		"created_by":          c.CreatedBy,
		"created_at":          c.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":          c.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// createA2AAgentCardHandler publishes one org-scoped Agent Card.
func createA2AAgentCardHandler(svc *a2a.Service, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeA2AError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		var req a2aCardPayload
		if !readA2AJSON(w, r, &req) {
			return
		}
		input := a2a.CreateCardInput{
			AgentID:            req.AgentID,
			Name:               req.Name,
			Description:        req.Description,
			Version:            req.Version,
			EndpointURL:        req.EndpointURL,
			Skills:             req.Skills,
			AuthScheme:         req.AuthScheme,
			PreferredTransport: req.PreferredTransport,
			CreatedBy:          claims.UserID,
		}
		if req.Provider != nil {
			input.Provider = *req.Provider
		}
		if req.Capabilities != nil {
			input.Capabilities = *req.Capabilities
		}
		// Tenant guard: the card is created with the caller's
		// organization_id; client-supplied org ids are ignored by design.
		card, err := svc.CreateCard(r.Context(), claims.OrganizationID, input)
		if err != nil {
			mapA2AError(w, err)
			return
		}
		if auditSvc != nil {
			// best-effort audit row (endpoint URLs are topology, not secrets)
			_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, "a2a.card_published",
				claims.OrganizationID, "a2a_cards/"+card.ID,
				map[string]any{"name": card.Name, "agent_id": card.AgentID, "version": card.Version, "endpoint_url": card.EndpointURL})
		}
		writeJSONA2A(w, http.StatusCreated, map[string]any{"agent_card": registryCardJSON(card)})
	}
}

// listA2AAgentCardsHandler returns the caller's Agent Cards.
func listA2AAgentCardsHandler(svc *a2a.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeA2AError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		list, err := svc.ListCards(r.Context(), claims.OrganizationID)
		if err != nil {
			mapA2AError(w, err)
			return
		}
		items := make([]map[string]any, 0, len(list))
		for _, card := range list {
			items = append(items, registryCardJSON(card))
		}
		writeJSONA2A(w, http.StatusOK, map[string]any{"agent_cards": items})
	}
}

// getA2AAgentCardHandler returns one card row within the caller's org.
func getA2AAgentCardHandler(svc *a2a.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeA2AError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		id := a2aCardID(w, r)
		if id == "" {
			return
		}
		card, err := svc.GetCard(r.Context(), claims.OrganizationID, id)
		if err != nil {
			mapA2AError(w, err)
			return
		}
		writeJSONA2A(w, http.StatusOK, map[string]any{"agent_card": registryCardJSON(card)})
	}
}

// a2aDiscoveryCardHandler renders the CANONICAL Agent Card JSON — the A2A
// discovery surface a generic A2A client consumes (camelCase keys, no
// platform internals).
func a2aDiscoveryCardHandler(svc *a2a.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeA2AError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		id := a2aCardID(w, r)
		if id == "" {
			return
		}
		card, err := svc.GetCard(r.Context(), claims.OrganizationID, id)
		if err != nil {
			mapA2AError(w, err)
			return
		}
		writeJSONA2A(w, http.StatusOK, card.ToJSON())
	}
}

// a2aCardPatchPayload mirrors the HTTP PATCH body (nil fields keep the
// stored value).
type a2aCardPatchPayload struct {
	AgentID            *string           `json:"agent_id"`
	Name               *string           `json:"name"`
	Description        *string           `json:"description"`
	Version            *string           `json:"version"`
	Provider           *a2a.CardProvider `json:"provider"`
	EndpointURL        *string           `json:"endpoint_url"`
	Capabilities       *a2a.Capabilities `json:"capabilities"`
	Skills             []a2a.Skill       `json:"skills"`
	AuthScheme         *string           `json:"auth_scheme"`
	PreferredTransport *string           `json:"preferred_transport"`
}

// patchA2AAgentCardHandler applies PATCH semantics to one card.
func patchA2AAgentCardHandler(svc *a2a.Service, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeA2AError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		id := a2aCardID(w, r)
		if id == "" {
			return
		}
		var req a2aCardPatchPayload
		if !readA2AJSON(w, r, &req) {
			return
		}
		updated, err := svc.UpdateCard(r.Context(), claims.OrganizationID, id, a2a.UpdateCardInput{
			AgentID:            req.AgentID,
			Name:               req.Name,
			Description:        req.Description,
			Version:            req.Version,
			Provider:           req.Provider,
			EndpointURL:        req.EndpointURL,
			Capabilities:       req.Capabilities,
			Skills:             req.Skills,
			AuthScheme:         req.AuthScheme,
			PreferredTransport: req.PreferredTransport,
			UpdatedBy:          claims.UserID,
		})
		if err != nil {
			mapA2AError(w, err)
			return
		}
		if auditSvc != nil {
			_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, "a2a.card_updated",
				claims.OrganizationID, "a2a_cards/"+id,
				map[string]any{"name": updated.Name, "agent_id": updated.AgentID, "version": updated.Version})
		}
		writeJSONA2A(w, http.StatusOK, map[string]any{"agent_card": registryCardJSON(updated)})
	}
}

// deleteA2AAgentCardHandler hard-deletes one card within the caller's
// organization (foreign/unknown ids surface as 404 without an existence
// leak).
func deleteA2AAgentCardHandler(svc *a2a.Service, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeA2AError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		id := a2aCardID(w, r)
		if id == "" {
			return
		}
		if err := svc.DeleteCard(r.Context(), claims.OrganizationID, id); err != nil {
			mapA2AError(w, err)
			return
		}
		if auditSvc != nil {
			_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, "a2a.card_deleted",
				claims.OrganizationID, "a2a_cards/"+id, nil)
		}
		writeJSONA2A(w, http.StatusOK, map[string]any{"deleted": true, "id": id})
	}
}

// a2aMessageHandler receives an inbound A2A-style message for one card:
// the service validates the card, resolves the target agent and starts the
// run through the RunSink (runs + queue exported API). The message payload
// is NEVER audited — only ids and its byte size.
func a2aMessageHandler(svc *a2a.Service, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeA2AError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		id := a2aCardID(w, r)
		if id == "" {
			return
		}
		var req struct {
			Message string `json:"message"`
		}
		if !readA2AJSON(w, r, &req) {
			return
		}
		inbound, err := svc.ReceiveMessage(r.Context(), claims.OrganizationID, id, req.Message, claims.UserID)
		if err != nil {
			mapA2AError(w, err)
			return
		}
		if auditSvc != nil {
			_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, "a2a.message_received",
				claims.OrganizationID, "a2a_cards/"+id,
				map[string]any{
					"card_id":       inbound.CardID,
					"agent_id":      inbound.AgentID,
					"run_id":        inbound.RunID,
					"task_id":       inbound.TaskID,
					"message_bytes": len(req.Message),
				})
		}
		writeJSONA2A(w, http.StatusAccepted, map[string]any{
			"card_id":  inbound.CardID,
			"agent_id": inbound.AgentID,
			"run_id":   inbound.RunID,
			"task_id":  inbound.TaskID,
			"status":   inbound.Status,
		})
	}
}

// a2aDelegateHandler performs an outbound delegation through the governed
// DelegateToAgentCard path (card resolution, policy gate, envelope POST).
// A gate denial is the policy-style verdict: 403 delegation_denied. A
// completed call — submitted or failed — is the outcome payload (200).
func a2aDelegateHandler(svc *a2a.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeA2AError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		var req struct {
			CardID  string `json:"card_id"`
			CardURL string `json:"card_url"`
			Message string `json:"message"`
		}
		if !readA2AJSON(w, r, &req) {
			return
		}
		result, err := svc.DelegateToAgentCard(r.Context(), claims.OrganizationID, a2a.DelegationRequest{
			CardID:  req.CardID,
			CardURL: req.CardURL,
			Message: req.Message,
			Actor:   claims.UserID,
		})
		if err != nil {
			mapA2AError(w, err)
			return
		}
		writeJSONA2A(w, http.StatusOK, map[string]any{
			"status":      result.Status,
			"http_status": result.HTTPStatus,
			"latency_ms":  result.LatencyMS,
			"card_id":     result.CardID,
			"endpoint":    result.Endpoint,
			"verdict":     result.Verdict,
		})
	}
}
