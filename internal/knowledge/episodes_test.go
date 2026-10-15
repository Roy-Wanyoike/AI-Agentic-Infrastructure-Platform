package knowledge

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// ---------------------------------------------------------------------------
// Test doubles (deterministic fakes, no network).
// ---------------------------------------------------------------------------

// recordingEpisodeStore captures SaveEpisodes batches and serves ListEpisodes
// from memory (for ingestor-level assertions).
type recordingEpisodeStore struct {
	mu      sync.Mutex
	saved   map[string]EpisodeRecord
	batches [][]EpisodeRecord
	now     time.Time
}

func newRecordingEpisodeStore() *recordingEpisodeStore {
	return &recordingEpisodeStore{saved: make(map[string]EpisodeRecord)}
}

func (r *recordingEpisodeStore) SaveEpisodes(_ context.Context, orgID string, episodes []EpisodeRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := append([]EpisodeRecord(nil), episodes...)
	r.batches = append(r.batches, cp)
	for _, rec := range cp {
		rec.OrganizationID = orgID
		r.saved[rec.ID] = rec
	}
	return nil
}

func (r *recordingEpisodeStore) ListEpisodes(_ context.Context, orgID string, window time.Duration, limit int) ([]EpisodeRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now
	if now.IsZero() {
		now = time.Now()
	}
	out := make([]EpisodeRecord, 0, len(r.saved))
	for _, rec := range r.saved {
		if rec.OrganizationID != orgID {
			continue
		}
		if window > 0 && rec.EpisodeTS.Before(now.Add(-window)) {
			continue
		}
		out = append(out, rec)
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *recordingEpisodeStore) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.saved)
}

// episodeStubEmbedder is a deterministic embedder stub: canned vectors per input,
// an optional error, and a call counter (to prove batching).
type episodeStubEmbedder struct {
	mu      sync.Mutex
	calls   int
	vectors map[string][]float64
	dim     int
	err     error
}

func (s *episodeStubEmbedder) Name() string { return "stub" }

func (s *episodeStubEmbedder) Embed(_ context.Context, inputs []string) ([][]float64, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	out := make([][]float64, len(inputs))
	for i, in := range inputs {
		if v, ok := s.vectors[in]; ok {
			out[i] = v
			continue
		}
		out[i] = make([]float64, s.dim)
	}
	return out, nil
}

func (s *episodeStubEmbedder) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// ---------------------------------------------------------------------------
// Time-decay math (documented formula: score * 2^(-age/halfLife)).
// ---------------------------------------------------------------------------

func TestTimeDecayRelevanceFormula(t *testing.T) {
	cases := []struct {
		name     string
		score    float64
		age      time.Duration
		halfLife time.Duration
		want     float64
	}{
		{"fresh memory is untouched", 1.0, 0, DefaultEpisodeHalfLife, 1.0},
		{"one half-life halves the score", 1.0, 24 * time.Hour, DefaultEpisodeHalfLife, 0.5},
		{"two half-lives quarter it", 1.0, 48 * time.Hour, DefaultEpisodeHalfLife, 0.25},
		{"partial half-life is exp2", 0.8, 12 * time.Hour, DefaultEpisodeHalfLife, 0.8 * math.Exp2(-0.5)},
		{"negative age clamps to zero", 0.7, -1 * time.Hour, DefaultEpisodeHalfLife, 0.7},
		{"negative score is zero", -0.5, 0, DefaultEpisodeHalfLife, 0},
		{"zero half-life disables decay", 0.6, 100 * time.Hour, 0, 0.6},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := TimeDecayRelevanceWithHalfLife(tc.score, tc.age, tc.halfLife)
			if math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("TimeDecayRelevanceWithHalfLife(%v, %v, %v) = %v, want %v", tc.score, tc.age, tc.halfLife, got, tc.want)
			}
		})
	}
	if TimeDecayRelevance(1.0, 24*time.Hour) != TimeDecayRelevanceWithHalfLife(1.0, 24*time.Hour, DefaultEpisodeHalfLife) {
		t.Fatal("TimeDecayRelevance must delegate to the default half-life")
	}
}

// ---------------------------------------------------------------------------
// Ingest: batching, identity stamping, idempotency, validation.
// ---------------------------------------------------------------------------

func sampleEpisodes() []ActivityEpisode {
	base := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	return []ActivityEpisode{
		{ActionType: EpisodeActionToolCall, Content: "called github.create_issue with title 'bug'", Entities: []string{"github", "create_issue"}, Timestamp: base},
		{ActionType: EpisodeActionReasoning, Content: "the failing test is TestSearch; likely index staleness", Timestamp: base.Add(time.Minute)},
		{ActionType: EpisodeActionDecision, Content: "approved deploy of payments-service v2", Entities: []string{"payments-service"}, Timestamp: base.Add(2 * time.Minute)},
	}
}

func TestRecordRunActivitiesBatchesAndStampsIdentity(t *testing.T) {
	store := newRecordingEpisodeStore()
	embedder := &episodeStubEmbedder{dim: OfflineEmbeddingDim}
	ing := NewEpisodeIngestorWith(store, embedder)

	if err := ing.RecordRunActivities(context.Background(), "org-1", "run-1", "agent-1", sampleEpisodes()); err != nil {
		t.Fatalf("RecordRunActivities returned error: %v", err)
	}
	if store.count() != 3 {
		t.Fatalf("expected 3 episode chunks stored, got %d", store.count())
	}
	// One batch -> exactly one embedder call (batching seam).
	if embedder.callCount() != 1 {
		t.Fatalf("expected exactly 1 embedder batch call, got %d", embedder.callCount())
	}
	// Identity comes from the recorder args, not the episode structs.
	for _, rec := range store.batches[0] {
		if rec.OrganizationID != "org-1" || rec.RunID != "run-1" || rec.AgentID != "agent-1" {
			t.Fatalf("record not stamped with run identity: %+v", rec)
		}
		if rec.CreatedAt.IsZero() {
			t.Fatal("record CreatedAt must be stamped")
		}
	}
}

func TestRecordRunActivitiesIdempotent(t *testing.T) {
	store := newRecordingEpisodeStore()
	ing := NewEpisodeIngestorWith(store, NewHashEmbedder())

	if err := ing.RecordRunActivities(context.Background(), "org-1", "run-1", "agent-1", sampleEpisodes()); err != nil {
		t.Fatalf("first ingest returned error: %v", err)
	}
	first := store.count()
	if first == 0 {
		t.Fatal("first ingest stored nothing")
	}
	if err := ing.RecordRunActivities(context.Background(), "org-1", "run-1", "agent-1", sampleEpisodes()); err != nil {
		t.Fatalf("re-ingest returned error: %v", err)
	}
	if store.count() != first {
		t.Fatalf("re-ingest duplicated episodes: %d -> %d", first, store.count())
	}
	// Deterministic ids: the same batch yields the same ids.
	ids1 := map[string]bool{}
	for _, rec := range store.batches[0] {
		ids1[rec.ID] = true
	}
	if len(ids1) != first {
		t.Fatalf("record ids not unique within batch: %d ids for %d records", len(ids1), first)
	}
}

func TestRecordRunActivitiesChunksLongContent(t *testing.T) {
	store := newRecordingEpisodeStore()
	ing := NewEpisodeIngestorWith(store, NewHashEmbedder())

	long := make([]byte, 0, DefaultChunkSize*3)
	for len(long) < DefaultChunkSize*3 {
		long = append(long, []byte("observation payload token ")...)
	}
	ep := []ActivityEpisode{{ActionType: EpisodeActionObservation, Content: string(long)}}
	if err := ing.RecordRunActivities(context.Background(), "org-1", "run-1", "agent-1", ep); err != nil {
		t.Fatalf("RecordRunActivities returned error: %v", err)
	}
	if store.count() < 2 {
		t.Fatalf("long content should be chunked into >=2 records, got %d", store.count())
	}
	ordinals := map[int]bool{}
	for _, rec := range store.batches[0] {
		ordinals[rec.ChunkOrdinal] = true
		if rec.ActionType != EpisodeActionObservation {
			t.Fatalf("chunk lost its action type: %+v", rec)
		}
	}
	if len(ordinals) != store.count() {
		t.Fatalf("chunk ordinals not unique: %v", ordinals)
	}
}

func TestRecordRunActivitiesValidation(t *testing.T) {
	ing := NewEpisodeIngestorWith(newRecordingEpisodeStore(), NewHashEmbedder())
	ctx := context.Background()

	if err := ing.RecordRunActivities(ctx, "", "run-1", "agent-1", nil); err == nil {
		t.Fatal("empty org id must be rejected")
	}
	if err := ing.RecordRunActivities(ctx, "org-1", "", "agent-1", nil); err == nil {
		t.Fatal("empty run id must be rejected")
	}
	if err := ing.RecordRunActivities(ctx, "org-1", "run-1", "", nil); err == nil {
		t.Fatal("empty agent id must be rejected")
	}
	bad := []ActivityEpisode{{ActionType: "gossip", Content: "x"}}
	if err := ing.RecordRunActivities(ctx, "org-1", "run-1", "agent-1", bad); err == nil {
		t.Fatal("invalid action type must be rejected")
	}
	empty := []ActivityEpisode{{ActionType: EpisodeActionDecision, Content: "   "}}
	if err := ing.RecordRunActivities(ctx, "org-1", "run-1", "agent-1", empty); err == nil {
		t.Fatal("blank content must be rejected")
	}
	if err := ing.RecordRunActivities(ctx, "org-1", "run-1", "agent-1", nil); err != nil {
		t.Fatalf("empty batch must be a no-op success, got %v", err)
	}
}

func TestRecordRunActivitiesZeroTimestampStamped(t *testing.T) {
	store := newRecordingEpisodeStore()
	ing := NewEpisodeIngestorWith(store, NewHashEmbedder())
	fixed := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	ing.now = func() time.Time { return fixed }

	err := ing.RecordRunActivities(context.Background(), "org-1", "run-1", "agent-1",
		[]ActivityEpisode{{ActionType: EpisodeActionReasoning, Content: "thinking"}})
	if err != nil {
		t.Fatalf("RecordRunActivities returned error: %v", err)
	}
	for _, rec := range store.batches[0] {
		if !rec.EpisodeTS.Equal(fixed.UTC()) {
			t.Fatalf("zero timestamp should be stamped with ingest time, got %v", rec.EpisodeTS)
		}
	}
}

func TestRecordRunActivitiesEmbedderFailureNonFatal(t *testing.T) {
	store := newRecordingEpisodeStore()
	embedder := &episodeStubEmbedder{dim: OfflineEmbeddingDim, err: errors.New("embeddings down")}
	ing := NewEpisodeIngestorWith(store, embedder)

	err := ing.RecordRunActivities(context.Background(), "org-1", "run-1", "agent-1", sampleEpisodes())
	if err == nil {
		t.Fatal("embedder failure must be surfaced to the caller")
	}
	if store.count() != 3 {
		t.Fatalf("episodes must still be stored unembedded, got %d", store.count())
	}
	for _, rec := range store.batches[0] {
		if len(rec.Embedding) != 0 {
			t.Fatal("unembedded records must carry no embedding")
		}
	}
}

// ---------------------------------------------------------------------------
// Temporal queries: windows, decay ranking, org isolation.
// ---------------------------------------------------------------------------

func TestSearchEpisodesWindowAndDecay(t *testing.T) {
	store := newRecordingEpisodeStore()
	ing := NewEpisodeIngestorWith(store, NewHashEmbedder())
	now := time.Now()
	store.now = now

	fresh := ActivityEpisode{ActionType: EpisodeActionObservation, Content: "payments deploy completed", Timestamp: now.Add(-time.Hour)}
	stale := ActivityEpisode{ActionType: EpisodeActionObservation, Content: "payments deploy failed", Timestamp: now.Add(-72 * time.Hour)}
	if err := ing.RecordRunActivities(context.Background(), "org-1", "run-1", "agent-1", []ActivityEpisode{fresh, stale}); err != nil {
		t.Fatalf("RecordRunActivities returned error: %v", err)
	}

	// Window: last 24 hours only sees the fresh episode.
	hits, err := ing.SearchEpisodes(context.Background(), "org-1", "payments deploy", 24*time.Hour)
	if err != nil {
		t.Fatalf("SearchEpisodes returned error: %v", err)
	}
	if len(hits) != 1 || !hits[0].Episode.Timestamp.Equal(fresh.Timestamp.UTC()) {
		t.Fatalf("24h window should return only the fresh episode, got %+v", hits)
	}

	// Unbounded window returns both, most relevant (decayed) first: the
	// stale episode shares the query tokens but its raw score is cut by
	// 3 half-lives (72h / 24h -> 1/8).
	hits, err = ing.SearchEpisodes(context.Background(), "org-1", "payments deploy", 0)
	if err != nil {
		t.Fatalf("SearchEpisodes returned error: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("unbounded window should return both episodes, got %d", len(hits))
	}
	if !hits[0].Episode.Timestamp.Equal(fresh.Timestamp.UTC()) {
		t.Fatalf("fresh episode must outrank the decayed stale one: %+v", hits)
	}
	if hits[0].Relevance > hits[0].Score+1e-9 {
		t.Fatal("decay must never boost a score above its raw value")
	}
	for _, hit := range hits {
		age := now.Sub(hit.Episode.Timestamp)
		want := TimeDecayRelevance(hit.Score, age)
		if math.Abs(hit.Relevance-want) > 1e-9 {
			t.Fatalf("relevance = %v, want decayed %v (score %v, age %v)", hit.Relevance, want, hit.Score, age)
		}
	}
}

func TestSearchEpisodesOrgIsolation(t *testing.T) {
	store := newRecordingEpisodeStore()
	ing := NewEpisodeIngestorWith(store, NewHashEmbedder())

	if err := ing.RecordRunActivities(context.Background(), "org-1", "run-1", "agent-1", sampleEpisodes()); err != nil {
		t.Fatalf("RecordRunActivities returned error: %v", err)
	}
	hits, err := ing.SearchEpisodes(context.Background(), "org-2", "payments deploy approved", 0)
	if err != nil {
		t.Fatalf("SearchEpisodes returned error: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("org-2 must not see org-1 episodes, got %d hits", len(hits))
	}
	// Same via the store seam.
	records, err := store.ListEpisodes(context.Background(), "org-2", 0, 100)
	if err != nil || len(records) != 0 {
		t.Fatalf("store-level org isolation broken: %d records (err=%v)", len(records), err)
	}
}

func TestSearchEpisodesValidation(t *testing.T) {
	ing := NewEpisodeIngestorWith(newRecordingEpisodeStore(), NewHashEmbedder())
	if _, err := ing.SearchEpisodes(context.Background(), "", "q", 0); err == nil {
		t.Fatal("empty org id must be rejected")
	}
	if _, err := ing.SearchEpisodes(context.Background(), "org-1", "  ", 0); err == nil {
		t.Fatal("empty query must be rejected")
	}
}

func TestValidEpisodeAction(t *testing.T) {
	for _, a := range []EpisodeAction{EpisodeActionToolCall, EpisodeActionReasoning, EpisodeActionDecision, EpisodeActionObservation} {
		if !ValidEpisodeAction(a) {
			t.Fatalf("%q must be a valid episode action", a)
		}
	}
	if ValidEpisodeAction("gossip") {
		t.Fatal("unknown action must be invalid")
	}
}

// ---------------------------------------------------------------------------
// Postgres store round-trip (sqlmock).
// ---------------------------------------------------------------------------

func newMockEpisodeDB(t *testing.T) (*pgEpisodeStore, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	return &pgEpisodeStore{db: db}, mock, func() { _ = db.Close() }
}

func episodeRecordForMock(orgID string, withEmbedding bool) EpisodeRecord {
	rec := EpisodeRecord{
		ID:             "abc123-0000",
		AgentID:        "agent-1",
		RunID:          "run-1",
		ActionType:     EpisodeActionToolCall,
		ChunkOrdinal:   2,
		Content:        "called github.create_issue",
		Entities:       []string{"github"},
		EpisodeTS:      time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC),
		CreatedAt:      time.Date(2025, 6, 1, 12, 5, 0, 0, time.UTC),
		OrganizationID: orgID,
	}
	if withEmbedding {
		rec.Embedding = []float64{1, 2, 3}
	}
	return rec
}

func TestPGEpisodeStoreSaveAndList(t *testing.T) {
	store, mock, closeDB := newMockEpisodeDB(t)
	defer closeDB()

	rec := episodeRecordForMock("org-1", true)
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(sqlInsertEpisode)).
		WithArgs(rec.ID, "org-1", rec.AgentID, rec.RunID, string(rec.ActionType),
			rec.ChunkOrdinal, rec.Content, entitiesParam(rec.Entities),
			embeddingParam(rec.Embedding), rec.EpisodeTS, rec.CreatedAt).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if err := store.SaveEpisodes(context.Background(), "org-1", []EpisodeRecord{rec}); err != nil {
		t.Fatalf("SaveEpisodes returned error: %v", err)
	}

	// Empty entities marshal to '[]' (NOT NULL column).
	if got := entitiesParam(nil); got != "[]" {
		t.Fatalf("entitiesParam(nil) = %v, want '[]'", got)
	}

	rows := sqlmock.NewRows([]string{"id", "organization_id", "agent_id", "run_id", "action_type", "chunk_ordinal", "content", "entities", "embedding", "episode_ts", "created_at"}).
		AddRow(rec.ID, "org-1", rec.AgentID, rec.RunID, string(rec.ActionType), rec.ChunkOrdinal, rec.Content, `["github"]`, `[1,2,3]`, rec.EpisodeTS, rec.CreatedAt)
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectEpisodes)).
		WithArgs("org-1", sqlmock.AnyArg(), episodeCandidateLimit).
		WillReturnRows(rows)
	got, err := store.ListEpisodes(context.Background(), "org-1", 24*time.Hour, 0)
	if err != nil {
		t.Fatalf("ListEpisodes returned error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 row, got %d", len(got))
	}
	if got[0].ID != rec.ID || got[0].ActionType != rec.ActionType || got[0].ChunkOrdinal != rec.ChunkOrdinal {
		t.Fatalf("round-trip mismatch: %+v", got[0])
	}
	if len(got[0].Entities) != 1 || got[0].Entities[0] != "github" {
		t.Fatalf("entities round-trip mismatch: %v", got[0].Entities)
	}
	if len(got[0].Embedding) != 3 {
		t.Fatalf("embedding round-trip mismatch: %v", got[0].Embedding)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPGEpisodeStoreSaveRollbackOnError(t *testing.T) {
	store, mock, closeDB := newMockEpisodeDB(t)
	defer closeDB()

	rec := episodeRecordForMock("org-1", false)
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(sqlInsertEpisode)).WillReturnError(errors.New("insert failed"))
	mock.ExpectRollback()
	if err := store.SaveEpisodes(context.Background(), "org-1", []EpisodeRecord{rec}); err == nil {
		t.Fatal("insert failure must abort the batch")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPGEpisodeStoreGuard(t *testing.T) {
	var nilStore *pgEpisodeStore
	if err := nilStore.guard(); err == nil {
		t.Fatal("nil db must be guarded")
	}
}

// Full round-trip through the ingestor against the SQL seam: the same run
// ingested twice issues the identical INSERT statements (idempotency at the
// persistence layer, ON CONFLICT (id) DO NOTHING).
func TestEpisodeIngestorPGRoundTripIdempotent(t *testing.T) {
	db, mock, closeDB := newMockDBForEpisodes(t)
	defer closeDB()

	ing := NewEpisodeIngestorWith(NewPostgresEpisodeStore(db), NewHashEmbedder())
	episodes := sampleEpisodes()

	for round := 0; round < 2; round++ {
		mock.ExpectBegin()
		for range episodes {
			mock.ExpectExec(regexp.QuoteMeta(sqlInsertEpisode)).
				WithArgs(sqlmock.AnyArg(), "org-1", "agent-1", "run-1", sqlmock.AnyArg(),
					sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
					sqlmock.AnyArg(), sqlmock.AnyArg()).
				WillReturnResult(sqlmock.NewResult(0, 1))
		}
		mock.ExpectCommit()
		if err := ing.RecordRunActivities(context.Background(), "org-1", "run-1", "agent-1", episodes); err != nil {
			t.Fatalf("round %d RecordRunActivities returned error: %v", round, err)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func newMockDBForEpisodes(t *testing.T) (*sql.DB, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	return db, mock, func() { _ = db.Close() }
}
