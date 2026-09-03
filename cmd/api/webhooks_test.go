package main

// Handler tests for the webhook endpoints (track 2-e). All tests run without
// infrastructure: in-memory auth/apikeys/audit/webhooks services.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agentos/internal/apikeys"
	"agentos/internal/audit"
	"agentos/internal/auth"
	"agentos/internal/webhooks"
)

type whTestEnv struct {
	mux     *http.ServeMux
	authSvc *auth.Service
	keysSvc *apikeys.Service
	whSvc   *webhooks.Service
}

func newWebhooksTestEnv(t *testing.T) *whTestEnv {
	t.Helper()
	env := &whTestEnv{
		mux:     http.NewServeMux(),
		authSvc: auth.NewService("test-jwt-secret"),
		keysSvc: apikeys.NewService(),
		whSvc:   webhooks.NewService(),
	}
	registerWebhooksRoutes(env.mux, env.whSvc, env.authSvc, env.keysSvc, audit.NewService())
	return env
}

// registerWhUser creates an org owner and returns the user + bearer token.
func (e *whTestEnv) registerWhUser(t *testing.T, email string) (*auth.User, string) {
	t.Helper()
	_, user, err := e.authSvc.Register("Acme", email, "secret123")
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	token, err := e.authSvc.GenerateToken(user)
	if err != nil {
		t.Fatalf("GenerateToken returned error: %v", err)
	}
	return user, token
}

func (e *whTestEnv) do(t *testing.T, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	e.mux.ServeHTTP(rr, req)
	return rr
}

// createWhViaAPI creates a webhook through POST /webhooks/create and returns
// the id + one-time secret.
func (e *whTestEnv) createWhViaAPI(t *testing.T, token, url string, events []string) (string, string) {
	t.Helper()
	payload := map[string]any{"url": url, "events": events}
	raw, _ := json.Marshal(payload)
	rr := e.do(t, http.MethodPost, "/webhooks/create", token, string(raw))
	if rr.Code != http.StatusCreated {
		t.Fatalf("create webhook: expected 201, got %d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Webhook struct {
			ID string `json:"id"`
		} `json:"webhook"`
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("create webhook response should be valid JSON: %v", err)
	}
	if resp.Webhook.ID == "" || resp.Secret == "" {
		t.Fatalf("create webhook response missing id/secret: %s", rr.Body.String())
	}
	return resp.Webhook.ID, resp.Secret
}

func TestWebhooksRequireAuth(t *testing.T) {
	env := newWebhooksTestEnv(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/webhooks"},
		{http.MethodPost, "/webhooks/create"},
		{http.MethodDelete, "/webhooks/wh-1"},
		{http.MethodGet, "/webhooks/wh-1/deliveries"},
	} {
		rr := env.do(t, tc.method, tc.path, "", "")
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s: expected 401, got %d body=%s", tc.method, tc.path, rr.Code, rr.Body.String())
		}
	}
}

func TestWebhookViewerReadOnly(t *testing.T) {
	env := newWebhooksTestEnv(t)
	_, ownerToken := env.registerWhUser(t, "owner@example.com")
	env.createWhViaAPI(t, ownerToken, "https://hooks.example.com/agent", []string{"run.failed"})

	// A viewer: registered user downgraded to VIEWER before token generation.
	_, viewer, err := env.authSvc.Register("Acme", "viewer@example.com", "secret123")
	if err != nil {
		t.Fatalf("Register viewer returned error: %v", err)
	}
	viewer.Role = "VIEWER"
	viewerToken, err := env.authSvc.GenerateToken(viewer)
	if err != nil {
		t.Fatalf("GenerateToken viewer returned error: %v", err)
	}

	rr := env.do(t, http.MethodGet, "/webhooks", viewerToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("viewer GET /webhooks: expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	rr = env.do(t, http.MethodPost, "/webhooks/create", viewerToken, `{"url":"https://x.example.com"}`)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("viewer POST /webhooks/create: expected 403, got %d body=%s", rr.Code, rr.Body.String())
	}
	rr = env.do(t, http.MethodDelete, "/webhooks/wh-1", viewerToken, "")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("viewer DELETE /webhooks/{id}: expected 403, got %d body=%s", rr.Code, rr.Body.String())
	}
	// deliveries are a read: seed one webhook into the viewer's org via the
	// service (viewers cannot create through the API) and expect 200.
	if _, _, err := env.whSvc.CreateWebhook(t.Context(), viewer.Organization, "https://viewer.example.com/hook", nil); err != nil {
		t.Fatalf("service-level webhook create returned error: %v", err)
	}
	rr = env.do(t, http.MethodGet, "/webhooks/"+viewerOrgWebhookID(t, env, viewer.Organization)+"/deliveries", viewerToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("viewer GET deliveries: expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// viewerOrgWebhookID looks up the first webhook id of an org via the service.
func viewerOrgWebhookID(t *testing.T, env *whTestEnv, orgID string) string {
	t.Helper()
	list, err := env.whSvc.ListWebhooks(t.Context(), orgID)
	if err != nil || len(list) == 0 {
		t.Fatalf("ListWebhooks returned %d webhooks (err=%v)", len(list), err)
	}
	return list[0].ID
}

func TestWebhookCreateReturnsSecretOnce(t *testing.T) {
	env := newWebhooksTestEnv(t)
	user, token := env.registerWhUser(t, "creator@example.com")

	whID, secret := env.createWhViaAPI(t, token, "https://hooks.example.com/agent", []string{"run.failed", "run.failed"})

	// The stored record only carries the SHA-256 hash of the secret.
	stored, err := env.whSvc.GetWebhook(t.Context(), user.Organization, whID)
	if err != nil {
		t.Fatalf("GetWebhook returned error: %v", err)
	}
	if stored.SecretHash != webhooks.HashSecret(secret) {
		t.Fatal("stored secret_hash should be the sha256 of the returned secret")
	}
	if stored.URL != "https://hooks.example.com/agent" {
		t.Fatalf("stored url mismatch: %q", stored.URL)
	}

	rr := env.do(t, http.MethodGet, "/webhooks", token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /webhooks: expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	var listResp struct {
		Webhooks []struct {
			ID        string   `json:"id"`
			URL       string   `json:"url"`
			Events    []string `json:"events"`
			Status    string   `json:"status"`
			SecretSet bool     `json:"secret_set"`
			Secret    string   `json:"secret"`
			CreatedAt string   `json:"created_at"`
		} `json:"webhooks"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("list response should be valid JSON: %v", err)
	}
	if len(listResp.Webhooks) != 1 {
		t.Fatalf("expected 1 webhook, got %d body=%s", len(listResp.Webhooks), rr.Body.String())
	}
	whView := listResp.Webhooks[0]
	if whView.ID != whID {
		t.Fatalf("expected webhook id %q, got %q", whID, whView.ID)
	}
	if whView.Status != "active" || !whView.SecretSet {
		t.Fatalf("expected active+secret_set, got status=%q secret_set=%v", whView.Status, whView.SecretSet)
	}
	if len(whView.Events) != 1 || whView.Events[0] != "run.failed" {
		t.Fatalf("duplicate event types should be deduped, got %v", whView.Events)
	}
	if whView.Secret != "" {
		t.Fatal("listing must never leak the secret")
	}
	if _, err := time.Parse(time.RFC3339, whView.CreatedAt); err != nil {
		t.Fatalf("created_at should be RFC3339, got %q", whView.CreatedAt)
	}
}

func TestWebhookCreateValidation(t *testing.T) {
	env := newWebhooksTestEnv(t)
	_, token := env.registerWhUser(t, "validator@example.com")

	cases := []struct {
		name   string
		body   string
		status int
		code   string
	}{
		{"relative url", `{"url":"/relative/path"}`, http.StatusUnprocessableEntity, "VALIDATION_ERROR"},
		{"ftp scheme", `{"url":"ftp://example.com/x"}`, http.StatusUnprocessableEntity, "VALIDATION_ERROR"},
		{"missing url", `{"events":["run.failed"]}`, http.StatusUnprocessableEntity, "VALIDATION_ERROR"},
		{"unknown event", `{"url":"https://ok.example.com","events":["bogus.event"]}`, http.StatusUnprocessableEntity, "VALIDATION_ERROR"},
		{"malformed json", `{"url":`, http.StatusBadRequest, "INVALID_REQUEST"},
	}
	for _, tc := range cases {
		rr := env.do(t, http.MethodPost, "/webhooks/create", token, tc.body)
		if rr.Code != tc.status {
			t.Fatalf("%s: expected %d, got %d body=%s", tc.name, tc.status, rr.Code, rr.Body.String())
		}
		var errResp struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(rr.Body.Bytes(), &errResp)
		if errResp.Error.Code != tc.code {
			t.Fatalf("%s: expected error code %q, got %q", tc.name, tc.code, errResp.Error.Code)
		}
	}
}

func TestWebhookDeleteScoped(t *testing.T) {
	env := newWebhooksTestEnv(t)
	_, tokenA := env.registerWhUser(t, "org-a@example.com")
	_, tokenB := env.registerWhUser(t, "org-b@example.com")
	whID, _ := env.createWhViaAPI(t, tokenA, "https://a.example.com/hook", nil)

	// Cross-tenant delete surfaces as 404 (never 403: existence is not leaked).
	rr := env.do(t, http.MethodDelete, "/webhooks/"+whID, tokenB, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant delete: expected 404, got %d body=%s", rr.Code, rr.Body.String())
	}
	var errResp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &errResp)
	if errResp.Error.Code != "WEBHOOK_NOT_FOUND" {
		t.Fatalf("expected WEBHOOK_NOT_FOUND, got %q", errResp.Error.Code)
	}

	rr = env.do(t, http.MethodDelete, "/webhooks/"+whID, tokenA, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("owner delete: expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"deleted":true`) {
		t.Fatalf("expected {\"deleted\":true}, got %s", rr.Body.String())
	}
	// Second delete is 404 (record gone).
	rr = env.do(t, http.MethodDelete, "/webhooks/"+whID, tokenA, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("repeat delete: expected 404, got %d", rr.Code)
	}
	// Unknown id is 404.
	rr = env.do(t, http.MethodDelete, "/webhooks/does-not-exist", tokenA, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown id delete: expected 404, got %d", rr.Code)
	}
}

func TestWebhookDeliveriesListing(t *testing.T) {
	env := newWebhooksTestEnv(t)
	_, tokenA := env.registerWhUser(t, "deliveries-a@example.com")
	_, tokenB := env.registerWhUser(t, "deliveries-b@example.com")
	whID, _ := env.createWhViaAPI(t, tokenA, "https://a.example.com/hook", []string{"run.failed"})

	now := time.Now().UTC()
	older := &webhooks.Delivery{
		ID: "d-1", WebhookID: whID,
		EventID: "evt-1", EventType: "run.failed",
		Status: webhooks.DeliveryDelivered, Attempts: 1,
		LastStatusCode: 200, LatencyMS: 42, CreatedAt: now.Add(-2 * time.Minute),
	}
	newer := &webhooks.Delivery{
		ID: "d-2", WebhookID: whID,
		EventID: "evt-2", EventType: "run.failed",
		Status: webhooks.DeliveryFailed, Attempts: 3,
		LastStatusCode: 500, LatencyMS: 900, Error: "upstream 500",
		CreatedAt: now.Add(-1 * time.Minute),
	}
	for _, d := range []*webhooks.Delivery{older, newer} {
		if err := env.whSvc.UpsertDelivery(t.Context(), d); err != nil {
			t.Fatalf("UpsertDelivery returned error: %v", err)
		}
	}

	rr := env.do(t, http.MethodGet, "/webhooks/"+whID+"/deliveries", tokenA, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET deliveries: expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	var listResp struct {
		Deliveries []struct {
			ID             string `json:"id"`
			EventType      string `json:"event_type"`
			Status         string `json:"status"`
			Attempts       int    `json:"attempts"`
			LastStatusCode int    `json:"last_status_code"`
			LatencyMS      int64  `json:"latency_ms"`
			Error          string `json:"error"`
			CreatedAt      string `json:"created_at"`
		} `json:"deliveries"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("deliveries response should be valid JSON: %v", err)
	}
	if len(listResp.Deliveries) != 2 {
		t.Fatalf("expected 2 deliveries, got %d body=%s", len(listResp.Deliveries), rr.Body.String())
	}
	if listResp.Deliveries[0].ID != "d-2" || listResp.Deliveries[1].ID != "d-1" {
		t.Fatalf("deliveries should be newest first, got %s then %s", listResp.Deliveries[0].ID, listResp.Deliveries[1].ID)
	}
	failed := listResp.Deliveries[0]
	if failed.Status != "failed" || failed.Attempts != 3 || failed.LastStatusCode != 500 || failed.Error != "upstream 500" {
		t.Fatalf("delivery fields mismatch: %+v", failed)
	}
	if _, err := time.Parse(time.RFC3339, failed.CreatedAt); err != nil {
		t.Fatalf("delivery created_at should be RFC3339, got %q", failed.CreatedAt)
	}

	// limit=1 truncates to the newest record.
	rr = env.do(t, http.MethodGet, "/webhooks/"+whID+"/deliveries?limit=1", tokenA, "")
	_ = json.Unmarshal(rr.Body.Bytes(), &listResp)
	if len(listResp.Deliveries) != 1 || listResp.Deliveries[0].ID != "d-2" {
		t.Fatalf("limit=1 should return the newest delivery only, got %+v", listResp.Deliveries)
	}
	// invalid limit -> 400
	rr = env.do(t, http.MethodGet, "/webhooks/"+whID+"/deliveries?limit=abc", tokenA, "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("limit=abc: expected 400, got %d", rr.Code)
	}
	rr = env.do(t, http.MethodGet, "/webhooks/"+whID+"/deliveries?limit=0", tokenA, "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("limit=0: expected 400, got %d", rr.Code)
	}

	// Cross-tenant access -> 404.
	rr = env.do(t, http.MethodGet, "/webhooks/"+whID+"/deliveries", tokenB, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant deliveries: expected 404, got %d body=%s", rr.Code, rr.Body.String())
	}
	// Unknown webhook -> 404.
	rr = env.do(t, http.MethodGet, "/webhooks/nope/deliveries", tokenA, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown webhook deliveries: expected 404, got %d", rr.Code)
	}
	// Malformed sub-path -> 404.
	rr = env.do(t, http.MethodGet, "/webhooks/"+whID+"/extra/deliveries", tokenA, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("malformed deliveries path: expected 404, got %d", rr.Code)
	}
}

func TestWebhooksMethodNotAllowed(t *testing.T) {
	env := newWebhooksTestEnv(t)
	_, token := env.registerWhUser(t, "methods@example.com")
	whID, _ := env.createWhViaAPI(t, token, "https://m.example.com/hook", nil)

	for _, tc := range []struct{ method, path string }{
		{http.MethodPut, "/webhooks"},
		{http.MethodDelete, "/webhooks"},
		{http.MethodGet, "/webhooks/create"},
		{http.MethodPut, "/webhooks/" + whID},
		{http.MethodPost, "/webhooks/" + whID + "/deliveries"},
	} {
		rr := env.do(t, tc.method, tc.path, token, "")
		if rr.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s: expected 405, got %d body=%s", tc.method, tc.path, rr.Code, rr.Body.String())
		}
	}
}

func TestWebhookAPIKeyAuthPath(t *testing.T) {
	env := newWebhooksTestEnv(t)
	user, _ := env.registerWhUser(t, "apikey@example.com")
	key, err := env.keysSvc.Create(user.Organization, user.ID, "test-key")
	if err != nil {
		t.Fatalf("apikey create returned error: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/webhooks", nil)
	req.Header.Set("X-API-Key", key.Value)
	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("X-API-Key GET /webhooks: expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
}
