package orchestration

import "testing"

func TestPlannerCreatesPhaseSpecialists(t *testing.T) {
	planner := NewPlanner()
	plan := planner.BuildDefaultPlan()
	if len(plan.Specialists) < 10 {
		t.Fatalf("expected at least 10 specialists for the roadmap, got %d", len(plan.Specialists))
	}
	if len(plan.Tasks) == 0 {
		t.Fatal("default plan should include tasks")
	}
	if len(plan.PhaseNames()) < 10 {
		t.Fatalf("expected at least 10 phases, got %d", len(plan.PhaseNames()))
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

func TestPlannerDeploysOneSpecialistPerPhase(t *testing.T) {
	planner := NewPlanner()
	plan := planner.BuildDefaultPlan()
	assigned, err := planner.DeployPhaseAgents(plan)
	if err != nil {
		t.Fatalf("DeployPhaseAgents returned error: %v", err)
	}
	if len(assigned) != len(plan.Specialists) {
		t.Fatalf("expected %d deployed specialists, got %d", len(plan.Specialists), len(assigned))
	}
	seenPhases := map[string]int{}
	for _, specialist := range assigned {
		if specialist.Phase == "" {
			t.Fatal("deployed specialist should belong to a phase")
		}
		seenPhases[specialist.Phase]++
	}
	if len(seenPhases) != len(plan.PhaseNames()) {
		t.Fatalf("expected %d unique phases, got %d", len(plan.PhaseNames()), len(seenPhases))
	}
	for _, phase := range plan.PhaseNames() {
		if seenPhases[phase] != 1 {
			t.Fatalf("phase %q should have exactly one specialist assigned, got %d", phase, seenPhases[phase])
		}
	}
}

func TestPlannerNextPhaseAdvancesOnlyWhenCurrentPhasePasses(t *testing.T) {
	planner := NewPlanner()
	plan := planner.BuildDefaultPlan()
	if next, ok := planner.NextPhase(plan, "foundation"); ok || next != "" {
		t.Fatal("next phase should not advance before foundation passes")
	}
	for _, task := range plan.Tasks {
		if task.Phase == "foundation" {
			planner.CompleteTask(plan, task.Phase, task.ID, "go test ./...")
		}
	}
	if next, ok := planner.NextPhase(plan, "foundation"); !ok || next != "auth" {
		t.Fatalf("expected foundation to advance to auth, got next=%q ok=%v", next, ok)
	}
}
