package workflows

import "testing"

func TestApprovalGateRequiresExplicitDecision(t *testing.T) {
	service := NewService()
	wf, err := service.Create("Approval Workflow", []Step{
		{ID: "s1", Type: StepCondition, Name: "approval", Config: map[string]any{"expression": "needs_approval", "value": true}},
		{ID: "s2", Type: StepEnd, Name: "end"},
	})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if _, err := service.ExecuteWithApproval(wf.ID, ""); err == nil {
		t.Fatal("ExecuteWithApproval should require a decision")
	}
	trace, err := service.ExecuteWithApproval(wf.ID, "approved")
	if err != nil {
		t.Fatalf("ExecuteWithApproval returned error: %v", err)
	}
	if trace == "" {
		t.Fatal("workflow trace should not be empty")
	}
}
