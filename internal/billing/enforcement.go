package billing

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Enforcement flag (issue #47): AGENTOS_BILLING_ENFORCEMENT gates whether the
// platform DENIES work over quota (run creation + worker execution) instead of
// only displaying quota state. Default OFF — the flag is parsed with
// strconv.ParseBool so "1"/"t"/"T"/"true"/"TRUE"/"True" enable it and
// "0"/"f"/"F"/"false"/"FALSE"/"False" (or unset/empty) keep it off; any other
// value is treated as off (misconfiguration never silently enables billing
// enforcement).
//
// Both enforcement consumers (cmd/api create-run handler and the cmd/worker
// pre-execution gate) call EnforcementFromEnv so the two processes can never
// drift on the flag semantics. It is read at decision time (not process start)
// so tests can flip it with t.Setenv and operators can rotate it on restart.
const EnforcementEnvVar = "AGENTOS_BILLING_ENFORCEMENT"

// ReasonQuotaExceeded is the machine-readable denial reason shared by the
// enforcement surfaces: the API 402 error code, the worker's failed-run
// output/reason slot, and the audit metadata "reason" field.
const ReasonQuotaExceeded = "quota_exceeded"

// EnforcementFromEnv reports whether quota enforcement is enabled via
// EnforcementEnvVar (default OFF — see the comment above).
func EnforcementFromEnv() bool {
	raw, ok := os.LookupEnv(EnforcementEnvVar)
	if !ok || strings.TrimSpace(raw) == "" {
		return false
	}
	on, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	return on
}

// QuotaExceededMessage renders the operator-facing denial message for an
// exceeded quota. Deterministic and stable: the API 402 body and the worker
// audit metadata both carry it verbatim.
func QuotaExceededMessage(q *QuotaStatus) string {
	return fmt.Sprintf(
		"monthly run quota exceeded: included %d runs, consumed %d runs for the current billing period; upgrade the plan or wait for the period to reset",
		q.IncludedRuns, q.ConsumedRuns,
	)
}
