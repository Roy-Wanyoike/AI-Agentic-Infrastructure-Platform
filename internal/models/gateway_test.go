package models

// gateway_test.go — issue #139 coverage: virtual-key lifecycle (plaintext
// once, hashing, revoke, expiry), the allow-list nil/empty/listed contract,
// the spend ledger month boundaries (UTC), the Authorize denial boundaries
// (exactly-at-limit allowed, projected overspend denied, unlimited passes),
// GuardedProvider allow/record/deny paths against an httptest fake provider,
// and org isolation. Postgres store coverage lives in gateway_store_test.go.

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"agentos/internal/audit"
)

// --- fakes -----------------------------------------------------------------

// fakeGatewayAuditor records audit rows through the Auditor seam.
type fakeGatewayAuditor struct {
	mu      sync.Mutex
	entries []*audit.Entry
}

func (f *fakeGatewayAuditor) LogCtx(_ context.Context, actor, action, orgID, resource string, metadata map[string]any) (*audit.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, &audit.Entry{
		Actor: actor, Action: action, OrganizationID: orgID, Resource: resource, Metadata: metadata,
	})
	return f.entries[len(f.entries)-1], nil
}

func (f *fakeGatewayAuditor) actions() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]int{}
	for _, e := range f.entries {
		out[e.Action]++
	}
	return out
}

// gatewayStoreFake is an in-test GatewayStore recording call counts so the
// durable-mode routing is observable (mirrors agentKeyStoreFake).
type gatewayStoreFake struct {
	mu     sync.Mutex
	keys   map[string]*VirtualKey
	byHash map[string]*VirtualKey
	calls  map[string]int
	spend  map[string]float64 // orgID|keyID|month|model -> cents
}

func newGatewayStoreFake() *gatewayStoreFake {
	return &gatewayStoreFake{
		keys:   make(map[string]*VirtualKey),
		byHash: make(map[string]*VirtualKey),
		calls:  make(map[string]int),
		spend:  make(map[string]float64),
	}
}

func (f *gatewayStoreFake) CreateVirtualKey(_ context.Context, key *VirtualKey) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["create"]++
	k := *key
	f.keys[k.ID] = &k
	f.byHash[k.KeyHash] = &k
	return nil
}

func (f *gatewayStoreFake) GetVirtualKeyByHash(_ context.Context, hash string) (*VirtualKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["getByHash"]++
	if key, ok := f.byHash[hash]; ok {
		k := *key
		return &k, nil
	}
	return nil, ErrVirtualKeyNotFound
}

func (f *gatewayStoreFake) GetVirtualKey(_ context.Context, orgID, id string) (*VirtualKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["get"]++
	key, ok := f.keys[id]
	if !ok || key.OrganizationID != orgID {
		return nil, ErrVirtualKeyNotFound
	}
	k := *key
	return &k, nil
}

func (f *gatewayStoreFake) ListVirtualKeys(_ context.Context, orgID string) ([]*VirtualKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["list"]++
	out := make([]*VirtualKey, 0)
	for _, key := range f.keys {
		if key.OrganizationID == orgID {
			k := *key
			out = append(out, &k)
		}
	}
	return out, nil
}

func (f *gatewayStoreFake) UpdateVirtualKey(_ context.Context, key *VirtualKey) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["update"]++
	if _, ok := f.keys[key.ID]; !ok {
		return ErrVirtualKeyNotFound
	}
	k := *key
	f.keys[k.ID] = &k
	f.byHash[k.KeyHash] = &k
	return nil
}

func (f *gatewayStoreFake) RevokeVirtualKey(_ context.Context, orgID, id string, revokedAt time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["revoke"]++
	key, ok := f.keys[id]
	if !ok || key.OrganizationID != orgID {
		return ErrVirtualKeyNotFound
	}
	key.Status = VirtualKeyStatusRevoked
	t := revokedAt
	key.RevokedAt = &t
	return nil
}

func (f *gatewayStoreFake) RecordSpend(_ context.Context, orgID, keyID, model string, costCents float64, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["recordSpend"]++
	f.spend[orgID+"|"+keyID+"|"+monthKey(at)+"|"+model] += costCents
	return nil
}

func (f *gatewayStoreFake) SpendForMonth(_ context.Context, orgID, keyID, month string) (float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["spendForMonth"]++
	var total float64
	prefix := orgID + "|" + keyID + "|" + month + "|"
	for k, v := range f.spend {
		if strings.HasPrefix(k, prefix) {
			total += v
		}
	}
	return total, nil
}

// countingProvider wraps a Provider counting Complete calls.
type countingProvider struct {
	inner Provider
	calls int
}

func (p *countingProvider) Name() string { return "counting" }

func (p *countingProvider) Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error) {
	p.calls++
	return p.inner.Complete(ctx, req)
}

// newGatewayTestService returns an in-memory service + auditor fake.
func newGatewayTestService() (*GatewayService, *fakeGatewayAuditor) {
	svc := NewGatewayService()
	aud := &fakeGatewayAuditor{}
	svc.SetAuditor(aud)
	return svc, aud
}

// mustCreateKey is the test convenience creator (owner actor).
func mustCreateKey(t *testing.T, svc *GatewayService, orgID string, in VirtualKeyInput) (*VirtualKey, string) {
	t.Helper()
	key, plaintext, err := svc.CreateVirtualKey(context.Background(), orgID, "owner-1", in)
	if err != nil {
		t.Fatalf("CreateVirtualKey: %v", err)
	}
	return key, plaintext
}

// --- lifecycle --------------------------------------------------------------

func TestCreateVirtualKeyPlaintextOnceAndHashScheme(t *testing.T) {
	svc, _ := newGatewayTestService()
	key, plaintext, err := svc.CreateVirtualKey(context.Background(), "org-1", "user-1", VirtualKeyInput{Label: "ci key"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.HasPrefix(plaintext, "vk_") || len(plaintext) != len(VirtualKeyPrefix)+64 {
		t.Fatalf("plaintext shape wrong: %q (len %d)", plaintext, len(plaintext))
	}
	// Hashing mirrors the apikeys scheme: sha256 hex of the full plaintext.
	if key.KeyHash != "" {
		t.Fatalf("returned key must be redacted (no hash), got %q", key.KeyHash)
	}
	if key.KeyPrefix != plaintext[:12] {
		t.Fatalf("prefix mismatch: %q vs %q", key.KeyPrefix, plaintext[:12])
	}
	// The plaintext resolves exactly once through the auth path...
	resolved, rerr := svc.ResolveByPlaintext(context.Background(), plaintext)
	if rerr != nil {
		t.Fatalf("resolve: %v", rerr)
	}
	if resolved.ID != key.ID || resolved.KeyHash != hashVirtualKey(plaintext) {
		t.Fatalf("resolve mismatch: %+v", resolved)
	}
	if resolved.KeyHash == "" {
		t.Fatalf("internal resolve must keep the hash (auth path)")
	}
	// ...while management reads never expose hash or plaintext.
	got, gerr := svc.GetVirtualKey(context.Background(), "org-1", key.ID)
	if gerr != nil {
		t.Fatalf("get: %v", gerr)
	}
	if got.KeyHash != "" {
		t.Fatalf("management read must drop the hash: %+v", got)
	}
	list, lerr := svc.ListVirtualKeys(context.Background(), "org-1")
	if lerr != nil || len(list) != 1 {
		t.Fatalf("list: %v (%d)", lerr, len(list))
	}
	if list[0].KeyHash != "" || strings.Contains(plaintext, list[0].Label) {
		t.Fatalf("list response must be secret-free: %+v", list[0])
	}
	// Two keys never share a plaintext.
	_, plaintext2, _ := svc.CreateVirtualKey(context.Background(), "org-1", "user-1", VirtualKeyInput{Label: "second"})
	if plaintext == plaintext2 {
		t.Fatalf("two keys must not share a plaintext")
	}
}

func TestVirtualKeyRevokeLifecycleAndResolution(t *testing.T) {
	svc, aud := newGatewayTestService()
	key, plaintext := mustCreateKey(t, svc, "org-1", VirtualKeyInput{Label: "revoke me"})

	if err := svc.RevokeVirtualKey(context.Background(), "org-9", key.ID); !errors.Is(err, ErrVirtualKeyNotFound) {
		t.Fatalf("foreign revoke must be NotFound, got %v", err)
	}
	if err := svc.RevokeVirtualKey(context.Background(), "org-1", "missing"); !errors.Is(err, ErrVirtualKeyNotFound) {
		t.Fatalf("unknown revoke must be NotFound, got %v", err)
	}
	// Revocation is idempotent and flips status + resolution.
	if err := svc.RevokeVirtualKey(context.Background(), "org-1", key.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := svc.RevokeVirtualKey(context.Background(), "org-1", key.ID); err != nil {
		t.Fatalf("revoke must be idempotent: %v", err)
	}
	got, _ := svc.GetVirtualKey(context.Background(), "org-1", key.ID)
	if got.Status != VirtualKeyStatusRevoked || !got.Revoked() {
		t.Fatalf("status not revoked: %+v", got)
	}
	if _, err := svc.ResolveByPlaintext(context.Background(), plaintext); !errors.Is(err, ErrVirtualKeyNotFound) {
		t.Fatalf("revoked key must not resolve (no leak), got %v", err)
	}
	if _, err := svc.ResolveByPlaintext(context.Background(), "vk_wrong"); !errors.Is(err, ErrVirtualKeyNotFound) {
		t.Fatalf("unknown plaintext must be NotFound, got %v", err)
	}
	if _, err := svc.ResolveByPlaintext(context.Background(), ""); !errors.Is(err, ErrVirtualKeyNotFound) {
		t.Fatalf("empty plaintext must be NotFound, got %v", err)
	}
	if actions := aud.actions(); actions[AuditGatewayKeyCreated] != 1 || actions[AuditGatewayKeyRevoked] != 1 {
		t.Fatalf("lifecycle audit rows missing: %v", actions)
	}
}

func TestVirtualKeyExpirySemantics(t *testing.T) {
	svc, _ := newGatewayTestService()
	past := time.Now().UTC().Add(-time.Hour)
	future := time.Now().UTC().Add(time.Hour)
	key, plaintext := mustCreateKey(t, svc, "org-1", VirtualKeyInput{Label: "expiring", ExpiresAt: &past})
	if !key.Expired(time.Now().UTC()) {
		t.Fatalf("expired key must report Expired")
	}
	if _, err := svc.ResolveByPlaintext(context.Background(), plaintext); !errors.Is(err, ErrVirtualKeyNotFound) {
		t.Fatalf("expired key must not resolve, got %v", err)
	}
	if err := svc.Authorize(context.Background(), "org-1", key.ID, "gpt-4o", 0, time.Now()); !errors.Is(err, ErrVirtualKeyExpired) {
		t.Fatalf("expired authorize must be ErrVirtualKeyExpired, got %v", err)
	}

	key2, plaintext2 := mustCreateKey(t, svc, "org-1", VirtualKeyInput{Label: "live", ExpiresAt: &future})
	if key2.Expired(time.Now().UTC()) {
		t.Fatalf("future expiry must not report Expired")
	}
	if _, err := svc.ResolveByPlaintext(context.Background(), plaintext2); err != nil {
		t.Fatalf("unexpired key must resolve: %v", err)
	}

	// PATCH can clear the expiry (zero-time pointer) and restore resolution.
	if _, err := svc.UpdateVirtualKey(context.Background(), "org-1", key.ID, VirtualKeyUpdate{ExpiresAt: &time.Time{}}); err != nil {
		t.Fatalf("clear expiry: %v", err)
	}
	if _, err := svc.ResolveByPlaintext(context.Background(), plaintext); err != nil {
		t.Fatalf("cleared expiry must restore resolution: %v", err)
	}
}

// --- allow-list semantics ---------------------------------------------------

func TestVirtualKeyAllowListSemantics(t *testing.T) {
	ctx := context.Background()
	svc, _ := newGatewayTestService()
	now := time.Now().UTC()

	// nil = ALL models allowed.
	allKey, _ := mustCreateKey(t, svc, "org-1", VirtualKeyInput{Label: "all"})
	for _, model := range []string{"gpt-4o", "claude-x", "weird-vendor/model", ""} {
		if err := svc.Authorize(ctx, "org-1", allKey.ID, model, 0, now); err != nil {
			t.Fatalf("nil allow-list must admit %q: %v", model, err)
		}
	}

	// empty = NONE allowed.
	noneKey, _ := mustCreateKey(t, svc, "org-1", VirtualKeyInput{Label: "none", Models: []string{}})
	for _, model := range []string{"gpt-4o", "claude-x"} {
		err := svc.Authorize(ctx, "org-1", noneKey.ID, model, 0, now)
		var mna *ModelNotAllowedError
		if !errors.As(err, &mna) || !errors.Is(err, ErrModelNotAllowed) {
			t.Fatalf("empty allow-list must deny %q with typed error, got %v", model, err)
		}
		if mna.Model != model || mna.KeyID != noneKey.ID {
			t.Fatalf("denial details wrong: %+v", mna)
		}
	}

	// listed = enforced, case-insensitive, vendor/model suffix-aware.
	listedKey, _ := mustCreateKey(t, svc, "org-1", VirtualKeyInput{Label: "listed", Models: []string{"GPT-4o", "llama-3.1-8b-instruct"}})
	for _, model := range []string{"gpt-4o", "GPT-4O", "openai/gpt-4o", "llama-3.1-8b-instruct"} {
		if err := svc.Authorize(ctx, "org-1", listedKey.ID, model, 0, now); err != nil {
			t.Fatalf("listed %q must be admitted: %v", model, err)
		}
	}
	for _, model := range []string{"gpt-4o-mini", "claude-3", "other/gpt-4o-mini"} {
		if err := svc.Authorize(ctx, "org-1", listedKey.ID, model, 0, now); !errors.Is(err, ErrModelNotAllowed) {
			t.Fatalf("unlisted %q must be denied, got %v", model, err)
		}
	}

	// Create/update validation: empty entries rejected, dedupe stable.
	if _, _, err := svc.CreateVirtualKey(ctx, "org-1", "u", VirtualKeyInput{Label: "bad", Models: []string{"  "}}); !errors.Is(err, ErrInvalidVirtualKey) {
		t.Fatalf("blank allow-list entry must be invalid, got %v", err)
	}
	if _, _, err := svc.CreateVirtualKey(ctx, "org-1", "u", VirtualKeyInput{Label: "", MonthlyLimitCents: -1}); !errors.Is(err, ErrInvalidVirtualKey) {
		t.Fatalf("empty label / negative limit must be invalid, got %v", err)
	}
	upd, err := svc.UpdateVirtualKey(ctx, "org-1", listedKey.ID, VirtualKeyUpdate{Models: &[]string{"gpt-4o", "GPT-4O", "o3-mini"}})
	if err != nil || len(upd.Models) != 2 {
		t.Fatalf("dedupe expected 2 entries, got %v (%v)", upd.Models, err)
	}
}

// --- spend ledger -----------------------------------------------------------

func TestSpendLedgerMonthBoundariesUTC(t *testing.T) {
	ctx := context.Background()
	svc, _ := newGatewayTestService()
	key, _ := mustCreateKey(t, svc, "org-1", VirtualKeyInput{Label: "ledger"})

	janTail := time.Date(2024, 1, 31, 23, 59, 59, int(time.Second-time.Nanosecond), time.UTC)
	febHead := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)
	if err := svc.RecordSpend(ctx, "org-1", key.ID, "gpt-4o", 40, janTail); err != nil {
		t.Fatalf("record jan: %v", err)
	}
	if err := svc.RecordSpend(ctx, "org-1", key.ID, "gpt-4o", 5, febHead); err != nil {
		t.Fatalf("record feb: %v", err)
	}
	// A non-UTC timestamp that crosses into the next UTC month must bucket
	// by UTC (Jan 31 23:00 at UTC-5 == Feb 1 04:00 UTC).
	est5 := time.FixedZone("est5", -5*3600)
	if err := svc.RecordSpend(ctx, "org-1", key.ID, "gpt-4o-mini", 2.5, time.Date(2024, 1, 31, 23, 0, 0, 0, est5)); err != nil {
		t.Fatalf("record est: %v", err)
	}

	usedJan, limitJan, err := svc.Spend(ctx, "org-1", key.ID, janTail)
	if err != nil || limitJan != 0 || usedJan != 40 {
		t.Fatalf("jan spend = %v/%v err %v (want 40/0/unlimited)", usedJan, limitJan, err)
	}
	usedFeb, _, err := svc.Spend(ctx, "org-1", key.ID, febHead)
	if err != nil || math.Abs(usedFeb-7.5) > 1e-9 {
		t.Fatalf("feb spend = %v err %v (want 7.5 = 5 + 2.5 UTC-crossed)", usedFeb, err)
	}

	// Per-model attribution survives in the bucket coordinates (org-scoped).
	if err := svc.RecordSpend(ctx, "org-2", key.ID, "gpt-4o", 999, febHead); err != nil {
		t.Fatalf("record other org: %v", err)
	}
	if usedFeb, _ := svc.spendForMonth(ctx, "org-1", key.ID, monthKey(febHead)); math.Abs(usedFeb-7.5) > 1e-9 {
		t.Fatalf("org isolation broken in ledger: %v", usedFeb)
	}
	if err := svc.RecordSpend(ctx, "org-1", key.ID, "gpt-4o", -1, febHead); !errors.Is(err, ErrInvalidVirtualKey) {
		t.Fatalf("negative cost must be rejected, got %v", err)
	}
}

// --- Authorize boundaries ---------------------------------------------------

func TestAuthorizeDenialBoundaries(t *testing.T) {
	ctx := context.Background()
	svc, aud := newGatewayTestService()
	now := time.Date(2024, 3, 15, 12, 0, 0, 0, time.UTC)

	limited, _ := mustCreateKey(t, svc, "org-1", VirtualKeyInput{Label: "cap", MonthlyLimitCents: 100})
	if err := svc.RecordSpend(ctx, "org-1", limited.ID, "gpt-4o", 40, now); err != nil {
		t.Fatalf("seed spend: %v", err)
	}

	// Exactly-at-limit projects are allowed.
	if err := svc.Authorize(ctx, "org-1", limited.ID, "gpt-4o", 60, now); err != nil {
		t.Fatalf("projected == limit must pass: %v", err)
	}
	// Strictly-over-limit projections are denied, carrying details.
	err := svc.Authorize(ctx, "org-1", limited.ID, "gpt-4o", 60.01, now)
	var sle *SpendLimitError
	if !errors.As(err, &sle) || !errors.Is(err, ErrSpendLimitExceeded) {
		t.Fatalf("projected overspend must be typed denial, got %v", err)
	}
	if sle.LimitCents != 100 || sle.UsedCents != 40 || math.Abs(sle.ProjectedCents-100.01) > 1e-9 || sle.KeyID != limited.ID {
		t.Fatalf("spend denial details wrong: %+v", sle)
	}
	// Negative estimates clamp to 0 (never fake allowance headroom).
	if err := svc.Authorize(ctx, "org-1", limited.ID, "gpt-4o", -5, now); err != nil {
		t.Fatalf("negative estimate must clamp to 0: %v", err)
	}
	// At/over the limit with a fresh (0) estimate: exactly-at allowed.
	if err := svc.RecordSpend(ctx, "org-1", limited.ID, "gpt-4o", 60, now); err != nil {
		t.Fatalf("top-up spend: %v", err)
	}
	if err := svc.Authorize(ctx, "org-1", limited.ID, "gpt-4o", 0, now); err != nil {
		t.Fatalf("used == limit with 0 estimate must pass (boundary): %v", err)
	}
	if err := svc.Authorize(ctx, "org-1", limited.ID, "gpt-4o", 0.01, now); !errors.Is(err, ErrSpendLimitExceeded) {
		t.Fatalf("used == limit with positive estimate must deny: %v", err)
	}

	// Unlimited (0 sentinel) passes regardless of spend.
	unlimited, _ := mustCreateKey(t, svc, "org-1", VirtualKeyInput{Label: "unlimited"})
	if err := svc.RecordSpend(ctx, "org-1", unlimited.ID, "gpt-4o", 1e6, now); err != nil {
		t.Fatalf("seed unlimited spend: %v", err)
	}
	if err := svc.Authorize(ctx, "org-1", unlimited.ID, "gpt-4o", 1e6, now); err != nil {
		t.Fatalf("unlimited key must pass: %v", err)
	}

	// Unknown / foreign keys never authorize (no existence leak).
	for _, tc := range []struct{ org, id string }{
		{"org-9", limited.ID}, {"org-1", "missing"}, {"org-1", ""},
	} {
		if err := svc.Authorize(ctx, tc.org, tc.id, "gpt-4o", 0, now); !errors.Is(err, ErrVirtualKeyNotFound) {
			t.Fatalf("authorize(%s,%s) must be NotFound, got %v", tc.org, tc.id, err)
		}
	}

	// Denials audited: 2 x spend_denied for the capped key.
	if actions := aud.actions(); actions[AuditGatewaySpendDenied] != 2 {
		t.Fatalf("spend denial audit rows wrong: %v", actions)
	}
}

func TestAuthorizeLedgerFailureFailsClosed(t *testing.T) {
	ctx := context.Background()
	// A store that errors on reads: enforcement must surface the failure
	// (fail closed) instead of faking a zero-used allowance.
	svc := NewGatewayServiceWithStore(&failingGatewayStore{gatewayStoreFake: newGatewayStoreFake(), spendErr: true})
	key, _, err := svc.CreateVirtualKey(ctx, "org-1", "u", VirtualKeyInput{Label: "cap", MonthlyLimitCents: 100})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	err = svc.Authorize(ctx, "org-1", key.ID, "gpt-4o", 1, time.Now())
	if err == nil || errors.Is(err, ErrSpendLimitExceeded) || errors.Is(err, ErrModelNotAllowed) {
		t.Fatalf("ledger failure must surface as a non-denial error, got %v", err)
	}
	// Unlimited keys skip the ledger entirely and still authorize.
	key2, _, err := svc.CreateVirtualKey(ctx, "org-1", "u", VirtualKeyInput{Label: "unlimited"})
	if err != nil {
		t.Fatalf("create2: %v", err)
	}
	if err := svc.Authorize(ctx, "org-1", key2.ID, "gpt-4o", 1, time.Now()); err != nil {
		t.Fatalf("unlimited key must not touch the ledger: %v", err)
	}
}

// failingGatewayStore fails the selected operations (create still works so
// fixtures can be built).
type failingGatewayStore struct {
	*gatewayStoreFake
	spendErr bool
}

func (f *failingGatewayStore) SpendForMonth(ctx context.Context, orgID, keyID, month string) (float64, error) {
	if f.spendErr {
		return 0, errors.New("ledger unavailable")
	}
	return f.gatewayStoreFake.SpendForMonth(ctx, orgID, keyID, month)
}

// --- store-backed CRUD ------------------------------------------------------

func TestGatewayServiceCRUDThroughStore(t *testing.T) {
	ctx := context.Background()
	store := newGatewayStoreFake()
	svc := NewGatewayServiceWithStore(store)

	key, plaintext, err := svc.CreateVirtualKey(ctx, "org-1", "user-1", VirtualKeyInput{Label: "durable", MonthlyLimitCents: 500})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if store.calls["create"] != 1 {
		t.Fatalf("create must hit the store: %v", store.calls)
	}

	// Allow-list pointer semantics over the durable path:
	//   nil pointer = unchanged, &nil = ALL, &[] = NONE, &[...] = listed.
	if _, err := svc.UpdateVirtualKey(ctx, "org-1", key.ID, VirtualKeyUpdate{Models: &[]string{}}); err != nil {
		t.Fatalf("update to none: %v", err)
	}
	got, _ := svc.GetVirtualKey(ctx, "org-1", key.ID)
	if got.Models == nil || len(got.Models) != 0 {
		t.Fatalf("empty allow-list must round-trip as enabled-and-empty: %#v", got.Models)
	}
	if _, err := svc.UpdateVirtualKey(ctx, "org-1", key.ID, VirtualKeyUpdate{Models: &[]string{"gpt-4o"}}); err != nil {
		t.Fatalf("update to listed: %v", err)
	}
	nilList := []string(nil)
	if _, err := svc.UpdateVirtualKey(ctx, "org-1", key.ID, VirtualKeyUpdate{Models: &nilList}); err != nil {
		t.Fatalf("update to all: %v", err)
	}
	got, _ = svc.GetVirtualKey(ctx, "org-1", key.ID)
	if got.Models != nil {
		t.Fatalf("nil allow-list must round-trip as disabled: %#v", got.Models)
	}
	if _, err := svc.UpdateVirtualKey(ctx, "org-9", key.ID, VirtualKeyUpdate{Label: ptrString("x")}); !errors.Is(err, ErrVirtualKeyNotFound) {
		t.Fatalf("foreign update must be NotFound, got %v", err)
	}

	// The store keeps the hash; the auth path works against it.
	resolved, err := svc.ResolveByPlaintext(ctx, plaintext)
	if err != nil || resolved.ID != key.ID {
		t.Fatalf("resolve via store: %v", err)
	}
	if store.calls["getByHash"] != 1 {
		t.Fatalf("resolve must hit the store: %v", store.calls)
	}

	// Revoke routes through the store and flips the cached row.
	if err := svc.RevokeVirtualKey(ctx, "org-1", key.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if store.calls["revoke"] != 1 {
		t.Fatalf("revoke must hit the store: %v", store.calls)
	}
	if _, err := svc.ResolveByPlaintext(ctx, plaintext); err == nil {
		t.Fatalf("revoked key must not resolve")
	}
	if err := svc.RevokeVirtualKey(ctx, "org-1", "missing"); !errors.Is(err, ErrVirtualKeyNotFound) {
		t.Fatalf("unknown revoke: %v", err)
	}
}

func ptrString(s string) *string { return &s }

func TestGatewayOrgIsolation(t *testing.T) {
	ctx := context.Background()
	svc, _ := newGatewayTestService()
	keyA, _ := mustCreateKey(t, svc, "org-a", VirtualKeyInput{Label: "a", MonthlyLimitCents: 100})
	mustCreateKey(t, svc, "org-b", VirtualKeyInput{Label: "b"})

	listA, _ := svc.ListVirtualKeys(ctx, "org-a")
	listB, _ := svc.ListVirtualKeys(ctx, "org-b")
	if len(listA) != 1 || len(listB) != 1 || listA[0].ID == listB[0].ID {
		t.Fatalf("lists must be tenant-scoped: %d/%d", len(listA), len(listB))
	}
	// Every cross-tenant operation is a plain NotFound.
	if _, err := svc.GetVirtualKey(ctx, "org-b", keyA.ID); !errors.Is(err, ErrVirtualKeyNotFound) {
		t.Fatalf("cross get: %v", err)
	}
	// Spend is org-scoped: org-b usage never shows on org-a's key.
	now := time.Date(2024, 5, 1, 0, 0, 0, 0, time.UTC)
	_ = svc.RecordSpend(ctx, "org-b", keyA.ID, "gpt-4o", 50, now) // foreign key id, but the ledger is coordinate-scoped
	if err := svc.Authorize(ctx, "org-a", keyA.ID, "gpt-4o", 100, now); err != nil {
		t.Fatalf("org-a must not see org-b spend: %v", err)
	}
}

// --- GuardedProvider --------------------------------------------------------

// newFakeOpenAIProvider spins an httptest server speaking the OpenAI
// chat/completions shape and returns a provider pointed at it.
func newFakeOpenAIProvider(t *testing.T, usageJSON string, calls *int) (*OpenAIProvider, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			*calls++
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-4o-mini","choices":[{"message":{"role":"assistant","content":"pong"}}],"usage":` + usageJSON + `}`))
	}))
	t.Cleanup(srv.Close)
	return NewOpenAIProvider("openai", "test-key", srv.URL, "gpt-4o-mini", srv.Client()), srv
}

const fakeUsage = `{"prompt_tokens":1000,"completion_tokens":2000,"total_tokens":3000}`

func TestGuardedProviderAllowsAndRecordsActualSpend(t *testing.T) {
	ctx := context.Background()
	svc, _ := newGatewayTestService()
	key, plaintext := mustCreateKey(t, svc, "org-1", VirtualKeyInput{Label: "guarded"}) // nil allow-list, unlimited
	inner, _ := newFakeOpenAIProvider(t, fakeUsage, nil)
	guarded := NewGuardedProvider(inner, svc, "org-1", key.ID)

	resp, err := guarded.Complete(ctx, CompletionRequest{Prompt: "ping", Model: "gpt-4o-mini"})
	if err != nil {
		t.Fatalf("guarded complete: %v", err)
	}
	if resp.Text != "pong" || guarded.Name() != inner.Name() {
		t.Fatalf("decorator must be transparent: %+v name=%q", resp, guarded.Name())
	}
	// Actual metering: (1000*0.15 + 2000*0.60)/1M * 100 = 0.135 cents.
	used, _, err := svc.Spend(ctx, "org-1", key.ID, time.Now().UTC())
	if err != nil || math.Abs(used-0.135) > 1e-9 {
		t.Fatalf("actual spend = %v err %v (want 0.135)", used, err)
	}
	// The plaintext of a GUARDED key still resolves (worker auth path).
	if resolved, rerr := svc.ResolveByPlaintext(ctx, plaintext); rerr != nil || resolved.ID != key.ID {
		t.Fatalf("resolve after guarded call: %v", rerr)
	}
}

func TestGuardedProviderDeniesModelBeforeInner(t *testing.T) {
	svc, aud := newGatewayTestService()
	key, _ := mustCreateKey(t, svc, "org-1", VirtualKeyInput{Label: "pinned", Models: []string{"gpt-4o"}})
	calls := 0
	inner, _ := newFakeOpenAIProvider(t, fakeUsage, &calls)
	guarded := NewGuardedProvider(inner, svc, "org-1", key.ID)

	_, err := guarded.Complete(context.Background(), CompletionRequest{Prompt: "ping", Model: "gpt-4o-mini"})
	if !errors.Is(err, ErrModelNotAllowed) {
		t.Fatalf("expected model_not_allowed, got %v", err)
	}
	if calls != 0 {
		t.Fatalf("inner provider must NOT be called on denial, got %d calls", calls)
	}
	if actions := aud.actions(); actions[AuditGatewayModelNotAllowed] != 1 {
		t.Fatalf("model denial must be audited: %v", actions)
	}
}

func TestGuardedProviderDeniesOverspendWithEstimate(t *testing.T) {
	svc, aud := newGatewayTestService()
	// Cap of 10 cents; a 1M-max-token gpt-4o-mini request prices at 60
	// cents (output-only) -> the ESTIMATE alone must deny pre-flight.
	key, _ := mustCreateKey(t, svc, "org-1", VirtualKeyInput{Label: "cap", MonthlyLimitCents: 10})
	calls := 0
	inner, _ := newFakeOpenAIProvider(t, fakeUsage, &calls)
	guarded := NewGuardedProvider(inner, svc, "org-1", key.ID)

	_, err := guarded.Complete(context.Background(), CompletionRequest{Prompt: "ping", Model: "gpt-4o-mini", MaxTokens: 1_000_000})
	if !errors.Is(err, ErrSpendLimitExceeded) {
		t.Fatalf("expected spend_limit_exceeded from estimate, got %v", err)
	}
	if calls != 0 {
		t.Fatalf("inner provider must NOT be called on overspend, got %d calls", calls)
	}
	if actions := aud.actions(); actions[AuditGatewaySpendDenied] != 1 {
		t.Fatalf("spend denial must be audited: %v", actions)
	}

	// A modest request passes and records the ACTUAL cost after delivery.
	resp, err := guarded.Complete(context.Background(), CompletionRequest{Prompt: "ping", Model: "gpt-4o-mini", MaxTokens: 1000})
	if err != nil || resp == nil {
		t.Fatalf("modest request must pass: %v", err)
	}
	if used, _, serr := svc.Spend(context.Background(), "org-1", key.ID, time.Now().UTC()); serr != nil || math.Abs(used-0.135) > 1e-9 {
		t.Fatalf("actual spend after modest request = %v err %v", used, serr)
	}
}

func TestGuardedProviderUnpricedModelZeroCostPreCheck(t *testing.T) {
	svc, _ := newGatewayTestService()
	// Unknown-to-pricing models degrade to the 0-cost pre-check: the
	// allow-list + used-to-date are enforced, the estimate is 0.
	key, _ := mustCreateKey(t, svc, "org-1", VirtualKeyInput{Label: "cap", MonthlyLimitCents: 100})
	if err := svc.RecordSpend(context.Background(), "org-1", key.ID, "exotic", 40, time.Now().UTC()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	inner := NewStaticProvider("static", "pong")
	guarded := NewGuardedProvider(inner, svc, "org-1", key.ID)

	if _, err := guarded.Complete(context.Background(), CompletionRequest{Prompt: "ping", Model: "exotic"}); err != nil {
		t.Fatalf("0-cost pre-check under the limit must pass: %v", err)
	}
	if err := svc.RecordSpend(context.Background(), "org-1", key.ID, "exotic", 70, time.Now().UTC()); err != nil {
		t.Fatalf("seed2: %v", err)
	}
	if _, err := guarded.Complete(context.Background(), CompletionRequest{Prompt: "ping", Model: "exotic"}); !errors.Is(err, ErrSpendLimitExceeded) {
		t.Fatalf("blown cap with unpriced model must deny on used-to-date, got %v", err)
	}
}

func TestGuardedProviderEdgePaths(t *testing.T) {
	ctx := context.Background()
	svc, _ := newGatewayTestService()
	key, _ := mustCreateKey(t, svc, "org-1", VirtualKeyInput{Label: "edge", Models: []string{"gpt-4o-mini"}})

	// Provider errors pass through untouched and record NO spend.
	inner, srv := newFakeOpenAIProvider(t, fakeUsage, nil)
	srv.Close() // connection refused -> transport error
	guarded := NewGuardedProvider(inner, svc, "org-1", key.ID)
	if _, err := guarded.Complete(ctx, CompletionRequest{Prompt: "ping", Model: "gpt-4o-mini", MaxTokens: 10}); err == nil {
		t.Fatalf("transport failure must surface")
	}
	if used, _ := svc.spendForMonth(ctx, "org-1", key.ID, monthKey(time.Now())); used != 0 {
		t.Fatalf("failed completions must not record spend: %v", used)
	}

	// Unnameable (empty) model against an ENABLED allow-list is a denial.
	inner2 := NewStaticProvider("static", "pong")
	guarded2 := NewGuardedProvider(inner2, svc, "org-1", key.ID)
	if _, err := guarded2.Complete(ctx, CompletionRequest{Prompt: "ping"}); !errors.Is(err, ErrModelNotAllowed) {
		t.Fatalf("empty model must be denied by an enabled allow-list, got %v", err)
	}

	// A nil gateway degrades to a pure passthrough (documented unguarded
	// wiring), and a nil inner provider is rejected.
	passthrough := NewGuardedProvider(NewStaticProvider("static", "pong"), nil, "org-1", key.ID)
	if resp, err := passthrough.Complete(ctx, CompletionRequest{Prompt: "ping"}); err != nil || resp == nil {
		t.Fatalf("nil gateway must pass through: %v", err)
	}
	nilInner := NewGuardedProvider(nil, svc, "org-1", key.ID)
	if _, err := nilInner.Complete(ctx, CompletionRequest{Prompt: "ping"}); err == nil {
		t.Fatalf("nil inner provider must be rejected")
	}
}

func TestGuardedProviderDenialsCarryHTTPSemantics(t *testing.T) {
	// The documented handler mapping: SpendLimitError -> 402,
	// ModelNotAllowedError -> 403 (exercised through mapGatewayDenial in the
	// cmd/api tests; here we pin the sentinel wiring).
	sle := &SpendLimitError{KeyID: "k", LimitCents: 10, UsedCents: 5, ProjectedCents: 15}
	if !errors.Is(sle, ErrSpendLimitExceeded) || !strings.Contains(sle.Error(), "spend_limit_exceeded") {
		t.Fatalf("spend error contract broken: %v", sle)
	}
	mna := &ModelNotAllowedError{KeyID: "k", Model: "m"}
	if !errors.Is(mna, ErrModelNotAllowed) || !strings.Contains(mna.Error(), "model_not_allowed") {
		t.Fatalf("model error contract broken: %v", mna)
	}
}

// --- audit seam -------------------------------------------------------------

func TestGatewaySpendAuditOptIn(t *testing.T) {
	ctx := context.Background()
	svc, aud := newGatewayTestService()
	key, _ := mustCreateKey(t, svc, "org-1", VirtualKeyInput{Label: "volume"})

	// Default OFF: per-completion volume stays out of the trail.
	if err := svc.RecordSpend(ctx, "org-1", key.ID, "gpt-4o", 1, time.Now()); err != nil {
		t.Fatalf("record: %v", err)
	}
	if actions := aud.actions(); actions[AuditGatewaySpendRecorded] != 0 {
		t.Fatalf("spend recording must be silent by default: %v", actions)
	}
	svc.SetSpendAudit(true)
	if err := svc.RecordSpend(ctx, "org-1", key.ID, "gpt-4o", 2, time.Now()); err != nil {
		t.Fatalf("record2: %v", err)
	}
	if actions := aud.actions(); actions[AuditGatewaySpendRecorded] != 1 {
		t.Fatalf("opt-in spend audit missing: %v", actions)
	}
}
