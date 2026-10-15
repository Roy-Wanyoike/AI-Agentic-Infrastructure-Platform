package synth

// types.go — issue #118 (flagship): shared contract for the Synthetic
// Assurance subsystem ("AI synthetic users & continuous product assurance").
//
// This file is the ENGINEERING-LEAD-PINNED type contract that every wave-10
// synthetic-assurance track builds against. It intentionally contains ONLY
// the vocabulary shared across tracks (registry, workers, journeys,
// evidence/findings, GitHub sync, release gates, exploration, production
// schedules). Track-specific behavior lives in the track-owned files:
//
//	#147 registry        -> service.go, store.go, http.go      (10-o)
//	#148 workers         -> worker.go, worker_api.go           (10-p)
//	#149 journeys        -> journeys.go, personas.go           (10-q)
//	#150 evidence        -> evidence.go, findings.go           (10-r)
//	#151 GitHub sync     -> github.go, github_sync.go          (10-s)
//	#155/#156 explore    -> explore.go, schedule.go, compare.go (10-v)
//	#152 quality gates   -> internal/qualitygates (separate package, 10-t)
//	#153 adversarial     -> internal/evaluations (additive, 10-u)
//
// Dual-mode house style: in-memory service by default; Postgres store via
// NewServiceWithStore when a db handle exists (migration 034 owns the
// registry tables; later tracks add their own tables with PRE-ASSIGNED
// numbers — do NOT create new migrations without orchestrator approval).
//
// All records are org-scoped (tenant isolation is non-negotiable) and every
// exported symbol here is part of the cross-track API surface — change with
// orchestrator approval only.

import (
	"errors"
	"time"
)

// Contract-wide errors (wrap with %w for context; never shadow).
var (
	ErrNotFound         = errors.New("synth: not found")
	ErrInvalid          = errors.New("synth: invalid")
	ErrTargetUnverified = errors.New("synth: target not verified")
	ErrScopeViolation   = errors.New("synth: outside authorized test scope")
	ErrReadOnlyMode     = errors.New("synth: write action denied in read-only mode")
	ErrBudgetExceeded   = errors.New("synth: run budget exceeded")
	ErrKilled           = errors.New("synth: killed by kill switch")
)

// EnvironmentKind enumerates where tests may run.
type EnvironmentKind string

const (
	EnvLocal      EnvironmentKind = "local"
	EnvDev        EnvironmentKind = "dev"
	EnvStaging    EnvironmentKind = "staging"
	EnvProduction EnvironmentKind = "production"
)

// EnvironmentMode pins the safety posture per environment.
type EnvironmentMode string

const (
	// ModePreProduction allows full write journeys (fixtures, cleanup).
	ModePreProduction EnvironmentMode = "pre_production"
	// ModeProductionReadOnly denies every state-changing action server-side.
	ModeProductionReadOnly EnvironmentMode = "production_read_only"
	// ModeReleaseComparison allows writes ONLY through isolated synthetic accounts.
	ModeReleaseComparison EnvironmentMode = "release_comparison"
)

// Project is the tenant-scoped container for synthetic testing.
type Project struct {
	ID        string
	OrgID     string
	Name      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Target is an application under test. Tests may only execute against
// VERIFIED targets (ownership challenge flow, issue #147).
type Target struct {
	ID          string
	OrgID       string
	ProjectID   string
	Name        string
	BaseURL     string
	Verified    bool
	VerifiedAt  *time.Time
	VerifyToken string // ownership challenge token (never exported via API)
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Environment binds a target to a runnable context with hard safety rails.
type Environment struct {
	ID             string
	OrgID          string
	TargetID       string
	Kind           EnvironmentKind
	Mode           EnvironmentMode
	AllowedDomains []string      // network policy: requests outside these are scope violations
	RateCapPerMin  int           // 0 = unlimited (pre-production only)
	BudgetPerRun   int           // max steps per run (0 = default 200)
	QuietWindows   []QuietWindow // blocked intervals
	TestWindows    []QuietWindow // when non-empty, runs allowed ONLY inside
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// QuietWindow is a timezone-aware interval policy on weekdays
// (0=Sunday .. 6=Saturday) between Start and End ("HH:MM", Location = IANA name).
type QuietWindow struct {
	Weekdays []int
	Start    string
	End      string
	Location string
	Reason   string
}

// Persona is a synthetic identity profile. Personas NEVER impersonate real
// customer accounts — they map to dedicated synthetic credentials by
// reference only (issue #147/#149).
type Persona struct {
	ID          string
	OrgID       string
	Name        string
	BuiltIn     string            // "" for custom; else a built-in key (visitor, new_user, returning_user, standard_customer, administrator, restricted_user, mobile_user, accessibility_user, slow_network_user)
	RoleHints   []string          // role/permission hints for journey planning
	Constraints map[string]string // e.g. network=slow3g, viewport=mobile, a11y=strict
	Version     int
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// JourneyStatus is the review lifecycle of a journey spec.
type JourneyStatus string

const (
	JourneyDraft     JourneyStatus = "draft"     // generated candidates start here
	JourneyReviewed  JourneyStatus = "reviewed"  // explicitly approved against an oracle
	JourneyScheduled JourneyStatus = "scheduled" // eligible for recurring runs
	JourneyRetired   JourneyStatus = "retired"
)

// RiskClass drives gate strictness (issue #149/#152).
type RiskClass string

const (
	RiskLow      RiskClass = "low"
	RiskMedium   RiskClass = "medium"
	RiskHigh     RiskClass = "high"
	RiskCritical RiskClass = "critical"
)

// Journey is a versioned test specification: explicit preconditions,
// steps, assertions, cleanup. Success criteria are ALWAYS explicit —
// model confidence can never mark a run passed (issue #149).
type Journey struct {
	ID            string
	OrgID         string
	ProjectID     string
	EnvironmentID string
	PersonaID     string
	Name          string
	Status        JourneyStatus
	Risk          RiskClass
	Version       int
	Preconditions []string
	Steps         []JourneyStep
	Assertions    []Assertion
	Cleanup       []JourneyStep
	Tags          []string
	Source        string // "authored" | "exploration" | "regression"
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// JourneyStep is one atomic action. Deterministic steps carry explicit
// selectors/inputs; AI-assisted navigation is a hint, never the oracle.
type JourneyStep struct {
	Index     int
	Action    string // navigate|click|fill|select|wait|api_call|assert|extract
	Target    string // selector, URL, or API spec reference
	Value     string
	WriteOp   bool   // true = state-changing; enforced against Environment.Mode
	TimeoutMS int    // 0 = worker default
	Hint      string // optional AI navigation hint
}

// AssertionKind enumerates deterministic outcome checks.
type AssertionKind string

const (
	AssertElementVisible AssertionKind = "element_visible"
	AssertTextContains   AssertionKind = "text_contains"
	AssertAPIStatus      AssertionKind = "api_status"
	AssertJSONSchema     AssertionKind = "json_schema"
	AssertURLPattern     AssertionKind = "url_pattern"
	AssertNoConsoleError AssertionKind = "no_console_error"
	AssertA11y           AssertionKind = "a11y_violations_below"
	AssertLatencyBelow   AssertionKind = "latency_below_ms"
)

// Assertion is a deterministic, versioned expected outcome.
type Assertion struct {
	Kind     AssertionKind
	Target   string
	Expected string
	Severity RiskClass
}

// RunStatus is the execution state machine for synthetic runs (#148).
type RunStatus string

const (
	RunPending    RunStatus = "pending"
	RunQueued     RunStatus = "queued"
	RunRunning    RunStatus = "running"
	RunPaused     RunStatus = "paused"
	RunCancelling RunStatus = "cancelling"
	RunCompleted  RunStatus = "completed"
	RunFailed     RunStatus = "failed"
	RunCancelled  RunStatus = "cancelled"
	// RunInconclusive: interrupted/lost worker — explicitly NOT pass/fail (#148).
	RunInconclusive RunStatus = "inconclusive"
)

// RunKind distinguishes product surfaces a run executes on (#148).
type RunKind string

const (
	KindBrowser RunKind = "browser" // isolated Playwright worker
	KindAPI     RunKind = "api"     // HTTP/API test worker
)

// Run is one execution of a journey. Evidence references point at redacted
// artifacts; raw secrets and PII never enter this record (#148/#150).
type Run struct {
	ID            string
	OrgID         string
	ProjectID     string
	JourneyID     string
	JourneyVer    int
	EnvironmentID string
	PersonaID     string
	PersonaVer    int
	Kind          RunKind
	Status        RunStatus
	AppVersion    string // build/release under test (release comparison)
	StartedAt     *time.Time
	FinishedAt    *time.Time
	StepEvents    []StepEvent
	EvidenceRefs  []string
	Error         string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// StepEvent is an ordered, correlated execution event with optional
// redacted request/response metadata, console/network capture refs (#148).
type StepEvent struct {
	Index        int
	StepIndex    int
	Type         string // action|assertion|console|network|error|navigation
	Status       string // passed|failed|skipped|blocked|inconclusive
	Message      string
	RedactedMeta map[string]string
	EvidenceRef  string
	At           time.Time
}

// FindingStatus is the versioned finding taxonomy (#150): a confirmed defect
// is independently reproduced; hypotheses are NEVER presented as verified
// root cause.
type FindingStatus string

const (
	FindingConfirmed     FindingStatus = "confirmed"
	FindingLikely        FindingStatus = "likely"
	FindingFlaky         FindingStatus = "flaky"
	FindingHarness       FindingStatus = "harness_failure"
	FindingEnvironmental FindingStatus = "environmental_failure"
	FindingInconclusive  FindingStatus = "inconclusive"
)

// Severity for findings and adversarial scenarios (#150/#153).
type Severity string

const (
	SevInfo     Severity = "info"
	SevLow      Severity = "low"
	SevMedium   Severity = "medium"
	SevHigh     Severity = "high"
	SevCritical Severity = "critical"
)

// Finding links the exact run/journey/assertion that produced it with a
// stable dedup fingerprint (#150). Occurrence history is append-only —
// dedup never merges across tenants and never discards occurrences.
type Finding struct {
	ID           string
	OrgID        string
	RunID        string
	JourneyID    string
	AssertionIx  int
	Status       FindingStatus
	Severity     Severity
	Title        string
	Description  string
	Fingerprint  string // stable hash: org+target+assertion+failure-class
	ReproSteps   string
	EvidenceRefs []string
	// Reproduction (independent re-run in isolation — never replays
	// production writes):
	ReproRunID   string
	Reproducible *bool
	Confidence   float64 // 0-1; uncertainty is explicit, never collapsed
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// GitHubRef tracks the external issue created from a finding (#151):
// idempotent creation via Fingerprint, safe updates, least-privilege.
type GitHubRef struct {
	ID             string
	OrgID          string
	FindingID      string
	Fingerprint    string
	Repo           string // allow-listed repository
	IssueNumber    int
	IssueURL       string
	IdempotencyKey string
	SyncedAt       time.Time
}

// ScopeDecision records a server-side scope/policy verdict for any
// navigation or action (#155): untrusted page content can never expand
// authorized scope.
type ScopeDecision struct {
	Allowed bool
	Reason  string // allowlisted_domain|write_denied_readonly|rate_capped|kill_switch|budget|...
	At      time.Time
}
