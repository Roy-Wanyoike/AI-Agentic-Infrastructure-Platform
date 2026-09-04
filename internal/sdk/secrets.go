package sdk

// Secrets resource (issue #54 CLI+SDK parity): the typed mirror of the
// secrets HTTP surface in cmd/api/secrets.go.
//
// Endpoints (all under /v1/secrets, tenant taken from the credentials):
//
//	POST   /secrets               -> CreateSecret  (agents.write, OWNER/ADMIN)
//	GET    /secrets               -> ListSecrets   (runs.execute, MEMBER+)
//	DELETE /secrets/{name}        -> DeleteSecret  (agents.write, OWNER/ADMIN)
//	POST   /secrets/{name}/reveal -> RevealSecret (organization.manage, OWNER)
//
// SECURITY: a secret value exists in exactly two wire places — the request
// body of POST /secrets and the ONE-TIME reveal response. List/create/delete
// responses are metadata-only by construction; this SDK mirrors that and
// never logs or caches values.

import (
	"context"
	"time"
)

// SecretMetadata is the value-free projection every secrets endpoint
// (except reveal) returns.
type SecretMetadata struct {
	Name       string    `json:"name"`
	KeyVersion int       `json:"key_version"`
	CreatedBy  string    `json:"created_by"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// SecretList is the wrapped shape of GET /v1/secrets (metadata only).
type SecretList struct {
	Secrets []SecretMetadata `json:"secrets"`
}

// CreateSecretRequest is the POST /v1/secrets body. The Value is consumed
// exactly once server-side (seal → store) and never echoed back.
type CreateSecretRequest struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// RevealedSecret is the ONE-TIME response of POST /v1/secrets/{name}/reveal:
// the metadata projection plus the plaintext value.
type RevealedSecret struct {
	SecretMetadata
	Value string `json:"value"`
}

// CreateSecret seals and stores a new org-scoped secret
// (POST /v1/secrets, 201). 409 SECRET_ALREADY_EXISTS surfaces as *APIError.
func (c *Client) CreateSecret(ctx context.Context, req CreateSecretRequest) (*SecretMetadata, error) {
	var out struct {
		Secret SecretMetadata `json:"secret"`
	}
	if err := c.do(ctx, httpMethodPost, "/secrets", nil, req, &out); err != nil {
		return nil, err
	}
	return &out.Secret, nil
}

// ListSecrets returns metadata ONLY (names + bookkeeping) for the caller's
// organization (GET /v1/secrets). Viewers are rejected server-side (403).
func (c *Client) ListSecrets(ctx context.Context) (*SecretList, error) {
	var out SecretList
	if err := c.do(ctx, httpMethodGet, "/secrets", nil, nil, &out); err != nil {
		return nil, err
	}
	if out.Secrets == nil {
		out.Secrets = []SecretMetadata{}
	}
	return &out, nil
}

// RevealSecret returns the plaintext value EXACTLY ONCE
// (POST /v1/secrets/{name}/reveal, OWNER only). Treat the returned Value as
// ephemeral: the server will never hand it out again. Audit entries record
// the reveal without the value.
func (c *Client) RevealSecret(ctx context.Context, name string) (*RevealedSecret, error) {
	var out struct {
		Secret RevealedSecret `json:"secret"`
	}
	if err := c.do(ctx, httpMethodPost, "/secrets/"+urlPathEscape(name)+"/reveal", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out.Secret, nil
}

// DeleteSecret soft-deletes one secret within the caller's organization
// (DELETE /v1/secrets/{name}; tombstone — foreign/unknown names surface as
// 404 without an existence leak).
func (c *Client) DeleteSecret(ctx context.Context, name string) error {
	var out struct {
		Deleted bool `json:"deleted"`
	}
	return c.do(ctx, httpMethodDelete, "/secrets/"+urlPathEscape(name), nil, nil, &out)
}
