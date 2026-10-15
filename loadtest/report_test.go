package loadtest

import (
	"bytes"
	"encoding/json"
	"testing"
)

// TestFanoutDLSShape pins the workflow-stress DAG: entry -> N branches ->
// join, every executable node carrying config.agent_id (the DSL validator
// rejects agent nodes without it).
func TestFanoutDLSShape(t *testing.T) {
	dsl := FanoutDSL("agent-1", 3)
	raw, err := json.Marshal(dsl)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var parsed struct {
		Nodes []struct {
			ID     string         `json:"id"`
			Type   string         `json:"type"`
			Config map[string]any `json:"config"`
		} `json:"nodes"`
		Edges []struct {
			From      string `json:"from"`
			To        string `json:"to"`
			Condition string `json:"condition"`
		} `json:"edges"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(parsed.Nodes) != 5 { // entry + 3 branches + join
		t.Fatalf("nodes = %d, want 5", len(parsed.Nodes))
	}
	if len(parsed.Edges) != 6 { // 2 per branch
		t.Fatalf("edges = %d, want 6", len(parsed.Edges))
	}
	for i, n := range parsed.Nodes {
		if n.ID == "" {
			t.Fatalf("node %d has no id", i)
		}
		if n.Type != "agent" {
			t.Fatalf("node %s type = %s, want agent", n.ID, n.Type)
		}
		if n.Config["agent_id"] != "agent-1" {
			t.Fatalf("node %s missing config.agent_id: %v", n.ID, n.Config)
		}
	}
	// Fan-out topology: entry feeds every branch; every branch feeds join.
	outOfEntry := 0
	intoJoin := 0
	for _, e := range parsed.Edges {
		if e.From == "entry" {
			outOfEntry++
		}
		if e.To == "join" {
			intoJoin++
		}
		if e.Condition != "on_success" {
			t.Fatalf("edge %v->%v condition = %q, want on_success", e.From, e.To, e.Condition)
		}
	}
	if outOfEntry != 3 || intoJoin != 3 {
		t.Fatalf("fan-out shape broken: entry out=%d, join in=%d", outOfEntry, intoJoin)
	}

	// Width 1 still forms a valid chain (entry -> fan_0 -> join).
	one := FanoutDSL("a", 1)
	b, _ := json.Marshal(one)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	nodes := m["nodes"].([]any)
	edges := m["edges"].([]any)
	if len(nodes) != 3 || len(edges) != 2 {
		t.Fatalf("width-1 DAG = %d nodes / %d edges, want 3/2", len(nodes), len(edges))
	}
}

// TestReportJSONRoundTrip pins the report schema: a report survives a JSON
// round trip and keeps the fields the comparator and baselines rely on.
func TestReportJSONRoundTrip(t *testing.T) {
	b := newReportBuilder("sustained", DefaultConfig())
	b.op("create_run").add(201, 201, 1.5)
	b.op("healthz").add(200, 200, 0.3)
	b.setupOp("provision").add(201, 201, 120)
	rep := b.build(0, nil, map[string]any{"k": "v"})
	rep.Base = "http://127.0.0.1:8080"

	raw, err := func() ([]byte, error) {
		var buf bytes.Buffer
		if err := rep.WriteJSON(&buf); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}()
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	var back Report
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Tool != ToolName || back.Scenario != "sustained" {
		t.Fatalf("tool/scenario = %s/%s", back.Tool, back.Scenario)
	}
	if len(back.Ops) != 2 || back.Ops[0].Op != "create_run" || back.Ops[0].P95Ms != 1.5 {
		t.Fatalf("ops = %+v", back.Ops)
	}
	if len(back.SetupOps) != 1 || back.SetupOps[0].Op != "setup.provision" {
		t.Fatalf("setup ops = %+v", back.SetupOps)
	}
	if back.Totals.Requests != 2 || back.Totals.ErrorRate != 0 {
		t.Fatalf("totals = %+v (setup must be excluded)", back.Totals)
	}
	if back.Extra["k"] != "v" {
		t.Fatalf("extra = %+v", back.Extra)
	}
}

// TestSetupOpExcludedFromCompareCheck verifies the setup-op prefix used by
// the comparator gate.
func TestSetupOpExcludedFromCompareCheck(t *testing.T) {
	if !isSetupOp("setup.provision") {
		t.Fatal("setup.provision must be flagged as a setup op")
	}
	if isSetupOp("create_run") || isSetupOp("setupfoo") {
		t.Fatal("non-setup ops misclassified")
	}
}
