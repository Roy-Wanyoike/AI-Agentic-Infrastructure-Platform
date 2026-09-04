package sdk

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestBeginSSOLogin returns the 302 Location instead of following it.
func TestBeginSSOLogin(t *testing.T) {
	followed := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/auth/sso/acme-corp/login" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("followed") == "1" {
			followed = true // a redirect hop would land here with the marker set
		}
		http.Redirect(w, r, "/v1/auth/sso/callback?followed=1", http.StatusFound)
	}))
	defer srv.Close()

	url, err := New(WithBaseURL(srv.URL)).BeginSSOLogin(context.Background(), "acme-corp")
	if err != nil {
		t.Fatalf("BeginSSOLogin: %v", err)
	}
	if !strings.Contains(url, "/v1/auth/sso/callback?followed=1") {
		t.Errorf("authorize url = %q", url)
	}
	if followed {
		t.Error("the redirect must NOT be followed")
	}
}

func TestBeginSSOLoginErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"SSO_NOT_CONFIGURED","message":"sso is not configured for this organization"}}`))
	}))
	defer srv.Close()

	_, err := New(WithBaseURL(srv.URL)).BeginSSOLogin(context.Background(), "acme-corp")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 404 || apiErr.Code != "SSO_NOT_CONFIGURED" {
		t.Fatalf("want 404 SSO_NOT_CONFIGURED, got %v", err)
	}
}

func TestBeginSSOLoginRedirectWithoutLocation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	if _, err := New(WithBaseURL(srv.URL)).BeginSSOLogin(context.Background(), "acme"); err == nil {
		t.Fatal("expected an error for a 302 without a Location header")
	}
}

// TestMintSCIMToken checks the route, method and the one-time secret shape.
func TestMintSCIMToken(t *testing.T) {
	var gotAuth string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/scim/tokens" {
			t.Errorf("request line = %s %s", r.Method, r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":{"id":"st-1","organization_id":"org-1","created_by":"u-1",` +
			`"created_at":"2025-07-01T12:00:00Z"},"secret":"scim_aabbccdd00112233aabbccdd00112233aabbccdd00112233aabbccdd00112233"}`))
	}, WithToken("platform-session-token"))

	minted, err := c.MintSCIMToken(context.Background())
	if err != nil {
		t.Fatalf("MintSCIMToken: %v", err)
	}
	if minted.Token.ID != "st-1" || minted.Token.OrganizationID != "org-1" {
		t.Errorf("token = %+v", minted.Token)
	}
	if !strings.HasPrefix(minted.Secret, "scim_") {
		t.Errorf("secret = %q", minted.Secret)
	}
	if gotAuth != "Bearer platform-session-token" {
		t.Errorf("mint must use the platform credential, got %q", gotAuth)
	}
}

// TestListSCIMUsers proves the scim_ credential is the bearer AND that the
// SCIM 2.0 error document maps onto *APIError with the RFC 7644 detail.
func TestListSCIMUsers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/scim/v2/Users" &&
			r.Header.Get("Authorization") == "Bearer scim_secret":
			if got := r.URL.Query().Get("filter"); got != `userName eq "dev@acme.test"` {
				t.Errorf("filter = %q", got)
			}
			w.Header().Set("Content-Type", "application/scim+json")
			_, _ = w.Write([]byte(`{"schemas":["urn:ietf:params:scim:api:messages:2.0:ListResponse"],` +
				`"totalResults":2,"startIndex":1,"itemsPerPage":2,` +
				`"Resources":[{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"id":"u-1",` +
				`"userName":"dev@acme.test","active":true,` +
				`"emails":[{"value":"dev@acme.test","primary":true}],` +
				`"meta":{"resourceType":"User","created":"2025-07-01T12:00:00.000Z",` +
				`"lastModified":"2025-07-01T12:00:00.000Z","location":"/scim/v2/Users/u-1"}},` +
				`{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"id":"u-2",` +
				`"userName":"ops@acme.test","active":false}]}`))
		default:
			w.Header().Set("Content-Type", "application/scim+json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"schemas":["urn:ietf:params:scim:api:messages:2.0:Error"],` +
				`"status":"401","detail":"invalid or revoked scim token"}`))
		}
	}))
	defer srv.Close()

	scimClient := New(WithBaseURL(srv.URL), WithToken("scim_secret"))
	list, err := scimClient.ListSCIMUsers(context.Background(), `userName eq "dev@acme.test"`)
	if err != nil {
		t.Fatalf("ListSCIMUsers: %v", err)
	}
	if list.TotalResults != 2 || len(list.Resources) != 2 {
		t.Fatalf("list = %+v", list)
	}
	first := list.Resources[0]
	if first.UserName != "dev@acme.test" || !first.Active || len(first.Emails) != 1 {
		t.Errorf("first = %+v", first)
	}
	if first.Meta.ResourceType != "User" || first.Meta.Location != "/scim/v2/Users/u-1" {
		t.Errorf("meta = %+v", first.Meta)
	}
	if list.Resources[1].Emails != nil {
		t.Errorf("sparse user should omit emails: %+v", list.Resources[1])
	}

	// Session/API-key credentials are rejected by the SCIM guard; the SDK
	// must surface the RFC 7644 detail, not the raw JSON document.
	_, err = New(WithBaseURL(srv.URL), WithToken("session-token")).ListSCIMUsers(context.Background(), "")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 401 {
		t.Fatalf("want 401 *APIError, got %v", err)
	}
	if apiErr.Message != "invalid or revoked scim token" {
		t.Errorf("SCIM error detail not mapped: %+v", apiErr)
	}
}

func TestListSCIMUsersEmptyResourcesStaysSlice(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			t.Errorf("empty filter should send no query, got %q", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"schemas":["urn:ietf:params:scim:api:messages:2.0:ListResponse"],` +
			`"totalResults":0,"startIndex":1,"itemsPerPage":0,"Resources":[]}`))
	}, WithToken("scim_secret"))
	list, err := c.ListSCIMUsers(context.Background(), "")
	if err != nil {
		t.Fatalf("ListSCIMUsers: %v", err)
	}
	if list.Resources == nil || len(list.Resources) != 0 {
		t.Errorf("want non-nil empty Resources, got %#v", list.Resources)
	}
}
