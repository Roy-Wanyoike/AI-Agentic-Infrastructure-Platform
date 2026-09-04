package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

// TestSecretsResource drives the secrets surface against one fake API,
// asserting routes, methods, request bodies and the metadata-only/value-once
// wire shapes of cmd/api/secrets.go.
func TestSecretsResource(t *testing.T) {
	var reqBody map[string]any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/secrets":
			_ = json.NewDecoder(r.Body).Decode(&reqBody)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"secret":{"name":"STRIPE_KEY","key_version":1,"created_by":"u-1",` +
				`"created_at":"2025-07-01T12:00:00Z","updated_at":"2025-07-01T12:00:00Z"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/secrets":
			_, _ = w.Write([]byte(`{"secrets":[{"name":"STRIPE_KEY","key_version":1,"created_by":"u-1",` +
				`"created_at":"2025-07-01T12:00:00Z","updated_at":"2025-07-01T12:00:00Z"}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/secrets/STRIPE_KEY/reveal":
			_, _ = w.Write([]byte(`{"secret":{"name":"STRIPE_KEY","key_version":1,"created_by":"u-1",` +
				`"created_at":"2025-07-01T12:00:00Z","updated_at":"2025-07-01T12:00:00Z","value":"sk_live_once"}}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/secrets/STRIPE_KEY":
			_, _ = w.Write([]byte(`{"deleted":true}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ctx := context.Background()

	meta, err := c.CreateSecret(ctx, CreateSecretRequest{Name: "STRIPE_KEY", Value: "sk_live_123"})
	if err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	if meta.Name != "STRIPE_KEY" || meta.KeyVersion != 1 {
		t.Errorf("meta = %+v", meta)
	}
	// The value travels in the request body exactly once; the response is
	// metadata-only (decoding would have failed on an unexpected "value").
	if reqBody["name"] != "STRIPE_KEY" || reqBody["value"] != "sk_live_123" {
		t.Errorf("create body = %v", reqBody)
	}

	list, err := c.ListSecrets(ctx)
	if err != nil {
		t.Fatalf("ListSecrets: %v", err)
	}
	if len(list.Secrets) != 1 || list.Secrets[0].CreatedBy != "u-1" {
		t.Errorf("list = %+v", list.Secrets)
	}

	revealed, err := c.RevealSecret(ctx, "STRIPE_KEY")
	if err != nil {
		t.Fatalf("RevealSecret: %v", err)
	}
	if revealed.Value != "sk_live_once" || revealed.Name != "STRIPE_KEY" {
		t.Errorf("revealed = %+v", revealed)
	}

	if err := c.DeleteSecret(ctx, "STRIPE_KEY"); err != nil {
		t.Fatalf("DeleteSecret: %v", err)
	}
}

func TestSecretsDeleteEscapesName(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RequestURI() != "/v1/secrets/a%2Fb" {
			t.Errorf("RequestURI = %q", r.URL.RequestURI())
		}
		_, _ = w.Write([]byte(`{"deleted":true}`))
	})
	if err := c.DeleteSecret(context.Background(), "a/b"); err != nil {
		t.Fatalf("DeleteSecret: %v", err)
	}
}

func TestSecretsConflictError(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"SECRET_ALREADY_EXISTS","message":"secret already exists"}}`))
	})
	_, err := c.CreateSecret(context.Background(), CreateSecretRequest{Name: "X", Value: "Y"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 409 || apiErr.Code != "SECRET_ALREADY_EXISTS" {
		t.Fatalf("want 409 SECRET_ALREADY_EXISTS, got %v", err)
	}
}
