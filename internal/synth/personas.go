package synth

// personas.go — issue #149 (10-q): built-in synthetic personas, persona
// validation, and the org-scoped persona registry service.
//
// A persona is a synthetic identity profile (types.go): it NEVER impersonates
// a real customer account and carries only role/permission hints for journey
// planning plus deterministic test constraints (network throttling, viewport,
// accessibility posture, input modality). The 9 BUILT-IN personas are the
// shared vocabulary every track plans journeys against; custom personas may
// extend them but can never CLAIM a built-in key without matching the
// canonical definition byte-for-byte (anti-forgery rule, ValidatePersona).
//
// Versioning contract:
//   - created personas start at Version 1;
//   - every mutation bumps the version (PersonaService.UpdatePersona);
//   - built-in personas are immutable (derive instead — DerivePersona);
//   - version identity (id, version) is what run snapshots pin
//     (VersionedRunInputs in journeys.go), so "same version ⇒ same bytes".
//
// Persistence is dual-mode: narrow PersonaStore interface here with an
// in-memory implementation; the Postgres implementation belongs to the
// registry track's store follow-up (migration 034 already owns
// synth_personas). This file deliberately contains no SQL.

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Persona errors (all wrap the contract's ErrInvalid so callers can branch
// on the broad class while still matching the specific sentinel).
var (
	// ErrBuiltInImmutable is returned when a caller tries to mutate a
	// built-in persona. Built-ins are shared vocabulary: derive a custom
	// persona instead (DerivePersona).
	ErrBuiltInImmutable = fmt.Errorf("%w: built-in personas are immutable", ErrInvalid)
	// ErrBuiltInClaim is returned when a custom persona claims a built-in
	// key without matching the canonical definition exactly.
	ErrBuiltInClaim = fmt.Errorf("%w: persona claims a built-in key it does not match", ErrInvalid)
	// ErrPersonaInUse is returned when deleting a persona that seeded
	// built-in vocabulary or is otherwise protected.
	ErrPersonaInUse = fmt.Errorf("%w: persona cannot be deleted", ErrInvalid)
)

// Built-in persona keys. These are the ONLY values allowed in
// Persona.BuiltIn ("" means custom persona).
const (
	PersonaVisitor           = "visitor"
	PersonaNewUser           = "new_user"
	PersonaReturningUser     = "returning_user"
	PersonaStandardCustomer  = "standard_customer"
	PersonaAdministrator     = "administrator"
	PersonaRestrictedUser    = "restricted_user"
	PersonaMobileUser        = "mobile_user"
	PersonaAccessibilityUser = "accessibility_user"
	PersonaSlowNetworkUser   = "slow_network_user"
)

// Canonical constraint keys for Persona.Constraints. types.go documents the
// examples (network=slow3g, viewport=mobile, a11y=strict); these constants
// pin the spellings. Org-specific extension keys are allowed as long as they
// are lowercase snake_case with non-empty values (unknown keys are NOT
// validated against this set — see ValidatePersona).
const (
	ConstraintNetwork  = "network"  // values: NetworkSlow3G | NetworkRegular
	ConstraintViewport = "viewport" // values: ViewportMobile | ViewportDesktop
	ConstraintA11y     = "a11y"     // values: A11yStrict | A11yOff
	ConstraintInput    = "input"    // values: InputKeyboardOnly | InputTouch | InputPointer
)

// Canonical constraint values.
const (
	NetworkSlow3G     = "slow3g"
	NetworkRegular    = "regular"
	ViewportMobile    = "mobile"
	ViewportDesktop   = "desktop"
	A11yStrict        = "strict"
	A11yOff           = "none"
	InputKeyboardOnly = "keyboard_only"
	InputTouch        = "touch"
	InputPointer      = "pointer"
)

// personaBlueprint is the canonical, immutable definition of one built-in
// persona. Blueprints are the anti-forgery oracle: a persona claiming a
// built-in key must match its blueprint role hints and constraints exactly
// (same sequence, same pairs).
type personaBlueprint struct {
	key         string
	name        string
	roleHints   []string
	constraints map[string]string
}

// builtInPersonaBlueprints is the single source of truth for the 9 built-in
// personas. Order here is the canonical presentation order (mirrored by
// BuiltInPersonas and BuiltInPersonaKeys).
var builtInPersonaBlueprints = []personaBlueprint{
	{
		key:  PersonaVisitor,
		name: "Visitor",
		// Unauthenticated browsing: only public surfaces may be exercised.
		roleHints:   []string{"unauthenticated", "public_surfaces_only"},
		constraints: map[string]string{},
	},
	{
		key:  PersonaNewUser,
		name: "New User",
		// Fresh account: onboarding state, no saved preferences, empty history.
		roleHints:   []string{"authenticated", "onboarding_state", "self_service"},
		constraints: map[string]string{},
	},
	{
		key:  PersonaReturningUser,
		name: "Returning User",
		// Established account with saved preferences and prior history.
		roleHints:   []string{"authenticated", "saved_preferences", "prior_history"},
		constraints: map[string]string{},
	},
	{
		key:  PersonaStandardCustomer,
		name: "Standard Customer",
		// Fully provisioned paying customer: billing flows in scope.
		roleHints:   []string{"authenticated", "billing_enabled", "full_self_service"},
		constraints: map[string]string{},
	},
	{
		key:  PersonaAdministrator,
		name: "Administrator",
		// Highest-privilege synthetic identity. Only ever mapped to dedicated
		// synthetic admin credentials (never a real account).
		roleHints: []string{"authenticated", "admin_console", "user_management", "settings_write", "audit_visible"},
		// Admin flows must stay legible under the strict a11y bar too.
		constraints: map[string]string{ConstraintA11y: A11yOff},
	},
	{
		key:  PersonaRestrictedUser,
		name: "Restricted User",
		// Deliberately minimal permissions: verifies denials, not just grants.
		roleHints:   []string{"authenticated", "minimal_permissions", "read_only_surfaces"},
		constraints: map[string]string{},
	},
	{
		key:  PersonaMobileUser,
		name: "Mobile User",
		// Touch-first device profile; journeys must not assume hover or
		// desktop-width layouts.
		roleHints:   []string{"authenticated", "touch_first_navigation"},
		constraints: map[string]string{ConstraintViewport: ViewportMobile, ConstraintInput: InputTouch},
	},
	{
		key:  PersonaAccessibilityUser,
		name: "Accessibility User",
		// Keyboard-only operation under the strict a11y posture; pairs with
		// AssertA11y assertions in journeys.
		roleHints:   []string{"authenticated", "assistive_technology"},
		constraints: map[string]string{ConstraintA11y: A11yStrict, ConstraintInput: InputKeyboardOnly},
	},
	{
		key:  PersonaSlowNetworkUser,
		name: "Slow Network User",
		// Throttled network profile; pairs with AssertLatencyBelow assertions.
		roleHints:   []string{"authenticated", "latency_resilient_flows"},
		constraints: map[string]string{ConstraintNetwork: NetworkSlow3G},
	},
}

// BuiltInPersonaKeys returns the 9 built-in persona keys in canonical order.
// The slice is a fresh copy; callers may mutate it freely.
func BuiltInPersonaKeys() []string {
	keys := make([]string, 0, len(builtInPersonaBlueprints))
	for _, bp := range builtInPersonaBlueprints {
		keys = append(keys, bp.key)
	}
	return keys
}

// IsBuiltInPersonaKey reports whether key is one of the 9 built-in keys.
func IsBuiltInPersonaKey(key string) bool {
	for _, bp := range builtInPersonaBlueprints {
		if bp.key == key {
			return true
		}
	}
	return false
}

// BuiltInPersonas returns the 9 built-in personas as template records
// (org-scoping and persistence are the caller's/service's job — see
// PersonaService.SeedBuiltInPersonas). Templates carry Version 1, empty IDs
// and OrgID. Every call returns fresh deep copies: callers can never mutate
// the canonical blueprints through the returned slice.
func BuiltInPersonas() []Persona {
	out := make([]Persona, 0, len(builtInPersonaBlueprints))
	for _, bp := range builtInPersonaBlueprints {
		out = append(out, blueprintPersona(bp, ""))
	}
	return out
}

// NewBuiltInPersona instantiates one built-in persona for an org. The ID is
// left empty (the store/service stamps it); BuiltIn carries the key so the
// anti-forgery validation stays enforceable after persistence.
func NewBuiltInPersona(key, orgID string) (Persona, error) {
	bp, ok := blueprintByKey(key)
	if !ok {
		return Persona{}, fmt.Errorf("%w: unknown built-in persona key %q", ErrInvalid, key)
	}
	return blueprintPersona(bp, orgID), nil
}

// blueprintPersona materializes a blueprint into a Persona record (deep
// copies of hints/constraints; Version 1; empty ID).
func blueprintPersona(bp personaBlueprint, orgID string) Persona {
	hints := make([]string, len(bp.roleHints))
	copy(hints, bp.roleHints)
	constraints := make(map[string]string, len(bp.constraints))
	for k, v := range bp.constraints {
		constraints[k] = v
	}
	return Persona{
		ID:          "",
		OrgID:       orgID,
		Name:        bp.name,
		BuiltIn:     bp.key,
		RoleHints:   hints,
		Constraints: constraints,
		Version:     1,
	}
}

func blueprintByKey(key string) (personaBlueprint, bool) {
	for _, bp := range builtInPersonaBlueprints {
		if bp.key == key {
			return bp, true
		}
	}
	return personaBlueprint{}, false
}

// ValidatePersona enforces the persona contract:
//
//   - Name non-empty; RoleHints non-empty strings without duplicates;
//   - Constraints keys lowercase snake_case with non-empty values; values
//     for the canonical keys (network/viewport/a11y/input) must be one of
//     the known constants;
//   - BuiltIn must be "" (custom persona) or one of the 9 built-in keys;
//   - ANTI-FORGERY: a persona claiming a built-in key must match the
//     canonical blueprint exactly (same role hints in the same order, same
//     constraint pairs). This is the "custom personas cannot claim built-in
//     keys" rule — claiming the key grants nothing unless the definition is
//     the canonical one, and mutations away from the blueprint are rejected.
//
// ValidatePersona does not mutate p and does not require ID/OrgID (those are
// stamped by the persistence layer); org presence is enforced by the service.
func ValidatePersona(p *Persona) error {
	if p == nil {
		return fmt.Errorf("%w: persona is nil", ErrInvalid)
	}
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("%w: persona name is required", ErrInvalid)
	}
	if p.BuiltIn != "" && !IsBuiltInPersonaKey(p.BuiltIn) {
		return fmt.Errorf("%w: unknown built-in persona key %q", ErrInvalid, p.BuiltIn)
	}
	if err := validateRoleHints(p.RoleHints); err != nil {
		return err
	}
	if err := validateConstraints(p.Constraints); err != nil {
		return err
	}
	if p.BuiltIn != "" {
		bp, _ := blueprintByKey(p.BuiltIn)
		if !personaMatchesBlueprint(p, bp) {
			return fmt.Errorf("%w: key %q", ErrBuiltInClaim, p.BuiltIn)
		}
	}
	return nil
}

func validateRoleHints(hints []string) error {
	seen := make(map[string]bool, len(hints))
	for _, h := range hints {
		h = strings.TrimSpace(h)
		if h == "" {
			return fmt.Errorf("%w: persona role hints must be non-empty", ErrInvalid)
		}
		if seen[h] {
			return fmt.Errorf("%w: duplicate persona role hint %q", ErrInvalid, h)
		}
		seen[h] = true
	}
	return nil
}

func validateConstraints(constraints map[string]string) error {
	for k, v := range constraints {
		if k == "" || k != strings.ToLower(k) || strings.TrimSpace(k) != k || strings.Contains(k, "-") {
			return fmt.Errorf("%w: persona constraint key %q must be lowercase snake_case", ErrInvalid, k)
		}
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("%w: persona constraint %q has an empty value", ErrInvalid, k)
		}
		if !constraintValueAllowed(k, v) {
			return fmt.Errorf("%w: persona constraint %q=%q is not a known value", ErrInvalid, k, v)
		}
	}
	return nil
}

// constraintValueAllowed enforces the canonical value vocabulary for the
// canonical constraint keys. Unknown (org-extension) keys accept any
// non-empty value.
func constraintValueAllowed(key, value string) bool {
	switch key {
	case ConstraintNetwork:
		return value == NetworkSlow3G || value == NetworkRegular
	case ConstraintViewport:
		return value == ViewportMobile || value == ViewportDesktop
	case ConstraintA11y:
		return value == A11yStrict || value == A11yOff
	case ConstraintInput:
		return value == InputKeyboardOnly || value == InputTouch || value == InputPointer
	default:
		return true
	}
}

// personaMatchesBlueprint reports whether p is byte-for-byte the canonical
// definition (role hints sequence + constraint pairs).
func personaMatchesBlueprint(p *Persona, bp personaBlueprint) bool {
	if len(p.RoleHints) != len(bp.roleHints) {
		return false
	}
	for i, h := range bp.roleHints {
		if p.RoleHints[i] != h {
			return false
		}
	}
	if len(p.Constraints) != len(bp.constraints) {
		return false
	}
	for k, v := range bp.constraints {
		got, ok := p.Constraints[k]
		if !ok || got != v {
			return false
		}
	}
	return true
}

// ClonePersona returns a deep copy of p. The copy preserves identity
// (ID/OrgID/BuiltIn/Version) — it is the safe way to hand a stored record to
// mutating code without touching the store. For a NEW derived persona use
// DerivePersona instead.
func ClonePersona(p Persona) Persona {
	p.RoleHints = append([]string(nil), p.RoleHints...)
	p.Constraints = cloneConstraints(p.Constraints)
	return p
}

func cloneConstraints(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// DerivePersona builds a NEW custom persona from p's profile: role hints and
// constraints are copied, the built-in claim is cleared (a derived persona is
// by definition custom), the version resets to 1 and the caller-supplied newID
// is stamped. Use it to specialize a built-in (e.g. administrator + slow
// network) without forking the shared vocabulary.
func DerivePersona(p Persona, newID string) Persona {
	d := ClonePersona(p)
	d.ID = newID
	d.Name = p.Name + " (derived)"
	d.BuiltIn = ""
	d.Version = 1
	return d
}

// PersonaStore is the narrow persistence contract for personas. Implementations
// MUST be org-scoped: every lookup keyed by (orgID, id) must return
// ErrNotFound for a foreign org's record (no cross-tenant existence leak).
// The Postgres implementation (synth_personas, migration 034) is a registry
// track follow-up; InMemoryPersonaStore below is the default.
type PersonaStore interface {
	PutPersona(ctx context.Context, p Persona) error
	GetPersona(ctx context.Context, orgID, id string) (Persona, error)
	ListPersonas(ctx context.Context, orgID string) ([]Persona, error)
	DeletePersona(ctx context.Context, orgID, id string) error
}

// InMemoryPersonaStore is the default PersonaStore: mutex-guarded map,
// org-scoped reads, defensive copies on the way in and out.
type InMemoryPersonaStore struct {
	mu    sync.RWMutex
	byID  map[string]Persona
	order []string // insertion order for deterministic listing
}

// NewInMemoryPersonaStore returns an empty ready-to-use store.
func NewInMemoryPersonaStore() *InMemoryPersonaStore {
	return &InMemoryPersonaStore{byID: make(map[string]Persona)}
}

// PutPersona upserts a persona (deep-copied). Zero-value timestamps are
// stamped here so direct store users still get sane records.
func (s *InMemoryPersonaStore) PutPersona(ctx context.Context, p Persona) error {
	now := time.Now()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	p.UpdatedAt = now
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.byID[p.ID]; !exists {
		s.order = append(s.order, p.ID)
	}
	s.byID[p.ID] = ClonePersona(p)
	return nil
}

// GetPersona returns a deep copy of the persona; foreign-org and unknown ids
// are indistinguishable (ErrNotFound).
func (s *InMemoryPersonaStore) GetPersona(ctx context.Context, orgID, id string) (Persona, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.byID[id]
	if !ok || p.OrgID != orgID {
		return Persona{}, fmt.Errorf("%w: persona %s", ErrNotFound, id)
	}
	return ClonePersona(p), nil
}

// ListPersonas returns all personas of one org in insertion order (deep copies).
func (s *InMemoryPersonaStore) ListPersonas(ctx context.Context, orgID string) ([]Persona, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Persona, 0, len(s.order))
	for _, id := range s.order {
		if p, ok := s.byID[id]; ok && p.OrgID == orgID {
			out = append(out, ClonePersona(p))
		}
	}
	return out, nil
}

// DeletePersona removes the persona; unknown or foreign ids yield ErrNotFound.
func (s *InMemoryPersonaStore) DeletePersona(ctx context.Context, orgID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.byID[id]
	if !ok || p.OrgID != orgID {
		return fmt.Errorf("%w: persona %s", ErrNotFound, id)
	}
	delete(s.byID, id)
	for i, oid := range s.order {
		if oid == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	return nil
}

// PersonaService is the org-scoped persona registry over a PersonaStore. It
// owns validation, version bumps on mutation, built-in seeding/immutability
// and deterministic timestamps/ids (injectable for tests).
type PersonaService struct {
	store PersonaStore
	now   func() time.Time
	newID func() string
}

// NewPersonaService wraps store. The clock and id generator are injectable
// fields for deterministic tests.
func NewPersonaService(store PersonaStore) *PersonaService {
	return &PersonaService{store: store, now: time.Now, newID: newSynthID}
}

// SeedBuiltInPersonas idempotently instantiates all 9 built-in personas for
// orgID. Existing built-in personas of the org are left untouched (never
// re-versioned by seeding); the returned slice is in canonical blueprint
// order. Deterministic ids (builtin-<key>-<orgid hash-free suffix>) keep
// repeated seeding stable across calls.
func (s *PersonaService) SeedBuiltInPersonas(ctx context.Context, orgID string) ([]Persona, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, fmt.Errorf("%w: org id is required", ErrInvalid)
	}
	existing, err := s.store.ListPersonas(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("list personas: %w", err)
	}
	have := make(map[string]bool, len(existing))
	for _, p := range existing {
		if p.BuiltIn != "" {
			have[p.BuiltIn] = true
		}
	}
	seeded := make([]Persona, 0, len(builtInPersonaBlueprints))
	for _, bp := range builtInPersonaBlueprints {
		if have[bp.key] {
			continue
		}
		p := blueprintPersona(bp, orgID)
		p.ID = "builtin-" + bp.key + "-" + orgScopedSuffix(orgID)
		p.CreatedAt = s.now()
		p.UpdatedAt = p.CreatedAt
		if err := s.store.PutPersona(ctx, p); err != nil {
			return nil, fmt.Errorf("seed persona %s: %w", bp.key, err)
		}
		seeded = append(seeded, ClonePersona(p))
	}
	return seeded, nil
}

// orgScopedSuffix derives a short deterministic per-org suffix for built-in
// persona ids so seeding is idempotent without global coordination.
func orgScopedSuffix(orgID string) string {
	sum := uint64(1469598103934665603)
	for _, b := range []byte(orgID) {
		sum ^= uint64(b)
		sum *= 1099511628211
	}
	return strings.ToLower(hex.EncodeToString([]byte{byte(sum >> 56), byte(sum >> 48), byte(sum >> 40), byte(sum >> 32)}))
}

// CreatePersona validates and persists a new persona at Version 1.
func (s *PersonaService) CreatePersona(ctx context.Context, in Persona) (Persona, error) {
	if strings.TrimSpace(in.OrgID) == "" {
		return Persona{}, fmt.Errorf("%w: persona org id is required", ErrInvalid)
	}
	in.ID = s.newID()
	in.Version = 1
	in.CreatedAt = s.now()
	in.UpdatedAt = in.CreatedAt
	if err := ValidatePersona(&in); err != nil {
		return Persona{}, err
	}
	if err := s.store.PutPersona(ctx, ClonePersona(in)); err != nil {
		return Persona{}, fmt.Errorf("put persona: %w", err)
	}
	return in, nil
}

// GetPersona fetches a deep copy of the persona.
func (s *PersonaService) GetPersona(ctx context.Context, orgID, id string) (Persona, error) {
	return s.store.GetPersona(ctx, orgID, id)
}

// ListPersonas returns all personas of one org in insertion order.
func (s *PersonaService) ListPersonas(ctx context.Context, orgID string) ([]Persona, error) {
	return s.store.ListPersonas(ctx, orgID)
}

// UpdatePersona applies mutate to a fresh copy of the stored persona,
// re-validates the result and bumps the version. Rules:
//
//   - built-in personas are immutable (ErrBuiltInImmutable) — use
//     DerivePersona to specialize one;
//   - the BuiltIn claim cannot be added, changed or dropped by mutation:
//     adding a claim is only possible with a byte-exact canonical match
//     (ValidatePersona anti-forgery), and dropping/switching one changes
//     identity, which requires a new persona;
//   - every successful update bumps Version and UpdatedAt (version identity
//     feeds run snapshots — VersionedRunInputs).
func (s *PersonaService) UpdatePersona(ctx context.Context, orgID, id string, mutate func(*Persona)) (Persona, error) {
	if mutate == nil {
		return Persona{}, fmt.Errorf("%w: mutate function is required", ErrInvalid)
	}
	current, err := s.store.GetPersona(ctx, orgID, id)
	if err != nil {
		return Persona{}, err
	}
	if current.BuiltIn != "" {
		return Persona{}, fmt.Errorf("%w: persona %s (built-in %q)", ErrBuiltInImmutable, id, current.BuiltIn)
	}
	next := ClonePersona(current)
	mutate(&next)
	if next.BuiltIn != current.BuiltIn {
		return Persona{}, fmt.Errorf("%w: BuiltIn claim is immutable (use a new persona)", ErrInvalid)
	}
	if err := ValidatePersona(&next); err != nil {
		return Persona{}, err
	}
	next.Version = current.Version + 1
	next.CreatedAt = current.CreatedAt
	next.UpdatedAt = s.now()
	if err := s.store.PutPersona(ctx, next); err != nil {
		return Persona{}, fmt.Errorf("put persona: %w", err)
	}
	return ClonePersona(next), nil
}

// DeletePersona removes a custom persona. Built-in personas are shared
// vocabulary and cannot be deleted (ErrPersonaInUse); retire them by not
// using them in new journeys.
func (s *PersonaService) DeletePersona(ctx context.Context, orgID, id string) error {
	current, err := s.store.GetPersona(ctx, orgID, id)
	if err != nil {
		return err
	}
	if current.BuiltIn != "" {
		return fmt.Errorf("%w: persona %s is built-in", ErrPersonaInUse, id)
	}
	return s.store.DeletePersona(ctx, orgID, id)
}

// newSynthID generates a prefixed random id ("per_ab12…"). Panics only if
// the OS entropy source fails, which is unrecoverable for a security-
// relevant id.
func newSynthID() string {
	return newSynthIDWithPrefix("id")
}

func newSynthIDWithPrefix(prefix string) string {
	var b [8]byte
	if _, err := crand.Read(b[:]); err != nil {
		panic("synth: entropy source unavailable: " + err.Error())
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}

// sortedKeys is a tiny helper for deterministic map iteration in reports.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
