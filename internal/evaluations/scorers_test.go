package evaluations

import "testing"

func ptrFloat(v float64) *float64 { return &v }

func TestScoreExact(t *testing.T) {
	c := Case{ID: "c1", Scorer: ScorerExact, Expected: "42"}
	if score, passed, err := c.Score("42", 0, 0); err != nil || !passed || score != 1.0 {
		t.Fatalf("exact match should pass: score=%v passed=%v err=%v", score, passed, err)
	}
	if score, passed, err := c.Score(" 42", 0, 0); err != nil || passed || score != 0.0 {
		t.Fatalf("exact match must be byte-for-byte (no trimming): score=%v passed=%v err=%v", score, passed, err)
	}
	if _, passed, _ := c.Score("", 0, 0); passed {
		t.Fatal("empty output should not match non-empty expected")
	}
}

func TestScoreContains(t *testing.T) {
	c := Case{ID: "c1", Scorer: ScorerContains, Expected: "total is 42"}
	if _, passed, err := c.Score("The total is 42 dollars.", 0, 0); err != nil || !passed {
		t.Fatalf("substring should pass: passed=%v err=%v", passed, err)
	}
	if _, passed, _ := c.Score("The total is 41 dollars.", 0, 0); passed {
		t.Fatal("missing substring should fail")
	}
	empty := Case{ID: "c2", Scorer: ScorerContains, Expected: ""}
	if _, passed, _ := empty.Score("anything", 0, 0); !passed {
		t.Fatal("empty expected substring is contained in any output")
	}
}

func TestScoreRegex(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		output  string
		want    bool
		wantErr bool
	}{
		{"anchored match", "^ok$", "ok", true, false},
		{"anchored no match", "^ok$", "not ok", false, false},
		{"search semantics", "ok", "lookup", true, false},
		{"case sensitive", "^OK$", "ok", false, false},
		{"empty output", "^ok$", "", false, false},
		{"empty pattern matches everything", "", "anything", true, false},
		{"alternation", "^(pass|ok)$", "pass", true, false},
		{"special chars", `^\d+\.\d{2}$`, "12.34", true, false},
		{"special chars no match", `^\d+\.\d{2}$`, "12,34", false, false},
		{"invalid pattern", "([unclosed", "ok", false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := Case{ID: "c1", Scorer: ScorerRegex, Params: Params{Pattern: tc.pattern}}
			_, passed, err := c.Score(tc.output, 0, 0)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for pattern %q", tc.pattern)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if passed != tc.want {
				t.Fatalf("pattern %q output %q: want passed=%v got %v", tc.pattern, tc.output, tc.want, passed)
			}
		})
	}
}

func TestScoreLatencyUnderMs(t *testing.T) {
	c := Case{ID: "c1", Scorer: ScorerLatencyUnderMs, Params: Params{ThresholdMS: ptrFloat(1500)}}
	if _, passed, err := c.Score("out", 1499.9, 0); err != nil || !passed {
		t.Fatalf("latency under threshold should pass: passed=%v err=%v", passed, err)
	}
	if _, passed, _ := c.Score("out", 1500, 0); !passed {
		t.Fatal("threshold is inclusive: latency == threshold should pass")
	}
	if _, passed, _ := c.Score("out", 1500.1, 0); passed {
		t.Fatal("latency above threshold should fail")
	}
	missing := Case{ID: "c2", Scorer: ScorerLatencyUnderMs}
	if _, _, err := missing.Score("out", 1, 0); err == nil {
		t.Fatal("missing threshold_ms should return a scorer error")
	}
}

func TestScoreCostUnderCents(t *testing.T) {
	c := Case{ID: "c1", Scorer: ScorerCostUnderCents, Params: Params{ThresholdCents: ptrFloat(10)}}
	if _, passed, err := c.Score("out", 0, 9.99); err != nil || !passed {
		t.Fatalf("cost under threshold should pass: passed=%v err=%v", passed, err)
	}
	if _, passed, _ := c.Score("out", 0, 10); !passed {
		t.Fatal("threshold is inclusive: cost == threshold should pass")
	}
	if _, passed, _ := c.Score("out", 0, 10.01); passed {
		t.Fatal("cost above threshold should fail")
	}
	missing := Case{ID: "c2", Scorer: ScorerCostUnderCents}
	if _, _, err := missing.Score("out", 0, 1); err == nil {
		t.Fatal("missing threshold_cents should return a scorer error")
	}
}

func TestScoreUnknownScorer(t *testing.T) {
	c := Case{ID: "c1", Scorer: Scorer("bogus")}
	if _, _, err := c.Score("out", 0, 0); err == nil {
		t.Fatal("unknown scorer should return an error")
	}
}

func TestParamsValidate(t *testing.T) {
	valid := []struct {
		scorer Scorer
		params Params
	}{
		{ScorerExact, Params{}},
		{ScorerContains, Params{}},
		{ScorerRegex, Params{Pattern: "^ok$"}},
		{ScorerLatencyUnderMs, Params{ThresholdMS: ptrFloat(1)}},
		{ScorerCostUnderCents, Params{ThresholdCents: ptrFloat(0.01)}},
	}
	for _, tc := range valid {
		if err := tc.params.Validate(tc.scorer); err != nil {
			t.Fatalf("params for %q should be valid: %v", tc.scorer, err)
		}
	}
	invalid := []struct {
		scorer Scorer
		params Params
	}{
		{Scorer("nope"), Params{}},
		{ScorerRegex, Params{}},
		{ScorerRegex, Params{Pattern: "("}},
		{ScorerLatencyUnderMs, Params{}},
		{ScorerLatencyUnderMs, Params{ThresholdMS: ptrFloat(0)}},
		{ScorerLatencyUnderMs, Params{ThresholdMS: ptrFloat(-5)}},
		{ScorerCostUnderCents, Params{}},
		{ScorerCostUnderCents, Params{ThresholdCents: ptrFloat(0)}},
	}
	for _, tc := range invalid {
		if err := tc.params.Validate(tc.scorer); err == nil {
			t.Fatalf("params for %q (%+v) should be invalid", tc.scorer, tc.params)
		}
	}
}
