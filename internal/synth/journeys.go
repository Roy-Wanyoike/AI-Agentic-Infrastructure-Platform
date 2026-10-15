package synth

// journeys.go — issue #149 (10-q): versioned journey specs, the enforced
// review lifecycle, deterministic outcome assertions, capability coverage
// mapping and reproducible run inputs.
//
// Core invariants (all enforced here, re-checked by the #148 worker):
//
//  1. EVERY journey carries at least one explicit Assertion. Success criteria
//     are always deterministic and versioned — model confidence can never
//     mark a run passed (ErrAssertionsRequired).
//  2. Lifecycle is draft → reviewed → scheduled → retired (CanTransitionJourney).
//     Drafts are the only editable state; reviewed/scheduled specs are
//     immutable except through CloneJourney (version-bump clone).
//  3. Write operations (JourneyStep.WriteOp) must be flagged and any write
//     journey must define Cleanup steps (ErrCleanupRequired). Runtime
//     enforcement against Environment.Mode is the worker's job (#148).
//  4. Scheduling requires the reviewed status; high/critical journeys
//     additionally require an auditable JourneyReview record for the current
//     version (ErrReviewRequired).
//
// Reproducibility: VersionedRunInputs produces a canonical byte snapshot of
// the exact inputs that determine a run's outcome (journey spec at its
// version + persona profile at its version + environment policy). Identical
// inputs ⇒ identical bytes, so evidence (#150) can pin what was tested.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Journey-specific sentinels (all wrap the contract's ErrInvalid).
var (
	// ErrJourneyImmutable: the journey's status forbids spec edits.
	ErrJourneyImmutable = fmt.Errorf("%w: journey spec is immutable in this status", ErrInvalid)
	// ErrAssertionsRequired: a journey without explicit assertions can
	// never be created, updated, reviewed or scheduled.
	ErrAssertionsRequired = fmt.Errorf("%w: journey requires at least one explicit assertion", ErrInvalid)
	// ErrCleanupRequired: write-op journeys must define cleanup steps.
	ErrCleanupRequired = fmt.Errorf("%w: write-op journey requires cleanup steps", ErrInvalid)
	// ErrReviewRequired: scheduling blocked until an auditable review
	// exists for the current journey version (high/critical gate).
	ErrReviewRequired = fmt.Errorf("%w: journey must be reviewed before scheduling", ErrInvalid)
	// ErrTransition: illegal status transition.
	ErrTransition = fmt.Errorf("%w: illegal journey status transition", ErrInvalid)
)

// Step action vocabulary (JourneyStep.Action). Anything else is rejected.
const (
	StepNavigate = "navigate"
	StepClick    = "click"
	StepFill     = "fill"
	StepSelect   = "select"
	StepWait     = "wait"
	StepAPICall  = "api_call"
	StepAssert   = "assert"
	StepExtract  = "extract"
)

// validStepActions is the closed action set from the pinned contract.
var validStepActions = map[string]bool{
	StepNavigate: true,
	StepClick:    true,
	StepFill:     true,
	StepSelect:   true,
	StepWait:     true,
	StepAPICall:  true,
	StepAssert:   true,
	StepExtract:  true,
}

// Journey source provenance (Journey.Source).
const (
	SourceAuthored    = "authored"
	SourceExploration = "exploration"
	SourceRegression  = "regression"
)

// IsValidStepAction reports whether action is in the closed step vocabulary.
func IsValidStepAction(action string) bool { return validStepActions[action] }

// IsValidRiskClass reports whether r is a valid RiskClass value.
func IsValidRiskClass(r RiskClass) bool {
	switch r {
	case RiskLow, RiskMedium, RiskHigh, RiskCritical:
		return true
	}
	return false
}

// IsValidJourneyStatus reports whether s is a valid JourneyStatus value.
func IsValidJourneyStatus(s JourneyStatus) bool {
	switch s {
	case JourneyDraft, JourneyReviewed, JourneyScheduled, JourneyRetired:
		return true
	}
	return false
}

// IsValidAssertionKind reports whether k is one of the deterministic
// assertion kinds from the pinned contract.
func IsValidAssertionKind(k AssertionKind) bool {
	switch k {
	case AssertElementVisible, AssertTextContains, AssertAPIStatus, AssertJSONSchema,
		AssertURLPattern, AssertNoConsoleError, AssertA11y, AssertLatencyBelow:
		return true
	}
	return false
}

// CanTransitionJourney enforces the lifecycle:
//
//	draft → reviewed → scheduled → retired
//	   └─────────────┬─────────────┘   (early retirement allowed)
//	             retired (terminal; nothing leaves retired)
//
// Drafts may be retired (discard a generated candidate), reviewed may be
// retired or scheduled, scheduled may only be retired. Self-transitions and
// backward transitions are illegal — mutation of a reviewed/scheduled spec
// goes through CloneJourney.
func CanTransitionJourney(from, to JourneyStatus) bool {
	switch from {
	case JourneyDraft:
		return to == JourneyReviewed || to == JourneyRetired
	case JourneyReviewed:
		return to == JourneyScheduled || to == JourneyRetired
	case JourneyScheduled:
		return to == JourneyRetired
	default: // retired or invalid
		return false
	}
}

// FlagWriteOps returns a copy of steps with WriteOp set to true for actions
// that inherently mutate application state (fill/select are form data entry;
// an api_call carrying a non-GET method hint in Target/Value is a write).
// click and api_call without a method hint stay author-judgment — the flag
// is a spec-level declaration the worker enforces against Environment.Mode.
func FlagWriteOps(steps []JourneyStep) []JourneyStep {
	out := make([]JourneyStep, len(steps))
	copy(out, steps)
	for i := range out {
		switch out[i].Action {
		case StepFill, StepSelect:
			out[i].WriteOp = true
		case StepAPICall:
			if methodIsWrite(extractHTTPMethod(out[i].Target)) || methodIsWrite(extractHTTPMethod(out[i].Value)) {
				out[i].WriteOp = true
			}
		}
	}
	return out
}

// extractHTTPMethod pulls a leading "METHOD " token out of an api_call
// target/value ("POST /v1/users" → "POST").
func extractHTTPMethod(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, " \t"); i > 0 {
		return strings.ToUpper(s[:i])
	}
	return ""
}

func methodIsWrite(method string) bool {
	switch method {
	case "POST", "PUT", "PATCH", "DELETE":
		return true
	}
	return false
}

// ValidateJourney enforces the full journey contract (see file header for
// invariants 1–3). It never mutates j. Steps must be indexed sequentially
// from 0 — deterministic ordering is part of the spec identity.
func ValidateJourney(j *Journey) error {
	if j == nil {
		return fmt.Errorf("%w: journey is nil", ErrInvalid)
	}
	if strings.TrimSpace(j.Name) == "" {
		return fmt.Errorf("%w: journey name is required", ErrInvalid)
	}
	if strings.TrimSpace(j.OrgID) == "" || strings.TrimSpace(j.ProjectID) == "" ||
		strings.TrimSpace(j.EnvironmentID) == "" || strings.TrimSpace(j.PersonaID) == "" {
		return fmt.Errorf("%w: journey org/project/environment/persona ids are required", ErrInvalid)
	}
	if !IsValidJourneyStatus(j.Status) {
		return fmt.Errorf("%w: unknown journey status %q", ErrInvalid, j.Status)
	}
	if !IsValidRiskClass(j.Risk) {
		return fmt.Errorf("%w: journey risk classification is required (low|medium|high|critical), got %q", ErrInvalid, j.Risk)
	}
	switch j.Source {
	case "", SourceAuthored, SourceExploration, SourceRegression:
	default:
		return fmt.Errorf("%w: unknown journey source %q", ErrInvalid, j.Source)
	}
	for _, pre := range j.Preconditions {
		if strings.TrimSpace(pre) == "" {
			return fmt.Errorf("%w: journey preconditions must be non-empty", ErrInvalid)
		}
	}
	if len(j.Steps) == 0 {
		return fmt.Errorf("%w: journey requires at least one step", ErrInvalid)
	}
	for i, step := range j.Steps {
		if step.Index != i {
			return fmt.Errorf("%w: step index %d out of sequence at position %d", ErrInvalid, step.Index, i)
		}
		if !IsValidStepAction(step.Action) {
			return fmt.Errorf("%w: step %d has invalid action %q (allowed: navigate|click|fill|select|wait|api_call|assert|extract)", ErrInvalid, i, step.Action)
		}
		if strings.TrimSpace(step.Target) == "" && step.Action != StepWait {
			return fmt.Errorf("%w: step %d (%s) requires a target", ErrInvalid, i, step.Action)
		}
	}
	hasWrite := false
	for _, step := range j.Steps {
		if step.WriteOp {
			hasWrite = true
			break
		}
	}
	for i, step := range j.Cleanup {
		if step.Index != i {
			return fmt.Errorf("%w: cleanup step index %d out of sequence at position %d", ErrInvalid, step.Index, i)
		}
		if !IsValidStepAction(step.Action) {
			return fmt.Errorf("%w: cleanup step %d has invalid action %q", ErrInvalid, i, step.Action)
		}
	}
	// Invariant 3: write journeys restore state explicitly.
	if hasWrite && len(j.Cleanup) == 0 {
		return ErrCleanupRequired
	}
	// Invariant 1: explicit success criteria are non-negotiable.
	if len(j.Assertions) == 0 {
		return ErrAssertionsRequired
	}
	for i, a := range j.Assertions {
		if err := validateAssertion(i, a); err != nil {
			return err
		}
	}
	return nil
}

func validateAssertion(ix int, a Assertion) error {
	if !IsValidAssertionKind(a.Kind) {
		return fmt.Errorf("%w: assertion %d has unknown kind %q", ErrInvalid, ix, a.Kind)
	}
	// Target-intrinsic kinds (page-wide checks) may omit Target.
	targetIntrinsic := a.Kind == AssertNoConsoleError || a.Kind == AssertA11y
	if !targetIntrinsic && strings.TrimSpace(a.Target) == "" {
		return fmt.Errorf("%w: assertion %d (%s) requires a target", ErrInvalid, ix, a.Kind)
	}
	// Expected outcome required where "empty" is meaningless.
	expectationRequired := a.Kind == AssertTextContains || a.Kind == AssertAPIStatus ||
		a.Kind == AssertJSONSchema || a.Kind == AssertURLPattern || a.Kind == AssertLatencyBelow ||
		a.Kind == AssertA11y
	if expectationRequired && strings.TrimSpace(a.Expected) == "" {
		return fmt.Errorf("%w: assertion %d (%s) requires an expected value", ErrInvalid, ix, a.Kind)
	}
	if !IsValidRiskClass(a.Severity) {
		return fmt.Errorf("%w: assertion %d requires a valid severity (low|medium|high|critical)", ErrInvalid, ix)
	}
	return nil
}

// JourneyReview is one auditable approval of a journey version. Reviews are
// recorded by the service (in memory for this track; the durable registry
// store follow-up persists them alongside synth_journeys).
type JourneyReview struct {
	JourneyID string
	Version   int
	Reviewer  string // non-empty; who approved the oracle
	At        time.Time
}

// JourneyStore is the narrow persistence contract for journeys (org-scoped
// like PersonaStore; Postgres impl is a registry follow-up over
// synth_journeys — migration 034 already owns the table).
type JourneyStore interface {
	PutJourney(ctx context.Context, j Journey) error
	GetJourney(ctx context.Context, orgID, id string) (Journey, error)
	ListJourneys(ctx context.Context, orgID string) ([]Journey, error)
	DeleteJourney(ctx context.Context, orgID, id string) error
}

// InMemoryJourneyStore is the default JourneyStore: mutex-guarded map with
// defensive copies both ways.
type InMemoryJourneyStore struct {
	mu    sync.RWMutex
	byID  map[string]Journey
	order []string
}

// NewInMemoryJourneyStore returns an empty ready-to-use store.
func NewInMemoryJourneyStore() *InMemoryJourneyStore {
	return &InMemoryJourneyStore{byID: make(map[string]Journey)}
}

func cloneJourney(j Journey) Journey {
	j.Preconditions = append([]string(nil), j.Preconditions...)
	j.Steps = append([]JourneyStep(nil), j.Steps...)
	j.Assertions = append([]Assertion(nil), j.Assertions...)
	j.Cleanup = append([]JourneyStep(nil), j.Cleanup...)
	j.Tags = append([]string(nil), j.Tags...)
	return j
}

// PutJourney upserts a journey (deep-copied); zero CreatedAt is stamped.
func (s *InMemoryJourneyStore) PutJourney(ctx context.Context, j Journey) error {
	now := time.Now()
	if j.CreatedAt.IsZero() {
		j.CreatedAt = now
	}
	j.UpdatedAt = now
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.byID[j.ID]; !exists {
		s.order = append(s.order, j.ID)
	}
	s.byID[j.ID] = cloneJourney(j)
	return nil
}

// GetJourney returns a deep copy; foreign-org and unknown ids are both ErrNotFound.
func (s *InMemoryJourneyStore) GetJourney(ctx context.Context, orgID, id string) (Journey, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j, ok := s.byID[id]
	if !ok || j.OrgID != orgID {
		return Journey{}, fmt.Errorf("%w: journey %s", ErrNotFound, id)
	}
	return cloneJourney(j), nil
}

// ListJourneys returns all journeys of one org in insertion order.
func (s *InMemoryJourneyStore) ListJourneys(ctx context.Context, orgID string) ([]Journey, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Journey, 0, len(s.order))
	for _, id := range s.order {
		if j, ok := s.byID[id]; ok && j.OrgID == orgID {
			out = append(out, cloneJourney(j))
		}
	}
	return out, nil
}

// DeleteJourney removes the journey; unknown or foreign ids yield ErrNotFound.
func (s *InMemoryJourneyStore) DeleteJourney(ctx context.Context, orgID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.byID[id]
	if !ok || j.OrgID != orgID {
		return fmt.Errorf("%w: journey %s", ErrNotFound, id)
	}
	delete(s.byID, id)
	for i, oid := range s.order {
		if oid == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	return nil
}

// JourneyService owns the journey lifecycle over a JourneyStore. It
// cross-checks persona existence through PersonaStore so a journey can never
// reference a persona outside its org. Clock and id generator are injectable
// for deterministic tests.
type JourneyService struct {
	store    JourneyStore
	personas PersonaStore
	now      func() time.Time
	newID    func() string

	mu      sync.Mutex
	reviews map[string][]JourneyReview // journeyID → review history (append-only)
}

// NewJourneyService wraps the stores.
func NewJourneyService(store JourneyStore, personas PersonaStore) *JourneyService {
	return &JourneyService{
		store:    store,
		personas: personas,
		now:      time.Now,
		newID:    func() string { return newSynthIDWithPrefix("jrn") },
		reviews:  make(map[string][]JourneyReview),
	}
}

// CreateJourney validates and persists a NEW journey. Status is forced to
// draft regardless of input (candidates enter the lifecycle at the bottom),
// Version is forced to 1. The referenced persona must exist in the same org.
func (s *JourneyService) CreateJourney(ctx context.Context, in Journey) (Journey, error) {
	in.ID = s.newID()
	in.Status = JourneyDraft
	in.Version = 1
	in.CreatedAt = s.now()
	in.UpdatedAt = in.CreatedAt
	if err := s.validatePersisted(ctx, &in); err != nil {
		return Journey{}, err
	}
	if err := s.store.PutJourney(ctx, cloneJourney(in)); err != nil {
		return Journey{}, fmt.Errorf("put journey: %w", err)
	}
	return cloneJourney(in), nil
}

// validatePersisted runs the spec validation plus registry cross-checks
// (persona existence and org consistency).
func (s *JourneyService) validatePersisted(ctx context.Context, j *Journey) error {
	if err := ValidateJourney(j); err != nil {
		return err
	}
	persona, err := s.personas.GetPersona(ctx, j.OrgID, j.PersonaID)
	if err != nil {
		return fmt.Errorf("journey persona %s: %w", j.PersonaID, err)
	}
	if persona.OrgID != j.OrgID { // defensive; store already scopes
		return fmt.Errorf("%w: persona %s belongs to another org", ErrInvalid, j.PersonaID)
	}
	return nil
}

// GetJourney fetches a deep copy.
func (s *JourneyService) GetJourney(ctx context.Context, orgID, id string) (Journey, error) {
	return s.store.GetJourney(ctx, orgID, id)
}

// ListJourneys returns all journeys of one org in insertion order.
func (s *JourneyService) ListJourneys(ctx context.Context, orgID string) ([]Journey, error) {
	return s.store.ListJourneys(ctx, orgID)
}

// UpdateJourney applies mutate to a DRAFT journey, re-validates (assertions,
// cleanup, actions, persona) and bumps the version. Reviewed/scheduled specs
// are immutable: use CloneJourney (version-bump clone) to evolve them.
func (s *JourneyService) UpdateJourney(ctx context.Context, orgID, id string, mutate func(*Journey)) (Journey, error) {
	if mutate == nil {
		return Journey{}, fmt.Errorf("%w: mutate function is required", ErrInvalid)
	}
	current, err := s.store.GetJourney(ctx, orgID, id)
	if err != nil {
		return Journey{}, err
	}
	if current.Status != JourneyDraft {
		return Journey{}, fmt.Errorf("%w: journey %s is %s (clone to evolve)", ErrJourneyImmutable, id, current.Status)
	}
	next := cloneJourney(current)
	mutate(&next)
	next.Status = JourneyDraft // edits never smuggle a status change
	if err := s.validatePersisted(ctx, &next); err != nil {
		return Journey{}, err
	}
	next.Version = current.Version + 1
	next.CreatedAt = current.CreatedAt
	next.UpdatedAt = s.now()
	if err := s.store.PutJourney(ctx, next); err != nil {
		return Journey{}, fmt.Errorf("put journey: %w", err)
	}
	return cloneJourney(next), nil
}

// ReviewJourney moves a draft to reviewed, recording an auditable review for
// the exact version. The reviewer identity is mandatory — an approval
// without a who is worthless. High/critical scheduling depends on this
// record (ScheduleJourney).
func (s *JourneyService) ReviewJourney(ctx context.Context, orgID, id, reviewer string) (Journey, error) {
	if strings.TrimSpace(reviewer) == "" {
		return Journey{}, fmt.Errorf("%w: reviewer identity is required", ErrInvalid)
	}
	current, err := s.store.GetJourney(ctx, orgID, id)
	if err != nil {
		return Journey{}, err
	}
	if !CanTransitionJourney(current.Status, JourneyReviewed) {
		return Journey{}, fmt.Errorf("%w: %s → reviewed for journey %s", ErrTransition, current.Status, id)
	}
	if err := s.validatePersisted(ctx, &current); err != nil {
		return Journey{}, fmt.Errorf("journey no longer valid: %w", err)
	}
	current.Status = JourneyReviewed
	current.UpdatedAt = s.now()
	if err := s.store.PutJourney(ctx, current); err != nil {
		return Journey{}, fmt.Errorf("put journey: %w", err)
	}
	s.mu.Lock()
	s.reviews[id] = append(s.reviews[id], JourneyReview{
		JourneyID: id,
		Version:   current.Version,
		Reviewer:  reviewer,
		At:        current.UpdatedAt,
	})
	s.mu.Unlock()
	return cloneJourney(current), nil
}

// Reviews returns a copy of the review history for a journey (oldest first).
func (s *JourneyService) Reviews(journeyID string) []JourneyReview {
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.reviews[journeyID]
	out := make([]JourneyReview, len(src))
	copy(out, src)
	return out
}

// ScheduleJourney moves a reviewed journey to scheduled. Beyond the status
// chain, the spec is re-validated and — for high/critical risk — an auditable
// review record MUST exist for the current version (ErrReviewRequired). This
// is the "high/critical journeys require review before schedule" gate; the
// reviewed status is itself the gate for every risk class.
func (s *JourneyService) ScheduleJourney(ctx context.Context, orgID, id string) (Journey, error) {
	current, err := s.store.GetJourney(ctx, orgID, id)
	if err != nil {
		return Journey{}, err
	}
	if !CanTransitionJourney(current.Status, JourneyScheduled) {
		return Journey{}, fmt.Errorf("%w: %s → scheduled for journey %s", ErrTransition, current.Status, id)
	}
	if err := s.validatePersisted(ctx, &current); err != nil {
		return Journey{}, fmt.Errorf("journey no longer valid: %w", err)
	}
	if (current.Risk == RiskHigh || current.Risk == RiskCritical) && !s.hasReviewForVersion(id, current.Version) {
		return Journey{}, fmt.Errorf("%w: journey %s v%d (%s risk) has no review record",
			ErrReviewRequired, id, current.Version, current.Risk)
	}
	current.Status = JourneyScheduled
	current.UpdatedAt = s.now()
	if err := s.store.PutJourney(ctx, current); err != nil {
		return Journey{}, fmt.Errorf("put journey: %w", err)
	}
	return cloneJourney(current), nil
}

func (s *JourneyService) hasReviewForVersion(journeyID string, version int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.reviews[journeyID] {
		if r.Version == version {
			return true
		}
	}
	return false
}

// RetireJourney moves a draft/reviewed/scheduled journey to retired
// (terminal). Retired specs are kept for evidence lineage but never run.
func (s *JourneyService) RetireJourney(ctx context.Context, orgID, id string) (Journey, error) {
	current, err := s.store.GetJourney(ctx, orgID, id)
	if err != nil {
		return Journey{}, err
	}
	if !CanTransitionJourney(current.Status, JourneyRetired) {
		return Journey{}, fmt.Errorf("%w: %s → retired for journey %s", ErrTransition, current.Status, id)
	}
	current.Status = JourneyRetired
	current.UpdatedAt = s.now()
	if err := s.store.PutJourney(ctx, current); err != nil {
		return Journey{}, fmt.Errorf("put journey: %w", err)
	}
	return cloneJourney(current), nil
}

// CloneJourney performs the version-bump clone: the ONLY way to evolve a
// reviewed or scheduled spec. The source is never touched; the clone is a
// fresh draft with Version = source.Version + 1 (lineage stays ordered) and
// a new id. Content is deep-copied — later edits to either side cannot leak.
func (s *JourneyService) CloneJourney(ctx context.Context, orgID, id string) (Journey, error) {
	src, err := s.store.GetJourney(ctx, orgID, id)
	if err != nil {
		return Journey{}, err
	}
	clone := cloneJourney(src)
	clone.ID = s.newID()
	clone.Status = JourneyDraft
	clone.Version = src.Version + 1
	clone.CreatedAt = s.now()
	clone.UpdatedAt = clone.CreatedAt
	if err := s.validatePersisted(ctx, &clone); err != nil {
		return Journey{}, fmt.Errorf("clone validation: %w", err)
	}
	if err := s.store.PutJourney(ctx, clone); err != nil {
		return Journey{}, fmt.Errorf("put journey: %w", err)
	}
	return cloneJourney(clone), nil
}

// DeleteJourney removes a journey from the registry. Only drafts and retired
// specs may be deleted — reviewed/scheduled specs carry review/run lineage.
func (s *JourneyService) DeleteJourney(ctx context.Context, orgID, id string) error {
	current, err := s.store.GetJourney(ctx, orgID, id)
	if err != nil {
		return err
	}
	if current.Status == JourneyReviewed || current.Status == JourneyScheduled {
		return fmt.Errorf("%w: %s journey %s must be retired before deletion", ErrInvalid, current.Status, id)
	}
	return s.store.DeleteJourney(ctx, orgID, id)
}

// ---------------------------------------------------------------------------
// Coverage mapping
// ---------------------------------------------------------------------------

// CapabilityCoverage aggregates the journeys covering one product capability
// tag. Tags are normalized (trimmed, lowercased) so "Billing" and "billing"
// report as one capability.
type CapabilityCoverage struct {
	Capability   string
	JourneyIDs   []string // sorted, deduplicated
	StatusCounts map[JourneyStatus]int
}

// JourneyCoverage is the capability coverage report for a set of journeys.
type JourneyCoverage struct {
	TotalJourneys    int
	UntaggedJourneys []string             // sorted journey ids with no usable tags
	Capabilities     []CapabilityCoverage // sorted by Capability
}

// MapCapabilities builds the coverage report: journeys → product capability
// tags (from Journey.Tags). A journey counts toward EVERY tag it carries.
// Output ordering is fully deterministic (sorted capabilities, sorted ids).
func MapCapabilities(journeys []Journey) JourneyCoverage {
	report := JourneyCoverage{TotalJourneys: len(journeys)}
	byCapability := make(map[string]map[string]JourneyStatus)
	untagged := make(map[string]bool)
	for _, j := range journeys {
		tagged := false
		for _, raw := range j.Tags {
			cap := strings.ToLower(strings.TrimSpace(raw))
			if cap == "" {
				continue
			}
			tagged = true
			if byCapability[cap] == nil {
				byCapability[cap] = make(map[string]JourneyStatus)
			}
			byCapability[cap][j.ID] = j.Status
		}
		if !tagged {
			untagged[j.ID] = true
		}
	}
	for cap, ids := range byCapability {
		cc := CapabilityCoverage{Capability: cap, JourneyIDs: make([]string, 0, len(ids)), StatusCounts: make(map[JourneyStatus]int)}
		for id, status := range ids {
			cc.JourneyIDs = append(cc.JourneyIDs, id)
			cc.StatusCounts[status]++
		}
		sort.Strings(cc.JourneyIDs)
		report.Capabilities = append(report.Capabilities, cc)
	}
	sort.Slice(report.Capabilities, func(i, k int) bool {
		return report.Capabilities[i].Capability < report.Capabilities[k].Capability
	})
	for id := range untagged {
		report.UntaggedJourneys = append(report.UntaggedJourneys, id)
	}
	sort.Strings(report.UntaggedJourneys)
	return report
}

// Gaps returns the wanted capabilities (normalized) with zero coverage,
// sorted. This is the release-gate primitive (#152): untested capabilities
// block, untagged journeys do not silently count.
func (c JourneyCoverage) Gaps(want []string) []string {
	have := make(map[string]bool, len(c.Capabilities))
	for _, cc := range c.Capabilities {
		have[cc.Capability] = true
	}
	var gaps []string
	seen := make(map[string]bool, len(want))
	for _, w := range want {
		cap := strings.ToLower(strings.TrimSpace(w))
		if cap == "" || seen[cap] || have[cap] {
			continue
		}
		seen[cap] = true
		gaps = append(gaps, cap)
	}
	sort.Strings(gaps)
	return gaps
}

// CoverageFor returns the coverage of one capability (normalized lookup).
func (c JourneyCoverage) CoverageFor(capability string) (CapabilityCoverage, bool) {
	cap := strings.ToLower(strings.TrimSpace(capability))
	for _, cc := range c.Capabilities {
		if cc.Capability == cap {
			return cc, true
		}
	}
	return CapabilityCoverage{}, false
}

// ---------------------------------------------------------------------------
// Reproducibility: canonical run input snapshots
// ---------------------------------------------------------------------------

// RunInputsSchemaV1 identifies the canonical serialization produced by
// VersionedRunInputs. Bump only with orchestrator approval (evidence track
// #150 pins snapshots against it).
const RunInputsSchemaV1 = "agentos.synth.run_inputs.v1"

type runInputsSnapshot struct {
	Schema      string              `json:"schema"`
	Journey     journeySnapshot     `json:"journey"`
	Persona     personaSnapshot     `json:"persona"`
	Environment environmentSnapshot `json:"environment"`
}

type journeySnapshot struct {
	ID            string            `json:"id"`
	Version       int               `json:"version"`
	Name          string            `json:"name"`
	Status        JourneyStatus     `json:"status"`
	Risk          RiskClass         `json:"risk"`
	Source        string            `json:"source"`
	Preconditions []string          `json:"preconditions"`
	Steps         []journeyStepSnap `json:"steps"`
	Assertions    []assertionSnap   `json:"assertions"`
	Cleanup       []journeyStepSnap `json:"cleanup"`
	Tags          []string          `json:"tags"`
}

type journeyStepSnap struct {
	Index     int    `json:"index"`
	Action    string `json:"action"`
	Target    string `json:"target"`
	Value     string `json:"value,omitempty"`
	WriteOp   bool   `json:"write_op"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`
	Hint      string `json:"hint,omitempty"`
}

type assertionSnap struct {
	Kind     AssertionKind `json:"kind"`
	Target   string        `json:"target"`
	Expected string        `json:"expected"`
	Severity RiskClass     `json:"severity"`
}

type personaSnapshot struct {
	ID          string         `json:"id"`
	Version     int            `json:"version"`
	BuiltIn     string         `json:"built_in"`
	RoleHints   []string       `json:"role_hints"`
	Constraints []constraintKV `json:"constraints"`
}

type constraintKV struct {
	K string `json:"k"`
	V string `json:"v"`
}

type environmentSnapshot struct {
	ID             string        `json:"id"`
	Kind           string        `json:"kind"`
	Mode           string        `json:"mode"`
	AllowedDomains []string      `json:"allowed_domains"`
	RateCapPerMin  int           `json:"rate_cap_per_min"`
	BudgetPerRun   int           `json:"budget_per_run"`
	QuietWindows   []QuietWindow `json:"quiet_windows"`
	TestWindows    []QuietWindow `json:"test_windows"`
}

// VersionedRunInputs returns a deterministic, canonical byte serialization of
// exactly the versioned inputs that determine a run's outcome: the journey
// spec at its version, the persona profile at its version, and the
// environment policy. Runners snapshot these bytes with each Run
// (Run.JourneyVer/PersonaVer) so evidence (#150) can prove WHAT was tested
// and release comparison (#155) can diff inputs across builds.
//
// Determinism contract:
//   - field order is fixed by the snapshot structs; map-shaped data
//     (persona constraints) is flattened to sorted key/value pairs;
//   - set-shaped data (allowed domains, tags, role hints) is sorted and
//     deduplicated; ordered collections (steps, assertions, windows) keep
//     their sequence — order is semantically meaningful there;
//   - timestamps (CreatedAt/UpdatedAt) are EXCLUDED on purpose: identity is
//     (id, version) + content, and version bumps on every mutation, so two
//     records at the same version must produce the same bytes regardless of
//     when they were written;
//   - fixture identity travels inside the journey spec: fixture references
//     are written as deterministic precondition strings (e.g.
//     "fixture:billing_users:v3") and materialized by the FixtureSet track
//     (fixtures.go), so the snapshot covers them without extra inputs.
//
// A nil journey/persona/environment yields nil bytes (callers treat that as
// "no snapshot"); non-nil inputs are never mutated.
func VersionedRunInputs(j *Journey, p *Persona, env *Environment) []byte {
	if j == nil || p == nil || env == nil {
		return nil
	}
	snap := runInputsSnapshot{
		Schema:  RunInputsSchemaV1,
		Journey: buildJourneySnapshot(j),
		Persona: buildPersonaSnapshot(p),
		Environment: environmentSnapshot{
			ID:             env.ID,
			Kind:           string(env.Kind),
			Mode:           string(env.Mode),
			AllowedDomains: sortedUniqueStrings(env.AllowedDomains),
			RateCapPerMin:  env.RateCapPerMin,
			BudgetPerRun:   env.BudgetPerRun,
			QuietWindows:   append([]QuietWindow(nil), env.QuietWindows...),
			TestWindows:    append([]QuietWindow(nil), env.TestWindows...),
		},
	}
	b, err := json.Marshal(snap)
	if err != nil { // structs contain only JSON-encodable types; unreachable in practice
		panic("synth: run input snapshot encode failed: " + err.Error())
	}
	return b
}

func buildJourneySnapshot(j *Journey) journeySnapshot {
	steps := make([]journeyStepSnap, len(j.Steps))
	for i, s := range j.Steps {
		steps[i] = journeyStepSnap{
			Index: s.Index, Action: s.Action, Target: s.Target, Value: s.Value,
			WriteOp: s.WriteOp, TimeoutMS: s.TimeoutMS, Hint: s.Hint,
		}
	}
	cleanup := make([]journeyStepSnap, len(j.Cleanup))
	for i, s := range j.Cleanup {
		cleanup[i] = journeyStepSnap{
			Index: s.Index, Action: s.Action, Target: s.Target, Value: s.Value,
			WriteOp: s.WriteOp, TimeoutMS: s.TimeoutMS, Hint: s.Hint,
		}
	}
	assertions := make([]assertionSnap, len(j.Assertions))
	for i, a := range j.Assertions {
		assertions[i] = assertionSnap{Kind: a.Kind, Target: a.Target, Expected: a.Expected, Severity: a.Severity}
	}
	return journeySnapshot{
		ID:            j.ID,
		Version:       j.Version,
		Name:          j.Name,
		Status:        j.Status,
		Risk:          j.Risk,
		Source:        j.Source,
		Preconditions: sortedUniqueStrings(j.Preconditions),
		Steps:         steps,
		Assertions:    assertions,
		Cleanup:       cleanup,
		Tags:          sortedUniqueStrings(j.Tags),
	}
}

func buildPersonaSnapshot(p *Persona) personaSnapshot {
	hints := sortedUniqueStrings(p.RoleHints)
	constraints := make([]constraintKV, 0, len(p.Constraints))
	for _, k := range sortedKeys(p.Constraints) {
		constraints = append(constraints, constraintKV{K: k, V: p.Constraints[k]})
	}
	return personaSnapshot{
		ID:          p.ID,
		Version:     p.Version,
		BuiltIn:     p.BuiltIn,
		RoleHints:   hints,
		Constraints: constraints,
	}
}

// sortedUniqueStrings returns a sorted, deduplicated copy (empty entries
// dropped) — the canonical form for set-shaped string data.
func sortedUniqueStrings(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
