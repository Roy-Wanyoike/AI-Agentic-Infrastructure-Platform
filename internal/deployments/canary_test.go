package deployments

// Canary service tests (issue #13): validation, transitions, and the
// deterministic per-agent split (weight 0 -> always stable, 100 -> always
// canary, ~50 -> stable per-agent side across two agent keys).

import (
	"context"
	"errors"
	"testing"
)

func canaryFixture(resolver VersionChecker) *Service {
	return NewService(resolver)
}

// healthyWithCanary creates a deployment (optionally with a staged canary
// config) and promotes it to healthy; canaryVersion 0 = no canary.
func healthyWithCanary(t *testing.T, svc *Service, orgID, agentID string, version, canaryVersion, canaryWeight int, environment string) *Deployment {
	t.Helper()
	ctx := context.Background()
	var (
		deployment *Deployment
		err        error
	)
	if canaryVersion > 0 {
		deployment, err = svc.CreateCanaryDeploymentCtx(ctx, orgID, agentID, version, canaryVersion, canaryWeight, environment, "user-1")
	} else {
		deployment, err = svc.CreateDeploymentCtx(ctx, orgID, agentID, version, environment, "user-1")
	}
	if err != nil {
		t.Fatalf("create deployment (v%d canary v%d@%d) returned error: %v", version, canaryVersion, canaryWeight, err)
	}
	for range 3 {
		if _, err := svc.PromoteDeploymentCtx(ctx, orgID, deployment.ID); err != nil {
			t.Fatalf("promote to healthy returned error: %v", err)
		}
	}
	return deployment
}

func TestCanaryDeterministicSelection(t *testing.T) {
	ctx := context.Background()
	svc := canaryFixture(&fakeResolver{
		allowed:  map[string]bool{"agent-a/2": true, "agent-a/3": true},
		existing: map[string]bool{"agent-a/2": true, "agent-a/3": true},
	})
	orgID, agentID := "org-split", "agent-a"
	environment := EnvironmentProduction

	// No canary at all: the stable version resolves.
	stable := healthyWithCanary(t, svc, orgID, agentID, 2, 0, 0, environment)
	for range 5 {
		got, err := svc.ResolveVersionCtx(ctx, orgID, agentID, environment)
		if err != nil || got != 2 {
			t.Fatalf("no canary should resolve stable v2, got %d/%v", got, err)
		}
	}

	// Attach canary v3 with weight 0: configured, but ALWAYS stable.
	if _, err := svc.SetCanaryVersionCtx(ctx, orgID, stable.ID, 3); err != nil {
		t.Fatalf("SetCanaryVersionCtx returned error: %v", err)
	}
	for range 5 {
		got, err := svc.ResolveVersionCtx(ctx, orgID, agentID, environment)
		if err != nil || got != 2 {
			t.Fatalf("weight 0 must always resolve stable v2, got %d/%v", got, err)
		}
	}

	// Weight 100: ALWAYS canary.
	if _, err := svc.SetCanaryWeightCtx(ctx, orgID, stable.ID, 100); err != nil {
		t.Fatalf("SetCanaryWeightCtx(100) returned error: %v", err)
	}
	for range 5 {
		got, err := svc.ResolveVersionCtx(ctx, orgID, agentID, environment)
		if err != nil || got != 3 {
			t.Fatalf("weight 100 must always resolve canary v3, got %d/%v", got, err)
		}
	}

	// ~50%: the side is deterministic PER AGENT (same inputs -> same side)
	// and both sides are exercised across two different agent keys.
	// Pick two agent keys whose buckets fall on opposite sides of 50.
	sideA, sideB := "agent-a", ""
	for i := 0; ; i++ {
		candidate := "agent-a" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		if candidate == sideA {
			continue
		}
		if (canaryBucket(orgID, candidate) < 50) != (canaryBucket(orgID, sideA) < 50) {
			sideB = candidate
			break
		}
		if i > 500 {
			t.Fatal("no opposite-side agent key found (bucket distribution broken)")
		}
	}
	bucketA, bucketB := canaryBucket(orgID, sideA), canaryBucket(orgID, sideB)
	if (bucketA < 50) == (bucketB < 50) {
		t.Fatalf("fixture broken: buckets %d/%d on the same side", bucketA, bucketB)
	}
	// sideA already serves a canary v3 @50 (weight set above); give sideB
	// its own healthy deployment with the same split (own fixture so the
	// resolver allow-lists sideB's versions).
	svcB := canaryFixture(&fakeResolver{
		allowed:  map[string]bool{sideB + "/2": true, sideB + "/3": true},
		existing: map[string]bool{sideB + "/2": true, sideB + "/3": true},
	})
	depB := healthyWithCanary(t, svcB, orgID, sideB, 2, 3, 50, environment)
	if depB.CanaryWeight != 50 {
		t.Fatalf("expected staged canary weight 50, got %d", depB.CanaryWeight)
	}
	gotB, err := svcB.ResolveVersionCtx(ctx, orgID, sideB, environment)
	if err != nil {
		t.Fatalf("ResolveVersionCtx(sideB) returned error: %v", err)
	}
	wantB := 2
	if bucketB < 50 {
		wantB = 3
	}
	if gotB != wantB {
		t.Fatalf("agent %q (bucket %d) should resolve v%d at weight 50, got v%d", sideB, bucketB, wantB, gotB)
	}
	gotA, err := svc.ResolveVersionCtx(ctx, orgID, sideA, environment)
	if err != nil {
		t.Fatalf("ResolveVersionCtx(sideA) returned error: %v", err)
	}
	wantA := 2
	if bucketA < 50 {
		wantA = 3
	}
	if gotA != wantA {
		t.Fatalf("agent %q (bucket %d) should resolve v%d at weight 50, got v%d", sideA, bucketA, wantA, gotA)
	}
	// Determinism: repeating the resolution never flips the side.
	for range 10 {
		if again, _ := svc.ResolveVersionCtx(ctx, orgID, sideA, environment); again != wantA {
			t.Fatalf("resolution must be deterministic, got %d then %d", wantA, again)
		}
		if again, _ := svcB.ResolveVersionCtx(ctx, orgID, sideB, environment); again != wantB {
			t.Fatalf("resolution must be deterministic, got %d then %d", wantB, again)
		}
	}
	if (gotA == 3) == (gotB == 3) {
		t.Fatalf("expected both sides exercised across the two agent keys, got v%d and v%d", gotA, gotB)
	}
}

func TestCanaryWeightValidation(t *testing.T) {
	ctx := context.Background()
	svc := canaryFixture(&fakeResolver{
		allowed:  map[string]bool{"agent-a/2": true, "agent-a/3": true},
		existing: map[string]bool{"agent-a/2": true, "agent-a/3": true},
	})
	orgID, agentID := "org-1", "agent-a"

	// Create-time validation: weights outside 0-100 are rejected.
	for _, weight := range []int{-1, 101, 1000} {
		if _, err := svc.CreateCanaryDeploymentCtx(ctx, orgID, agentID, 2, 3, weight, EnvironmentProduction, "user-1"); !errors.Is(err, ErrInvalidCanaryWeight) {
			t.Fatalf("create with weight %d should be ErrInvalidCanaryWeight, got %v", weight, err)
		}
	}
	deployment := healthyWithCanary(t, svc, orgID, agentID, 2, 3, 10, EnvironmentProduction)

	// SetCanaryWeightCtx rejects out-of-range weights (no silent clamp).
	for _, weight := range []int{-1, 101} {
		if _, err := svc.SetCanaryWeightCtx(ctx, orgID, deployment.ID, weight); !errors.Is(err, ErrInvalidCanaryWeight) {
			t.Fatalf("SetCanaryWeightCtx(%d) should be ErrInvalidCanaryWeight, got %v", weight, err)
		}
	}
	// Boundary weights are valid and clamp the split domain.
	if got, err := svc.SetCanaryWeightCtx(ctx, orgID, deployment.ID, 0); err != nil || got.CanaryWeight != 0 {
		t.Fatalf("weight 0 is valid, got %v/%v", got, err)
	}
	if got, err := svc.SetCanaryWeightCtx(ctx, orgID, deployment.ID, 100); err != nil || got.CanaryWeight != 100 {
		t.Fatalf("weight 100 is valid, got %v/%v", got, err)
	}
}

func TestCanaryVersionValidation(t *testing.T) {
	ctx := context.Background()
	svc := canaryFixture(&fakeResolver{
		allowed: map[string]bool{
			"agent-a/2": true, // published, of agent-a (stable target)
		},
		existing: map[string]bool{
			"agent-a/2": true, "agent-a/3": true, // draft v3 of agent-a
			"agent-b/9": true, // exists, but of ANOTHER agent
		},
	})
	orgID := "org-1"

	// Canary version equal to the stable version is meaningless.
	if _, err := svc.CreateCanaryDeploymentCtx(ctx, orgID, "agent-a", 2, 2, 10, EnvironmentProduction, "user-1"); !errors.Is(err, ErrInvalidCanaryVersion) {
		t.Fatalf("canary == stable should be ErrInvalidCanaryVersion, got %v", err)
	}
	// Non-positive canary version.
	if _, err := svc.CreateCanaryDeploymentCtx(ctx, orgID, "agent-a", 2, 0, 10, EnvironmentProduction, "user-1"); !errors.Is(err, ErrInvalidCanaryVersion) {
		t.Fatalf("canary version 0 should be ErrInvalidCanaryVersion, got %v", err)
	}
	// A DRAFT version of the same agent is a valid canary target (only one
	// version per agent can be published at a time, so canaries ship from
	// unpublished snapshots).
	if _, err := svc.CreateCanaryDeploymentCtx(ctx, orgID, "agent-a", 2, 3, 10, EnvironmentProduction, "user-1"); err != nil {
		t.Fatalf("draft canary of the same agent should deploy, got %v", err)
	}
	// A version of ANOTHER agent does not exist under agent-a -> rejected.
	if _, err := svc.CreateCanaryDeploymentCtx(ctx, orgID, "agent-a", 2, 9, 10, EnvironmentProduction, "user-1"); !errors.Is(err, ErrInvalidCanaryVersion) {
		t.Fatalf("cross-agent canary should be ErrInvalidCanaryVersion, got %v", err)
	}
	// A version that exists nowhere is rejected too.
	if _, err := svc.CreateCanaryDeploymentCtx(ctx, orgID, "agent-a", 2, 99, 10, EnvironmentProduction, "user-1"); !errors.Is(err, ErrInvalidCanaryVersion) {
		t.Fatalf("unknown canary should be ErrInvalidCanaryVersion, got %v", err)
	}

	// Same checks through SetCanaryVersionCtx on a healthy row.
	deployment := healthyWithCanary(t, svc, orgID, "agent-a", 2, 3, 10, EnvironmentProduction)
	if _, err := svc.SetCanaryVersionCtx(ctx, orgID, deployment.ID, 2); !errors.Is(err, ErrInvalidCanaryVersion) {
		t.Fatalf("set canary == stable should be ErrInvalidCanaryVersion, got %v", err)
	}
	if _, err := svc.SetCanaryVersionCtx(ctx, orgID, deployment.ID, 0); !errors.Is(err, ErrInvalidCanaryVersion) {
		t.Fatalf("set canary 0 should be ErrInvalidCanaryVersion, got %v", err)
	}
	if _, err := svc.SetCanaryVersionCtx(ctx, orgID, deployment.ID, 9); !errors.Is(err, ErrInvalidCanaryVersion) {
		t.Fatalf("set cross-agent canary should be ErrInvalidCanaryVersion, got %v", err)
	}
	// Cross-tenant row lookups stay not-found.
	if _, err := svc.SetCanaryVersionCtx(ctx, "org-2", deployment.ID, 3); !errors.Is(err, ErrDeploymentNotFound) {
		t.Fatalf("foreign-tenant canary set should be ErrDeploymentNotFound, got %v", err)
	}
}

func TestCanaryPromoteAndAbort(t *testing.T) {
	ctx := context.Background()
	svc := canaryFixture(&fakeResolver{
		allowed:  map[string]bool{"agent-a/2": true, "agent-a/3": true},
		existing: map[string]bool{"agent-a/2": true, "agent-a/3": true},
	})
	orgID, agentID := "org-1", "agent-a"
	environment := EnvironmentProduction

	// Promote/abort/weight without a canary configured.
	plain := healthyWithCanary(t, svc, orgID, agentID, 2, 0, 0, environment)
	if _, err := svc.PromoteCanaryCtx(ctx, orgID, plain.ID); !errors.Is(err, ErrNoCanary) {
		t.Fatalf("promote without canary should be ErrNoCanary, got %v", err)
	}
	if _, err := svc.AbortCanaryCtx(ctx, orgID, plain.ID); !errors.Is(err, ErrNoCanary) {
		t.Fatalf("abort without canary should be ErrNoCanary, got %v", err)
	}
	if _, err := svc.SetCanaryWeightCtx(ctx, orgID, plain.ID, 10); !errors.Is(err, ErrNoCanary) {
		t.Fatalf("set weight without canary should be ErrNoCanary, got %v", err)
	}

	// Full canary flow: attach v3, ramp to 50, verify the split, promote.
	deployment := healthyWithCanary(t, svc, orgID, agentID, 2, 3, 10, environment)
	if _, err := svc.SetCanaryWeightCtx(ctx, orgID, deployment.ID, 50); err != nil {
		t.Fatalf("SetCanaryWeightCtx returned error: %v", err)
	}
	resolved, err := svc.ResolveVersionCtx(ctx, orgID, agentID, environment)
	if err != nil {
		t.Fatalf("ResolveVersionCtx returned error: %v", err)
	}
	wantSide := 2
	if canaryBucket(orgID, agentID) < 50 {
		wantSide = 3
	}
	if resolved != wantSide {
		t.Fatalf("weight 50 should resolve v%d for this agent, got v%d", wantSide, resolved)
	}

	promoted, err := svc.PromoteCanaryCtx(ctx, orgID, deployment.ID)
	if err != nil {
		t.Fatalf("PromoteCanaryCtx returned error: %v", err)
	}
	if promoted.Version != 3 || promoted.CanaryVersion != 0 || promoted.CanaryWeight != 0 {
		t.Fatalf("promote should swap stable=canary and clear config, got %+v", promoted)
	}
	if promoted.Status != StatusHealthy {
		t.Fatalf("promote must keep the row healthy (no lifecycle transition), got %q", promoted.Status)
	}
	for range 5 {
		got, err := svc.ResolveVersionCtx(ctx, orgID, agentID, environment)
		if err != nil || got != 3 {
			t.Fatalf("after promote v3 is stable and always resolves, got %d/%v", got, err)
		}
	}

	// Abort on the (now canary-less) row is ErrNoCanary again; attach + abort clears config and keeps stable.
	if _, err := svc.SetCanaryVersionCtx(ctx, orgID, deployment.ID, 2); err != nil {
		t.Fatalf("attach canary v2 returned error: %v", err)
	}
	aborted, err := svc.AbortCanaryCtx(ctx, orgID, deployment.ID)
	if err != nil {
		t.Fatalf("AbortCanaryCtx returned error: %v", err)
	}
	if aborted.Version != 3 || aborted.CanaryVersion != 0 || aborted.CanaryWeight != 0 {
		t.Fatalf("abort must clear canary and keep stable v3, got %+v", aborted)
	}
	if got, _ := svc.ResolveVersionCtx(ctx, orgID, agentID, environment); got != 3 {
		t.Fatalf("after abort stable v3 serves traffic, got v%d", got)
	}
}

func TestCanaryRequiresHealthyDeployment(t *testing.T) {
	ctx := context.Background()
	svc := canaryFixture(&fakeResolver{
		allowed:  map[string]bool{"agent-a/2": true},
		existing: map[string]bool{"agent-a/2": true, "agent-a/3": true},
	})
	orgID, agentID := "org-1", "agent-a"

	// A fresh (requested) deployment with a STAGED canary config: the split
	// does not serve traffic yet, and traffic-changing ops are rejected.
	staged, err := svc.CreateCanaryDeploymentCtx(ctx, orgID, agentID, 2, 3, 25, EnvironmentProduction, "user-1")
	if err != nil {
		t.Fatalf("CreateCanaryDeploymentCtx returned error: %v", err)
	}
	if !staged.HasCanary() {
		t.Fatal("create-with-canary should stage the canary config")
	}
	// The staged canary NEVER serves traffic while the row is not healthy:
	// production has no healthy deployment at all, and a healthy row in another
	// environment resolves its own stable version.
	if _, err := svc.ResolveVersionCtx(ctx, orgID, agentID, staged.Environment); !errors.Is(err, ErrNoServingDeployment) {
		t.Fatalf("staged canary row must not serve traffic, got %v", err)
	}
	stableElsewhere := healthyWithCanary(t, svc, orgID, agentID, 2, 0, 0, EnvironmentStaging)
	if got, err := svc.ResolveVersionCtx(ctx, orgID, agentID, stableElsewhere.Environment); err != nil || got != 2 {
		t.Fatalf("staging should resolve its stable v2, got %d/%v", got, err)
	}
	if _, err := svc.PromoteCanaryCtx(ctx, orgID, staged.ID); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("promote canary on requested row should be ErrInvalidTransition, got %v", err)
	}
	if _, err := svc.AbortCanaryCtx(ctx, orgID, staged.ID); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("abort canary on requested row should be ErrInvalidTransition, got %v", err)
	}
	if _, err := svc.SetCanaryWeightCtx(ctx, orgID, staged.ID, 50); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("set weight on requested row should be ErrInvalidTransition, got %v", err)
	}

	// A canary-less non-healthy row reports the missing canary first.
	plain, err := svc.CreateDeploymentCtx(ctx, orgID, agentID, 2, EnvironmentStaging, "user-1")
	if err != nil {
		t.Fatalf("CreateDeploymentCtx returned error: %v", err)
	}
	if _, err := svc.SetCanaryWeightCtx(ctx, orgID, plain.ID, 50); !errors.Is(err, ErrNoCanary) {
		t.Fatalf("weight on canary-less requested row should be ErrNoCanary, got %v", err)
	}
}

func TestCanaryClearedOnDemote(t *testing.T) {
	ctx := context.Background()
	svc := canaryFixture(&fakeResolver{
		allowed:  map[string]bool{"agent-a/1": true, "agent-a/2": true, "agent-a/3": true},
		existing: map[string]bool{"agent-a/1": true, "agent-a/2": true, "agent-a/3": true},
	})
	orgID, agentID := "org-1", "agent-a"
	environment := EnvironmentProduction

	old := healthyWithCanary(t, svc, orgID, agentID, 1, 2, 50, environment)
	if got, err := svc.ResolveVersionCtx(ctx, orgID, agentID, environment); err != nil {
		t.Fatalf("ResolveVersionCtx returned error: %v", err)
	} else if got != 1 && got != 2 {
		t.Fatalf("split should resolve v1 or v2, got v%d", got)
	}

	// A newer deployment reaches healthy -> the old healthy row (with its
	// canary config) is demoted; the new row serves with no canary.
	newer := healthyWithCanary(t, svc, orgID, agentID, 3, 0, 0, environment)
	if old.Status != StatusFailed || old.SupersededAt == nil {
		t.Fatalf("previous healthy row should be demoted, got %+v", old)
	}
	if old.HasCanary() {
		t.Fatalf("demoted row must lose its canary config, got %+v", old)
	}
	got, err := svc.ResolveVersionCtx(ctx, orgID, agentID, environment)
	if err != nil || got != 3 {
		t.Fatalf("new healthy row resolves stable v3, got %d/%v", got, err)
	}
	if newer.HasCanary() {
		t.Fatalf("new row should carry no canary, got %+v", newer)
	}
}

func TestResolveVersionErrors(t *testing.T) {
	ctx := context.Background()
	svc := canaryFixture(nil)

	// No deployment at all -> nothing serves traffic.
	if _, err := svc.ResolveVersionCtx(ctx, "org-1", "agent-missing", EnvironmentProduction); !errors.Is(err, ErrNoServingDeployment) {
		t.Fatalf("no deployment should be ErrNoServingDeployment, got %v", err)
	}
	// Unknown environment is rejected before any lookup.
	if _, err := svc.ResolveVersionCtx(ctx, "org-1", "agent-1", "chaos"); !errors.Is(err, ErrInvalidEnvironment) {
		t.Fatalf("unknown environment should be ErrInvalidEnvironment, got %v", err)
	}
	// Tenant isolation: another tenant never sees the deployment.
	deployment := healthyWithCanary(t, svc, "org-1", "agent-1", 1, 2, 100, EnvironmentProduction)
	if !deployment.HasCanary() {
		t.Fatal("fixture should carry a canary")
	}
	if _, err := svc.ResolveVersionCtx(ctx, "org-2", "agent-1", EnvironmentProduction); !errors.Is(err, ErrNoServingDeployment) {
		t.Fatalf("foreign tenant should be ErrNoServingDeployment, got %v", err)
	}
	if got, err := svc.ResolveVersionCtx(ctx, "org-1", "agent-1", EnvironmentProduction); err != nil || got != 2 {
		t.Fatalf("weight 100 canary must always resolve v2, got %d/%v", got, err)
	}
}
