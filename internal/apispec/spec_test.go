package apispec

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// specPath is relative to this package directory (go test runs with the CWD
// set to the package under test). The document is never written by this
// package: it is a pure consumer of the contract.
const specPath = "../../api/openapi.yaml"

// httpMethods are the OpenAPI operation keys allowed on a Path Item
// (https://spec.openapis.org/oas/v3.1.0#path-item-object).
var httpMethods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

// loadSpec parses api/openapi.yaml into a generic document tree.
//
// The document is decoded through yaml.Node and flattened manually so that
// duplicate mapping keys collapse with last-key-wins semantics — the same
// behavior as the tooling that merged the wave-3 fragments. (A strict map
// decode would reject the whole document over leftover duplicate keys instead
// of checking the integrity assertions below.) Duplicated keys are surfaced
// through t.Logf so spec owners can see and clean them.
func loadSpec(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("cannot read %s: %v", specPath, err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		t.Fatalf("cannot parse %s as YAML: %v", specPath, err)
	}
	duplicateSpots = nil // diagnostics are per-conversion
	dups := 0
	converted := convertNode(&root, &dups, "")
	if dups > 0 {
		t.Logf("NOTE for spec owners: %d duplicate mapping key(s) collapsed with last-key-wins at: %s",
			dups, strings.Join(duplicateSpots, ", "))
	}
	doc, ok := converted.(map[string]any)
	if !ok || doc == nil {
		t.Fatalf("%s parsed to %T, want a mapping", specPath, converted)
	}
	return doc
}

// duplicateSpots accumulates the locations of collapsed duplicate keys during
// the last conversion (diagnostics only; reset per conversion).
var duplicateSpots []string

// convertNode flattens a yaml.Node tree into plain Go values. Mapping keys
// that repeat collapse last-wins; occurrences are counted and located.
func convertNode(n *yaml.Node, dups *int, at string) any {
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) > 0 {
			return convertNode(n.Content[0], dups, at)
		}
		return nil
	case yaml.MappingNode:
		out := map[string]any{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			key := n.Content[i].Value
			if _, exists := out[key]; exists {
				*dups++
				duplicateSpots = append(duplicateSpots, at+"/"+key)
			}
			out[key] = convertNode(n.Content[i+1], dups, at+"/"+key)
		}
		return out
	case yaml.SequenceNode:
		out := make([]any, 0, len(n.Content))
		for i, c := range n.Content {
			out = append(out, convertNode(c, dups, at+"/"+strconv.Itoa(i)))
		}
		return out
	case yaml.ScalarNode:
		var v any
		if err := n.Decode(&v); err == nil {
			return v
		}
		return n.Value
	default:
		return nil
	}
}

// ref is one located $ref: where it was found and what it points at.
type ref struct {
	at string // JSON pointer of the node holding the $ref keyword
	to string // raw $ref value
}

// collectRefs walks the whole document and returns every $ref keyword.
func collectRefs(node any, at string) []ref {
	var out []ref
	switch v := node.(type) {
	case map[string]any:
		for key, val := range v {
			if key == "$ref" {
				if s, ok := val.(string); ok {
					out = append(out, ref{at: at, to: s})
					continue
				}
			}
			out = append(out, collectRefs(val, at+"/"+key)...)
		}
	case []any:
		for i, val := range v {
			out = append(out, collectRefs(val, at+"/"+strconv.Itoa(i))...)
		}
	}
	return out
}

// resolvePointer resolves an internal JSON Pointer reference (e.g.
// "#/components/schemas/Agent") against the document root, per RFC 6901 with
// the leading "#" used by OpenAPI. It returns an error for anything it cannot
// resolve, including external (non-#) references: the shipped contract is a
// single self-contained document, so a pointer outside it counts as dangling
// for CI purposes.
func resolvePointer(root map[string]any, ptr string) (any, error) {
	if ptr == "" {
		return nil, errors.New("empty $ref")
	}
	if !strings.HasPrefix(ptr, "#/") {
		return nil, fmt.Errorf("not an internal reference (want prefix %q, got %q)", "#/", ptr)
	}
	encoded := strings.Split(strings.TrimPrefix(ptr, "#/"), "/")
	var node any = root
	for _, seg := range encoded {
		// Unescape per RFC 6901: ~1 -> "/", ~0 -> "~".
		seg = strings.ReplaceAll(seg, "~1", "/")
		seg = strings.ReplaceAll(seg, "~0", "~")
		switch v := node.(type) {
		case map[string]any:
			next, ok := v[seg]
			if !ok {
				return nil, fmt.Errorf("segment %q not found", seg)
			}
			node = next
		case []any:
			idx, err := strconv.Atoi(seg)
			if err != nil || idx < 0 || idx >= len(v) {
				return nil, fmt.Errorf("segment %q is not a valid array index", seg)
			}
			node = v[idx]
		default:
			return nil, fmt.Errorf("segment %q traverses a scalar", seg)
		}
	}
	return node, nil
}

// TestSpecExists guards the test suite itself: the contract must be present
// and parse as an OpenAPI document with paths and components.
func TestSpecExists(t *testing.T) {
	doc := loadSpec(t)
	if v, ok := doc["openapi"].(string); !ok || !strings.HasPrefix(v, "3.") {
		t.Fatalf("expected an OpenAPI 3.x document, got openapi=%v", doc["openapi"])
	}
	if _, ok := doc["paths"].(map[string]any); !ok {
		t.Fatalf("document has no paths object")
	}
	if _, ok := doc["components"].(map[string]any); !ok {
		t.Fatalf("document has no components object")
	}
}

// TestAllRefsResolve walks every node of the document and asserts each $ref
// resolves to an existing node (issue #17: dangling refs must fail CI).
func TestAllRefsResolve(t *testing.T) {
	doc := loadSpec(t)
	refs := collectRefs(doc, "")
	if len(refs) == 0 {
		t.Fatalf("no $ref found in %s — the walker is broken or the spec was replaced", specPath)
	}
	var failures []string
	for _, r := range refs {
		if _, err := resolvePointer(doc, r.to); err != nil {
			failures = append(failures, fmt.Sprintf("dangling $ref %q at %s: %v", r.to, r.at, err))
		}
	}
	if len(failures) > 0 {
		sort.Strings(failures)
		t.Errorf("%d dangling $ref(s):\n%s", len(failures), strings.Join(failures, "\n"))
	}
}

// operations returns every operation node keyed by "<path> <method>".
func operations(t *testing.T, doc map[string]any) map[string]map[string]any {
	t.Helper()
	ops := map[string]map[string]any{}
	paths, _ := doc["paths"].(map[string]any)
	for path, itemAny := range paths {
		item, ok := itemAny.(map[string]any)
		if !ok {
			continue
		}
		// A Path Item may itself be a reference; resolve it before
		// looking for operations.
		if p, hasRef := item["$ref"].(string); hasRef {
			resolved, err := resolvePointer(doc, p)
			if err != nil {
				continue // reported by TestAllRefsResolve
			}
			item, ok = resolved.(map[string]any)
			if !ok {
				continue
			}
		}
		for _, method := range httpMethods {
			op, ok := item[method].(map[string]any)
			if !ok {
				continue
			}
			ops[path+" "+method] = op
		}
	}
	return ops
}

// TestOperationIDsUnique asserts every operationId in the document is unique
// (issue #17). operationId is optional per the OpenAPI spec, so uniqueness
// applies to the ids that are present.
func TestOperationIDsUnique(t *testing.T) {
	doc := loadSpec(t)
	seen := map[string]string{}
	var dups []string
	for key, op := range operations(t, doc) {
		id, ok := op["operationId"].(string)
		if !ok || strings.TrimSpace(id) == "" {
			continue
		}
		if first, clash := seen[id]; clash {
			dups = append(dups, fmt.Sprintf("operationId %q used by %s and %s", id, first, key))
			continue
		}
		seen[id] = key
	}
	if len(dups) > 0 {
		sort.Strings(dups)
		t.Errorf("%d duplicate operationId(s):\n%s", len(dups), strings.Join(dups, "\n"))
	}
}

// TestEveryPathItemDeclaresAnOperation asserts no path is left empty (issue
// #17) — a path item must declare at least one operation (or be a reference
// to one that does).
func TestEveryPathItemDeclaresAnOperation(t *testing.T) {
	doc := loadSpec(t)
	paths, ok := doc["paths"].(map[string]any)
	if !ok {
		t.Fatalf("document has no paths object")
	}
	methodSet := map[string]bool{}
	for _, m := range httpMethods {
		methodSet[m] = true
	}
	var empty []string
	for path, itemAny := range paths {
		item, ok := itemAny.(map[string]any)
		if !ok {
			empty = append(empty, fmt.Sprintf("%s: path item is %T, want an object", path, itemAny))
			continue
		}
		if p, hasRef := item["$ref"].(string); hasRef {
			resolved, err := resolvePointer(doc, p)
			if err != nil {
				empty = append(empty, fmt.Sprintf("%s: path item $ref %q does not resolve: %v", path, p, err))
				continue
			}
			item, ok = resolved.(map[string]any)
			if !ok {
				empty = append(empty, fmt.Sprintf("%s: path item $ref %q is not an object", path, p))
				continue
			}
		}
		hasOp := false
		for key := range item {
			if methodSet[key] {
				hasOp = true
				break
			}
		}
		if !hasOp {
			empty = append(empty, fmt.Sprintf("%s: path item declares no operation (only %v)", path, keys(item)))
		}
	}
	if len(empty) > 0 {
		sort.Strings(empty)
		t.Errorf("%d path item(s) without operations:\n%s", len(empty), strings.Join(empty, "\n"))
	}
}

// TestEveryOperationHasResponses asserts every operation declares a responses
// object with at least one response (issue #17).
func TestEveryOperationHasResponses(t *testing.T) {
	doc := loadSpec(t)
	var missing []string
	for key, op := range operations(t, doc) {
		responses, ok := op["responses"].(map[string]any)
		if !ok || len(responses) == 0 {
			missing = append(missing, fmt.Sprintf("%s: operation has no responses", key))
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("%d operation(s) without responses:\n%s", len(missing), strings.Join(missing, "\n"))
	}
}

// keys returns the map keys in sorted order (deterministic error messages).
func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
