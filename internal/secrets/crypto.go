package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
)

// crypto.go implements the at-rest encryption core (issue #25):
//
//   - AES-256-GCM (stdlib crypto/aes + crypto/cipher ONLY, no new deps) with a
//     per-secret random 12-byte nonce from crypto/rand on every Seal call.
//   - A versioned envelope string ("v1:" prefix) is the canonical serialized
//     form of one encrypted secret:
//
//         v1:<keyVersion>:<base64(nonce)>:<base64(ciphertext)>
//
//     The "v1" prefix is the ENVELOPE FORMAT version (AES-256-GCM with
//     per-secret nonce). The embedded keyVersion selects which master key
//     decrypts the payload, so key rotation never bricks stored rows: seal
//     with the current key, open with the key registered under the envelope's
//     version. Postgres rows keep the same parts in dedicated columns
//     (ciphertext BYTEA, nonce BYTEA, key_version INT) instead of the envelope
//     string; FormatEnvelope/ParseEnvelope convert losslessly.
//   - Decryption is authenticated: a wrong master key or a tampered
//     ciphertext/nonce fails GCM verification and surfaces as ErrDecryptFailed
//     (never as garbage plaintext).

const (
	// EnvelopePrefix marks the envelope format version. A future format change
	// (e.g. different AEAD or nonce strategy) bumps this to "v2" while v1
	// envelopes stay readable.
	EnvelopePrefix = "v1"

	// KeyLength is the required master key size for AES-256 (bytes).
	KeyLength = 32

	// NonceLength is the standard GCM nonce size (bytes).
	NonceLength = 12

	// MasterKeyEnvVar holds the base64-encoded 32-byte master key.
	MasterKeyEnvVar = "AGENTOS_SECRETS_MASTER_KEY"
)

var (
	// ErrInvalidMasterKey is returned when a master key is not exactly 32 bytes.
	ErrInvalidMasterKey = errors.New("secrets: master key must be 32 bytes of base64-decoded data for AES-256")

	// ErrInvalidEnvelope is returned when an envelope string is malformed.
	ErrInvalidEnvelope = errors.New("secrets: invalid envelope format")

	// ErrUnknownKeyVersion is returned when an envelope references a key
	// version that is not registered in the cipher (rotation not applied here).
	ErrUnknownKeyVersion = errors.New("secrets: envelope references an unknown key version")

	// ErrDecryptFailed wraps AES-GCM authentication failures: wrong key,
	// tampered ciphertext, or tampered nonce. The underlying error is never
	// returned verbatim so failure detail cannot leak timing/oracle hints.
	ErrDecryptFailed = errors.New("secrets: decryption failed (wrong key or tampered data)")

	// ErrMasterKeyRequired is the fail-fast signal for Postgres mode: no
	// service may persist secrets without a usable master key.
	ErrMasterKeyRequired = errors.New("secrets: AGENTOS_SECRETS_MASTER_KEY is required in Postgres mode (base64 of 32 bytes)")
)

// Cipher seals/opens secret values with AES-256-GCM. Keys are registered by
// integer version; Seal always uses the current (highest registered) version
// while Open selects the key named by the envelope, which is what makes
// rotation possible without re-encrypting the world in one shot.
type Cipher struct {
	mu      sync.RWMutex
	keys    map[int][]byte
	current int
}

// NewCipher validates the master key (32 bytes for AES-256) and registers it
// as key version 1 (the initial/current version).
func NewCipher(key []byte) (*Cipher, error) {
	if len(key) != KeyLength {
		return nil, ErrInvalidMasterKey
	}
	k := make([]byte, KeyLength)
	copy(k, key)
	return &Cipher{
		keys:    map[int][]byte{1: k},
		current: 1,
	}, nil
}

// NewCipherFromEnv builds a Cipher from AGENTOS_SECRETS_MASTER_KEY
// (base64-encoded 32 bytes, e.g. `openssl rand -base64 32`). It fails fast
// when the variable is missing, undecodable, or the wrong length — Postgres
// mode refuses to run without encryption.
func NewCipherFromEnv() (*Cipher, error) {
	raw := strings.TrimSpace(os.Getenv(MasterKeyEnvVar))
	if raw == "" {
		return nil, ErrMasterKeyRequired
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidMasterKey, err.Error())
	}
	return NewCipher(key)
}

// AddKey registers a future rotation key. Versions must be strictly
// increasing (sealing always moves forward); re-registering an existing
// version or a non-32-byte key is rejected.
func (c *Cipher) AddKey(version int, key []byte) error {
	if c == nil {
		return ErrInvalidMasterKey
	}
	if len(key) != KeyLength {
		return ErrInvalidMasterKey
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if version <= c.current {
		return fmt.Errorf("secrets: rotation key version must be > %d", c.current)
	}
	if _, exists := c.keys[version]; exists {
		return fmt.Errorf("secrets: key version %d already registered", version)
	}
	k := make([]byte, KeyLength)
	copy(k, key)
	c.keys[version] = k
	c.current = version
	return nil
}

// CurrentVersion reports the key version Seal uses.
func (c *Cipher) CurrentVersion() int {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.current
}

// Seal encrypts plaintext into a v1 envelope with a fresh random nonce.
func (c *Cipher) Seal(plaintext string) (string, error) {
	if c == nil {
		return "", ErrMasterKeyRequired
	}
	c.mu.RLock()
	version := c.current
	key := c.keys[version]
	c.mu.RUnlock()
	if len(key) != KeyLength {
		return "", ErrInvalidMasterKey
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, NonceLength)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("secrets: nonce generation failed: %w", err)
	}
	// GCM appends its 16-byte auth tag to the returned ciphertext.
	sealed := gcm.Seal(nil, nonce, []byte(plaintext), nil)
	return FormatEnvelope(version, nonce, sealed), nil
}

// Open decrypts a v1 envelope using the key registered under the envelope's
// embedded version. Wrong key / tampered data -> ErrDecryptFailed.
func (c *Cipher) Open(envelope string) (string, error) {
	version, nonce, ciphertext, err := ParseEnvelope(envelope)
	if err != nil {
		return "", err
	}
	return c.openParts(version, nonce, ciphertext)
}

// OpenParts decrypts stored parts (the Postgres column shape) without going
// through the envelope string.
func (c *Cipher) OpenParts(keyVersion int, nonce, ciphertext []byte) (string, error) {
	if c == nil {
		return "", ErrMasterKeyRequired
	}
	return c.openParts(keyVersion, nonce, ciphertext)
}

func (c *Cipher) openParts(keyVersion int, nonce, ciphertext []byte) (string, error) {
	c.mu.RLock()
	key, ok := c.keys[keyVersion]
	c.mu.RUnlock()
	if !ok {
		return "", ErrUnknownKeyVersion
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", ErrDecryptFailed
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", ErrDecryptFailed
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		// Authenticated failure: wrong key or tampered bytes. Collapse to one
		// sentinel so callers cannot distinguish oracle details.
		return "", ErrDecryptFailed
	}
	return string(plaintext), nil
}

// FormatEnvelope serializes (keyVersion, nonce, ciphertext) into the canonical
// v1 envelope: v1:<keyVersion>:<b64(nonce)>:<b64(ciphertext)>.
func FormatEnvelope(keyVersion int, nonce, ciphertext []byte) string {
	return EnvelopePrefix + ":" + strconv.Itoa(keyVersion) +
		":" + base64.StdEncoding.EncodeToString(nonce) +
		":" + base64.StdEncoding.EncodeToString(ciphertext)
}

// ParseEnvelope splits a v1 envelope back into its parts.
func ParseEnvelope(envelope string) (keyVersion int, nonce, ciphertext []byte, err error) {
	if !strings.HasPrefix(envelope, EnvelopePrefix+":") {
		return 0, nil, nil, ErrInvalidEnvelope
	}
	parts := strings.Split(envelope, ":")
	if len(parts) != 4 {
		return 0, nil, nil, ErrInvalidEnvelope
	}
	version, err := strconv.Atoi(parts[1])
	if err != nil || version <= 0 {
		return 0, nil, nil, ErrInvalidEnvelope
	}
	nonce, err = base64.StdEncoding.DecodeString(parts[2])
	if err != nil || len(nonce) != NonceLength {
		return 0, nil, nil, ErrInvalidEnvelope
	}
	ciphertext, err = base64.StdEncoding.DecodeString(parts[3])
	if err != nil || len(ciphertext) == 0 {
		return 0, nil, nil, ErrInvalidEnvelope
	}
	return version, nonce, ciphertext, nil
}

// newEphemeralKey generates a random 32-byte key for in-memory mode, where no
// configuration is required: values are still encrypted at rest in process
// memory (the map never holds plaintext), just with a key that dies with the
// process.
func newEphemeralKey() ([]byte, error) {
	key := make([]byte, KeyLength)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return key, nil
}
