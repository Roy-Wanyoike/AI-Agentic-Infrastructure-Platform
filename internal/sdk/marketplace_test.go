package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
)

// TestMarketplaceResource drives the marketplace surface against one fake
// API, asserting routes, query strings, request bodies and the wrapped
// response shapes of cmd/api/marketplace.go.
func TestMarketplaceResource(t *testing.T) {
	var reqBody map[string]any
	var gotQuery string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/marketplace/listings":
			_, _ = w.Write([]byte(`{"listings":[{"id":"l-1","publisher_org_id":"org-1","publisher_user_id":"u-1",` +
				`"source_agent_id":"a-1","name":"Support","slug":"support","description":"helpdesk",` +
				`"tags":["support"],"status":"published","download_count":3,` +
				`"version_snapshot":{"name":"Support","description":"helpdesk","instructions":"be kind",` +
				`"model":"gpt-4o-mini","status":"DRAFT"},` +
				`"created_at":"2025-07-01T12:00:00Z","updated_at":"2025-07-01T12:00:00Z"}],` +
				`"next_cursor":"c-2"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/marketplace/listings/support":
			_, _ = w.Write([]byte(`{"listing":{"id":"l-1","slug":"support","name":"Support","status":"published",` +
				`"tags":[],"download_count":3,"version_snapshot":null,` +
				`"created_at":"2025-07-01T12:00:00Z","updated_at":"2025-07-01T12:00:00Z"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/marketplace/listings/support/install":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"listing":{"id":"l-1","slug":"support","name":"Support","status":"published",` +
				`"download_count":4,"version_snapshot":null,` +
				`"created_at":"2025-07-01T12:00:00Z","updated_at":"2025-07-02T12:00:00Z"},` +
				`"agent":{"id":"a-9","organization_id":"org-2","name":"Support","description":"helpdesk",` +
				`"instructions":"be kind","model":"gpt-4o-mini","status":"DRAFT",` +
				`"created_at":"2025-07-02T12:00:00Z"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/marketplace/listings":
			_ = json.NewDecoder(r.Body).Decode(&reqBody)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"listing":{"id":"l-2","slug":"researcher","name":"Researcher","status":"draft",` +
				`"tags":["research"],"download_count":0,"version_snapshot":null,` +
				`"created_at":"2025-07-03T12:00:00Z","updated_at":"2025-07-03T12:00:00Z"}}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/marketplace/listings/researcher":
			_, _ = w.Write([]byte(`{"deleted":true}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ctx := context.Background()

	page, err := c.BrowseListings(ctx, BrowseOptions{Query: "sup", Tags: []string{"support"}, Limit: 5, Cursor: "c-1"})
	if err != nil {
		t.Fatalf("BrowseListings: %v", err)
	}
	if len(page.Listings) != 1 || page.NextCursor != "c-2" {
		t.Errorf("page = %+v", page)
	}
	l := page.Listings[0]
	if l.Slug != "support" || l.DownloadCount != 3 || len(l.Tags) != 1 {
		t.Errorf("listing = %+v", l)
	}
	if l.VersionSnapshot == nil || l.VersionSnapshot.Model != "gpt-4o-mini" || l.VersionSnapshot.Instructions != "be kind" {
		t.Errorf("snapshot = %+v", l.VersionSnapshot)
	}
	if gotQuery != "cursor=c-1&limit=5&q=sup&tags=support" {
		t.Errorf("query = %q", gotQuery)
	}

	one, err := c.GetListing(ctx, "support")
	if err != nil {
		t.Fatalf("GetListing: %v", err)
	}
	if one.Name != "Support" || one.VersionSnapshot != nil {
		t.Errorf("get = %+v", one)
	}

	installed, err := c.InstallListing(ctx, "support")
	if err != nil {
		t.Fatalf("InstallListing: %v", err)
	}
	if installed.Agent.ID != "a-9" || installed.Agent.OrganizationID != "org-2" || installed.Listing.DownloadCount != 4 {
		t.Errorf("install = %+v", installed)
	}

	published, err := c.PublishListing(ctx, PublishListingRequest{
		AgentID: "a-1", Name: "Researcher", Tags: []string{"research"}, Status: "draft",
	})
	if err != nil {
		t.Fatalf("PublishListing: %v", err)
	}
	if published.Slug != "researcher" || published.Status != "draft" {
		t.Errorf("publish = %+v", published)
	}
	if reqBody["agent_id"] != "a-1" || reqBody["status"] != "draft" {
		t.Errorf("publish body = %v", reqBody)
	}

	if err := c.UnlistListing(ctx, "researcher"); err != nil {
		t.Fatalf("UnlistListing: %v", err)
	}
}

func TestBrowseListingsEmptyAndMinimalQuery(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			t.Errorf("empty options should send no query, got %q", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"listings":[],"next_cursor":""}`))
	})
	page, err := c.BrowseListings(context.Background(), BrowseOptions{})
	if err != nil {
		t.Fatalf("BrowseListings: %v", err)
	}
	if page.Listings == nil || len(page.Listings) != 0 {
		t.Errorf("want non-nil empty listings, got %#v", page.Listings)
	}
}

func TestGetListingNotFound(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"LISTING_NOT_FOUND","message":"marketplace listing not found"}}`))
	})
	_, err := c.GetListing(context.Background(), "missing")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 404 || apiErr.Code != "LISTING_NOT_FOUND" {
		t.Fatalf("want 404 LISTING_NOT_FOUND, got %v", err)
	}
	if want := fmt.Sprintf("%s", "marketplace listing not found"); apiErr.Message != want {
		t.Errorf("message = %q", apiErr.Message)
	}
}
