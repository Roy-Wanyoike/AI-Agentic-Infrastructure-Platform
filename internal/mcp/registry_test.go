package mcp

// registry_test.go — the org-scoped MCP server registry (issue #82):
// registration health-check + static tools cache, force semantics, the
// re-test refresh, tenant guards, tool building (naming/dedupe/status
// gating), secret-backed auth injection, the tool-call audit seam and the
// Postgres store (sqlmock).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"agentos/internal/audit"
	"agentos/internal/secrets"
	"agentos/internal/tools"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
)

// Compile-time structural-adapter proofs (connectors precedent): the
// platform secrets service satisfies the registry's SecretResolver and the
// platform audit service satisfies its Auditor WITHOUT wrappers, so the
// orchestrator wiring can pass both directly.
var (
	_ SecretResolver = (*secrets.Service)(nil)
	_ Auditor        = (*audit.Service)(nil)
)

// recordingAuditor collects the mcp.tool_call rows for assertions.
type recordingAuditor struct {
	mu      sync.Mutex
	entries []*audit.Entry
}

func (r *recordingAuditor) LogCtx(_ context.Context, actor, action, organizationID, resource string, metadata map[string]any) (*audit.Entry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, &audit.Entry{
		ID:             fmt.Sprintf("entry-%d", len(r.entries)+1),
		Actor:          actor,
		Action:         action,
		OrganizationID: organizationID,
		Resource:       resource,
		Metadata:       metadata,
	})
	return r.entries[len(r.entries)-1], nil
}

func (r *recordingAuditor) all() []*audit.Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*audit.Entry(nil), r.entries...)
}

// weatherTools is the fixture's remote catalog.
func weatherTools() []RemoteTool {
	return []RemoteTool{
		{
			Name:        "get_weather",
			Description: "Returns the weather for a city",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"city": map[string]any{"type": "string"}},
				"required":   []string{"city"},
			},
		},
		{Name: "ping"},
	}
}

// weatherToolsJSON is the deterministic JSON encoding of weatherTools()
// (encoding/json sorts map keys; struct field order is fixed).
func weatherToolsJSON() string {
	b, err := json.Marshal(weatherTools())
	if err != nil {
		panic(err)
	}
	return string(b)
}

func newWeatherFixture() *mcpFixture {
	return newMCPFixture(func(f *mcpFixture) { f.tools = weatherTools() })
}

func TestRegistryCreateServerHealthChecksAndCachesTools(t *testing.T) {
	f := newWeatherFixture()
	defer f.close()
	reg := NewRegistry()

	srv, err := reg.CreateServer(context.Background(), "org-a", CreateServerInput{
		Name:        "Weather MCP",
		EndpointURL: f.srv.URL,
	}, "user-1", false)
	if err != nil {
		t.Fatalf("CreateServer failed: %v", err)
	}
	if srv.Status != StatusOK {
		t.Fatalf("status = %q, want ok", srv.Status)
	}
	if srv.ProtocolVersion != ProtocolVersion {
		t.Fatalf("protocol version not recorded: %q", srv.ProtocolVersion)
	}
	if len(srv.Tools) != 2 || srv.Tools[0].Name != "get_weather" || srv.Tools[1].Name != "ping" {
		t.Fatalf("static tools cache wrong: %+v", srv.Tools)
	}
	if srv.LastCheckAt == nil {
		t.Fatalf("last_check_at must be stamped")
	}
	if srv.AuthHeader != DefaultAuthHeader {
		t.Fatalf("auth_header default: %q", srv.AuthHeader)
	}
	if got := f.count(); got != 2 {
		t.Fatalf("registration must run initialize + tools/list, got %d requests", got)
	}
}

func TestRegistryCreateServerUnreachableRejectedUnlessForced(t *testing.T) {
	reg := NewRegistry()
	ctx := context.Background()

	// Dead endpoint (nothing listens).
	_, err := reg.CreateServer(ctx, "org-a", CreateServerInput{
		Name: "dead", EndpointURL: "http://127.0.0.1:1/mcp",
	}, "user-1", false)
	if !errors.Is(err, ErrServerUnreachable) {
		t.Fatalf("expected ErrServerUnreachable, got %v", err)
	}

	// force=true stores the server as unreachable with an empty cache.
	srv, err := reg.CreateServer(ctx, "org-a", CreateServerInput{
		Name: "dead", EndpointURL: "http://127.0.0.1:1/mcp",
	}, "user-1", true)
	if err != nil {
		t.Fatalf("forced registration failed: %v", err)
	}
	if srv.Status != StatusUnreachable {
		t.Fatalf("forced status = %q, want unreachable", srv.Status)
	}
	if len(srv.Tools) != 0 {
		t.Fatalf("an unreachable server must cache no tools: %+v", srv.Tools)
	}
}

func TestRegistryCreateServerDisabledSkipsProbe(t *testing.T) {
	f := newMCPFixture(nil) // even a live server: disabled must NOT probe
	defer f.close()
	reg := NewRegistry()

	srv, err := reg.CreateServer(context.Background(), "org-a", CreateServerInput{
		Name: "off", EndpointURL: f.srv.URL, Status: StatusDisabled,
	}, "user-1", false)
	if err != nil {
		t.Fatalf("CreateServer failed: %v", err)
	}
	if srv.Status != StatusDisabled || srv.LastCheckAt != nil || len(srv.Tools) != 0 {
		t.Fatalf("disabled registration must skip the probe: %+v", srv)
	}
	if f.count() != 0 {
		t.Fatalf("disabled registration must not touch the wire, got %d requests", f.count())
	}
}

func TestRegistryCreateServerValidation(t *testing.T) {
	f := newWeatherFixture()
	defer f.close()
	reg := NewRegistry()
	ctx := context.Background()

	cases := []struct {
		name string
		in   CreateServerInput
		want error
	}{
		{"blank name", CreateServerInput{EndpointURL: f.srv.URL}, ErrNameRequired},
		{"long name", CreateServerInput{Name: strings.Repeat("x", 256), EndpointURL: f.srv.URL}, ErrNameTooLong},
		{"blank endpoint", CreateServerInput{Name: "x"}, ErrEndpointRequired},
		{"bad scheme", CreateServerInput{Name: "x", EndpointURL: "ftp://host/mcp"}, ErrEndpointInvalid},
		{"hostless", CreateServerInput{Name: "x", EndpointURL: "http://"}, ErrEndpointInvalid},
		{"bad auth header", CreateServerInput{Name: "x", EndpointURL: f.srv.URL, AuthHeader: "Bad Header"}, ErrAuthHeaderInvalid},
		{"bad status", CreateServerInput{Name: "x", EndpointURL: f.srv.URL, Status: "detached"}, ErrStatusInvalid},
	}
	for _, tc := range cases {
		if _, err := reg.CreateServer(ctx, "org-a", tc.in, "user-1", false); !errors.Is(err, tc.want) {
			t.Fatalf("%s: expected %v, got %v", tc.name, tc.want, err)
		}
	}
	if _, err := reg.CreateServer(ctx, " ", CreateServerInput{Name: "x", EndpointURL: f.srv.URL}, "u", false); !errors.Is(err, ErrOrgRequired) {
		t.Fatalf("blank org must be ErrOrgRequired, got %v", err)
	}
	if _, err := reg.CreateServer(ctx, "org-a", CreateServerInput{Name: "x", EndpointURL: f.srv.URL}, " ", false); !errors.Is(err, ErrUpdatedByRequired) {
		t.Fatalf("blank actor must be ErrUpdatedByRequired, got %v", err)
	}
}

func TestRegistryCreateServerDuplicateName(t *testing.T) {
	f := newWeatherFixture()
	defer f.close()
	reg := NewRegistry()
	ctx := context.Background()

	if _, err := reg.CreateServer(ctx, "org-a", CreateServerInput{Name: "dup", EndpointURL: f.srv.URL}, "u", false); err != nil {
		t.Fatalf("first create failed: %v", err)
	}
	if _, err := reg.CreateServer(ctx, "org-a", CreateServerInput{Name: "dup", EndpointURL: f.srv.URL}, "u", false); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("expected ErrDuplicate, got %v", err)
	}
	// Same name in another org is fine (org scoping).
	if _, err := reg.CreateServer(ctx, "org-b", CreateServerInput{Name: "dup", EndpointURL: f.srv.URL}, "u", false); err != nil {
		t.Fatalf("same name in another org must succeed: %v", err)
	}
}

func TestRegistryTestServerRefreshesCacheAndStatus(t *testing.T) {
	f := newWeatherFixture()
	defer f.close()
	reg := NewRegistry()
	ctx := context.Background()

	srv, err := reg.CreateServer(ctx, "org-a", CreateServerInput{Name: "w", EndpointURL: f.srv.URL}, "u", false)
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// The remote adds a tool, then the re-test refreshes the cache.
	f.mutate(func(f *mcpFixture) {
		f.tools = append(weatherTools(), RemoteTool{Name: "get_forecast", Description: "5-day forecast"})
	})
	refreshed, err := reg.TestServer(ctx, "org-a", srv.ID)
	if err != nil {
		t.Fatalf("TestServer failed: %v", err)
	}
	if refreshed.Status != StatusOK || len(refreshed.Tools) != 3 {
		t.Fatalf("re-test must refresh the cache: status=%s tools=%d", refreshed.Status, len(refreshed.Tools))
	}

	// After a later failure the re-test records unreachable and empties the
	// cache (the remote goes away).
	f.close()
	refreshed, err = reg.TestServer(ctx, "org-a", srv.ID)
	if err != nil {
		t.Fatalf("TestServer over a dead remote must still update the row: %v", err)
	}
	if refreshed.Status != StatusUnreachable || len(refreshed.Tools) != 0 {
		t.Fatalf("dead re-test must be unreachable with an empty cache: %+v", refreshed)
	}
}

func TestRegistryUpdateServerReprobes(t *testing.T) {
	f1 := newWeatherFixture()
	defer f1.close()
	f2 := newWeatherFixture()
	defer f2.close()
	reg := NewRegistry()
	ctx := context.Background()

	srv, err := reg.CreateServer(ctx, "org-a", CreateServerInput{Name: "w", EndpointURL: f1.srv.URL}, "u", false)
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	updated, err := reg.UpdateServer(ctx, "org-a", srv.ID, CreateServerInput{
		Name: "w2", EndpointURL: f2.srv.URL,
	}, "u2", false)
	if err != nil {
		t.Fatalf("update failed: %v", err)
	}
	if updated.Name != "w2" || updated.EndpointURL != f2.srv.URL || updated.Status != StatusOK {
		t.Fatalf("update mismatch: %+v", updated)
	}
	if len(f2.recorded()) < 2 {
		t.Fatalf("update must re-run the handshake against the new endpoint")
	}
	if updated.CreatedBy != "u" {
		t.Fatalf("update must preserve the creator: %q", updated.CreatedBy)
	}

	// Unreachable update is refused unless forced (dead endpoint).
	if _, err := reg.UpdateServer(ctx, "org-a", srv.ID, CreateServerInput{
		Name: "w3", EndpointURL: "http://127.0.0.1:1/mcp",
	}, "u", false); !errors.Is(err, ErrServerUnreachable) {
		t.Fatalf("unreachable update must be refused, got %v", err)
	}
}

func TestRegistryTenantGuard(t *testing.T) {
	f := newWeatherFixture()
	defer f.close()
	reg := NewRegistry()
	ctx := context.Background()

	srv, err := reg.CreateServer(ctx, "org-a", CreateServerInput{Name: "secret-srv", EndpointURL: f.srv.URL}, "u", false)
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// Org B sees nothing: get/list/update/delete/test all 404-equivalent.
	if _, err := reg.GetServer(ctx, "org-b", srv.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-org get must be ErrNotFound, got %v", err)
	}
	list, err := reg.ListServers(ctx, "org-b")
	if err != nil || len(list) != 0 {
		t.Fatalf("cross-org list must be empty: %v", err)
	}
	if _, err := reg.UpdateServer(ctx, "org-b", srv.ID, CreateServerInput{Name: "x", EndpointURL: f.srv.URL}, "u", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-org update must be ErrNotFound, got %v", err)
	}
	if err := reg.DeleteServer(ctx, "org-b", srv.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-org delete must be ErrNotFound, got %v", err)
	}
	if _, err := reg.TestServer(ctx, "org-b", srv.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-org re-test must be ErrNotFound, got %v", err)
	}

	// The tool build for org B never exposes org A's servers (THE tenant
	// guard on the agent-facing surface).
	regA, regB := tools.NewRegistry(), tools.NewRegistry()
	if err := reg.RegisterToolsInto(ctx, "org-a", regA); err != nil {
		t.Fatalf("RegisterToolsInto(org-a) failed: %v", err)
	}
	if err := reg.RegisterToolsInto(ctx, "org-b", regB); err != nil {
		t.Fatalf("RegisterToolsInto(org-b) failed: %v", err)
	}
	if len(regB.Names()) != 0 {
		t.Fatalf("org B must not see org A's tools: %v", regB.Names())
	}
	if len(regA.Names()) != 2 {
		t.Fatalf("org A must see its two cached tools: %v", regA.Names())
	}

	// Org A can delete its own server.
	if err := reg.DeleteServer(ctx, "org-a", srv.ID); err != nil {
		t.Fatalf("own-org delete failed: %v", err)
	}
	if _, err := reg.GetServer(ctx, "org-a", srv.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted server must be gone, got %v", err)
	}
}

func TestRegistryBuildToolsNamingAndStatusGating(t *testing.T) {
	f := newWeatherFixture()
	defer f.close()
	reg := NewRegistry()
	ctx := context.Background()

	// Two servers whose names sanitize identically, plus a disabled one.
	if _, err := reg.CreateServer(ctx, "org-a", CreateServerInput{Name: "Acme CRM", EndpointURL: f.srv.URL}, "u", false); err != nil {
		t.Fatalf("create 1 failed: %v", err)
	}
	if _, err := reg.CreateServer(ctx, "org-a", CreateServerInput{Name: "acme-crm", EndpointURL: f.srv.URL}, "u", false); err != nil {
		t.Fatalf("create 2 failed: %v", err)
	}
	if _, err := reg.CreateServer(ctx, "org-a", CreateServerInput{Name: "off", EndpointURL: f.srv.URL, Status: StatusDisabled}, "u", false); err != nil {
		t.Fatalf("create 3 failed: %v", err)
	}
	if _, err := reg.CreateServer(ctx, "org-a", CreateServerInput{Name: "dead", EndpointURL: "http://127.0.0.1:1/mcp"}, "u", true); err != nil {
		t.Fatalf("create 4 failed: %v", err)
	}

	built, err := reg.BuildTools(ctx, "org-a")
	if err != nil {
		t.Fatalf("BuildTools failed: %v", err)
	}
	names := make([]string, 0, len(built))
	for _, tool := range built {
		names = append(names, tool.Name())
	}
	// Deterministic (servers and tools sorted): the second identical server
	// dedupes with _2 suffixes; disabled/unreachable contribute nothing.
	want := []string{
		"mcp_acme_crm_get_weather",
		"mcp_acme_crm_ping",
		"mcp_acme_crm_get_weather_2",
		"mcp_acme_crm_ping_2",
	}
	if len(names) != len(want) {
		t.Fatalf("built names = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("built names = %v, want %v", names, want)
		}
	}
	// Names are registry-safe ([a-z0-9_] only).
	for _, n := range names {
		for _, r := range n {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
				t.Fatalf("unsafe tool name %q", n)
			}
		}
	}
}

func TestRegistryToolInvocationWithSecretAuthAndAudit(t *testing.T) {
	f := newMCPFixture(func(f *mcpFixture) {
		f.tools = weatherTools()
		f.callResult = callToolResult{Content: []textContent{{Type: "text", Text: "22C and sunny"}}}
	})
	defer f.close()

	// Org-scoped secret (the platform secrets service itself, in-memory).
	secSvc := secrets.NewService()
	if _, err := secSvc.Create(context.Background(), "org-a", "WEATHER_API_KEY", "Bearer tok-weather", "user-1"); err != nil {
		t.Fatalf("secret create failed: %v", err)
	}

	reg := NewRegistry()
	reg.resolver = secSvc
	aud := &recordingAuditor{}
	reg.SetAuditor(aud)

	srv, err := reg.CreateServer(context.Background(), "org-a", CreateServerInput{
		Name:        "Weather MCP",
		EndpointURL: f.srv.URL,
		SecretRef:   "WEATHER_API_KEY",
		AuthHeader:  "Authorization",
	}, "user-1", false)
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	if got := f.header("Authorization"); got != "Bearer tok-weather" {
		t.Fatalf("registration probes must carry the resolved secret, got %q", got)
	}

	// The agent-facing tool executes through the platform Tool contract.
	built, err := reg.BuildTools(context.Background(), "org-a")
	if err != nil {
		t.Fatalf("BuildTools failed: %v", err)
	}
	if len(built) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(built))
	}
	var weatherTool tools.Tool
	for _, tool := range built {
		if tool.Name() == "mcp_weather_mcp_get_weather" {
			weatherTool = tool
		}
	}
	if weatherTool == nil {
		t.Fatalf("weather tool missing among %v", built)
	}
	out, err := weatherTool.Execute(map[string]any{"city": "Berlin"})
	if err != nil {
		t.Fatalf("tool execution failed: %v", err)
	}
	if out["content"] != "22C and sunny" {
		t.Fatalf("result mapping failed: %v", out)
	}
	if got := f.header("Authorization"); got != "Bearer tok-weather" {
		t.Fatalf("call-time auth header wrong: %q", got)
	}

	// The audit row: server ref + remote tool name, ok:true, no arguments.
	entries := aud.all()
	if len(entries) != 1 {
		t.Fatalf("expected exactly one mcp.tool_call row, got %d", len(entries))
	}
	e := entries[0]
	if e.Action != "mcp.tool_call" || e.Actor != "runtime" || e.OrganizationID != "org-a" {
		t.Fatalf("audit identity wrong: %+v", e)
	}
	if e.Resource != "mcp_servers/"+srv.ID {
		t.Fatalf("audit resource = %q, want the server ref", e.Resource)
	}
	if e.Metadata["tool"] != "get_weather" || e.Metadata["server"] != "Weather MCP" ||
		e.Metadata["server_id"] != srv.ID || e.Metadata["ok"] != true {
		t.Fatalf("audit metadata wrong: %+v", e.Metadata)
	}
	if _, has := e.Metadata["arguments"]; has {
		t.Fatalf("arguments must never be audited: %+v", e.Metadata)
	}
}

func TestRegistryToolInvocationFailureAuditedAndSurfaces(t *testing.T) {
	f := newWeatherFixture()
	defer f.close()
	reg := NewRegistry()
	aud := &recordingAuditor{}
	reg.SetAuditor(aud)

	srv, err := reg.CreateServer(context.Background(), "org-a", CreateServerInput{
		Name: "Weather MCP", EndpointURL: f.srv.URL,
	}, "user-1", false)
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// The remote dies after registration: the tool is still registered
	// (status ok) but every call fails with a visible error.
	f.close()
	built, err := reg.BuildTools(context.Background(), "org-a")
	if err != nil || len(built) == 0 {
		t.Fatalf("tools must still be built for an ok server: %v (%d)", err, len(built))
	}
	_, callErr := built[0].Execute(map[string]any{"city": "Berlin"})
	if callErr == nil {
		t.Fatalf("a dead remote must surface a tool error")
	}

	entries := aud.all()
	if len(entries) != 1 || entries[0].Metadata["ok"] != false {
		t.Fatalf("the failed call must be audited with ok:false: %+v", entries)
	}
	if msg, _ := entries[0].Metadata["error"].(string); msg == "" {
		t.Fatalf("the audit row must carry the failure text")
	}
	if entries[0].Resource != "mcp_servers/"+srv.ID {
		t.Fatalf("the failed-call audit must carry the server ref: %q", entries[0].Resource)
	}
}

func TestRegistryToolInvocationRemoteIsError(t *testing.T) {
	f := newMCPFixture(func(f *mcpFixture) {
		f.tools = weatherTools()
		f.callResult = callToolResult{
			Content: []textContent{{Type: "text", Text: "city not found"}},
			IsError: true,
		}
	})
	defer f.close()
	reg := NewRegistry()
	if _, err := reg.CreateServer(context.Background(), "org-a", CreateServerInput{
		Name: "W", EndpointURL: f.srv.URL,
	}, "u", false); err != nil {
		t.Fatalf("create failed: %v", err)
	}
	built, err := reg.BuildTools(context.Background(), "org-a")
	if err != nil || len(built) == 0 {
		t.Fatalf("BuildTools failed: %v", err)
	}
	_, callErr := built[0].Execute(nil)
	var toolErr *ToolCallError
	if !errors.As(callErr, &toolErr) {
		t.Fatalf("the remote isError result must surface as *ToolCallError, got %v", callErr)
	}
}

func TestRegistrySecretResolutionFailureSurfacesCleanly(t *testing.T) {
	f := newWeatherFixture()
	defer f.close()

	secSvc := secrets.NewService() // no WEATHER_API_KEY created
	reg := NewRegistry()
	reg.resolver = secSvc

	// Registration probes WITH a configured secret ref fail resolution ->
	// the server is unreachable (or forced in).
	_, err := reg.CreateServer(context.Background(), "org-a", CreateServerInput{
		Name: "W", EndpointURL: f.srv.URL, SecretRef: "WEATHER_API_KEY",
	}, "u", false)
	if !errors.Is(err, ErrServerUnreachable) {
		t.Fatalf("unresolvable secret at registration must be unreachable, got %v", err)
	}
	if f.count() != 0 {
		t.Fatalf("a failed auth resolution must never hit the wire, got %d requests", f.count())
	}

	// No resolver wired at all: same graceful degradation.
	reg2 := NewRegistry()
	if _, err := reg2.CreateServer(context.Background(), "org-a", CreateServerInput{
		Name: "W", EndpointURL: f.srv.URL, SecretRef: "ANY",
	}, "u", true); err != nil {
		t.Fatalf("forced registration failed: %v", err)
	}
	built, err := reg2.BuildTools(context.Background(), "org-a")
	if err != nil || len(built) != 0 {
		t.Fatalf("an unreachable server must contribute no tools: %v (%d)", err, len(built))
	}
}

// ---------------------------------------------------------------------------
// Postgres mode (sqlmock, routed through the Registry)
// ---------------------------------------------------------------------------

func newMockRegistry(t *testing.T) (*Registry, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	reg, err := NewRegistryWithStore(NewPostgresStore(db), nil)
	if err != nil {
		t.Fatalf("NewRegistryWithStore failed: %v", err)
	}
	return reg, mock, func() { _ = db.Close() }
}

func TestRegistryStoreCreateAndRoundTrip(t *testing.T) {
	f := newWeatherFixture()
	defer f.close()
	reg, mock, close := newMockRegistry(t)
	defer close()
	ctx := context.Background()

	// Registration probes the live fixture, then INSERTs with the probe
	// results (tools_cache is the deterministic JSON of the cache).
	mock.ExpectExec(`INSERT INTO mcp_servers`).
		WithArgs(sqlmock.AnyArg(), "org-a", "Weather MCP", f.srv.URL, "",
			"Authorization", StatusOK, sqlmock.AnyArg(), ProtocolVersion,
			`[{"name":"get_weather","description":"Returns the weather for a city","inputSchema":{"properties":{"city":{"type":"string"}},"required":["city"],"type":"object"}},{"name":"ping"}]`,
			"user-1", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	srv, err := reg.CreateServer(ctx, "org-a", CreateServerInput{
		Name: "Weather MCP", EndpointURL: f.srv.URL,
	}, "user-1", false)
	if err != nil {
		t.Fatalf("CreateServer over store failed: %v", err)
	}

	// Get: scoped SELECT round-trips the JSONB cache.
	mock.ExpectQuery(`SELECT id, organization_id, name, endpoint_url,`).
		WithArgs(srv.ID, "org-a").
		WillReturnRows(serverRow(srv))
	got, err := reg.GetServer(ctx, "org-a", srv.ID)
	if err != nil {
		t.Fatalf("GetServer over store failed: %v", err)
	}
	if got.Status != StatusOK || got.ProtocolVersion != ProtocolVersion || len(got.Tools) != 2 {
		t.Fatalf("cache round-trip failed: %+v", got)
	}
	if got.Tools[0].Name != "get_weather" || got.Tools[0].InputSchema == nil {
		t.Fatalf("tool cache round-trip failed: %+v", got.Tools)
	}

	// No rows -> ErrNotFound (no existence leak).
	mock.ExpectQuery(`SELECT id, organization_id, name, endpoint_url,`).
		WithArgs("other", "org-a").
		WillReturnError(sql.ErrNoRows)
	if _, err := reg.GetServer(ctx, "org-a", "other"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no-rows Get must be ErrNotFound, got %v", err)
	}

	// List: org-scoped.
	mock.ExpectQuery(`SELECT id, organization_id, name, endpoint_url,`).
		WithArgs("org-a").
		WillReturnRows(serverRow(srv))
	list, err := reg.ListServers(ctx, "org-a")
	if err != nil || len(list) != 1 {
		t.Fatalf("List over store failed: %v (%d)", err, len(list))
	}
}

// serverRow builds the sqlmock row for one server (column order of the
// SELECTs in store.go).
func serverRow(s *Server) *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "organization_id", "name", "endpoint_url",
		"secret_ref", "auth_header", "status",
		"last_check_at", "protocol_version", "tools_cache",
		"created_by", "created_at", "updated_at",
	}).AddRow(s.ID, s.OrganizationID, s.Name, s.EndpointURL,
		s.SecretRef, s.AuthHeader, s.Status,
		s.LastCheckAt, s.ProtocolVersion, toolsCacheJSON(s.Tools),
		s.CreatedBy, s.CreatedAt, s.UpdatedAt)
}

func TestRegistryStoreDuplicateMapsToErrDuplicate(t *testing.T) {
	f := newWeatherFixture()
	defer f.close()
	reg, mock, close := newMockRegistry(t)
	defer close()

	mock.ExpectExec(`INSERT INTO mcp_servers`).
		WillReturnError(&pq.Error{Code: "23505"})
	if _, err := reg.CreateServer(context.Background(), "org-a", CreateServerInput{
		Name: "dup", EndpointURL: f.srv.URL,
	}, "u", false); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("SQLSTATE 23505 must map to ErrDuplicate, got %v", err)
	}
}

func TestRegistryStoreUpdateAndDelete(t *testing.T) {
	f := newWeatherFixture()
	defer f.close()
	reg, mock, close := newMockRegistry(t)
	defer close()
	ctx := context.Background()

	// Seed the row the Update flow loads first (Get-then-UPDATE).
	seeded := &Server{
		ID: "srv-1", OrganizationID: "org-a", Name: "w", EndpointURL: f.srv.URL,
		AuthHeader: DefaultAuthHeader, Status: StatusOK,
		ProtocolVersion: ProtocolVersion, Tools: []RemoteTool{},
		CreatedBy: "u", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	mock.ExpectQuery(`SELECT id, organization_id, name, endpoint_url,`).
		WithArgs("srv-1", "org-a").
		WillReturnRows(serverRow(seeded))
	mock.ExpectExec(`UPDATE mcp_servers SET`).
		WithArgs("srv-1", "org-a", "w2", f.srv.URL, "", "Authorization",
			StatusOK, sqlmock.AnyArg(), ProtocolVersion, weatherToolsJSON(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	updated, err := reg.UpdateServer(ctx, "org-a", "srv-1", CreateServerInput{
		Name: "w2", EndpointURL: f.srv.URL,
	}, "u2", false)
	if err != nil {
		t.Fatalf("UpdateServer over store failed: %v", err)
	}
	if updated.Name != "w2" || updated.CreatedBy != "u" {
		t.Fatalf("update mismatch: %+v", updated)
	}

	// Delete: 0 affected rows -> ErrNotFound.
	mock.ExpectExec(`DELETE FROM mcp_servers`).
		WithArgs("srv-x", "org-a").
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := reg.DeleteServer(ctx, "org-a", "srv-x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("0-row delete must be ErrNotFound, got %v", err)
	}
}
