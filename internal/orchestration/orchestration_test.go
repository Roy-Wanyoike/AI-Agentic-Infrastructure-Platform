package orchestration

import "testing"

func TestPlannerCreatesPhaseSpecialists(t *testing.T) {
	planner := NewPlanner()
	plan := planner.BuildDefaultPlan()
	if len(plan.Specialists) < 4 {
		t.Fatalf("expected at least 4 specialists, got %d", len(plan.Specialists))
	}
	if len(plan.Tasks) == 0 {
		t.Fatal("default plan should include tasks")
	}
	if plan.PhaseNames()[0] == "" {
		t.Fatal("phase order should be defined")
	}
}

func TestTaskCompletionChecksVerification(t *testing.T) {
	planner := NewPlanner()
	plan := planner.BuildDefaultPlan()
	if err := planner.CompleteTask(plan, "foundation", "repo-scaffold", "go test ./..."); err != nil {
		t.Fatalf("foundation task should be completable with verification: %v", err)
	}
	if err := planner.CompleteTask(plan, "auth", "rbac", ""); err == nil {
		t.Fatal("task with empty verification should be rejected")
	}
}

func TestNextPhaseIsBlockedUntilCurrentPhasePasses(t *testing.T) {
	planner := NewPlanner()
	plan := planner.BuildDefaultPlan()
	if planner.CanAdvance(plan, "foundation") {
		t.Fatal("new plan should not advance before all tasks are complete")
	}
	for _, task := range plan.Tasks {
		if task.Phase == "foundation" {
			planner.CompleteTask(plan, task.Phase, task.ID, "go test ./...")
		}
	}
	if !planner.CanAdvance(plan, "foundation") {
		t.Fatal("completed foundation tasks should allow advancement")
	}
}
