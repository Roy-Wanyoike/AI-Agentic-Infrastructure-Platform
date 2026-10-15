package runtime

// limiter_hooks.go — issue #124 additive hook file: granular rate limiting
// at the runtime tool and model seams, NEXT TO the existing governance
// policy seam (checkToolPolicy). The engine lives in internal/limiter
// (sliding-window counters, Redis + in-memory fallback); this file adds:
//
//   - GranularLimiter: the consumer-side seam interface (issue #124: "all
//     hooks behind a Limiter interface with a no-op default"). *limiter.
//     Enforcer satisfies it; noopLimiter is the zero-configuration default
//     so unwired modes (tests, offline dev) are unaffected.
//   - ToolRateLimitError / ModelRateLimitError: typed denials carrying the
//     machine-readable "rate_limited" code, recorded on the step.
//   - Runner hook methods (checkToolRateLimit / checkModelRateLimit /
//     reportModelTokens) called from the minimal seam lines in runtime.go:
//
//	* executeTool (provider loop): after the policy + agent-key checks,
//	  BEFORE the tool resolves/executes — a denial returns a
//	  ToolRateLimitError; the run CONTINUES (the observation fed back to
//	  the model states the denial) and the step records the code.
//	* the offline calculator path: same check order as the policy seam
//	  (skip semantics — the run completes through the offline fallback).
//	* the provider loop model call: BEFORE r.provider.Complete — a denial
//	  records the failed model step and fails the run with the code;
//	  AFTER a successful call the true token usage is reported into the
//	  model scope's TPM window (one-call-granularity token accounting).
//
// Denials REQUIRE the run scope (policies.WithRunScope — the worker stamps
// it from the task payload): a runner invoked without tenant context is
// never rate limited (eval runner, tests), matching the policy seam's
// behavior of evaluating only within a known organization.

import (
	"context"
	"fmt"

	"agentos/internal/limiter"
	"agentos/internal/models"
	"agentos/internal/policies"
)

// RateLimitCode is the machine-readable code for a rate-limit denial,
// shared by run-creation HTTP responses, step errors and audit vocabulary.
const RateLimitCode = limiter.ReasonRateLimited

// GranularLimiter is the hook seam (issue #124). The zero value is nil:
// a runner built without WithGranularLimiter/SetGranularLimiter never
// consults the limiter (no-op default).
type GranularLimiter interface {
	// AllowTool checks the tool scope for one invocation.
	AllowTool(ctx context.Context, orgID, tool string) limiter.Decision
	// AllowModel checks the model scope before one provider call.
	AllowModel(ctx context.Context, orgID, model string, cost limiter.Tokens) limiter.Decision
	// ReportModelTokens charges the true token usage of one completed call
	// into the model scope's TPM window (best-effort accounting).
	ReportModelTokens(ctx context.Context, orgID, model string, tokens int)
}

// ToolRateLimitError marks a tool invocation blocked BEFORE execution by the
// granular tool-scope rate limit (issue #124). The Error() string leads with
// the machine-readable code so the recorded step, the fed-back observation
// and greppable logs share one vocabulary.
type ToolRateLimitError struct {
	Tool     string
	Decision limiter.Decision
}

// Error implements error.
func (e *ToolRateLimitError) Error() string {
	reason := ""
	if e.Decision.Reason != "" {
		reason = ": " + e.Decision.Reason
	}
	return fmt.Sprintf("%s: tool %q rate limited%s", RateLimitCode, e.Tool, reason)
}

// ModelRateLimitError marks a provider call refused BEFORE issuance by the
// granular model-scope rate limit (issue #124). The run fails with this
// cause; the failed model step carries the code.
type ModelRateLimitError struct {
	Model    string
	Decision limiter.Decision
}

// Error implements error.
func (e *ModelRateLimitError) Error() string {
	reason := ""
	if e.Decision.Reason != "" {
		reason = ": " + e.Decision.Reason
	}
	return fmt.Sprintf("%s: model %q rate limited%s", RateLimitCode, e.Model, reason)
}

// WithGranularLimiter attaches the granular rate-limit seam (issue #124).
// Passing nil keeps the legacy always-allow behavior (nil-safe DI).
func WithGranularLimiter(g GranularLimiter) Option {
	return func(r *Runner) { r.limiter = g }
}

// SetGranularLimiter attaches (or clears, via nil) the granular rate-limit
// seam at runtime (worker wiring precedent: SetPolicyEnforcer).
func (r *Runner) SetGranularLimiter(g GranularLimiter) {
	if r == nil {
		return
	}
	r.limiter = g
}

// runOrg resolves the tenant the run executes for (stamped by the worker
// via policies.WithRunScope). ok=false when there is no tenant context:
// the limiter is never consulted without one.
func runOrg(ctx context.Context) (orgID string, ok bool) {
	scope := policies.RunScopeFromContext(ctx)
	if scope.OrganizationID == "" {
		return "", false
	}
	return scope.OrganizationID, true
}

// checkToolRateLimit consults the optional granular seam for one tool
// invocation (nil limiter, unknown tenant -> allowed). Returns a non-nil
// Decision only when the invocation must NOT execute.
func (r *Runner) checkToolRateLimit(ctx context.Context, name string) *limiter.Decision {
	if r.limiter == nil {
		return nil
	}
	orgID, ok := runOrg(ctx)
	if !ok {
		return nil
	}
	if d := r.limiter.AllowTool(ctx, orgID, name); !d.Allowed {
		return &d
	}
	return nil
}

// checkModelRateLimit consults the optional granular seam before one
// provider call (model RPM; TPM is charged post-call by reportModelTokens).
// Returns a non-nil Decision only when the call must NOT be issued.
func (r *Runner) checkModelRateLimit(ctx context.Context, model string) *limiter.Decision {
	if r.limiter == nil {
		return nil
	}
	orgID, ok := runOrg(ctx)
	if !ok {
		return nil
	}
	if d := r.limiter.AllowModel(ctx, orgID, model, limiter.Tokens{Requests: 1}); !d.Allowed {
		return &d
	}
	return nil
}

// reportModelTokens charges the true token usage of one completed provider
// call into the model scope's TPM window (best-effort: observability and
// accounting must never break execution).
func (r *Runner) reportModelTokens(ctx context.Context, model string, usage models.Usage) {
	if r.limiter == nil || usage.TotalTokens <= 0 {
		return
	}
	orgID, ok := runOrg(ctx)
	if !ok {
		return
	}
	r.limiter.ReportModelTokens(ctx, orgID, model, usage.TotalTokens)
}
