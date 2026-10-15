// Package batch implements the bulk-operation job tracker behind the issue
// #123 batch endpoints (POST /batch/runs, /batch/workflows/{id}/trigger,
// /batch/agents/update, /batch/agents/deploy, /batch/runs/cancel,
// /batch/secrets).
//
// The Service is deliberately domain-agnostic: the HTTP wiring layer
// (cmd/api/batch.go) turns each request item into a Unit — a closure that
// calls the real runs/agents/workflows/deployments/secrets services — and
// hands the list to Submit. This package owns everything the issue pins:
//
//   - job tracking: one batch record (id, org, type, per-item states
//     pending/running/succeeded/failed/cancelled, per-item error and result
//     metadata, timestamps);
//   - a goroutine pool honoring max_concurrency (default 4, hard cap 32) with
//     strict per-item isolation: one failing item never aborts the batch;
//   - cancel semantics: cancelling a batch cancels the in-flight items'
//     contexts and marks every not-yet-started item cancelled;
//   - progress events: every item transition emits an additive
//     "batch.updated" event {batch_id, completed, total, item_states} on the
//     EXISTING SSE stream seam (internal/streaming, keyed by the batch id) —
//     consumed via GET /batch/{id}/events;
//   - audit: batch.created / batch.completed rows through the same narrow
//     LogCtx audit seam every other track uses (best-effort, never fails a
//     batch);
//   - capped history: at most HistoryPerOrg batches retained per organization
//     (default 1000, oldest evicted first) and a hard per-batch item cap
//     (env AGENTOS_BATCH_MAX_ITEMS, default 100) enforced BEFORE any item
//     runs — atomic validation.
//
// Storage is in-memory BY DESIGN this round in BOTH API modes (no batch
// migration is assigned); the Store seam (Put/Get/Update) keeps the service
// dual-mode-ready so a durable store slots in without touching callers, and
// the per-org history cap is the MemoryStore's responsibility.
//
// Concurrency contract: every Batch mutation flows through Store.Update
// (single-writer under the store lock) and readers only ever receive copies,
// so a returned *Batch snapshot is safe to render/read while the batch keeps
// running. Item.Result maps are replaced wholesale on terminal transitions and
// never mutated afterwards.
package batch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"agentos/internal/audit"
)

// BatchType discriminates what kind of work a batch tracks. It is recorded on
// the batch record, on audit rows and (derived) on the SSE payloads.
type BatchType string

const (
	// TypeRuns: one run per item (one agent over N inputs, or N agents over a
	// shared input); each item creates a real run row and enqueues its
	// agent.run task on the queue service.
	TypeRuns BatchType = "runs"
	// TypeWorkflows: one workflow execution per item (one input each).
	TypeWorkflows BatchType = "workflows"
	// TypeAgentUpdate: one bulk agent patch applied per item.
	TypeAgentUpdate BatchType = "agent_update"
	// TypeAgentDeploy: one deployment created per item.
	TypeAgentDeploy BatchType = "agent_deploy"
	// TypeRunCancel: one run cancellation per item.
	TypeRunCancel BatchType = "run_cancel"
	// TypeSecrets: one secret creation per item (values never surface in any
	// response or event payload).
	TypeSecrets BatchType = "secrets"
)

// ItemStatus is the lifecycle state of one batch item.
type ItemStatus string

const (
	// ItemPending: the item has not started (waiting for a pool slot).
	ItemPending ItemStatus = "pending"
	// ItemRunning: the item's Unit closure is executing.
	ItemRunning ItemStatus = "running"
	// ItemSucceeded: the item completed without error.
	ItemSucceeded ItemStatus = "succeeded"
	// ItemFailed: the item returned an error (the message is recorded on the
	// item; the batch continues — per-item isolation).
	ItemFailed ItemStatus = "failed"
	// ItemCancelled: the item never ran or was aborted because the batch was
	// cancelled (its context fired).
	ItemCancelled ItemStatus = "cancelled"
)

// Batch-level lifecycle statuses. A batch is "running" until every item is
// terminal; it then becomes "completed", or "cancelled" when at least one
// item ended cancelled.
const (
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusCancelled = "cancelled"
)

// SSE event coordinates for batch progress (additive event type on the
// existing run-event stream seam — internal/streaming — keyed by batch id).
const (
	// EventBatchType is the streaming event Type for batch progress.
	EventBatchType = "batch"
	// EventBatchUpdated is the streaming event Name emitted at every item
	// transition. Payload: {batch_id, completed, total, item_states}.
	EventBatchUpdated = "batch.updated"
)

// Tuning defaults pinned by issue #123.
const (
	// DefaultMaxItems caps the items accepted per batch submission when
	// AGENTOS_BATCH_MAX_ITEMS is unset.
	DefaultMaxItems = 100
	// DefaultHistoryPerOrg caps retained batches per organization.
	DefaultHistoryPerOrg = 1000
	// DefaultConcurrency is the pool size when a submission omits
	// max_concurrency.
	DefaultConcurrency = 4
	// MaxConcurrencyCap is the hard ceiling for any submission's pool size.
	MaxConcurrencyCap = 32
)

// EnvMaxItems is the environment variable overriding DefaultMaxItems at
// construction time (read once by MaxItemsFromEnv).
const EnvMaxItems = "AGENTOS_BATCH_MAX_ITEMS"

// Errors returned by Submit/Get/Wait; the HTTP layer maps them onto
// 422 (ErrNoItems, ErrTooManyItems) and 404 (ErrBatchNotFound).
var (
	// ErrOrgRequired is returned when the tenant scope is missing.
	ErrOrgRequired = errors.New("batch: organization id is required")
	// ErrNoItems is returned when a submission carries no items.
	ErrNoItems = errors.New("batch: at least one item is required")
	// ErrTooManyItems is returned when a submission exceeds the configured
	// per-batch item cap (atomic validation: nothing runs).
	ErrTooManyItems = errors.New("batch: too many items for one batch")
	// ErrBatchNotFound is returned when a batch id is unknown to the caller's
	// organization (cross-org access is indistinguishable from missing).
	ErrBatchNotFound = errors.New("batch: batch not found")
)

// Item is one unit of work inside a batch. Ref is the stable caller-facing
// reference (agent id, run id, secret name, or the zero-based input index for
// input-fanned batches); Result carries per-item output metadata (e.g.
// {"run_id": "..."}), never secret values.
type Item struct {
	Ref       string         `json:"ref"`
	Status    ItemStatus     `json:"status"`
	Error     string         `json:"error,omitempty"`
	Result    map[string]any `json:"result,omitempty"`
	StartedAt *time.Time     `json:"started_at,omitempty"`
	EndedAt   *time.Time     `json:"ended_at,omitempty"`
}

// Batch is one tracked bulk operation. Records are created by Submit, mutated
// ONLY through Store.Update transitions, and handed to callers as copies.
type Batch struct {
	ID             string    `json:"id"`
	OrganizationID string    `json:"organization_id"`
	Type           BatchType `json:"type"`
	Status         string    `json:"status"`
	MaxConcurrency int       `json:"max_concurrency"`
	Total          int       `json:"total"`
	// Completed counts items in a terminal state (succeeded/failed/cancelled).
	Completed   int        `json:"completed"`
	Items       []Item     `json:"items"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

// IsTerminal reports whether every item reached a terminal state (the batch
// status is then completed or cancelled and the record is immutable).
func (b *Batch) IsTerminal() bool {
	return b != nil && b.Status != StatusRunning
}

// Summary is the partial-failure rollup pinned by the issue contract
// ({total, succeeded, failed} plus the additive cancelled counter).
type Summary struct {
	Total     int `json:"total"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Cancelled int `json:"cancelled"`
}

// Summary computes the current rollup from the item states.
func (b *Batch) Summary() Summary {
	out := Summary{Total: len(b.Items)}
	for _, it := range b.Items {
		switch it.Status {
		case ItemSucceeded:
			out.Succeeded++
		case ItemFailed:
			out.Failed++
		case ItemCancelled:
			out.Cancelled++
		}
	}
	return out
}

// snapshot returns a copy of the record safe to retain by readers: the Items
// slice is copied (Item.Result maps are treated as immutable by contract).
func (b *Batch) snapshot() *Batch {
	if b == nil {
		return nil
	}
	out := *b
	out.Items = append([]Item(nil), b.Items...)
	return &out
}

// Unit is one item of work handed to Submit. Run executes the item's real
// operation (create a run, patch an agent, ...) and must honor ctx
// cancellation; it returns per-item result metadata or an error (a per-item
// failure never aborts the batch). Run is always invoked on its own
// goroutine and at most MaxConcurrency units of the same batch run
// concurrently.
type Unit struct {
	Ref string
	Run func(ctx context.Context) (map[string]any, error)
}

// SubmitOptions carries the per-submission knobs the HTTP layer derives from
// the request.
type SubmitOptions struct {
	// Actor is the authenticated principal id stamped on audit rows.
	Actor string
	// MaxConcurrency is the requested pool size; values <=0 select the
	// service default (4) and values above the cap (32) are clamped.
	MaxConcurrency int
}

// Store persists batch records. Implementations MUST scope Get and Update by
// organization_id (tenant guard) so a caller can never observe another
// tenant's batch (cross-org access reads as not-found). Mutations happen
// exclusively through Update — the store lock is the single-writer point —
// and fn must not retain the *Batch pointer after returning. The current
// implementation is the in-memory MemoryStore (both API modes, by design this
// round); a durable store slots in behind this seam without touching callers.
type Store interface {
	// Put inserts a new batch record (called exactly once per submission).
	Put(b *Batch) error
	// Get fetches one batch strictly within one tenant; ok is false when the
	// batch is unknown to the organization. The returned record is a copy.
	Get(orgID, id string) (*Batch, bool)
	// Update applies fn to the stored record under the store lock. It
	// returns false when the batch is unknown to the organization.
	Update(orgID, id string, fn func(b *Batch)) bool
}

// MemoryStore is the in-memory Store: per-organization maps with an
// insertion-ordered eviction queue enforcing the capped history
// (HistoryPerOrg batches per org, oldest evicted first). Safe for concurrent
// use.
type MemoryStore struct {
	mu    sync.Mutex
	orgs  map[string]map[string]*Batch
	order map[string][]string // per-org batch ids, oldest first (eviction)
	cap   int
}

// NewMemoryStore returns the in-memory store retaining at most
// historyPerOrg batches per organization (<=0 selects DefaultHistoryPerOrg).
func NewMemoryStore(historyPerOrg int) *MemoryStore {
	if historyPerOrg <= 0 {
		historyPerOrg = DefaultHistoryPerOrg
	}
	return &MemoryStore{
		orgs:  make(map[string]map[string]*Batch),
		order: make(map[string][]string),
		cap:   historyPerOrg,
	}
}

// Put inserts the record and evicts the oldest batch of the organization when
// the history cap is exceeded.
func (m *MemoryStore) Put(b *Batch) error {
	if b == nil {
		return errors.New("batch: nil batch record")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	org, ok := m.orgs[b.OrganizationID]
	if !ok {
		org = make(map[string]*Batch)
		m.orgs[b.OrganizationID] = org
	}
	org[b.ID] = b.snapshot()
	m.order[b.OrganizationID] = append(m.order[b.OrganizationID], b.ID)
	if m.cap > 0 {
		for len(m.order[b.OrganizationID]) > m.cap {
			oldest := m.order[b.OrganizationID][0]
			m.order[b.OrganizationID] = m.order[b.OrganizationID][1:]
			delete(org, oldest)
		}
	}
	return nil
}

// Get returns a copy of the record when it belongs to the organization.
func (m *MemoryStore) Get(orgID, id string) (*Batch, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.orgs[orgID][id]
	if !ok {
		return nil, false
	}
	return b.snapshot(), true
}

// Update applies fn under the store lock (single-writer mutation point).
func (m *MemoryStore) Update(orgID, id string, fn func(b *Batch)) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.orgs[orgID][id]
	if !ok {
		return false
	}
	fn(b)
	return true
}

// Auditor is the narrow audit seam (structurally satisfied by *audit.Service;
// the LogCtx signature matches, mirroring the agents.AgentAuditor /
// tenant.AuditPublisher conventions). Audit failures NEVER fail a batch.
type Auditor interface {
	LogCtx(ctx context.Context, actor, action, organizationID, resource string, metadata map[string]any) (*audit.Entry, error)
}

// ProgressSink is the additive batch-progress SSE seam — structurally
// satisfied by *streaming.Service. Events publish under the batch id as the
// stream key so GET /batch/{id}/events (ServeSSE on the same service) replays
// them, exactly like the run-event stream.
type ProgressSink interface {
	Publish(streamID, eventType, name string, payload map[string]any)
}

// Options tunes the service. Zero values select the documented defaults;
// MaxItems additionally honors EnvMaxItems through NewService.
type Options struct {
	// Store persists the records (nil selects the in-memory MemoryStore with
	// HistoryPerOrg).
	Store Store
	// MaxItems caps the items per submission (<=0 selects the
	// AGENTOS_BATCH_MAX_ITEMS value or DefaultMaxItems).
	MaxItems int
	// HistoryPerOrg caps retained batches per organization when the default
	// (in-memory) store is used (<=0 selects DefaultHistoryPerOrg).
	HistoryPerOrg int
	// DefaultConcurrency is the pool size for submissions without an explicit
	// max_concurrency (<=0 selects DefaultConcurrency).
	DefaultConcurrency int
	// MaxConcurrencyCap is the hard pool ceiling (<=0 selects
	// MaxConcurrencyCap).
	MaxConcurrencyCap int
}

// Service tracks bulk operations and executes their items through a bounded
// goroutine pool. Safe for concurrent use.
type Service struct {
	mu     sync.Mutex
	store  Store
	limits struct {
		maxItems           int
		historyPerOrg      int
		defaultConcurrency int
		maxConcurrency     int
	}
	streamer ProgressSink
	auditor  Auditor
	// cancels holds the context cancel func of every RUNNING batch (removed
	// at completion); done holds the per-batch channel closed exactly once by
	// the pool goroutine when the batch turns terminal (Wait's wake signal).
	cancels map[string]context.CancelFunc
	done    map[string]chan struct{}
}

// NewService returns the service with the in-memory store and the env-derived
// item cap (AGENTOS_BATCH_MAX_ITEMS, default 100).
func NewService() *Service {
	return NewServiceWithOptions(Options{MaxItems: MaxItemsFromEnv()})
}

// NewServiceWithOptions returns the service with explicit options (tests and
// alternative wiring); zero limits select the documented defaults.
func NewServiceWithOptions(opts Options) *Service {
	store := opts.Store
	if store == nil {
		store = NewMemoryStore(opts.HistoryPerOrg)
	}
	s := &Service{
		store:   store,
		cancels: make(map[string]context.CancelFunc),
		done:    make(map[string]chan struct{}),
	}
	s.limits.maxItems = opts.MaxItems
	if s.limits.maxItems <= 0 {
		s.limits.maxItems = DefaultMaxItems
	}
	s.limits.historyPerOrg = opts.HistoryPerOrg
	if s.limits.historyPerOrg <= 0 {
		s.limits.historyPerOrg = DefaultHistoryPerOrg
	}
	s.limits.defaultConcurrency = opts.DefaultConcurrency
	if s.limits.defaultConcurrency <= 0 {
		s.limits.defaultConcurrency = DefaultConcurrency
	}
	s.limits.maxConcurrency = opts.MaxConcurrencyCap
	if s.limits.maxConcurrency <= 0 {
		s.limits.maxConcurrency = MaxConcurrencyCap
	}
	return s
}

// MaxItemsFromEnv reads EnvMaxItems once; unset/invalid/non-positive values
// select DefaultMaxItems.
func MaxItemsFromEnv() int {
	raw := strings.TrimSpace(os.Getenv(EnvMaxItems))
	if raw == "" {
		return DefaultMaxItems
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return DefaultMaxItems
	}
	return n
}

// SetStreamer wires (or replaces) the batch-progress SSE seam. Nil-safe.
func (s *Service) SetStreamer(p ProgressSink) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streamer = p
}

// SetAuditor wires (or replaces) the audit seam. Nil-safe; passing nil
// disables the audit emission.
func (s *Service) SetAuditor(a Auditor) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auditor = a
}

// Submit registers a batch and starts executing its units through the
// concurrency pool. Validation is ATOMIC: the org scope, the item count (the
// per-batch cap) and the pool size are resolved before any record exists and
// before any Unit runs, so an invalid submission leaves nothing behind.
//
// The pool runs on a context detached from the caller's ctx (a disconnected
// HTTP request must not abort a batch someone else is polling); callers wait
// via Wait, poll Get or subscribe to the SSE stream. The returned snapshot
// has every item pending and the batch running.
func (s *Service) Submit(ctx context.Context, orgID string, typ BatchType, units []Unit, opts SubmitOptions) (*Batch, error) {
	if s == nil {
		return nil, errors.New("batch: service is nil")
	}
	orgID = strings.TrimSpace(orgID)
	if orgID == "" {
		return nil, ErrOrgRequired
	}
	if len(units) == 0 {
		return nil, ErrNoItems
	}
	if s.limits.maxItems > 0 && len(units) > s.limits.maxItems {
		return nil, fmt.Errorf("%w: %d items > cap %d", ErrTooManyItems, len(units), s.limits.maxItems)
	}
	pool := s.clampConcurrency(opts.MaxConcurrency)

	now := time.Now().UTC()
	record := &Batch{
		ID:             uuid.NewString(),
		OrganizationID: orgID,
		Type:           typ,
		Status:         StatusRunning,
		MaxConcurrency: pool,
		Total:          len(units),
		Items:          make([]Item, len(units)),
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	for i, u := range units {
		ref := strings.TrimSpace(u.Ref)
		if ref == "" {
			ref = strconv.Itoa(i)
		}
		record.Items[i] = Item{Ref: ref, Status: ItemPending}
	}
	if err := s.store.Put(record); err != nil {
		return nil, err
	}

	// The pool context is detached from the request on purpose (documented in
	// Submit): cancellation is EXPLICIT via Cancel, never a side effect of a
	// client disconnect.
	runCtx, cancel := context.WithCancel(context.Background())
	completed := make(chan struct{})
	s.mu.Lock()
	s.cancels[record.ID] = cancel
	s.done[record.ID] = completed
	s.mu.Unlock()

	s.audit(ctx, opts.Actor, "batch.created", orgID, "batches/"+record.ID, map[string]any{
		"batch_id":        record.ID,
		"type":            string(typ),
		"total":           record.Total,
		"max_concurrency": pool,
	})

	go s.execute(runCtx, cancel, orgID, record.ID, units, pool, completed, opts.Actor)
	return record.snapshot(), nil
}

// clampConcurrency resolves the pool size for one submission: <=0 selects the
// default, values above the cap are clamped, and the configured bounds floor
// at 1.
func (s *Service) clampConcurrency(requested int) int {
	def := s.limits.defaultConcurrency
	if def < 1 {
		def = DefaultConcurrency
	}
	ceil := s.limits.maxConcurrency
	if ceil < 1 {
		ceil = MaxConcurrencyCap
	}
	if requested <= 0 {
		return def
	}
	if requested > ceil {
		return ceil
	}
	return requested
}

// execute is the pool goroutine: it runs every unit bounded by the pool
// semaphore (honoring max_concurrency), then finalizes the batch. Each unit
// is fully isolated — a panic or error in one unit can never take down the
// batch or its siblings.
func (s *Service) execute(ctx context.Context, cancel context.CancelFunc, orgID, batchID string, units []Unit, pool int, completed chan struct{}, actor string) {
	defer cancel()
	if pool < 1 {
		pool = DefaultConcurrency
	}
	sem := make(chan struct{}, pool)
	var wg sync.WaitGroup
	for i := range units {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			// Acquire a pool slot, or bail out when the batch is cancelled
			// while waiting. The release must stay conditional on the
			// acquisition: an unconditional release would hand back a token
			// the goroutine never took and inflate the pool beyond
			// max_concurrency.
			acquired := false
			select {
			case sem <- struct{}{}:
				acquired = true
			case <-ctx.Done():
				// Batch cancelled while waiting for a slot: the item never
				// runs; runItem records the cancelled state.
			}
			defer func() {
				if acquired {
					<-sem
				}
			}()
			s.runItem(ctx, orgID, batchID, idx, units[idx])
		}(i)
	}
	wg.Wait()
	s.finalize(orgID, batchID, completed, actor)
}

// runItem drives one unit through its transitions:
// pending -> running -> succeeded | failed | cancelled. When the batch
// context is already cancelled the unit never runs. A unit error is recorded
// on the item (per-item isolation); only a batch-context cancellation flips
// the item to cancelled.
func (s *Service) runItem(ctx context.Context, orgID, batchID string, idx int, u Unit) {
	if ctx.Err() != nil {
		s.transition(orgID, batchID, idx, ItemCancelled, nil, "batch cancelled before the item started")
		return
	}
	s.transition(orgID, batchID, idx, ItemRunning, nil, "")

	var (
		result map[string]any
		err    error
	)
	func() {
		// Panic isolation: a panicking unit is a failed item, never a crashed
		// pool goroutine (the semaphore-release defer stays intact).
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("item panicked: %v", r)
				result = nil
			}
		}()
		result, err = u.Run(ctx)
	}()

	switch {
	case err == nil:
		s.transition(orgID, batchID, idx, ItemSucceeded, result, "")
	case ctx.Err() != nil:
		s.transition(orgID, batchID, idx, ItemCancelled, nil, "batch cancelled while the item was running")
	default:
		s.transition(orgID, batchID, idx, ItemFailed, nil, err.Error())
	}
}

// transition applies one item mutation under the store lock, rolls the batch
// counters/status forward (flipping the batch terminal when the last item
// settles) and emits the additive batch.updated SSE event. Terminal items are
// never resurrected.
func (s *Service) transition(orgID, batchID string, idx int, status ItemStatus, result map[string]any, errMsg string) {
	s.store.Update(orgID, batchID, func(b *Batch) {
		if idx < 0 || idx >= len(b.Items) {
			return
		}
		it := &b.Items[idx]
		now := time.Now().UTC()
		if !terminalItem(it.Status) {
			it.Status = status
			it.Error = errMsg
			it.Result = result
			if status == ItemRunning {
				it.StartedAt = &now
			} else {
				it.EndedAt = &now
			}
		}
		b.Completed = 0
		anyCancelled := false
		for i := range b.Items {
			switch b.Items[i].Status {
			case ItemSucceeded, ItemFailed, ItemCancelled:
				b.Completed++
			}
			if b.Items[i].Status == ItemCancelled {
				anyCancelled = true
			}
		}
		b.UpdatedAt = now
		if b.Completed == len(b.Items) && b.Status == StatusRunning {
			if anyCancelled {
				b.Status = StatusCancelled
			} else {
				b.Status = StatusCompleted
			}
			b.CompletedAt = &now
		}
	})
	s.emitProgress(orgID, batchID)
}

// finalize runs the completion side effects exactly once per batch (only the
// pool goroutine calls it, after every unit settled): the Wait wake signal is
// closed and the completion audit row is written.
func (s *Service) finalize(orgID, batchID string, completed chan struct{}, actor string) {
	s.mu.Lock()
	delete(s.cancels, batchID)
	delete(s.done, batchID)
	s.mu.Unlock()
	close(completed)

	b, ok := s.Get(orgID, batchID)
	if !ok {
		return
	}
	sum := b.Summary()
	s.audit(context.Background(), actor, "batch.completed", orgID, "batches/"+batchID, map[string]any{
		"batch_id":  b.ID,
		"type":      string(b.Type),
		"status":    b.Status,
		"total":     sum.Total,
		"succeeded": sum.Succeeded,
		"failed":    sum.Failed,
		"cancelled": sum.Cancelled,
	})
}

// Cancel cancels a running batch: every not-yet-started item is marked
// cancelled immediately and the in-flight items' contexts fire (their units
// observe cancellation and settle as cancelled). Idempotent: cancelling an
// already-terminal batch returns its current snapshot untouched. The batch
// status flips to cancelled/completed when the pool drains (see transition).
func (s *Service) Cancel(ctx context.Context, orgID, batchID, actor string) (*Batch, error) {
	if s == nil {
		return nil, errors.New("batch: service is nil")
	}
	orgID = strings.TrimSpace(orgID)
	batchID = strings.TrimSpace(batchID)
	if orgID == "" {
		return nil, ErrOrgRequired
	}

	s.mu.Lock()
	cancelFn := s.cancels[batchID]
	s.mu.Unlock()

	updated := s.store.Update(orgID, batchID, func(b *Batch) {
		if b.Status != StatusRunning {
			return
		}
		now := time.Now().UTC()
		for i := range b.Items {
			if b.Items[i].Status == ItemPending {
				b.Items[i].Status = ItemCancelled
				b.Items[i].Error = "batch cancelled before the item started"
				b.Items[i].EndedAt = &now
			}
		}
		b.UpdatedAt = now
	})
	if !updated {
		return nil, ErrBatchNotFound
	}
	if cancelFn != nil {
		cancelFn()
	}
	s.emitProgress(orgID, batchID)
	b, _ := s.Get(orgID, batchID)
	return b, nil
}

// Get returns the current snapshot of one batch strictly within the caller's
// organization (a foreign batch reads as not-found — the cross-org 404
// convention).
func (s *Service) Get(orgID, id string) (*Batch, bool) {
	if s == nil {
		return nil, false
	}
	orgID, id = strings.TrimSpace(orgID), strings.TrimSpace(id)
	if orgID == "" || id == "" {
		return nil, false
	}
	return s.store.Get(orgID, id)
}

// Wait blocks until the batch turns terminal or ctx is done, then returns the
// final snapshot. Unknown batches return ErrBatchNotFound immediately.
func (s *Service) Wait(ctx context.Context, orgID, id string) (*Batch, error) {
	if s == nil {
		return nil, errors.New("batch: service is nil")
	}
	b, ok := s.Get(orgID, id)
	if !ok {
		return nil, ErrBatchNotFound
	}
	if b.IsTerminal() {
		return b, nil
	}
	s.mu.Lock()
	ch := s.done[id]
	s.mu.Unlock()
	if ch == nil {
		// Defensive: the record is running but the wake channel is gone
		// (only possible mid-Submit); the snapshot is the best answer.
		return b, nil
	}
	select {
	case <-ch:
		if final, ok := s.Get(orgID, id); ok {
			return final, nil
		}
		return b, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// emitProgress publishes the additive batch.updated event on the wired SSE
// seam (no-op when unwired). Payload: {batch_id, completed, total,
// item_states} — item_states maps every item ref to its current status.
func (s *Service) emitProgress(orgID, batchID string) {
	s.mu.Lock()
	streamer := s.streamer
	s.mu.Unlock()
	if streamer == nil {
		return
	}
	b, ok := s.Get(orgID, batchID)
	if !ok {
		return
	}
	states := make(map[string]string, len(b.Items))
	for _, it := range b.Items {
		states[it.Ref] = string(it.Status)
	}
	streamer.Publish(b.ID, EventBatchType, EventBatchUpdated, map[string]any{
		"batch_id":    b.ID,
		"completed":   b.Completed,
		"total":       b.Total,
		"item_states": states,
	})
}

// audit writes one best-effort audit row through the seam (nil-safe; failures
// never affect the batch).
func (s *Service) audit(ctx context.Context, actor, action, orgID, resource string, metadata map[string]any) {
	s.mu.Lock()
	auditor := s.auditor
	s.mu.Unlock()
	if auditor == nil {
		return
	}
	if actor == "" {
		actor = "system"
	}
	_, _ = auditor.LogCtx(ctx, actor, action, orgID, resource, metadata)
}

// terminalItem reports whether an item can no longer change state.
func terminalItem(st ItemStatus) bool {
	switch st {
	case ItemSucceeded, ItemFailed, ItemCancelled:
		return true
	}
	return false
}
