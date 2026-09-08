package marketplace

// signing_service.go — the service layer over the signing primitives in
// signing.go (issue #78): publisher key registration/list/revocation, the
// org-level allow_unsigned install policy, publish-time signature capture and
// install-time verification (tamper evidence + provenance).
//
// Dual-mode like the rest of the service: when the durable store implements
// SigningKeyStore (the pgStore does) keys and org settings live in Postgres
// (migration 022); the in-memory mode keeps them in maps guarded by the same
// mutex as the listings. A custom Store that does not implement SigningKeyStore
// surfaces ErrSigningStoreUnavailable instead of silently dropping signatures.

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"
)

// ErrSigningStoreUnavailable is returned when the configured listing store
// cannot hold signing keys (a Store that does not implement SigningKeyStore).
var ErrSigningStoreUnavailable = errors.New("marketplace signing keys require a signing-capable store")

// signingKeyStore returns the SigningKeyStore capability of the configured
// store, or nil when the service runs in-memory (store == nil). A non-nil
// store WITHOUT the capability also returns nil and is reported through
// signingStoreMissing.
func (s *Service) signingKeyStore() SigningKeyStore {
	if s == nil || s.store == nil {
		return nil
	}
	if ks, ok := s.store.(SigningKeyStore); ok {
		return ks
	}
	return nil
}

// signingStoreMissing reports whether a non-nil store lacks the signing
// capability (distinct from the in-memory mode, which implements keys itself).
func (s *Service) signingStoreMissing() bool {
	return s != nil && s.store != nil && s.signingKeyStore() == nil
}

// ---------------------------------------------------------------------------
// Publisher keys
// ---------------------------------------------------------------------------

// RegisterKey stores the PUBLIC half of a publisher Ed25519 keypair for the
// caller's organization (the private half never leaves the publisher's
// client). Duplicate (org, public_key) pairs surface ErrDuplicateKey.
func (s *Service) RegisterKey(ctx context.Context, orgID string, in RegisterKeyInput) (*SigningKey, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgRequired
	}
	key, err := registerKeyValidation(in)
	if err != nil {
		return nil, err
	}
	if s.signingStoreMissing() {
		return nil, ErrSigningStoreUnavailable
	}
	key.ID = newSigningKeyID()
	key.OrganizationID = orgID
	key.CreatedAt = time.Now().UTC()

	if ks := s.signingKeyStore(); ks != nil {
		if err := ks.CreateSigningKey(ctx, key); err != nil {
			return nil, err
		}
		return key, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.keys {
		if existing.OrganizationID == orgID && existing.PublicKey == key.PublicKey {
			return nil, ErrDuplicateKey
		}
	}
	s.keys[key.ID] = key
	return key, nil
}

// ListKeys returns the caller org's registered signing keys, oldest first.
func (s *Service) ListKeys(ctx context.Context, orgID string) ([]*SigningKey, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgRequired
	}
	if s.signingStoreMissing() {
		return nil, ErrSigningStoreUnavailable
	}
	if ks := s.signingKeyStore(); ks != nil {
		return ks.ListSigningKeys(ctx, orgID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*SigningKey, 0, len(s.keys))
	for _, key := range s.keys {
		if key.OrganizationID == orgID {
			out = append(out, key)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// RevokeKey flips a key to status=revoked so it can no longer produce
// verifiable signatures (future installs of listings signed with it are
// blocked; agents already installed stay untouched). Unknown / foreign-org
// key ids surface ErrKeyNotFound; revoking an already-revoked key is
// idempotent (nil).
func (s *Service) RevokeKey(ctx context.Context, orgID, keyID string) error {
	if strings.TrimSpace(orgID) == "" {
		return ErrOrgRequired
	}
	if strings.TrimSpace(keyID) == "" {
		return ErrKeyNotFound
	}
	if s.signingStoreMissing() {
		return ErrSigningStoreUnavailable
	}
	if ks := s.signingKeyStore(); ks != nil {
		return ks.RevokeSigningKey(ctx, orgID, keyID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key, ok := s.keys[keyID]
	if !ok || key.OrganizationID != orgID {
		return ErrKeyNotFound
	}
	if key.Status == KeyStatusActive {
		key.Status = KeyStatusRevoked
		now := time.Now().UTC()
		key.RevokedAt = &now
	}
	return nil
}

// activeKey resolves the signing key for verification paths (publish and
// install). EVERY failure mode — unknown id, foreign org, REVOKED status —
// collapses to ErrUnknownSigningKey so the wire contract exposes exactly the
// issue-#78 codes (no existence leak for keys, no revoked/unknown distinction).
func (s *Service) activeKey(ctx context.Context, orgID, keyID string) (*SigningKey, error) {
	if strings.TrimSpace(keyID) == "" {
		return nil, ErrUnknownSigningKey
	}
	if ks := s.signingKeyStore(); ks != nil {
		key, err := ks.GetSigningKey(ctx, orgID, keyID)
		if err != nil || key.Status != KeyStatusActive {
			return nil, ErrUnknownSigningKey
		}
		return key, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key, ok := s.keys[keyID]
	if !ok || key.OrganizationID != orgID || key.Status != KeyStatusActive {
		return nil, ErrUnknownSigningKey
	}
	return key, nil
}

// ---------------------------------------------------------------------------
// Org-level install policy (allow_unsigned)
// ---------------------------------------------------------------------------

// GetOrgSettings returns the marketplace install policy for the org. Missing
// rows read as the zero value: allow_unsigned=false (default policy: verify).
func (s *Service) GetOrgSettings(ctx context.Context, orgID string) (*OrgSettings, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgRequired
	}
	if s.signingStoreMissing() {
		return nil, ErrSigningStoreUnavailable
	}
	if ks := s.signingKeyStore(); ks != nil {
		return ks.GetOrgSettings(ctx, orgID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if settings, ok := s.orgSettings[orgID]; ok {
		return settings, nil
	}
	return &OrgSettings{}, nil
}

// SetAllowUnsigned flips the org's install policy for unsigned NEW listings
// (legacy pre-signing listings are always installable regardless). The
// settings row is created on first write; UpdatedAt is stamped server-side.
func (s *Service) SetAllowUnsigned(ctx context.Context, orgID string, allow bool) (*OrgSettings, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgRequired
	}
	if s.signingStoreMissing() {
		return nil, ErrSigningStoreUnavailable
	}
	if ks := s.signingKeyStore(); ks != nil {
		return ks.UpsertOrgSettings(ctx, orgID, allow)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	settings := &OrgSettings{AllowUnsigned: allow, UpdatedAt: time.Now().UTC()}
	s.orgSettings[orgID] = settings
	return settings, nil
}

// ---------------------------------------------------------------------------
// Publish-time signature capture + install-time verification
// ---------------------------------------------------------------------------

// applyPublishSignature validates and attaches the detached signature of a
// publish request onto the freshly built listing. exactly-one-of signature /
// key id is ErrSignatureIncomplete; an unregistered, foreign or revoked key is
// ErrUnknownSigningKey; a signature that does not verify over the canonical
// manifest of the snapshot being published is ErrSignatureInvalid. On success
// the listing carries signature + key id + alg + signed_at (server clock) +
// the content-addressed manifest hash.
func (s *Service) applyPublishSignature(ctx context.Context, orgID string, listing *Listing, snap Snapshot, signature, keyID string) error {
	if (strings.TrimSpace(signature) == "") != (strings.TrimSpace(keyID) == "") {
		return ErrSignatureIncomplete
	}
	if strings.TrimSpace(signature) == "" {
		return nil // unsigned publish (legacy behavior, now subject to install policy)
	}
	key, err := s.activeKey(ctx, orgID, keyID)
	if err != nil {
		return err
	}
	canonical, err := manifestBytes(snap)
	if err != nil {
		return err
	}
	if err := verifyDetachedSignature(key.PublicKey, signature, canonical); err != nil {
		return err
	}
	now := listing.UpdatedAt // publish timestamp already stamped by Publish
	listing.Signature = signature
	listing.SigningKeyID = key.ID
	listing.SigAlg = key.Alg
	listing.SignedAt = &now
	listing.ManifestHash = ManifestHash(canonical)
	return nil
}

// verifyInstall enforces the install-time provenance policy (issue #78):
//
//   - legacy (pre-signing) listings are grandfathered and always installable;
//   - fully signed listings must (1) still hash to the manifest hash recorded
//     at publish time (snapshot_tampered on any post-publish mutation), (2)
//     reference a registered ACTIVE key of the publisher org
//     (unknown_signing_key — covers revocation), and (3) carry a signature
//     that verifies over the canonical manifest of the STORED snapshot
//     (signature_invalid);
//   - unsigned NEW listings are blocked with unsigned_listing unless the
//     INSTALLING org enabled allow_unsigned (default: verify);
//   - a half-signed row (some provenance columns set, others empty) cannot
//     come from the API and reads as snapshot_tampered (defense in depth).
func (s *Service) verifyInstall(ctx context.Context, callerOrgID string, listing *Listing, snap Snapshot) error {
	if listing.LegacyListing {
		return nil // pre-signing listings stay installable (grandfathering)
	}
	signed := listing.Signature != "" && listing.SigningKeyID != "" && listing.ManifestHash != ""
	unsigned := listing.Signature == "" && listing.SigningKeyID == "" && listing.ManifestHash == ""
	switch {
	case unsigned:
		settings, err := s.GetOrgSettings(ctx, callerOrgID)
		if err != nil {
			return err
		}
		if !settings.AllowUnsigned {
			return ErrUnsignedListing
		}
		return nil
	case signed:
		canonical, err := manifestBytes(snap)
		if err != nil {
			return err
		}
		if ManifestHash(canonical) != listing.ManifestHash {
			return ErrSnapshotTampered
		}
		key, err := s.activeKey(ctx, listing.PublisherOrgID, listing.SigningKeyID)
		if err != nil {
			return err
		}
		return verifyDetachedSignature(key.PublicKey, listing.Signature, canonical)
	default:
		return ErrSnapshotTampered
	}
}

// ProvenanceFor resolves the verifiable origin document of a listing under
// the SAME visibility rules as GetBySlug (published listings are global read;
// draft/unlisted are visible only to their publisher org). The fingerprint is
// resolved from the publisher's key row when available — including REVOKED
// keys, whose provenance history remains auditable.
func (s *Service) ProvenanceFor(ctx context.Context, callerOrgID, slug string) (*Provenance, error) {
	listing, err := s.listingBySlug(ctx, slug)
	if err != nil {
		return nil, err
	}
	if listing.Status != StatusPublished && listing.PublisherOrgID != callerOrgID {
		return nil, ErrNotFound
	}
	prov := &Provenance{
		Slug:           listing.Slug,
		PublisherOrgID: listing.PublisherOrgID,
		SigningKeyID:   listing.SigningKeyID,
		Alg:            listing.SigAlg,
		SignedAt:       listing.SignedAt,
		ManifestHash:   listing.ManifestHash,
		Signed:         listing.Signature != "",
	}
	if listing.SigningKeyID != "" {
		// Fingerprint lookup tolerates failure: a missing key row degrades to
		// an empty fingerprint, it must never hide the provenance document.
		if ks := s.signingKeyStore(); ks != nil {
			if key, kerr := ks.GetSigningKey(ctx, listing.PublisherOrgID, listing.SigningKeyID); kerr == nil {
				prov.Fingerprint = key.Fingerprint()
			}
		} else if !s.signingStoreMissing() {
			s.mu.Lock()
			if key, ok := s.keys[listing.SigningKeyID]; ok && key.OrganizationID == listing.PublisherOrgID {
				prov.Fingerprint = key.Fingerprint()
			}
			s.mu.Unlock()
		}
	}
	return prov, nil
}
