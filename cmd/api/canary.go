package main

// Canary deployments HTTP handlers (issue #13). These routes change WHAT
// SERVES TRAFFIC for an agent+environment, so every one of them requires the
// strictest existing deployment permission (deployments.deploy -> OWNER/ADMIN;
// no new permission enum was introduced - see internal/auth/
// permissions_deployments.go).
//
// Endpoints (registered on apiMux by registerCanaryRoutes, called from
// registerDeploymentsRoutes; served under BOTH /v1 and /api/v1):
//
//      POST /deployments/{id}/canary          -> deployments.deploy
//      POST /deployments/{id}/canary/promote  -> deployments.deploy
//      POST /deployments/{id}/canary/abort    -> deployments.deploy
//      GET  /agents/{agentId}/canary/status   -> runs.read   (issue #51, additive)
//
// The tenant is taken from the auth claims only; error bodies use the shared
// {"error":{"code","message"}} envelope via writeErrorVD. Semantics:
//
//      canary         attach/replace the canary version and/or move the split
//                     point (omitted fields keep their current value); the
//                     optional canary_policy attaches/replaces the eval-gated
//                     promotion policy (issue #51)
//      canary/promote canary becomes stable (version swap, canary cleared)
//      canary/abort   clear the canary, keep the stable version
//      canary/status  read model: current split %, policy in effect, last
//                     automatic decision (+reason) and fresh sample stats —
//                     runs.read is enough (read-only, all roles)

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"agentos/internal/apikeys"
	"agentos/internal/audit"
	"agentos/internal/auth"
	"agentos/internal/deployments"
	"agentos/internal/evaluations"
)

// setDeploymentCanaryHandler serves POST /deployments/{id}/canary with body
// {"canary_version": int (optional), "canary_weight": int (optional),
// "canary_policy": object (optional, issue #51)}. At least one field must be
// provided. canary_version attaches/replaces the canary (must be an EXISTING
// version of the SAME agent - any publication status, because only one
// version can be published at a time and a canary by definition runs NEXT to
// the stable version - differing from the stable version); canary_weight
// moves the 0-100 split point (requires an attached canary; out-of-range
// values are rejected, not clamped); canary_policy attaches/replaces (or
// clears, with null) the eval-gated promotion policy and opens a fresh
// evaluation window.
func setDeploymentCanaryHandler(depSvc *deployments.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if strings.TrimSpace(id) == "" {
			writeErrorVD(w, http.StatusNotFound, "DEPLOYMENT_NOT_FOUND", "deployment not found")
			return
		}
		orgID, ok := claimsOrgIDVD(w, r)
		if !ok {
			return
		}
		var req struct {
			CanaryVersion *int                              `json:"canary_version"`
			CanaryWeight  *int                              `json:"canary_weight"`
			CanaryPolicy  *deployments.AgentPromotionPolicy `json:"canary_policy"`
		}
		if !readJSONVD(w, r, &req) {
			return
		}
		if req.CanaryVersion == nil && req.CanaryWeight == nil && req.CanaryPolicy == nil {
			writeErrorVD(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "canary_version, canary_weight or canary_policy is required")
			return
		}
		// Tenant guard: the service scopes every lookup by organization_id.
		deployment, err := depSvc.GetDeploymentCtx(r.Context(), orgID, id)
		if err != nil {
			if !writeDeploymentsError(w, err) {
				writeErrorVD(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			}
			return
		}
		// Order matters: attach the version first so a {canary_version,
		// canary_policy} body also works on a canary-less row (the policy
		// requires an attached canary), then the policy, then the weight.
		if req.CanaryVersion != nil {
			deployment, err = depSvc.SetCanaryVersionCtx(r.Context(), orgID, id, *req.CanaryVersion)
			if err != nil {
				if !writeDeploymentsError(w, err) {
					writeErrorVD(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
				}
				return
			}
		}
		if req.CanaryPolicy != nil {
			deployment, err = depSvc.SetCanaryPromotionPolicyCtx(r.Context(), orgID, id, req.CanaryPolicy)
			if err != nil {
				if !writeDeploymentsError(w, err) {
					writeErrorVD(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
				}
				return
			}
		}
		if req.CanaryWeight != nil {
			deployment, err = depSvc.SetCanaryWeightCtx(r.Context(), orgID, id, *req.CanaryWeight)
			if err != nil {
				if !writeDeploymentsError(w, err) {
					writeErrorVD(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
				}
				return
			}
		}
		writeJSONVD(w, http.StatusOK, map[string]any{"deployment": newDeploymentView(deployment)})
	}
}

// promoteCanaryHandler serves POST /deployments/{id}/canary/promote: the
// canary becomes the stable version (deployment.Version swaps to the canary
// version and the canary config is cleared; the row stays healthy).
func promoteCanaryHandler(depSvc *deployments.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		orgID, ok := claimsOrgIDVD(w, r)
		if !ok {
			return
		}
		deployment, err := depSvc.PromoteCanaryCtx(r.Context(), orgID, id)
		if err != nil {
			if !writeDeploymentsError(w, err) {
				writeErrorVD(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			}
			return
		}
		writeJSONVD(w, http.StatusOK, map[string]any{"deployment": newDeploymentView(deployment)})
	}
}

// abortCanaryHandler serves POST /deployments/{id}/canary/abort: clears the
// canary config and keeps the stable version serving 100% of traffic.
func abortCanaryHandler(depSvc *deployments.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		orgID, ok := claimsOrgIDVD(w, r)
		if !ok {
			return
		}
		deployment, err := depSvc.AbortCanaryCtx(r.Context(), orgID, id)
		if err != nil {
			if !writeDeploymentsError(w, err) {
				writeErrorVD(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			}
			return
		}
		writeJSONVD(w, http.StatusOK, map[string]any{"deployment": newDeploymentView(deployment)})
	}
}

// registerCanaryRoutes mounts the canary traffic-split routes on apiMux.
// Permission mapping: deployments.deploy (OWNER/ADMIN) for the
// traffic-changing operations - these change what serves traffic, exactly
// like promote/rollback; runs.read (all roles) for the additive read-only
// status endpoint (issue #51), which carries run/eval sample aggregates.
// Called from registerDeploymentsRoutes so the whole deployment surface stays
// registered in one place.
func registerCanaryRoutes(apiMux *http.ServeMux, depSvc *deployments.Service, authSvc *auth.Service, apiKeysSvc *apikeys.Service) {
	wrap := func(perm auth.Permission, h http.HandlerFunc) http.Handler {
		return auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(auth.RequirePermission(authSvc, perm)(h))
	}
	// ServeMux precedence: the /promote and /abort patterns are more
	// specific than /canary and win under real wiring.
	apiMux.Handle("POST /deployments/{id}/canary", wrap(auth.PermissionDeploymentsDeploy, setDeploymentCanaryHandler(depSvc)))
	apiMux.Handle("POST /deployments/{id}/canary/promote", wrap(auth.PermissionDeploymentsDeploy, promoteCanaryHandler(depSvc)))
	apiMux.Handle("POST /deployments/{id}/canary/abort", wrap(auth.PermissionDeploymentsDeploy, abortCanaryHandler(depSvc)))
	// Issue #51 (additive): the eval-gated promotion status read model. More
	// specific than the /agents/ catch-all, so it wins under real wiring.
	apiMux.Handle("GET /agents/{agentId}/canary/status", wrap(auth.PermissionRunsRead, canaryStatusHandler(depSvc)))
}

// canaryStatusHandler serves GET /agents/{agentId}/canary/status
// (?environment=, default production): current split %, sample stats (runs
// counted, pass rate, p95, avg cost), last automatic decision + reason, and
// the policy in effect. Read-only surface (runs.read).
func canaryStatusHandler(depSvc *deployments.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		agentID := r.PathValue("agentId")
		if strings.TrimSpace(agentID) == "" {
			writeErrorVD(w, http.StatusNotFound, "AGENT_NOT_FOUND", "agent not found")
			return
		}
		orgID, ok := claimsOrgIDVD(w, r)
		if !ok {
			return
		}
		environment := r.URL.Query().Get("environment")
		if strings.TrimSpace(environment) == "" {
			environment = deployments.EnvironmentProduction
		}
		status, err := depSvc.CanaryStatusCtx(r.Context(), orgID, agentID, environment)
		if err != nil {
			switch {
			case errors.Is(err, deployments.ErrInvalidEnvironment):
				writeErrorVD(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", err.Error())
			case errors.Is(err, deployments.ErrNoServingDeployment):
				writeErrorVD(w, http.StatusNotFound, "NOT_FOUND", "no healthy deployment serves this agent+environment")
			default:
				writeErrorVD(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			}
			return
		}
		writeJSONVD(w, http.StatusOK, map[string]any{"canary_status": status})
	}
}

// WireCanaryAutoPromotion connects the evaluation runner to the canary
// promotion engine (issue #51) — the "CI/CD for agents" loop:
//
//	eval run completes (evaluations.Service.RunDataset)
//	  -> completion observer (async, detached context)
//	  -> deployments.Service.OnEvalRunCompleted
//	  -> policy decision (gated by AGENTOS_CANARY_AUTOPROMOTE, default OFF)
//	  -> promote/rollback + audit event + persisted reason
//
// The deployments engine pulls its evidence through the injected
// EvalSampleSource (adapted here from the evaluations service) so neither
// package imports the other. All wiring is nil-safe: services without a
// database (zero-infrastructure mode) behave identically. Called from
// newApp AFTER both services are constructed; the feature stays OFF unless
// AGENTOS_CANARY_AUTOPROMOTE is truthy.
func WireCanaryAutoPromotion(evalSvc *evaluations.Service, depSvc *deployments.Service, auditSvc *audit.Service, logr *slog.Logger) {
	if evalSvc == nil || depSvc == nil {
		return
	}
	// Evidence: completed eval-run aggregates for one agent (tenant-scoped).
	depSvc.SetCanarySampleSource(deployments.EvalSampleSourceFunc(
		func(ctx context.Context, orgID, agentID string, limit int) ([]deployments.EvalSample, error) {
			samples, err := evalSvc.ListRunSamples(ctx, orgID, agentID, limit)
			if err != nil {
				return nil, err
			}
			out := make([]deployments.EvalSample, 0, len(samples))
			for _, sample := range samples {
				out = append(out, deployments.EvalSample{
					RunID:       sample.RunID,
					CreatedAt:   sample.CreatedAt,
					Cases:       sample.Cases,
					Passed:      sample.Passed,
					LatenciesMS: sample.LatenciesMS,
					CostCents:   sample.CostCents,
				})
			}
			return out, nil
		}))
	// Audit: one structured entry per automatic decision (best-effort).
	if auditSvc != nil {
		depSvc.SetCanaryDecisionAuditer(deployments.CanaryDecisionAuditerFunc(
			func(ctx context.Context, orgID string, deployment *deployments.Deployment, decision *deployments.CanaryDecision) {
				action := "deployment.canary_auto_promote"
				if decision.Action == deployments.CanaryDecisionRollback {
					action = "deployment.canary_auto_rollback"
				}
				if _, err := auditSvc.LogCtx(ctx, "canary-autopilot", action, orgID, deployment.ID, map[string]any{
					"agent_id":       deployment.AgentID,
					"environment":    deployment.Environment,
					"reason":         decision.Reason,
					"runs_counted":   decision.RunsCounted,
					"pass_rate":      decision.PassRate,
					"p95_latency_ms": decision.P95LatencyMS,
					"avg_cost_cents": decision.AvgCostCents,
				}); err != nil {
					logr.Warn("canary decision audit failed", "error", err.Error())
				}
			}))
	}
	// Seam: fire the engine after every completed eval run.
	evalSvc.SetCompletionObserver(func(ctx context.Context, run *evaluations.EvalRun) {
		depSvc.OnEvalRunCompleted(ctx, run.OrganizationID, run.AgentID)
	})
	logr.Info("canary autopromotion wired", "flag", deployments.AutoPromoteEnvVar, "enabled", deployments.AutoPromotionFromEnv())
}
