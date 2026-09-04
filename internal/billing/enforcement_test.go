package billing

import (
	"testing"
	"time"
)

// TestEnforcementFromEnv pins the AGENTOS_BILLING_ENFORCEMENT parsing contract
// shared by the create-run handler (cmd/api) and the worker pre-execution gate
// (cmd/worker): unset/empty -> OFF, strconv.ParseBool spellings honored, any
// other value -> OFF (misconfiguration never silently enables enforcement).
func TestEnforcementFromEnv(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		set  bool
		want bool
	}{
		{"unset defaults off", "", false, false},
		{"empty is off", "", true, false},
		{"true", "true", true, true},
		{"TRUE", "TRUE", true, true},
		{"one", "1", true, true},
		{"capital T", "T", true, true},
		{"false", "false", true, false},
		{"zero", "0", true, false},
		{"capital F", "F", true, false},
		{"garbage is off (never fail-open on typo)", "yes-please", true, false},
		{"whitespace around true", "  true  ", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv(EnforcementEnvVar, tc.raw)
			} else {
				t.Setenv(EnforcementEnvVar, "")
			}
			if got := EnforcementFromEnv(); got != tc.want {
				t.Fatalf("EnforcementFromEnv() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestQuotaExceededMessage pins the deterministic denial message shape so the
// API 402 body and worker audit metadata agree verbatim.
func TestQuotaExceededMessage(t *testing.T) {
	q := &QuotaStatus{
		OrganizationID: "org-1",
		IncludedRuns:   10,
		ConsumedRuns:   12,
		Exceeded:       true,
		PeriodStart:    time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		PeriodEnd:      time.Date(2025, 1, 31, 0, 0, 0, 0, time.UTC),
	}
	want := "monthly run quota exceeded: included 10 runs, consumed 12 runs for the current billing period; upgrade the plan or wait for the period to reset"
	if got := QuotaExceededMessage(q); got != want {
		t.Fatalf("QuotaExceededMessage() = %q, want %q", got, want)
	}
}
