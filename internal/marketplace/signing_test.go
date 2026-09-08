package marketplace

// signing_test.go — pins for the signing primitives (issue #78): the
// canonicalization golden (deterministic JSON: sorted keys, no HTML escaping,
// no insignificant whitespace, verbatim numbers, no trailing newline), the
// content addresses (sha256 hex fingerprints / manifest hashes) and the
// Ed25519 detached-signature round trip.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

// TestCanonicalManifestJSONGolden pins the EXACT canonical bytes. Any change
// here invalidates every stored signature + manifest hash, so this test must
// fail loudly if the document shape ever drifts.
func TestCanonicalManifestJSONGolden(t *testing.T) {
	// Map input on purpose: proves the output does not depend on the source
	// key order (rule 1: keys sorted by UTF-8 bytes).
	input := map[string]any{
		"status":       "DRAFT",
		"name":         "Agent <b>& \"quotes\" ",
		"model":        "gpt-4o",
		"instructions": "line1\nline2 é中",
		"description":  "desc",
		"count":        json.Number("1.0"),
	}
	want := `{"count":1.0,"description":"desc","instructions":"line1\nline2 é中","model":"gpt-4o","name":"Agent <b>& \"quotes\" ","status":"DRAFT"}`
	got, err := CanonicalManifestJSON(input)
	if err != nil {
		t.Fatalf("CanonicalManifestJSON: %v", err)
	}
	if string(got) != want {
		t.Fatalf("canonicalization golden mismatch:\n got: %s\nwant: %s", got, want)
	}

	// Rule 7: no trailing newline; rule 3: <, >, & literal.
	if bytes.HasSuffix(got, []byte("\n")) {
		t.Fatal("canonical output must not carry a trailing newline")
	}
	if !strings.Contains(string(got), "<b>&") {
		t.Fatal("HTML characters must not be escaped (SetEscapeHTML(false))")
	}

	// Idempotence: canonical(canonical(x)) == canonical(x). RawMessage keeps
	// the bytes a document (a plain []byte would marshal as base64).
	again, err := CanonicalManifestJSON(json.RawMessage(got))
	if err != nil {
		t.Fatalf("CanonicalManifestJSON(again): %v", err)
	}
	if !bytes.Equal(got, again) {
		t.Fatalf("canonicalization not idempotent: %s vs %s", got, again)
	}

	// Rule 1 via struct input: field declaration order must not matter — the
	// Manifest struct (Description..Status) and a map with the same five
	// entries canonicalize to identical bytes.
	structBytes, err := CanonicalManifestJSON(Manifest{
		Description:  "d",
		Instructions: "i",
		Model:        "m",
		Name:         "Support Bot",
		Status:       "DRAFT",
	})
	if err != nil {
		t.Fatalf("CanonicalManifestJSON(struct): %v", err)
	}
	mapBytes, err := CanonicalManifestJSON(map[string]any{
		"description": "d", "instructions": "i", "model": "m",
		"name": "Support Bot", "status": "DRAFT",
	})
	if err != nil {
		t.Fatalf("CanonicalManifestJSON(map): %v", err)
	}
	if !bytes.Equal(structBytes, mapBytes) {
		t.Fatalf("struct and map inputs must canonicalize identically: %s vs %s", structBytes, mapBytes)
	}

	// Rule 5: numbers stay verbatim (no float renormalization).
	nums, err := CanonicalManifestJSON(json.RawMessage(`{"n":1.0,"big":12345678901234567890}`))
	if err != nil {
		t.Fatalf("CanonicalManifestJSON(numbers): %v", err)
	}
	if want := `{"big":12345678901234567890,"n":1.0}`; string(nums) != want {
		t.Fatalf("number literals must be preserved verbatim: got %s want %s", nums, want)
	}
}

// TestManifestHashPin pins the content address: sha256 over the exact
// canonical manifest bytes, lowercase hex — computed independently here AND
// against a hardcoded vector so an algorithm drift cannot slip through.
func TestManifestHashPin(t *testing.T) {
	snap := Snapshot{Name: "Support Bot", Description: "d", Instructions: "i", Model: "m", Status: "DRAFT"}
	canonical, err := manifestBytes(snap)
	if err != nil {
		t.Fatalf("manifestBytes: %v", err)
	}
	wantCanonical := `{"description":"d","instructions":"i","model":"m","name":"Support Bot","status":"DRAFT"}`
	if string(canonical) != wantCanonical {
		t.Fatalf("manifest golden mismatch:\n got: %s\nwant: %s", canonical, wantCanonical)
	}

	if got, want := ManifestHash(canonical), "bc5427a3c62b907f3958f38f722756f79e948112cb1bc4b6c275cfa1be80bfd0"; got != want {
		t.Fatalf("ManifestHash pin: got %s want %s", got, want)
	}
	sum := sha256.Sum256(canonical)
	if got := hex.EncodeToString(sum[:]); got != ManifestHash(canonical) {
		t.Fatalf("ManifestHash must be sha256-hex of the canonical bytes: %s vs %s", got, ManifestHash(canonical))
	}
}

// TestFingerprintPin pins the key fingerprint: sha256 over the RAW public key
// bytes, lowercase hex.
func TestFingerprintPin(t *testing.T) {
	if got, want := FingerprintBytes([]byte("agentos")), "5b72ba9448008bb7da5d31b39591ec30e3ae90975939fec8196340955322b54c"; got != want {
		t.Fatalf("FingerprintBytes pin: got %s want %s", got, want)
	}
}

// TestDecodePublicKey enforces the registration rule: base64 (std) of exactly
// ed25519.PublicKeySize bytes.
func TestDecodePublicKey(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	got, err := DecodePublicKey(base64.StdEncoding.EncodeToString(pub))
	if err != nil {
		t.Fatalf("DecodePublicKey(valid): %v", err)
	}
	if !bytes.Equal(got, pub) || ed25519.PrivateKeySize != len(priv) {
		t.Fatal("DecodePublicKey must round-trip the raw key bytes")
	}

	for name, bad := range map[string]string{
		"not base64":  "!!!not-base64!!!",
		"too short":   base64.StdEncoding.EncodeToString([]byte("short")),
		"empty":       "",
		"hex instead": hex.EncodeToString(pub),
	} {
		if _, err := DecodePublicKey(bad); err == nil {
			t.Fatalf("DecodePublicKey(%s) must fail", name)
		} else if !strings.Contains(err.Error(), ErrPublicKeyInvalid.Error()) {
			t.Fatalf("DecodePublicKey(%s) = %v, want ErrPublicKeyInvalid", name, err)
		}
	}
}

// TestVerifyDetachedSignatureRoundTrip covers the full sign -> verify path
// plus every tamper/malformation branch.
func TestVerifyDetachedSignatureRoundTrip(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	pubB64 := base64.StdEncoding.EncodeToString(pub)

	canonical, err := manifestBytes(Snapshot{Name: "n", Description: "d", Instructions: "i", Model: "m", Status: "DRAFT"})
	if err != nil {
		t.Fatalf("manifestBytes: %v", err)
	}
	sig := ed25519.Sign(priv, canonical)
	sigB64 := base64.StdEncoding.EncodeToString(sig)

	if err := verifyDetachedSignature(pubB64, sigB64, canonical); err != nil {
		t.Fatalf("verifyDetachedSignature(valid): %v", err)
	}

	// Tampered manifest -> invalid.
	tampered := append([]byte(nil), canonical...)
	tampered[len(tampered)-2] = 'X'
	if err := verifyDetachedSignature(pubB64, sigB64, tampered); err == nil {
		t.Fatal("tampered manifest must not verify")
	}

	// Wrong key -> invalid.
	otherPub, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	_ = otherPub
	if err := verifyDetachedSignature(base64.StdEncoding.EncodeToString(otherPriv.Public().(ed25519.PublicKey)), sigB64, canonical); err == nil {
		t.Fatal("foreign key must not verify")
	}

	// Malformed signature encodings.
	for name, bad := range map[string]string{
		"not base64":  "!!!",
		"too short":   base64.StdEncoding.EncodeToString(sig[:32]),
		"empty":       "",
		"wrong bytes": base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)),
	} {
		if err := verifyDetachedSignature(pubB64, bad, canonical); err == nil {
			t.Fatalf("verifyDetachedSignature(%s) must fail", name)
		}
	}

	// Malformed public key.
	if err := verifyDetachedSignature("!!!", sigB64, canonical); err == nil {
		t.Fatal("invalid public key must fail")
	}
}

// TestRegisterKeyValidation pins the key-registration normalization.
func TestRegisterKeyValidation(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	pubB64 := base64.StdEncoding.EncodeToString(pub)

	key, err := registerKeyValidation(RegisterKeyInput{PublicKey: pubB64})
	if err != nil {
		t.Fatalf("registerKeyValidation(valid): %v", err)
	}
	if key.Alg != SignatureAlgEd25519 || key.Status != KeyStatusActive {
		t.Fatalf("defaults: alg=%s status=%s", key.Alg, key.Status)
	}
	if key.Fingerprint() != FingerprintBytes(pub) {
		t.Fatalf("fingerprint mismatch: %s", key.Fingerprint())
	}

	// Explicit alg (only ed25519 accepted).
	if _, err := registerKeyValidation(RegisterKeyInput{PublicKey: pubB64, Alg: "ed25519"}); err != nil {
		t.Fatalf("registerKeyValidation(explicit ed25519): %v", err)
	}
	if _, err := registerKeyValidation(RegisterKeyInput{PublicKey: pubB64, Alg: "rsa"}); !strings.Contains(err.Error(), ErrAlgUnsupported.Error()) {
		t.Fatalf("registerKeyValidation(rsa) = %v, want ErrAlgUnsupported", err)
	}
	// Invalid public key material.
	if _, err := registerKeyValidation(RegisterKeyInput{PublicKey: "!!!"}); err == nil {
		t.Fatal("invalid public key must fail")
	}
}
