package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"agentos/internal/policies"
)

// storageCtx carries the org scope the storage tool requires.
func storageCtx(org string) context.Context {
	return policies.WithRunScope(context.Background(), policies.RunScope{
		OrganizationID: org,
		Environment:    "production",
	})
}

func TestFileStorageLifecycle(t *testing.T) {
	tool := NewFileStorageTool(NewInMemoryStorageBackend())
	ctx := storageCtx("org-1")

	// Empty list renders an honest empty state.
	out, err := tool.ExecuteContext(ctx, map[string]any{"operation": "list"})
	if err != nil {
		t.Fatalf("initial list failed: %v", err)
	}
	if out["count"] != 0 {
		t.Fatalf("expected empty list, got %#v", out)
	}

	// Upload two objects (one with a default content type).
	if _, err := tool.ExecuteContext(ctx, map[string]any{
		"operation": "upload", "path": "reports/q3.csv",
		"content": "a,b\n1,2\n", "content_type": "text/csv",
	}); err != nil {
		t.Fatalf("upload failed: %v", err)
	}
	up, err := tool.ExecuteContext(ctx, map[string]any{
		"operation": "upload", "path": "notes/hello.txt", "content": "hi",
	})
	if err != nil {
		t.Fatalf("upload failed: %v", err)
	}
	if up["content_type"] != "application/octet-stream" || up["size"] != 2 || up["path"] != "notes/hello.txt" {
		t.Fatalf("unexpected upload result: %#v", up)
	}

	// List is sorted and metadata-only.
	out, err = tool.ExecuteContext(ctx, map[string]any{"operation": "list", "prefix": "reports/"})
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	objects, _ := out["objects"].([]map[string]any)
	if out["count"] != 1 || len(objects) != 1 || objects[0]["path"] != "reports/q3.csv" || objects[0]["size"] != int64(8) {
		t.Fatalf("unexpected list output: %#v", out)
	}
	if _, hasContent := objects[0]["content"]; hasContent {
		t.Fatal("list entries must be metadata-only")
	}

	// Full list is deterministic (sorted by path).
	out, _ = tool.ExecuteContext(ctx, map[string]any{"operation": "list"})
	objects, _ = out["objects"].([]map[string]any)
	if len(objects) != 2 || objects[0]["path"] != "notes/hello.txt" || objects[1]["path"] != "reports/q3.csv" {
		t.Fatalf("expected sorted paths, got %#v", objects)
	}

	// Download round-trips the bytes.
	down, err := tool.ExecuteContext(ctx, map[string]any{"operation": "download", "path": "reports/q3.csv"})
	if err != nil {
		t.Fatalf("download failed: %v", err)
	}
	if down["content"] != "a,b\n1,2\n" || down["content_type"] != "text/csv" {
		t.Fatalf("unexpected download: %#v", down)
	}

	// Delete is idempotent; afterwards download fails typed.
	del, err := tool.ExecuteContext(ctx, map[string]any{"operation": "delete", "path": "reports/q3.csv"})
	if err != nil || del["deleted"] != true {
		t.Fatalf("delete failed: %v %#v", err, del)
	}
	if _, err := tool.ExecuteContext(ctx, map[string]any{"operation": "delete", "path": "reports/q3.csv"}); err != nil {
		t.Fatalf("re-delete should stay idempotent: %v", err)
	}
	if _, err := tool.ExecuteContext(ctx, map[string]any{"operation": "download", "path": "reports/q3.csv"}); !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("expected ErrObjectNotFound after delete, got %v", err)
	}

	// Download of a never-written object fails typed too.
	if _, err := tool.ExecuteContext(ctx, map[string]any{"operation": "download", "path": "nope.txt"}); !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("expected ErrObjectNotFound, got %v", err)
	}
}

func TestFileStorageTenantIsolation(t *testing.T) {
	tool := NewFileStorageTool(NewInMemoryStorageBackend())
	if _, err := tool.ExecuteContext(storageCtx("org-1"), map[string]any{
		"operation": "upload", "path": "secret.txt", "content": "org one data",
	}); err != nil {
		t.Fatalf("upload failed: %v", err)
	}

	// Another org sees neither the object nor its listing.
	if _, err := tool.ExecuteContext(storageCtx("org-2"), map[string]any{
		"operation": "download", "path": "secret.txt",
	}); !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("cross-tenant download must fail typed, got %v", err)
	}
	out, err := tool.ExecuteContext(storageCtx("org-2"), map[string]any{"operation": "list"})
	if err != nil {
		t.Fatalf("org-2 list failed: %v", err)
	}
	if out["count"] != 0 {
		t.Fatalf("org-2 must not see org-1 objects: %#v", out)
	}

	// org-2 can safely create its own object at the same path.
	if _, err := tool.ExecuteContext(storageCtx("org-2"), map[string]any{
		"operation": "upload", "path": "secret.txt", "content": "org two data",
	}); err != nil {
		t.Fatalf("org-2 upload failed: %v", err)
	}
	down, err := tool.ExecuteContext(storageCtx("org-2"), map[string]any{"operation": "download", "path": "secret.txt"})
	if err != nil || down["content"] != "org two data" {
		t.Fatalf("org-2 object damaged: %v %#v", err, down)
	}
	down, err = tool.ExecuteContext(storageCtx("org-1"), map[string]any{"operation": "download", "path": "secret.txt"})
	if err != nil || down["content"] != "org one data" {
		t.Fatalf("org-1 object damaged: %v %#v", err, down)
	}

	// org-2 deleting its copy leaves org-1's object intact.
	if _, err := tool.ExecuteContext(storageCtx("org-2"), map[string]any{"operation": "delete", "path": "secret.txt"}); err != nil {
		t.Fatalf("org-2 delete failed: %v", err)
	}
	if _, err := tool.ExecuteContext(storageCtx("org-1"), map[string]any{"operation": "download", "path": "secret.txt"}); err != nil {
		t.Fatalf("org-1 object must survive org-2 delete: %v", err)
	}
}

func TestFileStorageTraversalRejection(t *testing.T) {
	tool := NewFileStorageTool(NewInMemoryStorageBackend())
	ctx := storageCtx("org-1")
	cases := []struct {
		name string
		path any
	}{
		{"parent traversal", "../etc/passwd"},
		{"nested traversal", "reports/../../etc/passwd"},
		{"dot-dot segment", "reports/.."},
		{"dot segment", "reports/./x"},
		{"absolute path", "/etc/passwd"},
		{"absolute windows drive", `\etc\passwd`},
		{"backslash anywhere", `reports\q3.csv`},
		{"empty path", "   "},
		{"missing path", nil},
		{"empty segment", "reports//q3.csv"},
		{"control character", "reports/\x00evil"},
		{"only dots", ".."},
		{"oversized path", strings.Repeat("a/", MaxFileStoragePathBytes)},
		{"non-string path", 42},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, op := range []string{"upload", "download", "delete"} {
				_, err := tool.ExecuteContext(ctx, map[string]any{"operation": op, "path": tc.path, "content": "x"})
				if err == nil {
					t.Fatalf("expected %s to reject %q", op, tc.path)
				}
				if !errors.Is(err, ErrPathInvalid) && !errors.Is(err, ErrInvalidToolInput) {
					t.Fatalf("expected a path/input rejection, got %v", err)
				}
			}
			// List prefixes obey the same rules.
			if _, err := tool.ExecuteContext(ctx, map[string]any{"operation": "list", "prefix": "../"}); !errors.Is(err, ErrPathInvalid) {
				t.Fatalf("expected prefix traversal rejection, got %v", err)
			}
		})
	}
}

func TestFileStorageSizeAndListCaps(t *testing.T) {
	tool := NewFileStorageTool(NewInMemoryStorageBackend())
	ctx := storageCtx("org-1")

	// Oversized upload refused before the backend is touched.
	big := strings.Repeat("x", DefaultFileStorageMaxObjectBytes+1)
	if _, err := tool.ExecuteContext(ctx, map[string]any{
		"operation": "upload", "path": "big.bin", "content": big,
	}); !errors.Is(err, ErrObjectTooLarge) {
		t.Fatalf("expected ErrObjectTooLarge, got %v", err)
	}
	out, _ := tool.ExecuteContext(ctx, map[string]any{"operation": "list"})
	if out["count"] != 0 {
		t.Fatalf("rejected upload must not have stored anything: %#v", out)
	}

	// A direct oversized write (bypassing the tool, e.g. a future connector
	// backend) is refused on download.
	oversized := NewInMemoryStorageBackend()
	if err := oversized.Put(ctx, "org-1", "huge.bin", make([]byte, DefaultFileStorageMaxObjectBytes+1), "application/octet-stream"); err != nil {
		t.Fatalf("backend Put failed: %v", err)
	}
	toolHuge := NewFileStorageTool(oversized)
	if _, err := toolHuge.ExecuteContext(ctx, map[string]any{"operation": "download", "path": "huge.bin"}); !errors.Is(err, ErrObjectTooLarge) {
		t.Fatalf("expected download refusal for oversized object, got %v", err)
	}

	// At-limit upload succeeds (boundary).
	exact := strings.Repeat("x", DefaultFileStorageMaxObjectBytes)
	if _, err := tool.ExecuteContext(ctx, map[string]any{
		"operation": "upload", "path": "exact.bin", "content": exact,
	}); err != nil {
		t.Fatalf("boundary upload should pass: %v", err)
	}

	// List limit clamps.
	if got := storageListLimit(float64(99999)); got != MaxFileStorageListLimit {
		t.Fatalf("list limit must clamp to %d, got %d", MaxFileStorageListLimit, got)
	}
	if got := storageListLimit(nil); got != DefaultFileStorageListLimit {
		t.Fatalf("list limit default should be %d, got %d", DefaultFileStorageListLimit, got)
	}
}

func TestFileStorageFailClosedAndValidation(t *testing.T) {
	// No backend wired -> registered but fails closed.
	unwired := NewFileStorageTool(nil)
	if _, err := unwired.ExecuteContext(storageCtx("org-1"), map[string]any{
		"operation": "list",
	}); !errors.Is(err, ErrStorageRequired) {
		t.Fatalf("expected ErrStorageRequired, got %v", err)
	}

	// No org scope -> refused before the backend is consulted.
	tool := NewFileStorageTool(NewInMemoryStorageBackend())
	if _, err := tool.ExecuteContext(context.Background(), map[string]any{"operation": "list"}); err == nil ||
		!strings.Contains(err.Error(), "organization scope") {
		t.Fatalf("expected organization-scope refusal, got %v", err)
	}

	// Unknown operation.
	if _, err := tool.ExecuteContext(storageCtx("org-1"), map[string]any{"operation": "sync"}); err == nil {
		t.Fatal("expected unknown-operation rejection")
	}
	// Upload without content.
	if _, err := tool.ExecuteContext(storageCtx("org-1"), map[string]any{"operation": "upload", "path": "x.txt"}); err == nil {
		t.Fatal("expected missing-content rejection")
	}
}

func TestFileStoragePolicyGateDeniesBeforeBackend(t *testing.T) {
	backend := NewInMemoryStorageBackend()
	gate := NewToolPolicyGate(policies.EvaluatorFunc(func(_ context.Context, _ string, _ policies.EvaluateRequest) (policies.Decision, error) {
		return policies.Decision{Decision: policies.EffectDeny, Reason: "storage paused"}, nil
	}))
	tool := NewFileStorageTool(backend).WithPolicyGate(gate)
	_, err := tool.ExecuteContext(storageCtx("org-1"), map[string]any{"operation": "list"})
	if !errors.Is(err, ToolPolicyDenied) {
		t.Fatalf("expected ToolPolicyDenied, got %v", err)
	}
	out, _ := backend.List(context.Background(), "org-1", "")
	if len(out) != 0 {
		t.Fatalf("denied call must not touch the backend: %#v", out)
	}
}

func TestFileStorageDescribeToolSchema(t *testing.T) {
	info := NewFileStorageTool(nil).DescribeTool()
	props, _ := info.InputSchema["properties"].(map[string]any)
	for _, key := range []string{"operation", "path", "content", "content_type", "prefix", "limit"} {
		if _, ok := props[key]; !ok {
			t.Fatalf("schema should document %q: %v", key, info.InputSchema)
		}
	}
	if req, ok := info.InputSchema["required"].([]string); !ok || len(req) != 1 || req[0] != "operation" {
		t.Fatalf("schema should require operation, got %#v", info.InputSchema["required"])
	}
}
