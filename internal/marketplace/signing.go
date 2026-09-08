package marketplace

// signing.go — publisher provenance for the marketplace (issue #78):
// Ed25519 publisher signing keys, the canonical manifest a signature covers,
// detached-signature verification and the org-level allow_unsigned setting.
//
// SIGNING FLOW (the private key never leaves the publisher's client):
//
//  1. The publisher generates an Ed25519 keypair locally (crypto/ed25519).
//  2. The publisher registers ONLY the public key — base64 (std encoding) of
//     the raw 32-byte ed25519.PublicKey — through POST /marketplace/keys.
//     The server issues a key id and stores status=active.
//  3. Before publishing, the client builds the manifest of the snapshot it
//     is about to publish (exactly the five config fields of
//     agents.AgentSnapshot — see Manifest), serializes it with
//     CanonicalManifestJSON and signs those exact bytes:
//
//      sig := ed25519.Sign(priv, canonicalManifestBytes)
//
//  4. The publish request carries the detached signature (base64 std of the
//     64-byte value) plus the signing key id. The server re-derives the
//     manifest from the snapshot it stores, verifies the signature against
//     the registered key and persists signature + key id + alg + signed_at
//     + the content-addressed manifest hash.
//  5. Install re-derives the manifest from the STORED snapshot, re-hashes it
//     (tamper evidence) and re-verifies the signature against the
//     publisher's registered key. Postgres JSONB re-normalizes whitespace
//     and key order, which is precisely why the signature covers the
//     canonical manifest — not the raw snapshot bytes.
//
// FINGERPRINT: sha256 over the raw 32-byte public key, lowercase hex (64
// chars). MANIFEST HASH: sha256 over the canonical manifest bytes, lowercase
// hex (64 chars). Both are stable, opaque identifiers.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Signing key lifecycle statuses.
const (
	KeyStatusActive  = "active"
	KeyStatusRevoked = "revoked"
)

// SignatureAlgEd25519 is the only supported signature algorithm.
const SignatureAlgEd25519 = "ed25519"

// Errors for the provenance subsystem. The install failure modes map to the
// contract codes unsigned_listing / unknown_signing_key / signature_invalid /
// snapshot_tampered (see cmd/api/marketplace.go mapMktError).
var (
	// ErrUnknownSigningKey is returned when a signature cannot be attributed
	// to a registered, ACTIVE key of the publisher org: the key id is
	// unknown, registered to another org, or REVOKED (revocation blocks
	// future installs; agents already installed stay untouched).
	ErrUnknownSigningKey = errors.New("signing key is not registered (or has been revoked) for the publisher organization")
	// ErrSignatureInvalid is returned when the detached signature does not
	// verify against the publisher's registered key and the stored manifest.
	ErrSignatureInvalid = errors.New("listing signature does not verify")
	// ErrUnsignedListing is returned at install when a NEW listing was
	// published without a signature and the installing org has not enabled
	// the allow_unsigned org setting (default policy: verify).
	ErrUnsignedListing = errors.New("listing is unsigned and the installing organization requires signed listings")
	// ErrSnapshotTampered is returned at install when the stored snapshot no
	// longer hashes to the manifest hash recorded at publish time (any
	// post-publish mutation invalidates the listing).
	ErrSnapshotTampered = errors.New("listing snapshot does not match the manifest hash recorded at publish time")
	// ErrSignatureIncomplete is returned at publish when exactly one of the
	// signature / key_id pair is provided.
	ErrSignatureIncomplete = errors.New("publish requires both signature and key_id when signing")
	// ErrKeyNotFound is returned for unknown signing keys and for foreign-org
	// keys (no existence leak across tenants).
	ErrKeyNotFound = errors.New("marketplace signing key not found")
	// ErrDuplicateKey is returned when the org already registered the same
	// public key.
	ErrDuplicateKey = errors.New("signing key already registered")
	// ErrPublicKeyInvalid is returned when the registered public key is not a
	// base64-encoded 32-byte Ed25519 public key.
	ErrPublicKeyInvalid = errors.New("public_key must be base64 (std) of a 32-byte ed25519 public key")
	// ErrAlgUnsupported is returned when a non-ed25519 algorithm is requested.
	ErrAlgUnsupported = errors.New("unsupported signature algorithm; only ed25519 is supported")
)

// SigningKey is one registered publisher public key (org-scoped). The PRIVATE
// half never touches the server: only the base64 public key is stored.
type SigningKey struct {
	ID             string
	OrganizationID string
	PublicKey      string // base64 (std) of the raw 32-byte ed25519 public key
	Alg            string // "ed25519"
	Status         string // active|revoked
	CreatedAt      time.Time
	RevokedAt      *time.Time
}

// Fingerprint returns sha256(raw public key bytes) as lowercase hex. It is
// the human-comparable key identifier surfaced in the provenance document.
func (k *SigningKey) Fingerprint() string {
	raw, err := base64.StdEncoding.DecodeString(k.PublicKey)
	if err != nil {
		return ""
	}
	return FingerprintBytes(raw)
}

// FingerprintBytes hex-encodes sha256 over raw key material (shared by the
// key model and tests).
func FingerprintBytes(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// DecodePublicKey parses a registered base64 public key into an
// ed25519.PublicKey, enforcing the 32-byte length. Exported so SDK clients
// and tests can share the exact parsing rule with the server.
func DecodePublicKey(b64 string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, ErrPublicKeyInvalid
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, ErrPublicKeyInvalid
	}
	return ed25519.PublicKey(raw), nil
}

// Manifest is the EXACT document a listing signature covers: the five config
// fields of the listing snapshot (the agents.AgentSnapshot document shape —
// name/description/instructions/model/status), nothing more, nothing less.
//
// Rationale: these five fields are the entire install surface (Install replays
// exactly them into the new agent), Postgres JSONB normalizes whitespace and
// key order so raw snapshot bytes are not stable, and extra keys tolerated by
// validateSnapshot are inert (they are never installed). Signing the manifest
// therefore signs exactly what an install consumes.
type Manifest struct {
	Description  string `json:"description"`
	Instructions string `json:"instructions"`
	Model        string `json:"model"`
	Name         string `json:"name"`
	Status       string `json:"status"`
}

// BuildManifest projects a validated snapshot onto the signed document.
func BuildManifest(snap Snapshot) Manifest {
	return Manifest{
		Description:  snap.Description,
		Instructions: snap.Instructions,
		Model:        snap.Model,
		Name:         snap.Name,
		Status:       snap.Status,
	}
}

// CanonicalManifestJSON serializes v into the AgentOS canonical JSON form
// used for marketplace signatures and manifest hashes. The rules (pinned by
// the golden test in signing_test.go) are:
//
//  1. Object keys are sorted lexicographically by their UTF-8 bytes
//     (Go encoding/json map marshaling order) — input key order is
//     irrelevant.
//  2. No insignificant whitespace: compact output, zero indentation.
//  3. No HTML escaping: '<', '>' and '&' are emitted literally
//     (json.Encoder.SetEscapeHTML(false)) — unlike encoding/json's default.
//  4. Strings use the RFC 8259 minimal escaping produced by encoding/json.
//  5. Numbers are preserved verbatim from the source document (decoded via
//     json.Number; no float renormalization, so "1.0" stays "1.0").
//  6. Array element order is preserved.
//  7. The result is UTF-8 WITHOUT a trailing newline (json.Encoder's
//     automatic newline is trimmed).
func CanonicalManifestJSON(v any) ([]byte, error) {
	// Normalize through an intermediate encode/decode so any Go value
	// (struct, map, raw pointer graph) collapses onto the same document
	// tree; UseNumber keeps number literals verbatim for rule 5.
	intermediate, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(intermediate))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // rule 3
	enc.SetIndent("", "")    // rule 2
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil // rule 7
}

// ManifestHash is the content address of a canonical manifest: sha256 over
// the exact canonical bytes, lowercase hex (64 chars).
func ManifestHash(canonical []byte) string {
	return FingerprintBytes(canonical)
}

// manifestBytes derives the canonical manifest bytes of a snapshot document.
func manifestBytes(snap Snapshot) ([]byte, error) {
	return CanonicalManifestJSON(BuildManifest(snap))
}

// verifyDetachedSignature checks a base64 (std) detached signature over the
// canonical manifest against the registered public key.
func verifyDetachedSignature(publicKeyB64, signatureB64 string, manifest []byte) error {
	pub, err := DecodePublicKey(publicKeyB64)
	if err != nil {
		return err
	}
	sig, err := base64.StdEncoding.DecodeString(signatureB64)
	if err != nil {
		return fmt.Errorf("%w: signature is not valid base64", ErrSignatureInvalid)
	}
	if len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("%w: signature must be %d bytes", ErrSignatureInvalid, ed25519.SignatureSize)
	}
	if !ed25519.Verify(pub, manifest, sig) {
		return ErrSignatureInvalid
	}
	return nil
}

// OrgSettings is the marketplace-scoped org configuration. Deliberately NOT
// part of the policy engine (internal/policies stays decoupled): the
// marketplace owns its own org-level row so the provenance seam can evolve
// independently.
type OrgSettings struct {
	// AllowUnsigned is the org's install policy for unsigned NEW listings.
	// false (default, zero value) = verify: installs of unsigned new listings
	// are blocked with unsigned_listing. true = this org accepts unsigned
	// listings. Legacy listings (published before signing existed) are ALWAYS
	// installable regardless of this flag (grandfathering).
	AllowUnsigned bool      `json:"allow_unsigned"`
	UpdatedAt     time.Time `json:"-"`
}

// Provenance is the verifiable origin document of a listing (GET
// /marketplace/listings/{slug}/provenance).
type Provenance struct {
	Slug           string
	PublisherOrgID string
	SigningKeyID   string // "" when unsigned
	Fingerprint    string // "" when unsigned or the key row is unavailable
	Alg            string // "" when unsigned
	SignedAt       *time.Time
	ManifestHash   string // "" for legacy (pre-signing) listings
	Signed         bool
}

// RegisterKeyInput is the key-registration request.
type RegisterKeyInput struct {
	PublicKey string // base64 (std) of the raw 32-byte ed25519 public key
	Alg       string // optional; defaults to (and only allows) ed25519
}

// registerKeyValidation normalizes and validates RegisterKeyInput.
func registerKeyValidation(in RegisterKeyInput) (*SigningKey, error) {
	alg := in.Alg
	if alg == "" {
		alg = SignatureAlgEd25519
	}
	if alg != SignatureAlgEd25519 {
		return nil, ErrAlgUnsupported
	}
	if _, err := DecodePublicKey(in.PublicKey); err != nil {
		return nil, err
	}
	return &SigningKey{
		PublicKey: in.PublicKey,
		Alg:       alg,
		Status:    KeyStatusActive,
	}, nil
}

// newSigningKeyID generates the key identifier.
func newSigningKeyID() string { return uuid.NewString() }
