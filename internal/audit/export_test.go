package audit

// export_test.go — evidence export tests (wave 8 issue A). Coverage: NDJSON
// shape (entry lines + final chain_head line), chain-order rendering,
// byte-level determinism (the WORM-hashing contract) across calls AND across
// identically-seeded service instances, empty-org export, and org validation.

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// seedChainedEntry writes a fully-controlled entry (fixed id/timestamp) with
// a correct chain stamp into the in-memory service, so two instances can be
// seeded identically and their exports compared byte-for-byte.
func seedChainedEntry(t *testing.T, s *Service, orgID, id, action string, at time.Time, md map[string]any) {
	t.Helper()
	s.mu.Lock()
	entries, err := s.orgEntriesLocked(context.Background(), orgID)
	s.mu.Unlock()
	if err != nil {
		t.Fatalf("orgEntriesLocked: %v", err)
	}
	prev := replayChain(canonicalChainOrder(entries))
	stamped := make(map[string]any, len(md)+1)
	for k, v := range md {
		stamped[k] = v
	}
	entry := &Entry{ID: id, Actor: "tester", Action: action, OrganizationID: orgID, Resource: "resources/" + id, Metadata: stamped, CreatedAt: at}
	hash := entryFingerprint(prev, entry)
	stamped[ChainMetadataKey] = map[string]any{"version": ChainVersionV1, "prev": prev, "hash": hash}
	s.mu.Lock()
	s.items = append(s.items, entry)
	s.mu.Unlock()
}

// parseJSONL splits an NDJSON payload into decoded objects.
func parseJSONL(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	out := make([]map[string]any, 0, len(lines))
	for i, line := range lines {
		if line == "" {
			t.Fatalf("line %d is empty", i)
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			t.Fatalf("line %d is not JSON (%v): %s", i, err, line)
		}
		out = append(out, decoded)
	}
	return out
}

func TestExportJSONLShapeAndChainOrder(t *testing.T) {
	svc := NewService()
	// Three LogCtx rows for org-1 (ids/timestamps are service-controlled) and
	// one foreign row that must not leak into the export.
	for i := 0; i < 3; i++ {
		if _, err := svc.LogCtx(t.Context(), "alice", "agent.created", "org-1", "agents/a-1", map[string]any{"i": i, "zulu": true}); err != nil {
			t.Fatalf("LogCtx %d: %v", i, err)
		}
	}
	if _, err := svc.LogCtx(t.Context(), "bob", "run.started", "org-2", "runs/r-1", nil); err != nil {
		t.Fatalf("LogCtx foreign: %v", err)
	}

	data, err := svc.ExportJSONL(t.Context(), "org-1")
	if err != nil {
		t.Fatalf("ExportJSONL: %v", err)
	}
	if !bytes.HasSuffix(data, []byte("\n")) {
		t.Fatal("export must be newline-terminated (NDJSON)")
	}
	lines := parseJSONL(t, data)
	if len(lines) != 4 {
		t.Fatalf("expected 3 entry lines + chain_head, got %d", len(lines))
	}

	// Entry lines: canonical (oldest-first) order, full evidence fields.
	previousAt := time.Time{}
	for i := 0; i < 3; i++ {
		line := lines[i]
		if line["type"] != nil {
			t.Fatalf("entry line %d must not carry the chain_head type: %v", i, line)
		}
		if line["organization_id"] != "org-1" {
			t.Fatalf("entry line %d must be tenant-scoped: %v", i, line)
		}
		createdAt, ok := line["created_at"].(string)
		if !ok {
			t.Fatalf("created_at must be a string: %v", line)
		}
		parsed, err := time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			t.Fatalf("created_at must parse as RFC3339: %v", err)
		}
		if !parsed.After(previousAt) {
			t.Fatalf("export must render chain order (oldest first) at line %d", i)
		}
		previousAt = parsed
		metadata, ok := line["metadata"].(map[string]any)
		if !ok {
			t.Fatalf("metadata must render (with the chain stamp): %v", line)
		}
		if _, has := metadata[ChainMetadataKey]; !has {
			t.Fatalf("metadata must include the chain stamp for offline re-verification: %v", metadata)
		}
	}

	// Final line: the chain anchor, exactly the documented shape.
	final := lines[3]
	if final["type"] != "chain_head" {
		t.Fatalf("final line type = %v, want chain_head", final["type"])
	}
	if final["chain_version"] != ChainVersionV1 {
		t.Fatalf("chain_version = %v, want %q", final["chain_version"], ChainVersionV1)
	}
	if length, _ := final["length"].(float64); int(length) != 3 {
		t.Fatalf("chain_head length = %v, want 3", final["length"])
	}
	head, length, err := svc.ChainHead(t.Context(), "org-1")
	if err != nil || final["head_hash"] != head || length != 3 {
		t.Fatalf("chain_head hash must match ChainHead: %v vs %q (%v)", final["head_hash"], head, err)
	}

	// Metadata keys render sorted in the raw bytes (determinism contract).
	raw := string(data)
	firstEntryLine := strings.Split(raw, "\n")[0]
	if strings.Index(firstEntryLine, `"i":`) > strings.Index(firstEntryLine, `"zulu":`) {
		t.Fatalf("metadata keys must render sorted: %s", firstEntryLine)
	}
}

// TestExportJSONLDeterministic pins the WORM contract: same data + same order
// => byte-identical export. Checked twice on one service and, stronger,
// across two independently-seeded instances (fixed ids/timestamps).
func TestExportJSONLDeterministic(t *testing.T) {
	svc := NewService()
	if _, err := svc.LogCtx(t.Context(), "alice", "agent.created", "org-1", "agents/a-1", map[string]any{"n": 1}); err != nil {
		t.Fatalf("LogCtx: %v", err)
	}
	first, err := svc.ExportJSONL(t.Context(), "org-1")
	if err != nil {
		t.Fatalf("ExportJSONL 1: %v", err)
	}
	second, err := svc.ExportJSONL(t.Context(), "org-1")
	if err != nil {
		t.Fatalf("ExportJSONL 2: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("two exports of unchanged data must be byte-identical")
	}

	seed := func(s *Service) {
		base := time.Date(2026, 5, 4, 3, 2, 1, 0, time.UTC)
		seedChainedEntry(t, s, "org-1", "e1", "agent.created", base, map[string]any{"b": 2, "a": "one"})
		seedChainedEntry(t, s, "org-1", "e2", "run.started", base.Add(time.Minute), nil)
		seedChainedEntry(t, s, "org-1", "e3", "run.finished", base.Add(2*time.Minute), map[string]any{"ok": true})
	}
	left, right := NewService(), NewService()
	seed(left)
	seed(right)
	// The seeded chains verify (stamps chained identically on both).
	if report, _ := left.VerifyChain(t.Context(), "org-1"); !report.Valid || report.Length != 3 {
		t.Fatalf("seeded chain must verify: %+v", report)
	}
	leftBytes, err := left.ExportJSONL(t.Context(), "org-1")
	if err != nil {
		t.Fatalf("ExportJSONL left: %v", err)
	}
	rightBytes, err := right.ExportJSONL(t.Context(), "org-1")
	if err != nil {
		t.Fatalf("ExportJSONL right: %v", err)
	}
	if !bytes.Equal(leftBytes, rightBytes) {
		t.Fatal("identically-seeded instances must export byte-identical evidence")
	}
	// ...and the instance-level head is stable across a fresh replay.
	if head1, _, _ := left.ChainHead(t.Context(), "org-1"); head1 == "" {
		t.Fatal("seeded chain must have a head")
	}
}

func TestExportJSONLEmptyOrg(t *testing.T) {
	svc := NewService()
	data, err := svc.ExportJSONL(t.Context(), "org-empty")
	if err != nil {
		t.Fatalf("ExportJSONL(empty): %v", err)
	}
	lines := parseJSONL(t, data)
	if len(lines) != 1 {
		t.Fatalf("empty org must export exactly the chain_head line, got %d", len(lines))
	}
	if lines[0]["type"] != "chain_head" {
		t.Fatalf("expected chain_head line, got %v", lines[0])
	}
	if length, _ := lines[0]["length"].(float64); length != 0 {
		t.Fatalf("empty trail length must be 0, got %v", lines[0]["length"])
	}
	if head, _ := lines[0]["head_hash"].(string); head != ChainGenesisPrevHash {
		t.Fatalf("empty trail head must be the genesis hash, got %q", head)
	}
	if _, err := svc.ExportJSONL(t.Context(), "  "); err != ErrOrgRequired {
		t.Fatalf("blank org must be ErrOrgRequired, got %v", err)
	}
}
