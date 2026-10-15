package qualitygates

import (
	"context"
	"errors"
	"testing"
	"time"
)

// pinnedClock returns an injectable clock stuck at gateNow (deterministic
// service tests).
func pinnedClock() func() time.Time {
	return func() time.Time { return gateNow }
}

func newTestService(t *testing.T) (*Service, *InMemoryPolicyStore, *InMemoryEvidenceStore, *[]map[string]any) {
	t.Helper()
	policies := NewInMemoryPolicyStore()
	evidence := NewInMemoryEvidenceStore()
	svc := NewService(policies, evidence)
	svc.SetClock(pinnedClock())
	var audited []map[string]any
	svc.SetAuditSink(func(ctx context.Context, actor, action, orgID, resource string, meta map[string]any) {
		row := map[string]any{"actor": actor, "action": action, "org": orgID, "resource": resource}
		for k, v := range meta {
			row[k] = v
		}
		audited = append(audited, row)
	})
	return svc, policies, evidence, &audited
}

func createDefaultPolicy(t *testing.T, svc *Service) *GatePolicy {
	t.Helper()
	p, err := svc.CreatePolicy(context.Background(), "org-a", "owner@x.io", GatePolicy{
		Name:   "release-default",
		Active: true,
		RequiredSuites: []SuiteRequirement{
			{Kind: SuiteEvaluations, MinPassRate: 0.8, MaxAge: 24 * time.Hour},
			validSecuritySuite(),
		},
	})
	if err != nil {
		t.Fatalf("create policy: %v", err)
	}
	return p
}

func TestServicePolicyLifecycleIsVersioned(t *testing.T) {
	svc, _, _, audited := newTestService(t)
	ctx := context.Background()

	created := createDefaultPolicy(t, svc)
	if created.Version != 1 || created.OrgID != "org-a" || created.CreatedBy == "" {
		t.Fatalf("created policy wrong: %+v", created)
	}
	if created.CreatedBy != "owner@x.io" {
		t.Fatalf("created_by must be the actor: %q", created.CreatedBy)
	}
	if !created.CreatedAt.Equal(gateNow) || !created.UpdatedAt.Equal(gateNow) {
		t.Fatalf("service clock must stamp createdAt/updatedAt: %v %v", created.CreatedAt, created.UpdatedAt)
	}

	// Update appends version 2 (immutable history).
	updated, err := svc.UpdatePolicy(ctx, "org-a", created.ID, "owner@x.io", GatePolicy{
		Name:   "release-default",
		Active: true,
		RequiredSuites: []SuiteRequirement{
			{Kind: SuiteEvaluations, MinPassRate: 0.9, MaxAge: 24 * time.Hour},
			validSecuritySuite(),
		},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Version != 2 {
		t.Fatalf("want version 2, got %d", updated.Version)
	}
	v1, err := svc.GetPolicy(ctx, "org-a", created.ID, 1)
	if err != nil {
		t.Fatalf("get v1: %v", err)
	}
	if v1.RequiredSuites[0].MinPassRate != 0.8 {
		t.Fatalf("history must be immutable: v1 pass rate %v", v1.RequiredSuites[0].MinPassRate)
	}
	if !v1.CreatedAt.Equal(gateNow) {
		t.Fatalf("v1 must keep its creation stamp: %v", v1.CreatedAt)
	}

	// List returns only the latest version.
	list, err := svc.ListPolicies(ctx, "org-a")
	if err != nil || len(list) != 1 || list[0].Version != 2 {
		t.Fatalf("list wrong: %v %v", list, err)
	}

	// Unknown/foreign ids are not-found (no tenant probing).
	if _, err := svc.GetPolicy(ctx, "org-b", created.ID, 0); !errors.Is(err, ErrPolicyNotFound) {
		t.Fatalf("want ErrPolicyNotFound, got %v", err)
	}

	// Delete removes every version (audited).
	if err := svc.DeletePolicy(ctx, "org-a", created.ID, "owner@x.io"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := svc.GetPolicy(ctx, "org-a", created.ID, 1); !errors.Is(err, ErrPolicyNotFound) {
		t.Fatalf("want ErrPolicyNotFound after delete, got %v", err)
	}
	actions := auditActions(*audited)
	for _, want := range []string{AuditPolicyCreated, AuditPolicyUpdated, AuditPolicyDeleted} {
		if !contains(actions, want) {
			t.Fatalf("missing audit action %s in %v", want, actions)
		}
	}
}

func auditActions(rows []map[string]any) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r["action"].(string))
	}
	return out
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func TestServiceApproveExceptionIsTimeBoundedAuditedAndForcedActor(t *testing.T) {
	svc, _, _, audited := newTestService(t)
	ctx := context.Background()
	policy := createDefaultPolicy(t, svc)

	// Happy path: approver is ALWAYS the authenticated actor (a client-
	// supplied ApprovedBy can never forge attribution).
	approved, err := svc.ApproveException(ctx, "org-a", policy.ID, "admin@x.io", Exception{
		Reason:     "flaky probe during holiday freeze",
		ApprovedBy: "attacker@x.io", // must be overwritten
		ExpiresAt:  gateNow.Add(48 * time.Hour),
		Suite:      SuiteEvaluations,
	})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if approved.Version != 2 {
		t.Fatalf("approval must create a new version, got %d", approved.Version)
	}
	if len(approved.Exceptions) != 1 || approved.Exceptions[0].ApprovedBy != "admin@x.io" {
		t.Fatalf("ApprovedBy must be the actor, not the request: %+v", approved.Exceptions)
	}
	if !approved.Exceptions[0].ApprovedAt.Equal(gateNow) {
		t.Fatalf("ApprovedAt must be the service clock: %v", approved.Exceptions[0].ApprovedAt)
	}

	// Already-expired waivers are rejected (time-bounded only, unexpired
	// approvals).
	if _, err := svc.ApproveException(ctx, "org-a", policy.ID, "admin@x.io", Exception{
		Reason: "past", ExpiresAt: gateNow.Add(-time.Second),
	}); !errors.Is(err, ErrInvalidException) {
		t.Fatalf("want ErrInvalidException for past expiry, got %v", err)
	}
	// Security suites are structurally unwaivable at the service layer too.
	if _, err := svc.ApproveException(ctx, "org-a", policy.ID, "admin@x.io", Exception{
		Reason: "no", Suite: SuiteSecurityAdversarial, ExpiresAt: gateNow.Add(time.Hour),
	}); !errors.Is(err, ErrInvalidException) {
		t.Fatalf("want ErrInvalidException for security suite, got %v", err)
	}
	// Missing reason rejected.
	if _, err := svc.ApproveException(ctx, "org-a", policy.ID, "admin@x.io", Exception{
		ExpiresAt: gateNow.Add(time.Hour),
	}); !errors.Is(err, ErrInvalidException) {
		t.Fatalf("want ErrInvalidException for missing reason, got %v", err)
	}

	if !contains(auditActions(*audited), AuditExceptionApproved) {
		t.Fatalf("approval must be audited: %v", auditActions(*audited))
	}
}

func TestServiceEvaluateResolvesRefsAndDeterministic(t *testing.T) {
	svc, _, evidence, audited := newTestService(t)
	ctx := context.Background()
	policy := createDefaultPolicy(t, svc)

	good := completedRec(SuiteEvaluations, "ev-e", gateNow.Add(-time.Hour), nil)
	good.OrgID = "org-a"
	if err := evidence.PutEvidence(ctx, &good); err != nil {
		t.Fatalf("put evidence: %v", err)
	}

	d, err := svc.Evaluate(ctx, "org-a", policy.ID, []string{"ev-e"})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if d.Recommendation != RecommendHold {
		t.Fatalf("missing security evidence must hold: %+v", d)
	}
	if d.PolicyEvaluationVersion != policy.EvaluationVersion() || d.PolicyVersion != policy.Version {
		t.Fatalf("decision must pin the evaluated policy: %+v", d)
	}

	// Unknown/foreign refs are a caller error, not a silent block.
	if _, err := svc.Evaluate(ctx, "org-a", policy.ID, []string{"nope"}); !errors.Is(err, ErrEvidenceNotFound) {
		t.Fatalf("want ErrEvidenceNotFound, got %v", err)
	}

	// Inactive policies are refused.
	inactive, err := svc.CreatePolicy(ctx, "org-a", "owner@x.io", GatePolicy{
		Name: "off", RequiredSuites: []SuiteRequirement{{Kind: SuiteEvaluations}},
	})
	if err != nil {
		t.Fatalf("create inactive: %v", err)
	}
	if _, err := svc.Evaluate(ctx, "org-a", inactive.ID, nil); !errors.Is(err, ErrInactivePolicy) {
		t.Fatalf("want ErrInactivePolicy, got %v", err)
	}

	if !contains(auditActions(*audited), AuditDecisionRecommended) {
		t.Fatalf("evaluations must be audited: %v", auditActions(*audited))
	}
}
