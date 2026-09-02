// Package events defines the AgentOS standard event model and the
// publisher/subscriber plumbing every other service uses to emit and consume
// domain events (track 2-e).
//
// Three Publisher implementations ship out of the box:
//
//   - NoopPublisher:  zero-infrastructure mode (default when AGENTOS_NATS_URL
//     is unset) — Publish discards, Subscribe returns a never-firing channel.
//   - MemoryPublisher: in-process ring buffer (1000 events) + subscriber
//     channels; the default fallback when NATS is unreachable.
//   - NATSPublisher:  JetStream-backed, subject agentos.events.<event.type>.
//     The constructor returns an error when NATS is unreachable so callers can
//     fall back to the memory publisher.
//
// NewFromEnv picks the right implementation from AGENTOS_NATS_URL.
package events

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Event type constants pinned by the wave-2 contract (track 2-e). Every
// published event MUST use one of these types.
const (
	EventAgentCreated        = "agent.created"
	EventAgentUpdated        = "agent.updated"
	EventRunStarted          = "run.started"
	EventRunCompleted        = "run.completed"
	EventRunFailed           = "run.failed"
	EventRunCancelled        = "run.cancelled"
	EventStepStarted         = "step.started"
	EventStepCompleted       = "step.completed"
	EventApprovalRequested   = "approval.requested"
	EventApprovalDecided     = "approval.decided"
	EventDeploymentCompleted = "deployment.completed"
	EventDeploymentFailed    = "deployment.failed"
	EventWebhookReceived     = "webhook.received"
)

// AllEventTypes lists every valid event type (used for validation and by the
// delivery worker when subscribing to "all events").
func AllEventTypes() []string {
	return []string{
		EventAgentCreated,
		EventAgentUpdated,
		EventRunStarted,
		EventRunCompleted,
		EventRunFailed,
		EventRunCancelled,
		EventStepStarted,
		EventStepCompleted,
		EventApprovalRequested,
		EventApprovalDecided,
		EventDeploymentCompleted,
		EventDeploymentFailed,
		EventWebhookReceived,
	}
}

// IsValidEventType reports whether t is one of the pinned contract event types.
func IsValidEventType(t string) bool {
	for _, known := range AllEventTypes() {
		if known == t {
			return true
		}
	}
	return false
}

// Resource identifies the domain object an event is about.
type Resource struct {
	Type string `json:"type,omitempty"`
	ID   string `json:"id,omitempty"`
}

// Event is the standard AgentOS event envelope. TenantID carries the
// organization_id tenant scope (multi-tenancy rule); consumers MUST NOT
// deliver an event outside its tenant.
type Event struct {
	ID          string         `json:"id"`
	Type        string         `json:"type"`
	TenantID    string         `json:"tenant_id"`
	ProjectID   string         `json:"project_id,omitempty"`
	Timestamp   time.Time      `json:"timestamp"`
	Resource    Resource       `json:"resource,omitempty"`
	ExecutionID string         `json:"execution_id,omitempty"`
	TraceID     string         `json:"trace_id,omitempty"`
	Payload     map[string]any `json:"payload,omitempty"`
}

var (
	// ErrInvalidEvent is returned when a publisher receives an event that
	// violates the contract (unknown type or missing tenant scope).
	ErrInvalidEvent = errors.New("events: invalid event")
	// ErrSubscribeClosed is returned when subscribing to a closed publisher.
	ErrSubscribeClosed = errors.New("events: publisher is closed")
)

// Publisher accepts events for distribution. Implementations must be safe for
// concurrent use.
type Publisher interface {
	Publish(ctx context.Context, event Event) error
}

// Subscriber receives events matching a type filter. An empty (or nil) types
// slice subscribes to ALL event types. The returned cancel func unsubscribes
// and releases resources; the channel is closed after cancel. Implementations
// must be safe for concurrent use.
type Subscriber interface {
	Subscribe(types []string) (<-chan Event, func(), error)
}

// NewEvent builds a contract-valid event with a fresh UUID and UTC timestamp.
// It is the convenience constructor other services should use when publishing.
func NewEvent(eventType, tenantID string, resourceType, resourceID string, payload map[string]any) Event {
	return Event{
		ID:        uuid.NewString(),
		Type:      eventType,
		TenantID:  tenantID,
		Timestamp: time.Now().UTC(),
		Resource:  Resource{Type: resourceType, ID: resourceID},
		Payload:   payload,
	}
}

// ensureDefaults fills in the identity/timestamp fields when a producer left
// them empty so every persisted/published event is contract-complete.
func ensureDefaults(e *Event) {
	if e.ID == "" {
		e.ID = uuid.NewString()
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}
	e.Timestamp = e.Timestamp.UTC()
}

// validate enforces the contract invariants shared by all publishers.
func validate(e *Event) error {
	if e == nil {
		return ErrInvalidEvent
	}
	if e.Type == "" || !IsValidEventType(e.Type) {
		return ErrInvalidEvent
	}
	if e.TenantID == "" {
		return ErrInvalidEvent
	}
	return nil
}
