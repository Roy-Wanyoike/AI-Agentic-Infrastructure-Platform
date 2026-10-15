package agents

// Issue #122 tests: agent export / import / clone portability.
//
// Coverage:
//   - export: bundle shape (schema_version "1"), configuration-only payload
//     (ids/org/status/timestamps/credentials structurally absent), sorted
//     deterministic field order, tenant guard;
//   - import: typed validation table (every *PortabilityError code, wrapping
//     ErrInvalidBundle) with NO partial writes, fresh identity (new id,
//     caller's org, fresh created_at, status DRAFT, fresh v1 version) and the
//     never-trust-client guarantee (extra identity keys in the payload are
//     dropped — org/status/created_at can never be smuggled in);
//   - clone: deep copy within the same org (new id, "-copy" suffix, status
//     reset DRAFT, fresh v1) with the source untouched, cross-org clone
//     rejected as not-found (no existence leak);
//   - store mode (sqlmock): the durable export (org-scoped SELECT, no
//     writes), import and clone INSERTs binding the caller's org and the
//     reset DRAFT status.

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// portabilityFixture seeds one agent in orgID and returns the service and id.
func portabilityFixture(t *testing.T, orgID string) (*Service, *Agent) {
	t.Helper()
	svc := NewService()
	agent, err := svc.CreateAgentCtx(t.Context(), orgID, "Support Agent", "front-line helper", "be helpful", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("seed agent failed: %v", err)
	}
	return svc, agent
}

func TestExportBundleShapeIsConfigOnly(t *testing.T) {
	svc, agent := portabilityFixture(t, "org-export")
	bundle, err := svc.ExportAgent(t.Context(), "org-export", agent.ID)
	if err != nil {
		t.Fatalf("export failed: %v", err)
	}
	if bundle.SchemaVersion != BundleSchemaVersion || BundleSchemaVersion != "1" {
		t.Fatalf("schema_version = %q, want %q", bundle.SchemaVersion, "1")
	}
	if bundle.Agent.Name != "Support Agent" || bundle.Agent.Description != "front-line helper" ||
		bundle.Agent.Instructions != "be helpful" || bundle.Agent.Model != "gpt-4o-mini" {
		t.Fatalf("bundle config mismatch: %+v", bundle.Agent)
	}
	if bundle.Agent.Tools == nil || len(bundle.Agent.Tools) != 0 {
		t.Fatalf("tools must be an empty non-nil list today, got %v", bundle.Agent.Tools)
	}
	if _, err := time.Parse(time.RFC3339, bundle.ExportedAt); err != nil {
		t.Fatalf("exported_at must be RFC3339, got %q: %v", bundle.ExportedAt, err)
	}

	// Deterministic field order: two marshals are byte-identical and the keys
	// appear in declaration order (schema_version, exported_at, agent{name,
	// description, instructions, model, tools}).
	raw, err := json.Marshal(bundle)
	if err != nil {
		t.Fatalf("marshal bundle failed: %v", err)
	}
	again, _ := json.Marshal(bundle)
	if string(raw) != string(again) {
		t.Fatal("bundle marshaling must be deterministic")
	}
	s := string(raw)
	if !strings.HasPrefix(s, `{"schema_version":"1","exported_at":"`) {
		t.Fatalf("unexpected bundle prefix (field order drift): %s", s)
	}
	order := []string{`"agent":{"name":`, `"description":`, `"instructions":`, `"model":`, `"tools":`}
	last := -1
	for _, key := range order {
		idx := strings.Index(s, key)
		if idx <= last {
			t.Fatalf("field order drift: %s at %d after %d in %s", key, idx, last, s)
		}
		last = idx
	}
}

func TestExportOmitsIdentityAndSecretMaterial(t *testing.T) {
	svc, agent := portabilityFixture(t, "org-secret-hunt")
	bundle, err := svc.ExportAgent(t.Context(), "org-secret-hunt", agent.ID)
	if err != nil {
		t.Fatalf("export failed: %v", err)
	}
	raw, _ := json.Marshal(bundle)
	s := string(raw)
	// Identity and bookkeeping are structurally absent...
	for _, forbidden := range []string{
		agent.ID,                // agent id
		agent.CurrentVersionID,  // version id
		"org-secret-hunt",       // tenant id
		`"organization`,         // any org-shaped key
		`"id"`,                  // any id key
		`"created_at"`,          // timestamps (fresh on import)
		`"status"`,              // runtime lifecycle state (reset on import)
		agent.Status,            // the value too ("DRAFT")
		`"api_key"`, `"secret"`, // credential shapes
	} {
		if strings.Contains(s, forbidden) {
			t.Fatalf("exported bundle leaked %q: %s", forbidden, s)
		}
	}
	// ...and the only tool-shaped values are registry NAMES.
	for _, tool := range bundle.Agent.Tools {
		if strings.Contains(tool, "://") || strings.Contains(tool, "sk_") {
			t.Fatalf("tools must carry registry names, got %q", tool)
		}
	}
}

func TestExportTenantGuard(t *testing.T) {
	svc, agent := portabilityFixture(t, "org-owner")
	cases := []struct{ name, orgID, agentID string }{
		{"cross-org", "org-foreign", agent.ID},
		{"unknown agent", "org-owner", "agent-does-not-exist"},
		{"blank org", "", agent.ID},
		{"blank agent", "org-owner", ""},
	}
	for _, tc := range cases {
		if _, err := svc.ExportAgent(t.Context(), tc.orgID, tc.agentID); !errors.Is(err, ErrAgentNotFound) {
			t.Fatalf("%s: expected ErrAgentNotFound, got %v", tc.name, err)
		}
	}
}

func TestImportCreatesFreshAgentInCallerOrg(t *testing.T) {
	svc, source := portabilityFixture(t, "org-source")
	bundle, err := svc.ExportAgent(t.Context(), "org-source", source.ID)
	if err != nil {
		t.Fatalf("export failed: %v", err)
	}
	bundle.Agent.Tools = []string{"calculator", "http_request"}

	before := time.Now().UTC().Add(-time.Minute)
	imported, err := svc.ImportAgent(t.Context(), "org-dest", bundle)
	if err != nil {
		t.Fatalf("import failed: %v", err)
	}
	if imported.OrganizationID != "org-dest" {
		t.Fatalf("imported org = %q, want org-dest (caller tenant)", imported.OrganizationID)
	}
	if imported.ID == "" || imported.ID == source.ID {
		t.Fatalf("imported id must be fresh, got %q", imported.ID)
	}
	if imported.Status != string(StateDraft) {
		t.Fatalf("imported status = %q, want DRAFT", imported.Status)
	}
	if imported.CreatedAt.Before(before) {
		t.Fatalf("imported created_at must be server-assigned now, got %v", imported.CreatedAt)
	}
	if imported.Name != "Support Agent" || imported.Description != "front-line helper" ||
		imported.Instructions != "be helpful" || imported.Model != "gpt-4o-mini" {
		t.Fatalf("imported config mismatch: %+v", imported)
	}
	// A fresh v1 version carries the copied config.
	versions, err := svc.ListVersionsCtx(t.Context(), "org-dest", imported.ID)
	if err != nil || len(versions) != 1 {
		t.Fatalf("imported agent must have exactly one version, got %d err=%v", len(versions), err)
	}
	if versions[0].Version != 1 || versions[0].Instructions != "be helpful" || versions[0].Model != "gpt-4o-mini" {
		t.Fatalf("imported v1 mismatch: %+v", versions[0])
	}
	// The source is untouched.
	original, err := svc.GetAgentCtx(t.Context(), "org-source", source.ID)
	if err != nil || original.Status != string(StateDraft) || original.OrganizationID != "org-source" {
		t.Fatalf("source mutated by import: %+v err=%v", original, err)
	}
}

func TestImportNeverTrustsClientIdentity(t *testing.T) {
	svc := NewService()
	// A hand-built payload smuggling identity fields at BOTH levels — the
	// typed decode must drop them all (unknown fields for the bundle struct).
	payload := `{
                "schema_version": "1",
                "exported_at": "1999-01-01T00:00:00Z",
                "id": "evil-bundle-id",
                "organization_id": "org-evil",
                "created_at": "1999-01-01T00:00:00Z",
                "status": "ACTIVE",
                "agent": {
                        "id": "evil-agent-id",
                        "organization_id": "org-evil",
                        "name": "Smuggled Agent",
                        "description": "d",
                        "instructions": "be helpful",
                        "model": "gpt-4o-mini",
                        "status": "ACTIVE",
                        "current_version_id": "evil-version",
                        "created_at": "1999-01-01T00:00:00Z"
                }
        }`
	var bundle ExportBundle
	if err := json.Unmarshal([]byte(payload), &bundle); err != nil {
		t.Fatalf("decode payload failed: %v", err)
	}
	before := time.Now().UTC().Add(-time.Minute)
	imported, err := svc.ImportAgent(t.Context(), "org-honest", &bundle)
	if err != nil {
		t.Fatalf("import failed: %v", err)
	}
	if imported.OrganizationID != "org-honest" {
		t.Fatalf("client organization_id was trusted: %q", imported.OrganizationID)
	}
	if imported.ID == "evil-agent-id" || imported.ID == "evil-bundle-id" {
		t.Fatalf("client id was trusted: %q", imported.ID)
	}
	if imported.Status != string(StateDraft) {
		t.Fatalf("client status was trusted: %q", imported.Status)
	}
	if imported.CreatedAt.Before(before) {
		t.Fatalf("client created_at was trusted: %v", imported.CreatedAt)
	}
	if imported.CurrentVersionID == "evil-version" || imported.CurrentVersionID == "" {
		t.Fatalf("version id must be server-assigned, got %q", imported.CurrentVersionID)
	}
}

func TestImportValidationTable(t *testing.T) {
	validBundle := func(mutate func(*ExportBundle)) *ExportBundle {
		b := &ExportBundle{
			SchemaVersion: "1",
			ExportedAt:    time.Now().UTC().Format(time.RFC3339),
			Agent: ExportedAgent{
				Name:         "Imported Agent",
				Description:  "d",
				Instructions: "be helpful",
				Model:        "gpt-4o-mini",
				Tools:        []string{},
			},
		}
		if mutate != nil {
			mutate(b)
		}
		return b
	}
	cases := []struct {
		name   string
		bundle *ExportBundle
		code   string
	}{
		{"nil bundle", nil, PortabilityCodeBundleRequired},
		{"empty schema_version", validBundle(func(b *ExportBundle) { b.SchemaVersion = "" }), PortabilityCodeSchemaVersion},
		{"future schema_version", validBundle(func(b *ExportBundle) { b.SchemaVersion = "2" }), PortabilityCodeSchemaVersion},
		{"blank name", validBundle(func(b *ExportBundle) { b.Agent.Name = "   " }), PortabilityCodeNameRequired},
		{"blank instructions", validBundle(func(b *ExportBundle) { b.Agent.Instructions = "" }), PortabilityCodeInstructionsRequired},
		{"oversized instructions", validBundle(func(b *ExportBundle) { b.Agent.Instructions = strings.Repeat("a", MaxInstructionsBytes+1) }), PortabilityCodeInstructionsTooLarge},
		{"blank model", validBundle(func(b *ExportBundle) { b.Agent.Model = "  " }), PortabilityCodeModelRequired},
		{"unknown model", validBundle(func(b *ExportBundle) { b.Agent.Model = "totally-made-up-model" }), PortabilityCodeUnknownModel},
		{"unknown tool", validBundle(func(b *ExportBundle) { b.Agent.Tools = []string{"calculator", "shell_exec"} }), PortabilityCodeUnknownTool},
		{"blank tool entry", validBundle(func(b *ExportBundle) { b.Agent.Tools = []string{" "} }), PortabilityCodeUnknownTool},
	}
	for _, tc := range cases {
		svc := NewService()
		agent, err := svc.ImportAgent(t.Context(), "org-validate", tc.bundle)
		if err == nil {
			t.Fatalf("%s: expected rejection, got agent %+v", tc.name, agent)
		}
		var pErr *PortabilityError
		if !errors.As(err, &pErr) {
			t.Fatalf("%s: expected *PortabilityError, got %T (%v)", tc.name, err, err)
		}
		if pErr.Code != tc.code {
			t.Fatalf("%s: code = %q, want %q (err=%v)", tc.name, pErr.Code, tc.code, err)
		}
		if !errors.Is(err, ErrInvalidBundle) {
			t.Fatalf("%s: must wrap ErrInvalidBundle, got %v", tc.name, err)
		}
		// Rejected imports leave NO agent behind.
		list, _ := svc.ListAgentsCtx(t.Context(), "org-validate")
		if len(list) != 0 {
			t.Fatalf("%s: rejected import left %d agents behind", tc.name, len(list))
		}
	}
}

func TestImportAcceptsCatalogVariants(t *testing.T) {
	svc := NewService()
	cases := []struct {
		name, model string
		tools       []string
	}{
		{"canonical model", "gpt-4o-mini", nil},
		{"case-insensitive model", "GPT-4O-MINI", []string{"calculator"}},
		{"vendor-prefixed model", "openai/gpt-4o", []string{"http_request"}},
		{"full catalog", "mistral-small", []string{"calculator", "http_request"}},
	}
	for _, tc := range cases {
		org := "org-catalog-" + tc.name
		agent, err := svc.ImportAgent(t.Context(), org, &ExportBundle{
			SchemaVersion: "1",
			Agent: ExportedAgent{
				Name:         "Catalog Agent " + tc.name,
				Instructions: "be helpful",
				Model:        tc.model,
				Tools:        tc.tools,
			},
		})
		if err != nil {
			t.Fatalf("%s: import failed: %v", tc.name, err)
		}
		if agent.OrganizationID != org || agent.ID == "" {
			t.Fatalf("%s: unexpected agent %+v", tc.name, agent)
		}
		orgAgents, _ := svc.ListAgentsCtx(t.Context(), org)
		if got := len(orgAgents); got != 1 {
			t.Fatalf("%s: org agent count = %d, want 1", tc.name, got)
		}
	}
}

func TestExportImportRoundTripAcrossOrgs(t *testing.T) {
	svc, source := portabilityFixture(t, "org-roundtrip-a")
	bundle, err := svc.ExportAgent(t.Context(), "org-roundtrip-a", source.ID)
	if err != nil {
		t.Fatalf("export failed: %v", err)
	}
	imported, err := svc.ImportAgent(t.Context(), "org-roundtrip-b", bundle)
	if err != nil {
		t.Fatalf("import failed: %v", err)
	}
	// Re-export of the imported agent yields the same configuration (modulo
	// exported_at): the bundle is a faithful carrier.
	again, err := svc.ExportAgent(t.Context(), "org-roundtrip-b", imported.ID)
	if err != nil {
		t.Fatalf("re-export failed: %v", err)
	}
	if !reflect.DeepEqual(again.Agent, bundle.Agent) {
		t.Fatalf("round-trip config drift: %+v vs %+v", again.Agent, bundle.Agent)
	}
}

func TestCloneDeepCopiesWithinSameOrg(t *testing.T) {
	svc, source := portabilityFixture(t, "org-clone")
	// Move the source to ACTIVE: the clone must still reset to DRAFT.
	for _, target := range []AgentState{StateBuilding, StateActive} {
		if err := svc.TransitionState(t.Context(), "org-clone", source.ID, target); err != nil {
			t.Fatalf("activate failed: %v", err)
		}
	}
	source, err := svc.GetAgentCtx(t.Context(), "org-clone", source.ID)
	if err != nil {
		t.Fatalf("reload source failed: %v", err)
	}

	clone, err := svc.CloneAgent(t.Context(), "org-clone", source.ID)
	if err != nil {
		t.Fatalf("clone failed: %v", err)
	}
	if clone.OrganizationID != "org-clone" {
		t.Fatalf("clone org = %q, want org-clone", clone.OrganizationID)
	}
	if clone.ID == source.ID || clone.ID == "" {
		t.Fatalf("clone id must be fresh, got %q", clone.ID)
	}
	if clone.Name != "Support Agent-copy" {
		t.Fatalf("clone name = %q, want %q", clone.Name, "Support Agent-copy")
	}
	if clone.Status != string(StateDraft) {
		t.Fatalf("clone status = %q, want DRAFT (reset)", clone.Status)
	}
	if clone.Description != source.Description || clone.Instructions != source.Instructions || clone.Model != source.Model {
		t.Fatalf("clone config mismatch: %+v vs %+v", clone, source)
	}
	if !clone.CreatedAt.After(source.CreatedAt.Add(-time.Minute)) || !clone.UpdatedAt.After(source.UpdatedAt.Add(-time.Minute)) {
		t.Fatalf("clone timestamps must be fresh: clone %v/%v source %v/%v", clone.CreatedAt, clone.UpdatedAt, source.CreatedAt, source.UpdatedAt)
	}
	// The clone owns a fresh v1 version (not the source's version history).
	versions, err := svc.ListVersionsCtx(t.Context(), "org-clone", clone.ID)
	if err != nil || len(versions) != 1 || versions[0].Version != 1 {
		t.Fatalf("clone must start with exactly one v1, got %d err=%v", len(versions), err)
	}
	// The source is untouched: still ACTIVE, same id, same version pointer.
	after, err := svc.GetAgentCtx(t.Context(), "org-clone", source.ID)
	if err != nil || after.Status != string(StateActive) || after.CurrentVersionID != source.CurrentVersionID {
		t.Fatalf("source mutated by clone: %+v err=%v", after, err)
	}
	// Independent second clone: both copies coexist.
	second, err := svc.CloneAgent(t.Context(), "org-clone", source.ID)
	if err != nil || second.ID == clone.ID {
		t.Fatalf("second clone failed: %v (ids %q vs %q)", err, second.ID, clone.ID)
	}
}

func TestCloneCrossOrgRejected(t *testing.T) {
	svc, source := portabilityFixture(t, "org-clone-owner")
	cases := []struct{ name, orgID, agentID string }{
		{"cross-org", "org-clone-foreign", source.ID},
		{"unknown agent", "org-clone-owner", "agent-does-not-exist"},
		{"blank org", "", source.ID},
	}
	for _, tc := range cases {
		if _, err := svc.CloneAgent(t.Context(), tc.orgID, tc.agentID); !errors.Is(err, ErrAgentNotFound) {
			t.Fatalf("%s: expected ErrAgentNotFound, got %v", tc.name, err)
		}
	}
	// The rejected attempts left no copies behind.
	list, _ := svc.ListAgentsCtx(t.Context(), "org-clone-owner")
	if len(list) != 1 {
		t.Fatalf("cross-org clone attempts left %d agents, want 1", len(list))
	}
}

// --- store mode (sqlmock) ----------------------------------------------------

func portabilitySQLAgentRows() []string {
	return []string{"id", "organization_id", "name", "description", "instructions", "model", "status", "current_version_id", "created_at", "updated_at"}
}

func TestExportStoreModeRendersBundleWithoutWrites(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()
	org := "org-store-export"
	now := time.Now().UTC()
	svc := NewServiceWithStore(NewPostgresStore(db))

	mock.ExpectQuery("FROM agents WHERE id = \\$1 AND organization_id = \\$2").
		WithArgs("agent-1", org).
		WillReturnRows(sqlmock.NewRows(portabilitySQLAgentRows()).
			AddRow("agent-1", org, "Support Agent", "d", "be helpful", "gpt-4o-mini", "ACTIVE", "v1", now, now))

	bundle, err := svc.ExportAgent(context.Background(), org, "agent-1")
	if err != nil {
		t.Fatalf("store export failed: %v", err)
	}
	if bundle.Agent.Name != "Support Agent" || bundle.Agent.Model != "gpt-4o-mini" || bundle.SchemaVersion != "1" {
		t.Fatalf("store export bundle mismatch: %+v", bundle)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

func TestImportStoreModeInsertsDraftWithCallerOrg(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()
	svc := NewServiceWithStore(NewPostgresStore(db))

	// The guarded INSERT binds the CALLER's org, the fresh identity and the
	// reset DRAFT status (never anything client-supplied).
	mock.ExpectExec("INSERT INTO agents").
		WithArgs(sqlmock.AnyArg(), "org-store-import", "Imported Agent", "d", "be helpful", "gpt-4o-mini", "DRAFT",
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO agent_versions").
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), `{"model":"gpt-4o-mini"}`, "be helpful", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	imported, err := svc.ImportAgent(context.Background(), "org-store-import", &ExportBundle{
		SchemaVersion: "1",
		Agent:         ExportedAgent{Name: "Imported Agent", Description: "d", Instructions: "be helpful", Model: "gpt-4o-mini"},
	})
	if err != nil {
		t.Fatalf("store import failed: %v", err)
	}
	if imported.Status != string(StateDraft) || imported.OrganizationID != "org-store-import" {
		t.Fatalf("store import identity mismatch: %+v", imported)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

func TestCloneStoreModeInsertsCopyWithResetStatus(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()
	org := "org-store-clone"
	now := time.Now().UTC()
	svc := NewServiceWithStore(NewPostgresStore(db))

	// Org-scoped source lookup.
	mock.ExpectQuery("FROM agents WHERE id = \\$1 AND organization_id = \\$2").
		WithArgs("agent-1", org).
		WillReturnRows(sqlmock.NewRows(portabilitySQLAgentRows()).
			AddRow("agent-1", org, "Source Agent", "d", "be helpful", "gpt-4o-mini", "ACTIVE", "v1", now, now))
	// The copy INSERT: -copy name, CALLER's org, reset DRAFT status.
	mock.ExpectExec("INSERT INTO agents").
		WithArgs(sqlmock.AnyArg(), org, "Source Agent-copy", "d", "be helpful", "gpt-4o-mini", "DRAFT",
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO agent_versions").
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), `{"model":"gpt-4o-mini"}`, "be helpful", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	clone, err := svc.CloneAgent(context.Background(), org, "agent-1")
	if err != nil {
		t.Fatalf("store clone failed: %v", err)
	}
	if clone.Status != string(StateDraft) || clone.Name != "Source Agent-copy" {
		t.Fatalf("store clone mismatch: %+v", clone)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

func TestCloneStoreModeCrossOrgWritesNothing(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()
	svc := NewServiceWithStore(NewPostgresStore(db))
	// Foreign org: the scoped SELECT returns no rows; the missing INSERT
	// expectations make any stray write fail the test.
	mock.ExpectQuery("FROM agents WHERE id = \\$1 AND organization_id = \\$2").
		WithArgs("agent-1", "org-foreign").
		WillReturnRows(sqlmock.NewRows(portabilitySQLAgentRows()))
	if _, err := svc.CloneAgent(context.Background(), "org-foreign", "agent-1"); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("cross-org clone must be ErrAgentNotFound, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}
