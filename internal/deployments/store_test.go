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
		Promotion: &CanaryPromotion{Policy: AgentPromotionPolicy{MinPassRate: 0.8, MinCanaryRuns: 3, MaxP95LatencyMs: 0, MaxCostPerRunCents: 0}, WindowStart: createdAt},
	}
	mock.ExpectExec("INSERT INTO deployments").
		WithArgs("dep-1", orgID, agentID, 3, EnvironmentStaging, StatusRequested, `{"error_rate":0.25,"last_check_at":null}`, "user-1", createdAt, createdAt, nil, 4, 10,
			`{"policy":{"min_pass_rate":0.8,"min_canary_runs":3,"max_p95_latency_ms":0,"max_cost_per_run_cents":0},"window_start":"2025-01-02T03:04:05Z"}`).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := store.CreateDeployment(ctx, orgID, deployment); err != nil {
		t.Fatalf("CreateDeployment returned error: %v", err)
	}

	// GetDeployment scans health JSON + NULL superseded_at + canary fields
	// + the canary_promotion JSONB (issue #51).
	mock.ExpectQuery("SELECT id, organization_id, agent_id, version, environment, status").
		WithArgs("dep-1", orgID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "agent_id", "version", "environment", "status", "health", "created_by", "created_at", "updated_at", "superseded_at", "canary_version", "canary_weight", "canary_promotion"}).
			AddRow("dep-1", orgID, agentID, 3, EnvironmentStaging, StatusHealthy, `{"error_rate":0,"last_check_at":"2025-01-02T03:04:05Z"}`, "user-1", createdAt, createdAt, nil, 4, 10,
				`{"policy":{"min_pass_rate":0.8,"min_canary_runs":3},"window_start":"2025-01-02T03:04:05Z","decision":{"action":"promote","reason":"pass_rate 0.95 >= 0.80 → promote","decided_at":"2025-01-02T03:04:05Z","runs_counted":3,"pass_rate":0.95,"p95_latency_ms":10,"avg_cost_cents":2,"policy":{"min_pass_rate":0.8,"min_canary_runs":3}}}`))
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
	// Issue #51: the promotion state round-trips through the JSONB column.
	if got.Promotion == nil || got.Promotion.Policy.MinPassRate != 0.8 || got.Promotion.Policy.MinCanaryRuns != 3 {
		t.Fatalf("promotion policy should round-trip through the store, got %+v", got.Promotion)
	}
	if got.Promotion.Decision == nil || got.Promotion.Decision.Action != CanaryDecisionPromote ||
		got.Promotion.Decision.Reason != "pass_rate 0.95 >= 0.80 → promote" {
		t.Fatalf("recorded decision should round-trip through the store, got %+v", got.Promotion.Decision)
	}

	// GetDeployment with no rows -> ErrDeploymentNotFound.
	mock.ExpectQuery("SELECT id, organization_id, agent_id, version, environment, status").
		WithArgs("dep-x", orgID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "agent_id", "version", "environment", "status", "health", "created_by", "created_at", "updated_at", "superseded_at", "canary_version", "canary_weight", "canary_promotion"}))
	if _, err := store.GetDeployment(ctx, orgID, "dep-x"); !errors.Is(err, ErrDeploymentNotFound) {
		t.Fatalf("expected ErrDeploymentNotFound, got %v", err)
	}

	// ListDeployments (agent filter passed through).
	mock.ExpectQuery("WHERE organization_id = \\$1 AND \\(\\$2 = '' OR agent_id = \\$2\\)").
		WithArgs(orgID, agentID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "agent_id", "version", "environment", "status", "health", "created_by", "created_at", "updated_at", "superseded_at", "canary_version", "canary_weight", "canary_promotion"}).
			AddRow("dep-1", orgID, agentID, 3, EnvironmentStaging, StatusFailed, nil, "user-1", createdAt, createdAt, createdAt, 0, 0, nil))
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
	deployment.Promotion = nil // promote/abort paths keep policy+decision; this test clears everything
	mock.ExpectExec("UPDATE deployments").
		WithArgs(StatusHealthy, `{"error_rate":0.25,"last_check_at":null}`, createdAt, nil, 0, 0, nil, "dep-1", orgID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.UpdateDeployment(ctx, orgID, deployment); err != nil {
		t.Fatalf("UpdateDeployment returned error: %v", err)
	}
	mock.ExpectExec("UPDATE deployments").
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), "dep-missing", orgID).
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := store.UpdateDeployment(ctx, orgID, &Deployment{ID: "dep-missing"}); !errors.Is(err, ErrDeploymentNotFound) {
		t.Fatalf("expected ErrDeploymentNotFound, got %v", err)
	}

	// GetHealthyDeployment: found then empty.
	mock.ExpectQuery("status = 'healthy'").
		WithArgs(orgID, agentID, EnvironmentStaging).
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "agent_id", "version", "environment", "status", "health", "created_by", "created_at", "updated_at", "superseded_at", "canary_version", "canary_weight", "canary_promotion"}).
			AddRow("dep-1", orgID, agentID, 3, EnvironmentStaging, StatusHealthy, nil, "user-1", createdAt, createdAt, nil, 5, 25, nil))
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
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "agent_id", "version", "environment", "status", "health", "created_by", "created_at", "updated_at", "superseded_at", "canary_version", "canary_weight", "canary_promotion"}))
	none, err := store.GetHealthyDeployment(ctx, orgID, agentID, EnvironmentDevelopment)
	if err != nil || none != nil {
		t.Fatalf("expected nil healthy deployment, got %v/%v", none, err)
	}

	// GetPreviousHealthyDeployment.
	mock.ExpectQuery("superseded_at IS NOT NULL AND id <> \\$4").
		WithArgs(orgID, agentID, EnvironmentStaging, "dep-9").
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "agent_id", "version", "environment", "status", "health", "created_by", "created_at", "updated_at", "superseded_at", "canary_version", "canary_weight", "canary_promotion"}).
			AddRow("dep-1", orgID, agentID, 3, EnvironmentStaging, StatusFailed, nil, "user-1", createdAt, createdAt, createdAt, 0, 0, nil))
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
