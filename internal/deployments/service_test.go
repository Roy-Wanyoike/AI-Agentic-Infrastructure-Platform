package deployments

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"agentos/internal/agents"
)

// fakeResolver allow-lists (agentID, version) pairs considered published
// (stable deploy targets) and implements VersionExistenceChecker so canary
// targets are validated for existence within (org, agent) - any status -
// exactly like the real agents.VersionsService.
type fakeResolver struct {
	allowed  map[string]bool // published (usable as the STABLE version)
	existing map[string]bool // exists in any status (usable as a CANARY)
}

func (f *fakeResolver) ResolvePublishedVersion(_ context.Context, _, agentID string, version int) error {
	if f.allowed == nil {
		return nil
	}
	if f.allowed[agentID+"/"+strconv.Itoa(version)] {
		return nil
	}
	return errors.New("version not published")
}

func (f *fakeResolver) GetVersionCtx(_ context.Context, _, agentID string, version int) (*agents.ConfigVersion, error) {
	key := agentID + "/" + strconv.Itoa(version)
	exists := f.existing != nil && f.existing[key]
	if !exists && f.existing == nil {
		// No explicit existence list: fall back to the published list
		// (or allow everything when that is unset too).
		exists = f.allowed == nil || f.allowed[key]
	}
	if !exists {
		return nil, agents.ErrVersionNotFound
	}
	return &agents.ConfigVersion{AgentID: agentID, Version: version, Status: agents.VersionStatusDraft}, nil
}

func newDeploymentsFixture(resolver VersionChecker) *Service {
	return NewService(resolver)
}

func deployAndPromote(t *testing.T, svc *Service, orgID, agentID string, version int, environment string) *Deployment {
	t.Helper()
	ctx := context.Background()
	deployment, err := svc.CreateDeploymentCtx(ctx, orgID, agentID, version, environment, "user-1")
	if err != nil {
		t.Fatalf("CreateDeploymentCtx(v%d) returned error: %v", version, err)
	}
	for range 3 {
		if _, err := svc.PromoteDeploymentCtx(ctx, orgID, deployment.ID); err != nil {
			t.Fatalf("PromoteDeploymentCtx(v%d) returned error: %v", version, err)
		}
	}
	return deployment
}

func TestDeploymentLifecycleTransitions(t *testing.T) {
	ctx := context.Background()
	svc := newDeploymentsFixture(nil)
	orgID, agentID := "org-1", "agent-1"

	deployment, err := svc.CreateDeploymentCtx(ctx, orgID, agentID, 2, EnvironmentStaging, "user-1")
	if err != nil {
		t.Fatalf("CreateDeploymentCtx returned error: %v", err)
	}
	if deployment.Status != StatusRequested {
		t.Fatalf("new deployments start requested, got %q", deployment.Status)
	}

	transitions := []string{StatusValidated, StatusDeploying, StatusHealthy}
	for _, want := range transitions {
		got, err := svc.PromoteDeploymentCtx(ctx, orgID, deployment.ID)
		if err != nil {
			t.Fatalf("PromoteDeploymentCtx -> %s returned error: %v", want, err)
		}
		if got.Status != want {
			t.Fatalf("expected status %q after promote, got %q", want, got.Status)
		}
	}

	// healthy is terminal.
	if _, err := svc.PromoteDeploymentCtx(ctx, orgID, deployment.ID); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("promote on healthy should be ErrInvalidTransition, got %v", err)
	}
	// healthy carries a last_check_at stamp.
	if deployment.Health == nil || deployment.Health.LastCheckAt == nil {
		t.Fatalf("healthy deployment should stamp health.last_check_at, got %+v", deployment.Health)
	}

	// fail is rejected on terminal states.
	if _, err := svc.FailDeploymentCtx(ctx, orgID, deployment.ID, "nope"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("fail on healthy should be ErrInvalidTransition, got %v", err)
	}
}

func TestDeploymentFailFromRequested(t *testing.T) {
	ctx := context.Background()
	svc := newDeploymentsFixture(nil)

	deployment, err := svc.CreateDeploymentCtx(ctx, "org-1", "agent-1", 1, EnvironmentDevelopment, "user-1")
	if err != nil {
		t.Fatalf("CreateDeploymentCtx returned error: %v", err)
	}
	failed, err := svc.FailDeploymentCtx(ctx, "org-1", deployment.ID, "boom")
	if err != nil {
		t.Fatalf("FailDeploymentCtx returned error: %v", err)
	}
	if failed.Status != StatusFailed || failed.Health.Error != "boom" {
		t.Fatalf("unexpected failed deployment: %+v", failed)
	}
	if _, err := svc.PromoteDeploymentCtx(ctx, "org-1", deployment.ID); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("promote on failed should be ErrInvalidTransition, got %v", err)
	}
}

func TestDeploymentCreateValidation(t *testing.T) {
	ctx := context.Background()
	svc := newDeploymentsFixture(&fakeResolver{allowed: map[string]bool{"agent-1/2": true}})

	if _, err := svc.CreateDeploymentCtx(ctx, "org-1", "agent-1", 2, "chaos", "user-1"); !errors.Is(err, ErrInvalidEnvironment) {
		t.Fatalf("unknown environment should be ErrInvalidEnvironment, got %v", err)
	}
	if _, err := svc.CreateDeploymentCtx(ctx, "org-1", "agent-1", 0, EnvironmentProduction, "user-1"); !errors.Is(err, ErrVersionNotDeployable) {
		t.Fatalf("version 0 should be ErrVersionNotDeployable, got %v", err)
	}
	// draft version rejected through the resolver
	if _, err := svc.CreateDeploymentCtx(ctx, "org-1", "agent-1", 9, EnvironmentProduction, "user-1"); !errors.Is(err, ErrVersionNotDeployable) {
		t.Fatalf("unpublished version should be ErrVersionNotDeployable, got %v", err)
	}
	if _, err := svc.CreateDeploymentCtx(ctx, "org-1", "agent-1", 2, EnvironmentProduction, "user-1"); err != nil {
		t.Fatalf("published version should deploy, got %v", err)
	}
}

func TestOneHealthyDeploymentPerAgentEnvironment(t *testing.T) {
	ctx := context.Background()
	svc := newDeploymentsFixture(nil)
	orgID, agentID := "org-1", "agent-1"

	d1 := deployAndPromote(t, svc, orgID, agentID, 1, EnvironmentProduction)
	d2 := deployAndPromote(t, svc, orgID, agentID, 2, EnvironmentProduction)

	healthy, err := svc.healthyDeployment(ctx, orgID, agentID, EnvironmentProduction)
	if err != nil {
		t.Fatalf("healthyDeployment returned error: %v", err)
	}
	if healthy == nil || healthy.ID != d2.ID {
		t.Fatalf("expected the newest deployment healthy, got %+v", healthy)
	}
	// The previous healthy row is demoted and stamped as superseded.
	if d1.Status != StatusFailed || d1.SupersededAt == nil {
		t.Fatalf("superseded deployment should be failed+superseded, got %+v", d1)
	}
	if d1.Health == nil || d1.Health.SupersededBy != d2.ID {
		t.Fatalf("superseded deployment should reference its replacement, got %+v", d1.Health)
	}
	// The same agent in another environment is unaffected.
	d3 := deployAndPromote(t, svc, orgID, agentID, 3, EnvironmentStaging)
	if d3.Status != StatusHealthy {
		t.Fatalf("staging deployment should be healthy independently, got %q", d3.Status)
	}
	// Foreign tenant cannot see or promote the deployment.
	if _, err := svc.PromoteDeploymentCtx(ctx, "org-2", d2.ID); !errors.Is(err, ErrDeploymentNotFound) {
		t.Fatalf("foreign tenant promote should be ErrDeploymentNotFound, got %v", err)
	}
	if _, err := svc.GetDeploymentCtx(ctx, "org-2", d2.ID); !errors.Is(err, ErrDeploymentNotFound) {
		t.Fatalf("foreign tenant get should be ErrDeploymentNotFound, got %v", err)
	}
}

func TestRollbackPicksPreviousHealthy(t *testing.T) {
	ctx := context.Background()
	svc := newDeploymentsFixture(nil)
	orgID, agentID := "org-1", "agent-1"

	d1 := deployAndPromote(t, svc, orgID, agentID, 1, EnvironmentProduction)
	d2 := deployAndPromote(t, svc, orgID, agentID, 2, EnvironmentProduction)
	d3 := deployAndPromote(t, svc, orgID, agentID, 3, EnvironmentProduction)

	rollback, version, err := svc.RollbackDeploymentCtx(ctx, orgID, d3.ID, "user-2")
	if err != nil {
		t.Fatalf("RollbackDeploymentCtx returned error: %v", err)
	}
	if version != 2 {
		t.Fatalf("expected rollback to version 2, got %d", version)
	}
	if rollback.Version != 2 || rollback.Status != StatusHealthy {
		t.Fatalf("rollback should create a healthy v2 deployment, got %+v", rollback)
	}
	healthy, _ := svc.healthyDeployment(ctx, orgID, agentID, EnvironmentProduction)
	if healthy == nil || healthy.ID != rollback.ID {
		t.Fatalf("environment should point at the rollback deployment, got %+v", healthy)
	}
	if d3.Status != StatusFailed || d3.SupersededAt == nil {
		t.Fatalf("d3 should be superseded after rollback, got %+v", d3)
	}
	if d1.ID == rollback.ID || d2.ID == rollback.ID {
		t.Fatal("rollback must create a NEW deployment row, not mutate history")
	}
	// Rolling back again walks further back in healthy history (to v3 here,
	// the most recent other healthy deployment relative to the current v2).
	rollback2, version2, err := svc.RollbackDeploymentCtx(ctx, orgID, rollback.ID, "user-2")
	if err != nil {
		t.Fatalf("second RollbackDeploymentCtx returned error: %v", err)
	}
	if version2 != 3 || rollback2.Version != 3 {
		t.Fatalf("second rollback should target version 3, got %d", version2)
	}
	// List returns the full ledger for the agent.
	list, err := svc.ListDeploymentsCtx(ctx, orgID, agentID)
	if err != nil {
		t.Fatalf("ListDeploymentsCtx returned error: %v", err)
	}
	if len(list) != 5 {
		t.Fatalf("expected 5 deployment rows (3 + 2 rollbacks), got %d", len(list))
	}
}

func TestRollbackNoPreviousHealthy(t *testing.T) {
	ctx := context.Background()
	svc := newDeploymentsFixture(nil)

	d1 := deployAndPromote(t, svc, "org-1", "agent-1", 1, EnvironmentProduction)
	if _, _, err := svc.RollbackDeploymentCtx(ctx, "org-1", d1.ID, "user-2"); !errors.Is(err, ErrNoPreviousHealthy) {
		t.Fatalf("rollback with no previous healthy should be ErrNoPreviousHealthy, got %v", err)
	}
}

func TestRollbackIdempotentWhenAlreadyServing(t *testing.T) {
	ctx := context.Background()
	svc := newDeploymentsFixture(nil)
	orgID, agentID := "org-1", "agent-1"

	d1 := deployAndPromote(t, svc, orgID, agentID, 1, EnvironmentProduction)
	// v2 never reaches healthy (failed mid-flight).
	d2, err := svc.CreateDeploymentCtx(ctx, orgID, agentID, 2, EnvironmentProduction, "user-1")
	if err != nil {
		t.Fatalf("CreateDeploymentCtx returned error: %v", err)
	}
	if _, err := svc.FailDeploymentCtx(ctx, orgID, d2.ID, "deploy exploded"); err != nil {
		t.Fatalf("FailDeploymentCtx returned error: %v", err)
	}
	// Rollback(d2): previous healthy is v1, which is STILL the healthy row ->
	// idempotent re-point, no new row.
	got, version, err := svc.RollbackDeploymentCtx(ctx, orgID, d2.ID, "user-2")
	if err != nil {
		t.Fatalf("RollbackDeploymentCtx returned error: %v", err)
	}
	if version != 1 || got.ID != d1.ID {
		t.Fatalf("expected idempotent rollback to existing v1 deployment, got %+v (%d)", got, version)
	}
}

func TestDeploymentTimestampsRFC3339UTC(t *testing.T) {
	svc := newDeploymentsFixture(nil)
	deployment, err := svc.CreateDeploymentCtx(context.Background(), "org-1", "agent-1", 1, EnvironmentDevelopment, "user-1")
	if err != nil {
		t.Fatalf("CreateDeploymentCtx returned error: %v", err)
	}
	if deployment.CreatedAt.Location() != time.UTC {
		t.Fatalf("timestamps must be UTC, got %v", deployment.CreatedAt.Location())
	}
}
