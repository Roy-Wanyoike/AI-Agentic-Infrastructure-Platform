package deployments

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestDeploymentsStoreSQLMock(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()
	store := NewPostgresStore(db)
	ctx := context.Background()
	orgID, agentID := "org-1", "agent-1"
	createdAt := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)

	// CreateDeployment with health payload + nil superseded_at + canary
	// config (canary_version/canary_weight are the two trailing columns).
	deployment := &Deployment{
		ID: "dep-1", OrganizationID: orgID, AgentID: agentID, Version: 3,
		Environment: EnvironmentStaging, Status: StatusRequested,
		Health: &Health{ErrorRate: 0.25}, CreatedBy: "user-1",
		CreatedAt: createdAt, UpdatedAt: createdAt,
		CanaryVersion: 4, CanaryWeight: 10,
	}
	mock.ExpectExec("INSERT INTO deployments").
		WithArgs("dep-1", orgID, agentID, 3, EnvironmentStaging, StatusRequested, `{"error_rate":0.25,"last_check_at":null}`, "user-1", createdAt, createdAt, nil, 4, 10).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := store.CreateDeployment(ctx, orgID, deployment); err != nil {
		t.Fatalf("CreateDeployment returned error: %v", err)
	}

	// GetDeployment scans health JSON + NULL superseded_at + canary fields.
	mock.ExpectQuery("SELECT id, organization_id, agent_id, version, environment, status").
		WithArgs("dep-1", orgID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "agent_id", "version", "environment", "status", "health", "created_by", "created_at", "updated_at", "superseded_at", "canary_version", "canary_weight"}).
			AddRow("dep-1", orgID, agentID, 3, EnvironmentStaging, StatusHealthy, `{"error_rate":0,"last_check_at":"2025-01-02T03:04:05Z"}`, "user-1", createdAt, createdAt, nil, 4, 10))
	got, err := store.GetDeployment(ctx, orgID, "dep-1")
	if err != nil {
		t.Fatalf("GetDeployment returned error: %v", err)
	}
	if got.Status != StatusHealthy || got.Health == nil || got.Health.ErrorRate != 0 {
		t.Fatalf("unexpected scanned deployment: %+v", got)
	}
	if got.SupersededAt != nil {
		t.Fatalf("expected nil superseded_at, got %v", got.SupersededAt)
	}
	if got.CanaryVersion != 4 || got.CanaryWeight != 10 {
		t.Fatalf("canary fields should round-trip through the store, got %d/%d", got.CanaryVersion, got.CanaryWeight)
	}
	if !got.HasCanary() {
		t.Fatal("HasCanary should be true for canary_version > 0")
	}

	// GetDeployment with no rows -> ErrDeploymentNotFound.
	mock.ExpectQuery("SELECT id, organization_id, agent_id, version, environment, status").
		WithArgs("dep-x", orgID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "agent_id", "version", "environment", "status", "health", "created_by", "created_at", "updated_at", "superseded_at", "canary_version", "canary_weight"}))
	if _, err := store.GetDeployment(ctx, orgID, "dep-x"); !errors.Is(err, ErrDeploymentNotFound) {
		t.Fatalf("expected ErrDeploymentNotFound, got %v", err)
	}

	// ListDeployments (agent filter passed through).
	mock.ExpectQuery("WHERE organization_id = \\$1 AND \\(\\$2 = '' OR agent_id = \\$2\\)").
		WithArgs(orgID, agentID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "agent_id", "version", "environment", "status", "health", "created_by", "created_at", "updated_at", "superseded_at", "canary_version", "canary_weight"}).
			AddRow("dep-1", orgID, agentID, 3, EnvironmentStaging, StatusFailed, nil, "user-1", createdAt, createdAt, createdAt, 0, 0))
	list, err := store.ListDeployments(ctx, orgID, agentID)
	if err != nil {
		t.Fatalf("ListDeployments returned error: %v", err)
	}
	if len(list) != 1 || list[0].Status != StatusFailed || list[0].Health != nil {
		t.Fatalf("unexpected list: %+v", list)
	}
	if list[0].SupersededAt == nil || !list[0].SupersededAt.Equal(createdAt) {
		t.Fatalf("expected superseded_at scanned, got %v", list[0].SupersededAt)
	}
	// Demoted rows carry a cleared canary config (the service zeroes the
	// fields before persisting; the store just round-trips them).
	if list[0].CanaryVersion != 0 || list[0].CanaryWeight != 0 || list[0].HasCanary() {
		t.Fatalf("demoted row should carry no canary config, got %+v", list[0])
	}

	// UpdateDeployment: 1 row affected -> ok; 0 rows -> not found. Canary
	// fields are mutable and written with every update.
	deployment.Status = StatusHealthy
	deployment.CanaryVersion = 0
	deployment.CanaryWeight = 0
	mock.ExpectExec("UPDATE deployments").
		WithArgs(StatusHealthy, `{"error_rate":0.25,"last_check_at":null}`, createdAt, nil, 0, 0, "dep-1", orgID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.UpdateDeployment(ctx, orgID, deployment); err != nil {
		t.Fatalf("UpdateDeployment returned error: %v", err)
	}
	mock.ExpectExec("UPDATE deployments").
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), "dep-missing", orgID).
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := store.UpdateDeployment(ctx, orgID, &Deployment{ID: "dep-missing"}); !errors.Is(err, ErrDeploymentNotFound) {
		t.Fatalf("expected ErrDeploymentNotFound, got %v", err)
	}

	// GetHealthyDeployment: found then empty.
	mock.ExpectQuery("status = 'healthy'").
		WithArgs(orgID, agentID, EnvironmentStaging).
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "agent_id", "version", "environment", "status", "health", "created_by", "created_at", "updated_at", "superseded_at", "canary_version", "canary_weight"}).
			AddRow("dep-1", orgID, agentID, 3, EnvironmentStaging, StatusHealthy, nil, "user-1", createdAt, createdAt, nil, 5, 25))
	healthy, err := store.GetHealthyDeployment(ctx, orgID, agentID, EnvironmentStaging)
	if err != nil {
		t.Fatalf("GetHealthyDeployment returned error: %v", err)
	}
	if healthy == nil || healthy.ID != "dep-1" {
		t.Fatalf("unexpected healthy deployment: %+v", healthy)
	}
	if healthy.CanaryVersion != 5 || healthy.CanaryWeight != 25 {
		t.Fatalf("healthy row should scan canary config, got %d/%d", healthy.CanaryVersion, healthy.CanaryWeight)
	}
	mock.ExpectQuery("status = 'healthy'").
		WithArgs(orgID, agentID, EnvironmentDevelopment).
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "agent_id", "version", "environment", "status", "health", "created_by", "created_at", "updated_at", "superseded_at", "canary_version", "canary_weight"}))
	none, err := store.GetHealthyDeployment(ctx, orgID, agentID, EnvironmentDevelopment)
	if err != nil || none != nil {
		t.Fatalf("expected nil healthy deployment, got %v/%v", none, err)
	}

	// GetPreviousHealthyDeployment.
	mock.ExpectQuery("superseded_at IS NOT NULL AND id <> \\$4").
		WithArgs(orgID, agentID, EnvironmentStaging, "dep-9").
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "agent_id", "version", "environment", "status", "health", "created_by", "created_at", "updated_at", "superseded_at", "canary_version", "canary_weight"}).
			AddRow("dep-1", orgID, agentID, 3, EnvironmentStaging, StatusFailed, nil, "user-1", createdAt, createdAt, createdAt, 0, 0))
	previous, err := store.GetPreviousHealthyDeployment(ctx, orgID, agentID, EnvironmentStaging, "dep-9")
	if err != nil {
		t.Fatalf("GetPreviousHealthyDeployment returned error: %v", err)
	}
	if previous == nil || previous.ID != "dep-1" {
		t.Fatalf("unexpected previous healthy: %+v", previous)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}
