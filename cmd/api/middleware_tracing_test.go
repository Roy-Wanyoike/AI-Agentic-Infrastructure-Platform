package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"agentos/internal/auth"
	"agentos/internal/observability"
)

// TestTracingMiddlewareDisabledIsIdentity pins the default-off contract at the
// API layer: with AGENTOS_TRACING_ENABLED unset the glue returns the very next
// handler (comparable identity) and responses are byte-identical.
func TestTracingMiddlewareDisabledIsIdentity(t *testing.T) {
	t.Setenv("AGENTOS_TRACING_ENABLED", "garbage-value") // ParseBool garbage=off contract: stays off

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"service":"agentos"}`))
	})

	got := tracingMiddleware(handler)
	if _, ok := got.(http.HandlerFunc); !ok {
		t.Fatalf("disabled tracingMiddleware must return the next handler unchanged, got %T", got)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/agents", nil)
	recDirect, recWrapped := httptest.NewRecorder(), httptest.NewRecorder()
	handler.ServeHTTP(recDirect, req)
	tracingMiddleware(handler).ServeHTTP(recWrapped, httptest.NewRequest(http.MethodGet, "/v1/agents", nil))
	if recDirect.Code != recWrapped.Code || recDirect.Body.String() != recWrapped.Body.String() {
		t.Fatalf("disabled middleware changed behavior: direct=%d/%q wrapped=%d/%q",
			recDirect.Code, recDirect.Body.String(), recWrapped.Code, recWrapped.Body.String())
	}
}

// TestTracingMiddlewareDisabledWithEnvButNoSetup covers the flip-side safety
// net: the env flag alone (without a successful SetupTracing call) must still
// yield a pure pass-through — no provider, no spans, no behavior change.
func TestTracingMiddlewareDisabledWithEnvButNoSetup(t *testing.T) {
	if observability.TracingEnabled() {
		t.Fatal("precondition: no provider installed in the test binary")
	}
	t.Setenv("AGENTOS_TRACING_ENABLED", "true")

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	wrapped := tracingMiddleware(handler)
	// observability.TracingMiddleware returns next when no provider is
	// installed, so the chain stays a pass-through either way.
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/runs", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
}

// TestTracingMiddlewareEnabledEmitsServerSpanAndOrgClaim proves the wired
// chain, when enabled, produces a server span carrying route/method/status and
// the org claim stamped by the auth seam (the exact one-line instrumentation
// proposed for internal/auth/middleware.go), and that the auth claims context
// and the tracing context coexist through the chain.
func TestTracingMiddlewareEnabledEmitsServerSpanAndOrgClaim(t *testing.T) {
	t.Setenv("AGENTOS_TRACING_ENABLED", "true")
	spans, restore := observability.InstallTestTracing()
	defer restore()

	authSvc := auth.NewService("test-secret")
	_, user, err := authSvc.RegisterCtx(t.Context(), "org-trace", "tracer@example.com", "password123")
	if err != nil {
		t.Fatalf("register failed: %v", err)
	}
	token, err := authSvc.LoginCtx(t.Context(), "tracer@example.com", "password123")
	if err != nil {
		t.Fatalf("login failed: %v", err)
	}

	// The inner handler emulates the documented auth seam: claims already sit
	// in the context (RequireAuth validated the token); the one-line call
	// stamps the tenant onto the in-flight server span.
	inner := auth.RequireAuth(authSvc)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, cerr := auth.ExtractClaims(r.Context())
		if cerr != nil {
			t.Errorf("claims must survive the tracing context wrap: %v", cerr)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if claims.OrganizationID != user.Organization {
			t.Errorf("org claim = %q, want %q", claims.OrganizationID, user.Organization)
		}
		observability.SetSpanOrgID(r.Context(), claims.OrganizationID) // <-- auth seam
		w.WriteHeader(http.StatusOK)
	}))

	srv := httptest.NewServer(tracingMiddleware(inner))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/agents", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}

	recorded := spans()
	if len(recorded) != 1 {
		t.Fatalf("expected exactly 1 server span, got %d: %+v", len(recorded), recorded)
	}
	span := recorded[0]
	if span.Name != "GET unmatched" {
		// Outermost middleware never sees a ServeMux pattern (none here);
		// the span name falls back to method + unmatched, matching metrics.
		t.Fatalf("span name = %q, want GET unmatched", span.Name)
	}
	if span.SpanKind.String() != "server" {
		t.Fatalf("span kind = %v, want server", span.SpanKind)
	}
	if got := span.Attr(observability.AttrHTTPMethod); got != http.MethodGet {
		t.Errorf("%s = %q, want GET", observability.AttrHTTPMethod, got)
	}
	if got := span.Attr(observability.AttrHTTPStatus); got != "200" {
		t.Errorf("%s = %q, want 200", observability.AttrHTTPStatus, got)
	}
	if got := span.Attr(observability.AttrOrgID); got != user.Organization {
		t.Errorf("org claim missing from server span: got %q, want %q", got, user.Organization)
	}
}
