package marketplace

// signing_service_test.go — issue-#78 service scenarios on the in-memory
// mode: the sign -> publish -> install round trip, the four distinct install
// block errors (unsigned_listing / unknown_signing_key / signature_invalid /
// snapshot_tampered), the allow_unsigned org policy with legacy
// grandfathering, revocation semantics, key org-scoping and the provenance
// read model. Postgres-mode pins live in signing_store_test.go.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

// genTestKey returns a fresh Ed25519 keypair.
func genTestKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	return pub, priv
}

// mustRegisterKey registers a public key for org and returns its id.
func mustRegisterKey(t *testing.T, svc *Service, org string, pub ed25519.PublicKey) string {
	t.Helper()
	key, err := svc.RegisterKey(context.Background(), org, RegisterKeyInput{
		PublicKey: base64.StdEncoding.EncodeToString(pub),
	})
	if err != nil {
		t.Fatalf("RegisterKey(%s): %v", org, err)
	}
	return key.ID
}

// signSnapshot signs the canonical manifest of snap (the exact bytes the
// server will re-derive from the stored snapshot).
func signSnapshot(t *testing.T, priv ed25519.PrivateKey, snap Snapshot) string {
	t.Helper()
	canonical, err := manifestBytes(snap)
	if err != nil {
		t.Fatalf("manifestBytes: %v", err)
	}
	return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, canonical))
}

// publishSigned publishes org-a's source agent with a detached signature.
func publishSigned(t *testing.T, svc *Service, priv ed25519.PrivateKey, keyID, agentID, slug string) *Listing {
	t.Helper()
	listing, err := svc.Publish(context.Background(), "org-a", "user-a", PublishInput{
		AgentID:      agentID,
		Name:         "Support Bot",
		Slug:         slug,
		Signature:    signSnapshot(t, priv, Snapshot{Name: "Support Bot", Description: "Helps customers", Instructions: "Be polite", Model: "gpt-4o-mini", Status: "DRAFT"}),
		SigningKeyID: keyID,
	})
	if err != nil {
		t.Fatalf("Publish(signed): %v", err)
	}
	return listing
}

// seedSourceAgent creates org-a's source agent with the canonical fixture
// config used across these tests and returns its (generated) id.
func seedSourceAgent(t *testing.T, svc *Service) string {
	t.Helper()
	agent, err := svc.agents.CreateAgentCtx(context.Background(), "org-a", "Support Bot", "Helps customers", "Be polite", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("CreateAgentCtx: %v", err)
	}
	return agent.ID
}

// newSigningFixture returns a service with org-a's source agent seeded; the
// generated agent id is threaded through the harness so publishes target it.
func newSigningFixture(t *testing.T) (*Service, string) {
	t.Helper()
	svc, _, _ := newFixture(t)
	return svc, seedSourceAgent(t, svc)
}

func TestSignPublishInstallRoundTrip(t *testing.T) {
	svc, agentID := newSigningFixture(t)
	ctx := context.Background()
	pub, priv := genTestKey(t)
	keyID := mustRegisterKey(t, svc, "org-a", pub)

	listing := publishSigned(t, svc, priv, keyID, agentID, "support-bot")
	if listing.Signature == "" || listing.ManifestHash == "" || listing.SigAlg != SignatureAlgEd25519 {
		t.Fatalf("signed publish must persist provenance: %+v", listing)
	}
	if listing.SigningKeyID != keyID {
		t.Fatalf("listing key id = %q, want %q", listing.SigningKeyID, keyID)
	}
	if listing.SignedAt == nil || listing.LegacyListing {
		t.Fatalf("signed publish must stamp signed_at and legacy=false: %+v", listing)
	}
	wantHash := ManifestHash(mustCanonical(t, Snapshot{Name: "Support Bot", Description: "Helps customers", Instructions: "Be polite", Model: "gpt-4o-mini", Status: "DRAFT"}))
	if listing.ManifestHash != wantHash {
		t.Fatalf("manifest hash = %s, want %s", listing.ManifestHash, wantHash)
	}

	// Install into org-b (default policy: verify) — the signature verifies.
	result, err := svc.Install(ctx, "org-b", "support-bot")
	if err != nil {
		t.Fatalf("Install(signed): %v", err)
	}
	if result.Agent.OrganizationID != "org-b" || result.Agent.Instructions != "Be polite" {
		t.Fatalf("unexpected installed agent: %+v", result.Agent)
	}

	// Provenance document after install.
	prov, err := svc.ProvenanceFor(ctx, "org-b", "support-bot")
	if err != nil {
		t.Fatalf("ProvenanceFor: %v", err)
	}
	if !prov.Signed || prov.PublisherOrgID != "org-a" || prov.SigningKeyID != keyID || prov.ManifestHash != wantHash {
		t.Fatalf("unexpected provenance: %+v", prov)
	}
	if prov.Fingerprint == "" || len(prov.Fingerprint) != 64 {
		t.Fatalf("provenance fingerprint missing: %+v", prov)
	}
	if prov.SignedAt == nil || !prov.SignedAt.Equal(*listing.SignedAt) {
		t.Fatalf("provenance signed_at must match the listing: %+v", prov)
	}
}

func mustCanonical(t *testing.T, snap Snapshot) []byte {
	t.Helper()
	canonical, err := manifestBytes(snap)
	if err != nil {
		t.Fatalf("manifestBytes: %v", err)
	}
	return canonical
}

func TestPublishSigningContract(t *testing.T) {
	svc, agentID := newSigningFixture(t)
	ctx := context.Background()
	pub, priv := genTestKey(t)
	keyID := mustRegisterKey(t, svc, "org-a", pub)

	// Exactly one of signature / key id -> incomplete.
	if _, err := svc.Publish(ctx, "org-a", "user-a", PublishInput{AgentID: agentID, Name: "n", Slug: "half-1", Signature: "AAA"}); !errors.Is(err, ErrSignatureIncomplete) {
		t.Fatalf("signature without key id: got %v", err)
	}
	if _, err := svc.Publish(ctx, "org-a", "user-a", PublishInput{AgentID: agentID, Name: "n", Slug: "half-2", SigningKeyID: keyID}); !errors.Is(err, ErrSignatureIncomplete) {
		t.Fatalf("key id without signature: got %v", err)
	}

	// Unregistered key id -> unknown_signing_key (no catalog entry created).
	if _, err := svc.Publish(ctx, "org-a", "user-a", PublishInput{AgentID: agentID, Name: "n", Slug: "ghost", Signature: "AAA", SigningKeyID: "key-ghost"}); !errors.Is(err, ErrUnknownSigningKey) {
		t.Fatalf("unregistered key: got %v", err)
	}

	// Signature over a DIFFERENT manifest -> signature_invalid.
	if _, err := svc.Publish(ctx, "org-a", "user-a", PublishInput{
		AgentID:      agentID,
		Name:         "Support Bot",
		Slug:         "wrong-sig",
		Signature:    signSnapshot(t, priv, Snapshot{Name: "Other", Description: "d", Instructions: "i", Model: "m", Status: "DRAFT"}),
		SigningKeyID: keyID,
	}); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("signature over another manifest: got %v", err)
	}

	// A foreign org's key cannot sign org-a's listing.
	otherPub, _ := genTestKey(t)
	foreignID := mustRegisterKey(t, svc, "org-b", otherPub)
	if _, err := svc.Publish(ctx, "org-a", "user-a", PublishInput{
		AgentID:      agentID,
		Name:         "Support Bot",
		Slug:         "foreign-key",
		Signature:    signSnapshot(t, priv, Snapshot{Name: "Support Bot", Description: "Helps customers", Instructions: "Be polite", Model: "gpt-4o-mini", Status: "DRAFT"}),
		SigningKeyID: foreignID,
	}); !errors.Is(err, ErrUnknownSigningKey) {
		t.Fatalf("foreign-org key at publish: got %v", err)
	}

	// None of the failed publishes may have leaked into the catalog.
	if _, err := svc.GetBySlug(ctx, "org-a", "ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed publish must not store a listing: got %v", err)
	}
}

func TestInstallTamperedSnapshotBlocked(t *testing.T) {
	svc, agentID := newSigningFixture(t)
	ctx := context.Background()
	pub, priv := genTestKey(t)
	keyID := mustRegisterKey(t, svc, "org-a", pub)
	listing := publishSigned(t, svc, priv, keyID, agentID, "support-bot")

	// Post-publish mutation of the stored snapshot (any drift from the
	// content address recorded at publish time).
	svc.mu.Lock()
	listing.VersionSnapshot = `{"name":"Support Bot","description":"Helps customers","instructions":"BE EVIL","model":"gpt-4o-mini","status":"DRAFT"}`
	svc.mu.Unlock()
	if _, err := svc.Install(ctx, "org-b", "support-bot"); !errors.Is(err, ErrSnapshotTampered) {
		t.Fatalf("tampered snapshot: got %v, want ErrSnapshotTampered", err)
	}

	// allow_unsigned must NOT bypass tamper evidence.
	if _, err := svc.SetAllowUnsigned(ctx, "org-b", true); err != nil {
		t.Fatalf("SetAllowUnsigned: %v", err)
	}
	if _, err := svc.Install(ctx, "org-b", "support-bot"); !errors.Is(err, ErrSnapshotTampered) {
		t.Fatalf("tampered snapshot with allow_unsigned: got %v", err)
	}
}

func TestInstallUnsignedPolicy(t *testing.T) {
	svc, agentID := newSigningFixture(t)
	ctx := context.Background()

	// Unsigned publish (legacy behavior, still allowed).
	listing, err := svc.Publish(ctx, "org-a", "user-a", PublishInput{AgentID: agentID, Name: "Support Bot", Slug: "support-bot"})
	if err != nil {
		t.Fatalf("Publish(unsigned): %v", err)
	}
	if listing.Signature != "" || listing.ManifestHash != "" || listing.LegacyListing {
		t.Fatalf("unsigned publish must leave provenance empty: %+v", listing)
	}

	// Default policy: verify -> unsigned_listing.
	if _, err := svc.Install(ctx, "org-b", "support-bot"); !errors.Is(err, ErrUnsignedListing) {
		t.Fatalf("unsigned install under default policy: got %v", err)
	}
	// The policy belongs to the INSTALLING org: org-a's own choice is irrelevant.
	if _, err := svc.SetAllowUnsigned(ctx, "org-a", true); err != nil {
		t.Fatalf("SetAllowUnsigned(org-a): %v", err)
	}
	if _, err := svc.Install(ctx, "org-b", "support-bot"); !errors.Is(err, ErrUnsignedListing) {
		t.Fatalf("installer org policy is the decisive one: got %v", err)
	}

	// Explicit org opt-in unlocks the install.
	if _, err := svc.SetAllowUnsigned(ctx, "org-b", true); err != nil {
		t.Fatalf("SetAllowUnsigned(org-b): %v", err)
	}
	if _, err := svc.Install(ctx, "org-b", "support-bot"); err != nil {
		t.Fatalf("Install after allow_unsigned: %v", err)
	}
}

func TestLegacyListingGrandfathered(t *testing.T) {
	svc, _ := newSigningFixture(t)
	ctx := context.Background()

	// A pre-signing row (migration 022 marks every existing listing legacy):
	// unsigned, no manifest hash, installable regardless of the policy.
	svc.mu.Lock()
	svc.items["l-legacy"] = &Listing{
		ID: "l-legacy", PublisherOrgID: "org-a", PublisherUserID: "user-a",
		SourceAgentID:   "agent-a",
		VersionSnapshot: `{"name":"Support Bot","description":"Helps customers","instructions":"Be polite","model":"gpt-4o-mini","status":"DRAFT"}`,
		Name:            "Support Bot", Slug: "legacy-bot", Status: StatusPublished,
		LegacyListing: true,
	}
	svc.mu.Unlock()

	if _, err := svc.Install(ctx, "org-b", "legacy-bot"); err != nil {
		t.Fatalf("legacy listing must stay installable (grandfathering): %v", err)
	}

	// Provenance of a legacy listing: unsigned, empty hash (documented shape).
	prov, err := svc.ProvenanceFor(ctx, "org-b", "legacy-bot")
	if err != nil {
		t.Fatalf("ProvenanceFor(legacy): %v", err)
	}
	if prov.Signed || prov.ManifestHash != "" || prov.SigningKeyID != "" {
		t.Fatalf("legacy provenance must be empty-signed: %+v", prov)
	}
}

func TestRevocationBlocksFutureInstallsNotExistingAgents(t *testing.T) {
	svc, agentID := newSigningFixture(t)
	ctx := context.Background()
	pub, priv := genTestKey(t)
	keyID := mustRegisterKey(t, svc, "org-a", pub)
	publishSigned(t, svc, priv, keyID, agentID, "support-bot")

	// First install (key still active) succeeds.
	first, err := svc.Install(ctx, "org-b", "support-bot")
	if err != nil {
		t.Fatalf("Install(before revoke): %v", err)
	}

	// Revoke: future installs of listings signed with the key are blocked...
	if err := svc.RevokeKey(ctx, "org-a", keyID); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}
	if _, err := svc.Install(ctx, "org-b", "support-bot"); !errors.Is(err, ErrUnknownSigningKey) {
		t.Fatalf("install after revocation: got %v, want ErrUnknownSigningKey", err)
	}

	// ...but agents already installed stay untouched.
	existing, err := svc.agents.ListAgentsCtx(ctx, "org-b")
	if err != nil {
		t.Fatalf("ListAgentsCtx: %v", err)
	}
	if len(existing) != 1 || existing[0].ID != first.Agent.ID {
		t.Fatalf("revocation must not touch installed agents: %+v", existing)
	}

	// A revoked key cannot sign NEW listings either.
	if _, err := svc.Publish(ctx, "org-a", "user-a", PublishInput{
		AgentID:      agentID,
		Name:         "Support Bot",
		Slug:         "after-revoke",
		Signature:    signSnapshot(t, priv, Snapshot{Name: "Support Bot", Description: "Helps customers", Instructions: "Be polite", Model: "gpt-4o-mini", Status: "DRAFT"}),
		SigningKeyID: keyID,
	}); !errors.Is(err, ErrUnknownSigningKey) {
		t.Fatalf("publish with revoked key: got %v", err)
	}

	// Revocation is idempotent; foreign orgs and unknown ids are ErrKeyNotFound.
	if err := svc.RevokeKey(ctx, "org-a", keyID); err != nil {
		t.Fatalf("re-revoke must be idempotent: %v", err)
	}
	if err := svc.RevokeKey(ctx, "org-b", keyID); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("foreign revoke: got %v", err)
	}
	if err := svc.RevokeKey(ctx, "org-a", "key-ghost"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("unknown revoke: got %v", err)
	}
}

func TestKeyOrgScopingAndListing(t *testing.T) {
	svc, _ := newSigningFixture(t)
	ctx := context.Background()
	pubA, _ := genTestKey(t)
	pubB, _ := genTestKey(t)
	idA := mustRegisterKey(t, svc, "org-a", pubA)
	idB := mustRegisterKey(t, svc, "org-b", pubB)

	keysA, err := svc.ListKeys(ctx, "org-a")
	if err != nil {
		t.Fatalf("ListKeys(org-a): %v", err)
	}
	if len(keysA) != 1 || keysA[0].ID != idA || keysA[0].OrganizationID != "org-a" {
		t.Fatalf("org-a must see only its own key: %+v", keysA)
	}
	keysB, err := svc.ListKeys(ctx, "org-b")
	if err != nil {
		t.Fatalf("ListKeys(org-b): %v", err)
	}
	if len(keysB) != 1 || keysB[0].ID != idB {
		t.Fatalf("org-b must see only its own key: %+v", keysB)
	}

	// Duplicate registration of the same public key in the same org.
	if _, err := svc.RegisterKey(ctx, "org-a", RegisterKeyInput{PublicKey: base64.StdEncoding.EncodeToString(pubA)}); !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("duplicate key: got %v", err)
	}
	// The same key material under ANOTHER org is a distinct row (org-scoped).
	if _, err := svc.RegisterKey(ctx, "org-c", RegisterKeyInput{PublicKey: base64.StdEncoding.EncodeToString(pubA)}); err != nil {
		t.Fatalf("same key in another org must register: %v", err)
	}

	// Org is required.
	if _, err := svc.RegisterKey(ctx, "  ", RegisterKeyInput{PublicKey: base64.StdEncoding.EncodeToString(pubA)}); !errors.Is(err, ErrOrgRequired) {
		t.Fatalf("missing org: got %v", err)
	}
	if err := svc.RevokeKey(ctx, "  ", idA); !errors.Is(err, ErrOrgRequired) {
		t.Fatalf("missing org (revoke): got %v", err)
	}
}

func TestProvenanceVisibilityAndUnsigned(t *testing.T) {
	svc, agentID := newSigningFixture(t)
	ctx := context.Background()

	// Unsigned NEW listing: provenance answers with signed=false.
	if _, err := svc.Publish(ctx, "org-a", "user-a", PublishInput{AgentID: agentID, Name: "Support Bot", Slug: "support-bot"}); err != nil {
		t.Fatalf("Publish(unsigned): %v", err)
	}
	prov, err := svc.ProvenanceFor(ctx, "org-b", "support-bot")
	if err != nil {
		t.Fatalf("ProvenanceFor(unsigned): %v", err)
	}
	if prov.Signed || prov.SigningKeyID != "" || prov.Alg != "" || prov.SignedAt != nil || prov.ManifestHash != "" {
		t.Fatalf("unsigned provenance must be empty: %+v", prov)
	}
	if prov.PublisherOrgID != "org-a" || prov.Slug != "support-bot" {
		t.Fatalf("unsigned provenance still names the publisher: %+v", prov)
	}

	// Draft listings are publisher-private: foreign callers get ErrNotFound.
	svc.mu.Lock()
	svc.items["l-draft"] = &Listing{
		ID: "l-draft", PublisherOrgID: "org-a", Slug: "draft-bot", Status: StatusDraft,
		VersionSnapshot: `{"name":"d","instructions":"i","model":"m"}`,
	}
	svc.mu.Unlock()
	if _, err := svc.ProvenanceFor(ctx, "org-b", "draft-bot"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("draft provenance cross-tenant: got %v", err)
	}
	// The publisher org sees its own draft.
	if _, err := svc.ProvenanceFor(ctx, "org-a", "draft-bot"); err != nil {
		t.Fatalf("draft provenance (publisher): %v", err)
	}
	// Unknown slugs are ErrNotFound everywhere.
	if _, err := svc.ProvenanceFor(ctx, "org-a", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown slug provenance: got %v", err)
	}
}

func TestRevokedKeyFingerprintRemainsResolvable(t *testing.T) {
	svc, agentID := newSigningFixture(t)
	ctx := context.Background()
	pub, priv := genTestKey(t)
	keyID := mustRegisterKey(t, svc, "org-a", pub)
	publishSigned(t, svc, priv, keyID, agentID, "support-bot")
	if err := svc.RevokeKey(ctx, "org-a", keyID); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}

	// Provenance history stays auditable: key id + fingerprint remain
	// resolvable even though the key can no longer verify installs.
	prov, err := svc.ProvenanceFor(ctx, "org-b", "support-bot")
	if err != nil {
		t.Fatalf("ProvenanceFor: %v", err)
	}
	if !prov.Signed || prov.SigningKeyID != keyID || prov.Fingerprint == "" {
		t.Fatalf("revoked-key provenance must stay resolvable: %+v", prov)
	}
}

func TestOrgSettingsRoundTrip(t *testing.T) {
	svc, _ := newSigningFixture(t)
	ctx := context.Background()

	// Missing row IS the default policy.
	settings, err := svc.GetOrgSettings(ctx, "org-b")
	if err != nil {
		t.Fatalf("GetOrgSettings(default): %v", err)
	}
	if settings.AllowUnsigned {
		t.Fatal("default policy must be verify (allow_unsigned=false)")
	}
	if settings.UpdatedAt != (settings.UpdatedAt) { // zero time
		t.Fatal("default settings must carry no updated_at")
	}

	first, err := svc.SetAllowUnsigned(ctx, "org-b", true)
	if err != nil {
		t.Fatalf("SetAllowUnsigned: %v", err)
	}
	if !first.AllowUnsigned || first.UpdatedAt.IsZero() {
		t.Fatalf("settings write must stamp updated_at: %+v", first)
	}
	got, err := svc.GetOrgSettings(ctx, "org-b")
	if err != nil {
		t.Fatalf("GetOrgSettings: %v", err)
	}
	if !got.AllowUnsigned {
		t.Fatal("settings must persist")
	}
	if _, err := svc.SetAllowUnsigned(ctx, "org-b", false); err != nil {
		t.Fatalf("SetAllowUnsigned(false): %v", err)
	}
	if got, _ = svc.GetOrgSettings(ctx, "org-b"); got.AllowUnsigned {
		t.Fatal("settings must flip back to verify")
	}
	if _, err := svc.GetOrgSettings(ctx, " "); !errors.Is(err, ErrOrgRequired) {
		t.Fatalf("missing org (settings): got %v", err)
	}
}

// listingOnlyStore implements ONLY the issue-#28 Store contract — proving the
// signing capability stays optional and degrades with a clear error.
type listingOnlyStore struct{}

func (listingOnlyStore) CreateListing(context.Context, *Listing) error { return nil }
func (listingOnlyStore) GetListingBySlug(context.Context, string) (*Listing, error) {
	return nil, ErrNotFound
}
func (listingOnlyStore) BrowseListings(context.Context, BrowseOptions) ([]*Listing, string, error) {
	return nil, "", nil
}
func (listingOnlyStore) IncrementDownloadCount(context.Context, string) (int, error) {
	return 0, ErrNotFound
}
func (listingOnlyStore) UnlistListing(context.Context, string, string) error { return ErrNotFound }

func TestSigningRequiresSigningCapableStore(t *testing.T) {
	svc := NewServiceWithStore(listingOnlyStore{}, nil, nil)
	ctx := context.Background()
	pub, _ := genTestKey(t)

	if _, err := svc.RegisterKey(ctx, "org-a", RegisterKeyInput{PublicKey: base64.StdEncoding.EncodeToString(pub)}); !errors.Is(err, ErrSigningStoreUnavailable) {
		t.Fatalf("RegisterKey over a listing-only store: got %v", err)
	}
	if _, err := svc.ListKeys(ctx, "org-a"); !errors.Is(err, ErrSigningStoreUnavailable) {
		t.Fatalf("ListKeys over a listing-only store: got %v", err)
	}
	if err := svc.RevokeKey(ctx, "org-a", "k"); !errors.Is(err, ErrSigningStoreUnavailable) {
		t.Fatalf("RevokeKey over a listing-only store: got %v", err)
	}
	if _, err := svc.GetOrgSettings(ctx, "org-a"); !errors.Is(err, ErrSigningStoreUnavailable) {
		t.Fatalf("GetOrgSettings over a listing-only store: got %v", err)
	}
}

// TestUnsignedSnapshotExtraKeysInert pins WHY the manifest covers exactly the
// five config fields: extra keys tolerated by validateSnapshot are never
// installed, so ignoring them at verification cannot change what runs.
func TestUnsignedSnapshotExtraKeysInert(t *testing.T) {
	svc, _ := newSigningFixture(t)
	ctx := context.Background()
	pub, priv := genTestKey(t)
	keyID := mustRegisterKey(t, svc, "org-a", pub)

	// Publish with a snapshot that carries an inert extra key: the manifest
	// (five fields) is unchanged, so a signature over the manifest verifies.
	svc.mu.Lock()
	svc.items["l-extra"] = &Listing{
		ID: "l-extra", PublisherOrgID: "org-a", Slug: "extra-bot", Status: StatusPublished,
		VersionSnapshot: `{"name":"Support Bot","description":"Helps customers","instructions":"Be polite","model":"gpt-4o-mini","status":"DRAFT","future_field":{"nested":true}}`,
		Signature:       signSnapshot(t, priv, Snapshot{Name: "Support Bot", Description: "Helps customers", Instructions: "Be polite", Model: "gpt-4o-mini", Status: "DRAFT"}),
		SigningKeyID:    keyID, SigAlg: SignatureAlgEd25519,
		ManifestHash: ManifestHash(mustCanonical(t, Snapshot{Name: "Support Bot", Description: "Helps customers", Instructions: "Be polite", Model: "gpt-4o-mini", Status: "DRAFT"})),
	}
	svc.mu.Unlock()

	result, err := svc.Install(ctx, "org-b", "extra-bot")
	if err != nil {
		t.Fatalf("Install(extra keys): %v", err)
	}
	if result.Agent.Name != "Support Bot" || strings.Contains(result.Agent.Instructions, "future") {
		t.Fatalf("install surface must stay the five config fields: %+v", result.Agent)
	}
}
