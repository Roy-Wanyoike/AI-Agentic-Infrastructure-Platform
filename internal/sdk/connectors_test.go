package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

// TestConnectorsResource drives the connectors surface against one fake API,
// asserting routes, request bodies (secret NAME only, never a value) and the
// response shapes of cmd/api/connectors.go.
func TestConnectorsResource(t *testing.T) {
	var reqBody map[string]any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/connectors":
			_ = json.NewDecoder(r.Body).Decode(&reqBody)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"connector":{"id":"c-1","name":"Acme CRM","type":"http",` +
				`"base_url":"https://api.acme.test","secret_ref":"ACME_TOKEN","status":"active",` +
				`"config":{"auth_style":"bearer","headers":{"X-Tenant":"acme"},"api_key_header":"",` +
				`"api_key_prefix":"","username":""},` +
				`"created_by":"u-1","created_at":"2025-07-01T12:00:00Z","updated_at":"2025-07-01T12:00:00Z",` +
				`"last_check_at":null,"last_check_status":""}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/connectors":
			_, _ = w.Write([]byte(`{"connectors":[{"id":"c-1","name":"Acme CRM","type":"http",` +
				`"base_url":"https://api.acme.test","secret_ref":"ACME_TOKEN","status":"active",` +
				`"config":{"auth_style":"bearer"},` +
				`"created_by":"u-1","created_at":"2025-07-01T12:00:00Z","updated_at":"2025-07-01T12:00:00Z",` +
				`"last_check_at":"2025-07-02T08:00:00Z","last_check_status":"ok"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/connectors/c-1":
			_, _ = w.Write([]byte(`{"connector":{"id":"c-1","name":"Acme CRM","type":"http",` +
				`"base_url":"https://api.acme.test","secret_ref":"ACME_TOKEN","status":"active",` +
				`"config":{},` +
				`"created_by":"u-1","created_at":"2025-07-01T12:00:00Z","updated_at":"2025-07-01T12:00:00Z",` +
				`"last_check_at":null,"last_check_status":""}}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/connectors/c-1":
			_, _ = w.Write([]byte(`{"deleted":true}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/connectors/c-1/test":
			_, _ = w.Write([]byte(`{"test":{"connector_id":"c-1","status":"ok","status_code":200,` +
				`"latency_ms":42,"checked_at":"2025-07-02T08:00:00Z"}}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ctx := context.Background()

	created, err := c.CreateConnector(ctx, CreateConnectorRequest{
		Name:      "Acme CRM",
		Type:      "http",
		BaseURL:   "https://api.acme.test",
		AuthStyle: "bearer",
		Headers:   map[string]string{"X-Tenant": "acme"},
		SecretRef: "ACME_TOKEN", // NAME reference — a value here would be a bug
	})
	if err != nil {
		t.Fatalf("CreateConnector: %v", err)
	}
	if created.ID != "c-1" || created.SecretRef != "ACME_TOKEN" {
		t.Errorf("created = %+v", created)
	}
	if created.LastCheckAt != nil || created.LastCheckStatus != "" {
		t.Errorf("fresh connector must never have been checked: %+v", created)
	}
	if reqBody["secret_ref"] != "ACME_TOKEN" || reqBody["base_url"] != "https://api.acme.test" {
		t.Errorf("create body = %v", reqBody)
	}

	list, err := c.ListConnectors(ctx)
	if err != nil {
		t.Fatalf("ListConnectors: %v", err)
	}
	if len(list.Connectors) != 1 || list.Connectors[0].LastCheckStatus != "ok" {
		t.Errorf("list = %+v", list.Connectors)
	}
	if list.Connectors[0].LastCheckAt == nil {
		t.Errorf("last_check_at should decode")
	}

	one, err := c.GetConnector(ctx, "c-1")
	if err != nil {
		t.Fatalf("GetConnector: %v", err)
	}
	if one.Name != "Acme CRM" {
		t.Errorf("get = %+v", one)
	}

	test, err := c.TestConnector(ctx, "c-1")
	if err != nil {
		t.Fatalf("TestConnector: %v", err)
	}
	if test.Status != "ok" || test.StatusCode != 200 || test.LatencyMS != 42 || test.Error != "" {
		t.Errorf("test = %+v", test)
	}

	if err := c.DeleteConnector(ctx, "c-1"); err != nil {
		t.Fatalf("DeleteConnector: %v", err)
	}
}

func TestConnectorValidationError(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":{"code":"VALIDATION_ERROR","message":"connector type is invalid"}}`))
	})
	_, err := c.CreateConnector(context.Background(), CreateConnectorRequest{Name: "x", Type: "nope", BaseURL: "https://x"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 422 || apiErr.Code != "VALIDATION_ERROR" {
		t.Fatalf("want 422 VALIDATION_ERROR, got %v", err)
	}
}
