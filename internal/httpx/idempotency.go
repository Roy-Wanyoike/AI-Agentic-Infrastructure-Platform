package httpx

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Task 2-c (governance): idempotency middleware.
//
// POST requests carrying an `Idempotency-Key` header are executed once: the
// response status + body are stored for 24h under (owner, key, scope) and
// replayed verbatim on retries of the same request, with the additional
// response header `X-Idempotent-Replay: true`.
//
// The owner partition comes from the authenticated principal when available
// (WithIdempotencyOwner, typically the organization_id from auth claims) and
// falls back to a hash of the caller credentials / client IP.
//
// Stores: NewInMemoryIdempotencyStore (zero-infrastructure mode) and
// NewPostgresIdempotencyStore (table idempotency_keys, migration 008). 5xx
// responses are never stored so clients can safely retry server failures.

const (
	// IdempotencyKeyHeader is the request header carrying the client key.
	IdempotencyKeyHeader = "Idempotency-Key"
	// IdempotentReplayHeader marks replayed responses.
	IdempotentReplayHeader = "X-Idempotent-Replay"
	// IdempotencyTTL is how long stored responses are kept.
	IdempotencyTTL = 24 * time.Hour
)

// IdempotencyRecord is one stored request outcome.
type IdempotencyRecord struct {
	Key            string
	OrganizationID string // owner partition (organization or credential hash)
	Scope          string // request path; keeps the same key endpoint-local
	StatusCode     int
	ContentType    string
	Body           []byte
	CreatedAt      time.Time
	ExpiresAt      time.Time
}

// IdempotencyStore persists idempotency records. A miss is (nil, nil);
// storage failures are reported as errors and degrade to pass-through.
type IdempotencyStore interface {
	Get(ctx context.Context, orgID, key, scope string) (*IdempotencyRecord, error)
	Put(ctx context.Context, record *IdempotencyRecord) error
}

type idempotencyOwnerKey struct{}

// WithIdempotencyOwner pins the idempotency owner partition (normally the
// authenticated organization_id) onto the request context. Wrap it around the
// idempotency middleware so the owner is present when the middleware reads it.
func WithIdempotencyOwner(ctx context.Context, owner string) context.Context {
	if strings.TrimSpace(owner) == "" {
		return ctx
	}
	return context.WithValue(ctx, idempotencyOwnerKey{}, strings.TrimSpace(owner))
}

// IdempotencyOwnerFromContext returns the pinned owner, or "" when absent.
func IdempotencyOwnerFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	owner, _ := ctx.Value(idempotencyOwnerKey{}).(string)
	return owner
}

// NewIdempotencyMiddleware returns middleware that makes POSTs with an
// Idempotency-Key header replay-safe using the given store.
func NewIdempotencyMiddleware(store IdempotencyStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if store == nil || r.Method != http.MethodPost {
				next.ServeHTTP(w, r)
				return
			}
			key := strings.TrimSpace(r.Header.Get(IdempotencyKeyHeader))
			if key == "" {
				next.ServeHTTP(w, r)
				return
			}

			owner := IdempotencyOwnerFromContext(r.Context())
			if owner == "" {
				owner = idempotencyCallerOwner(r)
			}
			scope := r.URL.Path

			if cached, err := store.Get(r.Context(), owner, key, scope); err == nil && cached != nil {
				w.Header().Set(IdempotentReplayHeader, "true")
				contentType := cached.ContentType
				if contentType == "" {
					contentType = "application/json; charset=utf-8"
				}
				w.Header().Set("Content-Type", contentType)
				w.WriteHeader(cached.StatusCode)
				_, _ = w.Write(cached.Body)
				return
			} else if err != nil {
				slog.Warn("idempotency store lookup failed; executing request", "error", err.Error(), "key", key)
			}

			recorder := &idempotencyResponseWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(recorder, r)

			// Store client-error and success outcomes for replay; never store
			// server failures so clients can retry them.
			if recorder.status < http.StatusInternalServerError {
				record := &IdempotencyRecord{
					Key:            key,
					OrganizationID: owner,
					Scope:          scope,
					StatusCode:     recorder.status,
					ContentType:    w.Header().Get("Content-Type"),
					Body:           recorder.body(),
					CreatedAt:      time.Now().UTC(),
					ExpiresAt:      time.Now().UTC().Add(IdempotencyTTL),
				}
				if err := store.Put(r.Context(), record); err != nil {
					slog.Warn("idempotency store write failed", "error", err.Error(), "key", key)
				}
			}
		})
	}
}

// idempotencyCallerOwner resolves the owner partition without auth context:
// a hash of the API key or bearer token, else the client IP. Raw credentials
// are never stored or logged.
func idempotencyCallerOwner(r *http.Request) string {
	if owner := IdempotencyOwnerFromContext(r.Context()); owner != "" {
		return owner
	}
	if key := strings.TrimSpace(r.Header.Get("X-API-Key")); key != "" {
		return "key:" + shortHash(key)
	}
	if authz := strings.TrimSpace(r.Header.Get("Authorization")); authz != "" {
		return "token:" + shortHash(authz)
	}
	return "ip:" + clientIP(r)
}

// idempotencyResponseWriter captures status + body while proxying headers.
type idempotencyResponseWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	buf         bytes.Buffer
}

func (w *idempotencyResponseWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *idempotencyResponseWriter) Write(b []byte) (int, error) {
	w.wroteHeader = true
	w.buf.Write(b)
	return w.ResponseWriter.Write(b)
}

func (w *idempotencyResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *idempotencyResponseWriter) body() []byte {
	return append([]byte(nil), w.buf.Bytes()...)
}

// ---------------------------------------------------------------------------
// In-memory store
// ---------------------------------------------------------------------------

type memIdempotencyKey struct {
	org   string
	key   string
	scope string
}

// InMemoryIdempotencyStore is the zero-infrastructure IdempotencyStore.
type InMemoryIdempotencyStore struct {
	records map[memIdempotencyKey]IdempotencyRecord
}

// NewInMemoryIdempotencyStore returns an empty in-memory store.
func NewInMemoryIdempotencyStore() *InMemoryIdempotencyStore {
	return &InMemoryIdempotencyStore{records: make(map[memIdempotencyKey]IdempotencyRecord)}
}

// Get returns the stored record, dropping it when expired.
func (s *InMemoryIdempotencyStore) Get(_ context.Context, orgID, key, scope string) (*IdempotencyRecord, error) {
	if s == nil {
		return nil, nil
	}
	k := memIdempotencyKey{org: orgID, key: key, scope: scope}
	rec, ok := s.records[k]
	if !ok {
		return nil, nil
	}
	if time.Now().UTC().After(rec.ExpiresAt) {
		delete(s.records, k)
		return nil, nil
	}
	body := append([]byte(nil), rec.Body...)
	rec.Body = body
	return &rec, nil
}

// Put stores (or overwrites) the record.
func (s *InMemoryIdempotencyStore) Put(_ context.Context, record *IdempotencyRecord) error {
	if s == nil || record == nil {
		return nil
	}
	k := memIdempotencyKey{org: record.OrganizationID, key: record.Key, scope: record.Scope}
	s.records[k] = *record
	return nil
}

// ---------------------------------------------------------------------------
// Postgres store
// ---------------------------------------------------------------------------

// SQL for the idempotency_keys table created by migrations/008_policies.sql.
const (
	sqlGetIdempotency = `SELECT key, organization_id, scope, status_code, content_type, response_body, created_at, expires_at
                FROM idempotency_keys
                WHERE organization_id = $1 AND key = $2 AND scope = $3 AND expires_at > NOW()`

	sqlPutIdempotency = `INSERT INTO idempotency_keys
                (key, organization_id, scope, status_code, content_type, response_body, created_at, expires_at)
                VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
                ON CONFLICT (organization_id, key, scope) DO UPDATE SET
                        status_code = EXCLUDED.status_code,
                        content_type = EXCLUDED.content_type,
                        response_body = EXCLUDED.response_body,
                        created_at = EXCLUDED.created_at,
                        expires_at = EXCLUDED.expires_at`
)

// PostgresIdempotencyStore is the durable IdempotencyStore backed by the
// idempotency_keys table.
type PostgresIdempotencyStore struct {
	db *sql.DB
}

// NewPostgresIdempotencyStore returns a Store backed by *sql.DB (lib/pq).
func NewPostgresIdempotencyStore(db *sql.DB) *PostgresIdempotencyStore {
	return &PostgresIdempotencyStore{db: db}
}

// Get resolves a stored outcome for one owner partition; expired rows are
// filtered by the expires_at predicate.
func (s *PostgresIdempotencyStore) Get(ctx context.Context, orgID, key, scope string) (*IdempotencyRecord, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	var rec IdempotencyRecord
	var body string
	err := s.db.QueryRowContext(ctx, sqlGetIdempotency, orgID, key, scope).
		Scan(&rec.Key, &rec.OrganizationID, &rec.Scope, &rec.StatusCode, &rec.ContentType, &body, &rec.CreatedAt, &rec.ExpiresAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	rec.Body = []byte(body)
	return &rec, nil
}

// Put upserts the stored outcome (24h TTL applied by the caller).
func (s *PostgresIdempotencyStore) Put(ctx context.Context, record *IdempotencyRecord) error {
	if s == nil || s.db == nil || record == nil {
		return nil
	}
	if record.ExpiresAt.IsZero() {
		record.ExpiresAt = time.Now().UTC().Add(IdempotencyTTL)
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, sqlPutIdempotency,
		record.Key, record.OrganizationID, record.Scope, record.StatusCode,
		record.ContentType, string(record.Body), record.CreatedAt, record.ExpiresAt)
	return err
}

// NewIdempotencyStoreFromDB follows the dual-mode convention: the durable
// Postgres store when db is available, the in-memory store otherwise.
func NewIdempotencyStoreFromDB(db *sql.DB) IdempotencyStore {
	if db == nil {
		return NewInMemoryIdempotencyStore()
	}
	return NewPostgresIdempotencyStore(db)
}

// NewIdempotencyKey is a convenience helper for handlers that mint keys.
func NewIdempotencyKey() string {
	return uuid.NewString()
}

// FormatRetryAfter formats a duration as the integer seconds expected in a
// Retry-After response header (minimum 1).
func FormatRetryAfter(d time.Duration) string {
	seconds := int64(d.Seconds())
	if seconds < 1 {
		seconds = 1
	}
	return strconv.FormatInt(seconds, 10)
}
