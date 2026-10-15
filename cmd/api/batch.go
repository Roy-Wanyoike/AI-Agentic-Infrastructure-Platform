package main

// batch.go — issue #123 HTTP wiring: the batch/bulk operations surface on top
// of internal/batch.
//
// Endpoints (registered on apiMux by registerBatchRoutes; served under BOTH
// /v1 and /api/v1 like every versioned surface):
//
//	POST /batch/runs                    {agent_id|agent_ids[], input|inputs[], max_concurrency}
//	POST /batch/workflows/{id}/trigger  {inputs[], max_concurrency}
//	POST /batch/agents/update           {agent_ids[], patch{name?,description?,instructions?,model?}, max_concurrency}
//	POST /batch/agents/deploy           {agent_ids[], version?, environment?, max_concurrency}
//	POST /batch/runs/cancel             {run_ids[], max_concurrency}
//	POST /batch/secrets                 {secrets[{name,value}], max_concurrency}
//	GET  /batch/{id}                    polling (batch.read, all roles)
//	GET  /batch/{id}/events             SSE progress (batch.read; existing stream seam)
//	POST /batch/{id}/cancel             cancel a running batch (batch.write)
//
// Design (mirrors cmd/api/residency.go / orchestration.go):
//   - RBAC uses the NEW issue #123 grants (internal/auth/permissions_batch.go):
//     batch.read (OWNER/ADMIN/MEMBER/VIEWER — batch progress is an
//     observability surface) and batch.write (OWNER/ADMIN/MEMBER —
//     submissions create runs, mutate agents/secrets and consume model
//     tokens, the execution tier of runs.execute; VIEWER excluded).
//   - Tenant scope comes EXCLUSIVELY from the auth claims; a caller can never
//     name a foreign tenant (cross-org GET /batch/{id} is 404).
//   - Atomic validation: every whole-request invalidity (no items, ambiguous
//     selectors, empty patch, oversized batch, unknown workflow target) is
//     rejected 422/404 BEFORE any item runs.
//   - Partial failure: items run through the internal/batch pool with per-item
//     isolation; the response is 200 when everything succeeded and 207 with
//     the {batch_id, summary, items} body when any item failed/cancelled.
//   - Each runs item is a REAL run: runs.CreateRunCtx + the same "agent.run"
//     queue task the single-run path enqueues (the queue is the execution
//     trigger; workers consume it exactly as before).
//   - The execution seams are narrow interfaces adapted HERE around the
//     dual-mode services (orchestration.RunFactory precedent) — runs, agents,
//     workflows, deployments and secrets are import-only for this track.
//   - Secret values appear in exactly one wire place (the request body); they
//     are never echoed in responses, items, events or audit metadata.
//   - Progress rides the EXISTING SSE stream seam: the batch service emits
//     additive "batch" / "batch.updated" events keyed by the batch id, served
//     by GET /batch/{id}/events through streaming.Service.ServeSSE (the same
//     renderer as GET /runs/{id}/events).

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"agentos/internal/agents"
	"agentos/internal/apikeys"
	"agentos/internal/audit"
	"agentos/internal/auth"
	"agentos/internal/batch"
	"agentos/internal/deployments"
	"agentos/internal/queue"
	"agentos/internal/runs"
	"agentos/internal/secrets"
	"agentos/internal/streaming"
	"agentos/internal/workflows"
)

// --- narrow seams (internal/batch is domain-agnostic; the HTTP layer binds it) ---

// batchRunSeam is the narrow runs-service surface the batch endpoints use
// (structurally satisfied by *runs.Service; fakes in tests).
type batchRunSeam interface {
	// CreateRunCtx persists a QUEUED run row for one tenant.
	CreateRunCtx(ctx context.Context, orgID, agentID, input string) (*runs.Run, error)
	// CancelRun transitions a run to cancelled (idempotent).
	CancelRun(ctx context.Context, orgID, id string) (*runs.Run, error)
}

// batchAgentSeam is the narrow agents-service surface for bulk update
// (structurally satisfied by *agents.Service).
type batchAgentSeam interface {
	// GetAgentCtx resolves one agent strictly within the tenant.
	GetAgentCtx(ctx context.Context, orgID, id string) (*agents.Agent, error)
	// UpdateAgentCtx persists name/description/instructions/model/status
	// within the tenant.
	UpdateAgentCtx(ctx context.Context, orgID string, agent *agents.Agent) error
}

// batchWorkflowSeam is the narrow workflows-service surface for bulk trigger
// (structurally satisfied by *workflows.Service).
type batchWorkflowSeam interface {
	// GetWorkflow resolves one workflow strictly within the tenant (the
	// pre-flight 404 check).
	GetWorkflow(ctx context.Context, orgID, id string) (*workflows.Workflow, error)
	// ExecuteWorkflow expands the workflow DAG into agent runs for one input.
	ExecuteWorkflow(ctx context.Context, orgID, workflowID, input, actor string) (*workflows.ExecutionResult, error)
}

// batchVersionsSeam resolves deployable versions for bulk deploy
// (structurally satisfied by *agents.VersionsService).
type batchVersionsSeam interface {
	// ListVersionsCtx lists one agent's versions within the tenant.
	ListVersionsCtx(ctx context.Context, orgID, agentID string) ([]*agents.ConfigVersion, error)
}

// batchDeploymentSeam creates one deployment per item (structurally satisfied
// by *deployments.Service — the exact call the single deploy path makes).
type batchDeploymentSeam interface {
	// CreateDeploymentCtx validates environment + published target version
	// and inserts the deployment row within the tenant.
	CreateDeploymentCtx(ctx context.Context, orgID, agentID string, version int, environment, createdBy string) (*deployments.Deployment, error)
}

// batchSecretSeam creates one encrypted secret per item (structurally
// satisfied by *secrets.Service).
type batchSecretSeam interface {
	// Create seals the value and stores the secret within the tenant; the
	// returned metadata never carries the value.
	Create(ctx context.Context, orgID, name, value, createdBy string) (*secrets.Secret, error)
}

// batchSeams bundles the execution seams the batch surface dispatches to;
// built once by newBatchSeams (real services) and replaceable with fakes in
// tests.
type batchSeams struct {
	Runs        batchRunSeam
	Agents      batchAgentSeam
	Workflows   batchWorkflowSeam
	Versions    batchVersionsSeam
	Deployments batchDeploymentSeam
	Secrets     batchSecretSeam
	Queue       *queue.Queue
}

// Compile-time proof the dual-mode services satisfy the seams unchanged.
var (
	_ batchRunSeam        = (*runs.Service)(nil)
	_ batchAgentSeam      = (*agents.Service)(nil)
	_ batchWorkflowSeam   = (*workflows.Service)(nil)
	_ batchVersionsSeam   = (*agents.VersionsService)(nil)
	_ batchDeploymentSeam = (*deployments.Service)(nil)
	_ batchSecretSeam     = (*secrets.Service)(nil)
)

// newBatchSeams adapts the app's dual-mode services onto the batch seams
// (one wiring point; the queue is shared so batch items are the same
// "agent.run" tasks workers already consume).
func newBatchSeams(a *app) *batchSeams {
	return &batchSeams{
		Runs:        a.runsSvc,
		Agents:      a.agentsSvc,
		Workflows:   a.wfSvc,
		Versions:    a.versionsSvc,
		Deployments: a.deploymentsSvc,
		Secrets:     a.secretsSvc,
		Queue:       a.queueSvc,
	}
}

// batchDefaultEnvironment is used when a deploy request omits environment.
const batchDefaultEnvironment = "production"

// registerBatchRoutes mounts the issue #123 batch surface.
func registerBatchRoutes(apiMux *http.ServeMux, svc *batch.Service, seams *batchSeams, streamSvc *streaming.Service, authSvc *auth.Service, apiKeysSvc *apikeys.Service, auditSvc *audit.Service) {
	if apiMux == nil || svc == nil {
		return
	}
	// auth wrap pattern from cmd/api/main.go / residency.go:
	// RequireAuthOrAPIKey outer, RequirePermission inner.
	wrap := func(perm auth.Permission, h http.Handler) http.Handler {
		return auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(auth.RequirePermission(authSvc, perm)(h))
	}
	// Submissions: batch.write (OWNER/ADMIN/MEMBER).
	apiMux.Handle("POST /batch/runs", wrap(auth.PermissionBatchWrite, http.HandlerFunc(batchRunsHandler(svc, seams))))
	apiMux.Handle("POST /batch/workflows/{id}/trigger", wrap(auth.PermissionBatchWrite, http.HandlerFunc(batchWorkflowTriggerHandler(svc, seams))))
	apiMux.Handle("POST /batch/agents/update", wrap(auth.PermissionBatchWrite, http.HandlerFunc(batchAgentUpdateHandler(svc, seams))))
	apiMux.Handle("POST /batch/agents/deploy", wrap(auth.PermissionBatchWrite, http.HandlerFunc(batchAgentDeployHandler(svc, seams))))
	apiMux.Handle("POST /batch/runs/cancel", wrap(auth.PermissionBatchWrite, http.HandlerFunc(batchRunCancelHandler(svc, seams))))
	apiMux.Handle("POST /batch/secrets", wrap(auth.PermissionBatchWrite, http.HandlerFunc(batchSecretsHandler(svc, seams))))
	apiMux.Handle("POST /batch/{id}/cancel", wrap(auth.PermissionBatchWrite, http.HandlerFunc(batchCancelHandler(svc, auditSvc))))
	// Progress surfaces: batch.read (all roles). The SSE endpoint reuses the
	// run-event stream renderer on the batch id as the stream key.
	apiMux.Handle("GET /batch/{id}", wrap(auth.PermissionBatchRead, http.HandlerFunc(batchGetHandler(svc))))
	apiMux.Handle("GET /batch/{id}/events", wrap(auth.PermissionBatchRead, http.HandlerFunc(batchEventsHandler(svc, streamSvc))))
}

// --- shared request plumbing -------------------------------------------------

// batchClaims extracts the caller claims or writes the 401/404 shape
// (residencyClaims convention; the tenant comes from claims ONLY).
func batchClaims(w http.ResponseWriter, r *http.Request) (orgID, actor string, ok bool) {
	claims, err := auth.ExtractClaims(r.Context())
	if err != nil {
		writeResError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
		return "", "", false
	}
	if claims.OrganizationID == "" {
		writeResError(w, http.StatusNotFound, "NOT_FOUND", "organization not found")
		return "", "", false
	}
	return claims.OrganizationID, claims.UserID, true
}

// batchConcurrency sanitizes the request's max_concurrency (<=0 defers to the
// service default; the pool cap clamps the rest).
func batchConcurrency(n int) int {
	if n <= 0 {
		return 0
	}
	return n
}

// batchIDOf extracts the {id} path value or writes the 404 shape.
func batchIDOf(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeResError(w, http.StatusNotFound, "NOT_FOUND", "batch id is required")
		return "", false
	}
	return id, true
}

// submitBatch performs the atomic-validation error mapping (422), submits the
// units and blocks until the batch settles. The pool runs detached from the
// request, so a client disconnect never aborts the batch — the caller polls
// GET /batch/{id} or subscribes to GET /batch/{id}/events. ok=false when the
// response was already written.
func submitBatch(w http.ResponseWriter, r *http.Request, svc *batch.Service, orgID string, typ batch.BatchType, units []batch.Unit, maxConcurrency int, actor string) (*batch.Batch, bool) {
	b, err := svc.Submit(r.Context(), orgID, typ, units, batch.SubmitOptions{
		Actor:          actor,
		MaxConcurrency: batchConcurrency(maxConcurrency),
	})
	if err != nil {
		switch {
		case errors.Is(err, batch.ErrNoItems), errors.Is(err, batch.ErrTooManyItems), errors.Is(err, batch.ErrOrgRequired):
			// Atomic validation: nothing ran, nothing was stored.
			writeResError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", err.Error())
		default:
			writeResError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "batch submission failed")
		}
		return nil, false
	}
	final, err := svc.Wait(r.Context(), orgID, b.ID)
	if err != nil {
		// The request went away or timed out; the batch keeps running and the
		// client can re-attach via polling or the SSE stream.
		writeResError(w, http.StatusRequestTimeout, "BATCH_WAIT_INTERRUPTED", "batch still running; poll GET /batch/{id} or subscribe to GET /batch/{id}/events")
		return nil, false
	}
	return final, true
}

// renderBatchResponse emits the partial-failure contract body: 200 when every
// item succeeded, 207 when any item failed or was cancelled. Items render as
// {ref, status, error?, <result fields>} — Result metadata (run_id,
// deployment_id, ...) is flattened onto the item; secret values can never
// appear because no secret item ever carries its value in Result.
func renderBatchResponse(w http.ResponseWriter, b *batch.Batch, extras map[string]any) {
	body := map[string]any{
		"batch_id": b.ID,
		"status":   b.Status,
		"summary":  b.Summary(),
		"items":    batchItemsJSON(b.Items),
	}
	for k, v := range extras {
		body[k] = v
	}
	status := http.StatusOK
	sum := b.Summary()
	if sum.Failed > 0 || sum.Cancelled > 0 {
		status = http.StatusMultiStatus
	}
	writeResJSON(w, status, body)
}

// batchItemsJSON projects the item list onto the wire shape.
func batchItemsJSON(items []batch.Item) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		m := map[string]any{"ref": it.Ref, "status": string(it.Status)}
		if it.Error != "" {
			m["error"] = it.Error
		}
		for k, v := range it.Result {
			m[k] = v
		}
		out = append(out, m)
	}
	return out
}

// batchRecordJSON projects a full batch record for GET /batch/{id} and the
// cancel response.
func batchRecordJSON(b *batch.Batch) map[string]any {
	body := map[string]any{
		"batch_id":        b.ID,
		"organization_id": b.OrganizationID,
		"type":            string(b.Type),
		"status":          b.Status,
		"max_concurrency": b.MaxConcurrency,
		"summary":         b.Summary(),
		"total":           b.Total,
		"completed":       b.Completed,
		"items":           batchItemsJSON(b.Items),
		"created_at":      b.CreatedAt.UTC(),
		"updated_at":      b.UpdatedAt.UTC(),
	}
	if b.CompletedAt != nil {
		body["completed_at"] = b.CompletedAt.UTC()
	}
	return body
}

// collectResults gathers one result field from the succeeded items in
// submission order (e.g. the run_ids contract of POST /batch/runs).
func collectResults(b *batch.Batch, field string) []string {
	out := []string{}
	for _, it := range b.Items {
		if it.Status != batch.ItemSucceeded {
			continue
		}
		if v, ok := it.Result[field].(string); ok && v != "" {
			out = append(out, v)
		}
	}
	return out
}

// --- POST /batch/runs ---------------------------------------------------------

// batchRunsHandler implements POST /batch/runs: one agent over N inputs
// ({agent_id, inputs[]}) or N agents over a shared input ({agent_ids[],
// input}). Every item becomes a REAL run (runs.CreateRunCtx) plus the same
// "agent.run" queue task the single-run path enqueues, so workers consume
// batch runs exactly like single ones.
func batchRunsHandler(svc *batch.Service, seams *batchSeams) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, actor, ok := batchClaims(w, r)
		if !ok {
			return
		}
		var req struct {
			AgentID        string   `json:"agent_id"`
			AgentIDs       []string `json:"agent_ids"`
			Input          string   `json:"input"`
			Inputs         []string `json:"inputs"`
			MaxConcurrency int      `json:"max_concurrency"`
		}
		if !decodeResBody(w, r, &req) {
			return
		}
		// Atomic validation BEFORE any item runs.
		switch {
		case strings.TrimSpace(req.AgentID) != "" && len(req.AgentIDs) > 0:
			writeResError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "agent_id and agent_ids are mutually exclusive")
			return
		case len(req.Inputs) > 0 && len(req.AgentIDs) > 0:
			writeResError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "inputs requires a single agent_id")
			return
		case len(req.Inputs) > 0 && strings.TrimSpace(req.AgentID) == "":
			writeResError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "inputs requires agent_id")
			return
		case len(req.Inputs) == 0 && len(req.AgentIDs) == 0 && strings.TrimSpace(req.AgentID) == "":
			writeResError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "agent_id or agent_ids is required")
			return
		}

		createRun := func(agentID, input string) batch.Unit {
			return batch.Unit{Ref: agentID, Run: func(ctx context.Context) (map[string]any, error) {
				if strings.TrimSpace(agentID) == "" {
					return nil, errors.New("agent id is required")
				}
				runRow, err := seams.Runs.CreateRunCtx(ctx, orgID, agentID, input)
				if err != nil {
					return nil, err
				}
				// Same task contract as the single-run handler: the payload
				// carries the tenant, the agent, the input and the run id.
				payload := map[string]any{
					"organization_id": orgID,
					"agent_id":        agentID,
					"input":           input,
					"run_id":          runRow.ID,
				}
				q := seams.Queue
				if q == nil {
					q = queue.NewQueue() // legacy zero-infra fallback (create-run handler precedent)
				}
				task := q.Enqueue("agent.run", payload)
				if task == nil {
					return nil, errors.New("failed to enqueue run")
				}
				return map[string]any{"run_id": runRow.ID, "task_id": task.ID}, nil
			}}
		}

		var units []batch.Unit
		switch {
		case len(req.Inputs) > 0:
			// One agent over N inputs: the ref is the input index (stable
			// caller-facing reference; the created run id rides the item).
			for i, input := range req.Inputs {
				units = append(units, batch.Unit{
					Ref: strconv.Itoa(i),
					Run: createRun(strings.TrimSpace(req.AgentID), input).Run,
				})
			}
		case len(req.AgentIDs) > 0:
			for _, id := range req.AgentIDs {
				units = append(units, createRun(strings.TrimSpace(id), req.Input))
			}
		default:
			units = append(units, createRun(strings.TrimSpace(req.AgentID), req.Input))
		}

		final, ok := submitBatch(w, r, svc, orgID, batch.TypeRuns, units, req.MaxConcurrency, actor)
		if !ok {
			return
		}
		renderBatchResponse(w, final, map[string]any{"run_ids": collectResults(final, "run_id")})
	}
}

// --- POST /batch/workflows/{id}/trigger ---------------------------------------

// batchWorkflowTriggerHandler implements POST /batch/workflows/{id}/trigger:
// one workflow execution per input. The workflow target is resolved ONCE
// before any item runs (unknown workflow -> 404; empty inputs -> 422).
func batchWorkflowTriggerHandler(svc *batch.Service, seams *batchSeams) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, actor, ok := batchClaims(w, r)
		if !ok {
			return
		}
		wfID, ok := batchIDOf(w, r)
		if !ok {
			return
		}
		var req struct {
			Inputs         []string `json:"inputs"`
			MaxConcurrency int      `json:"max_concurrency"`
		}
		if !decodeResBody(w, r, &req) {
			return
		}
		// Atomic validation BEFORE any item runs.
		if len(req.Inputs) == 0 {
			writeResError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "inputs is required (one entry per workflow execution)")
			return
		}
		if _, err := seams.Workflows.GetWorkflow(r.Context(), orgID, wfID); err != nil {
			writeResError(w, http.StatusNotFound, "NOT_FOUND", "workflow not found")
			return
		}

		units := make([]batch.Unit, 0, len(req.Inputs))
		for i, input := range req.Inputs {
			units = append(units, batch.Unit{Ref: strconv.Itoa(i), Run: func(ctx context.Context) (map[string]any, error) {
				res, err := seams.Workflows.ExecuteWorkflow(ctx, orgID, wfID, input, actor)
				if err != nil {
					return nil, err
				}
				out := map[string]any{"workflow_run_id": res.WorkflowRunID}
				if len(res.RunIDs) > 0 {
					out["run_ids"] = res.RunIDs
				}
				return out, nil
			}})
		}

		final, ok := submitBatch(w, r, svc, orgID, batch.TypeWorkflows, units, req.MaxConcurrency, actor)
		if !ok {
			return
		}
		renderBatchResponse(w, final, map[string]any{
			"workflow_id":      wfID,
			"workflow_run_ids": collectResults(final, "workflow_run_id"),
		})
	}
}

// --- POST /batch/agents/update -------------------------------------------------

// batchAgentPatch is the validated-once bulk patch. Pointers distinguish
// "absent" from "set"; application mirrors the single-agent update semantics
// (unchanged fields are preserved from the fetched agent).
type batchAgentPatch struct {
	Name         *string `json:"name"`
	Description  *string `json:"description"`
	Instructions *string `json:"instructions"`
	Model        *string `json:"model"`
}

// empty reports whether the patch touches no field (atomic 422).
func (p batchAgentPatch) empty() bool {
	return p.Name == nil && p.Description == nil && p.Instructions == nil && p.Model == nil
}

// validate enforces the agents-service value rules ONCE for the whole request
// (name/instructions/model must be non-blank when set) so an invalid patch
// never reaches a single item.
func (p batchAgentPatch) validate() error {
	for _, f := range []struct {
		field string
		val   *string
	}{
		{"name", p.Name},
		{"instructions", p.Instructions},
		{"model", p.Model},
	} {
		if f.val != nil && strings.TrimSpace(*f.val) == "" {
			return errors.New("patch." + f.field + " must not be blank")
		}
	}
	return nil
}

// apply mutates the fetched agent with the set fields.
func (p batchAgentPatch) apply(agent *agents.Agent) {
	if p.Name != nil {
		agent.Name = strings.TrimSpace(*p.Name)
	}
	if p.Description != nil {
		agent.Description = *p.Description
	}
	if p.Instructions != nil {
		agent.Instructions = strings.TrimSpace(*p.Instructions)
	}
	if p.Model != nil {
		agent.Model = strings.TrimSpace(*p.Model)
	}
}

// batchAgentUpdateHandler implements POST /batch/agents/update: the patch is
// validated ONCE (422 before any item), then applied per agent with per-item
// results (unknown agent -> failed item, never a batch abort).
func batchAgentUpdateHandler(svc *batch.Service, seams *batchSeams) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, actor, ok := batchClaims(w, r)
		if !ok {
			return
		}
		var req struct {
			AgentIDs       []string        `json:"agent_ids"`
			Patch          batchAgentPatch `json:"patch"`
			MaxConcurrency int             `json:"max_concurrency"`
		}
		if !decodeResBody(w, r, &req) {
			return
		}
		// Atomic validation BEFORE any item runs.
		switch {
		case len(req.AgentIDs) == 0:
			writeResError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "agent_ids is required")
			return
		case req.Patch.empty():
			writeResError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "patch must set at least one of name, description, instructions, model")
			return
		}
		if err := req.Patch.validate(); err != nil {
			writeResError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", err.Error())
			return
		}

		units := make([]batch.Unit, 0, len(req.AgentIDs))
		for _, id := range req.AgentIDs {
			units = append(units, batch.Unit{Ref: id, Run: func(ctx context.Context) (map[string]any, error) {
				if strings.TrimSpace(id) == "" {
					return nil, errors.New("agent id is required")
				}
				agent, err := seams.Agents.GetAgentCtx(ctx, orgID, id)
				if err != nil {
					return nil, errors.New("agent not found")
				}
				req.Patch.apply(agent)
				if err := seams.Agents.UpdateAgentCtx(ctx, orgID, agent); err != nil {
					return nil, err
				}
				return map[string]any{"agent_id": agent.ID}, nil
			}})
		}

		final, ok := submitBatch(w, r, svc, orgID, batch.TypeAgentUpdate, units, req.MaxConcurrency, actor)
		if !ok {
			return
		}
		renderBatchResponse(w, final, map[string]any{"updated_agent_ids": collectResults(final, "agent_id")})
	}
}

// --- POST /batch/agents/deploy --------------------------------------------------

// batchAgentDeployHandler implements POST /batch/agents/deploy: one deployment
// per agent through the EXACT single-deploy call (deployments.CreateDeploymentCtx).
// An explicit version deploys every agent at that version; when omitted, each
// item resolves the agent's latest published version (an agent without one is
// a failed item, never a batch abort).
func batchAgentDeployHandler(svc *batch.Service, seams *batchSeams) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, actor, ok := batchClaims(w, r)
		if !ok {
			return
		}
		var req struct {
			AgentIDs       []string `json:"agent_ids"`
			Version        int      `json:"version"`
			Environment    string   `json:"environment"`
			MaxConcurrency int      `json:"max_concurrency"`
		}
		if !decodeResBody(w, r, &req) {
			return
		}
		// Atomic validation BEFORE any item runs: empty selector or an
		// environment outside the documented set never reaches an item.
		environment := strings.TrimSpace(req.Environment)
		if environment == "" {
			environment = batchDefaultEnvironment
		}
		switch environment {
		case "development", "staging", "production":
		default:
			writeResError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "environment must be one of development|staging|production")
			return
		}
		if len(req.AgentIDs) == 0 {
			writeResError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "agent_ids is required")
			return
		}
		if req.Version < 0 {
			writeResError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "version must be a positive integer")
			return
		}

		units := make([]batch.Unit, 0, len(req.AgentIDs))
		for _, id := range req.AgentIDs {
			units = append(units, batch.Unit{Ref: id, Run: func(ctx context.Context) (map[string]any, error) {
				if strings.TrimSpace(id) == "" {
					return nil, errors.New("agent id is required")
				}
				version := req.Version
				if version <= 0 {
					// Resolve the agent's latest published version; the
					// deployments service re-validates it (exists + published).
					versions, err := seams.Versions.ListVersionsCtx(ctx, orgID, id)
					if err != nil {
						return nil, err
					}
					best := 0
					for _, v := range versions {
						if v.Status == agents.VersionStatusPublished && v.Version > best {
							best = v.Version
						}
					}
					if best == 0 {
						return nil, errors.New("agent has no published version to deploy")
					}
					version = best
				}
				dep, err := seams.Deployments.CreateDeploymentCtx(ctx, orgID, id, version, environment, actor)
				if err != nil {
					return nil, err
				}
				return map[string]any{
					"deployment_id": dep.ID,
					"version":       dep.Version,
					"environment":   dep.Environment,
				}, nil
			}})
		}

		final, ok := submitBatch(w, r, svc, orgID, batch.TypeAgentDeploy, units, req.MaxConcurrency, actor)
		if !ok {
			return
		}
		renderBatchResponse(w, final, map[string]any{
			"environment":    environment,
			"deployment_ids": collectResults(final, "deployment_id"),
		})
	}
}

// --- POST /batch/runs/cancel -----------------------------------------------------

// batchRunCancelHandler implements POST /batch/runs/cancel: the single-run
// cancel semantics (runs.CancelRun — idempotent for already-cancelled runs,
// invalid-transition for terminal ones) applied per item.
func batchRunCancelHandler(svc *batch.Service, seams *batchSeams) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, actor, ok := batchClaims(w, r)
		if !ok {
			return
		}
		var req struct {
			RunIDs         []string `json:"run_ids"`
			MaxConcurrency int      `json:"max_concurrency"`
		}
		if !decodeResBody(w, r, &req) {
			return
		}
		// Atomic validation BEFORE any item runs.
		if len(req.RunIDs) == 0 {
			writeResError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "run_ids is required")
			return
		}

		units := make([]batch.Unit, 0, len(req.RunIDs))
		for _, id := range req.RunIDs {
			units = append(units, batch.Unit{Ref: id, Run: func(ctx context.Context) (map[string]any, error) {
				if strings.TrimSpace(id) == "" {
					return nil, errors.New("run id is required")
				}
				runRow, err := seams.Runs.CancelRun(ctx, orgID, id)
				if err != nil {
					return nil, err
				}
				return map[string]any{"run_id": runRow.ID, "status": string(runRow.Status)}, nil
			}})
		}

		final, ok := submitBatch(w, r, svc, orgID, batch.TypeRunCancel, units, req.MaxConcurrency, actor)
		if !ok {
			return
		}
		renderBatchResponse(w, final, map[string]any{"cancelled_run_ids": collectResults(final, "run_id")})
	}
}

// --- POST /batch/secrets -----------------------------------------------------------

// batchSecretsHandler implements POST /batch/secrets: one encrypted secret per
// item through the secrets service. Values exist ONLY in the request body —
// responses carry the name, the item ref and the envelope key version, never
// the value.
func batchSecretsHandler(svc *batch.Service, seams *batchSeams) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, actor, ok := batchClaims(w, r)
		if !ok {
			return
		}
		var req struct {
			Secrets []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"secrets"`
			MaxConcurrency int `json:"max_concurrency"`
		}
		if !decodeResBody(w, r, &req) {
			return
		}
		// Atomic validation BEFORE any item runs.
		if len(req.Secrets) == 0 {
			writeResError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "secrets is required")
			return
		}

		units := make([]batch.Unit, 0, len(req.Secrets))
		for _, s := range req.Secrets {
			name, value := s.Name, s.Value
			units = append(units, batch.Unit{Ref: strings.TrimSpace(name), Run: func(ctx context.Context) (map[string]any, error) {
				if strings.TrimSpace(name) == "" {
					return nil, errors.New("secret name is required")
				}
				if value == "" {
					return nil, errors.New("secret value is required")
				}
				created, err := seams.Secrets.Create(ctx, orgID, name, value, actor)
				if err != nil {
					return nil, err
				}
				// Metadata only: the value never comes back.
				return map[string]any{"name": created.Name, "key_version": created.KeyVersion}, nil
			}})
		}

		final, ok := submitBatch(w, r, svc, orgID, batch.TypeSecrets, units, req.MaxConcurrency, actor)
		if !ok {
			return
		}
		createdNames := []string{}
		for _, it := range final.Items {
			if it.Status == batch.ItemSucceeded {
				createdNames = append(createdNames, it.Ref)
			}
		}
		renderBatchResponse(w, final, map[string]any{"created": createdNames})
	}
}

// --- GET /batch/{id}, GET /batch/{id}/events, POST /batch/{id}/cancel --------------

// batchGetHandler implements GET /batch/{id}: the polling surface. A batch
// from another organization reads as not-found (404), never 403.
func batchGetHandler(svc *batch.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, _, ok := batchClaims(w, r)
		if !ok {
			return
		}
		id, ok := batchIDOf(w, r)
		if !ok {
			return
		}
		b, found := svc.Get(orgID, id)
		if !found {
			writeResError(w, http.StatusNotFound, "NOT_FOUND", "batch not found")
			return
		}
		writeResJSON(w, http.StatusOK, batchRecordJSON(b))
	}
}

// batchEventsHandler implements GET /batch/{id}/events: the additive
// batch.updated progress events on the EXISTING SSE seam (streaming.Service
// keyed by the batch id, the same renderer as GET /runs/{id}/events). The
// org-scope check runs before the stream is opened.
func batchEventsHandler(svc *batch.Service, streamSvc *streaming.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, _, ok := batchClaims(w, r)
		if !ok {
			return
		}
		id, ok := batchIDOf(w, r)
		if !ok {
			return
		}
		if _, found := svc.Get(orgID, id); !found {
			writeResError(w, http.StatusNotFound, "NOT_FOUND", "batch not found")
			return
		}
		if streamSvc == nil {
			writeResError(w, http.StatusServiceUnavailable, "STREAMING_UNAVAILABLE", "event streaming is not available")
			return
		}
		streamSvc.ServeSSE(w, r, id)
	}
}

// batchCancelHandler implements POST /batch/{id}/cancel: cancels a running
// batch (in-flight items' contexts fire; pending items never run). Idempotent
// on terminal batches. The cancellation is audit-logged best-effort.
func batchCancelHandler(svc *batch.Service, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, actor, ok := batchClaims(w, r)
		if !ok {
			return
		}
		id, ok := batchIDOf(w, r)
		if !ok {
			return
		}
		b, err := svc.Cancel(r.Context(), orgID, id, actor)
		if err != nil {
			if errors.Is(err, batch.ErrBatchNotFound) {
				writeResError(w, http.StatusNotFound, "NOT_FOUND", "batch not found")
				return
			}
			writeResError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "batch cancellation failed")
			return
		}
		if auditSvc != nil {
			_, _ = auditSvc.LogCtx(r.Context(), actor, "batch.cancelled", orgID, "batches/"+b.ID, map[string]any{
				"batch_id": b.ID,
				"status":   b.Status,
			})
		}
		writeResJSON(w, http.StatusOK, batchRecordJSON(b))
	}
}
