package tools

import (
	"context"
	"database/sql"
	"reflect"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"agentos/internal/policies"
)

// unknownTool is a bare Tool (no Described implementation) so the listing
// degrades to name-only entries for unannotated tools.
type unknownTool struct{}

func (unknownTool) Name() string { return "mystery" }

func (unknownTool) Execute(map[string]any) (map[string]any, error) { return nil, nil }

func TestDefaultRegistryContents(t *testing.T) {
	registry := DefaultRegistry()
	names := registry.Names()
	// issue #111: the expanded built-in catalog (calculator + http_request
	// from issue #18, plus the six new tools registered additively through
	// catalog_builtin.go).
	want := []string{
		"calculator", CodeExecToolName, DatabaseToolName, FileStorageToolName,
		HTTPToolName, JSONTransformToolName, SearchToolName, WebhookToolName,
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("DefaultRegistry should register exactly the built-in tools %v, got %v", want, names)
	}
}

func TestDefaultRegistryDescribesExpandedCatalog(t *testing.T) {
	// Every expanded-catalog tool must publish a description and an input
	// schema (issue #111: "JSON schemas following the Calculator/HTTP
	// pattern"), and each schema must carry object type + required keys.
	byName := make(map[string]ToolInfo)
	for _, info := range DefaultRegistry().List() {
		byName[info.Name] = info
	}
	for _, name := range []string{
		DatabaseToolName, SearchToolName, CodeExecToolName,
		WebhookToolName, JSONTransformToolName, FileStorageToolName,
	} {
		info, ok := byName[name]
		if !ok {
			t.Fatalf("%s missing from the default catalog", name)
		}
		if info.Description == "" {
			t.Errorf("%s should publish a description", name)
		}
		if info.InputSchema["type"] != "object" {
			t.Errorf("%s input schema should be an object schema: %v", name, info.InputSchema)
		}
		props, _ := info.InputSchema["properties"].(map[string]any)
		if len(props) == 0 {
			t.Errorf("%s input schema should document properties", name)
		}
		if req, ok := info.InputSchema["required"].([]string); !ok || len(req) == 0 {
			t.Errorf("%s input schema should declare required keys: %v", name, info.InputSchema["required"])
		}
	}
	// database_query must document its security posture in the schema:
	// connection + sql are the required arguments.
	props, _ := byName[DatabaseToolName].InputSchema["properties"].(map[string]any)
	for _, key := range []string{"connection", "sql", "params", "row_limit", "readonly", "timeout_ms"} {
		if _, ok := props[key]; !ok {
			t.Errorf("database_query schema should document %q", key)
		}
	}
}

func TestRegisterBuiltinCatalogWithOptionsWiresSeams(t *testing.T) {
	sink := NewMemoryAuditSink(4)
	backend := NewInMemoryStorageBackend()

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer func() { _ = db.Close() }()
	mock.ExpectPrepare(regexp.QuoteMeta("SELECT 1")).ExpectQuery().WillReturnRows(sqlmock.NewRows([]string{"one"}).AddRow("1"))

	registry := NewRegistry()
	err = RegisterBuiltinCatalogWithOptions(registry, BuiltinCatalogOptions{
		Audit:            sink,
		Search:           SearchConfig{EndpointURL: "https://search.example.internal"},
		WebhookAllowList: []string{"https://hooks.example.com/"},
		Database: DBConnectionProviderFunc(func(_ context.Context, orgID, name string) (*sql.DB, error) {
			return db, nil
		}),
		FileStorage: backend,
	})
	if err != nil {
		t.Fatalf("RegisterBuiltinCatalogWithOptions failed: %v", err)
	}
	names := registry.Names()
	// Only the six new tools: the options entry point is purely additive
	// (calculator/http_request are registered by the caller, see
	// DefaultRegistry for the combined wiring).
	want := []string{
		CodeExecToolName, DatabaseToolName, FileStorageToolName,
		JSONTransformToolName, SearchToolName, WebhookToolName,
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("options registration should register exactly the six tools %v, got %v", want, names)
	}

	// The wired database seam is live: a query runs end-to-end and is audited.
	tool, _ := registry.Get(DatabaseToolName)
	dbTool, ok := tool.(interface {
		ExecuteContext(context.Context, map[string]any) (map[string]any, error)
	})
	if !ok {
		t.Fatalf("%s should implement ContextAware", DatabaseToolName)
	}
	ctx := policies.WithRunScope(context.Background(), policies.RunScope{OrganizationID: "org-1"})
	if _, err := dbTool.ExecuteContext(ctx, map[string]any{
		"connection": "warehouse", "sql": "SELECT 1",
	}); err != nil {
		t.Fatalf("wired database tool failed: %v", err)
	}
	if len(sink.Events()) != 1 {
		t.Fatalf("wired audit sink should hold one event, got %d", len(sink.Events()))
	}
	// The wired file storage backend is live too.
	stored, _ := registry.Get(FileStorageToolName)
	storageTool, ok := stored.(interface {
		ExecuteContext(context.Context, map[string]any) (map[string]any, error)
	})
	if !ok {
		t.Fatalf("%s should implement ContextAware", FileStorageToolName)
	}
	if _, err := storageTool.ExecuteContext(ctx, map[string]any{
		"operation": "upload", "path": "x.txt", "content": "hi",
	}); err != nil {
		t.Fatalf("wired storage tool failed: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

func TestRegisterBuiltinCatalogRejectsBadWebhookAllowList(t *testing.T) {
	registry := NewRegistry()
	err := RegisterBuiltinCatalogWithOptions(registry, BuiltinCatalogOptions{
		WebhookAllowList: []string{"ftp://hooks.example.com/"},
	})
	if err == nil {
		t.Fatal("invalid allow-list entries must fail registration")
	}
	// The registry stays usable; the webhook must NOT be registered (the
	// registration aborted before it), later tools are not registered either.
	if _, ok := registry.Get(WebhookToolName); ok {
		t.Fatal("webhook must not be registered after a failed allow-list parse")
	}
	if _, ok := registry.Get(JSONTransformToolName); ok {
		t.Fatal("tools after the webhook must not be registered on failure")
	}
	// The default entry point is safe with any registry, even nil.
	if err := RegisterBuiltinCatalog(nil); err != nil {
		t.Fatalf("nil registry should be a no-op, got %v", err)
	}
}

func TestRegistryListSortedAndComplete(t *testing.T) {
	registry := NewRegistry()
	// Register out of order to prove List() sorts by name.
	registry.Register(NewHTTPRequestTool())
	registry.Register(NewCalculatorTool())
	registry.Register(unknownTool{})

	list := registry.List()
	if len(list) != 3 {
		t.Fatalf("expected 3 catalog entries, got %d: %+v", len(list), list)
	}
	wantOrder := []string{"calculator", "http_request", "mystery"}
	for i, want := range wantOrder {
		if list[i].Name != want {
			t.Fatalf("entry %d should be %q, got %q (list: %+v)", i, want, list[i].Name, list)
		}
	}
}

func TestRegistryListDescribedMetadata(t *testing.T) {
	registry := DefaultRegistry()
	list := registry.List()
	byName := make(map[string]ToolInfo, len(list))
	for _, info := range list {
		byName[info.Name] = info
	}

	calc, ok := byName["calculator"]
	if !ok {
		t.Fatal("calculator missing from catalog")
	}
	if calc.Description == "" {
		t.Error("calculator should publish a description")
	}
	if calc.InputSchema["type"] != "object" {
		t.Errorf("calculator input schema should be an object schema, got %v", calc.InputSchema)
	}
	props, _ := calc.InputSchema["properties"].(map[string]any)
	if _, ok := props["expression"]; !ok {
		t.Errorf("calculator input schema should document the expression property: %v", calc.InputSchema)
	}
	if req, ok := calc.InputSchema["required"].([]string); !ok || len(req) != 1 || req[0] != "expression" {
		t.Errorf("calculator input schema should require expression, got %v", calc.InputSchema["required"])
	}

	httpTool, ok := byName[HTTPToolName]
	if !ok {
		t.Fatal("http_request missing from catalog")
	}
	if httpTool.Description == "" {
		t.Error("http_request should publish a description")
	}
	props, _ = httpTool.InputSchema["properties"].(map[string]any)
	for _, key := range []string{"url", "method", "headers", "body", "timeout_ms"} {
		if _, ok := props[key]; !ok {
			t.Errorf("http_request input schema should document %q: %v", key, httpTool.InputSchema)
		}
	}
	if req, ok := httpTool.InputSchema["required"].([]string); !ok || len(req) != 1 || req[0] != "url" {
		t.Errorf("http_request input schema should require url, got %v", httpTool.InputSchema["required"])
	}
}

func TestRegistryListNameOnlyForUndescribedTools(t *testing.T) {
	registry := NewRegistry()
	registry.Register(unknownTool{})
	list := registry.List()
	if len(list) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(list))
	}
	if list[0].Name != "mystery" {
		t.Errorf("registry key should be the authoritative name, got %q", list[0].Name)
	}
	if list[0].Description != "" || list[0].InputSchema != nil {
		t.Errorf("undescribed tool should carry no catalog metadata: %+v", list[0])
	}
}

func TestRegistryListNilAndEmptySafe(t *testing.T) {
	var nilRegistry *Registry
	list := nilRegistry.List()
	if list == nil || len(list) != 0 {
		t.Fatalf("nil registry should list as empty non-nil slice, got %#v", list)
	}
	if got := NewRegistry().List(); got == nil || len(got) != 0 {
		t.Fatalf("empty registry should list as empty non-nil slice, got %#v", got)
	}
}
