package tools

// filestorage.go — the file storage tool (issue #111): a StorageBackend
// seam plus an org-scoped upload/download/list/delete tool with hard path
// sanitization and a 10 MiB object cap.
//
// Security model:
//
//   - The StorageBackend interface is the seam. The only built-in backend is
//     InMemoryStorageBackend (zero-infrastructure, per-process, for tests and
//     single-node deployments). A connector-backed HTTP implementation and an
//     S3-backed one are EXPLICIT FOLLOW-UP WORK — this file claims neither:
//     no S3 types, no SDK assumptions, no configuration knobs that pretend an
//     object store exists.
//   - Per-organization prefixing is enforced at the TOOL layer: the org id
//     comes from the run-scope context (policies.WithRunScope, stamped by the
//     worker) — never from tool input — and is passed to the backend as a
//     dedicated argument, so backends key storage as (orgID, path). One
//     tenant can never read, list, overwrite or delete another tenant's
//     objects even if a backend implementation is sloppy (tests assert the
//     isolation against the InMemory backend and the tool contract).
//   - Path sanitization REJECTS rather than rewrites: absolute paths, ".."
//     segments, "." segments, empty segments, backslashes and control
//     characters are refused (no silent normalization that could turn a
//     traversal into a legal-looking path).
//   - Size limit: uploads larger than DefaultFileStorageMaxObjectBytes
//     (10 MiB) are refused before the backend is touched; downloads of
//     objects above the same cap are refused (a connector-backed backend
//     could hold bigger objects; the tool never streams them into a run).
//   - Tenant scoping is fail-closed: without an organization run scope the
//     tool refuses to execute; with a nil backend it registers (so the
//     catalog stays complete) but fails closed at execution time.
import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"agentos/internal/policies"
)

// FileStorageToolName is the registry name of the file storage tool.
const FileStorageToolName = "file_storage"

// File storage defaults/hard caps (issue #111).
const (
	// DefaultFileStorageMaxObjectBytes is the 10 MiB object size limit.
	DefaultFileStorageMaxObjectBytes = 10 << 20
	// MaxFileStoragePathBytes bounds one sanitized path.
	MaxFileStoragePathBytes = 512
	// DefaultFileStorageListLimit bounds one list call.
	DefaultFileStorageListLimit = 100
	// MaxFileStorageListLimit is the hard list ceiling.
	MaxFileStorageListLimit = 1000
)

// File storage tool errors.
var (
	// ErrStorageRequired is returned when no storage backend is wired.
	ErrStorageRequired = errors.New("tools: no storage backend is wired")
	// ErrPathInvalid covers every rejected path shape (traversal, absolute,
	// empty segments, backslashes, control characters, size).
	ErrPathInvalid = errors.New("tools: storage path rejected")
	// ErrObjectNotFound is returned by backends when the path holds nothing.
	ErrObjectNotFound = errors.New("tools: object not found")
	// ErrObjectTooLarge covers both oversized uploads and oversized downloads.
	ErrObjectTooLarge = errors.New("tools: object exceeds the storage size limit")
)

// FileStorageObject is one listing entry (metadata only; content is never
// returned by List).
type FileStorageObject struct {
	Path        string `json:"path"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
}

// StorageBackend is the seam between the tool and object storage. Backends
// receive the organization id as a dedicated argument and MUST key objects as
// (orgID, path); all methods must be safe for concurrent use. The InMemory
// implementation below is the built-in reference; S3 and connector-backed
// HTTP backends are documented follow-up work (NOT claimed here).
type StorageBackend interface {
	Put(ctx context.Context, orgID, path string, content []byte, contentType string) error
	Get(ctx context.Context, orgID, path string) ([]byte, string, error)
	List(ctx context.Context, orgID, prefix string) ([]FileStorageObject, error)
	Delete(ctx context.Context, orgID, path string) error
}

// InMemoryStorageBackend is the zero-infrastructure StorageBackend: objects
// live in a per-process map keyed by (orgID, path). Lists are sorted by path
// for determinism. It is process-local BY DESIGN — data does not survive
// restarts and is never shared between processes.
type InMemoryStorageBackend struct {
	mu      sync.RWMutex
	objects map[string]memoryObject
}

type memoryObject struct {
	content     []byte
	contentType string
}

// NewInMemoryStorageBackend returns an empty backend.
func NewInMemoryStorageBackend() *InMemoryStorageBackend {
	return &InMemoryStorageBackend{objects: make(map[string]memoryObject)}
}

func memoryKey(orgID, path string) string { return orgID + "\x00" + path }

// Put implements StorageBackend.
func (b *InMemoryStorageBackend) Put(_ context.Context, orgID, path string, content []byte, contentType string) error {
	if b == nil {
		return ErrStorageRequired
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.objects[memoryKey(orgID, path)] = memoryObject{
		content:     append([]byte(nil), content...), // defensive copy: callers may reuse the buffer
		contentType: contentType,
	}
	return nil
}

// Get implements StorageBackend.
func (b *InMemoryStorageBackend) Get(_ context.Context, orgID, path string) ([]byte, string, error) {
	if b == nil {
		return nil, "", ErrStorageRequired
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	obj, ok := b.objects[memoryKey(orgID, path)]
	if !ok {
		return nil, "", fmt.Errorf("%w: %s", ErrObjectNotFound, path)
	}
	return append([]byte(nil), obj.content...), obj.contentType, nil
}

// List implements StorageBackend, sorted by path (deterministic).
func (b *InMemoryStorageBackend) List(_ context.Context, orgID, prefix string) ([]FileStorageObject, error) {
	if b == nil {
		return nil, ErrStorageRequired
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]FileStorageObject, 0)
	for key, obj := range b.objects {
		org, path, found := strings.Cut(key, "\x00")
		if !found || org != orgID {
			continue
		}
		if prefix != "" && !strings.HasPrefix(path, prefix) {
			continue
		}
		out = append(out, FileStorageObject{Path: path, Size: int64(len(obj.content)), ContentType: obj.contentType})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// Delete implements StorageBackend (idempotent: deleting a missing object is
// not an error — the tool surfaces deleted=true either way).
func (b *InMemoryStorageBackend) Delete(_ context.Context, orgID, path string) error {
	if b == nil {
		return ErrStorageRequired
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.objects, memoryKey(orgID, path))
	return nil
}

// FileStorageTool exposes upload/download/list/delete over a StorageBackend.
type FileStorageTool struct {
	backend   StorageBackend
	gate      *ToolPolicyGate
	maxObject int
}

// NewFileStorageTool returns the tool over the given backend. backend may be
// nil: the tool registers (so the catalog stays complete) but fails closed at
// execution time with ErrStorageRequired.
func NewFileStorageTool(backend StorageBackend) *FileStorageTool {
	return &FileStorageTool{
		backend:   backend,
		maxObject: DefaultFileStorageMaxObjectBytes,
	}
}

// WithPolicyGate attaches the per-tool policy gate (see policy.go).
func (t *FileStorageTool) WithPolicyGate(gate *ToolPolicyGate) *FileStorageTool {
	t.gate = gate
	return t
}

// Name implements Tool.
func (t *FileStorageTool) Name() string { return FileStorageToolName }

// DescribeTool publishes catalog metadata (issue #111).
func (t *FileStorageTool) DescribeTool() ToolInfo {
	return ToolInfo{
		Name: t.Name(),
		Description: "Uploads, downloads, lists and deletes org-scoped files " +
			"(10 MiB per object; paths are sanitized — traversal and absolute " +
			"paths are rejected). Backed by the platform storage seam; the " +
			"built-in backend is per-process memory.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"operation": map[string]any{
					"type":        "string",
					"enum":        []string{"upload", "download", "list", "delete"},
					"description": "Storage operation to perform.",
				},
				"path": map[string]any{
					"type":        "string",
					"description": "Relative object path (required for upload/download/delete; list prefix is optional). No leading '/', no '..' segments.",
				},
				"content": map[string]any{
					"type":        "string",
					"description": "upload only: object content (max 10 MiB).",
				},
				"content_type": map[string]any{
					"type":        "string",
					"description": "upload only: MIME type (defaults to application/octet-stream).",
				},
				"prefix": map[string]any{
					"type":        "string",
					"description": "list only: optional path prefix filter.",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "list only: maximum entries (default 100, max 1000).",
				},
				"estimated_cost_cents": map[string]any{
					"type":        "integer",
					"description": "Optional estimated cost surfaced to the policy engine (max_cost_cents condition).",
				},
			},
			"required": []string{"operation"},
		},
	}
}

// Execute implements Tool using a background context.
func (t *FileStorageTool) Execute(input map[string]any) (map[string]any, error) {
	return t.ExecuteContext(context.Background(), input)
}

// ExecuteContext implements ContextAware: the org scope comes from the run
// context (fail closed without one) and scopes every backend call.
func (t *FileStorageTool) ExecuteContext(ctx context.Context, input map[string]any) (map[string]any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if t == nil {
		return nil, errors.New("tools: file storage tool is not initialized")
	}
	if err := t.gate.Allow(ctx, t.Name(), toolEstimatedCostCents(input["estimated_cost_cents"])); err != nil {
		return nil, err
	}
	orgID := policies.RunScopeFromContext(ctx).OrganizationID
	if orgID == "" {
		return nil, errors.New("tools: organization scope is required (run context)")
	}
	if t.backend == nil {
		return nil, ErrStorageRequired
	}

	operation, err := stringArg(input["operation"])
	if err != nil {
		return nil, fmt.Errorf("%w: operation: %v", ErrInvalidToolInput, err)
	}
	switch strings.ToLower(strings.TrimSpace(operation)) {
	case "upload":
		return t.upload(ctx, orgID, input)
	case "download":
		return t.download(ctx, orgID, input)
	case "list":
		return t.list(ctx, orgID, input)
	case "delete":
		return t.delete(ctx, orgID, input)
	default:
		return nil, fmt.Errorf("%w: operation %q is not allowed (upload|download|list|delete)", ErrInvalidToolInput, operation)
	}
}

// upload stores one object under the org scope.
func (t *FileStorageTool) upload(ctx context.Context, orgID string, input map[string]any) (map[string]any, error) {
	path, err := sanitizeStoragePath(input["path"])
	if err != nil {
		return nil, err
	}
	content, err := stringArg(input["content"])
	if err != nil {
		return nil, fmt.Errorf("%w: content: %v", ErrInvalidToolInput, err)
	}
	if len(content) > t.maxObject {
		return nil, fmt.Errorf("%w: upload of %d bytes exceeds the %d byte limit", ErrObjectTooLarge, len(content), t.maxObject)
	}
	contentType := "application/octet-stream"
	if v, err := stringArg(input["content_type"]); err == nil {
		contentType = strings.TrimSpace(v)
	}
	if err := t.backend.Put(ctx, orgID, path, []byte(content), contentType); err != nil {
		return nil, fmt.Errorf("storing %s failed: %v", path, err)
	}
	return map[string]any{
		"operation":    "upload",
		"path":         path,
		"size":         len(content),
		"content_type": contentType,
	}, nil
}

// download fetches one object (refusing objects above the tool's size cap).
func (t *FileStorageTool) download(ctx context.Context, orgID string, input map[string]any) (map[string]any, error) {
	path, err := sanitizeStoragePath(input["path"])
	if err != nil {
		return nil, err
	}
	content, contentType, err := t.backend.Get(ctx, orgID, path)
	if err != nil {
		return nil, err
	}
	if len(content) > t.maxObject {
		return nil, fmt.Errorf("%w: %s is %d bytes (limit %d)", ErrObjectTooLarge, path, len(content), t.maxObject)
	}
	return map[string]any{
		"operation":    "download",
		"path":         path,
		"content":      string(content),
		"size":         len(content),
		"content_type": contentType,
	}, nil
}

// list returns metadata for the org's objects under the optional prefix.
func (t *FileStorageTool) list(ctx context.Context, orgID string, input map[string]any) (map[string]any, error) {
	prefix := ""
	if v, err := stringArg(input["prefix"]); err == nil {
		var perr error
		prefix, perr = sanitizeStoragePrefix(v)
		if perr != nil {
			return nil, perr
		}
	}
	limit := storageListLimit(input["limit"])
	objects, err := t.backend.List(ctx, orgID, prefix)
	if err != nil {
		return nil, fmt.Errorf("listing %q failed: %v", prefix, err)
	}
	if len(objects) > limit {
		objects = objects[:limit]
	}
	out := make([]map[string]any, 0, len(objects))
	for _, obj := range objects {
		out = append(out, map[string]any{
			"path":         obj.Path,
			"size":         obj.Size,
			"content_type": obj.ContentType,
		})
	}
	return map[string]any{
		"operation": "list",
		"prefix":    prefix,
		"objects":   out,
		"count":     len(out),
	}, nil
}

// delete removes one object (idempotent like the backend).
func (t *FileStorageTool) delete(ctx context.Context, orgID string, input map[string]any) (map[string]any, error) {
	path, err := sanitizeStoragePath(input["path"])
	if err != nil {
		return nil, err
	}
	if err := t.backend.Delete(ctx, orgID, path); err != nil {
		return nil, fmt.Errorf("deleting %s failed: %v", path, err)
	}
	return map[string]any{"operation": "delete", "path": path, "deleted": true}, nil
}

// storageListLimit clamps the list "limit" into [1, MaxFileStorageListLimit].
func storageListLimit(value any) int {
	limit := DefaultFileStorageListLimit
	switch v := value.(type) {
	case float64:
		if v >= 1 {
			limit = int(v)
		}
	case int:
		if v >= 1 {
			limit = v
		}
	case int64:
		if v >= 1 {
			limit = int(v)
		}
	}
	if limit > MaxFileStorageListLimit {
		return MaxFileStorageListLimit
	}
	return limit
}

// sanitizeStoragePath validates a tool-supplied object path. It REJECTS
// instead of rewriting: paths must be relative, non-empty, at most
// MaxFileStoragePathBytes, built of non-empty "."/".."-free segments, and
// free of backslashes and control characters.
func sanitizeStoragePath(value any) (string, error) {
	raw, err := stringArg(value)
	if err != nil {
		return "", fmt.Errorf("%w: path: %v", ErrInvalidToolInput, err)
	}
	path := strings.TrimSpace(raw)
	if path == "" {
		return "", fmt.Errorf("%w: path is required", ErrPathInvalid)
	}
	if len(path) > MaxFileStoragePathBytes {
		return "", fmt.Errorf("%w: path exceeds %d bytes", ErrPathInvalid, MaxFileStoragePathBytes)
	}
	if strings.HasPrefix(path, "/") || strings.HasPrefix(path, `\`) {
		return "", fmt.Errorf("%w: absolute paths are not allowed", ErrPathInvalid)
	}
	if strings.ContainsRune(path, '\\') {
		return "", fmt.Errorf("%w: backslashes are not allowed", ErrPathInvalid)
	}
	for _, r := range path {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("%w: control characters are not allowed", ErrPathInvalid)
		}
	}
	for _, segment := range strings.Split(path, "/") {
		switch segment {
		case "", ".", "..":
			return "", fmt.Errorf("%w: path segment %q is not allowed", ErrPathInvalid, segment)
		}
	}
	return path, nil
}

// sanitizeStoragePrefix validates a list prefix: same character rules as
// paths, but a trailing "/" is allowed (it is a boundary, not an empty
// segment) and the empty prefix means "everything".
func sanitizeStoragePrefix(raw string) (string, error) {
	prefix := strings.TrimSpace(raw)
	if prefix == "" {
		return "", nil
	}
	// A prefix is allowed to end with "/" (a boundary the path validator
	// would reject as an empty segment), so validate the trimmed form.
	trimmed := strings.TrimSuffix(prefix, "/")
	if trimmed == "" {
		return "", fmt.Errorf("%w: prefix %q is not allowed", ErrPathInvalid, prefix)
	}
	if _, err := sanitizeStoragePath(trimmed); err != nil {
		return "", err
	}
	return prefix, nil
}
