package synth

// findings.go — issue #150 (flagship #118, track 10-r): defect finding
// classification, dedup, independent reproduction and status transitions.
//
// This file OWNS:
//   - FailureClass + DetectFailureClass: DETERMINISTIC signal classification
//     (harness vs environmental vs timeout vs assertion). Heuristics are
//     marker-based and documented; they never consult a model.
//   - RootCauseClassifier: the OPTIONAL AI-assist seam. nil (the default)
//     means deterministic-only classification. Assist text is language-
//     enforced: it MUST carry the "hypothesis:" prefix marker or it is
//     discarded — hypotheses are never represented as verified root cause.
//   - Fingerprint: stable SHA-256 over org+target+assertion-kind+failure-
//     class via canonical JSON (documented, fixed field order). Dedup never
//     merges across orgs (the org is INSIDE the hash) and never discards
//     occurrence history (rows per (org, fingerprint, run)).
//   - FindingService: Classify (pure), Record (dedup + occurrence log),
//     Reproduce (isolated re-run via ReproRunner — never replays production
//     writes), Transition (validated, timestamped status changes),
//     Occurrences (append-only history).
//   - FindingStore interface + InMemoryFindingStore (row-per-occurrence
//     semantics, mirroring migration 034's ux_synth_findings_fp index).
//
// Statuses and severities come from the pinned vocabulary in types.go
// (FindingStatus, Severity). Classification precedence (documented):
//
//      1. independent reproduction (strongest evidence)  -> confirmed / flaky
//      2. harness signals (driver/worker)                -> harness_failure
//      3. environmental signals (infra)                  -> environmental_failure
//      4. failed assertion with consistent signals       -> likely
//      5. anything else                                  -> inconclusive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Failure classes and deterministic signal detection
// ---------------------------------------------------------------------------

// FailureClass is the deterministic failure bucket of a run. It is a
// component of the dedup fingerprint, so the vocabulary must stay stable.
type FailureClass string

const (
	// FailureAssertion: a deterministic assertion evaluated to a mismatch.
	FailureAssertion FailureClass = "assertion_failure"
	// FailureHarness: the test driver/worker itself errored — the app is
	// not implicated.
	FailureHarness FailureClass = "harness_failure"
	// FailureEnvironmental: infrastructure signals (DNS, connection
	// refused, 5xx gateway, TLS, rate limits) — not an app defect.
	FailureEnvironmental FailureClass = "environmental_failure"
	// FailureTimeout: a timeout with no harness/environmental marker — the
	// app failed to satisfy the expectation in time.
	FailureTimeout FailureClass = "timeout_failure"
	// FailureNone: no failure signal was detected.
	FailureNone FailureClass = "none"
)

// finding harness markers: errors that implicate the test driver/worker,
// never the application under test. Matched case-insensitively.
var harnessMarkers = []string{
	"worker", "driver", "playwright", "harness", "browser process",
	"cdp connection", "context disposed",
}

// environmental markers: infrastructure/infra-adjacent failure signals.
var environmentalMarkers = []string{
	"connection refused", "econnrefused", "dns", "no route to host",
	"network is unreachable", "connection reset", "502", "503", "504",
	"bad gateway", "service unavailable", "gateway timeout", "tls handshake",
	"certificate", "rate limit", "429",
}

// DetectFailureClass scans run.Error plus error/console/network step-event
// messages plus failed-assertion messages and returns ONE deterministic
// class. Precedence (documented, first match wins):
//
//	harness > environmental > timeout > assertion > none
//
// Harness wins over environmental because a dead worker commonly produces
// connection errors too; the worker failure is the actionable signal.
func DetectFailureClass(run Run, results []AssertionOutcome) FailureClass {
	var texts []string
	if run.Error != "" {
		texts = append(texts, run.Error)
	}
	for _, ev := range run.StepEvents {
		if ev.Type == "error" || ev.Type == "console" || ev.Type == "network" {
			texts = append(texts, ev.Message)
		}
	}
	for _, r := range results {
		if !r.Passed && r.Message != "" {
			texts = append(texts, r.Message)
		}
	}
	joined := strings.ToLower(strings.Join(texts, "\n"))

	containsAny := func(markers []string) bool {
		for _, m := range markers {
			if strings.Contains(joined, m) {
				return true
			}
		}
		return false
	}
	switch {
	case containsAny(harnessMarkers):
		return FailureHarness
	case containsAny(environmentalMarkers):
		return FailureEnvironmental
	case containsAny([]string{"timeout", "timed out"}):
		return FailureTimeout
	case anyFailed(results):
		return FailureAssertion
	default:
		return FailureNone
	}
}

func anyFailed(results []AssertionOutcome) bool {
	for i := range results {
		if !results[i].Passed {
			return true
		}
	}
	return false
}

// firstFailed returns the failed outcome with the lowest index, or nil.
func firstFailed(results []AssertionOutcome) *AssertionOutcome {
	var best *AssertionOutcome
	for i := range results {
		if results[i].Passed {
			continue
		}
		if best == nil || results[i].Index < best.Index {
			r := results[i]
			best = &r
		}
	}
	return best
}

// ---------------------------------------------------------------------------
// Fingerprint (canonical, org-scoped)
// ---------------------------------------------------------------------------

// fingerprintCanonical is the canonical JSON payload hashed into a finding
// fingerprint. Field order is FIXED (struct, not map) so encoding/json
// output is byte-stable across Go versions and processes; keys are already
// alphabetical by convention.
type fingerprintCanonical struct {
	AssertionKind string `json:"assertion_kind"`
	FailureClass  string `json:"failure_class"`
	Org           string `json:"org"`
	Target        string `json:"target"`
}

// FingerprintNoneKind is the assertion_kind used when a run produced no
// failing assertion (harness/environmental/inconclusive classes).
const FingerprintNoneKind = "none"

// Fingerprint returns the stable dedup key: SHA-256 over the canonical JSON
// of {org, target, assertion_kind, failure_class}. Because org is INSIDE the
// hash, identical findings in different orgs can never collide — dedup never
// merges across tenants by construction.
func Fingerprint(orgID, targetID string, kind AssertionKind, class FailureClass) string {
	k := string(kind)
	if k == "" {
		k = FingerprintNoneKind
	}
	payload, err := json.Marshal(fingerprintCanonical{
		AssertionKind: k,
		FailureClass:  string(class),
		Org:           orgID,
		Target:        targetID,
	})
	if err != nil {
		// Marshal of a fixed struct cannot fail; kept defensive.
		return ""
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// Optional AI-assist seam (hypothesis language enforced)
// ---------------------------------------------------------------------------

// HypothesisPrefix is the mandatory marker for AI-assisted root-cause text.
// Anything without it is discarded by the service — a hypothesis must never
// be represented as verified root cause (#150 contract).
const HypothesisPrefix = "hypothesis:"

// RootCauseClassifier is the OPTIONAL AI-assist seam for finding
// classification. The default (nil) keeps classification fully
// deterministic. Implementations suggest at most a hypothesis; they never
// decide status, severity or confidence.
type RootCauseClassifier interface {
	// SuggestRootCause inspects the evidence bundle and returns an optional
	// hypothesis. Returned text MUST start with "hypothesis:" (case-
	// insensitive) or the service discards it. ok=false means "no suggestion".
	SuggestRootCause(ctx context.Context, b EvidenceBundle) (hypothesis string, ok bool)
}

// enforceHypothesisLanguage validates AI-assist text against the #150
// language contract. It returns the normalized hypothesis and ok=true only
// when the text carries the mandatory "hypothesis:" prefix marker; any
// certainty-asserting text without the marker is rejected.
func enforceHypothesisLanguage(text string) (string, bool) {
	t := strings.TrimSpace(text)
	if t == "" {
		return "", false
	}
	if !strings.HasPrefix(strings.ToLower(t), HypothesisPrefix) {
		return "", false
	}
	return t, true
}

// ---------------------------------------------------------------------------
// Reproduction (isolated re-run)
// ---------------------------------------------------------------------------

// ReproRequest tells a ReproRunner what to re-run. The runner executes in an
// ISOLATED environment allocated for reproduction; production writes are
// never replayed.
type ReproRequest struct {
	OrgID          string
	FindingID      string
	OriginalRunID  string
	JourneyID      string
	AssertionIx    int
	EnvironmentID  string          // isolated environment (allocated by the runner)
	Mode           EnvironmentMode // pinned vocabulary
	ReadOnlySubset bool            // true => runner MUST execute only non-write steps
}

// ReproResult is the outcome of one independent reproduction attempt.
type ReproResult struct {
	ReproRunID     string // run id of the isolated re-run
	Reproduced     bool   // true => the failure independently reproduced
	At             time.Time
	EnvironmentID  string
	Mode           EnvironmentMode
	ReadOnlySubset bool
	Note           string // optional runner note (runner-owned, pre-redacted)
}

// ReproRunner is the narrow seam for independent reproduction. Implementations
// run against an ISOLATED environment and MUST NEVER replay production
// writes; in production_read_only mode they may only execute the read-only
// subset (see SupportsReadOnly).
type ReproRunner interface {
	// SupportsReadOnly reports whether this runner can execute the read-only
	// subset of a journey (navigation/assertions without state changes).
	// Runners that cannot guarantee it are refused up-front when the
	// isolation environment is production_read_only.
	SupportsReadOnly() bool
	// ReproRun executes the isolated reproduction for req and returns the
	// outcome. Errors mean the reproduction itself failed (harness-level);
	// the finding is left untouched.
	ReproRun(ctx context.Context, req ReproRequest) (ReproResult, error)
}

// ---------------------------------------------------------------------------
// Occurrence log
// ---------------------------------------------------------------------------

// Occurrence is one observed instance of a deduped finding (append-only
// history — never discarded, never merged across orgs).
type Occurrence struct {
	RunID string
	At    time.Time
}

// ---------------------------------------------------------------------------
// Store interface + in-memory implementation
// ---------------------------------------------------------------------------

// FindingFilter narrows List queries. Zero fields are ignored; Limit <= 0
// means unlimited.
type FindingFilter struct {
	Status      FindingStatus
	Fingerprint string
	JourneyID   string
	Limit       int
}

// FindingStore persists findings. Semantics are ROW-PER-OCCURRENCE: one row
// per (org, fingerprint, run) — exactly migration 034's
// ux_synth_findings_fp(org_id, fingerprint, run_id) unique index. The
// canonical finding for a fingerprint is its latest row; history is the
// ordered set of all rows.
type FindingStore interface {
	// Record upserts one occurrence row keyed by (org, fingerprint, run).
	Record(ctx context.Context, f Finding) error
	// Get returns one finding by id within org (ErrNotFound otherwise).
	Get(ctx context.Context, orgID, id string) (Finding, error)
	// GetByFingerprint returns the canonical (latest) finding for a
	// fingerprint within org (ErrNotFound when none).
	GetByFingerprint(ctx context.Context, orgID, fingerprint string) (Finding, error)
	// ListByFingerprint returns the full occurrence history for a
	// fingerprint within org, oldest first. Dedup never loses history.
	ListByFingerprint(ctx context.Context, orgID, fingerprint string) ([]Finding, error)
	// List returns findings within org matching filter, newest first.
	List(ctx context.Context, orgID string, filter FindingFilter) ([]Finding, error)
}

// InMemoryFindingStore is a mutex-guarded FindingStore with row-per-
// occurrence semantics (this track owns it). Primary key is (org, id); the
// service derives ids deterministically from (org, fingerprint, run).
type InMemoryFindingStore struct {
	mu   sync.Mutex
	rows map[string]Finding // key: orgID + "\x1f" + id
}

// NewInMemoryFindingStore returns an empty store.
func NewInMemoryFindingStore() *InMemoryFindingStore {
	return &InMemoryFindingStore{rows: map[string]Finding{}}
}

func fsKey(orgID, id string) string { return orgID + "\x1f" + id }

// Record upserts one occurrence row.
func (s *InMemoryFindingStore) Record(ctx context.Context, f Finding) error {
	if f.OrgID == "" || f.ID == "" {
		return fmt.Errorf("%w: finding requires org and id", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[fsKey(f.OrgID, f.ID)] = f
	return nil
}

// Get returns one finding by id within org.
func (s *InMemoryFindingStore) Get(ctx context.Context, orgID, id string) (Finding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.rows[fsKey(orgID, id)]
	if !ok {
		return Finding{}, fmt.Errorf("%w: finding %q", ErrNotFound, id)
	}
	return f, nil
}

// GetByFingerprint returns the canonical (latest by UpdatedAt, then RunID)
// finding row for the fingerprint within org.
func (s *InMemoryFindingStore) GetByFingerprint(ctx context.Context, orgID, fingerprint string) (Finding, error) {
	rows, err := s.ListByFingerprint(ctx, orgID, fingerprint)
	if err != nil {
		return Finding{}, err
	}
	if len(rows) == 0 {
		return Finding{}, fmt.Errorf("%w: fingerprint %s", ErrNotFound, fingerprint)
	}
	return rows[len(rows)-1], nil
}

// ListByFingerprint returns the occurrence history oldest-first
// (CreatedAt asc, RunID asc — deterministic).
func (s *InMemoryFindingStore) ListByFingerprint(ctx context.Context, orgID, fingerprint string) ([]Finding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Finding{}
	for _, f := range s.rows {
		if f.OrgID != orgID || f.Fingerprint != fingerprint {
			continue
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].RunID < out[j].RunID
	})
	return out, nil
}

// List returns findings within org matching filter, newest first
// (UpdatedAt desc, ID desc), honoring Limit.
func (s *InMemoryFindingStore) List(ctx context.Context, orgID string, filter FindingFilter) ([]Finding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Finding{}
	for _, f := range s.rows {
		if f.OrgID != orgID {
			continue
		}
		if filter.Status != "" && f.Status != filter.Status {
			continue
		}
		if filter.Fingerprint != "" && f.Fingerprint != filter.Fingerprint {
			continue
		}
		if filter.JourneyID != "" && f.JourneyID != filter.JourneyID {
			continue
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return out[i].ID > out[j].ID
	})
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out, nil
}

// Compile-time interface check.
var _ FindingStore = (*InMemoryFindingStore)(nil)

// ---------------------------------------------------------------------------
// Status transition rules (enforced)
// ---------------------------------------------------------------------------

// findingTransitions encodes the enforced status machine (#150):
//
//	confirmed          -> confirmed, flaky            (confirmed → flaky possible)
//	likely             -> any                          (hypothesis stage)
//	flaky              -> flaky, confirmed, likely, inconclusive
//	harness_failure    -> harness_failure, confirmed, environmental_failure
//	environmental_failure -> environmental_failure, confirmed, harness_failure
//	inconclusive       -> any
//
// harness_failure and environmental_failure are TERMINAL-ISH: they may only
// be left WITH NEW EVIDENCE, enforced as a non-empty reason (see
// ValidateTransition) and, for confirmed, as a recorded independent
// reproduction (see FindingService.Transition).
var findingTransitions = map[FindingStatus]map[FindingStatus]bool{
	FindingConfirmed: {
		FindingConfirmed: true, FindingFlaky: true,
	},
	FindingLikely: {
		FindingLikely: true, FindingConfirmed: true, FindingFlaky: true,
		FindingHarness: true, FindingEnvironmental: true, FindingInconclusive: true,
	},
	FindingFlaky: {
		FindingFlaky: true, FindingConfirmed: true, FindingLikely: true,
		FindingInconclusive: true,
	},
	FindingHarness: {
		FindingHarness: true, FindingConfirmed: true, FindingEnvironmental: true,
	},
	FindingEnvironmental: {
		FindingEnvironmental: true, FindingConfirmed: true, FindingHarness: true,
	},
	FindingInconclusive: {
		FindingInconclusive: true, FindingLikely: true, FindingConfirmed: true,
		FindingFlaky: true, FindingHarness: true, FindingEnvironmental: true,
	},
}

// validFindingStatus reports whether st is part of the pinned taxonomy.
func validFindingStatus(st FindingStatus) bool {
	_, ok := findingTransitions[st]
	return ok
}

// ValidateTransition enforces the status machine. Same-status transitions
// are always allowed (timestamp refresh). Leaving a terminal-ish status
// (harness_failure, environmental_failure) requires a non-empty reason —
// the caller's record of the NEW EVIDENCE that justifies reclassification.
func ValidateTransition(from, to FindingStatus, reason string) error {
	if !validFindingStatus(from) {
		return fmt.Errorf("%w: unknown finding status %q", ErrInvalid, from)
	}
	if !validFindingStatus(to) {
		return fmt.Errorf("%w: unknown finding status %q", ErrInvalid, to)
	}
	if from == to {
		return nil
	}
	if !findingTransitions[from][to] {
		return fmt.Errorf("%w: finding transition %s -> %s is not allowed", ErrInvalid, from, to)
	}
	if (from == FindingHarness || from == FindingEnvironmental) && strings.TrimSpace(reason) == "" {
		return fmt.Errorf("%w: leaving terminal-ish status %s requires a new-evidence reason", ErrInvalid, from)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Confidence (documented weights over bundle completeness)
// ---------------------------------------------------------------------------

// Confidence bases per status plus the completeness bonus. Confidence =
// base + 0.10 * bundle completeness, capped at 1.0. A confirmed finding
// with a fully complete bundle reaches exactly 1.0; an inconclusive one
// with no evidence sits at 0.10. The table is intentionally simple and
// documented — confidence must never fake precision.
const (
	confBaseConfirmed     = 0.90
	confBaseLikely        = 0.55
	confBaseFlaky         = 0.60
	confBaseHarness       = 0.45
	confBaseEnvironmental = 0.45
	confBaseInconclusive  = 0.10
	confCompletenessBonus = 0.10
)

func confidenceFor(st FindingStatus, completeness float64) float64 {
	var base float64
	switch st {
	case FindingConfirmed:
		base = confBaseConfirmed
	case FindingLikely:
		base = confBaseLikely
	case FindingFlaky:
		base = confBaseFlaky
	case FindingHarness:
		base = confBaseHarness
	case FindingEnvironmental:
		base = confBaseEnvironmental
	default:
		base = confBaseInconclusive
	}
	c := base + confCompletenessBonus*completeness
	if c > 1 {
		c = 1
	}
	if c < 0 {
		c = 0
	}
	return c
}

// ---------------------------------------------------------------------------
// FindingService
// ---------------------------------------------------------------------------

// StatusTransitionRecord is the timestamped audit entry for one validated
// status change (service-level audit log; Postgres persistence of the row
// state itself is the store's job).
type StatusTransitionRecord struct {
	FindingID string
	From      FindingStatus
	To        FindingStatus
	Reason    string
	At        time.Time
}

// FindingService classifies, dedups, persists and re-verifies findings
// (#150). Deterministic signals always decide; the optional AI seam can
// only add hypothesis text.
type FindingService struct {
	store FindingStore
	ai    RootCauseClassifier
	now   func() time.Time

	mu          sync.Mutex
	transitions map[string][]StatusTransitionRecord // finding id -> audit trail
}

// NewFindingService builds a service over store. store may be nil (an
// InMemoryFindingStore is created — useful for tests and offline tooling).
// ai may be nil: the default is deterministic-only classification. now may
// be nil (time.Now is used); inject a fixed clock for determinism.
func NewFindingService(store FindingStore, ai RootCauseClassifier, now func() time.Time) *FindingService {
	if store == nil {
		store = NewInMemoryFindingStore()
	}
	if now == nil {
		now = time.Now
	}
	return &FindingService{
		store:       store,
		ai:          ai,
		now:         now,
		transitions: map[string][]StatusTransitionRecord{},
	}
}

// Classify computes a Finding from deterministic signals (issue #150
// signature, plus the explicit targetID the fingerprint contract requires —
// the pinned Run type deliberately carries no target id).
//
// Precedence: independent reproduction (confirmed/flaky) > harness >
// environmental > likely (failed assertion + consistent signals) >
// inconclusive. The evidence bundle is built internally (redaction applied)
// so confidence reflects bundle completeness. No store access, no clock
// besides `now`; safe to call concurrently.
func (s *FindingService) Classify(run Run, targetID string, results []AssertionOutcome, repro *ReproResult) Finding {
	if run.OrgID == "" || run.ID == "" {
		return Finding{Status: FindingInconclusive}
	}
	bundle, err := NewEvidenceBundle(run).
		WithAssertionResults(results).
		WithEvidenceRefs(run.EvidenceRefs).
		Build(s.now())
	if err != nil {
		return Finding{Status: FindingInconclusive}
	}

	class := DetectFailureClass(run, results)
	failed := firstFailed(results)

	var status FindingStatus
	switch {
	case repro != nil && repro.ReproRunID != "" && repro.Reproduced:
		// Independent reproduction is the strongest evidence: it overrides
		// even harness/environmental verdicts from the original run.
		status = FindingConfirmed
	case repro != nil && repro.ReproRunID != "" && !repro.Reproduced:
		// A real isolated re-run happened and the failure did NOT appear.
		status = FindingFlaky
	case class == FailureHarness:
		status = FindingHarness
	case class == FailureEnvironmental:
		status = FindingEnvironmental
	case failed != nil:
		// Failed assertion with consistent deterministic signals, no
		// independent reproduction yet.
		status = FindingLikely
	default:
		status = FindingInconclusive
	}

	severity := severityFor(class, failed)

	fp := Fingerprint(run.OrgID, targetID, assertionKindOf(failed), class)
	f := Finding{
		OrgID:       run.OrgID,
		RunID:       run.ID,
		JourneyID:   run.JourneyID,
		AssertionIx: assertionIxOf(failed),
		Status:      status,
		Severity:    severity,
		Fingerprint: fp,
		ReproSteps:  strings.Join(bundle.ReproSteps, "\n"),
		Confidence:  confidenceFor(status, bundle.Completeness()),
	}
	f.Title = titleFor(run, class, failed)
	f.Description = descriptionFor(run, class, failed)
	f.EvidenceRefs = append([]string{}, run.EvidenceRefs...)

	// Optional AI assist: hypothesis text only, language-enforced. A
	// classifier that asserts certainty without the "hypothesis:" marker is
	// DISCARDED — never presented as verified root cause.
	if s.ai != nil {
		if hyp, ok := s.ai.SuggestRootCause(context.Background(), bundle); ok {
			if normalized, ok := enforceHypothesisLanguage(hyp); ok {
				f.Description += "\n" + normalized
			}
		}
	}

	// Repro bookkeeping flows into the record when provided.
	if repro != nil && repro.ReproRunID != "" {
		f.ReproRunID = repro.ReproRunID
		reproduced := repro.Reproduced
		f.Reproducible = &reproduced
	}
	return f
}

func assertionKindOf(failed *AssertionOutcome) AssertionKind {
	if failed == nil {
		return ""
	}
	return failed.Kind
}

func assertionIxOf(failed *AssertionOutcome) int {
	if failed == nil {
		return -1
	}
	return failed.Index
}

// severityFor: assertion-driven statuses inherit the assertion severity
// (caller default medium); harness failures are informational (the app is
// not implicated); environmental failures are low (infra noise, still worth
// surfacing).
func severityFor(class FailureClass, failed *AssertionOutcome) Severity {
	switch class {
	case FailureHarness:
		return SevInfo
	case FailureEnvironmental:
		return SevLow
	}
	if failed != nil && failed.Severity != "" {
		return Severity(failed.Severity)
	}
	return SevMedium
}

func titleFor(run Run, class FailureClass, failed *AssertionOutcome) string {
	switch class {
	case FailureHarness:
		return fmt.Sprintf("Harness failure in journey %s (app not implicated)", run.JourneyID)
	case FailureEnvironmental:
		return fmt.Sprintf("Environmental failure during journey %s", run.JourneyID)
	}
	if failed != nil {
		return fmt.Sprintf("%s assertion failed on %s", failed.Kind, failed.Target)
	}
	return fmt.Sprintf("Inconclusive run %s", run.ID)
}

func descriptionFor(run Run, class FailureClass, failed *AssertionOutcome) string {
	switch class {
	case FailureHarness:
		return "Deterministic harness/worker signals in the run error stream; " +
			"the application under test is not implicated. Hypotheses about the " +
			"product require independent reproduction first."
	case FailureEnvironmental:
		return "Deterministic infrastructure signals (connectivity, gateway, TLS " +
			"or rate limiting) during the run; not an application defect."
	}
	if failed != nil {
		return fmt.Sprintf("Assertion #%d (%s) on %s expected %q; observed %q.",
			failed.Index, failed.Kind, failed.Target, failed.Expected, failed.Actual)
	}
	return fmt.Sprintf("Run ended in status %s without a failing assertion; signals were inconclusive.", run.Status)
}

// findingOccurrenceID is the deterministic row id for one occurrence:
// fnd_<sha256(org, fingerprint, run)[0:24]>.
func findingOccurrenceID(orgID, fingerprint, runID string) string {
	return "fnd_" + evShortHash("finding", orgID, fingerprint, runID)[:24]
}

// unionRefs merges evidence ref lists deterministically (first-seen order,
// no duplicates) — history is never lost on dedup.
func unionRefs(lists ...[]string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, l := range lists {
		for _, r := range l {
			if r == "" || seen[r] {
				continue
			}
			seen[r] = true
			out = append(out, r)
		}
	}
	return out
}

// validateFindingInput enforces structural invariants before persistence.
func (s *FindingService) validateFindingInput(f Finding) error {
	if f.OrgID == "" || f.RunID == "" || f.JourneyID == "" {
		return fmt.Errorf("%w: finding requires org, run and journey", ErrInvalid)
	}
	if len(f.Fingerprint) != 64 {
		return fmt.Errorf("%w: finding fingerprint must be a sha-256 hex digest", ErrInvalid)
	}
	if !validFindingStatus(f.Status) {
		return fmt.Errorf("%w: unknown finding status %q", ErrInvalid, f.Status)
	}
	return nil
}

// Record persists a classified finding with dedup. First sighting: the
// occurrence row is created (deterministic id). Repeat fingerprint within
// the same org: a NEW occurrence row is appended (history is never lost)
// and status changes are validated against the transition rules — an
// invalid transition records the occurrence under the canonical status
// instead. Org isolation is structural: fingerprints embed the org and all
// store lookups are org-scoped.
func (s *FindingService) Record(ctx context.Context, f Finding) (Finding, error) {
	if err := s.validateFindingInput(f); err != nil {
		return Finding{}, err
	}
	now := s.now()

	history, err := s.store.ListByFingerprint(ctx, f.OrgID, f.Fingerprint)
	if err != nil {
		return Finding{}, err
	}
	if len(history) == 0 {
		f.ID = findingOccurrenceID(f.OrgID, f.Fingerprint, f.RunID)
		f.CreatedAt = now
		f.UpdatedAt = now
		if err := s.store.Record(ctx, f); err != nil {
			return Finding{}, err
		}
		return f, nil
	}

	canonical := history[len(history)-1]
	status := canonical.Status
	if err := ValidateTransition(canonical.Status, f.Status, "new classified occurrence"); err == nil {
		status = transitionOnOccurrence(canonical, f)
	} // else: disallowed transition — occurrence recorded under canonical status.

	occ := f
	occ.ID = findingOccurrenceID(f.OrgID, f.Fingerprint, f.RunID)
	occ.CreatedAt = now
	occ.UpdatedAt = now
	occ.Status = status
	occ.ReproRunID = firstNonEmpty(f.ReproRunID, canonical.ReproRunID)
	occ.Reproducible = firstReproducible(f.Reproducible, canonical.Reproducible)
	occ.EvidenceRefs = unionRefs(canonical.EvidenceRefs, f.EvidenceRefs)
	if err := s.store.Record(ctx, occ); err != nil {
		return Finding{}, err
	}
	return occ, nil
}

// terminalishFindingStatus reports whether a status is terminal-ish:
// harness_failure / environmental_failure may only be reclassified with NEW
// evidence (documented in the transition map above).
func terminalishFindingStatus(st FindingStatus) bool {
	return st == FindingHarness || st == FindingEnvironmental
}

// transitionOnOccurrence resolves the status a deduped occurrence records.
// Leaving a terminal-ish status for confirmed additionally requires new
// evidence in the form of an independent reproduction (carried by the new
// occurrence or already recorded on the canonical finding); the harness<->-
// environmental sibling reclassification is driven by the deterministic
// signals of the new occurrence itself.
func transitionOnOccurrence(canonical Finding, next Finding) FindingStatus {
	if canonical.Status == next.Status {
		return next.Status
	}
	if terminalishFindingStatus(canonical.Status) && next.Status == FindingConfirmed {
		newEvidence := (next.Reproducible != nil && *next.Reproducible) ||
			(canonical.Reproducible != nil && *canonical.Reproducible)
		if !newEvidence {
			return canonical.Status
		}
	}
	return next.Status
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstReproducible(vals ...*bool) *bool {
	for _, v := range vals {
		if v != nil {
			return v
		}
	}
	return nil
}

// Occurrences returns the append-only occurrence log for a fingerprint
// within org (derived from the occurrence rows; never merges across orgs).
func (s *FindingService) Occurrences(ctx context.Context, orgID, fingerprint string) ([]Occurrence, error) {
	rows, err := s.store.ListByFingerprint(ctx, orgID, fingerprint)
	if err != nil {
		return nil, err
	}
	out := make([]Occurrence, 0, len(rows))
	for _, r := range rows {
		out = append(out, Occurrence{RunID: r.RunID, At: r.CreatedAt})
	}
	return out, nil
}

// Get / Latest / List are thin org-scoped store passthroughs.
func (s *FindingService) Get(ctx context.Context, orgID, id string) (Finding, error) {
	return s.store.Get(ctx, orgID, id)
}

// Latest returns the canonical (most recent) finding for a fingerprint.
func (s *FindingService) Latest(ctx context.Context, orgID, fingerprint string) (Finding, error) {
	return s.store.GetByFingerprint(ctx, orgID, fingerprint)
}

// List returns findings within org matching filter.
func (s *FindingService) List(ctx context.Context, orgID string, filter FindingFilter) ([]Finding, error) {
	return s.store.List(ctx, orgID, filter)
}

// Reproduce independently re-runs the finding's failing assertion in an
// ISOLATED environment via re, records repro_run_id + reproducible, and
// applies the validated status transition.
//
// Safety rails (enforced):
//   - env must belong to the finding's org (ErrScopeViolation otherwise);
//   - env.Mode == production_read_only is refused UNLESS the runner
//     supports the read-only subset (SupportsReadOnly) — the request is
//     then flagged ReadOnlySubset and the runner MUST NOT execute writes;
//     production writes are never replayed either way.
//
// A runner error leaves the finding untouched (the reproduction itself
// failed — harness-level, not app evidence). A completed re-run records
// repro fields and, when the transition rules allow, moves confirmed/flaky;
// on terminal-ish statuses a non-confirmed result only records the new
// evidence without a status change (documented).
func (s *FindingService) Reproduce(ctx context.Context, f *Finding, env Environment, re ReproRunner) (Finding, error) {
	if f == nil || re == nil {
		return Finding{}, fmt.Errorf("%w: reproduce requires a finding and a runner", ErrInvalid)
	}
	if env.OrgID != f.OrgID {
		return Finding{}, fmt.Errorf("%w: reproduction environment org %q != finding org %q",
			ErrScopeViolation, env.OrgID, f.OrgID)
	}
	readOnlySubset := env.Mode == ModeProductionReadOnly
	if readOnlySubset && !re.SupportsReadOnly() {
		return Finding{}, fmt.Errorf("%w: runner cannot honor the read-only subset required by %s",
			ErrReadOnlyMode, ModeProductionReadOnly)
	}

	req := ReproRequest{
		OrgID:          f.OrgID,
		FindingID:      f.ID,
		OriginalRunID:  f.RunID,
		JourneyID:      f.JourneyID,
		AssertionIx:    f.AssertionIx,
		EnvironmentID:  env.ID,
		Mode:           env.Mode,
		ReadOnlySubset: readOnlySubset,
	}
	res, err := re.ReproRun(ctx, req)
	if err != nil {
		return Finding{}, fmt.Errorf("synth: isolated reproduction failed: %w", err)
	}
	if res.ReproRunID == "" {
		return Finding{}, fmt.Errorf("%w: runner returned an empty repro run id", ErrInvalid)
	}

	now := s.now()
	f.ReproRunID = res.ReproRunID
	f.Reproducible = &res.Reproduced

	target := FindingFlaky
	if res.Reproduced {
		target = FindingConfirmed
	}
	if err := ValidateTransition(f.Status, target, "independent reproduction"); err == nil {
		f.Status = target
		if c := confidenceFor(f.Status, 0); c > f.Confidence {
			f.Confidence = c
		}
	} // else (terminal-ish + non-confirmed result): evidence recorded, status retained.
	f.UpdatedAt = now
	if err := s.store.Record(ctx, *f); err != nil {
		return Finding{}, err
	}
	return *f, nil
}

// Transition applies a validated, timestamped status change. Terminal-ish
// statuses require a non-empty new-evidence reason; confirming requires the
// finding to already carry an independent reproduction (reproducible=true).
// Every accepted transition is appended to the service-level audit log.
func (s *FindingService) Transition(ctx context.Context, orgID, id string, to FindingStatus, reason string) (Finding, error) {
	f, err := s.store.Get(ctx, orgID, id)
	if err != nil {
		return Finding{}, err
	}
	if err := ValidateTransition(f.Status, to, reason); err != nil {
		return Finding{}, err
	}
	if to == FindingConfirmed && (f.Reproducible == nil || !*f.Reproducible) {
		return Finding{}, fmt.Errorf(
			"%w: confirming requires an independently reproduced run (record reproduction first)", ErrInvalid)
	}
	from := f.Status
	f.Status = to
	f.UpdatedAt = s.now()
	if err := s.store.Record(ctx, f); err != nil {
		return Finding{}, err
	}
	s.mu.Lock()
	s.transitions[f.ID] = append(s.transitions[f.ID], StatusTransitionRecord{
		FindingID: f.ID, From: from, To: to, Reason: reason, At: f.UpdatedAt,
	})
	s.mu.Unlock()
	return f, nil
}

// Transitions returns the timestamped audit trail for a finding (service
// lifetime scope).
func (s *FindingService) Transitions(findingID string) []StatusTransitionRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]StatusTransitionRecord{}, s.transitions[findingID]...)
	return out
}
