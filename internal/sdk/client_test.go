package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// ---------- Client construction / endpoint building ----------

func TestEndpointBuildsVersionedPaths(t *testing.T) {
	tests := []struct {
		base, path, want string
	}{
		{"http://localhost:8080", "/agents", "http://localhost:8080/v1/agents"},
		{"http://localhost:8080/", "/agents", "http://localhost:8080/v1/agents"},   // trailing slash tolerated
		{"http://localhost:8080/v1", "/agents", "http://localhost:8080/v1/agents"}, // /v1 suffix never doubled
		{"http://localhost:8080/v1/", "/runs/run-1/steps", "http://localhost:8080/v1/runs/run-1/steps"},
		{" http://host:9000 ", "/agents", "http://host:9000/v1/agents"}, // whitespace trimmed
	}
	for _, tc := range tests {
		c := New(WithBaseURL(tc.base))
		if got := c.endpoint(tc.path, nil); got != tc.want {
			t.Errorf("endpoint(base=%q, %q) = %q, want %q", tc.base, tc.path, got, tc.want)
		}
	}
	c := New() // default
	if c.BaseURL() != DefaultBaseURL {
		t.Errorf("default base = %q, want %q", c.BaseURL(), DefaultBaseURL)
	}
}

func TestEndpointEncodesQuery(t *testing.T) {
	c := New()
	q := url.Values{}
	q.Set("organization_id", "org 1")
	got := c.endpoint("/agents", q)
	want := "http://localhost:8080/v1/agents?organization_id=org+1"
	if got != want {
		t.Errorf("endpoint with query = %q, want %q", got, want)
	}
}

func TestAuthMode(t *testing.T) {
	if got := New().AuthMode(); got != "none" {
		t.Errorf("AuthMode() = %q, want none", got)
	}
	if got := New(WithToken("t")).AuthMode(); got != "bearer" {
		t.Errorf("AuthMode(token) = %q, want bearer", got)
	}
	if got := New(WithAPIKey("k")).AuthMode(); got != "api-key" {
		t.Errorf("AuthMode(key) = %q, want api-key", got)
	}
}

// ---------- Auth header handling (httptest, no real network) ----------

func TestRequestHeadersBearerToken(t *testing.T) {
	var gotAuth, gotAccept, gotMethod, gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]Agent{})
	}))
	defer srv.Close()

	c := New(WithBaseURL(srv.URL), WithToken("tok-123"))
	if _, err := c.ListAgents(context.Background(), "org-9"); err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if gotAuth != "Bearer tok-123" {
		t.Errorf("Authorization = %q, want Bearer tok-123", gotAuth)
	}
	if gotAccept != "application/json" {
		t.Errorf("Accept = %q", gotAccept)
	}
	if gotMethod != http.MethodGet || gotPath != "/v1/agents" || gotQuery != "organization_id=org-9" {
		t.Errorf("request line = %s %s?%s", gotMethod, gotPath, gotQuery)
	}
}

func TestRequestHeadersAPIKey(t *testing.T) {
	var gotKey, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-API-Key")
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(WithBaseURL(srv.URL), WithAPIKey("ak-1"))
	if _, err := c.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if gotKey != "ak-1" {
		t.Errorf("X-API-Key = %q, want ak-1", gotKey)
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q, want empty (api-key mode)", gotAuth)
	}
}

func TestRequestHeadersBothCredentials(t *testing.T) {
	var gotKey, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-API-Key")
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(WithBaseURL(srv.URL), WithToken("t"), WithAPIKey("k"))
	if _, err := c.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if gotAuth != "Bearer t" || gotKey != "k" {
		t.Errorf("headers = %q / %q, want Bearer t / k", gotAuth, gotKey)
	}
}

// ---------- Error mapping ----------

func TestAPIErrorEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"WORKFLOW_NOT_FOUND","message":"workflow not found"}}`))
	}))
	defer srv.Close()

	_, err := New(WithBaseURL(srv.URL)).GetWorkflow(context.Background(), "wf-x")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %v (%T)", err, err)
	}
	if apiErr.StatusCode != 404 || apiErr.Code != "WORKFLOW_NOT_FOUND" || apiErr.Message != "workflow not found" {
		t.Errorf("APIError = %+v", apiErr)
	}
	if !apiErr.IsStatus(http.StatusNotFound) {
		t.Error("IsStatus(404) = false")
	}
	if !strings.Contains(apiErr.Error(), "404 Not Found (WORKFLOW_NOT_FOUND): workflow not found") {
		t.Errorf("Error() = %q", apiErr.Error())
	}
}

func TestAPIErrorPlainTextBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "agent not found", http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := New(WithBaseURL(srv.URL)).GetAgent(context.Background(), "missing")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %v", err)
	}
	if apiErr.StatusCode != 404 || apiErr.Code != "" || apiErr.Message != "agent not found" {
		t.Errorf("APIError = %+v", apiErr)
	}
	if !strings.Contains(apiErr.Error(), "agent not found") {
		t.Errorf("Error() = %q", apiErr.Error())
	}
	if apiErr.RawBody == "" {
		t.Error("RawBody should keep the raw payload")
	}
}

func TestAPIErrorValidationArray(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"errors":[{"code":"missing_agent_id","message":"node a requires config.agent_id","node_id":"a"},{"code":"empty_graph","message":"workflow requires at least one node"}]}`))
	}))
	defer srv.Close()

	_, err := New(WithBaseURL(srv.URL)).ExecuteWorkflow(context.Background(), "wf", "in")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %v", err)
	}
	if apiErr.StatusCode != 422 || len(apiErr.ValidationErrors) != 2 {
		t.Fatalf("APIError = %+v", apiErr)
	}
	// The human rendering lists every item on its own line.
	lines := strings.Split(apiErr.Error(), "\n")
	if len(lines) != 3 {
		t.Fatalf("Error() should render one line per item, got %q", apiErr.Error())
	}
	if !strings.Contains(lines[1], "[missing_agent_id] node a requires config.agent_id (node: a)") {
		t.Errorf("line 1 = %q", lines[1])
	}
	if !strings.Contains(lines[2], "workflow requires at least one node") {
		t.Errorf("line 2 = %q", lines[2])
	}
}

func TestAPIErrorUnauthorizedShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "missing authorization header", http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := New(WithBaseURL(srv.URL)).ListRuns(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 401 {
		t.Fatalf("want 401 *APIError, got %v", err)
	}
}

func TestContextCancellationAbortsRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New(WithBaseURL(srv.URL)).ListRuns(ctx); err == nil {
		t.Fatal("expected context error for cancelled request")
	}
}

func TestEmptyResponseOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	if _, err := New(WithBaseURL(srv.URL)).GetWorkflowRun(context.Background(), "wr-1"); err != nil {
		t.Fatalf("204 with no body should not error: %v", err)
	}
}

// ---------- Login / Register envelopes ----------

func TestLogin(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/auth/login" {
			t.Errorf("request line = %s %s", r.Method, r.URL.Path)
		}
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "jwt-abc"})
	}))
	defer srv.Close()

	token, err := New(WithBaseURL(srv.URL)).Login(context.Background(), "a@b.c", "pw")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if token != "jwt-abc" {
		t.Errorf("token = %q", token)
	}
	var req map[string]string
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("request body: %v", err)
	}
	if req["email"] != "a@b.c" || req["password"] != "pw" {
		t.Errorf("request body = %v", req)
	}
}

func TestLoginBadCredentialsMaps401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := New(WithBaseURL(srv.URL)).Login(context.Background(), "a@b.c", "wrong")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 401 {
		t.Fatalf("want 401, got %v", err)
	}
}

func TestRegister(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The handler builds the response from map literals: nested PascalCase keys.
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"organization":{"ID":"org-1","Name":"Acme"},"user":{"ID":"u-1","Organization":"org-1","Email":"a@b.c","Role":"OWNER"}}`))
	}))
	defer srv.Close()

	res, err := New(WithBaseURL(srv.URL)).Register(context.Background(), "Acme", "a@b.c", "pw")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if res.Organization.ID != "org-1" || res.User.Role != "OWNER" || res.User.Email != "a@b.c" {
		t.Errorf("RegisterResult = %+v", res)
	}
}
