package sdk

// Marketplace resource (issue #54 CLI+SDK parity): the typed mirror of the
// agent-marketplace HTTP surface in cmd/api/marketplace.go.
//
// Endpoints (all under /v1/marketplace):
//
//	POST   /marketplace/listings            -> PublishListing  (OWNER/ADMIN)
//	GET    /marketplace/listings            -> BrowseListings  (any role; GLOBAL catalog)
//	GET    /marketplace/listings/{slug}     -> GetListing      (any role)
//	POST   /marketplace/listings/{slug}/install -> InstallListing (OWNER/ADMIN)
//	DELETE /marketplace/listings/{slug}     -> UnlistListing   (publisher org)
//
// Reads are global (published rows form the catalog for every org); writes
// are org-scoped from the credentials. Version snapshots contain config
// only — never request-supplied JSON — mirrored by ListingSnapshot.

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ListingSnapshot is the immutable agent-config document embedded in a
// listing (name/description/instructions/model/status — config only).
type ListingSnapshot struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	Instructions string `json:"instructions"`
	Model        string `json:"model"`
	Status       string `json:"status"`
}

// Listing is one marketplace catalog entry.
type Listing struct {
	ID              string           `json:"id"`
	PublisherOrgID  string           `json:"publisher_org_id"`
	PublisherUserID string           `json:"publisher_user_id"`
	SourceAgentID   string           `json:"source_agent_id"`
	Name            string           `json:"name"`
	Slug            string           `json:"slug"`
	Description     string           `json:"description"`
	Tags            []string         `json:"tags"`
	Status          string           `json:"status"` // draft|published|unlisted
	DownloadCount   int              `json:"download_count"`
	VersionSnapshot *ListingSnapshot `json:"version_snapshot"`
	CreatedAt       time.Time        `json:"created_at"`
	UpdatedAt       time.Time        `json:"updated_at"`
}

// ListingPage is the wrapped shape of GET /v1/marketplace/listings with its
// keyset-pagination cursor (NextCursor is "" when the page is exhausted).
type ListingPage struct {
	Listings   []Listing `json:"listings"`
	NextCursor string    `json:"next_cursor"`
}

// BrowseOptions selects the catalog search. Query is a case-insensitive
// substring match over name/description; Tags is an ANY-overlap filter; the
// page size and cursor mirror the server's keyset pagination.
type BrowseOptions struct {
	Query  string
	Tags   []string
	Limit  int // <= 0 lets the server default
	Cursor string
}

// PublishListingRequest is the POST /v1/marketplace/listings body. Version 0
// (the default) snapshots the agent's CURRENT live configuration; a positive
// version publishes that immutable config-version snapshot. Slug and
// description are optional (derived server-side when empty). Status is
// "published" (default) or "draft".
type PublishListingRequest struct {
	AgentID     string   `json:"agent_id"`
	Version     int      `json:"version,omitempty"`
	Name        string   `json:"name,omitempty"`
	Slug        string   `json:"slug,omitempty"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Status      string   `json:"status,omitempty"`
}

// InstalledAgent is the newly created agent of an install response
// (snake_case — distinct from the legacy PascalCase Agent wire shape the
// /agents handlers emit).
type InstalledAgent struct {
	ID             string    `json:"id"`
	OrganizationID string    `json:"organization_id"`
	Name           string    `json:"name"`
	Description    string    `json:"description"`
	Instructions   string    `json:"instructions"`
	Model          string    `json:"model"`
	Status         string    `json:"status"`
	CreatedAt      time.Time `json:"created_at"`
}

// InstallResult is the 201 body of POST /v1/marketplace/listings/{slug}/install:
// the listing (with its refreshed download_count) plus the freshly created
// agent in the caller's org. Name collisions get a deterministic -2/-3/…
// suffix server-side.
type InstallResult struct {
	Listing Listing        `json:"listing"`
	Agent   InstalledAgent `json:"agent"`
}

// BrowseListings searches the GLOBAL published catalog
// (GET /v1/marketplace/listings?q&tags&limit&cursor).
func (c *Client) BrowseListings(ctx context.Context, opts BrowseOptions) (*ListingPage, error) {
	query := make(url.Values)
	if opts.Query != "" {
		query.Set("q", opts.Query)
	}
	if len(opts.Tags) > 0 {
		query.Set("tags", strings.Join(opts.Tags, ","))
	}
	if opts.Limit > 0 {
		query.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Cursor != "" {
		query.Set("cursor", opts.Cursor)
	}
	var out ListingPage
	if err := c.do(ctx, httpMethodGet, "/marketplace/listings", query, nil, &out); err != nil {
		return nil, err
	}
	if out.Listings == nil {
		out.Listings = []Listing{}
	}
	return &out, nil
}

// GetListing resolves one listing by its globally-unique slug
// (GET /v1/marketplace/listings/{slug}). Draft/unlisted listings are visible
// only to their publisher org (foreign callers get 404).
func (c *Client) GetListing(ctx context.Context, slug string) (*Listing, error) {
	var out struct {
		Listing Listing `json:"listing"`
	}
	if err := c.do(ctx, httpMethodGet, "/marketplace/listings/"+urlPathEscape(slug), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out.Listing, nil
}

// PublishListing turns an existing agent of the caller's org into a catalog
// listing (POST /v1/marketplace/listings, 201).
func (c *Client) PublishListing(ctx context.Context, req PublishListingRequest) (*Listing, error) {
	var out struct {
		Listing Listing `json:"listing"`
	}
	if err := c.do(ctx, httpMethodPost, "/marketplace/listings", nil, req, &out); err != nil {
		return nil, err
	}
	return &out.Listing, nil
}

// InstallListing creates a NEW agent in the caller's org from the listing
// snapshot and bumps the download counter
// (POST /v1/marketplace/listings/{slug}/install, 201).
func (c *Client) InstallListing(ctx context.Context, slug string) (*InstallResult, error) {
	var out InstallResult
	if err := c.do(ctx, httpMethodPost, "/marketplace/listings/"+urlPathEscape(slug)+"/install", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UnlistListing removes a listing from the public catalog
// (DELETE /v1/marketplace/listings/{slug}). The caller's org must BE the
// publisher org; foreign/unknown slugs surface as 404.
func (c *Client) UnlistListing(ctx context.Context, slug string) error {
	var out struct {
		Deleted bool `json:"deleted"`
	}
	return c.do(ctx, httpMethodDelete, "/marketplace/listings/"+urlPathEscape(slug), nil, nil, &out)
}
