package secrets

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

// service.go is the org-scoped secrets service (issue #25), dual-mode per the
// platform convention:
//
//   - NewService(): in-memory mode, zero infrastructure. Values are still
//     sealed with AES-256-GCM under an ephemeral process-lifetime key, so no
//     plaintext ever rests in the map and no master key env is required.
//   - NewServiceWithStore(NewPostgresStore(db), cipher): Postgres mode. The
//     cipher is MANDATORY and construction fails fast when missing/invalid —
//     persisting unencrypted secrets would be a security regression.
//
// Org isolation is absolute: every operation is keyed by (organization_id,
// name) and cross-tenant reads surface as ErrNotFound, never as a 403 leak
// (same convention as the scheduler track).
//
// The plaintext of a secret exists in exactly two places: the Create/Update
// argument and the Resolve return value (the runtime seam consumed later by
// connectors/tools). List and Get return metadata ONLY and never values.

var (
	ErrOrgRequired       = errors.New("organization_id is required")
	ErrNameRequired      = errors.New("secret name is required")
	ErrValueRequired     = errors.New("secret value is required")
	ErrNameInvalid       = errors.New("secret name must match [A-Za-z0-9] followed by [A-Za-z0-9._-] (max 255 chars)")
	ErrValueTooLarge     = errors.New("secret value exceeds the 64 KiB limit")
	ErrNotFound          = errors.New("secret not found")
	ErrDuplicate         = errors.New("secret already exists")
	ErrUpdatedByRequired = errors.New("actor identity is required")
)

// namePattern keeps secret names URL-path safe (DELETE /v1/secrets/{name}) and
// shell/env-var friendly. Repo convention elsewhere is free-form ids, but
// secret names are typed by humans into env-style references, so a strict
// charset avoids encoding surprises end to end.
var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,254}$`)

// MaxValueBytes bounds a single secret value (64 KiB is generous for API keys
// and tokens while keeping DPoP-sized payloads out of the table).
const MaxValueBytes = 64 * 1024

// Secret is secret METADATA — it deliberately carries no value. Every
// list/get projection in the service and every HTTP list response uses this
// shape, so "list never returns values" is a type-level guarantee.
type Secret struct {
	Name       string    `json:"name"`
	KeyVersion int       `json:"key_version"`
	CreatedBy  string    `json:"created_by"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Record is the encrypted row as seen by a Store: envelope parts plus audit
// bookkeeping. Ciphertext/Nonce are opaque sealed bytes.
type Record struct {
	OrgID      string
	Name       string
	Envelope   string
	Nonce      []byte
	Ciphertext []byte
	KeyVersion int
	CreatedBy  string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	DeletedAt  *time.Time
}

// Store abstracts durable secret storage. Tenant scoping is enforced in the
// store layer too (every query filters organization_id); the service treats
// the store as untrusted and re-checks isolation on the read path. ListMeta
// and GetMeta MUST NOT select ciphertext columns (defense in depth: the
// encrypted material is never even read for metadata projections).
type Store interface {
	// Create inserts one sealed row; a live (org, name) conflict maps to
	// ErrDuplicate. Tombstoned names are reusable (partial unique index).
	Create(ctx context.Context, rec *Record) error
	// Update replaces the sealed material of one live row within one tenant.
	Update(ctx context.Context, rec *Record) error
	// SoftDelete tombstones one live row within one tenant (deleted_at set).
	SoftDelete(ctx context.Context, orgID, name string, deletedAt time.Time) error
	// ListMeta returns metadata of all live secrets of exactly one tenant.
	ListMeta(ctx context.Context, orgID string) ([]*Record, error)
	// GetMeta returns metadata of one live secret within one tenant.
	GetMeta(ctx context.Context, orgID, name string) (*Record, error)
	// GetEncrypted returns the full sealed row (ciphertext + nonce) of one
	// live secret within one tenant; only the Resolve path calls it.
	GetEncrypted(ctx context.Context, orgID, name string) (*Record, error)
}

// Service is the dual-mode secrets service.
type Service struct {
	mu     sync.RWMutex
	items  map[string]map[string]*Record // orgID -> name -> record (memory mode)
	store  Store
	cipher *Cipher
}

// NewService returns the in-memory secrets service. It seals values under an
// ephemeral random key, so zero-infrastructure mode needs no configuration
// while still never holding plaintext at rest.
func NewService() *Service {
	key, err := newEphemeralKey()
	if err != nil {
		// crypto/rand failing is unrecoverable; panic like other stdlib
		// crypto users would. (Unreachable on every supported platform.)
		panic(fmt.Sprintf("secrets: ephemeral key generation failed: %v", err))
	}
	cipher, err := NewCipher(key)
	if err != nil {
		panic(fmt.Sprintf("secrets: ephemeral cipher init failed: %v", err))
	}
	return &Service{
		items:  make(map[string]map[string]*Record),
		cipher: cipher,
	}
}

// NewServiceWithCipher returns an in-memory service bound to an explicit
// cipher (tests / deterministic key material).
func NewServiceWithCipher(cipher *Cipher) *Service {
	return &Service{
		items:  make(map[string]map[string]*Record),
		cipher: cipher,
	}
}

// NewServiceWithStore returns a Postgres-backed service. The cipher is
// mandatory: a nil/invalid cipher is a construction-time error (fail fast),
// because falling back to plaintext storage is exactly the regression issue
// #25 exists to prevent.
func NewServiceWithStore(store Store, cipher *Cipher) (*Service, error) {
	if store == nil {
		return nil, errors.New("secrets: store is required")
	}
	if cipher == nil || cipher.CurrentVersion() <= 0 {
		return nil, ErrMasterKeyRequired
	}
	return &Service{store: store, cipher: cipher}, nil
}

// NewPostgresService is the cmd/api wiring helper: Postgres store plus the
// env-provided master key, failing fast (error, not silent fallback) when
// AGENTOS_SECRETS_MASTER_KEY is missing/invalid.
func NewPostgresService(db *sql.DB) (*Service, error) {
	cipher, err := NewCipherFromEnv()
	if err != nil {
		return nil, err
	}
	return NewServiceWithStore(NewPostgresStore(db), cipher)
}

// validateName applies the shared name rules.
func validateName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", ErrNameRequired
	}
	if !namePattern.MatchString(name) {
		return "", ErrNameInvalid
	}
	return name, nil
}

// validateInput applies the shared create/update rules and returns the
// normalized name.
func validateInput(orgID, name, value string) (string, error) {
	if strings.TrimSpace(orgID) == "" {
		return "", ErrOrgRequired
	}
	norm, err := validateName(name)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(value) == "" {
		return "", ErrValueRequired
	}
	if len(value) > MaxValueBytes {
		return "", ErrValueTooLarge
	}
	return norm, nil
}

// meta projects a Record (or row) onto the value-free metadata shape.
func meta(name string, keyVersion int, createdBy string, createdAt, updatedAt time.Time) *Secret {
	return &Secret{
		Name:       name,
		KeyVersion: keyVersion,
		CreatedBy:  createdBy,
		CreatedAt:  createdAt.UTC(),
		UpdatedAt:  updatedAt.UTC(),
	}
}

// Create seals value and stores a new secret within one tenant.
func (s *Service) Create(ctx context.Context, orgID, name, value, createdBy string) (*Secret, error) {
	if s == nil {
		return nil, errors.New("secrets: service is nil")
	}
	norm, err := validateInput(orgID, name, value)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(createdBy) == "" {
		return nil, ErrUpdatedByRequired
	}

	envelope, err := s.cipher.Seal(value)
	if err != nil {
		return nil, err
	}
	keyVersion, nonce, ciphertext, err := ParseEnvelope(envelope)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	rec := &Record{
		OrgID:      orgID,
		Name:       norm,
		Envelope:   envelope,
		Nonce:      nonce,
		Ciphertext: ciphertext,
		KeyVersion: keyVersion,
		CreatedBy:  createdBy,
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	if s.store != nil {
		if err := s.store.Create(ctx, rec); err != nil {
			return nil, err
		}
		return meta(rec.Name, rec.KeyVersion, rec.CreatedBy, rec.CreatedAt, rec.UpdatedAt), nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	orgItems, ok := s.items[orgID]
	if !ok {
		orgItems = make(map[string]*Record)
		s.items[orgID] = orgItems
	}
	if existing := orgItems[norm]; existing != nil && existing.DeletedAt == nil {
		return nil, ErrDuplicate
	}
	orgItems[norm] = rec
	return meta(rec.Name, rec.KeyVersion, rec.CreatedBy, rec.CreatedAt, rec.UpdatedAt), nil
}

// Update rotates the value of one existing secret within one tenant (new
// random nonce + current key version).
func (s *Service) Update(ctx context.Context, orgID, name, value, updatedBy string) (*Secret, error) {
	if s == nil {
		return nil, errors.New("secrets: service is nil")
	}
	norm, err := validateInput(orgID, name, value)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(updatedBy) == "" {
		return nil, ErrUpdatedByRequired
	}

	envelope, err := s.cipher.Seal(value)
	if err != nil {
		return nil, err
	}
	keyVersion, nonce, ciphertext, err := ParseEnvelope(envelope)
	if err != nil {
		return nil, err
	}

	if s.store != nil {
		// Preserve the original creator + created_at: load the live row first
		// (org-guarded) so the UPDATE can restate them.
		existing, err := s.store.GetMeta(ctx, orgID, norm)
		if err != nil {
			return nil, err
		}
		rec := &Record{
			OrgID:      orgID,
			Name:       norm,
			Envelope:   envelope,
			Nonce:      nonce,
			Ciphertext: ciphertext,
			KeyVersion: keyVersion,
			CreatedBy:  existing.CreatedBy,
			CreatedAt:  existing.CreatedAt,
			UpdatedAt:  time.Now().UTC(),
		}
		if err := s.store.Update(ctx, rec); err != nil {
			return nil, err
		}
		return meta(rec.Name, rec.KeyVersion, rec.CreatedBy, rec.CreatedAt, rec.UpdatedAt), nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.items[orgID][norm]
	if !ok || rec == nil || rec.DeletedAt != nil {
		return nil, ErrNotFound
	}
	rec.Envelope = envelope
	rec.Nonce = nonce
	rec.Ciphertext = ciphertext
	rec.KeyVersion = keyVersion
	rec.UpdatedAt = time.Now().UTC()
	return meta(rec.Name, rec.KeyVersion, rec.CreatedBy, rec.CreatedAt, rec.UpdatedAt), nil
}

// Delete soft-deletes one secret within one tenant (tombstone, ciphertext
// row stays for audit/forensics). Unknown or foreign names are ErrNotFound.
func (s *Service) Delete(ctx context.Context, orgID, name string) error {
	if s == nil {
		return errors.New("secrets: service is nil")
	}
	if strings.TrimSpace(orgID) == "" {
		return ErrOrgRequired
	}
	norm, err := validateName(name)
	if err != nil {
		return err
	}
	if s.store != nil {
		return s.store.SoftDelete(ctx, orgID, norm, time.Now().UTC())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.items[orgID][norm]
	if !ok || rec == nil || rec.DeletedAt != nil {
		return ErrNotFound
	}
	now := time.Now().UTC()
	rec.DeletedAt = &now
	rec.UpdatedAt = now
	// Best-effort scrub of the sealed material: the tombstone keeps metadata,
	// the key material goes. (Resolve on a tombstone is already blocked.)
	rec.Envelope = ""
	rec.Nonce = nil
	rec.Ciphertext = nil
	return nil
}

// List returns metadata for every live secret of one tenant. Values are
// structurally absent (the Secret type has no value field, and the store's
// ListMeta query never selects ciphertext columns).
func (s *Service) List(ctx context.Context, orgID string) ([]*Secret, error) {
	if s == nil {
		return nil, errors.New("secrets: service is nil")
	}
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgRequired
	}
	if s.store != nil {
		recs, err := s.store.ListMeta(ctx, orgID)
		if err != nil {
			return nil, err
		}
		out := make([]*Secret, 0, len(recs))
		for _, rec := range recs {
			out = append(out, meta(rec.Name, rec.KeyVersion, rec.CreatedBy, rec.CreatedAt, rec.UpdatedAt))
		}
		return out, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Secret, 0)
	for _, rec := range s.items[orgID] {
		if rec == nil || rec.DeletedAt != nil {
			continue
		}
		out = append(out, meta(rec.Name, rec.KeyVersion, rec.CreatedBy, rec.CreatedAt, rec.UpdatedAt))
	}
	// Deterministic order (map iteration is random): by name ASC, matching the
	// SQL ORDER BY name ASC.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Name < out[j-1].Name; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

// Get returns metadata of one live secret within one tenant.
func (s *Service) Get(ctx context.Context, orgID, name string) (*Secret, error) {
	if s == nil {
		return nil, errors.New("secrets: service is nil")
	}
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgRequired
	}
	norm, err := validateName(name)
	if err != nil {
		return nil, err
	}
	if s.store != nil {
		rec, err := s.store.GetMeta(ctx, orgID, norm)
		if err != nil {
			return nil, err
		}
		return meta(rec.Name, rec.KeyVersion, rec.CreatedBy, rec.CreatedAt, rec.UpdatedAt), nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.items[orgID][norm]
	if !ok || rec == nil || rec.DeletedAt != nil {
		return nil, ErrNotFound
	}
	return meta(rec.Name, rec.KeyVersion, rec.CreatedBy, rec.CreatedAt, rec.UpdatedAt), nil
}

// Resolve is the RUNTIME SEAM (issue #25): connectors/tools call it at
// execution time to turn (orgID, name) into plaintext. It decrypts through
// the same authenticated path as every other read; unknown/foreign names and
// tombstones are ErrNotFound, and key/tamper failures are ErrDecryptFailed.
func (s *Service) Resolve(ctx context.Context, orgID, name string) (string, error) {
	if s == nil {
		return "", errors.New("secrets: service is nil")
	}
	if strings.TrimSpace(orgID) == "" {
		return "", ErrOrgRequired
	}
	norm, err := validateName(name)
	if err != nil {
		return "", err
	}
	if s.store != nil {
		rec, err := s.store.GetEncrypted(ctx, orgID, norm)
		if err != nil {
			return "", err
		}
		return s.cipher.OpenParts(rec.KeyVersion, rec.Nonce, rec.Ciphertext)
	}
	s.mu.RLock()
	rec, ok := s.items[orgID][norm]
	if !ok || rec == nil || rec.DeletedAt != nil {
		s.mu.RUnlock()
		return "", ErrNotFound
	}
	envelope := rec.Envelope
	s.mu.RUnlock()
	return s.cipher.Open(envelope)
}
