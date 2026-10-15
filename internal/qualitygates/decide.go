package qualitygates

// decide.go — the deterministic gate evaluation (issue #152).
//
// Decide is a PURE function: same policy + same evidence + same `now` =>
// byte-identical Decision (pinned by TestDecideDeterministicPinned). There
// is no clock read, no map iteration in output-affecting order, no
// randomness. Recommendations:
//
//	promote   every required suite passed (or was waived by an unexpired
//	          exception on a non-security suite)
//	abort     a HARD SECURITY FAILURE: a fail-closed (security) suite
//	          failed — the release must not proceed AND any in-flight
//	          canary must stop (aborts outrank everything)
//	rollback  a proven quality regression: a non-security suite failed its
//	          thresholds — the canary is worse than baseline
//	hold      evidence is missing / stale / inconclusive: nothing is
//	          proven yet; the canary keeps running while evidence accrues
//	          (mirrors the deployments MinCanaryRuns evidence gate)
//
// Precedence (deterministic): abort > rollback > hold > promote.
//
// Check statuses:
//
//	passed        suite satisfied every threshold
//	failed        a threshold (or hard security) check failed
//	blocked       required suite has NO evidence
//	stale         evidence older than the requirement's MaxAge (blocks)
//	inconclusive  run inconclusive or recorded zero cases (blocks)
//	skipped       waived by an unexpired exception (non-security only)
//
// Structural security enforcement: a requirement with FailClosed=true
// (mandatory for security_adversarial, see Validate) ignores ALL score
// balancing — its failures are never excepted, never averaged away, and
// always abort. Exceptions apply ONLY to non-fail-closed suites and only
// while unexpired (ExpiresAt.After(now)).

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// CheckStatus is the per-suite check outcome (see file header).
type CheckStatus string

const (
	CheckPassed       CheckStatus = "passed"
	CheckFailed       CheckStatus = "failed"
	CheckBlocked      CheckStatus = "blocked"
	CheckSkipped      CheckStatus = "skipped"
	CheckStale        CheckStatus = "stale"
	CheckInconclusive CheckStatus = "inconclusive"
)

// Recommendation is the gate's release verdict (see file header).
type Recommendation string

const (
	RecommendPromote  Recommendation = "promote"
	RecommendHold     Recommendation = "hold"
	RecommendAbort    Recommendation = "abort"
	RecommendRollback Recommendation = "rollback"
)

// CheckResult is one required suite's outcome inside a Decision.
type CheckResult struct {
	// Suite is the suite family evaluated (the requirement Kind).
	Suite string `json:"suite"`
	// Status is the check outcome.
	Status CheckStatus `json:"status"`
	// Reason is a deterministic human-readable explanation quoting the
	// observed value against the threshold (or the waiver/block cause).
	Reason string `json:"reason"`
}

// Decision is the gate evaluation output (immutable by convention).
type Decision struct {
	// PolicyID / PolicyVersion pin the policy that produced the decision.
	PolicyID string `json:"policy_id"`
	// PolicyVersion is the integer policy version evaluated.
	PolicyVersion int `json:"policy_version"`
	// PolicyEvaluationVersion is the policy's EvaluationVersion content
	// hash — the audit-grade pin of the exact rule set.
	PolicyEvaluationVersion string `json:"policy_evaluation_version"`
	// Recommendation is the verdict.
	Recommendation Recommendation `json:"recommendation"`
	// CheckResults is one entry per required suite, sorted by suite kind
	// (deterministic order regardless of policy list order).
	CheckResults []CheckResult `json:"check_results"`
	// EvaluatedAt is the injected decision time (never a wall clock read).
	EvaluatedAt time.Time `json:"evaluated_at"`
}

// Decide evaluates the policy against the evidence set at instant `now`
// and returns the deterministic Decision (see the file header for the full
// rule matrix). Evidence records are matched to requirements BY SUITE
// KIND; when several records for one kind are supplied the NEWEST one
// (CollectedAt desc, ID desc tiebreak) is evaluated — the caller is
// responsible for subject-scoping the input set (the service layer
// resolves explicit evidence refs).
func Decide(policy *GatePolicy, evidence []EvidenceRecord, now time.Time) *Decision {
	d := &Decision{
		PolicyID:                "",
		PolicyVersion:           0,
		PolicyEvaluationVersion: "",
		Recommendation:          RecommendPromote,
		CheckResults:            []CheckResult{},
		EvaluatedAt:             now.UTC(),
	}
	if policy == nil {
		// A nil policy is a caller contract violation; the decision is
		// "hold everything" rather than a panic (defensive, documented).
		d.Recommendation = RecommendHold
		d.CheckResults = append(d.CheckResults, CheckResult{
			Suite: "", Status: CheckBlocked, Reason: "no policy supplied",
		})
		return d
	}
	d.PolicyID = policy.ID
	d.PolicyVersion = policy.Version
	d.PolicyEvaluationVersion = policy.EvaluationVersion()

	newest := newestBySuite(evidence)

	// Canonical requirement order: sorted by kind (stable regardless of
	// how the policy stored its list — output is a pure function of
	// policy CONTENT, not list order).
	reqs := make([]SuiteRequirement, len(policy.RequiredSuites))
	copy(reqs, policy.RequiredSuites)
	sort.SliceStable(reqs, func(i, j int) bool { return reqs[i].Kind < reqs[j].Kind })

	// abortSeen/rollbackSeen/holdSeen track the highest-priority signal.
	decision := RecommendPromote
	setWorst := func(r Recommendation) {
		if rank(r) < rank(decision) {
			decision = r
		}
	}
	for _, req := range reqs {
		result := evaluateRequirement(policy, req, newest[req.Kind], now)
		d.CheckResults = append(d.CheckResults, result)
		switch result.Status {
		case CheckFailed:
			if req.FailClosed {
				setWorst(RecommendAbort) // hard security failure — always aborts
			} else {
				setWorst(RecommendRollback) // proven quality regression
			}
		case CheckBlocked, CheckStale, CheckInconclusive:
			setWorst(RecommendHold) // nothing proven yet — wait for evidence
		case CheckPassed, CheckSkipped:
			// satisfied (or waived) — no downgrade
		}
	}
	d.Recommendation = decision
	return d
}

// rank orders the recommendation precedence (lower = stronger): abort >
// rollback > hold > promote.
func rank(r Recommendation) int {
	switch r {
	case RecommendAbort:
		return 0
	case RecommendRollback:
		return 1
	case RecommendHold:
		return 2
	default:
		return 3
	}
}

// evaluateRequirement computes one suite's CheckResult (see the file
// header for the status matrix).
func evaluateRequirement(policy *GatePolicy, req SuiteRequirement, evidence EvidenceRecord, now time.Time) CheckResult {
	suite := string(req.Kind)
	res := CheckResult{Suite: suite}

	// 1) Missing evidence blocks (never passes by absence).
	if evidence.ID == "" {
		res.Status = CheckBlocked
		res.Reason = "required suite has no evidence"
		return res
	}

	// 2) Staleness: evidence older than MaxAge is stale and blocks per
	// policy (freshness is part of the gate contract, not advisory).
	if req.MaxAge > 0 && now.Sub(evidence.CollectedAt) > req.MaxAge {
		res.Status = CheckStale
		res.Reason = fmt.Sprintf("evidence %s collected %s ago exceeds max age %s (stale evidence blocks)", evidence.ID, formatDuration(now.Sub(evidence.CollectedAt)), formatDuration(req.MaxAge))
		return res
	}

	// 3) Inconclusive runs are explicitly NOT pass/fail.
	if evidence.Result.Status == EvidenceInconclusive {
		res.Status = CheckInconclusive
		res.Reason = fmt.Sprintf("evidence %s is inconclusive (interrupted run is neither passed nor failed)", evidence.ID)
		return res
	}
	if evidence.Result.Cases <= 0 {
		res.Status = CheckInconclusive
		res.Reason = fmt.Sprintf("evidence %s recorded no cases (nothing to evaluate)", evidence.ID)
		return res
	}

	// 4) HARD SECURITY FAILURE — evaluated BEFORE any aggregate so a
	// fail-closed suite can never be rescued by score balancing (100%
	// pass rate does not excuse a critical finding).
	if finding := firstFindingAbove(evidence.Result.Findings, req.MaxSeverityAllowed); finding != nil {
		res.Status = CheckFailed
		res.Reason = fmt.Sprintf("hard security failure: finding %q severity %s exceeds max allowed %s (aggregate scores ignored)", finding.Title, finding.Severity, req.MaxSeverityAllowed)
		return res
	}

	// 5) Aggregate thresholds in fixed order: coverage, pass rate, p95.
	passRate := float64(evidence.Result.Passed) / float64(evidence.Result.Cases)
	failed := ""
	switch {
	case req.MinCoveragePct > 0 && evidence.Result.CoveragePct < req.MinCoveragePct:
		failed = fmt.Sprintf("coverage %s%% < %s%%", formatFloat(evidence.Result.CoveragePct), formatFloat(req.MinCoveragePct))
	case req.MinPassRate > 0 && passRate < req.MinPassRate:
		failed = fmt.Sprintf("pass_rate %s < %s", formatFloat(passRate), formatFloat(req.MinPassRate))
	case req.MaxP95Ms > 0 && evidence.Result.P95Ms > req.MaxP95Ms:
		failed = fmt.Sprintf("p95_ms %d > %d", evidence.Result.P95Ms, req.MaxP95Ms)
	}
	if failed != "" {
		res.Status = CheckFailed
		res.Reason = failed
		// 6) Exceptions override ONLY non-security suites and only while
		// unexpired. FailClosed suites are structurally unwaivable —
		// this branch is unreachable for them by policy validation, and
		// the guard below enforces it even against a hand-built policy.
		if !req.FailClosed && findActiveException(policy.Exceptions, req.Kind, now) != nil {
			ex := findActiveException(policy.Exceptions, req.Kind, now)
			res.Status = CheckSkipped
			res.Reason = fmt.Sprintf("%s (waived by exception %q approved by %s, expires %s)", failed, ex.Reason, ex.ApprovedBy, ex.ExpiresAt.UTC().Format(time.RFC3339))
			return res
		}
		return res
	}

	res.Status = CheckPassed
	res.Reason = fmt.Sprintf("evidence %s satisfied every threshold (pass_rate %s, coverage %s%%, p95_ms %d)", evidence.ID, formatFloat(passRate), formatFloat(evidence.Result.CoveragePct), evidence.Result.P95Ms)
	return res
}

// firstFindingAbove returns the first finding (input order) ranked strictly
// above the allowed severity. An unset (empty) MaxSeverityAllowed tolerates
// everything EXCEPT for fail-closed suites, where validation already made
// it impossible — the guard keeps Decide total for hand-built policies.
func firstFindingAbove(findings []EvidenceFinding, allowed Severity) *EvidenceFinding {
	allowedRank := 4 // tolerate everything when unset (info..critical <= 4)
	if allowed != "" {
		r, ok := severityRank(allowed)
		if !ok {
			// Unknown severity string on a hand-built policy: fail CLOSED
			// (tolerate nothing above info) rather than open.
			allowedRank = 0
		} else {
			allowedRank = r
		}
	}
	for i := range findings {
		if rank, ok := severityRank(findings[i].Severity); ok && rank > allowedRank {
			f := findings[i]
			return &f
		}
	}
	return nil
}

// findActiveException returns the FIRST (deterministic: sorted by expiry
// ascending, then reason) unexpired exception matching the suite kind —
// an exact-kind match wins over the blanket "" match.
func findActiveException(exs []Exception, kind SuiteKind, now time.Time) *Exception {
	var best *Exception
	for i := range exs {
		ex := &exs[i]
		if ex.Suite != kind && ex.Suite != "" {
			continue
		}
		if ex.Suite == "" && kind == SuiteSecurityAdversarial {
			continue // blanket waivers never touch security suites (belt-and-braces)
		}
		if !ex.ExpiresAt.After(now) {
			continue // expired: no effect
		}
		if best == nil ||
			(best.Suite == "" && ex.Suite == kind) || // exact match outranks blanket
			(best.Suite == ex.Suite && ex.ExpiresAt.Before(best.ExpiresAt)) {
			best = ex
		}
	}
	return best
}

// formatFloat renders a float deterministically (shortest exact repr).
func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// formatDuration renders a duration deterministically (Go default layout).
func formatDuration(d time.Duration) string {
	s := d.String()
	return strings.TrimSpace(s)
}
