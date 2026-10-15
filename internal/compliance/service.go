// Package compliance — wave-9 issue #138: the compliance EVIDENCE PACK.
//
// A READ-ONLY aggregation service that turns existing platform primitives
// (audit chain, policies, approvals, deployments/canary, evaluations, agent
// keys, MCP registry, versions) into auditor-ready artifacts:
//
//	Controls(ctx, orgID)               control catalog × frameworks × satisfied-by × evidence
//	TechnicalDoc(ctx, orgID, agentID)  per-agent Annex-IV-style documentation (JSON + Markdown)
//	EvidenceBundle(ctx, orgID, from, to)  deterministic JSONL evidence export
//
// READ-ONLY POSTURE (contract): the service consumes its collaborators
// exclusively through the narrow reader interfaces below. None of those
// interfaces carries a write method, so the package CANNOT append audit
// rows, mutate deployments, or otherwise change state it evidences — a
// compliance read must never contaminate the trail it reports on (same
// posture as cmd/api/audit_chain.go). Because it is pure aggregation there
// is NO database migration for this package, by design.
//
// DETERMINISM: artifacts are byte-stable for unchanged data. Fixed catalogs
// are declared as ordered slices, every rendered list is explicitly sorted,
// timestamps render as RFC3339 UTC, and the evidence bundle contains NO
// wall-clock generation timestamp (the request window is part of the bytes;
// "when was this pulled" is answered by the operator's WORM copy, not by
// re-export drift). The injectable Now clock covers the derived windows
// (30-day audit posture) and the technical-doc generated_at stamp.
//
// SECRET HYGIENE: artifacts carry counts, hashes (the audit chain head is
// sha256 hex, non-sensitive by construction), identifiers and NAMES — never
// secret material, tool payloads, or audit metadata bodies.
package compliance

import (
	"context"
	"errors"
	"time"

	"agentos/internal/agents"
	"agentos/internal/apikeys"
	"agentos/internal/approvals"
	"agentos/internal/audit"
	"agentos/internal/deployments"
	"agentos/internal/evaluations"
	"agentos/internal/mcp"
	"agentos/internal/policies"
)

// Sentinel errors surfaced to the HTTP layer.
var (
	// ErrAgentNotFound is returned by TechnicalDoc when the agent does not
	// exist or belongs to another organization (unknown and foreign collapse
	// into one — no existence leak).
	ErrAgentNotFound = errors.New("compliance: agent not found")
	// ErrOrgRequired guards against an empty tenant scope.
	ErrOrgRequired = errors.New("compliance: organization required")
	// ErrWindowRequired is returned when an evidence window boundary is missing.
	ErrWindowRequired = errors.New("compliance: evidence window requires from and to")
	// ErrWindowRange is returned when from is after to.
	ErrWindowRange = errors.New("compliance: evidence window from must not be after to")
	// ErrInvalidWindowInput is returned when a window boundary cannot be parsed.
	ErrInvalidWindowInput = errors.New("compliance: invalid evidence window boundary")
)

// ---------------------------------------------------------------------------
// Narrow read-only interfaces (constructed from the concrete services in
// cmd/api/main.go via newComplianceService; tests stub them).
// ---------------------------------------------------------------------------

// AuditTrailReader is the read-only audit surface: the tamper-evident chain
// head plus the org trail listing (for windowed entry counts).
// *audit.Service satisfies this (chain.go / service.go).
type AuditTrailReader interface {
	ChainHead(ctx context.Context, orgID string) (string, int, error)
	ListCtx(ctx context.Context, orgID string) ([]*audit.Entry, error)
}

// AgentsReader resolves agents within one tenant.
// *agents.Service satisfies this.
type AgentsReader interface {
	GetAgentCtx(ctx context.Context, orgID, id string) (*agents.Agent, error)
	ListAgentsCtx(ctx context.Context, orgID string) ([]*agents.Agent, error)
}

// VersionsReader lists an agent's configuration versions (lifecycle/change
// management evidence). *agents.VersionsService satisfies this.
type VersionsReader interface {
	ListVersionsCtx(ctx context.Context, orgID, agentID string) ([]*agents.ConfigVersion, error)
}

// DeploymentsReader lists deployment rows (optionally for one agent; empty
// agentID = all agents of the tenant). *deployments.Service satisfies this.
type DeploymentsReader interface {
	ListDeploymentsCtx(ctx context.Context, orgID, agentID string) ([]*deployments.Deployment, error)
}

// EvaluationsReader is the eval evidence surface: dataset inventory plus the
// aggregated completed-run samples per agent. *evaluations.Service satisfies
// this.
type EvaluationsReader interface {
	ListDatasets(ctx context.Context, orgID string) ([]*evaluations.Dataset, error)
	ListRunSamples(ctx context.Context, orgID, agentID string, limit int) ([]evaluations.RunSample, error)
}

// PoliciesReader lists the tenant's governance policies.
// *policies.Service satisfies this.
type PoliciesReader interface {
	ListPoliciesCtx(ctx context.Context, orgID string) ([]*policies.Policy, error)
}

// OrgKeysReader counts org-scoped API keys (access lifecycle evidence).
// *apikeys.Service satisfies this.
type OrgKeysReader interface {
	ListKeysCtx(ctx context.Context, orgID string) ([]*apikeys.APIKey, error)
}

// AgentKeysReader lists agent-scoped keys (tool allow-list surface + agent
// identity evidence). *apikeys.Service satisfies this.
type AgentKeysReader interface {
	ListAgentKeys(ctx context.Context, orgID, agentID string) ([]*apikeys.AgentKey, error)
}

// ApprovalsReader lists human-oversight approvals (optionally filtered by
// status; empty = all). *approvals.Service satisfies this.
type ApprovalsReader interface {
	List(ctx context.Context, orgID, status string) ([]*approvals.Approval, error)
}

// MCPRegistryReader lists the tenant's registered MCP servers (supply-chain
// evidence). *mcp.Registry satisfies this.
type MCPRegistryReader interface {
	ListServers(ctx context.Context, orgID string) ([]*mcp.Server, error)
}

// Compile-time proof that the concrete platform services satisfy the reader
// interfaces WITHOUT any modification to those packages (pure consumption).
var (
	_ AuditTrailReader  = (*audit.Service)(nil)
	_ AgentsReader      = (*agents.Service)(nil)
	_ VersionsReader    = (*agents.VersionsService)(nil)
	_ DeploymentsReader = (*deployments.Service)(nil)
	_ EvaluationsReader = (*evaluations.Service)(nil)
	_ PoliciesReader    = (*policies.Service)(nil)
	_ OrgKeysReader     = (*apikeys.Service)(nil)
	_ AgentKeysReader   = (*apikeys.Service)(nil)
	_ ApprovalsReader   = (*approvals.Service)(nil)
	_ MCPRegistryReader = (*mcp.Registry)(nil)
)

// Deps wires the read-only surfaces. Every field is optional (nil = the
// corresponding live evidence collector is unavailable and artifacts degrade
// to static pointers / zero counts instead of failing — an auditor artifact
// must render even when one backend hiccups).
type Deps struct {
	Audit       AuditTrailReader
	Agents      AgentsReader
	Versions    VersionsReader
	Deployments DeploymentsReader
	Evaluations EvaluationsReader
	Policies    PoliciesReader
	OrgAPIKeys  OrgKeysReader
	AgentKeys   AgentKeysReader
	Approvals   ApprovalsReader
	MCPRegistry MCPRegistryReader

	// Now overrides the clock used for derived windows (30-day audit
	// posture) and the technical-doc generated_at stamp. nil = time.Now.
	Now func() time.Time
}

// Service is the read-only compliance aggregation service. It holds no
// state of its own beyond the deps, so artifacts are pure functions of
// (collaborator data, request window, catalog version).
type Service struct {
	deps Deps
	now  func() time.Time
}

// NewService constructs the service. Zero deps render degraded but
// functional artifacts (static pointers only).
func NewService(deps Deps) *Service {
	return &Service{deps: deps}
}

// clock returns the effective UTC clock instant (never panics on nil).
func (s *Service) clock() time.Time {
	if s == nil || s.deps.Now == nil {
		return time.Now().UTC()
	}
	return s.deps.Now().UTC()
}

// ---------------------------------------------------------------------------
// Control catalog (static, declared in code) + live evidence injection.
// ---------------------------------------------------------------------------

// Frameworks is the master framework list rendered on the controls report.
var Frameworks = []string{"SOC2", "ISO42001", "EU_AI_ACT", "NIST_AIRMF"}

const (
	// EvidenceKindLive marks evidence measured from a live service at
	// request time; EvidenceKindStaticPointer marks a pointer to the
	// implementing surface (auditors follow the pointer).
	EvidenceKindLive          = "live"
	EvidenceKindStaticPointer = "static_pointer"

	// auditChainVersionV1 mirrors internal/audit.ChainVersionV1 (borrowed
	// literal so the evidence pack never depends on audit internals).
	auditChainVersionV1 = "audit-chain/v1"

	// auditPostureWindowDays is the derived look-back window of the
	// per-agent audit posture (issue #138: last 30-day entry count).
	auditPostureWindowDays = 30

	// evalSampleLimit caps completed-run samples pulled per agent for the
	// technical doc (same spirit as deployments.MaxCanarySampleRuns).
	evalSampleLimit = 100

	// CatalogVersion tags the static catalog shape in rendered artifacts.
	CatalogVersion = "compliance-catalog/v1"
)

// Control is one catalog control: what platform primitive satisfies it, and
// which framework tags it maps to. The catalog is static; live evidence is
// attached at request time by control ID.
type Control struct {
	ID          string
	Name        string
	Description string
	SatisfiedBy []string
	Frameworks  []string
}

// Domain groups controls (identity, isolation, audit, oversight, risk,
// lifecycle, supply chain, residency).
type Domain struct {
	ID          string
	Name        string
	Description string
	Controls    []Control
}

// catalog returns the STATIC control catalog. Declaration order is the
// render order (determinism by construction). Framework tags are mapping
// hints for auditors: SOC 2 TSC criteria (CCx.y), ISO/IEC 42001:2023 Annex A
// categories (A.n), EU AI Act articles (ART_n), NIST AI RMF functions.
func catalog() []Domain {
	return []Domain{
		{
			ID:          "identity_access",
			Name:        "Identity & Access",
			Description: "Who (human or agent) can act, and how identity is federated, provisioned and scoped.",
			Controls: []Control{
				{
					ID:          "identity_access.sso_scim",
					Name:        "Federated identity & lifecycle provisioning",
					Description: "OIDC SSO with JIT provisioning and SCIM 2.0 user lifecycle; IdP secrets held by reference in the secrets store.",
					SatisfiedBy: []string{"OIDC SSO (issue #29)", "SCIM 2.0 provisioning (issue #29)", "audited SCIM token revocation (issue #79)"},
					Frameworks:  []string{"SOC2_TSC_CC6.1", "SOC2_TSC_CC6.2", "ISO42001_A.7", "EUAI_ACT_ART_14", "NIST_AIRMF_GOVERN"},
				},
				{
					ID:          "identity_access.rbac",
					Name:        "Role-based access control",
					Description: "OWNER/ADMIN/MEMBER/VIEWER roles with per-surface permission grants enforced by middleware on every route.",
					SatisfiedBy: []string{"auth.RequirePermission middleware", "per-domain permission enums (audit.read, deployments.*, ...)"},
					Frameworks:  []string{"SOC2_TSC_CC6.1", "SOC2_TSC_CC6.3", "ISO42001_A.7", "NIST_AIRMF_GOVERN"},
				},
				{
					ID:          "identity_access.agent_keys",
					Name:        "Agent-scoped keys & tool allow-lists (agent identity v1)",
					Description: "ag_ keys bound to one agent with tool allow-lists, expiry and revocation; least privilege for non-human actors.",
					SatisfiedBy: []string{"agent-scoped API keys with tool allow-lists (issue #100)", "runtime tool gate (agent_key_tool_not_allowed)"},
					Frameworks:  []string{"SOC2_TSC_CC6.1", "ISO42001_A.7", "EUAI_ACT_ART_14", "NIST_AIRMF_MANAGE"},
				},
			},
		},
		{
			ID:          "tenant_isolation_secrets",
			Name:        "Tenant Isolation & Secrets",
			Description: "Every store is organization-scoped; credentials are encrypted and never rendered.",
			Controls: []Control{
				{
					ID:          "tenant_isolation.secrets_encryption",
					Name:        "Encrypted org-scoped secrets",
					Description: "Secrets persist under AES-256-GCM envelope encryption keyed by AGENTOS_SECRETS_MASTER_KEY; plaintext persistence is a fail-fast regression.",
					SatisfiedBy: []string{"secrets service AES-256-GCM (issue #25)", "secret refs resolved at use-time (connectors, MCP registry, SSO client secrets)"},
					Frameworks:  []string{"SOC2_TSC_CC6.1", "SOC2_TSC_CC6.7", "ISO42001_A.7", "EUAI_ACT_ART_10", "NIST_AIRMF_MEASURE"},
				},
				{
					ID:          "tenant_isolation.org_guards",
					Name:        "Tenant guard on every read and write",
					Description: "Store queries filter by organization_id; foreign ids collapse into not-found without an existence leak.",
					SatisfiedBy: []string{"tenant-guarded Store contracts across services", "cross-tenant 404 convention on all org-scoped routes"},
					Frameworks:  []string{"SOC2_TSC_CC6.1", "ISO42001_A.7", "NIST_AIRMF_MEASURE"},
				},
			},
		},
		{
			ID:          "logging_audit",
			Name:        "Logging & Audit",
			Description: "Append-only, org-scoped audit trail with a tamper-evident hash chain and deterministic exports.",
			Controls: []Control{
				{
					ID:          "logging_audit.tamper_evident_chain",
					Name:        "Tamper-evident audit chain",
					Description: "Every audit row carries a service-owned hash-chain stamp (sha256 over a length-prefixed canonical encoding); verification replays positionally and names the first break. The head hash anchors external WORM storage.",
					SatisfiedBy: []string{"audit chain v1 (wave-8 issue #94)", "GET /v1/audit/verify", "GET /v1/audit/export (deterministic JSONL + X-Audit-Head-Hash)"},
					Frameworks:  []string{"SOC2_TSC_CC7.2", "SOC2_TSC_CC7.3", "ISO42001_A.6", "EUAI_ACT_ART_12", "NIST_AIRMF_MEASURE"},
				},
				{
					ID:          "logging_audit.audit_events",
					Name:        "Append-only org audit trail",
					Description: "Structured action/resource/metadata events for every privileged action, listed via a keyset-paginated read-only endpoint.",
					SatisfiedBy: []string{"audit service (issue #18)", "GET /v1/audit-events (audit.read = OWNER/ADMIN)"},
					Frameworks:  []string{"SOC2_TSC_CC7.2", "ISO42001_A.6", "EUAI_ACT_ART_12", "NIST_AIRMF_MEASURE"},
				},
			},
		},
		{
			ID:          "human_oversight",
			Name:        "Human Oversight",
			Description: "Dangerous actions pause for explicit human decisions before they execute.",
			Controls: []Control{
				{
					ID:          "human_oversight.approvals",
					Name:        "Approval-gated actions (HITL)",
					Description: "Runs/actions can require an approval record; the decision (approve/reject/cancel with reason and approver) is stored and audited. Policy conditions can mark decisions as approval-required.",
					SatisfiedBy: []string{"approvals service (issue #9)", "run policy enforcement approval seam (issue #75)", "policy conditions.require_approval"},
					Frameworks:  []string{"SOC2_TSC_CC8.1", "ISO42001_A.9", "EUAI_ACT_ART_14", "NIST_AIRMF_MANAGE"},
				},
			},
		},
		{
			ID:          "risk_policy",
			Name:        "Risk & Policy",
			Description: "A real policy engine decides allow/deny on tools, agents, workflows and deployments before execution.",
			Controls: []Control{
				{
					ID:          "risk_policy.engine",
					Name:        "Enforced policy engine",
					Description: "Org-scoped policies with allow/deny effects, resource types, action lists and conditions (tool allow-list, cost cap, environment); enforced on run creation and tool calls — not advisory.",
					SatisfiedBy: []string{"policies service + enforcer (issue #16)", "run-time enforcement seam (issue #75)"},
					Frameworks:  []string{"SOC2_TSC_CC5.2", "ISO42001_A.5", "EUAI_ACT_ART_9", "NIST_AIRMF_GOVERN"},
				},
			},
		},
		{
			ID:          "lifecycle_change_mgmt",
			Name:        "Lifecycle & Change Management",
			Description: "Agent changes are versioned snapshots; rollouts are canaried and gated on evaluation evidence.",
			Controls: []Control{
				{
					ID:          "lifecycle_change_mgmt.versions",
					Name:        "Versioned agent configuration",
					Description: "Immutable version snapshots (draft/published/archived) with publish, rollback and diff; deployments reference published versions only.",
					SatisfiedBy: []string{"config versions service (issue #21)", "versions diff endpoint"},
					Frameworks:  []string{"SOC2_TSC_CC8.1", "ISO42001_A.6", "EUAI_ACT_ART_15", "NIST_AIRMF_MANAGE"},
				},
				{
					ID:          "lifecycle_change_mgmt.eval_gated_canary",
					Name:        "Eval-gated canary promotion",
					Description: "Canary splits are deterministic per agent; an optional promotion policy (min pass rate, min runs, p95 latency, cost caps) promotes or rolls back automatically from completed eval runs, with the single decision recorded on the row and audited.",
					SatisfiedBy: []string{"canary deployments (issue #13)", "eval-gated autopromotion (issue #51)", "canary status endpoint"},
					Frameworks:  []string{"SOC2_TSC_CC8.1", "ISO42001_A.6", "EUAI_ACT_ART_15", "NIST_AIRMF_MANAGE"},
				},
			},
		},
		{
			ID:          "supply_chain",
			Name:        "Supply Chain",
			Description: "Third-party agent packages and MCP tool servers are registered, signed and drift-monitored.",
			Controls: []Control{
				{
					ID:          "supply_chain.marketplace_signing",
					Name:        "Marketplace package signing & verification",
					Description: "Marketplace listings carry publisher signatures verified on install; installs come from version snapshots.",
					SatisfiedBy: []string{"marketplace signing & verification (issue #34)", "install from version snapshots (issue #28)"},
					Frameworks:  []string{"SOC2_TSC_CC9.2", "ISO42001_A.10", "EUAI_ACT_ART_25", "NIST_AIRMF_MAP"},
				},
				{
					ID:          "supply_chain.mcp_trust",
					Name:        "MCP server registry & toolset trust",
					Description: "Org-scoped MCP registry with structural toolset fingerprints, drift detection, version pinning and explicit change acceptance; optional pre-wire argument scanning.",
					SatisfiedBy: []string{"MCP registry (issue #82)", "MCP trust layer (issue #95)", "evidence-first arg scan (MCP_ARG_SCAN_BLOCKED)"},
					Frameworks:  []string{"SOC2_TSC_CC9.2", "ISO42001_A.10", "EUAI_ACT_ART_25", "NIST_AIRMF_MAP"},
				},
			},
		},
		{
			ID:          "residency",
			Name:        "Residency",
			Description: "Where tenant data lives and how it is fenced.",
			Controls: []Control{
				{
					ID:          "residency.org_scoped_storage",
					Name:        "Org-scoped storage & deployment residency",
					Description: "All tenant rows (agents, runs, audit, secrets, evals) live in one deployment store scoped by organization_id; no cross-org replication surface exists. Deployment residency is an operator decision (DATABASE_URL / AGENTOS_QUEUE endpoints).",
					SatisfiedBy: []string{"single-logical-store org scoping", "operator-managed Postgres/Redis endpoints (residency surface: deploy/ + docker-compose.*)"},
					Frameworks:  []string{"SOC2_TSC_CC6.7", "ISO42001_A.7", "EUAI_ACT_ART_10", "NIST_AIRMF_MAP"},
				},
			},
		},
	}
}

// Evidence is one evidence pointer attached to a control at request time.
// Exactly one of Value (live) / Surface (static pointer) is meaningful.
type Evidence struct {
	Kind    string `json:"kind"`
	Source  string `json:"source"`
	Value   any    `json:"value,omitempty"`
	Surface string `json:"surface,omitempty"`
	Note    string `json:"note,omitempty"`
}

// staticEvidence is a pointer to an implementing surface.
func staticEvidence(source, surface, note string) Evidence {
	return Evidence{Kind: EvidenceKindStaticPointer, Source: source, Surface: surface, Note: note}
}

// liveEvidence is a measured value pulled at request time.
func liveEvidence(source string, value any) Evidence {
	return Evidence{Kind: EvidenceKindLive, Source: source, Value: value}
}

// degradedEvidence replaces a live collector that failed: the artifact stays
// renderable and the gap is explicit for the auditor.
func degradedEvidence(source string) Evidence {
	return Evidence{Kind: EvidenceKindStaticPointer, Source: source, Note: "live evidence unavailable"}
}

// ControlsReport is the GET /v1/compliance/controls payload.
type ControlsReport struct {
	OrganizationID string         `json:"organization_id"`
	GeneratedAt    string         `json:"generated_at"`
	CatalogVersion string         `json:"catalog_version"`
	Frameworks     []string       `json:"frameworks"`
	Domains        []DomainReport `json:"domains"`
}

// DomainReport is one rendered domain.
type DomainReport struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Controls    []ControlReport `json:"controls"`
}

// ControlReport is one rendered control with its evidence pointers.
type ControlReport struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	SatisfiedBy []string   `json:"satisfied_by"`
	Frameworks  []string   `json:"frameworks"`
	Evidence    []Evidence `json:"evidence"`
}

// Controls builds the control-mapping report for one organization: the
// static catalog with live evidence pointers filled in per org. Collector
// failures degrade that control to an explicit "live evidence unavailable"
// pointer — the report never fails as a whole.
func (s *Service) Controls(ctx context.Context, orgID string) (*ControlsReport, error) {
	if orgID == "" {
		return nil, ErrOrgRequired
	}
	live := s.collectLiveEvidence(ctx, orgID)

	domains := make([]DomainReport, 0, 8)
	for _, d := range catalog() {
		dr := DomainReport{ID: d.ID, Name: d.Name, Description: d.Description, Controls: make([]ControlReport, 0, len(d.Controls))}
		for _, c := range d.Controls {
			cr := ControlReport{
				ID:          c.ID,
				Name:        c.Name,
				Description: c.Description,
				SatisfiedBy: append([]string(nil), c.SatisfiedBy...),
				Frameworks:  append([]string(nil), c.Frameworks...),
				Evidence:    append([]Evidence(nil), live[c.ID]...),
			}
			if cr.Evidence == nil {
				cr.Evidence = []Evidence{}
			}
			dr.Controls = append(dr.Controls, cr)
		}
		domains = append(domains, dr)
	}
	return &ControlsReport{
		OrganizationID: orgID,
		GeneratedAt:    s.clock().Format(time.RFC3339),
		CatalogVersion: CatalogVersion,
		Frameworks:     append([]string(nil), Frameworks...),
		Domains:        domains,
	}, nil
}

// collectLiveEvidence gathers per-control evidence from the wired readers.
// Each collector is independent and best-effort: a nil dep or a read error
// yields a degraded pointer for that control only.
func (s *Service) collectLiveEvidence(ctx context.Context, orgID string) map[string][]Evidence {
	out := make(map[string][]Evidence)

	// identity_access — SSO/SCIM counts are NOT exported by their services
	// (the config store and the token store stay encapsulated), so those two
	// remain static pointers; org key counts are measured live.
	if s.deps.OrgAPIKeys != nil {
		if keys, err := s.deps.OrgAPIKeys.ListKeysCtx(ctx, orgID); err == nil {
			out["identity_access.rbac"] = append(out["identity_access.rbac"],
				liveEvidence("apikeys.ListKeysCtx", map[string]any{"org_api_keys": len(keys)}))
		} else {
			out["identity_access.rbac"] = append(out["identity_access.rbac"], degradedEvidence("apikeys.ListKeysCtx"))
		}
	}
	out["identity_access.sso_scim"] = append(out["identity_access.sso_scim"],
		staticEvidence("sso.ConfigStore", "organizations.sso_config (OIDC; GET /v1/auth/sso/{org_slug}/login)",
			"connection count not exported read-only; inspect the SSO surface"),
		staticEvidence("scim.Service", "GET /v1/scim/v2/Users (token-gated)",
			"token count not exported read-only; token revocation is audited (scim_token.revoked)"))

	// logging_audit — the live chain anchor (head hash + trail length).
	if s.deps.Audit != nil {
		if head, length, err := s.deps.Audit.ChainHead(ctx, orgID); err == nil {
			out["logging_audit.tamper_evident_chain"] = append(out["logging_audit.tamper_evident_chain"],
				liveEvidence("audit.ChainHead", map[string]any{"head_hash": head, "length": length, "chain_version": auditChainVersionV1}))
		} else {
			out["logging_audit.tamper_evident_chain"] = append(out["logging_audit.tamper_evident_chain"], degradedEvidence("audit.ChainHead"))
		}
	}

	// human_oversight — approval counts by status (cheap: one listing).
	if s.deps.Approvals != nil {
		if list, err := s.deps.Approvals.List(ctx, orgID, ""); err == nil {
			out["human_oversight.approvals"] = append(out["human_oversight.approvals"],
				liveEvidence("approvals.List", countApprovals(list)))
		} else {
			out["human_oversight.approvals"] = append(out["human_oversight.approvals"], degradedEvidence("approvals.List"))
		}
	}

	// risk_policy — policy inventory (total/enabled by effect).
	if s.deps.Policies != nil {
		if list, err := s.deps.Policies.ListPoliciesCtx(ctx, orgID); err == nil {
			out["risk_policy.engine"] = append(out["risk_policy.engine"],
				liveEvidence("policies.ListPoliciesCtx", policySummaryCounts(list)))
		} else {
			out["risk_policy.engine"] = append(out["risk_policy.engine"], degradedEvidence("policies.ListPoliciesCtx"))
		}
	}

	// lifecycle_change_mgmt — canary state across the tenant's deployments.
	if s.deps.Deployments != nil {
		if rows, err := s.deps.Deployments.ListDeploymentsCtx(ctx, orgID, ""); err == nil {
			out["lifecycle_change_mgmt.eval_gated_canary"] = append(out["lifecycle_change_mgmt.eval_gated_canary"],
				liveEvidence("deployments.ListDeploymentsCtx", deploymentSummaryCounts(rows)))
		} else {
			out["lifecycle_change_mgmt.eval_gated_canary"] = append(out["lifecycle_change_mgmt.eval_gated_canary"], degradedEvidence("deployments.ListDeploymentsCtx"))
		}
	}

	// supply_chain — registered MCP servers (registry size).
	if s.deps.MCPRegistry != nil {
		if servers, err := s.deps.MCPRegistry.ListServers(ctx, orgID); err == nil {
			out["supply_chain.mcp_trust"] = append(out["supply_chain.mcp_trust"],
				liveEvidence("mcp.Registry.ListServers", map[string]any{"registered_servers": len(servers)}))
		} else {
			out["supply_chain.mcp_trust"] = append(out["supply_chain.mcp_trust"], degradedEvidence("mcp.Registry.ListServers"))
		}
	}

	return out
}

// approvalSummary is the human-oversight count shape (rendered in the
// controls report and the evidence bundle; ordered struct = stable bytes).
type approvalSummary struct {
	Pending   int `json:"pending"`
	Approved  int `json:"approved"`
	Rejected  int `json:"rejected"`
	Cancelled int `json:"cancelled"`
	Total     int `json:"total"`
}

// countApprovals tallies one approval listing by status.
func countApprovals(list []*approvals.Approval) approvalSummary {
	sum := approvalSummary{Total: len(list)}
	for _, a := range list {
		if a == nil {
			continue
		}
		switch a.Status {
		case approvals.StatusPending:
			sum.Pending++
		case approvals.StatusApproved:
			sum.Approved++
		case approvals.StatusRejected:
			sum.Rejected++
		case approvals.StatusCancelled:
			sum.Cancelled++
		}
	}
	return sum
}

// policySummary is the policy-inventory count shape.
type policySummary struct {
	Total   int `json:"total"`
	Enabled int `json:"enabled"`
	Allow   int `json:"effect_allow"`
	Deny    int `json:"effect_deny"`
}

// policySummaryCounts tallies a policy listing deterministically.
func policySummaryCounts(list []*policies.Policy) policySummary {
	sum := policySummary{Total: len(list)}
	for _, p := range list {
		if p == nil {
			continue
		}
		switch p.Effect {
		case policies.EffectAllow:
			sum.Allow++
		case policies.EffectDeny:
			sum.Deny++
		}
		if p.Enabled {
			sum.Enabled++
		}
	}
	return sum
}

// deploymentSummary is the lifecycle/canary count shape.
type deploymentSummary struct {
	Total        int `json:"total"`
	Healthy      int `json:"healthy"`
	WithCanary   int `json:"with_canary"`
	WithPolicy   int `json:"with_promotion_policy"`
	WithDecision int `json:"with_recorded_decision"`
}

// deploymentSummaryCounts tallies a deployment listing deterministically.
func deploymentSummaryCounts(rows []*deployments.Deployment) deploymentSummary {
	sum := deploymentSummary{Total: len(rows)}
	for _, d := range rows {
		if d == nil {
			continue
		}
		if d.Status == deployments.StatusHealthy {
			sum.Healthy++
		}
		if d.HasCanary() {
			sum.WithCanary++
			if d.Promotion != nil {
				sum.WithPolicy++
				if d.Promotion.Decision != nil {
					sum.WithDecision++
				}
			}
		}
	}
	return sum
}
