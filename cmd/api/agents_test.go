package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentos/internal/agents"
	"agentos/internal/auth"
	"agentos/internal/queue"
)

func TestListAgentsRequiresMatchingOrganization(t *testing.T) {
	authService := auth.NewService("dev-secret")
	_, user, err := authService.Register("Acme", "alice@example.com", "secret123")
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	token, err := authService.GenerateToken(user)
	if err != nil {
		t.Fatalf("GenerateToken returned error: %v", err)
	}

	agentService := agents.NewService()
	if _, err := agentService.Create(user.Organization, "Support Agent", "desc", "help users", "gpt-4o-mini"); err != nil {
		t.Fatalf("Create agent returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/agents?organization_id=org-999", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	auth.RequireAuth(authService)(listAgentsHandler(agentService)).ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected status %d, got %d body=%s", http.StatusForbidden, rr.Code, rr.Body.String())
	}
}

func TestCreateAgentRequiresMatchingOrganization(t *testing.T) {
	authService := auth.NewService("dev-secret")
	_, user, err := authService.Register("Acme", "bob@example.com", "secret123")
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	token, err := authService.GenerateToken(user)
	if err != nil {
		t.Fatalf("GenerateToken returned error: %v", err)
	}

	agentService := agents.NewService()
	payload := `{"organization_id":"org-999","name":"Support Agent","description":"desc","instructions":"help","model":"gpt-4o-mini"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/agents/create", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	auth.RequireAuth(authService)(createAgentHandler(agentService)).ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected status %d, got %d body=%s", http.StatusForbidden, rr.Code, rr.Body.String())
	}
}

func TestCreateRunRequiresMatchingOrganization(t *testing.T) {
	authService := auth.NewService("dev-secret")
	_, user, err := authService.Register("Acme", "runs@example.com", "secret123")
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	token, err := authService.GenerateToken(user)
	if err != nil {
		t.Fatalf("GenerateToken returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/runs", strings.NewReader(`{"organization_id":"org-999","agent_id":"agent-1","input":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	auth.RequireAuth(authService)(createRunHandler(queue.NewQueue())).ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected status %d, got %d body=%s", http.StatusForbidden, rr.Code, rr.Body.String())
	}
}

func TestCreateRunEnqueuesQueuedTask(t *testing.T) {
	authService := auth.NewService("dev-secret")
	_, user, err := authService.Register("Acme", "queued@example.com", "secret123")
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	token, err := authService.GenerateToken(user)
	if err != nil {
		t.Fatalf("GenerateToken returned error: %v", err)
	}

	q := queue.NewQueue()
	req := httptest.NewRequest(http.MethodPost, "/v1/runs", strings.NewReader(`{"organization_id":"`+user.Organization+`","agent_id":"agent-1","input":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	auth.RequireAuth(authService)(createRunHandler(q)).ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d body=%s", http.StatusCreated, rr.Code, rr.Body.String())
	}
	if q.Length() != 1 {
		t.Fatalf("expected queue length 1 after enqueue, got %d", q.Length())
	}
	if task := q.Peek(); task == nil || task.Type != "agent.run" {
		t.Fatal("expected queued task to be an agent.run task")
	}
}
