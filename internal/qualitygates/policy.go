// Package qualitygates implements the unified release assurance policy
// (issue #152): versioned, tenant-scoped gate policies that combine the
// platform's existing assurance evidence — evaluation runs, synthetic
// journeys, security/adversarial suites and canary health — into ONE
// deterministic promote/hold/abort/rollback recommendation.
//
// Design
// ------
//
//	GatePolicy     versioned rule set owned by a tenant (policy.go)
//	EvidenceRecord immutable, content-hashed suite evidence (evidence.go)
//	Decide         pure policy evaluation -> Decision (decide.go)
//	CanaryAdvisor  narrow bridge to the existing deployments machinery
//	               (canary_bridge.go — internal/deployments is NOT modified)
//
// The package EXTENDS the existing release machinery (internal/deployments
// canary + eval-gated promotion); it never replaces it. A Decision is a
// RECOMMENDATION: applying it to live traffic goes through the
// CanaryAdvisor bridge wired by the orchestrator.
//
// Safety posture (fail-closed by construction):
//
//   - Security-critical suites (Kind == security_adversarial with
//     FailClosed = true, enforced at validation) ignore aggregate score
//     balancing entirely: one hard finding above MaxSeverityAllowed fails
//     the check and forces an abort recommendation. They can never be
//     waived by exceptions.
//   - Missing required evidence blocks; stale evidence blocks (per-policy
//     MaxAge); inconclusive evidence blocks (explicitly NOT pass/fail).
//   - Exceptions are strictly time-bounded and apply ONLY to non-security
//     suites while unexpired.
//
// Storage: the in-memory policy/evidence stores are the default in BOTH
// API modes this round. The Postgres policy/evidence store is a documented
// follow-up (pre-assign the migration with the orchestrator before adding
// one — see docs rule in internal/synth/types.go).
package qualitygates

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SuiteKind enumerates the assurance suite families a gate policy may
// require. The values match the platform's existing evidence producers:
//
//	evaluations          internal/evaluations runs (datasets/scorers)
//	synthetic_journeys   internal/synth journey runs (issue #118/#149)
//	security_adversarial adversarial/security scenarios (issue #153 family)
//	canary_health        live canary health evidence (internal/deployments)
type SuiteKind string

const (
	SuiteEvaluations         SuiteKind = "evaluations"
	SuiteSyntheticJourneys   SuiteKind = "synthetic_journeys"
	SuiteSecurityAdversarial SuiteKind = "security_adversarial"
	SuiteCanaryHealth        SuiteKind = "canary_health"
)

// Valid reports whether k is one of the four suite families.
func (k SuiteKind) Valid() bool {
	switch k {
	case SuiteEvaluations, SuiteSyntheticJourneys, SuiteSecurityAdversarial, SuiteCanaryHealth:
		return true
	}
	return false
}

// Severity mirrors the platform-wide finding severity ladder (same values
// as internal/synth.Severity; declared locally so this package stays
// dependency-free on read-only internals). Rank orders severities so
// MaxSeverityAllowed comparisons are total.
type Severity string

const (
	SevInfo     Severity = "info"
	SevLow      Severity = "low"
	SevMedium   Severity = "medium"
	SevHigh     Severity = "high"
	SevCritical Severity = "critical"
)

// severityRank returns the total order position of s (info=0 .. critical=4)
// and ok=false for unknown severities.
func severityRank(s Severity) (int, bool) {
	switch s {
	case SevInfo:
		return 0, true
	case SevLow:
		return 1, true
	case SevMedium:
		return 2, true
	case SevHigh:
		return 3, true
	case SevCritical:
		return 4, true
	}
	return 0, false
}

// SuiteRequirement is one required assurance suite inside a GatePolicy: the
// threshold block a suite's evidence must satisfy for the release to pass.
// A value of 0 (or "") disables the corresponding threshold — EXCEPT on
// security_adversarial suites, where MaxSeverityAllowed is mandatory
// (fail-closed: the severity tolerance must be explicit).
type SuiteRequirement struct {
	// Kind selects the suite family. Exactly one requirement per kind is
	// allowed per policy (deterministic evidence matching).
	Kind SuiteKind `json:"kind"`
	// MinCoveragePct is the minimum suite coverage in percent [0,100].
	MinCoveragePct float64 `json:"min_coverage_pct"`
	// MinPassRate is the minimum case pass rate in [0,1].
	MinPassRate float64 `json:"min_pass_rate"`
	// MaxP95Ms is the maximum tolerated p95 latency in milliseconds.
	MaxP95Ms int64 `json:"max_p95_ms"`
	// MaxSeverityAllowed is the highest finding severity tolerated (a
	// finding ranked above it fails the check). Mandatory (non-empty) for
	// security_adversarial suites.
	MaxSeverityAllowed Severity `json:"max_severity_allowed"`
	// MaxAge is the evidence freshness window: evidence older than MaxAge
	// is STALE and blocks. 0 disables staleness (evidence never expires).
	MaxAge time.Duration `json:"max_age_ns"`
	// FailClosed marks the suite as security-critical: its checks ignore
	// aggregate score balancing AND exceptions — any failure aborts.
	// Mandatory true for security_adversarial suites (structural
	// enforcement, see Validate).
	FailClosed bool `json:"fail_closed"`
}

// Exception is a time-bounded waiver for ONE non-security suite family
// (or all of them when Suite is ""). Exceptions are recorded with an
// approver and an expiry; Decide honors them only while unexpired and
// NEVER for FailClosed suites.
type Exception struct {
	// Suite is the waived suite family; "" matches every non-security
	// suite. Security-critical suites are structurally unwaivable.
	Suite SuiteKind `json:"suite"`
	// Reason is the human-readable justification (required).
	Reason string `json:"reason"`
	// ApprovedBy is the authenticated actor who approved the waiver
	// (forced to the caller identity at the service layer — never trusted
	// from the request body).
	ApprovedBy string `json:"approved_by"`
	// ApprovedAt is stamped at approval time.
	ApprovedAt time.Time `json:"approved_at"`
	// ExpiresAt is the hard expiry (required non-zero: exceptions are
	// time-bounded only). Unexpired means ExpiresAt.After(now).
	ExpiresAt time.Time `json:"expires_at"`
}

// GatePolicy is the versioned, tenant-scoped release assurance rule set
// (issue #152). Policies are immutable versions: every mutation (update or
// exception approval) appends a NEW version — history is retained and any
// decision cites the exact Version + EvaluationVersion it ran against.
type GatePolicy struct {
	// ID is the policy identity (stable across versions, tenant-scoped).
	ID string `json:"id"`
	// OrgID scopes the policy to one tenant (never empty).
	OrgID string `json:"org_id"`
	// Name is the human-readable policy name.
	Name string `json:"name"`
	// Version is the monotonically increasing version counter (1-based).
	Version int `json:"version"`
	// RequiredSuites are the suites a release must satisfy. At least one
	// is required; one per Kind at most.
	RequiredSuites []SuiteRequirement `json:"required_suites"`
	// Exceptions are the time-bounded waivers attached to the policy.
	Exceptions []Exception `json:"exceptions"`
	// Active marks the policy eligible for evaluation. Inactive policies
	// are readable but the evaluate endpoint refuses them (422).
	Active bool `json:"active"`
	// CreatedBy records the actor that created version 1.
	CreatedBy string `json:"created_by"`
	// CreatedAt is the creation stamp of version 1 (preserved across
	// versions).
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt is the stamp of THIS version.
	UpdatedAt time.Time `json:"updated_at"`
}

// Package-wide errors. Callers wrap with %w and match with errors.Is.
var (
	// ErrInvalidPolicy is the validation failure root: every field-level
	// rejection wraps it with a specific message.
	ErrInvalidPolicy = errors.New("qualitygates: invalid policy")
	// ErrOrgRequired is returned when a tenant scope is missing.
	ErrOrgRequired = errors.New("qualitygates: organization required")
	// ErrPolicyNotFound is returned for unknown or foreign policies
	// (foreign IDs resolve to not-found: tenants can never probe each
	// other's policies).
	ErrPolicyNotFound = errors.New("qualitygates: policy not found")
	// ErrInvalidException is returned for malformed or already-expired
	// exception approvals (exceptions are time-bounded only).
	ErrInvalidException = errors.New("qualitygates: invalid exception")
	// ErrInactivePolicy is returned when evaluating a policy that is not
	// Active.
	ErrInactivePolicy = errors.New("qualitygates: policy is not active")
)

// Validate rejects out-of-domain policy values. Invalid input is REJECTED,
// never silently clamped (house contract, mirrors internal/deployments).
//
// Rules:
//
//   - name required (trimmed); at least one required suite; at most one
//     requirement per Kind.
//   - thresholds in range: MinCoveragePct [0,100], MinPassRate [0,1],
//     MaxP95Ms >= 0, MaxAge >= 0.
//   - MaxSeverityAllowed, when set, must be a known severity.
//   - security_adversarial requirements MUST carry FailClosed = true AND an
//     explicit MaxSeverityAllowed (structural enforcement: security
//     suites are fail-closed and unwaivable; an unspecified tolerance is
//     ambiguous and therefore rejected).
//   - exceptions must be time-bounded (non-zero ExpiresAt) with reason and
//     approver; Suite must be a valid kind or "".
func (p *GatePolicy) Validate() error {
	if p == nil {
		return fmt.Errorf("%w: nil policy", ErrInvalidPolicy)
	}
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("%w: name is required", ErrInvalidPolicy)
	}
	if len(p.RequiredSuites) == 0 {
		return fmt.Errorf("%w: at least one required suite", ErrInvalidPolicy)
	}
	seen := make(map[SuiteKind]bool, len(p.RequiredSuites))
	for i, req := range p.RequiredSuites {
		if !req.Kind.Valid() {
			return fmt.Errorf("%w: required_suites[%d]: unknown kind %q", ErrInvalidPolicy, i, req.Kind)
		}
		if seen[req.Kind] {
			return fmt.Errorf("%w: required_suites[%d]: duplicate kind %q (one requirement per kind)", ErrInvalidPolicy, i, req.Kind)
		}
		seen[req.Kind] = true
		if req.MinCoveragePct < 0 || req.MinCoveragePct > 100 {
			return fmt.Errorf("%w: required_suites[%d]: min_coverage_pct must be within [0,100]", ErrInvalidPolicy, i)
		}
		if req.MinPassRate < 0 || req.MinPassRate > 1 {
			return fmt.Errorf("%w: required_suites[%d]: min_pass_rate must be within [0,1]", ErrInvalidPolicy, i)
		}
		if req.MaxP95Ms < 0 {
			return fmt.Errorf("%w: required_suites[%d]: max_p95_ms must be >= 0", ErrInvalidPolicy, i)
		}
		if req.MaxAge < 0 {
			return fmt.Errorf("%w: required_suites[%d]: max_age must be >= 0", ErrInvalidPolicy, i)
		}
		if req.MaxSeverityAllowed != "" {
			if _, ok := severityRank(req.MaxSeverityAllowed); !ok {
				return fmt.Errorf("%w: required_suites[%d]: unknown max_severity_allowed %q", ErrInvalidPolicy, i, req.MaxSeverityAllowed)
			}
		}
		// Structural enforcement (issue #152): security-critical suites are
		// fail-closed and unwaivable. Validate refuses the softened shape so
		// no policy can even be STORED with a waivable security suite.
		if req.Kind == SuiteSecurityAdversarial {
			if !req.FailClosed {
				return fmt.Errorf("%w: required_suites[%d]: security_adversarial suites must be fail_closed (never waivable, ignores score balancing)", ErrInvalidPolicy, i)
			}
			if req.MaxSeverityAllowed == "" {
				return fmt.Errorf("%w: required_suites[%d]: security_adversarial suites must declare max_severity_allowed explicitly (fail-closed)", ErrInvalidPolicy, i)
			}
		}
	}
	for i, ex := range p.Exceptions {
		if err := validateExceptionShape(ex); err != nil {
			return fmt.Errorf("%w: exceptions[%d]: %v", ErrInvalidPolicy, i, err)
		}
	}
	return nil
}

// validateExceptionShape enforces the time-bounded-only contract for one
// exception.
func validateExceptionShape(ex Exception) error {
	if strings.TrimSpace(ex.Reason) == "" {
		return fmt.Errorf("%w: reason is required", ErrInvalidException)
	}
	if strings.TrimSpace(ex.ApprovedBy) == "" {
		return fmt.Errorf("%w: approved_by is required", ErrInvalidException)
	}
	if ex.ExpiresAt.IsZero() {
		return fmt.Errorf("%w: expires_at is required (exceptions are time-bounded only)", ErrInvalidException)
	}
	if ex.Suite != "" && !ex.Suite.Valid() {
		return fmt.Errorf("%w: unknown suite kind %q", ErrInvalidException, ex.Suite)
	}
	return nil
}

// EvaluationVersion returns a deterministic content hash (hex sha256) of
// the EXACT policy content: identity, version, canonicalized requirements
// (sorted by kind, shortest-repr float formatting) and canonicalized
// exceptions (sorted by suite/reason/approver/expiry). The same policy
// content always yields the same version string — decisions can pin it as
// evidence of the rule set they were made against. The hash is
// content-addressed: editing any field (including approval metadata)
// changes it.
func (p *GatePolicy) EvaluationVersion() string {
	if p == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("qualitygates/policy/v1|")
	b.WriteString(p.ID)
	b.WriteString("|v")
	b.WriteString(strconv.Itoa(p.Version))
	b.WriteString("|active=")
	b.WriteString(strconv.FormatBool(p.Active))

	reqs := make([]SuiteRequirement, len(p.RequiredSuites))
	copy(reqs, p.RequiredSuites)
	sort.SliceStable(reqs, func(i, j int) bool { return reqs[i].Kind < reqs[j].Kind })
	for _, r := range reqs {
		b.WriteString("|suite=")
		b.WriteString(string(r.Kind))
		b.WriteString(",cov=")
		b.WriteString(strconv.FormatFloat(r.MinCoveragePct, 'f', -1, 64))
		b.WriteString(",pass=")
		b.WriteString(strconv.FormatFloat(r.MinPassRate, 'f', -1, 64))
		b.WriteString(",p95=")
		b.WriteString(strconv.FormatInt(r.MaxP95Ms, 10))
		b.WriteString(",sev=")
		b.WriteString(string(r.MaxSeverityAllowed))
		b.WriteString(",age=")
		b.WriteString(strconv.FormatInt(int64(r.MaxAge), 10))
		b.WriteString(",fc=")
		b.WriteString(strconv.FormatBool(r.FailClosed))
	}

	exs := make([]Exception, len(p.Exceptions))
	copy(exs, p.Exceptions)
	sort.SliceStable(exs, func(i, j int) bool {
		if exs[i].Suite != exs[j].Suite {
			return exs[i].Suite < exs[j].Suite
		}
		if exs[i].Reason != exs[j].Reason {
			return exs[i].Reason < exs[j].Reason
		}
		if exs[i].ApprovedBy != exs[j].ApprovedBy {
			return exs[i].ApprovedBy < exs[j].ApprovedBy
		}
		return exs[i].ExpiresAt.Before(exs[j].ExpiresAt)
	})
	for _, e := range exs {
		b.WriteString("|ex=")
		b.WriteString(string(e.Suite))
		b.WriteString(",")
		b.WriteString(e.Reason)
		b.WriteString(",")
		b.WriteString(e.ApprovedBy)
		b.WriteString(",")
		b.WriteString(e.ExpiresAt.UTC().Format(time.RFC3339Nano))
	}

	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
