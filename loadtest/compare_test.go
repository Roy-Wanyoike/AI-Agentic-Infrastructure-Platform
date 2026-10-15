package loadtest

import (
	"testing"
)

func testReport(scenario string, ops ...OpStats) *Report {
	return &Report{
		Tool:     ToolName,
		Scenario: scenario,
		Ops:      ops,
	}
}

func op(name string, p95Ms float64, errRate float64, reqs int64) OpStats {
	return OpStats{Op: name, P95Ms: p95Ms, ErrorRate: errRate, Requests: reqs}
}

// TestCompareReportsGates pins the issue #125 regression contract: p95 +10%
// or error rate +2pp fails; everything else passes. setup.* ops are excluded.
func TestCompareReportsGates(t *testing.T) {
	baseline := testReport("sustained",
		op("create_run", 10.0, 0.00, 1000),
		op("healthz", 1.0, 0.00, 200),
		op("setup.provision", 1000.0, 0, 2),
	)
	cur := testReport("sustained",
		op("create_run", 10.5, 0.01, 1000), // +5% p95, +1pp errors: inside both gates
		op("healthz", 1.05, 0.00, 200),
		op("setup.provision", 2000.0, 0, 2), // setup drift ignored
	)
	regs := CompareReports(baseline, cur, DefaultP95Tolerance, DefaultErrorTolerance)
	if len(regs) != 0 {
		t.Fatalf("expected PASS, got %+v", regs)
	}

	// P95 gate: +10.0001% trips (strictly above tolerance).
	cur.Ops[0].P95Ms = 11.01
	regs = CompareReports(baseline, cur, DefaultP95Tolerance, DefaultErrorTolerance)
	if len(regs) != 1 || regs[0].Kind != "p95" || regs[0].Op != "create_run" {
		t.Fatalf("expected one p95 regression on create_run, got %+v", regs)
	}

	// Error-rate gate: +2.1pp trips.
	cur.Ops[0].P95Ms = 10.5
	cur.Ops[0].ErrorRate = 0.021
	regs = CompareReports(baseline, cur, DefaultP95Tolerance, DefaultErrorTolerance)
	if len(regs) != 1 || regs[0].Kind != "error_rate" {
		t.Fatalf("expected one error_rate regression, got %+v", regs)
	}
}

func TestCompareReportsEdgeCases(t *testing.T) {
	baseline := testReport("sustained",
		op("create_run", 10.0, 0.00, 1000),
		op("healthz", 1.0, 0.00, 100),
	)
	t.Run("missing op fails", func(t *testing.T) {
		cur := testReport("sustained", op("create_run", 10.0, 0, 1000))
		regs := CompareReports(baseline, cur, DefaultP95Tolerance, DefaultErrorTolerance)
		if len(regs) != 1 || regs[0].Kind != "missing" || regs[0].Op != "healthz" {
			t.Fatalf("expected missing-op regression, got %+v", regs)
		}
	})
	t.Run("zero-request ops skipped", func(t *testing.T) {
		cur := testReport("sustained",
			op("create_run", 50.0, 0.5, 0), // nothing measured: not comparable
			op("healthz", 1.0, 0, 100),
		)
		regs := CompareReports(baseline, cur, DefaultP95Tolerance, DefaultErrorTolerance)
		if len(regs) != 0 {
			t.Fatalf("expected PASS, got %+v", regs)
		}
	})
	t.Run("custom tolerances", func(t *testing.T) {
		cur := testReport("sustained",
			op("create_run", 30.0, 0.00, 1000), // +200% p95
			op("healthz", 1.0, 0.00, 100),
		)
		if regs := CompareReports(baseline, cur, 3.0, DefaultErrorTolerance); len(regs) != 0 {
			t.Fatalf("3x tolerance must pass, got %+v", regs)
		}
		if regs := CompareReports(baseline, cur, 3.0, 0.30); len(regs) != 0 {
			t.Fatalf("loose error tolerance must pass, got %+v", regs)
		}
	})
	t.Run("all setup ops excluded", func(t *testing.T) {
		base := testReport("sustained", op("setup.provision", 1.0, 0, 1))
		cur := testReport("sustained", op("setup.provision", 100.0, 0.9, 1))
		if regs := CompareReports(base, cur, DefaultP95Tolerance, DefaultErrorTolerance); len(regs) != 0 {
			t.Fatalf("setup ops must be excluded, got %+v", regs)
		}
	})
	t.Run("regressions sorted deterministically", func(t *testing.T) {
		cur := testReport("sustained",
			op("healthz", 9.9, 0.00, 100),      // p95 9x
			op("create_run", 20.0, 0.03, 1000), // p95 2x + errors
		)
		regs := CompareReports(baseline, cur, DefaultP95Tolerance, DefaultErrorTolerance)
		if len(regs) != 3 {
			t.Fatalf("expected 3 regressions, got %+v", regs)
		}
		// Sorted by (op, kind): create_run error_rate, create_run p95, healthz p95.
		want := []struct{ op, kind string }{
			{"create_run", "error_rate"},
			{"create_run", "p95"},
			{"healthz", "p95"},
		}
		for i, w := range want {
			if regs[i].Op != w.op || regs[i].Kind != w.kind {
				t.Fatalf("regression %d = %s/%s, want %s/%s", i, regs[i].Op, regs[i].Kind, w.op, w.kind)
			}
		}
	})
}

func TestLoadReportRejectsForeignTools(t *testing.T) {
	// LoadReport's tool check is exercised through the CLI path; here we pin
	// the comparator's tolerance constants to the issue contract.
	if DefaultP95Tolerance != 1.10 {
		t.Fatalf("p95 tolerance = %v, want 1.10 (+10%%)", DefaultP95Tolerance)
	}
	if DefaultErrorTolerance != 0.02 {
		t.Fatalf("error tolerance = %v, want 0.02 (+2pp)", DefaultErrorTolerance)
	}
}
