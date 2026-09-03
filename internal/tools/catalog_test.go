package tools

import (
	"reflect"
	"testing"
)

// unknownTool is a bare Tool (no Described implementation) so the listing
// degrades to name-only entries for unannotated tools.
type unknownTool struct{}

func (unknownTool) Name() string { return "mystery" }

func (unknownTool) Execute(map[string]any) (map[string]any, error) { return nil, nil }

func TestDefaultRegistryContents(t *testing.T) {
	registry := DefaultRegistry()
	names := registry.Names()
	want := []string{"calculator", HTTPToolName}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("DefaultRegistry should register exactly the built-in tools %v, got %v", want, names)
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
