package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// TestAPIKeysResource drives the api-keys surface against one fake API,
// asserting routes and the mint-once/metadata-only contract of
// cmd/api/apikeys.go.
func TestAPIKeysResource(t *testing.T) {
	var reqBody map[string]any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/api-keys":
			_ = json.NewDecoder(r.Body).Decode(&reqBody)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"api_key":{"id":"ak-1","name":"ci-deploy","prefix":"ak_ab12",` +
				`"created_by":"u-1","created_at":"2025-07-01T12:00:00Z","revoked":false},` +
				`"value":"ak_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/api-keys":
			_, _ = w.Write([]byte(`{"api_keys":[{"id":"ak-1","name":"ci-deploy","prefix":"ak_ab12",` +
				`"created_by":"u-1","created_at":"2025-07-01T12:00:00Z","revoked":false}]}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/api-keys/ak-1":
			_, _ = w.Write([]byte(`{"revoked":true}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ctx := context.Background()

	created, err := c.CreateAPIKey(ctx, CreateAPIKeyRequest{Name: "ci-deploy"})
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	if created.APIKey.ID != "ak-1" || created.APIKey.Prefix != "ak_ab12" || created.APIKey.Revoked {
		t.Errorf("metadata = %+v", created.APIKey)
	}
	if !strings.HasPrefix(created.Value, "ak_") || len(created.Value) != len("ak_")+64 {
		t.Errorf("value shape = %q", created.Value)
	}
	if reqBody["name"] != "ci-deploy" {
		t.Errorf("create body = %v", reqBody)
	}

	list, err := c.ListAPIKeys(ctx)
	if err != nil {
		t.Fatalf("ListAPIKeys: %v", err)
	}
	if len(list.APIKeys) != 1 || list.APIKeys[0].CreatedBy != "u-1" {
		t.Errorf("list = %+v", list.APIKeys)
	}

	if err := c.RevokeAPIKey(ctx, "ak-1"); err != nil {
		t.Fatalf("RevokeAPIKey: %v", err)
	}
}

func TestAPIKeysListEmptyStaysSlice(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"api_keys":[]}`))
	})
	list, err := c.ListAPIKeys(context.Background())
	if err != nil {
		t.Fatalf("ListAPIKeys: %v", err)
	}
	if list.APIKeys == nil || len(list.APIKeys) != 0 {
		t.Errorf("want non-nil empty list, got %#v", list.APIKeys)
	}
}

func TestRevokeAPIKeyNotFound(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"API_KEY_NOT_FOUND","message":"api key not found"}}`))
	})
	err := c.RevokeAPIKey(context.Background(), "missing")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 404 || apiErr.Code != "API_KEY_NOT_FOUND" {
		t.Fatalf("want 404 API_KEY_NOT_FOUND, got %v", err)
	}
}
