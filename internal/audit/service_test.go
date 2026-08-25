package audit

import "testing"

func TestAuditLogLifecycle(t *testing.T) {
    service := NewService()
    entry := service.Log("user-1", "agent.created", "org-1", "agents/agent-1")
    if entry == nil || entry.ID == "" { t.Fatal("audit entry should be populated") }
    if len(service.List()) != 1 { t.Fatalf("expected 1 audit entry, got %d", len(service.List())) }
}
