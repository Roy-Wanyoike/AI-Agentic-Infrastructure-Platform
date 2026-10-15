package qualitygates

import (
	"context"
	"errors"
	"testing"
	"time"
)

func evidenceFixture() *EvidenceRecord {
	return &EvidenceRecord{
		ID:          "ev-1",
		OrgID:       "org-a",
		Suite:       SuiteEvaluations,
		CollectedAt: time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC),
		Result: SuiteResult{
			Status:   EvidenceCompleted,
			Cases:    10,
			Passed:   9,
			P95Ms:    1200,
			Findings: []EvidenceFinding{{Severity: SevLow, Title: "slow case"}},
		},
		Subject: EvidenceSubject{AppVersion: "v42", AgentID: "agent-1", AgentVersion: 7, ToolVersions: []string{"search@2"}},
	}
}

func TestEvidenceValidateRejections(t *testing.T) {
	cases := map[string]func(*EvidenceRecord){
		"missing id":           func(r *EvidenceRecord) { r.ID = "" },
		"missing org":          func(r *EvidenceRecord) { r.OrgID = "" },
		"unknown suite":        func(r *EvidenceRecord) { r.Suite = "chaos" },
		"missing collected_at": func(r *EvidenceRecord) { r.CollectedAt = time.Time{} },
		"unknown status":       func(r *EvidenceRecord) { r.Result.Status = "maybe" },
		"unknown severity":     func(r *EvidenceRecord) { r.Result.Findings[0].Severity = Severity("blocker") },
	}
	for name, mutate := range cases {
		rec := evidenceFixture()
		mutate(rec)
		if err := rec.Validate(); err == nil || !errors.Is(err, ErrInvalidEvidence) {
			t.Errorf("%s: want ErrInvalidEvidence, got %v", name, err)
		}
	}
}

func TestEvidenceStoreStampsHashAndVerifies(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryEvidenceStore()
	rec := evidenceFixture()
	if err := store.PutEvidence(ctx, rec); err != nil {
		t.Fatalf("put: %v", err)
	}
	if rec.ContentHash == "" {
		t.Fatal("PutEvidence must stamp a content hash")
	}
	if !rec.Verify() {
		t.Fatal("stamped record must verify")
	}

	got, err := store.GetEvidence(ctx, "org-a", "ev-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ContentHash != rec.ContentHash || got.Result.Passed != 9 {
		t.Fatalf("stored record wrong: %+v", got)
	}

	// Immutability: mutating the returned copy never touches the store.
	got.Result.Passed = 0
	got.Result.Findings[0].Severity = SevCritical
	again, err := store.GetEvidence(ctx, "org-a", "ev-1")
	if err != nil {
		t.Fatalf("get after mutation: %v", err)
	}
	if again.Result.Passed != 9 || again.Result.Findings[0].Severity != SevLow {
		t.Fatalf("store mutation leaked: %+v", again)
	}
}

func TestEvidenceStoreRejectsTamperedAndForeignReads(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryEvidenceStore()

	// A record arriving with a WRONG hash is rejected at the boundary.
	rec := evidenceFixture()
	rec.ContentHash = "deadbeef"
	if err := store.PutEvidence(ctx, rec); !errors.Is(err, ErrTamperedEvidence) {
		t.Fatalf("want ErrTamperedEvidence, got %v", err)
	}

	// A tampered STORED row is detected on read (integrity, not silence).
	good := evidenceFixture()
	if err := store.PutEvidence(ctx, good); err != nil {
		t.Fatalf("put: %v", err)
	}
	store.mu.Lock()
	corrupt := store.items["org-a"]["ev-1"] // simulate storage corruption
	corrupt.Result.Passed = 10
	store.items["org-a"]["ev-1"] = corrupt
	store.mu.Unlock()
	if _, err := store.GetEvidence(ctx, "org-a", "ev-1"); !errors.Is(err, ErrTamperedEvidence) {
		t.Fatalf("want ErrTamperedEvidence on corrupt read, got %v", err)
	}

	// Foreign/unknown refs resolve to not-found (no tenant probing).
	if _, err := store.GetEvidence(ctx, "org-b", "ev-1"); !errors.Is(err, ErrEvidenceNotFound) {
		t.Fatalf("want ErrEvidenceNotFound, got %v", err)
	}
	if _, err := store.GetEvidence(ctx, "org-a", "nope"); !errors.Is(err, ErrEvidenceNotFound) {
		t.Fatalf("want ErrEvidenceNotFound, got %v", err)
	}
}

func TestEvidenceStoreIdempotentReputAndListOrder(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryEvidenceStore()
	first := evidenceFixture()
	if err := store.PutEvidence(ctx, first); err != nil {
		t.Fatalf("put: %v", err)
	}
	// Identical re-put is idempotent.
	same := evidenceFixture()
	if err := store.PutEvidence(ctx, same); err != nil {
		t.Fatalf("idempotent re-put: %v", err)
	}
	// ANY content difference is a mutation attempt.
	diff := evidenceFixture()
	diff.Result.Passed = 5
	if err := store.PutEvidence(ctx, diff); err == nil || !errors.Is(err, ErrInvalidEvidence) {
		t.Fatalf("want ErrInvalidEvidence for divergent re-put, got %v", err)
	}

	newer := evidenceFixture()
	newer.ID = "ev-2"
	newer.CollectedAt = first.CollectedAt.Add(time.Hour)
	if err := store.PutEvidence(ctx, newer); err != nil {
		t.Fatalf("put newer: %v", err)
	}
	list, err := store.ListEvidence(ctx, "org-a")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 || list[0].ID != "ev-2" || list[1].ID != "ev-1" {
		t.Fatalf("list must be newest-first: %v", idsOf(list))
	}
	if _, err := store.ListEvidence(ctx, ""); !errors.Is(err, ErrOrgRequired) {
		t.Fatalf("want ErrOrgRequired, got %v", err)
	}
}

func idsOf(recs []*EvidenceRecord) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.ID)
	}
	return out
}

func TestNewestBySuiteTiebreakDeterministic(t *testing.T) {
	at := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	a := EvidenceRecord{ID: "a", Suite: SuiteEvaluations, CollectedAt: at}
	b := EvidenceRecord{ID: "b", Suite: SuiteEvaluations, CollectedAt: at} // same instant
	c := EvidenceRecord{ID: "c", Suite: SuiteEvaluations, CollectedAt: at.Add(-time.Minute)}
	// Input order must not matter: newest wins, ties break on id desc.
	for _, order := range [][]EvidenceRecord{{a, b, c}, {c, b, a}, {b, a, c}} {
		got := newestBySuite(order)
		if got[SuiteEvaluations].ID != "b" {
			t.Fatalf("want newest (b) to win, got %q (order %v)", got[SuiteEvaluations].ID, order)
		}
	}
}
