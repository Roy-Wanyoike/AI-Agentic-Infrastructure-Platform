package tenant

// residency_test.go — issue #140 coverage: region validation, the documented
// classification table (in-region / out-region / unknown, incl. boundary
// tokens), policy lifecycle + validation, the full Check decision matrix,
// audit evidence (host-only metadata), posture reflection, org isolation and
// the Postgres store (sqlmock, following the internal/*/store_test.go
// pattern). No network: classification is pure string work.

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"agentos/internal/audit"
)

// capturedAudit records every AuditPublisher call (order-preserving).
type capturedAudit struct {
	mu      sync.Mutex
	entries []*audit.Entry
}

func (c *capturedAudit) LogCtx(_ context.Context, actor, action, orgID, resource string, metadata map[string]any) (*audit.Entry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := &audit.Entry{Actor: actor, Action: action, OrganizationID: orgID, Resource: resource, Metadata: metadata}
	c.entries = append(c.entries, entry)
	return entry, nil
}

func (c *capturedAudit) count(action string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, e := range c.entries {
		if e.Action == action {
			n++
		}
	}
	return n
}

func (c *capturedAudit) last(action string) *audit.Entry {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.entries) - 1; i >= 0; i-- {
		if c.entries[i].Action == action {
			return c.entries[i]
		}
	}
	return nil
}

// brokenStore fails every policy read (the fail-closed path).
type brokenStore struct{ err error }

func (b brokenStore) GetPolicy(context.Context, string) (*ResidencyPolicy, error) { return nil, b.err }
func (b brokenStore) PutPolicy(context.Context, *ResidencyPolicy) error           { return b.err }
func (b brokenStore) RecordViolation(context.Context, string, *ViolationRecord) error {
	return nil
}
func (b brokenStore) LastViolation(context.Context, string) (*ViolationRecord, error) {
	return nil, nil
}

func newTestService(t *testing.T) (*ResidencyService, *capturedAudit) {
	t.Helper()
	svc := NewResidencyService()
	cap := &capturedAudit{}
	svc.SetAuditPublisher(cap)
	return svc, cap
}

// --- Region enum + validation ------------------------------------------------

func TestRegionValidate(t *testing.T) {
	for _, region := range KnownRegions() {
		if err := region.Validate(); err != nil {
			t.Errorf("Region(%q).Validate() = %v, want nil", region, err)
		}
	}
	if got := len(KnownRegions()); got != 7 {
		t.Errorf("KnownRegions() has %d entries, want 7 (six buckets + custom)", got)
	}
	for _, invalid := range []Region{"", "unknown", "EU-CENTRAL", "mars-central", "eu_central"} {
		if err := invalid.Validate(); !errors.Is(err, ErrInvalidRegion) {
			t.Errorf("Region(%q).Validate() = %v, want ErrInvalidRegion", invalid, err)
		}
	}
}

// --- Classification table -----------------------------------------------------

func TestRegionOfTable(t *testing.T) {
	cases := []struct {
		url  string
		want Region
		note string
	}{
		// AWS regional endpoints (any service label).
		{"https://s3.eu-central-1.amazonaws.com/bucket", RegionEUCentral, "AWS eu-central-1"},
		{"https://lambda.eu-west-2.amazonaws.com", RegionEUWest, "AWS eu-west-2"},
		{"https://dynamodb.us-east-2.amazonaws.com", RegionUSEast, "AWS us-east-2"},
		{"https://ec2.us-west-1.amazonaws.com", RegionUSWest, "AWS us-west-1"},
		{"https://sns.ap-south-1.amazonaws.com", RegionAPSouth, "AWS ap-south-1"},
		{"https://rds.ap-northeast-3.amazonaws.com", RegionAPNortheast, "AWS ap-northeast-3"},
		// Explicit host table.
		{"https://s3.amazonaws.com", RegionUSEast, "legacy global S3 endpoint"},
		// GCP region names.
		{"https://europe-west3-run.googleapis.com", RegionEUWest, "GCP europe-west3"},
		{"https://asia-northeast1-notebooks.googleapis.com", RegionAPNortheast, "GCP asia-northeast1"},
		{"https://europe-central2-fn.cloudfunctions.net", RegionEUCentral, "GCP europe-central2"},
		// Azure region names.
		{"https://acct.westeurope.blob.storage.azure.net", RegionEUWest, "Azure West Europe"},
		{"https://myapp.northeurope.cloudapp.azure.com", RegionEUWest, "Azure North Europe"},
		{"https://db.germanywestcentral.cloudapp.azure.com", RegionEUCentral, "Azure Germany WC"},
		{"https://acct.eastus2.blob.core.windows.net", RegionUSEast, "Azure East US 2"},
		{"https://acct.centralindia.cloudapp.azure.com", RegionAPSouth, "Azure Central India"},
		{"https://svc.japaneast.cloudapp.azure.com", RegionAPNortheast, "Azure Japan East"},
		// Deliberately unmapped -> unknown (strict mode fails closed there).
		{"https://storage.googleapis.com", RegionUnknown, "GCP global multi-region"},
		{"https://acct.blob.core.windows.net", RegionUnknown, "Azure global blob"},
		{"https://svc.ap-southeast-1.amazonaws.com", RegionUnknown, "SE Asia is not ap-south"},
		{"https://asia-southeast1-run.googleapis.com", RegionUnknown, "SE Asia is not ap-south"},
		{"https://localhost:8080", RegionUnknown, "localhost"},
		{"http://127.0.0.1:9000", RegionUnknown, "loopback literal"},
		{"", RegionUnknown, "empty input"},
		{"https://example-tooling.io/call", RegionUnknown, "no region token"},
	}
	for _, tc := range cases {
		if got := RegionOf(tc.url); got != tc.want {
			t.Errorf("RegionOf(%q) = %q, want %q (%s)", tc.url, got, tc.want, tc.note)
		}
	}
}

func TestRegionOfDeterministicAndQuerySafe(t *testing.T) {
	withQuery := "https://user:secret@data.eu-central-1.example.com/v1/run?api_key=hunter2&trace=1"
	first := RegionOf(withQuery)
	for i := 0; i < 5; i++ {
		if got := RegionOf(withQuery); got != first {
			t.Fatalf("RegionOf not deterministic: %q then %q", first, got)
		}
	}
	if first != RegionEUCentral {
		t.Fatalf("userinfo/query must not break classification, got %q", first)
	}
	host := HostOf(withQuery)
	if host != "data.eu-central-1.example.com" {
		t.Fatalf("HostOf = %q, want the bare host", host)
	}
	if strings.ContainsAny(host, ":@/?#") {
		t.Fatalf("evidence host must be stripped of port/userinfo/query: %q", host)
	}
	// Scheme-less and port forms classify like their URL counterparts.
	for _, tc := range []struct {
		raw  string
		want Region
	}{
		{"s3.eu-west-1.amazonaws.com/bucket", RegionEUWest},
		{"ec2.us-west-2.amazonaws.com:443", RegionUSWest},
		{"  https://HOST.AP-Northeast-1.AWSAmazon.COM  ", RegionAPNortheast},
	} {
		if got := RegionOf(tc.raw); got != tc.want {
			t.Errorf("RegionOf(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// --- Policy lifecycle + validation --------------------------------------------

func TestDefaultPolicyUnpinned(t *testing.T) {
	svc, _ := newTestService(t)
	policy, err := svc.GetPolicy(context.Background(), "org-1")
	if err != nil {
		t.Fatalf("GetPolicy: %v", err)
	}
	if policy.IsPinned() {
		t.Fatalf("fresh org must be unpinned: %+v", policy)
	}
	if len(policy.AllowedRegions) != 0 || policy.PinnedRegion != "" || policy.StrictMode {
		t.Fatalf("default policy wrong: %+v", policy)
	}
	// Clones: mutating the returned policy must not corrupt the service.
	policy.StrictMode = true
	again, _ := svc.GetPolicy(context.Background(), "org-1")
	if again.StrictMode {
		t.Fatalf("GetPolicy must return a defensive copy")
	}
}

func TestUpdatePolicyRoundtripPreservesCreatedAt(t *testing.T) {
	svc, cap := newTestService(t)
	ctx := context.Background()

	created, err := svc.UpdatePolicy(ctx, "org-1", PolicyUpdate{PinnedRegion: "eu-central", StrictMode: true})
	if err != nil {
		t.Fatalf("UpdatePolicy: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	updated, err := svc.UpdatePolicy(ctx, "org-1", PolicyUpdate{PinnedRegion: "eu-west", AllowedRegions: []string{"us-east"}, StrictMode: false})
	if err != nil {
		t.Fatalf("UpdatePolicy#2: %v", err)
	}
	if !updated.CreatedAt.Equal(created.CreatedAt) {
		t.Fatalf("CreatedAt must survive updates: %v vs %v", updated.CreatedAt, created.CreatedAt)
	}
	if !updated.UpdatedAt.After(updated.CreatedAt) {
		t.Fatalf("UpdatedAt must advance: %v vs %v", updated.UpdatedAt, updated.CreatedAt)
	}
	got, _ := svc.GetPolicy(ctx, "org-1")
	if got.PinnedRegion != RegionEUWest || len(got.AllowedRegions) != 1 || got.AllowedRegions[0] != "us-east" || got.StrictMode {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	if cap.count(AuditActionPolicyUpdated) != 2 {
		t.Fatalf("residency.policy_updated audits = %d, want 2", cap.count(AuditActionPolicyUpdated))
	}
	if ev := cap.last(AuditActionPolicyUpdated); ev.Metadata["previous_pinned_region"] != "eu-central" {
		t.Fatalf("policy audit must carry the previous pin: %v", ev.Metadata)
	}
}

func TestUpdatePolicyValidation(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	cases := []struct {
		name string
		in   PolicyUpdate
		want error
	}{
		{"unknown pinned region", PolicyUpdate{PinnedRegion: "mars-central"}, ErrInvalidRegion},
		{"unknown allowed region", PolicyUpdate{PinnedRegion: "eu-west", AllowedRegions: []string{"unknown"}}, ErrInvalidRegion},
		{"strict with nothing pinned", PolicyUpdate{StrictMode: true}, ErrInvalidPolicy},
		{"custom pin is valid", PolicyUpdate{PinnedRegion: "custom"}, nil},
		{"list-only pinning is valid", PolicyUpdate{AllowedRegions: []string{"us-east"}}, nil},
	}
	for _, tc := range cases {
		_, err := svc.UpdatePolicy(ctx, "org-1", tc.in)
		if tc.want == nil && err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
		}
		if tc.want != nil && !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	if _, err := svc.UpdatePolicy(ctx, "  ", PolicyUpdate{PinnedRegion: "eu-west"}); !errors.Is(err, ErrOrgRequired) {
		t.Errorf("empty org id: err = %v, want ErrOrgRequired", err)
	}
}

func TestAllowedRegionsNormalizedDedupe(t *testing.T) {
	svc, _ := newTestService(t)
	policy, err := svc.UpdatePolicy(context.Background(), "org-1", PolicyUpdate{
		PinnedRegion:   "eu-central",
		AllowedRegions: []string{" US-East ", "us-east", "EU-West", "", "us-east"},
	})
	if err != nil {
		t.Fatalf("UpdatePolicy: %v", err)
	}
	want := []string{"us-east", "eu-west"}
	if len(policy.AllowedRegions) != len(want) {
		t.Fatalf("allowed = %v, want %v", policy.AllowedRegions, want)
	}
	for i := range want {
		if policy.AllowedRegions[i] != want[i] {
			t.Fatalf("allowed[%d] = %q, want %q (order-preserving dedupe)", i, policy.AllowedRegions[i], want[i])
		}
	}
}

// --- Check decision matrix -----------------------------------------------------

func TestCheckUnpinnedAllowsAndAudits(t *testing.T) {
	svc, cap := newTestService(t)
	v := svc.Check(context.Background(), "org-1", "https://s3.us-east-1.amazonaws.com/data")
	if !v.Allowed || v.Reason != ReasonUnpinned || v.StrictMode || v.Mode != ModeUnpinned {
		t.Fatalf("unpinned verdict wrong: %+v", v)
	}
	if v.Region != RegionUSEast {
		t.Fatalf("verdict must still classify: %+v", v)
	}
	if got := cap.count(AuditActionCheck); got != 1 {
		t.Fatalf("residency.check audits = %d, want 1", got)
	}
	if got := cap.count(AuditActionViolation); got != 0 {
		t.Fatalf("unpinned allow must not emit residency.violation: %d", got)
	}
}

func TestCheckNonStrictFlagsButAllows(t *testing.T) {
	svc, cap := newTestService(t)
	ctx := context.Background()
	if _, err := svc.UpdatePolicy(ctx, "org-1", PolicyUpdate{PinnedRegion: "eu-central"}); err != nil {
		t.Fatalf("UpdatePolicy: %v", err)
	}
	v := svc.Check(ctx, "org-1", "https://dynamodb.us-east-1.amazonaws.com")
	if !v.Allowed || v.Reason != ReasonOutOfRegionFlagged || v.StrictMode || v.Mode != ModePinned {
		t.Fatalf("non-strict out-of-region verdict wrong: %+v", v)
	}
	ev := cap.last(AuditActionCheck)
	if ev == nil || ev.Metadata["flagged"] != true {
		t.Fatalf("flagged allow must be audited with flagged=true: %+v", ev)
	}
	posture, err := svc.Posture(ctx, "org-1")
	if err != nil || posture.LastViolation == nil {
		t.Fatalf("flagged evidence must surface in posture: %+v err=%v", posture, err)
	}
	if posture.LastViolation.Reason != ReasonOutOfRegionFlagged {
		t.Fatalf("evidence reason wrong: %+v", posture.LastViolation)
	}
}

func TestCheckStrictDecisionMatrix(t *testing.T) {
	cases := []struct {
		name      string
		url       string
		wantOK    bool
		wantFor   string
		wantAudit string
	}{
		{"in-region allowed", "https://s3.eu-central-1.amazonaws.com", true, ReasonInRegion, AuditActionCheck},
		{"out-of-region denied", "https://dynamodb.us-east-1.amazonaws.com", false, ReasonOutOfRegion, AuditActionViolation},
		{"unknown denied fail-closed", "https://acct.blob.core.windows.net", false, ReasonUnclassifiable, AuditActionViolation},
		{"localhost denied fail-closed", "http://127.0.0.1:8080", false, ReasonUnclassifiable, AuditActionViolation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, cap := newTestService(t)
			ctx := context.Background()
			if _, err := svc.UpdatePolicy(ctx, "org-1", PolicyUpdate{PinnedRegion: "eu-central", StrictMode: true}); err != nil {
				t.Fatalf("UpdatePolicy: %v", err)
			}
			v := svc.Check(ctx, "org-1", tc.url)
			if v.Allowed != tc.wantOK || v.Reason != tc.wantFor || !v.StrictMode || v.Mode != ModePinned {
				t.Fatalf("verdict = %+v, want allowed=%v reason=%q strict pinned", v, tc.wantOK, tc.wantFor)
			}
			if got := cap.count(tc.wantAudit); got != 1 {
				t.Fatalf("%s audits = %d, want 1", tc.wantAudit, got)
			}
			if tc.wantOK && cap.count(AuditActionViolation) != 0 {
				t.Fatalf("allow must not emit residency.violation")
			}
		})
	}
}

func TestCheckAllowedRegionsOverride(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	if _, err := svc.UpdatePolicy(ctx, "org-1", PolicyUpdate{PinnedRegion: "eu-central", AllowedRegions: []string{"us-east"}}); err != nil {
		t.Fatalf("UpdatePolicy: %v", err)
	}
	if v := svc.Check(ctx, "org-1", "https://dynamodb.us-east-1.amazonaws.com"); !v.Allowed || v.Reason != ReasonInRegion {
		t.Fatalf("allowed region must pass: %+v", v)
	}
	if v := svc.Check(ctx, "org-1", "https://sns.ap-south-1.amazonaws.com"); !v.Allowed || v.Reason != ReasonOutOfRegionFlagged {
		t.Fatalf("unlisted region must flag-but-allow in observe mode: %+v", v)
	}
}

func TestCheckCustomPinStrictDeniesAllCloud(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	if _, err := svc.UpdatePolicy(ctx, "org-1", PolicyUpdate{PinnedRegion: "custom", StrictMode: true}); err != nil {
		t.Fatalf("custom pin must be accepted: %v", err)
	}
	if v := svc.Check(ctx, "org-1", "https://s3.eu-west-1.amazonaws.com"); v.Allowed || v.Reason != ReasonOutOfRegion {
		t.Fatalf("known cloud endpoint must be denied under a custom pin: %+v", v)
	}
	if v := svc.Check(ctx, "org-1", "https://byoc.internal.corp"); v.Allowed || v.Reason != ReasonUnclassifiable {
		t.Fatalf("unclassifiable endpoints stay fail-closed (RegionOf never yields custom): %+v", v)
	}
}

func TestCheckFailsClosedWhenStoreUnavailable(t *testing.T) {
	svc, cap := newTestService(t)
	svc.store = brokenStore{err: errors.New("db down")}
	v := svc.Check(context.Background(), "org-1", "https://s3.eu-central-1.amazonaws.com")
	if v.Allowed || v.Reason != ReasonPolicyUnavailable {
		t.Fatalf("store failure must fail closed: %+v", v)
	}
	if got := cap.count(AuditActionViolation); got != 1 {
		t.Fatalf("fail-closed deny must be audited: %d", got)
	}
}

func TestCheckAuditMetadataCarriesHostOnly(t *testing.T) {
	svc, cap := newTestService(t)
	ctx := context.Background()
	if _, err := svc.UpdatePolicy(ctx, "org-1", PolicyUpdate{PinnedRegion: "eu-central", StrictMode: true}); err != nil {
		t.Fatalf("UpdatePolicy: %v", err)
	}
	svc.Check(ctx, "org-1", "https://s3.us-west-1.amazonaws.com/payload?token=supersecret")
	ev := cap.last(AuditActionViolation)
	if ev == nil {
		t.Fatalf("deny must be audited")
	}
	for _, key := range []string{"url_host", "verdict", "region", "reason", "strict_mode", "mode"} {
		if _, ok := ev.Metadata[key]; !ok {
			t.Errorf("metadata missing %q: %v", key, ev.Metadata)
		}
	}
	if ev.Metadata["url_host"] != "s3.us-west-1.amazonaws.com" {
		t.Errorf("url_host = %v, want the bare host", ev.Metadata["url_host"])
	}
	if ev.Metadata["verdict"] != "denied" || ev.Metadata["region"] != "us-west" {
		t.Errorf("verdict/region wrong: %v", ev.Metadata)
	}
	for key, val := range ev.Metadata {
		if s, ok := val.(string); ok && (strings.Contains(s, "token=supersecret") || strings.Contains(s, "https://")) {
			t.Errorf("metadata %q leaks URL material: %v", key, s)
		}
	}
	if ev.Resource != "residency/policy" {
		t.Errorf("resource = %q", ev.Resource)
	}
}

// --- Posture ------------------------------------------------------------------

func TestPostureReflection(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	posture, err := svc.Posture(ctx, "org-1")
	if err != nil {
		t.Fatalf("Posture: %v", err)
	}
	if posture.Mode != ModeUnpinned || posture.LastViolation != nil || posture.Policy == nil {
		t.Fatalf("initial posture wrong: %+v", posture)
	}

	if _, err := svc.UpdatePolicy(ctx, "org-1", PolicyUpdate{PinnedRegion: "eu-central", StrictMode: true}); err != nil {
		t.Fatalf("UpdatePolicy: %v", err)
	}
	svc.Check(ctx, "org-1", "https://dynamodb.us-east-2.amazonaws.com")

	posture, err = svc.Posture(ctx, "org-1")
	if err != nil {
		t.Fatalf("Posture: %v", err)
	}
	if posture.Mode != ModePinned {
		t.Fatalf("mode = %q, want pinned", posture.Mode)
	}
	v := posture.LastViolation
	if v == nil {
		t.Fatalf("last_violation must be present after a strict deny")
	}
	if v.URLHost != "dynamodb.us-east-2.amazonaws.com" || v.Region != RegionUSEast || v.Reason != ReasonOutOfRegion || !v.StrictMode {
		t.Fatalf("violation record wrong: %+v", v)
	}
	if v.At.IsZero() {
		t.Fatalf("violation timestamp missing")
	}
	// A posture with no violation history must NOT fabricate one.
	other, err := svc.Posture(ctx, "org-2")
	if err != nil {
		t.Fatalf("Posture org-2: %v", err)
	}
	if other.LastViolation != nil {
		t.Fatalf("org-2 must have no violation evidence: %+v", other.LastViolation)
	}
}

// --- Isolation + seams ----------------------------------------------------------

func TestOrgIsolation(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	if _, err := svc.UpdatePolicy(ctx, "org-a", PolicyUpdate{PinnedRegion: "eu-central", StrictMode: true}); err != nil {
		t.Fatalf("UpdatePolicy: %v", err)
	}
	// org-a generates its own evidence with a strict deny.
	if v := svc.Check(ctx, "org-a", "https://s3.us-west-2.amazonaws.com"); v.Allowed {
		t.Fatalf("org-a strict deny expected: %+v", v)
	}

	// org-b sees no pinning and is unaffected by org-a's strict deny.
	if v := svc.Check(ctx, "org-b", "https://s3.us-east-1.amazonaws.com"); !v.Allowed || v.Reason != ReasonUnpinned {
		t.Fatalf("org-b must be unpinned: %+v", v)
	}
	policy, err := svc.GetPolicy(ctx, "org-b")
	if err != nil || policy.IsPinned() {
		t.Fatalf("org-b policy leak: %+v err=%v", policy, err)
	}
	postureB, _ := svc.Posture(ctx, "org-b")
	if postureB.LastViolation != nil {
		t.Fatalf("org-a evidence must not leak into org-b posture: %+v", postureB.LastViolation)
	}
	postureA, _ := svc.Posture(ctx, "org-a")
	if postureA.LastViolation == nil {
		t.Fatalf("org-a evidence missing")
	}
}

func TestGuardSeam(t *testing.T) {
	var guard Guard = NewResidencyService()
	v := guard.Check(context.Background(), "org-1", "https://s3.eu-west-1.amazonaws.com")
	if !v.Allowed {
		t.Fatalf("*ResidencyService must satisfy Guard: %+v", v)
	}
	var nop Guard = NopGuard{}
	if v := nop.Check(context.Background(), "org-1", "anything"); !v.Allowed || v.Reason != ReasonUnpinned || v.Mode != ModeUnpinned {
		t.Fatalf("NopGuard must allow: %+v", v)
	}
}

// --- Postgres store (sqlmock, migration 031) -------------------------------------

func newPgResidencyStore(t *testing.T) (Store, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewPostgresStore(db), mock
}

func TestPgStorePolicyRoundtrip(t *testing.T) {
	store, mock := newPgResidencyStore(t)
	ctx := context.Background()
	now := time.Date(2026, 1, 15, 9, 30, 0, 0, time.UTC)

	policy := &ResidencyPolicy{
		OrgID:          "org-1",
		PinnedRegion:   RegionEUWest,
		AllowedRegions: []string{"eu-central", "us-east"},
		StrictMode:     true,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	// First write: UPDATE misses (0 rows) then INSERT (update-first upsert).
	mock.ExpectExec("UPDATE residency_policies").
		WithArgs("org-1", "eu-west", `["eu-central","us-east"]`, true, now).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("INSERT INTO residency_policies").
		WithArgs("org-1", "eu-west", `["eu-central","us-east"]`, true, now, now).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := store.PutPolicy(ctx, policy); err != nil {
		t.Fatalf("PutPolicy insert path: %v", err)
	}

	// Second write: UPDATE path (1 row affected, no INSERT).
	mock.ExpectExec("UPDATE residency_policies").
		WithArgs("org-1", "eu-west", `["eu-central","us-east"]`, true, now).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.PutPolicy(ctx, policy); err != nil {
		t.Fatalf("PutPolicy update path: %v", err)
	}

	mock.ExpectQuery("SELECT organization_id, pinned_region, COALESCE\\(allowed_regions::text, '\\[\\]'\\), strict_mode, created_at, updated_at FROM residency_policies WHERE organization_id = \\$1").
		WithArgs("org-1").
		WillReturnRows(sqlmock.NewRows([]string{"organization_id", "pinned_region", "allowed_regions", "strict_mode", "created_at", "updated_at"}).
			AddRow("org-1", "eu-west", `["eu-central","us-east"]`, true, now, now))
	got, err := store.GetPolicy(ctx, "org-1")
	if err != nil {
		t.Fatalf("GetPolicy: %v", err)
	}
	if got.OrgID != "org-1" || got.PinnedRegion != RegionEUWest || !got.StrictMode ||
		len(got.AllowedRegions) != 2 || got.AllowedRegions[1] != "us-east" || !got.CreatedAt.Equal(now) {
		t.Fatalf("scanned policy wrong: %+v", got)
	}

	// Tenant guard: a foreign org gets ErrPolicyNotFound (the service maps
	// that to the unpinned default — no existence leak either way).
	mock.ExpectQuery("SELECT organization_id").
		WithArgs("org-foreign").
		WillReturnError(sql.ErrNoRows)
	if _, err := store.GetPolicy(ctx, "org-foreign"); !errors.Is(err, ErrPolicyNotFound) {
		t.Fatalf("foreign-org GetPolicy = %v, want ErrPolicyNotFound", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

func TestPgStoreServiceUnpinnedDefaultForForeignOrg(t *testing.T) {
	store, mock := newPgResidencyStore(t)
	ctx := context.Background()
	mock.ExpectQuery("SELECT organization_id").
		WithArgs("org-b").
		WillReturnError(sql.ErrNoRows)
	svc := NewResidencyServiceWithStore(store)
	policy, err := svc.GetPolicy(ctx, "org-b")
	if err != nil {
		t.Fatalf("GetPolicy: %v", err)
	}
	if policy.IsPinned() {
		t.Fatalf("foreign org must map to the unpinned default: %+v", policy)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

func TestPgStoreViolationRoundtrip(t *testing.T) {
	store, mock := newPgResidencyStore(t)
	ctx := context.Background()
	at := time.Date(2026, 1, 15, 9, 31, 0, 0, time.UTC)

	mock.ExpectExec("INSERT INTO residency_violations").
		WithArgs(sqlmock.AnyArg(), "org-1", "dynamodb.us-east-2.amazonaws.com", "us-east", "out_of_region", true, at).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := store.RecordViolation(ctx, "org-1", &ViolationRecord{
		At: at, URLHost: "dynamodb.us-east-2.amazonaws.com", Region: RegionUSEast, Reason: ReasonOutOfRegion, StrictMode: true,
	}); err != nil {
		t.Fatalf("RecordViolation: %v", err)
	}

	mock.ExpectQuery("SELECT url_host, region, reason, strict_mode, created_at FROM residency_violations WHERE organization_id = \\$1 ORDER BY created_at DESC, id DESC LIMIT 1").
		WithArgs("org-1").
		WillReturnRows(sqlmock.NewRows([]string{"url_host", "region", "reason", "strict_mode", "created_at"}).
			AddRow("dynamodb.us-east-2.amazonaws.com", "us-east", "out_of_region", true, at))
	got, err := store.LastViolation(ctx, "org-1")
	if err != nil {
		t.Fatalf("LastViolation: %v", err)
	}
	if got == nil || got.URLHost != "dynamodb.us-east-2.amazonaws.com" || got.Region != RegionUSEast || got.Reason != ReasonOutOfRegion || !got.StrictMode || !got.At.Equal(at) {
		t.Fatalf("scanned violation wrong: %+v", got)
	}

	// No evidence rows -> (nil, nil), not an error.
	mock.ExpectQuery("SELECT url_host").
		WithArgs("org-2").
		WillReturnError(sql.ErrNoRows)
	none, err := store.LastViolation(ctx, "org-2")
	if err != nil || none != nil {
		t.Fatalf("LastViolation with no rows = (%v, %v), want (nil, nil)", none, err)
	}

	// Tenant guard: org-a evidence never answers org-b's query (the WHERE
	// organization_id = $1 guard above is exercised by the org-2 lookup).
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}
