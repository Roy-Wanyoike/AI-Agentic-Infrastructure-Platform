package workflows

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// envWithOutputs builds an EvalEnv with pre-recorded node outputs and context.
func envWithOutputs(outputs map[string]string, ctx map[string]any) EvalEnv {
	env := NewEvalEnv()
	for id, doc := range outputs {
		env.Outputs[id] = json.RawMessage(doc)
	}
	for k, v := range ctx {
		env.Context[k] = v
	}
	return env
}

func TestEvaluateExpressionLiteralsAndArithmetic(t *testing.T) {
	env := NewEvalEnv()
	cases := []struct {
		name string
		expr string
		want any
	}{
		{"number literal", "42", float64(42)},
		{"float literal", "2.5", 2.5},
		{"string literal", `'hello'`, "hello"},
		{"double-quoted string", `"hello"`, "hello"},
		{"string escapes", `'a\nb\tc\"d\\e'`, "a\nb\tc\"d\\e"},
		{"true", "true", true},
		{"false", "false", false},
		{"null", "null", nil},
		{"addition", "1 + 2", float64(3)},
		{"subtraction", "10 - 4", float64(6)},
		{"multiplication", "6 * 7", float64(42)},
		{"division", "9 / 2", 4.5},
		{"negation", "-5 + 1", float64(-4)},
		{"string concatenation", `'foo' + 'bar'`, "foobar"},
		{"precedence mul over add", "1 + 2 * 3", float64(7)},
		{"precedence add left-assoc", "10 - 3 - 2", float64(5)},
		{"parens override precedence", "(1 + 2) * 3", float64(9)},
		{"nested parens", "((1 + 2) * (3 + 4))", float64(21)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EvaluateExpression(tc.expr, env)
			if err != nil {
				t.Fatalf("EvaluateExpression(%q) returned error: %v", tc.expr, err)
			}
			if got != tc.want {
				t.Fatalf("EvaluateExpression(%q) = %#v, want %#v", tc.expr, got, tc.want)
			}
		})
	}
}

func TestEvaluateExpressionComparisonsAndLogic(t *testing.T) {
	env := envWithOutputs(nil, nil)
	cases := []struct {
		name string
		expr string
		want bool
	}{
		{"eq numbers", "1 == 1.0", true},
		{"eq numbers false", "1 == 2", false},
		{"neq", "1 != 2", true},
		{"lt", "2 < 10", true},
		{"lte", "2 <= 2", true},
		{"gt", "10 > 2", true},
		{"gte", "3 >= 4", false},
		{"string order", `'apple' < 'banana'`, true},
		{"string order false", `'zebra' < 'apple'`, false},
		{"eq strings", `'a' == 'a'`, true},
		{"eq across types is false not error", `1 == '1'`, false},
		{"eq null", "null == null", true},
		{"eq null vs value", "null == 0", false},
		{"neq null vs value", "null != 0", true},
		{"and truthy", "true and 1", true},
		{"and falsy", "true and 0", false},
		{"or truthy", "false or 'x'", true},
		{"or falsy", "false or ''", false},
		{"not", "not false", true},
		{"not bang alias", "!false", true},
		{"double not", "not not true", true},
		{"precedence and over or", "false and false or true", true},
		{"parens flip precedence", "false and (false or true)", false},
		{"comparison binds tighter than and", "1 < 2 and 3 > 2", true},
		{"arithmetic in comparison", "2 + 2 == 4", true},
		{"unary minus in comparison", "-1 < 0", true},
		{"short-circuit or skips errors", "true or $.nodes.missing.output", true},
		{"short-circuit and skips errors", "false and $.nodes.missing.output", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EvaluateCondition(tc.expr, env)
			if err != nil {
				t.Fatalf("EvaluateCondition(%q) returned error: %v", tc.expr, err)
			}
			if got != tc.want {
				t.Fatalf("EvaluateCondition(%q) = %v, want %v", tc.expr, got, tc.want)
			}
		})
	}
}

func TestEvaluateExpressionPathReferences(t *testing.T) {
	env := envWithOutputs(map[string]string{
		"classify": `{"category": "urgent", "score": 0.9, "tags": ["a", "b"]}`,
		"fetch":    `{"items": [10, 20, 30], "meta": {"count": 3}}`,
		"plain":    `"scalar"`,
		"number":   `7`,
		"nested":   `{"output": "deep"}`,
	}, map[string]any{"tenant": "acme", "flags": map[string]any{"beta": true}})
	cases := []struct {
		name string
		expr string
		want any
	}{
		{"node output whole doc", "$.nodes.fetch.output", map[string]any{"items": []any{float64(10), float64(20), float64(30)}, "meta": map[string]any{"count": float64(3)}}},
		{"node output field", "$.nodes.classify.output.category", "urgent"},
		{"node output shorthand without output marker", "$.nodes.classify.category", "urgent"},
		{"nested array index", "$.nodes.fetch.output.items[1]", float64(20)},
		{"nested object path", "$.nodes.fetch.output.meta.count", float64(3)},
		{"scalar output doc", "$.nodes.plain.output", "scalar"},
		{"numeric output doc", "$.nodes.number.output", float64(7)},
		{"literal key named output", "$.nodes.nested.output.output", "deep"},
		{"input root", "$.input", ""},
		{"context key", "$.context.tenant", "acme"},
		{"context nested path", "$.context.flags.beta", true},
		{"loop index", "$.loop.index", float64(-1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EvaluateExpression(tc.expr, env)
			if err != nil {
				t.Fatalf("EvaluateExpression(%q) returned error: %v", tc.expr, err)
			}
			if !jsonEqual(got, tc.want) {
				t.Fatalf("EvaluateExpression(%q) = %#v, want %#v", tc.expr, got, tc.want)
			}
		})
	}

	// $.item reflects the loop frame when set.
	env.Item = map[string]any{"id": float64(7)}
	env.LoopIndex = 2
	got, err := EvaluateExpression("$.item.id + $.loop.index", env)
	if err != nil {
		t.Fatalf("item expression returned error: %v", err)
	}
	if got != float64(9) {
		t.Fatalf("$.item.id + $.loop.index = %#v, want 9", got)
	}
}

func TestEvaluateExpressionErrors(t *testing.T) {
	env := envWithOutputs(map[string]string{"x": `{"output": {"n": 1}}`}, nil)
	cases := []struct {
		name     string
		expr     string
		wantCode string
	}{
		{"unknown identifier", "foo == 1", ExprErrUnknownIdent},
		{"bare identifier", "enabled", ExprErrUnknownIdent},
		{"unknown function", "len($.nodes.x.output) > 0", ExprErrUnknownFunction},
		{"unknown function no args", "now()", ExprErrUnknownFunction},
		{"unknown root", "$.secrets.api_key == 'x'", ExprErrUnknownIdent},
		{"unknown node id", "$.nodes.missing.output == 1", ExprErrUnknownIdent},
		{"missing path key", "$.nodes.x.output.nope == 1", ExprErrPathNotFound},
		{"array index out of range", "$.nodes.x.output[5] == 1", ExprErrPathNotFound},
		{"key on array", "$.nodes.x.output[0].name == 1", ExprErrPathNotFound},
		{"unknown context key", "$.context.missing == 1", ExprErrUnknownIdent},
		{"type mismatch arithmetic", "1 + 'a'", ExprErrTypeMismatch},
		{"type mismatch string concat", "'a' + 1", ExprErrTypeMismatch},
		{"type mismatch ordering", "1 < 'a'", ExprErrTypeMismatch},
		{"type mismatch ordering null", "null < 1", ExprErrTypeMismatch},
		{"type mismatch unary minus", "-'a'", ExprErrTypeMismatch},
		{"division by zero", "1 / 0", ExprErrDivisionByZero},
		{"syntax trailing token", "1 2", ExprErrSyntax},
		{"syntax unclosed paren", "(1 + 2", ExprErrSyntax},
		{"syntax lone op", "* 3", ExprErrSyntax},
		{"syntax bare assignment", "1 = 2", ExprErrSyntax},
		{"syntax path no segment", "$ == 1", ExprErrSyntax},
		{"syntax path number after dot", "$.nodes.1 == 1", ExprErrSyntax},
		{"syntax unterminated string", `'abc == 1`, ExprErrSyntax},
		{"syntax bad escape", `'a\qb' == 1`, ExprErrSyntax},
		{"syntax input descend", "$.input.sub == 1", ExprErrPathNotFound},
		{"empty expression", "   ", ExprErrSyntax},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := EvaluateExpression(tc.expr, env)
			if err == nil {
				t.Fatalf("EvaluateExpression(%q) expected error, got nil", tc.expr)
			}
			var ee *ExprError
			if !asExprError(err, &ee) {
				t.Fatalf("EvaluateExpression(%q) error type = %T, want *ExprError", tc.expr, err)
			}
			if ee.Code != tc.wantCode {
				t.Fatalf("EvaluateExpression(%q) error code = %q, want %q (%v)", tc.expr, ee.Code, tc.wantCode, err)
			}
		})
	}
}

func TestEvaluateExpressionCaps(t *testing.T) {
	env := NewEvalEnv()

	// Length cap: 4KB.
	long := strings.Repeat("1+", MaxExpressionLength/2) + "1"
	_, err := EvaluateExpression(long, env)
	var ee *ExprError
	if err == nil || !asExprError(err, &ee) || ee.Code != ExprErrLengthExceeded {
		t.Fatalf("over-long expression should be expr_length_exceeded, got %v", err)
	}

	// Depth cap: nested parens beyond 64.
	deep := strings.Repeat("(", MaxExpressionDepth+1) + "1" + strings.Repeat(")", MaxExpressionDepth+1)
	_, err = EvaluateExpression(deep, env)
	if err == nil || !asExprError(err, &ee) || ee.Code != ExprErrDepthExceeded {
		t.Fatalf("over-deep expression should be expr_depth_exceeded, got %v", err)
	}

	// Depth cap via unary chain (no parens).
	unaries := strings.Repeat("not ", MaxExpressionDepth+1) + "true"
	_, err = EvaluateExpression(unaries, env)
	if err == nil || !asExprError(err, &ee) || ee.Code != ExprErrDepthExceeded {
		t.Fatalf("over-deep unary chain should be expr_depth_exceeded, got %v", err)
	}

	// At the caps the engine still works.
	okDeep := strings.Repeat("(", MaxExpressionDepth) + "1" + strings.Repeat(")", MaxExpressionDepth)
	if got, err := EvaluateExpression(okDeep, env); err != nil || got != float64(1) {
		t.Fatalf("expression exactly at depth cap should evaluate, got %v err %v", got, err)
	}
}

func TestEvaluateConditionTruthiness(t *testing.T) {
	env := NewEvalEnv()
	cases := []struct {
		expr string
		want bool
	}{
		{"true", true},
		{"null", false},
		{"0", false},
		{"0.0", false},
		{"1", true},
		{"''", false},
		{"'x'", true},
		{"$.nodes.empty.output", false},    // [] -> falsy
		{"$.nodes.items.output", true},     // [1] -> truthy
		{"$.nodes.obj.output", true},       // {"a":1} -> truthy
		{"$.nodes.emptyobj.output", false}, // {} -> falsy
		{"$.nodes.nullout.output", false},  // null output -> falsy
	}
	outputs := map[string]string{
		"empty":    `[]`,
		"items":    `[1]`,
		"obj":      `{"a": 1}`,
		"emptyobj": `{}`,
		"nullout":  `null`,
	}
	for id, doc := range outputs {
		env.Outputs[id] = json.RawMessage(doc)
	}
	for _, tc := range cases {
		got, err := EvaluateCondition(tc.expr, env)
		if err != nil {
			t.Fatalf("EvaluateCondition(%q) returned error: %v", tc.expr, err)
		}
		if got != tc.want {
			t.Fatalf("EvaluateCondition(%q) = %v, want %v", tc.expr, got, tc.want)
		}
	}
}

func TestEvaluateExpressionErrorPosition(t *testing.T) {
	_, err := EvaluateExpression("1 + foo", NewEvalEnv())
	ee, ok := err.(*ExprError)
	if !ok {
		t.Fatalf("expected *ExprError, got %T", err)
	}
	if ee.Pos != 4 {
		t.Fatalf("expected error position 4, got %d", ee.Pos)
	}
	if !strings.Contains(ee.Error(), "expr_unknown_identifier") {
		t.Fatalf("Error() should carry the machine code, got %q", ee.Error())
	}
}

func TestBranchTargetsFromConfig(t *testing.T) {
	got := BranchTargetsFromConfig(map[string]any{
		"branches": map[string]any{"true": "n-yes", "false": "n-no"},
	})
	if got.True != "n-yes" || got.False != "n-no" {
		t.Fatalf("BranchTargetsFromConfig = %#v", got)
	}
	if empty := BranchTargetsFromConfig(nil); empty.True != "" || empty.False != "" {
		t.Fatalf("missing branches should yield empty targets, got %#v", empty)
	}
	if partial := BranchTargetsFromConfig(map[string]any{"branches": map[string]any{"true": 42}}); partial.True != "" || partial.False != "" {
		t.Fatalf("non-string targets should be ignored, got %#v", partial)
	}
}

func TestLoopConfigFromNode(t *testing.T) {
	n := Node{ID: "l1", Type: NodeLoop, Config: map[string]any{
		"expression":     "$.nodes.src.output.items",
		"max_iterations": float64(7),
		"body":           []any{"b1", "b2", "  ", 42},
	}}
	cfg := LoopConfigFromNode(n)
	if cfg.Expression != "$.nodes.src.output.items" || cfg.MaxIterations != 7 || len(cfg.Body) != 2 {
		t.Fatalf("LoopConfigFromNode = %#v", cfg)
	}

	// Defaults + hard cap.
	def := LoopConfigFromNode(Node{ID: "l2", Type: NodeLoop, Config: map[string]any{"expression": "true"}})
	if def.MaxIterations != DefaultLoopMaxIterations {
		t.Fatalf("default max_iterations = %d, want %d", def.MaxIterations, DefaultLoopMaxIterations)
	}
	capped := LoopConfigFromNode(Node{ID: "l3", Type: NodeLoop, Config: map[string]any{"expression": "true", "max_iterations": float64(9999999)}})
	if capped.MaxIterations != MaxLoopIterationsHardCap {
		t.Fatalf("max_iterations should clamp to the hard cap, got %d", capped.MaxIterations)
	}
}

func TestRetryPolicyDelayFor(t *testing.T) {
	p := RetryPolicy{Attempts: 5, BaseDelay: 100 * time.Millisecond, Multiplier: 2, MaxDelay: 800 * time.Millisecond}
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 100 * time.Millisecond},
		{2, 200 * time.Millisecond},
		{3, 400 * time.Millisecond},
		{4, 800 * time.Millisecond}, // capped
		{5, 800 * time.Millisecond},
		{0, 100 * time.Millisecond}, // clamped to first attempt
	}
	for _, tc := range cases {
		if got := p.DelayFor(tc.attempt); got != tc.want {
			t.Fatalf("DelayFor(%d) = %v, want %v", tc.attempt, got, tc.want)
		}
	}

	// Multiplier < 1 falls through to the max delay (fail-safe).
	bad := RetryPolicy{Attempts: 3, BaseDelay: time.Second, Multiplier: 0.5, MaxDelay: 50 * time.Millisecond}
	if got := bad.DelayFor(2); got != 50*time.Millisecond {
		t.Fatalf("sub-unity multiplier should fall back to max delay, got %v", got)
	}
}

func TestRetryConfigFromNode(t *testing.T) {
	target, policy := RetryConfigFromNode(Node{ID: "r1", Type: NodeRetry, Config: map[string]any{
		"target":   "flaky",
		"attempts": float64(5),
		"backoff":  map[string]any{"base_delay_ms": float64(10), "multiplier": float64(3), "max_delay_ms": float64(90)},
	}})
	if target != "flaky" {
		t.Fatalf("target = %q, want flaky", target)
	}
	if policy.Attempts != 5 || policy.BaseDelay != 10*time.Millisecond || policy.Multiplier != 3 || policy.MaxDelay != 90*time.Millisecond {
		t.Fatalf("policy = %#v", policy)
	}

	// Defaults when config omits everything.
	defTarget, defPolicy := RetryConfigFromNode(Node{ID: "r2", Type: NodeRetry})
	if defTarget != "" || defPolicy.Attempts != DefaultRetryAttempts {
		t.Fatalf("defaults = %q %#v", defTarget, defPolicy)
	}
	// Hard cap.
	_, capped := RetryConfigFromNode(Node{ID: "r3", Type: NodeRetry, Config: map[string]any{"attempts": float64(100000)}})
	if capped.Attempts != MaxRetryAttemptsHardCap {
		t.Fatalf("attempts should clamp to the hard cap, got %d", capped.Attempts)
	}
}

func TestWaitUntilConfigFromNode(t *testing.T) {
	cfg := WaitUntilConfigFromNode(Node{ID: "w1", Type: NodeWaitUntil, Config: map[string]any{
		"expression":       "$.nodes.dep.output.ready",
		"timeout_seconds":  float64(5),
		"poll_interval_ms": float64(20),
	}})
	if cfg.Expression != "$.nodes.dep.output.ready" || cfg.Timeout != 5*time.Second || cfg.PollInterval != 20*time.Millisecond {
		t.Fatalf("WaitUntilConfigFromNode = %#v", cfg)
	}
	def := WaitUntilConfigFromNode(Node{ID: "w2", Type: NodeWaitUntil, Config: map[string]any{"expression": "true"}})
	if def.Timeout != DefaultWaitTimeout || def.PollInterval != DefaultWaitPollInterval {
		t.Fatalf("defaults = %#v", def)
	}
	floored := WaitUntilConfigFromNode(Node{ID: "w3", Type: NodeWaitUntil, Config: map[string]any{"expression": "true", "poll_interval_ms": float64(0)}})
	if floored.PollInterval < MinWaitPollInterval {
		t.Fatalf("poll interval should be floored at %v, got %v", MinWaitPollInterval, floored.PollInterval)
	}
}

func TestSwitchMatchValue(t *testing.T) {
	cases := []struct {
		v    any
		want string
	}{
		{nil, "null"},
		{"urgent", "urgent"},
		{true, "true"},
		{false, "false"},
		{float64(42), "42"},
		{float64(2.5), "2.5"},
		{[]any{"a"}, `["a"]`},
	}
	for _, tc := range cases {
		if got := SwitchMatchValue(tc.v); got != tc.want {
			t.Fatalf("SwitchMatchValue(%#v) = %q, want %q", tc.v, got, tc.want)
		}
	}
}

func TestSwitchValueMatches(t *testing.T) {
	if !switchValueMatches("urgent", "urgent") || switchValueMatches("urgent", "low") {
		t.Fatal("string case matching broken")
	}
	if !switchValueMatches(float64(3), "3") {
		t.Fatal("numeric case matching broken")
	}
	if !switchValueMatches(true, "true") || !switchValueMatches(nil, "null") {
		t.Fatal("bool/null case matching broken")
	}
}
