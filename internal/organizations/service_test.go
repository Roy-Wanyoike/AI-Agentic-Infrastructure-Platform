package organizations

import "testing"

func TestOrganizationLifecycle(t *testing.T) {
	service := NewService()
	org, err := service.Create("Acme")
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if org.ID == "" {
		t.Fatal("organization ID should not be empty")
	}
	if err := service.AddMember(org.ID, "user-1", "OWNER"); err != nil {
		t.Fatalf("AddMember returned error: %v", err)
	}
	if len(service.Members(org.ID)) != 1 {
		t.Fatalf("expected 1 member, got %d", len(service.Members(org.ID)))
	}
}
