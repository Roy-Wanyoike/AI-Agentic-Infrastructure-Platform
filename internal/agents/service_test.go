package agents

import "testing"

func TestCreateAndListAgents(t *testing.T) {
	service := NewService()
	agent, err := service.Create("org-1", "Support Agent", "Handles customer support", "Be helpful", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if agent.ID == "" {
		t.Fatal("created agent should have an ID")
	}
	if len(service.List("org-1")) != 1 {
		t.Fatalf("expected 1 agent in org-1, got %d", len(service.List("org-1")))
	}
	if _, err := service.Create("", "Bad", "desc", "instructions", "model"); err == nil {
		t.Fatal("Create should reject empty organization IDs")
	}
}

func TestCreateVersion(t *testing.T) {
	service := NewService()
	agent, err := service.Create("org-1", "Support Agent", "desc", "v1 instructions", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	version, err := service.CreateVersion(agent.ID, "v2 instructions", "gpt-4o")
	if err != nil {
		t.Fatalf("CreateVersion returned error: %v", err)
	}
	if version.Version != 2 {
		t.Fatalf("expected version 2, got %d", version.Version)
	}
	if agent.CurrentVersionID == "" {
		t.Fatal("agent should have a current version after version creation")
	}
}
