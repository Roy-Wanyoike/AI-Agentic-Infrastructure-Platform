package main

// Worker-side governance enforcement wiring (issue #75). The runtime tool
// seam evaluates every tool invocation against the organization's policies
// BEFORE execution (denied tools are never invoked and the denial lands on
// the recorded step); this file picks the policy decision source for the
// worker process and stamps the per-run scope (tenant + environment) onto
// the execution context.

import (
	"context"
	"database/sql"
	"log/slog"
	"os"

	"agentos/internal/policies"
	"agentos/internal/runtime"
)

// newWorkerPolicyEvaluator picks the policy decision source for the worker
// process (issue #75):
//
//   - pull mode (AGENTOS_API_PULL=true with AGENTOS_API set): the API's
//     POST /v1/policies/evaluate endpoint, so tool-level enforcement holds
//     in the two-process deployment where the worker holds no policy state
//     of its own;
//   - Postgres mode (DSN configured): the durable policy records, the same
//     source of truth the API's enforcement seam reads;
//   - zero-infrastructure mode: the process-local in-memory service. It
//     starts empty, so decisions default to allow until policies are created
//     in the same process — no-policy deployments keep legacy behavior.
//
// The caller wraps the evaluator in an Enforcer (wirePolicyEnforcer), which
// honors AGENTOS_POLICY_ENFORCEMENT (default ON). The evaluator never
// returns nil: a disabled flag is the Enforcer's concern.
func newWorkerPolicyEvaluator(logr *slog.Logger, db *sql.DB) policies.Evaluator {
	if os.Getenv("AGENTOS_API_PULL") == "true" {
		if apiBase := os.Getenv("AGENTOS_API"); apiBase != "" {
			logr.Info("worker policy enforcement evaluates against the API",
				"flag", policies.EnforcementEnvVar, "endpoint", "/v1/policies/evaluate")
			return policies.NewHTTPEvaluator(apiBase, os.Getenv("AGENTOS_API_KEY"))
		}
		logr.Warn("worker policy enforcement: AGENTOS_API_PULL is set but AGENTOS_API is empty; using local policy state")
	}
	if db != nil {
		logr.Info("worker policy enforcement reads durable policy records",
			"flag", policies.EnforcementEnvVar)
		return policies.NewServiceWithStore(policies.NewPostgresStore(db))
	}
	logr.Info("worker policy enforcement uses in-process policy state",
		"flag", policies.EnforcementEnvVar)
	return policies.NewService()
}

// wirePolicyEnforcer attaches the governance seam to the runner once at
// startup, before any task is consumed. The Enforcer evaluates
// AGENTOS_POLICY_ENFORCEMENT at construction (default ON).
func wirePolicyEnforcer(runner *runtime.Runner, logr *slog.Logger, db *sql.DB) {
	runner.SetPolicyEnforcer(policies.NewEnforcerWithEvaluator(newWorkerPolicyEvaluator(logr, db)))
}

// runScopeContext stamps the run scope (tenant + environment) the runtime
// tool seam reads for policy checks. The environment rides on the task
// payload (create-run handler, issue #75); a blank value keeps the platform
// default (production), applied by RunScopeFromContext at evaluation time.
func runScopeContext(ctx context.Context, orgID, environment string) context.Context {
	return policies.WithRunScope(ctx, policies.RunScope{
		OrganizationID: orgID,
		Environment:    environment,
	})
}
