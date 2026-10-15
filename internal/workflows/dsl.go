package workflows

import (
	"errors"
	"fmt"
	"strings"
)

// NodeType enumerates the node types supported by the workflow DSL
// (pinned by docs/wave2-api-contract.md).
type NodeType string

const (
	NodeAgent     NodeType = "agent"
	NodeTool      NodeType = "tool"
	NodeCondition NodeType = "condition"
	NodeParallel  NodeType = "parallel"
	NodeApproval  NodeType = "approval"
	NodeDelay     NodeType = "delay"
	NodeWebhook   NodeType = "webhook"
	// Control-flow extensions (issue #109). NodeCondition keeps its DSL type
	// string and gains if/else semantics through config.branches; the switch,
	// loop, retry and wait_until types extend the vocabulary additively so
	// previously stored workflows keep validating.
	NodeSwitch    NodeType = "switch"
	NodeLoop      NodeType = "loop"
	NodeRetry     NodeType = "retry"
	NodeWaitUntil NodeType = "wait_until"
	// NodeEnd is the legacy terminal marker kept for workflows created
	// through the pre-DSL Step API.
	NodeEnd NodeType = "end"
)

// isControlFlowNodeType reports whether a node type drives routing/execution
// control rather than doing work itself. Control-flow nodes cannot be loop
// bodies or retry targets (the executor executes them on its own schedule,
// not per loop item / per retry attempt).
func isControlFlowNodeType(t NodeType) bool {
	switch t {
	case NodeCondition, NodeSwitch, NodeLoop, NodeRetry, NodeWaitUntil, NodeParallel, NodeApproval:
		return true
	}
	return false
}

// isLoopOwnableNodeType reports whether a node type may appear in a loop body
// or as a retry target: executable work nodes and passive structural nodes.
func isLoopOwnableNodeType(t NodeType) bool {
	switch t {
	case NodeAgent, NodeTool, NodeDelay, NodeWebhook, NodeEnd:
		return true
	}
	return false
}

// Edge condition values.
const (
	EdgeOnSuccess = "on_success"
	EdgeOnFailure = "on_failure"
	EdgeAlways    = "always"
)

// Node is one vertex of the workflow DAG.
type Node struct {
	ID     string         `json:"id"`
	Type   NodeType       `json:"type"`
	Name   string         `json:"name,omitempty"`
	Config map[string]any `json:"config,omitempty"`
}

// Edge is one directed connection between two nodes.
type Edge struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Condition string `json:"condition,omitempty"`
}

// DSL is the workflow definition stored as JSONB.
type DSL struct {
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
}

// ValidationError is a single DSL validation failure; the API renders the
// slice as the 422 body {"errors":[{"code","message","node_id"}]}.
type ValidationError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	NodeID  string `json:"node_id,omitempty"`
}

// ValidationErrors bundles validation failures and implements error so
// handlers can map it to 422 with errors.Is / type assertion.
type ValidationErrors struct {
	Errors []ValidationError `json:"errors"`
}

func (e *ValidationErrors) Error() string {
	if e == nil || len(e.Errors) == 0 {
		return "workflow validation failed"
	}
	return fmt.Sprintf("workflow validation failed: %s", e.Errors[0].Message)
}

// nodeTypes is the set of DSL node types accepted by ValidateDSL.
var nodeTypes = map[NodeType]bool{
	NodeAgent:     true,
	NodeTool:      true,
	NodeCondition: true,
	NodeParallel:  true,
	NodeApproval:  true,
	NodeDelay:     true,
	NodeWebhook:   true,
	NodeSwitch:    true,
	NodeLoop:      true,
	NodeRetry:     true,
	NodeWaitUntil: true,
	NodeEnd:       true,
}

// ValidateDSL checks the structural rules pinned by the API contract plus the
// issue #109 control-flow rules:
//   - non-empty graph
//   - unique, non-empty node ids
//   - known node types
//   - per-type required config (agent -> agent_id, tool -> tool_id,
//     condition -> expression, delay -> seconds, webhook -> url,
//     switch/loop/wait_until -> expression, retry -> target)
//   - every edge references existing nodes with a valid condition
//   - condition/switch/loop/retry wiring references existing node ids
//     (branches, cases, bodies, targets) and every control-flow expression
//     parses (invalid_expression errors carry node id + reason)
//   - the graph is acyclic (DFS cycle detection); edges INTO a loop node are
//     loop-back edges, exempt from the cycle rule but restricted to the
//     loop's own body and capped by the loop's max_iterations at run time
//
// It returns nil when the DSL is valid.
func ValidateDSL(d DSL) []ValidationError {
	errs := make([]ValidationError, 0)
	if len(d.Nodes) == 0 {
		return append(errs, ValidationError{Code: "empty_graph", Message: "workflow requires at least one node"})
	}
	ids := make(map[string]bool, len(d.Nodes))
	for _, n := range d.Nodes {
		if strings.TrimSpace(n.ID) == "" {
			errs = append(errs, ValidationError{Code: "missing_node_id", Message: "node id is required"})
			continue
		}
		if ids[n.ID] {
			errs = append(errs, ValidationError{Code: "duplicate_node_id", Message: fmt.Sprintf("duplicate node id %q", n.ID), NodeID: n.ID})
			continue
		}
		ids[n.ID] = true
		if !nodeTypes[n.Type] {
			errs = append(errs, ValidationError{Code: "unknown_node_type", Message: fmt.Sprintf("unknown node type %q", n.Type), NodeID: n.ID})
		}
		if msg := validateNodeConfig(n); msg != "" {
			errs = append(errs, ValidationError{Code: "missing_config", Message: msg, NodeID: n.ID})
		}
	}
	for _, e := range d.Edges {
		if e.From == "" || !ids[e.From] {
			errs = append(errs, ValidationError{Code: "missing_node_ref", Message: fmt.Sprintf("edge references unknown source node %q", e.From), NodeID: e.From})
			continue
		}
		if e.To == "" || !ids[e.To] {
			errs = append(errs, ValidationError{Code: "missing_node_ref", Message: fmt.Sprintf("edge references unknown target node %q", e.To), NodeID: e.To})
			continue
		}
		if e.Condition != "" && e.Condition != EdgeOnSuccess && e.Condition != EdgeOnFailure && e.Condition != EdgeAlways {
			errs = append(errs, ValidationError{Code: "invalid_edge_condition", Message: fmt.Sprintf("edge condition must be %s, %s or %s", EdgeOnSuccess, EdgeOnFailure, EdgeAlways), NodeID: e.From})
		}
	}
	if cycleNode := findCycle(d, ids); cycleNode != "" {
		errs = append(errs, ValidationError{Code: "cycle_detected", Message: fmt.Sprintf("graph contains a cycle through node %q", cycleNode), NodeID: cycleNode})
	}
	errs = append(errs, validateControlFlow(d, ids)...)
	if len(errs) == 0 {
		return nil
	}
	return errs
}

// validateControlFlow enforces the issue #109 control-flow wiring rules that
// need the whole graph (branch/case/default targets, loop bodies, retry
// ownership, expression syntax, per-node numeric ranges). Every failure is a
// typed ValidationError carrying the offending node id. Edges into loop nodes
// need no special validation: they are either structural loop-back edges from
// the loop's own body (ignored by the executor's activation graph) or normal
// forward activation edges — both are legal.
func validateControlFlow(d DSL, ids map[string]bool) []ValidationError {
	errs := make([]ValidationError, 0)
	types := make(map[string]NodeType, len(d.Nodes))
	for _, n := range d.Nodes {
		types[n.ID] = n.Type
	}

	// Owner registry: every loop body node / retry target must have exactly
	// one owner so the executor cannot double-execute a node.
	owner := make(map[string]string, len(d.Nodes))
	claimOwner := func(nodeID, owned, code string) {
		if prev, ok := owner[owned]; ok {
			errs = append(errs, ValidationError{Code: code, Message: fmt.Sprintf("node %q is already owned by %s %q", owned, types[prev], prev), NodeID: nodeID})
			return
		}
		owner[owned] = nodeID
	}

	for _, n := range d.Nodes {
		switch n.Type {
		case NodeCondition:
			errs = append(errs, validateExpressionConfig(n, configString(n.Config, "expression"))...)
			bt := BranchTargetsFromConfig(n.Config)
			if raw, present := n.Config["branches"]; present {
				if _, ok := raw.(map[string]any); !ok {
					errs = append(errs, ValidationError{Code: "missing_config", Message: "condition node config.branches must be an object {true, false}", NodeID: n.ID})
				} else if bt.True == "" && bt.False == "" {
					errs = append(errs, ValidationError{Code: "missing_config", Message: "condition node config.branches must set at least one target", NodeID: n.ID})
				}
			}
			for _, target := range []string{bt.True, bt.False} {
				if target != "" && !ids[target] {
					errs = append(errs, ValidationError{Code: "dangling_branch_target", Message: fmt.Sprintf("condition branch references unknown node %q", target), NodeID: n.ID})
				}
			}
		case NodeSwitch:
			errs = append(errs, validateExpressionConfig(n, configString(n.Config, "expression"))...)
			cases, defaultTarget := SwitchTargetsFromConfig(n.Config)
			if len(cases) == 0 && defaultTarget == "" {
				errs = append(errs, ValidationError{Code: "missing_config", Message: "switch node requires config.cases and/or config.default", NodeID: n.ID})
			}
			for value, target := range cases {
				if target == "" {
					errs = append(errs, ValidationError{Code: "missing_config", Message: fmt.Sprintf("switch case %q has no target node id", value), NodeID: n.ID})
					continue
				}
				if !ids[target] {
					errs = append(errs, ValidationError{Code: "dangling_branch_target", Message: fmt.Sprintf("switch case %q references unknown node %q", value, target), NodeID: n.ID})
				}
			}
			if defaultTarget != "" && !ids[defaultTarget] {
				errs = append(errs, ValidationError{Code: "dangling_branch_target", Message: fmt.Sprintf("switch default references unknown node %q", defaultTarget), NodeID: n.ID})
			}
		case NodeLoop:
			cfg := LoopConfigFromNode(n)
			errs = append(errs, validateExpressionConfig(n, cfg.Expression)...)
			if len(cfg.Body) == 0 {
				errs = append(errs, ValidationError{Code: "missing_config", Message: "loop node requires a non-empty config.body node id list", NodeID: n.ID})
			}
			bodySet := make(map[string]bool, len(cfg.Body))
			for _, bodyID := range cfg.Body {
				if bodySet[bodyID] {
					errs = append(errs, ValidationError{Code: "duplicate_loop_body", Message: fmt.Sprintf("loop body lists node %q twice", bodyID), NodeID: n.ID})
					continue
				}
				bodySet[bodyID] = true
				if !ids[bodyID] {
					errs = append(errs, ValidationError{Code: "dangling_loop_body", Message: fmt.Sprintf("loop body references unknown node %q", bodyID), NodeID: n.ID})
					continue
				}
				if !isLoopOwnableNodeType(types[bodyID]) {
					errs = append(errs, ValidationError{Code: "loop_body_invalid_node", Message: fmt.Sprintf("loop body node %q has type %q; bodies may only contain agent, tool, delay, webhook or end nodes", bodyID, types[bodyID]), NodeID: n.ID})
				}
				claimOwner(n.ID, bodyID, "duplicate_loop_body")
			}
		case NodeRetry:
			target, _ := RetryConfigFromNode(n)
			if target == "" {
				errs = append(errs, ValidationError{Code: "missing_config", Message: "retry node requires config.target", NodeID: n.ID})
			} else {
				if !ids[target] {
					errs = append(errs, ValidationError{Code: "dangling_retry_target", Message: fmt.Sprintf("retry target references unknown node %q", target), NodeID: n.ID})
				} else if !isLoopOwnableNodeType(types[target]) {
					errs = append(errs, ValidationError{Code: "invalid_retry_target", Message: fmt.Sprintf("retry target %q has type %q; only agent, tool, delay, webhook or end nodes can be retried", target, types[target]), NodeID: n.ID})
				}
				claimOwner(n.ID, target, "duplicate_retry_target")
			}
			if v, present := configNumber(n.Config, "attempts"); present && v < 1 {
				errs = append(errs, ValidationError{Code: "missing_config", Message: "retry node config.attempts must be a number >= 1", NodeID: n.ID})
			}
			if raw, ok := n.Config["backoff"].(map[string]any); ok {
				if v, present := configNumber(raw, "base_delay_ms"); present && v <= 0 {
					errs = append(errs, ValidationError{Code: "missing_config", Message: "retry node backoff.base_delay_ms must be > 0", NodeID: n.ID})
				}
				if v, present := configNumber(raw, "multiplier"); present && v < 1 {
					errs = append(errs, ValidationError{Code: "missing_config", Message: "retry node backoff.multiplier must be >= 1", NodeID: n.ID})
				}
				if v, present := configNumber(raw, "max_delay_ms"); present && v <= 0 {
					errs = append(errs, ValidationError{Code: "missing_config", Message: "retry node backoff.max_delay_ms must be > 0", NodeID: n.ID})
				}
			}
		case NodeWaitUntil:
			cfg := WaitUntilConfigFromNode(n)
			errs = append(errs, validateExpressionConfig(n, cfg.Expression)...)
			if v, present := configNumber(n.Config, "timeout_seconds"); present && v <= 0 {
				errs = append(errs, ValidationError{Code: "missing_config", Message: "wait_until node config.timeout_seconds must be > 0", NodeID: n.ID})
			}
			if v, present := configNumber(n.Config, "poll_interval_ms"); present && v <= 0 {
				errs = append(errs, ValidationError{Code: "missing_config", Message: "wait_until node config.poll_interval_ms must be > 0", NodeID: n.ID})
			}
		}
	}
	return errs
}

// validateExpressionConfig parses one control-flow expression at workflow
// save time so syntax errors surface as typed 422 validation errors instead of
// mid-run failures.
func validateExpressionConfig(n Node, expr string) []ValidationError {
	if strings.TrimSpace(expr) == "" {
		return nil // the missing_config pass already reported it
	}
	// Syntax-only check: at save time no node outputs exist, so path
	// references ($.nodes.<id>.output..., $.context.<key>) resolve at run
	// time. Parse errors, bare identifiers, function calls and unknown roots
	// are still rejected here with the node id + reason attached.
	if err := ValidateExpressionSyntax(expr); err != nil {
		reason := err.Error()
		var ee *ExprError
		if asExprError(err, &ee) {
			reason = ee.Message
		}
		return []ValidationError{{Code: "invalid_expression", Message: fmt.Sprintf("node %s: invalid expression: %s", n.ID, reason), NodeID: n.ID}}
	}
	return nil
}

// validateNodeConfig returns "" when the node carries the config its type
// requires, otherwise a human-readable message.
func validateNodeConfig(n Node) string {
	switch n.Type {
	case NodeAgent:
		if configString(n.Config, "agent_id") == "" {
			return "agent node requires config.agent_id"
		}
	case NodeTool:
		if configString(n.Config, "tool_id") == "" {
			return "tool node requires config.tool_id"
		}
	case NodeCondition:
		if configString(n.Config, "expression") == "" {
			return "condition node requires config.expression"
		}
	case NodeSwitch:
		if configString(n.Config, "expression") == "" {
			return "switch node requires config.expression"
		}
	case NodeLoop:
		if configString(n.Config, "expression") == "" {
			return "loop node requires config.expression"
		}
	case NodeRetry:
		if configString(n.Config, "target") == "" {
			return "retry node requires config.target"
		}
	case NodeWaitUntil:
		if configString(n.Config, "expression") == "" {
			return "wait_until node requires config.expression"
		}
	case NodeDelay:
		if configFloat(n.Config, "seconds") <= 0 {
			return "delay node requires config.seconds > 0"
		}
	case NodeWebhook:
		if configString(n.Config, "url") == "" {
			return "webhook node requires config.url"
		}
	}
	return ""
}

// findCycle runs a depth-first search over the graph and returns the first
// node discovered on a back edge (i.e. part of a cycle), or "" when the graph
// is acyclic. Edges referencing unknown nodes are ignored so the check is
// safe to run alongside the other validations. Edges INTO a loop node are
// exempt from the static cycle rule (issue #109): a loop-back edge from the
// loop's own body is structural (the loop node executes once, its body
// iterates under max_iterations), and any other edge into a loop is a forward
// activation whose target executes at most once per run.
func findCycle(d DSL, known map[string]bool) string {
	loopTargets := make(map[string]bool, len(d.Nodes))
	for _, n := range d.Nodes {
		if n.Type == NodeLoop {
			loopTargets[n.ID] = true
		}
	}
	adjacent := make(map[string][]string)
	for _, e := range d.Edges {
		if known[e.From] && known[e.To] && !loopTargets[e.To] {
			adjacent[e.From] = append(adjacent[e.From], e.To)
		}
	}
	const (
		white = 0 // unvisited
		gray  = 1 // in progress (on the DFS stack)
		black = 2 // finished
	)
	color := make(map[string]int, len(d.Nodes))
	var visit func(id string) string
	visit = func(id string) string {
		color[id] = gray
		for _, next := range adjacent[id] {
			switch color[next] {
			case gray:
				return next
			case white:
				if found := visit(next); found != "" {
					return found
				}
			}
		}
		color[id] = black
		return ""
	}
	for _, n := range d.Nodes {
		if color[n.ID] == white {
			if found := visit(n.ID); found != "" {
				return found
			}
		}
	}
	return ""
}

// TopoOrder returns the node ids in a dependency-respecting (topological)
// order using Kahn's algorithm with deterministic tie-breaking on the DSL
// declaration order. It fails with an error when the graph contains a cycle.
// Loop-back edges (edges into a loop node, issue #109) are ignored: the loop
// node is placed once and its body executes per item under the loop's
// iteration cap.
func TopoOrder(d DSL) ([]string, error) {
	loopTargets := make(map[string]bool, len(d.Nodes))
	for _, n := range d.Nodes {
		if n.Type == NodeLoop {
			loopTargets[n.ID] = true
		}
	}
	inDegree := make(map[string]int, len(d.Nodes))
	order := make([]string, 0, len(d.Nodes))
	for _, n := range d.Nodes {
		inDegree[n.ID] += 0
	}
	for _, e := range d.Edges {
		if _, ok := inDegree[e.From]; !ok {
			continue
		}
		if _, ok := inDegree[e.To]; !ok || loopTargets[e.To] {
			continue
		}
		inDegree[e.To]++
	}
	declared := make([]string, 0, len(d.Nodes))
	for _, n := range d.Nodes {
		declared = append(declared, n.ID)
	}
	remaining := len(declared)
	for remaining > 0 {
		progress := false
		for _, id := range declared {
			if inDegree[id] < 0 {
				continue // already emitted
			}
			if inDegree[id] == 0 {
				order = append(order, id)
				inDegree[id] = -1
				remaining--
				progress = true
				for _, e := range d.Edges {
					if e.From == id && inDegree[e.To] > 0 && !loopTargets[e.To] {
						inDegree[e.To]--
					}
				}
			}
		}
		if !progress {
			return nil, errors.New("workflow graph contains a cycle")
		}
	}
	return order, nil
}

// configString reads a string config entry safely.
func configString(config map[string]any, key string) string {
	if config == nil {
		return ""
	}
	if v, ok := config[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// configFloat reads a numeric config entry safely (JSON numbers decode as
// float64; integers stored by Go callers may be int/int64).
func configFloat(config map[string]any, key string) float64 {
	v, _ := configNumber(config, key)
	return v
}

// configNumber reads a numeric config entry and reports whether the key was
// present at all, so validation can distinguish an explicit 0 (a mistake)
// from an absent key (fall back to the documented default).
func configNumber(config map[string]any, key string) (float64, bool) {
	if config == nil {
		return 0, false
	}
	switch v := config[key].(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	}
	return 0, false
}

// nodeMap indexes the DSL nodes by id.
func nodeMap(d DSL) map[string]Node {
	out := make(map[string]Node, len(d.Nodes))
	for _, n := range d.Nodes {
		out[n.ID] = n
	}
	return out
}

// firstAgentNodeID returns the agent_id of the first agent node (declaration
// order) or "" when the graph has none. Tool nodes without an explicit
// config.agent_id fall back to it so their runs reference a real agent row.
func firstAgentNodeID(d DSL) string {
	for _, n := range d.Nodes {
		if n.Type == NodeAgent {
			if id := configString(n.Config, "agent_id"); id != "" {
				return id
			}
		}
	}
	return ""
}

// resolveTemplate substitutes the {{input}} placeholder of node config inputs
// with the workflow execution input and, inside loop bodies, the {{item}}
// placeholder with the current iteration item; an empty template passes the
// input through unchanged.
func resolveTemplate(template, input string) string {
	if strings.TrimSpace(template) == "" {
		return input
	}
	return strings.ReplaceAll(template, "{{input}}", input)
}

// SwitchTargetsFromConfig reads a switch node's config.cases (map of match
// value -> target node id) and config.default. Non-string or empty case
// values are reported as empty targets so validation can flag them.
func SwitchTargetsFromConfig(config map[string]any) (cases map[string]string, defaultTarget string) {
	cases = make(map[string]string)
	if raw, ok := config["cases"].(map[string]any); ok {
		for value, target := range raw {
			if s, ok := target.(string); ok {
				cases[value] = strings.TrimSpace(s)
			} else {
				cases[value] = ""
			}
		}
	}
	defaultTarget = configString(config, "default")
	return cases, defaultTarget
}
