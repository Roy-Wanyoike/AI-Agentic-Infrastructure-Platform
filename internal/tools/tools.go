package tools

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

type Tool interface {
	Name() string
	Execute(map[string]any) (map[string]any, error)
}

type Registry struct {
	tools map[string]Tool
}

func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

func (r *Registry) Register(tool Tool) {
	if r == nil || r.tools == nil { return }
	r.tools[tool.Name()] = tool
}

func (r *Registry) Get(name string) (Tool, bool) {
	tool, found := r.tools[name]
	return tool, found
}

type CalculatorTool struct{}

func NewCalculatorTool() *CalculatorTool { return &CalculatorTool{} }

func (t *CalculatorTool) Name() string { return "calculator" }

func (t *CalculatorTool) Execute(input map[string]any) (map[string]any, error) {
	expression, ok := input["expression"].(string)
	if !ok || strings.TrimSpace(expression) == "" {
		return nil, errors.New("expression is required")
	}
	result, err := evaluateExpression(expression)
	if err != nil {
		return nil, err
	}
	if math.Abs(result-math.Trunc(result)) < 1e-9 {
		return map[string]any{"result": int(result), "expression": expression}, nil
	}
	return map[string]any{"result": result, "expression": expression}, nil
}

func evaluateExpression(expression string) (float64, error) {
	trimmed := strings.TrimSpace(expression)
	if trimmed == "" { return 0, errors.New("expression is required") }
	value, err := strconv.ParseFloat(trimmed, 64)
	if err == nil { return value, nil }
	if strings.Contains(trimmed, "+") {
		parts := strings.SplitN(trimmed, "+", 2)
		left, errLeft := evaluateExpression(parts[0])
		right, errRight := evaluateExpression(parts[1])
		if errLeft != nil || errRight != nil { return 0, fmt.Errorf("invalid expression: %s", trimmed) }
		return left + right, nil
	}
	if strings.Contains(trimmed, "-") {
		parts := strings.SplitN(trimmed, "-", 2)
		left, errLeft := evaluateExpression(parts[0])
		right, errRight := evaluateExpression(parts[1])
		if errLeft != nil || errRight != nil { return 0, fmt.Errorf("invalid expression: %s", trimmed) }
		return left - right, nil
	}
	if strings.Contains(trimmed, "*") {
		parts := strings.SplitN(trimmed, "*", 2)
		left, errLeft := evaluateExpression(parts[0])
		right, errRight := evaluateExpression(parts[1])
		if errLeft != nil || errRight != nil { return 0, fmt.Errorf("invalid expression: %s", trimmed) }
		return left * right, nil
	}
	if strings.Contains(trimmed, "/") {
		parts := strings.SplitN(trimmed, "/", 2)
		left, errLeft := evaluateExpression(parts[0])
		right, errRight := evaluateExpression(parts[1])
		if errLeft != nil || errRight != nil { return 0, fmt.Errorf("invalid expression: %s", trimmed) }
		if math.Abs(right) < 1e-9 { return 0, errors.New("division by zero") }
		return left / right, nil
	}
	return 0, fmt.Errorf("unsupported expression: %s", trimmed)
}
