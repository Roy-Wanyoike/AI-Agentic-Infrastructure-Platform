package synth

// journeys_test.go — issue #149: table-driven coverage for the journey
// lifecycle (create/update/review/schedule/retire/clone), spec validation,
// capability coverage mapping and run-input snapshot reproducibility.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// journeyFixture builds a valid minimal journey for tests.
func journeyFixture(org string) Journey {
	return Journey{
		OrgID:         org,
		ProjectID:     "proj-1",
		EnvironmentID: "env-1",
		PersonaID:     "per-1",
		Name:          "Login works",
		Status:        JourneyDraft,
		Risk:          RiskLow,
		Source:        SourceAuthored,
		Preconditions: []string{"user exists"},
		Steps: []JourneyStep{
			{Index: 0, Action: StepNavigate, Target: "https://app.test/login"},
			{Index: 1, Action: StepFill, Target: "#email", Value: "synth-visitor-abc123", WriteOp: true},
			{Index: 2, Action: StepClick, Target: "#submit"},
		},
		Assertions: []Assertion{
			{Kind: AssertURLPattern, Target: "url", Expected: "https://app.test/dashboard", Severity: RiskMedium},
			{Kind: AssertNoConsoleError, Expected: "0", Severity: RiskLow},
		},
		Cleanup: []JourneyStep{
			{Index: 0, Action: StepAPICall, Target: "DELETE /v1/test-users/synth-visitor-abc123", WriteOp: true},
		},
		Tags: []string{"auth"},
	}
}

// seedPersona registers the persona a fixture journey references.
func seedPersona(t *testing.T, ctx context.Context, svc *JourneyService, org, id string) {
	t.Helper()
	if err := svc.personas.PutPersona(ctx, Persona{ID: id, OrgID: org, Name: "Test Persona", RoleHints: []string{"authenticated"}}); err != nil {
		t.Fatalf("seed persona: %v", err)
	}
}

func newTestJourneyService(t *testing.T) *JourneyService {
	t.Helper()
	ctx := context.Background()
	js := NewJourneyService(NewInMemoryJourneyStore(), NewInMemoryPersonaStore())
	now := time.Date(2025, 3, 1, 10, 0, 0, 0, time.UTC)
	js.now = func() time.Time { now = now.Add(time.Second); return now }
	seq := 0
	js.newID = func() string { seq++; return fmt.Sprintf("jrn-%02d", seq) }
	seedPersona(t, ctx, js, "org-1", "per-1")
	return js
}

func TestValidateJourney(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(j *Journey)
		wantErr error // nil = valid; sentinel match via errors.Is
	}{
		{"valid", func(j *Journey) {}, nil},
		{"empty name", func(j *Journey) { j.Name = "" }, ErrInvalid},
		{"missing org", func(j *Journey) { j.OrgID = "" }, ErrInvalid},
		{"missing project", func(j *Journey) { j.ProjectID = "" }, ErrInvalid},
		{"missing environment", func(j *Journey) { j.EnvironmentID = "" }, ErrInvalid},
		{"missing persona", func(j *Journey) { j.PersonaID = "" }, ErrInvalid},
		{"bad status", func(j *Journey) { j.Status = "archived" }, ErrInvalid},
		{"missing risk", func(j *Journey) { j.Risk = "" }, ErrInvalid},
		{"bad risk", func(j *Journey) { j.Risk = "existential" }, ErrInvalid},
		{"bad source", func(j *Journey) { j.Source = "dreamed" }, ErrInvalid},
		{"empty precondition", func(j *Journey) { j.Preconditions = []string{"ok", " "} }, ErrInvalid},
		{"no steps", func(j *Journey) { j.Steps = nil }, ErrInvalid},
		{"step index gap", func(j *Journey) { j.Steps[2].Index = 5 }, ErrInvalid},
		{"invalid action", func(j *Journey) { j.Steps[0].Action = "hover" }, ErrInvalid},
		{"step without target", func(j *Journey) { j.Steps[0].Target = "" }, ErrInvalid},
		{"wait without target ok", func(j *Journey) {
			j.Steps = append(j.Steps, JourneyStep{Index: 3, Action: StepWait, Target: "", Value: "200"})
		}, nil},
		{"write journey without cleanup", func(j *Journey) { j.Cleanup = nil }, ErrCleanupRequired},
		{"cleanup index gap", func(j *Journey) { j.Cleanup[0].Index = 3 }, ErrInvalid},
		{"cleanup invalid action", func(j *Journey) { j.Cleanup[0].Action = "teleport" }, ErrInvalid},
		{"no assertions", func(j *Journey) { j.Assertions = nil }, ErrAssertionsRequired},
		{"unknown assertion kind", func(j *Journey) {
			j.Assertions[0].Kind = "vibes_positive"
		}, ErrInvalid},
		{"assertion without target", func(j *Journey) {
			j.Assertions[0].Target = ""
		}, ErrInvalid},
		{"assertion without expected", func(j *Journey) {
			j.Assertions[0].Expected = ""
		}, ErrInvalid},
		{"assertion without severity", func(j *Journey) {
			j.Assertions[0].Severity = ""
		}, ErrInvalid},
		{"no_console_error may omit target", func(j *Journey) {
			j.Assertions[1].Target = ""
		}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			j := journeyFixture("org-1")
			tc.mutate(&j)
			err := ValidateJourney(&j)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("ValidateJourney() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ValidateJourney() = %v, want %v", err, tc.wantErr)
			}
		})
	}
	t.Run("nil journey", func(t *testing.T) {
		if err := ValidateJourney(nil); !errors.Is(err, ErrInvalid) {
			t.Fatalf("err = %v, want ErrInvalid", err)
		}
	})
}

func TestFlagWriteOps(t *testing.T) {
	tests := []struct {
		action string
		target string
		want   bool
	}{
		{StepFill, "#email", true},
		{StepSelect, "#country", true},
		{StepNavigate, "https://app.test", false},
		{StepClick, "#submit", false}, // author judgment stays author judgment
		{StepAPICall, "GET /v1/users", false},
		{StepAPICall, "POST /v1/users", true},
		{StepAPICall, "", false},
		{StepWait, "200", false},
		{StepExtract, ".price", false},
	}
	for _, tc := range tests {
		got := FlagWriteOps([]JourneyStep{{Action: tc.action, Target: tc.target}})
		if got[0].WriteOp != tc.want {
			t.Errorf("FlagWriteOps(%s %q).WriteOp = %v, want %v", tc.action, tc.target, got[0].WriteOp, tc.want)
		}
	}
	// Input must not be mutated.
	in := []JourneyStep{{Action: StepFill, Target: "#email"}}
	_ = FlagWriteOps(in)
	if in[0].WriteOp {
		t.Fatalf("FlagWriteOps mutated its input")
	}
}

func TestCanTransitionJourney(t *testing.T) {
	tests := []struct {
		from, to JourneyStatus
		want     bool
	}{
		{JourneyDraft, JourneyReviewed, true},
		{JourneyDraft, JourneyRetired, true},
		{JourneyDraft, JourneyScheduled, false}, // review is not optional
		{JourneyDraft, JourneyDraft, false},
		{JourneyReviewed, JourneyScheduled, true},
		{JourneyReviewed, JourneyRetired, true},
		{JourneyReviewed, JourneyDraft, false}, // immutable; clone instead
		{JourneyScheduled, JourneyRetired, true},
		{JourneyScheduled, JourneyReviewed, false},
		{JourneyRetired, JourneyDraft, false}, // terminal
		{JourneyRetired, JourneyScheduled, false},
	}
	for _, tc := range tests {
		if got := CanTransitionJourney(tc.from, tc.to); got != tc.want {
			t.Errorf("CanTransitionJourney(%s → %s) = %v, want %v", tc.from, tc.to, got, tc.want)
		}
	}
}

func TestJourneyServiceLifecycle(t *testing.T) {
	ctx := context.Background()
	js := newTestJourneyService(t)

	created, err := js.CreateJourney(ctx, journeyFixture("org-1"))
	if err != nil {
		t.Fatalf("CreateJourney: %v", err)
	}
	if created.Status != JourneyDraft || created.Version != 1 || created.ID == "" {
		t.Fatalf("created journey not stamped: %+v", created)
	}

	// Drafts are editable; edits bump the version.
	updated, err := js.UpdateJourney(ctx, "org-1", created.ID, func(j *Journey) {
		j.Name = "Login works (v2)"
	})
	if err != nil {
		t.Fatalf("UpdateJourney: %v", err)
	}
	if updated.Version != 2 || updated.Status != JourneyDraft {
		t.Fatalf("update result: version=%d status=%s", updated.Version, updated.Status)
	}

	// Create forces draft even if the caller pretends otherwise.
	pretend := journeyFixture("org-1")
	pretend.Status = JourneyScheduled
	forced, err := js.CreateJourney(ctx, pretend)
	if err != nil {
		t.Fatalf("CreateJourney(pretend): %v", err)
	}
	if forced.Status != JourneyDraft {
		t.Fatalf("CreateJourney allowed status %s", forced.Status)
	}

	// Review requires a reviewer.
	if _, err := js.ReviewJourney(ctx, "org-1", created.ID, "  "); !errors.Is(err, ErrInvalid) {
		t.Fatalf("ReviewJourney(empty reviewer) = %v, want ErrInvalid", err)
	}
	reviewed, err := js.ReviewJourney(ctx, "org-1", created.ID, "alice")
	if err != nil {
		t.Fatalf("ReviewJourney: %v", err)
	}
	if reviewed.Status != JourneyReviewed {
		t.Fatalf("status after review = %s", reviewed.Status)
	}
	revs := js.Reviews(created.ID)
	if len(revs) != 1 || revs[0].Reviewer != "alice" || revs[0].Version != 2 {
		t.Fatalf("review record = %+v", revs)
	}

	// Reviewed is immutable.
	if _, err := js.UpdateJourney(ctx, "org-1", created.ID, func(j *Journey) {}); !errors.Is(err, ErrJourneyImmutable) {
		t.Fatalf("update reviewed = %v, want ErrJourneyImmutable", err)
	}

	// Schedule the reviewed journey.
	scheduled, err := js.ScheduleJourney(ctx, "org-1", created.ID)
	if err != nil {
		t.Fatalf("ScheduleJourney: %v", err)
	}
	if scheduled.Status != JourneyScheduled {
		t.Fatalf("status after schedule = %s", scheduled.Status)
	}

	// Retire from scheduled; retired is terminal.
	retired, err := js.RetireJourney(ctx, "org-1", created.ID)
	if err != nil {
		t.Fatalf("RetireJourney: %v", err)
	}
	if retired.Status != JourneyRetired {
		t.Fatalf("status after retire = %s", retired.Status)
	}
	for _, op := range []struct {
		name string
		call func() error
	}{
		{"review retired", func() error { _, err := js.ReviewJourney(ctx, "org-1", created.ID, "bob"); return err }},
		{"schedule retired", func() error { _, err := js.ScheduleJourney(ctx, "org-1", created.ID); return err }},
		{"retire retired", func() error { _, err := js.RetireJourney(ctx, "org-1", created.ID); return err }},
	} {
		if err := op.call(); !errors.Is(err, ErrTransition) {
			t.Errorf("%s = %v, want ErrTransition", op.name, err)
		}
	}

	// Retired specs may be deleted (drafts too); reviewed/scheduled may not.
	if err := js.DeleteJourney(ctx, "org-1", created.ID); err != nil {
		t.Fatalf("DeleteJourney(retired): %v", err)
	}
	draft, err := js.CreateJourney(ctx, journeyFixture("org-1"))
	if err != nil {
		t.Fatalf("CreateJourney: %v", err)
	}
	if err := js.DeleteJourney(ctx, "org-1", draft.ID); err != nil {
		t.Fatalf("DeleteJourney(draft): %v", err)
	}
	if _, err := js.GetJourney(ctx, "org-1", draft.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted journey get = %v, want ErrNotFound", err)
	}
}

func TestSchedulingGates(t *testing.T) {
	ctx := context.Background()
	js := newTestJourneyService(t)

	// Draft cannot be scheduled — review is mandatory for every class.
	draft, err := js.CreateJourney(ctx, journeyFixture("org-1"))
	if err != nil {
		t.Fatalf("CreateJourney: %v", err)
	}
	if _, err := js.ScheduleJourney(ctx, "org-1", draft.ID); !errors.Is(err, ErrTransition) {
		t.Fatalf("schedule draft = %v, want ErrTransition", err)
	}

	// Risk classification is re-verified at schedule time.
	high := journeyFixture("org-1")
	high.Risk = RiskCritical
	critical, err := js.CreateJourney(ctx, high)
	if err != nil {
		t.Fatalf("CreateJourney(critical): %v", err)
	}
	if _, err := js.ReviewJourney(ctx, "org-1", critical.ID, "alice"); err != nil {
		t.Fatalf("ReviewJourney: %v", err)
	}
	scheduled, err := js.ScheduleJourney(ctx, "org-1", critical.ID)
	if err != nil {
		t.Fatalf("ScheduleJourney(critical, reviewed): %v", err)
	}
	if scheduled.Status != JourneyScheduled {
		t.Fatalf("critical journey not scheduled: %s", scheduled.Status)
	}

	// Version-bump clone of a high-risk journey: the NEW version has no
	// review record yet, so scheduling it must be refused (defense in depth
	// on top of the draft→reviewed chain).
	clone, err := js.CloneJourney(ctx, "org-1", critical.ID)
	if err != nil {
		t.Fatalf("CloneJourney: %v", err)
	}
	if clone.Status != JourneyDraft || clone.Version != scheduled.Version+1 || clone.ID == scheduled.ID {
		t.Fatalf("clone wrong: %+v", clone)
	}
	if _, err := js.ScheduleJourney(ctx, "org-1", clone.ID); !errors.Is(err, ErrTransition) {
		t.Fatalf("schedule clone (draft) = %v, want ErrTransition", err)
	}
	if _, err := js.ReviewJourney(ctx, "org-1", clone.ID, "bob"); err != nil {
		t.Fatalf("ReviewJourney(clone): %v", err)
	}
	afterReview, err := js.ScheduleJourney(ctx, "org-1", clone.ID)
	if err != nil {
		t.Fatalf("ScheduleJourney(clone reviewed): %v", err)
	}
	if afterReview.Version != clone.Version {
		t.Fatalf("scheduled version drift: %d vs %d", afterReview.Version, clone.Version)
	}
	if revs := js.Reviews(clone.ID); len(revs) != 1 || revs[0].Version != clone.Version {
		t.Fatalf("clone review records = %+v", revs)
	}
}

func TestJourneyOrgIsolation(t *testing.T) {
	ctx := context.Background()
	js := newTestJourneyService(t)
	created, err := js.CreateJourney(ctx, journeyFixture("org-1"))
	if err != nil {
		t.Fatalf("CreateJourney: %v", err)
	}
	for _, op := range []struct {
		name string
		call func() error
	}{
		{"get", func() error { _, err := js.GetJourney(ctx, "org-2", created.ID); return err }},
		{"review", func() error { _, err := js.ReviewJourney(ctx, "org-2", created.ID, "mallory"); return err }},
		{"schedule", func() error { _, err := js.ScheduleJourney(ctx, "org-2", created.ID); return err }},
		{"retire", func() error { _, err := js.RetireJourney(ctx, "org-2", created.ID); return err }},
		{"clone", func() error { _, err := js.CloneJourney(ctx, "org-2", created.ID); return err }},
		{"delete", func() error { return js.DeleteJourney(ctx, "org-2", created.ID) }},
	} {
		if err := op.call(); !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrInvalid) {
			t.Errorf("cross-org %s = %v, want ErrNotFound/ErrInvalid", op.name, err)
		}
	}
	// A journey cannot reference another org's persona.
	seedPersona(t, ctx, js, "org-2", "per-2")
	foreign := journeyFixture("org-2")
	foreign.PersonaID = "per-1" // org-1's persona
	if _, err := js.CreateJourney(ctx, foreign); err == nil || !errors.Is(err, ErrNotFound) {
		t.Fatalf("journey with foreign persona = %v, want ErrNotFound", err)
	}
}

func TestCloneJourneyDeepCopy(t *testing.T) {
	ctx := context.Background()
	js := newTestJourneyService(t)
	src, err := js.CreateJourney(ctx, journeyFixture("org-1"))
	if err != nil {
		t.Fatalf("CreateJourney: %v", err)
	}
	if _, err := js.ReviewJourney(ctx, "org-1", src.ID, "alice"); err != nil {
		t.Fatalf("ReviewJourney: %v", err)
	}
	clone, err := js.CloneJourney(ctx, "org-1", src.ID)
	if err != nil {
		t.Fatalf("CloneJourney: %v", err)
	}
	// Mutating the clone must never touch the source.
	clone.Steps[0].Target = "https://evil.test"
	clone.Assertions = nil
	clone.Preconditions[0] = "tampered"
	after, err := js.GetJourney(ctx, "org-1", src.ID)
	if err != nil {
		t.Fatalf("GetJourney: %v", err)
	}
	if after.Steps[0].Target != "https://app.test/login" || len(after.Assertions) != 2 ||
		after.Preconditions[0] != "user exists" {
		t.Fatalf("clone leaked into source: %+v", after)
	}
	if after.Status != JourneyReviewed || after.Version != 1 {
		t.Fatalf("source mutated by clone: status=%s version=%d", after.Status, after.Version)
	}
}

func TestMapCapabilities(t *testing.T) {
	journeys := []Journey{
		{ID: "j1", Status: JourneyScheduled, Tags: []string{"Billing", "checkout"}},
		{ID: "j2", Status: JourneyDraft, Tags: []string{" billing "}},
		{ID: "j3", Status: JourneyReviewed, Tags: []string{"auth", "  "}},
		{ID: "j4", Status: JourneyRetired, Tags: nil},
	}
	report := MapCapabilities(journeys)
	if report.TotalJourneys != 4 {
		t.Fatalf("TotalJourneys = %d, want 4", report.TotalJourneys)
	}
	if len(report.Capabilities) != 3 {
		t.Fatalf("capabilities = %d, want 3 (billing, checkout, auth)", len(report.Capabilities))
	}
	// Sorted capability names.
	if report.Capabilities[0].Capability != "auth" ||
		report.Capabilities[1].Capability != "billing" ||
		report.Capabilities[2].Capability != "checkout" {
		t.Fatalf("capabilities not sorted/normalized: %+v", report.Capabilities)
	}
	billing, ok := report.CoverageFor("BILLING")
	if !ok {
		t.Fatalf("CoverageFor(billing) missing")
	}
	if len(billing.JourneyIDs) != 2 || billing.JourneyIDs[0] != "j1" || billing.JourneyIDs[1] != "j2" {
		t.Fatalf("billing journeys = %v, want [j1 j2]", billing.JourneyIDs)
	}
	if billing.StatusCounts[JourneyScheduled] != 1 || billing.StatusCounts[JourneyDraft] != 1 {
		t.Fatalf("billing status counts = %v", billing.StatusCounts)
	}
	// Untagged journeys are listed, never silently counted as coverage.
	if len(report.UntaggedJourneys) != 1 || report.UntaggedJourneys[0] != "j4" {
		t.Fatalf("untagged = %v, want [j4]", report.UntaggedJourneys)
	}
	// Gaps: wanted capabilities with zero coverage.
	gaps := report.Gaps([]string{"Auth", "search", "billing", "", "search"})
	if len(gaps) != 1 || gaps[0] != "search" {
		t.Fatalf("gaps = %v, want [search]", gaps)
	}
}

func TestVersionedRunInputsDeterministic(t *testing.T) {
	build := func() []byte {
		j := journeyFixture("org-1")
		j.ID = "jrn_1"
		j.Status = JourneyScheduled
		j.Version = 7
		// Same constraints, different insertion order every iteration.
		p := Persona{ID: "per_1", OrgID: "org-1", Name: "Mobile User", BuiltIn: PersonaMobileUser, Version: 3,
			RoleHints:   []string{"z-hint", "a-hint", "m-hint"},
			Constraints: map[string]string{}}
		for i, k := range []string{"z", "a", "m", "network", "viewport", "input"} {
			_ = i
			p.Constraints[k] = "v" + k
		}
		p.Constraints[ConstraintNetwork] = NetworkSlow3G
		p.Constraints[ConstraintViewport] = ViewportMobile
		p.Constraints[ConstraintInput] = InputTouch
		env := &Environment{ID: "env_1", OrgID: "org-1", TargetID: "tgt_1", Kind: EnvStaging, Mode: ModePreProduction,
			AllowedDomains: []string{"z.test", "a.test", "m.test"}, RateCapPerMin: 10, BudgetPerRun: 200}
		// Timestamps differ per construction — they must NOT affect bytes.
		j.CreatedAt = time.Now()
		j.UpdatedAt = time.Now()
		p.CreatedAt = time.Now()
		return VersionedRunInputs(&j, &p, env)
	}
	first := build()
	if first == nil {
		t.Fatalf("VersionedRunInputs returned nil for valid inputs")
	}
	for i := 0; i < 50; i++ {
		if !bytes.Equal(first, build()) {
			t.Fatalf("run inputs not deterministic at iteration %d", i)
		}
	}
	// Canonical JSON: re-encoding the decoded document must round-trip.
	var decoded map[string]any
	if err := json.Unmarshal(first, &decoded); err != nil {
		t.Fatalf("snapshot is not JSON: %v", err)
	}
	if decoded["schema"] != RunInputsSchemaV1 {
		t.Fatalf("snapshot schema = %v", decoded["schema"])
	}

	// Sensitivity: every version-bearing change must change the bytes.
	j := journeyFixture("org-1")
	j.ID = "jrn_1"
	p := Persona{ID: "per_1", OrgID: "org-1", Name: "P", Version: 1, RoleHints: []string{"r"}}
	env := &Environment{ID: "env_1", Kind: EnvDev, Mode: ModePreProduction}
	base := VersionedRunInputs(&j, &p, env)

	jv2 := j
	jv2.Version = 2
	if bytes.Equal(base, VersionedRunInputs(&jv2, &p, env)) {
		t.Fatalf("journey version bump did not change snapshot")
	}
	pv2 := p
	pv2.Version = 2
	if bytes.Equal(base, VersionedRunInputs(&j, &pv2, env)) {
		t.Fatalf("persona version bump did not change snapshot")
	}
	ev2 := *env
	ev2.Mode = ModeProductionReadOnly
	if bytes.Equal(base, VersionedRunInputs(&j, &p, &ev2)) {
		t.Fatalf("environment mode change did not change snapshot")
	}
	jAdd := j
	jAdd.Assertions = append([]Assertion(nil), j.Assertions...)
	jAdd.Assertions[0].Expected = "https://other.test"
	if bytes.Equal(base, VersionedRunInputs(&jAdd, &p, env)) {
		t.Fatalf("assertion change did not change snapshot")
	}
}

func TestVersionedRunInputsNilInputs(t *testing.T) {
	j := journeyFixture("org-1")
	p := Persona{ID: "per", Version: 1}
	env := &Environment{ID: "env"}
	if got := VersionedRunInputs(nil, &p, env); got != nil {
		t.Fatalf("nil journey → %v, want nil", got)
	}
	if got := VersionedRunInputs(&j, nil, env); got != nil {
		t.Fatalf("nil persona → %v, want nil", got)
	}
	if got := VersionedRunInputs(&j, &p, nil); got != nil {
		t.Fatalf("nil environment → %v, want nil", got)
	}
}

func TestVersionedRunInputsExcludesTimestamps(t *testing.T) {
	// The reproducibility contract: same (id, version) + content ⇒ same
	// bytes, regardless of when the records were written. Clone a journey
	// through the service (fresh timestamps, new id) and pin both specs at
	// the same version/content — only then compare content shapes.
	j1 := journeyFixture("org-1")
	j2 := journeyFixture("org-1")
	j1.ID, j2.ID = "jrn_x", "jrn_x"
	j1.Version, j2.Version = 4, 4
	j1.CreatedAt = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	j2.CreatedAt = time.Date(2030, 6, 15, 0, 0, 0, 0, time.UTC)
	p1 := Persona{ID: "per_x", Version: 2, Name: "P", RoleHints: []string{"r"}}
	p2 := p1
	p2.CreatedAt = time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	env1 := &Environment{ID: "env_x", Kind: EnvDev, Mode: ModePreProduction}
	env2 := &Environment{ID: "env_x", Kind: EnvDev, Mode: ModePreProduction}
	env1.CreatedAt = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	env2.CreatedAt = time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC)
	b1 := VersionedRunInputs(&j1, &p1, env1)
	b2 := VersionedRunInputs(&j2, &p2, env2)
	if !bytes.Equal(b1, b2) {
		t.Fatalf("timestamps leaked into run input snapshot:\n%s\n---\n%s", b1, b2)
	}
	// And the snapshot must not contain epoch strings at all.
	if strings.Contains(string(b1), "created_at") || strings.Contains(string(b1), "updated_at") {
		t.Fatalf("snapshot embeds timestamp fields: %s", b1)
	}
}

func TestJourneyStoreContract(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryJourneyStore()
	j := journeyFixture("org-1")
	j.ID = "jrn_1"
	if err := store.PutJourney(ctx, j); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := store.GetJourney(ctx, "org-2", "jrn_1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-org Get = %v, want ErrNotFound", err)
	}
	if err := store.DeleteJourney(ctx, "org-1", "jrn_1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.GetJourney(ctx, "org-1", "jrn_1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("post-delete Get = %v, want ErrNotFound", err)
	}
}
