package main

// mcp.go — issue #82 worker-side wiring: per-org runner registries so
// externally-registered MCP tools are executable by agent runs.
//
// Why per-org runners: MCP tool adapters are ORG-SCOPED (a server registered
// by org A must never resolve for org B) while the runtime Runner holds ONE
// tools.Registry for the process. Tasks execute concurrently in this worker,
// so mutating a single shared registry per task would race and could leak a
// cross-tenant tool under a colliding name. Instead the worker keeps one
// runner per organization, each with its own registry: the platform built-ins
// (calculator, http_request — sandbox-wrapped when AGENTOS_TOOL_SANDBOX=exec)
// plus that org's cached MCP tools (registry.BuildTools only exposes servers
// with status ok). Runners are cheap structs; the cache is bounded by the
// number of active orgs and shares the provider, step recorder, metrics and
// the stateless policy enforcer.
//
// Zero-infrastructure mode: the worker's in-memory MCP registry is always
// empty (registrations live in the API process's memory), so every org
// runner degrades to exactly the pre-#82 base registry — no behavior change.
// MCP tool execution therefore requires Postgres mode, exactly like durable
// connectors state.

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"

	"agentos/internal/agents"
	"agentos/internal/audit"
	"agentos/internal/events"
	"agentos/internal/mcp"
	"agentos/internal/models"
	"agentos/internal/observability"
	"agentos/internal/policies"
	"agentos/internal/runtime"
	"agentos/internal/secrets"
	"agentos/internal/tools"
)

// orgRunnerCache lazily builds one *runtime.Runner per organization.
type orgRunnerCache struct {
	mu      sync.Mutex
	runners map[string]*runtime.Runner

	agentsvc      *agents.Service
	provider      models.Provider
	recorder      runtime.StepRecorder
	metrics       *observability.Metrics
	enforcer      *policies.Enforcer
	limiter       runtime.GranularLimiter // issue #124: granular rate-limit seam (nil-safe)
	agentKeyAudit *audit.Service          // wave-8: agent-key tool-denial audit sink (nil-safe)
	runtimeCfg    runtime.RuntimeConfig   // issue #107: runner-wide conversation-history/budget defaults (nil-safe)
	eventPub      events.Publisher        // issue #107: run lifecycle event seam (nil-safe)
	sandbox       tools.SandboxExecutor
	mcpReg        *mcp.Registry
	stopped       bool // issue #107: set by stopAll; runners built afterwards refuse runs
	logr          *slog.Logger
}

// newOrgRunnerCache wires the cache. sandbox may be nil (in-process tool
// execution); mcpReg may be nil (issue #82 surface absent); the cache stays
// fully functional either way.
func newOrgRunnerCache(
	agentsvc *agents.Service,
	provider models.Provider,
	recorder runtime.StepRecorder,
	metrics *observability.Metrics,
	enforcer *policies.Enforcer,
	sandbox tools.SandboxExecutor,
	mcpReg *mcp.Registry,
	logr *slog.Logger,
) *orgRunnerCache {
	return &orgRunnerCache{
		runners:  make(map[string]*runtime.Runner),
		agentsvc: agentsvc,
		provider: provider,
		recorder: recorder,
		metrics:  metrics,
		enforcer: enforcer,
		sandbox:  sandbox,
		mcpReg:   mcpReg,
		logr:     logr,
	}
}

// baseRegistry builds a fresh registry with the platform built-ins,
// sandbox-wrapped when the sandbox runner is configured (mirrors the
// startup wiring in main.go).
func (c *orgRunnerCache) baseRegistry() *tools.Registry {
	reg := tools.NewRegistry()
	reg.Register(tools.NewCalculatorTool())
	reg.Register(tools.NewHTTPRequestTool())
	if c.sandbox != nil {
		if err := tools.WithSandboxRunner(reg, c.sandbox, tools.HTTPToolName); err != nil {
			c.logr.Warn("sandbox enablement failed for org registry; tools stay in-process", "error", err.Error())
		}
	}
	return reg
}

// forOrg returns the runner serving orgID: cached when present, otherwise
// built from the base registry plus the org's MCP tool adapters.
func (c *orgRunnerCache) forOrg(ctx context.Context, orgID string) *runtime.Runner {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r, ok := c.runners[orgID]; ok {
		return r
	}

	reg := c.baseRegistry()
	if c.mcpReg != nil && orgID != "" {
		// RegisterToolsInto is a no-op for a nil registry and registers only
		// this org's healthy servers' cached tools; failures degrade to the
		// base registry (visible tool absence, never a cross-org leak).
		if err := c.mcpReg.RegisterToolsInto(ctx, orgID, reg); err != nil {
			c.logr.Warn("mcp tool registration failed; org runs on built-in tools",
				"org", orgID, "error", err.Error())
		}
	}

	runner := runtime.NewRunnerWithOptions(c.agentsvc, reg,
		runtime.WithProvider(c.provider),
		runtime.WithStepRecorder(c.recorder),
		runtime.WithMetrics(c.metrics),
		runtime.WithRuntimeConfig(c.runtimeCfg), // issue #107: zero value = safe defaults
		runtime.WithEventPublisher(c.eventPub),  // issue #107: nil = event-free legacy behavior
	)
	runner.SetPolicyEnforcer(c.enforcer)
	runner.SetAgentKeyAudit(c.agentKeyAudit) // wave-8: best-effort agentkey.tool_denied rows
	runner.SetGranularLimiter(c.limiter)     // issue #124: granular rate-limit seam (nil = disabled)
	if c.stopped {
		// issue #107: the cache is draining (or drained) — a runner built
		// after stopAll must refuse runs exactly like the stopped ones.
		runner.Stop()
	}
	c.runners[orgID] = runner
	return runner
}

// workerMCPRegistry builds the worker-side MCP registry: Postgres-backed
// (shared durable registration state with the API, secret refs resolved
// through the same durable secrets store) when db is available; in-memory
// and always empty in zero-infrastructure mode (registrations live in the
// API process's memory). The Postgres secrets service reads the master key
// from the environment exactly like the API does (secrets.NewPostgresService).
func workerMCPRegistry(db *sql.DB, auditSvc *audit.Service, logr *slog.Logger) *mcp.Registry {
	if db == nil {
		return mcp.NewRegistry()
	}
	secretsSvc, serr := secrets.NewPostgresService(db)
	if serr != nil {
		logr.Warn("mcp registry: secrets unavailable; servers with secret_refs will fail their calls", "error", serr.Error())
	}
	reg, err := mcp.NewRegistryWithStore(mcp.NewPostgresStore(db), secretsSvc)
	if err != nil {
		logr.Warn("mcp registry init failed; MCP tools unavailable in worker", "error", err.Error())
		return mcp.NewRegistry()
	}
	reg.SetAuditor(auditSvc)
	return reg
}

// setAgentKeyAudit wires the agent-key tool-denial audit sink for every
// runner built by this cache (wave-8). Called once at startup before task
// consumption; guarded by the cache mutex for safety.
func (c *orgRunnerCache) setAgentKeyAudit(svc *audit.Service) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.agentKeyAudit = svc
}

// setGranularLimiter wires the granular rate-limit seam (issue #124) for
// every runner built by this cache. Called once at startup before task
// consumption; guarded by the cache mutex (the setAgentKeyAudit pattern).
func (c *orgRunnerCache) setGranularLimiter(g runtime.GranularLimiter) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.limiter = g
}

// setRuntimeConfig wires the runner-wide runtime configuration (issue #107:
// conversation history + token budget defaults from the AGENTOS_RUNTIME_*
// environment variables) for every runner built by this cache. Call once at
// startup before task consumption; guarded by the cache mutex.
func (c *orgRunnerCache) setRuntimeConfig(cfg runtime.RuntimeConfig) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.runtimeCfg = cfg
}

// setEventPublisher wires the run lifecycle event seam (issue #107) for every
// runner built by this cache. Call once at startup before task consumption;
// guarded by the cache mutex.
func (c *orgRunnerCache) setEventPublisher(pub events.Publisher) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.eventPub = pub
}

// stopAll marks the cache as shutting down (issue #107): every cached runner
// refuses new runs from now on (runtime.ErrShuttingDown) and runners built
// afterwards are born stopped. Non-blocking; see shutdownAll for the
// draining variant. Idempotent.
func (c *orgRunnerCache) stopAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopped = true
	for _, r := range c.runners {
		r.Stop()
	}
}

// shutdownAll drains the cache (issue #107): stopAll semantics plus a bounded
// wait (ctx, the worker's grace period) for every cached runner's in-flight
// runs. Returns the first timeout error so the caller logs the runs it could
// not wait for.
func (c *orgRunnerCache) shutdownAll(ctx context.Context) error {
	c.stopAll()
	c.mu.Lock()
	runners := make([]*runtime.Runner, 0, len(c.runners))
	for _, r := range c.runners {
		runners = append(runners, r)
	}
	c.mu.Unlock()
	for _, r := range runners {
		if err := r.Shutdown(ctx); err != nil {
			return err
		}
	}
	return nil
}
