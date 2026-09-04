package sdk

// Identity resource (issue #54 CLI+SDK parity): the typed mirror of the
// SSO (OIDC) + SCIM surfaces in cmd/api/sso.go and cmd/api/scim.go.
//
// Endpoints:
//
//      GET  /auth/sso/{org_slug}/login -> BeginSSOLogin (browser flow; 302)
//      POST /scim/tokens               -> MintSCIMToken (OWNER only; secret once)
//      GET  /scim/v2/Users             -> ListSCIMUsers (scim_ bearer ONLY)
//
// Two credential models live here, mirroring the server's deliberate split:
//   - the SSO login start and the token mint authenticate with the PLATFORM
//     surface (session token or API key; the mint additionally requires the
//     OWNER role);
//   - the SCIM 2.0 protocol endpoints accept ONLY a dedicated scim_ bearer
//     credential (RequireSCIMToken) — session tokens and API keys are
//     deliberately rejected, so call ListSCIMUsers with a client whose token
//     IS the scim_ secret (see the CLI's `sso list` for the wiring).

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// SCIMToken is the metadata of a minted SCIM bearer credential. The token
// hash is deliberately not echoed — it is neither a secret nor anything a
// client needs.
type SCIMToken struct {
	ID             string    `json:"id"`
	OrganizationID string    `json:"organization_id"`
	CreatedBy      string    `json:"created_by"`
	CreatedAt      time.Time `json:"created_at"`
}

// MintedSCIMToken is the 201 body of POST /v1/scim/tokens: the credential
// metadata plus the plaintext secret of the form scim_<64 hex>, shown
// EXACTLY ONCE (only its SHA-256 hex hash is persisted server-side).
type MintedSCIMToken struct {
	Token  SCIMToken `json:"token"`
	Secret string    `json:"secret"`
}

// SCIMUserEmail is one entry of the emails multi-valued attribute.
type SCIMUserEmail struct {
	Value   string `json:"value"`
	Primary bool   `json:"primary"`
}

// SCIMUserMeta is the standard meta block of a SCIM resource.
type SCIMUserMeta struct {
	ResourceType string `json:"resourceType"`
	Created      string `json:"created"`
	LastModified string `json:"lastModified"`
	Location     string `json:"location"`
}

// SCIMUser is the SCIM 2.0 core user projection of a directory identity
// (SSO JIT-provisioned or SCIM-created; no display names, no credential
// material, no roles — the platform's documented SCIM omissions).
type SCIMUser struct {
	Schemas  []string        `json:"schemas"`
	ID       string          `json:"id"`
	UserName string          `json:"userName"`
	Active   bool            `json:"active"`
	Emails   []SCIMUserEmail `json:"emails,omitempty"`
	Meta     SCIMUserMeta    `json:"meta"`
}

// SCIMUserList is the SCIM 2.0 ListResponse envelope
// (urn:ietf:params:scim:api:messages:2.0:ListResponse).
type SCIMUserList struct {
	Schemas      []string   `json:"schemas"`
	TotalResults int        `json:"totalResults"`
	StartIndex   int        `json:"startIndex"`
	ItemsPerPage int        `json:"itemsPerPage"`
	Resources    []SCIMUser `json:"Resources"`
}

// BeginSSOLogin starts the OIDC authorization-code flow for an organization
// (GET /v1/auth/sso/{org_slug}/login) and returns the IdP authorize URL.
// The API answers with a 302 redirect; this method never follows it — the
// Location header IS the value a browser should open. Unknown slugs and
// unconfigured orgs both 404 (ORG_NOT_FOUND / SSO_NOT_CONFIGURED); upstream
// discovery failures surface as 502 *APIError with no upstream detail.
func (c *Client) BeginSSOLogin(ctx context.Context, orgSlug string) (string, error) {
	full := c.endpoint("/auth/sso/"+urlPathEscape(orgSlug)+"/login", nil)
	req, err := c.newRequest(ctx, httpMethodGet, full, nil)
	if err != nil {
		return "", err
	}
	// Shallow-copy the configured client so the redirect is surfaced (not
	// followed) while keeping the transport and timeout settings.
	hc := *c.http
	hc.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode == http.StatusFound {
		loc, err := resp.Location()
		if err != nil {
			return "", fmt.Errorf("sso login redirect carried no location header")
		}
		return loc.String(), nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", newAPIError(resp, raw)
	}
	return "", fmt.Errorf("sso login: unexpected %s response (wanted a 302 redirect)", resp.Status)
}

// MintSCIMToken mints a SCIM bearer credential for the caller's organization
// (POST /v1/scim/tokens, 201, OWNER only). The Secret is shown EXACTLY ONCE.
func (c *Client) MintSCIMToken(ctx context.Context) (*MintedSCIMToken, error) {
	var out MintedSCIMToken
	if err := c.do(ctx, httpMethodPost, "/scim/tokens", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListSCIMUsers lists the org's directory identities (GET /v1/scim/v2/Users).
// filter must be empty or exactly `userName eq "..."` (any other filter is a
// 400 — unsupported filters never degrade into a full listing).
//
// AUTH: the SCIM surface accepts ONLY the dedicated scim_ bearer credential.
// Build a dedicated client for this call:
//
//	scimClient := sdk.New(sdk.WithBaseURL(base), sdk.WithToken(scimSecret))
//	list, err := scimClient.ListSCIMUsers(ctx, "")
func (c *Client) ListSCIMUsers(ctx context.Context, filter string) (*SCIMUserList, error) {
	var query url.Values
	if filter != "" {
		query = url.Values{"filter": []string{filter}}
	}
	var out SCIMUserList
	if err := c.do(ctx, httpMethodGet, "/scim/v2/Users", query, nil, &out); err != nil {
		return nil, err
	}
	if out.Resources == nil {
		out.Resources = []SCIMUser{}
	}
	return &out, nil
}
