package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"agentos/internal/apikeys"
)

func TestPasswordHashAndVerify(t *testing.T) {
	hash, err := HashPassword("secret123")
	if err != nil {
		t.Fatalf("HashPassword returned error: %v", err)
	}
	if hash == "" {
		t.Fatal("HashPassword returned empty hash")
	}
	if !VerifyPassword(hash, "secret123") {
		t.Fatal("VerifyPassword should accept the original password")
	}
	if VerifyPassword(hash, "wrongpass") {
		t.Fatal("VerifyPassword should reject the wrong password")
	}
}

func TestNewServiceRegistrationFlow(t *testing.T) {
	service := NewService("dev-secret")
	org, user, err := service.Register("Acme", "alice@example.com", "secret123")
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	if org.Name != "Acme" {
		t.Fatalf("organization name mismatch: got %q", org.Name)
	}
	if user.Email != "alice@example.com" {
		t.Fatalf("user email mismatch: got %q", user.Email)
	}
	if !VerifyPassword(user.PasswordHash, "secret123") {
		t.Fatal("stored password hash should verify")
	}

	loggedIn, err := service.Login("alice@example.com", "secret123")
	if err != nil {
		t.Fatalf("Login returned error: %v", err)
	}
	if loggedIn == "" {
		t.Fatal("Login returned empty token")
	}
}

func TestServiceJWTRoundTrip(t *testing.T) {
	service := NewService("jwt-secret")
	_, user, err := service.Register("Acme", "jwt@example.com", "secret123")
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	token, err := service.GenerateToken(user)
	if err != nil {
		t.Fatalf("GenerateToken returned error: %v", err)
	}
	claims, err := service.ValidateToken(token)
	if err != nil {
		t.Fatalf("ValidateToken returned error: %v", err)
	}
	if claims.UserID != user.ID {
		t.Fatalf("token user ID mismatch: got %q want %q", claims.UserID, user.ID)
	}
	if claims.OrganizationID != user.Organization {
		t.Fatalf("token org ID mismatch: got %q want %q", claims.OrganizationID, user.Organization)
	}
	if _, err := service.ValidateToken("invalid.token"); err == nil {
		t.Fatal("ValidateToken should reject malformed tokens")
	}
}

func TestHasPermission(t *testing.T) {
	service := NewService("jwt-secret")
	_, user, err := service.Register("Acme", "rbac@example.com", "secret123")
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	if !service.HasPermission(user, PermissionAgentsRead) {
		t.Fatal("OWNER should have agents.read")
	}
	if !service.HasPermission(user, PermissionOrgManage) {
		t.Fatal("OWNER should have organization.manage")
	}
	user.Role = "VIEWER"
	if service.HasPermission(user, PermissionAgentsWrite) {
		t.Fatal("VIEWER should not have agents.write")
	}
}

func TestRequireOrganizationAccess(t *testing.T) {
	service := NewService("jwt-secret")
	_, user, err := service.Register("Acme", "tenant@example.com", "secret123")
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	token, err := service.GenerateToken(user)
	if err != nil {
		t.Fatalf("GenerateToken returned error: %v", err)
	}

	handler := RequireAuth(service)(RequireOrganizationAccess("org-999", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))

	req := httptest.NewRequest(http.MethodGet, "/tenant-check", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for org mismatch, got %d body=%s", resp.Code, resp.Body.String())
	}
}

func TestRequireAuthOrAPIKeyAcceptsAPIKeyHeader(t *testing.T) {
	authService := NewService("jwt-secret")
	apiKeyService := apikeys.NewService()
	key, err := apiKeyService.Create("org-api", "user-api", "integration-key")
	if err != nil {
		t.Fatalf("Create API key returned error: %v", err)
	}

	handler := RequireAuthOrAPIKey(authService, apiKeyService)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, err := ExtractClaims(r.Context())
		if err != nil {
			t.Fatalf("ExtractClaims returned error: %v", err)
		}
		if claims.OrganizationID != "org-api" {
			t.Fatalf("expected org-api claims, got %q", claims.OrganizationID)
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/with-api-key", nil)
	req.Header.Set("X-API-Key", key.Value)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200 for valid api key, got %d body=%s", resp.Code, resp.Body.String())
	}
}
