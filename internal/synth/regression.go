package synth

// regression.go — issue #151 (wave-10 track 10-s): the regression-test
// generation loop and fix verification for the synthetic-assurance track.
//
// Loop: a CONFIRMED finding (independently reproduced — the findings track
// never presents hypotheses as verified root cause) generates a candidate
// Journey (Source "regression"). The candidate is born Status=JourneyDraft
// and STAYS draft until a human-approved ApprovalGate records an explicit
// approval for its journey id; only then can the scheduling / release-gate
// tracks promote it. Generated candidates are therefore never scheduled and
// never release-blocking by accident.
//
// Deterministic conversion: repro lines that match the documented mini-
// grammar (`N. action target [value]`) become JourneySteps 1:1 — no model
// inference in the step path. Lines that do not parse are NOT invented into
// steps; the candidate is tagged "needs_authoring" and carries a placeholder
// so an author can finish it.
//
// Fix verification (VerifyFix) runs the candidate through a narrow Runner
// seam. The runner contract targets an ISOLATED, pre-production environment
// — this file performs no production writes and never touches the GitHub
// client. A harness failure maps to Inconclusive, never to Fail: a broken
// harness is not product evidence.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Approval gate — candidates stay draft until explicitly approved
// ---------------------------------------------------------------------------

// ApprovalGate records an explicit human approval for a generated journey
// candidate. Approve(journeyID) is the decision record; promotion helpers
// refuse to move a candidate out of JourneyDraft until its id is approved.
type ApprovalGate interface {
	Approve(journeyID string) error
}

// approvedChecker is the optional read-side capability of an ApprovalGate.
// InMemoryApprovalGate implements it; promotion requires a gate that can
// answer "was this id approved?" — an approve-only gate is rejected so the
// draft→reviewed transition can never be granted by an uncheckable claim.
type approvedChecker interface {
	Approved(journeyID string) bool
}

// InMemoryApprovalGate is the process-lifetime approval ledger.
type InMemoryApprovalGate struct {
	mu       sync.Mutex
	approved map[string]bool
}

// NewInMemoryApprovalGate returns an empty ledger.
func NewInMemoryApprovalGate() *InMemoryApprovalGate {
	return &InMemoryApprovalGate{approved: map[string]bool{}}
}

// Approve records an explicit approval for journeyID. Empty ids are refused.
func (g *InMemoryApprovalGate) Approve(journeyID string) error {
	if strings.TrimSpace(journeyID) == "" {
		return fmt.Errorf("%w: journey id is required for approval", ErrInvalid)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.approved[journeyID] = true
	return nil
}

// Approved reports whether journeyID carries a recorded approval.
func (g *InMemoryApprovalGate) Approved(journeyID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.approved[journeyID]
}

// PromoteRegressionCandidate moves an APPROVED regression candidate from
// JourneyDraft to JourneyReviewed — the state the scheduling and release-
// gate tracks consume. Unapproved candidates are refused (ErrNotApproved)
// and stay draft; non-regression or non-draft journeys are refused outright.
func PromoteRegressionCandidate(gate ApprovalGate, j *Journey) error {
	if gate == nil || j == nil {
		return fmt.Errorf("%w: promotion requires a gate and a journey", ErrInvalid)
	}
	checker, ok := gate.(approvedChecker)
	if !ok {
		return fmt.Errorf("%w: approval gate must expose its approval state for verification", ErrInvalid)
	}
	if !checker.Approved(j.ID) {
		return fmt.Errorf("%w: journey %s — candidates stay draft until explicitly approved", ErrNotApproved, j.ID)
	}
	if j.Source != SourceRegression {
		return fmt.Errorf("%w: journey %s has source %q, not %q", ErrInvalid, j.ID, j.Source, SourceRegression)
	}
	if j.Status != JourneyDraft {
		return fmt.Errorf("%w: journey %s is %q, only draft candidates are promotable", ErrInvalid, j.ID, j.Status)
	}
	j.Status = JourneyReviewed
	return nil
}

// tagNeedsAuthoring marks candidates whose repro notes did not fully
// convert into deterministic steps.
const tagNeedsAuthoring = "needs_authoring"

// ErrNotApproved is returned when promotion is attempted for an unapproved
// candidate.
var ErrNotApproved = fmt.Errorf("synth: regression candidate not approved")

// ---------------------------------------------------------------------------
// Candidate generation
// ---------------------------------------------------------------------------

// RegressionOptions carries the tenancy context for the generated candidate.
// The candidate is NOT scheduled against anything by generation alone.
type RegressionOptions struct {
	OrgID         string
	ProjectID     string
	EnvironmentID string
	PersonaID     string
}

// journeyIDSanitizer keeps generated ids URL/CLI friendly.
var journeyIDSanitizer = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// GenerateRegressionTest converts a CONFIRMED finding into a draft Journey
// candidate (Source "regression", Status JourneyDraft — REQUIRED: candidates
// stay draft until explicitly approved via ApprovalGate).
//
// Determinism: the journey id is a pure function of (org, fingerprint);
// structured repro lines map 1:1 onto JourneySteps. Where repro data is NOT
// structured, nothing is invented — the candidate is tagged
// "needs_authoring" and a placeholder step/assertion marks the gap for an
// author. All finding text is scrubbed (ScrubSecrets) before it lands in the
// spec.
func GenerateRegressionTest(f *Finding, opts RegressionOptions) (*Journey, error) {
	if f == nil {
		return nil, fmt.Errorf("%w: finding is nil", ErrInvalid)
	}
	if f.Status != FindingConfirmed {
		return nil, fmt.Errorf("%w: only CONFIRMED findings generate regression candidates (finding %s is %q)",
			ErrInvalid, f.ID, f.Status)
	}
	if strings.TrimSpace(f.ID) == "" || strings.TrimSpace(f.Fingerprint) == "" || strings.TrimSpace(f.Title) == "" {
		return nil, fmt.Errorf("%w: finding requires ID, Title and Fingerprint", ErrInvalid)
	}
	if strings.TrimSpace(opts.OrgID) == "" {
		return nil, fmt.Errorf("%w: RegressionOptions.OrgID is required", ErrInvalid)
	}
	if f.OrgID != opts.OrgID {
		return nil, fmt.Errorf("%w: finding org %q does not match options org %q", ErrInvalid, f.OrgID, opts.OrgID)
	}

	title := ScrubSecrets(f.Title)
	now := time.Now().UTC()
	id := regressionJourneyID(opts.OrgID, f.Fingerprint)

	steps, remainder := parseReproSteps(f.ReproSteps)
	needsAuthoring := len(remainder) > 0 || len(steps) == 0
	if needsAuthoring {
		// Nothing is invented: unconverted repro notes land in a marked
		// placeholder step that authoring MUST replace before promotion.
		hint := "author deterministic steps from the finding repro notes"
		if len(remainder) > 0 {
			hint += ": " + truncate(ScrubSecrets(strings.Join(remainder, " | ")), 400)
		} else if strings.TrimSpace(f.ReproSteps) != "" {
			hint += "; original notes: " + truncate(ScrubSecrets(f.ReproSteps), 400)
		}
		steps = append(steps, JourneyStep{
			Index:  len(steps) + 1,
			Action: "assert",
			Target: "needs_authoring",
			Hint:   hint,
		})
	}

	preconditions := []string{
		fmt.Sprintf("finding %s (fingerprint %s) was independently confirmed by the findings engine", f.ID, f.Fingerprint),
		"runs against an ISOLATED, pre-production environment only — never production (this candidate may contain write steps copied from the repro)",
	}
	if f.ReproRunID != "" {
		preconditions = append(preconditions, fmt.Sprintf("independent reproduction run: %s", f.ReproRunID))
	}
	if len(remainder) > 0 {
		preconditions = append(preconditions, fmt.Sprintf("author required: %d repro line(s) could not be converted deterministically", len(remainder)))
	}

	// One explicit success criterion is mandatory (types.go: success
	// criteria are ALWAYS explicit). Generation cannot know the real pass
	// condition for the fixed behavior, so it plants a marked placeholder
	// that authoring MUST replace before promotion.
	assertions := []Assertion{{
		Kind:     AssertTextContains,
		Target:   "needs_authoring",
		Expected: fmt.Sprintf("author: replace with the deterministic pass criterion for %q (defect must be absent)", title),
		Severity: riskForSeverity(f.Severity),
	}}

	tags := []string{SourceRegression, "finding:" + f.ID, "fingerprint:" + f.Fingerprint}
	if needsAuthoring {
		tags = append(tags, tagNeedsAuthoring)
	}

	return &Journey{
		ID:            id,
		OrgID:         opts.OrgID,
		ProjectID:     opts.ProjectID,
		EnvironmentID: opts.EnvironmentID,
		PersonaID:     opts.PersonaID,
		Name:          truncate("Regression: "+title, 140),
		Status:        JourneyDraft, // REQUIRED: draft until explicit approval
		Risk:          riskForSeverity(f.Severity),
		Version:       1,
		Preconditions: preconditions,
		Steps:         steps,
		Assertions:    assertions,
		Cleanup:       nil, // no generated teardown writes; authoring may add them
		Tags:          tags,
		Source:        SourceRegression,
		CreatedAt:     now,
		UpdatedAt:     now,
	}, nil
}

// regressionJourneyID derives the stable candidate id from (org, fingerprint).
func regressionJourneyID(orgID, fingerprint string) string {
	sum := sha256.Sum256([]byte("regression\x00" + orgID + "\x00" + fingerprint))
	return "reg-" + hex.EncodeToString(sum[:10])
}

// riskForSeverity maps the finding severity onto the gate-driven RiskClass.
// Unknown severities map to RiskHigh — fail safe toward STRICTER gates.
func riskForSeverity(s Severity) RiskClass {
	switch s {
	case SevInfo, SevLow:
		return RiskLow
	case SevMedium:
		return RiskMedium
	case SevHigh:
		return RiskHigh
	case SevCritical:
		return RiskCritical
	default:
		return RiskHigh
	}
}

// ---------------------------------------------------------------------------
// Deterministic repro-step conversion
// ---------------------------------------------------------------------------

// reproStepPattern is the documented mini-grammar for structured repro lines:
//
//	N. action target [value]     e.g. "1. navigate https://shop.dev/cart"
//	                               "2. fill #email qa@example.com"
//	                               "3. click button[type=submit]"
//
// Actions are exactly the JourneyStep action vocabulary; `wait` puts the
// duration in Target; `api_call` takes the URL as Target and the optional
// body as Value. Anything else is UNSTRUCTURED (needs_authoring).
var reproStepPattern = regexp.MustCompile(`^(\d+)[.)]\s+(\S+)\s*(.*)$`)

// reproActions is the JourneyStep action vocabulary from types.go.
var reproActions = map[string]bool{
	"navigate": true, "click": true, "fill": true, "select": true,
	"wait": true, "api_call": true, "assert": true, "extract": true,
}

// reproWriteActions are state-changing actions: Environment.Mode enforcement
// (types.go JourneyStep.WriteOp) applies to them at run time.
var reproWriteActions = map[string]bool{
	"click": true, "fill": true, "select": true, "api_call": true,
}

// parseReproSteps converts structured repro lines into JourneySteps 1:1 and
// returns everything it could NOT convert as unstructured remainder lines
// (never invented into steps).
func parseReproSteps(repro string) (steps []JourneyStep, unstructured []string) {
	for _, line := range strings.Split(repro, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		m := reproStepPattern.FindStringSubmatch(trimmed)
		if m == nil {
			unstructured = append(unstructured, trimmed)
			continue
		}
		action := strings.ToLower(m[2])
		if !reproActions[action] || m[3] == "" {
			unstructured = append(unstructured, trimmed)
			continue
		}
		// Target is the first field after the action; Value is the
		// remaining text (may itself contain spaces, e.g. fill values).
		fields := strings.SplitN(strings.TrimSpace(m[3]), " ", 2)
		target := fields[0]
		value := ""
		if len(fields) == 2 {
			value = fields[1]
		}
		steps = append(steps, JourneyStep{
			Index:   len(steps) + 1,
			Action:  action,
			Target:  ScrubSecrets(target),
			Value:   ScrubSecrets(value),
			WriteOp: reproWriteActions[action],
		})
	}
	return steps, unstructured
}

// NeedsAuthoring reports whether the candidate still carries the
// needs_authoring marker (i.e. its repro notes did not fully convert).
func NeedsAuthoring(j *Journey) bool {
	if j == nil {
		return false
	}
	for _, tag := range j.Tags {
		if tag == tagNeedsAuthoring {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Fix verification
// ---------------------------------------------------------------------------

// RunnerResult is the minimal outcome VerifyFix consumes from a Runner.
// Status reuses the pinned RunStatus vocabulary: completed → pass,
// failed → fail, anything else (cancelled, paused, lost worker, …) →
// inconclusive.
type RunnerResult struct {
	RunID        string
	Status       RunStatus
	EvidenceRefs []string
	Error        string
}

// Runner is the narrow execution seam for fix verification (issue #151).
// Contract: RunJourney executes the candidate against an ISOLATED,
// pre-production target — NEVER against production (the runner's
// Environment.Mode enforces JourneyStep.WriteOp policy server-side). This
// package performs no writes of its own and never mutates durable state.
type Runner interface {
	RunJourney(ctx context.Context, j *Journey) (RunnerResult, error)
}

// VerificationOutcome is the fix-verification verdict.
type VerificationOutcome string

const (
	VerifyPass         VerificationOutcome = "pass"
	VerifyFail         VerificationOutcome = "fail"
	VerifyInconclusive VerificationOutcome = "inconclusive"
)

// VerificationResult links a fix verification back to the ORIGINAL finding
// (finding id + fingerprint + generated journey) and carries the evidence
// references produced by the run.
type VerificationResult struct {
	FindingID    string
	Fingerprint  string
	JourneyID    string
	Outcome      VerificationOutcome
	RunID        string
	EvidenceRefs []string
	Detail       string
	CheckedAt    time.Time
}

// VerifyFix checks whether a confirmed finding still reproduces after a fix:
// it deterministically regenerates the regression candidate for f and hands
// it to the caller-provided Runner (isolated environment, never production).
//
// Verdict mapping: completed → VerifyPass; failed → VerifyFail; ANY runner
// error or non-terminal status → VerifyInconclusive (a harness failure is
// not product evidence — mirroring the findings taxonomy). The returned
// result always carries the original finding's id and fingerprint so callers
// can link it back.
func VerifyFix(ctx context.Context, f *Finding, re Runner) (VerificationResult, error) {
	res := VerificationResult{CheckedAt: time.Now().UTC()}
	if f == nil || re == nil {
		return res, fmt.Errorf("%w: VerifyFix requires a finding and a Runner", ErrInvalid)
	}
	if f.Status != FindingConfirmed {
		return res, fmt.Errorf("%w: fix verification requires a CONFIRMED finding (finding %s is %q)",
			ErrInvalid, f.ID, f.Status)
	}
	res.FindingID = f.ID
	res.Fingerprint = f.Fingerprint

	// The verified artifact is the deterministic candidate (draft by design
	// — verification is an explicit single-run, not a scheduled release run;
	// the draft gate guards SCHEDULING, not this loop).
	j, err := GenerateRegressionTest(f, RegressionOptions{OrgID: f.OrgID})
	if err != nil {
		return res, fmt.Errorf("regenerate candidate for verification: %w", err)
	}
	res.JourneyID = j.ID

	run, err := re.RunJourney(ctx, j)
	if err != nil {
		res.Outcome = VerifyInconclusive
		res.Detail = "runner error: " + err.Error()
		return res, nil
	}
	res.RunID = run.RunID
	res.EvidenceRefs = run.EvidenceRefs
	switch run.Status {
	case RunCompleted:
		res.Outcome = VerifyPass
		res.Detail = "defect did not reproduce on the isolated runner"
	case RunFailed:
		res.Outcome = VerifyFail
		if run.Error != "" {
			res.Detail = "defect reproduced: " + run.Error
		} else {
			res.Detail = "defect reproduced"
		}
	default:
		res.Outcome = VerifyInconclusive
		res.Detail = fmt.Sprintf("run %q (status %s) is not a pass/fail verdict", run.RunID, run.Status)
	}
	return res, nil
}
