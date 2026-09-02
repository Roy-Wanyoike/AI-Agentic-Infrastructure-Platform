package database

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestBuildDSN(t *testing.T) {
	cfg := Config{
		Host:     "localhost",
		Port:     5432,
		User:     "agentos",
		Password: "secret",
		DBName:   "agentos",
		SSLMode:  "disable",
	}

	dsn := cfg.BuildDSN()
	if dsn == "" {
		t.Fatal("BuildDSN returned empty DSN")
	}
	if dsn != "host=localhost port=5432 user=agentos password=secret dbname=agentos sslmode=disable" {
		t.Fatalf("unexpected DSN: %q", dsn)
	}
}

func TestOpen(t *testing.T) {
	cfg := DefaultConfig()
	db, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	if db == nil {
		t.Fatal("Open returned nil database handle")
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
}

func TestLoadMigrations(t *testing.T) {
	migrations, err := LoadMigrations("../../migrations")
	if err != nil {
		t.Fatalf("LoadMigrations returned error: %v", err)
	}
	if len(migrations) < 4 {
		t.Fatalf("expected at least four migrations, got %d", len(migrations))
	}
	if migrations[0].Version != 1 {
		t.Fatalf("first migration should be version 1, got %d", migrations[0].Version)
	}
	if !strings.Contains(migrations[0].SQL, "CREATE TABLE IF NOT EXISTS organizations") {
		t.Fatal("initial migration should create the organizations table")
	}

	entries, err := os.ReadDir("../../migrations")
	if err != nil {
		t.Fatalf("ReadDir migrations returned error: %v", err)
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) == ".md" {
			continue
		}
		seen[strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))] = true
	}
	for _, expected := range []string{"001_init_schema", "002_auth_tables", "003_agents_tables", "004_runs_and_steps"} {
		if !seen[expected] {
			t.Fatalf("missing migration %s.sql", expected)
		}
	}
}

func TestApplyMigrations(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	mock.ExpectExec("CREATE TABLE IF NOT EXISTS schema_migrations").
		WillReturnResult(sqlmock.NewResult(0, 0))

	for _, migration := range []struct {
		version int
		name    string
		sql     string
	}{
		{version: 1, name: "init_schema", sql: "CREATE TABLE IF NOT EXISTS organizations"},
		{version: 2, name: "auth_tables", sql: "CREATE TABLE IF NOT EXISTS organization_memberships"},
		{version: 3, name: "agents_tables", sql: "CREATE TABLE IF NOT EXISTS agent_versions"},
		{version: 4, name: "runs_and_steps", sql: "CREATE TABLE IF NOT EXISTS run_steps"},
		{version: 5, name: "persistence_hardening", sql: "ALTER TABLE agents ADD COLUMN IF NOT EXISTS description"},
		{version: 6, name: "workflows_approvals", sql: "ALTER TABLE workflows ADD COLUMN IF NOT EXISTS description"},
		{version: 8, name: "policies", sql: "CREATE TABLE IF NOT EXISTS policies"},
	} {
		mock.ExpectQuery("SELECT version FROM schema_migrations WHERE version = \\$1").
			WithArgs(migration.version).
			WillReturnRows(sqlmock.NewRows([]string{"version"}))
		mock.ExpectBegin()
		mock.ExpectExec(migration.sql).
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec("INSERT INTO schema_migrations").
			WithArgs(migration.version, migration.name).
			WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectCommit()
	}

	migrations, err := LoadMigrations("../../migrations")
	if err != nil {
		t.Fatalf("LoadMigrations returned error: %v", err)
	}
	if err := ApplyMigrations(db, migrations); err != nil {
		t.Fatalf("ApplyMigrations returned error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Host == "" {
		t.Fatal("default host should not be empty")
	}
	if cfg.Port == 0 {
		t.Fatal("default port should not be zero")
	}
}

func TestRepositoryCreateOrganizationAndUser(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	repo := NewRepository(db)
	mock.ExpectExec("INSERT INTO organizations").
		WithArgs(sqlmock.AnyArg(), "Acme").
		WillReturnResult(sqlmock.NewResult(1, 1))
	org, err := repo.CreateOrganization("Acme")
	if err != nil {
		t.Fatalf("CreateOrganization returned error: %v", err)
	}
	if org == nil || org.ID == "" {
		t.Fatal("CreateOrganization should return a persisted organization")
	}

	mock.ExpectExec("INSERT INTO users").
		WithArgs(sqlmock.AnyArg(), org.ID, "alice@example.com", "hashed-password", "OWNER").
		WillReturnResult(sqlmock.NewResult(1, 1))
	user, err := repo.CreateUser(org.ID, "alice@example.com", "hashed-password", "OWNER")
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	if user == nil || user.Email != "alice@example.com" {
		t.Fatal("CreateUser should persist a user row")
	}

	mock.ExpectQuery("SELECT id, organization_id, email, password_hash, role, created_at FROM users WHERE email = \\$1").
		WithArgs("alice@example.com").
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "email", "password_hash", "role", "created_at"}).
			AddRow(user.ID, org.ID, user.Email, user.PasswordHash, user.Role, user.CreatedAt))
	stored, ok := repo.GetUserByEmail("alice@example.com")
	if !ok || stored == nil {
		t.Fatal("GetUserByEmail should return the stored user")
	}
	if stored.Organization != org.ID {
		t.Fatalf("expected org %q, got %q", org.ID, stored.Organization)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestRepositoryListAgentsByOrg(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	repo := NewRepository(db)
	orgID := "org-tenant"
	mock.ExpectQuery("SELECT id, organization_id, name, model, status, created_at, updated_at FROM agents WHERE organization_id = \\$1 ORDER BY created_at DESC").
		WithArgs(orgID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "name", "model", "status", "created_at", "updated_at"}).
			AddRow("agent-1", orgID, "Support Agent", "gpt-4o-mini", "DRAFT", time.Now().UTC(), time.Now().UTC()))
	agentsList, err := repo.ListAgentsByOrg(orgID)
	if err != nil {
		t.Fatalf("ListAgentsByOrg returned error: %v", err)
	}
	if len(agentsList) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agentsList))
	}
	if agentsList[0].OrganizationID != orgID {
		t.Fatalf("expected org %q, got %q", orgID, agentsList[0].OrganizationID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestRepositoryCreateRunAndListRunsByOrg(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	repo := NewRepository(db)
	orgID := "org-run"
	agentID := "agent-run"
	mock.ExpectExec("INSERT INTO runs").
		WithArgs(sqlmock.AnyArg(), orgID, agentID, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	runID, err := repo.CreateRun(orgID, agentID, "hello")
	if err != nil {
		t.Fatalf("CreateRun returned error: %v", err)
	}
	if runID == "" {
		t.Fatal("CreateRun should return a persisted run identifier")
	}

	mock.ExpectQuery("SELECT id, organization_id, agent_id, status, created_at, updated_at FROM runs WHERE organization_id = \\$1 ORDER BY created_at DESC").
		WithArgs(orgID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "agent_id", "status", "created_at", "updated_at"}).
			AddRow(runID, orgID, agentID, "QUEUED", time.Now().UTC(), time.Now().UTC()))
	runs, err := repo.ListRunsByOrg(orgID)
	if err != nil {
		t.Fatalf("ListRunsByOrg returned error: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}
