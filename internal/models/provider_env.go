package models

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// Environment variables configuring the shared model provider. The worker
// (run execution) and the API process (evaluation runner) both build their
// provider through ProviderFromEnv, so these names are the single wiring
// contract for the whole platform.
const (
	// ProviderAPIKeyEnvVar enables real model calls when set to a non-blank
	// value. Without it every provider consumer stays in the deterministic
	// offline mode (zero-infrastructure development keeps working).
	ProviderAPIKeyEnvVar = "OPENAI_API_KEY"
	// ProviderBaseURLEnvVar points at any OpenAI-compatible endpoint
	// (OpenRouter, Groq, Together, local Ollama/vLLM, ...). Blank defaults
	// to DefaultOpenAIBaseURL.
	ProviderBaseURLEnvVar = "OPENAI_BASE_URL"
	// ProviderModelEnvVar is the default model id used when a request does
	// not carry one (the runtime always passes the agent's configured model,
	// so this is a fallback for callers without a model). May stay unset.
	ProviderModelEnvVar = "AGENTOS_WORKER_MODEL"
	// ProviderFallbackAPIKeyEnvVar enables failover chaining when set: the
	// fallback provider is tried after transient primary failures (rate
	// limits, 5xx, network timeouts — see IsTransient).
	ProviderFallbackAPIKeyEnvVar = "AGENTOS_FALLBACK_API_KEY"
	// ProviderFallbackBaseURLEnvVar is the fallback endpoint root. Blank
	// defaults to DefaultOpenAIBaseURL. Only read when
	// ProviderFallbackAPIKeyEnvVar is set.
	ProviderFallbackBaseURLEnvVar = "AGENTOS_FALLBACK_BASE_URL"
	// RoutingPolicyEnvVar enables the advanced model router (issue #112) with
	// the named policy (cost_optimize, latency_optimize, task_complexity,
	// round_robin, weighted) as the organization-wide default. When set, the
	// env-built provider is wrapped in a PolicyProvider over
	// RoutingCandidatesEnvVar model ids (served by the same OpenAI-compatible
	// endpoint as the primary); routing SUPERSEDES failover chaining — the
	// router fails over across its own candidates. Per-agent
	// runtime_config `routing` objects override the policy per request via
	// CompletionRequest.Routing.
	RoutingPolicyEnvVar = "AGENTOS_ROUTING_POLICY"
	// RoutingCandidatesEnvVar is the comma-separated list of model ids the
	// router rotates across (e.g. "gpt-4o-mini,gpt-4o"). Every id must be
	// known to the capability registry (built-ins or AGENTOS_MODELS_JSON
	// overrides) — unknown ids are skipped with a startup warning. Blank
	// defaults to the single AGENTOS_WORKER_MODEL id (only read when
	// RoutingPolicyEnvVar is set).
	RoutingCandidatesEnvVar = "AGENTOS_ROUTING_CANDIDATES"
)

// providerHTTPTimeout bounds a single HTTP attempt for env-configured
// providers (the client timeout the worker used before this constructor was
// extracted from cmd/worker/main.go).
const providerHTTPTimeout = 120 * time.Second

// ProviderFromEnv builds the env-driven model provider shared by the worker
// and the API's evaluation runner. It encapsulates the configuration the
// worker previously inlined in its main():
//
//   - OPENAI_API_KEY unset/blank  -> (nil, false): the caller keeps its
//     deterministic offline mode; a warning is logged.
//   - OPENAI_API_KEY set          -> an OpenAI-compatible provider against
//     OPENAI_BASE_URL (default https://api.openai.com/v1) with
//     AGENTOS_WORKER_MODEL as the default model id; "model provider
//     configured" is logged with the resolved base_url and model.
//   - AGENTOS_FALLBACK_API_KEY set -> the primary is wrapped in a
//     FailoverProvider whose fallback targets AGENTOS_FALLBACK_BASE_URL
//     (same default); only transient primary failures fail over.
//
// The returned bool reports whether real-model mode is active; callers use it
// to log which mode their process runs in. logr may be nil (slog.Default is
// used). The constructor never fails: an unreachable endpoint is only
// discovered at first request.
func ProviderFromEnv(logr *slog.Logger) (Provider, bool) {
	if logr == nil {
		logr = slog.Default()
	}

	apiKey := strings.TrimSpace(os.Getenv(ProviderAPIKeyEnvVar))
	if apiKey == "" {
		logr.Warn("no OPENAI_API_KEY set; running in offline deterministic mode")
		return nil, false
	}

	baseURL := strings.TrimSpace(os.Getenv(ProviderBaseURLEnvVar))
	if baseURL == "" {
		baseURL = DefaultOpenAIBaseURL
	}
	model := strings.TrimSpace(os.Getenv(ProviderModelEnvVar))

	client := &http.Client{Timeout: providerHTTPTimeout}
	primary := NewOpenAIProvider("openai-compatible", apiKey, baseURL, model, client)
	var provider Provider = primary

	// Failover chaining (identical to the worker's original inline block):
	// a fallback key silently upgrades the provider to primary->fallback;
	// without a fallback key the fallback base URL is never read.
	if fbKey := strings.TrimSpace(os.Getenv(ProviderFallbackAPIKeyEnvVar)); fbKey != "" {
		fbBase := strings.TrimSpace(os.Getenv(ProviderFallbackBaseURLEnvVar))
		if fbBase == "" {
			fbBase = DefaultOpenAIBaseURL
		}
		fallback := NewOpenAIProvider("fallback", fbKey, fbBase, model, client)
		if chained, cerr := NewFailoverProvider(primary, fallback); cerr == nil {
			provider = chained
			logr.Info("model provider failover enabled", "fallback_base_url", fbBase)
		}
		// cerr is impossible here (primary is non-nil); on the defensive path
		// the primary stays effective unchanged.
	}

	// Advanced model routing (issue #112): AGENTOS_ROUTING_POLICY wraps the
	// env-built chain in a PolicyProvider. Candidates are model ids on the
	// SAME OpenAI-compatible endpoint as the primary (the common gateway
	// shape); each id becomes its own provider instance so the router can
	// fail over across models. Routing supersedes failover chaining: when
	// routing is enabled the router replaces the chain (its candidate
	// order is the failover path). Misconfiguration NEVER breaks the run
	// path — an invalid policy or unregistered candidate logs a warning
	// and keeps the unwrapped provider.
	if policyName := strings.TrimSpace(os.Getenv(RoutingPolicyEnvVar)); policyName != "" {
		if router, rerr := routingProviderFromEnv(policyName, apiKey, baseURL, model, client, logr); rerr == nil {
			provider = router
		}
	}

	logr.Info("model provider configured", "base_url", baseURL, "model", model)
	return provider, true
}

// routingProviderFromEnv builds the PolicyProvider for the routing env vars
// (called only when AGENTOS_ROUTING_POLICY is set). Errors are logged as
// warnings by ProviderFromEnv's caller contract: the function returns the
// error and the caller keeps the unwrapped provider.
func routingProviderFromEnv(policyName, apiKey, baseURL, model string, client *http.Client, logr *slog.Logger) (Provider, error) {
	policy, err := ParseRoutingPolicy(policyName)
	if err != nil {
		logr.Warn("model routing disabled: invalid AGENTOS_ROUTING_POLICY", "error", err.Error())
		return nil, err
	}
	registry := DefaultCapabilityRegistry()

	candidates := make([]string, 0, 4)
	for _, id := range strings.Split(os.Getenv(RoutingCandidatesEnvVar), ",") {
		if id = strings.TrimSpace(id); id != "" {
			candidates = append(candidates, id)
		}
	}
	if len(candidates) == 0 && strings.TrimSpace(model) != "" {
		candidates = []string{strings.TrimSpace(model)}
	}
	if len(candidates) == 0 {
		err := fmt.Errorf("no routing candidates: set %s or %s", RoutingCandidatesEnvVar, ProviderModelEnvVar)
		logr.Warn("model routing disabled", "error", err.Error())
		return nil, err
	}

	routable := make([]Candidate, 0, len(candidates))
	for _, id := range candidates {
		caps, ok := registry.Lookup(id)
		if !ok {
			logr.Warn("model routing candidate skipped: unknown to the capability registry",
				"model", id, "env", CapabilitiesEnvVar)
			continue
		}
		routable = append(routable, Candidate{
			Model:    caps.Model,
			Provider: NewOpenAIProvider("route:"+caps.Model, apiKey, baseURL, caps.Model, client),
			Caps:     caps,
		})
	}
	if len(routable) == 0 {
		err := fmt.Errorf("no routable candidates registered (register models via %s)", CapabilitiesEnvVar)
		logr.Warn("model routing disabled", "error", err.Error())
		return nil, err
	}
	router, err := NewPolicyProvider(registry, policy, routable)
	if err != nil {
		logr.Warn("model routing disabled", "error", err.Error())
		return nil, err
	}
	logr.Info("model routing enabled", "policy", string(policy),
		"candidates", strings.Join(modelIDs(routable), ","))
	return router, nil
}
