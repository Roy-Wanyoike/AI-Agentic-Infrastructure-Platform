package synth

// service_test.go — issue #147 registry service, dual-mode behavior:
//
//      in-memory CRUD lifecycles for all four resources (table-driven),
//      the ownership verification flow (challenge issue -> wrong token
//      rejected -> confirm -> verified; idempotent re-confirm; no-op
//      re-challenge on verified targets),
//      the environment mode semantics pinned by types.go (production
//      defaults to production_read_only, pre_production never on production,
//      allowlist required for production, rate caps only honored in
//      pre_production, window validation),
//      persona built-in key validation + version bumps,
//      the AssertExecutionAllowed ScopeGuard seam with the typed errors from
//      types.go (unverified target denies ALL; production_read_only denies
//      writes; allowlist scope violations; rate caps),
//      store-backed mode via a recording fake store (source of truth,
//      error propagation, tenant guards) and cross-tenant 404 parity.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func newTestService() *Service { return NewService() }

// seedProject creates one project in orgID and fails the test on error.
func seedProject(t *testing.T, s *Service, orgID, name string) *Project {
	t.Helper()
	p, err := s.CreateProject(context.Background(), orgID, name)
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return p
}

// seedVerifiedTarget seeds a project + verified target (challenge confirmed).
func seedVerifiedTarget(t *testing.T, s *Service, orgID, baseURL string) (*Project, *Target) {
	t.Helper()
	p := seedProject(t, s, orgID, "proj")
	tg, err := s.CreateTarget(context.Background(), orgID, p.ID, "app", baseURL)
	if err != nil {
		t.Fatalf("seed target: %v", err)
	}
	challenge, err := s.StartTargetVerification(context.Background(), orgID, tg.ID)
	if err != nil {
		t.Fatalf("start verification: %v", err)
	}
	if _, err := s.ConfirmTargetVerification(context.Background(), orgID, tg.ID, challenge.VerifyToken); err != nil {
		t.Fatalf("confirm verification: %v", err)
	}
	verified, err := s.GetTarget(context.Background(), orgID, tg.ID)
	if err != nil {
		t.Fatalf("reload target: %v", err)
	}
	if !verified.Verified {
		t.Fatalf("seeded target not verified")
	}
	return p, verified
}

// fakeStore is a recording in-memory Store used to exercise store-backed
// mode without sqlmock (the real Postgres store has its own store_test.go).
type fakeStore struct {
	mu        sync.Mutex
	projects  map[string]*Project
	targets   map[string]*Target
	envs      map[string]*Environment
	personas  map[string]*Persona
	failOn    string // method name to fail with errBoom
	errBoom   error
	createEnv int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		projects: map[string]*Project{}, targets: map[string]*Target{},
		envs: map[string]*Environment{}, personas: map[string]*Persona{},
		errBoom: errors.New("boom"),
	}
}

func (f *fakeStore) maybeFail(method string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOn == method {
		return f.errBoom
	}
	return nil
}

func (f *fakeStore) CreateProject(_ context.Context, p *Project) error {
	if err := f.maybeFail("CreateProject"); err != nil {
		return err
	}
	f.projects[p.ID] = cloneProject(p)
	return nil
}
func (f *fakeStore) GetProject(_ context.Context, orgID, id string) (*Project, error) {
	if err := f.maybeFail("GetProject"); err != nil {
		return nil, err
	}
	if p, ok := f.projects[id]; ok && p.OrgID == orgID {
		return cloneProject(p), nil
	}
	return nil, ErrNotFound
}
func (f *fakeStore) ListProjects(_ context.Context, orgID string) ([]*Project, error) {
	out := []*Project{}
	for _, p := range f.projects {
		if p.OrgID == orgID {
			out = append(out, cloneProject(p))
		}
	}
	return out, f.maybeFail("ListProjects")
}
func (f *fakeStore) UpdateProject(_ context.Context, p *Project) error {
	if err := f.maybeFail("UpdateProject"); err != nil {
		return err
	}
	f.projects[p.ID] = cloneProject(p)
	return nil
}
func (f *fakeStore) DeleteProject(_ context.Context, orgID, id string) error {
	if err := f.maybeFail("DeleteProject"); err != nil {
		return err
	}
	delete(f.projects, id)
	return nil
}

func (f *fakeStore) CreateTarget(_ context.Context, t *Target) error {
	if err := f.maybeFail("CreateTarget"); err != nil {
		return err
	}
	f.targets[t.ID] = cloneTarget(t)
	return nil
}
func (f *fakeStore) GetTarget(_ context.Context, orgID, id string) (*Target, error) {
	if err := f.maybeFail("GetTarget"); err != nil {
		return nil, err
	}
	if t, ok := f.targets[id]; ok && t.OrgID == orgID {
		return cloneTarget(t), nil
	}
	return nil, ErrNotFound
}
func (f *fakeStore) ListTargets(_ context.Context, orgID, projectID string) ([]*Target, error) {
	out := []*Target{}
	for _, t := range f.targets {
		if t.OrgID == orgID && (projectID == "" || t.ProjectID == projectID) {
			out = append(out, cloneTarget(t))
		}
	}
	return out, f.maybeFail("ListTargets")
}
func (f *fakeStore) UpdateTarget(_ context.Context, t *Target) error {
	if err := f.maybeFail("UpdateTarget"); err != nil {
		return err
	}
	f.targets[t.ID] = cloneTarget(t)
	return nil
}
func (f *fakeStore) DeleteTarget(_ context.Context, orgID, id string) error {
	if err := f.maybeFail("DeleteTarget"); err != nil {
		return err
	}
	delete(f.targets, id)
	return nil
}

func (f *fakeStore) CreateEnvironment(_ context.Context, e *Environment) error {
	f.mu.Lock()
	f.createEnv++
	f.mu.Unlock()
	if err := f.maybeFail("CreateEnvironment"); err != nil {
		return err
	}
	f.envs[e.ID] = cloneEnvironment(e)
	return nil
}
func (f *fakeStore) GetEnvironment(_ context.Context, orgID, id string) (*Environment, error) {
	if err := f.maybeFail("GetEnvironment"); err != nil {
		return nil, err
	}
	if e, ok := f.envs[id]; ok && e.OrgID == orgID {
		return cloneEnvironment(e), nil
	}
	return nil, ErrNotFound
}
func (f *fakeStore) ListEnvironments(_ context.Context, orgID, targetID string) ([]*Environment, error) {
	out := []*Environment{}
	for _, e := range f.envs {
		if e.OrgID == orgID && (targetID == "" || e.TargetID == targetID) {
			out = append(out, cloneEnvironment(e))
		}
	}
	return out, f.maybeFail("ListEnvironments")
}
func (f *fakeStore) UpdateEnvironment(_ context.Context, e *Environment) error {
	if err := f.maybeFail("UpdateEnvironment"); err != nil {
		return err
	}
	f.envs[e.ID] = cloneEnvironment(e)
	return nil
}
func (f *fakeStore) DeleteEnvironment(_ context.Context, orgID, id string) error {
	if err := f.maybeFail("DeleteEnvironment"); err != nil {
		return err
	}
	delete(f.envs, id)
	return nil
}

func (f *fakeStore) CreatePersona(_ context.Context, p *Persona) error {
	if err := f.maybeFail("CreatePersona"); err != nil {
		return err
	}
	f.personas[p.ID] = clonePersona(p)
	return nil
}
func (f *fakeStore) GetPersona(_ context.Context, orgID, id string) (*Persona, error) {
	if err := f.maybeFail("GetPersona"); err != nil {
		return nil, err
	}
	if p, ok := f.personas[id]; ok && p.OrgID == orgID {
		return clonePersona(p), nil
	}
	return nil, ErrNotFound
}
func (f *fakeStore) ListPersonas(_ context.Context, orgID string) ([]*Persona, error) {
	out := []*Persona{}
	for _, p := range f.personas {
		if p.OrgID == orgID {
			out = append(out, clonePersona(p))
		}
	}
	return out, f.maybeFail("ListPersonas")
}
func (f *fakeStore) UpdatePersona(_ context.Context, p *Persona) error {
	if err := f.maybeFail("UpdatePersona"); err != nil {
		return err
	}
	f.personas[p.ID] = clonePersona(p)
	return nil
}
func (f *fakeStore) DeletePersona(_ context.Context, orgID, id string) error {
	if err := f.maybeFail("DeletePersona"); err != nil {
		return err
	}
	delete(f.personas, id)
	return nil
}

// ---------------------------------------------------------------------------
// projects
// ---------------------------------------------------------------------------

func TestProjectLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newTestService()

	p := seedProject(t, s, "org-a", "checkout")
	if p.ID == "" || p.OrgID != "org-a" || p.Name != "checkout" {
		t.Fatalf("created project wrong: %+v", p)
	}
	if p.CreatedAt.IsZero() || p.UpdatedAt.IsZero() {
		t.Fatalf("timestamps not assigned: %+v", p)
	}

	// Rename round-trips.
	upd, err := s.UpdateProject(ctx, "org-a", p.ID, "checkout-v2")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if upd.Name != "checkout-v2" {
		t.Fatalf("rename lost: %+v", upd)
	}
	got, err := s.GetProject(ctx, "org-a", p.ID)
	if err != nil || got.Name != "checkout-v2" {
		t.Fatalf("get after update: %v %+v", err, got)
	}

	// List is org-scoped.
	seedProject(t, s, "org-a", "billing")
	seedProject(t, s, "org-b", "other-org")
	list, err := s.ListProjects(ctx, "org-a")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("list len = %d, want 2", len(list))
	}

	// Cross-org reads are 404-equivalent (no existence leak).
	if _, err := s.GetProject(ctx, "org-b", p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-org get err = %v, want ErrNotFound", err)
	}
	if _, err := s.GetProject(ctx, "org-a", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown get err = %v, want ErrNotFound", err)
	}

	// Update/delete by a foreign org fail identically.
	if _, err := s.UpdateProject(ctx, "org-b", p.ID, "hijack"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-org update err = %v, want ErrNotFound", err)
	}
	if err := s.DeleteProject(ctx, "org-b", p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-org delete err = %v, want ErrNotFound", err)
	}

	// Delete removes it for everyone.
	if err := s.DeleteProject(ctx, "org-a", p.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetProject(ctx, "org-a", p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete err = %v, want ErrNotFound", err)
	}
}

func TestProjectValidation(t *testing.T) {
	s := newTestService()
	for _, tc := range []struct{ orgID, name string }{
		{"", "x"},        // no org
		{"org-a", ""},    // no name
		{"org-a", "   "}, // whitespace name
	} {
		if _, err := s.CreateProject(context.Background(), tc.orgID, tc.name); !errors.Is(err, ErrInvalid) {
			t.Errorf("CreateProject(%q,%q) err = %v, want ErrInvalid", tc.orgID, tc.name, err)
		}
	}
	if _, err := s.UpdateProject(context.Background(), "org-a", "p1", " "); !errors.Is(err, ErrInvalid) {
		t.Errorf("empty rename err = %v, want ErrInvalid", err)
	}
}

// ---------------------------------------------------------------------------
// targets + verification flow
// ---------------------------------------------------------------------------

func TestTargetLifecycleAndValidation(t *testing.T) {
	ctx := context.Background()
	s := newTestService()
	p := seedProject(t, s, "org-a", "proj")

	for _, tc := range []struct{ name, baseURL, want string }{
		{"", "https://x.io", "name is required"},
		{"app", "", "base_url is required"},
		{"app", "not-a-url", "absolute http(s)"},
		{"app", "ftp://x.io", "absolute http(s)"},
		{"app", "https://", "absolute http(s)"},
	} {
		if _, err := s.CreateTarget(ctx, "org-a", p.ID, tc.name, tc.baseURL); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("CreateTarget(%q,%q) err = %v, want containing %q", tc.name, tc.baseURL, err, tc.want)
		}
	}
	// Cross-org project anchor is not a validation error but a 404.
	if _, err := s.CreateTarget(ctx, "org-b", p.ID, "app", "https://x.io"); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-org anchor err = %v, want ErrNotFound", err)
	}

	tg, err := s.CreateTarget(ctx, "org-a", p.ID, "app", "https://app.example.com")
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	if tg.Verified {
		t.Fatalf("fresh target must be unverified")
	}
	if tg.VerifyToken == "" {
		t.Fatalf("challenge token must be issued at creation")
	}

	if _, err := s.UpdateTarget(ctx, "org-a", tg.ID, "app-v2"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	list, err := s.ListTargets(ctx, "org-a", p.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %d", err, len(list))
	}
	if _, err := s.ListTargets(ctx, "org-b", p.ID); err != nil {
		t.Fatalf("foreign list must be empty not error: %v", err)
	}
	if err := s.DeleteTarget(ctx, "org-b", tg.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-org delete err = %v", err)
	}
	if err := s.DeleteTarget(ctx, "org-a", tg.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetTarget(ctx, "org-a", tg.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete err = %v", err)
	}
}

func TestTargetVerificationFlow(t *testing.T) {
	ctx := context.Background()
	s := newTestService()
	p := seedProject(t, s, "org-a", "proj")
	tg, err := s.CreateTarget(ctx, "org-a", p.ID, "app", "https://app.example.com")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Wrong / empty tokens are rejected and leave the target unverified.
	for _, bad := range []string{"", "deadbeef", tg.VerifyToken + "x"} {
		if _, err := s.ConfirmTargetVerification(ctx, "org-a", tg.ID, bad); !errors.Is(err, ErrTargetUnverified) {
			t.Errorf("confirm with %q err = %v, want ErrTargetUnverified", bad, err)
		}
		if got, _ := s.GetTarget(ctx, "org-a", tg.ID); got.Verified {
			t.Fatalf("target verified by bad token %q", bad)
		}
	}

	// Re-issuing rotates the challenge (last challenge wins).
	re, err := s.StartTargetVerification(ctx, "org-a", tg.ID)
	if err != nil {
		t.Fatalf("re-challenge: %v", err)
	}
	if re.VerifyToken == "" || re.VerifyToken == tg.VerifyToken {
		t.Fatalf("challenge token not rotated")
	}
	if _, err := s.ConfirmTargetVerification(ctx, "org-a", tg.ID, tg.VerifyToken); !errors.Is(err, ErrTargetUnverified) {
		t.Fatalf("stale token accepted")
	}

	// Correct token verifies and CONSUMES the one-time secret.
	if _, err := s.ConfirmTargetVerification(ctx, "org-a", tg.ID, re.VerifyToken); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	got, _ := s.GetTarget(ctx, "org-a", tg.ID)
	if !got.Verified || got.VerifiedAt == nil || got.VerifyToken != "" {
		t.Fatalf("verified target wrong: verified=%v at=%v token=%q", got.Verified, got.VerifiedAt, got.VerifyToken)
	}

	// Idempotent: re-confirm stays verified; re-challenge never un-verifies.
	if _, err := s.ConfirmTargetVerification(ctx, "org-a", tg.ID, "anything"); err != nil {
		t.Fatalf("idempotent confirm: %v", err)
	}
	noop, err := s.StartTargetVerification(ctx, "org-a", tg.ID)
	if err != nil {
		t.Fatalf("re-challenge on verified: %v", err)
	}
	if !noop.Verified || noop.VerifyToken != "" {
		t.Fatalf("re-challenge mutated a verified target: %+v", noop)
	}
}

// ---------------------------------------------------------------------------
// environments: validation table (types.go mode semantics)
// ---------------------------------------------------------------------------

func TestEnvironmentValidation(t *testing.T) {
	ctx := context.Background()
	s := newTestService()
	_, verified := seedVerifiedTarget(t, s, "org-a", "https://app.example.com")

	cases := []struct {
		name    string
		spec    EnvSpec
		wantErr error
		check   func(t *testing.T, e *Environment)
	}{
		{
			name: "production defaults to production_read_only",
			spec: EnvSpec{Kind: EnvProduction, AllowedDomains: []string{"app.example.com"}},
			check: func(t *testing.T, e *Environment) {
				if e.Mode != ModeProductionReadOnly {
					t.Fatalf("mode = %q, want production_read_only", e.Mode)
				}
			},
		},
		{
			name:    "pre_production is never valid on production",
			spec:    EnvSpec{Kind: EnvProduction, Mode: ModePreProduction, AllowedDomains: []string{"app.example.com"}},
			wantErr: ErrInvalid,
		},
		{
			name:    "production requires an allowlist",
			spec:    EnvSpec{Kind: EnvProduction, Mode: ModeProductionReadOnly},
			wantErr: ErrInvalid,
		},
		{
			name: "release_comparison allowed on production with allowlist",
			spec: EnvSpec{Kind: EnvProduction, Mode: ModeReleaseComparison, AllowedDomains: []string{"app.example.com"}},
		},
		{
			name: "dev defaults to pre_production",
			spec: EnvSpec{Kind: EnvDev},
			check: func(t *testing.T, e *Environment) {
				if e.Mode != ModePreProduction {
					t.Fatalf("mode = %q, want pre_production", e.Mode)
				}
			},
		},
		{
			name:    "rate caps only honored in pre_production",
			spec:    EnvSpec{Kind: EnvStaging, Mode: ModeProductionReadOnly, RateCapPerMin: 10},
			wantErr: ErrInvalid,
		},
		{
			name: "rate cap valid in pre_production",
			spec: EnvSpec{Kind: EnvStaging, RateCapPerMin: 10},
		},
		{
			name:    "rate cap negative",
			spec:    EnvSpec{Kind: EnvDev, RateCapPerMin: -1},
			wantErr: ErrInvalid,
		},
		{
			name:    "budget negative",
			spec:    EnvSpec{Kind: EnvDev, BudgetPerRun: -5},
			wantErr: ErrInvalid,
		},
		{
			name:    "unknown kind",
			spec:    EnvSpec{Kind: "canary"},
			wantErr: ErrInvalid,
		},
		{
			name:    "unknown mode",
			spec:    EnvSpec{Kind: EnvDev, Mode: "yolo"},
			wantErr: ErrInvalid,
		},
		{
			name:    "domain with scheme rejected",
			spec:    EnvSpec{Kind: EnvDev, AllowedDomains: []string{"https://app.example.com"}},
			wantErr: ErrInvalid,
		},
		{
			name:    "empty weekday out of range",
			spec:    EnvSpec{Kind: EnvDev, QuietWindows: []QuietWindow{{Weekdays: []int{7}, Start: "09:00", End: "17:00", Location: "UTC"}}},
			wantErr: ErrInvalid,
		},
		{
			name:    "bad window time",
			spec:    EnvSpec{Kind: EnvDev, TestWindows: []QuietWindow{{Weekdays: []int{1}, Start: "9am", End: "17:00", Location: "UTC"}}},
			wantErr: ErrInvalid,
		},
		{
			name:    "bad window location",
			spec:    EnvSpec{Kind: EnvDev, QuietWindows: []QuietWindow{{Weekdays: []int{1}, Start: "09:00", End: "17:00", Location: "Mars/Olympus"}}},
			wantErr: ErrInvalid,
		},
		{
			name: "windows valid",
			spec: EnvSpec{
				Kind:         EnvDev,
				QuietWindows: []QuietWindow{{Weekdays: []int{0, 6}, Start: "00:00", End: "23:59", Location: "Europe/Berlin", Reason: "weekend"}},
				TestWindows:  []QuietWindow{{Weekdays: []int{1, 2, 3, 4, 5}, Start: "22:00", End: "04:00", Location: "UTC"}},
			},
			check: func(t *testing.T, e *Environment) {
				if len(e.QuietWindows) != 1 || len(e.TestWindows) != 1 {
					t.Fatalf("windows lost: %+v", e)
				}
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			e, err := s.CreateEnvironment(ctx, "org-a", verified.ID, tc.spec)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if tc.check != nil {
				tc.check(t, e)
			}
		})
	}
}

func TestEnvironmentLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newTestService()
	_, verified := seedVerifiedTarget(t, s, "org-a", "https://app.example.com")
	p2, verified2 := seedVerifiedTarget(t, s, "org-b", "https://other.example.com")

	e, err := s.CreateEnvironment(ctx, "org-a", verified.ID, EnvSpec{
		Kind: EnvProduction, AllowedDomains: []string{"app.example.com", " cdn.example.com "},
		BudgetPerRun: 50,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Domains are normalized (trimmed/lowercased).
	if len(e.AllowedDomains) != 2 || e.AllowedDomains[1] != "cdn.example.com" {
		t.Fatalf("domains not normalized: %v", e.AllowedDomains)
	}

	// Anchoring to a foreign org's target is a 404, not a leak.
	if _, err := s.CreateEnvironment(ctx, "org-b", verified.ID, EnvSpec{Kind: EnvDev}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-org anchor err = %v", err)
	}

	// Update re-validates: cannot drop the production allowlist.
	if _, err := s.UpdateEnvironment(ctx, "org-a", e.ID, EnvSpec{Kind: EnvProduction}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("allowlist drop err = %v, want ErrInvalid", err)
	}
	// Update cannot escalate production to pre_production.
	if _, err := s.UpdateEnvironment(ctx, "org-a", e.ID, EnvSpec{Kind: EnvProduction, Mode: ModePreProduction, AllowedDomains: []string{"app.example.com"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("escalation err = %v, want ErrInvalid", err)
	}
	upd, err := s.UpdateEnvironment(ctx, "org-a", e.ID, EnvSpec{Kind: EnvProduction, AllowedDomains: []string{"app.example.com"}, BudgetPerRun: 99})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if upd.BudgetPerRun != 99 {
		t.Fatalf("budget not applied: %+v", upd)
	}
	// Identity fields are immutable on update.
	if upd.TargetID != verified.ID || upd.OrgID != "org-a" {
		t.Fatalf("identity mutated: %+v", upd)
	}

	// Lists are org-scoped and narrowable by target.
	if list, _ := s.ListEnvironments(ctx, "org-a", verified.ID); len(list) != 1 {
		t.Fatalf("org-a list = %d", len(list))
	}
	if list, _ := s.ListEnvironments(ctx, "org-a", verified2.ID); len(list) != 0 {
		t.Fatalf("org-a narrowed list = %d", len(list))
	}
	if list, _ := s.ListEnvironments(ctx, "org-b", ""); len(list) != 0 {
		t.Fatalf("org-b list = %d", len(list))
	}
	_ = p2

	if err := s.DeleteEnvironment(ctx, "org-b", e.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-org delete err = %v", err)
	}
	if err := s.DeleteEnvironment(ctx, "org-a", e.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetEnvironment(ctx, "org-a", e.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete err = %v", err)
	}
}

// ---------------------------------------------------------------------------
// personas
// ---------------------------------------------------------------------------

func TestPersonaLifecycleAndValidation(t *testing.T) {
	ctx := context.Background()
	s := newTestService()

	for _, tc := range []struct{ name, builtIn string }{
		{"", ""},                   // no name
		{"ghost", "not_a_builtin"}, // unknown built-in key
	} {
		if _, err := s.CreatePersona(ctx, "org-a", PersonaSpec{Name: tc.name, BuiltIn: tc.builtIn}); !errors.Is(err, ErrInvalid) {
			t.Errorf("persona(%q,%q) err = %v, want ErrInvalid", tc.name, tc.builtIn, err)
		}
	}

	p, err := s.CreatePersona(ctx, "org-a", PersonaSpec{
		Name: "Mobile Shopper", BuiltIn: "mobile_user",
		RoleHints:   []string{"shopper"},
		Constraints: map[string]string{"network": "slow3g", "viewport": "mobile"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if p.Version != 1 {
		t.Fatalf("initial version = %d, want 1", p.Version)
	}

	upd, err := s.UpdatePersona(ctx, "org-a", p.ID, PersonaSpec{Name: "Mobile Shopper v2", Constraints: map[string]string{"a11y": "strict"}})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if upd.Version != 2 || upd.Name != "Mobile Shopper v2" || upd.BuiltIn != "" {
		t.Fatalf("update wrong: %+v", upd)
	}

	custom, err := s.CreatePersona(ctx, "org-a", PersonaSpec{Name: "QA Bot"})
	if err != nil {
		t.Fatalf("custom persona: %v", err)
	}
	if custom.BuiltIn != "" {
		t.Fatalf("custom persona built_in = %q, want empty", custom.BuiltIn)
	}

	if list, _ := s.ListPersonas(ctx, "org-a"); len(list) != 2 {
		t.Fatalf("list = %d, want 2", len(list))
	}
	if _, err := s.GetPersona(ctx, "org-b", p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-org get err = %v", err)
	}
	if err := s.DeletePersona(ctx, "org-b", p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-org delete err = %v", err)
	}
	if err := s.DeletePersona(ctx, "org-a", custom.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

// ---------------------------------------------------------------------------
// AssertExecutionAllowed (the ScopeGuard seam)
// ---------------------------------------------------------------------------

func TestAssertExecutionAllowed(t *testing.T) {
	ctx := context.Background()

	t.Run("unverified target denies all", func(t *testing.T) {
		s := newTestService()
		p := seedProject(t, s, "org-a", "proj")
		tg, _ := s.CreateTarget(ctx, "org-a", p.ID, "app", "https://app.example.com")
		e, err := s.CreateEnvironment(ctx, "org-a", tg.ID, EnvSpec{Kind: EnvDev})
		if err != nil {
			t.Fatalf("env: %v", err)
		}
		for _, writeOp := range []bool{false, true} {
			d, err := s.AssertExecutionAllowed(ctx, "org-a", e.ID, "https://app.example.com/x", writeOp)
			if !errors.Is(err, ErrTargetUnverified) || d.Allowed {
				t.Fatalf("writeOp=%v: err = %v decision=%+v, want ErrTargetUnverified", writeOp, err, d)
			}
			if d.Reason != ReasonTargetUnverified {
				t.Fatalf("reason = %q, want %q", d.Reason, ReasonTargetUnverified)
			}
		}
	})

	t.Run("production_read_only denies writes allows reads", func(t *testing.T) {
		s := newTestService()
		_, verified := seedVerifiedTarget(t, s, "org-a", "https://app.example.com")
		e, err := s.CreateEnvironment(ctx, "org-a", verified.ID, EnvSpec{Kind: EnvProduction, AllowedDomains: []string{"app.example.com"}})
		if err != nil {
			t.Fatalf("env: %v", err)
		}
		if _, err := s.AssertExecutionAllowed(ctx, "org-a", e.ID, "https://app.example.com/prices", true); !errors.Is(err, ErrReadOnlyMode) {
			t.Fatalf("write err = %v, want ErrReadOnlyMode", err)
		}
		d, err := s.AssertExecutionAllowed(ctx, "org-a", e.ID, "https://app.example.com/prices", false)
		if err != nil || !d.Allowed || d.Reason != ReasonAllowlistedDomain {
			t.Fatalf("read verdict = %+v err = %v", d, err)
		}
	})

	t.Run("allowlist scope violations", func(t *testing.T) {
		s := newTestService()
		_, verified := seedVerifiedTarget(t, s, "org-a", "https://app.example.com")
		e, _ := s.CreateEnvironment(ctx, "org-a", verified.ID, EnvSpec{Kind: EnvStaging, AllowedDomains: []string{"app.example.com"}})
		d, err := s.AssertExecutionAllowed(ctx, "org-a", e.ID, "https://evil.example.net/", false)
		if !errors.Is(err, ErrScopeViolation) || d.Allowed || d.Reason != ReasonDomainNotAllowed {
			t.Fatalf("verdict = %+v err = %v", d, err)
		}
		// Suffix matching respects label boundaries: evilexample.com != example.com.
		if _, err := s.AssertExecutionAllowed(ctx, "org-a", e.ID, "https://evilexample.com/", false); !errors.Is(err, ErrScopeViolation) {
			t.Fatalf("lookalike host accepted")
		}
		// Subdomains of an allowlisted domain are inside scope.
		if d, err := s.AssertExecutionAllowed(ctx, "org-a", e.ID, "https://api.app.example.com/v1", false); err != nil || !d.Allowed {
			t.Fatalf("subdomain denied: %+v %v", d, err)
		}
	})

	t.Run("no allowlist on non-production is documented allow", func(t *testing.T) {
		s := newTestService()
		_, verified := seedVerifiedTarget(t, s, "org-a", "https://app.example.com")
		e, _ := s.CreateEnvironment(ctx, "org-a", verified.ID, EnvSpec{Kind: EnvLocal})
		d, err := s.AssertExecutionAllowed(ctx, "org-a", e.ID, "http://localhost:3000/", false)
		if err != nil || !d.Allowed || d.Reason != ReasonNoAllowlist {
			t.Fatalf("verdict = %+v err = %v", d, err)
		}
	})

	t.Run("pre_production allows writes", func(t *testing.T) {
		s := newTestService()
		_, verified := seedVerifiedTarget(t, s, "org-a", "https://app.example.com")
		e, _ := s.CreateEnvironment(ctx, "org-a", verified.ID, EnvSpec{Kind: EnvStaging, AllowedDomains: []string{"app.example.com"}})
		d, err := s.AssertExecutionAllowed(ctx, "org-a", e.ID, "https://app.example.com/cart", true)
		if err != nil || !d.Allowed || d.Reason != ReasonPreProductionWrite {
			t.Fatalf("verdict = %+v err = %v", d, err)
		}
	})

	t.Run("release_comparison posture", func(t *testing.T) {
		s := newTestService()
		_, verified := seedVerifiedTarget(t, s, "org-a", "https://app.example.com")
		e, _ := s.CreateEnvironment(ctx, "org-a", verified.ID, EnvSpec{Kind: EnvProduction, Mode: ModeReleaseComparison, AllowedDomains: []string{"app.example.com"}})
		d, err := s.AssertExecutionAllowed(ctx, "org-a", e.ID, "https://app.example.com/checkout", true)
		if err != nil || !d.Allowed || d.Reason != ReasonReleaseComparison {
			t.Fatalf("verdict = %+v err = %v", d, err)
		}
	})

	t.Run("rate cap enforced in pre_production only", func(t *testing.T) {
		s := newTestService()
		_, verified := seedVerifiedTarget(t, s, "org-a", "https://app.example.com")
		e, _ := s.CreateEnvironment(ctx, "org-a", verified.ID, EnvSpec{Kind: EnvDev, RateCapPerMin: 2})
		for i := 0; i < 2; i++ {
			if _, err := s.AssertExecutionAllowed(ctx, "org-a", e.ID, "https://x.example.com/", false); err != nil {
				t.Fatalf("call %d denied: %v", i, err)
			}
		}
		d, err := s.AssertExecutionAllowed(ctx, "org-a", e.ID, "https://x.example.com/", false)
		if !errors.Is(err, ErrScopeViolation) || d.Reason != ReasonRateCapped {
			t.Fatalf("verdict = %+v err = %v, want rate_capped", d, err)
		}
		// A read-only-mode env with a cap was rejected at validation, so the
		// cap can never silently govern non-pre_production envs.
	})

	t.Run("cross-org environment is 404", func(t *testing.T) {
		s := newTestService()
		_, verified := seedVerifiedTarget(t, s, "org-a", "https://app.example.com")
		e, _ := s.CreateEnvironment(ctx, "org-a", verified.ID, EnvSpec{Kind: EnvDev})
		if _, err := s.AssertExecutionAllowed(ctx, "org-b", e.ID, "https://app.example.com/", false); !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})
}

// ---------------------------------------------------------------------------
// store-backed mode (fake recording store)
// ---------------------------------------------------------------------------

func TestStoreBackedMode(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	s := NewServiceWithStore(store)

	p, err := s.CreateProject(ctx, "org-a", "durable")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(store.projects) != 1 {
		t.Fatalf("store not written through")
	}

	// Reads flow through to the store: a record only the store knows is
	// visible (the store is the source of truth).
	store.projects["only-in-store"] = &Project{ID: "only-in-store", OrgID: "org-a", Name: "ghost"}
	got, err := s.GetProject(ctx, "org-a", "only-in-store")
	if err != nil || got.Name != "ghost" {
		t.Fatalf("store read-through failed: %v %+v", err, got)
	}

	// Store errors propagate (never swallowed into silent 200s).
	store.failOn = "CreateProject"
	if _, err := s.CreateProject(ctx, "org-a", "nope"); !errors.Is(err, store.errBoom) {
		t.Fatalf("store error swallowed: %v", err)
	}
	store.failOn = "UpdateTarget"
	tg, err := s.CreateTarget(ctx, "org-a", p.ID, "app", "https://x.io")
	if err != nil {
		t.Fatalf("target: %v", err)
	}
	if _, err := s.ConfirmTargetVerification(ctx, "org-a", tg.ID, "t"); !errors.Is(err, ErrTargetUnverified) {
		t.Fatalf("confirm: %v", err)
	}
	store.failOn = ""
	challenge, err := s.StartTargetVerification(ctx, "org-a", tg.ID)
	if err != nil {
		t.Fatalf("challenge: %v", err)
	}
	store.failOn = "UpdateTarget"
	if _, err := s.ConfirmTargetVerification(ctx, "org-a", tg.ID, challenge.VerifyToken); !errors.Is(err, store.errBoom) {
		t.Fatalf("update error swallowed: %v", err)
	}
	store.failOn = ""

	// Tenant guard stays intact in store mode.
	if _, err := s.GetProject(ctx, "org-b", p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-org err = %v", err)
	}

	// Verification state persists through the store.
	challenge, _ = s.StartTargetVerification(ctx, "org-a", tg.ID)
	if _, err := s.ConfirmTargetVerification(ctx, "org-a", tg.ID, challenge.VerifyToken); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if !store.targets[tg.ID].Verified || store.targets[tg.ID].VerifyToken != "" {
		t.Fatalf("verification not persisted: %+v", store.targets[tg.ID])
	}
}

// nil-service defensive contract (house style).
func TestNilService(t *testing.T) {
	var s *Service
	if _, err := s.CreateProject(context.Background(), "org", "x"); err == nil {
		t.Fatal("nil service must error")
	}
	if _, err := s.GetProject(context.Background(), "org", "x"); err == nil {
		t.Fatal("nil service get must error")
	}
}

// ---------------------------------------------------------------------------
// small helpers under test
// ---------------------------------------------------------------------------

func TestHostOf(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://App.Example.com:8443/a/b?q=1", "app.example.com"},
		{"http://user:pw@host.io/x", "host.io"},
		{"host.io:443/x", "host.io"},
		{"[::1]:8080/x", "::1"},
		{"", ""},
	} {
		if got := hostOf(tc.in); got != tc.want {
			t.Errorf("hostOf(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestValidHHMM(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"00:00", true}, {"23:59", true}, {"24:00", false}, {"9:00", false},
		{"09:60", false}, {"ab:cd", false}, {"", false}, {"0900", false},
	} {
		if got := validHHMM(tc.in); got != tc.want {
			t.Errorf("validHHMM(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestDefaultBudgetPerRunConstant(t *testing.T) {
	// The types.go contract: BudgetPerRun 0 = default 200. The constant the
	// worker tracks consume must stay pinned.
	if DefaultBudgetPerRun != 200 {
		t.Fatalf("DefaultBudgetPerRun = %d, want 200", DefaultBudgetPerRun)
	}
}

func TestChallengeTokenUniqueness(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		tok := newChallengeToken()
		if len(tok) != 64 {
			t.Fatalf("token length = %d, want 64 hex chars", len(tok))
		}
		if seen[tok] {
			t.Fatalf("duplicate challenge token")
		}
		seen[tok] = true
	}
}

// Concurrency smoke: parallel CRUD on one service must not race (run under
// -race in CI).
func TestConcurrentAccess(t *testing.T) {
	s := newTestService()
	ctx := context.Background()
	p := seedProject(t, s, "org-a", "proj")
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = s.CreateTarget(ctx, "org-a", p.ID, fmt.Sprintf("t%d", i), "https://x.io")
			_, _ = s.ListProjects(ctx, "org-a")
			_, _ = s.ListTargets(ctx, "org-a", p.ID)
		}(i)
	}
	wg.Wait()
}
