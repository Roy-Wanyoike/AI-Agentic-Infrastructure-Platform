package compliance

// techdoc.go — per-agent technical documentation (issue #138): an
// EU-AI-Act-Annex-IV-STYLE document generated from the agent's own platform
// data. It is a TEMPLATE over platform evidence, not a legal filing: every
// section maps to data the platform actually holds (identity, versions,
// evals, deployment/canary state, tool surface, policy bindings, audit
// posture), and the renderer is deterministic — stable section order,
// sorted lists, RFC3339 UTC timestamps, fixed float formatting.
//
// format=json renders ordered structs (encoding/json emits declaration
// order). format=md renders the Markdown document below; both derive from
// the SAME TechDoc value, so the two formats can never disagree.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"agentos/internal/agents"
	"agentos/internal/apikeys"
	"agentos/internal/deployments"
	"agentos/internal/policies"
)

// TechDoc is the structured per-agent technical document.
type TechDoc struct {
	Agent          TechDocAgent        `json:"agent"`
	OrganizationID string              `json:"organization_id"`
	GeneratedAt    string              `json:"generated_at"`
	FrameworkRef   string              `json:"framework_reference"`
	Versions       []TechDocVersion    `json:"versions"`
	Evaluations    TechDocEvaluations  `json:"evaluations"`
	Deployments    []TechDocDeployment `json:"deployments"`
	ToolSurface    TechDocToolSurface  `json:"tool_surface"`
	PolicyBindings []TechDocPolicy     `json:"policy_bindings"`
	AuditPosture   TechDocAudit        `json:"audit_posture"`
}

// TechDocAgent is the agent identity section.
type TechDocAgent struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Model       string `json:"model"`
	Status      string `json:"status"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

// TechDocVersion is one configuration version row.
type TechDocVersion struct {
	Version     int    `json:"version"`
	Status      string `json:"status"`
	CreatedAt   string `json:"created_at"`
	PublishedAt string `json:"published_at,omitempty"`
	PublishedBy string `json:"published_by,omitempty"`
}

// TechDocEvaluations aggregates the agent's eval evidence.
type TechDocEvaluations struct {
	Datasets       int     `json:"datasets"`
	DatasetCases   int     `json:"dataset_cases"`
	CompletedRuns  int     `json:"completed_runs"`
	PassRate       float64 `json:"pass_rate"`
	TotalCostCents float64 `json:"total_cost_cents"`
	LastRunAt      string  `json:"last_run_at,omitempty"`
	Note           string  `json:"note,omitempty"`
}

// TechDocDeployment is one deployment row's compliance view.
type TechDocDeployment struct {
	ID          string                 `json:"id"`
	Environment string                 `json:"environment"`
	Status      string                 `json:"status"`
	Version     int                    `json:"version"`
	Canary      *TechDocCanary         `json:"canary,omitempty"`
	Policy      *TechDocCanaryPolicy   `json:"promotion_policy,omitempty"`
	Decision    *TechDocCanaryDecision `json:"recorded_decision,omitempty"`
}

// TechDocCanary is the traffic split state.
type TechDocCanary struct {
	CanaryVersion int  `json:"canary_version"`
	Weight        int  `json:"weight_percent"`
	Active        bool `json:"active"`
}

// TechDocCanaryPolicy is the eval-gated promotion policy in effect.
type TechDocCanaryPolicy struct {
	MinPassRate        float64 `json:"min_pass_rate"`
	MinCanaryRuns      int     `json:"min_canary_runs"`
	MaxP95LatencyMs    int64   `json:"max_p95_latency_ms,omitempty"`
	MaxCostPerRunCents int64   `json:"max_cost_per_run_cents,omitempty"`
	WindowStart        string  `json:"window_start,omitempty"`
}

// TechDocCanaryDecision is the single recorded automatic decision.
type TechDocCanaryDecision struct {
	Action      string  `json:"action"`
	Reason      string  `json:"reason"`
	DecidedAt   string  `json:"decided_at"`
	PassRate    float64 `json:"pass_rate"`
	RunsCounted int     `json:"runs_counted"`
}

// TechDocToolSurface describes what the agent may call.
type TechDocToolSurface struct {
	Mode      string   `json:"mode"` // allowlist | unrestricted
	Tools     []string `json:"tools,omitempty"`
	AgentKeys int      `json:"agent_keys"`
	Note      string   `json:"note,omitempty"`
}

// TechDocPolicy is one policy binding relevant to agents.
type TechDocPolicy struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Effect          string   `json:"effect"`
	ResourceType    string   `json:"resource_type"`
	Actions         []string `json:"actions"`
	Enabled         bool     `json:"enabled"`
	Priority        int      `json:"priority"`
	RequireApproval bool     `json:"require_approval"`
}

// TechDocAudit is the audit posture of the agent's tenant.
type TechDocAudit struct {
	ChainHead         string `json:"chain_head"`
	ChainLength       int    `json:"chain_length"`
	EntriesLast30Days int    `json:"entries_last_30_days"`
	Note              string `json:"note,omitempty"`
}

// TechnicalDoc builds the Annex-IV-style document for one agent. Unknown or
// foreign agents surface ErrAgentNotFound (404 at the HTTP layer, no
// existence leak). Every read is through the read-only Deps interfaces; the
// method never mutates any collaborator.
func (s *Service) TechnicalDoc(ctx context.Context, orgID, agentID string) (*TechDoc, error) {
	if orgID == "" {
		return nil, ErrOrgRequired
	}
	if s.deps.Agents == nil {
		// Without the agents surface there is no agent identity to document:
		// unknown-by-construction (same 404 shape, no existence leak).
		return nil, ErrAgentNotFound
	}
	agent, err := s.deps.Agents.GetAgentCtx(ctx, orgID, agentID)
	if err != nil {
		if errors.Is(err, agents.ErrAgentNotFound) {
			return nil, ErrAgentNotFound
		}
		return nil, err
	}
	now := s.clock()

	doc := &TechDoc{
		Agent: TechDocAgent{
			ID:          agent.ID,
			Name:        agent.Name,
			Description: agent.Description,
			Model:       agent.Model,
			Status:      agent.Status,
			CreatedAt:   agent.CreatedAt.UTC().Format(time.RFC3339),
			UpdatedAt:   agent.UpdatedAt.UTC().Format(time.RFC3339),
		},
		OrganizationID: orgID,
		GeneratedAt:    now.Format(time.RFC3339),
		FrameworkRef:   "EU AI Act Annex IV (technical documentation) — template over AgentOS platform evidence",
		Versions:       []TechDocVersion{},
		Deployments:    []TechDocDeployment{},
		PolicyBindings: []TechDocPolicy{},
	}
	doc.Versions = s.docVersions(ctx, orgID, agentID)
	doc.Evaluations = s.docEvaluations(ctx, orgID, agentID)
	doc.Deployments = s.docDeployments(ctx, orgID, agentID)
	doc.ToolSurface = s.docToolSurface(ctx, orgID, agentID, now)
	doc.PolicyBindings = s.docPolicyBindings(ctx, orgID)
	doc.AuditPosture = s.docAuditPosture(ctx, orgID, now)
	return doc, nil
}

// docVersions renders the version history (ascending; the versions store
// already returns ascending order, the sort pins the contract).
func (s *Service) docVersions(ctx context.Context, orgID, agentID string) []TechDocVersion {
	if s.deps.Versions == nil {
		return nil
	}
	rows, err := s.deps.Versions.ListVersionsCtx(ctx, orgID, agentID)
	if err != nil {
		return nil
	}
	out := make([]TechDocVersion, 0, len(rows))
	for _, v := range rows {
		if v == nil {
			continue
		}
		tv := TechDocVersion{
			Version:     v.Version,
			Status:      v.Status,
			CreatedAt:   v.CreatedAt.UTC().Format(time.RFC3339),
			PublishedBy: v.PublishedBy,
		}
		if v.PublishedAt != nil {
			tv.PublishedAt = v.PublishedAt.UTC().Format(time.RFC3339)
		}
		out = append(out, tv)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out
}

// docEvaluations aggregates datasets + completed-run samples for the agent.
func (s *Service) docEvaluations(ctx context.Context, orgID, agentID string) TechDocEvaluations {
	ev := TechDocEvaluations{}
	if s.deps.Evaluations == nil {
		ev.Note = "evaluation surface not wired"
		return ev
	}
	if datasets, err := s.deps.Evaluations.ListDatasets(ctx, orgID); err == nil {
		ev.Datasets = len(datasets)
		for _, d := range datasets {
			if d != nil {
				ev.DatasetCases += d.CaseCount
			}
		}
	}
	samples, err := s.deps.Evaluations.ListRunSamples(ctx, orgID, agentID, evalSampleLimit)
	if err != nil {
		return ev
	}
	var cases, passed int
	var cost float64
	var last time.Time
	for _, smp := range samples {
		cases += smp.Cases
		passed += smp.Passed
		cost += smp.CostCents
		if smp.CreatedAt.After(last) {
			last = smp.CreatedAt
		}
	}
	ev.CompletedRuns = len(samples)
	if cases > 0 {
		ev.PassRate = round4(float64(passed) / float64(cases))
	}
	ev.TotalCostCents = round4(cost)
	if !last.IsZero() {
		ev.LastRunAt = last.UTC().Format(time.RFC3339)
	}
	return ev
}

// docDeployments renders the agent's deployment rows (deterministic order:
// environment, then id) with canary + promotion-policy state.
func (s *Service) docDeployments(ctx context.Context, orgID, agentID string) []TechDocDeployment {
	if s.deps.Deployments == nil {
		return nil
	}
	rows, err := s.deps.Deployments.ListDeploymentsCtx(ctx, orgID, agentID)
	if err != nil {
		return nil
	}
	out := make([]TechDocDeployment, 0, len(rows))
	for _, d := range rows {
		if d == nil {
			continue
		}
		td := TechDocDeployment{
			ID:          d.ID,
			Environment: d.Environment,
			Status:      d.Status,
			Version:     d.Version,
		}
		if d.HasCanary() {
			td.Canary = &TechDocCanary{
				CanaryVersion: d.CanaryVersion,
				Weight:        d.CanaryWeight,
				Active:        d.Status == deployments.StatusHealthy,
			}
			if d.Promotion != nil {
				td.Policy = &TechDocCanaryPolicy{
					MinPassRate:        round4(d.Promotion.Policy.MinPassRate),
					MinCanaryRuns:      d.Promotion.Policy.MinCanaryRuns,
					MaxP95LatencyMs:    d.Promotion.Policy.MaxP95LatencyMs,
					MaxCostPerRunCents: d.Promotion.Policy.MaxCostPerRunCents,
				}
				if !d.Promotion.WindowStart.IsZero() {
					td.Policy.WindowStart = d.Promotion.WindowStart.UTC().Format(time.RFC3339)
				}
				if dec := d.Promotion.Decision; dec != nil {
					td.Decision = &TechDocCanaryDecision{
						Action:      dec.Action,
						Reason:      dec.Reason,
						DecidedAt:   dec.DecidedAt.UTC().Format(time.RFC3339),
						PassRate:    round4(dec.PassRate),
						RunsCounted: dec.RunsCounted,
					}
				}
			}
		}
		out = append(out, td)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Environment != out[j].Environment {
			return out[i].Environment < out[j].Environment
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// docToolSurface derives the effective tool surface from the agent's active
// agent-scoped keys. Allow-list semantics follow internal/apikeys: nil =
// disabled (all tools), empty = no tools, entries = exactly those tools. An
// agent with NO active keys is unrestricted by construction (tool access is
// then governed by the org's RBAC + policies).
func (s *Service) docToolSurface(ctx context.Context, orgID, agentID string, now time.Time) TechDocToolSurface {
	ts := TechDocToolSurface{Mode: "unrestricted", Tools: []string{},
		Note: "no active agent-scoped keys — tool access governed by org RBAC and policies"}
	if s.deps.AgentKeys == nil {
		return ts
	}
	keys, err := s.deps.AgentKeys.ListAgentKeys(ctx, orgID, agentID)
	if err != nil {
		return ts
	}
	active := make([]*apikeys.AgentKey, 0, len(keys))
	for _, k := range keys {
		if k != nil && !k.Revoked() && !k.Expired(now) {
			active = append(active, k)
		}
	}
	ts.AgentKeys = len(active)
	if len(active) == 0 {
		return ts
	}
	union := make(map[string]struct{})
	for _, k := range active {
		if k.ToolAllowList == nil {
			// At least one active key has its allow-list disabled: the
			// effective surface is unrestricted (least-privilege gap made
			// visible to the auditor on purpose).
			return TechDocToolSurface{Mode: "unrestricted", Tools: []string{}, AgentKeys: len(active),
				Note: "an active agent key has its tool allow-list disabled"}
		}
		for _, t := range k.ToolAllowList {
			union[t] = struct{}{}
		}
	}
	tools := make([]string, 0, len(union))
	for t := range union {
		tools = append(tools, t)
	}
	sort.Strings(tools)
	return TechDocToolSurface{Mode: "allowlist", Tools: tools, AgentKeys: len(active)}
}

// docPolicyBindings renders the agent-relevant policies: resource_type
// "agent" or the wildcard. Sorted by policy id for stable bytes.
func (s *Service) docPolicyBindings(ctx context.Context, orgID string) []TechDocPolicy {
	if s.deps.Policies == nil {
		return nil
	}
	list, err := s.deps.Policies.ListPoliciesCtx(ctx, orgID)
	if err != nil {
		return nil
	}
	out := make([]TechDocPolicy, 0, len(list))
	for _, p := range list {
		if p == nil {
			continue
		}
		rt := strings.ToLower(p.ResourceType)
		if rt != policies.ResourceAgent && rt != policies.ResourceWildcard {
			continue
		}
		out = append(out, TechDocPolicy{
			ID:              p.ID,
			Name:            p.Name,
			Effect:          p.Effect,
			ResourceType:    p.ResourceType,
			Actions:         append([]string(nil), p.Actions...),
			Enabled:         p.Enabled,
			Priority:        p.Priority,
			RequireApproval: p.Conditions.RequireApproval,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// docAuditPosture renders the tenant's chain anchor plus the last-30-day
// entry count (window anchored at the service clock).
func (s *Service) docAuditPosture(ctx context.Context, orgID string, now time.Time) TechDocAudit {
	ap := TechDocAudit{}
	if s.deps.Audit == nil {
		ap.Note = "audit surface not wired"
		return ap
	}
	if head, length, err := s.deps.Audit.ChainHead(ctx, orgID); err == nil {
		ap.ChainHead = head
		ap.ChainLength = length
	}
	if entries, err := s.deps.Audit.ListCtx(ctx, orgID); err == nil {
		cutoff := now.Add(-auditPostureWindowDays * 24 * time.Hour)
		for _, e := range entries {
			if e != nil && !e.CreatedAt.UTC().Before(cutoff) && !e.CreatedAt.UTC().After(now) {
				ap.EntriesLast30Days++
			}
		}
	}
	return ap
}

// round4 fixes float precision for stable artifacts (4 decimals).
func round4(f float64) float64 {
	return math.Round(f*10000) / 10000
}

// ---------------------------------------------------------------------------
// Markdown rendering (format=md) — deterministic, stable section order.
// ---------------------------------------------------------------------------

// mdSections is the FIXED section order of the Markdown document.
var mdSections = []string{
	"Agent Identity",
	"Version History",
	"Evaluation Evidence",
	"Deployment & Canary Policy",
	"Tool Surface",
	"Policy Bindings",
	"Audit Posture",
}

// RenderMarkdown renders the doc as deterministic Markdown. The section
// order is pinned by mdSections; every list is sorted; every timestamp is
// RFC3339 UTC (they arrive pre-formatted from the struct).
func (s *Service) RenderMarkdown(doc *TechDoc) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "# Technical Documentation — %s\n\n", doc.Agent.Name)
	fmt.Fprintf(&b, "- Agent ID: %s\n", doc.Agent.ID)
	fmt.Fprintf(&b, "- Organization: %s\n", doc.OrganizationID)
	fmt.Fprintf(&b, "- Generated: %s\n", doc.GeneratedAt)
	fmt.Fprintf(&b, "- Framework reference: %s\n\n", doc.FrameworkRef)

	for _, title := range mdSections {
		switch title {
		case "Agent Identity":
			b.WriteString("## 1. Agent Identity\n\n")
			fmt.Fprintf(&b, "- ID: %s\n- Name: %s\n- Model: %s\n- Status: %s\n",
				doc.Agent.ID, doc.Agent.Name, doc.Agent.Model, doc.Agent.Status)
			fmt.Fprintf(&b, "- Description: %s\n- Created: %s\n- Updated: %s\n\n",
				doc.Agent.Description, doc.Agent.CreatedAt, doc.Agent.UpdatedAt)
		case "Version History":
			b.WriteString("## 2. Version History\n\n")
			if len(doc.Versions) == 0 {
				b.WriteString("- No configuration versions recorded.\n\n")
				continue
			}
			b.WriteString("| Version | Status | Created | Published | Published By |\n|---|---|---|---|---|\n")
			for _, v := range doc.Versions {
				fmt.Fprintf(&b, "| %d | %s | %s | %s | %s |\n", v.Version, v.Status, v.CreatedAt, orDash(v.PublishedAt), orDash(v.PublishedBy))
			}
			b.WriteString("\n")
		case "Evaluation Evidence":
			b.WriteString("## 3. Evaluation Evidence\n\n")
			fmt.Fprintf(&b, "- Datasets: %d (cases: %d)\n", doc.Evaluations.Datasets, doc.Evaluations.DatasetCases)
			fmt.Fprintf(&b, "- Completed runs: %d (pass rate: %.4f)\n", doc.Evaluations.CompletedRuns, doc.Evaluations.PassRate)
			fmt.Fprintf(&b, "- Total cost (cents): %.4f\n", doc.Evaluations.TotalCostCents)
			if doc.Evaluations.LastRunAt != "" {
				fmt.Fprintf(&b, "- Last run: %s\n", doc.Evaluations.LastRunAt)
			}
			if doc.Evaluations.Note != "" {
				fmt.Fprintf(&b, "- Note: %s\n", doc.Evaluations.Note)
			}
			b.WriteString("\n")
		case "Deployment & Canary Policy":
			b.WriteString("## 4. Deployment & Canary Policy\n\n")
			if len(doc.Deployments) == 0 {
				b.WriteString("- No deployments recorded.\n\n")
				continue
			}
			for _, d := range doc.Deployments {
				fmt.Fprintf(&b, "- Deployment %s (%s): status=%s version=%d", d.ID, d.Environment, d.Status, d.Version)
				if d.Canary != nil {
					fmt.Fprintf(&b, "; canary v%d at %d%% (active: %t)", d.Canary.CanaryVersion, d.Canary.Weight, d.Canary.Active)
				}
				if d.Policy != nil {
					fmt.Fprintf(&b, "; policy min_pass_rate=%.4f min_canary_runs=%d", d.Policy.MinPassRate, d.Policy.MinCanaryRuns)
				}
				if d.Decision != nil {
					fmt.Fprintf(&b, "; decision=%s (%s) at %s", d.Decision.Action, d.Decision.Reason, d.Decision.DecidedAt)
				}
				b.WriteString("\n")
			}
			b.WriteString("\n")
		case "Tool Surface":
			b.WriteString("## 5. Tool Surface\n\n")
			fmt.Fprintf(&b, "- Mode: %s\n- Active agent keys: %d\n", doc.ToolSurface.Mode, doc.ToolSurface.AgentKeys)
			if len(doc.ToolSurface.Tools) > 0 {
				fmt.Fprintf(&b, "- Tools: %s\n", strings.Join(doc.ToolSurface.Tools, ", "))
			}
			if doc.ToolSurface.Note != "" {
				fmt.Fprintf(&b, "- Note: %s\n", doc.ToolSurface.Note)
			}
			b.WriteString("\n")
		case "Policy Bindings":
			b.WriteString("## 6. Policy Bindings\n\n")
			if len(doc.PolicyBindings) == 0 {
				b.WriteString("- No agent-scoped policies.\n\n")
				continue
			}
			for _, p := range doc.PolicyBindings {
				fmt.Fprintf(&b, "- %s \"%s\": effect=%s resource=%s actions=%s enabled=%t require_approval=%t\n",
					p.ID, p.Name, p.Effect, p.ResourceType, strings.Join(p.Actions, "|"), p.Enabled, p.RequireApproval)
			}
			b.WriteString("\n")
		case "Audit Posture":
			b.WriteString("## 7. Audit Posture\n\n")
			fmt.Fprintf(&b, "- Chain head: %s\n- Chain length: %d\n- Entries last 30 days: %d\n",
				orDash(doc.AuditPosture.ChainHead), doc.AuditPosture.ChainLength, doc.AuditPosture.EntriesLast30Days)
			if doc.AuditPosture.Note != "" {
				fmt.Fprintf(&b, "- Note: %s\n", doc.AuditPosture.Note)
			}
			b.WriteString("\n")
		}
	}
	return []byte(b.String())
}

// orDash renders empty strings as "-" in tables (keeps cells non-empty).
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
