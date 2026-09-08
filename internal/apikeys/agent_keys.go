package apikeys

// Agent-scoped API keys (wave-8, agent identity v1): non-human credentials
// bound to ONE agent inside ONE organization, with a per-key tool allow-list
// enforced at the runtime tool-execution seam.
//
// Why a separate key type: user API keys authenticate as OWNER (the
// platform-wide model, cmd/api/apikeys.go) and are minted by humans for
// humans. Agent keys authenticate as the AGENT principal — the subject of
// every claim is the agent, not a user — and carry a least-privilege tool
// allow-list, because non-human credentials are long-lived, get copied into
// configs/env vars, and have no human in the loop to notice misuse (the CSA
// NHI gap). Management of agent keys is therefore OWNER/ADMIN only, stricter
// than user keys, whose metadata list is MEMBER+.
//
// Key-generation scheme mirrors the user-key conventions EXACTLY (same
// 32-byte crypto/rand body, same hex encoding, same SHA-256 hash-at-rest,
// same 12-char display prefix) with one difference: the prefix namespace is
// "ag_" (agent) instead of "ak_" (user/API), so the two credential types are
// visually distinct while remaining scheme-consistent, and the auth
// middleware can dispatch on the prefix without a second lookup.
//
// Plaintext is returned EXACTLY ONCE, by CreateAgentKey; it is never stored
// (only the SHA-256 hash is persisted) and never carried on the AgentKey
// struct — stricter than the legacy user APIKey, which keeps Value in memory.
//
// ALLOW-LIST SEMANTICS (deliberate, keep consistent everywhere):
//   - nil ToolAllowList  = allow-list DISABLED: every tool is allowed. This
//     is the default for a key minted without one, so an unrestricted agent
//     key behaves like the platform's other credentials; it also round-trips
//     losslessly as SQL NULL (JSONB) and JSON null.
//   - empty ToolAllowList = allow-list ENABLED and empty: NO tool is allowed.
//     That is the explicit least-privilege lockdown a caller asks for with
//     "tool_allow_list": [] — an empty list must never degenerate into
//     "everything".
//
// The runtime seam is AuthorizeAgentKeyTool below: a pure, nil-safe function
// (nil claims = the caller is not an agent key = allowed) so the shared
// runtime keeps a one-line gate instead of importing auth/RBAC machinery.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"agentos/internal/agents"
)

// AgentKeySecretPrefix is the prefix namespace of agent-key secrets. User
// keys use "ak_"; the one-letter difference keeps the credential types
// visually distinct in configs, logs and dashboards.
const AgentKeySecretPrefix = "ag_"

// AgentKeyToolDeniedCode is the machine-readable denial code returned by the
// runtime tool-execution seam when an agent key requests a tool that is not
// in its allow-list.
const AgentKeyToolDeniedCode = "agent_key_tool_not_allowed"

// AgentKeyToolDeniedAction is the audit action written (best-effort) when a
// tool invocation is denied by an agent key's allow-list.
const AgentKeyToolDeniedAction = "agentkey.tool_denied"

// agentKeyDisplayPrefixLen mirrors the user-key display prefix length.
const agentKeyDisplayPrefixLen = 12

// AgentKey is one agent-scoped credential. RevokedAt/LastUsedAt follow the
// user-key store conventions (soft revoke tombstone, best-effort usage
// stamp); ExpiresAt makes the credential self-expiring (NHI hygiene).
type AgentKey struct {
	ID             string
	OrganizationID string
	AgentID        string
	Name           string
	KeyPrefix      string // display prefix ("ag_…"), authenticates nothing
	KeyHash        string // SHA-256 hex of the full secret
	ToolAllowList  []string
	ExpiresAt      *time.Time
	RevokedAt      *time.Time
	LastUsedAt     *time.Time
	CreatedAt      time.Time
}

// Revoked reports whether the key has been revoked.
func (k *AgentKey) Revoked() bool {
	return k != nil && k.RevokedAt != nil
}

// Expired reports whether the key's expiry (if any) has passed.
func (k *AgentKey) Expired(now time.Time) bool {
	return k != nil && k.ExpiresAt != nil && !now.Before(*k.ExpiresAt)
}

var (
	// ErrAgentKeyNotFound is returned when a key id does not resolve within
	// the (organization_id, agent_id) scope — unknown, foreign tenant, or
	// belonging to a different agent all collapse into this one error so
	// nothing about existence leaks across tenants.
	ErrAgentKeyNotFound = errors.New("agent key not found")
	// ErrAgentNotFound is returned when the agent existence check fails (the
	// agent is unknown or belongs to another organization).
	ErrAgentNotFound = errors.New("agent not found")
)

// AgentKeyCreateRequest is the payload of CreateAgentKey.
type AgentKeyCreateRequest struct {
	Name string
	// ToolAllowList: nil = allow-list disabled (all tools); empty = no tools;
	// named entries = exactly those tools. Entries are trimmed and de-duplicated.
	ToolAllowList []string
	// ExpiresAt is optional; a past timestamp is rejected.
	ExpiresAt *time.Time
}

// AgentKeyUpdate is the PATCH payload of UpdateAgentKey. Pointer fields are
// tri-state: nil = leave the field unchanged. For ToolAllowList the inner
// pointer distinguishes "disabled" (nil inner slice: allow-list OFF, all
// tools) from "empty" (non-nil inner slice, zero entries: no tools). For
// ExpiresAt the zero time clears the expiry.
type AgentKeyUpdate struct {
	Name          *string
	ToolAllowList *[]string
	ExpiresAt     *time.Time
}

// AgentExistenceChecker is the seam for validating that an agent belongs to
// an organization (mirrors how the evaluations service cross-checks agents
// through agents.Service.GetAgentCtx). *agents.Service satisfies it.
type AgentExistenceChecker interface {
	GetAgentCtx(ctx context.Context, orgID, agentID string) (*agents.Agent, error)
}

// AgentKeyStore persists agent keys. Lookup by key hash resolves the tenant
// from the credential itself (the hash is globally unique); every id-based
// operation is guarded by BOTH organization_id and agent_id.
type AgentKeyStore interface {
	// CreateAgentKey inserts the key row with its organization_id tenant scope.
	CreateAgentKey(ctx context.Context, key *AgentKey) error
	// GetAgentKeyByHash resolves a key by its SHA-256 hash (authentication path).
	GetAgentKeyByHash(ctx context.Context, hash string) (*AgentKey, error)
	// GetAgentKey resolves one key within one org + agent (tenant guard).
	GetAgentKey(ctx context.Context, orgID, agentID, keyID string) (*AgentKey, error)
	// UpdateAgentKey persists name/allow-list/expiry changes for one key
	// within one org + agent (tenant guard).
	UpdateAgentKey(ctx context.Context, key *AgentKey) error
	// RevokeAgentKey soft-revokes one key within one org + agent (tenant guard).
	RevokeAgentKey(ctx context.Context, orgID, agentID, keyID string, revokedAt time.Time) error
	// ListAgentKeys returns the keys of exactly one (organization, agent) pair.
	ListAgentKeys(ctx context.Context, orgID, agentID string) ([]*AgentKey, error)
	// TouchAgentKeyLastUsed stamps the best-effort usage marker.
	TouchAgentKeyLastUsed(ctx context.Context, id string, usedAt time.Time) error
}

// generateAgentKeySecret mirrors generateAPIKeySecret with the "ag_" namespace.
func generateAgentKeySecret() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return AgentKeySecretPrefix + fmt.Sprintf("%d", time.Now().UTC().UnixNano()) + "_fallback"
	}
	return AgentKeySecretPrefix + hex.EncodeToString(buf)
}

// IsAgentKeyValue reports whether a presented secret is in the agent-key
// namespace. The auth middleware dispatches on this so a user key and an
// agent key never cross-validate.
func IsAgentKeyValue(value string) bool {
	return strings.HasPrefix(value, AgentKeySecretPrefix)
}

// agentKeyDisplayPrefix mirrors the user-key prefix derivation.
func agentKeyDisplayPrefix(secret string) string {
	prefix := secret
	if len(prefix) > agentKeyDisplayPrefixLen {
		prefix = prefix[:agentKeyDisplayPrefixLen]
	}
	return prefix
}

// normalizeToolAllowList trims entries, rejects blanks, and de-duplicates
// preserving first-occurrence order. nil stays nil (allow-list disabled) and
// an empty input stays empty (no tools) — the nil-vs-empty semantics above.
func normalizeToolAllowList(list []string) ([]string, error) {
	if list == nil {
		return nil, nil
	}
	out := make([]string, 0, len(list))
	seen := make(map[string]struct{}, len(list))
	for _, tool := range list {
		tool = strings.TrimSpace(tool)
		if tool == "" {
			return nil, errors.New("tool allow-list entries cannot be empty")
		}
		if _, dup := seen[tool]; dup {
			continue
		}
		seen[tool] = struct{}{}
		out = append(out, tool)
	}
	return out, nil
}

// agentKeysInMemory returns the service's in-memory agent-key map, creating
// it lazily so constructors other than NewService keep working.
func (s *Service) agentKeysInMemory() map[string]*AgentKey {
	if s.agentKeys == nil {
		s.agentKeys = make(map[string]*AgentKey)
	}
	return s.agentKeys
}

// SetAgentChecker wires the agent-membership seam used by Create/Update to
// validate that the target agent belongs to the organization. Mirrors the
// SetStore/SetRevocationStore style. A nil checker DISABLES the check (the
// legacy/test path); the API always wires it in both persistence modes.
func (s *Service) SetAgentChecker(checker AgentExistenceChecker) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.agentChecker = checker
}

// SetAgentStore replaces the durable agent-key store. NewServiceWithStore
// already adopts stores that implement AgentKeyStore (the Postgres store
// implements both interfaces), so this is only needed for explicit wiring.
func (s *Service) SetAgentStore(store AgentKeyStore) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.agentStore = store
}

// checkAgentInOrg validates the agent-membership seam. A nil checker (legacy
// in-process tests) skips the check by contract; cmd/api always wires one.
func (s *Service) checkAgentInOrg(ctx context.Context, orgID, agentID string) error {
	s.mu.Lock()
	checker := s.agentChecker
	s.mu.Unlock()
	if checker == nil {
		return nil
	}
	if _, err := checker.GetAgentCtx(ctx, orgID, agentID); err != nil {
		return ErrAgentNotFound
	}
	return nil
}

// CreateAgentKey generates a new agent-scoped key and persists it for one
// tenant. The plaintext secret is returned EXACTLY ONCE (second return
// value) and is never stored server-side.
func (s *Service) CreateAgentKey(ctx context.Context, orgID, agentID string, req AgentKeyCreateRequest) (*AgentKey, string, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, "", errors.New("organization id is required")
	}
	if strings.TrimSpace(agentID) == "" {
		return nil, "", errors.New("agent id is required")
	}
	if strings.TrimSpace(req.Name) == "" {
		return nil, "", errors.New("key name is required")
	}
	if req.ExpiresAt != nil && !req.ExpiresAt.After(time.Now().UTC()) {
		return nil, "", errors.New("expires_at must be in the future")
	}
	allowList, err := normalizeToolAllowList(req.ToolAllowList)
	if err != nil {
		return nil, "", err
	}
	if err := s.checkAgentInOrg(ctx, orgID, agentID); err != nil {
		return nil, "", err
	}
	secret := generateAgentKeySecret()
	key := &AgentKey{
		ID:             uuid.NewString(),
		OrganizationID: orgID,
		AgentID:        agentID,
		Name:           req.Name,
		KeyPrefix:      agentKeyDisplayPrefix(secret),
		KeyHash:        hashAPIKey(secret),
		ToolAllowList:  allowList,
		ExpiresAt:      req.ExpiresAt,
		CreatedAt:      time.Now().UTC(),
	}
	if s.agentStoreSnapshot() != nil {
		if err := s.agentStoreSnapshot().CreateAgentKey(ctx, key); err != nil {
			return nil, "", err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.agentKeysInMemory()[key.ID] = key
	return key, secret, nil
}

// agentStoreSnapshot reads the durable agent-key store under the service lock.
func (s *Service) agentStoreSnapshot() AgentKeyStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.agentStore
}

// EnsureAgentInOrg validates the agent-membership seam for the management
// surface: the agent must exist within exactly one organization (404
// semantics for agents of other tenants, no existence leak). A nil checker
// (legacy in-process tests) is a no-op success by contract.
func (s *Service) EnsureAgentInOrg(ctx context.Context, orgID, agentID string) error {
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(agentID) == "" {
		return ErrAgentNotFound
	}
	return s.checkAgentInOrg(ctx, orgID, agentID)
}

// GetAgentKey resolves one key within one (organization, agent) scope.
func (s *Service) GetAgentKey(ctx context.Context, orgID, agentID, keyID string) (*AgentKey, error) {
	if strings.TrimSpace(keyID) == "" {
		return nil, ErrAgentKeyNotFound
	}
	if store := s.agentStoreSnapshot(); store != nil {
		return store.GetAgentKey(ctx, orgID, agentID, keyID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key, ok := s.agentKeysInMemory()[keyID]
	if !ok || key.OrganizationID != orgID || key.AgentID != agentID {
		return nil, ErrAgentKeyNotFound
	}
	return key, nil
}

// UpdateAgentKey applies a PATCH to one key within one (organization, agent)
// scope. Unknown/foreign ids surface as ErrAgentKeyNotFound (404 semantics,
// no existence leak).
func (s *Service) UpdateAgentKey(ctx context.Context, orgID, agentID, keyID string, update AgentKeyUpdate) (*AgentKey, error) {
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(agentID) == "" {
		return nil, ErrAgentKeyNotFound
	}
	if update.Name != nil && strings.TrimSpace(*update.Name) == "" {
		return nil, errors.New("key name is required")
	}
	if update.ExpiresAt != nil && !update.ExpiresAt.IsZero() && !update.ExpiresAt.After(time.Now().UTC()) {
		return nil, errors.New("expires_at must be in the future")
	}
	var allowList *[]string
	if update.ToolAllowList != nil {
		normalized, err := normalizeToolAllowList(*update.ToolAllowList)
		if err != nil {
			return nil, err
		}
		// Preserve the disabled-vs-empty distinction: normalizeToolAllowList
		// returns nil only for a nil input (disabled); an empty input stays
		// an empty non-nil slice.
		allowList = &normalized
	}

	if store := s.agentStoreSnapshot(); store != nil {
		key, err := store.GetAgentKey(ctx, orgID, agentID, keyID)
		if err != nil {
			return nil, err
		}
		applyAgentKeyUpdate(key, update, allowList)
		if err := store.UpdateAgentKey(ctx, key); err != nil {
			return nil, err
		}
		s.mu.Lock()
		s.agentKeysInMemory()[key.ID] = key
		s.mu.Unlock()
		return key, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	key, ok := s.agentKeysInMemory()[keyID]
	if !ok || key.OrganizationID != orgID || key.AgentID != agentID {
		return nil, ErrAgentKeyNotFound
	}
	applyAgentKeyUpdate(key, update, allowList)
	return key, nil
}

// applyAgentKeyUpdate mutates the key in place. Callers hold the relevant
// lock (service mutex for memory, none needed for a store-fetched row).
func applyAgentKeyUpdate(key *AgentKey, update AgentKeyUpdate, allowList *[]string) {
	if update.Name != nil {
		key.Name = *update.Name
	}
	if allowList != nil {
		key.ToolAllowList = *allowList
	}
	if update.ExpiresAt != nil {
		if update.ExpiresAt.IsZero() {
			key.ExpiresAt = nil // explicit clear
		} else {
			copied := update.ExpiresAt.UTC()
			key.ExpiresAt = &copied
		}
	}
}

// RevokeAgentKey soft-revokes one key within one (organization, agent) scope.
// Idempotent per the user-key convention: revoking an already-revoked key is
// a success; only unknown/foreign ids are ErrAgentKeyNotFound.
func (s *Service) RevokeAgentKey(ctx context.Context, orgID, agentID, keyID string) error {
	if strings.TrimSpace(keyID) == "" {
		return ErrAgentKeyNotFound
	}
	if store := s.agentStoreSnapshot(); store != nil {
		if err := store.RevokeAgentKey(ctx, orgID, agentID, keyID, time.Now().UTC()); err != nil {
			return err
		}
		s.mu.Lock()
		if key, ok := s.agentKeysInMemory()[keyID]; ok {
			now := time.Now().UTC()
			key.RevokedAt = &now
		}
		s.mu.Unlock()
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key, ok := s.agentKeysInMemory()[keyID]
	if !ok || key.OrganizationID != orgID || key.AgentID != agentID {
		return ErrAgentKeyNotFound
	}
	now := time.Now().UTC()
	key.RevokedAt = &now
	return nil
}

// ListAgentKeys returns the keys of exactly one (organization, agent) pair,
// newest first (the durable store orders by created_at DESC; the in-memory
// projection is sorted here so both modes honor one contract, ties broken by
// id for determinism).
func (s *Service) ListAgentKeys(ctx context.Context, orgID, agentID string) ([]*AgentKey, error) {
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(agentID) == "" {
		return nil, ErrAgentKeyNotFound
	}
	if store := s.agentStoreSnapshot(); store != nil {
		return store.ListAgentKeys(ctx, orgID, agentID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*AgentKey, 0)
	for _, key := range s.agentKeysInMemory() {
		if key.OrganizationID == orgID && key.AgentID == agentID {
			out = append(out, key)
		}
	}
	sortAgentKeys(out)
	return out, nil
}

// ValidateAgentKeyCtx resolves an agent key by the hash of the presented
// secret, enforcing the lifecycle at AUTH TIME: revoked keys and expired
// keys never authenticate. On success the best-effort LastUsedAt stamp is
// written (NHI hygiene: dormant keys must be visible).
func (s *Service) ValidateAgentKeyCtx(ctx context.Context, value string) (*AgentKey, bool) {
	if strings.TrimSpace(value) == "" {
		return nil, false
	}
	hash := hashAPIKey(value)
	now := time.Now().UTC()
	if store := s.agentStoreSnapshot(); store != nil {
		key, err := store.GetAgentKeyByHash(ctx, hash)
		if err != nil || key == nil {
			return nil, false
		}
		// Defense in depth: the store select filters revoked rows, but the
		// lifecycle contract is enforced HERE regardless of store behavior.
		if key.Revoked() || key.Expired(now) {
			return nil, false
		}
		_ = store.TouchAgentKeyLastUsed(ctx, key.ID, now)
		return key, true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, key := range s.agentKeysInMemory() {
		if key.RevokedAt == nil && key.KeyHash == hash {
			if key.Expired(now) {
				return nil, false
			}
			stamped := now
			key.LastUsedAt = &stamped
			return key, true
		}
	}
	return nil, false
}

// sortAgentKeys orders newest-first with an id tiebreak (list contract).
func sortAgentKeys(keys []*AgentKey) {
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0; j-- {
			a, b := keys[j-1], keys[j]
			if a.CreatedAt.After(b.CreatedAt) || (a.CreatedAt.Equal(b.CreatedAt) && a.ID < b.ID) {
				continue
			}
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
}

// --- least-privilege tool-execution seam -----------------------------------

// AgentKeyClaims is the non-human identity an agent key authenticates with:
// the subject is the AGENT, the key is the credential, and the allow-list is
// the least-privilege boundary enforced at the runtime tool seam. It is a
// plain struct in this package (auth imports apikeys, so apikeys cannot
// import auth) and is stamped into request/task contexts below.
type AgentKeyClaims struct {
	KeyID          string
	AgentID        string
	OrganizationID string
	ToolAllowList  []string
}

type agentKeyClaimsContextKey struct{}

// WithAgentKeyClaims stamps the agent-key identity onto a context. A nil
// claims is a no-op (the context then reads as a non-agent-key caller).
func WithAgentKeyClaims(ctx context.Context, claims *AgentKeyClaims) context.Context {
	if claims == nil {
		return ctx
	}
	return context.WithValue(ctx, agentKeyClaimsContextKey{}, claims)
}

// AgentKeyClaimsFromContext resolves the agent-key identity from ctx. It
// returns nil for every non-agent-key caller (JWT user, user API key,
// unauthenticated) — the seam's nil-safety contract.
func AgentKeyClaimsFromContext(ctx context.Context) *AgentKeyClaims {
	if ctx == nil {
		return nil
	}
	claims, _ := ctx.Value(agentKeyClaimsContextKey{}).(*AgentKeyClaims)
	return claims
}

// AgentKeyClaimsFromPayload decodes the agent-key identity the create-run
// handler records on the task payload (key "agent_key"). Returns nil when
// the payload carries no agent-key identity or a malformed one — the worker
// context is trusted-internal (API → queue), so malformed identity degrades
// to "not an agent-key caller" rather than a hard failure.
func AgentKeyClaimsFromPayload(payload map[string]any) *AgentKeyClaims {
	if payload == nil {
		return nil
	}
	raw, ok := payload["agent_key"].(map[string]any)
	if !ok || raw == nil {
		return nil
	}
	claims := &AgentKeyClaims{}
	claims.KeyID, _ = raw["id"].(string)
	claims.AgentID, _ = raw["agent_id"].(string)
	claims.OrganizationID, _ = raw["organization_id"].(string)
	if list, ok := raw["tool_allow_list"].([]any); ok {
		allowList := make([]string, 0, len(list))
		for _, entry := range list {
			if tool, ok := entry.(string); ok {
				allowList = append(allowList, tool)
			}
		}
		claims.ToolAllowList = allowList
	}
	// No key id → not a usable agent-key identity.
	if strings.TrimSpace(claims.KeyID) == "" {
		return nil
	}
	return claims
}

// AgentKeyToolError marks a tool invocation that was blocked by an agent
// key's allow-list BEFORE execution. Error() begins with the machine-readable
// AgentKeyToolDeniedCode so run steps, API responses and audit greps share
// one vocabulary.
type AgentKeyToolError struct {
	KeyID   string
	AgentID string
	Tool    string
}

// Error implements error.
func (e *AgentKeyToolError) Error() string {
	return fmt.Sprintf("%s: tool %q is not in the allow-list of agent key %s", AgentKeyToolDeniedCode, e.Tool, e.KeyID)
}

// AuthorizeAgentKeyTool is the least-privilege seam enforced at tool
// execution time (runtime Runner). Nil-safe by contract:
//
//   - claims == nil            → the caller is not an agent key → allowed
//   - ToolAllowList == nil     → allow-list disabled → every tool allowed
//   - ToolAllowList empty      → allow-list enabled and empty → nothing allowed
//   - tool name present        → allowed
//
// Any other case returns *AgentKeyToolError carrying the machine-readable
// agent_key_tool_not_allowed code.
func AuthorizeAgentKeyTool(claims *AgentKeyClaims, toolName string) error {
	if claims == nil || claims.ToolAllowList == nil {
		return nil
	}
	for _, allowed := range claims.ToolAllowList {
		if allowed == toolName {
			return nil
		}
	}
	return &AgentKeyToolError{KeyID: claims.KeyID, AgentID: claims.AgentID, Tool: toolName}
}

// AgentKeyClaimsPayload renders the payload sub-object the create-run handler
// stamps into the queue task (AgentKeyClaimsFromPayload consumes it). The
// tool_allow_list entry marshals explicitly so the nil-vs-empty distinction
// survives the queue hop: a nil list renders as JSON null (allow-list
// disabled), an empty list as [] (no tools).
func AgentKeyClaimsPayload(claims *AgentKeyClaims) map[string]any {
	if claims == nil {
		return nil
	}
	var allowList any
	if claims.ToolAllowList != nil {
		allowList = claims.ToolAllowList
	}
	return map[string]any{
		"id":              claims.KeyID,
		"agent_id":        claims.AgentID,
		"organization_id": claims.OrganizationID,
		"tool_allow_list": allowList,
	}
}
