package agents

// portability.go — issue #122: agent export / import / clone (portability).
//
// ExportAgent renders one agent of one tenant as a portable ExportBundle:
// configuration only, deterministic field order, schema_version "1".
// Identity and credential fields (agent id, organization id, version ids,
// timestamps, API keys, secret VALUES) are structurally absent — the bundle
// carries configuration and tool NAMES (catalog references), never secret
// material, so a leaked bundle leaks nothing but a prompt.
//
// ImportAgent validates a bundle and creates a NEW agent in the caller's
// tenant: ids, organization and created_at are always reassigned server-side
// (the client is never trusted — unknown/extra bundle keys, including ids and
// org claims smuggled into the payload, are dropped by the typed decode).
// Validation is fail-closed with typed *PortabilityError values (wrapping
// ErrInvalidBundle) that the HTTP layer renders as 422 VALIDATION_ERROR:
//   - bundle schema_version must be "1" (forward compatible rejection);
//   - name/instructions/model must be present;
//   - instructions are size-capped (MaxInstructionsBytes);
//   - the model must be a KNOWN model id (models.LookupPricing catalog,
//     case-insensitive, vendor/model suffix matching — the same table that
//     meters runs, so "known" means "the platform can run and bill it");
//   - every tool name must exist in the tools registry catalog
//     (tools.DefaultRegistry — "calculator", "http_request").
//
// CloneAgent deep-copies one agent WITHIN the caller's organization: new id,
// name suffix "-copy", status reset to DRAFT (a clone is a fresh draft, never
// a running twin), fresh timestamps and a fresh v1 version. A cross-org clone
// attempt is rejected with ErrAgentNotFound (no existence leak), the same
// convention as every other tenant-guarded agents surface.
//
// Both Import and Clone go through CreateAgentCtx, which persists through the
// Store when one is configured (Postgres mode) and mirrors into the in-memory
// cache otherwise (dual-mode store pattern; org-guarded SQL in store.go).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"agentos/internal/models"
	"agentos/internal/tools"
)

// BundleSchemaVersion is the only export bundle schema this service writes
// and accepts. Bump on a breaking bundle change; ImportAgent rejects other
// versions with a typed error instead of guessing.
const BundleSchemaVersion = "1"

// MaxInstructionsBytes caps the instructions field of an imported bundle
// (64 KiB). Exported bundles always satisfy the cap (they were accepted on
// creation/update), so the cap only ever rejects oversized hand-built
// payloads. Bytes, not runes: the wire limit is what matters.
const MaxInstructionsBytes = 64 << 10

// ExportedAgent is the portable projection of one agent's configuration —
// exactly the fields another installation needs to re-create the agent, and
// nothing else. Field order IS the wire order (encoding/json emits struct
// fields in declaration order), which keeps exports byte-deterministic.
type ExportedAgent struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	Instructions string `json:"instructions"`
	Model        string `json:"model"`
	// Tools lists tool registry NAMES (catalog references) — never tool
	// credentials or connection material. The current agent row does not
	// persist per-agent tool bindings (the runtime resolves tools from the
	// registry), so exports emit an empty list today and import VALIDATES
	// the names against the catalog without persisting them; the field keeps
	// bundles forward-compatible with richer agents.
	Tools []string `json:"tools"`
}

// ExportBundle is the versioned export/import envelope. Structurally absent
// by design: agent id, organization id, status, version ids and timestamps —
// import always reassigns identity and resets status to DRAFT.
type ExportBundle struct {
	SchemaVersion string        `json:"schema_version"`
	ExportedAt    string        `json:"exported_at"` // RFC3339 UTC, informational; import ignores it
	Agent         ExportedAgent `json:"agent"`
}

// Portability error codes surfaced by *PortabilityError (embedded in the
// 422 VALIDATION_ERROR message by the HTTP layer).
const (
	PortabilityCodeBundleRequired       = "BUNDLE_REQUIRED"
	PortabilityCodeSchemaVersion        = "BUNDLE_SCHEMA_VERSION"
	PortabilityCodeNameRequired         = "NAME_REQUIRED"
	PortabilityCodeInstructionsRequired = "INSTRUCTIONS_REQUIRED"
	PortabilityCodeInstructionsTooLarge = "INSTRUCTIONS_TOO_LARGE"
	PortabilityCodeModelRequired        = "MODEL_REQUIRED"
	PortabilityCodeUnknownModel         = "UNKNOWN_MODEL"
	PortabilityCodeUnknownTool          = "UNKNOWN_TOOL"
)

// ErrInvalidBundle is the sentinel wrapped by every *PortabilityError so
// callers can classify import rejections with errors.Is.
var ErrInvalidBundle = errors.New("invalid agent export bundle")

// PortabilityError is the typed import-validation rejection. Code is a
// stable machine code (PortabilityCode* constants), Field names the offending
// bundle location and Detail carries the human explanation.
type PortabilityError struct {
	Code   string
	Field  string
	Detail string
}

// Error implements error with the code and detail embedded (the HTTP layer
// forwards this message inside the 422 VALIDATION_ERROR envelope).
func (e *PortabilityError) Error() string {
	if e.Field == "" {
		return fmt.Sprintf("invalid agent export bundle (%s): %s", e.Code, e.Detail)
	}
	return fmt.Sprintf("invalid agent export bundle (%s): %s: %s", e.Code, e.Field, e.Detail)
}

// Unwrap exposes the ErrInvalidBundle sentinel for errors.Is.
func (e *PortabilityError) Unwrap() error { return ErrInvalidBundle }

func portabilityError(code, field, detail string) *PortabilityError {
	return &PortabilityError{Code: code, Field: field, Detail: detail}
}

// knownToolNames returns the platform tool catalog (sorted registry names).
func knownToolNames() []string {
	return tools.DefaultRegistry().Names()
}

// ValidateBundle checks one bundle against the import contract WITHOUT
// persisting anything (shared by ImportAgent and tests). All rejections are
// typed *PortabilityError values wrapping ErrInvalidBundle.
func ValidateBundle(bundle *ExportBundle) error {
	if bundle == nil {
		return portabilityError(PortabilityCodeBundleRequired, "", "bundle is required")
	}
	if strings.TrimSpace(bundle.SchemaVersion) != BundleSchemaVersion {
		return portabilityError(PortabilityCodeSchemaVersion, "schema_version",
			fmt.Sprintf("unsupported schema_version %q; supported: %q", bundle.SchemaVersion, BundleSchemaVersion))
	}
	if strings.TrimSpace(bundle.Agent.Name) == "" {
		return portabilityError(PortabilityCodeNameRequired, "agent.name", "agent name is required")
	}
	if strings.TrimSpace(bundle.Agent.Instructions) == "" {
		return portabilityError(PortabilityCodeInstructionsRequired, "agent.instructions", "instructions are required")
	}
	if len(bundle.Agent.Instructions) > MaxInstructionsBytes {
		return portabilityError(PortabilityCodeInstructionsTooLarge, "agent.instructions",
			fmt.Sprintf("instructions are %d bytes; the cap is %d bytes", len(bundle.Agent.Instructions), MaxInstructionsBytes))
	}
	if strings.TrimSpace(bundle.Agent.Model) == "" {
		return portabilityError(PortabilityCodeModelRequired, "agent.model", "model is required")
	}
	// Known-model check: the pricing catalog doubles as the platform's model
	// registry (case-insensitive, vendor/model suffix matching), so an
	// imported agent can always be metered and run.
	if _, ok := models.LookupPricing(bundle.Agent.Model); !ok {
		return portabilityError(PortabilityCodeUnknownModel, "agent.model",
			fmt.Sprintf("model %q is not a known model id (resolved case-insensitively from the platform model catalog)", bundle.Agent.Model))
	}
	// Every referenced tool must exist in the catalog (fail-closed: a bundle
	// can never smuggle unknown tool names past import).
	catalog := knownToolNames()
	for _, toolName := range bundle.Agent.Tools {
		trimmed := strings.TrimSpace(toolName)
		known := false
		for _, candidate := range catalog {
			if trimmed == candidate {
				known = true
				break
			}
		}
		if !known {
			return portabilityError(PortabilityCodeUnknownTool, "agent.tools",
				fmt.Sprintf("tool %q is not in the platform tool catalog (known: %s)", toolName, strings.Join(catalog, ", ")))
		}
	}
	return nil
}

// ExportAgent renders one agent of one tenant as a portable ExportBundle.
//
// Semantics:
//   - Tenant guard: the lookup is organization-scoped; foreign/unknown
//     agents surface as ErrAgentNotFound with no existence leak.
//   - The bundle is configuration-only: ids, org, status, version ids,
//     timestamps and credential material are structurally absent (see
//     ExportBundle). Tools are emitted as an empty (non-nil) sorted-capable
//     list so the JSON stays deterministic.
//   - Field order is deterministic (struct declaration order); exported_at
//     is the only wall-clock field and is informational.
func (s *Service) ExportAgent(ctx context.Context, orgID, agentID string) (*ExportBundle, error) {
	if s == nil {
		return nil, ErrAgentNotFound
	}
	// Tenant guard: WHERE id = ? AND organization_id = ? (store mode) or the
	// in-memory org check — foreign/unknown agents are ErrAgentNotFound.
	agent, err := s.GetAgentCtx(ctx, orgID, agentID)
	if err != nil {
		return nil, ErrAgentNotFound
	}
	return &ExportBundle{
		SchemaVersion: BundleSchemaVersion,
		ExportedAt:    time.Now().UTC().Format(time.RFC3339),
		Agent: ExportedAgent{
			Name:         agent.Name,
			Description:  agent.Description,
			Instructions: agent.Instructions,
			Model:        agent.Model,
			Tools:        []string{},
		},
	}, nil
}

// ImportAgent validates one bundle and creates a NEW agent in the caller's
// tenant (orgID comes exclusively from the authenticated caller — never from
// the bundle).
//
// Semantics:
//   - Validation first (ValidateBundle): any rejection is a typed
//     *PortabilityError (422-able) and leaves NO agent behind.
//   - Identity is reassigned server-side: a fresh uuid, the CALLER's
//     organization, created_at/updated_at = now, status reset to DRAFT and a
//     fresh initial v1 version. Nothing from the bundle's identity-shaped
//     fields is trusted (they are not even part of the typed bundle; extra
//     payload keys are dropped at decode).
//   - Import is an in-org operation (the caller's tenant); exporting from
//     org A and importing into org B is exactly how agents move tenants.
func (s *Service) ImportAgent(ctx context.Context, orgID string, bundle *ExportBundle) (*Agent, error) {
	if s == nil {
		return nil, ErrAgentNotFound
	}
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrAgentNotFound
	}
	if err := ValidateBundle(bundle); err != nil {
		return nil, err
	}
	// CreateAgentCtx assigns id/org/timestamps and the initial v1 version.
	return s.CreateAgentCtx(ctx, orgID,
		strings.TrimSpace(bundle.Agent.Name),
		bundle.Agent.Description,
		strings.TrimSpace(bundle.Agent.Instructions),
		strings.TrimSpace(bundle.Agent.Model))
}

// CloneAgent deep-copies one agent within the SAME organization: new id,
// fresh timestamps, name suffixed "-copy", status reset to DRAFT and a fresh
// v1 version carrying the copied configuration.
//
// Semantics:
//   - Same-org only: the source is resolved through the org-scoped lookup,
//     so a cross-org clone attempt (caller from another tenant) is rejected
//     with ErrAgentNotFound — no existence leak across tenants.
//   - The source is never mutated: an ACTIVE (or archived) source stays
//     exactly as it was; the clone starts life as a DRAFT draft-copy.
//   - Config copied: name (+ "-copy"), description, instructions, model.
//     Status, ids, versions and timestamps are NOT copied (fresh identity).
func (s *Service) CloneAgent(ctx context.Context, orgID, agentID string) (*Agent, error) {
	if s == nil {
		return nil, ErrAgentNotFound
	}
	source, err := s.GetAgentCtx(ctx, orgID, agentID)
	if err != nil {
		// Cross-org / unknown source: same rejection, no existence leak.
		return nil, ErrAgentNotFound
	}
	// CreateAgentCtx resets status to DRAFT and mints the fresh identity +
	// v1 version — the deep copy of the configuration.
	return s.CreateAgentCtx(ctx, orgID,
		source.Name+"-copy",
		source.Description,
		source.Instructions,
		source.Model)
}
