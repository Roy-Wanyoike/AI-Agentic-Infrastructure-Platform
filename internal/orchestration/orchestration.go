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

func (p *Planner) BuildDefaultPlan() *Plan {
	plan := &Plan{
		Specialists: []Specialist{
			{ID: "foundation-agent", Name: "Foundation Agent", Phase: "foundation", Focus: "repo scaffold, config, build verification, migration setup"},
			{ID: "auth-agent", Name: "Auth Agent", Phase: "auth", Focus: "tenant auth, RBAC, API keys, organization access"},
			{ID: "agents-agent", Name: "Agents Agent", Phase: "agents", Focus: "agent CRUD, versioning, metadata, API contracts"},
			{ID: "runtime-agent", Name: "Runtime Agent", Phase: "runtime", Focus: "execution engine, tool loop, queue handoff, state handling"},
			{ID: "tools-agent", Name: "Tools Agent", Phase: "tools", Focus: "tool registry, permissions, safe execution, validation"},
			{ID: "worker-agent", Name: "Worker Agent", Phase: "async-worker", Focus: "async queue, retries, dead-letter, worker recovery"},
			{ID: "memory-agent", Name: "Memory Agent", Phase: "memory", Focus: "event memory, run history, searchable context store"},
			{ID: "streaming-agent", Name: "Streaming Agent", Phase: "streaming", Focus: "live run streams, SSE/WebSocket, dashboard status updates"},
			{ID: "workflow-agent", Name: "Workflow Agent", Phase: "workflows", Focus: "approval flow, workflow state machine, conditional steps"},
			{ID: "scheduler-agent", Name: "Scheduler Agent", Phase: "scheduler", Focus: "cron schedules, interval triggers, automation"},
			{ID: "hardening-agent", Name: "Hardening Agent", Phase: "hardening", Focus: "security headers, rate limits, quotas, production config"},
			{ID: "scale-agent", Name: "Scale Agent", Phase: "scale", Focus: "load tests, throughput checks, failover and recovery validation"},
		},
		Tasks: []Task{
			{ID: "repo-scaffold", Phase: "foundation", Description: "Verify repo structure, config, build, and migration baseline", Verification: "go test ./..."},
			{ID: "auth-core", Phase: "auth", Description: "Implement secure auth and tenant claims", Verification: "go test ./..."},
			{ID: "rbac", Phase: "auth", Description: "Enforce role-based permissions and org access", Verification: "go test ./..."},
			{ID: "agent-crud", Phase: "agents", Description: "Create and version agents with safe API contracts", Verification: "go test ./..."},
			{ID: "agent-runtime", Phase: "runtime", Description: "Implement agent execution runtime and run state model", Verification: "go test ./..."},
			{ID: "tooling", Phase: "tools", Description: "Register safe tools and execution policies", Verification: "go test ./..."},
			{ID: "async-worker", Phase: "async-worker", Description: "Queue work, retry failures, and run workers reliably", Verification: "go test ./..."},
			{ID: "memory-history", Phase: "memory", Description: "Capture memory and run history for traceability", Verification: "go test ./..."},
			{ID: "streaming-ui", Phase: "streaming", Description: "Expose live run status updates and dashboard hooks", Verification: "go test ./..."},
			{ID: "workflow-engine", Phase: "workflows", Description: "Implement workflow execution and approval model", Verification: "go test ./..."},
			{ID: "schedule-webhooks", Phase: "scheduler", Description: "Add cron-based triggers and event callbacks", Verification: "go test ./..."},
			{ID: "production-hardening", Phase: "hardening", Description: "Add production safeguards and security posture", Verification: "go test ./..."},
			{ID: "load-validation", Phase: "scale", Description: "Run throughput and resilience validation against the platform", Verification: "go test ./..."},
		},
	}
	return plan
}

func (p *Plan) PhaseNames() []string {
	return []string{"foundation", "auth", "agents", "runtime", "tools", "async-worker", "memory", "streaming", "workflows", "scheduler", "hardening", "scale"}
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

func (p *Planner) CanAdvance(plan *Plan, phase string) bool {
	if plan == nil {
		return false
	}
	for _, task := range plan.Tasks {
		if task.Phase == phase && !task.Completed {
			return false
		}
	}
	return true
}

func (p *Planner) DeployPhaseAgents(plan *Plan) ([]Specialist, error) {
	if plan == nil {
		return nil, errors.New("plan is required")
	}
	if len(plan.Specialists) == 0 {
		return nil, errors.New("no specialists configured")
	}

	phaseMap := make(map[string]Specialist, len(plan.Specialists))
	for _, specialist := range plan.Specialists {
		if strings.TrimSpace(specialist.Phase) == "" {
			return nil, fmt.Errorf("specialist %s is missing a phase", specialist.ID)
		}
		if _, exists := phaseMap[specialist.Phase]; exists {
			return nil, fmt.Errorf("duplicate specialist assigned to phase %s", specialist.Phase)
		}
		phaseMap[specialist.Phase] = specialist
	}

	orderedPhases := p.BuildDefaultPlan().PhaseNames()
	if len(plan.PhaseNames()) > 0 {
		orderedPhases = plan.PhaseNames()
	}

	assigned := make([]Specialist, 0, len(orderedPhases))
	for _, phase := range orderedPhases {
		specialist, ok := phaseMap[phase]
		if !ok {
			return nil, fmt.Errorf("no specialist assigned to phase %s", phase)
		}
		assigned = append(assigned, specialist)
	}
	return assigned, nil
}

func (p *Planner) NextPhase(plan *Plan, phase string) (string, bool) {
	if plan == nil {
		return "", false
	}
	if !p.CanAdvance(plan, phase) {
		return "", false
	}

	phases := plan.PhaseNames()
	for i, current := range phases {
		if current == phase {
			if i+1 < len(phases) {
				return phases[i+1], true
			}
			return "", false
		}
	}
	return "", false
}
