package secrets

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

// testKey is a deterministic 32-byte master key for unit tests.
func testKey(seed byte) []byte {
	key := make([]byte, KeyLength)
	for i := range key {
		key[i] = seed + byte(i)
	}
	return key
}

func mustCipher(t *testing.T, key []byte) *Cipher {
	t.Helper()
	c, err := NewCipher(key)
	if err != nil {
		t.Fatalf("NewCipher returned error: %v", err)
	}
	return c
}

func TestNewCipherRejectsWrongKeyLength(t *testing.T) {
	for _, size := range []int{0, 1, 16, 31, 33, 64} {
		if _, err := NewCipher(make([]byte, size)); err == nil {
			t.Errorf("NewCipher(%d bytes) should fail", size)
		}
	}
	if _, err := NewCipher(testKey(1)); err != nil {
		t.Fatalf("NewCipher(32 bytes) returned error: %v", err)
	}
}

func TestNewCipherFromEnv(t *testing.T) {
	t.Setenv(MasterKeyEnvVar, "")
	if _, err := NewCipherFromEnv(); err == nil {
		t.Fatal("missing env var must fail fast")
	}

	t.Setenv(MasterKeyEnvVar, "not-base64!!!")
	if _, err := NewCipherFromEnv(); err == nil {
		t.Fatal("undecodable base64 must fail fast")
	}

	t.Setenv(MasterKeyEnvVar, "c2hvcnQ=") // decodes to "short", not 32 bytes
	if _, err := NewCipherFromEnv(); err == nil {
		t.Fatal("wrong-length key must fail fast")
	}

	// StdEncoding of 32 deterministic bytes (what `openssl rand -base64 32`
	// emits, modulo the trimmed newline):
	t.Setenv(MasterKeyEnvVar, base64.StdEncoding.EncodeToString(testKey(7)))
	c, err := NewCipherFromEnv()
	if err != nil {
		t.Fatalf("valid env key failed: %v", err)
	}
	env, err := c.Seal("payload")
	if err != nil {
		t.Fatalf("Seal failed: %v", err)
	}
	got, err := c.Open(env)
	if err != nil || got != "payload" {
		t.Fatalf("round-trip failed: %q err=%v", got, err)
	}
}

func TestSealOpenRoundTrip(t *testing.T) {
	c := mustCipher(t, testKey(1))
	for _, plaintext := range []string{"sk-openai-abc123", "line1\nline2", "\x00\x01binary-ish", strings.Repeat("x", 4096)} {
		env, err := c.Seal(plaintext)
		if err != nil {
			t.Fatalf("Seal(%q) failed: %v", truncate(plaintext), err)
		}
		if !strings.HasPrefix(env, EnvelopePrefix+":") {
			t.Fatalf("envelope must carry %q prefix, got %q", EnvelopePrefix, truncate(env))
		}
		got, err := c.Open(env)
		if err != nil {
			t.Fatalf("Open failed: %v", err)
		}
		if got != plaintext {
			t.Fatalf("round-trip mismatch: got %q want %q", truncate(got), truncate(plaintext))
		}
	}
}

func TestSealNeverReturnsPlaintextAndNonceIsRandom(t *testing.T) {
	c := mustCipher(t, testKey(1))
	env1, err := c.Seal("super-secret-value")
	if err != nil {
		t.Fatalf("Seal failed: %v", err)
	}
	env2, err := c.Seal("super-secret-value")
	if err != nil {
		t.Fatalf("Seal failed: %v", err)
	}
	if env1 == env2 {
		t.Fatal("same plaintext sealed twice must produce different envelopes (per-secret random nonce)")
	}
	// The plaintext must never appear in the envelope (base64 of it included).
	if strings.Contains(env1, "super-secret-value") {
		t.Fatal("envelope leaks plaintext")
	}
	v1, n1, c1, err := ParseEnvelope(env1)
	if err != nil {
		t.Fatalf("ParseEnvelope failed: %v", err)
	}
	if v1 != 1 || len(n1) != NonceLength || len(c1) == 0 {
		t.Fatalf("unexpected envelope parts: v=%d nonceLen=%d ctLen=%d", v1, len(n1), len(c1))
	}
	if bytes.Contains(c1, []byte("super-secret-value")) {
		t.Fatal("ciphertext contains raw plaintext")
	}
}

func TestOpenWithWrongKeyFails(t *testing.T) {
	sealer := mustCipher(t, testKey(1))
	env, err := sealer.Seal("topsecret")
	if err != nil {
		t.Fatalf("Seal failed: %v", err)
	}
	wrong := mustCipher(t, testKey(2)) // different 32-byte key
	if got, err := wrong.Open(env); err == nil {
		t.Fatalf("wrong key must fail GCM auth, got plaintext %q", truncate(got))
	} else if err != ErrDecryptFailed {
		t.Fatalf("wrong key must surface ErrDecryptFailed, got %v", err)
	}
}

func TestOpenWithTamperedCiphertextFails(t *testing.T) {
	c := mustCipher(t, testKey(1))
	env, err := c.Seal("topsecret")
	if err != nil {
		t.Fatalf("Seal failed: %v", err)
	}
	version, nonce, ciphertext, err := ParseEnvelope(env)
	if err != nil {
		t.Fatalf("ParseEnvelope failed: %v", err)
	}

	// Flip one bit in the ciphertext.
	tampered := append([]byte(nil), ciphertext...)
	tampered[0] ^= 0x01
	if _, err := c.OpenParts(version, nonce, tampered); err != ErrDecryptFailed {
		t.Fatalf("tampered ciphertext must fail with ErrDecryptFailed, got %v", err)
	}

	// Flip one bit in the nonce.
	badNonce := append([]byte(nil), nonce...)
	badNonce[len(badNonce)-1] ^= 0x01
	if _, err := c.OpenParts(version, badNonce, ciphertext); err != ErrDecryptFailed {
		t.Fatalf("tampered nonce must fail with ErrDecryptFailed, got %v", err)
	}

	// Truncated ciphertext.
	if _, err := c.OpenParts(version, nonce, ciphertext[:len(ciphertext)-1]); err != ErrDecryptFailed {
		t.Fatalf("truncated ciphertext must fail with ErrDecryptFailed, got %v", err)
	}
}

func TestEnvelopeFormatParse(t *testing.T) {
	nonce := bytes.Repeat([]byte{0xAB}, NonceLength)
	ct := []byte{1, 2, 3, 4, 5}
	env := FormatEnvelope(3, nonce, ct)
	if !strings.HasPrefix(env, "v1:") {
		t.Fatalf("envelope must start with v1:, got %q", env)
	}
	version, gotNonce, gotCt, err := ParseEnvelope(env)
	if err != nil {
		t.Fatalf("ParseEnvelope failed: %v", err)
	}
	if version != 3 || !bytes.Equal(gotNonce, nonce) || !bytes.Equal(gotCt, ct) {
		t.Fatalf("envelope round-trip mismatch: v=%d nonce=%v ct=%v", version, gotNonce, gotCt)
	}

	bad := []string{"", "v2:1:AAAA:AAAA", "v1:x:AAAA:AAAA", "v1:0:AAAA:AAAA", "v1:1:!!!:AAAA", "v1:1:AAAA", "v1:1:AAAA:AAAA:EXTRA"}
	for _, tc := range bad {
		if _, _, _, err := ParseEnvelope(tc); err == nil {
			t.Errorf("ParseEnvelope(%q) should fail", tc)
		}
	}
}

func TestKeyRotationSeam(t *testing.T) {
	c := mustCipher(t, testKey(1))
	if c.CurrentVersion() != 1 {
		t.Fatalf("initial version should be 1, got %d", c.CurrentVersion())
	}
	oldEnv, err := c.Seal("rotate-me")
	if err != nil {
		t.Fatalf("Seal failed: %v", err)
	}

	// Rotate: register key version 2; old envelopes stay readable (open uses
	// the envelope's embedded version), new seals use version 2.
	if err := c.AddKey(2, testKey(2)); err != nil {
		t.Fatalf("AddKey failed: %v", err)
	}
	if c.CurrentVersion() != 2 {
		t.Fatalf("current version should be 2, got %d", c.CurrentVersion())
	}
	if got, err := c.Open(oldEnv); err != nil || got != "rotate-me" {
		t.Fatalf("old envelope must survive rotation: %q err=%v", got, err)
	}
	newEnv, err := c.Seal("rotate-me")
	if err != nil {
		t.Fatalf("Seal failed: %v", err)
	}
	v, _, _, err := ParseEnvelope(newEnv)
	if err != nil || v != 2 {
		t.Fatalf("new seal should embed version 2, got v=%d err=%v", v, err)
	}

	// Rotation guardrails: non-increasing versions and short keys rejected.
	if err := c.AddKey(2, testKey(3)); err == nil {
		t.Fatal("re-registering version 2 must fail")
	}
	if err := c.AddKey(1, testKey(3)); err == nil {
		t.Fatal("decreasing version must fail")
	}
	if err := c.AddKey(3, []byte("short")); err == nil {
		t.Fatal("short rotation key must fail")
	}

	// An envelope from an unregistered future version fails cleanly.
	if _, err := c.Open(FormatEnvelope(9, bytes.Repeat([]byte{1}, NonceLength), []byte{1})); err != ErrUnknownKeyVersion {
		t.Fatalf("unknown key version must surface ErrUnknownKeyVersion, got %v", err)
	}
}

func truncate(s string) string {
	if len(s) > 24 {
		return s[:24] + "..."
	}
	return s
}
