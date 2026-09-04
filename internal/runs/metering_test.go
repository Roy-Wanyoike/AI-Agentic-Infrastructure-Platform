package runs

// Metering tests (issue #57): the step-type count aggregate that backs the
// tool_calls_count meter — in-memory mode (service step cache), Postgres mode
// (sqlmock, tenant-guarded SQL pinned) and the honest error when a store
// cannot aggregate.

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func meterTestService(t *testing.T) (*Service, string) {
	t.Helper()
	svc := NewService()
	org := "org-meter"
	// Two runs for the org, one for a foreign tenant.
	runA, err := svc.CreateRunCtx(context.Background(), org, "agent-1", "hello")
	if err != nil {
		t.Fatalf("CreateRunCtx(A) returned error: %v", err)
	}
	runB, err := svc.CreateRunCtx(context.Background(), org, "agent-2", "world")
	if err != nil {
		t.Fatalf("CreateRunCtx(B) returned error: %v", err)
	}
	foreign, err := svc.CreateRunCtx(context.Background(), "org-other", "agent-1", "mine")
	if err != nil {
		t.Fatalf("CreateRunCtx(foreign) returned error: %v", err)
	}

	// runA: 3 tool steps + 1 model step; runB: 1 tool step; foreign: 2 tool
	// steps (must never be counted for org-meter).
	for i := 0; i < 3; i++ {
		if err := svc.RecordStep(context.Background(), org, runA.ID, &Step{StepType: StepTypeTool, Status: "COMPLETED"}); err != nil {
			t.Fatalf("RecordStep(tool A%d) returned error: %v", i, err)
		}
	}
	if err := svc.RecordStep(context.Background(), org, runA.ID, &Step{StepType: StepTypeModel, Status: "COMPLETED"}); err != nil {
		t.Fatalf("RecordStep(model) returned error: %v", err)
	}
	if err := svc.RecordStep(context.Background(), org, runB.ID, &Step{StepType: StepTypeTool, Status: "COMPLETED"}); err != nil {
		t.Fatalf("RecordStep(tool B) returned error: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := svc.RecordStep(context.Background(), "org-other", foreign.ID, &Step{StepType: StepTypeTool, Status: "COMPLETED"}); err != nil {
			t.Fatalf("RecordStep(foreign %d) returned error: %v", i, err)
		}
	}
	return svc, org
}

func TestAggregateStepCountsMemory(t *testing.T) {
	svc, org := meterTestService(t)
	ctx := context.Background()

	from := time.Now().UTC().Add(-time.Hour)
	to := time.Now().UTC().Add(time.Hour)

	count, err := svc.AggregateStepCountsCtx(ctx, org, StepTypeTool, from, to)
	if err != nil {
		t.Fatalf("AggregateStepCountsCtx returned error: %v", err)
	}
	if count != 4 {
		t.Fatalf("tool steps: expected 4 (3+1, foreign excluded), got %d", count)
	}

	// Step-type filter: model steps are not tool calls.
	count, err = svc.AggregateStepCountsCtx(ctx, org, StepTypeModel, from, to)
	if err != nil {
		t.Fatalf("AggregateStepCountsCtx(model) returned error: %v", err)
	}
	if count != 1 {
		t.Fatalf("model steps: expected 1, got %d", count)
	}

	// Unknown step type: an honest 0 (no rows match), not an error.
	count, err = svc.AggregateStepCountsCtx(ctx, org, "sandbox", from, to)
	if err != nil || count != 0 {
		t.Fatalf("unknown type: expected 0/nil, got %d/%v", count, err)
	}
}

func TestAggregateStepCountsWindowBoundaries(t *testing.T) {
	svc := NewService()
	run, err := svc.CreateRunCtx(context.Background(), "org-w", "agent-1", "in")
	if err != nil {
		t.Fatalf("CreateRunCtx returned error: %v", err)
	}
	at := time.Date(2026, 2, 10, 12, 0, 0, 0, time.UTC)
	step := &Step{StepType: StepTypeTool, Status: "COMPLETED", CreatedAt: at}
	if err := svc.RecordStep(context.Background(), "org-w", run.ID, step); err != nil {
		t.Fatalf("RecordStep returned error: %v", err)
	}

	// Half-open [from, to): the boundary instant counts when it is the lower
	// bound, not the upper.
	cases := []struct {
		name     string
		from, to time.Time
		want     int64
	}{
		{"from == step.CreatedAt", at, at.Add(time.Hour), 1},
		{"to == step.CreatedAt", at.Add(-time.Hour), at, 0},
		{"window before", at.Add(-2 * time.Hour), at.Add(-time.Hour), 0},
		{"window after", at.Add(time.Hour), at.Add(2 * time.Hour), 0},
	}
	for _, tc := range cases {
		got, err := svc.AggregateStepCountsCtx(context.Background(), "org-w", StepTypeTool, tc.from, tc.to)
		if err != nil {
			t.Fatalf("%s: returned error: %v", tc.name, err)
		}
		if got != tc.want {
			t.Fatalf("%s: expected %d, got %d", tc.name, tc.want, got)
		}
	}
}

func TestAggregateStepCountsValidation(t *testing.T) {
	svc := NewService()
	from := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)

	if _, err := svc.AggregateStepCountsCtx(context.Background(), "", StepTypeTool, from, to); err == nil {
		t.Fatal("empty org id must fail")
	}
	if _, err := svc.AggregateStepCountsCtx(context.Background(), "org-1", "", from, to); err == nil {
		t.Fatal("empty step type must fail")
	}
	if _, err := svc.AggregateStepCountsCtx(context.Background(), "org-1", StepTypeTool, time.Time{}, to); err == nil {
		t.Fatal("zero from must fail")
	}
	if _, err := svc.AggregateStepCountsCtx(context.Background(), "org-1", StepTypeTool, to, from); err == nil {
		t.Fatal("inverted window must fail")
	}
}

func TestAggregateStepCountsStoreMode(t *testing.T) {
	// Store mode: the SQL is pinned (tenant guard + step type + half-open
	// window) via sqlmock, mirroring the AggregateCosts store tests.
	store, mock, closeDB := newMockStore(t)
	defer closeDB()
	svc := NewServiceWithStore(store)

	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	rows := sqlmock.NewRows([]string{"count"}).AddRow(42)
	// Tenant guard: WHERE r.organization_id = $1 AND rs.step_type = $2 AND window.
	mock.ExpectQuery(regexp.QuoteMeta(sqlCountStepsByType)).
		WithArgs("org-1", StepTypeTool, from, to).
		WillReturnRows(rows)

	count, err := svc.AggregateStepCountsCtx(context.Background(), "org-1", StepTypeTool, from, to)
	if err != nil {
		t.Fatalf("AggregateStepCountsCtx returned error: %v", err)
	}
	if count != 42 {
		t.Fatalf("expected 42, got %d", count)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestAggregateStepCountsStoreWithoutCapability(t *testing.T) {
	// A store that does not implement the optional capability must produce an
	// honest error, never a fabricated 0.
	svc := NewServiceWithStore(&capabilitylessStore{})
	if _, err := svc.AggregateStepCountsCtx(context.Background(), "org-1", StepTypeTool,
		time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("store without aggregation capability must fail loudly")
	}
}

// capabilitylessStore satisfies the full runs.Store interface with panics —
// only the type assertion is under test.
type capabilitylessStore struct{}

func (capabilitylessStore) CreateRun(context.Context, *Run) error { panic("unused") }
func (capabilitylessStore) GetRun(context.Context, string, string) (*Run, error) {
	panic("unused")
}
func (capabilitylessStore) GetRunByID(context.Context, string) (*Run, error) { panic("unused") }
func (capabilitylessStore) ListRuns(context.Context, string) ([]*Run, error) { panic("unused") }
func (capabilitylessStore) UpdateRunStatus(context.Context, string, string, RunStatus, string) error {
	panic("unused")
}
func (capabilitylessStore) InsertStep(context.Context, string, *Step) error { panic("unused") }
func (capabilitylessStore) ListSteps(context.Context, string, string) ([]*Step, error) {
	panic("unused")
}
func (capabilitylessStore) AggregateCosts(context.Context, string, time.Time, time.Time, CostGroupBy) ([]CostBucket, error) {
	panic("unused")
}
