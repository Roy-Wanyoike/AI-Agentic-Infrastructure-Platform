package secrets

import "testing"

func TestServiceStoresAndRetrievesSecret(t *testing.T) {
	service := NewService()
	secret, err := service.Store("database-password", "super-secret")
	if err != nil {
		t.Fatalf("Store returned error: %v", err)
	}
	if secret == nil || secret.Name != "database-password" {
		t.Fatal("stored secret should be returned with a valid name")
	}
	value, ok := service.Get("database-password")
	if !ok {
		t.Fatal("secret should be retrievable by name")
	}
	if value != "super-secret" {
		t.Fatalf("expected value %q, got %q", "super-secret", value)
	}
}

func TestServiceRejectsBlankValues(t *testing.T) {
	service := NewService()
	if _, err := service.Store("", "value"); err == nil {
		t.Fatal("blank secret names should be rejected")
	}
	if _, err := service.Store("token", ""); err == nil {
		t.Fatal("blank secret values should be rejected")
	}
}

func TestServiceRotatesSecretValue(t *testing.T) {
	service := NewService()
	if _, err := service.Store("api-key", "old-value"); err != nil {
		t.Fatalf("Store returned error: %v", err)
	}
	rotated, err := service.Rotate("api-key", "new-value")
	if err != nil {
		t.Fatalf("Rotate returned error: %v", err)
	}
	if rotated == nil || rotated.Value != "new-value" {
		t.Fatal("rotated secret should contain the new value")
	}
	value, ok := service.Get("api-key")
	if !ok || value != "new-value" {
		t.Fatalf("expected rotated value %q, got %q, ok=%v", "new-value", value, ok)
	}
}
