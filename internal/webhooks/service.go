// Package webhooks manages outbound webhook endpoints and their delivery
// records (track 2-e). It is a dual-mode service:
//
//	NewService()                 — in-memory maps + mutex (zero-infrastructure)
//	NewServiceWithStore(store)   — Postgres-backed source of truth (migration 010)
//
// Secrets: POST /webhooks/create returns the HMAC secret EXACTLY ONCE. The
// durable record stores only a SHA-256 hash of the secret (secret_hash). The
// delivery worker still has to sign every payload, so the raw secret is
// derived deterministically at request time as
//
//	hex(HMAC-SHA256(signingKey, "webhook:"+webhookID))
//
// with signingKey from AGENTOS_WEBHOOK_SIGNING_KEY (dev default below). This
// keeps the secret out of the database while allowing re-derivation after
// restarts. Change the signing key and existing endpoints stop verifying.
package webhooks

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"agentos/internal/events"
)

// Webhook lifecycle statuses.
const (
	StatusActive   = "active"
	StatusDisabled = "disabled"
)

// Delivery statuses (contract: delivered|failed|retrying).
const (
	DeliveryDelivered = "delivered"
	DeliveryFailed    = "failed"
	DeliveryRetrying  = "retrying"
)

// DefaultSigningKey is used when AGENTOS_WEBHOOK_SIGNING_KEY is unset (dev /
// zero-infrastructure mode). Set a real key in production deployments.
const DefaultSigningKey = "agentos-dev-webhook-signing-key"

// deliveryRetention bounds the in-memory per-webhook delivery history.
const deliveryRetention = 100

// Webhook is an outbound endpoint subscribed to a set of event types. An empty
// Events list means "all event types".
type Webhook struct {
	ID             string
	OrganizationID string
	URL            string
	Events         []string
	SecretHash     string // SHA-256 hex of the signing secret; raw secret is never persisted
	Status         string // active | disabled
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Delivery is the delivery attempt history for one (webhook, event) pair. The
// record is created on the first attempt and updated after every retry, so
// Attempts/LastStatusCode/LatencyMS/Error always reflect the latest attempt.
type Delivery struct {
	ID             string
	WebhookID      string
	OrganizationID string
	EventID        string
	EventType      string
	Status         string // delivered | failed | retrying
	Attempts       int
	LastStatusCode int
	LatencyMS      int64
	Error          string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

var (
	// ErrWebhookNotFound is returned when the id does not exist in the caller's tenant.
	ErrWebhookNotFound = errors.New("webhook not found")
	// ErrInvalidWebhook is returned for contract violations on create.
	ErrInvalidWebhook = errors.New("invalid webhook")
)

// Store persists webhooks and their delivery records. Implementations MUST
// scope every query by organization_id (tenant guard).
type Store interface {
	// CreateWebhook inserts the webhook row within one tenant.
	CreateWebhook(ctx context.Context, webhook *Webhook) error
	// GetWebhook fetches one webhook strictly within one tenant.
	GetWebhook(ctx context.Context, orgID, id string) (*Webhook, error)
	// ListWebhooks returns all webhooks of exactly one tenant.
	ListWebhooks(ctx context.Context, orgID string) ([]*Webhook, error)
	// DeleteWebhook removes one webhook within one tenant.
	DeleteWebhook(ctx context.Context, orgID, id string) error
	// UpdateWebhook persists mutable webhook fields (status) within one tenant.
	UpdateWebhook(ctx context.Context, webhook *Webhook) error
	// UpsertDelivery inserts a delivery record or updates it by id (attempt
	// bookkeeping mutates the same row).
	UpsertDelivery(ctx context.Context, delivery *Delivery) error
	// ListDeliveries returns the most recent deliveries of one webhook within
	// one tenant, newest first, at most limit rows.
	ListDeliveries(ctx context.Context, orgID, webhookID string, limit int) ([]*Delivery, error)
}

// Service manages webhook registrations and delivery records.
type Service struct {
	mu         sync.Mutex
	webhooks   map[string]*Webhook
	deliveries map[string][]*Delivery // webhookID -> recent deliveries (newest last)
	store      Store
	signingKey string
}

// NewService returns the in-memory service (zero-infrastructure mode).
func NewService() *Service {
	return &Service{
		webhooks:   make(map[string]*Webhook),
		deliveries: make(map[string][]*Delivery),
		signingKey: DefaultSigningKey,
	}
}

// NewServiceWithStore returns a service whose source of truth is the durable
// store; in-memory maps stay as a write-through cache for the worker path.
func NewServiceWithStore(store Store) *Service {
	s := NewService()
	s.store = store
	return s
}

// SetSigningKey overrides the key used to derive webhook secrets (wiring may
// set it from AGENTOS_WEBHOOK_SIGNING_KEY). Changing it invalidates secrets
// already handed out for webhooks created under the previous key.
func (s *Service) SetSigningKey(key string) {
	if s == nil || strings.TrimSpace(key) == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.signingKey = key
}

// CreateWebhook validates the input, generates the endpoint and returns it
// together with the raw signing secret — the ONLY place the raw secret is
// ever exposed. The stored record carries the SHA-256 hash only.
func (s *Service) CreateWebhook(ctx context.Context, orgID, rawURL string, eventTypes []string) (*Webhook, string, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, "", fmt.Errorf("%w: organization id is required", ErrInvalidWebhook)
	}
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, "", fmt.Errorf("%w: url must be an absolute http(s) URL", ErrInvalidWebhook)
	}
	filtered := make([]string, 0, len(eventTypes))
	seen := make(map[string]struct{}, len(eventTypes))
	for _, t := range eventTypes {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if !events.IsValidEventType(t) {
			return nil, "", fmt.Errorf("%w: unknown event type %q", ErrInvalidWebhook, t)
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		filtered = append(filtered, t)
	}

	now := time.Now().UTC()
	webhook := &Webhook{
		ID:             uuid.NewString(),
		OrganizationID: orgID,
		URL:            parsed.String(),
		Events:         filtered,
		Status:         StatusActive,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	secret := s.deriveSecret(webhook.ID)
	webhook.SecretHash = HashSecret(secret)

	if s.store != nil {
		if err := s.store.CreateWebhook(ctx, webhook); err != nil {
			return nil, "", err
		}
	}
	s.mu.Lock()
	s.webhooks[webhook.ID] = webhook
	s.mu.Unlock()
	return webhook, secret, nil
}

// GetWebhook resolves one webhook within one tenant (organization_id guard).
func (s *Service) GetWebhook(ctx context.Context, orgID, id string) (*Webhook, error) {
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(id) == "" {
		return nil, ErrWebhookNotFound
	}
	if s.store != nil {
		return s.store.GetWebhook(ctx, orgID, id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	wh, ok := s.webhooks[id]
	if !ok || wh.OrganizationID != orgID {
		return nil, ErrWebhookNotFound
	}
	return wh, nil
}

// ListWebhooks returns all webhooks of one tenant, newest first.
func (s *Service) ListWebhooks(ctx context.Context, orgID string) ([]*Webhook, error) {
	if strings.TrimSpace(orgID) == "" {
		return []*Webhook{}, nil
	}
	if s.store != nil {
		return s.store.ListWebhooks(ctx, orgID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Webhook, 0)
	for _, wh := range s.webhooks {
		if wh.OrganizationID == orgID {
			out = append(out, wh)
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].CreatedAt.After(out[j-1].CreatedAt); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

// DeleteWebhook removes one webhook (and, in Postgres, cascades its delivery
// records) within one tenant.
func (s *Service) DeleteWebhook(ctx context.Context, orgID, id string) error {
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(id) == "" {
		return ErrWebhookNotFound
	}
	if s.store != nil {
		if err := s.store.DeleteWebhook(ctx, orgID, id); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	wh, ok := s.webhooks[id]
	if !ok || wh.OrganizationID != orgID {
		if s.store != nil {
			return nil // store already deleted; nothing cached
		}
		return ErrWebhookNotFound
	}
	delete(s.webhooks, id)
	delete(s.deliveries, id)
	return nil
}

// SetWebhookStatus flips a webhook between active|disabled within one tenant.
// Disabled endpoints are skipped by the delivery worker.
func (s *Service) SetWebhookStatus(ctx context.Context, orgID, id, status string) error {
	if status != StatusActive && status != StatusDisabled {
		return fmt.Errorf("%w: status must be active|disabled", ErrInvalidWebhook)
	}
	wh, err := s.GetWebhook(ctx, orgID, id)
	if err != nil {
		return err
	}
	wh.Status = status
	wh.UpdatedAt = time.Now().UTC()
	if s.store != nil {
		if err := s.store.UpdateWebhook(ctx, wh); err != nil {
			return err
		}
	}
	s.mu.Lock()
	s.webhooks[wh.ID] = wh
	s.mu.Unlock()
	return nil
}

// WebhooksForEvent returns the ACTIVE webhooks of one tenant subscribed to
// eventType (empty Events list = wildcard subscription).
func (s *Service) WebhooksForEvent(ctx context.Context, orgID, eventType string) ([]*Webhook, error) {
	all, err := s.ListWebhooks(ctx, orgID)
	if err != nil {
		return nil, err
	}
	out := make([]*Webhook, 0)
	for _, wh := range all {
		if wh.Status != StatusActive {
			continue
		}
		if webhookMatches(wh, eventType) {
			out = append(out, wh)
		}
	}
	return out, nil
}

func webhookMatches(wh *Webhook, eventType string) bool {
	if len(wh.Events) == 0 {
		return true
	}
	for _, t := range wh.Events {
		if t == eventType {
			return true
		}
	}
	return false
}

// UpsertDelivery persists a delivery record (insert on first attempt, update
// on retries). Store mode writes through; memory mode keeps a bounded per-
// webhook history.
func (s *Service) UpsertDelivery(ctx context.Context, delivery *Delivery) error {
	if delivery == nil {
		return errors.New("webhooks: delivery is required")
	}
	now := time.Now().UTC()
	if delivery.CreatedAt.IsZero() {
		delivery.CreatedAt = now
	}
	delivery.UpdatedAt = now
	if s.store != nil {
		if err := s.store.UpsertDelivery(ctx, delivery); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if delivery.WebhookID == "" {
		return nil
	}
	history := s.deliveries[delivery.WebhookID]
	for i, d := range history {
		if d.ID == delivery.ID {
			cp := *delivery
			history[i] = &cp
			s.deliveries[delivery.WebhookID] = history
			return nil
		}
	}
	cp := *delivery
	history = append(history, &cp)
	if len(history) > deliveryRetention {
		history = history[len(history)-deliveryRetention:]
	}
	s.deliveries[delivery.WebhookID] = history
	return nil
}

// ListDeliveries returns the most recent deliveries of one webhook within one
// tenant (newest first).
func (s *Service) ListDeliveries(ctx context.Context, orgID, webhookID string, limit int) ([]*Delivery, error) {
	if _, err := s.GetWebhook(ctx, orgID, webhookID); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 50
	}
	if s.store != nil {
		return s.store.ListDeliveries(ctx, orgID, webhookID, limit)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	history := s.deliveries[webhookID]
	out := make([]*Delivery, 0, len(history))
	for i := len(history) - 1; i >= 0 && len(out) < limit; i-- {
		cp := *history[i]
		out = append(out, &cp)
	}
	return out, nil
}

// deriveSecret recomputes the raw signing secret for a webhook id. The value
// returned at creation time equals the value derived here at delivery time.
func (s *Service) deriveSecret(webhookID string) string {
	mac := hmac.New(sha256.New, []byte(s.signingKey))
	_, _ = mac.Write([]byte("webhook:" + webhookID))
	return hex.EncodeToString(mac.Sum(nil))
}

// secretForDelivery is the worker's signing key source for one webhook.
func (s *Service) secretForDelivery(wh *Webhook) string {
	return s.deriveSecret(wh.ID)
}

// HashSecret returns the SHA-256 hex hash stored in the durable record.
func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// VerifySecret checks a candidate raw secret against a stored hash.
func VerifySecret(secretHash, secret string) bool {
	if secret == "" || secretHash == "" {
		return false
	}
	return hmac.Equal([]byte(HashSecret(secret)), []byte(secretHash))
}

// SignPayload returns the X-AgentOS-Signature header value for a body:
// "sha256=<hex hmac-sha256(secret, body)>".
func SignPayload(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// VerifyPayload checks a signature header value against a body (constant-time).
func VerifyPayload(secret string, body []byte, signature string) bool {
	expected := SignPayload(secret, body)
	return hmac.Equal([]byte(expected), []byte(signature))
}

// RandomSecret generates a fresh crypto/rand secret (kept for callers that
// want an explicit secret instead of the derived scheme).
func RandomSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
