package apikeys

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type APIKey struct {
	ID        string
	Name      string
	Value     string
	Hash      string
	OrgID     string
	UserID    string
	Prefix    string
	CreatedAt time.Time
	Revoked   bool
}

var ErrKeyNotFound = errors.New("api key not found")

// Store persists API keys. Lookup by key hash resolves the tenant from the
// credential itself (the hash is globally unique); revocation and listing are
// guarded by organization_id.
type Store interface {
	// CreateKey inserts the key row with its organization_id tenant scope.
	CreateKey(ctx context.Context, key *APIKey) error
	// GetKeyByHash resolves a key by its SHA-256 hash (authentication path).
	GetKeyByHash(ctx context.Context, hash string) (*APIKey, error)
	// GetKeyByID resolves a key by primary key (trusted internal path used to
	// resolve the tenant for legacy context-free revocations).
	GetKeyByID(ctx context.Context, id string) (*APIKey, error)
	// RevokeKey revokes a key within one tenant (organization_id guard).
	RevokeKey(ctx context.Context, orgID, id string) error
	// ListKeys returns the keys of exactly one tenant (organization_id guard).
	ListKeys(ctx context.Context, orgID string) ([]*APIKey, error)
}

type Service struct {
	mu    sync.Mutex
	keys  map[string]*APIKey
	store Store
}

func NewService() *Service {
	return &Service{keys: make(map[string]*APIKey)}
}

// NewServiceWithStore returns a service backed by a durable store; the store
// is the source of truth and the in-memory map acts as a write-through cache.
func NewServiceWithStore(store Store) *Service {
	s := NewService()
	s.store = store
	return s
}

func (s *Service) SetStore(store Store) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store = store
}

func generateAPIKeySecret() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "ak_" + fmt.Sprintf("%d", time.Now().UTC().UnixNano()) + "_fallback"
	}
	return "ak_" + hex.EncodeToString(buf)
}

func hashAPIKey(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// Create is the legacy context-free entry point; see CreateCtx.
func (s *Service) Create(orgID, userID, name string) (*APIKey, error) {
	return s.CreateCtx(context.Background(), orgID, userID, name)
}

// CreateCtx generates a new API key and persists it for one tenant.
func (s *Service) CreateCtx(ctx context.Context, orgID, userID, name string) (*APIKey, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, errors.New("organization id is required")
	}
	if strings.TrimSpace(userID) == "" {
		return nil, errors.New("user id is required")
	}
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("key name is required")
	}
	secret := generateAPIKeySecret()
	prefix := secret
	if len(prefix) > 12 {
		prefix = prefix[:12]
	}
	key := &APIKey{
		ID:        uuid.NewString(),
		Name:      name,
		Value:     secret,
		Hash:      hashAPIKey(secret),
		OrgID:     orgID,
		UserID:    userID,
		Prefix:    prefix,
		CreatedAt: time.Now().UTC(),
		Revoked:   false,
	}
	if s.store != nil {
		if err := s.store.CreateKey(ctx, key); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys[key.ID] = key
	return key, nil
}

// Revoke is the legacy context-free entry point; see RevokeCtx.
func (s *Service) Revoke(id string) error {
	return s.RevokeCtx(context.Background(), "", id)
}

// RevokeCtx revokes a key. When orgID is empty (trusted internal path) the
// tenant is resolved from the key row first; the actual update is still
// executed with the organization_id guard.
func (s *Service) RevokeCtx(ctx context.Context, orgID, id string) error {
	if strings.TrimSpace(id) == "" {
		return ErrKeyNotFound
	}
	if s.store != nil {
		if strings.TrimSpace(orgID) == "" {
			key, err := s.store.GetKeyByID(ctx, id)
			if err != nil {
				return err
			}
			orgID = key.OrgID
		}
		if err := s.store.RevokeKey(ctx, orgID, id); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key, ok := s.keys[id]
	if !ok {
		if s.store != nil {
			return nil
		}
		return ErrKeyNotFound
	}
	key.Revoked = true
	return nil
}

// Validate is the legacy context-free entry point; see ValidateCtx.
func (s *Service) Validate(value string) (*APIKey, bool) {
	return s.ValidateCtx(context.Background(), value)
}

// ValidateCtx resolves an API key by the hash of the presented secret.
func (s *Service) ValidateCtx(ctx context.Context, value string) (*APIKey, bool) {
	if strings.TrimSpace(value) == "" {
		return nil, false
	}
	hash := hashAPIKey(value)
	if s.store != nil {
		key, err := s.store.GetKeyByHash(ctx, hash)
		if err != nil || key == nil {
			return nil, false
		}
		return key, true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, key := range s.keys {
		if !key.Revoked && key.Hash == hash {
			return key, true
		}
	}
	return nil, false
}

// ListKeysCtx returns the keys of exactly one tenant (organization_id guard).
func (s *Service) ListKeysCtx(ctx context.Context, orgID string) ([]*APIKey, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, errors.New("organization id is required")
	}
	if s.store != nil {
		return s.store.ListKeys(ctx, orgID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*APIKey, 0)
	for _, key := range s.keys {
		if key.OrgID == orgID {
			out = append(out, key)
		}
	}
	return out, nil
}

// jsonParam marshals a value for a JSONB column; nil stays SQL NULL.
func jsonParam(v any) any {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return string(b)
}
