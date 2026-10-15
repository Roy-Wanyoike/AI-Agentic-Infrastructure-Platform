package a2a

// service.go — the dual-mode org-scoped Agent Card registry (issue #136),
// following the internal/approvals + internal/mcp conventions exactly:
//
//   - NewService():          in-memory mode (maps + RWMutex), zero infra.
//   - NewServiceWithStore / SetStore(NewPostgresStore(db)): Postgres mode;
//     the store is the source of truth and the in-memory map is a
//     write-through cache.
//
// TENANT GUARD: every operation is keyed by organization_id and surfaced
// from the auth claims; cross-tenant or unknown ids are ErrNotFound (no
// existence leak).
//
// AGENT LINK: a card MUST reference an existing agent of the SAME org,
// validated at create/update time through the AgentExistenceChecker seam
// (*agents.Service satisfies it structurally via GetAgentCtx — the same
// seam shape the apikeys wave-8 agent keys use).
//
// AUDIT: card lifecycle rows are written by the HTTP handlers (the mcp
// registry handler pattern); outbound delegation outcomes are audited by
// the SERVICE via the Auditor seam (the mcp.tool_call precedent — the
// service owns the wire outcome data). Message payloads are NEVER logged;
// only card/endpoint/result metadata is.

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"agentos/internal/agents"
	"agentos/internal/audit"

	"github.com/google/uuid"
)

// AgentExistenceChecker is the seam for validating that an agent belongs to
// an organization (mirrors how the apikeys service cross-checks agents
// through agents.Service.GetAgentCtx). *agents.Service satisfies it.
type AgentExistenceChecker interface {
	GetAgentCtx(ctx context.Context, orgID, id string) (*agents.Agent, error)
}

// Auditor is the narrow audit seam for delegation outcomes. It is satisfied
// structurally by *audit.Service (its LogCtx signature matches), so the
// wiring layer can pass the shared service directly.
type Auditor interface {
	LogCtx(ctx context.Context, actor, action, organizationID, resource string, metadata map[string]any) (*audit.Entry, error)
}

// RunSink is the inbound-message execution seam: it resolves the card's
// target agent into a platform run (or a queued delegation) and returns the
// run/task ids. cmd/api wires an adapter over the runs + queue services'
// EXPORTED API (CreateRunCtx + Enqueue — the exact createRunHandler path).
type RunSink interface {
	StartRun(ctx context.Context, orgID, agentID, message string) (runID, taskID string, err error)
}

// CardStore abstracts durable card storage. Tenant scoping is enforced in
// the store layer too (every statement filters organization_id); the
// service treats the store as untrusted and re-checks isolation on the
// read path.
type CardStore interface {
	// CreateCard inserts one row with its organization_id scope.
	CreateCard(ctx context.Context, orgID string, c *AgentCard) error
	// GetCard returns one live card within one tenant.
	GetCard(ctx context.Context, orgID, id string) (*AgentCard, error)
	// ListCards returns all cards of exactly one tenant (created ASC).
	ListCards(ctx context.Context, orgID string) ([]*AgentCard, error)
	// UpdateCard replaces the mutable fields of one row within one tenant
	// (0 rows affected -> ErrNotFound).
	UpdateCard(ctx context.Context, orgID string, c *AgentCard) error
	// DeleteCard hard-deletes one row within one tenant (0 rows -> ErrNotFound).
	DeleteCard(ctx context.Context, orgID, id string) error
}

// Service is the dual-mode A2A Agent Card registry.
type Service struct {
	mu    sync.RWMutex
	cards map[string]map[string]*AgentCard // orgID -> id -> card (memory mode)
	store CardStore

	agentChecker AgentExistenceChecker
	auditor      Auditor
	gate         DelegationGate
	sink         RunSink
	httpClient   *http.Client
	timeout      time.Duration
}

// NewService returns the in-memory registry (zero infrastructure).
func NewService() *Service {
	return &Service{
		cards:      make(map[string]map[string]*AgentCard),
		httpClient: defaultDelegationClient(),
		timeout:    DefaultDelegationTimeout,
	}
}

// NewServiceWithStore returns a service whose source of truth is a durable
// store; the in-memory map remains a write-through cache.
func NewServiceWithStore(store CardStore) *Service {
	s := NewService()
	s.store = store
	return s
}

// SetStore swaps the durable store (main.go wires the Postgres store when
// the database is connected). Nil-safe.
func (s *Service) SetStore(store CardStore) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store = store
}

// SetAgentChecker wires (or replaces) the agent-existence seam. Nil-safe.
// A nil checker DISABLES the agent validation (legacy/test path) — the API
// wiring always sets it.
func (s *Service) SetAgentChecker(c AgentExistenceChecker) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.agentChecker = c
}

// SetAuditor wires (or replaces) the delegation audit seam. Nil-safe.
func (s *Service) SetAuditor(a Auditor) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auditor = a
}

// SetDelegationGate wires (or replaces) the outbound policy gate. Nil-safe.
// A nil gate ALLOWS delegations with the recorded verdict
// "allowed_no_gate" (issue #136 contract).
func (s *Service) SetDelegationGate(g DelegationGate) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gate = g
}

// SetRunSink wires (or replaces) the inbound-message execution seam.
// Nil-safe.
func (s *Service) SetRunSink(sink RunSink) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sink = sink
}

// CreateCard validates and publishes one agent card for one tenant
// (organization_id comes from the authenticated caller, never the client).
func (s *Service) CreateCard(ctx context.Context, orgID string, in CreateCardInput) (*AgentCard, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgRequired
	}
	name := strings.TrimSpace(in.Name)
	if err := validateCardFields(name, in.Version, in.EndpointURL); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.AgentID) == "" {
		return nil, ErrAgentRequired
	}
	if err := s.ensureAgentInOrg(ctx, orgID, in.AgentID); err != nil {
		return nil, err
	}
	transport := strings.TrimSpace(in.PreferredTransport)
	if transport == "" {
		transport = DefaultTransport
	}
	authScheme := strings.TrimSpace(in.AuthScheme)
	if authScheme == "" {
		authScheme = DefaultAuthScheme
	}
	now := time.Now().UTC()
	card := &AgentCard{
		ID:                 uuid.NewString(),
		OrganizationID:     orgID,
		AgentID:            in.AgentID,
		Name:               name,
		Description:        in.Description,
		Version:            strings.TrimSpace(in.Version),
		Provider:           in.Provider,
		EndpointURL:        strings.TrimSpace(in.EndpointURL),
		Capabilities:       in.Capabilities,
		Skills:             normalizeSkills(in.Skills),
		AuthScheme:         authScheme,
		PreferredTransport: transport,
		CreatedBy:          in.CreatedBy,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store != nil {
		if err := s.store.CreateCard(ctx, orgID, card); err != nil {
			return nil, err
		}
	}
	s.cacheCardLocked(card)
	return card, nil
}

// GetCard resolves one card strictly within one tenant (foreign/unknown
// ids are ErrNotFound without an existence leak).
func (s *Service) GetCard(ctx context.Context, orgID, id string) (*AgentCard, error) {
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(id) == "" {
		return nil, ErrNotFound
	}
	s.mu.RLock()
	store := s.store
	s.mu.RUnlock()
	if store != nil {
		return store.GetCard(ctx, orgID, id)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	card, ok := s.cards[orgID][id]
	if !ok {
		return nil, ErrNotFound
	}
	return card, nil
}

// ListCards returns the tenant's cards (created ASC in durable mode,
// insertion order in memory mode).
func (s *Service) ListCards(ctx context.Context, orgID string) ([]*AgentCard, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgRequired
	}
	s.mu.RLock()
	store := s.store
	s.mu.RUnlock()
	if store != nil {
		return store.ListCards(ctx, orgID)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*AgentCard, 0, len(s.cards[orgID]))
	for _, card := range s.cards[orgID] {
		out = append(out, card)
	}
	return out, nil
}

// UpdateCard applies PATCH semantics to one card within one tenant:
// nil fields keep the stored value; provided fields replace it. A changed
// agent link is re-validated against the same org.
func (s *Service) UpdateCard(ctx context.Context, orgID, id string, in UpdateCardInput) (*AgentCard, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgRequired
	}
	current, err := s.GetCard(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	updated := *current
	if in.AgentID != nil {
		if strings.TrimSpace(*in.AgentID) == "" {
			return nil, ErrAgentRequired
		}
		if err := s.ensureAgentInOrg(ctx, orgID, *in.AgentID); err != nil {
			return nil, err
		}
		updated.AgentID = *in.AgentID
	}
	if in.Name != nil {
		updated.Name = strings.TrimSpace(*in.Name)
	}
	if in.Description != nil {
		updated.Description = *in.Description
	}
	if in.Version != nil {
		updated.Version = strings.TrimSpace(*in.Version)
	}
	if in.Provider != nil {
		updated.Provider = *in.Provider
	}
	if in.EndpointURL != nil {
		updated.EndpointURL = strings.TrimSpace(*in.EndpointURL)
	}
	if in.Capabilities != nil {
		updated.Capabilities = *in.Capabilities
	}
	if in.Skills != nil {
		updated.Skills = normalizeSkills(in.Skills)
	}
	if in.AuthScheme != nil {
		updated.AuthScheme = strings.TrimSpace(*in.AuthScheme)
	}
	if in.PreferredTransport != nil {
		updated.PreferredTransport = strings.TrimSpace(*in.PreferredTransport)
	}
	if in.PreferredTransport == nil && current.PreferredTransport == "" {
		updated.PreferredTransport = DefaultTransport
	}
	if err := validateCardFields(updated.Name, updated.Version, updated.EndpointURL); err != nil {
		return nil, err
	}
	updated.UpdatedAt = time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store != nil {
		if err := s.store.UpdateCard(ctx, orgID, &updated); err != nil {
			return nil, err
		}
	}
	s.cacheCardLocked(&updated)
	return &updated, nil
}

// DeleteCard hard-deletes one card within the caller's organization
// (foreign/unknown ids surface as ErrNotFound without an existence leak).
func (s *Service) DeleteCard(ctx context.Context, orgID, id string) error {
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(id) == "" {
		return ErrNotFound
	}
	// Existence check keeps the 404 semantics uniform across modes.
	if _, err := s.GetCard(ctx, orgID, id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store != nil {
		if err := s.store.DeleteCard(ctx, orgID, id); err != nil {
			return err
		}
	}
	if cards, ok := s.cards[orgID]; ok {
		delete(cards, id)
	}
	return nil
}

// InboundMessage is the result of an accepted A2A inbound message.
type InboundMessage struct {
	CardID  string `json:"card_id"`
	AgentID string `json:"agent_id"`
	RunID   string `json:"run_id"`
	TaskID  string `json:"task_id"`
	Status  string `json:"status"`
}

// ReceiveMessage validates the inbound A2A message against the card,
// resolves the card's target agent, and hands the message to the wired
// RunSink (cmd/api adapts it to runs.Service.CreateRunCtx + queue.Queue.
// Enqueue — the same exported path the create-run handler uses). The
// message payload is never audited; only ids/sizes are.
func (s *Service) ReceiveMessage(ctx context.Context, orgID, cardID, message, actor string) (*InboundMessage, error) {
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(cardID) == "" {
		return nil, ErrNotFound
	}
	if strings.TrimSpace(message) == "" {
		return nil, ErrMessageRequired
	}
	card, err := s.GetCard(ctx, orgID, cardID)
	if err != nil {
		return nil, err
	}
	// Resolve the target agent: the card's agent must still exist in this
	// org at message time (a card whose agent was deleted must not route).
	if err := s.ensureAgentInOrg(ctx, orgID, card.AgentID); err != nil {
		return nil, err
	}
	s.mu.RLock()
	sink := s.sink
	s.mu.RUnlock()
	if sink == nil {
		return nil, ErrRunSinkUnavailable
	}
	runID, taskID, err := sink.StartRun(ctx, orgID, card.AgentID, message)
	if err != nil {
		return nil, err
	}
	return &InboundMessage{
		CardID:  card.ID,
		AgentID: card.AgentID,
		RunID:   runID,
		TaskID:  taskID,
		Status:  StatusSubmitted,
	}, nil
}

// ensureAgentInOrg validates the agent link through the checker seam
// (nil checker disables the check — documented test path).
func (s *Service) ensureAgentInOrg(ctx context.Context, orgID, agentID string) error {
	s.mu.RLock()
	checker := s.agentChecker
	s.mu.RUnlock()
	if checker == nil {
		return nil
	}
	if _, err := checker.GetAgentCtx(ctx, orgID, agentID); err != nil {
		return ErrAgentUnknown
	}
	return nil
}

// cacheCardLocked maintains the write-through cache (caller holds s.mu
// write-locked or construction-locked).
func (s *Service) cacheCardLocked(card *AgentCard) {
	if s.cards == nil {
		s.cards = make(map[string]map[string]*AgentCard)
	}
	org, ok := s.cards[card.OrganizationID]
	if !ok {
		org = make(map[string]*AgentCard)
		s.cards[card.OrganizationID] = org
	}
	org[card.ID] = card
}
