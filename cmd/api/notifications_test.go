package main

// Handler tests for the notification subscription endpoints (issue #83).
// All tests run without infrastructure: in-memory auth/apikeys/webhooks and
// the in-memory notifications service (delivery seam nil — CRUD does not
// deliver).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentos/internal/apikeys"
	"agentos/internal/auth"
	"agentos/internal/notifications"
	"agentos/internal/webhooks"
)

type nsubTestEnv struct {
	mux     *http.ServeMux
	authSvc *auth.Service
	keysSvc *apikeys.Service
	hooks   *webhooks.Service
	svc     *notifications.Service
}

func newNotificationsTestEnv(t *testing.T) *nsubTestEnv {
	t.Helper()
	env := &nsubTestEnv{
		mux:     http.NewServeMux(),
		authSvc: auth.NewService("test-jwt-secret"),
		keysSvc: apikeys.NewService(),
		hooks:   webhooks.NewService(),
	}
	env.svc = notifications.NewService(nil, env.hooks, nil)
	registerNotificationsRoutes(env.mux, env.svc, env.authSvc, env.keysSvc)
	return env
}

// registerNsubUser creates an org user (owner by default) and returns the user
// + bearer token.
func (e *nsubTestEnv) registerNsubUser(t *testing.T, email, role string) (*auth.User, string) {
	t.Helper()
	_, user, err := e.authSvc.Register("Acme", email, "secret123")
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	if role != "" {
		user.Role = role
	}
	token, err := e.authSvc.GenerateToken(user)
	if err != nil {
		t.Fatalf("GenerateToken returned error: %v", err)
	}
	return user, token
}

func (e *nsubTestEnv) do(t *testing.T, method, path, token, body string) *httptest.ResponseRecorder {
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

// createNsubHook creates a webhook through the service and returns its id.
func (e *nsubTestEnv) createNsubHook(t *testing.T, orgID string) string {
	t.Helper()
	wh, _, err := e.hooks.CreateWebhook(t.Context(), orgID, "https://hooks.example.com/alerts", nil)
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}
	return wh.ID
}

func TestNotificationSubscriptionsRequireAuth(t *testing.T) {
	env := newNotificationsTestEnv(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/notifications/subscriptions"},
		{http.MethodPost, "/notifications/subscriptions"},
		{http.MethodDelete, "/notifications/subscriptions/nsub-1"},
	} {
		rr := env.do(t, tc.method, tc.path, "", "")
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s: expected 401, got %d body=%s", tc.method, tc.path, rr.Code, rr.Body.String())
		}
	}
}

func TestNotificationSubscriptionsViewerReadOnly(t *testing.T) {
	env := newNotificationsTestEnv(t)
	owner, ownerToken := env.registerNsubUser(t, "owner@example.com", "")
	whID := env.createNsubHook(t, owner.Organization)

	// Owner seeds one subscription for the shared org.
	payload := `{"webhook_id":"` + whID + `","event_types":["run.failed"]}`
	rr := env.do(t, http.MethodPost, "/notifications/subscriptions", ownerToken, payload)
	if rr.Code != http.StatusCreated {
		t.Fatalf("owner POST: expected 201, got %d body=%s", rr.Code, rr.Body.String())
	}

	// A viewer can read but never write.
	_, viewerToken := env.registerNsubUser(t, "viewer@example.com", "VIEWER")
	rr = env.do(t, http.MethodGet, "/notifications/subscriptions", viewerToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("viewer GET: expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	rr = env.do(t, http.MethodPost, "/notifications/subscriptions", viewerToken, payload)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("viewer POST: expected 403, got %d body=%s", rr.Code, rr.Body.String())
	}
	rr = env.do(t, http.MethodDelete, "/notifications/subscriptions/nsub-1", viewerToken, "")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("viewer DELETE: expected 403, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestNotificationSubscriptionLifecycleOverHTTP(t *testing.T) {
	env := newNotificationsTestEnv(t)
	user, token := env.registerNsubUser(t, "owner@example.com", "")
	whID := env.createNsubHook(t, user.Organization)

	// Create.
	payload := `{"webhook_id":"` + whID + `","event_types":["run.failed","approval.requested"]}`
	rr := env.do(t, http.MethodPost, "/notifications/subscriptions", token, payload)
	if rr.Code != http.StatusCreated {
		t.Fatalf("POST: expected 201, got %d body=%s", rr.Code, rr.Body.String())
	}
	var created struct {
		Subscription struct {
			ID         string   `json:"id"`
			WebhookID  string   `json:"webhook_id"`
			EventTypes []string `json:"event_types"`
			CreatedAt  string   `json:"created_at"`
		} `json:"subscription"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("create response not JSON: %v", err)
	}
	subID := created.Subscription.ID
	if subID == "" || created.Subscription.WebhookID != whID {
		t.Fatalf("create response missing identity: %s", rr.Body.String())
	}
	if len(created.Subscription.EventTypes) != 2 || created.Subscription.CreatedAt == "" {
		t.Fatalf("create response wrong shape: %s", rr.Body.String())
	}

	// List contains it.
	rr = env.do(t, http.MethodGet, "/notifications/subscriptions", token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET: expected 200, got %d", rr.Code)
	}
	var listed struct {
		Subscriptions []map[string]any `json:"subscriptions"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("list response not JSON: %v", err)
	}
	if len(listed.Subscriptions) != 1 || listed.Subscriptions[0]["id"] != subID {
		t.Fatalf("list should contain the subscription: %s", rr.Body.String())
	}

	// Delete.
	rr = env.do(t, http.MethodDelete, "/notifications/subscriptions/"+subID, token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("DELETE: expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"deleted":true`) {
		t.Fatalf("delete response wrong: %s", rr.Body.String())
	}

	// Gone: list empty, delete again 404.
	rr = env.do(t, http.MethodGet, "/notifications/subscriptions", token, "")
	if rr.Code != http.StatusOK || strings.Contains(rr.Body.String(), subID) {
		t.Fatalf("subscription should be gone: %d %s", rr.Code, rr.Body.String())
	}
	rr = env.do(t, http.MethodDelete, "/notifications/subscriptions/"+subID, token, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("re-delete: expected 404, got %d", rr.Code)
	}
}

func TestCreateNotificationSubscriptionValidationOverHTTP(t *testing.T) {
	env := newNotificationsTestEnv(t)
	user, token := env.registerNsubUser(t, "owner@example.com", "")
	whID := env.createNsubHook(t, user.Organization)

	t.Run("unknown event type returns 400 listing valid types", func(t *testing.T) {
		rr := env.do(t, http.MethodPost, "/notifications/subscriptions", token,
			`{"webhook_id":"`+whID+`","event_types":["canary.rolledback"]}`)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d body=%s", rr.Code, rr.Body.String())
		}
		for _, valid := range []string{"run.failed", "approval.requested", "deployment.failed"} {
			if !strings.Contains(rr.Body.String(), valid) {
				t.Fatalf("error message should list %q: %s", valid, rr.Body.String())
			}
		}
	})

	t.Run("empty event types returns 400", func(t *testing.T) {
		rr := env.do(t, http.MethodPost, "/notifications/subscriptions", token,
			`{"webhook_id":"`+whID+`","event_types":[]}`)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d body=%s", rr.Code, rr.Body.String())
		}
	})

	t.Run("unknown webhook returns 404", func(t *testing.T) {
		rr := env.do(t, http.MethodPost, "/notifications/subscriptions", token,
			`{"webhook_id":"wh-does-not-exist","event_types":["run.failed"]}`)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d body=%s", rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "WEBHOOK_NOT_FOUND") {
			t.Fatalf("expected WEBHOOK_NOT_FOUND code: %s", rr.Body.String())
		}
	})

	t.Run("missing webhook_id returns 400", func(t *testing.T) {
		rr := env.do(t, http.MethodPost, "/notifications/subscriptions", token,
			`{"event_types":["run.failed"]}`)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d body=%s", rr.Code, rr.Body.String())
		}
	})

	t.Run("malformed JSON returns 400", func(t *testing.T) {
		rr := env.do(t, http.MethodPost, "/notifications/subscriptions", token, `{not json`)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d body=%s", rr.Code, rr.Body.String())
		}
	})
}

func TestNotificationSubscriptionsCrossTenantIsolationOverHTTP(t *testing.T) {
	env := newNotificationsTestEnv(t)
	ownerA, tokenA := env.registerNsubUser(t, "a@example.com", "")
	whA := env.createNsubHook(t, ownerA.Organization)

	// Org B: second org (Register always creates a fresh org per unique name? —
	// use another user in a different org by registering with a distinct org name).
	_, userB, err := env.authSvc.Register("Beta", "b@example.com", "secret123")
	if err != nil {
		t.Fatalf("Register B: %v", err)
	}
	tokenB, err := env.authSvc.GenerateToken(userB)
	if err != nil {
		t.Fatalf("GenerateToken B: %v", err)
	}

	payload := `{"webhook_id":"` + whA + `","event_types":["run.failed"]}`
	rr := env.do(t, http.MethodPost, "/notifications/subscriptions", tokenA, payload)
	if rr.Code != http.StatusCreated {
		t.Fatalf("org A POST: expected 201, got %d body=%s", rr.Code, rr.Body.String())
	}
	var created struct {
		Subscription struct {
			ID string `json:"id"`
		} `json:"subscription"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("create response not JSON: %v", err)
	}

	// Org B cannot see or delete org A's subscription.
	rr = env.do(t, http.MethodGet, "/notifications/subscriptions", tokenB, "")
	if rr.Code != http.StatusOK || strings.Contains(rr.Body.String(), created.Subscription.ID) {
		t.Fatalf("org B must not see org A subscriptions: %d %s", rr.Code, rr.Body.String())
	}
	rr = env.do(t, http.MethodDelete, "/notifications/subscriptions/"+created.Subscription.ID, tokenB, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("org B delete of org A subscription: expected 404, got %d", rr.Code)
	}

	// Org B cannot bind org A's webhook (FK-ish guard over HTTP).
	payloadB := `{"webhook_id":"` + whA + `","event_types":["run.failed"]}`
	rr = env.do(t, http.MethodPost, "/notifications/subscriptions", tokenB, payloadB)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("org B binding org A webhook: expected 404, got %d body=%s", rr.Code, rr.Body.String())
	}
}
