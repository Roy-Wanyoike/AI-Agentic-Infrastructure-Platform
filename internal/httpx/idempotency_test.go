package httpx

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// countingHandler records how many times the real backend executed.
type countingHandler struct {
	calls  int
	status int
	body   string
}

func (h *countingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.calls++
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(h.status)
	_, _ = w.Write([]byte(h.body))
}

func TestIdempotencyMiddlewareReplaysPost(t *testing.T) {
	store := NewInMemoryIdempotencyStore()
	backend := &countingHandler{status: http.StatusCreated, body: `{"id":"run-1"}`}
	handler := NewIdempotencyMiddleware(store)(backend)

	postWithKey := func(key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/runs", strings.NewReader("{}"))
		req.Header.Set(IdempotencyKeyHeader, key)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	first := postWithKey("idem-1")
	if first.Code != http.StatusCreated {
		t.Fatalf("first execution should return backend status, got %d", first.Code)
	}
	if first.Body.String() != `{"id":"run-1"}` {
		t.Fatalf("unexpected first body: %q", first.Body.String())
	}
	if replay := first.Header().Get(IdempotentReplayHeader); replay == "true" {
		t.Fatal("first execution must not be marked as replay")
	}

	second := postWithKey("idem-1")
	if second.Code != http.StatusCreated || second.Body.String() != `{"id":"run-1"}` {
		t.Fatalf("replay should return stored status+body, got %d %q", second.Code, second.Body.String())
	}
	if second.Header().Get(IdempotentReplayHeader) != "true" {
		t.Fatal("replay must set X-Idempotent-Replay: true")
	}
	if backend.calls != 1 {
		t.Fatalf("backend must execute once, got %d calls", backend.calls)
	}

	// A different key is a different operation.
	_ = postWithKey("idem-2")
	if backend.calls != 2 {
		t.Fatalf("new key should execute the backend, got %d calls", backend.calls)
	}
}

func TestIdempotencyMiddlewarePassThroughRules(t *testing.T) {
	store := NewInMemoryIdempotencyStore()
	backend := &countingHandler{status: http.StatusOK, body: "ok"}
	handler := NewIdempotencyMiddleware(store)(backend)

	// Non-POST with a key passes through every time.
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/runs", nil)
		req.Header.Set(IdempotencyKeyHeader, "idem-get")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET should pass through, got %d", rec.Code)
		}
	}
	// POST without a key passes through.
	req := httptest.NewRequest(http.MethodPost, "/v1/runs", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST without key should pass through, got %d", rec.Code)
	}
	if backend.calls != 4 {
		t.Fatalf("expected 4 backend executions, got %d", backend.calls)
	}
}

func TestIdempotencyMiddlewareDoesNotStoreServerErrors(t *testing.T) {
	store := NewInMemoryIdempotencyStore()
	backend := &countingHandler{status: http.StatusInternalServerError, body: "boom"}
	handler := NewIdempotencyMiddleware(store)(backend)

	execute := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/runs", nil)
		req.Header.Set(IdempotencyKeyHeader, "idem-err")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	_ = execute()
	_ = execute()
	if backend.calls != 2 {
		t.Fatalf("5xx responses must not be replayed; expected 2 executions, got %d", backend.calls)
	}
}

func TestIdempotencyMiddlewareScopeAndOwnerPartitions(t *testing.T) {
	store := NewInMemoryIdempotencyStore()
	backend := &countingHandler{status: http.StatusOK, body: "ok"}

	// Same key on a different path executes independently (scope = path).
	runHandler := NewIdempotencyMiddleware(store)(backend)
	agentHandler := NewIdempotencyMiddleware(store)(backend)
	postTo := func(h http.Handler, path string) {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set(IdempotencyKeyHeader, "idem-scope")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
	}
	postTo(runHandler, "/v1/runs")
	postTo(agentHandler, "/v1/agents/create")
	if backend.calls != 2 {
		t.Fatalf("different scopes must execute independently, got %d calls", backend.calls)
	}

	// Different owner partitions isolate the same key even on the same path.
	ownerHandler := func(owner string) http.Handler {
		inner := NewIdempotencyMiddleware(store)(backend)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			inner.ServeHTTP(w, r.WithContext(WithIdempotencyOwner(r.Context(), owner)))
		})
	}
	postAs := func(h http.Handler) {
		req := httptest.NewRequest(http.MethodPost, "/v1/runs", nil)
		req.Header.Set(IdempotencyKeyHeader, "idem-owner")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
	}
	postAs(ownerHandler("org-a"))
	postAs(ownerHandler("org-a"))
	if backend.calls != 3 {
		t.Fatalf("same owner+key should replay; expected 3 executions so far, got %d", backend.calls)
	}
	postAs(ownerHandler("org-b"))
	if backend.calls != 4 {
		t.Fatalf("different owner must execute, got %d calls", backend.calls)
	}
}

func TestInMemoryIdempotencyStoreExpiry(t *testing.T) {
	store := NewInMemoryIdempotencyStore()
	ctx := context.Background()
	rec := &IdempotencyRecord{
		Key: "k", OrganizationID: "org", Scope: "/p", StatusCode: 200, Body: []byte("x"),
		CreatedAt: time.Now().UTC().Add(-time.Hour), ExpiresAt: time.Now().UTC().Add(-time.Minute),
	}
	if err := store.Put(ctx, rec); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := store.Get(ctx, "org", "k", "/p")
	if err != nil || got != nil {
		t.Fatalf("expired record must be dropped, got %+v err %v", got, err)
	}
}

func TestPostgresIdempotencyStore(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	store := NewPostgresIdempotencyStore(db)
	ctx := context.Background()

	// Miss -> (nil, nil).
	mock.ExpectQuery(`SELECT key, organization_id, scope, status_code, content_type, response_body, created_at, expires_at`).
		WithArgs("org-1", "idem-1", "/v1/runs").
		WillReturnError(sql.ErrNoRows)
	got, err := store.Get(ctx, "org-1", "idem-1", "/v1/runs")
	if err != nil || got != nil {
		t.Fatalf("expected miss (nil, nil), got %+v err %v", got, err)
	}

	// Hit -> record with body.
	mock.ExpectQuery(`SELECT key, organization_id, scope, status_code, content_type, response_body, created_at, expires_at`).
		WithArgs("org-1", "idem-1", "/v1/runs").
		WillReturnRows(sqlmock.NewRows([]string{
			"key", "organization_id", "scope", "status_code", "content_type", "response_body", "created_at", "expires_at",
		}).AddRow("idem-1", "org-1", "/v1/runs", 201, "application/json", `{"id":"run-1"}`, time.Now().UTC(), time.Now().UTC().Add(time.Hour)))
	got, err = store.Get(ctx, "org-1", "idem-1", "/v1/runs")
	if err != nil {
		t.Fatalf("Get hit failed: %v", err)
	}
	if got == nil || got.StatusCode != 201 || string(got.Body) != `{"id":"run-1"}` {
		t.Fatalf("unexpected record: %+v", got)
	}

	// Put -> upsert.
	mock.ExpectExec(`INSERT INTO idempotency_keys`).
		WithArgs("idem-2", "org-1", "/v1/runs", 201, "application/json", `{"ok":true}`, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := store.Put(ctx, &IdempotencyRecord{
		Key: "idem-2", OrganizationID: "org-1", Scope: "/v1/runs",
		StatusCode: 201, ContentType: "application/json", Body: []byte(`{"ok":true}`),
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPostgresIdempotencyStoreNilDB(t *testing.T) {
	store := NewPostgresIdempotencyStore(nil)
	if got, err := store.Get(context.Background(), "org", "k", "/p"); got != nil || err != nil {
		t.Fatalf("nil db should behave as miss, got %+v err %v", got, err)
	}
	if err := store.Put(context.Background(), &IdempotencyRecord{Key: "k"}); err != nil {
		t.Fatalf("nil db Put should no-op, got %v", err)
	}
}
