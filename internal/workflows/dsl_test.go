package workflows

import (
	"strings"
	"testing"
)

func validAgentNode(id, agentID string) Node {
	return Node{ID: id, Type: NodeAgent, Config: map[string]any{"agent_id": agentID}}
}

func TestValidateDSLValidGraphReturnsNil(t *testing.T) {
	dsl := DSL{
		Nodes: []Node{
			validAgentNode("n1", "agent-1"),
			{ID: "n2", Type: NodeTool, Config: map[string]any{"tool_id": "tool-1", "agent_id": "agent-1"}},
			{ID: "n3", Type: NodeCondition, Config: map[string]any{"expression": "ok"}},
			{ID: "n4", Type: NodeParallel},
			{ID: "n5", Type: NodeApproval},
			{ID: "n6", Type: NodeDelay, Config: map[string]any{"seconds": 5}},
			{ID: "n7", Type: NodeWebhook, Config: map[string]any{"url": "https://example.test/hook"}},
		},
		Edges: []Edge{
			{From: "n1", To: "n2", Condition: EdgeOnSuccess},
			{From: "n2", To: "n3", Condition: EdgeAlways},
			{From: "n3", To: "n4", Condition: EdgeOnFailure},
		},
	}
	if errs := ValidateDSL(dsl); errs != nil {
		t.Fatalf("valid DSL should pass validation, got %#v", errs)
	}
}

func TestValidateDSLEmptyGraph(t *testing.T) {
	errs := ValidateDSL(DSL{})
	if len(errs) != 1 || errs[0].Code != "empty_graph" {
		t.Fatalf("expected single empty_graph error, got %#v", errs)
	}
}

func TestValidateDSLNodeErrors(t *testing.T) {
	cases := []struct {
		name     string
		node     Node
		wantCode string
	}{
		{"missing id", Node{Type: NodeAgent, Config: map[string]any{"agent_id": "a"}}, "missing_node_id"},
		{"duplicate id", validAgentNode("dup", "a"), "duplicate_node_id"},
		{"unknown type", Node{ID: "x", Type: NodeType("quantum")}, "unknown_node_type"},
		{"agent missing agent_id", Node{ID: "x", Type: NodeAgent}, "missing_config"},
		{"tool missing tool_id", Node{ID: "x", Type: NodeTool}, "missing_config"},
		{"condition missing expression", Node{ID: "x", Type: NodeCondition}, "missing_config"},
		{"delay missing seconds", Node{ID: "x", Type: NodeDelay}, "missing_config"},
		{"delay zero seconds", Node{ID: "x", Type: NodeDelay, Config: map[string]any{"seconds": 0}}, "missing_config"},
		{"webhook missing url", Node{ID: "x", Type: NodeWebhook}, "missing_config"},
	}
	for _, tc := range cases {
		// Two nodes so the duplicate-id case actually collides.
		dsl := DSL{Nodes: []Node{validAgentNode("dup", "seed"), tc.node}}
		errs := ValidateDSL(dsl)
		if len(errs) == 0 || errs[0].Code != tc.wantCode {
			t.Fatalf("%s: expected code %q, got %#v", tc.name, tc.wantCode, errs)
		}
		if errs[0].NodeID != tc.node.ID && tc.wantCode != "missing_node_id" && tc.wantCode != "duplicate_node_id" {
			t.Fatalf("%s: expected node_id %q, got %#v", tc.name, tc.node.ID, errs[0])
		}
	}
}

func TestValidateDSLEdgeErrors(t *testing.T) {
	cases := []struct {
		name     string
		edges    []Edge
		wantCode string
	}{
		{"missing target", []Edge{{From: "n1", To: "ghost"}}, "missing_node_ref"},
		{"missing source", []Edge{{From: "ghost", To: "n1"}}, "missing_node_ref"},
		{"invalid condition", []Edge{{From: "n1", To: "n1", Condition: "sometimes"}}, "invalid_edge_condition"},
	}
	for _, tc := range cases {
		dsl := DSL{Nodes: []Node{validAgentNode("n1", "a")}, Edges: tc.edges}
		errs := ValidateDSL(dsl)
		if len(errs) == 0 || errs[0].Code != tc.wantCode {
			t.Fatalf("%s: expected code %q, got %#v", tc.name, tc.wantCode, errs)
		}
	}
}

func TestValidateDSLCycleDetection(t *testing.T) {
	// a -> b -> c -> a
	dsl := DSL{
		Nodes: []Node{validAgentNode("a", "1"), validAgentNode("b", "2"), validAgentNode("c", "3")},
		Edges: []Edge{{From: "a", To: "b"}, {From: "b", To: "c"}, {From: "c", To: "a"}},
	}
	errs := ValidateDSL(dsl)
	if len(errs) != 1 || errs[0].Code != "cycle_detected" {
		t.Fatalf("expected cycle_detected, got %#v", errs)
	}
	if errs[0].NodeID == "" {
		t.Fatalf("cycle error should name the offending node, got %#v", errs[0])
	}

	// self loop
	selfLoop := DSL{
		Nodes: []Node{validAgentNode("a", "1")},
		Edges: []Edge{{From: "a", To: "a"}},
	}
	if errs := ValidateDSL(selfLoop); len(errs) != 1 || errs[0].Code != "cycle_detected" {
		t.Fatalf("expected cycle_detected for self loop, got %#v", errs)
	}

	// diamond (a->b, a->c, b->d, c->d) is NOT a cycle
	diamond := DSL{
		Nodes: []Node{validAgentNode("a", "1"), validAgentNode("b", "2"), validAgentNode("c", "3"), validAgentNode("d", "4")},
		Edges: []Edge{{From: "a", To: "b"}, {From: "a", To: "c"}, {From: "b", To: "d"}, {From: "c", To: "d"}},
	}
	if errs := ValidateDSL(diamond); errs != nil {
		t.Fatalf("diamond graph should be acyclic, got %#v", errs)
	}
}

func TestTopoOrderRespectsDependencies(t *testing.T) {
	dsl := DSL{
		Nodes: []Node{validAgentNode("a", "1"), validAgentNode("b", "2"), validAgentNode("c", "3"), validAgentNode("d", "4")},
		Edges: []Edge{{From: "a", To: "b"}, {From: "a", To: "c"}, {From: "b", To: "d"}, {From: "c", To: "d"}},
	}
	order, err := TopoOrder(dsl)
	if err != nil {
		t.Fatalf("TopoOrder returned error: %v", err)
	}
	position := make(map[string]int, len(order))
	for i, id := range order {
		position[id] = i
	}
	if len(position) != 4 {
		t.Fatalf("expected all 4 nodes in order, got %v", order)
	}
	if position["a"] > position["b"] || position["a"] > position["c"] || position["b"] > position["d"] || position["c"] > position["d"] {
		t.Fatalf("order violates dependencies: %v", order)
	}
	// Deterministic: same input, same order.
	again, err := TopoOrder(dsl)
	if err != nil || strings.Join(again, ",") != strings.Join(order, ",") {
		t.Fatalf("TopoOrder should be deterministic: %v vs %v", order, again)
	}
}

func TestTopoOrderCycleErrors(t *testing.T) {
	dsl := DSL{
		Nodes: []Node{validAgentNode("a", "1"), validAgentNode("b", "2")},
		Edges: []Edge{{From: "a", To: "b"}, {From: "b", To: "a"}},
	}
	if _, err := TopoOrder(dsl); err == nil {
		t.Fatal("TopoOrder should fail on a cyclic graph")
	}
}
