package mcp

// registry.go — the org-scoped MCP server registry (issue #82): the
// governance layer that lets an organization REGISTER external MCP servers,
// health-checks them at registration (initialize ping), and exposes their
// tools to agents as native platform tools.
//
// Dual-mode per the platform convention:
//   - NewRegistry(): in-memory mode, zero infrastructure.
//   - NewRegistryWithStore(NewPostgresStore(db), resolver): Postgres mode.
//
// TOOL EXPOSURE DECISION (documented v1 contract, issue #82 asks to pick
// exactly one approach): STATIC CACHE. At registration (and at every
// successful re-test) the service calls tools/list and caches the remote
// tool definitions (name, description, inputSchema) on the server row.
// Agent-facing tools are built from that cache under sanitized flat names
// `mcp_<server>_<tool>` and refreshed ONLY by the explicit re-test endpoint
// (POST /v1/mcp-servers/{id}/test). No dynamic per-run discovery: it keeps
// the tool list stable for prompts, the registry deterministic, and matches
// the connectors health-check model (last_check_at + explicit test).
//
// The logical tool type is `mcp:<server-ref>:<tool>`; the sanitized flat
// registry name is the concrete identifier a model invokes (colons are
// avoided in tool names for prompt/parse hygiene — the server name and
// remote tool name are preserved verbatim in the adapter metadata and in
// every audit row).
//
// TENANT GUARD: every operation is keyed by organization_id and surfaced
// from the auth claims; cross-tenant reads are ErrNotFound (no existence
// leak). Tool adapters built by RegisterToolsInto(ctx, orgID, ...) expose
// ONLY that org's servers, and each adapter resolves its auth secret
// strictly within its OWNING org — an adapter registered for org A can
// never authenticate as org B, and org B's tool builds never include
// org A's servers.
//
// AUDIT: every external tool invocation is audited (action "mcp.tool_call",
// server ref + remote tool name in metadata) through the optional Auditor
// seam — structurally satisfied by *audit.Service; the wiring layer passes
// it in, keeping this package free of the audit store. Arguments are never
// logged (they may embed secret material).

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"agentos/internal/audit"
	"agentos/internal/tools"

	"github.com/google/uuid"
)

// Server statuses.
const (
	// StatusOK: the last initialize handshake succeeded (tools are exposed).
	StatusOK = "ok"
	// StatusUnreachable: the last handshake failed (tools are NOT exposed).
	StatusUnreachable = "unreachable"
	// StatusDisabled: governance switch — no probes, no tools exposed.
	StatusDisabled = "disabled"
)

// DefaultCheckTimeout bounds each request of a registration/health probe.
const DefaultCheckTimeout = 5 * time.Second

// DefaultAuthHeader is the header the auth secret is injected into when the
// registration does not name one.
const DefaultAuthHeader = "Authorization"

// Service errors (handler layer maps them onto the contract codes).
var (
	ErrOrgRequired            = errors.New("organization_id is required")
	ErrNameRequired           = errors.New("mcp server name is required")
	ErrNameTooLong            = errors.New("mcp server name exceeds 255 characters")
	ErrEndpointRequired       = errors.New("endpoint_url is required")
	ErrEndpointInvalid        = errors.New("endpoint_url must be an absolute http(s) URL with a host")
	ErrStatusInvalid          = errors.New("status must be one of: ok, unreachable, disabled")
	ErrAuthHeaderInvalid      = errors.New("auth_header must be a valid HTTP header name")
	ErrNotFound               = errors.New("mcp server not found")
	ErrDuplicate              = errors.New("mcp server already exists")
	ErrUpdatedByRequired      = errors.New("actor identity is required")
	ErrServerUnreachable      = errors.New("mcp server unreachable")
	ErrSecretResolverRequired = errors.New("no secret resolver is wired for this registry")
)

// headerNamePattern validates the auth_header override (RFC 7230 token).
var headerNamePattern = regexp.MustCompile(`^[!#$%&'*+\-.^_` + "`" + `~0-9A-Za-z]+$`)

// Server is the org-scoped registry row for one external MCP server.
type Server struct {
	ID              string
	OrganizationID  string
	Name            string
	EndpointURL     string // full URL JSON-RPC POSTs go to
	SecretRef       string // optional NAME reference into the secrets store ("" = anonymous)
	AuthHeader      string // header the resolved secret is injected into (default Authorization)
	Status          string // ok | unreachable | disabled
	LastCheckAt     *time.Time
	ProtocolVersion string       // negotiated at the last successful handshake ("" otherwise)
	Tools           []RemoteTool // static tools/list cache (refreshed at registration + re-test)
	CreatedBy       string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// CreateServerInput is the validated-on-arrival payload for Create/Update.
type CreateServerInput struct {
	Name        string
	EndpointURL string
	SecretRef   string
	AuthHeader  string
	// Status optionally forces "disabled" at registration (skips the
	// health check). Any other value is derived from the probe.
	Status string
}

// SecretResolver is the minimal injected seam for resolving secret_ref
// names into values at call time. The method shape matches the platform
// secrets service's Resolve(ctx, orgID, name) exactly (connectors
// precedent), so that service satisfies it WITHOUT an adapter and WITHOUT
// this package importing it.
// SecretResolverFunc adapts a plain function to the SecretResolver seam
// (mirrors connectors.SecretResolverFunc; cmd/api wiring passes the shared
// secrets service's Resolve method).
type SecretResolverFunc func(ctx context.Context, orgID, name string) (string, error)

// Resolve satisfies SecretResolver.
func (f SecretResolverFunc) Resolve(ctx context.Context, orgID, name string) (string, error) {
	return f(ctx, orgID, name)
}

type SecretResolver interface {
	Resolve(ctx context.Context, orgID, name string) (string, error)
}

// Auditor is the narrow audit seam for external tool-call rows. It is
// satisfied structurally by *audit.Service (its LogCtx signature matches),
// so the wiring layer can pass the shared service directly.
type Auditor interface {
	LogCtx(ctx context.Context, actor, action, organizationID, resource string, metadata map[string]any) (*audit.Entry, error)
}

// ServerStore abstracts durable registry storage. Tenant scoping is
// enforced in the store layer too (every statement filters organization_id);
// the service treats the store as untrusted and re-checks isolation on the
// read path.
type ServerStore interface {
	// Create inserts one row; a live (org, name) conflict maps to ErrDuplicate.
	Create(ctx context.Context, s *Server) error
	// Update replaces the mutable fields of one row within one tenant.
	Update(ctx context.Context, s *Server) error
	// Delete hard-deletes one row within one tenant (0 rows -> ErrNotFound).
	Delete(ctx context.Context, orgID, id string) error
	// Get returns one live server within one tenant.
	Get(ctx context.Context, orgID, id string) (*Server, error)
	// List returns all servers of exactly one tenant (name ASC).
	List(ctx context.Context, orgID string) ([]*Server, error)
}

// Registry is the dual-mode MCP server registry.
type Registry struct {
	mu         sync.RWMutex
	items      map[string]map[string]*Server // orgID -> id -> server (memory mode)
	store      ServerStore
	resolver   SecretResolver
	auditor    Auditor
	httpClient *http.Client
	timeout    time.Duration
}

// NewRegistry returns the in-memory registry (zero infrastructure).
func NewRegistry() *Registry {
	return &Registry{
		items:      make(map[string]map[string]*Server),
		httpClient: defaultProbeClient(),
		timeout:    DefaultCheckTimeout,
	}
}

// NewRegistryWithStore returns a Postgres-backed registry. The store is
// mandatory (fail fast: silently degrading a governance surface to memory
// would be a regression); the resolver may be nil, in which case servers
// with a secret_ref fail their probe/calls with ErrSecretResolverRequired
// instead of at construction.
func NewRegistryWithStore(store ServerStore, resolver SecretResolver) (*Registry, error) {
	if store == nil {
		return nil, errors.New("mcp: store is required")
	}
	return &Registry{
		store:      store,
		resolver:   resolver,
		httpClient: defaultProbeClient(),
		timeout:    DefaultCheckTimeout,
	}, nil
}

// SetAuditor wires (or replaces) the tool-call audit seam. Nil-safe.
func (r *Registry) SetAuditor(a Auditor) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.auditor = a
}

// SetSecretResolver wires (or replaces) the secret-ref resolver. Nil-safe.
// (NewRegistryWithStore takes it as an argument; the memory-mode wiring in
// tests and scripts uses this setter.)
func (r *Registry) SetSecretResolver(res SecretResolver) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resolver = res
}

// SetHTTPClient replaces the client used by registration/health probes
// (tests inject short-timeout clients against httptest servers).
func (r *Registry) SetHTTPClient(c *http.Client) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if c == nil {
		c = defaultProbeClient()
	}
	r.httpClient = c
}

// SetCheckTimeout bounds each probe request (tests shrink it). <= 0 restores
// the default.
func (r *Registry) SetCheckTimeout(d time.Duration) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if d <= 0 {
		d = DefaultCheckTimeout
	}
	r.timeout = d
}

func defaultProbeClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			// Proxy intentionally disabled (connectors precedent).
			Proxy: nil,
		},
	}
}

// normalizeServerInput validates CreateServerInput and fills defaults.
func normalizeServerInput(in CreateServerInput) (CreateServerInput, error) {
	out := in
	out.Name = strings.TrimSpace(out.Name)
	if out.Name == "" {
		return out, ErrNameRequired
	}
	if len(out.Name) > 255 {
		return out, ErrNameTooLong
	}
	out.EndpointURL = strings.TrimSpace(out.EndpointURL)
	if out.EndpointURL == "" {
		return out, ErrEndpointRequired
	}
	if !validEndpointURL(out.EndpointURL) {
		return out, ErrEndpointInvalid
	}
	out.SecretRef = strings.TrimSpace(out.SecretRef)
	out.AuthHeader = strings.TrimSpace(out.AuthHeader)
	if out.AuthHeader == "" {
		out.AuthHeader = DefaultAuthHeader
	}
	if !headerNamePattern.MatchString(out.AuthHeader) {
		return out, ErrAuthHeaderInvalid
	}
	out.Status = strings.TrimSpace(strings.ToLower(out.Status))
	switch out.Status {
	case "":
		out.Status = "" // derived from the probe
	case StatusDisabled:
		out.Status = StatusDisabled
	case StatusOK, StatusUnreachable:
		// Requested statuses are probe-derived; treat them as "auto".
		out.Status = ""
	default:
		return out, ErrStatusInvalid
	}
	return out, nil
}

// validEndpointURL enforces an absolute http(s) URL with a host (the
// connectors base_url rule; the outbound client re-parses per request).
func validEndpointURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https")
}

// probe runs the registration/health handshake against srv: initialize
// ping, then tools/list to refresh the static cache. It records the outcome
// (status, last_check_at, protocol_version, tools) ON srv. Probe failures
// are not errors — they ARE the "unreachable" status; hard caller errors
// (tenant guard, store) are handled by the callers.
func (r *Registry) probe(ctx context.Context, orgID string, srv *Server) {
	client := r.clientFor(orgID, srv)
	now := time.Now().UTC()
	srv.LastCheckAt = &now

	init, err := client.Initialize(ctx)
	if err != nil {
		srv.Status = StatusUnreachable
		srv.ProtocolVersion = ""
		srv.Tools = []RemoteTool{}
		return
	}
	srv.ProtocolVersion = init.ProtocolVersion

	cached, err := client.ListTools(ctx)
	if err != nil {
		srv.Status = StatusUnreachable
		srv.Tools = []RemoteTool{}
		return
	}
	srv.Status = StatusOK
	srv.Tools = cached
}

// clientFor builds the outbound client for one server, binding the
// org-scoped secret resolution as the per-request auth header provider.
func (r *Registry) clientFor(orgID string, srv *Server) *Client {
	r.mu.RLock()
	resolver, httpClient, timeout := r.resolver, r.httpClient, r.timeout
	r.mu.RUnlock()

	opts := []ClientOption{WithClientTimeout(timeout), WithClientHTTPClient(httpClient)}
	if ref := srv.SecretRef; ref != "" {
		authHeader := srv.AuthHeader
		opts = append(opts, WithClientAuthHeader(func(ctx context.Context) (string, string, error) {
			if resolver == nil {
				return "", "", ErrSecretResolverRequired
			}
			value, err := resolver.Resolve(ctx, orgID, ref)
			if err != nil {
				return "", "", err
			}
			return authHeader, value, nil
		}))
	}
	return NewClient(srv.EndpointURL, opts...)
}

// CreateServer validates, health-checks (unless disabled) and stores a new
// MCP server within one tenant. An unreachable endpoint aborts the
// registration with ErrServerUnreachable unless force is set — the caller
// (HTTP layer) renders that as 422; ?force=true stores the server as
// "unreachable" (no tools cached) so the operator can fix the endpoint and
// re-test later.
func (r *Registry) CreateServer(ctx context.Context, orgID string, in CreateServerInput, createdBy string, force bool) (*Server, error) {
	if r == nil {
		return nil, errors.New("mcp: registry is nil")
	}
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgRequired
	}
	if strings.TrimSpace(createdBy) == "" {
		return nil, ErrUpdatedByRequired
	}
	norm, err := normalizeServerInput(in)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	srv := &Server{
		ID:             uuid.NewString(),
		OrganizationID: orgID,
		Name:           norm.Name,
		EndpointURL:    norm.EndpointURL,
		SecretRef:      norm.SecretRef,
		AuthHeader:     norm.AuthHeader,
		Status:         StatusUnreachable,
		Tools:          []RemoteTool{},
		CreatedBy:      createdBy,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	if norm.Status == StatusDisabled {
		srv.Status = StatusDisabled // governance switch: no probe at registration
	} else {
		r.probe(ctx, orgID, srv)
		if srv.Status != StatusOK && !force {
			return nil, fmt.Errorf("%w: initialize handshake against %s failed", ErrServerUnreachable, srv.EndpointURL)
		}
	}

	if r.store != nil {
		if err := r.store.Create(ctx, srv); err != nil {
			return nil, err
		}
		return srv, nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	orgItems, ok := r.items[orgID]
	if !ok {
		orgItems = make(map[string]*Server)
		r.items[orgID] = orgItems
	}
	for _, existing := range orgItems {
		if existing != nil && existing.Name == norm.Name {
			return nil, ErrDuplicate
		}
	}
	r.items[orgID][srv.ID] = srv
	return srv, nil
}

// UpdateServer replaces the mutable fields of one server within one tenant
// and re-runs the health check (the endpoint may have changed), refreshing
// the tool cache. Disabled updates skip the probe. Unknown/foreign ids are
// ErrNotFound.
func (r *Registry) UpdateServer(ctx context.Context, orgID, id string, in CreateServerInput, updatedBy string, force bool) (*Server, error) {
	if r == nil {
		return nil, errors.New("mcp: registry is nil")
	}
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgRequired
	}
	if strings.TrimSpace(updatedBy) == "" {
		return nil, ErrUpdatedByRequired
	}
	norm, err := normalizeServerInput(in)
	if err != nil {
		return nil, err
	}
	srv, err := r.GetServer(ctx, orgID, id)
	if err != nil {
		return nil, err
	}

	srv.Name = norm.Name
	srv.EndpointURL = norm.EndpointURL
	srv.SecretRef = norm.SecretRef
	srv.AuthHeader = norm.AuthHeader
	srv.UpdatedAt = time.Now().UTC()

	if norm.Status == StatusDisabled {
		srv.Status = StatusDisabled
		srv.Tools = []RemoteTool{}
		srv.ProtocolVersion = ""
	} else {
		r.probe(ctx, orgID, srv)
		if srv.Status != StatusOK && !force {
			return nil, fmt.Errorf("%w: initialize handshake against %s failed", ErrServerUnreachable, srv.EndpointURL)
		}
	}

	if r.store != nil {
		if err := r.store.Update(ctx, srv); err != nil {
			return nil, err
		}
	}
	return srv, nil
}

// DeleteServer hard-deletes one server within one tenant (the cached tools
// stop being exposed on the next RegisterToolsInto). Unknown or foreign ids
// are ErrNotFound (no existence leak across tenants).
func (r *Registry) DeleteServer(ctx context.Context, orgID, id string) error {
	if r == nil {
		return errors.New("mcp: registry is nil")
	}
	if strings.TrimSpace(orgID) == "" {
		return ErrOrgRequired
	}
	if strings.TrimSpace(id) == "" {
		return ErrNotFound
	}
	if r.store != nil {
		return r.store.Delete(ctx, orgID, id)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	orgItems, ok := r.items[orgID]
	if !ok {
		return ErrNotFound
	}
	if _, ok := orgItems[id]; !ok {
		return ErrNotFound
	}
	delete(orgItems, id)
	return nil
}

// ListServers returns every server of one tenant ordered by name ASC.
func (r *Registry) ListServers(ctx context.Context, orgID string) ([]*Server, error) {
	if r == nil {
		return nil, errors.New("mcp: registry is nil")
	}
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgRequired
	}
	if r.store != nil {
		return r.store.List(ctx, orgID)
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Server, 0)
	for _, rec := range r.items[orgID] {
		if rec == nil {
			continue
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// GetServer returns one live server within one tenant.
func (r *Registry) GetServer(ctx context.Context, orgID, id string) (*Server, error) {
	if r == nil {
		return nil, errors.New("mcp: registry is nil")
	}
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgRequired
	}
	if strings.TrimSpace(id) == "" {
		return nil, ErrNotFound
	}
	if r.store != nil {
		return r.store.Get(ctx, orgID, id)
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	rec, ok := r.items[orgID][id]
	if !ok || rec == nil {
		return nil, ErrNotFound
	}
	return rec, nil
}

// TestServer re-runs the health check for one server (explicit re-test
// endpoint): initialize ping + tools/list refresh, persisting status,
// last_check_at, protocol_version and the refreshed tool cache. Disabled
// servers stay probeable (the check informs the enable decision, connectors
// precedent). The refreshed server is returned.
func (r *Registry) TestServer(ctx context.Context, orgID, id string) (*Server, error) {
	if r == nil {
		return nil, errors.New("mcp: registry is nil")
	}
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgRequired
	}
	srv, err := r.GetServer(ctx, orgID, id)
	if err != nil {
		return nil, err
	}

	r.probe(ctx, orgID, srv)
	srv.UpdatedAt = time.Now().UTC()

	if r.store != nil {
		if err := r.store.Update(ctx, srv); err != nil {
			return nil, err
		}
	}
	return srv, nil
}

// BuildTools returns tool adapters for EVERY exposed tool of one org's
// registered servers (the TENANT GUARD seam): only servers in status "ok"
// of exactly the given org are exposed — disabled and unreachable servers
// contribute no tools, and another org's servers are structurally absent.
// Names are deterministic (mcp_<server>_<tool>, sanitized, deduplicated
// with numeric suffixes on collision).
func (r *Registry) BuildTools(ctx context.Context, orgID string) ([]tools.Tool, error) {
	servers, err := r.ListServers(ctx, orgID)
	if err != nil {
		return nil, err
	}
	r.mu.RLock()
	auditor := r.auditor
	r.mu.RUnlock()

	// Deterministic order: servers by name, then cached tools by name.
	sort.Slice(servers, func(i, j int) bool { return servers[i].Name < servers[j].Name })

	out := make([]tools.Tool, 0)
	used := make(map[string]bool)
	for _, srv := range servers {
		if srv == nil || srv.Status != StatusOK {
			continue // disabled/unreachable servers degrade to "no tools"
		}
		caller := r.callerFor(ctx, orgID, srv, auditor)
		cached := append([]RemoteTool(nil), srv.Tools...)
		sort.Slice(cached, func(i, j int) bool { return cached[i].Name < cached[j].Name })
		for _, rt := range cached {
			name := dedupeName(used, registryToolName(srv.Name, rt.Name))
			out = append(out, tools.NewMCPTool(name, srv.ID, srv.Name, rt.Name, rt.Description, rt.InputSchema, caller))
		}
	}
	return out, nil
}

// RegisterToolsInto registers one org's MCP tool adapters into a tools
// registry (the wiring seam the runtime consumes: the existing
// tools.Registry + the runtime's automatic policy enforcement apply to MCP
// tools exactly like native ones). Registry nil-safe: a nil target is a
// no-op.
func (r *Registry) RegisterToolsInto(ctx context.Context, orgID string, reg *tools.Registry) error {
	if r == nil {
		return errors.New("mcp: registry is nil")
	}
	if strings.TrimSpace(orgID) == "" {
		return ErrOrgRequired
	}
	if reg == nil {
		return nil
	}
	built, err := r.BuildTools(ctx, orgID)
	if err != nil {
		return err
	}
	for _, t := range built {
		reg.Register(t)
	}
	return nil
}

// callerFor wraps the outbound client for one server with the audit seam
// (every external invocation writes one mcp.tool_call row with the server
// ref + remote tool name; arguments are never logged).
func (r *Registry) callerFor(_ context.Context, orgID string, srv *Server, auditor Auditor) tools.MCPToolCaller {
	client := r.clientFor(orgID, srv)
	if auditor == nil {
		return client
	}
	return &auditedCaller{
		inner:      client,
		orgID:      orgID,
		serverID:   srv.ID,
		serverName: srv.Name,
		auditor:    auditor,
	}
}

// auditedCaller decorates a tools.MCPToolCaller with the audit row.
type auditedCaller struct {
	inner      tools.MCPToolCaller
	orgID      string
	serverID   string
	serverName string
	auditor    Auditor
}

// maxAuditErrorBytes bounds the error text stored in audit metadata.
const maxAuditErrorBytes = 512

// CallTool implements tools.MCPToolCaller (audit is best-effort: it never
// alters the tool outcome).
func (c *auditedCaller) CallTool(ctx context.Context, toolName string, args map[string]any) (map[string]any, error) {
	res, err := c.inner.CallTool(ctx, toolName, args)
	if c.auditor != nil {
		metadata := map[string]any{
			"channel":   "mcp-client",
			"server":    c.serverName,
			"server_id": c.serverID,
			"tool":      toolName,
			"ok":        err == nil,
		}
		if err != nil {
			metadata["error"] = truncateString(err.Error(), maxAuditErrorBytes)
		}
		_, _ = c.auditor.LogCtx(ctx, "runtime", "mcp.tool_call", c.orgID, "mcp_servers/"+c.serverID, metadata)
	}
	return res, err
}

// truncateString caps s at max bytes (plain byte cut is fine for audit
// metadata; the text is diagnostic only).
func truncateString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "...[truncated]"
}

// dedupeName returns name, or name_2 / name_3 ... when the name was already
// handed out (deterministic: no map-iteration randomness feeds the suffix).
func dedupeName(used map[string]bool, name string) string {
	if !used[name] {
		used[name] = true
		return name
	}
	for n := 2; ; n++ {
		candidate := fmt.Sprintf("%s_%d", name, n)
		if !used[candidate] {
			used[candidate] = true
			return candidate
		}
	}
}

// registryToolName renders the agent-facing tool name for one remote tool:
// `mcp_<server>_<tool>`, sanitized to [a-z0-9_] (the platform's tool-name
// convention — no colons/unicode in prompt-facing names).
func registryToolName(serverName, toolName string) string {
	serverSeg := sanitizeSegment(serverName, 48)
	toolSeg := sanitizeSegment(toolName, 48)
	if serverSeg == "" {
		serverSeg = "server"
	}
	if toolSeg == "" {
		toolSeg = "tool"
	}
	name := "mcp_" + serverSeg + "_" + toolSeg
	if len(name) > 120 {
		name = name[:120]
	}
	return strings.TrimRight(name, "_")
}

// sanitizeSegment lowercases s, replaces every run of non [a-z0-9]
// characters with a single "_", trims separators and caps the length.
func sanitizeSegment(s string, max int) string {
	var b strings.Builder
	lastSep := true // leading separators dropped
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			lastSep = false
		default:
			if !lastSep && unicode.IsPrint(r) {
				b.WriteByte('_')
				lastSep = true
			}
		}
		if b.Len() >= max {
			break
		}
	}
	out := strings.Trim(b.String(), "_")
	if len(out) > max {
		out = out[:max]
	}
	return strings.TrimRight(out, "_")
}
