package secrets

import (
	"bytes"
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// metadataContainsValue reports whether the serialized metadata projection of
// one secret carries the plaintext value anywhere. It is the belt-and-braces
// companion to the structural check below: even if a value field were added to
// Secret by accident, this helper would catch the leak in every list/get test.
func metadataContainsValue(s *Secret, value string) bool {
	if s == nil || value == "" {
		return false
	}
	b, err := json.Marshal(s)
	if err != nil {
		// A projection that cannot even serialize is treated as leaking.
		return true
	}
	return strings.Contains(string(b), value)
}

// serializeForInspection marshals v to JSON for leak-scanning assertions.
func serializeForInspection(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("serializeForInspection: %v", err)
	}
	return string(b)
}

// captureArg is a sqlmock.Argument that records the []byte driver value it
// matched, so tests can assert on the exact bytes the store hands to the DB
// (e.g. ciphertext must not contain the plaintext).
type captureArg struct {
	captured *[]byte
}

func (c captureArg) Match(v driver.Value) bool {
	b, ok := v.([]byte)
	if !ok {
		return false
	}
	*c.captured = append((*c.captured)[:0], b...)
	return true
}

// ---------------------------------------------------------------------------
// In-memory service behavior
// ---------------------------------------------------------------------------

func TestServiceCreateListGetRoundTrip(t *testing.T) {
	svc := NewService() // in-memory mode: no master key env required
	ctx := context.Background()

	meta, err := svc.Create(ctx, "org-1", "OPENAI_API_KEY", "sk-live-123", "user-1")
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if meta.Name != "OPENAI_API_KEY" || meta.CreatedBy != "user-1" || meta.KeyVersion <= 0 {
		t.Fatalf("unexpected metadata: %+v", meta)
	}

	list, err := svc.List(ctx, "org-1")
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(list) != 1 || list[0].Name != "OPENAI_API_KEY" {
		t.Fatalf("unexpected list: %+v", list)
	}

	got, err := svc.Get(ctx, "org-1", "OPENAI_API_KEY")
	if err != nil || got.Name != "OPENAI_API_KEY" {
		t.Fatalf("Get returned %+v err=%v", got, err)
	}

	value, err := svc.Resolve(ctx, "org-1", "OPENAI_API_KEY")
	if err != nil || value != "sk-live-123" {
		t.Fatalf("Resolve returned %q err=%v", value, err)
	}
}

func TestServiceRejectsInvalidInput(t *testing.T) {
	svc := NewService()
	ctx := context.Background()

	cases := []struct {
		name, org, secret, value, actor string
		want                            error
	}{
		{"missing org", "", "n", "v", "u", ErrOrgRequired},
		{"missing name", "org-1", "", "v", "u", ErrNameRequired},
		{"bad name charset", "org-1", "bad name!", "v", "u", ErrNameInvalid},
		{"name starting with dot", "org-1", ".hidden", "v", "u", ErrNameInvalid},
		{"missing value", "org-1", "n", "", "u", ErrValueRequired},
		{"blank value", "org-1", "n", "   ", "u", ErrValueRequired},
		{"missing actor", "org-1", "n", "v", "", ErrUpdatedByRequired},
		{"value too large", "org-1", "n", strings.Repeat("x", MaxValueBytes+1), "u", ErrValueTooLarge},
	}
	for _, tc := range cases {
		if _, err := svc.Create(ctx, tc.org, tc.secret, tc.value, tc.actor); !errors.Is(err, tc.want) {
			t.Errorf("%s: expected %v, got %v", tc.name, tc.want, err)
		}
	}

	// Boundary: exactly 64 KiB is accepted.
	if _, err := svc.Create(ctx, "org-1", "big", strings.Repeat("x", MaxValueBytes), "u"); err != nil {
		t.Fatalf("64KiB value should be accepted, got %v", err)
	}
}

func TestServiceListNeverReturnsValues(t *testing.T) {
	svc := NewService()
	ctx := context.Background()
	const value = "sk-very-secret-do-not-leak"
	if _, err := svc.Create(ctx, "org-1", "api_key", value, "u"); err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	list, err := svc.List(ctx, "org-1")
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	for _, item := range list {
		if item.Name == "api_key" && metadataContainsValue(item, value) {
			t.Fatal("metadata projection must never carry the value")
		}
	}

	got, err := svc.Get(ctx, "org-1", "api_key")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if metadataContainsValue(got, value) {
		t.Fatal("Get-metadata must never carry the value")
	}

	// Structural check: the metadata struct itself has no value field, so its
	// serialized form can only contain the known metadata keys.
	encoded := serializeForInspection(t, list)
	for _, forbidden := range []string{value, "value", "ciphertext", "plaintext"} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("list serialization leaks %q: %s", forbidden, encoded)
		}
	}
}

func TestServiceDuplicateCreateRejected(t *testing.T) {
	svc := NewService()
	ctx := context.Background()
	if _, err := svc.Create(ctx, "org-1", "n", "v1", "u"); err != nil {
		t.Fatalf("first Create failed: %v", err)
	}
	if _, err := svc.Create(ctx, "org-1", "n", "v2", "u"); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate Create must be ErrDuplicate, got %v", err)
	}
	// Same name in another org is fine (org scoping).
	if _, err := svc.Create(ctx, "org-2", "n", "v2", "u"); err != nil {
		t.Fatalf("cross-org same-name Create failed: %v", err)
	}
}

func TestServiceUpdateRotatesValue(t *testing.T) {
	svc := NewService()
	ctx := context.Background()
	if _, err := svc.Create(ctx, "org-1", "n", "old", "creator"); err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	first, _ := svc.Get(ctx, "org-1", "n")

	// Update replaces the value and bumps updated_at (new nonce => new envelope).
	if _, err := svc.Update(ctx, "org-1", "n", "new", "rotator"); err != nil {
		t.Fatalf("Update failed: %v", err)
	}
	if got, err := svc.Resolve(ctx, "org-1", "n"); err != nil || got != "new" {
		t.Fatalf("Resolve after update: %q err=%v", got, err)
	}
	second, _ := svc.Get(ctx, "org-1", "n")
	if second.CreatedBy != "creator" {
		t.Fatalf("update must preserve created_by, got %q", second.CreatedBy)
	}
	if !second.UpdatedAt.After(first.UpdatedAt) {
		t.Fatalf("update must bump updated_at: %v -> %v", first.UpdatedAt, second.UpdatedAt)
	}

	// Updating an unknown or foreign secret is 404-ish, not a create.
	if _, err := svc.Update(ctx, "org-1", "missing", "v", "u"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown-name Update must be ErrNotFound, got %v", err)
	}
	if _, err := svc.Update(ctx, "org-2", "n", "v", "u"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-org Update must be ErrNotFound, got %v", err)
	}
}

func TestServiceOrgIsolationIsAbsolute(t *testing.T) {
	svc := NewService()
	ctx := context.Background()
	if _, err := svc.Create(ctx, "org-a", "shared_name", "org-a-value", "u"); err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if _, err := svc.Create(ctx, "org-b", "b_only", "org-b-value", "u"); err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	// org-b cannot resolve, read, list, update or delete org-a's secret —
	// foreign resources surface as ErrNotFound (no existence leak).
	if v, err := svc.Resolve(ctx, "org-b", "shared_name"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-org Resolve must be ErrNotFound, got %q err=%v", v, err)
	}
	if _, err := svc.Get(ctx, "org-b", "shared_name"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-org Get must be ErrNotFound, got %v", err)
	}
	if err := svc.Delete(ctx, "org-b", "shared_name"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-org Delete must be ErrNotFound, got %v", err)
	}
	list, err := svc.List(ctx, "org-b")
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(list) != 1 || list[0].Name != "b_only" {
		t.Fatalf("org-b list must only contain its own secrets: %+v", list)
	}
	// And org-a's value is untouched by the attempted cross-org operations.
	if v, err := svc.Resolve(ctx, "org-a", "shared_name"); err != nil || v != "org-a-value" {
		t.Fatalf("org-a secret damaged: %q err=%v", v, err)
	}
}

func TestServiceSoftDeleteBlocksResolveAndAllowsRecreate(t *testing.T) {
	svc := NewService()
	ctx := context.Background()
	if _, err := svc.Create(ctx, "org-1", "gone", "v", "u"); err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if err := svc.Delete(ctx, "org-1", "gone"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	// Tombstone: resolve/get/list/delete all behave as not-found.
	if _, err := svc.Resolve(ctx, "org-1", "gone"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted secret must not resolve, got %v", err)
	}
	if _, err := svc.Get(ctx, "org-1", "gone"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted secret must not Get, got %v", err)
	}
	if err := svc.Delete(ctx, "org-1", "gone"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double delete must be ErrNotFound, got %v", err)
	}
	if list, err := svc.List(ctx, "org-1"); err != nil || len(list) != 0 {
		t.Fatalf("deleted secret must leave the list: %+v err=%v", list, err)
	}
	// Partial-unique semantics: the tombstoned name can be recreated fresh.
	if _, err := svc.Create(ctx, "org-1", "gone", "v2", "u"); err != nil {
		t.Fatalf("recreate after soft delete failed: %v", err)
	}
	if v, err := svc.Resolve(ctx, "org-1", "gone"); err != nil || v != "v2" {
		t.Fatalf("recreated secret must resolve to the new value: %q err=%v", v, err)
	}
}

// ---------------------------------------------------------------------------
// Postgres store (sqlmock): SQL shape + ciphertext-never-plaintext assertions
// ---------------------------------------------------------------------------

func newMockStore(t *testing.T) (Store, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	return NewPostgresStore(db), mock, func() { _ = db.Close() }
}

func TestPostgresStoreCreateInsertsCiphertextNotPlaintext(t *testing.T) {
	store, mock, close := newMockStore(t)
	defer close()
	ctx := context.Background()
	now := time.Now().UTC()

	cipher := mustCipher(t, testKey(1))
	envelope, err := cipher.Seal("sk-plaintext-value")
	if err != nil {
		t.Fatalf("Seal failed: %v", err)
	}
	keyVersion, nonce, ciphertext, err := ParseEnvelope(envelope)
	if err != nil {
		t.Fatalf("ParseEnvelope failed: %v", err)
	}

	// SECURITY ASSERTION: the bytes handed to the DB must not contain (or be)
	// the plaintext.
	if bytes.Contains(ciphertext, []byte("sk-plaintext-value")) || string(ciphertext) == "sk-plaintext-value" {
		t.Fatal("ciphertext must differ from plaintext bytes")
	}
	if bytes.Equal(nonce, bytes.Repeat(nonce, 2)) && len(nonce) == 0 {
		t.Fatal("nonce must be present")
	}

	mock.ExpectExec(`INSERT INTO secrets`).
		WithArgs("org-1", "api_key", ciphertext, nonce, keyVersion, "user-1", now, now).
		WillReturnResult(sqlmock.NewResult(1, 1))

	err = store.Create(ctx, &Record{
		OrgID: "org-1", Name: "api_key", Envelope: envelope, Nonce: nonce,
		Ciphertext: ciphertext, KeyVersion: keyVersion, CreatedBy: "user-1",
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPostgresStoreUpdateScopedAndNotFound(t *testing.T) {
	store, mock, close := newMockStore(t)
	defer close()
	ctx := context.Background()
	now := time.Now().UTC()

	// Tenant guard: WHERE organization_id = $1 AND name = $2 AND deleted_at IS NULL.
	mock.ExpectExec(`UPDATE secrets SET ciphertext = \$3, nonce = \$4, key_version = \$5, updated_at = \$6 WHERE organization_id = \$1 AND name = \$2 AND deleted_at IS NULL`).
		WithArgs("org-1", "api_key", []byte("ct"), []byte("nonce-12-byte"), 1, now).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.Update(ctx, &Record{
		OrgID: "org-1", Name: "api_key", Ciphertext: []byte("ct"),
		Nonce: []byte("nonce-12-byte"), KeyVersion: 1, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("Update returned error: %v", err)
	}

	// Zero rows affected -> ErrNotFound (unknown OR foreign name).
	mock.ExpectExec(`UPDATE secrets`).
		WithArgs("org-1", "api_key", []byte("ct"), []byte("nonce-12-byte"), 1, now).
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := store.Update(ctx, &Record{
		OrgID: "org-1", Name: "api_key", Ciphertext: []byte("ct"),
		Nonce: []byte("nonce-12-byte"), KeyVersion: 1, UpdatedAt: now,
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("zero-rows Update must be ErrNotFound, got %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPostgresStoreSoftDelete(t *testing.T) {
	store, mock, close := newMockStore(t)
	defer close()
	ctx := context.Background()
	now := time.Now().UTC()

	mock.ExpectExec(`UPDATE secrets SET deleted_at = \$3, updated_at = \$3 WHERE organization_id = \$1 AND name = \$2 AND deleted_at IS NULL`).
		WithArgs("org-1", "api_key", now).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.SoftDelete(ctx, "org-1", "api_key", now); err != nil {
		t.Fatalf("SoftDelete returned error: %v", err)
	}

	mock.ExpectExec(`UPDATE secrets`).
		WithArgs("org-1", "api_key", now).
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := store.SoftDelete(ctx, "org-1", "api_key", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("zero-rows SoftDelete must be ErrNotFound, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPostgresStoreListMetaSelectsNoCiphertextColumns(t *testing.T) {
	store, mock, close := newMockStore(t)
	defer close()
	ctx := context.Background()
	now := time.Now().UTC()

	rows := sqlmock.NewRows([]string{"name", "key_version", "created_by", "created_at", "updated_at"}).
		AddRow("api_key", 1, "user-1", now, now)
	// Tenant guard + metadata-only projection (no ciphertext/nonce columns).
	mock.ExpectQuery(`SELECT name, key_version, created_by, created_at, updated_at FROM secrets WHERE organization_id = \$1 AND deleted_at IS NULL ORDER BY name ASC`).
		WithArgs("org-1").
		WillReturnRows(rows)

	recs, err := store.ListMeta(ctx, "org-1")
	if err != nil {
		t.Fatalf("ListMeta returned error: %v", err)
	}
	if len(recs) != 1 || recs[0].Name != "api_key" || recs[0].Ciphertext != nil || recs[0].Nonce != nil {
		t.Fatalf("unexpected metadata rows: %+v", recs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPostgresStoreGetEncryptedReturnsSealedMaterial(t *testing.T) {
	store, mock, close := newMockStore(t)
	defer close()
	ctx := context.Background()
	now := time.Now().UTC()

	cipher := mustCipher(t, testKey(1))
	envelope, err := cipher.Seal("resolve-me")
	if err != nil {
		t.Fatalf("Seal failed: %v", err)
	}
	keyVersion, nonce, ciphertext, err := ParseEnvelope(envelope)
	if err != nil {
		t.Fatalf("ParseEnvelope failed: %v", err)
	}

	rows := sqlmock.NewRows([]string{"name", "key_version", "created_by", "created_at", "updated_at", "ciphertext", "nonce"}).
		AddRow("api_key", keyVersion, "user-1", now, now, ciphertext, nonce)
	mock.ExpectQuery(`SELECT name, key_version, created_by, created_at, updated_at, ciphertext, nonce FROM secrets WHERE organization_id = \$1 AND name = \$2 AND deleted_at IS NULL`).
		WithArgs("org-1", "api_key").
		WillReturnRows(rows)

	rec, err := store.GetEncrypted(ctx, "org-1", "api_key")
	if err != nil {
		t.Fatalf("GetEncrypted returned error: %v", err)
	}
	// The stored sealed material must decrypt back through the service path.
	got, err := cipher.OpenParts(rec.KeyVersion, rec.Nonce, rec.Ciphertext)
	if err != nil || got != "resolve-me" {
		t.Fatalf("OpenParts of stored row: %q err=%v", got, err)
	}

	// Unknown/foreign -> ErrNotFound.
	mock.ExpectQuery(`SELECT name, key_version, created_by, created_at, updated_at, ciphertext, nonce`).
		WithArgs("org-1", "missing").
		WillReturnRows(sqlmock.NewRows([]string{"name", "key_version", "created_by", "created_at", "updated_at", "ciphertext", "nonce"}))
	if _, err := store.GetEncrypted(ctx, "org-1", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown row must be ErrNotFound, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Service over the Postgres store (fail-fast + end-to-end Resolve through SQL)
// ---------------------------------------------------------------------------

func TestNewServiceWithStoreFailFastWithoutCipher(t *testing.T) {
	store, _, close := newMockStore(t)
	defer close()
	if _, err := NewServiceWithStore(store, nil); !errors.Is(err, ErrMasterKeyRequired) {
		t.Fatalf("Postgres mode without cipher must fail fast, got %v", err)
	}
	if _, err := NewServiceWithStore(nil, mustCipher(t, testKey(1))); err == nil {
		t.Fatal("nil store must be rejected")
	}
}

func TestServiceOverStoreCreateAndResolveViaSQL(t *testing.T) {
	store, mock, close := newMockStore(t)
	defer close()
	ctx := context.Background()

	svc, err := NewServiceWithStore(store, mustCipher(t, testKey(1)))
	if err != nil {
		t.Fatalf("NewServiceWithStore failed: %v", err)
	}

	// Capture the INSERT args to prove the service sealed before persisting.
	var sealedCT []byte
	var sealedNonce []byte
	mock.ExpectExec(`INSERT INTO secrets`).
		WithArgs("org-1", "api_key", captureArg{&sealedCT}, captureArg{&sealedNonce},
			sqlmock.AnyArg(), "user-1", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	if _, err := svc.Create(ctx, "org-1", "api_key", "sk-live-42", "user-1"); err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	// SECURITY ASSERTION: the bytes handed to the DB must not contain (or
	// be) the plaintext, and a per-secret random nonce must be present.
	if sealedCT == nil || sealedNonce == nil {
		t.Fatal("INSERT must bind ciphertext and nonce arguments")
	}
	if bytes.Contains(sealedCT, []byte("sk-live-42")) || string(sealedCT) == "sk-live-42" {
		t.Fatal("service must seal before persisting: ciphertext carries plaintext")
	}
	if len(sealedNonce) != NonceLength {
		t.Fatalf("unexpected nonce length %d", len(sealedNonce))
	}

	// Resolve path: GetEncrypted returns the sealed row; OpenParts restores it.
	cipher := mustCipher(t, testKey(1))
	envelope, err := cipher.Seal("sk-live-42")
	if err != nil {
		t.Fatalf("Seal failed: %v", err)
	}
	keyVersion, nonce, ciphertext, err := ParseEnvelope(envelope)
	if err != nil {
		t.Fatalf("ParseEnvelope failed: %v", err)
	}
	now := time.Now().UTC()
	mock.ExpectQuery(`SELECT name, key_version, created_by, created_at, updated_at, ciphertext, nonce FROM secrets WHERE organization_id = \$1 AND name = \$2 AND deleted_at IS NULL`).
		WithArgs("org-1", "api_key").
		WillReturnRows(sqlmock.NewRows([]string{"name", "key_version", "created_by", "created_at", "updated_at", "ciphertext", "nonce"}).
			AddRow("api_key", keyVersion, "user-1", now, now, ciphertext, nonce))
	got, err := svc.Resolve(ctx, "org-1", "api_key")
	if err != nil || got != "sk-live-42" {
		t.Fatalf("Resolve through store: %q err=%v", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestServiceOverStoreListMeta(t *testing.T) {
	store, mock, close := newMockStore(t)
	defer close()
	ctx := context.Background()
	svc, err := NewServiceWithStore(store, mustCipher(t, testKey(1)))
	if err != nil {
		t.Fatalf("NewServiceWithStore failed: %v", err)
	}
	now := time.Now().UTC()
	mock.ExpectQuery(`SELECT name, key_version, created_by, created_at, updated_at FROM secrets WHERE organization_id = \$1 AND deleted_at IS NULL ORDER BY name ASC`).
		WithArgs("org-1").
		WillReturnRows(sqlmock.NewRows([]string{"name", "key_version", "created_by", "created_at", "updated_at"}).
			AddRow("api_key", 1, "user-1", now, now))
	list, err := svc.List(ctx, "org-1")
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(list) != 1 || list[0].Name != "api_key" {
		t.Fatalf("unexpected list: %+v", list)
	}
	if metadataContainsValue(list[0], "anything") {
		t.Fatal("metadata must be value-free")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}
