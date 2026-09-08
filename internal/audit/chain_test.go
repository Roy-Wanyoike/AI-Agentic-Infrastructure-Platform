package audit

// chain_test.go — tamper-evidence tests for the hash chain (wave 8 issue A).
// Coverage: fingerprint determinism + golden vector, empty-org verdicts,
// multi-entry verification in both modes (in-memory and Store-backed),
// per-org head cache behavior, tamper detection through an injected
// corruption hook (the store-agnostic pure verifier over crafted sequences),
// legacy (pre-feature) row anchoring, and the logical-clock guarantee.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// corruptibleStore is a Store that lets tests corrupt stored rows after the
// fact — the "injected corruption hook" simulating a database-level edit of
// an append-only row. It holds the same *Entry pointers LogCtx persisted.
type corruptibleStore struct {
	entries []*Entry
}

func (s *corruptibleStore) InsertEntry(_ context.Context, entry *Entry) error {
	s.entries = append(s.entries, entry)
	return nil
}

func (s *corruptibleStore) ListEntries(_ context.Context, orgID string) ([]*Entry, error) {
	out := make([]*Entry, 0)
	for _, entry := range s.entries {
		if entry.OrganizationID == orgID {
			out = append(out, entry)
		}
	}
	return out, nil
}

// goldenEntryFingerprint pins the v1 canonical encoding byte-for-byte so no
// refactor can silently re-hash v1 evidence. The exact input:
//
//	prev = "" (genesis), fields = audit-chain/v1 | "" | 0d9f4e5e-8f5e-4a4f-9a3b-6a1c2d3e4f5a |
//	user-alice | agent.created | org-acme | agents/a-1 | 2026-09-08T12:30:00.123456789Z |
//	{"a":"one","b":2,"nested":{"x":1.5,"y":true}}
//
// each length-prefixed as "%d:%s|" and fed to sha256.
func goldenEntry() *Entry {
	return &Entry{
		ID:             "0d9f4e5e-8f5e-4a4f-9a3b-6a1c2d3e4f5a",
		Actor:          "user-alice",
		Action:         "agent.created",
		OrganizationID: "org-acme",
		Resource:       "agents/a-1",
		Metadata: map[string]any{
			"a":      "one",
			"b":      2,
			"nested": map[string]any{"x": 1.5, "y": true},
		},
		CreatedAt: time.Date(2026, 9, 8, 12, 30, 0, 123456789, time.UTC),
	}
}

func TestEntryFingerprintGoldenAndDeterminism(t *testing.T) {
	const want = "21eed181de51f11013cab2fe402083b4f3e4c3c409874ef08ed8938de9418a04"
	entry := goldenEntry()
	if got := entryFingerprint(ChainGenesisPrevHash, entry); got != want {
		t.Fatalf("fingerprint drifted from the v1 golden vector: got %s want %s", got, want)
	}
	// Deterministic: identical input must re-hash identically.
	if again := entryFingerprint(ChainGenesisPrevHash, goldenEntry()); again != want {
		t.Fatalf("fingerprint not deterministic: %s", again)
	}
	// Every input field must move the hash (length-prefixed encoding has no
	// delimiter ambiguity to exploit).
	mutations := map[string]func(*Entry){
		"id":             func(e *Entry) { e.ID = "different-id" },
		"actor":          func(e *Entry) { e.Actor = "user-mallory" },
		"action":         func(e *Entry) { e.Action = "agent.deleted" },
		"organizationID": func(e *Entry) { e.OrganizationID = "org-other" },
		"resource":       func(e *Entry) { e.Resource = "agents/a-2" },
		"createdAt":      func(e *Entry) { e.CreatedAt = e.CreatedAt.Add(time.Nanosecond) },
		"metadata":       func(e *Entry) { e.Metadata["a"] = "two" },
	}
	base := entryFingerprint(ChainGenesisPrevHash, entry)
	for name, mutate := range mutations {
		altered := goldenEntry()
		mutate(altered)
		if entryFingerprint(ChainGenesisPrevHash, altered) == base {
			t.Errorf("mutation %q did not change the fingerprint", name)
		}
	}
	// prev_hash sensitivity: a different predecessor changes the hash.
	if entryFingerprint(strings.Repeat("a", 64), entry) == base {
		t.Error("prev hash must be part of the fingerprint input")
	}
	// The stamp key is excluded: adding a stamp never changes the hash.
	withStamp := goldenEntry()
	withStamp.Metadata[ChainMetadataKey] = map[string]any{"version": ChainVersionV1, "prev": "x", "hash": "y"}
	if entryFingerprint(ChainGenesisPrevHash, withStamp) != base {
		t.Error("reserved stamp key must be excluded from the fingerprint")
	}
	// nil and empty metadata hash identically (canonical "{}").
	empty := goldenEntry()
	empty.Metadata = nil
	emptyNoID := goldenEntry()
	emptyNoID.Metadata = map[string]any{}
	if entryFingerprint("p", empty) != entryFingerprint("p", emptyNoID) {
		t.Error("nil and empty metadata must canonicalize identically")
	}
}

func TestChainHeadAndVerifyEmptyOrg(t *testing.T) {
	svc := NewService()
	head, length, err := svc.ChainHead(t.Context(), "org-empty")
	if err != nil || head != ChainGenesisPrevHash || length != 0 {
		t.Fatalf("empty org head: got (%q, %d, %v)", head, length, err)
	}
	report, err := svc.VerifyChain(t.Context(), "org-empty")
	if err != nil {
		t.Fatalf("VerifyChain(empty): %v", err)
	}
	if !report.Valid || report.Length != 0 || report.HeadHash != ChainGenesisPrevHash {
		t.Fatalf("empty org must verify trivially: %+v", report)
	}
	if report.BrokenAtIndex != 0 || report.ExpectedHeadHash != "" {
		t.Fatalf("empty report must carry no break fields: %+v", report)
	}
	if _, _, err := svc.ChainHead(t.Context(), "  "); !errors.Is(err, ErrOrgRequired) {
		t.Fatalf("blank org must be ErrOrgRequired, got %v", err)
	}
	if _, err := svc.VerifyChain(t.Context(), ""); !errors.Is(err, ErrOrgRequired) {
		t.Fatalf("blank org must be ErrOrgRequired, got %v", err)
	}
}

// TestChainVerifiesMultiEntry pins the happy path in BOTH modes: in-memory
// and Store-backed. The chain must verify across two orgs without cross-talk,
// and the head must match ChainHead, VerifyEntries and the export anchor.
func TestChainVerifiesMultiEntry(t *testing.T) {
	for _, mode := range []struct {
		name  string
		build func() *Service
	}{
		{"in-memory", func() *Service { return NewService() }},
		{"store", func() *Service { return NewServiceWithStore(&corruptibleStore{}) }},
	} {
		t.Run(mode.name, func(t *testing.T) {
			svc := mode.build()
			for i := 0; i < 4; i++ {
				if _, err := svc.LogCtx(t.Context(), "alice", "agent.created", "org-a", "agents/a-1",
					map[string]any{"i": i, "note": "ключ 🔑"}); err != nil {
					t.Fatalf("LogCtx %d: %v", i, err)
				}
			}
			if _, err := svc.LogCtx(t.Context(), "bob", "run.started", "org-b", "runs/r-1", nil); err != nil {
				t.Fatalf("LogCtx foreign org: %v", err)
			}

			report, err := svc.VerifyChain(t.Context(), "org-a")
			if err != nil {
				t.Fatalf("VerifyChain: %v", err)
			}
			if !report.Valid || report.Length != 4 {
				t.Fatalf("org-a chain must verify with 4 entries: %+v", report)
			}
			if report.HeadHash == "" {
				t.Fatal("a non-empty chain must have a non-empty head")
			}
			head, length, err := svc.ChainHead(t.Context(), "org-a")
			if err != nil || head != report.HeadHash || length != report.Length {
				t.Fatalf("ChainHead disagree with VerifyChain: (%q,%d) vs %+v", head, length, report)
			}
			foreign, err := svc.VerifyChain(t.Context(), "org-b")
			if err != nil || !foreign.Valid || foreign.Length != 1 {
				t.Fatalf("org-b chain must verify independently: %+v err=%v", foreign, err)
			}
			if foreign.HeadHash == report.HeadHash {
				t.Fatal("distinct orgs must not share a chain head")
			}
			// The pure verifier over the listed entries must agree.
			entries, err := svc.ListCtx(t.Context(), "org-a")
			if err != nil {
				t.Fatalf("ListCtx: %v", err)
			}
			if pure := VerifyEntries(entries); *pure != *report {
				t.Fatalf("VerifyEntries must agree with VerifyChain: %+v vs %+v", pure, report)
			}
		})
	}
}

func TestChainHeadCacheTracksInserts(t *testing.T) {
	svc := NewService()
	if _, err := svc.LogCtx(t.Context(), "alice", "agent.created", "org-1", "agents/a-1", nil); err != nil {
		t.Fatalf("LogCtx: %v", err)
	}
	head1, len1, err := svc.ChainHead(t.Context(), "org-1")
	if err != nil || len1 != 1 {
		t.Fatalf("first head: (%q, %d, %v)", head1, len1, err)
	}
	if _, err := svc.LogCtx(t.Context(), "bob", "run.started", "org-1", "runs/r-1", nil); err != nil {
		t.Fatalf("LogCtx 2: %v", err)
	}
	head2, len2, err := svc.ChainHead(t.Context(), "org-1")
	if err != nil || len2 != 2 {
		t.Fatalf("second head: (%q, %d, %v)", head2, len2, err)
	}
	if head1 == head2 {
		t.Fatal("an append must advance the chain head")
	}
	// Warm cache is stable and consistent with a fresh verification.
	head3, len3, _ := svc.ChainHead(t.Context(), "org-1")
	if head3 != head2 || len3 != 2 {
		t.Fatalf("warm cache drifted: (%q, %d)", head3, len3)
	}
	if report, _ := svc.VerifyChain(t.Context(), "org-1"); report.HeadHash != head2 || report.Length != 2 || !report.Valid {
		t.Fatalf("verification disagrees with the cached head: %+v vs %q", report, head2)
	}
}

// TestVerifyChainDetectsTamperedEntry uses the corruption hook: rows exist
// and verify, then the STORED copy of one row is edited (the append-only
// contract is violated at the persistence layer). The chain must name the
// first provable break and separate the claimed head (what the stamps say)
// from the recomputed head (what the data hashes to now).
func TestVerifyChainDetectsTamperedEntry(t *testing.T) {
	store := &corruptibleStore{}
	svc := NewServiceWithStore(store)
	for i := 0; i < 4; i++ {
		if _, err := svc.LogCtx(t.Context(), "alice", "agent.created", "org-1", "agents/a-1", map[string]any{"i": i}); err != nil {
			t.Fatalf("LogCtx %d: %v", i, err)
		}
	}
	healthy, err := svc.VerifyChain(t.Context(), "org-1")
	if err != nil || !healthy.Valid {
		t.Fatalf("pre-tamper chain must be valid: %+v err=%v", healthy, err)
	}

	t.Run("field tamper", func(t *testing.T) {
		store.entries[1].Action = "agent.tampered"
		report, err := svc.VerifyChain(t.Context(), "org-1")
		if err != nil {
			t.Fatalf("VerifyChain: %v", err)
		}
		if report.Valid {
			t.Fatalf("tampered entry must invalidate the chain: %+v", report)
		}
		if report.BrokenAtIndex != 1 {
			t.Fatalf("BrokenAtIndex = %d, want 1", report.BrokenAtIndex)
		}
		if report.ExpectedHeadHash != healthy.HeadHash {
			t.Fatalf("ExpectedHeadHash must be the claimed (pre-tamper) head: got %q want %q", report.ExpectedHeadHash, healthy.HeadHash)
		}
		if report.HeadHash == healthy.HeadHash {
			t.Fatal("recomputed head must move after tampering")
		}
		if report.Length != 4 {
			t.Fatalf("tamper detection must not drop entries: %+v", report)
		}
	})

	t.Run("metadata tamper", func(t *testing.T) {
		fresh := &corruptibleStore{}
		freshSvc := NewServiceWithStore(fresh)
		for i := 0; i < 3; i++ {
			if _, err := freshSvc.LogCtx(t.Context(), "alice", "run.started", "org-2", "runs/r-1", map[string]any{"amount": 1}); err != nil {
				t.Fatalf("LogCtx %d: %v", i, err)
			}
		}
		baseline, _ := freshSvc.VerifyChain(t.Context(), "org-2")
		fresh.entries[2].Metadata["amount"] = 1_000_000
		report, _ := freshSvc.VerifyChain(t.Context(), "org-2")
		if report.Valid || report.BrokenAtIndex != 2 || report.ExpectedHeadHash != baseline.HeadHash {
			t.Fatalf("metadata tamper must break at 3rd entry: %+v (baseline %+v)", report, baseline)
		}
	})

	t.Run("stamp tamper", func(t *testing.T) {
		fresh := &corruptibleStore{}
		freshSvc := NewServiceWithStore(fresh)
		for i := 0; i < 3; i++ {
			if _, err := freshSvc.LogCtx(t.Context(), "alice", "run.started", "org-3", "runs/r-1", nil); err != nil {
				t.Fatalf("LogCtx %d: %v", i, err)
			}
		}
		// Rewriting the stamp's hash (without recomputing content) must break.
		stamp, _ := fresh.entries[0].Metadata[ChainMetadataKey].(map[string]any)
		stamp["hash"] = strings.Repeat("f", 64)
		report, _ := freshSvc.VerifyChain(t.Context(), "org-3")
		if report.Valid || report.BrokenAtIndex != 0 {
			t.Fatalf("forged stamp must break at 0: %+v", report)
		}
	})
}

// TestVerifyEntriesCraftedSequence is the store-agnostic tamper test: the
// pure verifier judges hand-crafted sequences with hand-computed stamps.
func TestVerifyEntriesCraftedSequence(t *testing.T) {
	craft := func(count int) []*Entry {
		entries := make([]*Entry, 0, count)
		prev := ChainGenesisPrevHash
		for i := 0; i < count; i++ {
			e := &Entry{
				ID:             time.Duration(i).String(), // ids sort with i
				Actor:          "crafter",
				Action:         "crafted.entry",
				OrganizationID: "org-craft",
				Resource:       "things/1",
				Metadata:       map[string]any{"i": i},
				CreatedAt:      time.Date(2026, 1, 1, 0, 0, i, 0, time.UTC),
			}
			hash := entryFingerprint(prev, e)
			e.Metadata[ChainMetadataKey] = map[string]any{"version": ChainVersionV1, "prev": prev, "hash": hash}
			prev = hash
			entries = append(entries, e)
		}
		return entries
	}
	entries := craft(5)
	if report := VerifyEntries(entries); !report.Valid || report.Length != 5 {
		t.Fatalf("crafted chain must verify: %+v", report)
	}
	// The verifier is order-insensitive over the input (canonical ordering).
	shuffled := []*Entry{entries[3], entries[0], entries[4], entries[1], entries[2]}
	if report := VerifyEntries(shuffled); !report.Valid || report.BrokenAtIndex != 0 {
		t.Fatalf("input order must not matter: %+v", report)
	}
	// Tamper the middle entry.
	entries[2].Actor = "attacker"
	if report := VerifyEntries(entries); report.Valid || report.BrokenAtIndex != 2 {
		t.Fatalf("middle tamper must break at 2: %+v", report)
	}
	// Tamper the first entry.
	first := craft(3)
	first[0].Resource = "things/2"
	if report := VerifyEntries(first); report.Valid || report.BrokenAtIndex != 0 {
		t.Fatalf("first-entry tamper must break at 0: %+v", report)
	}
	// A completely unstamped sequence still replays (pre-feature rows are
	// hashed positionally, and nothing contradicts them).
	bare := craft(3)
	for _, e := range bare {
		delete(e.Metadata, ChainMetadataKey)
	}
	if report := VerifyEntries(bare); !report.Valid || report.Length != 3 || report.HeadHash == "" {
		t.Fatalf("unstamped sequence must replay: %+v", report)
	}
}

func TestCanonicalChainOrderSorts(t *testing.T) {
	svc := NewService()
	base := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	// Seeded out of chronological order; two share a timestamp (id ASC breaks
	// the tie).
	seedEntry(t, svc, "org-1", base.Add(2*time.Minute), "m", "agent.created")
	seedEntry(t, svc, "org-1", base, "z", "agent.created")
	seedEntry(t, svc, "org-1", base, "a", "agent.created")
	seedEntry(t, svc, "org-1", base.Add(time.Minute), "k", "agent.created")

	entries, _ := svc.ListCtx(t.Context(), "org-1")
	ordered := canonicalChainOrder(entries)
	want := []string{"a", "z", "k", "m"}
	for i, id := range want {
		if ordered[i].ID != id {
			t.Fatalf("position %d: got %s want %s", i, ordered[i].ID, id)
		}
	}
}

// TestLogCtxStampsChainMetadata pins the write-path contract: the service
// owns the reserved key, the caller's map is copied (never mutated), and the
// stamp can be recomputed from the row itself.
func TestLogCtxStampsChainMetadata(t *testing.T) {
	svc := NewService()
	callerMetadata := map[string]any{"name": "helper"}
	entry, err := svc.LogCtx(t.Context(), "alice", "agent.created", "org-1", "agents/a-1", callerMetadata)
	if err != nil {
		t.Fatalf("LogCtx: %v", err)
	}
	if _, forged := callerMetadata[ChainMetadataKey]; forged {
		t.Fatal("the caller's map must not be mutated with the stamp")
	}
	raw, ok := entry.Metadata[ChainMetadataKey]
	if !ok {
		t.Fatalf("entry metadata must carry the chain stamp: %+v", entry.Metadata)
	}
	stamp, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("stamp must be an object: %T", raw)
	}
	if stamp["version"] != ChainVersionV1 {
		t.Fatalf("stamp version = %v, want %q", stamp["version"], ChainVersionV1)
	}
	if stamp["prev"] != ChainGenesisPrevHash {
		t.Fatalf("first entry must chain from genesis: %v", stamp["prev"])
	}
	// The stored hash must be exactly the recomputed fingerprint of the row.
	if stamp["hash"] != entryFingerprint(ChainGenesisPrevHash, entry) {
		t.Fatalf("stamp hash must equal the recomputed fingerprint: %v", stamp["hash"])
	}
	// Caller-supplied values for the reserved key are overwritten (no forgery).
	forged, err := svc.LogCtx(t.Context(), "alice", "agent.created", "org-1", "agents/a-2",
		map[string]any{ChainMetadataKey: map[string]any{"hash": "forged"}})
	if err != nil {
		t.Fatalf("LogCtx forged: %v", err)
	}
	if report, _ := svc.VerifyChain(t.Context(), "org-1"); !report.Valid || report.Length != 2 {
		t.Fatalf("service must own the reserved key: %+v", report)
	}
	if got := forged.Metadata[ChainMetadataKey].(map[string]any)["hash"]; got == "forged" {
		t.Fatal("caller-supplied stamp must be overwritten by the service")
	}
}

// TestLogCtxTimestampsStrictlyIncreasing pins the logical clock: even when
// the wall clock quantizes, two rows can never share (or invert) a
// timestamp, so canonical chain order always equals insertion order.
func TestLogCtxTimestampsStrictlyIncreasing(t *testing.T) {
	svc := NewService()
	previous := time.Time{}
	for i := 0; i < 50; i++ {
		entry, err := svc.LogCtx(t.Context(), "alice", "tick", "org-1", "clock", nil)
		if err != nil {
			t.Fatalf("LogCtx %d: %v", i, err)
		}
		if !entry.CreatedAt.After(previous) {
			t.Fatalf("CreatedAt %s not after previous %s at %d", entry.CreatedAt, previous, i)
		}
		previous = entry.CreatedAt
	}
}

// TestVerifyChainAnchorsLegacyRows proves pre-feature rows (no stamp) are
// pulled under the chain by the next stamped entry — and that tampering such
// a row surfaces at the first PROVABLE break (the stamped successor).
func TestVerifyChainAnchorsLegacyRows(t *testing.T) {
	svc := NewService()
	seedEntry(t, svc, "org-1", time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), "legacy-1", "agent.created")
	if _, err := svc.LogCtx(t.Context(), "alice", "agent.created", "org-1", "agents/a-1", nil); err != nil {
		t.Fatalf("LogCtx: %v", err)
	}
	if _, err := svc.LogCtx(t.Context(), "alice", "run.started", "org-1", "runs/r-1", nil); err != nil {
		t.Fatalf("LogCtx 2: %v", err)
	}
	report, err := svc.VerifyChain(t.Context(), "org-1")
	if err != nil || !report.Valid || report.Length != 3 {
		t.Fatalf("legacy prefix must verify through the successor stamps: %+v err=%v", report, err)
	}
	// Corrupt the legacy row: the break surfaces at the next stamped entry.
	svc.mu.Lock()
	svc.items[0].Action = "agent.tampered"
	svc.mu.Unlock()
	broken, _ := svc.VerifyChain(t.Context(), "org-1")
	if broken.Valid || broken.BrokenAtIndex != 1 {
		t.Fatalf("legacy tamper must surface at the stamped successor: %+v", broken)
	}
}

// TestVerifyChainToleratesMiddleStampStrip documents the honest limitation:
// stripping a MIDDLE stamp does not move the chain (the stamp is excluded
// from fingerprints) and does not free the row's content — any field edit
// still breaks the successor's prev link. Only the LAST entry can be
// stripped-and-edited without internal detection, which is what the WORM
// head anchor (export) is for.
func TestVerifyChainToleratesMiddleStampStrip(t *testing.T) {
	svc := NewService()
	for i := 0; i < 3; i++ {
		if _, err := svc.LogCtx(t.Context(), "alice", "tick", "org-1", "clock", nil); err != nil {
			t.Fatalf("LogCtx %d: %v", i, err)
		}
	}
	baseline, _ := svc.VerifyChain(t.Context(), "org-1")
	svc.mu.Lock()
	delete(svc.items[1].Metadata, ChainMetadataKey)
	svc.mu.Unlock()
	report, _ := svc.VerifyChain(t.Context(), "org-1")
	if !report.Valid || report.HeadHash != baseline.HeadHash {
		t.Fatalf("middle stamp strip must not move the chain: %+v (baseline %+v)", report, baseline)
	}
	// ...but the stripped row's content is still bound: editing it breaks the
	// successor.
	svc.items[1].Action = "agent.tampered"
	broken, _ := svc.VerifyChain(t.Context(), "org-1")
	if broken.Valid || broken.BrokenAtIndex != 2 {
		t.Fatalf("content edit after strip must break at the successor: %+v", broken)
	}
}

func TestSetStoreInvalidatesHeadCache(t *testing.T) {
	store := &corruptibleStore{}
	svc := NewService()
	if _, err := svc.LogCtx(t.Context(), "alice", "agent.created", "org-1", "agents/a-1", nil); err != nil {
		t.Fatalf("LogCtx: %v", err)
	}
	inMemoryHead, _, err := svc.ChainHead(t.Context(), "org-1")
	if err != nil {
		t.Fatalf("ChainHead: %v", err)
	}
	// Swapping the store changes the listing source: the cached head must be
	// recomputed from the new backend.
	durable := &corruptibleStore{}
	durable.entries = append(durable.entries, &Entry{
		ID: "durable-1", Actor: "bob", Action: "agent.created", OrganizationID: "org-1",
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	svc.SetStore(durable)
	head, length, err := svc.ChainHead(t.Context(), "org-1")
	if err != nil || length != 1 {
		t.Fatalf("post-swap head: (%q, %d, %v)", head, length, err)
	}
	if head == inMemoryHead {
		t.Fatal("head cache must be recomputed after SetStore")
	}
	_ = store // unused directly; kept to document the intent
}
