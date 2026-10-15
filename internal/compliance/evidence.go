package compliance

// evidence.go — the deterministic evidence bundle (issue #138): a JSONL
// snapshot of an org's compliance posture over a FIXED window [from, to].
//
// Line shape (fixed order, one JSON object per line, '\n'-terminated):
//
//	{"type":"compliance_evidence","org":...,"from":...,"to":...}
//	{"type":"audit_chain_head","head_hash":...,"chain_length":N,
//	 "entries_in_window":M,"chain_version":"audit-chain/v1"}
//	{"type":"policy_summary","total":T,"enabled":E,
//	 "effect_allow":A,"effect_deny":D}                 (when policies wired)
//	{"type":"approval_oversight_summary","pending":..,"approved":..,
//	 "rejected":..,"cancelled":..,"total":..}          (when approvals wired;
//	                                                    counts are approvals
//	                                                    CREATED inside the
//	                                                    window, judged by
//	                                                    current status)
//	{"type":"access_lifecycle_summary","api_keys":K,"agent_keys":G}
//	{"type":"evidence_end","generated_for_window":true}
//
// DETERMINISM CONTRACT (mirrors internal/audit/export.go): for unchanged
// data + the same fixed window the bytes are IDENTICAL across calls and
// processes. There is deliberately NO generation timestamp in the bundle —
// the window boundaries are the only time coordinates, so an operator can
// sha256 the file into WORM storage and any later re-export that disagrees
// is a change in the underlying data, not in the clock.
//
// SECRET HYGIENE: the bundle carries COUNTS, hashes and window bounds only —
// never audit metadata bodies, never tool payloads, never secret material.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"agentos/internal/approvals"
	"agentos/internal/audit"
)

// ParseWindow parses the evidence window request parameters. Both boundaries
// are required. Accepted forms:
//
//   - RFC3339 timestamps ("2025-01-01T00:00:00Z") — honored verbatim;
//   - plain dates ("2025-01-01") — from resolves to midnight UTC, to
//     resolves to the END of that day (23:59:59.999999999 UTC) so a
//     calendar window includes its last day.
//
// The window is inclusive on both ends: [from, to].
func ParseWindow(fromRaw, toRaw string) (time.Time, time.Time, error) {
	if fromRaw == "" || toRaw == "" {
		return time.Time{}, time.Time{}, ErrWindowRequired
	}
	from, err := parseBoundary(fromRaw, false)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	to, err := parseBoundary(toRaw, true)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if from.After(to) {
		return time.Time{}, time.Time{}, ErrWindowRange
	}
	return from.UTC(), to.UTC(), nil
}

// parseBoundary parses one boundary; endOfDay promotes date-only values to
// the end of that day.
func parseBoundary(raw string, endOfDay bool) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", raw); err == nil {
		if endOfDay {
			return t.Add(24*time.Hour - time.Nanosecond), nil
		}
		return t, nil
	}
	return time.Time{}, fmt.Errorf("%w: %q is not RFC3339 or YYYY-MM-DD", ErrInvalidWindowInput, raw)
}

// evHeader is line 1: the bundle identity + window.
type evHeader struct {
	Type           string `json:"type"`
	OrganizationID string `json:"org"`
	From           string `json:"from"`
	To             string `json:"to"`
}

// evChainHead is line 2: the tamper-evident anchor + windowed entry count.
type evChainHead struct {
	Type            string `json:"type"`
	HeadHash        string `json:"head_hash"`
	ChainLength     int    `json:"chain_length"`
	EntriesInWindow int    `json:"entries_in_window"`
	ChainVersion    string `json:"chain_version"`
}

// evPolicySummary is line 3 (embedded counts keep a flat, stable shape).
type evPolicySummary struct {
	Type string `json:"type"`
	policySummary
}

// evApprovalSummary is line 4.
type evApprovalSummary struct {
	Type string `json:"type"`
	approvalSummary
}

// evAccessSummary is line 5. Pointer fields render only when the
// corresponding collector is wired (nil = section unavailable, not zero).
type evAccessSummary struct {
	Type      string `json:"type"`
	APIKeys   *int   `json:"api_keys,omitempty"`
	AgentKeys *int   `json:"agent_keys,omitempty"`
}

// evFooter is the final line.
type evFooter struct {
	Type               string `json:"type"`
	GeneratedForWindow bool   `json:"generated_for_window"`
}

// EvidenceBundle renders the JSONL evidence bundle for one org over the
// inclusive window [from, to]. Read-only by construction: it consumes the
// Deps interfaces and appends nothing anywhere.
func (s *Service) EvidenceBundle(ctx context.Context, orgID string, from, to time.Time) ([]byte, error) {
	if orgID == "" {
		return nil, ErrOrgRequired
	}
	if from.IsZero() || to.IsZero() {
		return nil, ErrWindowRequired
	}
	if from.After(to) {
		return nil, ErrWindowRange
	}
	from = from.UTC()
	to = to.UTC()

	out := make([]byte, 0, 1024)
	line := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		out = append(out, b...)
		out = append(out, '\n')
		return nil
	}

	// Line 1: header.
	if err := line(evHeader{
		Type:           "compliance_evidence",
		OrganizationID: orgID,
		From:           from.Format(time.RFC3339Nano),
		To:             to.Format(time.RFC3339Nano),
	}); err != nil {
		return nil, err
	}

	// Line 2: audit chain anchor (+ windowed entry count). Omitted only when
	// the audit surface is entirely unwired.
	if s.deps.Audit != nil {
		var head string
		var chainLen, inWindow int
		if h, l, err := s.deps.Audit.ChainHead(ctx, orgID); err == nil {
			head, chainLen = h, l
		}
		if entries, err := s.deps.Audit.ListCtx(ctx, orgID); err == nil {
			inWindow = countEntriesInWindow(entries, from, to)
		}
		if err := line(evChainHead{
			Type:            "audit_chain_head",
			HeadHash:        head,
			ChainLength:     chainLen,
			EntriesInWindow: inWindow,
			ChainVersion:    auditChainVersionV1,
		}); err != nil {
			return nil, err
		}
	}

	// Line 3: policy inventory summary.
	if s.deps.Policies != nil {
		var sum policySummary
		if list, err := s.deps.Policies.ListPoliciesCtx(ctx, orgID); err == nil {
			sum = policySummaryCounts(list)
		}
		if err := line(evPolicySummary{Type: "policy_summary", policySummary: sum}); err != nil {
			return nil, err
		}
	}

	// Line 4: human-oversight summary (approvals created inside the window,
	// counted by current status).
	if s.deps.Approvals != nil {
		var sum approvalSummary
		if list, err := s.deps.Approvals.List(ctx, orgID, ""); err == nil {
			inWindowList := make([]*approvals.Approval, 0, len(list))
			for _, a := range list {
				if a != nil && inWindow(a.CreatedAt, from, to) {
					inWindowList = append(inWindowList, a)
				}
			}
			sum = countApprovals(inWindowList)
		}
		if err := line(evApprovalSummary{Type: "approval_oversight_summary", approvalSummary: sum}); err != nil {
			return nil, err
		}
	}

	// Line 5: access lifecycle summary (live counts; nil when unwired).
	access := evAccessSummary{Type: "access_lifecycle_summary"}
	if s.deps.OrgAPIKeys != nil {
		if keys, err := s.deps.OrgAPIKeys.ListKeysCtx(ctx, orgID); err == nil {
			n := len(keys)
			access.APIKeys = &n
		}
	}
	if s.deps.Agents != nil && s.deps.AgentKeys != nil {
		if rows, err := s.deps.Agents.ListAgentsCtx(ctx, orgID); err == nil {
			total := 0
			for _, agent := range rows {
				if agent == nil {
					continue
				}
				if keys, err := s.deps.AgentKeys.ListAgentKeys(ctx, orgID, agent.ID); err == nil {
					total += len(keys)
				}
			}
			access.AgentKeys = &total
		}
	}
	if err := line(access); err != nil {
		return nil, err
	}

	// Line 6: footer.
	if err := line(evFooter{Type: "evidence_end", GeneratedForWindow: true}); err != nil {
		return nil, err
	}
	return out, nil
}

// inWindow reports whether ts falls in the inclusive window [from, to].
func inWindow(ts time.Time, from, to time.Time) bool {
	t := ts.UTC()
	return !t.Before(from) && !t.After(to)
}

// countEntriesInWindow counts audit entries whose CreatedAt falls in the
// window. The audit Entry type is referenced so the audit import stays
// meaningful: the bundle reads the org trail through the read-only listing.
func countEntriesInWindow(entries []*audit.Entry, from, to time.Time) int {
	n := 0
	for _, e := range entries {
		if e != nil && inWindow(e.CreatedAt, from, to) {
			n++
		}
	}
	return n
}
