package memory

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Memory scopes (migration 014 memory_snippets.scope).
const (
	ScopeShortTerm = "short_term"
	ScopeLongTerm  = "long_term"
)

// Retrieval bounds.
const (
	// DefaultRetrieveK is the number of snippets returned when the caller
	// does not specify k.
	DefaultRetrieveK = 5
	// MaxRetrieveK caps a single retrieval so one request cannot scan the
	// whole org for an unbounded k.
	MaxRetrieveK = 50
	// scoreFloor drops hash-collision noise from the offline embedder:
	// candidates below a 1% relevance never enter the top-k.
	scoreFloor = 0.01
)

// ErrInvalidScope is returned when a snippet carries an unknown scope.
var ErrInvalidScope = errors.New("memory: scope must be short_term or long_term")

// Snippet is one org/agent-scoped memory entry. AgentID is empty for
// organization-level (shared) memory. ExpiresAt drives short-term expiry:
// expired snippets are invisible to listings and retrieval (the Postgres
// store filters them in SQL, the in-memory service by wall clock).
type Snippet struct {
	ID             string
	OrganizationID string
	AgentID        string
	Scope          string
	Content        string
	Importance     float64
	ExpiresAt      *time.Time
	Embedding      []float64
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Expired reports whether the snippet is past its expires_at timestamp.
func (s *Snippet) Expired(now time.Time) bool {
	return s != nil && s.ExpiresAt != nil && now.After(*s.ExpiresAt)
}

// SnippetInput is the caller-supplied shape for one snippet in a PUT request.
type SnippetInput struct {
	Scope      string
	Content    string
	Importance float64
	ExpiresAt  *time.Time
	Embedding  []float64
}

// PutRequest replaces the snippet set for one (organization, agent) pair.
// AgentID may be empty to manage organization-level memory.
type PutRequest struct {
	AgentID  string
	Snippets []SnippetInput
}

// RetrieveOptions tunes retrieval scope.
type RetrieveOptions struct {
	// AgentID scopes retrieval to one agent's memory PLUS the org-level
	// shared memory. Empty means the whole organization.
	AgentID string
	// K bounds the result count (default DefaultRetrieveK, max MaxRetrieveK).
	K int
}

// ScoredSnippet is a retrieval hit: the snippet plus its relevance score in
// [0,1] (cosine similarity when embeddings are available on both sides,
// lexical token overlap otherwise).
type ScoredSnippet struct {
	Snippet Snippet
	Score   float64
}

// Embedder turns text into a vector. The deterministic hashing embedder
// satisfies it offline; an OpenAI-compatible client can be injected.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float64, error)
}

// Service is the dual-mode memory service: pure in-memory map (zero
// infrastructure mode) or Postgres-backed store.
type Service struct {
	mu       sync.Mutex
	snippets map[string]*Snippet
	store    SnippetStore
	embedder Embedder
	now      func() time.Time
}

// NewService returns the in-memory service (zero-infrastructure mode) using
// the deterministic offline embedder.
func NewService() *Service {
	return newService(nil, NewHashEmbedder())
}

// NewServiceWithStore returns a service whose source of truth is Postgres
// (migration 014 memory_snippets). Embeddings use the deterministic offline
// embedder unless a client supplies vectors on write; retrieval always
// honors them via cosine similarity.
func NewServiceWithStore(db *sql.DB) *Service {
	return newService(NewPostgresStore(db), NewHashEmbedder())
}

// NewServiceWithEmbedder returns an in-memory service with a custom embedder
// (used by tests and by callers wiring a real embeddings backend).
func NewServiceWithEmbedder(embedder Embedder) *Service {
	return newService(nil, embedder)
}

func newService(store SnippetStore, embedder Embedder) *Service {
	if embedder == nil {
		embedder = NewHashEmbedder()
	}
	return &Service{
		snippets: make(map[string]*Snippet),
		store:    store,
		embedder: embedder,
		now:      time.Now,
	}
}

// PutSnippets atomically replaces the snippet set for (orgID, req.AgentID).
// Scopes are normalized (default long_term), importance clamped to [0,1],
// and missing embeddings are generated with the service embedder (an embed
// failure is non-fatal: the snippet is stored without a vector and retrieval
// falls back to lexical scoring for it).
func (s *Service) PutSnippets(ctx context.Context, orgID string, req PutRequest) ([]Snippet, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, errors.New("organization id is required")
	}
	now := s.now().UTC()
	out := make([]Snippet, 0, len(req.Snippets))
	for _, in := range req.Snippets {
		if strings.TrimSpace(in.Content) == "" {
			return nil, errors.New("snippet content is required")
		}
		scope := normalizeScope(in.Scope)
		if scope == "" {
			return nil, ErrInvalidScope
		}
		snippet := Snippet{
			ID:             uuid.NewString(),
			OrganizationID: orgID,
			AgentID:        strings.TrimSpace(req.AgentID),
			Scope:          scope,
			Content:        in.Content,
			Importance:     clamp01(in.Importance),
			ExpiresAt:      in.ExpiresAt,
			CreatedAt:      now,
			UpdatedAt:      now,
		}
		if len(in.Embedding) > 0 {
			snippet.Embedding = append([]float64(nil), in.Embedding...)
		} else if s.embedder != nil {
			if vec, err := s.embedder.Embed(ctx, in.Content); err == nil {
				snippet.Embedding = vec
			}
		}
		out = append(out, snippet)
	}

	if s.store != nil {
		rows := make([]*Snippet, len(out))
		for i := range out {
			rows[i] = &out[i]
		}
		if err := s.store.ReplaceAgentSnippets(ctx, orgID, req.AgentID, rows); err != nil {
			return nil, err
		}
		return out, nil
	}

	s.mu.Lock()
	agentID := strings.TrimSpace(req.AgentID)
	for id, sn := range s.snippets {
		if sn.OrganizationID == orgID && sn.AgentID == agentID {
			delete(s.snippets, id)
		}
	}
	for i := range out {
		cp := out[i]
		s.snippets[cp.ID] = &cp
	}
	s.mu.Unlock()
	return out, nil
}

// ListSnippets returns the visible (non-expired) snippets of one tenant.
// agentID filters to exactly that agent's snippets; empty returns every
// snippet of the organization (org-level + all agents).
func (s *Service) ListSnippets(ctx context.Context, orgID, agentID string) ([]Snippet, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, errors.New("organization id is required")
	}
	now := s.now().UTC()
	var rows []*Snippet
	if s.store != nil {
		var err error
		rows, err = s.store.ListSnippets(ctx, orgID, agentID)
		if err != nil {
			return nil, err
		}
	} else {
		s.mu.Lock()
		for _, sn := range s.snippets {
			if sn.OrganizationID != orgID {
				continue
			}
			if agentID != "" && sn.AgentID != agentID {
				continue
			}
			rows = append(rows, sn)
		}
		s.mu.Unlock()
	}
	out := make([]Snippet, 0, len(rows))
	for _, sn := range rows {
		if sn.Expired(now) {
			continue // short-term expiry honored on every read
		}
		out = append(out, *sn)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// Retrieve scores the org-scoped candidate snippets against query and returns
// the top-k. Candidates are the agent's own snippets plus org-level shared
// memory when opts.AgentID is set, otherwise the whole organization. Expired
// snippets never score. When embeddings exist on both sides (service
// embedder, client-supplied vectors) cosine similarity is used; snippets
// without a usable vector fall back to lexical token overlap so retrieval
// still works with mixed data.
func (s *Service) Retrieve(ctx context.Context, orgID, query string, opts RetrieveOptions) ([]ScoredSnippet, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, errors.New("organization id is required")
	}
	if strings.TrimSpace(query) == "" {
		return nil, errors.New("query is required")
	}
	k := opts.K
	if k <= 0 {
		k = DefaultRetrieveK
	}
	if k > MaxRetrieveK {
		k = MaxRetrieveK
	}
	now := s.now().UTC()

	var rows []*Snippet
	if s.store != nil {
		if opts.AgentID != "" {
			own, err := s.store.ListSnippetsForAgent(ctx, orgID, opts.AgentID)
			if err != nil {
				return nil, err
			}
			rows = own
		} else {
			all, err := s.store.ListSnippets(ctx, orgID, "")
			if err != nil {
				return nil, err
			}
			rows = all
		}
	} else {
		s.mu.Lock()
		for _, sn := range s.snippets {
			if sn.OrganizationID != orgID {
				continue
			}
			if opts.AgentID != "" && sn.AgentID != opts.AgentID && sn.AgentID != "" {
				continue
			}
			rows = append(rows, sn)
		}
		s.mu.Unlock()
	}

	// Query vector: used only against snippets of the same dimension.
	queryVec, _ := s.embedder.Embed(ctx, query)
	queryTokens := tokenSet(query)

	scored := make([]ScoredSnippet, 0, len(rows))
	for _, sn := range rows {
		if sn.Expired(now) {
			continue
		}
		score := 0.0
		if len(queryVec) > 0 && len(sn.Embedding) == len(queryVec) {
			score = cosineSimilarity(queryVec, sn.Embedding)
		} else {
			score = lexicalScore(queryTokens, sn.Content)
		}
		if score <= scoreFloor {
			continue
		}
		scored = append(scored, ScoredSnippet{Snippet: *sn, Score: score})
	}
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].Score != scored[j].Score {
			return scored[i].Score > scored[j].Score
		}
		if scored[i].Snippet.Importance != scored[j].Snippet.Importance {
			return scored[i].Snippet.Importance > scored[j].Snippet.Importance
		}
		return scored[i].Snippet.ID < scored[j].Snippet.ID
	})
	if len(scored) > k {
		scored = scored[:k]
	}
	return scored, nil
}

func normalizeScope(scope string) string {
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case "":
		return ScopeLongTerm
	case ScopeShortTerm:
		return ScopeShortTerm
	case ScopeLongTerm:
		return ScopeLongTerm
	default:
		return ""
	}
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	if math.IsNaN(v) {
		return 0
	}
	return v
}
