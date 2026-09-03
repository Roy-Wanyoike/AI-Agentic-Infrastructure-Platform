package connectors

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// service.go is the org-scoped connectors service (issue #30), dual-mode per
// the platform convention:
//
//   - NewService(): in-memory mode, zero infrastructure.
//   - NewServiceWithStore(NewPostgresStore(db), resolver): Postgres mode.
//
// A connector is the governed registry entry for an external system (CRM,
// internal API, SaaS webhook receiver). The service owns three responsibilities:
//
//  1. CRUD with validation (types, base URLs, auth styles, statuses).
//  2. Test(): a live health check (GET to base_url, 5s default timeout) that
//     records last_check_at / last_check_status ("ok" | "error").
//  3. BuildRequest(): turns a connector + method/path/body into an
//     *http.Request, resolving secret_ref through the injected SecretResolver
//     seam and applying the configured auth style plus header templates.
//
// SECRET HANDLING: secret VALUES never rest in this package. Only the
// secret_ref NAME is stored; values are fetched per request through
// SecretResolver and applied directly onto the outgoing request headers. They
// are never part of the Connector projection, never logged, never echoed.
//
// The SecretResolver interface is intentionally declared with the method
// shape `Resolve(ctx, orgID, name) (string, error)` so that the platform
// secrets service satisfies it STRUCTURALLY — callers wire the concrete
// service (or any adapter) without this package importing it.

var (
	ErrOrgRequired            = errors.New("organization_id is required")
	ErrNameRequired           = errors.New("connector name is required")
	ErrNameTooLong            = errors.New("connector name exceeds 255 characters")
	ErrTypeInvalid            = errors.New("connector type must be one of: webhook, http")
	ErrBaseURLRequired        = errors.New("base_url is required")
	ErrBaseURLInvalid         = errors.New("base_url must be an absolute http(s) URL with a host")
	ErrStatusInvalid          = errors.New("status must be one of: active, disabled")
	ErrAuthStyleInvalid       = errors.New("auth_style must be one of: none, bearer, basic, api_key_header")
	ErrNotFound               = errors.New("connector not found")
	ErrDuplicate              = errors.New("connector already exists")
	ErrConnectorDisabled      = errors.New("connector is disabled")
	ErrInvalidMethod          = errors.New("method must be one of: GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS")
	ErrUpdatedByRequired      = errors.New("actor identity is required")
	ErrSecretRefRequired      = errors.New("secret_ref is required for the configured auth style")
	ErrSecretResolverRequired = errors.New("no secret resolver is wired for this service")
	ErrInvalidConnectorRef    = errors.New("connector does not belong to this organization")
)

// Connector types.
const (
	TypeWebhook = "webhook"
	TypeHTTP    = "http"
)

// Connector statuses.
const (
	StatusActive   = "active"
	StatusDisabled = "disabled"
)

// Auth styles (config.auth_style).
const (
	AuthStyleNone         = "none"
	AuthStyleBearer       = "bearer"
	AuthStyleBasic        = "basic"
	AuthStyleAPIKeyHeader = "api_key_header"
)

// DefaultAPIKeyHeader is the header used for the api_key_header style when
// config.api_key_header does not name one.
const DefaultAPIKeyHeader = "X-API-Key"

// DefaultTestTimeout bounds a single live health-check probe (issue #30: 5s).
const DefaultTestTimeout = 5 * time.Second

// allowedMethods is the BuildRequest method allowlist (governance: the
// framework exists to constrain outbound calls, not to forward arbitrary
// HTTP verbs).
var allowedMethods = map[string]bool{
	http.MethodGet:     true,
	http.MethodHead:    true,
	http.MethodPost:    true,
	http.MethodPut:     true,
	http.MethodPatch:   true,
	http.MethodDelete:  true,
	http.MethodOptions: true,
}

// validAuthStyles mirrors the CHECK constraint in migration 020.
var validAuthStyles = map[string]bool{
	AuthStyleNone:         true,
	AuthStyleBearer:       true,
	AuthStyleBasic:        true,
	AuthStyleAPIKeyHeader: true,
}

// Config is the connector's NON-SECRET request configuration (stored as
// JSONB). It is safe to render in API responses: it contains header
// TEMPLATES and auth style parameters, never secret values.
type Config struct {
	// AuthStyle is one of none|bearer|basic|api_key_header ("" -> none).
	AuthStyle string `json:"auth_style,omitempty"`
	// Headers are static header templates merged into every built request
	// (per-request headers win over these; auth headers win over both).
	Headers map[string]string `json:"headers,omitempty"`
	// APIKeyHeader names the header for the api_key_header style
	// ("" -> DefaultAPIKeyHeader).
	APIKeyHeader string `json:"api_key_header,omitempty"`
	// APIKeyPrefix is prepended to the resolved secret for the
	// api_key_header style (e.g. "Bearer " when the remote expects a scheme
	// inside a custom header).
	APIKeyPrefix string `json:"api_key_prefix,omitempty"`
	// Username is the basic-auth username (the password comes from the
	// secret referenced by secret_ref).
	Username string `json:"username,omitempty"`
}

// Connector is the org-scoped registry row.
type Connector struct {
	ID              string
	OrganizationID  string
	Name            string
	Type            string // webhook | http
	BaseURL         string
	Config          Config
	SecretRef       string // NAME reference into the secrets store ("" = none)
	Status          string // active | disabled
	LastCheckAt     *time.Time
	LastCheckStatus string // "" (never checked) | ok | error
	CreatedBy       string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// TestResult is the outcome of one live health-check probe. Error messages
// never contain secret values (resolver failures surface the secret NAME and
// the underlying error kind only).
type TestResult struct {
	ConnectorID string    `json:"connector_id"`
	Status      string    `json:"status"` // ok | error
	StatusCode  int       `json:"status_code"`
	LatencyMS   int64     `json:"latency_ms"`
	Error       string    `json:"error,omitempty"`
	CheckedAt   time.Time `json:"checked_at"`
}

// SecretResolver is the minimal injected seam for resolving secret_ref names
// into values at request-build time. The method shape matches the platform
// secrets service's Resolve(ctx, orgID, name) exactly, so that service
// satisfies this interface WITHOUT an adapter and WITHOUT this package
// importing it (the orchestrator wires the concrete service directly).
type SecretResolver interface {
	Resolve(ctx context.Context, orgID, name string) (string, error)
}

// SecretResolverFunc adapts a plain function (and, transitively, any resolver
// whose signature drifts from the interface) into a SecretResolver.
type SecretResolverFunc func(ctx context.Context, orgID, name string) (string, error)

// Resolve implements SecretResolver.
func (f SecretResolverFunc) Resolve(ctx context.Context, orgID, name string) (string, error) {
	if f == nil {
		return "", ErrSecretResolverRequired
	}
	return f(ctx, orgID, name)
}

// Store abstracts durable connector storage. Tenant scoping is enforced in
// the store layer too (every statement filters organization_id); the service
// treats the store as untrusted and re-checks isolation on the read path.
type Store interface {
	// Create inserts one row; a live (org, name) conflict maps to ErrDuplicate.
	Create(ctx context.Context, c *Connector) error
	// Update replaces the mutable fields (name/type/base_url/config/
	// secret_ref/status) of one row within one tenant.
	Update(ctx context.Context, c *Connector) error
	// Delete hard-deletes one row within one tenant (RowsAffected 0 ->
	// ErrNotFound).
	Delete(ctx context.Context, orgID, id string) error
	// Get returns one live connector within one tenant.
	Get(ctx context.Context, orgID, id string) (*Connector, error)
	// List returns all connectors of exactly one tenant (name ASC).
	List(ctx context.Context, orgID string) ([]*Connector, error)
	// UpdateCheckStatus records the outcome of one health-check probe
	// (last_check_at + last_check_status; updated_at is intentionally left
	// untouched — last_check_at carries the freshness).
	UpdateCheckStatus(ctx context.Context, orgID, id string, checkedAt time.Time, status string) error
}

// Service is the dual-mode connectors service.
type Service struct {
	mu         sync.RWMutex
	items      map[string]map[string]*Connector // orgID -> id -> connector (memory mode)
	store      Store
	resolver   SecretResolver
	httpClient *http.Client
}

// NewService returns the in-memory connectors service (zero infrastructure).
func NewService() *Service {
	return &Service{
		items:      make(map[string]map[string]*Connector),
		httpClient: defaultHTTPClient(),
	}
}

// NewServiceWithStore returns a Postgres-backed service. The store is
// mandatory (fail fast: silently degrading a governance surface to memory
// would be a regression); the resolver may be nil, in which case connectors
// that need a secret fail at BuildRequest/Test time with
// ErrSecretResolverRequired instead of at construction.
func NewServiceWithStore(store Store, resolver SecretResolver) (*Service, error) {
	if store == nil {
		return nil, errors.New("connectors: store is required")
	}
	return &Service{
		store:      store,
		resolver:   resolver,
		httpClient: defaultHTTPClient(),
	}, nil
}

// SetSecretResolver wires (or replaces) the secret-ref resolver. Nil-safe.
func (s *Service) SetSecretResolver(r SecretResolver) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolver = r
}

// SetHTTPClient replaces the client used by Test probes (tests inject short
// timeouts). Nil restores the default 5s client.
func (s *Service) SetHTTPClient(c *http.Client) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if c == nil {
		c = defaultHTTPClient()
	}
	s.httpClient = c
}

func defaultHTTPClient() *http.Client {
	return &http.Client{
		Timeout: DefaultTestTimeout,
		Transport: &http.Transport{
			// Proxy intentionally disabled: an environment proxy would silently
			// reroute governed outbound calls.
			Proxy:               nil,
			TLSHandshakeTimeout: DefaultTestTimeout,
		},
	}
}

// CreateInput is the validated-on-arrival payload for Create/Update.
type CreateInput struct {
	Name      string
	Type      string
	BaseURL   string
	Config    Config
	SecretRef string
	Status    string
}

// normalizeInput validates CreateInput and fills defaults (status "",
// config auth_style "" -> active / none).
func normalizeInput(in CreateInput) (CreateInput, error) {
	out := in
	out.Name = strings.TrimSpace(out.Name)
	if out.Name == "" {
		return out, ErrNameRequired
	}
	if len(out.Name) > 255 {
		return out, ErrNameTooLong
	}
	out.Type = strings.TrimSpace(strings.ToLower(out.Type))
	if out.Type != TypeWebhook && out.Type != TypeHTTP {
		return out, ErrTypeInvalid
	}
	out.BaseURL = strings.TrimSpace(out.BaseURL)
	if out.BaseURL == "" {
		return out, ErrBaseURLRequired
	}
	u, err := url.Parse(out.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return out, ErrBaseURLInvalid
	}
	out.Status = strings.TrimSpace(strings.ToLower(out.Status))
	if out.Status == "" {
		out.Status = StatusActive
	}
	if out.Status != StatusActive && out.Status != StatusDisabled {
		return out, ErrStatusInvalid
	}
	out.SecretRef = strings.TrimSpace(out.SecretRef)
	out.Config.AuthStyle = strings.TrimSpace(strings.ToLower(out.Config.AuthStyle))
	if out.Config.AuthStyle == "" {
		out.Config.AuthStyle = AuthStyleNone
	}
	if !validAuthStyles[out.Config.AuthStyle] {
		return out, ErrAuthStyleInvalid
	}
	// Config hygiene: header template keys/values must not carry CR/LF
	// (header injection via stored templates).
	for k, v := range out.Config.Headers {
		if strings.ContainsAny(k, "\r\n") || strings.ContainsAny(v, "\r\n") {
			return out, ErrAuthStyleInvalid
		}
	}
	return out, nil
}

// Create validates and stores a new connector within one tenant.
func (s *Service) Create(ctx context.Context, orgID string, in CreateInput, createdBy string) (*Connector, error) {
	if s == nil {
		return nil, errors.New("connectors: service is nil")
	}
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgRequired
	}
	if strings.TrimSpace(createdBy) == "" {
		return nil, ErrUpdatedByRequired
	}
	norm, err := normalizeInput(in)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	rec := &Connector{
		ID:             uuid.NewString(),
		OrganizationID: orgID,
		Name:           norm.Name,
		Type:           norm.Type,
		BaseURL:        norm.BaseURL,
		Config:         norm.Config,
		SecretRef:      norm.SecretRef,
		Status:         norm.Status,
		CreatedBy:      createdBy,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	if s.store != nil {
		if err := s.store.Create(ctx, rec); err != nil {
			return nil, err
		}
		return rec, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	orgItems, ok := s.items[orgID]
	if !ok {
		orgItems = make(map[string]*Connector)
		s.items[orgID] = orgItems
	}
	for _, existing := range orgItems {
		if existing != nil && existing.Name == norm.Name {
			return nil, ErrDuplicate
		}
	}
	orgItems[rec.ID] = rec
	return rec, nil
}

// Update replaces the mutable governance fields of one connector within one
// tenant, preserving creator and creation time. Unknown/foreign ids are
// ErrNotFound.
func (s *Service) Update(ctx context.Context, orgID, id string, in CreateInput, updatedBy string) (*Connector, error) {
	if s == nil {
		return nil, errors.New("connectors: service is nil")
	}
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgRequired
	}
	if strings.TrimSpace(updatedBy) == "" {
		return nil, ErrUpdatedByRequired
	}
	norm, err := normalizeInput(in)
	if err != nil {
		return nil, err
	}

	if s.store != nil {
		// Preserve original creator + created_at: load the live row first
		// (org-guarded) so the UPDATE can restate them.
		existing, err := s.store.Get(ctx, orgID, id)
		if err != nil {
			return nil, err
		}
		existing.Name = norm.Name
		existing.Type = norm.Type
		existing.BaseURL = norm.BaseURL
		existing.Config = norm.Config
		existing.SecretRef = norm.SecretRef
		existing.Status = norm.Status
		existing.UpdatedAt = time.Now().UTC()
		if err := s.store.Update(ctx, existing); err != nil {
			return nil, err
		}
		return existing, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.items[orgID][id]
	if !ok || rec == nil {
		return nil, ErrNotFound
	}
	rec.Name = norm.Name
	rec.Type = norm.Type
	rec.BaseURL = norm.BaseURL
	rec.Config = norm.Config
	rec.SecretRef = norm.SecretRef
	rec.Status = norm.Status
	rec.UpdatedAt = time.Now().UTC()
	return rec, nil
}

// Delete hard-deletes one connector within one tenant. Unknown or foreign
// ids are ErrNotFound (no existence leak across tenants).
func (s *Service) Delete(ctx context.Context, orgID, id string) error {
	if s == nil {
		return errors.New("connectors: service is nil")
	}
	if strings.TrimSpace(orgID) == "" {
		return ErrOrgRequired
	}
	if strings.TrimSpace(id) == "" {
		return ErrNotFound
	}
	if s.store != nil {
		return s.store.Delete(ctx, orgID, id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	orgItems, ok := s.items[orgID]
	if !ok {
		return ErrNotFound
	}
	rec, ok := orgItems[id]
	if !ok || rec == nil {
		return ErrNotFound
	}
	delete(orgItems, id)
	return nil
}

// List returns every connector of one tenant ordered by name ASC.
func (s *Service) List(ctx context.Context, orgID string) ([]*Connector, error) {
	if s == nil {
		return nil, errors.New("connectors: service is nil")
	}
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgRequired
	}
	if s.store != nil {
		return s.store.List(ctx, orgID)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Connector, 0)
	for _, rec := range s.items[orgID] {
		if rec == nil {
			continue
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Get returns one live connector within one tenant.
func (s *Service) Get(ctx context.Context, orgID, id string) (*Connector, error) {
	if s == nil {
		return nil, errors.New("connectors: service is nil")
	}
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgRequired
	}
	return s.get(ctx, orgID, id)
}

// get is the shared org-guarded lookup (service-internal).
func (s *Service) get(ctx context.Context, orgID, id string) (*Connector, error) {
	if strings.TrimSpace(id) == "" {
		return nil, ErrNotFound
	}
	if s.store != nil {
		return s.store.Get(ctx, orgID, id)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.items[orgID][id]
	if !ok || rec == nil {
		return nil, ErrNotFound
	}
	return rec, nil
}

// Test runs the live health check for one connector (issue #30): a GET to
// base_url (no extra path) with the configured auth applied, bounded by the
// service's HTTP client timeout (5s by default). The outcome is recorded as
// last_check_at + last_check_status ("ok" for 2xx-3xx responses, "error" for
// non-2xx responses, network failures, timeouts and unresolvable secrets)
// and returned to the caller. Disabled connectors remain probeable — the
// check exists precisely to inform the enable/disable decision.
func (s *Service) Test(ctx context.Context, orgID, id string) (*TestResult, error) {
	if s == nil {
		return nil, errors.New("connectors: service is nil")
	}
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgRequired
	}
	c, err := s.get(ctx, orgID, id)
	if err != nil {
		return nil, err
	}

	result := s.probe(ctx, orgID, c)
	result.ConnectorID = c.ID
	result.CheckedAt = time.Now().UTC()

	if s.store != nil {
		if err := s.store.UpdateCheckStatus(ctx, orgID, c.ID, result.CheckedAt, result.Status); err != nil {
			return nil, err
		}
	} else {
		s.mu.Lock()
		if rec := s.items[orgID][c.ID]; rec != nil {
			checked := result.CheckedAt
			rec.LastCheckAt = &checked
			rec.LastCheckStatus = result.Status
		}
		s.mu.Unlock()
	}
	return result, nil
}

// probe builds and executes the health-check request, classifying the
// outcome. It never returns an error for probe failures — those ARE the
// result ("error" status); hard errors (connector load) are handled by the
// caller.
func (s *Service) probe(ctx context.Context, orgID string, c *Connector) *TestResult {
	result := &TestResult{Status: "error"}
	req, err := s.buildRequest(ctx, orgID, c, http.MethodGet, "", nil, nil, true)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	s.mu.RLock()
	client := s.httpClient
	s.mu.RUnlock()
	if client == nil {
		client = defaultHTTPClient()
	}
	start := time.Now()
	resp, err := client.Do(req)
	result.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		result.Error = err.Error()
		return result
	}
	defer func() { _ = resp.Body.Close() }()
	result.StatusCode = resp.StatusCode
	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		result.Status = "ok"
		return result
	}
	result.Error = fmt.Sprintf("upstream returned status %d", resp.StatusCode)
	return result
}

// BuildRequest turns a connector into an *http.Request (issue #30). Semantics:
//
//   - URL: base_url joined with path by string concatenation ("/" inserted
//     when missing) — the base path prefix is PRESERVED (no RFC-3986
//     reference resolution, so base_url "https://api.acme.com/v1" + path
//     "/contacts" yields "https://api.acme.com/v1/contacts").
//   - Headers, lowest to highest precedence: config.Headers templates, then
//     the per-request headers argument (wins over templates), then the
//     auth-style headers (always win — neither templates nor callers can
//     clobber injected credentials).
//   - secret_ref is resolved through the injected SecretResolver and applied
//     per config.auth_style: bearer -> "Authorization: Bearer <token>",
//     basic -> "Authorization: Basic base64(username:<password>)" (password
//     from the secret, username from config), api_key_header ->
//     "<config.api_key_header|X-API-Key>: <prefix><key>".
//   - A disabled connector refuses requests with ErrConnectorDisabled.
func (s *Service) BuildRequest(ctx context.Context, orgID string, c *Connector, method, path string, body []byte, headers map[string]string) (*http.Request, error) {
	if s == nil {
		return nil, errors.New("connectors: service is nil")
	}
	return s.buildRequest(ctx, orgID, c, method, path, body, headers, false)
}

// buildRequest is the shared implementation; allowDisabled lets the health
// check probe connectors whose status is disabled.
func (s *Service) buildRequest(ctx context.Context, orgID string, c *Connector, method, path string, body []byte, headers map[string]string, allowDisabled bool) (*http.Request, error) {
	if c == nil {
		return nil, ErrInvalidConnectorRef
	}
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgRequired
	}
	if c.OrganizationID != orgID {
		// Cross-tenant reference: surfaced as a generic refusal (no existence leak).
		return nil, ErrInvalidConnectorRef
	}
	if !allowDisabled && c.Status == StatusDisabled {
		return nil, ErrConnectorDisabled
	}
	method = strings.ToUpper(strings.TrimSpace(method))
	if method == "" {
		method = http.MethodGet
	}
	if !allowedMethods[method] {
		return nil, ErrInvalidMethod
	}

	hdr := make(http.Header)
	// 1. Static header templates from config.
	for k, v := range c.Config.Headers {
		if strings.TrimSpace(k) == "" {
			continue
		}
		hdr.Set(strings.TrimSpace(k), v)
	}
	// 2. Per-request headers (win over templates).
	for k, v := range headers {
		if strings.TrimSpace(k) == "" {
			continue
		}
		hdr.Set(strings.TrimSpace(k), v)
	}
	// 3. Auth-style injection (wins over everything above).
	if err := s.applyAuth(ctx, orgID, c, hdr); err != nil {
		return nil, err
	}

	target := joinURL(c.BaseURL, path)
	u, err := url.Parse(target)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, ErrBaseURLInvalid
	}

	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
		if hdr.Get("Content-Type") == "" {
			hdr.Set("Content-Type", "application/json")
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, target, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("connectors: build request: %w", err)
	}
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	return req, nil
}

// applyAuth resolves secret_ref (when the style requires it) and sets the
// auth headers on hdr. Auth style "none" is a no-op even when a secret_ref is
// configured (the ref is only consumed by an auth style).
func (s *Service) applyAuth(ctx context.Context, orgID string, c *Connector, hdr http.Header) error {
	style := c.Config.AuthStyle
	if style == "" {
		style = AuthStyleNone
	}
	if style == AuthStyleNone {
		return nil
	}
	if strings.TrimSpace(c.SecretRef) == "" {
		return ErrSecretRefRequired
	}
	s.mu.RLock()
	resolver := s.resolver
	s.mu.RUnlock()
	if resolver == nil {
		return ErrSecretResolverRequired
	}
	value, err := resolver.Resolve(ctx, orgID, c.SecretRef)
	if err != nil {
		// The secret NAME is connector configuration (not a secret value) and
		// is safe to surface; the underlying error must never carry values.
		return fmt.Errorf("connectors: resolve secret %q: %w", c.SecretRef, err)
	}
	switch style {
	case AuthStyleBearer:
		hdr.Set("Authorization", "Bearer "+value)
	case AuthStyleBasic:
		username := c.Config.Username
		hdr.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(username+":"+value)))
	case AuthStyleAPIKeyHeader:
		name := strings.TrimSpace(c.Config.APIKeyHeader)
		if name == "" {
			name = DefaultAPIKeyHeader
		}
		hdr.Set(name, c.Config.APIKeyPrefix+value)
	default:
		return ErrAuthStyleInvalid
	}
	return nil
}

// joinURL appends path to baseURL preserving the base path prefix.
func joinURL(baseURL, path string) string {
	base := strings.TrimRight(baseURL, "/")
	p := path
	if p != "" && !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return base + p
}

// configJSON marshals Config for JSONB storage ("{}" fallback on failure).
func configJSON(cfg Config) string {
	encoded, err := json.Marshal(cfg)
	if err != nil || encoded == nil {
		return "{}"
	}
	return string(encoded)
}
