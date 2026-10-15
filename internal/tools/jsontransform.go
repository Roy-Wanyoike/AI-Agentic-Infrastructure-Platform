package tools

// jsontransform.go — the JSON transform tool (issue #111): a jq-like MINIMAL
// reshaping language, pure and deterministic (no I/O, no clocks, no
// randomness; identical input always yields byte-identical output).
//
// The tool applies an ordered list of small ops to an input document; each
// op replaces the "current" document with its result, so ops chain:
//
//      data:  {"items":[{"price":9,"id":"a"},{"price":20,"id":"b"}]}
//      ops:
//        - {op: pick,   path: "items"}
//        - {op: filter, field: "price", cmp: "<", value: 10}
//        - {op: map,    template: {sku: "$.id", cheap: true}}
//      ->  {"result":[{"sku":"a","cheap":true}]}
//
// Ops:
//   - pick    {path}                        — select the value at a dot/bracket path.
//   - filter  {path?, field?, cmp, value?}  — keep array elements matching a
//     field-op-value predicate (field itself may be a sub-path). Supported
//     comparisons: == != < <= > >= contains exists. Ordering comparisons on
//     mismatched or non-comparable types exclude the element (jq-like: a
//     failed comparison is false, never an error).
//   - map     {path?, template}             — map an array's elements through
//     an object template whose string values starting with "$" are path
//     sub-expressions into the element ("$" = the element itself; missing
//     sub-paths resolve to null, documented jq-like behavior).
//   - template {body}                       — build an object from the current
//     document: string values containing "{{path}}" placeholders; a string
//     that is exactly one placeholder splices the typed value, embedded
//     placeholders interpolate with %v. Missing paths are an error (building
//     output from absent input is a caller bug, not a null to hide).
//
// Paths: dot and bracket segments ("a.b[0]", 'items["weird.key"]', "$.a.b"
// — the leading "$" is optional and stripped). Caps: input and result are
// each bounded at DefaultJSONTransformMaxBytes, ops at MaxJSONTransformOps,
// paths at MaxJSONPathBytes — a transform can never balloon the worker's
// memory.
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// JSONTransformToolName is the registry name of the JSON transform tool.
const JSONTransformToolName = "json_transform"

// JSON transform caps (issue #111).
const (
	// DefaultJSONTransformMaxBytes bounds BOTH the input document and the
	// encoded result (1 MiB each).
	DefaultJSONTransformMaxBytes = 1 << 20
	// MaxJSONTransformOps bounds the op list length.
	MaxJSONTransformOps = 16
	// MaxJSONPathBytes bounds a single path expression.
	MaxJSONPathBytes = 256
)

// JSON transform comparison operators.
const (
	jsonCmpEq       = "=="
	jsonCmpNe       = "!="
	jsonCmpLt       = "<"
	jsonCmpLe       = "<="
	jsonCmpGt       = ">"
	jsonCmpGe       = ">="
	jsonCmpContains = "contains"
	jsonCmpExists   = "exists"
)

// ErrJSONTransform is the base error for transform rejections (wrapped by
// every path/op/size failure this tool returns).
var ErrJSONTransform = errors.New("tools: json transform rejected")

// JSONTransformTool reshapes JSON documents with a minimal, pure op list.
type JSONTransformTool struct {
	gate    *ToolPolicyGate
	maxIn   int
	maxOut  int
	maxOps  int
	maxPath int
}

// NewJSONTransformTool returns the tool with default caps.
func NewJSONTransformTool() *JSONTransformTool {
	return &JSONTransformTool{
		maxIn:   DefaultJSONTransformMaxBytes,
		maxOut:  DefaultJSONTransformMaxBytes,
		maxOps:  MaxJSONTransformOps,
		maxPath: MaxJSONPathBytes,
	}
}

// WithPolicyGate attaches the per-tool policy gate (see policy.go).
func (t *JSONTransformTool) WithPolicyGate(gate *ToolPolicyGate) *JSONTransformTool {
	t.gate = gate
	return t
}

// Name implements Tool.
func (t *JSONTransformTool) Name() string { return JSONTransformToolName }

// DescribeTool publishes catalog metadata (issue #111).
func (t *JSONTransformTool) DescribeTool() ToolInfo {
	return ToolInfo{
		Name: t.Name(),
		Description: "Reshapes a JSON document with a minimal jq-like op list " +
			"(pick by dot/bracket path, filter arrays with field-op-value " +
			"predicates, map elements through object templates, build objects " +
			"from {{path}} templates). Pure and deterministic: no I/O; identical " +
			"input always produces identical output.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"data": map[string]any{
					"type":        "any",
					"description": "Input JSON value (object, array, scalar). Max 1 MiB encoded.",
				},
				"ops": map[string]any{
					"type": "array",
					"description": "Ordered ops; each replaces the current document with its result. " +
						"pick{path} | filter{path?,field?,cmp,value?} | map{path?,template} | template{body}.",
					"items": map[string]any{
						"type":                 "object",
						"additionalProperties": true,
					},
					"maxItems": MaxJSONTransformOps,
				},
				"estimated_cost_cents": map[string]any{
					"type":        "integer",
					"description": "Optional estimated cost surfaced to the policy engine (max_cost_cents condition).",
				},
			},
			"required": []string{"data", "ops"},
		},
	}
}

// Execute implements Tool.
func (t *JSONTransformTool) Execute(input map[string]any) (map[string]any, error) {
	return t.ExecuteContext(context.Background(), input)
}

// ExecuteContext implements ContextAware. The transform itself is pure
// synchronous computation (no I/O to cancel); the context feeds the optional
// policy gate, which consults the run scope it may carry.
func (t *JSONTransformTool) ExecuteContext(ctx context.Context, input map[string]any) (map[string]any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if t == nil {
		return nil, errors.New("tools: json transform tool is not initialized")
	}
	if err := t.gate.Allow(ctx, t.Name(), toolEstimatedCostCents(input["estimated_cost_cents"])); err != nil {
		return nil, err
	}
	return t.execute(input)
}

// execute runs the pure transform pipeline (split from the context wrapper
// so the computation is directly testable without a context).
func (t *JSONTransformTool) execute(input map[string]any) (map[string]any, error) {
	if t == nil {
		return nil, errors.New("tools: json transform tool is not initialized")
	}
	data, ok := input["data"]
	if !ok || data == nil {
		return nil, fmt.Errorf("%w: data is required", ErrInvalidToolInput)
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("%w: data is not JSON-encodable: %v", ErrInvalidToolInput, err)
	}
	if len(encoded) > t.maxIn {
		return nil, fmt.Errorf("%w: data exceeds %d bytes", ErrJSONTransform, t.maxIn)
	}
	rawOps, ok := input["ops"].([]any)
	if !ok || len(rawOps) == 0 {
		return nil, fmt.Errorf("%w: ops must be a non-empty array", ErrInvalidToolInput)
	}
	if len(rawOps) > t.maxOps {
		return nil, fmt.Errorf("%w: at most %d ops are allowed, got %d", ErrJSONTransform, t.maxOps, len(rawOps))
	}

	current := data
	for i, rawOp := range rawOps {
		op, ok := rawOp.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%w: ops[%d] must be an object, got %T", ErrInvalidToolInput, i, rawOp)
		}
		current, err = t.applyOp(i, op, current)
		if err != nil {
			return nil, err
		}
	}

	out, err := json.Marshal(current)
	if err != nil {
		return nil, fmt.Errorf("%w: result is not JSON-encodable: %v", ErrJSONTransform, err)
	}
	if len(out) > t.maxOut {
		return nil, fmt.Errorf("%w: result exceeds %d bytes", ErrJSONTransform, t.maxOut)
	}
	// Decode back through the JSON round-trip so the returned value is the
	// canonical, deterministic shape the model sees on the wire.
	var canonical any
	if err := json.Unmarshal(out, &canonical); err != nil {
		return nil, fmt.Errorf("%w: result round-trip failed: %v", ErrJSONTransform, err)
	}
	return map[string]any{
		"result":      canonical,
		"ops_applied": len(rawOps),
	}, nil
}

// applyOp dispatches one op against the current document.
func (t *JSONTransformTool) applyOp(i int, op map[string]any, current any) (any, error) {
	kind, err := stringArg(op["op"])
	if err != nil {
		return nil, fmt.Errorf("%w: ops[%d].op: %v", ErrInvalidToolInput, i, err)
	}
	switch kind {
	case "pick":
		path, err := t.opPath(i, op, true)
		if err != nil {
			return nil, err
		}
		segs, err := parseJSONPath(path)
		if err != nil {
			return nil, fmt.Errorf("%w: ops[%d]: %v", ErrInvalidToolInput, i, err)
		}
		value, err := lookupJSONPath(current, segs)
		if err != nil {
			return nil, fmt.Errorf("%w: ops[%d]: path %q: %v", ErrJSONTransform, i, path, err)
		}
		return value, nil
	case "filter":
		return t.applyFilter(i, op, current)
	case "map":
		return t.applyMap(i, op, current)
	case "template":
		body, ok := op["body"]
		if !ok {
			return nil, fmt.Errorf("%w: ops[%d].body is required for the template op", ErrInvalidToolInput, i)
		}
		return t.renderTemplate(i, body, current)
	default:
		return nil, fmt.Errorf("%w: ops[%d].op %q is not allowed (pick|filter|map|template)", ErrInvalidToolInput, i, kind)
	}
}

// applyFilter keeps the array elements matching the field-op-value predicate.
func (t *JSONTransformTool) applyFilter(i int, op map[string]any, current any) (any, error) {
	cmp, err := stringArg(op["cmp"])
	if err != nil {
		return nil, fmt.Errorf("%w: ops[%d].cmp: %v", ErrInvalidToolInput, i, err)
	}
	switch cmp {
	case jsonCmpEq, jsonCmpNe, jsonCmpLt, jsonCmpLe, jsonCmpGt, jsonCmpGe, jsonCmpContains, jsonCmpExists:
	default:
		return nil, fmt.Errorf("%w: ops[%d].cmp %q is not allowed (==,!=,<,<=,>,>=,contains,exists)", ErrInvalidToolInput, i, cmp)
	}
	var (
		fieldSegs []jsonPathSeg
		hasField  bool
	)
	if field, err := stringArg(op["field"]); err == nil {
		fieldSegs, err = parseJSONPath(field)
		if err != nil {
			return nil, fmt.Errorf("%w: ops[%d].field: %v", ErrInvalidToolInput, i, err)
		}
		hasField = true
	}

	items, err := t.selectArray(i, op, current)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(items))
	for _, item := range items {
		if jsonPredicate(item, hasField, fieldSegs, cmp, op["value"]) {
			out = append(out, item)
		}
	}
	return out, nil
}

// applyMap maps the selected array's elements through the object template.
func (t *JSONTransformTool) applyMap(i int, op map[string]any, current any) (any, error) {
	tmpl, ok := op["template"]
	if !ok {
		return nil, fmt.Errorf("%w: ops[%d].template is required for the map op", ErrInvalidToolInput, i)
	}
	items, err := t.selectArray(i, op, current)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(items))
	for _, item := range items {
		mapped, err := t.evalSubExpressions(i, tmpl, item)
		if err != nil {
			return nil, err
		}
		out = append(out, mapped)
	}
	return out, nil
}

// selectArray resolves the op's array: the value at path when given, else
// the current document itself. The result must be a JSON array.
func (t *JSONTransformTool) selectArray(i int, op map[string]any, current any) ([]any, error) {
	items, ok := current.([]any)
	if path, pathErr := t.opPath(i, op, false); pathErr == nil && path != "" {
		segs, err := parseJSONPath(path)
		if err != nil {
			return nil, fmt.Errorf("%w: ops[%d]: %v", ErrInvalidToolInput, i, err)
		}
		value, err := lookupJSONPath(current, segs)
		if err != nil {
			return nil, fmt.Errorf("%w: ops[%d]: path %q: %v", ErrJSONTransform, i, path, err)
		}
		items, ok = value.([]any)
	}
	if !ok {
		return nil, fmt.Errorf("%w: ops[%d] target must be a JSON array", ErrJSONTransform, i)
	}
	return items, nil
}

// opPath reads the optional/required "path" argument under the path cap.
func (t *JSONTransformTool) opPath(i int, op map[string]any, required bool) (string, error) {
	path, err := stringArg(op["path"])
	if err != nil {
		if required {
			return "", fmt.Errorf("%w: ops[%d].path: %v", ErrInvalidToolInput, i, err)
		}
		return "", nil // optional: absent/empty means "the document itself"
	}
	path = strings.TrimSpace(path)
	if len(path) > t.maxPath {
		return "", fmt.Errorf("%w: ops[%d].path exceeds %d bytes", ErrJSONTransform, i, t.maxPath)
	}
	return path, nil
}

// jsonPredicate evaluates one filter predicate against one element. Type
// mismatches exclude the element (a failed comparison is false, jq-like).
func jsonPredicate(item any, hasField bool, fieldSegs []jsonPathSeg, cmp string, want any) bool {
	value := item
	if hasField {
		got, err := lookupJSONPath(item, fieldSegs)
		if err != nil {
			return false // missing field: the predicate cannot match (jq-like)
		}
		value = got
	}
	switch cmp {
	case jsonCmpExists:
		return value != nil
	case jsonCmpEq:
		return jsonScalarEqual(value, want)
	case jsonCmpNe:
		return !jsonScalarEqual(value, want)
	case jsonCmpContains:
		return jsonContains(value, want)
	}
	// Ordering comparisons.
	leftNum, leftIsNum := jsonNumber(value)
	rightNum, rightIsNum := jsonNumber(want)
	leftStr, leftIsStr := value.(string)
	rightStr, rightIsStr := want.(string)
	switch cmp {
	case jsonCmpLt:
		if leftIsNum && rightIsNum {
			return leftNum < rightNum
		}
		if leftIsStr && rightIsStr {
			return leftStr < rightStr
		}
	case jsonCmpLe:
		if leftIsNum && rightIsNum {
			return leftNum <= rightNum
		}
		if leftIsStr && rightIsStr {
			return leftStr <= rightStr
		}
	case jsonCmpGt:
		if leftIsNum && rightIsNum {
			return leftNum > rightNum
		}
		if leftIsStr && rightIsStr {
			return leftStr > rightStr
		}
	case jsonCmpGe:
		if leftIsNum && rightIsNum {
			return leftNum >= rightNum
		}
		if leftIsStr && rightIsStr {
			return leftStr >= rightStr
		}
	}
	return false
}

// jsonNumber coerces the JSON scalar shapes into float64.
func jsonNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}

// jsonScalarEqual compares scalars without cross-type coercion beyond the
// numeric unification (1 == 1.0), plus deep equality for containers.
func jsonScalarEqual(a, b any) bool {
	if aNum, aOK := jsonNumber(a); aOK {
		if bNum, bOK := jsonNumber(b); bOK {
			return aNum == bNum
		}
		return false
	}
	switch aVal := a.(type) {
	case string:
		bVal, ok := b.(string)
		return ok && aVal == bVal
	case bool:
		bVal, ok := b.(bool)
		return ok && aVal == bVal
	case nil:
		return b == nil
	default:
		return jsonEqualValue(a, b)
	}
}

// jsonEqualValue is deep equality over JSON shapes via canonical encoding.
func jsonEqualValue(a, b any) bool {
	aJSON, errA := json.Marshal(a)
	bJSON, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return string(aJSON) == string(bJSON)
}

// jsonContains implements the contains comparison: substring for strings,
// deep-equal membership for arrays, key presence for objects.
func jsonContains(haystack, needle any) bool {
	switch hs := haystack.(type) {
	case string:
		ns, ok := needle.(string)
		return ok && strings.Contains(hs, ns)
	case []any:
		for _, item := range hs {
			if jsonScalarEqual(item, needle) || jsonEqualValue(item, needle) {
				return true
			}
		}
		return false
	case map[string]any:
		key, ok := needle.(string)
		if !ok {
			return false
		}
		_, present := hs[key]
		return present
	default:
		return false
	}
}

// evalSubExpressions evaluates a map-op template against one element:
// string values beginning with "$" are path sub-expressions into the element
// ("$" = the element itself); missing sub-paths resolve to null (jq-like).
func (t *JSONTransformTool) evalSubExpressions(i int, tmpl any, element any) (any, error) {
	switch typed := tmpl.(type) {
	case string:
		if strings.HasPrefix(typed, "$") {
			path := strings.TrimSpace(typed)
			if len(path) > t.maxPath {
				return nil, fmt.Errorf("%w: ops[%d] sub-expression exceeds %d bytes", ErrJSONTransform, i, t.maxPath)
			}
			segs, err := parseJSONPath(path)
			if err != nil {
				return nil, fmt.Errorf("%w: ops[%d]: %v", ErrInvalidToolInput, i, err)
			}
			if len(segs) == 0 {
				return element, nil
			}
			value, err := lookupJSONPath(element, segs)
			if err != nil {
				return nil, nil // missing sub-path -> null (documented)
			}
			return value, nil
		}
		return typed, nil
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, value := range typed {
			rendered, err := t.evalSubExpressions(i, value, element)
			if err != nil {
				return nil, err
			}
			out[key] = rendered
		}
		return out, nil
	case []any:
		out := make([]any, 0, len(typed))
		for _, value := range typed {
			rendered, err := t.evalSubExpressions(i, value, element)
			if err != nil {
				return nil, err
			}
			out = append(out, rendered)
		}
		return out, nil
	default:
		return typed, nil
	}
}

// renderTemplate builds the template op's output from the current document:
// "{{path}}" placeholders. A string that is exactly one placeholder splices
// the typed value; embedded placeholders interpolate with %v. Missing paths
// are an error.
func (t *JSONTransformTool) renderTemplate(i int, body any, doc any) (any, error) {
	switch typed := body.(type) {
	case string:
		return t.renderTemplateString(i, typed, doc)
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, value := range typed {
			rendered, err := t.renderTemplate(i, value, doc)
			if err != nil {
				return nil, err
			}
			out[key] = rendered
		}
		return out, nil
	case []any:
		out := make([]any, 0, len(typed))
		for _, value := range typed {
			rendered, err := t.renderTemplate(i, value, doc)
			if err != nil {
				return nil, err
			}
			out = append(out, rendered)
		}
		return out, nil
	default:
		return typed, nil
	}
}

// renderTemplateString substitutes {{path}} placeholders in one string.
func (t *JSONTransformTool) renderTemplateString(i int, s string, doc any) (any, error) {
	open := strings.Index(s, "{{")
	if open < 0 {
		return s, nil // literal
	}
	close := strings.Index(s[open:], "}}")
	if close < 0 {
		return nil, fmt.Errorf("%w: ops[%d]: template string %q has an unterminated {{ placeholder", ErrJSONTransform, i, s)
	}
	close += open
	path := strings.TrimSpace(s[open+2 : close])
	if len(path) > t.maxPath {
		return nil, fmt.Errorf("%w: ops[%d] template path exceeds %d bytes", ErrJSONTransform, i, t.maxPath)
	}
	segs, err := parseJSONPath(path)
	if err != nil {
		return nil, fmt.Errorf("%w: ops[%d]: %v", ErrInvalidToolInput, i, err)
	}
	value, lookupErr := lookupJSONPath(doc, segs)
	if lookupErr != nil {
		return nil, fmt.Errorf("%w: ops[%d]: template path %q: %v", ErrJSONTransform, i, path, lookupErr)
	}
	// Exactly one placeholder spanning the whole string: splice the value
	// with its type preserved (numbers stay numbers, objects stay objects).
	if open == 0 && close == len(s)-2 {
		return value, nil
	}
	rest, err := t.renderTemplateString(i, s[close+2:], doc)
	if err != nil {
		return nil, err
	}
	return s[:open] + fmt.Sprintf("%v", value) + fmt.Sprintf("%v", rest), nil
}

// jsonPathSeg is one resolved path segment: either an object key or an
// array index.
type jsonPathSeg struct {
	name    string
	index   int
	isIndex bool
}

// parseJSONPath parses the minimal dot/bracket grammar:
//
//	path    := ["$"] segment ("." segment)*
//	segment := identifier | index | quoted
//	index   := "[" digits "]"
//	quoted  := '"' any-chars '"'
//
// A leading "$" (jq-style root marker, optionally followed by ".") is
// stripped. Empty paths yield zero segments (the document itself).
func parseJSONPath(path string) ([]jsonPathSeg, error) {
	s := strings.TrimSpace(path)
	if s == "" {
		return nil, nil
	}
	if s == "$" {
		return nil, nil
	}
	if s[0] == '$' {
		s = s[1:]
		if s == "" {
			return nil, nil
		}
		if s[0] == '.' {
			s = s[1:]
		}
	}
	var (
		segs   []jsonPathSeg
		cursor int
	)
	for cursor < len(s) {
		switch s[cursor] {
		case '.':
			if len(segs) == 0 && cursor == 0 {
				return nil, fmt.Errorf("%w: path %q starts with '.'", ErrInvalidToolInput, path)
			}
			cursor++ // separator; next loop iteration reads the segment
			if cursor < len(s) && s[cursor] == '.' {
				return nil, fmt.Errorf("%w: path %q contains an empty segment", ErrInvalidToolInput, path)
			}
		case '[':
			end := strings.IndexByte(s[cursor:], ']')
			if end < 0 {
				return nil, fmt.Errorf("%w: path %q has an unterminated [ bracket", ErrInvalidToolInput, path)
			}
			end += cursor
			inner := strings.TrimSpace(s[cursor+1 : end])
			switch {
			case inner == "":
				return nil, fmt.Errorf("%w: path %q has an empty [] segment", ErrInvalidToolInput, path)
			case inner[0] == '"' || inner[0] == '\'':
				if len(inner) < 2 || inner[len(inner)-1] != inner[0] {
					return nil, fmt.Errorf("%w: path %q has an unterminated quoted key", ErrInvalidToolInput, path)
				}
				segs = append(segs, jsonPathSeg{name: inner[1 : len(inner)-1]})
			default:
				idx := 0
				for _, ch := range inner {
					if ch < '0' || ch > '9' {
						return nil, fmt.Errorf("%w: path %q has a non-numeric [index] (quote object keys)", ErrInvalidToolInput, path)
					}
					idx = idx*10 + int(ch-'0')
				}
				segs = append(segs, jsonPathSeg{index: idx, isIndex: true})
			}
			cursor = end + 1
		default:
			start := cursor
			for cursor < len(s) && s[cursor] != '.' && s[cursor] != '[' {
				cursor++
			}
			segment := s[start:cursor]
			if segment == "" {
				return nil, fmt.Errorf("%w: path %q contains an empty segment", ErrInvalidToolInput, path)
			}
			// A trailing numeric suffix after a key is a key itself (keys are
			// looked up verbatim); indexes must use [n] — predictable grammar.
			segs = append(segs, jsonPathSeg{name: segment})
		}
	}
	return segs, nil
}

// lookupJSONPath walks doc along segs. Missing keys/indexes and type
// mismatches are errors with the offending segment named.
func lookupJSONPath(doc any, segs []jsonPathSeg) (any, error) {
	current := doc
	for _, seg := range segs {
		switch {
		case seg.isIndex:
			items, ok := current.([]any)
			if !ok {
				return nil, fmt.Errorf("expected an array at [%d], got %T", seg.index, current)
			}
			if seg.index >= len(items) {
				return nil, fmt.Errorf("index [%d] out of range (length %d)", seg.index, len(items))
			}
			current = items[seg.index]
		default:
			obj, ok := current.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("expected an object at %q, got %T", seg.name, current)
			}
			value, present := obj[seg.name]
			if !present {
				return nil, fmt.Errorf("key %q not found", seg.name)
			}
			current = value
		}
	}
	return current, nil
}
