package billing

import "testing"

func TestPlanAllowsUsageAndTracksSpend(t *testing.T) {
	service := NewService()
	plan, err := service.CreatePlan("starter", 1200, 10)
	if err != nil {
		t.Fatalf("CreatePlan returned error: %v", err)
	}
	if plan == nil || plan.Name != "starter" {
		t.Fatal("plan should be created with a name")
	}
	if _, err := service.Consume("starter", 250); err != nil {
		t.Fatalf("Consume returned error: %v", err)
	}
	if _, err := service.Consume("starter", 900); err != nil {
		t.Fatalf("Consume returned error: %v", err)
	}
	if _, err := service.Consume("starter", 51); err == nil {
		t.Fatal("consume beyond plan quota should fail")
	}
	usage := service.Usage("starter")
	if usage != 1150 {
		t.Fatalf("expected usage 1150, got %d", usage)
	}
}

func TestServiceRejectsUnknownPlan(t *testing.T) {
	service := NewService()
	if _, err := service.Consume("unknown", 50); err == nil {
		t.Fatal("unknown plan should reject consumption")
	}
}
