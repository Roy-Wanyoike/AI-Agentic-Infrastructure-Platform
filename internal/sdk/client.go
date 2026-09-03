// Package sdk is the typed Go client for the AgentOS API.
//
// It is stdlib-only and mirrors the HTTP surface documented in
// api/openapi.yaml as implemented by the handlers in cmd/api/:
//
//   - base URL + explicit "/v1" path prefix handling (the API serves the same
//     mux under both /v1 and /api/v1; this client uses the /v1 form);
//   - both credential styles the RequireAuthOrAPIKey middleware accepts:
//     "Authorization: Bearer <token>" (login-issued HMAC token) and the
//     "X-API-Key" header;
//   - the API's mixed envelope shapes: bare arrays (GET /agents), bare
//     objects (GET /agents/{id}, GET /runs/{id}), wrapped objects
//     ({"workflow":…}, {"document":…}, {"run_id":…,"steps":[…]}) and the
//     structured error envelopes ({"error":{code,message}} for 4xx/5xx,
//     {"errors":[{code,message,node_id}]} for 422 DSL validation);
//   - plain-text error bodies (http.Error legacy handlers) are surfaced as
//     the message of the returned *APIError.
//
// Every method takes a context and honours its cancellation/deadline. The
// transport is injectable via WithHTTPClient (tests use httptest servers;
// no network is required).
package sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultBaseURL is used when WithBaseURL is not supplied.
const DefaultBaseURL = "http://localhost:8080"

// DefaultTimeout bounds a single request when no http.Client is injected.
const DefaultTimeout = 30 * time.Second

// Client is a typed client for the AgentOS API. It is safe for concurrent
// use by multiple goroutines once constructed.
type Client struct {
	baseURL string
	http    *http.Client
	token   string
	apiKey  string
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL sets the API origin, e.g. "http://localhost:8080". A trailing
// slash and an existing "/v1" suffix are tolerated (the client never
// duplicates the version prefix). Empty values keep the default.
func WithBaseURL(u string) Option {
	return func(c *Client) {
		u = strings.TrimRight(strings.TrimSpace(u), "/")
		if u == "" {
			return
		}
		c.baseURL = u
	}
}

// WithHTTPClient injects the transport (httptest servers, timeouts, proxies).
// A nil client keeps the default.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h != nil {
			c.http = h
		}
	}
}

// WithToken sets the bearer token issued by POST /v1/auth/login.
func WithToken(t string) Option {
	return func(c *Client) { c.token = strings.TrimSpace(t) }
}

// WithAPIKey sets the static API key sent as X-API-Key.
func WithAPIKey(k string) Option {
	return func(c *Client) { c.apiKey = strings.TrimSpace(k) }
}

// New builds a client. Without options it targets http://localhost:8080 with
// the default transport and a 30s per-request timeout.
func New(opts ...Option) *Client {
	c := &Client{
		baseURL: DefaultBaseURL,
		http:    &http.Client{Timeout: DefaultTimeout},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	return c
}

// BaseURL reports the configured origin (no trailing slash).
func (c *Client) BaseURL() string { return c.baseURL }

// AuthMode reports how requests are authenticated: "bearer", "api-key" or
// "none" (the wording used by the CLI's whoami command).
func (c *Client) AuthMode() string {
	switch {
	case c.token != "":
		return "bearer"
	case c.apiKey != "":
		return "api-key"
	default:
		return "none"
	}
}

// endpoint joins the base URL with an API path. The "/v1" prefix is added
// unless the base URL already ends with it.
func (c *Client) endpoint(path string, query url.Values) string {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	base := c.baseURL
	if !strings.HasSuffix(base, "/v1") { // never double the version prefix
		base += "/v1"
	}
	full := base + path
	if len(query) > 0 {
		full += "?" + query.Encode()
	}
	return full
}

// newRequest builds an authenticated request with the standard headers.
func (c *Client) newRequest(ctx context.Context, method, fullURL string, body []byte) (*http.Request, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Mirrors RequireAuthOrAPIKey: Authorization is checked first, so both
	// credentials may be advertised; the server resolves the precedence.
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}
	return req, nil
}

// do performs one request/response round trip: encoding the optional body,
// decoding a successful JSON response into out (nil out skips decoding) and
// mapping every non-2xx onto *APIError.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	full := c.endpoint(path, query)
	var payload []byte
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		payload = buf
	}
	req, err := c.newRequest(ctx, method, full, payload)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return newAPIError(resp, raw)
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode %s %s response: %w", method, path, err)
	}
	return nil
}
