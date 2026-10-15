package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"agentos/internal/policies"
)

// jsonDoc decodes a JSON literal into the generic tool-argument shape.
func jsonDoc(t *testing.T, raw string) any {
	t.Helper()
	var doc any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("test fixture is not valid JSON: %v", err)
	}
	return doc
}

// transformResult marshals the tool's result value back to JSON for
// readable table assertions.
func transformResult(t *testing.T, out map[string]any) string {
	t.Helper()
	blob, err := json.Marshal(out["result"])
	if err != nil {
		t.Fatalf("result is not JSON: %v", err)
	}
	return string(blob)
}

func TestJSONTransformTableDriven(t *testing.T) {
	tool := NewJSONTransformTool()
	cases := []struct {
		name    string
		data    string
		ops     string
		want    string // expected result JSON
		wantErr bool
	}{
		{
			name: "pick dot path",
			data: `{"a":{"b":{"c":42}}}`,
			ops:  `[{"op":"pick","path":"a.b.c"}]`,
			want: `42`,
		},
		{
			name: "pick bracket index",
			data: `{"rows":[10,20,30]}`,
			ops:  `[{"op":"pick","path":"rows[1]"}]`,
			want: `20`,
		},
		{
			name: "pick quoted key with dots",
			data: `{"weird.key":{"inner":1}}`,
			ops:  `[{"op":"pick","path":"[\"weird.key\"].inner"}]`,
			want: `1`,
		},
		{
			name: "pick jq root marker",
			data: `{"a":[{"n":1},{"n":2}]}`,
			ops:  `[{"op":"pick","path":"$.a[1].n"}]`,
			want: `2`,
		},
		{
			name:    "pick missing key errors",
			data:    `{"a":1}`,
			ops:     `[{"op":"pick","path":"nope"}]`,
			wantErr: true,
		},
		{
			name:    "pick index into object errors",
			data:    `{"a":{"b":1}}`,
			ops:     `[{"op":"pick","path":"a[0]"}]`,
			wantErr: true,
		},
		{
			name: "filter numeric less-than",
			data: `{"items":[{"p":5},{"p":15},{"p":9}]}`,
			ops:  `[{"op":"filter","path":"items","field":"p","cmp":"<","value":10}]`,
			want: `[{"p":5},{"p":9}]`,
		},
		{
			name: "filter equality and string compare",
			data: `[{"name":"alpha","tier":"gold"},{"name":"beta","tier":"silver"}]`,
			ops:  `[{"op":"filter","field":"tier","cmp":"==","value":"gold"}]`,
			want: `[{"name":"alpha","tier":"gold"}]`,
		},
		{
			name: "filter contains on string field",
			data: `[{"tag":"prod-eu"},{"tag":"dev"}]`,
			ops:  `[{"op":"filter","field":"tag","cmp":"contains","value":"prod"}]`,
			want: `[{"tag":"prod-eu"}]`,
		},
		{
			name: "filter exists",
			data: `[{"a":1},{"b":2}]`,
			ops:  `[{"op":"filter","field":"a","cmp":"exists"}]`,
			want: `[{"a":1}]`,
		},
		{
			name: "filter mismatched types exclude (jq-like)",
			data: `[{"p":"high"},{"p":7}]`,
			ops:  `[{"op":"filter","field":"p","cmp":">","value":1}]`,
			want: `[{"p":7}]`,
		},
		{
			name: "filter element itself without field",
			data: `[3,10,7]`,
			ops:  `[{"op":"filter","cmp":">=","value":7}]`,
			want: `[10,7]`,
		},
		{
			name: "filter missing field excludes element",
			data: `[{"p":1},{"q":2}]`,
			ops:  `[{"op":"filter","field":"p","cmp":"!=","value":999}]`,
			want: `[{"p":1}]`,
		},
		{
			name: "map with sub-expression templates",
			data: `{"items":[{"id":"a","price":2},{"id":"b","price":4}]}`,
			ops:  `[{"op":"map","path":"items","template":{"sku":"$.id","total":"$.price","currency":"USD"}}]`,
			want: `[{"currency":"USD","sku":"a","total":2},{"currency":"USD","sku":"b","total":4}]`,
		},
		{
			name: "map whole element via $",
			data: `[1,2]`,
			ops:  `[{"op":"map","template":{"value":"$"}}]`,
			want: `[{"value":1},{"value":2}]`,
		},
		{
			name: "map missing sub-path becomes null (documented)",
			data: `[{"id":"a"},{"id":"b","note":"x"}]`,
			ops:  `[{"op":"map","template":{"id":"$.id","note":"$.note"}}]`,
			want: `[{"id":"a","note":null},{"id":"b","note":"x"}]`,
		},
		{
			name: "template splices typed values",
			data: `{"user":{"name":"Ada"},"count":3}`,
			ops:  `[{"op":"template","body":{"who":"{{user.name}}","n":"{{count}}","note":"Ada wrote {{count}} items"}}]`,
			want: `{"n":3,"note":"Ada wrote 3 items","who":"Ada"}`,
		},
		{
			name: "template nested bodies",
			data: `{"env":"prod"}`,
			ops:  `[{"op":"template","body":{"meta":{"env":"{{env}}","tags":["{{env}}-a"]}}}]`,
			want: `{"meta":{"env":"prod","tags":["prod-a"]}}`,
		},
		{
			name:    "template missing path errors",
			data:    `{"a":1}`,
			ops:     `[{"op":"template","body":{"x":"{{missing}}"}}]`,
			wantErr: true,
		},
		{
			name: "chained pick-filter-map",
			data: `{"items":[{"price":9,"id":"a"},{"price":20,"id":"b"}]}`,
			ops: `[{"op":"pick","path":"items"},
                               {"op":"filter","field":"price","cmp":"<","value":10},
                               {"op":"map","template":{"sku":"$.id"}}]`,
			want: `[{"sku":"a"}]`,
		},
		{
			// 1 and 1.0 both match (numeric unification); the canonical
			// JSON round-trip re-encodes 1.0 as 1.
			name: "numeric unification 1 == 1.0",
			data: `[{"n":1},{"n":1.0},{"n":2}]`,
			ops:  `[{"op":"filter","field":"n","cmp":"==","value":1}]`,
			want: `[{"n":1},{"n":1}]`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := tool.Execute(map[string]any{
				"data": jsonDoc(t, tc.data),
				"ops":  jsonDoc(t, tc.ops),
			})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got result %s", transformResult(t, out))
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := transformResult(t, out); got != tc.want {
				t.Fatalf("result mismatch:\n got: %s\nwant: %s", got, tc.want)
			}
			if applied, _ := out["ops_applied"].(int); applied != len(jsonDoc(t, tc.ops).([]any)) {
				t.Fatalf("ops_applied should equal the op count, got %#v", out["ops_applied"])
			}
		})
	}
}

func TestJSONTransformDeterministicAndPure(t *testing.T) {
	tool := NewJSONTransformTool()
	input := map[string]any{
		"data": jsonDoc(t, `{"items":[{"a":1},{"a":2}]}`),
		"ops":  jsonDoc(t, `[{"op":"map","path":"items","template":{"v":"$.a"}}]`),
	}
	first, err := tool.Execute(input)
	if err != nil {
		t.Fatalf("first execution failed: %v", err)
	}
	for i := 0; i < 25; i++ {
		again, err := tool.Execute(input)
		if err != nil {
			t.Fatalf("repeat execution %d failed: %v", i, err)
		}
		if transformResult(t, first) != transformResult(t, again) {
			t.Fatalf("transform is not deterministic: %s vs %s", transformResult(t, first), transformResult(t, again))
		}
	}
	// Purity: the input document is copied, never mutated.
	if got := transformResult(t, map[string]any{"result": input["data"]}); got != `{"items":[{"a":1},{"a":2}]}` {
		t.Fatalf("input document was mutated: %s", got)
	}
}

func TestJSONTransformValidation(t *testing.T) {
	tool := NewJSONTransformTool()
	cases := []struct {
		name  string
		input map[string]any
	}{
		{"missing data", map[string]any{"ops": []any{map[string]any{"op": "pick", "path": "a"}}}},
		{"missing ops", map[string]any{"data": map[string]any{}}},
		{"empty ops", map[string]any{"data": map[string]any{"a": 1}, "ops": []any{}}},
		{"ops not array", map[string]any{"data": map[string]any{"a": 1}, "ops": "pick"}},
		{"op not object", map[string]any{"data": map[string]any{"a": 1}, "ops": []any{"pick"}}},
		{"unknown op", map[string]any{"data": map[string]any{"a": 1}, "ops": []any{map[string]any{"op": "exec"}}}},
		{"pick missing path", map[string]any{"data": map[string]any{"a": 1}, "ops": []any{map[string]any{"op": "pick"}}}},
		{"filter missing cmp", map[string]any{"data": []any{}, "ops": []any{map[string]any{"op": "filter"}}}},
		{"filter bad cmp", map[string]any{"data": []any{}, "ops": []any{map[string]any{"op": "filter", "cmp": ">>", "value": 1}}}},
		{"filter on non-array", map[string]any{"data": map[string]any{"a": 1}, "ops": []any{map[string]any{"op": "filter", "path": "a", "cmp": ">", "value": 0}}}},
		{"map missing template", map[string]any{"data": []any{1}, "ops": []any{map[string]any{"op": "map"}}}},
		{"map path misses array", map[string]any{"data": map[string]any{"a": 1}, "ops": []any{map[string]any{"op": "map", "path": "a", "template": map[string]any{}}}}},
		{"template missing body", map[string]any{"data": map[string]any{}, "ops": []any{map[string]any{"op": "template"}}}},
		{"unterminated bracket", map[string]any{"data": map[string]any{}, "ops": []any{map[string]any{"op": "pick", "path": "a[0"}}}},
		{"empty segment", map[string]any{"data": map[string]any{}, "ops": []any{map[string]any{"op": "pick", "path": "a..b"}}}},
		{"non-numeric index", map[string]any{"data": map[string]any{}, "ops": []any{map[string]any{"op": "pick", "path": "a[xyz]"}}}},
		{"oversized path", map[string]any{"data": map[string]any{}, "ops": []any{map[string]any{"op": "pick", "path": strings.Repeat("a.", MaxJSONPathBytes)}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tool.Execute(tc.input); err == nil {
				t.Fatalf("expected rejection for %s", tc.name)
			}
		})
	}
	// The too-many-ops fixture above must exceed the op cap only after the
	// cap is exceeded in general; assert the cap is what fired for the last
	// case by checking a dedicated oversized op list.
	ops := make([]any, MaxJSONTransformOps+1)
	for i := range ops {
		ops[i] = map[string]any{"op": "pick", "path": "a"}
	}
	_, err := tool.Execute(map[string]any{"data": map[string]any{"a": 1}, "ops": ops})
	if !errors.Is(err, ErrJSONTransform) || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("expected op-cap rejection, got %v", err)
	}
}

func TestJSONTransformSizeCaps(t *testing.T) {
	small := NewJSONTransformTool()
	small.maxIn = 32
	_, err := small.Execute(map[string]any{
		"data": map[string]any{"blob": strings.Repeat("x", 64)},
		"ops":  []any{map[string]any{"op": "pick", "path": "blob"}},
	})
	if !errors.Is(err, ErrJSONTransform) || !strings.Contains(err.Error(), "data exceeds") {
		t.Fatalf("expected input cap rejection, got %v", err)
	}

	out := NewJSONTransformTool()
	out.maxOut = 16
	_, err = out.Execute(map[string]any{
		"data": map[string]any{"blob": strings.Repeat("x", 64)},
		"ops":  []any{map[string]any{"op": "pick", "path": "blob"}},
	})
	if !errors.Is(err, ErrJSONTransform) || !strings.Contains(err.Error(), "result exceeds") {
		t.Fatalf("expected result cap rejection, got %v", err)
	}
}

func TestJSONTransformPolicyGateDenies(t *testing.T) {
	gate := NewToolPolicyGate(policies.EvaluatorFunc(func(_ context.Context, _ string, _ policies.EvaluateRequest) (policies.Decision, error) {
		return policies.Decision{Decision: policies.EffectDeny, Reason: "transforms paused"}, nil
	}))
	tool := NewJSONTransformTool().WithPolicyGate(gate)
	ctx := policies.WithRunScope(context.Background(), policies.RunScope{OrganizationID: "org-1"})
	_, err := tool.ExecuteContext(ctx, map[string]any{
		"data": map[string]any{"a": 1},
		"ops":  []any{map[string]any{"op": "pick", "path": "a"}},
	})
	if !errors.Is(err, ToolPolicyDenied) {
		t.Fatalf("expected ToolPolicyDenied, got %v", err)
	}
}
