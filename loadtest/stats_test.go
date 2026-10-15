package loadtest

import (
	"math"
	"testing"
)

// TestPercentileKnownDistributions pins the nearest-rank percentile against
// known inputs (issue #125 requirement: p50/p95/p99 computation unit-tested).
func TestPercentileKnownDistributions(t *testing.T) {
	tests := []struct {
		name string
		in   []float64
		p50  float64
		p95  float64
		p99  float64
	}{
		{
			name: "1..100",
			in:   seq(1, 100),
			p50:  50, p95: 95, p99: 99,
		},
		{
			name: "1..10",
			in:   seq(1, 10),
			// ceil(5)-1=4 -> 5; ceil(9.5)-1=9 -> 10; ceil(9.9)-1=9 -> 10
			p50: 5, p95: 10, p99: 10,
		},
		{
			name: "four samples",
			in:   []float64{1, 2, 3, 4},
			// ceil(2)-1=1 -> 2; ceil(3.8)-1=3 -> 4; ceil(3.96)-1=3 -> 4
			p50: 2, p95: 4, p99: 4,
		},
		{
			name: "single sample",
			in:   []float64{42},
			p50:  42, p95: 42, p99: 42,
		},
		{
			name: "duplicates",
			in:   []float64{1, 1, 1, 1, 5},
			// n=5: p50 idx ceil(2.5)-1=2 -> 1; p95 idx ceil(4.75)-1=4 -> 5
			p50: 1, p95: 5, p99: 5,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Percentile(tc.in, 50); got != tc.p50 {
				t.Errorf("p50 = %v, want %v", got, tc.p50)
			}
			if got := Percentile(tc.in, 95); got != tc.p95 {
				t.Errorf("p95 = %v, want %v", got, tc.p95)
			}
			if got := Percentile(tc.in, 99); got != tc.p99 {
				t.Errorf("p99 = %v, want %v", got, tc.p99)
			}
		})
	}
}

func TestPercentileEdges(t *testing.T) {
	if got := Percentile(nil, 95); got != 0 {
		t.Errorf("empty slice: got %v, want 0", got)
	}
	xs := seq(1, 100)
	if got := Percentile(xs, 0); got != 1 {
		t.Errorf("p0 = %v, want first sample", got)
	}
	if got := Percentile(xs, 100); got != 100 {
		t.Errorf("p100 = %v, want last sample", got)
	}
	if got := Percentile(xs, -1); got != 1 {
		t.Errorf("negative p clamps to first sample, got %v", got)
	}
	// The input must not be mutated.
	if xs[0] != 1 || xs[99] != 100 {
		t.Fatal("Percentile mutated its input")
	}
}

// TestMeanAndSlope check the hand-rolled statistics used by reports and the
// soak trend lines.
func TestMeanAndSlope(t *testing.T) {
	if got := Mean([]float64{1, 2, 3, 4}); got != 2.5 {
		t.Errorf("mean = %v, want 2.5", got)
	}
	if got := Mean(nil); got != 0 {
		t.Errorf("mean of empty = %v, want 0", got)
	}
	// Perfect linear growth y = 2x + 1: slope must be 2.
	xs := seq(0, 9)
	ys := make([]float64, len(xs))
	for i := range xs {
		ys[i] = 2*xs[i] + 1
	}
	if got := LinearSlope(xs, ys); math.Abs(got-2) > 1e-9 {
		t.Errorf("slope = %v, want 2", got)
	}
	// Flat line: slope 0.
	if got := LinearSlope(xs, []float64{3, 3, 3, 3, 3, 3, 3, 3, 3, 3}); got != 0 {
		t.Errorf("flat slope = %v, want 0", got)
	}
	// Degenerate inputs.
	if got := LinearSlope([]float64{1}, []float64{1}); got != 0 {
		t.Errorf("single-sample slope = %v, want 0", got)
	}
	if got := LinearSlope([]float64{1, 2, 3}, []float64{1, 2}); got != 0 {
		t.Errorf("length-mismatch slope = %v, want 0", got)
	}
	if got := LinearSlope([]float64{5, 5, 5}, []float64{1, 2, 3}); got != 0 {
		t.Errorf("flat-x slope = %v, want 0", got)
	}
}

// TestCollectorWindowsAndMerge exercises the collector bookkeeping the spike
// (rolling windows, phase merge) and soak (half-window error rates) rely on.
func TestCollectorWindowsAndMerge(t *testing.T) {
	a := newCollector()
	b := newCollector()
	a.add(200, 200, 1)
	a.add(-1, 200, 2) // transport error
	b.add(200, 200, 3)

	reqs, errs, _, _ := a.totals()
	if reqs != 2 || errs != 1 {
		t.Fatalf("a totals = %d/%d, want 2/1", reqs, errs)
	}
	a.merge(b)
	reqs, errs, drops, _ := a.totals()
	if reqs != 3 || errs != 1 {
		t.Fatalf("merged totals = %d/%d, want 3/1", reqs, errs)
	}
	a.drop()
	_, _, drops, _ = a.totals()
	if drops != 1 {
		t.Fatalf("drops = %d, want 1", drops)
	}

	lat, ts := a.timeline()
	if len(lat) != 3 || len(ts) != 3 {
		t.Fatalf("timeline length = %d/%d, want 3/3", len(lat), len(ts))
	}
	// Window far in the future captures nothing; a wide window captures all.
	w := window(lat, ts, ts[len(ts)-1]+0.5, 100)
	if len(w) != 3 {
		t.Fatalf("wide window = %d samples, want 3", len(w))
	}
	w = window(lat, ts, ts[len(ts)-1], 0.0)
	if len(w) != 1 {
		t.Fatalf("zero window = %d samples, want 1", len(w))
	}

	// totalsWindow splits by timestamp bounds.
	from := ts[0]
	mid := ts[1]
	// First window: only the first sample (a success).
	reqs1, errs1 := a.totalsWindow(from-1, mid)
	if reqs1 != 1 || errs1 != 0 {
		t.Fatalf("first window = %d/%d, want 1/0", reqs1, errs1)
	}
	// Second window: the transport-error sample plus the merged one.
	reqs2, errs2 := a.totalsWindow(mid, ts[2]+1)
	if reqs2 != 2 || errs2 != 1 {
		t.Fatalf("second window = %d/%d, want 2/1", reqs2, errs2)
	}
}

func seq(from, to int) []float64 {
	out := make([]float64, 0, to-from+1)
	for i := from; i <= to; i++ {
		out = append(out, float64(i))
	}
	return out
}
