package main

// orchestration.go — issue #108 HTTP wiring: the runtime multi-agent
// orchestration executor surface (agent-to-agent communication bus,
// delegation protocol and supervisor) on top of internal/orchestration.
//
// Endpoints (registered on apiMux by registerOrchestrationRoutes; served
// under BOTH /v1 and /api/v1 like every versioned surface):
//
//      POST /orchestration/delegate            {from_agent_id,to_agent_id,task,...}
//                                              -> 202 async run ids (issue #108)
//      POST /orchestration/messages            {from_agent_id,to_agent_id,message_type,payload}
//                                              -> 202 point-to-point bus delivery
//      GET  /orchestration/agents/{id}/messages?limit=
//                                              -> the agent's retained inbox (newest first)
//      POST /orchestration/supervisor/run      {task,...} -> routed + delegated result
//      GET  /orchestration/delegations?run_id= -> the delegation ledger (chain trace)
//
// Design (mirrors cmd/api/residency.go / mcp_trust.go):
//   - RBAC uses the NEW issue #108 grants (internal/auth/permissions_orchestration.go):
//     reads are orchestration.read (OWNER/ADMIN/MEMBER/VIEWER — delegation
//     chains and message traffic are observability surfaces); writes are
//     orchestration.write (OWNER/ADMIN — delegations create runs and consume
//     model tokens, the same tier as runs.execute provisioning).
//   - Tenant scope comes EXCLUSIVELY from the auth claims: no org-addressable
//     path ids exist on this surface, so a caller can never name a foreign
//     tenant (cross-tenant reads 404 one layer down in the bus/ledger).
//   - The seams (runs factory, runtime executor, approval router) are narrow
//     interfaces defined in internal/orchestration and adapted HERE around the
//     dual-mode *runs.Service / *approvals.Service — internal/runs and
//     internal/approvals are import-only; nothing inside them changes.
//   - POST /orchestration/delegate is ASYNC by contract: Prepare (validation,
//     ledger, approval gate, child-run creation) happens on the request path
//     and the run ids return immediately; Execute happens on a background
//     goroutine and its outcome lands in the ledger + the delegator's inbox
//     (GET /orchestration/agents/{id}/messages, GET /orchestration/delegations).
//   - Response rendering is an explicit field allowlist; errors use the house
//     envelope {"error":{"code","message"}}.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"agentos/internal/agents"
	"agentos/internal/apikeys"
	"agentos/internal/approvals"
	"agentos/internal/audit"
	"agentos/internal/auth"
	"agentos/internal/models"
	"agentos/internal/orchestration"
	"agentos/internal/runs"
	"agentos/internal/runtime"
)

// orchestrationStepType is the runs step type carrying delegation linkage
// metadata (the runs.Run model has no parent_run_id column — the chain is
// recorded in step metadata, see the RunFactory adapter below).
const orchestrationStepType = "delegation"

// --- seam adapters (internal/orchestration defines the narrow interfaces) ---

// orchestrationRunFactory adapts the dual-mode *runs.Service to the
// orchestration.RunFactory seam: child runs are real runs rows (QUEUED) and
// the delegation chain lives in run step metadata.
type orchestrationRunFactory struct {
	runsSvc *runs.Service
}

// CreateSubRun implements orchestration.RunFactory: create the child run row
// for the delegated agent and record the delegation linkage (parent run id,
// depth, delegator, parked status) as its first step. Best-effort step write:
// the run row is the contract, the metadata step is traceability.
func (f orchestrationRunFactory) CreateSubRun(ctx context.Context, orgID, agentID, input string, meta map[string]any) (string, error) {
	run, err := f.runsSvc.CreateRunCtx(ctx, orgID, agentID, input)
	if err != nil {
		return "", err
	}
	if len(meta) > 0 {
		step := &runs.Step{StepType: orchestrationStepType, Status: "PENDING", InputMeta: meta}
		if s, ok := meta["status"].(string); ok && s != "" {
			step.Status = s
		}
		_ = f.runsSvc.RecordStep(ctx, orgID, run.ID, step)
	}
	return run.ID, nil
}

// FinishSubRun implements orchestration.RunFactory: drive the child run to a
// terminal status (only the orchestrator's error path calls it — the Executor
// owns terminal transitions on result paths).
func (f orchestrationRunFactory) FinishSubRun(ctx context.Context, orgID, runID, status, output string) error {
	rs := runs.StatusFailed
	if status == "COMPLETED" {
		rs = runs.StatusCompleted
	}
	return f.runsSvc.UpdateStatusCtx(ctx, orgID, runID, rs, output)
}

// RecordParentDelegationStep implements orchestration.RunFactory: append the
// delegation step (with child token usage metadata) to the PARENT run's
// timeline so child usage is attributed to the delegating run. Token counts
// are attributed here; monetary cost stays with the child run's own priced
// model steps (documented best-effort attribution, issue #108).
func (f orchestrationRunFactory) RecordParentDelegationStep(ctx context.Context, orgID, parentRunID string, meta map[string]any) error {
	step := &runs.Step{StepType: orchestrationStepType, Status: "COMPLETED", InputMeta: meta}
	if s, ok := meta["status"].(string); ok && s != "" {
		step.Status = s
	}
	if tokens, ok := meta["tokens"].(map[string]any); ok {
		step.TokenUsage = tokens
	}
	return f.runsSvc.RecordStep(ctx, orgID, parentRunID, step)
}

// orchestrationRuntimeExecutor adapts the shared runtime runner to the
// orchestration.Executor seam — the same execution shape the worker uses
// (cmd/worker: UpdateStatus RUNNING -> Runner.RunWithID -> terminal
// UpdateStatus), executed in-process by the API.
type orchestrationRuntimeExecutor struct {
	runner  *runtime.Runner
	runsSvc *runs.Service
}

// ExecuteRun implements orchestration.Executor: drive the pre-created child
// run through the runtime. On a returned error the child run's terminal state
// is left to the orchestrator (it finalizes FAILED via the RunFactory seam);
// on a result path this adapter has already driven the terminal transition.
func (e orchestrationRuntimeExecutor) ExecuteRun(ctx context.Context, req orchestration.ExecuteRequest) (*orchestration.ExecutionResult, error) {
	_ = e.runsSvc.UpdateStatusCtx(ctx, req.OrgID, req.RunID, runs.StatusRunning, "")
	out, rerr := e.runner.RunWithID(ctx, req.RunID, req.AgentID, req.Input)
	if rerr != nil {
		return nil, rerr
	}
	_ = e.runsSvc.UpdateStatusCtx(ctx, req.OrgID, req.RunID, runs.RunStatus(out.Status), out.Output)
	return &orchestration.ExecutionResult{
		RunID:  req.RunID,
		Status: string(out.Status),
		Output: out.Output,
		Error:  out.Error,
		Tokens: orchestration.Usage{
			PromptTokens:     out.Tokens.PromptTokens,
			CompletionTokens: out.Tokens.CompletionTokens,
			TotalTokens:      out.Tokens.TotalTokens,
		},
		Steps: out.Steps,
	}, nil
}

// orchestrationApprovalRouter adapts the internal/approvals service to the
// orchestration.ApprovalRouter seam (import-only; the approvals service is
// the human-in-the-loop source of truth).
type orchestrationApprovalRouter struct {
	approvalsSvc *approvals.Service
}

// Request implements orchestration.ApprovalRouter.
func (r orchestrationApprovalRouter) Request(ctx context.Context, orgID string, in orchestration.ApprovalInput) (string, error) {
	approval, err := r.approvalsSvc.Request(ctx, orgID, approvals.RequestInput{
		RunID:     in.RunID,
		Resource:  in.Resource,
		Action:    in.Action,
		Reason:    in.Reason,
		Risk:      in.Risk,
		Requester: in.Requester,
	})
	if err != nil {
		return "", err
	}
	return approval.ID, nil
}

// orchestrationStepRecorder mirrors the worker's stepRecorderAdapter: model
// and tool steps of orchestration sub-runs become queryable run_steps rows
// (with priced model steps), so child runs carry a full execution timeline.
func orchestrationStepRecorder(runsSvc *runs.Service, agentsSvc *agents.Service) runtime.StepRecorder {
	return runtime.StepRecorderFunc(func(ctx context.Context, runID string, step runtime.Step) error {
		if step.Input == "" && step.Output == "" && step.Error == "" {
			return nil
		}
		run, ok := runsSvc.Get(runID)
		if !ok {
			return nil // run already pruned; nothing to attach the step to
		}
		started := time.Now().UTC().Add(-time.Duration(step.DurationMS) * time.Millisecond)
		rs := &runs.Step{
			RunID:       runID,
			StepType:    step.Type,
			Status:      step.Status,
			InputMeta:   map[string]any{"input": step.Input, "name": step.Name, "index": step.Index},
			OutputMeta:  map[string]any{"output": step.Output},
			Error:       step.Error,
			StartedAt:   started,
			CompletedAt: time.Now().UTC(),
		}
		if step.TokenUsage.TotalTokens > 0 {
			rs.TokenUsage = map[string]any{
				"prompt_tokens":     step.TokenUsage.PromptTokens,
				"completion_tokens": step.TokenUsage.CompletionTokens,
				"total_tokens":      step.TokenUsage.TotalTokens,
			}
			// Price model steps through the pricing hook (unknown model ->
			// 0 cents, never an error) — same convention as the worker.
			if step.Type == runtime.StepTypeModel {
				if agent, aerr := agentsSvc.GetAgentCtx(context.Background(), run.OrganizationID, run.AgentID); aerr == nil {
					rs.Cost = models.ComputeCostCents(agent.Model,
						step.TokenUsage.PromptTokens, step.TokenUsage.CompletionTokens)
				}
			}
		}
		return runsSvc.RecordStep(ctx, run.OrganizationID, runID, rs)
	})
}

// newOrchestrationService builds the issue #108 orchestration facade for this
// process: in-memory comm bus + delegation ledger in BOTH modes (documented
// scope decision — no orchestration store/migration this round; the durable
// trail lives in the runs and approvals rows the seams write). The runtime
// runner follows the env-driven provider construction shared by the worker
// and the eval runner (offline deterministic mode without OPENAI_API_KEY).
func newOrchestrationService(agentsSvc *agents.Service, runsSvc *runs.Service, approvalsSvc *approvals.Service, logr *slog.Logger) *orchestration.Service {
	provider, haveProvider := models.ProviderFromEnv(logr)
	if haveProvider {
		logr.Info("orchestration executor: live model provider configured", "provider", provider.Name())
	} else {
		logr.Info("orchestration executor: offline deterministic mode (no OPENAI_API_KEY)")
	}
	orchRunner := runtime.NewRunnerWithOptions(agentsSvc, nil,
		runtime.WithProvider(provider),
		runtime.WithStepRecorder(orchestrationStepRecorder(runsSvc, agentsSvc)),
	)
	svc, err := orchestration.NewService(orchestration.Options{
		Runs:      orchestrationRunFactory{runsSvc: runsSvc},
		Executor:  orchestrationRuntimeExecutor{runner: orchRunner, runsSvc: runsSvc},
		Approvals: orchestrationApprovalRouter{approvalsSvc: approvalsSvc},
		// capability_match makes the HTTP contract's required_capability
		// field real (POST /orchestration/supervisor/run): tasks without
		// a requirement route in configuration order, tasks with one
		// are filtered to capable workers (typed ErrNoCapableWorker
		// otherwise). round_robin, the facade default, would silently
		// ignore the field.
		Strategy: orchestration.RouteCapabilityMatch,
	})
	if err != nil {
		// Unreachable with the default facade options (the strategy defaults
		// are validated); the nil return keeps the wiring nil-safe.
		logr.Error("orchestration service init failed", "error", err)
		return nil
	}
	return svc
}

// --- HTTP surface -----------------------------------------------------------

// registerOrchestrationRoutes mounts the issue #108 orchestration surface and
// nil-guards the wiring (mirrors registerResidencyRoutes).
func registerOrchestrationRoutes(apiMux *http.ServeMux, svc *orchestration.Service, authSvc *auth.Service, apiKeysSvc *apikeys.Service, auditSvc *audit.Service) {
	if apiMux == nil || svc == nil {
		return
	}
	// auth wrap pattern from cmd/api/main.go / residency.go:
	// RequireAuthOrAPIKey outer, RequirePermission inner.
	wrap := func(perm auth.Permission, h http.Handler) http.Handler {
		return auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(auth.RequirePermission(authSvc, perm)(h))
	}
	// Writes: orchestration.write (OWNER/ADMIN) — delegations create runs and
	// consume model tokens; sends write into other agents' inboxes.
	apiMux.Handle("POST /orchestration/delegate", wrap(auth.PermissionOrchestrationWrite, http.HandlerFunc(orchestrationDelegateHandler(svc, auditSvc))))
	apiMux.Handle("POST /orchestration/messages", wrap(auth.PermissionOrchestrationWrite, http.HandlerFunc(orchestrationMessageSendHandler(svc, auditSvc))))
	apiMux.Handle("POST /orchestration/supervisor/run", wrap(auth.PermissionOrchestrationWrite, http.HandlerFunc(orchestrationSupervisorRunHandler(svc, auditSvc))))
	// Reads: orchestration.read (all roles — traceability surfaces).
	apiMux.Handle("GET /orchestration/agents/{id}/messages", wrap(auth.PermissionOrchestrationRead, http.HandlerFunc(orchestrationMessagesGetHandler(svc))))
	apiMux.Handle("GET /orchestration/delegations", wrap(auth.PermissionOrchestrationRead, http.HandlerFunc(orchestrationDelegationsGetHandler(svc))))
}

// --- shared helpers (distinct names; same envelope as residency.go) ---------

func writeOrchJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeOrchError(w http.ResponseWriter, status int, code, message string) {
	writeOrchJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": message}})
}

// decodeOrchBody decodes the JSON request body bounded to 1 MiB (the
// decodeResBody convention). Returns false when the response was written.
func decodeOrchBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.Body == nil {
		return true
	}
	defer r.Body.Close() //nolint:errcheck // read path; nothing actionable
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeOrchError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid request body")
		return false
	}
	if len(raw) == 0 {
		return true
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		writeOrchError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid JSON body")
		return false
	}
	return true
}

// orchClaims extracts the caller claims or writes the 401/404 shape,
// returning ok=false when the response was already written.
func orchClaims(w http.ResponseWriter, r *http.Request) (auth.UserClaims, bool) {
	claims, err := auth.ExtractClaims(r.Context())
	if err != nil {
		writeOrchError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
		return auth.UserClaims{}, false
	}
	if claims.OrganizationID == "" {
		writeOrchError(w, http.StatusNotFound, "NOT_FOUND", "organization not found")
		return auth.UserClaims{}, false
	}
	return claims, true
}

// orchAudit writes one best-effort audit row (house style: audit failures
// never fail the request). Nil-safe on the audit service.
func orchAudit(ctx context.Context, auditSvc *audit.Service, actor, action, orgID, resource string, metadata map[string]any) {
	if auditSvc == nil {
		return
	}
	_, _ = auditSvc.LogCtx(ctx, actor, action, orgID, resource, metadata)
}

// orchestrationUsageJSON renders the token usage block (snake_case).
func orchestrationUsageJSON(u orchestration.Usage) map[string]any {
	return map[string]any{
		"prompt_tokens":     u.PromptTokens,
		"completion_tokens": u.CompletionTokens,
		"total_tokens":      u.TotalTokens,
	}
}

// orchDelegationJSON renders one ledger record (explicit field allowlist).
func orchDelegationJSON(rec *orchestration.DelegationRecord) map[string]any {
	var completedAt any
	if !rec.CompletedAt.IsZero() {
		completedAt = rec.CompletedAt.UTC().Format(time.RFC3339)
	}
	return map[string]any{
		"id":                 rec.ID,
		"org_id":             rec.OrgID,
		"from_agent_id":      rec.FromAgentID,
		"to_agent_id":        rec.ToAgentID,
		"task":               rec.Task,
		"parent_run_id":      rec.ParentRunID,
		"child_run_id":       rec.ChildRunID,
		"depth":              rec.Depth,
		"status":             rec.Status,
		"approval_id":        rec.ApprovalID,
		"result":             rec.Result,
		"error":              rec.Error,
		"tokens":             orchestrationUsageJSON(rec.Tokens),
		"response_delivered": rec.ResponseDelivered,
		"created_at":         rec.CreatedAt.UTC().Format(time.RFC3339),
		"completed_at":       completedAt,
	}
}

// --- handlers -----------------------------------------------------------------

// orchestrationDelegateHandler is the async delegation entry point: Prepare
// on the request path (validation -> ledger -> delegator inbox registration
// -> approval gate -> child run creation) and 202 with the run ids; Execute
// runs on a background goroutine and its outcome lands in the ledger + the
// delegator's inbox (the two-phase protocol split — the record is the source
// of truth for the terminal state).
func orchestrationDelegateHandler(svc *orchestration.Service, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := orchClaims(w, r)
		if !ok {
			return
		}
		var body struct {
			FromAgentID     string `json:"from_agent_id"`
			ToAgentID       string `json:"to_agent_id"`
			Task            string `json:"task"`
			ParentRunID     string `json:"parent_run_id"`
			RequireApproval bool   `json:"require_approval"`
			// Depth is the delegation depth of the CALLING chain (0 for
			// top-level requests; the server enforces the configured limit).
			Depth int `json:"depth"`
		}
		if !decodeOrchBody(w, r, &body) {
			return
		}
		// The delegate contract requires a delegator agent (supervisor-routed
		// tasks belong to POST /orchestration/supervisor/run, where the empty
		// delegator is legal). The service itself tolerates an empty
		// FromAgentID; this surface pins the stricter contract.
		if strings.TrimSpace(body.FromAgentID) == "" || strings.TrimSpace(body.ToAgentID) == "" ||
			strings.TrimSpace(body.Task) == "" {
			writeOrchError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR",
				"from_agent_id, to_agent_id and task are required")
			return
		}
		rec, err := svc.Delegation.Prepare(r.Context(), orchestration.DelegationRequest{
			OrgID:           claims.OrganizationID,
			FromAgentID:     body.FromAgentID,
			ToAgentID:       body.ToAgentID,
			Task:            body.Task,
			ParentRunID:     body.ParentRunID,
			Depth:           body.Depth,
			RequireApproval: body.RequireApproval,
			Requester:       claims.UserID,
		})
		if err != nil {
			status, code := orchestrationPrepareErrorStatus(err)
			errBody := map[string]any{
				"error": map[string]any{"code": code, "message": err.Error()},
			}
			if rec != nil {
				errBody["delegation_id"] = rec.ID // the failed record stays traceable
			}
			writeOrchJSON(w, status, errBody)
			return
		}
		// Snapshot BEFORE spawning the executor goroutine (Execute mutates the
		// record; the response must render the prepared state).
		resp := map[string]any{
			"delegation_id": rec.ID,
			"child_run_id":  rec.ChildRunID,
			"status":        rec.Status,
			"depth":         rec.Depth,
		}
		if rec.ApprovalID != "" {
			resp["approval_id"] = rec.ApprovalID
		}
		orchAudit(r.Context(), auditSvc, claims.UserID, "orchestration.delegate", claims.OrganizationID,
			"orchestration/delegations/"+rec.ID, map[string]any{
				"from_agent_id": rec.FromAgentID,
				"to_agent_id":   rec.ToAgentID,
				"child_run_id":  rec.ChildRunID,
				"parent_run_id": rec.ParentRunID,
				"depth":         rec.Depth,
				"status":        rec.Status,
			})
		if rec.Status == orchestration.DelegationStatusPending {
			// Async execution (issue #108 contract: the run ids return
			// immediately). WithoutCancel detaches from the request lifecycle
			// while keeping trace values; the ledger + the delegator's inbox
			// carry the outcome.
			go func() {
				_, _ = svc.Delegation.Execute(context.WithoutCancel(r.Context()), rec)
			}()
		}
		writeOrchJSON(w, http.StatusAccepted, resp)
	}
}

// orchestrationPrepareErrorStatus maps Prepare failures onto the HTTP
// contract: request problems 422, unwired approval gate 409 (fail-closed),
// downstream failures 500 (the ledger holds the failed record either way).
func orchestrationPrepareErrorStatus(err error) (int, string) {
	switch {
	case errors.Is(err, orchestration.ErrInvalidRequest), errors.Is(err, orchestration.ErrDepthExceeded):
		return http.StatusUnprocessableEntity, "VALIDATION_ERROR"
	case errors.Is(err, orchestration.ErrApprovalUnavailable):
		return http.StatusConflict, "APPROVAL_UNAVAILABLE"
	default:
		return http.StatusInternalServerError, "DELEGATION_FAILED"
	}
}

// orchestrationMessageSendHandler delivers one point-to-point message through
// the comm bus. Delivery is synchronous and non-blocking: an unknown target
// inbox is 404, a saturated inbox is 409 (the caller may retry), success is
// 202 with the message id.
func orchestrationMessageSendHandler(svc *orchestration.Service, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := orchClaims(w, r)
		if !ok {
			return
		}
		var body struct {
			FromAgentID string          `json:"from_agent_id"`
			ToAgentID   string          `json:"to_agent_id"`
			MessageType string          `json:"message_type"`
			RunID       string          `json:"run_id"`
			Payload     json.RawMessage `json:"payload"`
		}
		if !decodeOrchBody(w, r, &body) {
			return
		}
		if strings.TrimSpace(body.FromAgentID) == "" || strings.TrimSpace(body.ToAgentID) == "" {
			writeOrchError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR",
				"from_agent_id and to_agent_id are required")
			return
		}
		msg := orchestration.AgentMessage{
			OrgID:       claims.OrganizationID,
			FromAgentID: body.FromAgentID,
			ToAgentID:   body.ToAgentID,
			RunID:       body.RunID,
			MessageType: body.MessageType,
			Payload:     body.Payload,
		}
		if err := svc.Bus.Send(r.Context(), msg); err != nil {
			switch {
			case errors.Is(err, orchestration.ErrInvalidMessage):
				writeOrchError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", err.Error())
			case errors.Is(err, orchestration.ErrNoSubscriber):
				writeOrchError(w, http.StatusNotFound, "NOT_FOUND", err.Error())
			case errors.Is(err, orchestration.ErrBusFull):
				writeOrchError(w, http.StatusConflict, "INBOX_FULL", err.Error())
			default:
				writeOrchError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
			}
			return
		}
		orchAudit(r.Context(), auditSvc, claims.UserID, "orchestration.message_sent", claims.OrganizationID,
			"orchestration/agents/"+body.ToAgentID, map[string]any{
				"from_agent_id": body.FromAgentID,
				"to_agent_id":   body.ToAgentID,
				"message_type":  body.MessageType,
				"run_id":        body.RunID,
			})
		writeOrchJSON(w, http.StatusAccepted, map[string]any{"delivered": true})
	}
}

// orchestrationMessagesGetHandler renders an agent's retained inbox (newest
// first). The inbox is org-scoped: the same agent id in another organization
// resolves to a different (possibly absent) inbox.
func orchestrationMessagesGetHandler(svc *orchestration.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := orchClaims(w, r)
		if !ok {
			return
		}
		agentID := r.PathValue("id")
		limit := 0
		if raw := r.URL.Query().Get("limit"); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 0 {
				writeOrchError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR",
					"limit must be a non-negative integer")
				return
			}
			limit = parsed
		}
		messages, err := svc.Bus.RecentMessages(claims.OrganizationID, agentID, limit)
		if err != nil {
			switch {
			case errors.Is(err, orchestration.ErrInvalidMessage):
				writeOrchError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", err.Error())
			case errors.Is(err, orchestration.ErrNoSubscriber):
				writeOrchError(w, http.StatusNotFound, "NOT_FOUND", err.Error())
			default:
				writeOrchError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
			}
			return
		}
		if messages == nil {
			messages = []orchestration.AgentMessage{}
		}
		writeOrchJSON(w, http.StatusOK, map[string]any{
			"agent_id": agentID,
			"count":    len(messages),
			"messages": messages,
		})
	}
}

// orchestrationSupervisorRunHandler routes one task through the supervisor:
// select a worker (configured routing strategy), delegate through the
// protocol and aggregate. Completed results are 200; approval-parked results
// are 202; routing exhaustion surfaces the typed supervisor errors.
func orchestrationSupervisorRunHandler(svc *orchestration.Service, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := orchClaims(w, r)
		if !ok {
			return
		}
		var body struct {
			Task               string `json:"task"`
			TaskRef            string `json:"task_ref"`
			FromAgentID        string `json:"from_agent_id"`
			ParentRunID        string `json:"parent_run_id"`
			RequiredCapability string `json:"required_capability"`
		}
		if !decodeOrchBody(w, r, &body) {
			return
		}
		result, err := svc.Supervisor.RunTask(r.Context(), orchestration.SupervisorTask{
			OrgID:              claims.OrganizationID,
			Task:               body.Task,
			TaskRef:            body.TaskRef,
			FromAgentID:        body.FromAgentID,
			ParentRunID:        body.ParentRunID,
			RequiredCapability: body.RequiredCapability,
		})
		if err != nil {
			var supErr *orchestration.SupervisorError
			switch {
			case errors.Is(err, orchestration.ErrInvalidRequest):
				writeOrchError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", err.Error())
			case errors.Is(err, orchestration.ErrNoWorkers):
				writeOrchError(w, http.StatusConflict, "NO_WORKERS", err.Error())
			case errors.Is(err, orchestration.ErrNoCapableWorker):
				writeOrchError(w, http.StatusConflict, "NO_CAPABLE_WORKER", err.Error())
			case errors.As(err, &supErr):
				attempts := make([]map[string]any, 0, len(supErr.Attempts))
				for _, a := range supErr.Attempts {
					attempts = append(attempts, map[string]any{
						"worker_agent_id": a.WorkerAgentID,
						"delegation_id":   a.DelegationID,
						"error":           a.Error,
					})
				}
				writeOrchJSON(w, http.StatusBadGateway, map[string]any{
					"error":    map[string]any{"code": "ALL_WORKERS_FAILED", "message": err.Error()},
					"task_ref": supErr.TaskRef,
					"attempts": attempts,
				})
			default:
				writeOrchError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
			}
			return
		}
		status := http.StatusOK
		if result.Status == orchestration.DelegationStatusAwaitingApproval {
			status = http.StatusAccepted
		}
		orchAudit(r.Context(), auditSvc, claims.UserID, "orchestration.supervisor_run", claims.OrganizationID,
			"orchestration/delegations/"+result.DelegationID, map[string]any{
				"task_ref":        result.TaskRef,
				"worker_agent_id": result.WorkerAgentID,
				"delegation_id":   result.DelegationID,
				"child_run_id":    result.ChildRunID,
				"status":          result.Status,
			})
		writeOrchJSON(w, status, map[string]any{
			"task_ref":        result.TaskRef,
			"task":            result.Task,
			"status":          result.Status,
			"worker_agent_id": result.WorkerAgentID,
			"delegation_id":   result.DelegationID,
			"child_run_id":    result.ChildRunID,
			"output":          result.Output,
			"tokens":          orchestrationUsageJSON(result.Tokens),
			"attempts":        supervisorAttemptsJSON(result.Attempts),
		})
	}
}

// supervisorAttemptsJSON renders the failed-attempt trace (possibly empty).
func supervisorAttemptsJSON(attempts []orchestration.WorkerAttempt) []map[string]any {
	out := make([]map[string]any, 0, len(attempts))
	for _, a := range attempts {
		out = append(out, map[string]any{
			"worker_agent_id": a.WorkerAgentID,
			"delegation_id":   a.DelegationID,
			"error":           a.Error,
		})
	}
	return out
}

// orchestrationDelegationsGetHandler renders the org's delegation ledger
// (chronological — chain order). Optional run_id filter matches records whose
// parent OR child run equals the id (the delegation chain visible from one
// run). Cross-tenant records never appear: the ledger is org-scoped.
func orchestrationDelegationsGetHandler(svc *orchestration.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := orchClaims(w, r)
		if !ok {
			return
		}
		runID := r.URL.Query().Get("run_id")
		records := svc.Delegation.ListDelegations(claims.OrganizationID, runID)
		rendered := make([]map[string]any, 0, len(records))
		for _, rec := range records {
			rendered = append(rendered, orchDelegationJSON(rec))
		}
		writeOrchJSON(w, http.StatusOK, map[string]any{
			"delegations": rendered,
			"count":       len(rendered),
		})
	}
}

// compile-time interface checks for the seam adapters.
var (
	_ orchestration.RunFactory     = orchestrationRunFactory{}
	_ orchestration.Executor       = orchestrationRuntimeExecutor{}
	_ orchestration.ApprovalRouter = orchestrationApprovalRouter{}
)
