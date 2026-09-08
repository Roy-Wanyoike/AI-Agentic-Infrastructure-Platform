// Package notifications implements operational alerting on top of the AgentOS
// event bus and the outbound webhooks delivery pipeline (issue #83).
//
// The platform previously shipped two disconnected pieces: this package (a
// dead in-memory message store with zero callers) and the webhooks service
// (signed outbound delivery with retries + status records). Issue #83 reshapes
// notifications into the missing link between them:
//
//   - A Subscription binds an EXISTING webhook endpoint of one organization to
//     a set of event types from the pinned contract (internal/events). There
//     is no email/SMTP infrastructure — a "notification" IS an org-scoped
//     webhook delivery of an operational event.
//   - Service implements events.Subscriber (the platform subscription seam) as
//     a filtering proxy: it only ever surfaces the pinned operational alert
//     types (AlertEventTypes), never the full event firehose.
//   - Service.Run consumes that stream and, for every event, looks up the
//     matching subscriptions of the event's tenant and dispatches the event to
//     each subscription's webhook THROUGH the webhooks delivery path
//     (webhooks.Worker.Deliver — same HMAC signing, retry/backoff and
//     per-attempt delivery status records as ordinary webhook traffic;
//     delivery failures surface via GET /webhooks/{id}/deliveries).
//
// Dual-mode per platform convention:
//
//	NewService(...)                    — in-memory maps (zero infrastructure)
//	NewServiceWithStore(store, ...)    — Postgres-backed (migration 024)
package notifications

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"agentos/internal/events"
	"agentos/internal/webhooks"
)

// neverMatchingEventType is a placeholder subscription filter that no
// contract-valid event can carry (events publishers validate against
// events.AllEventTypes). It guarantees a Subscribe request that names no
// pinned alert type receives NOTHING instead of accidentally widening to
// "all event types" (the publisher's empty-filter semantics).
const neverMatchingEventType = "notifications.filter.match-none"

// AlertEventTypes lists the pinned event types the notifications subscriber
// reacts to (issue #83: run failure, approval required, canary rollback).
//
// Canary rollback note: the pinned contract (internal/events/events.go) has no
// "canary.rolledback" type; a canary rollback surfaces as a failed deployment,
// so deployment.failed (events.EventDeploymentFailed) is the pinned constant
// subscribed to here.
func AlertEventTypes() []string {
	return []string{
		events.EventRunFailed,
		events.EventApprovalRequested,
		events.EventDeploymentFailed,
	}
}

// IsAlertEventType reports whether t is one of the pinned operational alert
// types this package subscribes to.
func IsAlertEventType(t string) bool {
	for _, known := range AlertEventTypes() {
		if known == t {
			return true
		}
	}
	return false
}

// Subscription is an org-scoped routing rule: deliver the listed operational
// event types to one existing webhook endpoint of the SAME organization.
type Subscription struct {
	ID             string
	OrganizationID string
	WebhookID      string
	EventTypes     []string
	CreatedAt      time.Time
}

var (
	// ErrSubscriptionNotFound is returned when the id does not exist in the
	// caller's tenant (existence is never leaked across tenants).
	ErrSubscriptionNotFound = errors.New("notification subscription not found")
	// ErrInvalidSubscription is returned for contract violations on create
	// (missing fields, unknown event types — the message lists the valid ones).
	ErrInvalidSubscription = errors.New("invalid notification subscription")
)

// Store persists subscriptions. Implementations MUST scope every query by
// organization_id (tenant guard).
type Store interface {
	// CreateSubscription inserts the subscription row within one tenant.
	CreateSubscription(ctx context.Context, sub *Subscription) error
	// GetSubscription fetches one subscription strictly within one tenant.
	GetSubscription(ctx context.Context, orgID, id string) (*Subscription, error)
	// ListSubscriptions returns all subscriptions of one tenant, newest first.
	ListSubscriptions(ctx context.Context, orgID string) ([]*Subscription, error)
	// DeleteSubscription removes one subscription within one tenant.
	DeleteSubscription(ctx context.Context, orgID, id string) error
}

// Dispatcher is the webhooks delivery seam. It is satisfied by
// *webhooks.Worker (Worker.Deliver): the delivery reuses the worker's signing,
// retry/backoff and delivery-status recording instead of forking that logic.
type Dispatcher interface {
	Deliver(ctx context.Context, wh *webhooks.Webhook, ev events.Event)
}

// Service manages alert subscriptions and reacts to operational events.
// It implements events.Subscriber (pinned-type filtering proxy over the
// platform event source) and is safe for concurrent use.
type Service struct {
	mu   sync.Mutex
	subs map[string]*Subscription // used when store == nil (in-memory mode)

	store   Store
	source  events.Subscriber
	hooks   *webhooks.Service
	deliver Dispatcher
	logr    *slog.Logger
}

// Option tunes the Service.
type Option func(*Service)

// WithLogger sets the logger (wiring may pass the app logger; default discards).
func WithLogger(l *slog.Logger) Option {
	return func(s *Service) {
		if l != nil {
			s.logr = l
		}
	}
}

// NewService returns the in-memory service (zero-infrastructure mode).
//
//	source   — the platform event publisher (must implement events.Subscriber;
//	            cmd/api wiring passes a.publisher.(events.Subscriber))
//	hooks    — the webhooks service; used to validate that a subscription's
//	            target webhook exists in the SAME organization and is active
//	deliver  — the delivery seam (webhooks worker); may be nil in tests, in
//	            which case matched events are logged and dropped
func NewService(source events.Subscriber, hooks *webhooks.Service, deliver Dispatcher, opts ...Option) *Service {
	return newService(nil, source, hooks, deliver, opts...)
}

// NewServiceWithStore returns a service whose source of truth is the durable
// store (migration 024); see NewService for the remaining parameters.
func NewServiceWithStore(store Store, source events.Subscriber, hooks *webhooks.Service, deliver Dispatcher, opts ...Option) *Service {
	return newService(store, source, hooks, deliver, opts...)
}

func newService(store Store, source events.Subscriber, hooks *webhooks.Service, deliver Dispatcher, opts ...Option) *Service {
	s := &Service{
		subs:    make(map[string]*Subscription),
		store:   store,
		source:  source,
		hooks:   hooks,
		deliver: deliver,
		logr:    slog.New(slog.DiscardHandler),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// compile-time proof that Service satisfies the platform subscriber seam.
var _ events.Subscriber = (*Service)(nil)

// Subscribe implements events.Subscriber as a filtering proxy over the
// platform event source: the returned channel only ever carries the pinned
// operational alert types (AlertEventTypes). nil/empty types selects exactly
// the pinned set; a selection naming no pinned type receives nothing (it never
// widens to "all events"). cancel is idempotent and closes the channel.
func (s *Service) Subscribe(types []string) (<-chan events.Event, func(), error) {
	if s.source == nil {
		return nil, nil, errors.New("notifications: no event source configured")
	}
	return s.source.Subscribe(filterAlertTypes(types))
}

// filterAlertTypes narrows a requested event-type selection to the pinned
// alert contract (see Subscribe).
func filterAlertTypes(types []string) []string {
	if len(types) == 0 {
		return append([]string(nil), AlertEventTypes()...)
	}
	out := make([]string, 0, len(types))
	seen := make(map[string]struct{}, len(types))
	for _, t := range types {
		t = strings.TrimSpace(t)
		if t == "" || !IsAlertEventType(t) {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	if len(out) == 0 {
		return []string{neverMatchingEventType}
	}
	return out
}

// Run consumes the pinned operational alert stream until ctx is cancelled or
// the source channel closes. For every event it fans the event out to each
// matching subscription's webhook through the webhooks delivery path.
func (s *Service) Run(ctx context.Context) error {
	ch, cancel, err := s.Subscribe(nil) // nil = exactly the pinned alert types
	if err != nil {
		return fmt.Errorf("notifications: subscribe: %w", err)
	}
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-ch:
			if !ok {
				return nil
			}
			s.processEvent(ctx, ev)
		}
	}
}

// processEvent dispatches one event to every matching subscription of the
// event's tenant. Never blocks on HTTP: the actual delivery is handed to the
// webhooks worker, which runs it on its own goroutine.
func (s *Service) processEvent(ctx context.Context, ev events.Event) {
	if ev.TenantID == "" || ev.Type == "" || !IsAlertEventType(ev.Type) {
		return
	}
	subs, err := s.matchingSubscriptions(ctx, ev.TenantID, ev.Type)
	if err != nil {
		s.logr.Error("notifications: match subscriptions failed", "error", err, "event_id", ev.ID)
		return
	}
	for _, sub := range subs {
		// FK-ish guard at dispatch time: the webhook must still exist in the
		// event's tenant (cross-tenant/unknown ids surface as not-found).
		wh, err := s.hooks.GetWebhook(ctx, ev.TenantID, sub.WebhookID)
		if err != nil {
			if errors.Is(err, webhooks.ErrWebhookNotFound) {
				s.logr.Warn("notifications: subscription target webhook missing",
					"subscription_id", sub.ID, "webhook_id", sub.WebhookID, "org_id", ev.TenantID)
				continue
			}
			s.logr.Error("notifications: resolve subscription webhook failed",
				"subscription_id", sub.ID, "error", err)
			continue
		}
		if wh.Status != webhooks.StatusActive {
			s.logr.Warn("notifications: subscription target webhook disabled, skipping",
				"subscription_id", sub.ID, "webhook_id", wh.ID)
			continue
		}
		s.dispatch(ctx, wh, ev)
	}
}

// dispatch hands one (webhook, event) pair to the webhooks delivery path.
// Async by construction: Worker.Deliver returns immediately.
func (s *Service) dispatch(ctx context.Context, wh *webhooks.Webhook, ev events.Event) {
	if s.deliver == nil {
		s.logr.Warn("notifications: no delivery seam configured, dropping event",
			"event_id", ev.ID, "webhook_id", wh.ID)
		return
	}
	s.deliver.Deliver(ctx, wh, ev)
}

// matchingSubscriptions returns the tenant's subscriptions subscribed to the
// event type (org-scoped by the store / in-memory map).
func (s *Service) matchingSubscriptions(ctx context.Context, orgID, eventType string) ([]*Subscription, error) {
	all, err := s.ListSubscriptions(ctx, orgID)
	if err != nil {
		return nil, err
	}
	out := make([]*Subscription, 0)
	for _, sub := range all {
		for _, t := range sub.EventTypes {
			if t == eventType {
				out = append(out, sub)
				break
			}
		}
	}
	return out, nil
}

// CreateSubscription validates the input and creates a subscription. The
// target webhook MUST exist in the SAME organization (webhooks.ErrWebhookNotFound
// otherwise) and every event type MUST be one of events.AllEventTypes (the
// returned error message lists the valid ones).
func (s *Service) CreateSubscription(ctx context.Context, orgID, webhookID string, eventTypes []string) (*Subscription, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, fmt.Errorf("%w: organization id is required", ErrInvalidSubscription)
	}
	if strings.TrimSpace(webhookID) == "" {
		return nil, fmt.Errorf("%w: webhook_id is required", ErrInvalidSubscription)
	}
	filtered, err := validateEventTypes(eventTypes)
	if err != nil {
		return nil, err
	}
	if s.hooks == nil {
		return nil, errors.New("notifications: webhooks service is required")
	}
	// FK-ish validation: the webhook must exist in the caller's organization.
	if _, err := s.hooks.GetWebhook(ctx, orgID, webhookID); err != nil {
		if errors.Is(err, webhooks.ErrWebhookNotFound) {
			return nil, fmt.Errorf("webhook %q not found in organization: %w", webhookID, webhooks.ErrWebhookNotFound)
		}
		return nil, err
	}

	now := time.Now().UTC()
	sub := &Subscription{
		ID:             uuid.NewString(),
		OrganizationID: orgID,
		WebhookID:      webhookID,
		EventTypes:     filtered,
		CreatedAt:      now,
	}
	if s.store != nil {
		if err := s.store.CreateSubscription(ctx, sub); err != nil {
			return nil, err
		}
	} else {
		s.mu.Lock()
		s.subs[sub.ID] = sub
		s.mu.Unlock()
	}
	return sub, nil
}

// validateEventTypes trims, de-duplicates and validates the requested event
// types against the pinned contract. At least one type is required (explicit
// subscription semantics; use the webhooks API directly for wildcard sinks).
func validateEventTypes(eventTypes []string) ([]string, error) {
	filtered := make([]string, 0, len(eventTypes))
	seen := make(map[string]struct{}, len(eventTypes))
	for _, t := range eventTypes {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if !events.IsValidEventType(t) {
			return nil, fmt.Errorf("%w: unknown event type %q; valid event types: %s",
				ErrInvalidSubscription, t, strings.Join(events.AllEventTypes(), ", "))
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		filtered = append(filtered, t)
	}
	if len(filtered) == 0 {
		return nil, fmt.Errorf("%w: at least one event type is required; valid event types: %s",
			ErrInvalidSubscription, strings.Join(events.AllEventTypes(), ", "))
	}
	return filtered, nil
}

// GetSubscription resolves one subscription within one tenant.
func (s *Service) GetSubscription(ctx context.Context, orgID, id string) (*Subscription, error) {
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(id) == "" {
		return nil, ErrSubscriptionNotFound
	}
	if s.store != nil {
		return s.store.GetSubscription(ctx, orgID, id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sub, ok := s.subs[id]
	if !ok || sub.OrganizationID != orgID {
		return nil, ErrSubscriptionNotFound
	}
	return sub, nil
}

// ListSubscriptions returns all subscriptions of one tenant, newest first.
func (s *Service) ListSubscriptions(ctx context.Context, orgID string) ([]*Subscription, error) {
	if strings.TrimSpace(orgID) == "" {
		return []*Subscription{}, nil
	}
	if s.store != nil {
		return s.store.ListSubscriptions(ctx, orgID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Subscription, 0)
	for _, sub := range s.subs {
		if sub.OrganizationID == orgID {
			out = append(out, sub)
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].CreatedAt.After(out[j-1].CreatedAt); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

// DeleteSubscription removes one subscription within one tenant. Unknown or
// foreign-tenant ids surface as ErrSubscriptionNotFound.
func (s *Service) DeleteSubscription(ctx context.Context, orgID, id string) error {
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(id) == "" {
		return ErrSubscriptionNotFound
	}
	if s.store != nil {
		return s.store.DeleteSubscription(ctx, orgID, id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sub, ok := s.subs[id]
	if !ok || sub.OrganizationID != orgID {
		return ErrSubscriptionNotFound
	}
	delete(s.subs, id)
	return nil
}
