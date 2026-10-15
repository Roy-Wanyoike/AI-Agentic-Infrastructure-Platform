package tools

import "fmt"

// catalog_builtin.go — the expanded built-in catalog registration (issue
// #111). The pre-existing registration lived in catalog.go's DefaultRegistry
// (calculator + http_request); this file ADDS the six new catalog tools
// (database_query, search, code_execute, webhook, json_transform,
// file_storage) behind one registration entry point so the wiring stays
// exactly one line at the registry build site:
//
//      internal/tools/catalog.go, DefaultRegistry(): the line
//          _ = RegisterBuiltinCatalog(r)
//      (documented there as the issue #111 wiring line).
//
// Fail-closed defaults make the default wiring safe in every process:
//   - database_query registers with no provider and refuses to run until an
//     operator wires DBConnectionProvider (RegisterBuiltinCatalogWithOptions);
//   - webhook registers with an EMPTY allow-list (default deny) and refuses
//     every target until an operator wires one;
//   - search reads AGENTOS_SEARCH_API_URL/AGENTOS_SEARCH_API_KEY from the
//     environment and fails closed with ErrSearchUnavailable when unset;
//   - file_storage registers with the per-process InMemoryStorageBackend;
//   - code_execute and json_transform need no configuration.
//
// Every tool consults the optional per-call ToolPolicyGate (policy.go), the
// opt-in defense-in-depth layer on top of the runtime's authoritative
// enforcer seam. The MemoryAuditSink is the zero-infrastructure default for
// database_query's audit trail; wire the platform audit service through
// RegisterBuiltinCatalogWithOptions for durable trails.
//
// cmd/worker (cmd/worker/main.go) keeps its explicit per-tool registration
// for its execution registry and can adopt RegisterBuiltinCatalogWithOptions
// as operators provision the seams; the API process (/v1/tools, MCP default
// registry) uses DefaultRegistry and therefore advertises the full catalog.

// BuiltinCatalogOptions carries the operator-provided seams for the six new
// built-in tools. Zero values select the fail-closed defaults documented in
// the file comment (nil audit -> bounded in-process memory sink; nil policy
// evaluator -> no per-call gate beyond the runtime seam).
type BuiltinCatalogOptions struct {
	// Audit receives tool audit records (database_query today). Nil selects
	// NewMemoryAuditSink(0) — the bounded, process-local diagnostics sink.
	Audit AuditSink
	// Policy, when non-nil, builds the per-call ToolPolicyGate consulted by
	// every registered tool BEFORE it does work (fails closed on deny/error).
	Policy ToolPolicyEvaluator
	// Database resolves org-scoped named connections for database_query.
	// Nil keeps the tool registered but fail-closed (ErrConnectionRequired).
	Database DBConnectionProvider
	// Search configures the web-mode endpoint; empty means unavailable (the
	// tool fails closed with ErrSearchUnavailable for web/hybrid web side).
	Search SearchConfig
	// Knowledge wires knowledge mode of the search tool (nil = unavailable).
	Knowledge KnowledgeSearcher
	// WebhookAllowLists are the webhook tool's allow-listed origins. Empty
	// keeps the tool registered in default-deny (every delivery refused).
	WebhookAllowList []string
	// WebhookSigner + WebhookSigningKeyRef configure HMAC-SHA256 signing via
	// the secrets seam; unset deliveries are sent unsigned (signed=false).
	WebhookSigner        WebhookSecretResolver
	WebhookSigningKeyRef string
	// FileStorage is the storage backend for file_storage; nil selects the
	// built-in InMemoryStorageBackend (per-process, non-durable).
	FileStorage StorageBackend
}

// RegisterBuiltinCatalog registers the six expanded-catalog tools with the
// fail-closed defaults (search endpoint from the environment, memory audit
// sink, no DB provider, default-deny webhook, in-memory storage). The default
// option set cannot fail, which is why DefaultRegistry can wire it in one
// line; operator-provided options go through RegisterBuiltinCatalogWithOptions.
// A nil registry is a no-op.
func RegisterBuiltinCatalog(r *Registry) error {
	return RegisterBuiltinCatalogWithOptions(r, BuiltinCatalogOptions{
		Search: SearchConfigFromEnv(),
	})
}

// RegisterBuiltinCatalogWithOptions registers the six expanded-catalog tools
// over the given operator seams. It returns an error only for invalid
// operator input (e.g. a malformed webhook allow-list entry) — registration
// itself is additive and idempotent per tool name (the registry overwrites
// same-named entries, exactly like Register).
func RegisterBuiltinCatalogWithOptions(r *Registry, opts BuiltinCatalogOptions) error {
	if r == nil {
		return nil
	}
	audit := opts.Audit
	if audit == nil {
		audit = NewMemoryAuditSink(0)
	}
	gate := NewToolPolicyGate(opts.Policy)

	r.Register(NewDatabaseTool(opts.Database).
		WithAuditSink(audit).
		WithPolicyGate(gate))

	r.Register(NewSearchTool(opts.Search, opts.Knowledge).
		WithPolicyGate(gate))

	r.Register(NewCodeExecTool().WithPolicyGate(gate))

	webhook, err := NewWebhookTool(opts.WebhookAllowList, opts.WebhookSigner, opts.WebhookSigningKeyRef)
	if err != nil {
		return fmt.Errorf("tools: builtin webhook registration failed: %w", err)
	}
	r.Register(webhook.WithPolicyGate(gate))

	r.Register(NewJSONTransformTool().WithPolicyGate(gate))

	storage := opts.FileStorage
	if storage == nil {
		storage = NewInMemoryStorageBackend()
	}
	r.Register(NewFileStorageTool(storage).WithPolicyGate(gate))

	return nil
}
