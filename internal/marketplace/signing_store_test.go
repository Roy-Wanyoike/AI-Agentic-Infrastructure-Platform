package marketplace

// signing_store_test.go — Postgres-mode pins for the provenance subsystem
// (issue #78, migration 022): the org-scoped key SQL (no existence leak), the
// idempotent revoke, the (organization_id, public_key) uniqueness mapping
// (23505 -> ErrDuplicateKey), the settings upsert and a full SIGNED
// publish -> install flow over the mocked pg store with a REAL agents domain.

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"

	"agentos/internal/agents"
)

func sampleSigningKey(id, org, status string) *SigningKey {
	key := &SigningKey{
		ID:             id,
		OrganizationID: org,
		PublicKey:      "dGVzdC1wdWJsaWMta2V5LW1hdGVyaWFsMTIzNA==", // base64; shape pinned by DecodePublicKey tests
		Alg:            SignatureAlgEd25519,
		Status:         status,
		CreatedAt:      tsA,
	}
	if status == KeyStatusRevoked {
		t := tsA.Add(time.Hour)
		key.RevokedAt = &t
	}
	return key
}

func signingKeyRows(keys ...*SigningKey) *sqlmock.Rows {
	rows := sqlmock.NewRows([]string{
		"id", "organization_id", "public_key", "alg", "status", "created_at", "revoked_at",
	})
	for _, k := range keys {
		rows.AddRow(k.ID, k.OrganizationID, k.PublicKey, k.Alg, k.Status, k.CreatedAt, k.RevokedAt)
	}
	return rows
}

func TestPostgresCreateSigningKey(t *testing.T) {
	mock, rawStore := newMockStore(t)
	ks, ok := rawStore.(SigningKeyStore)
	if !ok {
		t.Fatal("pgStore must implement SigningKeyStore")
	}
	ctx := context.Background()
	key := sampleSigningKey("k-1", "org-a", KeyStatusActive)

	mock.ExpectExec(regexp.QuoteMeta(sqlInsertSigningKey)).
		WithArgs("k-1", "org-a", key.PublicKey, SignatureAlgEd25519, KeyStatusActive, tsA, (*time.Time)(nil)).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := ks.CreateSigningKey(ctx, key); err != nil {
		t.Fatalf("CreateSigningKey: %v", err)
	}

	// Duplicate (organization_id, public_key): 23505 -> ErrDuplicateKey.
	mock.ExpectExec(regexp.QuoteMeta(sqlInsertSigningKey)).
		WillReturnError(&pq.Error{Code: "23505", Message: `duplicate key value violates unique constraint "marketplace_signing_keys_org_pub_key"`})
	if err := ks.CreateSigningKey(ctx, key); !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("duplicate key: got %v, want ErrDuplicateKey", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPostgresGetSigningKey(t *testing.T) {
	mock, rawStore := newMockStore(t)
	ks, ok := rawStore.(SigningKeyStore)
	if !ok {
		t.Fatal("pgStore must implement SigningKeyStore")
	}
	ctx := context.Background()

	// Org-scoped point lookup.
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectSigningKey)).
		WithArgs("k-1", "org-a").
		WillReturnRows(signingKeyRows(sampleSigningKey("k-1", "org-a", KeyStatusActive)))
	got, err := ks.GetSigningKey(ctx, "org-a", "k-1")
	if err != nil {
		t.Fatalf("GetSigningKey: %v", err)
	}
	if got.ID != "k-1" || got.OrganizationID != "org-a" || got.Status != KeyStatusActive {
		t.Fatalf("unexpected key: %+v", got)
	}

	// Unknown id OR foreign org -> ErrKeyNotFound (no existence leak).
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectSigningKey)).
		WithArgs("k-1", "org-zz").
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	if _, err := ks.GetSigningKey(ctx, "org-zz", "k-1"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("foreign key lookup: got %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPostgresListSigningKeys(t *testing.T) {
	mock, rawStore := newMockStore(t)
	ks, ok := rawStore.(SigningKeyStore)
	if !ok {
		t.Fatal("pgStore must implement SigningKeyStore")
	}
	ctx := context.Background()

	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectSigningKeysByOrg)).
		WithArgs("org-a").
		WillReturnRows(signingKeyRows(
			sampleSigningKey("k-1", "org-a", KeyStatusActive),
			sampleSigningKey("k-2", "org-a", KeyStatusRevoked),
		))
	keys, err := ks.ListSigningKeys(ctx, "org-a")
	if err != nil {
		t.Fatalf("ListSigningKeys: %v", err)
	}
	if len(keys) != 2 || keys[0].ID != "k-1" || keys[1].ID != "k-2" {
		t.Fatalf("unexpected key list: %+v", keys)
	}
	if keys[1].RevokedAt == nil || !keys[1].RevokedAt.Equal(tsA.Add(time.Hour)) {
		t.Fatalf("revoked_at must round-trip: %+v", keys[1])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPostgresRevokeSigningKey(t *testing.T) {
	mock, rawStore := newMockStore(t)
	ks, ok := rawStore.(SigningKeyStore)
	if !ok {
		t.Fatal("pgStore must implement SigningKeyStore")
	}
	ctx := context.Background()

	mock.ExpectExec(regexp.QuoteMeta(sqlRevokeSigningKey)).
		WithArgs("k-1", "org-a", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := ks.RevokeSigningKey(ctx, "org-a", "k-1"); err != nil {
		t.Fatalf("RevokeSigningKey: %v", err)
	}

	// Unknown OR foreign id: 0 rows -> ErrKeyNotFound.
	mock.ExpectExec(regexp.QuoteMeta(sqlRevokeSigningKey)).
		WithArgs("k-x", "org-a", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := ks.RevokeSigningKey(ctx, "org-a", "k-x"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("revoke unknown key: got %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPostgresOrgSettings(t *testing.T) {
	mock, rawStore := newMockStore(t)
	ks, ok := rawStore.(SigningKeyStore)
	if !ok {
		t.Fatal("pgStore must implement SigningKeyStore")
	}
	ctx := context.Background()

	// Missing row IS the default policy (allow_unsigned=false), never an error.
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectOrgSettings)).
		WithArgs("org-b").
		WillReturnRows(sqlmock.NewRows([]string{"allow_unsigned", "updated_at"}))
	settings, err := ks.GetOrgSettings(ctx, "org-b")
	if err != nil {
		t.Fatalf("GetOrgSettings(missing): %v", err)
	}
	if settings.AllowUnsigned || !settings.UpdatedAt.IsZero() {
		t.Fatalf("missing settings row must read as defaults: %+v", settings)
	}

	// Upsert returns the stored row with the server-stamped updated_at.
	mock.ExpectQuery(regexp.QuoteMeta(sqlUpsertOrgSettings)).
		WithArgs("org-b", true, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"allow_unsigned", "updated_at"}).AddRow(true, tsA))
	settings, err = ks.UpsertOrgSettings(ctx, "org-b", true)
	if err != nil {
		t.Fatalf("UpsertOrgSettings: %v", err)
	}
	if !settings.AllowUnsigned || !settings.UpdatedAt.Equal(tsA) {
		t.Fatalf("unexpected settings: %+v", settings)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

// TestServiceOverPostgresStoreSignedInstall drives the full signed flow
// through the pg store: publish pins the provenance INSERT columns, install
// re-verifies the signature against the org-scoped key row and the manifest
// hash recorded at publish time.
func TestServiceOverPostgresStoreSignedInstall(t *testing.T) {
	mock, rawStore := newMockStore(t)
	if _, ok := rawStore.(SigningKeyStore); !ok {
		t.Fatal("pgStore must implement SigningKeyStore")
	}
	agentsSvc := agents.NewService()
	ctx := context.Background()

	source, err := agentsSvc.CreateAgentCtx(ctx, "org-a", "Support Bot", "Helps customers", "Be polite", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("CreateAgentCtx: %v", err)
	}
	svc := NewServiceWithStore(rawStore, agentsSvc, nil)

	// Register the publisher key (org-scoped INSERT) and sign the canonical
	// manifest of the live-config snapshot. The service mints its own key id
	// (UUID) — subsequent pins use the returned key's id.
	pub, priv := genTestKey(t)
	key := sampleSigningKey("k-seed", "org-a", KeyStatusActive)
	key.PublicKey = base64.StdEncoding.EncodeToString(pub)
	mock.ExpectExec(regexp.QuoteMeta(sqlInsertSigningKey)).
		WithArgs(sqlmock.AnyArg(), "org-a", key.PublicKey, SignatureAlgEd25519, KeyStatusActive, sqlmock.AnyArg(), (*time.Time)(nil)).
		WillReturnResult(sqlmock.NewResult(1, 1))
	registered, err := svc.RegisterKey(ctx, "org-a", RegisterKeyInput{PublicKey: key.PublicKey})
	if err != nil {
		t.Fatalf("RegisterKey: %v", err)
	}
	keyID := registered.ID
	key.ID = keyID

	snap := Snapshot{Name: "Support Bot", Description: "Helps customers", Instructions: "Be polite", Model: "gpt-4o-mini", Status: "DRAFT"}
	canonical := mustCanonical(t, snap)
	sigBytes := ed25519.Sign(priv, canonical)
	if sigBytes == nil {
		t.Fatal("ed25519.Sign returned nil")
	}
	sig := base64.StdEncoding.EncodeToString(sigBytes)

	// Signed publish: slug pre-check (no rows) + INSERT carrying the
	// provenance columns (signature, key id, alg, signed_at, hash, legacy=0).
	listing := sampleListing("l-sig", "support-bot", StatusPublished)
	listing.SourceAgentID = source.ID
	// Publish order: applyPublishSignature verifies the signature against
	// the registered key FIRST (activeKey -> GetSigningKey), then the
	// global slug pre-check, then the listing INSERT.
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectSigningKey)).
		WithArgs(keyID, "org-a").
		WillReturnRows(signingKeyRows(key))
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectListingBySlug)).
		WithArgs("support-bot").
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectExec(regexp.QuoteMeta(sqlInsertListing)).
		WithArgs(sqlmock.AnyArg(), "org-a", "user-a", source.ID, sqlmock.AnyArg(), // live-config snapshot JSON
			"Support Bot", "support-bot", "Helps customers",
			sqlmock.AnyArg(), // normalized tags (empty -> {} in pg JSONB form)
			StatusPublished, 0,
			sqlmock.AnyArg(), sqlmock.AnyArg(),
			sig, keyID, SignatureAlgEd25519, sqlmock.AnyArg(),
			ManifestHash(canonical), false).
		WillReturnResult(sqlmock.NewResult(1, 1))
	published, err := svc.Publish(ctx, "org-a", "user-a", PublishInput{
		AgentID:      source.ID,
		Name:         "Support Bot",
		Signature:    sig,
		SigningKeyID: keyID,
	})
	if err != nil {
		t.Fatalf("Publish(signed): %v", err)
	}
	if published.ManifestHash != ManifestHash(canonical) || published.SigningKeyID != keyID {
		t.Fatalf("provenance not persisted: %+v", published)
	}

	// Install into org-b: point lookup + key lookup (active) + verify.
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectListingBySlug)).
		WithArgs("support-bot").
		WillReturnRows(listingRows(published))
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectSigningKey)).
		WithArgs(keyID, "org-a").
		WillReturnRows(signingKeyRows(key))
	mock.ExpectQuery(regexp.QuoteMeta(sqlIncrementDownloadCount)).
		WithArgs(published.ID).
		WillReturnRows(sqlmock.NewRows([]string{"download_count"}).AddRow(1))
	result, err := svc.Install(ctx, "org-b", "support-bot")
	if err != nil {
		t.Fatalf("Install(signed, pg mode): %v", err)
	}
	if result.Agent.Instructions != "Be polite" {
		t.Fatalf("unexpected installed agent: %+v", result.Agent)
	}

	// Revoked key: the SAME install is blocked (unknown_signing_key).
	revoked := sampleSigningKey(keyID, "org-a", KeyStatusRevoked)
	revoked.PublicKey = key.PublicKey
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectListingBySlug)).
		WithArgs("support-bot").
		WillReturnRows(listingRows(published))
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectSigningKey)).
		WithArgs(keyID, "org-a").
		WillReturnRows(signingKeyRows(revoked))
	if _, err := svc.Install(ctx, "org-b", "support-bot"); !errors.Is(err, ErrUnknownSigningKey) {
		t.Fatalf("install with revoked key: got %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}
