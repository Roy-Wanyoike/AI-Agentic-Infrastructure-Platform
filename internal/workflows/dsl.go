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
	// NodeEnd is the legacy terminal marker kept for workflows created
	// through the pre-DSL Step API.
	NodeEnd NodeType = "end"
)

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
	NodeEnd:       true,
}

// ValidateDSL checks the structural rules pinned by the API contract:
//   - non-empty graph
//   - unique, non-empty node ids
//   - known node types
//   - per-type required config (agent -> agent_id, tool -> tool_id,
//     condition -> expression, delay -> seconds, webhook -> url)
//   - every edge references existing nodes with a valid condition
//   - the graph is acyclic (DFS cycle detection)
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
	if len(errs) == 0 {
		return nil
	}
	return errs
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
// safe to run alongside the other validations.
func findCycle(d DSL, known map[string]bool) string {
	adjacent := make(map[string][]string)
	for _, e := range d.Edges {
		if known[e.From] && known[e.To] {
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
func TopoOrder(d DSL) ([]string, error) {
	inDegree := make(map[string]int, len(d.Nodes))
	order := make([]string, 0, len(d.Nodes))
	for _, n := range d.Nodes {
		inDegree[n.ID] += 0
	}
	for _, e := range d.Edges {
		if _, ok := inDegree[e.From]; !ok {
			continue
		}
		if _, ok := inDegree[e.To]; !ok {
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
					if e.From == id && inDegree[e.To] > 0 {
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
	if config == nil {
		return 0
	}
	switch v := config[key].(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	}
	return 0
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
// with the workflow execution input; an empty template passes the input
// through unchanged.
func resolveTemplate(template, input string) string {
	if strings.TrimSpace(template) == "" {
		return input
	}
	return strings.ReplaceAll(template, "{{input}}", input)
}
