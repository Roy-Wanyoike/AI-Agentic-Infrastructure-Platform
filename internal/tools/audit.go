package tools

import (
	"context"
	"sync"
)

// audit.go — the audit seam for tools that must record their activity
// (issue #111: every database_query call is audited with the SQL text hash
// and the row count; raw parameter values are NEVER audited).
//
// The seam is declared here (not imported from internal/audit) following the
// package's structural-interface convention (SandboxExecutor, MCPToolCaller):
// internal/tools stays dependency-free while the orchestrator wires the
// platform audit service through a tiny adapter at the registration site.
// AuditEvent intentionally mirrors the (actor, action, organization_id,
// resource, metadata) shape of audit.Service.LogCtx so the adapter is a
// one-liner.

// AuditEvent is one audit record emitted by a tool.
type AuditEvent struct {
	// Actor is the identity that performed the action. Tools use
	// "tool:<registry name>" (no user/session identity is available at tool
	// execution time; the run-level actor is on the surrounding audit trail).
	Actor string
	// Action is the namespaced verb, e.g. "tools.database_query".
	Action string
	// OrganizationID scopes the record to one tenant ("" = process-level).
	OrganizationID string
	// Resource is the specific target, e.g. the connection name.
	Resource string
	// Metadata carries structured, SECRET-FREE details. Callers must never
	// place parameter values, credentials or raw statement text here.
	Metadata map[string]any
}

// AuditSink receives tool audit events. Implementations must be safe for
// concurrent use and MUST NOT let audit failures break tool execution
// (callers treat recording as best-effort, matching the platform audit
// service's best-effort persistence contract).
type AuditSink interface {
	RecordToolAudit(ctx context.Context, event AuditEvent) error
}

// AuditSinkFunc adapts a plain function (and, transitively, any service whose
// method signature drifts from the interface) into an AuditSink.
type AuditSinkFunc func(ctx context.Context, event AuditEvent) error

// RecordToolAudit implements AuditSink.
func (f AuditSinkFunc) RecordToolAudit(ctx context.Context, event AuditEvent) error {
	if f == nil {
		return nil
	}
	return f(ctx, event)
}

// MemoryAuditSink is a bounded in-memory AuditSink used as the zero-infrastructure
// default in RegisterBuiltinCatalog. It keeps the most recent `keep` events
// (default and hard cap: DefaultMemoryAuditKeep) in insertion order so tests
// and embedded deployments can inspect what tools recorded without any
// backing store. It is process-local by design: durable audit trails are the
// audit service's job (wire it via RegisterBuiltinCatalogWithOptions).
type MemoryAuditSink struct {
	mu      sync.Mutex
	keep    int
	events  []AuditEvent
	dropped int
}

// DefaultMemoryAuditKeep bounds the in-memory ring (small on purpose: this is
// a diagnostics buffer, not a durable trail).
const DefaultMemoryAuditKeep = 500

// NewMemoryAuditSink returns a bounded sink keeping the most recent max(keep, 1)
// events. keep <= 0 selects DefaultMemoryAuditKeep.
func NewMemoryAuditSink(keep int) *MemoryAuditSink {
	if keep <= 0 {
		keep = DefaultMemoryAuditKeep
	}
	return &MemoryAuditSink{keep: keep}
}

// RecordToolAudit implements AuditSink. It never fails (errors would only
// discourage auditing); overflow evicts the oldest event and counts it.
func (s *MemoryAuditSink) RecordToolAudit(_ context.Context, event AuditEvent) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
	if len(s.events) > s.keep {
		s.events = s.events[len(s.events)-s.keep:]
		s.dropped++
	}
	return nil
}

// Events returns a copy of the recorded events, oldest first.
func (s *MemoryAuditSink) Events() []AuditEvent {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]AuditEvent, len(s.events))
	copy(out, s.events)
	return out
}

// Dropped reports how many events were evicted by the ring bound.
func (s *MemoryAuditSink) Dropped() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}
