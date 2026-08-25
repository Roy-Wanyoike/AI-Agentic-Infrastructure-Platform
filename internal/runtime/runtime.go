package runtime

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"agentos/internal/agents"
	"agentos/internal/tools"
)

type RunStatus string

const (
	StatusQueued    RunStatus = "QUEUED"
	StatusRunning   RunStatus = "RUNNING"
	StatusCompleted RunStatus = "COMPLETED"
	StatusFailed    RunStatus = "FAILED"
)

type Run struct {
	ID        string
	AgentID   string
	Input     string
	Output    string
	Status    RunStatus
	CreatedAt time.Time
	UpdatedAt time.Time
}

type Runner struct {
	agentService *agents.Service
	toolRegistry *tools.Registry
}

func NewRunner(agentService *agents.Service, toolRegistry *tools.Registry) *Runner {
	return &Runner{agentService: agentService, toolRegistry: toolRegistry}
}

func extractMathExpression(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return ""
	}
	lower := strings.ToLower(trimmed)
	for _, token := range []string{"what is ", "what's ", "calculate ", "compute ", "evaluate ", "solve "} {
		lower = strings.ReplaceAll(lower, token, "")
	}
	lower = strings.Trim(lower, "? .!:")
	if lower == "" {
		return ""
	}
	for _, ch := range []string{"+", "-", "*", "/", "%"} {
		if strings.Contains(lower, ch) {
			return strings.TrimSpace(lower)
		}
	}
	return ""
}

func (r *Runner) Run(ctx context.Context, agentID, input string) (*Run, error) {
	if r == nil || r.agentService == nil {
		return nil, errors.New("agent service is required")
	}
	if strings.TrimSpace(agentID) == "" {
		return nil, errors.New("agent id is required")
	}
	agent, ok := r.agentService.Get(agentID)
	if !ok {
		return nil, errors.New("agent not found")
	}
	_ = ctx
	output := ""
	if expr := extractMathExpression(input); expr != "" {
		if r.toolRegistry != nil {
			tool, ok := r.toolRegistry.Get("calculator")
			if ok {
				result, err := tool.Execute(map[string]any{"expression": expr})
				if err == nil {
					switch v := result["result"].(type) {
					case int64:
						output = strconv.FormatInt(v, 10)
					case float64:
						output = strconv.FormatFloat(v, 'f', -1, 64)
					case int:
						output = strconv.Itoa(v)
					case string:
						output = v
					default:
						output = "0"
					}
				}
			}
		}
	}
	if output == "" {
		output = "Completed " + agent.Name + " in response to: " + input
	}
	run := &Run{ID: "run-1", AgentID: agentID, Input: input, Output: output, Status: StatusCompleted, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	return run, nil
}
