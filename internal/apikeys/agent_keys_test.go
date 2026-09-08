package apikeys

// Wave-8 agent identity v1: lifecycle, allow-list semantics, tenant scoping,
// and the least-privilege tool-execution seam (AuthorizeAgentKeyTool).

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"agentos/internal/agents"
)

// --- fakes -----------------------------------------------------------------

// agentCheckerFake adapts a function to the AgentExistenceChecker seam
// (mirrors agents.Service.GetAgentCtx semantics for tests).
type agentCheckerFake func(ctx context.Context, orgID, agentID string) (*agents.Agent, error)

func (f agentCheckerFake) GetAgentCtx(ctx context.Context, orgID, agentID string) (*agents.Agent, error) {
	return f(ctx, orgID, agentID)
}

// agentKeyStoreFake is a minimal in-test AgentKeyStore that records call
// counts so the durable-mode routing is observable.
type agentKeyStoreFake struct {
	keys   map[string]*AgentKey
	byHash map[string]*AgentKey
	calls  map[string]int
}

func newAgentKeyStoreFake() *agentKeyStoreFake {
	return &agentKeyStoreFake{
		keys:   make(map[string]*AgentKey),
		byHash: make(map[string]*AgentKey),
		calls:  make(map[string]int),
	}
}

func (f *agentKeyStoreFake) CreateAgentKey(_ context.Context, key *AgentKey) error {
	f.calls["create"]++
	k := *key
	f.keys[k.ID] = &k
	f.byHash[k.KeyHash] = &k
	return nil
}

func (f *agentKeyStoreFake) GetAgentKeyByHash(_ context.Context, hash string) (*AgentKey, error) {
	f.calls["getByHash"]++
	if key, ok := f.byHash[hash]; ok {
		k := *key
		return &k, nil
	}
	return nil, ErrAgentKeyNotFound
}

func (f *agentKeyStoreFake) GetAgentKey(_ context.Context, orgID, agentID, keyID string) (*AgentKey, error) {
	f.calls["get"]++
	key, ok := f.keys[keyID]
	if !ok || key.OrganizationID != orgID || key.AgentID != agentID {
		return nil, ErrAgentKeyNotFound
	}
	k := *key
	return &k, nil
}

func (f *agentKeyStoreFake) UpdateAgentKey(_ context.Context, key *AgentKey) error {
	f.calls["update"]++
	if _, ok := f.keys[key.ID]; !ok {
		return ErrAgentKeyNotFound
	}
	k := *key
	f.keys[k.ID] = &k
	f.byHash[k.KeyHash] = &k
	return nil
}

func (f *agentKeyStoreFake) RevokeAgentKey(_ context.Context, orgID, agentID, keyID string, revokedAt time.Time) error {
	f.calls["revoke"]++
	key, ok := f.keys[keyID]
	if !ok || key.OrganizationID != orgID || key.AgentID != agentID {
		return ErrAgentKeyNotFound
	}
	key.RevokedAt = &revokedAt
	return nil
}

func (f *agentKeyStoreFake) ListAgentKeys(_ context.Context, orgID, agentID string) ([]*AgentKey, error) {
	f.calls["list"]++
	out := make([]*AgentKey, 0)
	for _, key := range f.keys {
		if key.OrganizationID == orgID && key.AgentID == agentID {
			k := *key
			out = append(out, &k)
		}
	}
	return out, nil
}

func (f *agentKeyStoreFake) TouchAgentKeyLastUsed(_ context.Context, id string, usedAt time.Time) error {
	f.calls["touch"]++
	if key, ok := f.keys[id]; ok {
		key.LastUsedAt = &usedAt
	}
	return nil
}

// combinedStoreFake implements BOTH Store (user keys) and AgentKeyStore, the
// shape the Postgres store presents; NewServiceWithStore must adopt the
// agent-key surface automatically.
type combinedStoreFake struct {
	Store
	agentKeys *agentKeyStoreFake
}

// --- service: create / prefix scheme / plaintext once -----------------------

func TestCreateAgentKeyPlaintextOnceAndPrefixScheme(t *testing.T) {
	svc := NewService()
	key, plaintext, err := svc.CreateAgentKey(context.Background(), "org-1", "agent-1", AgentKeyCreateRequest{Name: "ci-key"})
	if err != nil {
		t.Fatalf("CreateAgentKey: %v", err)
	}
	// Plaintext mirrors the user-key scheme: "ag_" + 64 hex chars (32 bytes).
	if !strings.HasPrefix(plaintext, AgentKeySecretPrefix) {
		t.Fatalf("plaintext should use the %q namespace, got %q", AgentKeySecretPrefix, plaintext)
	}
	if len(plaintext) != len(AgentKeySecretPrefix)+2*32 {
		t.Fatalf("plaintext body should be 32 bytes of hex, got len=%d", len(plaintext))
	}
	// Display prefix is the first 12 chars, scheme-consistent with user keys.
	if key.KeyPrefix != plaintext[:12] || len(key.KeyPrefix) != 12 {
		t.Fatalf("display prefix should be the first 12 chars, got %q (secret %q)", key.KeyPrefix, plaintext)
	}
	// The plaintext is NOT stored on the struct and not recoverable anywhere.
	if key.KeyHash == "" || key.KeyHash == plaintext {
		t.Fatal("key must carry only the SHA-256 hash, never the plaintext")
	}
	if got, want := key.KeyHash, hashAPIKey(plaintext); got != want {
		t.Fatalf("hash mismatch: got %s want %s", got, want)
	}
	if _, ok := svc.ValidateAgentKeyCtx(context.Background(), plaintext); !ok {
		t.Fatal("the returned plaintext must authenticate exactly once-minted")
	}
	// Metadata (list/get) must not carry any plaintext material.
	list, err := svc.ListAgentKeys(context.Background(), "org-1", "agent-1")
	if err != nil || len(list) != 1 {
		t.Fatalf("ListAgentKeys: %v (%d keys)", err, len(list))
	}
}

func TestCreateAgentKeyValidation(t *testing.T) {
	svc := NewService()
	future := time.Now().UTC().Add(time.Hour)
	cases := []struct {
		name string
		req  AgentKeyCreateRequest
	}{
		{"blank org", AgentKeyCreateRequest{Name: "k"}},
		{"blank agent", AgentKeyCreateRequest{Name: "k"}},
		{"blank name", AgentKeyCreateRequest{Name: "   "}},
		{"past expiry", AgentKeyCreateRequest{Name: "k", ExpiresAt: &[]time.Time{time.Now().UTC().Add(-time.Minute)}[0]}},
		{"blank allow-list entry", AgentKeyCreateRequest{Name: "k", ToolAllowList: []string{"calculator", "   "}}},
	}
	for _, tc := range cases {
		orgID, agentID := "org-1", "agent-1"
		switch tc.name {
		case "blank org":
			orgID = ""
		case "blank agent":
			agentID = ""
		}
		if _, _, err := svc.CreateAgentKey(context.Background(), orgID, agentID, tc.req); err == nil {
			t.Fatalf("%s: expected validation error", tc.name)
		}
	}
	_ = future
}

func TestCreateAgentKeyAllowListSemantics(t *testing.T) {
	svc := NewService()
	ctx := context.Background()

	// nil allow-list = DISABLED (all tools allowed).
	nilKey, _, err := svc.CreateAgentKey(ctx, "org-1", "agent-1", AgentKeyCreateRequest{Name: "nil"})
	if err != nil {
		t.Fatalf("nil allow-list create: %v", err)
	}
	if nilKey.ToolAllowList != nil {
		t.Fatalf("absent allow-list must stay nil, got %#v", nilKey.ToolAllowList)
	}
	// empty allow-list = ENABLED and empty (no tools).
	emptyKey, _, err := svc.CreateAgentKey(ctx, "org-1", "agent-1", AgentKeyCreateRequest{Name: "empty", ToolAllowList: []string{}})
	if err != nil {
		t.Fatalf("empty allow-list create: %v", err)
	}
	if emptyKey.ToolAllowList == nil || len(emptyKey.ToolAllowList) != 0 {
		t.Fatalf("empty allow-list must be non-nil and zero-length, got %#v", emptyKey.ToolAllowList)
	}
	// entries are trimmed and de-duplicated preserving order.
	namedKey, _, err := svc.CreateAgentKey(ctx, "org-1", "agent-1", AgentKeyCreateRequest{
		Name:          "named",
		ToolAllowList: []string{" calculator ", "http_request", "calculator"},
	})
	if err != nil {
		t.Fatalf("named allow-list create: %v", err)
	}
	if len(namedKey.ToolAllowList) != 2 || namedKey.ToolAllowList[0] != "calculator" || namedKey.ToolAllowList[1] != "http_request" {
		t.Fatalf("allow-list normalization wrong: %#v", namedKey.ToolAllowList)
	}
}

func TestCreateAgentKeyAgentMembership(t *testing.T) {
	ctx := context.Background()
	agentsInOrg := map[string]string{"agent-1": "org-1", "agent-2": "org-2"}
	checker := agentCheckerFake(func(_ context.Context, orgID, agentID string) (*agents.Agent, error) {
		if agentsInOrg[agentID] == orgID {
			return &agents.Agent{ID: agentID, OrganizationID: orgID}, nil
		}
		return nil, agents.ErrAgentNotFound
	})

	// No checker wired (legacy path): creation proceeds without the check.
	if _, _, err := NewService().CreateAgentKey(ctx, "org-1", "agent-9", AgentKeyCreateRequest{Name: "k"}); err != nil {
		t.Fatalf("nil checker must disable the membership check, got %v", err)
	}

	svc := NewService()
	svc.SetAgentChecker(checker)
	if _, _, err := svc.CreateAgentKey(ctx, "org-1", "agent-1", AgentKeyCreateRequest{Name: "k"}); err != nil {
		t.Fatalf("same-org agent should pass: %v", err)
	}
	if _, _, err := svc.CreateAgentKey(ctx, "org-1", "agent-2", AgentKeyCreateRequest{Name: "k"}); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("cross-org agent must be ErrAgentNotFound, got %v", err)
	}
	if _, _, err := svc.CreateAgentKey(ctx, "org-1", "agent-unknown", AgentKeyCreateRequest{Name: "k"}); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("unknown agent must be ErrAgentNotFound, got %v", err)
	}
	if err := svc.EnsureAgentInOrg(ctx, "org-2", "agent-2"); err != nil {
		t.Fatalf("EnsureAgentInOrg same-org: %v", err)
	}
	if err := svc.EnsureAgentInOrg(ctx, "org-2", "agent-1"); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("EnsureAgentInOrg cross-org: %v", err)
	}
}

// --- service: update / list / revoke ----------------------------------------

func agentKeyFixture(t *testing.T) (*Service, *AgentKey, string) {
	t.Helper()
	svc := NewService()
	key, plaintext, err := svc.CreateAgentKey(context.Background(), "org-1", "agent-1", AgentKeyCreateRequest{Name: "original"})
	if err != nil {
		t.Fatalf("fixture create: %v", err)
	}
	return svc, key, plaintext
}

func TestAgentKeyUpdateLifecycle(t *testing.T) {
	svc, key, _ := agentKeyFixture(t)
	ctx := context.Background()

	// name-only patch leaves everything else untouched
	newName := "renamed"
	updated, err := svc.UpdateAgentKey(ctx, "org-1", "agent-1", key.ID, AgentKeyUpdate{Name: &newName})
	if err != nil {
		t.Fatalf("name patch: %v", err)
	}
	if updated.Name != "renamed" || updated.ToolAllowList != nil || updated.ExpiresAt != nil {
		t.Fatalf("name patch changed too much: %#v", updated)
	}

	// allow-list patch: named → empty → disabled (tri-state)
	named := []string{"calculator"}
	if updated, err = svc.UpdateAgentKey(ctx, "org-1", "agent-1", key.ID, AgentKeyUpdate{ToolAllowList: &named}); err != nil || len(updated.ToolAllowList) != 1 {
		t.Fatalf("named patch: %v (%#v)", err, updated.ToolAllowList)
	}
	empty := []string{}
	if updated, err = svc.UpdateAgentKey(ctx, "org-1", "agent-1", key.ID, AgentKeyUpdate{ToolAllowList: &empty}); err != nil || updated.ToolAllowList == nil || len(updated.ToolAllowList) != 0 {
		t.Fatalf("empty patch must keep non-nil empty: %v (%#v)", err, updated.ToolAllowList)
	}
	disabled := []string(nil)
	if updated, err = svc.UpdateAgentKey(ctx, "org-1", "agent-1", key.ID, AgentKeyUpdate{ToolAllowList: &disabled}); err != nil || updated.ToolAllowList != nil {
		t.Fatalf("null patch must disable the allow-list: %v (%#v)", err, updated.ToolAllowList)
	}

	// expiry patch: set then clear
	future := time.Now().UTC().Add(24 * time.Hour)
	if updated, err = svc.UpdateAgentKey(ctx, "org-1", "agent-1", key.ID, AgentKeyUpdate{ExpiresAt: &future}); err != nil || updated.ExpiresAt == nil {
		t.Fatalf("expiry patch: %v", err)
	}
	cleared := time.Time{}
	if updated, err = svc.UpdateAgentKey(ctx, "org-1", "agent-1", key.ID, AgentKeyUpdate{ExpiresAt: &cleared}); err != nil || updated.ExpiresAt != nil {
		t.Fatalf("expiry clear: %v (%v)", err, updated.ExpiresAt)
	}

	// validation: blank name, past expiry, blank allow-list entry
	if _, err := svc.UpdateAgentKey(ctx, "org-1", "agent-1", key.ID, AgentKeyUpdate{Name: &[]string{"  "}[0]}); err == nil {
		t.Fatal("blank name patch must fail")
	}
	past := time.Now().UTC().Add(-time.Hour)
	if _, err := svc.UpdateAgentKey(ctx, "org-1", "agent-1", key.ID, AgentKeyUpdate{ExpiresAt: &past}); err == nil {
		t.Fatal("past expiry patch must fail")
	}
	badList := []string{" "}
	if _, err := svc.UpdateAgentKey(ctx, "org-1", "agent-1", key.ID, AgentKeyUpdate{ToolAllowList: &badList}); err == nil {
		t.Fatal("blank allow-list entry patch must fail")
	}

	// 404 semantics: wrong org, wrong agent, unknown id
	if _, err := svc.UpdateAgentKey(ctx, "org-2", "agent-1", key.ID, AgentKeyUpdate{}); !errors.Is(err, ErrAgentKeyNotFound) {
		t.Fatalf("cross-org update must be ErrAgentKeyNotFound, got %v", err)
	}
	if _, err := svc.UpdateAgentKey(ctx, "org-1", "agent-2", key.ID, AgentKeyUpdate{}); !errors.Is(err, ErrAgentKeyNotFound) {
		t.Fatalf("cross-agent update must be ErrAgentKeyNotFound, got %v", err)
	}
	if _, err := svc.UpdateAgentKey(ctx, "org-1", "agent-1", "missing", AgentKeyUpdate{}); !errors.Is(err, ErrAgentKeyNotFound) {
		t.Fatalf("unknown key update must be ErrAgentKeyNotFound, got %v", err)
	}
}

func TestAgentKeyListScopedByOrgAndAgent(t *testing.T) {
	svc := NewService()
	ctx := context.Background()
	if _, _, err := svc.CreateAgentKey(ctx, "org-1", "agent-1", AgentKeyCreateRequest{Name: "a"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, _, err := svc.CreateAgentKey(ctx, "org-1", "agent-1", AgentKeyCreateRequest{Name: "b"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.CreateAgentKey(ctx, "org-1", "agent-2", AgentKeyCreateRequest{Name: "other-agent"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.CreateAgentKey(ctx, "org-2", "agent-1", AgentKeyCreateRequest{Name: "other-org"}); err != nil {
		t.Fatal(err)
	}
	own, err := svc.ListAgentKeys(ctx, "org-1", "agent-1")
	if err != nil {
		t.Fatalf("ListAgentKeys: %v", err)
	}
	if len(own) != 2 {
		t.Fatalf("expected 2 keys for (org-1, agent-1), got %d", len(own))
	}
	// newest first
	if own[0].Name != "b" || own[1].Name != "a" {
		t.Fatalf("listing must be newest first, got [%s %s]", own[0].Name, own[1].Name)
	}
	if n, err := svc.ListAgentKeys(ctx, "org-1", "agent-2"); err != nil || len(n) != 1 {
		t.Fatalf("agent-2 list: %v (%d)", err, len(n))
	}
	if n, err := svc.ListAgentKeys(ctx, "org-2", "agent-1"); err != nil || len(n) != 1 {
		t.Fatalf("org-2 list: %v (%d)", err, len(n))
	}
	if n, err := svc.ListAgentKeys(ctx, "org-1", "agent-unknown"); err != nil || len(n) != 0 {
		t.Fatalf("unknown agent list: %v (%d)", err, len(n))
	}
}

func TestAgentKeyRevokeIdempotentAndValidate(t *testing.T) {
	svc, key, plaintext := agentKeyFixture(t)
	ctx := context.Background()

	if _, ok := svc.ValidateAgentKeyCtx(ctx, plaintext); !ok {
		t.Fatal("fresh key must authenticate")
	}
	if err := svc.RevokeAgentKey(ctx, "org-1", "agent-1", key.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	// idempotent re-revoke (user-key convention)
	if err := svc.RevokeAgentKey(ctx, "org-1", "agent-1", key.ID); err != nil {
		t.Fatalf("re-revoke must be idempotent, got %v", err)
	}
	if _, ok := svc.ValidateAgentKeyCtx(ctx, plaintext); ok {
		t.Fatal("revoked key must not authenticate")
	}
	if err := svc.RevokeAgentKey(ctx, "org-2", "agent-1", key.ID); !errors.Is(err, ErrAgentKeyNotFound) {
		t.Fatalf("cross-org revoke must be 404, got %v", err)
	}
	if err := svc.RevokeAgentKey(ctx, "org-1", "agent-2", key.ID); !errors.Is(err, ErrAgentKeyNotFound) {
		t.Fatalf("cross-agent revoke must be 404, got %v", err)
	}
}

// --- service: expiry honored at auth time -----------------------------------

func TestAgentKeyExpiryHonoredAtAuthTime(t *testing.T) {
	svc := NewService()
	ctx := context.Background()

	live := time.Now().UTC().Add(time.Hour)
	liveKey, livePlaintext, err := svc.CreateAgentKey(ctx, "org-1", "agent-1", AgentKeyCreateRequest{Name: "live", ExpiresAt: &live})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := svc.ValidateAgentKeyCtx(ctx, livePlaintext); !ok {
		t.Fatal("unexpired key must authenticate")
	}
	if _, ok := svc.ValidateAgentKeyCtx(ctx, "ag_not_a_real_key_but_long_enough_0123456789abcdef0123456789abcdef"); ok {
		t.Fatal("unknown key must not authenticate")
	}

	expired := time.Now().UTC().Add(-time.Second)
	if _, _, err := svc.CreateAgentKey(ctx, "org-1", "agent-1", AgentKeyCreateRequest{Name: "past", ExpiresAt: &expired}); err == nil {
		t.Fatal("creating a key with a past expiry must fail")
	}
	// Expiry passing WHILE the key exists is enforced at auth time.
	stored := svc.agentKeysInMemory()[liveKey.ID]
	past := time.Now().UTC().Add(-time.Minute)
	stored.ExpiresAt = &past
	if _, ok := svc.ValidateAgentKeyCtx(ctx, livePlaintext); ok {
		t.Fatal("expired key must not authenticate at auth time")
	}
}

func TestAgentKeyLastUsedStamp(t *testing.T) {
	svc, _, plaintext := agentKeyFixture(t)
	ctx := context.Background()
	if _, ok := svc.ValidateAgentKeyCtx(ctx, plaintext); !ok {
		t.Fatal("validate should succeed")
	}
	key := svc.agentKeysInMemory()[keyIDByName(t, svc, "original")]
	if key.LastUsedAt == nil {
		t.Fatal("successful auth should stamp LastUsedAt (NHI hygiene)")
	}
}

func keyIDByName(t *testing.T, svc *Service, name string) string {
	t.Helper()
	for id, key := range svc.agentKeysInMemory() {
		if key.Name == name {
			return id
		}
	}
	t.Fatalf("key %q not found", name)
	return ""
}

// --- service: dual-mode store routing ----------------------------------------

func TestAgentKeyStoreBackedRouting(t *testing.T) {
	ctx := context.Background()
	store := newAgentKeyStoreFake()
	svc := NewService()
	svc.SetAgentStore(store)

	key, plaintext, err := svc.CreateAgentKey(ctx, "org-1", "agent-1", AgentKeyCreateRequest{Name: "durable"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if store.calls["create"] != 1 {
		t.Fatalf("create must persist through the store, calls=%v", store.calls)
	}
	if _, ok := svc.ValidateAgentKeyCtx(ctx, plaintext); !ok {
		t.Fatal("hash lookup must go through the store")
	}
	if store.calls["getByHash"] != 1 {
		t.Fatalf("auth-time lookup must consult the store, calls=%v", store.calls)
	}
	if store.calls["touch"] != 1 {
		t.Fatalf("auth-time usage stamp must consult the store, calls=%v", store.calls)
	}
	// store is the source of truth for id-scoped reads
	if _, err := svc.GetAgentKey(ctx, "org-1", "agent-1", key.ID); err != nil {
		t.Fatalf("GetAgentKey via store: %v", err)
	}
	if store.calls["get"] != 1 {
		t.Fatalf("GetAgentKey must consult the store, calls=%v", store.calls)
	}
	// unknown/foreign ids surface 404 from the store too
	if _, err := svc.GetAgentKey(ctx, "org-2", "agent-1", key.ID); !errors.Is(err, ErrAgentKeyNotFound) {
		t.Fatalf("cross-org GetAgentKey: %v", err)
	}
	// revocation routes through the store and invalidates auth
	if err := svc.RevokeAgentKey(ctx, "org-1", "agent-1", key.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if store.calls["revoke"] != 1 {
		t.Fatal("revoke must persist through the store")
	}
	if _, ok := svc.ValidateAgentKeyCtx(ctx, plaintext); ok {
		t.Fatal("store-revoked key must not authenticate")
	}
}

func TestAgentKeyStoreAdoptedFromUserStore(t *testing.T) {
	agentStore := newAgentKeyStoreFake()
	// A store implementing BOTH interfaces (the pgStore shape) is adopted
	// automatically by NewServiceWithStore — no extra wiring call needed.
	combined := &combinedStoreFake{Store: nil, agentKeys: agentStore}
	// combinedStoreFake embeds the user Store interface (nil): the type
	// assertion in NewServiceWithStore must pick up AgentKeyStore.
	var userStore Store = struct {
		Store
		*agentKeyStoreFake
	}{Store: nil, agentKeyStoreFake: agentStore}
	_ = combined
	svc := NewServiceWithStore(userStore)
	if svc.agentStore == nil {
		t.Fatal("NewServiceWithStore must adopt a store that also implements AgentKeyStore")
	}
	// A user-key-only store leaves the agent surface in-memory.
	plain := NewServiceWithStore(nil)
	if plain.agentStore != nil {
		t.Fatal("nil store must not wire the agent store")
	}
}

// --- the seam: AuthorizeAgentKeyTool -----------------------------------------

func TestAuthorizeAgentKeyTool(t *testing.T) {
	cases := []struct {
		name    string
		claims  *AgentKeyClaims
		tool    string
		allowed bool
	}{
		{"nil claims (human/JWT/user-key caller)", nil, "anything", true},
		{"nil allow-list = all tools", &AgentKeyClaims{KeyID: "k", AgentID: "a", ToolAllowList: nil}, "anything", true},
		{"empty allow-list = no tools", &AgentKeyClaims{KeyID: "k", AgentID: "a", ToolAllowList: []string{}}, "calculator", false},
		{"named tool allowed", &AgentKeyClaims{KeyID: "k", AgentID: "a", ToolAllowList: []string{"calculator"}}, "calculator", true},
		{"non-listed tool denied", &AgentKeyClaims{KeyID: "k", AgentID: "a", ToolAllowList: []string{"calculator"}}, "http_request", false},
	}
	for _, tc := range cases {
		err := AuthorizeAgentKeyTool(tc.claims, tc.tool)
		if tc.allowed && err != nil {
			t.Fatalf("%s: expected allow, got %v", tc.name, err)
		}
		if !tc.allowed {
			var denied *AgentKeyToolError
			if !errors.As(err, &denied) {
				t.Fatalf("%s: expected AgentKeyToolError, got %v", tc.name, err)
			}
			if !strings.Contains(err.Error(), AgentKeyToolDeniedCode) {
				t.Fatalf("%s: error must carry the machine-readable code %q, got %q", tc.name, AgentKeyToolDeniedCode, err.Error())
			}
			if denied.Tool != tc.tool || denied.KeyID != tc.claims.KeyID || denied.AgentID != tc.claims.AgentID {
				t.Fatalf("%s: denial identity wrong: %#v", tc.name, denied)
			}
		}
	}
}

// --- claims context + payload round-trip --------------------------------------

func TestAgentKeyClaimsContextRoundTrip(t *testing.T) {
	ctx := context.Background()
	if claims := AgentKeyClaimsFromContext(ctx); claims != nil {
		t.Fatal("absent claims must read as nil (non-agent-key caller)")
	}
	claims := &AgentKeyClaims{KeyID: "k1", AgentID: "a1", OrganizationID: "org-1", ToolAllowList: []string{"calculator"}}
	ctx = WithAgentKeyClaims(ctx, claims)
	got := AgentKeyClaimsFromContext(ctx)
	if got != claims {
		t.Fatalf("context round-trip mismatch: %#v", got)
	}
	// nil claims stamp is a no-op and still reads nil.
	if WithAgentKeyClaims(ctx, nil) == nil {
		t.Fatal("WithAgentKeyClaims(nil) must not nil the context")
	}
	if AgentKeyClaimsFromContext(context.WithValue(ctx, agentKeyClaimsContextKey{}, (*AgentKeyClaims)(nil))) != nil {
		t.Fatal("nil-typed claims must read as nil")
	}
}

func TestAgentKeyClaimsPayloadRoundTrip(t *testing.T) {
	// nil list ↔ JSON null ↔ nil (disabled); empty list ↔ [] ↔ empty (no tools)
	claims := &AgentKeyClaims{KeyID: "k1", AgentID: "a1", OrganizationID: "org-1", ToolAllowList: nil}
	blob, err := json.Marshal(map[string]any{"agent_key": AgentKeyClaimsPayload(claims)})
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(blob, &payload); err != nil {
		t.Fatal(err)
	}
	got := AgentKeyClaimsFromPayload(payload)
	if got == nil || got.KeyID != "k1" || got.AgentID != "a1" || got.OrganizationID != "org-1" {
		t.Fatalf("nil-list payload round-trip: %#v", got)
	}
	if got.ToolAllowList != nil {
		t.Fatalf("nil allow-list must survive the queue hop as nil, got %#v", got.ToolAllowList)
	}

	claims.ToolAllowList = []string{}
	blob, _ = json.Marshal(map[string]any{"agent_key": AgentKeyClaimsPayload(claims)})
	_ = json.Unmarshal(blob, &payload)
	got = AgentKeyClaimsFromPayload(payload)
	if got == nil || got.ToolAllowList == nil || len(got.ToolAllowList) != 0 {
		t.Fatalf("empty allow-list must survive the queue hop non-nil, got %#v", got)
	}

	claims.ToolAllowList = []string{"calculator", "http_request"}
	blob, _ = json.Marshal(map[string]any{"agent_key": AgentKeyClaimsPayload(claims)})
	_ = json.Unmarshal(blob, &payload)
	got = AgentKeyClaimsFromPayload(payload)
	if got == nil || len(got.ToolAllowList) != 2 || got.ToolAllowList[0] != "calculator" {
		t.Fatalf("named allow-list round-trip: %#v", got)
	}
}

func TestAgentKeyClaimsFromPayloadEdgeCases(t *testing.T) {
	if AgentKeyClaimsFromPayload(nil) != nil {
		t.Fatal("nil payload must read as nil claims")
	}
	if AgentKeyClaimsFromPayload(map[string]any{}) != nil {
		t.Fatal("payload without agent_key must read as nil claims")
	}
	if AgentKeyClaimsFromPayload(map[string]any{"agent_key": map[string]any{}}) != nil {
		t.Fatal("agent_key without an id must read as nil claims")
	}
}

// --- Postgres store (sqlmock): SQL contract ----------------------------------

const agentKeySelectColumns = `id, organization_id, agent_id, name, prefix, key_hash, tool_allow_list, expires_at, revoked_at, last_used_at, created_at`

func agentKeyRow(id string, allowList driver.Value, expiresAt, revokedAt, lastUsedAt driver.Value) []driver.Value {
	return []driver.Value{id, "org-1", "agent-1", "name-" + id, "ag_000000000001", "hash-" + id,
		allowList, expiresAt, revokedAt, lastUsedAt, time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
}

func TestAgentKeyPgStoreCreateAndGetByHash(t *testing.T) {
	db, mock, cleanup := newAgentKeyMockDB(t)
	defer cleanup()
	// NewPostgresStore returns the user-key Store interface; the same handle
	// implements AgentKeyStore (exactly what NewServiceWithStore asserts on).
	store, ok := NewPostgresStore(db).(AgentKeyStore)
	if !ok {
		t.Fatal("pgStore must implement AgentKeyStore")
	}

	expires := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	key := &AgentKey{
		ID: "key-1", OrganizationID: "org-1", AgentID: "agent-1", Name: "durable",
		KeyPrefix: "ag_000000000001", KeyHash: "hash-1",
		ToolAllowList: []string{"calculator"}, ExpiresAt: &expires,
		CreatedAt: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
	}
	mock.ExpectExec(`INSERT INTO agent_api_keys \(id, organization_id, agent_id, name, prefix, key_hash, tool_allow_list, expires_at, created_at\) VALUES \(\$1, \$2, \$3, \$4, \$5, \$6, \$7, \$8, \$9\)`).
		WithArgs("key-1", "org-1", "agent-1", "durable", "ag_000000000001", "hash-1", `["calculator"]`, expires, key.CreatedAt).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := store.CreateAgentKey(context.Background(), key); err != nil {
		t.Fatalf("CreateAgentKey: %v", err)
	}

	// Authentication lookup: NULL allow-list decodes to nil (disabled).
	mock.ExpectQuery(`SELECT ` + agentKeySelectColumns + ` FROM agent_api_keys WHERE key_hash = \$1 AND revoked_at IS NULL`).
		WithArgs("hash-1").
		WillReturnRows(agentKeyRows(agentKeyRow("key-1", nil, nil, nil, nil)))
	got, err := store.GetAgentKeyByHash(context.Background(), "hash-1")
	if err != nil {
		t.Fatalf("GetAgentKeyByHash: %v", err)
	}
	if got.ToolAllowList != nil {
		t.Fatalf("SQL NULL allow-list must decode to nil, got %#v", got.ToolAllowList)
	}
	if got.Revoked() || got.ExpiresAt != nil {
		t.Fatalf("lifecycle fields wrong: %#v", got)
	}

	// '[]' decodes to empty non-nil (no tools); revoked/last_used present.
	mock.ExpectQuery(`SELECT ` + agentKeySelectColumns + ` FROM agent_api_keys WHERE key_hash = \$1 AND revoked_at IS NULL`).
		WithArgs("hash-2").
		WillReturnRows(agentKeyRows(agentKeyRow("key-2", "[]", nil, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC))))
	got2, err := store.GetAgentKeyByHash(context.Background(), "hash-2")
	if err != nil {
		t.Fatalf("GetAgentKeyByHash(2): %v", err)
	}
	if got2.ToolAllowList == nil || len(got2.ToolAllowList) != 0 {
		t.Fatalf("'[]' must decode to empty non-nil, got %#v", got2.ToolAllowList)
	}
	if !got2.Revoked() {
		t.Fatal("revoked_at must decode to Revoked()")
	}

	// No rows = ErrAgentKeyNotFound (404 semantics).
	mock.ExpectQuery(`SELECT ` + agentKeySelectColumns + ` FROM agent_api_keys WHERE key_hash = \$1 AND revoked_at IS NULL`).
		WithArgs("missing").
		WillReturnError(sql.ErrNoRows)
	if _, err := store.GetAgentKeyByHash(context.Background(), "missing"); !errors.Is(err, ErrAgentKeyNotFound) {
		t.Fatalf("missing key must be ErrAgentKeyNotFound, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

func TestAgentKeyPgStoreUpdateRevokeListTouch(t *testing.T) {
	db, mock, cleanup := newAgentKeyMockDB(t)
	defer cleanup()
	store, ok := NewPostgresStore(db).(AgentKeyStore)
	if !ok {
		t.Fatal("pgStore must implement AgentKeyStore")
	}
	ctx := context.Background()

	// Update with an empty allow-list: '[]' must reach the column (no tools).
	empty := []string{}
	key := &AgentKey{ID: "key-1", OrganizationID: "org-1", AgentID: "agent-1", Name: "patched",
		ToolAllowList: empty, KeyHash: "hash-1"}
	mock.ExpectExec(`UPDATE agent_api_keys SET name = \$1, tool_allow_list = \$2, expires_at = \$3 WHERE id = \$4 AND organization_id = \$5 AND agent_id = \$6`).
		WithArgs("patched", `[]`, nil, "key-1", "org-1", "agent-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.UpdateAgentKey(ctx, key); err != nil {
		t.Fatalf("UpdateAgentKey: %v", err)
	}

	// Update hitting zero rows = tenant guard rejection → 404.
	mock.ExpectExec(`UPDATE agent_api_keys SET name = \$1, tool_allow_list = \$2, expires_at = \$3 WHERE id = \$4 AND organization_id = \$5 AND agent_id = \$6`).
		WithArgs("patched", `[]`, nil, "key-1", "org-2", "agent-1").
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := store.UpdateAgentKey(ctx, &AgentKey{ID: "key-1", OrganizationID: "org-2", AgentID: "agent-1", Name: "patched", ToolAllowList: empty}); !errors.Is(err, ErrAgentKeyNotFound) {
		t.Fatalf("cross-tenant update must be ErrAgentKeyNotFound, got %v", err)
	}

	// Revoke, then an idempotent re-revoke (zero rows but the row exists).
	now := time.Now().UTC()
	mock.ExpectExec(`UPDATE agent_api_keys SET revoked_at = \$1 WHERE id = \$2 AND organization_id = \$3 AND agent_id = \$4 AND revoked_at IS NULL`).
		WithArgs(now, "key-1", "org-1", "agent-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.RevokeAgentKey(ctx, "org-1", "agent-1", "key-1", now); err != nil {
		t.Fatalf("RevokeAgentKey: %v", err)
	}
	mock.ExpectExec(`UPDATE agent_api_keys SET revoked_at = \$1 WHERE id = \$2 AND organization_id = \$3 AND agent_id = \$4 AND revoked_at IS NULL`).
		WithArgs(now, "key-1", "org-1", "agent-1").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT EXISTS \(SELECT 1 FROM agent_api_keys WHERE id = \$1 AND organization_id = \$2 AND agent_id = \$3\)`).
		WithArgs("key-1", "org-1", "agent-1").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	if err := store.RevokeAgentKey(ctx, "org-1", "agent-1", "key-1", now); err != nil {
		t.Fatalf("idempotent re-revoke must succeed, got %v", err)
	}
	// A foreign key (row truly absent) is a 404.
	mock.ExpectExec(`UPDATE agent_api_keys SET revoked_at = \$1 WHERE id = \$2 AND organization_id = \$3 AND agent_id = \$4 AND revoked_at IS NULL`).
		WithArgs(now, "key-x", "org-1", "agent-1").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT EXISTS \(SELECT 1 FROM agent_api_keys WHERE id = \$1 AND organization_id = \$2 AND agent_id = \$3\)`).
		WithArgs("key-x", "org-1", "agent-1").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	if err := store.RevokeAgentKey(ctx, "org-1", "agent-1", "key-x", now); !errors.Is(err, ErrAgentKeyNotFound) {
		t.Fatalf("foreign key revoke must be ErrAgentKeyNotFound, got %v", err)
	}

	// Listing is (org, agent)-scoped.
	mock.ExpectQuery(`SELECT `+agentKeySelectColumns+` FROM agent_api_keys WHERE organization_id = \$1 AND agent_id = \$2 ORDER BY created_at DESC`).
		WithArgs("org-1", "agent-1").
		WillReturnRows(agentKeyRows(agentKeyRow("key-1", `["calculator"]`, nil, nil, nil)))
	list, err := store.ListAgentKeys(ctx, "org-1", "agent-1")
	if err != nil || len(list) != 1 {
		t.Fatalf("ListAgentKeys: %v (%d)", err, len(list))
	}
	if len(list[0].ToolAllowList) != 1 || list[0].ToolAllowList[0] != "calculator" {
		t.Fatalf("allow-list decode wrong: %#v", list[0].ToolAllowList)
	}

	// Usage stamp.
	mock.ExpectExec(`UPDATE agent_api_keys SET last_used_at = \$1 WHERE id = \$2`).
		WithArgs(now, "key-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.TouchAgentKeyLastUsed(ctx, "key-1", now); err != nil {
		t.Fatalf("TouchAgentKeyLastUsed: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

func newAgentKeyMockDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	return db, mock, func() { _ = db.Close() }
}

func agentKeyRows(row []driver.Value) *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "organization_id", "agent_id", "name", "prefix", "key_hash",
		"tool_allow_list", "expires_at", "revoked_at", "last_used_at", "created_at",
	}).AddRow(row...)
}
