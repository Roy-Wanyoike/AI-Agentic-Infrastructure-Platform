package models

// gateway.go — issue #139 (wave-9 9-d): gateway VIRTUAL KEYS.
//
// A virtual key (vk_…) is an org-scoped credential that gates MODEL SPEND:
// it carries (a) a model allow-list and (b) an enforced monthly spend cap,
// and the worker's model provider is wrapped in a GuardedProvider bound to
// one key so every completion is authorized BEFORE it hits the provider and
// its actual token cost is recorded into a per-(key, month UTC) ledger.
// Denial semantics mirror the budget gates: model_not_allowed -> 403,
// spend_limit_exceeded -> 402.
//
// Dual-mode per the platform convention (mirrors internal/mcp registry):
//   - NewGatewayService(): in-memory mode, zero infrastructure.
//   - NewGatewayServiceWithStore(NewGatewayPostgresStore(db)): Postgres mode
//     (gateway_store.go + migration 030_virtual_keys.sql).
//
// ALLOW-LIST SEMANTICS (contractual nil-vs-empty, mirrors the agent-key
// tool allow-list of internal/apikeys):
//
//      nil    -> allow-list DISABLED: every model allowed
//      []     -> allow-list ENABLED and empty: NO model allowed
//      [...]  -> exactly these models (case-insensitive; a "vendor/model"
//                id also matches its bare suffix, consistent with
//                LookupPricing)
//
// SPEND CAPS: MonthlyLimitCents == 0 means UNLIMITED (documented sentinel,
// same as budgets/limits across the billing stack). Money is USD CENTS
// (fractional float64 in the ledger) — the single money unit of the billing
// stack. Ledger months are calendar months in UTC.
//
// ENFORCEMENT POSTURE: Authorize fails CLOSED on a ledger read error (a
// spend cap that silently allows on a metering outage is a compliance hole;
// the API-side budget gate made the same choice for NEW work). RecordSpend
// after a successful completion is best-effort (delivered work is never
// destroyed by a metering write failure — the same trade the worker budget
// gate documents); the projected check re-reads the ledger on every call so
// enforcement recovers as soon as the store does.
//
// SECRETS: the plaintext key exists ONCE (create response). Storage keeps a
// SHA-256 hex hash (the same scheme as internal/apikeys) plus a 12-char
// display prefix that authenticates nothing. List/Get responses are
// redacted (no hashes, no plaintext).

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"agentos/internal/audit"

	"github.com/google/uuid"
)

// GatewayKeyEnvVar names the environment variable that binds a worker (or
// any provider consumer) to one virtual key: when it holds a vk_ plaintext,
// the process wraps its model provider in a GuardedProvider (see
// cmd/worker/main.go wiring).
const GatewayKeyEnvVar = "AGENTOS_GATEWAY_KEY"

// VirtualKeyPrefix is the plaintext credential prefix (vk_...), mirroring
// the ak_ (agent keys) and plain (user API keys) conventions.
const VirtualKeyPrefix = "vk_"

// Virtual key lifecycle status.
const (
	VirtualKeyStatusActive  = "active"
	VirtualKeyStatusRevoked = "revoked"
)

// VirtualKey is one gateway virtual key. KeyHash is the SHA-256 hex of the
// full plaintext secret; KeyPrefix is a display prefix that authenticates
// nothing. Models follows the nil-vs-empty allow-list contract above.
type VirtualKey struct {
	ID                string
	OrganizationID    string
	Label             string
	KeyPrefix         string
	KeyHash           string
	CreatedBy         string
	Models            []string
	MonthlyLimitCents int64 // 0 = unlimited
	Status            string
	ExpiresAt         *time.Time
	RevokedAt         *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// Revoked reports whether the key has been revoked.
func (k *VirtualKey) Revoked() bool {
	return k != nil && k.Status == VirtualKeyStatusRevoked
}

// Expired reports whether the key's expiry (if any) has passed at now.
func (k *VirtualKey) Expired(now time.Time) bool {
	return k != nil && k.ExpiresAt != nil && !now.Before(*k.ExpiresAt)
}

// redacted returns a copy of the key safe for management responses: the
// hash (and revocation internals) are dropped, everything else round-trips.
func (k *VirtualKey) redacted() *VirtualKey {
	if k == nil {
		return nil
	}
	out := *k
	out.KeyHash = ""
	return &out
}

var (
	// ErrVirtualKeyNotFound is returned when a key id does not resolve
	// within the (organization, id) scope, or a plaintext does not resolve
	// to an ACTIVE key — unknown, foreign tenant, revoked and expired all
	// collapse into one error on the auth path so nothing leaks.
	ErrVirtualKeyNotFound = errors.New("models: gateway virtual key not found")
	// ErrVirtualKeyRevoked / ErrVirtualKeyExpired are returned by Authorize
	// (the org is already known there) for lifecycle-rejected keys.
	ErrVirtualKeyRevoked = errors.New("models: gateway virtual key revoked")
	ErrVirtualKeyExpired = errors.New("models: gateway virtual key expired")
	// ErrInvalidVirtualKey rejects malformed create/update input.
	ErrInvalidVirtualKey = errors.New("models: invalid virtual key")
	// ErrModelNotAllowed is the 403-semantics sentinel (see ModelNotAllowedError).
	ErrModelNotAllowed = errors.New("models: model not allowed by virtual key")
	// ErrSpendLimitExceeded is the 402-semantics sentinel (see SpendLimitError).
	ErrSpendLimitExceeded = errors.New("models: monthly spend limit exceeded")
)

// ModelNotAllowedError carries the allow-list denial details behind
// ErrModelNotAllowed (errors.Is matches the sentinel, errors.As the details).
type ModelNotAllowedError struct {
	KeyID string
	Model string
}

func (e *ModelNotAllowedError) Error() string {
	return fmt.Sprintf("model %q is not allowed by virtual key %s (model_not_allowed)", e.Model, e.KeyID)
}

// Is makes errors.Is(err, ErrModelNotAllowed) true for the detail type.
func (e *ModelNotAllowedError) Is(target error) bool { return target == ErrModelNotAllowed }

// SpendLimitError carries the 402 denial details behind
// ErrSpendLimitExceeded: the key, its monthly limit, the month-to-date used
// cents and the projected (used + estimate) cents that crossed it.
type SpendLimitError struct {
	KeyID          string
	LimitCents     float64
	UsedCents      float64
	ProjectedCents float64
}

func (e *SpendLimitError) Error() string {
	return fmt.Sprintf(
		"monthly spend limit exceeded for virtual key %s: limit %.2f cents, used %.2f cents, projected %.2f cents for the current calendar month (spend_limit_exceeded); raise the limit or wait for the month to reset",
		e.KeyID, e.LimitCents, e.UsedCents, e.ProjectedCents)
}

// Is makes errors.Is(err, ErrSpendLimitExceeded) true for the detail type.
func (e *SpendLimitError) Is(target error) bool { return target == ErrSpendLimitExceeded }

// Auditor is the narrow audit seam for gateway lifecycle + denial rows. It
// is satisfied structurally by *audit.Service (its LogCtx signature
// matches), mirroring the Auditor seam of internal/mcp — this package stays
// free of the audit store.
type Auditor interface {
	LogCtx(ctx context.Context, actor, action, organizationID, resource string, metadata map[string]any) (*audit.Entry, error)
}

// Gateway audit actions (spend_recorded is opt-in via SetSpendAudit to keep
// per-completion volume out of the trail by default).
const (
	AuditGatewayKeyCreated      = "gateway.key_created"
	AuditGatewayKeyRevoked      = "gateway.key_revoked"
	AuditGatewaySpendDenied     = "gateway.spend_denied"
	AuditGatewayModelNotAllowed = "gateway.model_not_allowed"
	AuditGatewaySpendRecorded   = "gateway.spend_recorded"
)

// VirtualKeyInput is the create payload. Models: nil = ALL models allowed;
// empty non-nil = NONE; else the exact allow-list.
type VirtualKeyInput struct {
	Label             string
	Models            []string
	MonthlyLimitCents int64
	ExpiresAt         *time.Time
}

// VirtualKeyUpdate is the PATCH payload; nil fields are left unchanged.
// Models is a pointer-to-slice so all three allow-list states are
// addressable: nil pointer = unchanged; pointer to nil slice = ALL models;
// pointer to empty slice = NONE; pointer to populated slice = that list.
type VirtualKeyUpdate struct {
	Label             *string
	Models            *[]string
	MonthlyLimitCents *int64
	// ExpiresAt: nil = unchanged; non-nil sets the expiry (a pointer to the
	// zero time clears it).
	ExpiresAt *time.Time
}

// GatewayStore persists virtual keys + the spend ledger. Every
// tenant-facing query is guarded by organization_id; the by-hash lookup is
// the authentication path (the hash resolves the tenant from the credential
// itself, like the api_keys convention).
type GatewayStore interface {
	// CreateVirtualKey inserts the key row with its organization_id scope.
	CreateVirtualKey(ctx context.Context, key *VirtualKey) error
	// GetVirtualKeyByHash resolves a key by SHA-256 hash (authentication path).
	GetVirtualKeyByHash(ctx context.Context, hash string) (*VirtualKey, error)
	// GetVirtualKey fetches one key strictly within one tenant.
	GetVirtualKey(ctx context.Context, orgID, id string) (*VirtualKey, error)
	// ListVirtualKeys returns the tenant's keys (created_at ASC, id ASC).
	ListVirtualKeys(ctx context.Context, orgID string) ([]*VirtualKey, error)
	// UpdateVirtualKey applies a full mutable-field update within one tenant.
	UpdateVirtualKey(ctx context.Context, key *VirtualKey) error
	// RevokeVirtualKey flips status to revoked within one tenant.
	RevokeVirtualKey(ctx context.Context, orgID, id string, revokedAt time.Time) error
	// RecordSpend appends one ledger row (append-only, org-guarded).
	RecordSpend(ctx context.Context, orgID, keyID, model string, costCents float64, at time.Time) error
	// SpendForMonth sums the ledger for one (org, key, month UTC) bucket.
	SpendForMonth(ctx context.Context, orgID, keyID, month string) (float64, error)
}

// ---------------------------------------------------------------------------
// Service (dual-mode)
// ---------------------------------------------------------------------------

// spendBucket is the in-memory ledger coordinate: one (org, key, month UTC,
// model) cell.
type spendBucket struct {
	orgID string
	keyID string
	month string
	model string
}

// GatewayService implements the virtual-key lifecycle, the auth-time
// resolution, the spend ledger and the enforcement decision. It is safe for
// concurrent use.
type GatewayService struct {
	mu     sync.RWMutex
	keys   map[string]*VirtualKey
	byHash map[string]*VirtualKey
	spend  map[spendBucket]float64

	store      GatewayStore
	auditor    Auditor
	auditSpend bool
}

// NewGatewayService returns the in-memory (zero-infrastructure) service.
func NewGatewayService() *GatewayService {
	return &GatewayService{
		keys:   make(map[string]*VirtualKey),
		byHash: make(map[string]*VirtualKey),
		spend:  make(map[spendBucket]float64),
	}
}

// NewGatewayServiceWithStore returns a service backed by a durable store;
// the store is the source of truth and the in-memory maps act as a
// write-through cache (mirrors NewServiceWithStore in internal/apikeys).
func NewGatewayServiceWithStore(store GatewayStore) *GatewayService {
	s := NewGatewayService()
	s.store = store
	return s
}

// SetAuditor wires (or replaces) the lifecycle/denial audit seam. Nil-safe.
func (s *GatewayService) SetAuditor(a Auditor) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auditor = a
}

// SetSpendAudit enables the optional gateway.spend_recorded audit row on
// every ledger write (default OFF to keep per-completion volume out of the
// trail; lifecycle + denial rows are always emitted). Nil-safe.
func (s *GatewayService) SetSpendAudit(enabled bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auditSpend = enabled
}

// generateVirtualKeySecret mints a vk_ plaintext (32 random bytes, hex),
// mirroring the apikeys secret generator.
func generateVirtualKeySecret() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		// Unreachable in practice; fail deterministic rather than empty.
		return VirtualKeyPrefix + fmt.Sprintf("%d_fallback", time.Now().UTC().UnixNano())
	}
	return VirtualKeyPrefix + hex.EncodeToString(buf)
}

// hashVirtualKey is the at-rest scheme: SHA-256 hex of the full plaintext —
// the SAME scheme internal/apikeys uses for user and agent keys (the
// function is unexported there; this is the equivalent, per the issue's
// mirror-the-convention instruction).
func hashVirtualKey(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// normalizeModels validates the allow-list input: trims entries, rejects
// empty/whitespace entries, dedupes preserving first-seen order, and
// preserves the nil-vs-empty contract (nil in -> nil out; empty in -> empty
// non-nil out).
func normalizeModels(in []string) ([]string, error) {
	if in == nil {
		return nil, nil
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, m := range in {
		trimmed := strings.ToLower(strings.TrimSpace(m))
		if trimmed == "" {
			return nil, fmt.Errorf("%w: model allow-list entries must be non-empty", ErrInvalidVirtualKey)
		}
		if _, dup := seen[trimmed]; dup {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	return out, nil
}

// modelAllowed matches a model against the allow-list: case-insensitive
// exact id, and a "vendor/model" id also matches a listed bare suffix —
// the same matching LookupPricing applies, so an allow-list entry
// "gpt-4o" admits "openai/gpt-4o" requests and vice versa.
func modelAllowed(list []string, model string) bool {
	id := strings.ToLower(strings.TrimSpace(model))
	for _, listed := range list {
		l := strings.ToLower(strings.TrimSpace(listed))
		if l == "" {
			continue
		}
		if id == l {
			return true
		}
		if idx := strings.LastIndex(id, "/"); idx >= 0 && idx+1 < len(id) && id[idx+1:] == l {
			return true
		}
		if idx := strings.LastIndex(l, "/"); idx >= 0 && idx+1 < len(l) && l[idx+1:] == id {
			return true
		}
	}
	return false
}

// monthKey buckets a timestamp into its calendar month in UTC ("YYYY-MM").
func monthKey(at time.Time) string {
	return at.UTC().Format("2006-01")
}

// CreateVirtualKey mints one key: generates the plaintext (returned ONCE),
// persists the hash and caches the row. The returned *VirtualKey is
// redacted (no hash). Lifecycle is audited (gateway.key_created, best
// effort — audit never fails a create).
func (s *GatewayService) CreateVirtualKey(ctx context.Context, orgID, createdBy string, in VirtualKeyInput) (*VirtualKey, string, error) {
	if s == nil {
		return nil, "", ErrInvalidVirtualKey
	}
	if strings.TrimSpace(orgID) == "" {
		return nil, "", fmt.Errorf("%w: organization id is required", ErrInvalidVirtualKey)
	}
	if strings.TrimSpace(in.Label) == "" {
		return nil, "", fmt.Errorf("%w: label is required", ErrInvalidVirtualKey)
	}
	if in.MonthlyLimitCents < 0 {
		return nil, "", fmt.Errorf("%w: monthly_limit_cents must be >= 0", ErrInvalidVirtualKey)
	}
	modelsAllowed, err := normalizeModels(in.Models)
	if err != nil {
		return nil, "", err
	}
	if in.ExpiresAt != nil && in.ExpiresAt.IsZero() {
		return nil, "", fmt.Errorf("%w: expires_at must be a real timestamp", ErrInvalidVirtualKey)
	}

	plaintext := generateVirtualKeySecret()
	prefix := plaintext
	if len(prefix) > 12 {
		prefix = prefix[:12]
	}
	now := time.Now().UTC()
	key := &VirtualKey{
		ID:                uuid.NewString(),
		OrganizationID:    orgID,
		Label:             strings.TrimSpace(in.Label),
		KeyPrefix:         prefix,
		KeyHash:           hashVirtualKey(plaintext),
		CreatedBy:         createdBy,
		Models:            modelsAllowed,
		MonthlyLimitCents: in.MonthlyLimitCents,
		Status:            VirtualKeyStatusActive,
		ExpiresAt:         in.ExpiresAt,
		CreatedAt:         now,
		UpdatedAt:         now,
	}

	if s.store != nil {
		if err := s.store.CreateVirtualKey(ctx, key); err != nil {
			return nil, "", err
		}
	}
	s.mu.Lock()
	s.keys[key.ID] = key
	s.byHash[key.KeyHash] = key
	s.mu.Unlock()

	s.audit(ctx, key.CreatedBy, AuditGatewayKeyCreated, orgID, "gateway_keys/"+key.ID, map[string]any{
		"key_id":              key.ID,
		"label":               key.Label,
		"prefix":              key.KeyPrefix,
		"models":              key.Models,
		"monthly_limit_cents": key.MonthlyLimitCents,
	})
	return key.redacted(), plaintext, nil
}

// GetVirtualKey returns one tenant key, redacted (no hash).
func (s *GatewayService) GetVirtualKey(ctx context.Context, orgID, id string) (*VirtualKey, error) {
	key, err := s.getUnredacted(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	return key.redacted(), nil
}

// getUnredacted resolves (org, id) through the store or the cache and
// returns an internal copy.
func (s *GatewayService) getUnredacted(ctx context.Context, orgID, id string) (*VirtualKey, error) {
	if strings.TrimSpace(id) == "" {
		return nil, ErrVirtualKeyNotFound
	}
	if s.store != nil {
		return s.store.GetVirtualKey(ctx, orgID, id)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	key, ok := s.keys[id]
	if !ok || key.OrganizationID != orgID {
		return nil, ErrVirtualKeyNotFound
	}
	out := *key
	return &out, nil
}

// ListVirtualKeys returns the tenant's keys, redacted (no hashes, no
// plaintext — list responses can never leak credential material).
func (s *GatewayService) ListVirtualKeys(ctx context.Context, orgID string) ([]*VirtualKey, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, fmt.Errorf("%w: organization id is required", ErrInvalidVirtualKey)
	}
	var keys []*VirtualKey
	if s.store != nil {
		stored, err := s.store.ListVirtualKeys(ctx, orgID)
		if err != nil {
			return nil, err
		}
		keys = stored
	} else {
		s.mu.RLock()
		keys = make([]*VirtualKey, 0, len(s.keys))
		for _, key := range s.keys {
			if key.OrganizationID == orgID {
				keys = append(keys, key)
			}
		}
		s.mu.RUnlock()
		sort.Slice(keys, func(i, j int) bool {
			if !keys[i].CreatedAt.Equal(keys[j].CreatedAt) {
				return keys[i].CreatedAt.Before(keys[j].CreatedAt)
			}
			return keys[i].ID < keys[j].ID
		})
	}
	out := make([]*VirtualKey, 0, len(keys))
	for _, key := range keys {
		out = append(out, key.redacted())
	}
	return out, nil
}

// UpdateVirtualKey patches label / allow-list / monthly limit / expiry
// within one tenant. The allow-list nil-vs-empty contract is addressable
// through VirtualKeyUpdate.Models (pointer-to-slice; see its doc).
func (s *GatewayService) UpdateVirtualKey(ctx context.Context, orgID, id string, u VirtualKeyUpdate) (*VirtualKey, error) {
	if u.MonthlyLimitCents != nil && *u.MonthlyLimitCents < 0 {
		return nil, fmt.Errorf("%w: monthly_limit_cents must be >= 0", ErrInvalidVirtualKey)
	}
	if u.Label != nil && strings.TrimSpace(*u.Label) == "" {
		return nil, fmt.Errorf("%w: label must be non-empty", ErrInvalidVirtualKey)
	}
	var modelsUpdate []string
	var modelsSet bool
	if u.Models != nil {
		normalized, err := normalizeModels(*u.Models)
		if err != nil {
			return nil, err
		}
		modelsUpdate = normalized
		modelsSet = true
	}

	key, err := s.getUnredacted(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	if u.Label != nil {
		key.Label = strings.TrimSpace(*u.Label)
	}
	if modelsSet {
		key.Models = modelsUpdate
	}
	if u.MonthlyLimitCents != nil {
		key.MonthlyLimitCents = *u.MonthlyLimitCents
	}
	if u.ExpiresAt != nil {
		if u.ExpiresAt.IsZero() {
			key.ExpiresAt = nil
		} else {
			t := u.ExpiresAt.UTC()
			key.ExpiresAt = &t
		}
	}
	key.UpdatedAt = time.Now().UTC()

	if s.store != nil {
		if err := s.store.UpdateVirtualKey(ctx, key); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	if cached, ok := s.keys[key.ID]; ok {
		*cached = *key
	} else {
		s.keys[key.ID] = key
		s.byHash[key.KeyHash] = key
	}
	s.mu.Unlock()
	return key.redacted(), nil
}

// RevokeVirtualKey flips the key to revoked within one tenant. Idempotent:
// revoking an already-revoked key succeeds (DELETE semantics); unknown or
// foreign ids are ErrVirtualKeyNotFound. Audited (gateway.key_revoked).
func (s *GatewayService) RevokeVirtualKey(ctx context.Context, orgID, id string) error {
	key, err := s.getUnredacted(ctx, orgID, id)
	if err != nil {
		return err
	}
	if key.Revoked() {
		return nil
	}
	now := time.Now().UTC()
	if s.store != nil {
		if err := s.store.RevokeVirtualKey(ctx, orgID, id, now); err != nil {
			return err
		}
	}
	key.Status = VirtualKeyStatusRevoked
	key.RevokedAt = &now
	key.UpdatedAt = now
	s.mu.Lock()
	if cached, ok := s.keys[key.ID]; ok {
		*cached = *key
	}
	s.mu.Unlock()

	s.audit(ctx, key.CreatedBy, AuditGatewayKeyRevoked, orgID, "gateway_keys/"+id, map[string]any{
		"key_id": id,
		"label":  key.Label,
	})
	return nil
}

// ResolveByPlaintext is the AUTH-TIME lookup: hash the presented vk_
// credential and resolve it to an ACTIVE, unexpired key. Unknown, revoked,
// expired and foreign credentials all collapse into
// ErrVirtualKeyNotFound (no existence leak on the authentication path).
// The returned key is internal (hash populated); it never crosses HTTP.
func (s *GatewayService) ResolveByPlaintext(ctx context.Context, plaintext string) (*VirtualKey, error) {
	trimmed := strings.TrimSpace(plaintext)
	if trimmed == "" {
		return nil, ErrVirtualKeyNotFound
	}
	hash := hashVirtualKey(trimmed)
	var key *VirtualKey
	if s.store != nil {
		stored, err := s.store.GetVirtualKeyByHash(ctx, hash)
		if err != nil {
			return nil, ErrVirtualKeyNotFound
		}
		key = stored
	} else {
		s.mu.RLock()
		if cached, ok := s.byHash[hash]; ok {
			out := *cached
			key = &out
		}
		s.mu.RUnlock()
		if key == nil {
			return nil, ErrVirtualKeyNotFound
		}
	}
	if key.Revoked() || key.Expired(time.Now().UTC()) {
		return nil, ErrVirtualKeyNotFound
	}
	return key, nil
}

// Authorize is the enforcement decision for ONE completion: lifecycle
// (active, unexpired) -> allow-list -> monthly spend cap. estCostCents is
// the caller's pre-flight estimate (0 = allow-list / used-to-date check
// only, e.g. when the model has no pricing entry). Exactly-at-limit
// projects are allowed; strictly-over-limit projections are denied.
//
// Errors: ErrVirtualKeyNotFound / ErrVirtualKeyRevoked / ErrVirtualKeyExpired,
// *ModelNotAllowedError (errors.Is ErrModelNotAllowed) and
// *SpendLimitError (errors.Is ErrSpendLimitExceeded). Ledger read failures
// surface as wrapped store errors — enforcement fails closed (see the file
// header). Denials are audited (best effort).
func (s *GatewayService) Authorize(ctx context.Context, orgID, keyID, model string, estCostCents float64, now time.Time) error {
	key, err := s.getUnredacted(ctx, orgID, keyID)
	if err != nil {
		return err
	}
	if key.Revoked() {
		return ErrVirtualKeyRevoked
	}
	if key.Expired(now) {
		return ErrVirtualKeyExpired
	}

	// Allow-list: nil = disabled (ALL models); empty = NONE.
	if key.Models != nil && !modelAllowed(key.Models, model) {
		denial := &ModelNotAllowedError{KeyID: key.ID, Model: model}
		s.audit(ctx, "gateway", AuditGatewayModelNotAllowed, orgID, "gateway_keys/"+key.ID, map[string]any{
			"key_id": key.ID,
			"label":  key.Label,
			"model":  model,
		})
		return denial
	}

	// Spend cap: 0 = unlimited (documented sentinel).
	if key.MonthlyLimitCents <= 0 {
		return nil
	}
	month := monthKey(now)
	used, err := s.spendForMonth(ctx, orgID, keyID, month)
	if err != nil {
		// Fail closed: a ledger outage must surface, never fake allowance.
		return fmt.Errorf("models: spend check failed: %w", err)
	}
	if estCostCents < 0 {
		estCostCents = 0
	}
	projected := used + estCostCents
	limit := float64(key.MonthlyLimitCents)
	if projected > limit {
		denial := &SpendLimitError{
			KeyID:          key.ID,
			LimitCents:     limit,
			UsedCents:      used,
			ProjectedCents: projected,
		}
		s.audit(ctx, "gateway", AuditGatewaySpendDenied, orgID, "gateway_keys/"+key.ID, map[string]any{
			"key_id":          key.ID,
			"label":           key.Label,
			"model":           model,
			"month":           month,
			"limit_cents":     limit,
			"used_cents":      used,
			"projected_cents": projected,
		})
		return denial
	}
	return nil
}

// RecordSpend appends actual cost to the (key, month UTC) ledger. costCents
// must be >= 0 (negative cost is invalid input, not a refund). A zero at is
// stamped with the current UTC time.
func (s *GatewayService) RecordSpend(ctx context.Context, orgID, keyID, model string, costCents float64, at time.Time) error {
	if s == nil {
		return fmt.Errorf("models: gateway service is required")
	}
	if costCents < 0 {
		return fmt.Errorf("%w: cost_cents must be >= 0", ErrInvalidVirtualKey)
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	month := monthKey(at)
	model = strings.TrimSpace(model)
	if s.store != nil {
		if err := s.store.RecordSpend(ctx, orgID, keyID, model, costCents, at); err != nil {
			return err
		}
	} else {
		s.mu.Lock()
		if s.spend == nil {
			s.spend = make(map[spendBucket]float64)
		}
		s.spend[spendBucket{orgID: orgID, keyID: keyID, month: month, model: model}] += costCents
		s.mu.Unlock()
	}
	if s.spendAuditEnabled() {
		s.audit(ctx, "gateway", AuditGatewaySpendRecorded, orgID, "gateway_keys/"+keyID, map[string]any{
			"key_id":     keyID,
			"model":      model,
			"month":      month,
			"cost_cents": costCents,
		})
	}
	return nil
}

// Spend returns the month-to-date used cents and the key's monthly limit
// (0 = unlimited) for one key. Ledger/store failures surface as an error —
// the spend endpoint must never report a false zero (false allowance).
func (s *GatewayService) Spend(ctx context.Context, orgID, keyID string, now time.Time) (float64, float64, error) {
	key, err := s.getUnredacted(ctx, orgID, keyID)
	if err != nil {
		return 0, 0, err
	}
	used, err := s.spendForMonth(ctx, orgID, keyID, monthKey(now))
	if err != nil {
		return 0, float64(key.MonthlyLimitCents), err
	}
	return used, float64(key.MonthlyLimitCents), nil
}

// spendForMonth sums one (org, key, month) bucket from the store or the
// in-memory ledger.
func (s *GatewayService) spendForMonth(ctx context.Context, orgID, keyID, month string) (float64, error) {
	if s.store != nil {
		return s.store.SpendForMonth(ctx, orgID, keyID, month)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var total float64
	for b, cents := range s.spend {
		if b.orgID == orgID && b.keyID == keyID && b.month == month {
			total += cents
		}
	}
	return total, nil
}

func (s *GatewayService) spendAuditEnabled() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.auditSpend
}

// audit writes one best-effort audit row through the seam; failures never
// break the caller (same convention as the MCP registry's tool-call rows).
func (s *GatewayService) audit(ctx context.Context, actor, action, orgID, resource string, metadata map[string]any) {
	if s == nil {
		return
	}
	s.mu.RLock()
	a := s.auditor
	s.mu.RUnlock()
	if a == nil {
		return
	}
	_, _ = a.LogCtx(ctx, actor, action, orgID, resource, metadata)
}

// ---------------------------------------------------------------------------
// GuardedProvider — the enforcement decorator
// ---------------------------------------------------------------------------

// GuardedProvider wraps ANY Provider with virtual-key enforcement: every
// Complete first calls GatewayService.Authorize with a pre-flight estimate
// (pricing table x max tokens when the model is priced, else a 0-cost
// allow-list/used-to-date pre-check) and, after a successful completion,
// records the ACTUAL cost (ComputeCostCents over the reported usage) into
// the (key, month UTC) ledger. Denial errors surface as
// *ModelNotAllowedError (403 model_not_allowed semantics) and
// *SpendLimitError (402 spend_limit_exceeded semantics) — handlers map them
// with errors.Is/As.
//
// The decorator is transparent in every other dimension: Name() delegates,
// provider errors pass through untouched, and spend recording is
// best-effort (a completed response is never destroyed by a metering write
// failure; the next Authorize re-reads the ledger, so enforcement
// self-heals).
type GuardedProvider struct {
	inner Provider
	gw    *GatewayService
	orgID string
	keyID string
}

var _ Provider = (*GuardedProvider)(nil)

// NewGuardedProvider binds one virtual key (orgID + keyID) to a provider.
func NewGuardedProvider(inner Provider, gw *GatewayService, orgID, keyID string) *GuardedProvider {
	return &GuardedProvider{inner: inner, gw: gw, orgID: orgID, keyID: keyID}
}

// Name implements Provider, delegating to the wrapped provider (transparent
// decorator — nothing keys on a wrapped name).
func (p *GuardedProvider) Name() string {
	if p == nil || p.inner == nil {
		return ""
	}
	return p.inner.Name()
}

// Complete implements Provider with enforcement: authorize (estimate) ->
// inner.Complete -> record actual spend.
func (p *GuardedProvider) Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error) {
	if p == nil || p.inner == nil {
		return nil, errors.New("models: guarded provider requires an inner provider")
	}
	if p.gw != nil {
		model := strings.TrimSpace(req.Model)
		// Pre-flight estimate: pricing table x max tokens when the model is
		// priced (prompt tokens estimated at ~4 chars/token, the same
		// heuristic StaticProvider reports); otherwise est stays 0 and the
		// authorization degrades to the allow-list + used-to-date check.
		est := 0.0
		if model != "" {
			if _, priced := LookupPricing(model); priced {
				promptTokens := estimateTokens(req.Prompt) + estimateTokens(req.System)
				for _, m := range req.Messages {
					promptTokens += estimateTokens(m.Content)
				}
				est = ComputeCostCents(model, promptTokens, req.MaxTokens)
			}
		}
		if err := p.gw.Authorize(ctx, p.orgID, p.keyID, model, est, time.Now()); err != nil {
			// No model on the request: with an allow-list ENABLED this is a
			// denial (an unnameable model cannot be admitted); with the
			// allow-list disabled it enforces the spend cap only. Guarded
			// callers should pin req.Model (the runtime does: agent.Model).
			return nil, err
		}
	}

	resp, err := p.inner.Complete(ctx, req)
	if err != nil {
		return nil, err
	}

	// Actual metering after delivery (best-effort; see type doc).
	if p.gw != nil {
		model := strings.TrimSpace(resp.Model)
		if model == "" {
			model = strings.TrimSpace(req.Model)
		}
		cost := ComputeCostCents(model, resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
		if cost > 0 || resp.Usage.TotalTokens > 0 {
			_ = p.gw.RecordSpend(ctx, p.orgID, p.keyID, model, cost, time.Now())
		}
	}
	return resp, nil
}

// marshalModelsParam maps the allow-list onto the JSONB column: nil -> SQL
// NULL (allow-list disabled), else the marshaled array ("[]" = NONE).
func marshalModelsParam(list []string) any {
	if list == nil {
		return nil
	}
	b, err := json.Marshal(list)
	if err != nil {
		return nil
	}
	return string(b)
}
