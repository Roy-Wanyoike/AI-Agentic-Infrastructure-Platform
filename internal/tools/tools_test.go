package tools

import "testing"

func TestCalculatorTool(t *testing.T) {
	tool := NewCalculatorTool()
	result, err := tool.Execute(map[string]any{"expression": "2 + 2"})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if result["result"] != 4 {
		t.Fatalf("expected result 4, got %#v", result["result"])
	}
}

func TestRegistry(t *testing.T) {
	registry := NewRegistry()
	registry.Register(NewCalculatorTool())
	if _, ok := registry.Get("calculator"); !ok {
		t.Fatal("expected calculator tool to be registered")
	}
}
