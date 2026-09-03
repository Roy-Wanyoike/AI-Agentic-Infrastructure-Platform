package memory

import (
	"context"
	"errors"
	"math"
	"reflect"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func newMockDB(t *testing.T) (*pgStore, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	return &pgStore{db: db}, mock, func() { _ = db.Close() }
}

func testSnippet(agentID string) *Snippet {
	expires := time.Date(2025, 7, 1, 0, 0, 0, 0, time.UTC)
	return &Snippet{
		ID:             "sn-1",
		OrganizationID: "org-1",
		AgentID:        agentID,
		Scope:          ScopeShortTerm,
		Content:        "prefers concise answers",
		Importance:     0.8,
		ExpiresAt:      &expires,
		Embedding:      []float64{0.25, 0.75},
		CreatedAt:      time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC),
		UpdatedAt:      time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC),
	}
}

func TestPGReplaceAgentSnippets(t *testing.T) {
	store, mock, closeDB := newMockDB(t)
	defer closeDB()

	sn := testSnippet("agent-1")
	mock.ExpectBegin()
	// Tenant guard: the scope delete is keyed by (organization_id, agent_id).
	mock.ExpectExec(regexp.QuoteMeta(sqlDeleteAgentSnippets)).
		WithArgs("org-1", "agent-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(sqlInsertSnippet)).
		WithArgs(sn.ID, "org-1", "agent-1", sn.Scope, sn.Content, sn.Importance,
			sn.ExpiresAt, embeddingParam(sn.Embedding), sn.CreatedAt, sn.UpdatedAt).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err := store.ReplaceAgentSnippets(context.Background(), "org-1", "agent-1", []*Snippet{sn}); err != nil {
		t.Fatalf("ReplaceAgentSnippets returned error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPGReplaceAgentSnippetsOrgLevelUsesNullAgent(t *testing.T) {
	store, mock, closeDB := newMockDB(t)
	defer closeDB()

	sn := testSnippet("")
	sn.ExpiresAt = nil
	sn.Embedding = nil
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(sqlDeleteAgentSnippets)).
		WithArgs("org-1", "").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(sqlInsertSnippet)).
		WithArgs(sn.ID, "org-1", nil, sn.Scope, sn.Content, sn.Importance,
			nil, nil, sn.CreatedAt, sn.UpdatedAt).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err := store.ReplaceAgentSnippets(context.Background(), "org-1", "", []*Snippet{sn}); err != nil {
		t.Fatalf("ReplaceAgentSnippets(org-level) returned error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPGReplaceRollsBackOnInsertError(t *testing.T) {
	store, mock, closeDB := newMockDB(t)
	defer closeDB()

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(sqlDeleteAgentSnippets)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(sqlInsertSnippet)).
		WillReturnError(errors.New("insert failed"))
	mock.ExpectRollback()

	if err := store.ReplaceAgentSnippets(context.Background(), "org-1", "agent-1",
		[]*Snippet{testSnippet("agent-1")}); err == nil {
		t.Fatal("insert failure must abort the replacement")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func snippetRows(agentID string, sn *Snippet) *sqlmock.Rows {
	expires := any(nil)
	if sn.ExpiresAt != nil {
		expires = *sn.ExpiresAt
	}
	embedding := any(nil)
	if len(sn.Embedding) > 0 {
		embedding = `[0.25,0.75]`
	}
	return sqlmock.NewRows([]string{
		"id", "organization_id", "COALESCE(agent_id, '')", "scope", "content",
		"importance", "expires_at", "COALESCE(embedding::text, '')", "created_at", "updated_at",
	}).AddRow(sn.ID, sn.OrganizationID, agentID, sn.Scope, sn.Content,
		sn.Importance, expires, embedding, sn.CreatedAt, sn.UpdatedAt)
}

func TestPGListSnippetsScoped(t *testing.T) {
	store, mock, closeDB := newMockDB(t)
	defer closeDB()

	sn := testSnippet("agent-1")
	// Tenant guard: WHERE organization_id = $1 AND agent_id = $2 (+ expiry guard).
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectSnippetsByAgent)).
		WithArgs("org-1", "agent-1").
		WillReturnRows(snippetRows("agent-1", sn))

	got, err := store.ListSnippets(context.Background(), "org-1", "agent-1")
	if err != nil {
		t.Fatalf("ListSnippets returned error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 snippet, got %d", len(got))
	}
	if got[0].AgentID != "agent-1" || got[0].Scope != ScopeShortTerm {
		t.Fatalf("unexpected snippet: %+v", got[0])
	}
	if got[0].ExpiresAt == nil || !got[0].ExpiresAt.Equal(*sn.ExpiresAt) {
		t.Fatalf("expires_at round-trip failed: %+v", got[0].ExpiresAt)
	}
	if !reflect.DeepEqual(got[0].Embedding, []float64{0.25, 0.75}) {
		t.Fatalf("embedding round-trip failed: %v", got[0].Embedding)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPGListSnippetsAllOrgsWhenAgentEmpty(t *testing.T) {
	store, mock, closeDB := newMockDB(t)
	defer closeDB()

	// Tenant guard: WHERE organization_id = $1 (no agent filter).
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectSnippetsAll)).
		WithArgs("org-1").
		WillReturnRows(snippetRows("", testSnippet("")))

	got, err := store.ListSnippets(context.Background(), "org-1", "")
	if err != nil {
		t.Fatalf("ListSnippets returned error: %v", err)
	}
	if len(got) != 1 || got[0].AgentID != "" {
		t.Fatalf("unexpected org listing: %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPGListSnippetsForAgentIncludesOrgLevel(t *testing.T) {
	store, mock, closeDB := newMockDB(t)
	defer closeDB()

	// Retrieval candidate set: agent rows + shared org-level rows.
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectSnippetsForAgent)).
		WithArgs("org-1", "agent-1").
		WillReturnRows(snippetRows("agent-1", testSnippet("agent-1")))

	got, err := store.ListSnippetsForAgent(context.Background(), "org-1", "agent-1")
	if err != nil {
		t.Fatalf("ListSnippetsForAgent returned error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(got))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}

	if _, err := store.ListSnippetsForAgent(context.Background(), "org-1", ""); err == nil {
		t.Fatal("ListSnippetsForAgent without agent id must be rejected")
	}
}

func TestPGGuardNilDB(t *testing.T) {
	var store *pgStore
	if err := store.guard(); err == nil {
		t.Fatal("nil database must be guarded")
	}
}

func TestEmbeddingParamRoundTrip(t *testing.T) {
	if embeddingParam(nil) != nil {
		t.Fatal("nil embedding must map to SQL NULL")
	}
	raw, ok := embeddingParam([]float64{0.5, -0.25}).(string)
	if !ok {
		t.Fatalf("embedding param should marshal to a JSON string, got %T", embeddingParam([]float64{0.5}))
	}
	if got := embeddingFromParam(raw); !reflect.DeepEqual(got, []float64{0.5, -0.25}) {
		t.Fatalf("embedding round-trip mismatch: %v", got)
	}
	if embeddingFromParam("") != nil {
		t.Fatal("empty column must scan to nil")
	}
	if embeddingFromParam("not json") != nil {
		t.Fatal("corrupt column must scan to nil (degrade, never panic)")
	}
}

func TestHashEmbedderDeterministic(t *testing.T) {
	embedder := NewHashEmbedder()
	ctx := context.Background()
	a, err := embedder.Embed(ctx, "The customer prefers email invoices monthly")
	if err != nil {
		t.Fatalf("Embed returned error: %v", err)
	}
	b, _ := embedder.Embed(ctx, "The customer prefers email invoices monthly")
	if len(a) != EmbeddingDim || len(b) != EmbeddingDim {
		t.Fatalf("embedding dimension mismatch: %d/%d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("hash embedder must be deterministic at index %d", i)
		}
	}
	norm := 0.0
	for _, v := range a {
		norm += v * v
	}
	if math.Abs(math.Sqrt(norm)-1) > 1e-9 {
		t.Fatalf("embedding should be L2-normalized, got norm %.6f", math.Sqrt(norm))
	}
	same, _ := embedder.Embed(ctx, "email invoices")
	if cosineSimilarity(a, same) <= 0.1 {
		t.Fatalf("overlapping vocabulary should score positively, got %.3f", cosineSimilarity(a, same))
	}
	other, _ := embedder.Embed(ctx, "zzz qqq xxx")
	if cosineSimilarity(a, other) > 0.25 {
		t.Fatalf("disjoint vocabulary should score low, got %.3f", cosineSimilarity(a, other))
	}
	empty, _ := embedder.Embed(ctx, "")
	if cosineSimilarity(a, empty) != 0 {
		t.Fatal("empty text must embed to a zero vector (cosine 0)")
	}
}

func TestCosineSimilarityEdgeCases(t *testing.T) {
	if cosineSimilarity(nil, nil) != 0 {
		t.Fatal("empty vectors score 0")
	}
	if cosineSimilarity([]float64{1, 0}, []float64{0, 1, 0}) != 0 {
		t.Fatal("dimension mismatch scores 0")
	}
	if cosineSimilarity([]float64{1, 0}, []float64{0, 1}) != 0 {
		t.Fatal("orthogonal vectors score 0")
	}
	if math.Abs(cosineSimilarity([]float64{1, 2}, []float64{2, 4})-1) > 1e-12 {
		t.Fatal("parallel vectors score 1")
	}
	if cosineSimilarity([]float64{1, 0}, []float64{-1, 0}) != 0 {
		t.Fatal("negative similarity clamps to 0")
	}
	if math.IsNaN(cosineSimilarity([]float64{0, 0}, []float64{1, 1})) {
		t.Fatal("zero vectors must not produce NaN")
	}
}
