package apikeys

import "testing"

func TestCreateAndRevokeKey(t *testing.T) {
	service := NewService()
	key, err := service.Create("org-1", "user-1", "prod-readonly")
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if key.Value == "" {
		t.Fatal("API key value should not be empty")
	}
	if _, ok := service.Validate(key.Value); !ok {
		t.Fatal("Validate should accept a fresh key")
	}
	if err := service.Revoke(key.ID); err != nil {
		t.Fatalf("Revoke returned error: %v", err)
	}
	if _, ok := service.Validate(key.Value); ok {
		t.Fatal("Validate should reject revoked keys")
	}
}
