package auth

// Issue #76 tests — the revocation deny-list stores (in-memory default and
// Postgres) plus the ONE documented revocation-identity strategy
// (TokenReference: "jti:<uuid>" for tokens carrying a jti, "tok:<sha256
// hex>" for legacy tokens without one).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestTokenReferenceUsesJTIWhenPresent(t *testing.T) {
	claims := &Claims{JTI: "token-id-1"}
	ref := TokenReference("any-token-string", claims)
	if ref != "jti:token-id-1" {
		t.Fatalf("expected jti key, got %q", ref)
	}
	// Pure derivation: the same token always yields the same key.
	if again := TokenReference("any-token-string", claims); again != ref {
		t.Fatalf("TokenReference must be deterministic: %q vs %q", ref, again)
	}
	// Whitespace-padded jtis are normalized away.
	if padded := TokenReference("any-token-string", &Claims{JTI: "  token-id-1 "}); padded != ref {
		t.Fatalf("jti must be trimmed: %q vs %q", ref, padded)
	}
}

func TestTokenReferenceHashesLegacyTokensWithoutJTI(t *testing.T) {
	token := "legacy.header.sig"
	sum := sha256.Sum256([]byte(token))
	want := "tok:" + hex.EncodeToString(sum[:])

	if got := TokenReference(token, nil); got != want {
		t.Fatalf("legacy token reference: got %q want %q", got, want)
	}
	if got := TokenReference(token, &Claims{}); got != want {
		t.Fatalf("empty-jti claims must fall back to the token hash: got %q want %q", got, want)
	}
	// Distinct legacy tokens must never share a deny-list key.
	if TokenReference("other.token.sig", nil) == want {
		t.Fatal("different legacy tokens must hash to different keys")
	}
}

func TestMemoryRevocationStoreRevokeAndLookup(t *testing.T) {
	store := NewMemoryRevocationStore()
	ctx := context.Background()
	if revoked, _ := store.Revoked(ctx, "k"); revoked {
		t.Fatal("unknown key must not read as revoked")
	}
	if err := store.Revoke(ctx, "k", time.Minute); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if revoked, _ := store.Revoked(ctx, "k"); !revoked {
		t.Fatal("denied key must read as revoked")
	}
	if revoked, _ := store.Revoked(ctx, "other"); revoked {
		t.Fatal("unrelated key must stay untouched")
	}
}

func TestMemoryRevocationStoreLazyExpiry(t *testing.T) {
	store := NewMemoryRevocationStore()
	// Seed an already-expired entry directly: expiry is lazy, so without a
	// lookup the entry lingers but must never read as revoked.
	store.mu.Lock()
	store.entries["dead"] = time.Now().Add(-time.Second)
	store.mu.Unlock()
	if revoked, _ := store.Revoked(context.Background(), "dead"); revoked {
		t.Fatal("expired entry must read as not revoked")
	}
}

func TestMemoryRevocationStoreSweepBoundsMemory(t *testing.T) {
	store := NewMemoryRevocationStore()
	store.mu.Lock()
	for i := 0; i < memoryRevocationSweepThreshold; i++ {
		store.entries["stale-"+strconv.Itoa(i)] = time.Now().Add(-time.Minute)
	}
	store.mu.Unlock()
	// One more revoke triggers the sweep: the expired backlog is dropped and
	// only live entries remain.
	if err := store.Revoke(context.Background(), "live", time.Minute); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if got := store.size(); got != 1 {
		t.Fatalf("sweep must bound the map to live entries, got %d", got)
	}
	if revoked, _ := store.Revoked(context.Background(), "live"); !revoked {
		t.Fatal("the freshly revoked key must survive the sweep")
	}
}

func TestMemoryRevocationStoreIgnoresEmptyKeyAndNonPositiveTTL(t *testing.T) {
	store := NewMemoryRevocationStore()
	ctx := context.Background()
	if err := store.Revoke(ctx, "", time.Minute); err != nil {
		t.Fatalf("empty-key Revoke must be a no-op success, got %v", err)
	}
	if err := store.Revoke(ctx, "k", 0); err != nil {
		t.Fatalf("zero-ttl Revoke must be a no-op success, got %v", err)
	}
	if err := store.Revoke(ctx, "k", -time.Minute); err != nil {
		t.Fatalf("negative-ttl Revoke must be a no-op success, got %v", err)
	}
	if store.size() != 0 {
		t.Fatal("no-op revokes must not create entries")
	}
}

func TestMemoryRevocationStoreConcurrentUse(t *testing.T) {
	store := NewMemoryRevocationStore()
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := string(rune('a' + i%26))
			_ = store.Revoke(ctx, key, time.Minute)
			_, _ = store.Revoked(ctx, key)
		}(i)
	}
	wg.Wait()
}

// failingRevocationStore models a deny-list backend outage.
type failingRevocationStore struct{}

func (failingRevocationStore) Revoke(context.Context, string, time.Duration) error {
	return errors.New("deny-list backend unavailable")
}
func (failingRevocationStore) Revoked(context.Context, string) (bool, error) {
	return false, errors.New("deny-list backend unavailable")
}

func TestPostgresRevocationStoreRevokeBindsAndSweeps(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	store := NewPostgresRevocationStore(db)

	// The upsert carries the key and the absolute expiry; the opportunistic
	// cleanup of dead rows follows in the same call.
	mock.ExpectExec("INSERT INTO auth_revocations").
		WithArgs("jti:abc", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM auth_revocations WHERE expires_at").
		WillReturnResult(sqlmock.NewResult(0, 0))

	if err := store.Revoke(context.Background(), "jti:abc", time.Minute); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestPostgresRevocationStoreRevokeNoopWithoutKeyOrTTL(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	store := NewPostgresRevocationStore(db)

	// No expectations: a non-positive TTL or empty key must not touch the DB.
	if err := store.Revoke(context.Background(), "jti:abc", 0); err != nil {
		t.Fatalf("zero-ttl Revoke: %v", err)
	}
	if err := store.Revoke(context.Background(), "  ", time.Minute); err != nil {
		t.Fatalf("empty-key Revoke: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unexpected statements executed: %v", err)
	}
}

func TestPostgresRevocationStoreRevokedLookup(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	store := NewPostgresRevocationStore(db)

	mock.ExpectQuery("SELECT EXISTS").
		WithArgs("jti:abc").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	revoked, err := store.Revoked(context.Background(), "jti:abc")
	if err != nil || !revoked {
		t.Fatalf("Revoked: got (%v, %v), want (true, nil)", revoked, err)
	}

	mock.ExpectQuery("SELECT EXISTS").
		WithArgs("jti:other").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	revoked, err = store.Revoked(context.Background(), "jti:other")
	if err != nil || revoked {
		t.Fatalf("Revoked miss: got (%v, %v), want (false, nil)", revoked, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestPostgresRevocationStoreNilDBFailsClosed(t *testing.T) {
	store := NewPostgresRevocationStore(nil)
	if _, err := store.Revoked(context.Background(), "k"); err == nil {
		t.Fatal("nil DB must surface an error, not a denial bypass")
	}
	if err := store.Revoke(context.Background(), "k", time.Minute); err == nil {
		t.Fatal("nil DB revoke must fail, not silently succeed")
	}
}

func TestRevocationStoresSatisfyInterface(t *testing.T) {
	var _ RevocationStore = NewMemoryRevocationStore()
	var _ RevocationStore = NewPostgresRevocationStore(nil)
	// The in-memory store is the constructor default: zero-infrastructure
	// mode must never start with a nil deny list.
	if NewService("s").revocations == nil {
		t.Fatal("NewService must wire the in-memory revocation store by default")
	}
}
