package limiter

// enforcer.go — the enforcement facade (issue #124 requirement 3): one
// object the three seams (run creation, tool invocation, model calls) call.
// It resolves the configured limits per scope and checks them through the
// *Limiter engine, then aggregates:
//
//      AllowRun   -> org scope + agent scope (both checked; both reported)
//      AllowTool  -> tool scope
//      AllowModel -> model scope (RPM pre-call + TPM via post-call Report)
//
// Enforcement semantics (documented in the issue):
//   - on denial NOTHING is consumed: a check is atomic across all of the
//     key's windows (single Lua script / single in-memory phase), so denied
//     traffic never eats budget from allowed traffic;
//   - every check reports Decision.Remaining for ALL active windows of the
//     key, whichever way the decision goes;
//   - metrics: every denial increments
//     agentos_rate_limited_total{scope,org} and every check refreshes the
//     agentos_rate_limit_remaining{scope,org,window} last-value gauges when
//     a Metrics handle is wired (WithMetrics). The same handle the API and
//     worker processes already expose through GET /v1/metrics.
//
// *Enforcer satisfies the consumer-side hook interfaces
// (internal/runtime.GranularLimiter and cmd/api.limiterGate); the no-op
// default for unwired modes is Noop below.

import (
	"context"
	"fmt"

	"agentos/internal/observability"
)

// Metrics family names (rendered through the existing observability seam —
// see internal/observability/prometheus.go, which parses the label suffix).
const (
	MetricRateLimitedTotal = "agentos_rate_limited_total"
	MetricRemainingGauge   = "agentos_rate_limit_remaining"
)

// MetricKey renders the labelled counter key for one denial
// (agentos_rate_limited_total{scope="agent",org="org-demo"}).
func MetricKey(scope Scope, orgID string) string {
	return fmt.Sprintf("%s{scope=%q,org=%q}", MetricRateLimitedTotal, string(scope), orgID)
}

// RemainingGaugeKey renders the labelled last-value gauge key
// (agentos_rate_limit_remaining{scope="agent",org="org-demo",window="rpm"}).
func RemainingGaugeKey(scope Scope, orgID, window string) string {
	return fmt.Sprintf("%s{scope=%q,org=%q,window=%q}", MetricRemainingGauge, string(scope), orgID, window)
}

// Enforcer is the granular enforcement facade over the Limiter engine and
// the limit Resolver. Safe for concurrent use.
type Enforcer struct {
	limiter  *Limiter
	resolver *Resolver
	metrics  *observability.Metrics
}

// EnforcerOption configures an Enforcer.
type EnforcerOption func(*Enforcer)

// WithMetrics wires the denial counter + remaining gauge into the process
// metrics registry (nil-safe: passing nil keeps the metric-free behavior).
func WithMetrics(m *observability.Metrics) EnforcerOption {
	return func(e *Enforcer) { e.metrics = m }
}

// NewEnforcer builds the facade. Both arguments are required for real
// enforcement: a nil resolver makes every key unlimited.
func NewEnforcer(l *Limiter, r *Resolver, opts ...EnforcerOption) *Enforcer {
	e := &Enforcer{limiter: l, resolver: r}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// Resolver exposes the resolver for the management surface (cache
// invalidation after out-of-band store edits) and tests.
func (e *Enforcer) Resolver() *Resolver { return e.resolver }

// AllowRun checks the ORG and AGENT scopes for one run creation (the
// hierarchy's top two tiers) in ONE atomic batch (Limiter.AllowBatch): a
// denied run consumes NOTHING from any scope, the aggregated Decision
// carries the minimum RetryAfter over every denied window and the Remaining
// of every checked window of both scopes, and the Scope field names the
// first denying scope in hierarchy order (org before agent).
func (e *Enforcer) AllowRun(ctx context.Context, orgID, agentID string) Decision {
	d, err := e.limiter.AllowBatch(ctx, []LimitKey{
		{OrgID: orgID, Scope: ScopeOrg, Subject: ""},
		{OrgID: orgID, Scope: ScopeAgent, Subject: agentID},
	}, Tokens{Requests: 1})
	if err != nil {
		// The engine already applied the fail mode to the Decision (open
		// allows, closed denies); the error carries no extra hook action.
		return d
	}
	e.observe(orgID, d)
	if !d.Allowed {
		e.incrDenied(d.Scope, orgID)
	}
	return d
}

// AllowTool checks the TOOL scope for one tool invocation. A denial does NOT
// abort the run: the runtime seam records the rate_limited denial on the
// step and continues (issue #124 requirement 3).
func (e *Enforcer) AllowTool(ctx context.Context, orgID, tool string) Decision {
	key := LimitKey{OrgID: orgID, Scope: ScopeTool, Subject: tool}
	d, err := e.limiter.Allow(ctx, key, Tokens{Requests: 1})
	if err != nil {
		return e.unavailable()
	}
	e.observeRemaining(key, d)
	if !d.Allowed {
		e.incrDenied(d.Scope, orgID)
	}
	return d
}

// AllowModel checks the MODEL scope before a provider call. cost.Requests
// counts the call against the model RPM window; cost.Tokens (when the
// caller can estimate) pre-charges the TPM window — the runtime seam passes
// zero tokens and reports the true usage after the call via
// ReportModelTokens (the documented one-call-granularity TPM semantics).
func (e *Enforcer) AllowModel(ctx context.Context, orgID, model string, cost Tokens) Decision {
	key := LimitKey{OrgID: orgID, Scope: ScopeModel, Subject: model}
	d, err := e.limiter.Allow(ctx, key, cost)
	if err != nil {
		return e.unavailable()
	}
	e.observeRemaining(key, d)
	if !d.Allowed {
		e.incrDenied(d.Scope, orgID)
	}
	return d
}

// ReportModelTokens charges the true token usage of one completed provider
// call into the model scope's TPM window (best-effort: accounting failures
// are logged inside the engine, never break the run).
func (e *Enforcer) ReportModelTokens(ctx context.Context, orgID, model string, tokens int) {
	if tokens <= 0 {
		return
	}
	_ = e.limiter.Report(ctx, LimitKey{OrgID: orgID, Scope: ScopeModel, Subject: model}, Tokens{Tokens: tokens})
}

// ListConfigs returns the org's stored rows (management surface).
func (e *Enforcer) ListConfigs(ctx context.Context, orgID string) ([]Config, error) {
	return e.resolver.store.List(ctx, orgID)
}

// UpsertConfig stores a row and invalidates the org's cached resolutions so
// enforcement sees the change immediately.
func (e *Enforcer) UpsertConfig(ctx context.Context, orgID string, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := e.resolver.store.Upsert(ctx, orgID, cfg); err != nil {
		return err
	}
	e.resolver.Invalidate(orgID)
	return nil
}

// DeleteConfig removes a row and invalidates the org's cached resolutions.
func (e *Enforcer) DeleteConfig(ctx context.Context, orgID string, scope Scope, subject string) error {
	if err := e.resolver.store.Delete(ctx, orgID, scope, subject); err != nil {
		return err
	}
	e.resolver.Invalidate(orgID)
	return nil
}

// unavailable is the engine-adjacent verdict for a resolution failure
// reaching the facade (the engine normally applies the fail mode itself;
// this keeps the facade total).
func (e *Enforcer) unavailable() Decision {
	return Decision{
		Allowed:    false,
		Reason:     ReasonLimiterUnavailable + ": rate limit resolution failed (fail closed)",
		RetryAfter: 0,
		Remaining:  map[string]int{},
	}
}

func (e *Enforcer) incrDenied(scope Scope, orgID string) {
	if e.metrics == nil {
		return
	}
	e.metrics.Inc(MetricKey(scope, orgID))
}

// observe refreshes the agentos_rate_limit_remaining last-value gauges for
// every window in a batch Decision. Remaining keys are "<scope>:<window>"
// and a batch spans scopes (org + agent), so the scope is parsed from the
// key rather than taken from one key struct.
func (e *Enforcer) observe(orgID string, d Decision) {
	if e.metrics == nil {
		return
	}
	for suffix, remaining := range d.Remaining {
		scope, window := suffix, ""
		for i := 0; i < len(suffix); i++ {
			if suffix[i] == ':' {
				scope, window = suffix[:i], suffix[i+1:]
				break
			}
		}
		e.metrics.Observe(RemainingGaugeKey(Scope(scope), orgID, window), float64(remaining))
	}
}

// observeRemaining refreshes the remaining gauges for a single-key Decision
// (AllowTool / AllowModel), where the scope is the key's scope.
func (e *Enforcer) observeRemaining(key LimitKey, d Decision) {
	if e.metrics == nil {
		return
	}
	for suffix, remaining := range d.Remaining {
		window := suffix
		// suffix is "<scope>:<window>"; strip the scope prefix.
		for i := 0; i < len(suffix); i++ {
			if suffix[i] == ':' {
				window = suffix[i+1:]
				break
			}
		}
		e.metrics.Observe(RemainingGaugeKey(key.Scope, key.OrgID, window), float64(remaining))
	}
}

// noopGate is the no-op enforcement seam: every check allows, nothing is
// counted or consumed. It is the default for unwired modes (tests,
// zero-config development) per the issue's "no-op default" requirement.
type noopGate struct{}

func (noopGate) AllowRun(context.Context, string, string) Decision {
	return Decision{Allowed: true, Reason: "limiter not wired"}
}

func (noopGate) AllowTool(context.Context, string, string) Decision {
	return Decision{Allowed: true, Reason: "limiter not wired"}
}

func (noopGate) AllowModel(context.Context, string, string, Tokens) Decision {
	return Decision{Allowed: true, Reason: "limiter not wired"}
}

func (noopGate) ReportModelTokens(context.Context, string, string, int) {}

// Gate is the hook-facing seam implemented by *Enforcer (and Noop). Hooks
// depend on this interface, never on the concrete type, so unwired modes
// and tests can substitute Noop.
type Gate interface {
	AllowRun(ctx context.Context, orgID, agentID string) Decision
	AllowTool(ctx context.Context, orgID, tool string) Decision
	AllowModel(ctx context.Context, orgID, model string, cost Tokens) Decision
	ReportModelTokens(ctx context.Context, orgID, model string, tokens int)
}

// Noop is the no-op Gate (issue #124: "a no-op default so unwired modes
// (tests, zero-config dev) are unaffected").
var Noop Gate = noopGate{}

// compile-time proof that *Enforcer implements the seam.
var _ Gate = (*Enforcer)(nil)
