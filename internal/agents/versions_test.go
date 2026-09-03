package agents

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func newVersionsFixture(t *testing.T) (*Service, *VersionsService, *Agent) {
	t.Helper()
	ctx := context.Background()
	agentSvc := NewService()
	agent, err := agentSvc.CreateAgentCtx(ctx, "org-1", "Support Agent", "desc", "help users v1", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("CreateAgentCtx returned error: %v", err)
	}
	return agentSvc, NewVersionsService(agentSvc), agent
}

func TestCreateVersionSnapshotsCurrentConfig(t *testing.T) {
	ctx := context.Background()
	agentSvc, versionsSvc, agent := newVersionsFixture(t)

	// Agent creation seeds legacy v1, so the first config version is 2.
	v1, err := versionsSvc.CreateVersionCtx(ctx, "org-1", agent.ID, "user-1")
	if err != nil {
		t.Fatalf("CreateVersionCtx returned error: %v", err)
	}
	if v1.Version != 2 {
		t.Fatalf("expected first config version 2, got %d", v1.Version)
	}
	if v1.Status != VersionStatusDraft {
		t.Fatalf("expected draft status, got %q", v1.Status)
	}
	if v1.PublishedAt != nil {
		t.Fatalf("draft version must not have published_at, got %v", v1.PublishedAt)
	}

	// Snapshot must capture the agent's current configuration.
	var snap AgentSnapshot
	if err := json.Unmarshal([]byte(v1.Snapshot), &snap); err != nil {
		t.Fatalf("snapshot is not valid JSON: %v", err)
	}
	if snap.Instructions != "help users v1" || snap.Model != "gpt-4o-mini" || snap.Name != "Support Agent" {
		t.Fatalf("snapshot did not capture current config: %+v", snap)
	}

	// Config drift, then a second snapshot must capture the new values.
	agent.Instructions = "help users v2"
	agent.Model = "gpt-4o"
	if err := agentSvc.UpdateAgentCtx(ctx, "org-1", agent); err != nil {
		t.Fatalf("UpdateAgentCtx returned error: %v", err)
	}
	v2, err := versionsSvc.CreateVersionCtx(ctx, "org-1", agent.ID, "user-1")
	if err != nil {
		t.Fatalf("second CreateVersionCtx returned error: %v", err)
	}
	if v2.Version != 3 {
		t.Fatalf("expected second config version 3, got %d", v2.Version)
	}
	var snap2 AgentSnapshot
	if err := json.Unmarshal([]byte(v2.Snapshot), &snap2); err != nil {
		t.Fatalf("snapshot is not valid JSON: %v", err)
	}
	if snap2.Instructions != "help users v2" || snap2.Model != "gpt-4o" {
		t.Fatalf("second snapshot did not capture drifted config: %+v", snap2)
	}
}

func TestPublishMarksVersionImmutable(t *testing.T) {
	ctx := context.Background()
	agentSvc, versionsSvc, agent := newVersionsFixture(t)

	v, err := versionsSvc.CreateVersionCtx(ctx, "org-1", agent.ID, "user-1")
	if err != nil {
		t.Fatalf("CreateVersionCtx returned error: %v", err)
	}
	published, err := versionsSvc.PublishVersionCtx(ctx, "org-1", agent.ID, v.Version, "user-1")
	if err != nil {
		t.Fatalf("PublishVersionCtx returned error: %v", err)
	}
	if published.Status != VersionStatusPublished {
		t.Fatalf("expected published status, got %q", published.Status)
	}
	if published.PublishedAt == nil || published.PublishedBy != "user-1" {
		t.Fatalf("publish should stamp published_at/published_by, got %v/%q", published.PublishedAt, published.PublishedBy)
	}
	firstPublishedAt := *published.PublishedAt
	snapshotBefore := published.Snapshot

	// Idempotent re-publish: published_at must NOT be reset (immutability).
	time.Sleep(10 * time.Millisecond)
	again, err := versionsSvc.PublishVersionCtx(ctx, "org-1", agent.ID, v.Version, "user-2")
	if err != nil {
		t.Fatalf("re-publish returned error: %v", err)
	}
	if !again.PublishedAt.Equal(firstPublishedAt) {
		t.Fatalf("re-publish must not reset published_at: %v != %v", again.PublishedAt, firstPublishedAt)
	}
	if again.Snapshot != snapshotBefore {
		t.Fatal("publish must never mutate the snapshot")
	}

	// The agent's current-version pointer follows the published version.
	got, err := agentSvc.GetAgentCtx(ctx, "org-1", agent.ID)
	if err != nil {
		t.Fatalf("GetAgentCtx returned error: %v", err)
	}
	if got.CurrentVersionID != v.ID {
		t.Fatalf("publish should re-point agent current version, got %q want %q", got.CurrentVersionID, v.ID)
	}
}

func TestPublishRepointsAgentAndArchivesPrevious(t *testing.T) {
	ctx := context.Background()
	agentSvc, versionsSvc, agent := newVersionsFixture(t)

	v2, err := versionsSvc.CreateVersionCtx(ctx, "org-1", agent.ID, "user-1")
	if err != nil {
		t.Fatalf("CreateVersionCtx returned error: %v", err)
	}
	if _, err := versionsSvc.PublishVersionCtx(ctx, "org-1", agent.ID, v2.Version, "user-1"); err != nil {
		t.Fatalf("PublishVersionCtx returned error: %v", err)
	}
	got, err := agentSvc.GetAgentCtx(ctx, "org-1", agent.ID)
	if err != nil {
		t.Fatalf("GetAgentCtx returned error: %v", err)
	}
	if got.CurrentVersionID != v2.ID {
		t.Fatalf("publish should re-point agent current version, got %q want %q", got.CurrentVersionID, v2.ID)
	}

	// Publishing a second version archives the first.
	v3, err := versionsSvc.CreateVersionCtx(ctx, "org-1", agent.ID, "user-1")
	if err != nil {
		t.Fatalf("CreateVersionCtx returned error: %v", err)
	}
	if _, err := versionsSvc.PublishVersionCtx(ctx, "org-1", agent.ID, v3.Version, "user-1"); err != nil {
		t.Fatalf("PublishVersionCtx returned error: %v", err)
	}
	list, err := versionsSvc.ListVersionsCtx(ctx, "org-1", agent.ID)
	if err != nil {
		t.Fatalf("ListVersionsCtx returned error: %v", err)
	}
	statuses := map[string]string{}
	for _, version := range list {
		statuses[version.ID] = version.Status
	}
	if statuses[v2.ID] != VersionStatusArchived || statuses[v3.ID] != VersionStatusPublished {
		t.Fatalf("expected v2 archived + v3 published, got %+v", statuses)
	}
}

func TestRollbackRestoresConfigFromSnapshot(t *testing.T) {
	ctx := context.Background()
	agentSvc, versionsSvc, agent := newVersionsFixture(t)

	// v2 = original config (help users v1 / gpt-4o-mini), published.
	v2, err := versionsSvc.CreateVersionCtx(ctx, "org-1", agent.ID, "user-1")
	if err != nil {
		t.Fatalf("CreateVersionCtx returned error: %v", err)
	}
	if _, err := versionsSvc.PublishVersionCtx(ctx, "org-1", agent.ID, v2.Version, "user-1"); err != nil {
		t.Fatalf("PublishVersionCtx(v2) returned error: %v", err)
	}

	// Drift + publish v3.
	agent.Instructions = "help users v2"
	agent.Model = "gpt-4o"
	if err := agentSvc.UpdateAgentCtx(ctx, "org-1", agent); err != nil {
		t.Fatalf("UpdateAgentCtx returned error: %v", err)
	}
	v3, err := versionsSvc.CreateVersionCtx(ctx, "org-1", agent.ID, "user-1")
	if err != nil {
		t.Fatalf("CreateVersionCtx returned error: %v", err)
	}
	if _, err := versionsSvc.PublishVersionCtx(ctx, "org-1", agent.ID, v3.Version, "user-1"); err != nil {
		t.Fatalf("PublishVersionCtx(v3) returned error: %v", err)
	}

	// Rollback to v2: re-publishes v2, archives v3, restores live config.
	rolledBack, err := versionsSvc.RollbackVersionCtx(ctx, "org-1", agent.ID, v2.Version, "user-2")
	if err != nil {
		t.Fatalf("RollbackVersionCtx returned error: %v", err)
	}
	if rolledBack.Version != v2.Version || rolledBack.Status != VersionStatusPublished {
		t.Fatalf("rollback should re-publish target, got %+v", rolledBack)
	}
	got, err := agentSvc.GetAgentCtx(ctx, "org-1", agent.ID)
	if err != nil {
		t.Fatalf("GetAgentCtx returned error: %v", err)
	}
	if got.Instructions != "help users v1" || got.Model != "gpt-4o-mini" {
		t.Fatalf("rollback should restore snapshot config, got %+v", got)
	}
	if got.CurrentVersionID != v2.ID {
		t.Fatalf("rollback should re-point current version to target, got %q", got.CurrentVersionID)
	}
	list, _ := versionsSvc.ListVersionsCtx(ctx, "org-1", agent.ID)
	statuses := map[string]string{}
	for _, version := range list {
		statuses[version.ID] = version.Status
	}
	if statuses[v3.ID] != VersionStatusArchived {
		t.Fatalf("rollback should archive the previously published version, got %+v", statuses)
	}

	// Rollback to the current version is a no-op.
	noop, err := versionsSvc.RollbackVersionCtx(ctx, "org-1", agent.ID, v2.Version, "user-2")
	if err != nil {
		t.Fatalf("idempotent rollback returned error: %v", err)
	}
	if noop.Version != v2.Version {
		t.Fatalf("idempotent rollback changed the current version: %d", noop.Version)
	}

	// Unknown target version surfaces as not found.
	if _, err := versionsSvc.RollbackVersionCtx(ctx, "org-1", agent.ID, 99, "user-2"); !errors.Is(err, ErrVersionNotFound) {
		t.Fatalf("expected ErrVersionNotFound, got %v", err)
	}
}

func TestVersionsTenantGuard(t *testing.T) {
	ctx := context.Background()
	_, versionsSvc, agent := newVersionsFixture(t)

	if _, err := versionsSvc.CreateVersionCtx(ctx, "org-1", agent.ID, "user-1"); err != nil {
		t.Fatalf("CreateVersionCtx returned error: %v", err)
	}
	if _, err := versionsSvc.ListVersionsCtx(ctx, "org-2", agent.ID); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("foreign tenant list should be ErrAgentNotFound, got %v", err)
	}
	if _, err := versionsSvc.GetVersionCtx(ctx, "org-2", agent.ID, 2); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("foreign tenant get should be ErrAgentNotFound, got %v", err)
	}
	if _, err := versionsSvc.RollbackVersionCtx(ctx, "org-2", agent.ID, 2, "user-9"); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("foreign tenant rollback should be ErrAgentNotFound, got %v", err)
	}
}

func TestResolvePublishedVersion(t *testing.T) {
	ctx := context.Background()
	_, versionsSvc, agent := newVersionsFixture(t)

	draft, err := versionsSvc.CreateVersionCtx(ctx, "org-1", agent.ID, "user-1")
	if err != nil {
		t.Fatalf("CreateVersionCtx returned error: %v", err)
	}
	if err := versionsSvc.ResolvePublishedVersion(ctx, "org-1", agent.ID, draft.Version); !errors.Is(err, ErrVersionNotPublished) {
		t.Fatalf("draft version should not be deployable, got %v", err)
	}
	if _, err := versionsSvc.PublishVersionCtx(ctx, "org-1", agent.ID, draft.Version, "user-1"); err != nil {
		t.Fatalf("PublishVersionCtx returned error: %v", err)
	}
	if err := versionsSvc.ResolvePublishedVersion(ctx, "org-1", agent.ID, draft.Version); err != nil {
		t.Fatalf("published version should be deployable, got %v", err)
	}
	if err := versionsSvc.ResolvePublishedVersion(ctx, "org-1", agent.ID, 42); !errors.Is(err, ErrVersionNotFound) {
		t.Fatalf("unknown version should be ErrVersionNotFound, got %v", err)
	}
}

func TestVersionsStoreSQLMock(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()
	store := NewVersionsPostgresStore(db)
	ctx := context.Background()
	orgID, agentID := "org-1", "agent-1"

	// CreateVersion: tenant-guarded INSERT..SELECT.
	publishedAt := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	version := &ConfigVersion{
		ID: "av-1", AgentID: agentID, OrganizationID: orgID, Version: 4,
		Snapshot: `{"name":"a","instructions":"i","model":"m"}`,
		Status:   VersionStatusPublished, PublishedAt: &publishedAt, PublishedBy: "user-1",
		CreatedAt: publishedAt,
	}
	mock.ExpectExec("INSERT INTO agent_versions").
		WithArgs("av-1", agentID, 4, version.Snapshot, version.Snapshot, "i", VersionStatusPublished, publishedAt, "user-1", publishedAt).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := store.CreateVersion(ctx, orgID, version); err != nil {
		t.Fatalf("CreateVersion returned error: %v", err)
	}

	// CreateVersion with 0 rows affected -> tenant guard -> ErrAgentNotFound.
	mock.ExpectExec("INSERT INTO agent_versions").
		WithArgs(sqlmock.AnyArg(), agentID, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := store.CreateVersion(ctx, orgID, version); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("expected ErrAgentNotFound, got %v", err)
	}

	// GetVersion scans a full row including NULL published_at.
	rows := sqlmock.NewRows([]string{"id", "agent_id", "organization_id", "version", "snapshot", "status", "published_at", "published_by", "created_at"}).
		AddRow("av-2", agentID, orgID, 5, `{"model":"gpt-4o"}`, VersionStatusDraft, nil, "", publishedAt)
	mock.ExpectQuery("SELECT av.id, av.agent_id, a.organization_id").
		WithArgs(orgID, agentID, 5).
		WillReturnRows(rows)
	got, err := store.GetVersion(ctx, orgID, agentID, 5)
	if err != nil {
		t.Fatalf("GetVersion returned error: %v", err)
	}
	if got.Version != 5 || got.Status != VersionStatusDraft || got.PublishedAt != nil {
		t.Fatalf("unexpected scanned version: %+v", got)
	}

	// GetVersion with no rows -> ErrVersionNotFound.
	mock.ExpectQuery("SELECT av.id, av.agent_id, a.organization_id").
		WithArgs(orgID, agentID, 6).
		WillReturnRows(sqlmock.NewRows([]string{"id", "agent_id", "organization_id", "version", "snapshot", "status", "published_at", "published_by", "created_at"}))
	if _, err := store.GetVersion(ctx, orgID, agentID, 6); !errors.Is(err, ErrVersionNotFound) {
		t.Fatalf("expected ErrVersionNotFound, got %v", err)
	}

	// GetPublishedVersion with a row.
	mock.ExpectQuery("COALESCE\\(av.status, 'draft'\\) = 'published'").
		WithArgs(orgID, agentID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "agent_id", "organization_id", "version", "snapshot", "status", "published_at", "published_by", "created_at"}).
			AddRow("av-1", agentID, orgID, 4, "{}", VersionStatusPublished, publishedAt, "user-1", publishedAt))
	published, err := store.GetPublishedVersion(ctx, orgID, agentID)
	if err != nil {
		t.Fatalf("GetPublishedVersion returned error: %v", err)
	}
	if published == nil || published.Version != 4 || published.PublishedAt == nil {
		t.Fatalf("unexpected published version: %+v", published)
	}

	// GetPublishedVersion with no rows -> nil, nil.
	mock.ExpectQuery("COALESCE\\(av.status, 'draft'\\) = 'published'").
		WithArgs(orgID, agentID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "agent_id", "organization_id", "version", "snapshot", "status", "published_at", "published_by", "created_at"}))
	none, err := store.GetPublishedVersion(ctx, orgID, agentID)
	if err != nil || none != nil {
		t.Fatalf("expected nil published version, got %v/%v", none, err)
	}

	// ListVersions ascending.
	mock.ExpectQuery("ORDER BY av.version ASC").
		WithArgs(orgID, agentID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "agent_id", "organization_id", "version", "snapshot", "status", "published_at", "published_by", "created_at"}).
			AddRow("av-1", agentID, orgID, 4, "{}", VersionStatusArchived, publishedAt, "user-1", publishedAt).
			AddRow("av-2", agentID, orgID, 5, "{}", VersionStatusPublished, publishedAt, "user-1", publishedAt))
	list, err := store.ListVersions(ctx, orgID, agentID)
	if err != nil {
		t.Fatalf("ListVersions returned error: %v", err)
	}
	if len(list) != 2 || list[0].Version != 4 || list[1].Version != 5 {
		t.Fatalf("unexpected list: %+v", list)
	}

	// UpdateVersionStatus: 1 row affected -> ok; 0 rows -> not found.
	mock.ExpectExec("UPDATE agent_versions av").
		WithArgs(VersionStatusPublished, publishedAt, "user-1", orgID, "av-2").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.UpdateVersionStatus(ctx, orgID, list[1]); err != nil {
		t.Fatalf("UpdateVersionStatus returned error: %v", err)
	}
	mock.ExpectExec("UPDATE agent_versions av").
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), orgID, "av-missing").
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := store.UpdateVersionStatus(ctx, orgID, &ConfigVersion{ID: "av-missing"}); !errors.Is(err, ErrVersionNotFound) {
		t.Fatalf("expected ErrVersionNotFound, got %v", err)
	}

	// NextVersionNumber.
	mock.ExpectQuery("COALESCE\\(MAX\\(av.version\\), 0\\) \\+ 1").
		WithArgs(orgID, agentID).
		WillReturnRows(sqlmock.NewRows([]string{"next"}).AddRow(6))
	next, err := store.NextVersionNumber(ctx, orgID, agentID)
	if err != nil {
		t.Fatalf("NextVersionNumber returned error: %v", err)
	}
	if next != 6 {
		t.Fatalf("expected next version 6, got %d", next)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Version diff (wave-3 track 3-e)
// ---------------------------------------------------------------------------

// TestDiffVersionsCtxSameVersionUnchanged verifies that diffing a version
// against itself reports every comparable field unchanged (contract case:
// "same-version diff -> all unchanged").
func TestDiffVersionsCtxSameVersionUnchanged(t *testing.T) {
	ctx := context.Background()
	_, versionsSvc, agent := newVersionsFixture(t)

	created, err := versionsSvc.CreateVersionCtx(ctx, "org-1", agent.ID, "user-1")
	if err != nil {
		t.Fatalf("CreateVersionCtx returned error: %v", err)
	}

	diff, err := versionsSvc.DiffVersionsCtx(ctx, "org-1", agent.ID, created.Version, created.Version)
	if err != nil {
		t.Fatalf("DiffVersionsCtx returned error: %v", err)
	}
	if diff.AgentID != agent.ID || diff.From != created.Version || diff.To != created.Version {
		t.Fatalf("unexpected diff header: %+v", diff)
	}
	if len(diff.Fields) == 0 {
		t.Fatal("expected at least one comparable field")
	}
	for _, field := range diff.Fields {
		if field.Changed {
			t.Fatalf("same-version diff reported %q as changed (%v -> %v)", field.Field, field.From, field.To)
		}
	}
}

// TestDiffVersionsCtxKnownDiffs verifies the field mapping for a known config
// drift: instructions map to system_prompt, model diffs, extras (name, status)
// surface unchanged, absent fields come back null/unchanged.
func TestDiffVersionsCtxKnownDiffs(t *testing.T) {
	ctx := context.Background()
	agentSvc, versionsSvc, agent := newVersionsFixture(t)

	v2, err := versionsSvc.CreateVersionCtx(ctx, "org-1", agent.ID, "user-1")
	if err != nil {
		t.Fatalf("CreateVersionCtx(v2) returned error: %v", err)
	}
	// Drift the live config, then snapshot v3.
	agent.Instructions = "help users v2"
	agent.Model = "gpt-4o"
	agent.Description = "tier-1 support"
	if err := agentSvc.UpdateAgentCtx(ctx, "org-1", agent); err != nil {
		t.Fatalf("UpdateAgentCtx returned error: %v", err)
	}
	v3, err := versionsSvc.CreateVersionCtx(ctx, "org-1", agent.ID, "user-1")
	if err != nil {
		t.Fatalf("CreateVersionCtx(v3) returned error: %v", err)
	}

	byField := func(diff *VersionDiff) map[string]VersionDiffField {
		out := make(map[string]VersionDiffField, len(diff.Fields))
		for _, field := range diff.Fields {
			out[field.Field] = field
		}
		return out
	}

	diff, err := versionsSvc.DiffVersionsCtx(ctx, "org-1", agent.ID, v2.Version, v3.Version)
	if err != nil {
		t.Fatalf("DiffVersionsCtx returned error: %v", err)
	}
	fields := byField(diff)

	model, ok := fields["model"]
	if !ok || !model.Changed || model.From != "gpt-4o-mini" || model.To != "gpt-4o" {
		t.Fatalf("unexpected model diff: %+v", model)
	}
	prompt, ok := fields["system_prompt"]
	if !ok || !prompt.Changed || prompt.From != "help users v1" || prompt.To != "help users v2" {
		t.Fatalf("instructions must diff as system_prompt, got: %+v", prompt)
	}
	description, ok := fields["description"]
	if !ok || !description.Changed || description.To != "tier-1 support" {
		t.Fatalf("unexpected description diff: %+v", description)
	}
	// temperature/params/tools are absent from today's snapshots: present in
	// the payload but null on both sides and unchanged.
	for _, absent := range []string{"temperature", "params", "tools"} {
		field, ok := fields[absent]
		if !ok {
			t.Fatalf("comparable field %q missing from diff payload", absent)
		}
		if field.Changed || field.From != nil || field.To != nil {
			t.Fatalf("absent field %q must be null/unchanged, got %+v", absent, field)
		}
	}
	// Extra snapshot keys (name, status) still diff, unchanged here.
	name, ok := fields["name"]
	if !ok || name.Changed || name.From != "Support Agent" {
		t.Fatalf("unexpected name diff: %+v", name)
	}
	if fields["status"].Changed {
		t.Fatalf("status must be unchanged, got %+v", fields["status"])
	}
}

// TestDiffVersionsCtxExtraKeysEmittedOnce guards against duplicate extra-key
// rows: keys present on BOTH snapshots (name, status today) must appear exactly
// once in fields — the union of both sides' extra keys, sorted — never one row
// per side (regression for a bug found in the live smoke test).
func TestDiffVersionsCtxExtraKeysEmittedOnce(t *testing.T) {
	ctx := context.Background()
	_, versionsSvc, agent := newVersionsFixture(t)

	v2, err := versionsSvc.CreateVersionCtx(ctx, "org-1", agent.ID, "user-1")
	if err != nil {
		t.Fatalf("CreateVersionCtx(v2) returned error: %v", err)
	}
	v3, err := versionsSvc.CreateVersionCtx(ctx, "org-1", agent.ID, "user-1")
	if err != nil {
		t.Fatalf("CreateVersionCtx(v3) returned error: %v", err)
	}

	diff, err := versionsSvc.DiffVersionsCtx(ctx, "org-1", agent.ID, v2.Version, v3.Version)
	if err != nil {
		t.Fatalf("DiffVersionsCtx returned error: %v", err)
	}

	// 6 contract-comparable fields + exactly one row per extra key (name,
	// status) = 8; no duplicates allowed.
	counts := make(map[string]int, len(diff.Fields))
	for _, field := range diff.Fields {
		counts[field.Field]++
	}
	for name, count := range counts {
		if count != 1 {
			t.Fatalf("field %q appeared %d times in the diff, want exactly 1 (fields: %+v)", name, count, diff.Fields)
		}
	}
	if len(diff.Fields) != 8 {
		t.Fatalf("expected 8 diff fields (6 comparable + name + status), got %d: %+v", len(diff.Fields), diff.Fields)
	}
	// Extra keys are appended after the comparable block, sorted.
	extraNames := []string{diff.Fields[6].Field, diff.Fields[7].Field}
	if extraNames[0] != "name" || extraNames[1] != "status" {
		t.Fatalf("extra keys must be sorted (name, status), got %v", extraNames)
	}
}

// TestDiffVersionsCtxUnknownVersionAndCrossTenant pins the error contract:
// unknown from/to -> ErrVersionNotFound; a foreign-tenant caller cannot diff
// versions of an agent it does not own (ErrAgentNotFound via the tenant guard).
func TestDiffVersionsCtxUnknownVersionAndCrossTenant(t *testing.T) {
	ctx := context.Background()
	agentSvc, versionsSvc, agent := newVersionsFixture(t)
	created, err := versionsSvc.CreateVersionCtx(ctx, "org-1", agent.ID, "user-1")
	if err != nil {
		t.Fatalf("CreateVersionCtx returned error: %v", err)
	}

	cases := []struct {
		name     string
		agentID  string
		orgID    string
		from, to int
		want     error
	}{
		{"unknown from version", agent.ID, "org-1", 99, created.Version, ErrVersionNotFound},
		{"unknown to version", agent.ID, "org-1", created.Version, 99, ErrVersionNotFound},
		{"unknown agent", "agent-missing", "org-1", 1, 2, ErrAgentNotFound},
		{"cross-tenant agent guard", agent.ID, "org-other", created.Version, created.Version, ErrAgentNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := versionsSvc.DiffVersionsCtx(ctx, tc.orgID, tc.agentID, tc.from, tc.to)
			if !errors.Is(err, tc.want) {
				t.Fatalf("expected %v, got %v", tc.want, err)
			}
		})
	}

	// Cross-agent defense in depth: a version row stamped with a foreign
	// agentID must surface as ErrVersionNotFound, never leak a diff.
	other, err := agentSvc.CreateAgentCtx(ctx, "org-1", "Other Agent", "d", "i", "m")
	if err != nil {
		t.Fatalf("CreateAgentCtx(other) returned error: %v", err)
	}
	foreign := &ConfigVersion{
		ID:             "av-foreign",
		AgentID:        other.ID,
		OrganizationID: "org-1",
		Version:        created.Version + 10,
		Snapshot:       `{"model":"m"}`,
		Status:         VersionStatusDraft,
	}
	// Plant the foreign-agent row under the requested agent's key to simulate
	// a store that returns a mismatched row.
	versionsSvc.mu.Lock()
	versionsSvc.items[agent.ID] = append(versionsSvc.items[agent.ID], foreign)
	versionsSvc.mu.Unlock()
	if _, err := versionsSvc.DiffVersionsCtx(ctx, "org-1", agent.ID, created.Version+10, created.Version+10); !errors.Is(err, ErrVersionNotFound) {
		t.Fatalf("cross-agent mismatch must be ErrVersionNotFound, got %v", err)
	}
}
