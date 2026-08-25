package workflows

import "testing"

func TestWorkflowLifecycle(t *testing.T) {
	service := NewService()
	steps := []Step{{ID: "s1", Type: StepAgent, Name: "agent"}, {ID: "s2", Type: StepEnd, Name: "end"}}
	wf, err := service.Create("Support Workflow", steps)
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if wf.ID == "" {
		t.Fatal("workflow should have an ID")
	}
	trace, err := service.Execute(wf.ID)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if trace == "" {
		t.Fatal("workflow trace should not be empty")
	}
}

func TestWorkflowConditionStepAndValidation(t *testing.T) {
	service := NewService()
	steps := []Step{
		{ID: "s1", Type: StepCondition, Name: "check", Config: map[string]any{"expression": "2 > 1", "value": true}},
		{ID: "s2", Type: StepEnd, Name: "end"},
	}
	wf, err := service.Create("Approval Workflow", steps)
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	trace, err := service.Execute(wf.ID)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if trace == "" {
		t.Fatal("workflow trace should not be empty")
	}
	if _, err := service.Create("", []Step{{ID: "s1", Type: StepEnd, Name: "end"}}); err == nil {
		t.Fatal("Create should reject empty workflow names")
	}
}
