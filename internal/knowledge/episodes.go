package knowledge

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Activity episodes — issue #130 (dynamic knowledge graph memory).
//
// Every agent run produces activities (tool calls, reasoning steps, human
// decisions, observations). Episode memory feeds those activities back into
// the knowledge layer as timestamped, entity-tagged chunks so later runs can
// query "what happened in the last N hours" with recency-weighted relevance.
//
// Layering:
//   - temporal layer: every episode row carries episode_ts (the activity
//     timestamp, NOT the insert time) and is scored through
//     TimeDecayRelevance at query time — no background re-scoring job.
//   - semantic layer: content is chunked with the existing ChunkText seam
//     and embedded with the existing Embedder seam (one batch call per
//     ingest), reusing the exact retrieval scoring of the document store.
// ---------------------------------------------------------------------------

// EpisodeAction is the kind of agent activity an episode captures.
type EpisodeAction string

// The four episode action types (wire values, pinned by migration 035's
// CHECK constraint and api/fragments/knowledge-graph.yaml).
const (
	// EpisodeActionToolCall is one agent tool invocation (name, args, result
	// summary as content).
	EpisodeActionToolCall EpisodeAction = "tool_call"
	// EpisodeActionReasoning is an intermediate reasoning/scratchpad step.
	EpisodeActionReasoning EpisodeAction = "reasoning"
	// EpisodeActionDecision is an approval, policy or routing decision.
	EpisodeActionDecision EpisodeAction = "decision"
	// EpisodeActionObservation is an observed side effect (tool output, state
	// change, webhook receipt).
	EpisodeActionObservation EpisodeAction = "observation"
)

// ValidEpisodeAction reports whether action is one of the four episode types.
func ValidEpisodeAction(action EpisodeAction) bool {
	switch action {
	case EpisodeActionToolCall, EpisodeActionReasoning, EpisodeActionDecision, EpisodeActionObservation:
		return true
	}
	return false
}

// ActivityEpisode is one agent activity captured from a run. It is the
// in-memory shape; persistence chunks long Content into EpisodeRecords.
type ActivityEpisode struct {
	AgentID    string        `json:"agent_id"`
	RunID      string        `json:"run_id"`
	OrgID      string        `json:"organization_id,omitempty"`
	ActionType EpisodeAction `json:"action_type"`
	Content    string        `json:"content"`
	Entities   []string      `json:"entities,omitempty"`
	Timestamp  time.Time     `json:"timestamp"`
}

// TimeDecayRelevance maps a raw relevance score and an age into a decayed
// relevance. Documented formula (exponential half-life decay):
//
//	decayed = score * 2^(-age / halfLife)   with halfLife = DefaultEpisodeHalfLife
//
// Every elapsed half-life halves the contribution: age 0 -> score, 24h ->
// score/2, 48h -> score/4. The half-life form was chosen over exp(-lambda*t)
// because the knob is directly interpretable ("how fresh must memory be to
// count half as much"). Negative ages (future timestamps, clock skew) clamp
// to 0 — recency never boosts a score above its raw value; a non-positive
// half-life disables decay entirely (score passes through).
func TimeDecayRelevance(score float64, age time.Duration) float64 {
	return TimeDecayRelevanceWithHalfLife(score, age, DefaultEpisodeHalfLife)
}

// TimeDecayRelevanceWithHalfLife is TimeDecayRelevance with an explicit
// half-life (tests and callers with a different freshness policy).
func TimeDecayRelevanceWithHalfLife(score float64, age, halfLife time.Duration) float64 {
	if score < 0 {
		return 0
	}
	if age < 0 {
		age = 0
	}
	if halfLife <= 0 {
		return score
	}
	return score * math.Exp2(-age.Hours()/halfLife.Hours())
}

// DefaultEpisodeHalfLife is the default recency half-life of episode search:
// memory from 24 hours ago counts half as much as memory from now.
const DefaultEpisodeHalfLife = 24 * time.Hour

// episodeCandidateLimit bounds the org-scoped episode scan per search
// (mirrors searchCandidateLimit for documents). Newest-first: episode memory
// is a recency structure, so the bounded window keeps the most recent
// episodes under load.
const episodeCandidateLimit = 2000

// EpisodeRecord is the durable shape of one episode CHUNK (an episode whose
// content exceeded DefaultChunkSize runes produces one record per chunk,
// sharing all metadata and distinguished by ChunkOrdinal + the deterministic
// ID suffix). IDs are derived from a SHA-256 fingerprint of the episode
// identity (org, agent, run, action, timestamp, content), so re-ingesting
// the same run batch converges to the same rows — batch ingest idempotency.
type EpisodeRecord struct {
	ID             string        `json:"id"`
	OrganizationID string        `json:"organization_id"`
	AgentID        string        `json:"agent_id"`
	RunID          string        `json:"run_id"`
	ActionType     EpisodeAction `json:"action_type"`
	ChunkOrdinal   int           `json:"chunk_ordinal"`
	Content        string        `json:"content"`
	Entities       []string      `json:"entities,omitempty"`
	Embedding      []float64     `json:"-"`
	EpisodeTS      time.Time     `json:"episode_ts"`
	CreatedAt      time.Time     `json:"created_at"`
}

// EpisodeStore abstracts durable episode storage. Implementations MUST scope
// every query by organization_id and stay idempotent under re-ingest
// (SaveEpisodes with identical input must not duplicate rows).
type EpisodeStore interface {
	// SaveEpisodes persists a batch of episode chunks. Idempotent per
	// EpisodeRecord.ID.
	SaveEpisodes(ctx context.Context, orgID string, episodes []EpisodeRecord) error
	// ListEpisodes returns at most limit episode chunks of one organization
	// with EpisodeTS >= now-window (window <= 0 means unbounded), newest
	// first. It is the candidate feed of SearchEpisodes.
	ListEpisodes(ctx context.Context, orgID string, window time.Duration, limit int) ([]EpisodeRecord, error)
}

// RuntimeEpisodeRecorder is the capture seam between run execution and
// episode memory (issue #130).
//
// WIRING FOLLOW-UP (documented, intentionally NOT done here): the
// internal/runtime and internal/worker packages are read-only for this track,
// so the production wiring — calling RecordRunActivities at run completion
// with the tool_call / reasoning / decision / observation events collected
// during the run — lands as a separate runtime change. The interface is
// exported at the knowledge boundary so the runtime change is additive:
// construct an EpisodeIngestor (or any RuntimeEpisodeRecorder) and pass it
// into the worker's run-completion hook.
type RuntimeEpisodeRecorder interface {
	// RecordRunActivities ingests one run's activity batch. Implementations
	// treat orgID/runID/agentID as the authoritative identity: they are
	// stamped onto every episode before persistence.
	RecordRunActivities(ctx context.Context, orgID, runID, agentID string, episodes []ActivityEpisode) error
}

// Compile-time proof the ingestor satisfies the runtime capture seam.
var _ RuntimeEpisodeRecorder = (*EpisodeIngestor)(nil)

// EpisodeIngestor batches episodes per run and ingests them into an
// EpisodeStore through the existing chunker + embedder seams. Dual-mode:
// in-memory (zero infrastructure) or Postgres (migration 035).
type EpisodeIngestor struct {
	store    EpisodeStore
	embedder Embedder
	now      func() time.Time
}

// NewEpisodeIngestor returns the in-memory ingestor (zero-infrastructure
// mode) with the environment-selected embedder.
func NewEpisodeIngestor() *EpisodeIngestor {
	return newEpisodeIngestor(NewInMemoryEpisodeStore(), NewEmbedderFromEnv())
}

// NewEpisodeIngestorWithStore returns a Postgres-backed ingestor
// (migration 035 knowledge_episodes) with the environment-selected embedder.
func NewEpisodeIngestorWithStore(db *sql.DB) *EpisodeIngestor {
	return newEpisodeIngestor(NewPostgresEpisodeStore(db), NewEmbedderFromEnv())
}

// NewEpisodeIngestorWith returns an ingestor over a custom store + embedder
// (tests wire deterministic fakes here).
func NewEpisodeIngestorWith(store EpisodeStore, embedder Embedder) *EpisodeIngestor {
	return newEpisodeIngestor(store, embedder)
}

func newEpisodeIngestor(store EpisodeStore, embedder Embedder) *EpisodeIngestor {
	if store == nil {
		store = NewInMemoryEpisodeStore()
	}
	if embedder == nil {
		embedder = NewHashEmbedder()
	}
	return &EpisodeIngestor{store: store, embedder: embedder, now: time.Now}
}

// RecordRunActivities implements RuntimeEpisodeRecorder. The batch is
// ingested as ONE store call. Identity: orgID/runID/agentID win over the
// per-episode fields (the run engine owns identity). Episodes with a zero
// Timestamp are stamped with the ingest time. An embedder failure is
// NON-FATAL (documented deviation, mirroring Service.IngestDocument): the
// chunks are stored unembedded and episode search degrades to lexical
// scoring; the wrapped error is still returned so callers can observe the
// degradation instead of losing it silently.
func (ing *EpisodeIngestor) RecordRunActivities(ctx context.Context, orgID, runID, agentID string, episodes []ActivityEpisode) error {
	if strings.TrimSpace(orgID) == "" {
		return errors.New("knowledge: organization id is required")
	}
	if strings.TrimSpace(runID) == "" {
		return errors.New("knowledge: run id is required")
	}
	if strings.TrimSpace(agentID) == "" {
		return errors.New("knowledge: agent id is required")
	}

	now := ing.now().UTC()
	texts := make([]string, 0, len(episodes))
	records := make([]EpisodeRecord, 0, len(episodes))
	for i, ep := range episodes {
		if !ValidEpisodeAction(ep.ActionType) {
			return fmt.Errorf("knowledge: episode %d has invalid action_type %q (want tool_call|reasoning|decision|observation)", i, ep.ActionType)
		}
		if strings.TrimSpace(ep.Content) == "" {
			return fmt.Errorf("knowledge: episode %d has empty content", i)
		}
		ts := ep.Timestamp
		if ts.IsZero() {
			ts = now
		} else {
			ts = ts.UTC()
		}
		// Chunker seam: long tool outputs/reasoning are split with the same
		// ~800-rune boundary-aware chunker as documents.
		chunks := ChunkText(ep.Content)
		for ordinal, chunk := range chunks {
			records = append(records, EpisodeRecord{
				ID:             episodeRecordID(orgID, agentID, runID, ep.ActionType, ts, ep.Content, ordinal),
				OrganizationID: orgID,
				AgentID:        agentID,
				RunID:          runID,
				ActionType:     ep.ActionType,
				ChunkOrdinal:   ordinal,
				Content:        chunk,
				Entities:       append([]string(nil), ep.Entities...),
				EpisodeTS:      ts,
				CreatedAt:      now,
			})
			texts = append(texts, chunk)
		}
	}
	if len(records) == 0 {
		return nil // empty batch: nothing to persist, not an error
	}

	// Embedder seam: one batch call for the whole run (same shape as the
	// document ingest pipeline).
	vectors, embedErr := ing.embedder.Embed(ctx, texts)
	if embedErr == nil && len(vectors) != len(records) {
		embedErr = fmt.Errorf("knowledge: episode embedder returned %d vectors for %d chunks", len(vectors), len(records))
	}
	if embedErr != nil {
		vectors = nil // store unembedded; search falls back to lexical
	}
	for i := range records {
		if vectors != nil {
			records[i].Embedding = vectors[i]
		}
	}
	if err := ing.store.SaveEpisodes(ctx, orgID, records); err != nil {
		return err
	}
	return embedErr
}

// episodeRecordID derives the deterministic record id: SHA-256 over the
// episode identity plus content (not the chunk text — chunks of one episode
// share the fingerprint base and differ only by ordinal suffix). The
// derivation is what makes batch ingest idempotent.
func episodeRecordID(orgID, agentID, runID string, action EpisodeAction, ts time.Time, content string, ordinal int) string {
	base := strings.Join([]string{orgID, agentID, runID, string(action), ts.Format(time.RFC3339Nano), content}, "\x1f")
	sum := sha256.Sum256([]byte(base))
	return hex.EncodeToString(sum[:16]) + fmt.Sprintf("-%04d", ordinal)
}

// EpisodeHit is one SearchEpisodes result: the episode chunk plus its raw
// relevance score and its time-decayed relevance.
type EpisodeHit struct {
	Episode   ActivityEpisode `json:"episode"`
	Score     float64         `json:"score"`     // raw relevance (cosine or lexical) in [0,1]
	Relevance float64         `json:"relevance"` // Score after TimeDecayRelevance
}

// SearchEpisodes runs the temporal query: org-scoped episodes within the
// window ("last N hours"; window <= 0 = unbounded), scored against the query
// with the same cosine/lexical fallback logic as document search, then
// weighted by TimeDecayRelevance. Results are ordered by decayed relevance
// (desc), then newest first, then record id — deterministic under ties.
// Episode content chunks carry their episode's metadata, so hits are
// self-describing for the memory prompt builder.
func (ing *EpisodeIngestor) SearchEpisodes(ctx context.Context, orgID, query string, window time.Duration) ([]EpisodeHit, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, errors.New("knowledge: organization id is required")
	}
	if strings.TrimSpace(query) == "" {
		return nil, errors.New("knowledge: query is required")
	}

	records, err := ing.store.ListEpisodes(ctx, orgID, window, episodeCandidateLimit)
	if err != nil {
		return nil, err
	}

	var queryVec []float64
	if vecs, err := ing.embedder.Embed(ctx, []string{query}); err == nil && len(vecs) > 0 {
		queryVec = vecs[0]
	}
	queryTokens := tokenSet(query)

	now := ing.now()
	hits := make([]EpisodeHit, 0, len(records))
	for _, rec := range records {
		score := 0.0
		if len(queryVec) > 0 && len(rec.Embedding) == len(queryVec) {
			score = cosineSimilarity(queryVec, rec.Embedding)
		} else {
			score = lexicalScore(queryTokens, rec.Content)
		}
		if score <= scoreFloor {
			continue // same noise floor as document search
		}
		relevance := TimeDecayRelevance(score, now.Sub(rec.EpisodeTS))
		hits = append(hits, EpisodeHit{
			Episode: ActivityEpisode{
				AgentID:    rec.AgentID,
				RunID:      rec.RunID,
				OrgID:      rec.OrganizationID,
				ActionType: rec.ActionType,
				Content:    rec.Content,
				Entities:   rec.Entities,
				Timestamp:  rec.EpisodeTS,
			},
			Score:     score,
			Relevance: relevance,
		})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Relevance != hits[j].Relevance {
			return hits[i].Relevance > hits[j].Relevance
		}
		if !hits[i].Episode.Timestamp.Equal(hits[j].Episode.Timestamp) {
			return hits[i].Episode.Timestamp.After(hits[j].Episode.Timestamp)
		}
		return episodeContentKey(hits[i]) < episodeContentKey(hits[j])
	})
	if len(hits) > MaxSearchK {
		hits = hits[:MaxSearchK]
	}
	return hits, nil
}

// episodeContentKey is the final tie-breaker (agent/run/action/ordinal are
// not recoverable from the hit alone; the fingerprint is stable per content).
func episodeContentKey(h EpisodeHit) string {
	return h.Episode.RunID + "\x1f" + h.Episode.AgentID + "\x1f" + h.Episode.Content
}

// ---------------------------------------------------------------------------
// In-memory episode store (zero-infrastructure mode).
// ---------------------------------------------------------------------------

// memoryEpisodeStore is the mutex-guarded in-memory EpisodeStore. Save is
// idempotent by construction (map keyed on the deterministic record id).
type memoryEpisodeStore struct {
	mu    sync.Mutex
	byID  map[string]EpisodeRecord
	byOrg map[string][]string // orgID -> record ids (insertion order)
}

// NewInMemoryEpisodeStore returns the in-memory EpisodeStore.
func NewInMemoryEpisodeStore() EpisodeStore {
	return &memoryEpisodeStore{byID: make(map[string]EpisodeRecord), byOrg: make(map[string][]string)}
}

// SaveEpisodes stores the batch; records already present (same deterministic
// id) are left untouched — batch ingest idempotency without SQL.
func (m *memoryEpisodeStore) SaveEpisodes(_ context.Context, orgID string, episodes []EpisodeRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, rec := range episodes {
		if _, ok := m.byID[rec.ID]; ok {
			continue
		}
		rec.OrganizationID = orgID // tenant stamp (defense in depth)
		m.byID[rec.ID] = rec
		m.byOrg[orgID] = append(m.byOrg[orgID], rec.ID)
	}
	return nil
}

// ListEpisodes returns the org's records with EpisodeTS >= now-window
// (window <= 0 unbounded), newest first (ties by id) — the same ordering the
// Postgres index provides.
func (m *memoryEpisodeStore) ListEpisodes(_ context.Context, orgID string, window time.Duration, limit int) ([]EpisodeRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	out := make([]EpisodeRecord, 0, len(m.byOrg[orgID]))
	for _, id := range m.byOrg[orgID] {
		rec, ok := m.byID[id]
		if !ok || rec.OrganizationID != orgID {
			continue
		}
		if window > 0 && rec.EpisodeTS.Before(now.Add(-window)) {
			continue
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].EpisodeTS.Equal(out[j].EpisodeTS) {
			return out[i].EpisodeTS.After(out[j].EpisodeTS)
		}
		return out[i].ID < out[j].ID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Postgres episode store (migration 035 knowledge_episodes).
// ---------------------------------------------------------------------------

// SQL for episode persistence. Tenant guard: organization_id on every
// statement. Idempotency: the deterministic record id plus ON CONFLICT DO
// NOTHING makes re-ingesting a run a no-op. No FK on agent_id/run_id: run
// retention must never cascade into long-lived memory (documented).
const (
	sqlInsertEpisode = `INSERT INTO knowledge_episodes (id, organization_id, agent_id, run_id, action_type, chunk_ordinal, content, entities, embedding, episode_ts, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9::jsonb, $10, $11) ON CONFLICT (id) DO NOTHING`
	// Tenant guard: org-scoped window query, newest-first, bounded by LIMIT
	// (ranking happens in Go via TimeDecayRelevance). $2 NULL = unbounded.
	sqlSelectEpisodes = `SELECT id, organization_id, agent_id, run_id, action_type, chunk_ordinal, content, COALESCE(entities::text, '[]'), COALESCE(embedding::text, ''), episode_ts, created_at FROM knowledge_episodes WHERE organization_id = $1 AND ($2::timestamptz IS NULL OR episode_ts >= $2) ORDER BY episode_ts DESC, id ASC LIMIT $3`
)

// pgEpisodeStore is the Postgres-backed EpisodeStore (migration 035).
type pgEpisodeStore struct {
	db *sql.DB
}

// NewPostgresEpisodeStore returns an EpisodeStore backed by *sql.DB.
func NewPostgresEpisodeStore(db *sql.DB) EpisodeStore {
	return &pgEpisodeStore{db: db}
}

func (s *pgEpisodeStore) guard() error {
	if s == nil || s.db == nil {
		return errors.New("knowledge: episode database is nil")
	}
	return nil
}

// SaveEpisodes inserts the batch in one transaction (all-or-nothing, same
// policy as CreateDocument).
func (s *pgEpisodeStore) SaveEpisodes(ctx context.Context, orgID string, episodes []EpisodeRecord) error {
	if err := s.guard(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, rec := range episodes {
		if _, err := tx.ExecContext(ctx, sqlInsertEpisode,
			rec.ID, orgID, rec.AgentID, rec.RunID, string(rec.ActionType),
			rec.ChunkOrdinal, rec.Content, entitiesParam(rec.Entities),
			embeddingParam(rec.Embedding), rec.EpisodeTS, rec.CreatedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListEpisodes returns the org-scoped windowed candidate feed (newest first).
func (s *pgEpisodeStore) ListEpisodes(ctx context.Context, orgID string, window time.Duration, limit int) ([]EpisodeRecord, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	var since any
	if window > 0 {
		since = time.Now().Add(-window)
	}
	if limit <= 0 {
		limit = episodeCandidateLimit
	}
	rows, err := s.db.QueryContext(ctx, sqlSelectEpisodes, orgID, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]EpisodeRecord, 0)
	for rows.Next() {
		var rec EpisodeRecord
		var action, entitiesRaw, embeddingRaw string
		if err := rows.Scan(&rec.ID, &rec.OrganizationID, &rec.AgentID, &rec.RunID,
			&action, &rec.ChunkOrdinal, &rec.Content, &entitiesRaw, &embeddingRaw,
			&rec.EpisodeTS, &rec.CreatedAt); err != nil {
			return nil, err
		}
		rec.ActionType = EpisodeAction(action)
		rec.Entities = entitiesFromParam(entitiesRaw)
		rec.Embedding = embeddingFromParam(embeddingRaw)
		out = append(out, rec)
	}
	return out, rows.Err()
}

// entitiesParam marshals the entity list for the JSONB column; empty stays
// '[]' (the column is NOT NULL).
func entitiesParam(entities []string) any {
	if len(entities) == 0 {
		return "[]"
	}
	b, err := json.Marshal(entities)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func entitiesFromParam(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}
