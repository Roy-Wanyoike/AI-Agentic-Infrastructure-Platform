package sdk

// APIKeys resource (issue #54 CLI+SDK parity): the typed mirror of the
// API-key management surface in cmd/api/apikeys.go.
//
// Endpoints (all under /v1/api-keys):
//
//	POST   /api-keys       -> CreateAPIKey  (agents.write, OWNER/ADMIN)
//	GET    /api-keys       -> ListAPIKeys   (runs.execute, MEMBER+)
//	DELETE /api-keys/{id}  -> RevokeAPIKey  (agents.write, OWNER/ADMIN)
//
// SECURITY: the plaintext key value exists in exactly one wire place — the
// create response, returned EXACTLY ONCE as the top-level "value" sibling of
// the metadata object. List projections are metadata-only BY CONSTRUCTION
// (prefix only, never the value, never the hash); revocation is idempotent
// per service semantics (re-revoking a revoked key stays 200).

import (
	"context"
	"time"
)

// APIKeyMetadata is the value-free projection of one API key. Prefix is the
// "ak_…" display prefix kept for identification; the server only ever
// matches the hash of the FULL value.
type APIKeyMetadata struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Prefix    string    `json:"prefix"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	Revoked   bool      `json:"revoked"`
}

// APIKeyList is the wrapped shape of GET /v1/api-keys (newest first).
type APIKeyList struct {
	APIKeys []APIKeyMetadata `json:"api_keys"`
}

// CreateAPIKeyRequest is the POST /v1/api-keys body.
type CreateAPIKeyRequest struct {
	Name string `json:"name"`
}

// CreatedAPIKey is the 201 body of POST /v1/api-keys: the metadata object
// plus the plaintext value. The value is delivered EXACTLY ONCE — the list
// and revoke responses never echo it and only its SHA-256 hash is persisted.
type CreatedAPIKey struct {
	APIKey APIKeyMetadata `json:"api_key"`
	Value  string         `json:"value"`
}

// CreateAPIKey mints a new key for the caller's organization
// (POST /v1/api-keys, 201). 422 VALIDATION_ERROR on an empty name surfaces
// as *APIError.
func (c *Client) CreateAPIKey(ctx context.Context, req CreateAPIKeyRequest) (*CreatedAPIKey, error) {
	var out CreatedAPIKey
	if err := c.do(ctx, httpMethodPost, "/api-keys", nil, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListAPIKeys returns metadata ONLY for the caller's organization, newest
// first (GET /v1/api-keys). Revoked keys stay listed with Revoked=true.
func (c *Client) ListAPIKeys(ctx context.Context) (*APIKeyList, error) {
	var out APIKeyList
	if err := c.do(ctx, httpMethodGet, "/api-keys", nil, nil, &out); err != nil {
		return nil, err
	}
	if out.APIKeys == nil {
		out.APIKeys = []APIKeyMetadata{}
	}
	return &out, nil
}

// RevokeAPIKey revokes one key within the caller's organization
// (DELETE /v1/api-keys/{id}). Unknown/foreign ids surface as 404; revoking
// an already-revoked key stays 200 (idempotent).
func (c *Client) RevokeAPIKey(ctx context.Context, id string) error {
	var out struct {
		Revoked bool `json:"revoked"`
	}
	return c.do(ctx, httpMethodDelete, "/api-keys/"+urlPathEscape(id), nil, nil, &out)
}
