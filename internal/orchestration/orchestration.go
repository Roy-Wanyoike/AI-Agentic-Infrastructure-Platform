package orchestration

import (
	"errors"
	"fmt"
	"strings"
)

type Specialist struct {
	ID          string
	Name        string
	Phase       string
	Focus       string
	Verification string
}

type Task struct {
	ID           string
	Phase        string
	Description  string
	Verification string
	Completed    bool
}

type Plan struct {
	Specialists []Specialist
	Tasks       []Task
}

type Planner struct{}

func NewPlanner() *Planner { return &Planner{} }

func (p *Planner) BuildDefaultPlan() Plan {
	return Plan{
		Specialists: []Specialist{
			{ID: "foundation-agent", Name: "Foundation Agent", Phase: "foundation", Focus: "repo scaffold, config, build verification"},
			{ID: "auth-agent", Name: "Auth Agent", Phase: "auth", Focus: "tenant auth, RBAC, API keys"},
			{ID: "runtime-agent", Name: "Runtime Agent", Phase: "runtime", Focus: "agents, tools, queue, execution loop"},
			{ID: "platform-agent", Name: "Platform Agent", Phase: "platform", Focus: "workflows, scheduling, integrations, observability"},
		},
		Tasks: []Task{
			{ID: "repo-scaffold", Phase: "foundation", Description: "Verify repo structure and Go build", Verification: "go test ./..."},
			{ID: "auth-core", Phase: "auth", Description: "Implement secure auth and tenant claims", Verification: "go test ./..."},
			{ID: "rbac", Phase: "auth", Description: "Enforce role-based permissions", Verification: "go test ./..."},
			{ID: "agent-runtime", Phase: "runtime", Description: "Implement agent execution runtime", Verification: "go test ./..."},
			{ID: "tooling", Phase: "runtime", Description: "Register safe tools and execution policies", Verification: "go test ./..."},
			{ID: "workflow-engine", Phase: "platform", Description: "Implement workflow execution model", Verification: "go test ./..."},
			{ID: "observability", Phase: "platform", Description: "Add metrics and telemetry", Verification: "go test ./..."},
		},
	}
}

func (p *Planner) PhaseNames() []string {
	return []string{"foundation", "auth", "runtime", "platform"}
}

func (p *Planner) CompleteTask(plan *Plan, phase, taskID, verification string) error {
	if plan == nil {
		return errors.New("plan is required")
	}
	if strings.TrimSpace(phase) == "" || strings.TrimSpace(taskID) == "" {
		return errors.New("phase and task id are required")
	}
	if strings.TrimSpace(verification) == "" {
		return errors.New("verification command is required")
	}
	for i := range plan.Tasks {
		if plan.Tasks[i].Phase == phase && plan.Tasks[i].ID == taskID {
			plan.Tasks[i].Completed = true
			plan.Tasks[i].Verification = verification
			return nil
		}
	}
	return fmt.Errorf("task %s not found in phase %s", taskID, phase)
}

func (p *Planner) CanAdvance(plan Plan, phase string) bool {
	for _, task := range plan.Tasks {
		if task.Phase == phase && !task.Completed {
			return false
		}
	}
	return true
}
