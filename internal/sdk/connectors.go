package sdk

// Connectors resource (issue #54 CLI+SDK parity): the typed mirror of the
// connectors-framework HTTP surface in cmd/api/connectors.go.
//
// Endpoints (all under /v1/connectors, writes OWNER/ADMIN, reads MEMBER+):
//
//	POST   /connectors           -> CreateConnector
//	GET    /connectors           -> ListConnectors
//	GET    /connectors/{id}      -> GetConnector
//	DELETE /connectors/{id}      -> DeleteConnector
//	POST   /connectors/{id}/test -> TestConnector (live health check)
//
// SECURITY: secret VALUES never appear anywhere in this surface. SecretRef
// is a NAME reference into the secrets store and ConnectorConfig carries
// header TEMPLATES and auth-style parameters only — mirrored structurally so
// a value could not slip through the typed client.

import (
	"context"
	"time"
)

// ConnectorConfig is the connector's NON-SECRET request configuration.
type ConnectorConfig struct {
	// AuthStyle is one of none|bearer|basic|api_key_header ("" -> none).
	AuthStyle string `json:"auth_style,omitempty"`
	// Headers are static header templates merged into every built request.
	Headers map[string]string `json:"headers,omitempty"`
	// APIKeyHeader names the header for the api_key_header style.
	APIKeyHeader string `json:"api_key_header,omitempty"`
	// APIKeyPrefix is prepended to the resolved secret for api_key_header.
	APIKeyPrefix string `json:"api_key_prefix,omitempty"`
	// Username is the basic-auth username (the password comes from the
	// secret referenced by SecretRef).
	Username string `json:"username,omitempty"`
}

// Connector is the org-scoped registry row. LastCheckAt/LastCheckStatus
// carry the outcome of the most recent live health-check probe (nil/""
// while never checked — the handler emits last_check_at:null explicitly).
type Connector struct {
	ID              string          `json:"id"`
	Name            string          `json:"name"`
	Type            string          `json:"type"` // webhook | http
	BaseURL         string          `json:"base_url"`
	SecretRef       string          `json:"secret_ref"`
	Status          string          `json:"status"` // active | disabled
	Config          ConnectorConfig `json:"config"`
	CreatedBy       string          `json:"created_by"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
	LastCheckAt     *time.Time      `json:"last_check_at"`
	LastCheckStatus string          `json:"last_check_status"` // ""|ok|error
}

// ConnectorList is the wrapped shape of GET /v1/connectors (name ASC).
type ConnectorList struct {
	Connectors []Connector `json:"connectors"`
}

// CreateConnectorRequest is the POST /v1/connectors body — exactly the
// handler's flattened payload (config fields inline, secret NAME only).
type CreateConnectorRequest struct {
	Name         string            `json:"name"`
	Type         string            `json:"type"`
	BaseURL      string            `json:"base_url"`
	AuthStyle    string            `json:"auth_style,omitempty"`
	Headers      map[string]string `json:"headers,omitempty"`
	APIKeyHeader string            `json:"api_key_header,omitempty"`
	APIKeyPrefix string            `json:"api_key_prefix,omitempty"`
	Username     string            `json:"username,omitempty"`
	SecretRef    string            `json:"secret_ref,omitempty"`
	Status       string            `json:"status,omitempty"`
}

// ConnectorTestResult is the outcome of one live health-check probe. Error
// messages never contain secret values (resolver failures surface the secret
// NAME and the underlying error kind only).
type ConnectorTestResult struct {
	ConnectorID string    `json:"connector_id"`
	Status      string    `json:"status"` // ok | error
	StatusCode  int       `json:"status_code"`
	LatencyMS   int64     `json:"latency_ms"`
	Error       string    `json:"error,omitempty"`
	CheckedAt   time.Time `json:"checked_at"`
}

// CreateConnector validates and registers a new org-scoped connector
// (POST /v1/connectors, 201). The request never carries secret VALUES.
func (c *Client) CreateConnector(ctx context.Context, req CreateConnectorRequest) (*Connector, error) {
	var out struct {
		Connector Connector `json:"connector"`
	}
	if err := c.do(ctx, httpMethodPost, "/connectors", nil, req, &out); err != nil {
		return nil, err
	}
	return &out.Connector, nil
}

// ListConnectors returns the caller's connectors, name ASC
// (GET /v1/connectors).
func (c *Client) ListConnectors(ctx context.Context) (*ConnectorList, error) {
	var out ConnectorList
	if err := c.do(ctx, httpMethodGet, "/connectors", nil, nil, &out); err != nil {
		return nil, err
	}
	if out.Connectors == nil {
		out.Connectors = []Connector{}
	}
	return &out, nil
}

// GetConnector returns one connector within the caller's organization
// (GET /v1/connectors/{id}).
func (c *Client) GetConnector(ctx context.Context, id string) (*Connector, error) {
	var out struct {
		Connector Connector `json:"connector"`
	}
	if err := c.do(ctx, httpMethodGet, "/connectors/"+urlPathEscape(id), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out.Connector, nil
}

// DeleteConnector hard-deletes one connector within the caller's
// organization (DELETE /v1/connectors/{id}; foreign/unknown ids surface as
// 404 without an existence leak).
func (c *Client) DeleteConnector(ctx context.Context, id string) error {
	var out struct {
		Deleted bool `json:"deleted"`
	}
	return c.do(ctx, httpMethodDelete, "/connectors/"+urlPathEscape(id), nil, nil, &out)
}

// TestConnector triggers the live health check (5s timeout; the outcome is
// recorded on the connector as last_check_at/last_check_status)
// (POST /v1/connectors/{id}/test).
func (c *Client) TestConnector(ctx context.Context, id string) (*ConnectorTestResult, error) {
	var out struct {
		Test ConnectorTestResult `json:"test"`
	}
	if err := c.do(ctx, httpMethodPost, "/connectors/"+urlPathEscape(id)+"/test", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out.Test, nil
}
